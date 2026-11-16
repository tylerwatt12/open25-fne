package framework

import (
	"bytes"
	"encoding/json"
	"io"
	"net/http"
	"net/url"
	"reflect"
	"strings"
	"testing"
)

func webUnitsSubmit(t *testing.T, client *http.Client, address, origin string, values url.Values, status int) []byte {
	t.Helper()
	r, err := http.NewRequest(http.MethodPost, address, strings.NewReader(values.Encode()))
	if err != nil {
		t.Fatal(err)
	}
	r.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	r.Header.Set("Origin", origin)
	response, err := client.Do(r)
	if err != nil {
		t.Fatal(err)
	}
	defer response.Body.Close()
	body, err := io.ReadAll(response.Body)
	if err != nil || response.StatusCode != status {
		t.Fatalf("unit form submission: HTTP%d, %v, %s", response.StatusCode, err, body)
	}
	return body
}

func TestEarlyWebBandPlanUnitsSaveExactHzAndKeepFailedInput(t *testing.T) {
	c := integrationNewCluster(t)
	client := earlyWebLogin(t, c.zone)
	address := c.zone.web.URL + "/identifiers/edit?row=0"
	body := earlyWebGet(t, client, address, 200)
	for _, want := range []string{"Band Plan", "Base frequency (MHz)", "Channel spacing (kHz)", "Subscriber transmit offset (MHz)", "Bandwidth (kHz)", "P25 Channel Identifier (0–15)"} {
		if !bytes.Contains(body, []byte(want)) {
			t.Fatal("Band Plan label/help missing", want)
		}
	}
	if bytes.Contains(body, []byte("Channel Plan")) {
		t.Fatal("old Channel Plan UI name remains")
	}
	planned := earlyWebGet(t, client, c.zone.web.URL+"/planned", 200)
	if !bytes.Contains(planned, []byte("Band Plan Subsets")) {
		t.Fatal("planned Band Plan capability missing")
	}
	form := earlyWebForm(t, body, "base_frequency_hz")
	if form.Get("base_frequency_hz") != "433.437500" || form.Get("channel_spacing_hz") != "12.5" || form.Get("uplink_offset_hz") != "5.000000" || form.Get("bandwidth_hz") != "12.5" {
		t.Fatal("Band Plan form did not show radio units", form)
	}
	beforeRaw, err := c.zone.service.Store.Config()
	if err != nil {
		t.Fatal(err)
	}
	before, err := webConfig(beforeRaw)
	if err != nil {
		t.Fatal(err)
	}
	webUnitsSubmit(t, client, address, c.zone.web.URL, form, http.StatusSeeOther)
	afterRaw, err := c.zone.service.Store.Config()
	if err != nil {
		t.Fatal(err)
	}
	after, err := webConfig(afterRaw)
	if err != nil || !reflect.DeepEqual(before, after) {
		t.Fatal("unchanged human-unit Save changed canonical configuration", err)
	}
	form = earlyWebForm(t, earlyWebGet(t, client, address, 200), "base_frequency_hz")
	form.Set("base_frequency_hz", "9223372036854.775807")
	form.Set("uplink_offset_hz", "-5.000001")
	webUnitsSubmit(t, client, address, c.zone.web.URL, form, http.StatusSeeOther)
	raw, err := c.zone.service.Store.Config()
	if err != nil {
		t.Fatal(err)
	}
	config, err := webConfig(raw)
	if err != nil {
		t.Fatal(err)
	}
	plan := config["channel_identifiers"].([]any)[0].(map[string]any)
	if webText(plan["base_frequency_hz"]) != "9223372036854775807" || webText(plan["uplink_offset_hz"]) != "-5000001" {
		t.Fatal("form lost canonical integer precision", plan)
	}
	list := earlyWebGet(t, client, c.zone.web.URL+"/identifiers", 200)
	if !bytes.Contains(list, []byte("9223372036854.775807")) || !bytes.Contains(list, []byte("-5.000001")) {
		t.Fatal("list units disagree with saved editor values")
	}
	form = earlyWebForm(t, earlyWebGet(t, client, address, 200), "base_frequency_hz")
	form.Set("base_frequency_hz", "433.123456")
	if _, err = c.zone.service.Store.db.Exec(`CREATE TRIGGER fail_unit_save BEFORE UPDATE ON framework_documents WHEN NEW.name='config' BEGIN SELECT RAISE(ABORT,'unit-save fixture failure'); END;`); err != nil {
		t.Fatal(err)
	}
	failed := webUnitsSubmit(t, client, address, c.zone.web.URL, form, http.StatusOK)
	if !bytes.Contains(failed, []byte("unit-save fixture failure")) {
		t.Fatal("storage failure was not reported", string(failed))
	}
	if got := earlyWebForm(t, failed, "base_frequency_hz").Get("base_frequency_hz"); got != "433.123456" {
		t.Fatal("failed save lost or double-scaled entered units", got)
	}
	still, err := c.zone.service.Store.Config()
	if err != nil || !bytes.Equal(raw, still) {
		t.Fatal("failed save changed stored configuration", err)
	}
}

