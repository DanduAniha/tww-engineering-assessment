package main

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

// All tests run against this fixed "now" so they don't depend on the real clock.
var fixedNow = time.Date(2026, 8, 11, 0, 0, 0, 0, time.UTC)

func newTestHandler() http.Handler {
	srv := NewServer(NewStore(), func() time.Time { return fixedNow }, 24*time.Hour)
	return srv.routes()
}

// evJSON builds one event as a JSON string.
func evJSON(id, campaign, contact, typ, ts string) string {
	return fmt.Sprintf(`{"event_id":%q,"campaign_id":%q,"contact_id":%q,"type":%q,"timestamp":%q}`,
		id, campaign, contact, typ, ts)
}

func batch(events ...string) string { return "[" + strings.Join(events, ",") + "]" }

func post(t *testing.T, h http.Handler, body string) (int, PostResponse) {
	t.Helper()
	req := httptest.NewRequest(http.MethodPost, "/events", strings.NewReader(body))
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	var resp PostResponse
	_ = json.Unmarshal(rec.Body.Bytes(), &resp) // error bodies have another shape; callers check Code
	return rec.Code, resp
}

func getStats(t *testing.T, h http.Handler, campaign string) CampaignStats {
	t.Helper()
	req := httptest.NewRequest(http.MethodGet, "/campaigns/"+campaign+"/stats", nil)
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("stats status = %d, body = %s", rec.Code, rec.Body.String())
	}
	var st CampaignStats
	if err := json.Unmarshal(rec.Body.Bytes(), &st); err != nil {
		t.Fatalf("decode stats: %v", err)
	}
	return st
}

func TestPostValidBatch(t *testing.T) {
	h := newTestHandler()
	code, resp := post(t, h, batch(
		evJSON("e1", "c1", "u1", "sent", "2026-08-10T10:00:00Z"),
		evJSON("e2", "c1", "u1", "delivered", "2026-08-10T10:00:05Z"),
		evJSON("e3", "c1", "u1", "opened", "2026-08-10T10:05:00Z"),
		evJSON("e4", "c1", "u1", "clicked", "2026-08-10T10:06:00Z"),
	))
	if code != http.StatusOK || resp.Accepted != 4 || resp.Received != 4 {
		t.Fatalf("code=%d resp=%+v", code, resp)
	}
	st := getStats(t, h, "c1")
	if st.CampaignID != "c1" || st.Sent != 1 || st.Delivered != 1 || st.Opened != 1 || st.Clicked != 1 {
		t.Fatalf("stats = %+v", st)
	}
}

func TestDuplicateRetryDoesNotDoubleCount(t *testing.T) {
	h := newTestHandler()
	body := batch(
		evJSON("e1", "c1", "u1", "sent", "2026-08-10T10:00:00Z"),
		evJSON("e2", "c1", "u1", "opened", "2026-08-10T10:05:00Z"),
	)
	post(t, h, body)
	code, resp := post(t, h, body) // the provider retries the exact same request
	if code != http.StatusOK || resp.Accepted != 0 || resp.Duplicates != 2 {
		t.Fatalf("retry: code=%d resp=%+v", code, resp)
	}
	st := getStats(t, h, "c1")
	if st.Sent != 1 || st.Opened != 1 {
		t.Fatalf("duplicates changed the stats: %+v", st)
	}
}

func TestMixedBatchKeepsValidEvents(t *testing.T) {
	h := newTestHandler()
	code, resp := post(t, h, batch(
		evJSON("e1", "c1", "u1", "sent", "2026-08-10T10:00:00Z"), // valid
		`42`, // not an object
		evJSON("e2", "c1", "u1", "bounced", "2026-08-10T10:00:00Z"),   // unsupported type
		evJSON("e3", "c1", "u1", "sent", "10-08-2026 09:00"),          // bad timestamp
		evJSON("e4", "c1", "u1", "delivered", "2026-08-10T10:00:05Z"), // valid
		evJSON("e1", "c1", "u1", "sent", "2026-08-10T10:00:00Z"),      // duplicate of the first
	))
	if code != http.StatusMultiStatus {
		t.Fatalf("code = %d, want 207; resp=%+v", code, resp)
	}
	if resp.Received != 6 || resp.Accepted != 2 || resp.Duplicates != 1 || resp.Rejected != 3 || len(resp.Issues) != 3 {
		t.Fatalf("resp = %+v", resp)
	}
	st := getStats(t, h, "c1")
	if st.Sent != 1 || st.Delivered != 1 {
		t.Fatalf("valid events were lost or double counted: %+v", st)
	}
}

