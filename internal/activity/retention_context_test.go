package activity

import (
	"context"
	"errors"
	"testing"
	"time"
)

func TestRetentionOperationsCancelWhileTheOnlyConnectionIsOccupied(t *testing.T) {
	for _, operation := range []string{"read_limits", "read_status", "save", "prune"} {
		t.Run(operation, func(t *testing.T) {
			s := activityOpen(t)
			connection, err := s.db.Conn(context.Background())
			if err != nil {
				t.Fatal(err)
			}
			defer connection.Close()
			ctx, cancel := context.WithCancel(context.Background())
			done := make(chan error, 1)
			go func() {
				var err error
				switch operation {
				case "read_limits":
					_, err = s.Retention(ctx)
				case "read_status":
					_, err = s.HistoryStatus(ctx)
				case "save":
					err = s.SaveRetention(ctx, defaultRetention)
				case "prune":
					_, err = s.Prune(ctx, time.Now().UTC())
				}
				done <- err
			}()
			// Leave the SQL connection occupied throughout cancellation.
			time.Sleep(10 * time.Millisecond)
			cancel()
			select {
			case err := <-done:
				if !errors.Is(err, context.Canceled) {
					t.Fatal("operation did not return its cancellation", err)
				}
			case <-time.After(200 * time.Millisecond):
				t.Fatal("canceled retention operation kept waiting for SQL")
			}
			wanted := uint64(0)
			if operation == "save" || operation == "prune" {
				wanted = 1
			}
			health := s.IngestionHealth()
			if health.MaintenanceFailures != wanted || health.WriteFailures != 0 {
				t.Fatalf("maintenance cancellation was counted incorrectly: %#v", health)
			}
		})
	}
}

func TestFailedOrCanceledRetentionCleanupRollsBackLimitsAndDeletedEvents(t *testing.T) {
	for _, failure := range []string{"reject_after_events_deleted", "cancel_after_events_deleted"} {
		t.Run(failure, func(t *testing.T) {
			s := activityOpen(t)
			at := time.Now().UTC().Add(-48 * time.Hour)
			for _, kind := range []string{"call_start", "call_end"} {
				e := event(kind, kind, "site-1", at)
				e.CallID = "expired-call"
				if err := s.Append(e); err != nil {
					t.Fatal(err)
				}
			}
			before, err := s.HistoryStatus(context.Background())
			if err != nil {
				t.Fatal(err)
			}
			trigger := `CREATE TRIGGER block_retention BEFORE DELETE ON calls BEGIN SELECT RAISE(ABORT,'injected cleanup rejection'); END`
			if failure == "cancel_after_events_deleted" {
				// Event cleanup runs first; interrupt later call cleanup after the
				// limits update and first delete have already executed in the tx.
				trigger = `CREATE TRIGGER block_retention BEFORE DELETE ON calls BEGIN SELECT (WITH RECURSIVE n(x) AS (VALUES(1) UNION ALL SELECT x+1 FROM n WHERE x<2000000) SELECT SUM(x) FROM n); END`
			}
			if _, err = s.db.Exec(trigger); err != nil {
				t.Fatal(err)
			}
			changed := defaultRetention
			changed.Days = 1
			ctx, cancel := context.WithTimeout(context.Background(), 20*time.Millisecond)
			err = s.SaveRetention(ctx, changed)
			cancel()
			if err == nil || (failure == "cancel_after_events_deleted" && !errors.Is(err, context.DeadlineExceeded)) {
				t.Fatal("cleanup failure was not returned", err)
			}
			after, err := s.HistoryStatus(context.Background())
			if err != nil {
				t.Fatal(err)
			}
			if after.Settings != before.Settings || after.Events != before.Events || after.Calls != before.Calls || after.PrunedUnuploaded != before.PrunedUnuploaded || after.PendingUpload != before.PendingUpload {
				t.Fatalf("failed cleanup persisted a partial save: before %#v after %#v", before, after)
			}
			if after.Ingestion.MaintenanceFailures != 1 || after.Ingestion.WriteFailures != 0 {
				t.Fatalf("one failed maintenance was counted as event loss or counted twice: %#v", after.Ingestion)
			}
			if _, err = s.db.Exec("DROP TRIGGER block_retention"); err != nil {
				t.Fatal(err)
			}
			if err = s.SaveRetention(context.Background(), changed); err != nil {
				t.Fatal(err)
			}
			after, err = s.HistoryStatus(context.Background())
			if err != nil || after.Settings != changed || after.Events != 0 || after.Calls != 0 || after.PrunedUnuploaded != 2 {
				t.Fatalf("successful retention did not commit the whole operation: %#v %v", after, err)
			}
		})
	}
}

func TestZeroRetentionSettingsDoNotCountAsFailedMaintenance(t *testing.T) {
	s := activityOpen(t)
	if err := s.SaveRetention(context.Background(), Retention{}); err != nil {
		t.Fatal(err)
	}
	if health := s.IngestionHealth(); health.MaintenanceFailures != 0 || health.WriteFailures != 0 {
		t.Fatalf("configuration save became a storage failure: %#v", health)
	}
}
