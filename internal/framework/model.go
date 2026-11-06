// Package framework contains independent System, Zone, Site and Application services.
package framework

import "time"

type Role string

const (
	RoleSystem      Role = "system"
	RoleZone        Role = "zone"
	RoleSite        Role = "site"
	RoleApplication Role = "application"
)

func (r Role) Valid() bool {
	return r == RoleSystem || r == RoleZone || r == RoleSite || r == RoleApplication
}

type SystemConfig struct {
	ID               string            `json:"id"`
	Name             string            `json:"name"`
	WACN             int               `json:"wacn"`
	SystemID         int               `json:"system_id"`
	Callsign         string            `json:"callsign"`
	VoiceMFID        int               `json:"voice_mfid"`
	BSIProfile       string            `json:"bsi_profile"`
	Radios           []Radio           `json:"radios"`
	Talkgroups       []Talkgroup       `json:"talkgroups"`
	StatusLabels     []SignalLabel     `json:"status_labels"`
	MessageLabels    []SignalLabel     `json:"message_labels"`
	ConsolePositions []ConsolePosition `json:"console_positions"`
}

type SignalLabel struct {
	ID    int    `json:"id"`
	Label string `json:"label"`
}

type Radio struct {
	Group   string `json:"group"`
	RID     int    `json:"rid"`
	Alias   string `json:"alias"`
	Enabled bool   `json:"enabled"`
	Kind    string `json:"kind"`
}

type Talkgroup struct {
	Group               string   `json:"group"`
	AffiliationAccess   string   `json:"affiliation_access"`
	AffiliationRadioIDs []int    `json:"affiliation_radio_ids"`
	TransmitAccess      string   `json:"transmit_access"`
	TransmitRadioIDs    []int    `json:"transmit_radio_ids"`
	TGID                int      `json:"tgid"`
	Alias               string   `json:"alias"`
	Enabled             bool     `json:"enabled"`
	SiteAccess          string   `json:"site_access"`
	SiteIDs             []string `json:"site_ids"`
	StartPolicy         string   `json:"start_policy"`
	Priority            int      `json:"priority"`
	AdmissionMode       string   `json:"admission_mode"`
	TransmitMode        string   `json:"transmit_mode"`
	AlgorithmID         int      `json:"algorithm_id"`
	KeyID               int      `json:"key_id"`
}

type ZoneConfig struct {
	ID                 string              `json:"id"`
	SystemID           string              `json:"system_id"`
	Name               string              `json:"name"`
	RFSSID             int                 `json:"rfss_id"`
	ChannelIdentifiers []ChannelIdentifier `json:"channel_identifiers"`
	Sites              []SiteIdentity      `json:"sites"`
}

type ChannelIdentifier struct {
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
	AdminURL string `json:"admin_url"`
}

type SiteConfig struct {
	BroadcastBandPlan bool           `json:"broadcast_band_plan"`
	ID                string         `json:"id"`
	Modems            []Modem        `json:"modems"`
	Channels          []Channel      `json:"channels"`
	Identification    Identification `json:"identification"`
	ControlPolicy     ControlPolicy  `json:"control_policy"`
}

// ControlPolicy changes runtime selection, never saved channel assignments.
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

type ApplicationConfig struct {
	Name          string `json:"name"`
	ConnectionURL string `json:"connection_url"`
	LocalSiteURL  string `json:"local_site_url"`
}

type Event struct {
	ID      int64     `json:"id"`
	At      time.Time `json:"at"`
	Kind    string    `json:"kind"`
	Message string    `json:"message"`
}
