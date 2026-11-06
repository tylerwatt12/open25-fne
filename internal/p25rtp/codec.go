package p25rtp

import (
	"encoding/binary"
	"fmt"

	"github.com/pion/rtp"
)

const (
	blockVoice   = 0
	blockPacket  = 1
	blockHeader  = 5
	blockRF      = 11
	blockConsole = 15
	// VBB1: RTP + control octet + four block headers + control + console +
	// HeaderWord + the longest FullRate voice block (encryption sync).
	maxPacketSize = 12 + 1 + 4*4 + 4 + 24 + 18 + 18
)

// Marshal serializes the VBB1 profile. It never invents quality, LSD or ES data.
func Marshal(p Packet) ([]byte, error) {
	if err := p.validate(); err != nil {
		return nil, err
	}
	types := []uint8{blockPacket}
	blocks := [][]byte{{uint8(p.Control.Type) | bit(p.Control.Mute, 7), p.Control.ServiceOptions, p.Control.TSN<<1 | bit(p.Control.Losing, 0), p.Control.Interval}}
	if p.RF != nil {
		b := make([]byte, 8)
		putSUID(b, p.RF.Source)
		b[7] = p.RF.TransmitPriority
		types, blocks = append(types, blockRF), append(blocks, b)
	}
	if p.Console != nil {
		b := make([]byte, 24)
		putSUID(b, p.Console.Console)
		b[7] = p.Console.TransmitPriority
		putSUID(b[8:], p.Console.PhysicalConsole)
		b[15] = p.Console.TransmissionDescriptor
		putSUID(b[16:], p.Console.VoiceSource)
		b[23] = p.Console.ConsoleTXPriority
		types, blocks = append(types, blockConsole), append(blocks, b)
	}
	if p.Header != nil {
		b, err := MarshalHeaderWord(*p.Header)
		if err != nil {
			return nil, err
		}
		types, blocks = append(types, blockHeader), append(blocks, b)
	}
	if p.Voice != nil {
		b, err := MarshalVoice(*p.Voice)
		if err != nil {
			return nil, err
		}
		types, blocks = append(types, blockVoice), append(blocks, b)
	}
	// BACA-B-2 Figure 43: E/type7, timestamp offset14, block length10.
	payload := make([]byte, 1+4*len(blocks))
	payload[0] = byte(len(blocks)) | bit(signal(p.Control.Type), 7)
	for i, b := range blocks {
		binary.BigEndian.PutUint32(payload[1+i*4:], uint32(0x80|types[i])<<24|uint32(len(b)))
	}
	for _, b := range blocks {
		payload = append(payload, b...)
	}
	wire := rtp.Packet{Header: rtp.Header{Version: 2, PayloadType: PayloadType, SequenceNumber: p.Sequence, Timestamp: p.Timestamp, SSRC: uint32(p.Role)}, Payload: payload}
	return wire.Marshal()
}

// MarshalHeaderWord serializes the 18-byte FullRate header without an RTP wrapper.
func MarshalHeaderWord(h HeaderWord) ([]byte, error) {
	if err := h.validate(); err != nil {
		return nil, err
	}
	b := make([]byte, 18)
	copy(b, h.MI[:])
	b[9] = h.AlgorithmID
	binary.BigEndian.PutUint16(b[10:], h.KeyID)
	b[12] = h.MFID
	binary.BigEndian.PutUint16(b[13:], h.GroupID)
	binary.BigEndian.PutUint16(b[15:], h.NID)
	b[17] = h.Superframe<<6 | h.VoiceBlockBundling<<4
	return b, nil
}

