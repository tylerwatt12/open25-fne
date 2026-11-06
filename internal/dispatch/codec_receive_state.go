package dispatch

import (
	"encoding/binary"
	"encoding/hex"
	"errors"
	"time"

	"open25.local/open25-fne/internal/p25crypto"
)

// These are private bridge-to-Site transport bounds, not CAI call timers.
const (
	OpaqueReceiveFreshness = 350 * time.Millisecond
	OpaqueReceiveBudget    = 1500 * time.Millisecond
)

// CodecReceiveActivity renews an existing receive owner without carrying or
// publishing audio. A control message cannot create a call or advance voice
// sequence numbers. LastMediaAt identifies the actual voice that it follows.
type CodecReceiveActivity struct {
	CallID, nativeID                   string
	TalkgroupID, SourceID              int
	ManufacturerID, AlgorithmID        uint8
	ServiceOptions                     uint8
	KeyID                              uint16
	Emergency, Encrypted               bool
	ControlSequence, LastVoiceSequence uint64
	LastMediaAt, ValidUntil            time.Time
}

type codecReceiveUpdate struct {
	frame    Frame
	activity *CodecReceiveActivity
}

type opaqueLiveness struct {
	CodecReceiveActivity
	remaining time.Duration
}

func decodeOpaqueLiveness(packet []byte) (opaqueLiveness, error) {
	if len(packet) != packetHeaderSize || string(packet[:4]) != "DV25" || packet[4] != 17 || packet[5] != 0 || packet[7] != 0 || binary.BigEndian.Uint16(packet[10:12]) != 0 || packet[56] != 0 || binary.BigEndian.Uint16(packet[58:60]) != 0 || packet[60] > 1 || packet[62] != 0 || packet[63] != 0 {
		return opaqueLiveness{}, errors.New("invalid native receive activity packet")
	}
	a := opaqueLiveness{CodecReceiveActivity: CodecReceiveActivity{nativeID: hex.EncodeToString(packet[32:48]), SourceID: int(binary.BigEndian.Uint32(packet[12:16])), TalkgroupID: int(binary.BigEndian.Uint32(packet[16:20])), ManufacturerID: packet[57], Emergency: packet[60] == 1, ServiceOptions: packet[61], AlgorithmID: packet[6], KeyID: binary.BigEndian.Uint16(packet[8:10]), ControlSequence: binary.BigEndian.Uint64(packet[24:32]), LastVoiceSequence: binary.BigEndian.Uint64(packet[48:56])}, remaining: time.Duration(binary.BigEndian.Uint32(packet[20:24])) * time.Millisecond}
	a.Encrypted = p25crypto.Supported(int(a.AlgorithmID))
	if (a.ServiceOptions&0x80 != 0) != a.Emergency || (a.ServiceOptions&0x40 != 0) != a.Encrypted {
		return opaqueLiveness{}, errors.New("receive activity service options disagree")
	}
	if a.SourceID < 1 || a.SourceID > 0xfffffb || a.TalkgroupID < 1 || a.TalkgroupID > 65534 || a.ManufacturerID > 1 || a.ControlSequence == 0 || a.LastVoiceSequence == 0 || a.remaining <= 0 || a.remaining > OpaqueReceiveBudget || (!a.Encrypted && (a.AlgorithmID != 0x80 || a.KeyID != 0)) {
		return opaqueLiveness{}, errors.New("invalid native receive activity identity or budget")
	}
	return a, nil
}

type opaqueIdentity struct {
	token  string
	source int
}
type retiredOpaque struct {
	sequence uint64
	terminal bool
	group    int
}
type opaqueReceiveState struct {
	call, native                   string
	header                         Frame
	voiceSequence, controlSequence uint64
	lastMedia, until               time.Time
	retired                        map[opaqueIdentity]retiredOpaque
	retiredOrder                   []opaqueIdentity
}

func (s *opaqueReceiveState) remember(terminal bool) {
	if s.call == "" {
		return
	}
	if s.retired == nil {
		s.retired = make(map[opaqueIdentity]retiredOpaque)
	}
	key := opaqueIdentity{s.native, s.header.SourceID}
	if _, known := s.retired[key]; !known {
		s.retiredOrder = append(s.retiredOrder, key)
	}
	s.retired[key] = retiredOpaque{s.voiceSequence, terminal, s.header.TalkgroupID}
	// This only rejects recent delayed voice. End and activity always require
	// the exact current owner, regardless of this bounded history.
	if len(s.retiredOrder) > 64 {
		delete(s.retired, s.retiredOrder[0])
		s.retiredOrder = s.retiredOrder[1:]
	}
}

func (s *opaqueReceiveState) end(now time.Time, terminal bool) (Frame, bool) {
	if s.call == "" {
		return Frame{}, false
	}
	f := Frame{CallID: s.call, TalkgroupID: s.header.TalkgroupID, SourceID: s.header.SourceID, Emergency: s.header.Emergency, ServiceOptions: s.header.ServiceOptions, ManufacturerID: s.header.ManufacturerID, Encrypted: s.header.Encrypted, AlgorithmID: s.header.AlgorithmID, KeyID: s.header.KeyID, Sequence: s.voiceSequence, End: true, CapturedAt: now}
	s.remember(terminal)
	s.call, s.native = "", ""
	s.until = time.Time{}
	return f, true
}

