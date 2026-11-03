package main

import (
	"bytes"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"net/http"
	"strconv"
	"strings"
	"time"
)

const (
	maxBodyBytes    = 5 << 20 // 5 MiB
	maxBatchSize    = 5000    // events per request
	defaultPageSize = 50
	maxPageSize     = 200
)

// Server wires HTTP handlers to the Store. The clock is injectable so tests
// can run against a fixed "now".
type Server struct {
	store         *Store
	now           func() time.Time
	maxFutureSkew time.Duration // 0 disables the "timestamp in the future" check
}

func NewServer(store *Store, now func() time.Time, maxFutureSkew time.Duration) *Server {
	return &Server{store: store, now: now, maxFutureSkew: maxFutureSkew}
}

func (s *Server) routes() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("POST /events", s.handlePostEvents)
	mux.HandleFunc("GET /campaigns/{campaignID}/stats", s.handleGetStats)
	mux.HandleFunc("GET /campaigns/{campaignID}/events", s.handleGetEvents)
	return mux
}

// handlePostEvents ingests a JSON array of events.
//
// Status codes:
//
//	200  every element was applied or was a harmless duplicate
//	207  some elements were applied/duplicates, some were rejected/conflicts
//	422  the array was readable but NOTHING in it was usable
//	400  the body is not a JSON array (or is not valid JSON at all)
//	413  body or batch too large
//
// Anything 2xx tells the provider "stop retrying". That is intentional for
// 207: retrying a batch won't fix a permanently invalid event, and the valid
// events in it have already been stored.
func (s *Server) handlePostEvents(w http.ResponseWriter, r *http.Request) {
	body, err := io.ReadAll(http.MaxBytesReader(w, r.Body, maxBodyBytes))
	if err != nil {
		var tooBig *http.MaxBytesError
		if errors.As(err, &tooBig) {
			writeError(w, http.StatusRequestEntityTooLarge, "request body too large")
			return
		}
		writeError(w, http.StatusBadRequest, "could not read request body")
		return
	}

	trimmed := bytes.TrimSpace(body)
	if len(trimmed) == 0 || trimmed[0] != '[' {
		writeError(w, http.StatusBadRequest, "request body must be a JSON array of events")
		return
	}

	// Decode into raw per-element messages first. This is the key step: the
	// array structure is checked once, but each event's *contents* are only
	// decoded later, one at a time, so one bad event cannot sink the others.
	var items []json.RawMessage
	if err := json.Unmarshal(trimmed, &items); err != nil {
		writeError(w, http.StatusBadRequest, "invalid JSON: "+err.Error())
		return
	}
	if len(items) > maxBatchSize {
		writeError(w, http.StatusRequestEntityTooLarge,
			fmt.Sprintf("batch has %d events; maximum is %d", len(items), maxBatchSize))
		return
	}

	now := s.now().UTC()
	resp := PostResponse{Received: len(items), Issues: []EventIssue{}}

	valid := make([]StoredEvent, 0, len(items))
	validIndex := make([]int, 0, len(items)) // position of valid[j] in the request
	for i, raw := range items {
		ev, problems := parseEvent(raw, now, s.maxFutureSkew)
		if len(problems) > 0 {
			resp.Rejected++
			resp.Issues = append(resp.Issues, EventIssue{
				Index: i, EventID: ev.EventID, Status: "rejected", Errors: problems,
			})
			continue
		}
		ev.ReceivedAt = now
		valid = append(valid, ev)
		validIndex = append(validIndex, i)
	}

	outcomes := s.store.AddBatch(valid)
	for j, outcome := range outcomes {
		switch outcome {
		case Accepted:
			resp.Accepted++
		case Duplicate:
			resp.Duplicates++
		case Conflict:
			resp.Conflicts++
			resp.Issues = append(resp.Issues, EventIssue{
				Index:   validIndex[j],
				EventID: valid[j].EventID,
				Status:  "conflict",
				Errors:  []string{"event_id already exists with different content; the original was kept"},
			})
		}
	}

	status := http.StatusOK
	switch problems := resp.Rejected + resp.Conflicts; {
	case problems == 0:
		// all good: 200
	case resp.Accepted+resp.Duplicates == 0:
		status = http.StatusUnprocessableEntity
	default:
		status = http.StatusMultiStatus
	}

	log.Printf("POST /events status=%d received=%d accepted=%d duplicates=%d conflicts=%d rejected=%d",
		status, resp.Received, resp.Accepted, resp.Duplicates, resp.Conflicts, resp.Rejected)
	writeJSON(w, status, resp)
}

// handleGetStats returns the counters for one campaign.
func (s *Server) handleGetStats(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, http.StatusOK, s.store.Stats(r.PathValue("campaignID")))
}

// handleGetEvents returns a page of a campaign's events, newest first.
// Query params: limit (1..200, default 50), cursor (from a previous page).
func (s *Server) handleGetEvents(w http.ResponseWriter, r *http.Request) {
	campaignID := r.PathValue("campaignID")

	limit := defaultPageSize
	if v := r.URL.Query().Get("limit"); v != "" {
		n, err := strconv.Atoi(v)
		if err != nil || n < 1 || n > maxPageSize {
			writeError(w, http.StatusBadRequest,
				fmt.Sprintf("limit must be an integer between 1 and %d", maxPageSize))
			return
		}
		limit = n
	}

	var cur *pageCursor
	if v := r.URL.Query().Get("cursor"); v != "" {
		c, err := decodeCursor(v)
		if err != nil {
			writeError(w, http.StatusBadRequest, "invalid cursor")
			return
		}
		cur = &c
	}

	events, more := s.store.Events(campaignID, limit, cur)
	page := EventsPage{CampaignID: campaignID, Events: events}
	if more && len(events) > 0 {
		last := events[len(events)-1]
		page.NextCursor = encodeCursor(last.Timestamp, last.EventID)
	}
	writeJSON(w, http.StatusOK, page)
}

// The cursor is just "timestamp|event_id" base64-encoded so clients treat it
// as opaque. event_id may contain '|', so decoding splits on the FIRST one
// only (RFC 3339 timestamps never contain '|').
func encodeCursor(t time.Time, eventID string) string {
	raw := t.UTC().Format(time.RFC3339Nano) + "|" + eventID
	return base64.RawURLEncoding.EncodeToString([]byte(raw))
}

func decodeCursor(s string) (pageCursor, error) {
	b, err := base64.RawURLEncoding.DecodeString(s)
	if err != nil {
		return pageCursor{}, err
	}
	parts := strings.SplitN(string(b), "|", 2)
	if len(parts) != 2 || parts[1] == "" {
		return pageCursor{}, errors.New("malformed cursor")
	}
	t, err := time.Parse(time.RFC3339Nano, parts[0])
	if err != nil {
		return pageCursor{}, err
	}
	return pageCursor{Timestamp: t, EventID: parts[1]}, nil
}

func writeError(w http.ResponseWriter, status int, msg string) {
	writeJSON(w, status, map[string]string{"error": msg})
}

func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	if err := json.NewEncoder(w).Encode(v); err != nil {
		log.Printf("writeJSON: %v", err)
	}
}
