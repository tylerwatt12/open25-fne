package activity

import (
	"context"
	"errors"
	"fmt"
	"path/filepath"
	"runtime"
	"testing"
	"time"

	"open25.local/open25-fne/internal/historypriority"
)

func waitCleanupState(t *testing.T, s *Store, ready func(CleanupStatus) bool) CleanupStatus {
	t.Helper()
	deadline := time.Now().Add(12 * time.Second)
	for time.Now().Before(deadline) {
		v := s.CleanupStatus()
		if ready(v) {
			return v
		}
		time.Sleep(time.Millisecond)
	}
	t.Fatalf("cleanup did not reach expected state: %#v", s.CleanupStatus())
	return CleanupStatus{}
}

func TestCleanupAcceptanceStaysBoundedWhileStorageAndQueuesAreBlocked(t *testing.T) {
	s := activityOpen(t)
	connection, err := s.db.Conn(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	defer connection.Close()
	baseline := runtime.NumGoroutine()
	started := time.Now()
	for i := 0; i < 10000; i++ {
		if err = s.RequestCleanup(); err != nil {
			t.Fatal(err)
		}
	}
	if time.Since(started) > 250*time.Millisecond {
		t.Fatal("accepting cleanup waited for storage")
	}
	if extra := runtime.NumGoroutine() - baseline; extra > 4 {
		t.Fatalf("requests created independent workers: %d extra goroutines", extra)
	}
	for i := 0; i < cap(s.queue)+128; i++ {
		s.Enqueue(event(fmt.Sprint(i), "status", "site-1", time.Now()))
	}
	if s.IngestionHealth().QueueDropped == 0 {
		t.Fatal("saturated observations were not reported")
	}
	if !s.CleanupStatus().Pending && !s.CleanupStatus().Running {
		t.Fatal("accepted cleanup disappeared while blocked")
	}
	if err = connection.Close(); err != nil {
		t.Fatal(err)
	}
	// A full queue drains in 32-record turns, with a quiet interval between
	// turns. This bounds history completion, separately from the fast acceptance
	// checks above.
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	if err = s.Flush(ctx); err != nil {
		t.Fatal(err)
	}
	// The later captures are deliberately outside each pre-insert cleanup.
	// Explicitly cover their final committed batch rather than assuming the
	// earlier request also promised completion for subsequently produced facts.
	if err = s.RequestCleanup(); err != nil {
		t.Fatal(err)
	}
	waitCleanupState(t, s, func(v CleanupStatus) bool { return !v.Pending && !v.Running && v.LastCompletedAt != nil })
}

func TestIdleCleanupDoesNotWriteOrSyncTheDatabase(t *testing.T) {
	s := activityOpen(t)
	var before, after int
	if err := s.readDB.QueryRow("PRAGMA data_version").Scan(&before); err != nil {
		t.Fatal(err)
	}
	if _, err := s.db.Exec(`CREATE TRIGGER reject_idle_update BEFORE UPDATE ON history_settings BEGIN SELECT RAISE(ABORT,'idle cleanup wrote settings'); END`); err != nil {
		t.Fatal(err)
	}
	// The trigger creation itself changes data_version; observe its committed baseline.
	if err := s.readDB.QueryRow("PRAGMA data_version").Scan(&before); err != nil {
		t.Fatal(err)
	}
	if err := s.RequestCleanup(); err != nil {
		t.Fatal(err)
	}
	waitCleanupState(t, s, func(v CleanupStatus) bool { return !v.Pending && !v.Running && v.LastCompletedAt != nil })
	if err := s.readDB.QueryRow("PRAGMA data_version").Scan(&after); err != nil {
		t.Fatal(err)
	}
	if before != after || s.IngestionHealth().MaintenanceFailures != 0 {
		t.Fatalf("empty cleanup mutated database: versions %d/%d, health %#v", before, after, s.IngestionHealth())
	}
}

func TestCleanupCommitsBoundedProgressAndWakesCannotBypassRest(t *testing.T) {
	s := activityOpen(t)
	now := time.Now().UTC()
	if err := s.Append(context.Background(), event("seed", "status", "site-1", now)); err != nil {
		t.Fatal(err)
	}
	tx, err := s.db.Begin()
	if err != nil {
		t.Fatal(err)
	}
	for i := 0; i < 3*maintenanceBatch+1; i++ {
		_, err = tx.Exec(`INSERT INTO radio_events SELECT ?,?,received_at,origin,site_id,channel_id,radio_id,talkgroup_id,call_id,type,label,message,audience,severity,outcome,source_kind,details FROM radio_events WHERE id='seed'`, fmt.Sprint(i), now.Add(-40*24*time.Hour).Format(stampFormat))
		if err != nil {
			t.Fatal(err)
		}
	}
	if _, err = tx.Exec("DELETE FROM radio_events WHERE id='seed'"); err != nil {
		t.Fatal(err)
	}
	if err = tx.Commit(); err != nil {
		t.Fatal(err)
	}
	if err = s.RequestCleanup(); err != nil {
		t.Fatal(err)
	}
	first := waitCleanupState(t, s, func(v CleanupStatus) bool { return v.Removed.Events > 0 && !v.Running })
	if first.Removed.Events != maintenanceBatch || !first.Pending || first.LastCompletedAt != nil {
		t.Fatalf("first batch falsely completed or exceeded bound: %#v", first)
	}
	// Flooding wakeups during cooldown cannot cause a second immediate SQL batch.
	until := time.Now().Add(cleanupPause / 2)
	for time.Now().Before(until) {
		if err = s.RequestCleanup(); err != nil {
			t.Fatal(err)
		}
		time.Sleep(time.Millisecond)
	}
	middle := s.CleanupStatus()
	if middle.Removed.Events != maintenanceBatch {
		t.Fatalf("wake bypassed paced rest: %#v", middle)
	}
	done := waitCleanupState(t, s, func(v CleanupStatus) bool { return !v.Pending && !v.Running && v.LastCompletedAt != nil })
	if done.Removed.Events != 3*maintenanceBatch+1 {
		t.Fatalf("cleanup progress omitted committed batches: %#v", done)
	}
}

func TestQueuedHistoryMutationsCancelWithoutChangingDurableState(t *testing.T) {
	for _, kind := range []string{"settings", "prune"} {
		t.Run(kind, func(t *testing.T) {
			s := activityOpen(t)
			if err := s.Append(context.Background(), event("retained", "status", "site-1", time.Now())); err != nil {
				t.Fatal(err)
			}
			limits, err := s.Retention(context.Background())
			if err != nil {
				t.Fatal(err)
			}
			connection, err := s.db.Conn(context.Background())
			if err != nil {
				t.Fatal(err)
			}
			defer connection.Close()
			ctx, cancel := context.WithTimeout(context.Background(), 20*time.Millisecond)
			defer cancel()
			switch kind {
			case "settings":
				changed := limits
				changed.MaxEvents = 0
				err = s.SaveRetention(ctx, changed)
			case "prune":
				_, err = s.Prune(ctx, time.Now().Add(90*24*time.Hour))
			}
			if !errors.Is(err, context.DeadlineExceeded) {
				t.Fatalf("cancelled operation returned %v", err)
			}
			if err = connection.Close(); err != nil {
				t.Fatal(err)
			}
			if err = s.Append(context.Background(), event("barrier", "status", "site-1", time.Now())); err != nil {
				t.Fatal(err)
			}
			after, err := s.HistoryStatus(context.Background())
			if err != nil {
				t.Fatal(err)
			}
			if after.Settings != limits || after.Events != 2 {
				t.Fatalf("cancelled operation changed durable data: %#v", after)
			}
		})
	}
}

func TestArchiveWriterPacesObservationsUploadsAndSettings(t *testing.T) {
	directory := t.TempDir()
	s, err := open(filepath.Join(directory, "activity.db"), true, func(ctx context.Context) (*historypriority.Turn, error) {
		return historypriority.AcquirePath(ctx, filepath.Join(directory, "host.lock"))
	})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = s.Close() })
	ready, stop := context.WithTimeout(context.Background(), 5*time.Second)
	if err = s.Flush(ready); err != nil {
		stop()
		t.Fatal(err)
	}
	stop()
	began := time.Now()
	if err := s.Append(context.Background(), event("first", "status", "site-1", time.Now())); err != nil {
		t.Fatal(err)
	}
	if err := s.Append(context.Background(), event("second", "status", "site-1", time.Now())); err != nil {
		t.Fatal(err)
	}
	limits, err := s.Retention(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if err = s.SaveRetention(context.Background(), limits); err != nil {
		t.Fatal(err)
	}
	// After initialization's owner barrier, these three durable writes require
	// two actual host quiet intervals. A fake/no-op turn cannot satisfy this.
	if elapsed := time.Since(began); elapsed < 500*time.Millisecond {
		t.Fatalf("ordinary writes bypassed storage pacing: %v", elapsed)
	}
	rows, err := s.Events(Query{})
	if err != nil || len(rows) != 2 {
		t.Fatal("paced archive writes were not committed", rows, err)
	}
}
