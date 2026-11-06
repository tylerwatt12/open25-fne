package framework

import (
	"bytes"
	"context"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"open25.local/open25-fne/internal/frameworkui"
	"open25.local/open25-fne/internal/radio"
)

func modemObservationFixture(now time.Time) radio.Status {
	on, off := true, false
	started := now.Add(-time.Minute)
	return radio.Status{State: "running", Mode: "hardware", SiteID: "site-1", Channels: []radio.ChannelStatus{{
		ID: "channel-1", Name: "Control", Role: "control", State: "ready", Ready: true,
		StartedAt: started, OperatingModemID: "modem-1", OperatingModemName: "Control modem",
		TXHz: 443450000, RXHz: 448450000, RPCPort: 15999, RESTPort: 16000, Device: "private-device-path",
		Telemetry: radio.ModemTelemetry{StartedAt: started, Samples: []radio.ModemSample{{At: now.Add(-time.Second), TX: &off, RX: &on, RXState: "Receiving voice"}, {At: now, TX: &on, RX: &on, RXState: "Receiving voice"}}, Receive: []radio.ReceiveStatistics{{At: now.Add(-5 * time.Second), Frames: 44, ProtectedBits: 50000, Errors: 70, UndecodableLC: 2, BERPercent: .1400}}},
	}}}
}

func TestModemStatusUsesExistingReportsAndProjectsOnlyObservations(t *testing.T) {
	c := integrationNewCluster(t)
	operation := &operationRadio{status: modemObservationFixture(time.Now().UTC()), applications: make(chan policyApplication, 16)}
	c.site.service.radio = operation
	integrationSyncAll(t, c)
	for _, pair := range []*integrationService{c.site, c.zone, c.system} {
		before, _ := pair.service.Store.Config()
		client := earlyWebLogin(t, pair)
		for _, suffix := range []string{"", "?fragment=1"} {
			body := string(earlyWebGet(t, client, pair.web.URL+"/modem-status"+suffix, http.StatusOK))
			for _, want := range []string{"Control modem", "443.45", "448.45", "0.1400%", "70 reported bit errors", "2 undecodable LC", "Sampled TX / RX activity", "<circle"} {
				if !strings.Contains(body, want) {
					t.Errorf("%s%s missing %q", pair.service.Config().Role, suffix, want)
				}
			}
			for _, forbidden := range []string{"private-device-path", "/dev/trunksystem-control", "15999", "16000", "password_hash", "name=tx_frequency_hz", "name=config", "name=material", "media-secret"} {
				if strings.Contains(body, forbidden) {
					t.Errorf("%s exposes private/unneeded data %q", pair.service.Config().Role, forbidden)
				}
			}
			if suffix == "" && (!strings.Contains(body, "modem-status.js") || !strings.Contains(body, "SNR / noise floor") || strings.Contains(body, "http-equiv=refresh")) {
				t.Fatal("dedicated live page lacks truthful measurements or uses whole-page reload")
			}
		}
		after, _ := pair.service.Store.Config()
		if !bytes.Equal(before, after) || operation.starts != 0 || operation.stops != 0 || len(operation.commands) != 0 {
			t.Fatal("read-only modem view changed programming or issued an operation")
		}
		result := httptest.NewRecorder()
		pair.service.webAdmin(result, httptest.NewRequest("POST", "/modem-status", nil), Session{Username: "administrator"}, "modem-status")
		if result.Code != http.StatusMethodNotAllowed {
			t.Fatal("modem status accepted POST")
		}
	}
	if reports := c.system.service.siteReports(); len(reports) == 0 || reports[0].Radio == nil || !reports[0].Radio.Channels[0].Telemetry.Samples[1].At.Equal(operation.status.Channels[0].Telemetry.Samples[1].At) {
		t.Fatal("Zone/System relay changed the original sample timestamp")
	}
	client := earlyWebLogin(t, c.dispatch)
	earlyWebGet(t, client, c.dispatch.web.URL+"/modem-status", http.StatusNotFound)
	anonymous := &http.Client{CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}
	earlyWebGet(t, anonymous, c.site.web.URL+"/modem-status?fragment=1", http.StatusSeeOther)
}

