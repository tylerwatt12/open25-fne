package activity

import (
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
	RejectedObservations     uint64     `json:"rejected_observations"`
	WriteFailures            uint64     `json:"write_failures"`
	MaintenanceFailures      uint64     `json:"maintenance_failures"`
	LastWriteSuccessAt       *time.Time `json:"last_write_success_at,omitempty"`
	LastWriteFailureAt       *time.Time `json:"last_write_failure_at,omitempty"`
	LastMaintenanceFailureAt *time.Time `json:"last_maintenance_failure_at,omitempty"`
	Degraded                 bool       `json:"degraded"`
}

type ingestionCounters struct {
	startedAt            time.Time
	queueDropped         atomic.Uint64
	rejected             atomic.Uint64
	writeFailures        atomic.Uint64
	maintenanceFailures  atomic.Uint64
	lastWriteSuccess     atomic.Int64
	lastWriteFailure     atomic.Int64
	lastMaintenanceError atomic.Int64
}

func ingestionTime(value int64) *time.Time {
	if value == 0 {
		return nil
	}
	at := time.Unix(0, value).UTC()
	return &at
}

func (s *Store) IngestionHealth() IngestionHealth {
	v := IngestionHealth{
		StartedAt:                s.ingestion.startedAt,
		QueueDepth:               len(s.queue),
		QueueCapacity:            cap(s.queue),
		QueueDropped:             s.ingestion.queueDropped.Load(),
		RejectedObservations:     s.ingestion.rejected.Load(),
		WriteFailures:            s.ingestion.writeFailures.Load(),
		MaintenanceFailures:      s.ingestion.maintenanceFailures.Load(),
		LastWriteSuccessAt:       ingestionTime(s.ingestion.lastWriteSuccess.Load()),
		LastWriteFailureAt:       ingestionTime(s.ingestion.lastWriteFailure.Load()),
		LastMaintenanceFailureAt: ingestionTime(s.ingestion.lastMaintenanceError.Load()),
	}
	v.Degraded = v.QueueDropped > 0 || v.RejectedObservations > 0 || v.WriteFailures > 0 || v.MaintenanceFailures > 0
	return v
}

func (s *Store) observeMaintenance(err error) {
	if err != nil {
		s.ingestion.maintenanceFailures.Add(1)
		s.ingestion.lastMaintenanceError.Store(time.Now().UTC().UnixNano())
	}
}