// MarshalVoice serializes supplied observations without synthesizing FEC or metadata.
func MarshalVoice(v FullRateVoice) ([]byte, error) {
	if err := v.validate(); err != nil {
		return nil, err
	}
	q := v.Quality
	b := make([]byte, 14)
	b[0] = byte(v.Type)
	copy(b[1:12], v.IMBE[:])
	b[12] = q.TotalErrors<<5 | q.ScaledErrors<<2 | bit(q.Mute, 1) | bit(q.Lost, 0)
	b[13] = q.U4Errors<<7 | q.U1Errors<<4 | q.Superframe<<2
	if v.LSD != nil {
		b = append(b, v.LSD.Words[0], v.LSD.Words[1], v.LSD.Status[0]<<2|v.LSD.Status[1])
	}
	if v.ES != nil {
		w := v.ES.Words
		n := uint32(w[0])<<18 | uint32(w[1])<<12 | uint32(w[2])<<6 | uint32(w[3])
		b = append(b, byte(n>>16), byte(n>>8), byte(n), v.ES.Status)
	}
	return b, nil
}

// ParseHeaderWord parses and owns the fixed 18-byte header.
func ParseHeaderWord(b []byte) (HeaderWord, error) {
	if len(b) != 18 || b[17]&15 != 0 {
		return HeaderWord{}, fmt.Errorf("p25rtp: invalid HeaderWord length or reserved bits")
	}
	h := HeaderWord{AlgorithmID: b[9], KeyID: binary.BigEndian.Uint16(b[10:]), MFID: b[12], GroupID: binary.BigEndian.Uint16(b[13:]), NID: binary.BigEndian.Uint16(b[15:]), Superframe: b[17] >> 6, VoiceBlockBundling: (b[17] >> 4) & 3}
	copy(h.MI[:], b[:9])
	if err := h.validate(); err != nil {
		return HeaderWord{}, err
	}
	return h, nil
}
func (h HeaderWord) validate() error {
	if h.Superframe != 0 || h.VoiceBlockBundling != 1 {
		return fmt.Errorf("p25rtp: HeaderWord requires initial SF0 and profile VBB1")
	}
	return nil
}

// Parse validates one VBB1 datagram and copies all accepted voice/metadata bytes.
// It retains no reference to data and changes no session, sequence or TSN state.
func Parse(data []byte) (Packet, error) {
	var p Packet
	if len(data) < 13 || len(data) > maxPacketSize || data[0] != 0x80 || data[1] != PayloadType {
		return p, fmt.Errorf("p25rtp: invalid FullRate RTP header or size")
	}
	var wire rtp.Packet
	if err := wire.Unmarshal(data); err != nil {
		return p, fmt.Errorf("p25rtp: RTP: %w", err)
	}
	p.Role, p.Sequence, p.Timestamp = Role(wire.SSRC), wire.SequenceNumber, wire.Timestamp
	b := wire.Payload
	count := int(b[0] & 0x3f)
	if b[0]&0x40 != 0 || count < 1 || count > 4 || len(b) < 1+4*count {
		return Packet{}, fmt.Errorf("p25rtp: invalid verbose block count")
	}
	pos := 1 + 4*count
	for i := range count {
		h := binary.BigEndian.Uint32(b[1+4*i:])
		typ, offset, length := uint8(h>>24)&0x7f, (h>>10)&0x3fff, int(h&0x3ff)
		if h>>31 != 1 || offset != 0 || length > len(b)-pos {
			return Packet{}, fmt.Errorf("p25rtp: invalid block %d header or length", i)
		}
		block := b[pos : pos+length]
		pos += length
		if i == 0 && typ != blockPacket {
			return Packet{}, fmt.Errorf("p25rtp: PacketType must be first")
		}
		switch typ {
		case blockPacket:
			if i != 0 || length != 4 {
				return Packet{}, fmt.Errorf("p25rtp: duplicate or invalid PacketType")
			}
			p.Control = PacketTypeControl{Type: PacketType(block[0] & 0x7f), Mute: block[0]&0x80 != 0, ServiceOptions: block[1], TSN: block[2] >> 1, Losing: block[2]&1 != 0, Interval: block[3]}
		case blockRF:
			if i != 1 || length != 8 || p.Console != nil || p.RF != nil {
				return Packet{}, fmt.Errorf("p25rtp: invalid RF control order or length")
			}
			p.RF = &RFControlWord{Source: getSUID(block), TransmitPriority: block[7]}
		case blockConsole:
			if i != 1 || length != 24 || p.RF != nil || p.Console != nil {
				return Packet{}, fmt.Errorf("p25rtp: invalid console control order or length")
			}
			p.Console = &ConsoleControlWord{Console: getSUID(block), TransmitPriority: block[7], PhysicalConsole: getSUID(block[8:]), TransmissionDescriptor: block[15], VoiceSource: getSUID(block[16:]), ConsoleTXPriority: block[23]}
		case blockHeader:
			if i < 2 || p.Header != nil || p.Voice != nil {
				return Packet{}, fmt.Errorf("p25rtp: invalid HeaderWord order")
			}
			h, err := ParseHeaderWord(block)
			if err != nil {
				return Packet{}, err
			}
			p.Header = &h
		case blockVoice:
			if i < 2 || p.Voice != nil {
				return Packet{}, fmt.Errorf("p25rtp: invalid or unsupported voice block order/count")
			}
			v, err := ParseVoice(block)
			if err != nil {
				return Packet{}, err
			}
			p.Voice = &v
		default:
			return Packet{}, fmt.Errorf("p25rtp: unsupported block type %d", typ)
		}
	}
	if pos != len(b) || (b[0]&0x80 != 0) != signal(p.Control.Type) {
		return Packet{}, fmt.Errorf("p25rtp: trailing data or incorrect signal bit")
	}
	if err := p.validate(); err != nil {
		return Packet{}, err
	}
	return p, nil
}

