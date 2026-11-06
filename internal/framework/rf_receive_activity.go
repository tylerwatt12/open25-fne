package framework

import (
	"open25.local/open25-fne/internal/dispatch"
	"time"
)

func (s *Service) codecReceiveEpoch(epoch uint64) bool {
	if s.broker == nil {
		return false
	}
	s.broker.mu.Lock()
	defer s.broker.mu.Unlock()
	return !s.broker.closed && !s.broker.paused && s.broker.epoch == epoch
}

// Native receive activity can only keep its existing admitted owner alive.
// It is never passed to a router's audio Write or used to create a grant.
func (s *Service) codecReceiveActivityAtEpoch(a dispatch.CodecReceiveActivity, epoch uint64) {
	now := time.Now()
	if !s.codecReceiveEpoch(epoch) || s.radio == nil || a.CallID == "" || a.ControlSequence == 0 || a.LastVoiceSequence == 0 || a.LastMediaAt.IsZero() || a.LastMediaAt.After(now) || !now.Before(a.ValidUntil) || a.ValidUntil.After(now.Add(dispatch.OpaqueReceiveFreshness)) || a.ValidUntil.After(a.LastMediaAt.Add(dispatch.OpaqueReceiveBudget)) {
		return
	}
	physical := false
	for _, r := range s.view().System.Radios {
		if r.RID == a.SourceID && r.Enabled && r.Kind == "subscriber" {
			physical = true
		}
	}
	if !physical || s.radioServiceInhibited(a.SourceID) || !subscriberGroupAllowed(s.view().System, a.SourceID, a.TalkgroupID, true) {
		return
	}
	id := a.CallID
	if s.rfAdmissions != nil {
		var ok bool
		id, ok = s.rfAdmissions.activityOwner(a)
		if !ok {
			return
		}
	}
	b := s.broker
	b.mu.Lock()
	defer b.mu.Unlock()
	stream := b.rfCalls[id]
	if stream == nil || b.closed || b.paused || b.epoch != epoch || stream.receiveApplicationID != a.CallID || stream.request.RadioID != a.SourceID || len(stream.request.Talkgroups) != 1 || stream.request.Talkgroups[0] != a.TalkgroupID || !stream.lastMediaAt.Equal(a.LastMediaAt) || stream.lastNativeVoiceSequence != a.LastVoiceSequence || a.ControlSequence <= stream.lastReceiveControl {
		return
	}
	h := stream.receiveHeader
	if h.ManufacturerID != a.ManufacturerID || h.Emergency != a.Emergency || h.Encrypted != a.Encrypted || h.AlgorithmID != a.AlgorithmID || h.KeyID != a.KeyID {
		return
	}
	stream.lastReceiveControl, stream.receiveUntil = a.ControlSequence, a.ValidUntil
}

// AllStart's committed native admission remains authoritative. A heartbeat
// may reference its bound media alias; it cannot bind a new alias, advance
// audio sequence numbers or refresh an unconfirmed native admission.
func (m *rfAdmissionManager) activityOwner(a dispatch.CodecReceiveActivity) (string, bool) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if id, known := m.aliases[a.CallID]; known {
		e := m.entries[id]
		if e == nil || e.ended || e.stream == nil || e.boundStream != a.CallID || e.request.State != "committed" || e.approvedAt.IsZero() || e.request.Source != a.SourceID || e.request.Group != a.TalkgroupID || e.request.Secure != a.Encrypted || time.Since(e.lastObserved) > time.Second || e.inputSequence != a.LastVoiceSequence {
			return "", false
		}
		if a.Encrypted && (e.boundAlgorithm != a.AlgorithmID || e.boundKey != a.KeyID) {
			return "", false
		}
		return e.stream.request.ID, true
	}
	if m.connected() && m.allStart(a.TalkgroupID) {
		return "", false
	}
	return a.CallID, true
}