func TestEarlyWebRFChannelUsesBandPlanAndChannelNumber(t *testing.T) {
	c := integrationNewCluster(t)
	client := earlyWebLogin(t, c.site)
	address := c.site.web.URL + "/channels/edit?row=0"
	page := earlyWebGet(t, client, address, 200)
	form := earlyWebForm(t, page, "channel_no")
	if form.Has("tx_frequency_hz") || form.Has("rx_frequency_hz") || !bytes.Contains(page, []byte("433.450000 MHz")) || !bytes.Contains(page, []byte("438.450000 MHz")) {
		t.Fatal("frequencies must be calculated output")
	}
	before, _ := c.site.service.Store.Config()
	listPage := earlyWebGet(t, client, c.site.web.URL+"/channels", 200)
	for _, want := range []string{"Frequency (MHz)", "<b>TX</b><span class=ts-numeric>433.450000</span>", "<b>RX</b><span class=ts-numeric>438.450000</span>"} {
		if !bytes.Contains(listPage, []byte(want)) {
			t.Fatalf("RF channel list missing %q", want)
		}
	}
	afterList, _ := c.site.service.Store.Config()
	if !bytes.Equal(before, afterList) {
		t.Fatal("listing calculated frequencies changed stored configuration")
	}
	want, _ := webConfig(before)
	form.Set("channel_no", "81")
	form.Set("tx_frequency_hz", "1")
	form.Set("rx_frequency_hz", "2")
	webUnitsSubmit(t, client, address, c.site.web.URL, form, http.StatusSeeOther)
	saved, _ := c.site.service.Store.Config()
	actual, _ := webConfig(saved)
	want["channels"].([]any)[0].(map[string]any)["channel_no"] = json.Number("81")
	if !reflect.DeepEqual(want, actual) {
		t.Fatal("channel-number edit altered other settings or accepted independent frequencies")
	}
	profile, err := c.site.service.profileRadio()
	if err != nil {
		t.Fatal(err)
	}
	if profile.Site.Channels[0].TXHz != 434450000 || profile.Site.Channels[0].RXHz != 439450000 {
		t.Fatal("runtime did not use Zone Band Plan")
	}
	listPage = earlyWebGet(t, client, c.site.web.URL+"/channels?q=439.45", 200)
	if !bytes.Contains(listPage, []byte("<b>TX</b><span class=ts-numeric>434.450000</span>")) || !bytes.Contains(listPage, []byte("<b>RX</b><span class=ts-numeric>439.450000</span>")) {
		t.Fatal("RF channel list did not recalculate or search frequencies after editing")
	}
	form = earlyWebForm(t, earlyWebGet(t, client, address, 200), "channel_no")
	form.Set("identifier", "15")
	webUnitsSubmit(t, client, address, c.site.web.URL, form, http.StatusSeeOther)
	listPage = earlyWebGet(t, client, c.site.web.URL+"/channels", 200)
	if bytes.Count(listPage, []byte("<span class=ts-numeric>Unavailable</span>")) != 2 {
		t.Fatal("RF channel list must show unavailable frequencies for a missing Band Plan")
	}
}
