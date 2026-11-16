package activity

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"path/filepath"
	"reflect"
	"testing"
	"time"

	"open25.local/open25-fne/internal/historypriority"
)

func TestSourceOutboxHasOnlyItsCurrentSchemaAndNoHistoryAuthority(t *testing.T) {
	f := newCoalescingFixture(t)
	rows, err := f.s.db.Query("SELECT name FROM sqlite_schema WHERE type='table' ORDER BY name")
	if err != nil {
		t.Fatal(err)
	}
	var tables []string
	for rows.Next() {
		var name string
		if err := rows.Scan(&name); err != nil {
			t.Fatal(err)
		}
		tables = append(tables, name)
	}
	if err := rows.Close(); err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(tables, []string{"outbox"}) {
		t.Fatal("producer retained another history owner", tables)
	}
	turns := f.turns.Load()
	if err := f.s.Append(context.Background(), coalescedEvents("not-an-archive", 1)...); !errors.Is(err, ErrArchiveOnly) {
		t.Fatal("outbox falsely acknowledged an inbound archive batch", err)
	}
	if _, err := f.s.Events(Query{}); !errors.Is(err, ErrArchiveOnly) {
		t.Fatal("outbox served a partial history", err)
	}
	if _, err := f.s.CallHistory(Query{}); !errors.Is(err, ErrArchiveOnly) {
		t.Fatal(err)
	}
	if _, err := f.s.Retention(context.Background()); !errors.Is(err, ErrArchiveOnly) {
		t.Fatal(err)
	}
	if err := f.s.SaveRetention(context.Background(), Retention{}); !errors.Is(err, ErrArchiveOnly) {
		t.Fatal(err)
	}
	if err := f.s.RequestCleanup(); !errors.Is(err, ErrArchiveOnly) {
		t.Fatal(err)
	}
	if _, err := f.s.Prune(context.Background(), time.Now()); !errors.Is(err, ErrArchiveOnly) {
		t.Fatal(err)
	}
	if _, ok := f.s.RetentionPolicy(); ok || f.turns.Load() != turns || len(f.s.commands) != 0 {
		t.Fatal("source acquired policy or scheduled historical maintenance")
	}
}

