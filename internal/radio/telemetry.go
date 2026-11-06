package radio

import (
	"encoding/json"
	"regexp"
	"strconv"
	"strings"
	"time"
)

// These are bounded observations of one native worker, not airtime accounting
// or a durable history. A missing measurement is never represented by zero.
const modemSampleLimit = 120
const modemReceiveLimit = 24

type ModemSample struct {
	At      time.Time `json:"at"`
	TX      *bool     `json:"tx"`
	RX      *bool     `json:"rx"`
	RXState string    `json:"rx_state"`
}

type ReceiveStatistics struct {
	At            time.Time `json:"at"`
	NativeAt      string    `json:"native_at,omitempty"`
	Frames        uint64    `json:"frames"`
	ProtectedBits uint64    `json:"protected_bits"`
	Errors        uint64    `json:"errors"`
	UndecodableLC uint64    `json:"undecodable_lc"`
	BERPercent    float64   `json:"ber_percent"`
}

type ModemTelemetry struct {
	StartedAt   time.Time           `json:"started_at"`
	Samples     []ModemSample       `json:"samples"`
	Receive     []ReceiveStatistics `json:"receive"`
	RXOverflows uint64              `json:"rx_overflows"`
	TXOverflows uint64              `json:"tx_overflows"`
	ErrorsAt    time.Time           `json:"errors_at"`
}

func (t *ModemTelemetry) begin(started time.Time) {
	if !t.StartedAt.Equal(started) {
		*t = ModemTelemetry{StartedAt: started}
	}
}

func (t *ModemTelemetry) sample(data map[string]json.RawMessage, at time.Time) {
	sample := ModemSample{At: at}
	if json.Unmarshal(data["tx"], &sample.TX) != nil {
		sample.TX = nil
	}
	var state *int
	if json.Unmarshal(data["p25RxState"], &state) == nil && state != nil {
		states := []string{"Listening", "Late entry", "Receiving voice", "Receiving data", "Rejected", "Invalid"}
		if *state >= 0 && *state < len(states) {
			sample.RXState = states[*state]
			active := *state == 1 || *state == 2 || *state == 3
			sample.RX = &active
		}
	}
	t.Samples = append(t.Samples, sample)
	if len(t.Samples) > modemSampleLimit {
		copy(t.Samples, t.Samples[len(t.Samples)-modemSampleLimit:])
		t.Samples = t.Samples[:modemSampleLimit]
	}
}

var receiveStatisticsLine = regexp.MustCompile(`total frames: ([0-9]+), bits: ([0-9]+), undecodable LC: ([0-9]+), errors: ([0-9]+), BER: ([0-9]+(?:\.[0-9]+)?)%`)

func (t *ModemTelemetry) observe(line string, at time.Time) {
	if strings.Contains(line, "(MODEM)") {
		if strings.Contains(line, "RX buffer has overflowed") {
			t.RXOverflows++
			t.ErrorsAt = at
		}
		if strings.Contains(line, "TX buffer has overflowed") {
			t.TXOverflows++
			t.ErrorsAt = at
		}
	}
	if !strings.Contains(line, "(RF)") || !strings.Contains(line, "P25") {
		return
	}
	m := receiveStatisticsLine.FindStringSubmatch(line)
	if m == nil {
		return
	}
	values := [4]uint64{}
	for i := range values {
		value, err := strconv.ParseUint(m[i+1], 10, 64)
		if err != nil {
			return
		}
		values[i] = value
	}
	ber, err := strconv.ParseFloat(m[5], 64)
	if err != nil {
		return
	}
	// The native receiver supplies this estimate and its denominator. Do not
	// present it as a complete channel BER or recalculate from rounded text.
	stats := ReceiveStatistics{At: at, Frames: values[0], ProtectedBits: values[1], UndecodableLC: values[2], Errors: values[3], BERPercent: ber}
	if stamp := nativeTime.FindStringSubmatch(line); len(stamp) == 2 {
		stats.NativeAt = stamp[1]
	}
	t.Receive = append(t.Receive, stats)
	if len(t.Receive) > modemReceiveLimit {
		copy(t.Receive, t.Receive[len(t.Receive)-modemReceiveLimit:])
		t.Receive = t.Receive[:modemReceiveLimit]
	}
}

func (t ModemTelemetry) clone() ModemTelemetry {
	t.Samples = append([]ModemSample{}, t.Samples...)
	for i := range t.Samples {
		if value := t.Samples[i].TX; value != nil {
			copy := *value
			t.Samples[i].TX = &copy
		}
		if value := t.Samples[i].RX; value != nil {
			copy := *value
			t.Samples[i].RX = &copy
		}
	}
	t.Receive = append([]ReceiveStatistics{}, t.Receive...)
	return t
}
