package framework

import (
	"bytes"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
)

func TestAdminNumberRepresentationSubmission(t *testing.T) {
	fields := []webField{{Name: "tgid", Label: "Talkgroup ID", Kind: "number"}}
	for _, test := range []struct{ base, decimal, hex, want string }{
		{"decimal", "1001", "FFFF", "1001"},
		{"hex", "1001", "12345", "74565"},
		{"hex", "1001", "-1", "-1"},
		{"hex", "1001", "20000000000001", "9007199254740993"},
		{"hex", "1001", "", ""},
	} {
		t.Run(test.base+test.hex, func(t *testing.T) {
			form := url.Values{"tgid": {test.decimal}, "tgid__hex": {test.hex}, "tgid__base": {test.base}}
			r := httptest.NewRequest("POST", "/", strings.NewReader(form.Encode()))
			r.Header.Set("Content-Type", "application/x-www-form-urlencoded")
			record := map[string]any{"unchanged": "kept"}
			if err := webReadFields(r, fields, record); err != nil {
				t.Fatal(err)
			}
			if webText(record["tgid"]) != test.want || record["unchanged"] != "kept" {
				t.Fatalf("representation changed: %#v", record)
			}
			if len(record) != 2 {
				t.Fatal("presentation fields entered configuration")
			}
		})
	}
}

func TestAdminIncompleteHexDoesNotSaveStaleDecimal(t *testing.T) {
	form := url.Values{"tgid": {"1001"}, "tgid__hex": {"G"}, "tgid__base": {"hex"}}
	r := httptest.NewRequest("POST", "/", strings.NewReader(form.Encode()))
	r.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	field := webField{Name: "tgid", Label: "Talkgroup ID", Kind: "number"}
	record := map[string]any{"tgid": 1001}
	if err := webReadFields(r, []webField{field}, record); err == nil {
		t.Fatal("unreadable hex silently saved old decimal")
	}
	var output bytes.Buffer
	webFields(&output, []webField{field}, record, r.PostForm)
	if !strings.Contains(output.String(), `name="tgid__hex" value="G"`) || !strings.Contains(output.String(), `name="tgid__base" value="hex"`) {
		t.Fatal("partial notation lost on error")
	}
}

func TestAdminNumberPaddingIsNotARangeRestriction(t *testing.T) {
	for text, want := range map[string]string{"1001": "03E9", "74565": "12345", "-1": "-0001", "": "", "9007199254740993": "20000000000001"} {
		if actual := webHexText(text, 4); actual != want {
			t.Fatalf("%s became %s, want %s", text, actual, want)
		}
	}
	var output bytes.Buffer
	webFields(&output, []webField{{Name: "channel_spacing_hz", Kind: "number"}}, map[string]any{"channel_spacing_hz": 12500})
	if strings.Contains(output.String(), "data-number-hex") {
		t.Fatal("physical units treated as hexadecimal identity")
	}
}

func TestAdminFailedHexKeepsSubmittedPhysicalUnits(t *testing.T) {
	fields := []webField{{Name: "identifier", Kind: "number"}, {Name: "tx_frequency_hz", Kind: "number"}, {Name: "channel_spacing_hz", Kind: "number"}, {Name: "calibration.rx_level", Kind: "number"}}
	values := url.Values{"identifier": {"2"}, "identifier__base": {"hex"}, "identifier__hex": {"G"}, "tx_frequency_hz": {"443.450001"}, "channel_spacing_hz": {"12.5"}, "calibration.rx_level": {"50"}}
	r := httptest.NewRequest("POST", "/", strings.NewReader(values.Encode()))
	r.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	record := map[string]any{"identifier": 2, "tx_frequency_hz": 443450000, "channel_spacing_hz": 12500, "calibration": map[string]any{"rx_level": 50}}
	if webReadFields(r, fields, record) == nil {
		t.Fatal("partial hex should produce a parsing error")
	}
	var output bytes.Buffer
	webFields(&output, fields, record, r.PostForm)
	for _, value := range []string{`name="tx_frequency_hz" value="443.450001"`, `name="channel_spacing_hz" value="12.5"`, `name="calibration.rx_level" value="50"`, `name="identifier__hex" value="G"`} {
		if !strings.Contains(output.String(), value) {
			t.Fatal("submitted representation changed", value)
		}
	}
}