func TestAllInvalidReturns422(t *testing.T) {
	h := newTestHandler()
	code, resp := post(t, h, batch(`{}`, `null`, `"x"`))
	if code != http.StatusUnprocessableEntity || resp.Rejected != 3 || resp.Accepted != 0 {
		t.Fatalf("code=%d resp=%+v", code, resp)
	}
}

func TestBodyThatIsNotAnArrayReturns400(t *testing.T) {
	h := newTestHandler()
	for _, body := range []string{``, `{}`, `not json`, `[1,2`, `null`} {
		if code, _ := post(t, h, body); code != http.StatusBadRequest {
			t.Errorf("body %q: code = %d, want 400", body, code)
		}
	}
}

func TestEmptyArrayIsFine(t *testing.T) {
	if code, resp := post(t, newTestHandler(), `[]`); code != http.StatusOK || resp.Received != 0 {
		t.Fatalf("code=%d resp=%+v", code, resp)
	}
}

func TestUnknownCampaignHasZeroStats(t *testing.T) {
	st := getStats(t, newTestHandler(), "nope")
	if st.CampaignID != "nope" || st.Sent+st.Delivered+st.Opened+st.Clicked != 0 || st.LastEventAt != nil {
		t.Fatalf("stats = %+v", st)
	}
}

func TestOutOfOrderAndLateEventsAreCounted(t *testing.T) {
	h := newTestHandler()
	// "opened" arrives before its "delivered", and "sent" arrives days late.
	post(t, h, batch(evJSON("e3", "c1", "u1", "opened", "2026-08-10T10:05:00Z")))
	post(t, h, batch(evJSON("e2", "c1", "u1", "delivered", "2026-08-10T10:00:05Z")))
	post(t, h, batch(evJSON("e1", "c1", "u1", "sent", "2026-08-07T10:00:00Z")))
	st := getStats(t, h, "c1")
	if st.Sent != 1 || st.Delivered != 1 || st.Opened != 1 {
		t.Fatalf("stats = %+v", st)
	}
	if st.LastEventAt == nil || !st.LastEventAt.Equal(time.Date(2026, 8, 10, 10, 5, 0, 0, time.UTC)) {
		t.Fatalf("last_event_at = %v, want the newest provider timestamp", st.LastEventAt)
	}
}

func TestOpenedCountsEventsUniqueOpenedCountsContacts(t *testing.T) {
	h := newTestHandler()
	post(t, h, batch(
		evJSON("e1", "c1", "u1", "opened", "2026-08-10T10:00:00Z"),
		evJSON("e2", "c1", "u1", "opened", "2026-08-10T11:00:00Z"),
		evJSON("e3", "c1", "u1", "opened", "2026-08-10T12:00:00Z"),
		evJSON("e4", "c1", "u2", "opened", "2026-08-10T12:00:00Z"),
		evJSON("e5", "c2", "u1", "opened", "2026-08-10T12:00:00Z"), // other campaign
	))
	c1 := getStats(t, h, "c1")
	if c1.Opened != 4 || c1.UniqueOpened != 2 {
		t.Fatalf("c1 = %+v", c1)
	}
	c2 := getStats(t, h, "c2")
	if c2.Opened != 1 || c2.UniqueOpened != 1 {
		t.Fatalf("c2 = %+v", c2)
	}
}

