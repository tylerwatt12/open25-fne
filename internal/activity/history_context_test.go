package activity

import (
	"context"
	"errors"
	"fmt"
	"testing"
	"time"
)

func historyContextRead(s *Store, name string, ctx context.Context) error {
	q := Query{Context: ctx}
	switch name {
	case "events":
		_, err := s.EventHistory(q)
		return err
	case "calls":
		_, err := s.CallHistory(q)
		return err
	case "call_destinations":
		_, err := s.destinationHistory(q, "test-call")
		return err
	case "locations":
		_, err := s.LocationHistory(q, time.Now().UTC())
		return err
	default:
		panic("unknown history reader")
	}
}

func TestHistoryReadsCancelWhileTheOnlyConnectionIsOccupied(t *testing.T) {
	for _, name := range []string{"events", "calls", "call_destinations", "locations"} {
		t.Run(name, func(t *testing.T) {
			s := activityOpen(t)
			connection, err := s.db.Conn(context.Background())
			if err != nil {
				t.Fatal(err)
			}
			defer connection.Close()
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			done := make(chan error, 1)
			waits := s.db.Stats().WaitCount
			go func() { done <- historyContextRead(s, name, ctx) }()
			deadline := time.Now().Add(time.Second)
			for s.db.Stats().WaitCount == waits && time.Now().Before(deadline) {
				time.Sleep(time.Millisecond)
			}
			cancel()
			select {
			case err := <-done:
				if !errors.Is(err, context.Canceled) {
					t.Fatal("history read did not return its cancellation", err)
				}
			case <-time.After(200 * time.Millisecond):
				// Release and join even when exercising the broken implementation.
				connection.Close()
				<-done
				t.Fatal("canceled history kept waiting for the occupied database")
			}
			if err := connection.Close(); err != nil {
				t.Fatal(err)
			}
			probe, stop := context.WithTimeout(context.Background(), time.Second)
			defer stop()
			if err := historyContextRead(s, name, probe); err != nil {
				t.Fatal("canceled reader prevented the next history read", err)
			}
		})
	}
}

func TestHistoryCancellationInterruptsActiveSQLAndReleasesItsReader(t *testing.T) {
	for _, reader := range []struct{ name, table string }{
		{"events", "radio_events"}, {"calls", "call_allocations"},
		{"call_destinations", "call_allocations"}, {"locations", "position_history"},
	} {
		t.Run(reader.name, func(t *testing.T) {
			s := activityOpen(t)
			at := time.Now().UTC()
			for _, kind := range []string{"call_start", "location"} {
				e := event(kind, kind, "site-1", at)
				e.CallID = "test-call"
				if kind == "location" {
					e.Details = map[string]any{"latitude": 41.5, "longitude": -81.7, "accuracy": 5, "fix_at": at}
				}
				if err := s.Append(e); err != nil {
					t.Fatal(err)
				}
			}
			// A view forces a genuine SQLite VM scan without a large database
			// fixture. Keeping the original rows/columns preserves each reader's
			// actual filters, scans and call-destination expansion path.
			if _, err := s.db.Exec(fmt.Sprintf(`ALTER TABLE %s RENAME TO saved_history;
CREATE VIEW %s AS WITH RECURSIVE n(x) AS (VALUES(0) UNION ALL SELECT x+1 FROM n WHERE x<5000000)
SELECT saved_history.* FROM saved_history CROSS JOIN n WHERE n.x=5000000`, reader.table, reader.table)); err != nil {
				t.Fatal(err)
			}
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			done := make(chan error, 1)
			go func() { done <- historyContextRead(s, reader.name, ctx) }()
			deadline := time.Now().Add(time.Second)
			for s.db.Stats().InUse == 0 && time.Now().Before(deadline) {
				time.Sleep(time.Millisecond)
			}
			// Location first reads retention settings; allow that small query to
			// finish before cancelling the deliberately expensive page reader.
			time.Sleep(10 * time.Millisecond)
			cancel()
			select {
			case err := <-done:
				if !errors.Is(err, context.Canceled) {
					t.Fatal("active history SQL did not return its cancellation", err)
				}
			case <-time.After(200 * time.Millisecond):
				<-done
				t.Fatal("canceled history SQL kept its reader active")
			}
			probe, stop := context.WithTimeout(context.Background(), time.Second)
			defer stop()
			var count int
			if err := s.db.QueryRowContext(probe, "SELECT COUNT(*) FROM saved_history").Scan(&count); err != nil || count == 0 {
				t.Fatal("cancellation did not release the shared SQL reader", count, err)
			}
			if s.db.Stats().InUse != 0 {
				t.Fatal("history reader retained the only database connection")
			}
		})
	}
}
