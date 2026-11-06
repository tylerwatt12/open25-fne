package framework

import (
	"sync"
	"time"

	"open25.local/open25-fne/internal/activity"
	"open25.local/open25-fne/internal/dispatch"
)

// One failure observation per connection keeps transport causes visible
// without recording media, keys or a per-frame diagnostic stream.
type mediaLinkDiagnostic struct {
	s           *Service
	peer        string
	generation  uint64
	once        sync.Once
	mu          sync.Mutex
	frames      uint64
	last        dispatch.Frame
	lastAge     time.Duration
	maxWork     time.Duration
	maxWorkSeq  uint64
	workStarted time.Time
}

func (d *mediaLinkDiagnostic) beginFrame(frame dispatch.Frame, age time.Duration) {
	d.mu.Lock()
	d.frames++
	// Store only the public identity/timing fields needed for this diagnostic.
	d.last = dispatch.Frame{CallID: frame.CallID, SourceID: frame.SourceID, TalkgroupID: frame.TalkgroupID, Sequence: frame.Sequence}
	d.lastAge = age
	d.workStarted = time.Now()
	d.mu.Unlock()
}

func (d *mediaLinkDiagnostic) endFrame(work time.Duration) {
	d.mu.Lock()
	if work > d.maxWork {
		d.maxWork, d.maxWorkSeq = work, d.last.Sequence
	}
	d.workStarted = time.Time{}
	d.mu.Unlock()
}

func (d *mediaLinkDiagnostic) fail(stage string, cause error) {
	if d == nil || (d.s.operationCtx != nil && d.s.operationCtx.Err() != nil) {
		return
	}
	d.once.Do(func() {
		message := "The media link closed during " + stage + "."
		if cause != nil {
			reason := cause.Error()
			if len(reason) > 512 {
				reason = reason[:512]
			}
			message += " " + reason
		}
		d.mu.Lock()
		frame := d.last
		if !d.workStarted.IsZero() && time.Since(d.workStarted) > d.maxWork {
			d.maxWork, d.maxWorkSeq = time.Since(d.workStarted), frame.Sequence
		}
		details := map[string]any{"stage": stage, "peer_id": d.peer, "generation": d.generation, "received_frames": d.frames, "last_sequence": frame.Sequence, "last_frame_age_ms": d.lastAge.Milliseconds(), "maximum_processing_ms": d.maxWork.Milliseconds(), "maximum_processing_sequence": d.maxWorkSeq}
		d.mu.Unlock()
		severity := "warning"
		if stage == "read" {
			// A peer may close normally during its own shutdown or failback.
			// The peer that knows a concrete failure records its warning.
			severity = "info"
		}
		d.s.emit(activity.Event{CallID: frame.CallID, RadioID: frame.SourceID, TalkgroupID: frame.TalkgroupID, Type: "media_link", Audience: "diagnostic", Severity: severity, Outcome: "disconnected", Message: message, Details: details})
	})
}

func (d *mediaLinkDiagnostic) suppress() {
	if d != nil {
		d.once.Do(func() {})
	}
}
