package activity

import (
	"errors"
	"sync/atomic"
	"time"
)

// IngestionHealth is an in-memory service-lifetime diagnostic. It remains
// readable when the activity database is unavailable and never retains the
// rejected event, an SQL error, media or credentials.
type IngestionHealth struct {
	StartedAt                time.Time  `json:"started_at"`
	QueueDepth               int        `json:"queue_depth"`
	QueueCapacity            int        `json:"queue_capacity"`
	QueueDropped             uint64     `json:"queue_dropped"`
	OutboxPending            int64      `json:"outbox_pending"`
	OutboxCapacity           int64      `json:"outbox_capacity,omitempty"`
	OldestPendingAt          *time.Time `json:"oldest_pending_at,omitempty"`
	OutboxDropped            uint64     `json:"outbox_dropped"`
	RejectedObservations     uint64     `json:"rejected_observations"`
	WriteFailures            uint64     `json:"write_failures"`
	MaintenanceFailures      uint64     `json:"maintenance_failures"`
	LastWriteSuccessAt       *time.Time `json:"last_write_success_at,omitempty"`
	LastWriteFailureAt       *time.Time `json:"last_write_failure_at,omitempty"`
	LastMaintenanceFailureAt *time.Time `json:"last_maintenance_failure_at,omitempty"`
	Degraded                 bool       `json:"degraded"`
	StorageInitializing      bool       `json:"storage_initializing,omitempty"`
	StorageUnavailable       bool       `json:"storage_unavailable,omitempty"`
	OpenFailures             uint64     `json:"open_failures,omitempty"`
	LastOpenFailureAt        *time.Time `json:"last_open_failure_at,omitempty"`
	Error                    string     `json:"error,omitempty"`
}

type ingestionCounters struct {
	startedAt            time.Time
	queueDropped         atomic.Uint64
	outboxDropped        atomic.Uint64
	rejected             atomic.Uint64
	writeFailures        atomic.Uint64
	maintenanceFailures  atomic.Uint64
	lastWriteSuccess     atomic.Int64
	lastWriteFailure     atomic.Int64
	lastMaintenanceError atomic.Int64
	openFailures         atomic.Uint64
	lastOpenFailure      atomic.Int64
}

func ingestionTime(value int64) *time.Time {
	if value == 0 {
		return nil
	}
	at := time.Unix(0, value).UTC()
	return &at
}

func (s *Store) IngestionHealth() IngestionHealth {
	if s == nil {
		return IngestionHealth{StorageUnavailable: true, Degraded: true, Error: ErrUnavailable.Error()}
	}
	v := IngestionHealth{
		StartedAt:                s.ingestion.startedAt,
		QueueDepth:               len(s.queue),
		QueueCapacity:            cap(s.queue),
		QueueDropped:             s.ingestion.queueDropped.Load(),
		OutboxPending:            s.outboxPending.Load(),
		OldestPendingAt:          ingestionTime(s.outboxOldest.Load()),
		OutboxDropped:            s.ingestion.outboxDropped.Load(),
		RejectedObservations:     s.ingestion.rejected.Load(),
		WriteFailures:            s.ingestion.writeFailures.Load(),
		MaintenanceFailures:      s.ingestion.maintenanceFailures.Load(),
		LastWriteSuccessAt:       ingestionTime(s.ingestion.lastWriteSuccess.Load()),
		LastWriteFailureAt:       ingestionTime(s.ingestion.lastWriteFailure.Load()),
		LastMaintenanceFailureAt: ingestionTime(s.ingestion.lastMaintenanceError.Load()),
		OpenFailures:             s.ingestion.openFailures.Load(),
		LastOpenFailureAt:        ingestionTime(s.ingestion.lastOpenFailure.Load()),
	}
	if !s.archive {
		v.OutboxCapacity = OutboxCapacity
	}
	v.Degraded = v.QueueDropped > 0 || v.OutboxDropped > 0 || v.RejectedObservations > 0 || v.WriteFailures > 0 || v.MaintenanceFailures > 0
	if err := s.InitializationError(); err != nil {
		v.StorageInitializing = errors.Is(err, ErrInitializing)
		v.StorageUnavailable = !v.StorageInitializing
		v.Degraded = v.Degraded || v.StorageUnavailable
		v.Error = err.Error()
	}
	return v
}

func (s *Store) observeMaintenance(err error) {
	if err != nil {
		s.ingestion.maintenanceFailures.Add(1)
		s.ingestion.lastMaintenanceError.Store(time.Now().UTC().UnixNano())
	}
}
