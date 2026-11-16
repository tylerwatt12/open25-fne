package activity

import (
	"context"
	"errors"
	"time"
)

var (
	ErrInitializing = errors.New("history storage is initializing")
	ErrUnavailable  = errors.New("history storage is unavailable")
)

// InitializationError is a nonblocking observation owned by this Store. A
// pending writer never makes controller startup or a status read wait for SQL.
func (s *Store) InitializationError() error {
	if s == nil {
		return ErrUnavailable
	}
	select {
	case <-s.ready:
		if s.initErr != nil {
			return ErrUnavailable
		}
		return nil
	default:
		return ErrInitializing
	}
}

func (s *Store) finishInitialization(err error) {
	s.mu.Lock()
	s.initErr = err
	if err != nil && !errors.Is(err, context.Canceled) {
		s.ingestion.openFailures.Add(1)
		s.ingestion.lastOpenFailure.Store(time.Now().UTC().UnixNano())
	}
	close(s.ready)
	s.mu.Unlock()
	if err == nil {
		return
	}
	// Admission is now closed to new work. Captures were best-effort queued,
	// not acknowledged; remote batches remain with their sender on failure.
	for {
		var w write
		select {
		case w = <-s.queue:
		case w = <-s.uploadQueue:
		case w = <-s.commands:
		default:
			return
		}
		switch {
		case w.barrier != nil:
			close(w.barrier)
		case w.pruned != nil:
			w.pruned <- pruneResult{err: ErrUnavailable}
		case w.result != nil:
			w.result <- ErrUnavailable
		default:
			s.ingestion.queueDropped.Add(1)
		}
	}
}