func TestModemStatusStalenessUnknownsAndChangedBindings(t *testing.T) {
	c := integrationNewCluster(t)
	integrationSyncAll(t, c)
	now := time.Now().UTC()
	status := modemObservationFixture(now)
	report := SiteReport{ID: "site-1", Name: "Test Site", ObservedAt: now, ReceivedAt: now, Config: storeFixtureSite(), Radio: &status}
	if err := c.zone.service.writeCache("site_reports", []SiteReport{report}); err != nil {
		t.Fatal(err)
	}
	render := func(at time.Time) string {
		var body bytes.Buffer
		c.zone.service.webModemReports(&body, at)
		return body.String()
	}
	if body := render(now); !strings.Contains(body, ">Transmitting</td>") || !strings.Contains(body, ">Receiving voice</td>") {
		t.Fatal("simultaneous TX/RX hidden", body)
	}
	if body := render(now.Add(5 * time.Second)); strings.Contains(body, ">Transmitting</td>") || strings.Contains(body, ">Receiving voice</td>") || !strings.Contains(body, "0.1400%") {
		t.Fatal("stale native sample became current, or historical call report was lost")
	}
	if body := render(now.Add(time.Hour)); !strings.Contains(body, "Stale · current state unknown") {
		t.Fatal("report age ignored")
	}
	status.State = "stopped"
	if err := c.zone.service.writeCache("site_reports", []SiteReport{report}); err != nil {
		t.Fatal(err)
	}
	if body := render(now); strings.Contains(body, ">Transmitting</td>") || strings.Contains(body, ">Receiving voice</td>") {
		t.Fatal("stopped Site retained current activity from its last worker observation")
	}
	status.State = "running"
	status.Channels[0].Telemetry.Samples[1].RX = nil
	status.Channels[0].Telemetry.Samples[1].TX = nil
	status.Channels[0].Telemetry.Receive = nil
	report.Config.Channels[0].ModemID = "modem-2"
	report.Config.Modems = append(report.Config.Modems, Modem{ID: "modem-2", Name: "New saved binding"}, Modem{ID: "unused", Name: "Unassigned modem"})
	rows := modemRows(report)
	if len(rows) != 3 || rows[0].ID != "modem-1" || rows[0].Programming != "Operating programming" || rows[1].ID != "modem-2" || rows[1].Programming != "Saved programming" || rows[2].Programming != "Unassigned" {
		t.Fatal("saved edit relabeled actual operating hardware", rows)
	}
	if err := c.zone.service.writeCache("site_reports", []SiteReport{report}); err != nil {
		t.Fatal(err)
	}
	if body := render(now); strings.Contains(body, ">Idle</td>") || !strings.Contains(body, "Unavailable · no completed RX report") || !strings.Contains(body, "Unassigned modem") || strings.Contains(body, "0.0000%") {
		t.Fatal("missing measurements were presented as zero/idle")
	}
}

func TestModemChartsKeepRealZeroAndDoNotInventIntervals(t *testing.T) {
	var body bytes.Buffer
	webModemReceiveChart(&body, nil, true)
	if strings.Contains(body.String(), "<circle") || !strings.Contains(body.String(), "Unavailable") {
		t.Fatal("missing statistics graphed as a value")
	}
	body.Reset()
	webModemReceiveChart(&body, []radio.ReceiveStatistics{{At: time.Now().UTC(), BERPercent: 0}}, true)
	if !strings.Contains(body.String(), "<circle") || !strings.Contains(body.String(), "cy=\"96.00\"") {
		t.Fatal("observed zero BER was hidden")
	}
	if strings.Contains(body.String(), "<polyline") {
		t.Fatal("per-call samples were interpolated into a continuous measurement")
	}
}

func TestModemStatusBrowser(t *testing.T) {
	if os.Getenv("TRUNKSYSTEM_MODEM_BROWSER_TEST") != "1" {
		t.Skip("set TRUNKSYSTEM_MODEM_BROWSER_TEST=1 for disposable telemetry browser qualification")
	}
	c := integrationNewCluster(t)
	c.site.service.radio = &operationRadio{status: modemObservationFixture(time.Now().UTC()), applications: make(chan policyApplication, 16)}
	integrationSyncAll(t, c)
	repo, err := filepath.Abs("../..")
	if err != nil {
		t.Fatal(err)
	}
	output := filepath.Join(repo, "build", "modem-status-ui-test", "browser")
	if target := os.Getenv("TRUNKSYSTEM_MODEM_BROWSER_EVIDENCE"); target != "" {
		output = target
	}
	type fixtureRole struct {
		Role     Role   `json:"role"`
		Origin   string `json:"origin"`
		Username string `json:"username"`
		Password string `json:"password"`
	}
	fixture := struct {
		Roles []fixtureRole `json:"roles"`
	}{}
	for _, pair := range []*integrationService{c.site, c.zone, c.system} {
		pair.service.assets = frameworkui.Assets()
		fixture.Roles = append(fixture.Roles, fixtureRole{pair.service.Config().Role, pair.web.URL, "administrator", integrationPassword})
	}
	fixturePath := filepath.Join(output, "fixture.private.json")
	if err := WritePrivateJSON(fixturePath, fixture); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.Remove(fixturePath) })
	ctx, cancel := context.WithTimeout(t.Context(), 3*time.Minute)
	defer cancel()
	command := exec.CommandContext(ctx, "node", filepath.Join(repo, "tools/framework/modem-status-ui-check.mjs"), fixturePath, output)
	result, err := command.CombinedOutput()
	t.Logf("Modem browser qualification: %s", result)
	if err != nil {
		t.Fatal(err)
	}
}
