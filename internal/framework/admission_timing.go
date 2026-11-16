package framework

import "time"

// Admission timing is opt-in, service-local and independent of activity storage.
// Configure the sink before starting calls and never replace it while in use.
// Records contain public correlation/timing only, never request proofs or media.
type admissionTiming struct {
	Stage, CallID string
	Elapsed       time.Duration
	Succeeded     bool
}

func (s *Service) admissionClock() time.Time {
	if s.admissionTimings == nil {
		return time.Time{}
	}
	return time.Now()
}

func (s *Service) admissionElapsed(start time.Time, stage, id string, succeeded bool) {
	if start.IsZero() {
		return
	}
	observation := admissionTiming{stage, id, time.Since(start), succeeded}
	select {
	case s.admissionTimings <- observation:
	default:
		s.admissionTimingDrops.Add(1)
	}
}
