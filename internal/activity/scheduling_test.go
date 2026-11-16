package activity

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"open25.local/open25-fne/internal/historypriority"
)

func TestHistoryGateRefusalDoesNotInitializeSQLOrClaimReadiness(t *testing.T) {
	path := filepath.Join(t.TempDir(), "never-created.db")
	refused := errors.New("injected private scheduling refusal")
	store, err := open(path, true, func(ctx context.Context) (*historypriority.Turn, error) {
		return nil, refused
	})
	if store == nil || err != nil {
		t.Fatalf("asynchronous initialization failed its constructor: %v %v", store, err)
	}
	defer store.Close()
	if err = store.Flush(context.Background()); !errors.Is(err, ErrUnavailable) {
		t.Fatalf("refused initialization falsely flushed: %v", err)
	}
	if err = store.InitializationError(); !errors.Is(err, ErrUnavailable) || store.IngestionHealth().OpenFailures != 1 {
		t.Fatal("refused initialization was not visible in Store-owned health", err)
	}
	if _, err = os.Stat(path); !os.IsNotExist(err) {
		t.Fatalf("refused scheduling still initialized SQLite: %v", err)
	}
}

func TestPendingInitializationQueuesCapturesAndReadsFailFastUntilReady(t *testing.T) {
	for _, archive := range []bool{false, true} {
		t.Run(fmt.Sprint(archive), func(t *testing.T) {
			entered, allow := make(chan struct{}), make(chan struct{})
			var calls atomic.Int32
			s, err := open(filepath.Join(t.TempDir(), "activity.db"), archive, func(ctx context.Context) (*historypriority.Turn, error) {
				if calls.Add(1) == 1 {
					close(entered)
					select {
					case <-allow:
					case <-ctx.Done():
						return nil, ctx.Err()
					}
				}
				return &historypriority.Turn{}, ctx.Err()
			})
			if err != nil {
				t.Fatal(err)
			}
			defer s.Close()
			<-entered
			for i := range 8 {
				if !s.Enqueue(Event{ID: fmt.Sprintf("pending-%d", i), Origin: "site-1", Type: "status"}) {
					t.Fatal("pending initialization rejected a bounded capture")
				}
			}
			if _, err = s.Pending(context.Background(), EventBatchLimit); !errors.Is(err, ErrInitializing) {
				t.Fatal("pending read did not fail fast with explicit state", err)
			}
			if health := s.IngestionHealth(); !health.StorageInitializing || health.StorageUnavailable || health.QueueDropped != 0 || health.OpenFailures != 0 {
				t.Fatalf("pending initialization was misreported: %+v", health)
			}
			close(allow)
			waitActivityReady(t, s)
			rows, err := s.Pending(context.Background(), EventBatchLimit)
			if archive {
				rows, err = s.Events(Query{})
			}
			if err != nil || len(rows) != 8 || s.InitializationError() != nil {
				t.Fatalf("pending captures did not survive initialization: rows=%d error=%v", len(rows), err)
			}
		})
	}

}

func TestCloseCancelsPendingInitializationAndAccountsUndurableCaptures(t *testing.T) {
	for _, archive := range []bool{false, true} {
		t.Run(fmt.Sprint(archive), func(t *testing.T) {
			path := filepath.Join(t.TempDir(), "never-opened.db")
			entered := make(chan struct{})
			s, err := open(path, archive, func(ctx context.Context) (*historypriority.Turn, error) {
				close(entered)
				<-ctx.Done()
				return nil, ctx.Err()
			})
			if err != nil {
				t.Fatal(err)
			}
			<-entered
			for i := range 8 {
				if !s.Enqueue(Event{ID: fmt.Sprintf("undurable-%d", i), Origin: "site-1", Type: "status"}) {
					t.Fatal("pending capture was not admitted")
				}
			}
			done := make(chan error, 1)
			go func() { done <- s.Close() }()
			select {
			case err = <-done:
				if err != nil {
					t.Fatal(err)
				}
			case <-time.After(time.Second):
				t.Fatal("Close could not cancel pending storage admission")
			}
			if health := s.IngestionHealth(); health.QueueDropped != 8 || health.OpenFailures != 0 || !health.StorageUnavailable {
				t.Fatalf("pending cancellation lost observations without accounting: %+v", health)
			}
			if _, err = os.Stat(path); !os.IsNotExist(err) {
				t.Fatal("cancelled initialization still opened SQLite", err)
			}
		})
	}

}