func (p Packet) validate() error {
	c := p.Control
	if (p.Role != GroupHome && p.Role != GroupServing) || c.Type > HeartbeatQuery || c.TSN > 127 {
		return fmt.Errorf("p25rtp: invalid group role, PacketType or TSN")
	}
	if (c.Type == HeartbeatQuery && c.TSN != 0) || (c.Type != Heartbeat && c.Type != HeartbeatQuery && c.TSN == 0) {
		return fmt.Errorf("p25rtp: invalid TSN for PacketType")
	}
	if (c.Type == Request && p.Role != GroupServing) || ((c.Type == Start || c.Type == Grant || c.Type == Wait || c.Type == Deny) && p.Role != GroupHome) {
		return fmt.Errorf("p25rtp: wrong source role for PacketType")
	}
	source := c.Type == Request || c.Type == Start || c.Type == Progress
	if source != (p.RF != nil || p.Console != nil) || (p.RF != nil && p.Console != nil) {
		return fmt.Errorf("p25rtp: exactly one PTT control word required for Request/Start/Progress only")
	}
	if p.RF != nil && (!validSUID(p.RF.Source) || p.RF.TransmitPriority>>4 > 2) {
		return fmt.Errorf("p25rtp: invalid RF identifier or transmit priority")
	}
	if p.Console != nil {
		w := p.Console
		if !validSUID(w.Console) || !validSUID(w.PhysicalConsole) || !validSUID(w.VoiceSource) || w.TransmitPriority>>4 > 2 || w.TransmissionDescriptor != 0 {
			return fmt.Errorf("p25rtp: invalid or unsupported console control")
		}
	}
	if (p.Header != nil || p.Voice != nil) && c.Type != Request && c.Type != Progress {
		return fmt.Errorf("p25rtp: header/voice requires Request or Progress")
	}
	if p.Header != nil {
		if err := p.Header.validate(); err != nil {
			return err
		}
	}
	if p.Voice != nil {
		if err := p.Voice.validate(); err != nil {
			return err
		}
	}

	return nil
}

