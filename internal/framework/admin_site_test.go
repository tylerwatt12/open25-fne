package framework

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"open25.local/open25-fne/internal/frameworkui"
	"open25.local/open25-fne/internal/radio"
)

func TestConciseChannelStatusDoesNotInferActivityFromOldOrAbsentSamples(t *testing.T) {
	now := time.Now().UTC()
	site := modemObservationFixture(now)
	ch := site.Channels[0]
	test := func(name, want string, change func(*radio.Status, *radio.ChannelStatus)) {
		t.Run(name, func(t *testing.T) {
			s, c := site, ch
			change(&s, &c)
			got, _ := adminChannelStatus(s, c, now)
			if got != want {
				t.Fatalf("got %q, want %q", got, want)
			}
		})
	}
	test("control", "Active (control)", func(*radio.Status, *radio.ChannelStatus) {})
	test("transmitting", "Active (voice, transmitting)", func(_ *radio.Status, c *radio.ChannelStatus) { c.Role = "voice" })
	test("receiving", "Active (voice, receiving)", func(_ *radio.Status, c *radio.ChannelStatus) {
		c.Role = "voice"
		c.Telemetry = radio.ModemTelemetry{StartedAt: c.StartedAt, Samples: []radio.ModemSample{{At: now, TX: ptrBool(false), RX: ptrBool(true)}}}
	})
	test("idle", "Active (voice, idle)", func(_ *radio.Status, c *radio.ChannelStatus) {
		c.Role = "voice"
		c.Telemetry = radio.ModemTelemetry{StartedAt: c.StartedAt, Samples: []radio.ModemSample{{At: now, TX: ptrBool(false), RX: ptrBool(false)}}}
	})
	test("busy is not proof of transmit", "Active (voice, in use)", func(_ *radio.Status, c *radio.ChannelStatus) {
		c.Role = "voice"
		c.Busy = true
		c.Telemetry = radio.ModemTelemetry{StartedAt: c.StartedAt, Samples: []radio.ModemSample{{At: now, TX: ptrBool(false), RX: ptrBool(false)}}}
	})
	test("no sample", "Status unavailable", func(_ *radio.Status, c *radio.ChannelStatus) { c.Telemetry.Samples = nil })
	test("old sample", "Status unavailable", func(_ *radio.Status, c *radio.ChannelStatus) {
		c.Telemetry = radio.ModemTelemetry{StartedAt: c.StartedAt, Samples: []radio.ModemSample{{At: now.Add(-4 * time.Second), TX: ptrBool(true)}}}
	})
	test("prior worker", "Status unavailable", func(_ *radio.Status, c *radio.ChannelStatus) { c.StartedAt = c.StartedAt.Add(time.Second) })
	test("no activity fields", "Status unavailable", func(_ *radio.Status, c *radio.ChannelStatus) {
		c.Telemetry = radio.ModemTelemetry{StartedAt: c.StartedAt, Samples: []radio.ModemSample{{At: now}}}
	})
	test("native unavailable", "Status unavailable", func(_ *radio.Status, c *radio.ChannelStatus) { c.Ready = false })
	test("stopped", "Offline", func(s *radio.Status, c *radio.ChannelStatus) { s.State = "stopped"; c.State = "stopped" })
	test("starting", "Starting", func(_ *radio.Status, c *radio.ChannelStatus) { c.State = "starting" })
	test("fault", "Fault", func(_ *radio.Status, c *radio.ChannelStatus) { c.State = "fault" })
	test("recovery", "Recovering", func(_ *radio.Status, c *radio.ChannelStatus) { c.State = "fault"; c.RecoveryWanted = true })
}
func ptrBool(v bool) *bool { return &v }