func TestAppendRejectsOversizedCurrentBatchBeforeQueueOrSQL(t *testing.T) {
	var calls atomic.Int32
	s, err := open(filepath.Join(t.TempDir(), "activity.db"), true, func(ctx context.Context) (*historypriority.Turn, error) {
		calls.Add(1)
		return &historypriority.Turn{}, ctx.Err()
	})
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	waitActivityReady(t, s)
	events := make([]Event, EventBatchLimit+1)
	for i := range events {
		events[i] = Event{ID: fmt.Sprintf("oversized-%d", i), Origin: "site-1", Type: "status"}
	}
	if err = s.Append(context.Background(), events...); err == nil {
		t.Fatal("oversized batch was accepted")
	}
	if calls.Load() != 1 {
		t.Fatal("oversized batch entered storage scheduling")
	}
	rows, err := s.Events(Query{Limit: 100})
	if err != nil || len(rows) != 0 {
		t.Fatalf("oversized batch was partially committed: %d %v", len(rows), err)
	}
	if health := s.IngestionHealth(); health.RejectedObservations != uint64(len(events)) || health.WriteFailures != 0 || health.MaintenanceFailures != 0 {
		t.Fatalf("oversized batch had the wrong failure classification: %+v", health)
	}
}

func TestHistoryGateCancellationBeforeSQLCannotCommitOrAcknowledge(t *testing.T) {
	var calls atomic.Int32
	var block atomic.Bool
	block.Store(true)
	waiting := make(chan struct{})
	var announced sync.Once
	s, err := open(filepath.Join(t.TempDir(), "activity.db"), true, func(ctx context.Context) (*historypriority.Turn, error) {
		if calls.Add(1) > 1 && block.Load() {
			announced.Do(func() { close(waiting) })
			<-ctx.Done()
			return nil, ctx.Err()
		}
		return &historypriority.Turn{}, nil
	})
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	ctx, cancel := context.WithTimeout(context.Background(), 400*time.Millisecond)
	defer cancel()
	err = s.Append(ctx, Event{ID: "cancelled", Origin: "site-1", Type: "status"})
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("cancelled upload claimed success: %v", err)
	}
	<-waiting
	block.Store(false)
	if err = s.Append(context.Background(), Event{ID: "committed", Origin: "site-1", Type: "status"}); err != nil {
		t.Fatal(err)
	}
	rows, err := s.Events(Query{Limit: 10})
	if err != nil || len(rows) != 1 || rows[0].ID != "committed" {
		t.Fatalf("cancelled turn touched SQLite or hid committed result: %+v %v", rows, err)
	}
	if health := s.IngestionHealth(); health.WriteFailures != 0 || health.MaintenanceFailures != 0 {
		t.Fatalf("caller cancellation became a storage failure: %+v", health)
	}
}

func TestHistoryGateFailureDegradesInboundWithoutFalseACK(t *testing.T) {
	var refuse atomic.Bool
	refused := errors.New("injected gate refusal")
	s, err := open(filepath.Join(t.TempDir(), "activity.db"), true, func(ctx context.Context) (*historypriority.Turn, error) {
		if refuse.Load() {
			return nil, refused
		}
		return &historypriority.Turn{}, ctx.Err()
	})
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	waitActivityReady(t, s)
	refuse.Store(true)
	err = s.Append(context.Background(), Event{ID: "unacknowledged", Origin: "site-1", Type: "status"})
	refuse.Store(false)
	if !errors.Is(err, refused) {
		t.Fatalf("refused upload was acknowledged: %v", err)
	}
	if health := s.IngestionHealth(); !health.Degraded || health.MaintenanceFailures != 1 || health.WriteFailures != 0 {
		t.Fatalf("gate refusal was hidden or double-counted: %+v", health)
	}
	rows, err := s.Events(Query{Limit: 10})
	if err != nil || len(rows) != 0 {
		t.Fatalf("refused upload reached history: %+v %v", rows, err)
	}
}

