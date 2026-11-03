package main

import (
	"sort"
	"sync"
	"time"
)

// AddOutcome says what happened to one event handed to Store.AddBatch.
type AddOutcome int

const (
	Accepted  AddOutcome = iota // new event, now counted
	Duplicate                   // same event_id and same content as one we hold
	Conflict                    // same event_id but DIFFERENT content; first one wins
)

// pageCursor marks a position in a campaign's event list (see Store.Events).
type pageCursor struct {
	Timestamp time.Time
	EventID   string
}

// Store is the in-memory database. One RWMutex protects everything:
// writers (AddBatch) take the write lock, readers (Stats, Events) take the
// read lock. Counters are updated in the same critical section that records
// the event_id, so "is it a duplicate?" and "bump the counter" can never be
// separated by another request.
type Store struct {
	mu        sync.RWMutex
	byID      map[string]*StoredEvent // event_id -> the event we accepted
	campaigns map[string]*campaignData
}

type campaignData struct {
	sent, delivered, opened, clicked int
	openers                          map[string]struct{} // contact_ids that opened
	clickers                         map[string]struct{} // contact_ids that clicked
	events                           []*StoredEvent      // sorted ascending by (Timestamp, EventID)
}

func NewStore() *Store {
	return &Store{
		byID:      make(map[string]*StoredEvent),
		campaigns: make(map[string]*campaignData),
	}
}

// AddBatch applies a batch of already-validated events under one lock, so a
// concurrent reader sees either none or all of the batch. The result has one
// outcome per input event, in order.
func (s *Store) AddBatch(evs []StoredEvent) []AddOutcome {
	out := make([]AddOutcome, len(evs))
	s.mu.Lock()
	defer s.mu.Unlock()
	for i, ev := range evs {
		out[i] = s.addLocked(ev)
	}
	return out
}

// addLocked must be called with s.mu held for writing.
func (s *Store) addLocked(ev StoredEvent) AddOutcome {
	if prev, ok := s.byID[ev.EventID]; ok {
		if sameContent(prev, &ev) {
			return Duplicate
		}
		return Conflict
	}

	e := ev // our own copy; the store owns it from here on
	s.byID[e.EventID] = &e

	c := s.campaigns[e.CampaignID]
	if c == nil {
		c = &campaignData{
			openers:  make(map[string]struct{}),
			clickers: make(map[string]struct{}),
		}
		s.campaigns[e.CampaignID] = c
	}
	switch e.Type {
	case TypeSent:
		c.sent++
	case TypeDelivered:
		c.delivered++
	case TypeOpened:
		c.opened++
		c.openers[e.ContactID] = struct{}{}
	case TypeClicked:
		c.clicked++
		c.clickers[e.ContactID] = struct{}{}
	}
	c.insert(&e)
	return Accepted
}

// sameContent decides "same event, retried" vs "same id, different event".
// Metadata and ReceivedAt are deliberately ignored: a provider adding a
// metadata field on a retry is still a retry.
func sameContent(a, b *StoredEvent) bool {
	return a.CampaignID == b.CampaignID &&
		a.ContactID == b.ContactID &&
		a.Type == b.Type &&
		a.Timestamp.Equal(b.Timestamp)
}

func eventLess(a, b *StoredEvent) bool {
	if !a.Timestamp.Equal(b.Timestamp) {
		return a.Timestamp.Before(b.Timestamp)
	}
	return a.EventID < b.EventID
}

// insert keeps c.events sorted. Late and out-of-order events simply land in
// the right place; they never change how counters are computed.
func (c *campaignData) insert(e *StoredEvent) {
	i := sort.Search(len(c.events), func(i int) bool {
		return !eventLess(c.events[i], e) // first index whose event is >= e
	})
	c.events = append(c.events, nil)
	copy(c.events[i+1:], c.events[i:])
	c.events[i] = e
}

// Stats returns the counters for one campaign. An unknown campaign is not an
// error: it has simply had no events yet, so every number is zero.
func (s *Store) Stats(campaignID string) CampaignStats {
	s.mu.RLock()
	defer s.mu.RUnlock()

	st := CampaignStats{CampaignID: campaignID}
	c := s.campaigns[campaignID]
	if c == nil {
		return st
	}
	st.Sent = c.sent
	st.Delivered = c.delivered
	st.Opened = c.opened
	st.Clicked = c.clicked
	st.UniqueOpened = len(c.openers)
	st.UniqueClicked = len(c.clickers)
	if n := len(c.events); n > 0 {
		t := c.events[n-1].Timestamp
		st.LastEventAt = &t
	}
	return st
}

// Events returns up to limit events, newest first (by provider timestamp,
// ties broken by event_id). If cur is non-nil, only events strictly older
// than the cursor position are returned. The bool reports whether more,
// older events exist. This is "keyset" pagination: unlike offset paging it
// does not skip or repeat items when new events arrive between page requests.
func (s *Store) Events(campaignID string, limit int, cur *pageCursor) ([]StoredEvent, bool) {
	s.mu.RLock()
	defer s.mu.RUnlock()

	out := []StoredEvent{}
	c := s.campaigns[campaignID]
	if c == nil {
		return out, false
	}

	end := len(c.events) // events[0:end] are the candidates
	if cur != nil {
		end = sort.Search(len(c.events), func(i int) bool {
			e := c.events[i]
			if !e.Timestamp.Equal(cur.Timestamp) {
				return e.Timestamp.After(cur.Timestamp)
			}
			return e.EventID >= cur.EventID // first index at or after the cursor
		})
	}
	start := end - limit
	if start < 0 {
		start = 0
	}
	for i := end - 1; i >= start; i-- {
		out = append(out, *c.events[i])
	}
	return out, start > 0
}