func TestSiteMockupPagesUseOwnedObservationsWithoutOperations(t *testing.T) {
	c := integrationNewCluster(t)
	s := c.site.service
	// Join supervision before installing this read-only observation fixture.
	s.operationCancel()
	s.operationWG.Wait()
	runtime := &operationRadio{status: modemObservationFixture(time.Now()), applications: make(chan policyApplication, 32)}
	s.radio = runtime
	client := earlyWebLogin(t, c.site)
	before, _ := s.Store.Config()
	for _, tc := range []struct {
		path string
		want []string
	}{
		{"/dashboard", []string{"summary-strip", "compact-topology", "Zone connection", "Channel / modem", "Active (control)", "433.450000"}},
		{"/live-status", []string{"Site status", "Registered radios", "Talkgroup affiliations"}},
		{"/live-status?channel=channel-1", []string{"Channel details", "Technical observations", "Native process state", "Channel setup", "/channels/edit?row=0"}},
		{"/radio-operation", []string{"Start Site", "Stop Site", "Start channel", "Stop channel", "Switch control channel"}},
		{"/activity-status", []string{"History delivery", "Awaiting acknowledgment", "Outbox limit", "Oldest pending", "System history"}},
		{"/channels", []string{"Assigned modem", "Frequency (MHz)"}},
		{"/modems", []string{"Assigned channel", "USB serial identity"}},
		{"/planned", []string{"Infrastructure alarms", "Alarm settings", "Notification rules", "Alarm acknowledgment"}},
	} {
		page := string(earlyWebGet(t, client, c.site.web.URL+tc.path, http.StatusOK))
		for _, want := range tc.want {
			if !strings.Contains(page, want) {
				t.Errorf("%s missing %q", tc.path, want)
			}
		}
		if strings.Contains(page, "Example data") || strings.Contains(page, "Design preview") || strings.Contains(page, ">Ready</th>") || strings.Contains(page, ">Busy</th>") {
			t.Fatalf("preview or obsolete summary leaked into %s", tc.path)
		}
	}
	after, _ := s.Store.Config()
	if !bytes.Equal(before, after) || runtime.starts != 0 || runtime.stops != 0 || len(runtime.commands) != 0 {
		t.Fatal("reading pages changed programming or radio operation")
	}
	runtime.mu.Lock()
	runtime.status.Channels = nil
	runtime.status.State = "running"
	runtime.mu.Unlock()
	var out bytes.Buffer
	s.webSiteChannelTable(&out, runtime.Status())
	if !strings.Contains(out.String(), "Status unavailable") || strings.Contains(out.String(), ">Offline<") {
		t.Fatal("missing worker invented a stopped channel", out.String())
	}
	runtime.mu.Lock()
	runtime.status.State = "stopped"
	runtime.mu.Unlock()
	out.Reset()
	s.webSiteChannelTable(&out, runtime.Status())
	if !strings.Contains(out.String(), ">Offline<") {
		t.Fatal("confirmed stopped channel not identified")
	}
}

