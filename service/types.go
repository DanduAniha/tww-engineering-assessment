package main

import "time"

// The four event types providers may send.
const (
	TypeSent      = "sent"
	TypeDelivered = "delivered"
	TypeOpened    = "opened"
	TypeClicked   = "clicked"
)

// Event is the raw payload exactly as a provider sends it. Everything is a
// plain string here on purpose: we validate and convert in parseEvent so that
// we can say precisely what is wrong with one event instead of failing the
// whole batch at decode time.
type Event struct {
	EventID    string         `json:"event_id"`
	CampaignID string         `json:"campaign_id"`
	ContactID  string         `json:"contact_id"`
	Type       string         `json:"type"`
	Timestamp  string         `json:"timestamp"`
	Metadata   map[string]any `json:"metadata,omitempty"`
}

// StoredEvent is an event that passed validation, with normalised values.
// Once it is in the Store it is never modified.
type StoredEvent struct {
	EventID    string         `json:"event_id"`
	CampaignID string         `json:"campaign_id"`
	ContactID  string         `json:"contact_id"`
	Type       string         `json:"type"`
	Timestamp  time.Time      `json:"timestamp"`   // when it happened at the provider (UTC)
	ReceivedAt time.Time      `json:"received_at"` // when we first accepted it (UTC)
	Metadata   map[string]any `json:"metadata,omitempty"`
}

// CampaignStats is the answer to GET /campaigns/{id}/stats.
type CampaignStats struct {
	CampaignID    string `json:"campaign_id"`
	Sent          int    `json:"sent"`
	Delivered     int    `json:"delivered"`
	Opened        int    `json:"opened"`         // distinct opened events
	Clicked       int    `json:"clicked"`        // distinct clicked events
	UniqueOpened  int    `json:"unique_opened"`  // distinct contacts that opened
	UniqueClicked int    `json:"unique_clicked"` // distinct contacts that clicked
	// LastEventAt is the newest provider timestamp we hold for this campaign,
	// or null if we hold none. It tells a dashboard how "live" the numbers are.
	LastEventAt *time.Time `json:"last_event_at"`
}

// EventsPage is the answer to GET /campaigns/{id}/events.
type EventsPage struct {
	CampaignID string        `json:"campaign_id"`
	Events     []StoredEvent `json:"events"`
	NextCursor string        `json:"next_cursor,omitempty"`
}

// PostResponse is the answer to POST /events.
type PostResponse struct {
	Received   int          `json:"received"`   // elements in the array
	Accepted   int          `json:"accepted"`   // new events, now counted
	Duplicates int          `json:"duplicates"` // already seen, identical: ignored
	Conflicts  int          `json:"conflicts"`  // already seen, but with different content: ignored
	Rejected   int          `json:"rejected"`   // failed validation: ignored
	Issues     []EventIssue `json:"issues"`     // details for every conflict / rejection
}

// EventIssue explains why one element of a batch was not applied.
type EventIssue struct {
	Index   int      `json:"index"` // position in the submitted array
	EventID string   `json:"event_id,omitempty"`
	Status  string   `json:"status"` // "rejected" or "conflict"
	Errors  []string `json:"errors"`
}
