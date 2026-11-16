package activity

import (
	"context"
	"database/sql/driver"
	"errors"
	"fmt"
	"path/filepath"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"modernc.org/sqlite"
	"open25.local/open25-fne/internal/historypriority"
)

type coalescingFixture struct {
	s       *Store
	turns   atomic.Int32
	block   atomic.Bool
	entered chan struct{}
	allow   chan struct{}
	once    sync.Once
}

func newCoalescingFixture(t *testing.T) *coalescingFixture {
	t.Helper()
	f := &coalescingFixture{entered: make(chan struct{}), allow: make(chan struct{})}
	var err error
	f.s, err = open(filepath.Join(t.TempDir(), "activity.db"), false, func(ctx context.Context) (*historypriority.Turn, error) {
		f.turns.Add(1)
		if f.block.CompareAndSwap(true, false) {
			close(f.entered)
			select {
			case <-f.allow:
			case <-ctx.Done():
				return nil, ctx.Err()
			}
		}
		return &historypriority.Turn{}, ctx.Err()
	})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { f.release(); _ = f.s.Close() })
	waitActivityReady(t, f.s)
	return f
}

func (f *coalescingFixture) release() { f.once.Do(func() { close(f.allow) }) }

func (f *coalescingFixture) wait(t *testing.T) {
	t.Helper()
	select {
	case <-f.entered:
	case <-time.After(2 * time.Second):
		t.Fatal("writer did not reach its next scheduling admission")
	}
}

func waitCoalescedJob(t *testing.T, queued func() bool) {
	t.Helper()
	deadline := time.Now().Add(2 * time.Second)
	for !queued() {
		if time.Now().After(deadline) {
			t.Fatal("second participant did not enter its bounded queue")
		}
		time.Sleep(time.Millisecond)
	}
}

func coalescedResult(t *testing.T, result <-chan error) error {
	t.Helper()
	select {
	case err := <-result:
		return err
	case <-time.After(2 * time.Second):
		t.Fatal("durable participant did not finish")
		return nil
	}
}

func coalescedEvents(prefix string, count int) []Event {
	events := make([]Event, count)
	for i := range events {
		events[i] = Event{ID: fmt.Sprintf("%s-%02d", prefix, i), Origin: "site-1", Type: prefix}
	}
	return events
}

func seedOutbox(t *testing.T, s *Store, events []Event) {
	t.Helper()
	for _, e := range events {
		if !s.Enqueue(e) {
			t.Fatal("source capture rejected")
		}
	}
	waitActivityReady(t, s)
}

func TestOutboxCaptureAndAcknowledgmentShareOneDurableTurn(t *testing.T) {
	for _, first := range []string{"capture", "ack"} {
		t.Run(first, func(t *testing.T) {
			f := newCoalescingFixture(t)
			old := coalescedEvents("archived", EventBatchLimit)
			seedOutbox(t, f.s, old)
			ids := make([]string, len(old))
			for i, e := range old {
				ids[i] = e.ID
			}
			reader, err := f.s.readDB.Conn(context.Background())
			if err != nil {
				t.Fatal(err)
			}
			defer reader.Close()
			var before, after int
			if err = reader.QueryRowContext(context.Background(), "PRAGMA data_version").Scan(&before); err != nil {
				t.Fatal(err)
			}
			turns := f.turns.Load()
			incoming := coalescedEvents("new", EventBatchLimit)
			result := make(chan error, 1)
			f.block.Store(true)
			if first == "capture" {
				if !f.s.Enqueue(incoming[0]) {
					t.Fatal("capture rejected")
				}
				f.wait(t)
				for _, e := range incoming[1:] {
					if !f.s.Enqueue(e) {
						t.Fatal("capture rejected")
					}
				}
				go func() { result <- f.s.Acknowledge(context.Background(), ids) }()
				waitCoalescedJob(t, func() bool { return len(f.s.commands) == 1 })
			} else {
				go func() { result <- f.s.Acknowledge(context.Background(), ids) }()
				f.wait(t)
				for _, e := range incoming {
					if !f.s.Enqueue(e) {
						t.Fatal("capture rejected")
					}
				}
			}
			f.release()
			if err = coalescedResult(t, result); err != nil {
				t.Fatal(err)
			}
			if err = reader.QueryRowContext(context.Background(), "PRAGMA data_version").Scan(&after); err != nil {
				t.Fatal(err)
			}
			var count, oldCount int
			if err = reader.QueryRowContext(context.Background(), "SELECT COUNT(*),COUNT(CASE WHEN event_id LIKE 'archived-%' THEN 1 END) FROM outbox").Scan(&count, &oldCount); err != nil {
				t.Fatal(err)
			}
			if f.turns.Load() != turns+1 || after != before+1 || count != EventBatchLimit || oldCount != 0 || f.s.IngestionHealth().OutboxPending != EventBatchLimit {
				t.Fatalf("capture/ACK did not share one durable commit: turns=%d/%d versions=%d/%d count=%d old=%d health=%+v", f.turns.Load(), turns, after, before, count, oldCount, f.s.IngestionHealth())
			}
		})
	}
}