func (s *opaqueReceiveState) expire(now time.Time) (Frame, bool) {
	if s.call == "" || now.Before(s.until) {
		return Frame{}, false
	}
	return s.end(now, false)
}

func (s *opaqueReceiveState) voice(f Frame, now time.Time) ([]Frame, error) {
	if !f.MediaValid() || f.Sequence == 0 {
		return nil, nil
	}
	if f.End {
		key := opaqueIdentity{f.CallID, f.SourceID}
		if s.call == "" || f.CallID != s.native || f.SourceID != s.header.SourceID {
			if previous, known := s.retired[key]; known && f.TalkgroupID == previous.group && f.Sequence > previous.sequence {
				s.retired[key] = retiredOpaque{f.Sequence, true, previous.group}
			}
			return nil, nil
		}
		if f.TalkgroupID != s.header.TalkgroupID || f.ManufacturerID != s.header.ManufacturerID || f.Sequence <= s.voiceSequence {
			return nil, nil
		}
		f.CallID = s.call
		// Token/source and forward sequence establish the terminal's owner.
		// Release using that owner's accepted public security profile;
		// terminal metadata cannot change its admission identity.
		f.Emergency = s.header.Emergency
		f.ServiceOptions = s.header.ServiceOptions
		f.ManufacturerID = s.header.ManufacturerID
		f.Encrypted = s.header.Encrypted
		f.AlgorithmID = s.header.AlgorithmID
		f.KeyID = s.header.KeyID
		f.MessageIndicator = nil
		s.voiceSequence = f.Sequence
		s.end(now, true)
		return []Frame{f}, nil
	}
	key := opaqueIdentity{f.CallID, f.SourceID}
	if previous, known := s.retired[key]; known && (previous.terminal || f.TalkgroupID != previous.group || f.Sequence <= previous.sequence) {
		return nil, nil
	}
	var out []Frame
	if s.call != "" && (f.CallID != s.native || f.SourceID != s.header.SourceID) {
		if ended, ok := s.end(now, true); ok {
			out = append(out, ended)
		}
	}
	if s.call == "" {
		// A timeout can recover from new real voice only until another native
		// owner has been accepted. It must not later displace that replacement.
		for identity, previous := range s.retired {
			if identity != key && !previous.terminal {
				previous.terminal = true
				s.retired[identity] = previous
			}
		}
		id, err := randomID()
		if err != nil {
			return out, err
		}
		s.call, s.native = id, f.CallID
		s.header = Frame{SourceID: f.SourceID, TalkgroupID: f.TalkgroupID, ManufacturerID: f.ManufacturerID, Encrypted: f.Encrypted, AlgorithmID: f.AlgorithmID, KeyID: f.KeyID}
		s.voiceSequence, s.controlSequence = 0, 0
	}
	if f.Sequence <= s.voiceSequence || f.TalkgroupID != s.header.TalkgroupID || f.ManufacturerID != s.header.ManufacturerID || f.Encrypted != s.header.Encrypted || f.AlgorithmID != s.header.AlgorithmID || f.KeyID != s.header.KeyID {
		return out, nil
	}
	s.header.ServiceOptions = f.ServiceOptions
	s.header.Emergency = f.Emergency // observed voice metadata, not a new floor/incident decision
	s.voiceSequence = f.Sequence
	s.lastMedia, s.until = now, now.Add(OpaqueReceiveFreshness)
	f.CapturedAt, f.CallID = now, s.call
	out = append(out, f)
	return out, nil
}

func (s *opaqueReceiveState) activity(a opaqueLiveness, now time.Time) (CodecReceiveActivity, bool) {
	if s.call == "" || !now.Before(s.until) || a.nativeID != s.native || a.SourceID != s.header.SourceID || a.TalkgroupID != s.header.TalkgroupID || a.ManufacturerID != s.header.ManufacturerID || a.Emergency != s.header.Emergency || a.ServiceOptions != s.header.ServiceOptions || a.Encrypted != s.header.Encrypted || a.AlgorithmID != s.header.AlgorithmID || a.KeyID != s.header.KeyID || a.ControlSequence <= s.controlSequence || a.LastVoiceSequence != s.voiceSequence {
		return CodecReceiveActivity{}, false
	}
	until := now.Add(min(a.remaining, OpaqueReceiveFreshness))
	if hard := s.lastMedia.Add(OpaqueReceiveBudget); hard.Before(until) {
		until = hard
	}
	if !now.Before(until) {
		return CodecReceiveActivity{}, false
	}
	s.controlSequence, s.until = a.ControlSequence, until
	a.CallID, a.nativeID = s.call, ""
	a.LastMediaAt, a.ValidUntil = s.lastMedia, until
	return a.CodecReceiveActivity, true
}