func TestCaptureSQLFailureIsCountedOnceAfterScheduling(t *testing.T) {
	s, err := open(filepath.Join(t.TempDir(), "activity.db"), true, func(ctx context.Context) (*historypriority.Turn, error) {
		return &historypriority.Turn{}, ctx.Err()
	})
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	waitActivityReady(t, s)
	if _, err = s.db.Exec(`CREATE TRIGGER reject_capture BEFORE INSERT ON radio_events BEGIN SELECT RAISE(FAIL,'injected failure'); END`); err != nil {
		t.Fatal(err)
	}
	if !s.Enqueue(Event{ID: "failed", Origin: "site-1", Type: "status"}) {
		t.Fatal("capture was not queued")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	if err = s.Flush(ctx); err != nil {
		t.Fatal(err)
	}
	if health := s.IngestionHealth(); health.WriteFailures != 1 || health.RejectedObservations != 0 {
		t.Fatalf("one failed capture was counted more than once: %+v", health)
	}
}

func TestCloseDrainsAllAcceptedCapturesWhileCleanupContextIsCancelled(t *testing.T) {
	path := filepath.Join(t.TempDir(), "activity.db")
	var calls atomic.Int32
	blocked := make(chan struct{})
	allow := make(chan struct{})
	firstCommit := make(chan int, 1)
	var s *Store
	s, err := open(path, true, func(ctx context.Context) (*historypriority.Turn, error) {
		switch calls.Add(1) {
		case 2:
			close(blocked)
			select {
			case <-allow:
			case <-ctx.Done():
				return nil, ctx.Err()
			}
		case 3:
			var retained int
			if err := s.readDB.QueryRow("SELECT count(*) FROM radio_events").Scan(&retained); err != nil {
				return nil, err
			}
			firstCommit <- retained
		}
		return &historypriority.Turn{}, ctx.Err()
	})
	if err != nil {
		t.Fatal(err)
	}
	if !s.Enqueue(Event{ID: "accepted-first", Origin: "site-1", Type: "status"}) {
		t.Fatal("first capture was not queued")
	}
	<-blocked
	started := time.Now()
	for i := range 1023 {
		if !s.Enqueue(Event{ID: fmt.Sprintf("accepted-%04d", i), Origin: "site-1", Type: "status"}) {
			t.Fatalf("capture %d did not fit the unchanged bounded queue", i)
		}
	}
	if elapsed := time.Since(started); elapsed > 500*time.Millisecond {
		t.Fatalf("capture waited for storage scheduling: %v", elapsed)
	}
	done := make(chan error, 1)
	go func() { done <- s.Close() }()
	select {
	case err := <-done:
		t.Fatalf("Close discarded its accepted blocked capture: %v", err)
	case <-time.After(30 * time.Millisecond):
	}
	close(allow)
	select {
	case err = <-done:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(20 * time.Second):
		t.Fatal("accepted captures failed to drain within paced shutdown budget")
	}
	db, err := sql.Open("sqlite", path)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	var retained int
	if err = db.QueryRow("SELECT count(*) FROM radio_events").Scan(&retained); err != nil || retained != 1024 {
		t.Fatalf("Close lost accepted observations: retained=%d error=%v", retained, err)
	}
	if got := <-firstCommit; got != EventBatchLimit {
		t.Fatalf("observations accumulated during gate wait did not coalesce into the admitted turn: %d", got)
	}
	if health := s.IngestionHealth(); health.QueueDropped != 0 || health.WriteFailures != 0 || health.MaintenanceFailures != 0 {
		t.Fatalf("drain changed radio capture/failure semantics: %+v", health)
	}
}

func TestHistoryCloseRefusalSubprocess(t *testing.T) {
	path := os.Getenv("OPEN25_ACTIVITY_CLOSE_REFUSAL_PATH")
	if path == "" {
		return
	}
	var refuse atomic.Bool
	refused := errors.New("injected final-close scheduling refusal")
	s, err := open(path, true, func(ctx context.Context) (*historypriority.Turn, error) {
		if refuse.Load() {
			return nil, refused
		}
		return &historypriority.Turn{}, ctx.Err()
	})
	if err != nil {
		t.Fatal(err)
	}
	if err = s.Append(context.Background(), Event{ID: "durable-after-refusal", Origin: "site-1", Type: "status"}); err != nil {
		t.Fatal(err)
	}
	beforeWAL, err := os.Stat(path + "-wal")
	if err != nil || beforeWAL.Size() == 0 {
		t.Fatal("test needs a committed nonempty WAL", err)
	}
	beforeDB, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	refuse.Store(true)
	if err = s.Close(); !errors.Is(err, refused) {
		t.Fatalf("refused close was reported as successful: %v", err)
	}
	// No ungated fallback may close this handle or run its implicit checkpoint.
	// The subprocess deliberately exits with it open, matching controller exit.
	if err = s.db.Ping(); err != nil {
		t.Fatalf("refusal still closed the SQLite handle: %v", err)
	}
	afterWAL, err := os.Stat(path + "-wal")
	if err != nil || afterWAL.Size() != beforeWAL.Size() {
		t.Fatalf("refusal still checkpointed/truncated WAL: before=%d after=%v error=%v", beforeWAL.Size(), afterWAL, err)
	}
	afterDB, err := os.Stat(path)
	if err != nil || afterDB.Size() != beforeDB.Size() || !afterDB.ModTime().Equal(beforeDB.ModTime()) {
		t.Fatal("refusal still wrote the main database", err)
	}
}

func TestHistoryCloseRefusalPreservesDurableWALUntilProcessExit(t *testing.T) {
	path := filepath.Join(t.TempDir(), "activity.db")
	command := exec.Command(os.Args[0], "-test.run=^TestHistoryCloseRefusalSubprocess$", "-test.v=false")
	command.Env = append(os.Environ(), "OPEN25_ACTIVITY_CLOSE_REFUSAL_PATH="+path)
	if output, err := command.CombinedOutput(); err != nil {
		t.Fatalf("close-refusal subprocess: %v %s", err, output)
	}
	// After kernel descriptor retirement, a separate offline reader must recover
	// the already committed observation from the surviving WAL.
	u := url.URL{Scheme: "file", Path: path, RawQuery: "mode=ro"}
	db, err := sql.Open("sqlite", u.String())
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	var retained int
	if err = db.QueryRow("SELECT count(*) FROM radio_events WHERE id='durable-after-refusal'").Scan(&retained); err != nil || retained != 1 {
		t.Fatalf("process exit lost acknowledged WAL data: retained=%d error=%v", retained, err)
	}
}
