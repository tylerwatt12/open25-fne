// Package dispatch owns operator audio sessions. It knows neither modem
// devices nor the configuration database; an authenticated routing boundary
// decides which network and RF legs a call is allowed to use.
package dispatch

import (
	"context"
	"errors"
	"time"
	"open25.local/open25-fne/internal/p25crypto"
	"open25.local/open25-fne/internal/p25rtp"
)

const (
	SampleRate    = 8000
	FrameSamples  = 160
	FrameBytes    = FrameSamples * 2
	FrameDuration = 20 * time.Millisecond
	MaxGroups     = 32
)

var (
	ErrBusy                      = errors.New("the selected talkgroup is busy")
	ErrUnavailable               = errors.New("audio routing is unavailable")
	ErrPermission                = errors.New("this action is not permitted for this radio ID")
	ErrP25MetadataNotSupported   = errors.New("P25 FullRate metadata is not supported by this media source")
	ErrP25SynchronizationPending = errors.New("waiting for P25 encryption synchronization")
)

type Principal struct {
	UserID           string `json:"user_id"`
	AutomationID     string `json:"automation_id,omitempty"`
	AutomationAction string `json:"automation_action,omitempty"`
	RadioID          int    `json:"radio_id"`
	ReceiveGroups    []int  `json:"receive_groups"`
	TransmitGroups   []int  `json:"transmit_groups"`
	CanTransmit      bool   `json:"can_transmit"`
	CanSecure        bool   `json:"can_secure"`
}

// KeyIdentity is public metadata for a key loaded by the trusted endpoint.
type KeyIdentity struct {
	AlgorithmID int `json:"algorithm_id"`
	KeyID       int `json:"key_id"`
}

type CallRequest struct {
	ID string `json:"id"`
	// LogicalCallID follows one conversation across one-use Site allocations.
	// A service's first accepting broker sets it; a receiving Site accepts it
	// only from its enrolled Zone. It is never a browser-supplied identity.
	LogicalCallID    string `json:"logical_call_id"`
	SessionID        string `json:"session_id"`
	UserID           string `json:"user_id"`
	AutomationID     string `json:"automation_id,omitempty"`
	AutomationAction string `json:"automation_action,omitempty"`
	RadioID          int    `json:"radio_id"`
	Talkgroups       []int  `json:"talkgroups"`
	Emergency        bool   `json:"emergency"`
	Priority         int    `json:"priority"`
	Secure           bool   `json:"secure"`
	AlgorithmID      int    `json:"algorithm_id"`
	KeyID            int    `json:"key_id"`
	// An RF AllStart reservation precedes the first actual voice/key identity.
	EncryptionPending   bool  `json:"encryption_pending,omitempty"`
	AuthorizationGroups []int `json:"authorization_groups,omitempty"`
}

// Grant is an opaque route identity. It never means that RF was requested or
// granted: the router may legitimately admit an IP-only call with zero RF legs.
type Grant struct {
	ID          string `json:"id"`
	Talkgroups  []int  `json:"talkgroups"`
	Secure      bool   `json:"secure"`
	AlgorithmID int    `json:"algorithm_id"`
	KeyID       int    `json:"key_id"`
}

type Frame struct {
	CallID              string            `json:"call_id"`
	LogicalCallID       string            `json:"logical_call_id,omitempty"`
	TalkgroupID         int               `json:"talkgroup_id"`
	SourceID            int               `json:"source_id"`
	SourceAlias         string            `json:"source_alias,omitempty"`
	SourceKind          string            `json:"source_kind,omitempty"`
	Sequence            uint64            `json:"sequence"`
	SourceEpoch         uint64            `json:"source_epoch"`
	PCM                 []byte            `json:"pcm,omitempty"`
	Encoded             []byte            `json:"encoded,omitempty"`
	P25                 *P25VoiceMetadata `json:"p25,omitempty"`
	ManufacturerID      uint8             `json:"mfid"`
	AlgorithmID         uint8             `json:"algorithm_id,omitempty"`
	KeyID               uint16            `json:"key_id,omitempty"`
	MessageIndicator    []byte            `json:"message_indicator,omitempty"`
	VoiceIndex          uint8             `json:"voice_index,omitempty"`
	CapturedAt          time.Time         `json:"captured_at"`
	End                 bool              `json:"end"`
	Emergency           bool              `json:"emergency"`
	ServiceOptions      uint8             `json:"service_options"`
	Encrypted           bool              `json:"encrypted"`
	ReceivedEncrypted   bool              `json:"-"`
	ReceivedAlgorithmID int               `json:"-"`
	ReceivedKeyID       int               `json:"-"`
}

// P25VoiceMetadata contains the source's FullRate metadata only. Encoded is
// the sole copy of the 88-bit word. Header is absent when no header was received.
type P25VoiceMetadata struct {
	Type    p25rtp.FrameType       `json:"type"`
	Quality p25rtp.Quality         `json:"quality"`
	LSD     *p25rtp.LowSpeedData   `json:"lsd,omitempty"`
	ES      *p25rtp.EncryptionSync `json:"es,omitempty"`
	Header  *p25rtp.HeaderWord     `json:"header,omitempty"`
}

func (m *P25VoiceMetadata) Clone() *P25VoiceMetadata {
	if m == nil {
		return nil
	}
	n := *m
	if m.LSD != nil {
		v := *m.LSD
		n.LSD = &v
	}
	if m.ES != nil {
		v := *m.ES
		n.ES = &v
	}
	if m.Header != nil {
		v := *m.Header
		n.Header = &v
	}
	return &n
}