// An opt-in disposable cluster for manual visual/keyboard checks. It never opens
// hardware and serves only loopback test listeners with fixture-only accounts.
func TestSiteMockupBrowserFixture(t *testing.T) {
	target := os.Getenv("OPEN25_SITE_UI_FIXTURE")
	if target == "" {
		t.Skip("set OPEN25_SITE_UI_FIXTURE for the disposable browser review")
	}
	c := integrationNewCluster(t)
	for _, pair := range []*integrationService{c.system, c.zone, c.site, c.otherSite, c.dispatch} {
		pair.service.assets = frameworkui.Assets()
	}
	now := time.Now().UTC()
	runtimes := []*operationRadio{}
	for _, pair := range []*integrationService{c.site, c.otherSite} {
		var saved SiteConfig
		if err := pair.service.owned(&saved); err != nil {
			t.Fatal(err)
		}
		saved.Modems = []Modem{{ID: "m1", Name: "Voice modem 1", Device: "/preview/board-one", Baud: 115200}, {ID: "m2", Name: "Voice modem 2", Device: "/preview/board-two", Baud: 115200}, {ID: "m3", Name: "Control modem", Device: "/preview/board-three", Baud: 115200}}
		saved.Channels = []Channel{{ID: "ch1", Name: "Voice channel 1", ModemID: "m1", Identifier: 2, ChannelNo: 81, PowerPercent: 20, Enabled: true}, {ID: "ch2", Name: "Voice channel 2", ModemID: "m2", Identifier: 2, ChannelNo: 61, PowerPercent: 20, Enabled: true}, {ID: "ch3", Name: "Control channel", ModemID: "m3", Identifier: 2, ChannelNo: 1, PowerPercent: 20, Enabled: true}}
		saved.ControlPolicy = ControlPolicy{Mode: "static", ChannelID: "ch1", CandidateIDs: []string{"ch1", "ch2", "ch3"}}
		if err := pair.service.Store.SaveConfig(storeDoc(t, saved)); err != nil {
			t.Fatal(err)
		}
		runtime := &operationRadio{applications: make(chan policyApplication, 128), status: radio.Status{State: "running", Mode: "hardware", Available: true, StartedAt: now.Add(-2 * time.Hour), FNEReady: true, ZoneConnected: true, TrunkingMode: "wide_area", ActiveControlChannelID: "ch1", ControlChangedAt: now.Add(-2 * time.Hour)}}
		for i, ch := range saved.Channels {
			role := "voice"
			if i == 0 {
				role = "control"
			}
			runtime.status.Channels = append(runtime.status.Channels, radio.ChannelStatus{ID: ch.ID, Name: ch.Name, ModemID: ch.ModemID, OperatingModemID: ch.ModemID, OperatingModemName: saved.Modems[i].Name, Device: saved.Modems[i].Device, Role: role, State: "ready", Ready: true, PID: 346391 + i, TXHz: []int64{434450000, 434200000, 433450000}[i], RXHz: []int64{439450000, 439200000, 438450000}[i], StartedAt: now.Add(-2 * time.Hour), Telemetry: radio.ModemTelemetry{StartedAt: now.Add(-2 * time.Hour), Samples: []radio.ModemSample{{At: now, TX: ptrBool(i == 0), RX: ptrBool(false), RXState: "Listening"}}}})
		}
		pair.service.radio = runtime
		runtimes = append(runtimes, runtime)
		if err := pair.service.writeCache("connection", Connection{Name: "Zone", State: "Connected", LastSuccess: now}); err != nil {
			t.Fatal(err)
		}
	}
	// Match the mockup's named owners and frequency reference in this fixture only.
	for _, pair := range []*integrationService{c.system, c.zone, c.site, c.otherSite, c.dispatch} {
		var zone ZoneConfig
		if pair.service.Config().Role == RoleZone {
			_ = pair.service.owned(&zone)
		} else {
			pair.service.readCache("zone", &zone)
		}
		zone.Name = "Zone 1"
		for i := range zone.Sites {
			zone.Sites[i].Name = map[string]string{"site-1": "Main", "site-2": "Test site 2"}[zone.Sites[i].ID]
			zone.Sites[i].AdminURL = map[string]string{"site-1": c.site.web.URL, "site-2": c.otherSite.web.URL}[zone.Sites[i].ID]
		}
		zone.ChannelIdentifiers = []ChannelIdentifier{{Identifier: 2, BaseHz: 433437500, SpacingHz: 12500, OffsetHz: 5000000}}
		if pair.service.Config().Role == RoleZone {
			_ = pair.service.Store.SaveConfig(storeDoc(t, zone))
		} else {
			_ = pair.service.writeCache("zone", zone)
		}
	}
	for _, pair := range []*integrationService{c.system, c.zone, c.site, c.otherSite, c.dispatch} {
		_ = pair.service.writeCache("topology", topology{SystemURL: c.system.web.URL, ZoneURL: c.zone.web.URL, ApplicationURL: c.dispatch.web.URL, SiteURLs: map[string]string{"site-1": c.site.web.URL, "site-2": c.otherSite.web.URL}})
		var system SystemConfig
		if pair.service.Config().Role == RoleSystem {
			_ = pair.service.owned(&system)
		} else {
			pair.service.readCache("system", &system)
		}
		system.Name = "Open25"
		if pair.service.Config().Role == RoleSystem {
			_ = pair.service.Store.SaveConfig(storeDoc(t, system))
		} else {
			_ = pair.service.writeCache("system", system)
		}
	}
	data := map[string]any{"system": c.system.web.URL, "zone": c.zone.web.URL, "site": c.site.web.URL, "other_site": c.otherSite.web.URL, "application": c.dispatch.web.URL, "username": "administrator", "password": integrationPassword}
	if err := os.MkdirAll(filepath.Dir(target), 0700); err != nil {
		t.Fatal(err)
	}
	raw, _ := json.Marshal(data)
	if err := os.WriteFile(target, raw, 0600); err != nil {
		t.Fatal(err)
	}
	fmt.Println("Disposable Site UI fixture ready")
	ctx, cancel := context.WithTimeout(context.Background(), 18*time.Minute)
	defer cancel()
	ticker := time.NewTicker(time.Second)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			if _, err := os.Stat(target + ".done"); err == nil {
				return
			}
			for _, pair := range []*integrationService{c.site, c.otherSite} {
				_ = pair.service.writeCache("connection", Connection{Name: "Zone", State: "Connected", LastSuccess: time.Now()})
			}
			for _, runtime := range runtimes {
				runtime.mu.Lock()
				for i := range runtime.status.Channels {
					ch := &runtime.status.Channels[i]
					ch.Telemetry.Samples = append([]radio.ModemSample(nil), ch.Telemetry.Samples...)
					ch.Telemetry.Samples[len(ch.Telemetry.Samples)-1].At = time.Now()
				}
				runtime.mu.Unlock()
			}
		}
	}
}
