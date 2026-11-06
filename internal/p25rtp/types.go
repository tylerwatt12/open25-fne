// Package p25rtp encodes the current FullRate group-voice RTP wire profile.
// It does not implement SIP, authorization, media pacing, FEC or cryptography.
// See README.md for the exact sources and private profile restrictions.
package p25rtp

const (
	PayloadType = 100
	ClockRate   = 8000
	FrameTicks  = 160 // One 20 ms FullRate voice block.
)

type Role uint32

const (
	GroupHome    Role = 1
	GroupServing Role = 2
)

type PacketType uint8

// TIA-102.BACA-B-2, 4.3.2, Figure 47, PDF 130 (printed 114).
const (
	Request PacketType = iota
	Grant
	Progress
	End
	Start
	Mute
	Unmute
	Wait
	Deny
	Heartbeat
	HeartbeatQuery
)

type PacketTypeControl struct {
	Type           PacketType
	Mute           bool
	ServiceOptions uint8 // Raw source octet; generated sources supply their explicit service policy.
	TSN            uint8 // Seven bits; not the RTP packet sequence number.
	Losing         bool
	Interval       uint8 // Seconds, meaningful for Heartbeat and HeartbeatQuery only.
}

// SUID is the fully qualified 56-bit subscriber or console identifier.
type SUID struct {
	WACN   uint32 // 20 bits
	System uint16 // 12 bits
	Unit   uint32 // 24 bits
}

type RFControlWord struct {
	Source           SUID
	TransmitPriority uint8 // Priority type in the high nibble, level in the low nibble.
}

type ConsoleControlWord struct {
	Console                SUID
	TransmitPriority       uint8
	PhysicalConsole        SUID
	TransmissionDescriptor uint8
	VoiceSource            SUID
	ConsoleTXPriority      uint8
}

type HeaderWord struct {
	MI                 [9]byte
	AlgorithmID        uint8
	KeyID              uint16
	MFID               uint8
	GroupID            uint16
	NID                uint16
	Superframe         uint8
	VoiceBlockBundling uint8
}

// FrameType values follow BACA-B-2 Table 38, PDF 128 (printed 112).
type FrameType uint8

const (
	FR1  FrameType = 0xc2
	FR2  FrameType = 0xc3
	FR3  FrameType = 0xc4
	FR4  FrameType = 0xc5
	FR5  FrameType = 0xc6
	FR6  FrameType = 0xc7
	FR7  FrameType = 0xc8
	FR8  FrameType = 0xc9
	FR9  FrameType = 0x6a
	FR10 FrameType = 0xcb
	FR11 FrameType = 0xcc
	FR12 FrameType = 0x6d
	FR13 FrameType = 0x6e
	FR14 FrameType = 0x6f
	FR15 FrameType = 0x70
	FR16 FrameType = 0x71
	FR17 FrameType = 0x72
	FR18 FrameType = 0x73
)

// Index is the zero-based position in the 18-frame FullRate superframe.
func (t FrameType) Index() (uint8, bool) {
	switch {
	case t >= FR1 && t <= FR8:
		return uint8(t - FR1), true
	case t == FR9:
		return 8, true
	case t == FR10 || t == FR11:
		return uint8(t-FR10) + 9, true
	case t >= FR12 && t <= FR18:
		return uint8(t-FR12) + 11, true
	default:
		return 0, false
	}
}

// Quality contains reported receiver values, not estimates calculated by this codec.
type Quality struct {
	TotalErrors  uint8 // Et, 0..7
	ScaledErrors uint8 // Er, 0..7
	Mute         bool
	Lost         bool
	U4Errors     uint8 // E4, 0..1
	U1Errors     uint8 // E1, 0..7
	Superframe   uint8 // SF, 0..3
}

type LowSpeedData struct {
	Words  [2]byte
	Status [2]uint8 // Si/Sj: 0 no errors, 1/2 corrected errors, 3 erasure.
}

// EncryptionSync retains Hamming-decoded, NOT Reed-Solomon-decoded symbols.
// Status is the raw ternary-weighted status value (0..80), not a bit-error count.
type EncryptionSync struct {
	Words  [4]uint8 // Four six-bit symbols for this FR12..FR17 block.
	Status uint8
}

type FullRateVoice struct {
	Type FrameType
	// IMBE is the 88-bit concatenation U0..U7, MSB first, with FEC removed.
	// Ciphertext remains opaque. This package neither converts nor decodes it.
	IMBE    [11]byte
	Quality Quality
	LSD     *LowSpeedData
	ES      *EncryptionSync
}

// Packet contains one complete RTP datagram. Sequence and Timestamp are supplied
// by the session owner; this stateless codec does not allocate or advance them.
type Packet struct {
	Role      Role
	Sequence  uint16
	Timestamp uint32
	Control   PacketTypeControl
	RF        *RFControlWord
	Console   *ConsoleControlWord
	Header    *HeaderWord
	Voice     *FullRateVoice
}
