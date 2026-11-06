package activity

import (
	"context"
	"encoding/json"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestIngestionReportsLossWithoutWaitingForBlockedStorage(t *testing.T) {
	s := activityOpen(t)
	connection, err := s.db.Conn(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	defer connection.Close()
	result := make(chan IngestionHealth, 1)
	go func() {
		for n := 0; n < cap(s.queue)+20; n++ {
			s.Enqueue(event(NewID(), "status", "site-1", time.Now()))
		}
		result <- s.IngestionHealth()
	}()
	var health IngestionHealth
	select {
	case health = <-result:
	case <-time.After(250 * time.Millisecond):
		t.Fatal("activity ingestion or health waited for the held database connection")
	}
	if health.QueueDropped == 0 || health.QueueCapacity != 1024 || health.QueueDepth > health.QueueCapacity || !health.Degraded || health.StartedAt.IsZero() || health.WriteFailures != 0 {
		t.Fatalf("queue loss was hidden or misreported: %+v", health)
	}
	if err := connection.Close(); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	if err := s.Flush(ctx); err != nil {
		t.Fatal(err)
	}
	after := s.IngestionHealth()
	if after.QueueDropped != health.QueueDropped || after.LastWriteSuccessAt == nil || !after.Degraded {
		t.Fatal("successful later writes hid records already lost", after)
	}
}

func TestIngestionSeparatesRejectedRecordsFailedWritesAndRecovery(t *testing.T) {
	s := activityOpen(t)
	if err := s.Append(Event{Type: "status"}); err == nil {
		t.Fatal("invalid observation was accepted")
	}
	if _, err := s.db.Exec(`CREATE TRIGGER reject_activity BEFORE INSERT ON radio_events BEGIN SELECT RAISE(ABORT,'private database detail'); END`); err != nil {
		t.Fatal(err)
	}
	if !s.Enqueue(event("storage-error", "status", "site-1", time.Now())) {
		t.Fatal("empty queue rejected a record")
	}
	if err := s.Flush(context.Background()); err != nil {
		t.Fatal(err)
	}
	health := s.IngestionHealth()
	if health.RejectedObservations != 1 || health.WriteFailures != 1 || health.LastWriteFailureAt == nil || health.LastWriteSuccessAt != nil || !health.Degraded {
		t.Fatalf("invalid input and storage failure were not distinguished: %+v", health)
	}
	raw, _ := json.Marshal(health)
	for _, value := range []string{"private", "database detail", "storage-error", "status", "site-1"} {
		if strings.Contains(string(raw), value) {
			t.Fatal("health retained event or database error details", string(raw))
		}
	}
	if _, err := s.db.Exec("DROP TRIGGER reject_activity"); err != nil {
		t.Fatal(err)
	}
	if err := s.Append(event("recovered", "status", "site-1", time.Now())); err != nil {
		t.Fatal(err)
	}
	after := s.IngestionHealth()
	if after.LastWriteSuccessAt == nil || !after.LastWriteSuccessAt.After(*health.LastWriteFailureAt) || after.WriteFailures != 1 || after.RejectedObservations != 1 || !after.Degraded {
		t.Fatal("write recovery erased the earlier loss or omitted successful write time", after)
	}
}

func TestIngestionMaintenanceFailureDoesNotClaimCommittedEventWasLost(t *testing.T) {
	s := activityOpen(t)
	if _, err := s.db.Exec(`CREATE TRIGGER reject_pruning BEFORE DELETE ON radio_events BEGIN SELECT RAISE(ABORT,'pruning denied'); END`); err != nil {
		t.Fatal(err)
	}
	s.pruneMu.Lock()
	s.appends = 255
	s.pruneMu.Unlock()
	if err := s.Append(event("committed-old-record", "status", "site-1", time.Now().Add(-90*24*time.Hour))); err == nil {
		t.Fatal("expected failed automatic retention cleanup")
	}
	rows, err := s.Events(Query{ID: "committed-old-record"})
	if err != nil || len(rows) != 1 {
		t.Fatal("the committed observation was not preserved", err)
	}
	health := s.IngestionHealth()
	if health.WriteFailures != 0 || health.MaintenanceFailures != 1 || health.LastMaintenanceFailureAt == nil || health.LastWriteSuccessAt == nil || !health.Degraded {
		t.Fatalf("retention failure was hidden or counted as lost committed write: %+v", health)
	}
}

func TestIngestionCountersDescribeOnlyCurrentServiceLifetime(t *testing.T) {
	path := filepath.Join(t.TempDir(), "activity.db")
	s, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	_ = s.Append(Event{Type: "invalid"})
	if err := s.Append(event("retained", "status", "site-1", time.Now())); err != nil {
		t.Fatal(err)
	}
	first := s.IngestionHealth()
	if err := s.Close(); err != nil {
		t.Fatal(err)
	}
	s, err = Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	health := s.IngestionHealth()
	rows, err := s.Events(Query{})
	if err != nil || len(rows) != 1 || !health.StartedAt.After(first.StartedAt) || health.RejectedObservations != 0 || health.LastWriteSuccessAt != nil || health.Degraded {
		t.Fatal("restarted diagnostics claimed prior-process counters or lost durable records", health, err)
	}
}