func TestSameEventIDWithDifferentContentIsAConflictAndFirstWins(t *testing.T) {
	h := newTestHandler()
	post(t, h, batch(evJSON("e1", "c1", "u1", "sent", "2026-08-10T10:00:00Z")))
	code, resp := post(t, h, batch(evJSON("e1", "c1", "u1", "clicked", "2026-08-10T10:00:00Z")))
	if code != http.StatusUnprocessableEntity || resp.Conflicts != 1 || resp.Accepted != 0 {
		t.Fatalf("code=%d resp=%+v", code, resp)
	}
	st := getStats(t, h, "c1")
	if st.Sent != 1 || st.Clicked != 0 {
		t.Fatalf("stats = %+v", st)
	}
}

func TestTimestampValidation(t *testing.T) {
	h := newTestHandler()
	// fixedNow is 2026-08-11T00:00Z; the allowed skew is 24h.
	_, resp := post(t, h, batch(
		evJSON("ok", "c1", "u1", "sent", "2026-08-11T12:00:00Z"),     // 12h ahead: allowed
		evJSON("future", "c1", "u1", "sent", "2026-08-13T00:00:00Z"), // 2 days ahead: rejected
		evJSON("offset", "c1", "u1", "sent", "2026-08-10T15:30:00+05:30"),
	))
	if resp.Accepted != 2 || resp.Rejected != 1 {
		t.Fatalf("resp = %+v", resp)
	}
}

func TestTypeAndWhitespaceAreNormalised(t *testing.T) {
	h := newTestHandler()
	code, resp := post(t, h, batch(evJSON(" e1 ", " c1 ", " u1 ", " OPENED ", "2026-08-10T10:00:00Z")))
	if code != http.StatusOK || resp.Accepted != 1 {
		t.Fatalf("code=%d resp=%+v", code, resp)
	}
	if st := getStats(t, h, "c1"); st.Opened != 1 {
		t.Fatalf("stats = %+v", st)
	}
	// The same event with its spaces removed is recognised as a duplicate.
	if _, resp := post(t, h, batch(evJSON("e1", "c1", "u1", "opened", "2026-08-10T10:00:00Z"))); resp.Duplicates != 1 {
		t.Fatalf("resp = %+v", resp)
	}
}

func TestWrongMethodIsRejected(t *testing.T) {
	rec := httptest.NewRecorder()
	newTestHandler().ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/events", nil))
	if rec.Code != http.StatusMethodNotAllowed {
		t.Fatalf("code = %d, want 405", rec.Code)
	}
}

func TestEventsPagination(t *testing.T) {
	h := newTestHandler()
	post(t, h, batch(
		evJSON("e1", "c1", "u1", "sent", "2026-08-10T10:00:00Z"),
		evJSON("e2", "c1", "u1", "sent", "2026-08-10T10:01:00Z"),
		evJSON("e3", "c1", "u1", "sent", "2026-08-10T10:02:00Z"),
		evJSON("e4", "c1", "u1", "sent", "2026-08-10T10:02:00Z"), // same timestamp as e3
		evJSON("e5", "c1", "u1", "sent", "2026-08-10T10:03:00Z"),
		evJSON("e6", "c1", "u1", "sent", "2026-08-10T10:04:00Z"),
		evJSON("e7", "c1", "u1", "sent", "2026-08-10T10:05:00Z"),
	))
	var ids []string
	cursor := ""
	pages := 0
	for {
		url := "/campaigns/c1/events?limit=3"
		if cursor != "" {
			url += "&cursor=" + cursor
		}
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, url, nil))
		if rec.Code != http.StatusOK {
			t.Fatalf("status %d: %s", rec.Code, rec.Body.String())
		}
		var page EventsPage
		if err := json.Unmarshal(rec.Body.Bytes(), &page); err != nil {
			t.Fatal(err)
		}
		for _, e := range page.Events {
			ids = append(ids, e.EventID)
		}
		pages++
		if page.NextCursor == "" {
			break
		}
		if pages > 10 {
			t.Fatal("pagination does not terminate")
		}
		cursor = page.NextCursor
	}
	// Newest first; e4 sorts after e3 on the timestamp tie, so it comes first.
	want := []string{"e7", "e6", "e5", "e4", "e3", "e2", "e1"}
	if strings.Join(ids, ",") != strings.Join(want, ",") || pages != 3 {
		t.Fatalf("ids=%v pages=%d, want %v in 3 pages", ids, pages, want)
	}
}

