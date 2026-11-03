package main

import (
	"encoding/json"
	"fmt"
	"strings"
	"time"
)

const maxFieldLen = 256

var supportedTypes = map[string]bool{
	TypeSent:      true,
	TypeDelivered: true,
	TypeOpened:    true,
	TypeClicked:   true,
}

// parseEvent decodes and validates ONE element of a batch. It returns the
// normalised event and a list of problems; an empty list means the event is
// valid. The returned event is only meaningful when there are no problems,
// except that EventID is filled in when possible so the caller can report it.
//
// Normalisation (documented in NOTES.md): surrounding whitespace is trimmed,
// the type is lower-cased, and the timestamp is converted to UTC.
func parseEvent(raw json.RawMessage, now time.Time, maxFutureSkew time.Duration) (StoredEvent, []string) {
	var in Event
	if err := json.Unmarshal(raw, &in); err != nil {
		return StoredEvent{}, []string{"malformed event: " + err.Error()}
	}

	var problems []string
	out := StoredEvent{Metadata: in.Metadata}

	var msg string
	if out.EventID, msg = cleanID("event_id", in.EventID); msg != "" {
		problems = append(problems, msg)
	}
	if out.CampaignID, msg = cleanID("campaign_id", in.CampaignID); msg != "" {
		problems = append(problems, msg)
	}
	if out.ContactID, msg = cleanID("contact_id", in.ContactID); msg != "" {
		problems = append(problems, msg)
	}

	out.Type = strings.ToLower(strings.TrimSpace(in.Type))
	switch {
	case out.Type == "":
		problems = append(problems, "type is required")
	case !supportedTypes[out.Type]:
		problems = append(problems, fmt.Sprintf("unsupported type %q (want sent, delivered, opened or clicked)", in.Type))
	}

	ts := strings.TrimSpace(in.Timestamp)
	if ts == "" {
		problems = append(problems, "timestamp is required")
	} else if t, err := time.Parse(time.RFC3339, ts); err != nil {
		problems = append(problems, "timestamp must be RFC 3339, e.g. 2026-08-10T09:15:04Z")
	} else if maxFutureSkew > 0 && t.After(now.Add(maxFutureSkew)) {
		problems = append(problems, "timestamp is too far in the future")
	} else {
		out.Timestamp = t.UTC()
	}

	return out, problems
}

// cleanID trims an identifier and checks it is present and not absurdly long.
// It returns the cleaned value and a problem description ("" if fine).
func cleanID(field, v string) (string, string) {
	v = strings.TrimSpace(v)
	if v == "" {
		return "", field + " is required"
	}
	if len(v) > maxFieldLen {
		return v, fmt.Sprintf("%s is longer than %d characters", field, maxFieldLen)
	}
	return v, ""
}
