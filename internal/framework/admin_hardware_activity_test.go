package framework

import (
	"bytes"
	"context"
	"fmt"
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

func TestHardwareActivityPreservesPhysicalOperatingBindingsAndUnknowns(t *testing.T) {
	now := time.Now().UTC()
	status := modemObservationFixture(now)
	status.Channels[0].Device = "/stable/old"
	status.Channels[0].Name = "Operating channel"
	status.Channels[0].Role = "voice"
	report := SiteReport{Radio: &status, Config: SiteConfig{Modems: []Modem{{ID: "modem-1", Name: "Saved renamed modem", Device: "/stable/new"}}, Channels: []Channel{{ID: "channel-1", ModemID: "modem-1"}}}}
	// Reassigning the saved modem does not move its live worker/history onto the
	// newly programmed physical device before the owner applies the new setup.
	rows := modemRows(report)
	if len(rows) != 2 || rows[0].Device != "/stable/old" || rows[0].Name != "Control modem" || rows[1].Device != "/stable/new" {
		t.Fatal("saved edit relabelled operating hardware", rows)
	}
	var body bytes.Buffer
	webHardwareActivity(&body, report, nil, now, "/stable/new", "")
	if strings.Contains(body.String(), "0.1585%") || strings.Contains(body.String(), "<circle") {
		t.Fatal("unselected/new device received old worker history or charts")
	}
	body.Reset()
	webHardwareActivity(&body, report, nil, now, "/stable/old", "ber")
	for _, want := range []string{"Operating channel", "433.450000", "0.1585%", "86</td>", "2</td>", "54253", "data-hardware-section=diagnostics", "Bit error rate of recent received calls"} {
		if !strings.Contains(body.String(), want) {
			t.Errorf("detail missing %q", want)
		}
	}
	status.Channels[0].Telemetry.Samples[1].At = now.Add(-time.Minute)
	body.Reset()
	webHardwareActivity(&body, report, nil, now, "/stable/old", "")
	if strings.Contains(body.String(), "Active (voice") || !strings.Contains(body.String(), "Status unavailable") || !strings.Contains(body.String(), "0.1585%") || strings.Contains(body.String(), "<circle") {
		t.Fatal("stale activity retained a live claim, discarded history, or rendered a closed chart")
	}
}

func TestHardwareActivityRoutesAreLocalReadOnlyAndDoNotDiscoverHardware(t *testing.T) {
	c := integrationNewCluster(t)
	s := c.site.service
	// Join supervision before installing this read-only observation fixture.
	s.operationCancel()
	s.operationWG.Wait()
	runtime := &operationRadio{status: modemObservationFixture(time.Now()), applications: make(chan policyApplication, 16)}
	runtime.status.Channels[0].Device = "/stable/current"
	s.radio = runtime
	before, _ := s.Store.Config()
	client := earlyWebLogin(t, c.site)
	body := string(earlyWebGet(t, client, c.site.web.URL+"/hardware?fragment=activity&detail=%2Fstable%2Fcurrent", http.StatusOK))
	if !strings.Contains(body, "data-hardware-updates") || strings.Contains(body, "Radio hardware discovery requires Linux") || strings.Contains(body, "Hardware inventory") {
		t.Fatal("live observation requested a hardware scan", body)
	}
	if !strings.Contains(body, "Active (control)") {
		t.Fatal("local live observation unavailable")
	}
	result := httptest.NewRecorder()
	s.webAdmin(result, httptest.NewRequest("POST", "/hardware", nil), Session{}, "hardware")
	if result.Code != http.StatusMethodNotAllowed {
		t.Fatal("hardware page accepted a mutation")
	}
	result = httptest.NewRecorder()
	s.webAdmin(result, httptest.NewRequest("GET", "/modem-status", nil), Session{}, "modem-status")
	if result.Code != http.StatusSeeOther || result.Header().Get("Location") != "/hardware" {
		t.Fatal("former Site page not consolidated")
	}
	after, _ := s.Store.Config()
	if !bytes.Equal(before, after) || runtime.starts != 0 || runtime.stops != 0 || len(runtime.commands) != 0 {
		t.Fatal("hardware activity changed programming or issued operations")
	}
	for _, pair := range []*integrationService{c.system, c.zone, c.dispatch} {
		other := earlyWebLogin(t, pair)
		earlyWebGet(t, other, pair.web.URL+"/hardware?fragment=activity", http.StatusNotFound)
	}
}

func TestHardwareActivityBrowser(t *testing.T) {
	if os.Getenv("OPEN25_HARDWARE_BROWSER_TEST") != "1" {
		t.Skip("set OPEN25_HARDWARE_BROWSER_TEST=1 for disposable inline activity checks")
	}
	c := integrationNewCluster(t)
	repo, _ := filepath.Abs("../..")
	evidence := os.Getenv("OPEN25_HARDWARE_BROWSER_EVIDENCE")
	if evidence == "" {
		evidence = t.TempDir()
	}
	fixture := map[string]any{"username": "administrator", "password": integrationPassword}
	for _, pair := range []*integrationService{c.system, c.zone, c.site, c.otherSite} {
		pair.service.assets = frameworkui.Assets()
	}
	for _, pair := range []*integrationService{c.site, c.otherSite} {
		s := pair.service
		// Join supervision before installing the browser's observation double.
		s.operationCancel()
		s.operationWG.Wait()
		var site SiteConfig
		_ = s.owned(&site)
		site.Modems = nil
		site.Channels = nil
		runtime := &operationRadio{applications: make(chan policyApplication, 16), status: radio.Status{State: "running", Mode: "hardware", Available: true, StartedAt: time.Now().Add(-time.Minute), FNEReady: true, ZoneConnected: true, InhibitionReady: true}}
		var inventory HardwareInventoryView
		for i := 0; i < 7; i++ {
			device := fmt.Sprintf("/stable/usb-%d", i)
			name := []string{"Control modem", "Voice modem 1", "Voice modem 2", "Control modem", "Voice modem 1", "Voice modem 2", "Spare USB modem"}[i]
			local := (i < 3) == (pair == c.site)
			modemID := fmt.Sprintf("modem-%d", i)
			channelID := fmt.Sprintf("channel-%d", i)
			entry := HardwareDeviceView{HardwareDevice: radio.HardwareDevice{ID: device, Device: device, Paths: []string{device}, ResolvedDevice: fmt.Sprintf("/dev/ttyACM%d", i), UID: fmt.Sprintf("USB-%c%d", 'A'+i/3, 101+i), Product: "USB modem", Present: true, PresenceKnown: true}}
			if i < 6 {
				entry.Assignments = []HardwareAssignmentView{{Local: local, ModemName: name, SiteName: "Other Site"}}
			}
			inventory.Devices = append(inventory.Devices, entry)
			if i >= 6 || !local {
				continue
			}
			site.Modems = append(site.Modems, Modem{ID: modemID, Name: name, Device: device})
			site.Channels = append(site.Channels, Channel{ID: channelID, Name: strings.Replace(name, "modem", "channel", 1), ModemID: modemID, Enabled: true, Identifier: 2, ChannelNo: 1, PowerPercent: 95})
			ch := modemObservationFixture(time.Now()).Channels[0]
			ch.ID = channelID
			ch.Device = device
			ch.OperatingModemID = modemID
			ch.OperatingModemName = name
			ch.Name = site.Channels[len(site.Channels)-1].Name
			ch.Role = "voice"
			if i%3 == 0 {
				ch.Role = "control"
				site.ControlPolicy = ControlPolicy{Mode: "static", ChannelID: channelID, CandidateIDs: []string{channelID}}
				runtime.status.ActiveControlChannelID = channelID
			}
			ch.Telemetry.Samples[1].TX = ptrBool(ch.Role == "control")
			ch.Telemetry.Samples[1].RX = ptrBool(false)
			ch.Telemetry.Samples[1].RXState = "Listening"
			runtime.status.Channels = append(runtime.status.Channels, ch)
		}
		s.operationMu.Lock()
		_ = s.Store.SaveConfig(storeDoc(t, site))
		s.radio = runtime
		profile, err := s.profileRadio()
		if err != nil {
			s.operationMu.Unlock()
			t.Fatal(err)
		}
		s.radioAppliedProfile = profile
		s.radioCandidateSignature = profileSignature(profile)
		s.radioRunActive = true
		s.radioRunStartedAt = runtime.status.StartedAt
		s.operationMu.Unlock()
		// Only inventory descriptors are fixture data. The shell, table, detail,
		// assets, authentication and live endpoint use the production renderers.
		base := s.Handler()
		pair.web.Close()
		pair.web = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if r.URL.Path != "/hardware" || r.URL.Query().Get("fragment") == "activity" {
				base.ServeHTTP(w, r)
				return
			}
			session, ok := s.session(r)
			if !ok {
				base.ServeHTTP(w, r)
				return
			}
			if r.Method != "GET" {
				w.WriteHeader(http.StatusMethodNotAllowed)
				return
			}
			now := time.Now().UTC()
			status := runtime.Status()
			report := SiteReport{Config: site, Radio: &status}
			zone := s.webKnownZone()
			if r.URL.Query().Get("fragment") == "inventory" {
				webHardwareInventory(w, inventory, report, zone, now)
			} else {
				s.webHardwarePage(w, r, session, inventory, report, zone, now)
			}
		}))
		settings := s.Config().Settings()
		settings.PublicURL = pair.web.URL
		if err := s.SaveSettings(settings); err != nil {
			t.Fatal(err)
		}
		ctx, cancel := context.WithCancel(t.Context())
		t.Cleanup(cancel)
		go func() {
			ticker := time.NewTicker(100 * time.Millisecond)
			defer ticker.Stop()
			for {
				select {
				case <-ctx.Done():
					return
				case now := <-ticker.C:
					runtime.mu.Lock()
					runtime.status.Channels = append([]radio.ChannelStatus(nil), runtime.status.Channels...)
					for j := range runtime.status.Channels {
						runtime.status.Channels[j].Telemetry.Samples = append([]radio.ModemSample(nil), runtime.status.Channels[j].Telemetry.Samples...)
						runtime.status.Channels[j].Telemetry.Samples[1].At = now
					}
					runtime.mu.Unlock()
				}
			}
		}()
	}
	fixture["system"], fixture["zone"], fixture["site"], fixture["other_site"] = c.system.web.URL, c.zone.web.URL, c.site.web.URL, c.otherSite.web.URL
	file := filepath.Join(evidence, "fixture.private.json")
	if err := WritePrivateJSON(file, fixture); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.Remove(file) })
	ctx, cancel := context.WithTimeout(t.Context(), 4*time.Minute)
	defer cancel()
	command := exec.CommandContext(ctx, "node", filepath.Join(repo, "tools/framework/hardware-activity-ui-check.mjs"), file, evidence)
	output, err := command.CombinedOutput()
	t.Logf("Hardware activity browser checks: %s", output)
	if err != nil {
		t.Fatal(err)
	}
}
