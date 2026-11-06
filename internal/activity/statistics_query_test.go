package activity

import (
	"context"
	"fmt"
	"reflect"
	"testing"
	"time"
)

func TestStatisticsAggregateMatchesScopedAllocationHistory(t *testing.T) {
	s := activityOpen(t)
	at := time.Now().UTC().Add(-time.Minute).Truncate(time.Second).Add(999999999 * time.Nanosecond)
	for _, record := range []struct {
		call, allocation, site, channel string
		tg                              int
		start, end                      time.Duration
		measured                        bool
		speech                          int64
	}{
		{"multi", "first", "site-1", "old-channel", 1001, 0, 1200 * time.Millisecond, true, 300},
		{"multi", "rejoin", "site-1", "new-channel", 1001, 2 * time.Second, 5 * time.Second, true, 1200},
		{"multi", "other-site", "site-2", "voice-2", 1001, 0, 6 * time.Second, true, 1700},
		{"multi", "other-group", "site-2", "voice-3", 1002, 0, 0, false, 0},
		{"ip-call", "ip-call", "", "", 1001, 10 * time.Second, 10005555555 * time.Nanosecond, true, 0},
		{"unknown-speech", "unknown-speech", "site-2", "voice-2", 1002, 20 * time.Second, 22 * time.Second, false, 0},
	} {
		for _, kind := range []string{"call_start", "call_end"} {
			if kind == "call_end" && record.end == 0 {
				continue
			}
			e := event(NewID(), kind, record.site, at.Add(record.start))
			e.CallID, e.ChannelID, e.TalkgroupID = record.call, record.channel, record.tg
			e.Details = map[string]any{"allocation_id": record.allocation}
			if kind == "call_end" {
				e.At, e.Outcome = at.Add(record.end), "completed"
				if record.measured {
					e.Details["speech_ms"] = record.speech
				}
			}
			if err := s.Append(e); err != nil {
				t.Fatal(err)
			}
		}
	}
	for _, q := range []Query{
		{}, {SiteID: "site-1"}, {SiteID: "site-2"}, {TalkgroupID: 1001},
		{ChannelID: "old-channel"}, {ChannelID: "new-channel"}, {CallID: "ip-call"},
		{Restricted: true, Groups: []int{1001}}, {Restricted: true, Groups: []int{}},
		{Since: at.Add(9 * time.Second), Until: at.Add(11 * time.Second)},
	} {
		page, err := s.CallHistory(q)
		if err != nil {
			t.Fatal(err)
		}
		var wanted Statistics
		var speech int64
		allMeasured := true
		for _, call := range page.Calls {
			wanted.Calls++
			if call.EndedAt != nil {
				wanted.CompletedCalls++
			}
			if call.LifetimeMS != nil {
				wanted.CallLifetimeMS += *call.LifetimeMS
			}
			if call.SpeechMS != nil {
				speech += *call.SpeechMS
			} else {
				allMeasured = false
			}
			for _, destination := range call.Destinations {
				if destination.SiteID != "" {
					wanted.SiteDeliveries++
				}
			}
		}
		if allMeasured && wanted.Calls > 0 {
			wanted.SpeechMS = &speech
		}
		got, err := s.Statistics(q)
		if err != nil {
			t.Fatal(err)
		}
		got.Events, got.ByType = 0, nil
		if !reflect.DeepEqual(got, wanted) {
			t.Fatalf("allocation aggregate differs for scope %+v: got %+v want %+v", q, got, wanted)
		}
	}
}

func TestStatisticsCancellationDoesNotWaitForOccupiedDatabase(t *testing.T) {
	s := activityOpen(t)
	connection, err := s.db.Conn(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	defer connection.Close()
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { _, err := s.Statistics(Query{Context: ctx}); done <- err }()
	cancel()
	select {
	case err := <-done:
		if err == nil {
			t.Fatal("canceled statistics query appeared successful")
		}
	case <-time.After(200 * time.Millisecond):
		t.Fatal("canceled statistics held the shared database wait")
	}
}

func TestStatisticsLargeHistoryHasNoPerCallSQLLookups(t *testing.T) {
	s := activityOpen(t)
	// Seed the persisted projection in one transaction: the regression measures
	// aggregate reading, not thousands of unrelated durable commit operations.
	tx, err := s.db.Begin()
	if err != nil {
		t.Fatal(err)
	}
	defer tx.Rollback()
	at := time.Now().UTC().Add(-time.Minute)
	for i := 0; i < 12000; i++ {
		id := fmt.Sprintf("call-%d", i)
		if _, err := tx.Exec("INSERT INTO calls VALUES(?,?,?,?)", id, 10001, at.Format(stampFormat), "rf"); err != nil {
			t.Fatal(err)
		}
		if _, err := tx.Exec("INSERT INTO call_allocations VALUES(?,?,?,?,?,?,?,?,?,?)", id, "site-1", 1001, id, "voice-1", at.Format(stampFormat), at.Add(time.Second).Format(stampFormat), "completed", nil, nil); err != nil {
			t.Fatal(err)
		}
	}
	if err := tx.Commit(); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	stats, err := s.Statistics(Query{Context: ctx})
	if err != nil || stats.Calls != 12000 || stats.SiteDeliveries != 12000 || stats.CallLifetimeMS != 12000000 || stats.CompletedCalls != 12000 || stats.SpeechMS != nil {
		t.Fatalf("large aggregate did not complete accurately inside its query budget: %+v %v", stats, err)
	}
}
