package radio

import "time"

// C4FMTestRequest describes one transient operation, never saved programming.
type C4FMTestRequest struct {
	ChannelID       string `json:"channel_id"`
	DurationSeconds int    `json:"duration_seconds"`
}

// C4FMTestStatus reports native observations, not measured RF quality.
type C4FMTestStatus struct {
	State           string    `json:"state"`
	ChannelID       string    `json:"channel_id"`
	ChannelName     string    `json:"channel_name"`
	ModemID         string    `json:"modem_id"`
	TXHz            int64     `json:"tx_frequency_hz"`
	PowerPercent    int       `json:"power_percent"`
	DurationSeconds int       `json:"duration_seconds"`
	Pattern         string    `json:"pattern"`
	StartedAt       time.Time `json:"started_at,omitempty"`
	EndsAt          time.Time `json:"ends_at,omitempty"`
	FinishedAt      time.Time `json:"finished_at,omitempty"`
	TXObserved      bool      `json:"tx_observed"`
	StopConfirmed   bool      `json:"stop_confirmed"`
	Error           string    `json:"error,omitempty"`
}
