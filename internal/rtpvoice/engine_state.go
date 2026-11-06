package rtpvoice

import (
	"time"
	"open25.local/open25-fne/internal/p25rtp"
)

// BACA-B Annex B Table58 PDF428-430 (printed404-406); B-2 keeps the
// numeric table and corrects Request/Wait transitions in7.8.2.1/7.9.3/7.9.5.
const (
	framePeriod        = 20 * time.Millisecond
	requestRetry       = 500 * time.Millisecond
	requestTimeout     = time.Second
	waitTimeout        = 5 * time.Second
	firstPacketTimeout = 500 * time.Millisecond
	mediaLossTimeout   = 200 * time.Millisecond
	startRetry         = 360 * time.Millisecond
	endRetry           = 20 * time.Millisecond
	heartbeatDefault   = 10 * time.Second
	startCopies        = 4
	endCopies          = 4
	txCapacity         = 12
	rxCapacity         = 16
	reorderDelay       = 40 * time.Millisecond
	maximumQueueAge    = 240 * time.Millisecond // Private, below routing's300ms age limit.
)

type enginePhase uint8

const (
	phaseAwaiting enginePhase = iota
	phaseRequesting
	phaseWaiting
	phaseActive
	phaseEnding
)

type engineState struct {
	phase                               enginePhase
	localSource                         bool
	origin                              p25rtp.Role
	local                               p25rtp.Role
	ready                               bool
	mediaSeen                           bool
	requestDue, requestUntil, waitUntil time.Time
	startDue                            time.Time
	startsLeft                          int
	mediaUntil                          time.Time
	heartbeatDue, heartbeatSeen         time.Time
	heartbeatInterval                   time.Duration
	reason                              EngineReason
}

func newEngineState(local, origin p25rtp.Role, now time.Time) (engineState, []p25rtp.PacketType) {
	s := engineState{local: local, origin: origin, localSource: local == origin, heartbeatDue: now.Add(heartbeatDefault), heartbeatSeen: now, heartbeatInterval: heartbeatDefault}
	var send []p25rtp.PacketType
	if local == p25rtp.GroupHome {
		send = append(send, p25rtp.HeartbeatQuery)
	}
	if s.localSource {
		if origin == p25rtp.GroupServing {
			s.phase = phaseRequesting
			s.requestDue = now.Add(requestRetry)
			s.requestUntil = now.Add(requestTimeout)
			send = append(send, p25rtp.Request)
		} else {
			s.phase = phaseActive
			s.ready = true
			s.mediaUntil = now.Add(firstPacketTimeout)
			s.startsLeft = startCopies - 1
			s.startDue = now.Add(startRetry)
			send = append(send, p25rtp.Start)
		}
	} else if origin == p25rtp.GroupServing {
		// Private initial-control guard for this pre-admitted one-spurt leg.
		s.mediaUntil = now.Add(requestTimeout)
	} else {
		s.mediaUntil = now.Add(firstPacketTimeout)
	}
	return s, send
}
func (s *engineState) ending(reason EngineReason) {
	if s.phase == phaseEnding {
		return
	}
	s.phase = phaseEnding
	s.reason = reason
	s.requestDue = time.Time{}
	s.requestUntil = time.Time{}
	s.waitUntil = time.Time{}
	s.startDue = time.Time{}
	s.startsLeft = 0
	s.mediaUntil = time.Time{}
}
func (s *engineState) tick(now time.Time) []p25rtp.PacketType {
	var out []p25rtp.PacketType
	if now.Sub(s.heartbeatSeen) > 4*s.heartbeatInterval {
		s.ending(ReasonHeartbeat)
	}
	if s.phase != phaseEnding {
		if !s.mediaUntil.IsZero() && !now.Before(s.mediaUntil) {
			if s.mediaSeen {
				s.ending(ReasonMediaLoss)
			} else {
				s.ending(ReasonFirstMedia)
			}
		}
		if s.phase == phaseRequesting && !now.Before(s.requestUntil) {
			s.ending(ReasonRequestTimeout)
		}
		if s.phase == phaseWaiting && !now.Before(s.waitUntil) {
			s.phase = phaseRequesting
			s.waitUntil = time.Time{}
			s.requestUntil = now.Add(requestTimeout)
			s.requestDue = now.Add(requestRetry)
			out = append(out, p25rtp.Request)
		} else if s.phase == phaseRequesting && !now.Before(s.requestDue) {
			s.requestDue = now.Add(requestRetry)
			out = append(out, p25rtp.Request)
		}
		if s.phase == phaseActive && s.startsLeft > 0 && !now.Before(s.startDue) {
			s.startsLeft--
			s.startDue = now.Add(startRetry)
			out = append(out, p25rtp.Start)
		}
	}
	if !now.Before(s.heartbeatDue) {
		s.heartbeatDue = now.Add(s.heartbeatInterval)
		out = append(out, p25rtp.Heartbeat)
	}
	return out
}
func (s *engineState) control(kind p25rtp.PacketType, now time.Time) []p25rtp.PacketType {
	if s.phase == phaseEnding {
		return nil
	}
	if s.localSource {
		if s.origin == p25rtp.GroupServing {
			switch kind {
			case p25rtp.Grant:
				if s.phase == phaseRequesting || s.phase == phaseWaiting {
					s.phase = phaseActive
					s.ready = true
					s.mediaUntil = now.Add(firstPacketTimeout)
					s.requestDue = time.Time{}
					s.requestUntil = time.Time{}
					s.waitUntil = time.Time{}
				}
			case p25rtp.Wait:
				if s.phase == phaseRequesting {
					s.phase = phaseWaiting
					s.requestDue = time.Time{}
					s.requestUntil = time.Time{}
					s.waitUntil = now.Add(waitTimeout)
				}
			case p25rtp.Deny:
				if s.phase == phaseRequesting || s.phase == phaseWaiting {
					s.ending(ReasonDenied)
				}
			}
		}
		return nil
	}
	switch kind {
	case p25rtp.Request:
		if s.origin == p25rtp.GroupServing {
			// Private Home first-media wait spans the initial1s Request transaction,
			// permitting the500ms lost-Grant retry; duplicates never extend it.
			if s.phase == phaseAwaiting {
				s.phase = phaseActive
				s.ready = true
				s.mediaUntil = now.Add(requestTimeout)
			}
			return []p25rtp.PacketType{p25rtp.Grant}
		}
	case p25rtp.Start:
		if s.origin == p25rtp.GroupHome && s.phase == phaseAwaiting {
			s.phase = phaseActive
			s.ready = true
			s.mediaUntil = now.Add(firstPacketTimeout)
		}
	case p25rtp.End:
		if s.phase == phaseActive {
			s.ending(ReasonRemoteEnd)
		}
	}
	return nil
}
func (s *engineState) progress(now time.Time) {
	if s.phase != phaseActive {
		return
	}
	s.mediaSeen = true
	s.mediaUntil = now.Add(mediaLossTimeout)
}
func (s *engineState) heartbeat(kind p25rtp.PacketType, interval uint8, now time.Time) []p25rtp.PacketType {
	// Private strict valid range from Table58. Invalid intervals cannot keep a
	// session alive or change its established heartbeat interval.
	if interval < 5 || interval > 20 {
		return nil
	}
	s.heartbeatSeen = now
	s.heartbeatInterval = time.Duration(interval) * time.Second
	if kind == p25rtp.HeartbeatQuery {
		s.heartbeatDue = now.Add(s.heartbeatInterval)
		return []p25rtp.PacketType{p25rtp.Heartbeat}
	}
	return nil
}