func TestOutboxSQLFailuresRollbackEveryParticipant(t *testing.T) {
	for _, failure := range []string{"ack", "insert", "commit"} {
		t.Run(failure, func(t *testing.T) {
			f := newCoalescingFixture(t)
			seedOutbox(t, f.s, coalescedEvents("retained", 1))
			statement := map[string]string{
				"ack":    `CREATE TRIGGER reject_ack BEFORE DELETE ON outbox BEGIN SELECT RAISE(ABORT,'injected ACK failure'); END`,
				"insert": `CREATE TRIGGER reject_insert BEFORE INSERT ON outbox WHEN NEW.event_id='new-01' BEGIN SELECT RAISE(ABORT,'injected insert failure'); END`,
				"commit": `PRAGMA foreign_keys=ON; CREATE TABLE required_parent(id INTEGER PRIMARY KEY); CREATE TABLE deferred_failure(id INTEGER REFERENCES required_parent(id) DEFERRABLE INITIALLY DEFERRED); CREATE TRIGGER reject_commit AFTER INSERT ON outbox BEGIN INSERT INTO deferred_failure VALUES(99); END`,
			}[failure]
			if _, err := f.s.db.Exec(statement); err != nil {
				t.Fatal(err)
			}
			incoming := coalescedEvents("new", 2)
			result := make(chan error, 1)
			f.block.Store(true)
			if !f.s.Enqueue(incoming[0]) {
				t.Fatal("capture rejected")
			}
			f.wait(t)
			if !f.s.Enqueue(incoming[1]) {
				t.Fatal("capture rejected")
			}
			go func() { result <- f.s.Acknowledge(context.Background(), []string{"retained-00"}) }()
			waitCoalescedJob(t, func() bool { return len(f.s.commands) == 1 })
			f.release()
			if coalescedResult(t, result) == nil {
				t.Fatal("failed shared turn falsely acknowledged")
			}
			pending, err := f.s.Pending(context.Background(), EventBatchLimit)
			if err != nil || len(pending) != 1 || pending[0].ID != "retained-00" {
				t.Fatal("partial capture/ACK survived rollback", pending, err)
			}
			health := f.s.IngestionHealth()
			if health.WriteFailures != 2 || health.MaintenanceFailures != 1 || health.RejectedObservations != 0 || health.OutboxPending != 1 {
				t.Fatalf("failed participants were not counted exactly once: %+v", health)
			}
		})
	}
}

