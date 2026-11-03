package main

import (
	"fmt"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

func mkEvent(id, campaign, contact, typ string, ts time.Time) StoredEvent {
	return StoredEvent{EventID: id, CampaignID: campaign, ContactID: contact, Type: typ, Timestamp: ts}
}

func TestStoreOutcomes(t *testing.T) {
	s := NewStore()
	ts := time.Date(2026, 8, 10, 10, 0, 0, 0, time.UTC)
	got := s.AddBatch([]StoredEvent{
		mkEvent("e1", "c", "u", TypeSent, ts),
		mkEvent("e1", "c", "u", TypeSent, ts),      // duplicate inside one batch
		mkEvent("e1", "c", "u", TypeDelivered, ts), // same id, different content
	})
	want := []AddOutcome{Accepted, Duplicate, Conflict}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("outcome[%d] = %v, want %v", i, got[i], want[i])
		}
	}
	if st := s.Stats("c"); st.Sent != 1 || st.Delivered != 0 {
		t.Fatalf("stats = %+v", st)
	}
}

// Writers and readers hammer the store at once. Run with -race.
func TestStoreConcurrentWritersAndReaders(t *testing.T) {
	s := NewStore()
	ts := time.Date(2026, 8, 10, 10, 0, 0, 0, time.UTC)
	var events []StoredEvent
	for i := 0; i < 200; i++ {
		events = append(events, mkEvent(fmt.Sprintf("e%d", i), "c", fmt.Sprintf("u%d", i%7), TypeOpened, ts.Add(time.Duration(i)*time.Second)))
	}

	var accepted atomic.Int64
	var wg sync.WaitGroup
	for w := 0; w < 16; w++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for _, o := range s.AddBatch(events) {
				if o == Accepted {
					accepted.Add(1)
				}
			}
		}()
	}
	for r := 0; r < 4; r++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for i := 0; i < 200; i++ {
				s.Stats("c")
				s.Events("c", 10, nil)
			}
		}()
	}
	wg.Wait()

	if accepted.Load() != 200 {
		t.Fatalf("accepted = %d, want 200", accepted.Load())
	}
	if st := s.Stats("c"); st.Opened != 200 || st.UniqueOpened != 7 {
		t.Fatalf("stats = %+v", st)
	}
}
