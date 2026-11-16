package activity

import (
	"context"
	"errors"
	"time"
)

// CleanupStatus describes work owned by the history writer, not RF readiness.
// Progress includes only committed deletions from the current cleanup run.
type CleanupStatus struct {
	Pending         bool       `json:"pending"`
	Running         bool       `json:"running"`
	LastStartedAt   *time.Time `json:"last_started_at,omitempty"`
	LastCompletedAt *time.Time `json:"last_completed_at,omitempty"`
	LastFailureAt   *time.Time `json:"last_failure_at,omitempty"`
	LastError       string     `json:"last_error,omitempty"`
	Removed         Removed    `json:"removed"`
}

func (s *Store) CleanupStatus() CleanupStatus {
	s.cleanupMu.Lock()
	defer s.cleanupMu.Unlock()
	v := s.cleanup
	copyTime := func(at *time.Time) *time.Time {
		if at == nil {
			return nil
		}
		value := *at
		return &value
	}
	v.LastStartedAt = copyTime(v.LastStartedAt)
	v.LastCompletedAt = copyTime(v.LastCompletedAt)
	v.LastFailureAt = copyTime(v.LastFailureAt)
	return v
}

// RequestCleanup coalesces work into one bounded signal. Acceptance means queued,
// never completed; neither a database connection nor a new goroutine is needed.
func (s *Store) RequestCleanup() error {
	if !s.archive {
		return ErrArchiveOnly
	}
	s.mu.RLock()
	defer s.mu.RUnlock()
	if s.closed {
		return errors.New("activity store is closed")
	}
	if s.initErr != nil {
		return ErrUnavailable
	}
	s.requestCleanup()
	select {
	case s.cleanupWake <- struct{}{}:
	default:
	}
	return nil
}

func (s *Store) requestCleanup() {
	s.cleanupMu.Lock()
	defer s.cleanupMu.Unlock()
	if !s.cleanup.Pending && !s.cleanup.Running {
		s.cleanup.Removed = Removed{}
	}
	s.cleanupRequests++
	s.cleanup.Pending = true
}

func (s *Store) cleanupBatch(now time.Time) (more bool, wrote bool) {
	generation := s.beginCleanup(now)
	removed, err := s.pruneStep(s.workerCtx, now)
	return s.finishCleanup(generation, removed, err)
}

func (s *Store) beginCleanup(now time.Time) uint64 {
	// A periodic timer may race an already queued signal. Consume that signal
	// before capturing the generation covered by this batch.
	select {
	case <-s.cleanupWake:
	default:
	}
	s.cleanupMu.Lock()
	generation := s.cleanupRequests
	s.cleanup.Running = true
	s.cleanup.Pending = false
	s.cleanup.LastStartedAt = &now
	s.cleanupMu.Unlock()
	return generation
}

// finishCleanup publishes only after its owning transaction has committed or
// rolled back. Inline archive work and standalone jobs share this one status.
func (s *Store) finishCleanup(generation uint64, removed Removed, err error) (more bool, wrote bool) {
	finished := time.Now().UTC()
	s.cleanupMu.Lock()
	defer s.cleanupMu.Unlock()
	s.cleanup.Running = false
	if errors.Is(err, context.Canceled) && s.workerCtx.Err() != nil {
		s.cleanup.Pending = false
		return false, false
	}
	// Another process can hold a reserved writer lock while this deferred
	// transaction reads. A failed read-to-write upgrade is SQLite backpressure,
	// not failed retention; rollback and retry through the paced owner.
	if sqliteBusy(err) {
		s.cleanup.Pending = true
		return true, false
	}
	s.observeMaintenance(err)
	if err != nil {
		s.cleanup.Pending = true
		s.cleanup.LastFailureAt = &finished
		s.cleanup.LastError = "History cleanup failed; background retry is pending."
		return true, true
	}
	s.cleanup.Removed.Events += removed.Events
	s.cleanup.Removed.Calls += removed.Calls
	s.cleanup.Removed.Locations += removed.Locations
	more = removed.more || s.cleanupRequests != generation
	s.cleanup.Pending = more
	if !more {
		s.cleanup.LastCompletedAt = &finished
		s.cleanup.LastError = ""
	}
	return more, removed.Events+removed.Calls+removed.Locations > 0
}

func sqliteBusy(err error) bool {
	var code interface{ Code() int }
	if !errors.As(err, &code) {
		return false
	}
	// The low byte also covers extended BUSY/LOCKED codes.
	value := code.Code() & 0xff
	return value == 5 || value == 6
}

// command hands ordinary durable history mutations to the owning worker.
// Callers can abandon their wait without stranding a worker on a result send.
func (s *Store) command(ctx context.Context, w write) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	w.ctx = ctx
	w.result = make(chan error, 1)
	s.mu.RLock()
	if s.closed {
		s.mu.RUnlock()
		return errors.New("activity store is closed")
	}
	if s.initErr != nil {
		s.mu.RUnlock()
		return ErrUnavailable
	}
	select {
	case s.commands <- w:
		s.mu.RUnlock()
	default:
		s.mu.RUnlock()
		return errors.New("history writer is busy; try again")
	}
	select {
	case err := <-w.result:
		return err
	case <-ctx.Done():
		return ctx.Err()
	}
}
