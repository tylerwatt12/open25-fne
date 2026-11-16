package activity

import (
	"context"
	"fmt"
	"testing"
	"time"
)

// Fixtures are prepared directly while the idle test writer has no queued work.
// Runtime producers always use Enqueue or bounded Append.
func seedArchiveRows(t *testing.T, s *Store, count int, at time.Time) {
	t.Helper()
	tx, err := s.db.Begin()
	if err != nil {
		t.Fatal(err)
	}
	defer tx.Rollback()
	for i := range count {
		stamp := at.Add(time.Duration(i) * time.Millisecond).Format(stampFormat)
		if _, err := tx.Exec("INSERT INTO radio_events VALUES(?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?)", fmt.Sprintf("old-%03d", i), stamp, stamp, "site-1", "site-1", "", 0, 0, "", "status", "Status", "Original observation", "operator", "info", "", "rf", "null"); err != nil {
			t.Fatal(err)
		}
	}
	if err := tx.Commit(); err != nil {
		t.Fatal(err)
	}
}

func TestArchiveCleanupAndIncomingBatchUseOneDurableCommit(t *testing.T) {
	s := activityOpen(t)
	if _, err := s.db.Exec("UPDATE history_settings SET max_events=32"); err != nil {
		t.Fatal(err)
	}
	seedArchiveRows(t, s, 64, time.Now().Add(-time.Hour))
	reader, err := s.readDB.Conn(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	defer reader.Close()
	var before, after int
	if err := reader.QueryRowContext(context.Background(), "PRAGMA data_version").Scan(&before); err != nil {
		t.Fatal(err)
	}
	if err := s.Append(context.Background(), coalescedEvents("new", EventBatchLimit)...); err != nil {
		t.Fatal(err)
	}
	var total, old, current int
	if err := reader.QueryRowContext(context.Background(), "PRAGMA data_version").Scan(&after); err != nil {
		t.Fatal(err)
	}
	if err := reader.QueryRowContext(context.Background(), "SELECT COUNT(*),COUNT(CASE WHEN id LIKE 'old-%' THEN 1 END),COUNT(CASE WHEN id LIKE 'new-%' THEN 1 END) FROM radio_events").Scan(&total, &old, &current); err != nil {
		t.Fatal(err)
	}
	cleanup := s.CleanupStatus()
	if after != before+1 || total != 64 || old != 32 || current != EventBatchLimit || cleanup.Removed.Events != 32 || !cleanup.Pending || cleanup.LastCompletedAt != nil {
		t.Fatalf("cleanup/inbound split commits, exceeded32 deletions, or reported false completion: versions=%d/%d total=%d old=%d new=%d cleanup=%+v", after, before, total, old, current, cleanup)
	}
	var oldest string
	if err := reader.QueryRowContext(context.Background(), "SELECT id FROM radio_events ORDER BY at,id LIMIT 1").Scan(&oldest); err != nil || oldest != "old-032" {
		t.Fatal("pre-insert cleanup changed retention survivor ordering", oldest, err)
	}
}

func TestIncomingExpiredObservationExistsDurablyBeforeLaterCleanup(t *testing.T) {
	s := activityOpen(t)
	incoming := event("late-original", "status", "site-1", time.Now().Add(-31*24*time.Hour))
	if err := s.Append(context.Background(), incoming); err != nil {
		t.Fatal(err)
	}
	rows, err := s.Events(Query{ID: incoming.ID})
	if err != nil || len(rows) != 1 || !rows[0].At.Equal(incoming.At) {
		t.Fatal("ACK described an observation inserted and pruned before first durable commit", rows, err)
	}
	if cleanup := s.CleanupStatus(); !cleanup.Pending || cleanup.LastCompletedAt != nil || cleanup.Removed.Events != 0 {
		t.Fatal("pre-insert cleanup falsely completed new expired observations", cleanup)
	}
	if err := s.RequestCleanup(); err != nil {
		t.Fatal(err)
	}
	cleanup := waitCleanupState(t, s, func(c CleanupStatus) bool { return !c.Pending && !c.Running && c.LastCompletedAt != nil })
	rows, err = s.Events(Query{ID: incoming.ID})
	if err != nil || len(rows) != 0 || cleanup.Removed.Events != 1 {
		t.Fatal("subsequent ordinary retention did not remove the expired fact", rows, cleanup, err)
	}
}

func TestArchiveInsertionFailureRollsBackCleanupProgress(t *testing.T) {
	s := activityOpen(t)
	seedArchiveRows(t, s, 1, time.Now().Add(-40*24*time.Hour))
	if _, err := s.db.Exec("CREATE TRIGGER reject_new BEFORE INSERT ON radio_events BEGIN SELECT RAISE(ABORT,'injected archive failure'); END"); err != nil {
		t.Fatal(err)
	}
	if err := s.Append(context.Background(), coalescedEvents("new", 2)...); err == nil {
		t.Fatal("failed shared transaction acknowledged incoming records")
	}
	rows, err := s.Events(Query{})
	cleanup := s.CleanupStatus()
	if err != nil || len(rows) != 1 || rows[0].ID != "old-000" || cleanup.Removed.Events != 0 || cleanup.LastCompletedAt != nil || !cleanup.Pending || cleanup.LastError == "" {
		t.Fatal("rolled-back cleanup exposed deletion progress or completion", rows, cleanup, err)
	}
	if health := s.IngestionHealth(); health.WriteFailures != 2 || health.MaintenanceFailures != 1 {
		t.Fatal("shared failed participants were counted incorrectly", health)
	}
	if _, err := s.db.Exec("DROP TRIGGER reject_new"); err != nil {
		t.Fatal(err)
	}
	if err := s.Append(context.Background(), coalescedEvents("new", 2)...); err != nil {
		t.Fatal(err)
	}
	rows, err = s.Events(Query{})
	if err != nil || len(rows) != 2 || s.CleanupStatus().Removed.Events != 1 {
		t.Fatal("ordinary retry did not commit cleanup and the whole incoming batch", rows, s.CleanupStatus(), err)
	}
}

func TestArchiveCleanupFailureCannotAcknowledgeIncomingBatch(t *testing.T) {
	s := activityOpen(t)
	seedArchiveRows(t, s, 1, time.Now().Add(-40*24*time.Hour))
	if _, err := s.db.Exec("CREATE TRIGGER reject_cleanup BEFORE DELETE ON radio_events BEGIN SELECT RAISE(ABORT,'injected cleanup failure'); END"); err != nil {
		t.Fatal(err)
	}
	if err := s.Append(context.Background(), coalescedEvents("new", EventBatchLimit)...); err == nil {
		t.Fatal("failed cleanup falsely acknowledged the incoming batch")
	}
	rows, err := s.Events(Query{})
	if err != nil || len(rows) != 1 || rows[0].ID != "old-000" || s.CleanupStatus().Removed.Events != 0 {
		t.Fatal("failed shared transaction partially committed", rows, s.CleanupStatus(), err)
	}
}
