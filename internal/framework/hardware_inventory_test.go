package framework

import (
	"bytes"
	"context"
	"encoding/json"
	"strings"
	"testing"
	"time"

	"open25.local/open25-fne/internal/radio"
)

func hardwareReportFixture(now time.Time, siteID, hostID string) SiteHardwareReport {
	report := SiteHardwareReport{SiteID: siteID, HostID: hostID, Hostname: "display-hostname", BootID: "boot-a", ControllerPID: 101, ControllerStartID: "start-controller", ObservedAt: now, ReceivedAt: now, Modems: []HardwareModemAssignment{{ID: "modem-a", Name: "Assigned modem", Device: "/dev/serial/by-id/assigned", Channels: []HardwareChannelAssignment{{ID: "channel-a", Name: "Voice promoted to control", Role: "voice", RuntimeRole: "control", Enabled: true, State: "running", PID: 202, StartID: "start-worker", Ready: true}}}}}
	report.Workers = []HardwareWorkerObservation{{ModemID: "modem-a", Device: report.Modems[0].Device, HardwareChannelAssignment: report.Modems[0].Channels[0]}}
	return report
}

func TestHardwareInventorySeparatesAssignmentsFromObservedOwners(t *testing.T) {
	now := time.Now().UTC()
	report := hardwareReportFixture(now, "site-2", "host-a")
	inventory := radio.HardwareInventory{HostID: "host-a", BootID: "boot-a", ObservedAt: now, Devices: []radio.HardwareDevice{{ID: "device-a", Device: "/dev/serial/by-id/discovered", Paths: []string{"/dev/serial/by-id/discovered", "/dev/serial/by-id/assigned"}, Present: true, PresenceKnown: true, OwnershipKnown: true, Openers: []radio.HardwareProcess{{PID: 101, Name: "controller", StartID: "start-controller"}, {PID: 202, Name: "dvmhost", StartID: "start-worker"}, {PID: 303, Name: "other-program", StartID: "unknown"}}}}}
	got := buildHardwareInventoryView(inventory, "site-1", []SiteHardwareReport{report}, 10*time.Second)
	if len(got.Devices) != 1 || len(got.Devices[0].Assignments) != 1 || len(got.Devices[0].Owners) != 3 {
		t.Fatalf("wrong inventory projection: %+v", got)
	}
	assignment := got.Devices[0].Assignments[0]
	if assignment.SiteID != "site-2" || assignment.Role != "voice" || assignment.RuntimeRole != "control" || assignment.Local || assignment.Stale || !assignment.Ready {
		t.Fatalf("configuration and actual role became mixed: %+v", assignment)
	}
	owners := got.Devices[0].Owners
	if owners[0].SiteID != "site-2" || owners[0].Role != "Site Controller" || owners[1].ChannelID != "channel-a" || owners[1].Role != "control" || owners[2].SiteID != "" || owners[2].PID != 303 {
		t.Fatalf("actual ownership was invented or lost: %+v", owners)
	}
}

func TestHardwareInventoryDoesNotTrustStaleOtherHostOrReusedPIDs(t *testing.T) {
	now := time.Now().UTC()
	inventory := radio.HardwareInventory{HostID: "host-a", BootID: "boot-a", ObservedAt: now, Devices: []radio.HardwareDevice{{Device: "/dev/serial/by-id/assigned", Present: true, Openers: []radio.HardwareProcess{{PID: 202, StartID: "start-worker"}}}}}
	for _, test := range []struct {
		name           string
		change         func(*SiteHardwareReport)
		wantAssignment bool
		wantStale      bool
	}{
		{"old-observation", func(r *SiteHardwareReport) { r.ObservedAt = now.Add(-time.Minute) }, true, true},
		{"old-receipt", func(r *SiteHardwareReport) { r.ReceivedAt = now.Add(-time.Minute) }, true, true},
		{"missing-observation", func(r *SiteHardwareReport) { r.ObservedAt = time.Time{} }, true, true},
		{"another-host-same-path", func(r *SiteHardwareReport) { r.HostID = "host-b" }, false, false},
		{"unknown-host-same-name", func(r *SiteHardwareReport) { r.HostID = "" }, false, false},
		{"previous-boot", func(r *SiteHardwareReport) { r.BootID = "boot-old" }, true, false},
		{"reused-pid", func(r *SiteHardwareReport) { r.Workers[0].StartID = "start-previous" }, true, false},
		{"unknown-process-lifetime", func(r *SiteHardwareReport) { r.Workers[0].StartID = "" }, true, false},
	} {
		t.Run(test.name, func(t *testing.T) {
			report := hardwareReportFixture(now, "site-2", "host-a")
			test.change(&report)
			got := buildHardwareInventoryView(inventory, "site-1", []SiteHardwareReport{report}, 10*time.Second).Devices[0]
			if (len(got.Assignments) > 0) != test.wantAssignment {
				t.Fatalf("wrong assignment provenance: %+v", got)
			}
			if test.wantAssignment && got.Assignments[0].Stale != test.wantStale {
				t.Fatalf("stale report became fresh: %+v", got.Assignments)
			}
			if len(got.Owners) != 1 || got.Owners[0].SiteID != "" || got.Owners[0].PID != 202 {
				t.Fatalf("unproven report claimed an actual process: %+v", got.Owners)
			}
		})
	}
}

