package rtpvoice

import (
	"errors"
	"fmt"
	"open25.local/open25-fne/internal/p25rtp"
)

// Spurt is an already authorized, immutable owner on this SIP media segment.
// The caller allocates an unused parity-correct TSN and supplies actual source
// identity and accepted service options. UDP cannot allocate or replace it.
type Spurt struct {
	Origin         p25rtp.Role
	TSN            uint8
	GroupID        uint16
	ServiceOptions uint8
	RF             *p25rtp.RFControlWord
	Console        *p25rtp.ConsoleControlWord
}

// Frame.Position is a voice ordinal unwrapped from actual SF*18+FT.Index modulo
// 72. It is never a native/DV25/control/RTP sequence number. The caller must fail
// the owner if an inactive interval makes a whole-cycle unwrap ambiguous.
type Frame struct {
	Position uint64
	Voice    p25rtp.FullRateVoice
	Header   *p25rtp.HeaderWord
}

type DeliveryKind uint8

const (
	FrameDelivery DeliveryKind = iota
	GapDelivery
	EndDelivery
)

type EngineReason string

const (
	ReasonLocalEnd       EngineReason = "local_end"
	ReasonRemoteEnd      EngineReason = "remote_end"
	ReasonFirstMedia     EngineReason = "first_media_timeout"
	ReasonMediaLoss      EngineReason = "media_loss"
	ReasonRequestTimeout EngineReason = "request_timeout"
	ReasonDenied         EngineReason = "denied"
	ReasonHeartbeat      EngineReason = "heartbeat_timeout"
	ReasonQueue          EngineReason = "queue_overflow"
	ReasonSource         EngineReason = "invalid_source_frame"
	ReasonCanceled       EngineReason = "canceled"
	ReasonTransport      EngineReason = "transport_error"
)

// A Gap has only Position/Timestamp; its zero Frame is not an audio observation.
// End is ordered after accepted media on graceful completion. Done is only the
// worker lifetime; the caller still performs its native fence and SIP release.
type Delivery struct {
	Kind      DeliveryKind
	Position  uint64
	Timestamp uint32
	Frame     Frame
	Reason    EngineReason
	token     bool
}

var (
	ErrSpurt         = errors.New("rtpvoice: invalid admitted spurt")
	ErrQueueFull     = errors.New("rtpvoice: bounded media queue is full")
	ErrFramePosition = errors.New("rtpvoice: ambiguous or inconsistent voice position")
	ErrEngineStarted = errors.New("rtpvoice: engine already started or closed")
	ErrDirection     = errors.New("rtpvoice: this spurt has a remote source")
	ErrSpurtTimeout  = errors.New("rtpvoice: talk spurt timed out")
	ErrSpurtDenied   = errors.New("rtpvoice: talk spurt denied")
)

func (s Spurt) copy() Spurt {
	if s.RF != nil {
		v := *s.RF
		s.RF = &v
	}
	if s.Console != nil {
		v := *s.Console
		s.Console = &v
	}
	return s
}
func (f Frame) copy() Frame {
	if f.Header != nil {
		v := *f.Header
		f.Header = &v
	}
	if f.Voice.LSD != nil {
		v := *f.Voice.LSD
		f.Voice.LSD = &v
	}
	if f.Voice.ES != nil {
		v := *f.Voice.ES
		f.Voice.ES = &v
	}
	return f
}
func (s Spurt) validate(local p25rtp.Role) error {
	if s.GroupID == 0 || s.TSN == 0 || s.TSN > 127 || (s.Origin != p25rtp.GroupHome && s.Origin != p25rtp.GroupServing) || (s.Origin == p25rtp.GroupHome) != (s.TSN%2 == 1) {
		return ErrSpurt
	}
	if local != p25rtp.GroupHome && local != p25rtp.GroupServing {
		return ErrSpurt
	}
	kind := p25rtp.Request
	if s.Origin == p25rtp.GroupHome {
		kind = p25rtp.Start
	}
	packet := p25rtp.Packet{Role: s.Origin, Control: p25rtp.PacketTypeControl{Type: kind, TSN: s.TSN, ServiceOptions: s.ServiceOptions}, RF: s.RF, Console: s.Console}
	if _, err := p25rtp.Marshal(packet); err != nil {
		return fmt.Errorf("%w: source/control fields", ErrSpurt)
	}
	return nil
}
func voiceSlot(v p25rtp.FullRateVoice) (uint64, error) {
	index, ok := v.Type.Index()
	if !ok || v.Quality.Superframe > 3 {
		return 0, ErrFramePosition
	}
	return uint64(v.Quality.Superframe)*18 + uint64(index), nil
}
func (f Frame) validate(group uint16) error {
	slot, err := voiceSlot(f.Voice)
	if err != nil || f.Position%72 != slot {
		return ErrFramePosition
	}
	if _, err = p25rtp.MarshalVoice(f.Voice); err != nil {
		return err
	}
	if f.Header != nil {
		if f.Header.GroupID != group {
			return ErrSpurt
		}
		if _, err = p25rtp.MarshalHeaderWord(*f.Header); err != nil {
			return err
		}
	}
	return nil
}
