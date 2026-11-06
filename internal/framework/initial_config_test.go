package framework

import (
	"bytes"
	"encoding/json"
	"strings"
	"testing"

	"golang.org/x/crypto/bcrypt"
)

func initialFixture(t *testing.T) map[string]any {
	t.Helper()
	hash, err := bcrypt.GenerateFromPassword([]byte("test-only"), bcrypt.MinCost)
	if err != nil {
		t.Fatal(err)
	}
	var doc map[string]any
	if err = json.Unmarshal([]byte(`{
"system": {"id":"system-1","name":"Test system","wacn":1,"system_id":1,"callsign":"N0TEST",
 "radios":[{"rid":10001,"alias":"Portable","enabled":true,"kind":"subscriber"}],
 "talkgroups":[{"tgid":1001,"admission_mode":"clear","transmit_mode":"clear","alias":"Test 1","enabled":true,"site_access":"selected","site_ids":["site-1"]},{"tgid":1099,"admission_mode":"clear","transmit_mode":"clear","alias":"Denied","enabled":false,"site_access":"all","site_ids":[]}],
 "status_labels":[{"id":1,"label":"READY"},{"id":2,"label":"BUSY"}],"message_labels":[{"id":1,"label":"TESTMSG"}]},
"zone": {"id":"zone-1","system_id":"system-1","name":"Zone 1","rfss_id":1,
 "sites":[{"id":"site-1","name":"Main site","site_id":1,"nac":659,"endpoint":"http://127.0.0.1:5002"}],
 "channel_identifiers":[{"identifier":2,"base_frequency_hz":443437500,"channel_spacing_hz":12500,"uplink_offset_hz":5000000,"bandwidth_hz":12500}]},
"sites": [{"id":"site-1","broadcast_band_plan":true,
 "modems":[{"id":"modem-control","name":"Control modem","device":"/dev/serial/by-id/control","baud":115200,"calibration":{"rx_level":50,"tx_level":50,"rx_offset_hz":120}},{"id":"modem-voice","name":"Voice modem","device":"/dev/serial/by-id/voice","baud":115200,"calibration":{"rx_level":40,"tx_level":45}}],
 "channels":[{"id":"control-1","modem_id":"modem-control","role":"control","name":"Control","identifier":2,"channel_no":1,"power_percent":95,"enabled":true},{"id":"voice-1","modem_id":"modem-voice","role":"voice","name":"Voice 1","identifier":2,"channel_no":81,"power_percent":90,"enabled":true}],
 "identification":{"enabled":false,"mode":"ham","wpm":20,"quiet_seconds":60}}],
"application":{"name":"Dispatch","connection_url":"http://127.0.0.1:5001","local_site_url":"http://127.0.0.1:5002"},
"administrator":{"username":"administrator","password_hash":""}
}`), &doc); err != nil {
		t.Fatal(err)
	}
	doc["administrator"].(map[string]any)["password_hash"] = string(hash)
	return doc
}

func decodeFixture(t *testing.T, doc map[string]any) (InitialConfig, error) {
	t.Helper()
	raw, err := json.Marshal(doc)
	if err != nil {
		t.Fatal(err)
	}
	return DecodeInitialConfig(bytes.NewReader(raw))
}

func TestInitialConfigurationPreservesCurrentDocuments(t *testing.T) {
	seed, err := decodeFixture(t, initialFixture(t))
	if err != nil {
		t.Fatal(err)
	}
	if seed.System.WACN != 1 || seed.System.SystemID != 1 || seed.Zone.RFSSID != 1 || len(seed.Sites) != 1 {
		t.Fatal("home and serving identities changed")
	}
	if seed.System.Radios[0].RID != 10001 || seed.System.Radios[0].Kind != "subscriber" || seed.System.Talkgroups[1].Enabled || seed.System.Talkgroups[0].SiteIDs[0] != "site-1" {
		t.Fatal("radio identity or talkgroup policy changed")
	}
	site := seed.Sites[0]
	for i, want := range [][2]int64{{443450000, 448450000}, {444450000, 449450000}} {
		ch := site.Channels[i]
		tx, rx, ok := bandPlanFrequencies(&seed.Zone, ch.Identifier, ch.ChannelNo)
		if !ok || tx != want[0] || rx != want[1] {
			t.Fatal("derived channel frequencies changed")
		}
	}

	if site.Channels[0].PowerPercent != 95 || site.Channels[1].PowerPercent != 90 || site.Modems[0].Calibration.RXOffsetHz != 120 || site.Modems[1].Calibration.TXLevel != 45 {
		t.Fatal("modem settings changed")
	}
	if site.Identification.Enabled || site.Identification.WPM != 20 || site.Identification.QuietSeconds != 60 {
		t.Fatal("station identification settings changed")
	}
	if seed.Administrator.Username != "administrator" || bcrypt.CompareHashAndPassword([]byte(seed.Administrator.PasswordHash), []byte("test-only")) != nil {
		t.Fatal("administrator verifier changed")
	}
}

func TestInitialConfigurationAcceptsMisconfigurationAndChecksCredentialEncoding(t *testing.T) {
	for _, test := range []struct {
		name string
		edit func(map[string]any)
	}{
		{"foreign system", func(d map[string]any) { d["zone"].(map[string]any)["system_id"] = "another-system" }},
		{"unknown identifier", func(d map[string]any) {
			d["sites"].([]any)[0].(map[string]any)["channels"].([]any)[0].(map[string]any)["identifier"] = 7
		}},
		{"unknown Site", func(d map[string]any) { d["sites"].([]any)[0].(map[string]any)["id"] = "unknown-site" }},
		{"repeated Site", func(d map[string]any) { sites := d["sites"].([]any); d["sites"] = append(sites, sites[0]) }},
		{"missing Site", func(d map[string]any) { d["sites"] = []any{} }},
		{"frequency disagrees with identifier", func(d map[string]any) {
			d["sites"].([]any)[0].(map[string]any)["channels"].([]any)[0].(map[string]any)["identifier"] = 99
		}},
		{"radio kind omitted", func(d map[string]any) {
			delete(d["system"].(map[string]any)["radios"].([]any)[0].(map[string]any), "kind")
		}},
		{"plaintext password", func(d map[string]any) { d["administrator"].(map[string]any)["password_hash"] = "not-a-bcrypt-verifier" }},
		{"obsolete input", func(d map[string]any) { d["management"] = map[string]any{"systems": []any{}} }},
		{"unknown field", func(d map[string]any) { d["system"].(map[string]any)["unknown"] = true }},
	} {
		t.Run(test.name, func(t *testing.T) {
			doc := initialFixture(t)
			test.edit(doc)
			_, err := decodeFixture(t, doc)
			if (err != nil) != (test.name == "plaintext password") {
				t.Fatalf("configuration reading: %v", err)
			}
		})
	}
}

func TestInitialConfigurationRejectsTrailingAndOversizedInput(t *testing.T) {
	raw, _ := json.Marshal(initialFixture(t))
	if _, err := DecodeInitialConfig(bytes.NewReader(append(raw, []byte(` {}`)...))); err == nil {
		t.Fatal("multiple JSON documents accepted")
	}
	if _, err := DecodeInitialConfig(strings.NewReader(strings.Repeat(" ", maxConfigurationBytes+1))); err == nil {
		t.Fatal("oversized input accepted")
	}
}
