package framework

import (
	"bytes"
	"encoding/json"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
)

func TestWebScaleDecimalPreservesRadioAndTimePrecision(t *testing.T) {
	for _, c := range []struct {
		text     string
		exponent int
		want     string
	}{
		{"443450000", -6, "443.45"}, {"443437500", -6, "443.4375"}, {"1", -6, "0.000001"},
		{"-5000000", -6, "-5"}, {"12500", -3, "12.5"}, {"6250", -3, "6.25"}, {"1", -3, "0.001"},
		{"9223372036854775807", -6, "9223372036854.775807"}, {"-9223372036854775808", -6, "-9223372036854.775808"},
		{"443.450001", 6, "443450001"}, {"443.4500001", 6, "443450000.1"}, {"0.000001", 6, "1"}, {"-5", 6, "-5000000"},
		{"4.4345e2", 6, "443450000"}, {".5", 6, "500000"}, {"+0001.250", 3, "1250"},
		{"900", -3, "0.9"}, {"-1", -3, "-0.001"}, {"0.753", 2, "75.3"}, {"75.3", -2, "0.753"},
		{"1e1000000", -6, "1e999994"}, {"1e-1000000", 6, "1e-999994"}, {"0", -6, "0"},
	} {
		t.Run(c.text, func(t *testing.T) {
			got, err := webScaleDecimal(c.text, c.exponent)
			if err != nil || got != c.want {
				t.Fatalf("scale %q by10^%d = %q, %v; want %q", c.text, c.exponent, got, err, c.want)
			}
		})
	}
	for _, text := range []string{"", "abc", "NaN", "Inf", "1/2", "1.2.3", "1e", "--1"} {
		if _, err := webScaleDecimal(text, 6); err == nil {
			t.Fatal("nondecimal text accepted", text)
		}
	}
}

func TestWebUnitsRenderAndReadOnlyKnownMeasurements(t *testing.T) {
	fields := []webField{{Name: "tx_frequency_hz", Label: "Site transmit frequency (MHz)", Kind: "number"}, {Name: "channel_spacing_hz", Label: "Channel spacing (kHz)", Kind: "number"}, {Name: "calibration.rx_offset_hz", Label: "Receive correction (Hz)", Kind: "number"}, {Name: "identifier", Label: "Band Plan ID", Kind: "number"}}
	record := map[string]any{"tx_frequency_hz": json.Number("9223372036854775807"), "channel_spacing_hz": json.Number("12500"), "calibration": map[string]any{"rx_offset_hz": json.Number("-120")}, "identifier": json.Number("2"), "keep": "unchanged"}
	var output bytes.Buffer
	webFields(&output, fields, record)
	for _, want := range []string{`id="f-tx_frequency_hz" type=number name="tx_frequency_hz" value="9223372036854.775807" step=any`, `name="channel_spacing_hz" value="12.5" step=any`, `name="calibration.rx_offset_hz" value="-120" step=any`, `data-number-decimal name="identifier" value="2"`} {
		if !strings.Contains(output.String(), want) {
			t.Fatal("measurement input or numeric attribute missing", want, output.String())
		}
	}
	form := url.Values{"tx_frequency_hz": {"9223372036854.775807"}, "channel_spacing_hz": {"12.5"}, "calibration.rx_offset_hz": {"-120"}, "identifier": {"2"}}
	r := httptest.NewRequest("POST", "/channels", strings.NewReader(form.Encode()))
	r.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	if err := webReadFields(r, fields, record); err != nil {
		t.Fatal(err)
	}
	if webText(record["tx_frequency_hz"]) != "9223372036854775807" || webText(record["channel_spacing_hz"]) != "12500" || webText(webValue(record, "calibration.rx_offset_hz")) != "-120" || webText(record["identifier"]) != "2" || record["keep"] != "unchanged" {
		t.Fatal("conversion changed canonical precision or unrelated fields", record)
	}
	if got := webDisplay(fields[0], record["tx_frequency_hz"]); got != "9223372036854.775807" {
		t.Fatal("collection and editor units disagree", got)
	}
	form.Set("tx_frequency_hz", "443.4500001")
	r = httptest.NewRequest("POST", "/channels", strings.NewReader(form.Encode()))
	r.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	if err := webReadFields(r, fields, record); err != nil || webText(record["tx_frequency_hz"]) != "443450000.1" {
		t.Fatal("sub-Hz input was rounded or given a new representability restriction", err)
	}
}