func TestEventsEndpointRejectsBadParams(t *testing.T) {
	h := newTestHandler()
	for _, q := range []string{"limit=0", "limit=abc", "limit=1000", "cursor=!!!"} {
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/campaigns/c1/events?"+q, nil))
		if rec.Code != http.StatusBadRequest {
			t.Errorf("%s: code = %d, want 400", q, rec.Code)
		}
	}
}

// Many requests carrying the SAME events at the same time: every event must
// be counted exactly once. Run with -race to also check for data races.
func TestConcurrentPostsCountEachEventOnce(t *testing.T) {
	h := newTestHandler()
	var events []string
	types := []string{"sent", "delivered", "opened", "clicked"}
	for i := 0; i < 100; i++ {
		events = append(events, evJSON(fmt.Sprintf("e%03d", i), "c1", fmt.Sprintf("u%d", i%10), types[i%4], "2026-08-10T10:00:00Z"))
	}
	body := batch(events...)

	var accepted atomic.Int64
	var wg sync.WaitGroup
	for g := 0; g < 20; g++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			_, resp := post(t, h, body)
			accepted.Add(int64(resp.Accepted))
			getStats(t, h, "c1") // a concurrent reader
		}()
	}
	wg.Wait()

	if accepted.Load() != 100 {
		t.Fatalf("accepted across all requests = %d, want exactly 100", accepted.Load())
	}
	st := getStats(t, h, "c1")
	if st.Sent != 25 || st.Delivered != 25 || st.Opened != 25 || st.Clicked != 25 {
		t.Fatalf("stats = %+v, want 25 of each", st)
	}
}

// The seed file mixes retries, conflicts and junk. The expected numbers below
// were derived independently of this code by reading the file (see NOTES.md).
func TestSeedFile(t *testing.T) {
	data, err := os.ReadFile("seed/events.json")
	if err != nil {
		t.Fatal(err)
	}
	h := newTestHandler()

	code, resp := post(t, h, string(data))
	if code != http.StatusMultiStatus {
		t.Fatalf("code = %d, want 207", code)
	}
	if resp.Received != 235 || resp.Accepted != 211 || resp.Duplicates != 15 || resp.Conflicts != 3 || resp.Rejected != 6 {
		t.Fatalf("resp = %+v", resp)
	}

	want := map[string]struct {
		sent, delivered, opened, clicked, uniqOpen, uniqClick int
		last                                                  string
	}{
		"cmp_summer_sale": {40, 35, 28, 12, 20, 12, "2026-08-10T18:28:00Z"},
		"cmp_welcome":     {20, 19, 20, 3, 12, 3, "2026-08-10T18:51:00Z"},
		"cmp_winback":     {12, 11, 7, 4, 6, 4, "2026-08-10T19:46:00Z"},
	}
	check := func() {
		t.Helper()
		for id, w := range want {
			st := getStats(t, h, id)
			if st.Sent != w.sent || st.Delivered != w.delivered || st.Opened != w.opened ||
				st.Clicked != w.clicked || st.UniqueOpened != w.uniqOpen || st.UniqueClicked != w.uniqClick {
				t.Errorf("%s: stats = %+v, want %+v", id, st, w)
			}
			if st.LastEventAt == nil || st.LastEventAt.Format(time.RFC3339) != w.last {
				t.Errorf("%s: last_event_at = %v, want %s", id, st.LastEventAt, w.last)
			}
		}
	}
	check()

	// Loading the whole file again must change nothing.
	code, resp = post(t, h, string(data))
	if code != http.StatusMultiStatus || resp.Accepted != 0 || resp.Duplicates != 226 || resp.Conflicts != 3 || resp.Rejected != 6 {
		t.Fatalf("second load: code=%d resp=%+v", code, resp)
	}
	check()
}