func TestHardwareReportsRelayOnlyNarrowSameHostAssignments(t *testing.T) {
	c := integrationNewCluster(t)
	integrationSyncAll(t, c)
	zone := c.zone.service
	now := time.Now().UTC()
	first := hardwareReportFixture(now, "site-1", "host-a")
	second := hardwareReportFixture(now, "site-2", "host-a")
	reports := []SiteReport{{ID: "site-1", Hardware: &first, Config: SiteConfig{ID: "site-1", Modems: []Modem{{Calibration: Calibration{RXLevel: 98}, Baud: 34567}}}}, {ID: "site-2", Hardware: &second, Config: SiteConfig{ID: "site-2", Modems: []Modem{{Device: "never-relay-whole-config", Baud: 45678}}}}}
	if err := zone.writeCache("site_reports", reports); err != nil {
		t.Fatal(err)
	}
	before, _ := c.site.service.Store.Config()
	incoming := catalog{}
	if err := c.site.service.upstreamRequest(context.Background(), "GET", "/internal/catalog", nil, &incoming); err != nil {
		t.Fatal(err)
	}
	if len(incoming.HardwareReports) != 2 || incoming.Zone == nil || len(incoming.Zone.Sites) != 1 || incoming.Zone.Sites[0].ID != "site-1" {
		t.Fatalf("hardware relay changed the Site catalogue boundary: %+v", incoming.HardwareReports)
	}
	raw, err := json.Marshal(incoming.HardwareReports)
	if err != nil {
		t.Fatal(err)
	}
	for _, forbidden := range []string{"calibration", "baud", "never-relay-whole-config", "password", "key_envelopes", "frequency"} {
		if strings.Contains(string(raw), forbidden) {
			t.Fatalf("narrow report exposed unrelated field %q: %s", forbidden, raw)
		}
	}
	if err := c.site.service.acceptCatalog(incoming); err != nil {
		t.Fatal(err)
	}
	var cached []SiteHardwareReport
	if !c.site.service.readCache("hardware_reports", &cached) || len(cached) != 2 {
		t.Fatal("same-host observations were not cached")
	}
	after, _ := c.site.service.Store.Config()
	if !bytes.Equal(before, after) {
		t.Fatal("receiving hardware observations changed Site configuration")
	}
	second.HostID = "different-host"
	if err := zone.writeCache("site_reports", reports); err != nil {
		t.Fatal(err)
	}
	if got := zone.hardwareReportsForSite("site-1"); len(got) != 1 || got[0].SiteID != "site-1" {
		t.Fatalf("different host leaked into local inventory: %+v", got)
	}
	first.HostID = ""
	if err := zone.writeCache("site_reports", reports); err != nil {
		t.Fatal(err)
	}
	if got := zone.hardwareReportsForSite("site-1"); len(got) != 0 {
		t.Fatalf("unknown host was guessed from hostname: %+v", got)
	}
}

