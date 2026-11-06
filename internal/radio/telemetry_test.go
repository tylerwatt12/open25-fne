package radio

import (
	"encoding/json"
	"fmt"
	"strings"
	"testing"
	"time"
)

func modemNative(raw string) map[string]json.RawMessage {
	data := map[string]json.RawMessage{}
	_ = json.Unmarshal([]byte(raw), &data)
	return data
}

func TestModemSamplesKeepIndependentTXRXAndUnknownMeasurements(t *testing.T) {
	var telemetry ModemTelemetry
	at := time.Now().UTC()
	for _, raw := range []string{`{"tx":true,"p25RxState":2}`, `{"tx":false,"p25RxState":3}`, `{"tx":true,"p25RxState":0}`, `{}`, `{"tx":"false","p25RxState":99}`, `{"tx":null,"p25RxState":null}`} {
		telemetry.sample(modemNative(raw), at)
	}
	samples := telemetry.Samples
	if !*samples[0].TX || !*samples[0].RX || samples[0].RXState != "Receiving voice" || *samples[1].TX || !*samples[1].RX || !*samples[2].TX || *samples[2].RX {
		t.Fatal("TX and RX were conflated", samples)
	}
	for _, sample := range samples[3:] {
		if sample.TX != nil || sample.RX != nil {
			t.Fatal("unavailable measurement became idle", sample)
		}
	}
	copy := telemetry.clone()
	*copy.Samples[0].TX = false
	if !*telemetry.Samples[0].TX {
		t.Fatal("snapshot shares mutable sample with controller")
	}
	for i := 0; i < modemSampleLimit+10; i++ {
		telemetry.sample(modemNative(`{"tx":false}`), at.Add(time.Duration(i)*time.Second))
	}
	if len(telemetry.Samples) != modemSampleLimit || !telemetry.Samples[0].At.Equal(at.Add(10*time.Second)) {
		t.Fatal("sample history is not bounded in order")
	}
}

func TestModemReceiveStatisticsAreNativeRFReportsOnly(t *testing.T) {
	var telemetry ModemTelemetry
	start := time.Now().UTC()
	telemetry.begin(start)
	line := "I: 2001-01-02 12:34:56.789 (RF) P25, TDU, total frames: 44, bits: 50000, undecodable LC: 2, errors: 70, BER: 0.1400%"
	telemetry.observe(line, start)
	if len(telemetry.Receive) != 1 || telemetry.Receive[0].BERPercent != .1400 || telemetry.Receive[0].Errors != 70 || telemetry.Receive[0].ProtectedBits != 50000 || telemetry.Receive[0].UndecodableLC != 2 || telemetry.Receive[0].NativeAt != "2001-01-02 12:34:56.789" {
		t.Fatal("native report not preserved", telemetry.Receive)
	}
	for _, bad := range []string{strings.Replace(line, "(RF)", "(NET)", 1), strings.Replace(line, "P25", "DMR", 1), strings.Replace(line, "44,", "-1,", 1), strings.Replace(line, "50000", "99999999999999999999999999", 1)} {
		telemetry.observe(bad, start)
	}
	if len(telemetry.Receive) != 1 {
		t.Fatal("unrelated or malformed diagnostic became a measurement")
	}
	for i := 0; i < modemReceiveLimit+5; i++ {
		telemetry.observe(fmt.Sprintf("I: (RF) P25, TDU, total frames: %d, bits: 1, undecodable LC: 0, errors: 0, BER: 0.0000%%", i), start.Add(time.Duration(i)*time.Second))
	}
	if len(telemetry.Receive) != modemReceiveLimit || telemetry.Receive[0].Frames != 5 || telemetry.Receive[len(telemetry.Receive)-1].BERPercent != 0 {
		t.Fatal("real zero or report history was lost")
	}
	telemetry.observe("E: (MODEM) RX buffer has overflowed", start)
	telemetry.observe("E: (MODEM) TX buffer has overflowed", start)
	if telemetry.RXOverflows != 1 || telemetry.TXOverflows != 1 {
		t.Fatal("modem overflow observations lost")
	}
	telemetry.begin(start.Add(time.Second))
	if len(telemetry.Receive) != 0 || telemetry.RXOverflows != 0 || len(telemetry.Samples) != 0 {
		t.Fatal("replacement worker inherited old statistics")
	}
}

func TestModemTelemetryRejectsRetiredWorkerObservations(t *testing.T) {
	c := testController(t, nil)
	started := time.Now().UTC()
	current := &child{id: "voice-1", role: "voice", done: make(chan struct{}), started: started}
	old := &child{id: "voice-1", role: "voice", done: make(chan struct{}), started: started.Add(-time.Minute)}
	c.mu.Lock()
	c.status.State = "running"
	c.status.Channels = []ChannelStatus{{ID: "voice-1", Role: "voice", StartedAt: started}}
	c.children["voice-1"] = current
	c.mu.Unlock()
	defer func() {
		c.mu.Lock()
		c.children = map[string]*child{}
		c.mu.Unlock()
	}()
	c.applyNativeStatus(current, modemNative(`{"tx":true,"p25RxState":2}`))
	line := "I: (RF) P25, TDU, total frames: 1, bits: 1234, undecodable LC: 0, errors: 1, BER: 0.0810%"
	c.observe(current, line)
	c.observe(old, line)
	c.applyNativeStatus(old, modemNative(`{"tx":false,"p25RxState":0}`))
	got := c.Status().Channels[0].Telemetry
	if len(got.Samples) != 1 || len(got.Receive) != 1 || !*got.Samples[0].TX || !*got.Samples[0].RX {
		t.Fatal("retired worker corrupted current observations", got)
	}
}
