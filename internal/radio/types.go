// Package radio supervises the native Phase 1 radio engines for one Site.
// It owns no catalogue database and never reaches through to another service.
package radio

import (
	"context"
	"encoding/json"
	"time"
)

type Profile struct {
	System     System        `json:"system"`
	Zone       Zone          `json:"zone"`
	Site       Site          `json:"site"`
	Network    Network       `json:"network,omitempty"`
	Inhibition InhibitPolicy `json:"inhibition"`
}
type System struct {
	ID         string      `json:"id"`
	Name       string      `json:"name"`
	WACN       int         `json:"wacn"`
	SystemID   int         `json:"system_id"`
	Callsign   string      `json:"callsign"`
	VoiceMFID  int         `json:"voice_mfid"`
	BSIProfile string      `json:"bsi_profile"`
	Radios     []Radio     `json:"radios"`
	Talkgroups []Talkgroup `json:"talkgroups"`
}
type Radio struct {
	RID     int    `json:"rid"`
	Alias   string `json:"alias"`
	Enabled bool   `json:"enabled"`
	Kind    string `json:"kind,omitempty"`
}
type Talkgroup struct {
	AffiliationAccess   string   `json:"affiliation_access"`
	AffiliationRadioIDs []int    `json:"affiliation_radio_ids"`
	TransmitAccess      string   `json:"transmit_access"`
	TransmitRadioIDs    []int    `json:"transmit_radio_ids"`
	StartPolicy         string   `json:"start_policy"`
	Priority            int      `json:"priority"`
	AdmissionMode       string   `json:"admission_mode"`
	TransmitMode        string   `json:"transmit_mode"`
	AlgorithmID         int      `json:"algorithm_id"`
	KeyID               int      `json:"key_id"`
	TGID                int      `json:"tgid"`
	Alias               string   `json:"alias"`
	Enabled             bool     `json:"enabled"`
	SiteAccess          string   `json:"site_access"`
	SiteIDs             []string `json:"site_ids"`
}
type Zone struct {
	ID                 string         `json:"id"`
	SystemID           string         `json:"system_id"`
	Name               string         `json:"name"`
	RFSSID             int            `json:"rfss_id"`
	ChannelIdentifiers []Identifier   `json:"channel_identifiers"`
	Sites              []SiteIdentity `json:"sites"`
}
type Identifier struct {
	Identifier  int   `json:"identifier"`
	BaseHz      int64 `json:"base_frequency_hz"`
	SpacingHz   int64 `json:"channel_spacing_hz"`
	OffsetHz    int64 `json:"uplink_offset_hz"`
	BandwidthHz int64 `json:"bandwidth_hz"`
}
type SiteIdentity struct {
	ID       string `json:"id"`
	Name     string `json:"name"`
	SiteID   int    `json:"site_id"`
	LRA      int    `json:"lra"`
	NAC      int    `json:"nac"`
	Endpoint string `json:"endpoint"`
}
type Site struct {
	BroadcastBandPlan bool           `json:"broadcast_band_plan"`
	ID                string         `json:"id"`
	Modems            []Modem        `json:"modems"`
	Channels          []Channel      `json:"channels"`
	Identification    Identification `json:"identification"`
	ControlPolicy     ControlPolicy  `json:"control_policy"`
}

type ControlPolicy struct {
	Mode            string  `json:"mode"`
	RotationEnabled bool    `json:"rotation_enabled"`
	RotationHours   float64 `json:"rotation_hours"`
}
type Modem struct {
	ID          string      `json:"id"`
	Name        string      `json:"name"`
	Device      string      `json:"device"`
	Baud        int         `json:"baud"`
	Calibration Calibration `json:"calibration"`
}
type Calibration struct {
	RXOffsetHz int64   `json:"rx_offset_hz"`
	TXOffsetHz int64   `json:"tx_offset_hz"`
	RXLevel    float64 `json:"rx_level"`
	TXLevel    float64 `json:"tx_level"`
	RXDCOffset int     `json:"rx_dc_offset"`
	TXDCOffset int     `json:"tx_dc_offset"`
	RXInvert   bool    `json:"rx_invert"`
	TXInvert   bool    `json:"tx_invert"`
	PTTInvert  bool    `json:"ptt_invert"`
}
type Channel struct {
	ID              string `json:"id"`
	Name            string `json:"name"`
	Role            string `json:"role"`
	Identifier      int    `json:"identifier"`
	ChannelNo       int    `json:"channel_no"`
	TXHz            int64  `json:"tx_frequency_hz"`
	RXHz            int64  `json:"rx_frequency_hz"`
	PowerPercent    int    `json:"power_percent"`
	ModemID         string `json:"modem_id"`
	Enabled         bool   `json:"enabled"`
	ControlEligible bool   `json:"control_eligible"`
	ControlPriority int    `json:"control_priority"`
}
type Identification struct {
	Enabled      bool   `json:"enabled"`
	Mode         string `json:"mode"`
	WPM          int    `json:"wpm"`
	QuietSeconds int    `json:"quiet_seconds"`
}
type Network struct {
	Address        string         `json:"address"`
	Port           int            `json:"port"`
	Secret         string         `json:"secret"`
	PeerBase       int            `json:"peer_base"`
	ControlPeerID  int            `json:"control_peer_id"`
	VoicePeerIDs   map[string]int `json:"voice_peer_ids"`
	ConsolePeerIDs map[int]int    `json:"console_peer_ids"`
	RPCSecret      string         `json:"-"`
}