func TestOwnHardwareReportRetainsConfiguredRoleAndActualRecovery(t *testing.T) {
	c := integrationNewCluster(t)
	site := SiteConfig{ID: "site-1", Modems: []Modem{{ID: "modem-a", Device: "/dev/serial/by-id/a"}, {ID: "unassigned", Device: "/dev/serial/by-id/missing"}}, Channels: []Channel{{ID: "channel-a", Name: "Alternate", ModemID: "modem-a", Role: "voice", Enabled: true}}}
	next := time.Now().Add(time.Second).UTC()
	status := radio.Status{Channels: []radio.ChannelStatus{{ID: "channel-a", ModemID: "modem-a", Device: "/dev/serial/by-id/a", Role: "control", State: "retrying", Error: "Actual modem result", RecoveryWanted: true, RecoveryAt: next}}}
	got := c.site.service.ownHardwareReport(site, status)
	if len(got.Modems) != 2 || len(got.Modems[1].Channels) != 0 {
		t.Fatalf("unassigned modem was hidden: %+v", got)
	}
	channel := got.Modems[0].Channels[0]
	if channel.Role != "voice" || channel.RuntimeRole != "control" || channel.State != "retrying" || channel.Error != "Actual modem result" || !channel.RecoveryWanted || !channel.RecoveryAt.Equal(next) {
		t.Fatalf("configured and observed settings were mixed: %+v", channel)
	}
}

func TestHardwareInventoryKeepsLiveWorkersSeparateAfterSavedBindingChanges(t *testing.T) {
	c := integrationNewCluster(t)
	site := SiteConfig{ID: "site-1", Modems: []Modem{{ID: "modem-new", Device: "/dev/serial/by-id/new"}}, Channels: []Channel{{ID: "channel-a", ModemID: "modem-new", Role: "voice", Enabled: true}}}
	status := radio.Status{Channels: []radio.ChannelStatus{{ID: "channel-a", Name: "Actual old worker", ModemID: "modem-old", Device: "/dev/serial/by-id/old", Role: "control", ConfiguredRole: "control", State: "running", Ready: true, PID: 202}}}
	report := c.site.service.ownHardwareReport(site, status)
	if report.Modems[0].Channels[0].Ready || report.Modems[0].Channels[0].PID != 0 || len(report.Workers) != 1 || report.Workers[0].Device != "/dev/serial/by-id/old" {
		t.Fatalf("saved rebinding inherited old worker readiness: %+v", report)
	}
	now := time.Now().UTC()
	report.HostID, report.BootID, report.ObservedAt, report.ReceivedAt = "host-a", "boot-a", now, now
	report.Workers[0].StartID = "start-worker"
	inventory := radio.HardwareInventory{HostID: "host-a", BootID: "boot-a", ObservedAt: now, Devices: []radio.HardwareDevice{{Device: "/dev/serial/by-id/old", Present: true, Openers: []radio.HardwareProcess{{PID: 202, StartID: "start-worker"}}}, {Device: "/dev/serial/by-id/new", Present: true}}}
	got := buildHardwareInventoryView(inventory, "site-1", []SiteHardwareReport{report}, 10*time.Second)
	if len(got.Devices[0].Assignments) != 0 || len(got.Devices[0].Observations) != 1 || got.Devices[0].Owners[0].ChannelID != "channel-a" || len(got.Devices[1].Assignments) != 1 || len(got.Devices[1].Observations) != 0 {
		t.Fatalf("current hardware use was lost or attributed to the new saved device: %+v", got)
	}
}

func TestOwnHardwareReportShowsStoppedOrUnavailableWithoutInventingReadiness(t *testing.T) {
	c := integrationNewCluster(t)
	site := storeFixtureSite()
	for _, status := range []radio.Status{{Available: true, State: "stopped"}, {Available: false, State: "stopped", Error: "Radio executables are not configured at this Site."}} {
		report := c.site.service.ownHardwareReport(site, status)
		for _, modem := range report.Modems {
			for _, channel := range modem.Channels {
				want := "stopped"
				if !status.Available {
					want = "unavailable"
				}
				if channel.State != want || channel.Error != status.Error || channel.Ready || channel.PID != 0 {
					t.Fatalf("missing runtime row was presented ambiguously: %+v", channel)
				}
			}
		}
	}
}
