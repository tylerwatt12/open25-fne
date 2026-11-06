package activity

import (
	"testing"
	"time"
)

func TestDelayedOldAllocationObservationsDoNotReplaceFinalMeasurements(t *testing.T) {
	s := activityOpen(t)
	at := time.Now().UTC().Add(-time.Minute)
	appendMeasured := func(id, kind string, after time.Duration, allocated, speech int64) {
		t.Helper()
		e := event(id, kind, "site-1", at.Add(after))
		e.CallID = "measured-conversation"
		e.Details = map[string]any{"allocation_id": "one-allocation", "allocated_ms": allocated, "speech_ms": speech}
		if err := s.Append(e); err != nil {
			t.Fatal(err)
		}
	}
	appendMeasured("start", "call_start", 0, 0, 0)
	appendMeasured("final-ending", "call_end", 5*time.Second, 4800, 4200)
	appendMeasured("delayed-old-ending", "call_end", 2*time.Second, 1800, 1200)
	appendMeasured("delayed-old-start", "call_start", 0, 0, 0)
	page, err := s.CallHistory(Query{})
	if err != nil || len(page.Calls) != 1 || len(page.Calls[0].Destinations) != 1 {
		t.Fatal("missing measured conversation", page, err)
	}
	d := page.Calls[0].Destinations[0]
	if d.EndedAt == nil || !d.EndedAt.Equal(at.Add(5*time.Second)) || d.AllocatedMS == nil || *d.AllocatedMS != 4800 || d.SpeechMS == nil || *d.SpeechMS != 4200 {
		t.Fatalf("old allocation fact replaced final measurements: %#v", d)
	}
	stats, err := s.Statistics(Query{})
	if err != nil || stats.SpeechMS == nil || *stats.SpeechMS != 4200 || stats.CallLifetimeMS != 5000 {
		t.Fatalf("aggregate used stale measurements: %#v %v", stats, err)
	}
	// A genuinely later final observation may replace the earlier result.
	appendMeasured("later-final-ending", "call_end", 6*time.Second, 5800, 4900)
	stats, err = s.Statistics(Query{})
	if err != nil || stats.SpeechMS == nil || *stats.SpeechMS != 4900 || stats.CallLifetimeMS != 6000 {
		t.Fatalf("new final observation was discarded: %#v %v", stats, err)
	}
}