func TestWebUnitsReadOnlyTablesPreserveNumbersNullsAndUnits(t *testing.T) {
	record := map[string]any{"tx_frequency_hz": json.Number("9223372036854775807"), "channel_spacing_hz": json.Number("12500"), "uplink_offset_hz": json.Number("-5000000"), "radio_id": json.Number("443450000"), "lifetime_ms": json.Number("1234"), "speech_ms": nil, "last_frame_age_ms": json.Number("250"), "latitude": json.Number("41.50000001"), "accuracy": json.Number("5.25"), "volume": json.Number("0.753"), "nested": []any{map[string]any{"rx_frequency_hz": json.Number("448450001")}}}
	before, _ := json.Marshal(record)
	var output bytes.Buffer
	columns := []webColumn{}
	for _, key := range []string{"tx_frequency_hz", "channel_spacing_hz", "uplink_offset_hz", "radio_id", "lifetime_ms", "speech_ms", "last_frame_age_ms", "latitude", "accuracy", "volume"} {
		columns = append(columns, webColumn{key, webLabel(key)})
	}
	webProperties(&output, record, columns)
	webTable(&output, []webColumn{{"rx_frequency_hz", "RX frequency (MHz)"}}, record["nested"])
	for _, want := range []string{"TX Frequency (MHz)", "9223372036854.775807", "Channel Spacing (kHz)", "12.5", "Uplink Offset (MHz)", ">-5<", "Radio ID", "443450000 / 1A6E8290 hex", "Lifetime (s)", ">1.234<", "Speech (s)", "—", "Last Frame Age (ms)", ">250<", "Latitude (°)", ">41.50000001<", "Accuracy (m)", ">5.25<", "Volume (%)", ">75.3<", ">448.450001<"} {
		if !strings.Contains(output.String(), want) {
			t.Fatal("read-only units/value missing", want, output.String())
		}
	}
	after, _ := json.Marshal(record)
	if !bytes.Equal(before, after) {
		t.Fatal("read-only presentation mutated data")
	}
	api := &webResult{code: 200}
	api.WriteString(`{"tx_frequency_hz":9223372036854775807}`)
	output.Reset()
	value, err := webConfig(api.Bytes())
	if err != nil {
		t.Fatal(err)
	}
	webProperties(&output, value, []webColumn{{"tx_frequency_hz", "TX frequency (MHz)"}})
	if !strings.Contains(output.String(), "9223372036854.775807") {
		t.Fatal("API presentation lost integer precision", output.String())
	}
}

func TestWebProfileUsesPercentWithoutChangingStoredGain(t *testing.T) {
	profile := map[string]any{"name": "Test", "receive": []any{map[string]any{"talkgroup_id": json.Number("1001"), "volume": json.Number("0.753"), "pan": json.Number("0"), "muted": false}}}
	var output bytes.Buffer
	webFields(&output, webDirectoryFields("profiles"), webProfileFields(profile))
	if !strings.Contains(output.String(), `name="receive_0_volume" value="75.3"`) {
		t.Fatal("profile volume did not use percent", output.String())
	}
	request := httptest.NewRequest("POST", "/profiles/edit", nil)
	request.PostForm = url.Values{"name": {"Test"}, "receive_row": {"0"}, "receive_0_talkgroup_id": {"1001"}, "receive_0_volume": {"75.3"}, "receive_0_pan": {"0"}}
	input := map[string]any{}
	if err := webReadFields(request, webDirectoryFields("profiles"), input); err != nil {
		t.Fatal(err)
	}
	if err := webReadProfile(input, profile); err != nil {
		t.Fatal(err)
	}
	receive := profile["receive"].([]any)[0].(map[string]any)
	if webText(receive["volume"]) != "0.753" {
		t.Fatal("profile percent did not roundtrip gain", receive["volume"])
	}
}