func (v FullRateVoice) validate() error {
	q := v.Quality
	n := voiceLength(v.Type)
	if n == 0 || q.TotalErrors > 7 || q.ScaledErrors > 7 || q.U4Errors > 1 || q.U1Errors > 7 || q.Superframe > 3 {
		return fmt.Errorf("p25rtp: invalid FullRate type or quality")
	}
	if (n == 17) != (v.LSD != nil) || (n == 18) != (v.ES != nil) {
		return fmt.Errorf("p25rtp: frame-specific LSD/ES required exactly for its frame type")
	}
	if v.LSD != nil && (v.LSD.Status[0] > 3 || v.LSD.Status[1] > 3) {
		return fmt.Errorf("p25rtp: invalid LSD status")
	}
	if v.ES != nil {
		e := v.ES
		if e.Status > 80 || e.Words[0] > 63 || e.Words[1] > 63 || e.Words[2] > 63 || e.Words[3] > 63 {
			return fmt.Errorf("p25rtp: invalid ES symbols or ternary status")
		}
	}
	return nil
}

// ParseVoice parses and owns one 14-, 17- or 18-byte FullRate block.
func ParseVoice(b []byte) (FullRateVoice, error) {
	var v FullRateVoice
	if len(b) < 14 || voiceLength(FrameType(b[0])) != len(b) || b[13]&3 != 0 {
		return v, fmt.Errorf("p25rtp: invalid FullRate length, frame type or reserved bits")
	}
	v.Type = FrameType(b[0])
	copy(v.IMBE[:], b[1:12])
	v.Quality = Quality{TotalErrors: b[12] >> 5, ScaledErrors: (b[12] >> 2) & 7, Mute: b[12]&2 != 0, Lost: b[12]&1 != 0, U4Errors: b[13] >> 7, U1Errors: (b[13] >> 4) & 7, Superframe: (b[13] >> 2) & 3}
	if len(b) == 17 {
		if b[16]&0xf0 != 0 {
			return FullRateVoice{}, fmt.Errorf("p25rtp: nonzero LSD reserved bits")
		}
		v.LSD = &LowSpeedData{Words: [2]byte{b[14], b[15]}, Status: [2]uint8{b[16] >> 2, b[16] & 3}}
	}
	if len(b) == 18 {
		if b[17]&0x80 != 0 {
			return FullRateVoice{}, fmt.Errorf("p25rtp: nonzero ES reserved bit")
		}
		n := uint32(b[14])<<16 | uint32(b[15])<<8 | uint32(b[16])
		v.ES = &EncryptionSync{Words: [4]uint8{uint8(n >> 18), uint8(n>>12) & 63, uint8(n>>6) & 63, uint8(n) & 63}, Status: b[17]}
	}
	if err := v.validate(); err != nil {
		return FullRateVoice{}, err
	}
	return v, nil
}

func voiceLength(t FrameType) int {
	switch t {
	case FR1, FR2, FR3, FR4, FR5, FR6, FR7, FR8, FR10, FR11:
		return 14
	case FR9, FR18:
		return 17
	case FR12, FR13, FR14, FR15, FR16, FR17:
		return 18
	default:
		return 0
	}
}

func signal(t PacketType) bool {
	return t == Start || t == Grant || t == Progress || t == Mute || t == Unmute
}

func validSUID(s SUID) bool { return s.WACN <= 0xfffff && s.System <= 0xfff && s.Unit <= 0xffffff }

func putSUID(b []byte, s SUID) {
	binary.BigEndian.PutUint32(b, s.WACN<<12|uint32(s.System))
	b[4], b[5], b[6] = byte(s.Unit>>16), byte(s.Unit>>8), byte(s.Unit)
}

func getSUID(b []byte) SUID {
	n := binary.BigEndian.Uint32(b)
	return SUID{WACN: n >> 12, System: uint16(n & 0xfff), Unit: uint32(b[4])<<16 | uint32(b[5])<<8 | uint32(b[6])}
}

func bit(set bool, shift uint) uint8 {
	if set {
		return 1 << shift
	}
	return 0
}