// WithGroup translates only the destination and its public header field.
// Source identity, ciphertext and receiver observations remain unchanged.
func (f Frame) WithGroup(group int) Frame {
	f.TalkgroupID = group
	f.P25 = f.P25.Clone()
	if f.P25 != nil && f.P25.Header != nil {
		f.P25.Header.GroupID = uint16(group)
	}
	return f
}

func (f Frame) WithEmergency(emergency bool) Frame {
	f.Emergency = emergency
	f.ServiceOptions &^= 0x80
	if emergency {
		f.ServiceOptions |= 0x80
	}
	return f
}

// MediaValid checks the media representation without interpreting ciphertext.
// PCM is clear audio at an authorized endpoint; encoded IMBE may pass through
// Sites and Zone without traffic keys. No key material belongs in a Frame.
func (f Frame) MediaValid() bool {
	if f.ManufacturerID != 0 && f.ManufacturerID != 1 {
		return false
	}
	if f.End {
		return len(f.PCM) == 0 && len(f.Encoded) == 0
	}
	if len(f.PCM) == FrameBytes && len(f.Encoded) == 0 {
		return !f.Encrypted && (f.AlgorithmID == 0 || f.AlgorithmID == 0x80) && f.KeyID == 0 && len(f.MessageIndicator) == 0
	}
	if len(f.PCM) != 0 || len(f.Encoded) != 11 || f.VoiceIndex > 17 || f.P25 == nil {
		return false
	}
	if (f.ServiceOptions&0x80 != 0) != f.Emergency || (f.ServiceOptions&0x40 != 0) != f.Encrypted {
		return false
	}
	if _, _, err := fullRatePayload(f); err != nil {
		return false
	}
	if f.AlgorithmID == p25crypto.Clear {
		return !f.Encrypted && f.KeyID == 0
	}
	// Encoded transport preserves raw Header/ESS. Public synchronization is
	// acquired at native endpoints; a relay neither derives nor fabricates MI.
	return p25crypto.Supported(int(f.AlgorithmID)) && f.Encrypted
}

// Router must serialize floors across all dispatch servers/sites and enforce
// radio/talkgroup/site permissions. A console subscription is never an RF
// affiliation. Begin must not allocate any RF leg without eligible RF interest.
// Every operation must honor cancellation. End must be idempotent.
type Router interface {
	Begin(context.Context, CallRequest) (Grant, error)
	Write(context.Context, Grant, Frame) error
	Renew(context.Context, Grant) error
	End(context.Context, Grant) error
}

// MediaGaps marks a missing media interval without inventing a voice frame.
// Encoded endpoints discard partial LDUs and resume at a complete boundary.
type MediaGaps interface {
	Gap(context.Context, Grant) error
}

// MediaSpurts ends actual speech while its admitted allocation remains held.
type MediaSpurts interface {
	EndSpurt(context.Context, Grant) error
}

type Event struct {
	At           time.Time `json:"at"`
	Kind         string    `json:"kind"`
	UserID       string    `json:"user_id"`
	AutomationID string    `json:"automation_id,omitempty"`
	RadioID      int       `json:"radio_id"`
	CallID       string    `json:"call_id"`
	Talkgroups   []int     `json:"talkgroups"`
	Outcome      string    `json:"outcome"`
	Message      string    `json:"message"`
}

type Options struct {
	Router Router
	// Available reports current in-memory readiness. It must not block or call Hub.
	Available          func() bool
	Event              func(Event)
	Lease              time.Duration
	MaxTransmit        time.Duration
	GrantTimeout       time.Duration
	AudioIdle          time.Duration
	MaxAudioAge        time.Duration
	ICEPortMin         uint16
	ICEPortMax         uint16
	LoopbackCandidates bool
	MaxSessions        int
}

// AudioSessions is implemented by a network controller that owns identities
// across several Dispatch servers. It never grants a transmit floor.
type AudioSessions interface {
	ClaimAudio(context.Context, Principal, string) error
	RenewAudio(context.Context, Principal, string) error
	ReleaseAudio(context.Context, Principal, string) error
}

type TXState struct {
	ID          string `json:"id,omitempty"`
	RequestID   string `json:"request_id,omitempty"`
	Talkgroups  []int  `json:"talkgroups"`
	State       string `json:"state"`
	Secure      bool   `json:"secure"`
	AlgorithmID int    `json:"algorithm_id"`
	KeyID       int    `json:"key_id"`
}

type Floor struct {
	CallID      string `json:"call_id"`
	RadioID     int    `json:"radio_id"`
	TalkgroupID int    `json:"talkgroup_id"`
	State       string `json:"state"`
}

type State struct {
	SessionID      string  `json:"session_id,omitempty"`
	RadioID        int     `json:"radio_id"`
	ReceiveGroups  []int   `json:"receive_groups"`
	TransmitGroups []int   `json:"transmit_groups"`
	Subscriptions  []int   `json:"subscriptions"`
	TX             TXState `json:"tx"`
	EndedRequestID string  `json:"ended_request_id,omitempty"`
	Floors         []Floor `json:"floors"`
	MediaConnected bool    `json:"media_connected"`
	Available      bool    `json:"available"`
}