type Options struct {
	StateDir string
	// MaintenanceLockPath is the owning Site's shared startup/exclusive
	// maintenance lock. Empty uses radio StateDir for standalone controllers.
	MaintenanceLockPath string
	EnginePath          string
	FNEPath             string
	AllowRF             bool
	BaseRPCPort         int
	FNEPort             int
	StartupTimeout      time.Duration
	OnEvent             func(Event)
	// OnInhibition durably saves current actual enforcement before it is
	// published. A failed save withdraws RF rather than forgetting protection.
	OnInhibition func([]InhibitRestore) error
}

type Event struct {
	ID             string    `json:"id"`
	At             time.Time `json:"at"`
	NativeAt       string    `json:"native_at,omitempty"`
	SiteID         string    `json:"site_id"`
	ChannelID      string    `json:"channel_id"`
	Kind           string    `json:"kind"`
	Audience       string    `json:"audience"`
	Direction      string    `json:"direction,omitempty"`
	RID            int       `json:"rid,omitempty"`
	TGID           int       `json:"tgid,omitempty"`
	Code           *int      `json:"code,omitempty"`
	EmergencyInfo1 *int      `json:"emergency_si1,omitempty"`
	EmergencyInfo2 *int      `json:"emergency_si2,omitempty"`
	Message        string    `json:"message"`
	Encrypted      bool      `json:"encrypted,omitempty"`
	RequestorID    int       `json:"requestor_id,omitempty"`
	Attempts       int       `json:"attempts,omitempty"`
}

// NativeFNEID is the address used by the native Host for these originated
// extended-function commands. It is distinct from the human/desk audit identity.
const NativeFNEID = 0xFFFFFC

type Affiliation struct {
	RID  int       `json:"rid"`
	TGID int       `json:"tgid"`
	At   time.Time `json:"at"`
}
type Registration struct {
	RID int       `json:"rid"`
	At  time.Time `json:"at"`
}
type ChannelStatus struct {
	Telemetry           ModemTelemetry `json:"telemetry"`
	OperatingModemID    string         `json:"operating_modem_id"`
	OperatingModemName  string         `json:"operating_modem_name"`
	ID                  string         `json:"id"`
	Name                string         `json:"name"`
	Role                string         `json:"role"`
	State               string         `json:"state"`
	PID                 int            `json:"pid"`
	TXHz                int64          `json:"tx_frequency_hz"`
	RXHz                int64          `json:"rx_frequency_hz"`
	PowerPercent        int            `json:"power_percent"`
	Ready               bool           `json:"ready"`
	Busy                bool           `json:"busy"`
	TGID                int            `json:"talkgroup,omitempty"`
	RID                 int            `json:"radio_id,omitempty"`
	Identifying         bool           `json:"identifying"`
	IdentificationState string         `json:"identification_state,omitempty"`
	Error               string         `json:"error,omitempty"`
	RPCPort             int            `json:"rpc_port,omitempty"`
	RESTPort            int            `json:"rest_port,omitempty"`
	StartedAt           time.Time      `json:"started_at,omitempty"`
	ModemID             string         `json:"modem_id"`
	Device              string         `json:"device"`
	ConfiguredRole      string         `json:"configured_role"`
	ControlEligible     bool           `json:"control_eligible"`
	RecoveryWanted      bool           `json:"recovery_wanted"`
	RecoveryAt          time.Time      `json:"recovery_at,omitempty"`
}
type Status struct {
	Available              bool            `json:"available"`
	MaintenanceSupported   bool            `json:"maintenance_supported"`
	State                  string          `json:"state"`
	Mode                   string          `json:"mode"`
	SiteID                 string          `json:"site_id"`
	ZoneConnected          bool            `json:"zone_connected"`
	TrunkingMode           string          `json:"trunking_mode"`
	FNEPID                 int             `json:"fne_pid"`
	FNEReady               bool            `json:"fne_ready"`
	Channels               []ChannelStatus `json:"channels"`
	Registrations          []Registration  `json:"registrations"`
	Affiliations           []Affiliation   `json:"affiliations"`
	Error                  string          `json:"error,omitempty"`
	StartedAt              time.Time       `json:"started_at,omitempty"`
	Inhibition             []InhibitState  `json:"inhibition"`
	InhibitionReady        bool            `json:"inhibition_ready"`
	ActiveControlChannelID string          `json:"active_control_channel_id"`
	ControlChangedAt       time.Time       `json:"control_changed_at,omitempty"`
	NextControlRotation    time.Time       `json:"next_control_rotation,omitempty"`
	ControlSwitchTarget    string          `json:"control_switch_target,omitempty"`
	ControlSwitchReason    string          `json:"control_switch_reason,omitempty"`
	ControlSwitchError     string          `json:"control_switch_error,omitempty"`
}

type Command struct {
	Action string `json:"action"`
	RID    int    `json:"rid"`
	TGID   int    `json:"tgid,omitempty"`
	Code   int    `json:"code,omitempty"`
}

type Native interface {
	Get(context.Context, string) (map[string]json.RawMessage, error)
	Put(context.Context, string, any) (map[string]json.RawMessage, error)
}