func TestOutboxPreservesProducerFactsOrderingAndNoTimeExpiry(t *testing.T) {
	f := newCoalescingFixture(t)
	first := event("first", "call_start", "site-1", time.Date(2000, 1, 1, 0, 0, 0, 0, time.UTC))
	first.ReceivedAt = first.At.Add(time.Second)
	first.CallID, first.Details = "original-call", map[string]any{"allocation_id": "one", "payload": "original"}
	second := event("second", "location", "site-1", first.At.Add(-time.Hour))
	second.ReceivedAt = first.ReceivedAt.Add(time.Second)
	seedOutbox(t, f.s, []Event{first, second})
	mutated := first
	mutated.Message, mutated.At = "duplicate must not replace original", time.Now()
	seedOutbox(t, f.s, []Event{mutated})
	pending, err := f.s.Pending(context.Background(), EventBatchLimit)
	if err != nil || len(pending) != 2 || pending[0].ID != first.ID || pending[1].ID != second.ID {
		t.Fatal("producer enqueue ordering changed", pending, err)
	}
	if !pending[0].At.Equal(first.At) || !pending[0].ReceivedAt.Equal(first.ReceivedAt) || !reflect.DeepEqual(pending[0].Details, first.Details) || pending[0].Message != first.Message {
		t.Fatal("outbox rewrote original producer facts", pending[0])
	}
	health := f.s.IngestionHealth()
	if health.OutboxPending != 2 || health.OutboxCapacity != OutboxCapacity || health.OldestPendingAt == nil || !health.OldestPendingAt.Equal(first.ReceivedAt) {
		t.Fatal("outbox progress did not describe durable state", health)
	}
	// Status stays a memory observation even while its WAL reader is unavailable.
	reader, err := f.s.readDB.Conn(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	defer reader.Close()
	started := time.Now()
	for range 1000 {
		if f.s.IngestionHealth().OutboxPending != 2 {
			t.Fatal("cached outbox progress changed")
		}
	}
	if time.Since(started) > 250*time.Millisecond {
		t.Fatal("outbox status waited for SQL")
	}
	if err := f.s.Acknowledge(context.Background(), []string{first.ID}); err != nil {
		t.Fatal(err)
	}
	if health := f.s.IngestionHealth(); health.OutboxPending != 1 || health.OldestPendingAt == nil || !health.OldestPendingAt.Equal(second.ReceivedAt) {
		t.Fatal("ACK did not publish committed next outbox item", health)
	}
}

func TestOutboxCapacityPreservesAcceptedRowsAndReportsOnlyNewDrops(t *testing.T) {
	path := filepath.Join(t.TempDir(), "outbox.db")
	db, err := sql.Open("sqlite", path)
	if err != nil {
		t.Fatal(err)
	}
	// Offline fixture preparation tests the real 100000-record bound without
	// sending 100000 normal captures through paced production turns.
	payload, err := json.Marshal(Event{ID: "offline", At: time.Now().Add(-400 * 24 * time.Hour), ReceivedAt: time.Now().Add(-399 * 24 * time.Hour), Origin: "site-1", Type: "status"})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec(outboxSchema); err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec(`WITH RECURSIVE n(i) AS (VALUES(0) UNION ALL SELECT i+1 FROM n WHERE i<?) INSERT INTO outbox(event_id,payload) SELECT printf('durable-%06d',i),json_set(?,'$.id',printf('durable-%06d',i)) FROM n`, OutboxCapacity-1, string(payload)); err != nil {
		t.Fatal(err)
	}
	if err := db.Close(); err != nil {
		t.Fatal(err)
	}
	s, err := open(path, false, func(ctx context.Context) (*historypriority.Turn, error) { return &historypriority.Turn{}, ctx.Err() })
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	waitActivityReady(t, s)
	seedOutbox(t, s, []Event{{ID: "new-1", Origin: "site-1", Type: "status"}, {ID: "durable-000000", Origin: "site-1", Type: "status"}, {ID: "new-2", Origin: "site-1", Type: "status"}})
	h := s.IngestionHealth()
	if h.OutboxPending != OutboxCapacity || h.OutboxDropped != 2 || h.QueueDropped != 0 || h.WriteFailures != 0 || !h.Degraded {
		t.Fatalf("capacity erased durable facts or hid new drops: %+v", h)
	}
	pending, err := s.Pending(context.Background(), 1000)
	if err != nil || len(pending) != EventBatchLimit || pending[0].ID != "durable-000000" {
		t.Fatal("full outbox changed its retained first batch", pending, err)
	}
	if err := s.Acknowledge(context.Background(), []string{"durable-000000"}); err != nil {
		t.Fatal(err)
	}
	seedOutbox(t, s, []Event{{ID: "accepted-after-ack", Origin: "site-1", Type: "status"}})
	if h := s.IngestionHealth(); h.OutboxPending != OutboxCapacity || h.OutboxDropped != 2 {
		t.Fatal("durable ACK did not free capacity for new observations", h)
	}
	var count int
	if err := s.db.QueryRow("SELECT COUNT(*) FROM outbox WHERE event_id='accepted-after-ack'").Scan(&count); err != nil || count != 1 {
		t.Fatal("freed capacity was not usable", count, err)
	}
}

func TestSystemArchiveSchemaAndRetentionSnapshotHaveOneOwner(t *testing.T) {
	s := activityOpen(t)
	for _, absent := range []string{"outbox", "radio_presence", "event_pending", "event_time", "allocations_call"} {
		var count int
		if err := s.db.QueryRow("SELECT COUNT(*) FROM sqlite_schema WHERE name=?", absent).Scan(&count); err != nil || count != 0 {
			t.Fatal("archive retained an obsolete source feature", absent, count, err)
		}
	}
	for _, column := range []struct{ table, name string }{{"radio_events", "uploaded"}, {"history_settings", "pruned_unuploaded"}} {
		var count int
		if err := s.db.QueryRow("SELECT COUNT(*) FROM pragma_table_info(?) WHERE name=?", column.table, column.name).Scan(&count); err != nil || count != 0 {
			t.Fatal("archive retained obsolete replication bookkeeping", column, count, err)
		}
	}
	if policy, ok := s.RetentionPolicy(); !ok || policy != defaultRetention {
		t.Fatal("System policy snapshot did not reflect committed initialization", policy, ok)
	}
	entered := Retention{Days: 6, MaxEvents: 151, MaxCalls: 41, MaxLocations: 29, StaleSeconds: 17}
	if err := s.SaveRetention(context.Background(), entered); err != nil {
		t.Fatal(err)
	}
	reader, err := s.readDB.Conn(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	defer reader.Close()
	if policy, ok := s.RetentionPolicy(); !ok || policy != entered {
		t.Fatal("catalog policy getter waited for SQL or changed committed policy", policy, ok)
	}
	if _, err := s.db.Exec("CREATE TRIGGER reject_policy BEFORE UPDATE ON history_settings BEGIN SELECT RAISE(ABORT,'injected failed policy'); END"); err != nil {
		t.Fatal(err)
	}
	if err := s.SaveRetention(context.Background(), Retention{Days: 99}); err == nil {
		t.Fatal("injected policy failure succeeded")
	}
	if policy, ok := s.RetentionPolicy(); !ok || policy != entered {
		t.Fatal("failed policy write replaced the committed catalog snapshot", policy, ok)
	}
	if err := s.Acknowledge(context.Background(), []string{"anything"}); err == nil {
		t.Fatal("System archive falsely accepted a source ACK")
	}
}
