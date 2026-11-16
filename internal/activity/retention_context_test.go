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
			// Open publishes the initialized Store before its host quiet turn
			// finishes. Cross that owner barrier before testing active SQL waits;
			// cancellation while merely queued has separate no-failure coverage.
			ready, stop := context.WithTimeout(context.Background(), 5*time.Second)
			if err := s.Flush(ready); err != nil {
				stop()
				t.Fatal(err)
			}
			stop()
			db := s.db
			if operation == "read_limits" || operation == "read_status" {
				db = s.readDB
			}
			connection, err := db.Conn(context.Background())
			if err != nil {
				t.Fatal(err)
			}
			defer connection.Close()
			ctx, cancel := context.WithCancel(context.Background())
			done := make(chan error, 1)
			waits := db.Stats().WaitCount
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
			// Prove that this operation reached the occupied SQL connection,
			// rather than assuming a scheduling delay is enough to enter SQL.
			deadline := time.Now().Add(time.Second)
			for db.Stats().WaitCount == waits && time.Now().Before(deadline) {
				time.Sleep(time.Millisecond)
			}
			if db.Stats().WaitCount == waits {
				cancel()
				_ = connection.Close()
				<-done
				t.Fatal("operation did not reach the occupied SQL connection")
			}
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
			if operation == "prune" {
				wanted = 1
			}
			// A canceled caller can return before the one owning writer has
			// observed cancellation and published its diagnostic result.
			deadline = time.Now().Add(200 * time.Millisecond)
			for operation == "prune" && s.IngestionHealth().MaintenanceFailures != wanted && time.Now().Before(deadline) {
				time.Sleep(time.Millisecond)
			}
			health := s.IngestionHealth()
			if health.MaintenanceFailures != wanted || health.WriteFailures != 0 {
				t.Fatalf("maintenance cancellation was counted incorrectly: %#v", health)
			}
		})
	}
}

func TestRetentionSaveCommitsPolicyIndependentlyOfCleanupFailure(t *testing.T) {
	s := activityOpen(t)
	at := time.Now().UTC().Add(-48 * time.Hour)
	for _, kind := range []string{"call_start", "call_end"} {
		e := event(kind, kind, "site-1", at)
		e.CallID = "expired-call"
		if err := s.Append(context.Background(), e); err != nil {
			t.Fatal(err)
		}
	}
	before, err := s.HistoryStatus(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if _, err = s.db.Exec(`CREATE TRIGGER block_retention BEFORE DELETE ON calls BEGIN SELECT RAISE(ABORT,'private cleanup detail'); END`); err != nil {
		t.Fatal(err)
	}
	changed := defaultRetention
	changed.Days = 1
	if err = s.SaveRetention(context.Background(), changed); err != nil {
		t.Fatal("saving settings waited for failing cleanup", err)
	}
	waitCleanupState(t, s, func(v CleanupStatus) bool { return v.LastFailureAt != nil })
	after, err := s.HistoryStatus(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if after.Settings != changed || after.Events != before.Events || after.Calls != before.Calls {
		t.Fatalf("policy must commit while failed deletion batch rolls back: %#v", after)
	}
	if after.Ingestion.MaintenanceFailures != 1 || after.Ingestion.WriteFailures != 0 || after.Cleanup.LastError != "History cleanup failed; background retry is pending." {
		t.Fatalf("failed cleanup is not visible independently of durable settings: %#v", after)
	}
	if _, err = s.db.Exec("DROP TRIGGER block_retention"); err != nil {
		t.Fatal(err)
	}
	if err = s.RequestCleanup(); err != nil {
		t.Fatal(err)
	}
	waitCleanupState(t, s, func(v CleanupStatus) bool { return !v.Pending && !v.Running && v.LastCompletedAt != nil })
	after, err = s.HistoryStatus(context.Background())
	if err != nil || after.Settings != changed || after.Events != 0 || after.Calls != 0 || after.Cleanup.LastError != "" {
		t.Fatalf("background recovery did not enforce unchanged saved policy: %#v %v", after, err)
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