func TestCanceledPiggybackACKDoesNotDiscardCapture(t *testing.T) {
	f := newCoalescingFixture(t)
	seedOutbox(t, f.s, coalescedEvents("retained", 1))
	f.block.Store(true)
	if !f.s.Enqueue(coalescedEvents("new", 1)[0]) {
		t.Fatal("capture rejected")
	}
	f.wait(t)
	ctx, cancel := context.WithCancel(context.Background())
	result := make(chan error, 1)
	go func() { result <- f.s.Acknowledge(ctx, []string{"retained-00"}) }()
	waitCoalescedJob(t, func() bool { return len(f.s.commands) == 1 })
	cancel()
	if err := coalescedResult(t, result); !errors.Is(err, context.Canceled) {
		t.Fatal(err)
	}
	f.release()
	waitActivityReady(t, f.s)
	pending, err := f.s.Pending(context.Background(), EventBatchLimit)
	if err != nil || len(pending) != 2 || pending[0].ID != "retained-00" || f.s.IngestionHealth().Degraded {
		t.Fatal("queued cancellation discarded captures or changed ACKs", pending, err, f.s.IngestionHealth())
	}
}

func TestOutboxValidationFailureNeverStartsSQL(t *testing.T) {
	f := newCoalescingFixture(t)
	seedOutbox(t, f.s, coalescedEvents("retained", 1))
	reader, err := f.s.readDB.Conn(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	defer reader.Close()
	var before, after int
	if err = reader.QueryRowContext(context.Background(), "PRAGMA data_version").Scan(&before); err != nil {
		t.Fatal(err)
	}
	incoming := coalescedEvents("invalid", 2)
	incoming[1].Origin = ""
	result := make(chan error, 1)
	f.block.Store(true)
	if !f.s.Enqueue(incoming[0]) {
		t.Fatal("capture rejected")
	}
	f.wait(t)
	if !f.s.Enqueue(incoming[1]) {
		t.Fatal("capture rejected")
	}
	go func() { result <- f.s.Acknowledge(context.Background(), []string{"retained-00"}) }()
	waitCoalescedJob(t, func() bool { return len(f.s.commands) == 1 })
	f.release()
	if coalescedResult(t, result) == nil {
		t.Fatal("invalid shared turn acknowledged")
	}
	if err = reader.QueryRowContext(context.Background(), "PRAGMA data_version").Scan(&after); err != nil || before != after {
		t.Fatal("validation changed durable data", before, after, err)
	}
	h := f.s.IngestionHealth()
	if h.RejectedObservations != 2 || h.WriteFailures != 0 || h.MaintenanceFailures != 0 || h.OutboxPending != 1 {
		t.Fatalf("rejection misclassified: %+v", h)
	}
}

func TestAcknowledgmentLimitRejectsBeforeScheduling(t *testing.T) {
	f := newCoalescingFixture(t)
	turns := f.turns.Load()
	if err := f.s.Acknowledge(context.Background(), make([]string, EventBatchLimit+1)); err == nil {
		t.Fatal("oversized ACK accepted")
	}
	if f.turns.Load() != turns || len(f.s.commands) != 0 || f.s.IngestionHealth().Degraded {
		t.Fatal("oversized ACK entered SQL scheduling")
	}
}

func TestAdmittedCancellationDoesNotAbortOutboxTransaction(t *testing.T) {
	enteredSQL, allowSQL := make(chan struct{}), make(chan struct{})
	var enteredOnce, allowOnce sync.Once
	defer allowOnce.Do(func() { close(allowSQL) })
	function := "outbox_sql_entry_" + NewID()
	if err := sqlite.RegisterScalarFunction(function, 0, func(_ *sqlite.FunctionContext, _ []driver.Value) (driver.Value, error) {
		enteredOnce.Do(func() { close(enteredSQL) })
		<-allowSQL
		return int64(0), nil
	}); err != nil {
		t.Fatal(err)
	}
	f := newCoalescingFixture(t)
	seedOutbox(t, f.s, coalescedEvents("retained", 1))
	if _, err := f.s.db.Exec("CREATE TRIGGER pause_ack BEFORE DELETE ON outbox BEGIN SELECT " + function + "(); END"); err != nil {
		t.Fatal(err)
	}
	f.block.Store(true)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	result := make(chan error, 1)
	go func() { result <- f.s.Acknowledge(ctx, []string{"retained-00"}) }()
	f.wait(t)
	for _, e := range coalescedEvents("new", 2) {
		if !f.s.Enqueue(e) {
			t.Fatal("capture rejected")
		}
	}
	f.release()
	select {
	case <-enteredSQL:
	case <-time.After(2 * time.Second):
		t.Fatal("transaction never entered SQL")
	}
	cancel()
	if !errors.Is(coalescedResult(t, result), context.Canceled) {
		t.Fatal("abandoned request did not stop waiting")
	}
	allowOnce.Do(func() { close(allowSQL) })
	waitActivityReady(t, f.s)
	pending, err := f.s.Pending(context.Background(), EventBatchLimit)
	if err != nil || len(pending) != 2 || pending[0].ID != "new-00" || f.s.IngestionHealth().OutboxPending != 2 {
		t.Fatal("caller cancellation poisoned durable source work", pending, err)
	}
}

func TestQueuedOutboxACKCancellationBeforeSQLPreservesDurableRows(t *testing.T) {
	f := newCoalescingFixture(t)
	seedOutbox(t, f.s, coalescedEvents("retained", 1))
	connection, err := f.s.db.Conn(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	defer connection.Close()
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Millisecond)
	defer cancel()
	if err := f.s.Acknowledge(ctx, []string{"retained-00"}); !errors.Is(err, context.DeadlineExceeded) {
		t.Fatal("queued ACK did not cancel", err)
	}
	if err := connection.Close(); err != nil {
		t.Fatal(err)
	}
	waitActivityReady(t, f.s)
	pending, err := f.s.Pending(context.Background(), EventBatchLimit)
	if err != nil || len(pending) != 1 || f.s.IngestionHealth().Degraded {
		t.Fatal("pre-SQL cancellation deleted durable rows or claimed failed storage", pending, err)
	}
}

func TestAdmittedSystemAppendCommitsAfterLostResponseAndDeduplicatesRetry(t *testing.T) {
	enteredSQL, allowSQL := make(chan struct{}), make(chan struct{})
	var enteredOnce, allowOnce sync.Once
	defer allowOnce.Do(func() { close(allowSQL) })
	function := "archive_sql_entry_" + NewID()
	if err := sqlite.RegisterScalarFunction(function, 0, func(_ *sqlite.FunctionContext, _ []driver.Value) (driver.Value, error) {
		enteredOnce.Do(func() { close(enteredSQL) })
		<-allowSQL
		return int64(0), nil
	}); err != nil {
		t.Fatal(err)
	}
	s := activityOpen(t)
	if _, err := s.db.Exec("CREATE TRIGGER pause_archive BEFORE INSERT ON radio_events BEGIN SELECT " + function + "(); END"); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	batch := coalescedEvents("durable-archive", EventBatchLimit)
	for i := range batch {
		batch[i].At = time.Now().UTC().Add(-time.Duration(i) * time.Minute)
	}
	result := make(chan error, 1)
	go func() { result <- s.Append(ctx, batch...) }()
	select {
	case <-enteredSQL:
	case <-time.After(2 * time.Second):
		t.Fatal("System transaction never entered SQL")
	}
	cancel()
	if !errors.Is(coalescedResult(t, result), context.Canceled) {
		t.Fatal("abandoned System request did not stop waiting")
	}
	allowOnce.Do(func() { close(allowSQL) })
	waitActivityReady(t, s)
	if err := s.Append(context.Background(), batch...); err != nil {
		t.Fatal(err)
	}
	rows, err := s.Events(Query{Limit: 100})
	if err != nil || len(rows) != EventBatchLimit {
		t.Fatal("System lost admitted observations or duplicated a retry", rows, err)
	}
	original := map[string]time.Time{}
	for _, e := range batch {
		original[e.ID] = e.At
	}
	for _, e := range rows {
		if !original[e.ID].Equal(e.At) {
			t.Fatal("System replaced the producer identity/time", e)
		}
	}
}
