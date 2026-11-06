//go:build linux || darwin

package radio

import (
	"context"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"syscall"
	"testing"
	"time"
)

func selectionProfile() Profile {
	p := fixtureProfile()
	p.Site.ControlPolicy = ControlPolicy{Mode: "dynamic", RotationHours: 24}
	for i := range p.Site.Channels {
		p.Site.Channels[i].ControlEligible = true
		p.Site.Channels[i].ControlPriority = i
	}
	return p
}

func selectionProfileThree() Profile {
	p := selectionProfile()
	p.Site.Modems = append(p.Site.Modems, Modem{ID: "voice2", Name: "Voice 2", Device: "/dev/serial/by-id/voice2", Baud: 115200, Calibration: Calibration{RXLevel: 50, TXLevel: 50}})
	p.Site.Channels = append(p.Site.Channels, Channel{ID: "voice-2", Name: "Voice 2", Role: "voice", Identifier: 2, ChannelNo: 121, TXHz: 444950000, RXHz: 449950000, PowerPercent: 80, ModemID: "voice2", Enabled: true, ControlEligible: true, ControlPriority: 2})
	return p
}

func selectedChannel(c *Controller, id string) ChannelStatus {
	for _, row := range c.Status().Channels {
		if row.ID == id {
			return row
		}
	}
	return ChannelStatus{}
}

func selectionWorker(c *Controller, id string) *child {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.children[id]
}

func selectionFlag(t *testing.T, worker *child, name string) string {
	t.Helper()
	path := filepath.Join(worker.dir, name)
	if err := os.WriteFile(path, []byte("offline fixture"), 0600); err != nil {
		t.Fatal(err)
	}
	return path
}

func waitSelected(t *testing.T, c *Controller, id string) {
	t.Helper()
	waitRadio(t, "selected control channel did not become ready: "+id, func() bool {
		status := c.Status()
		c.mu.Lock()
		pending := c.controlSwitch != nil
		c.mu.Unlock()
		return status.State == "running" && status.ActiveControlChannelID == id && selectedChannel(c, id).Ready && !pending
	})
}

func assertSelectionUnchanged(t *testing.T, c *Controller, configured Profile) {
	t.Helper()
	c.mu.Lock()
	snapshot := cloneProfile(c.configuredProfile)
	c.mu.Unlock()
	if !reflect.DeepEqual(snapshot.Site, configured.Site) {
		t.Fatal("runtime control selection rewrote saved role, modem or frequency configuration")
	}
	for _, channel := range configured.Site.Channels {
		row := selectedChannel(c, channel.ID)
		modem, _ := findModem(configured, channel.ModemID)
		if row.ConfiguredRole != channel.Role || row.TXHz != channel.TXHz || row.RXHz != channel.RXHz || row.Device != modem.Device || row.ModemID != channel.ModemID {
			t.Fatalf("channel %s lost configured role or physical pair/device: %+v", channel.ID, row)
		}
	}
}

func TestControlSelectionManualWaitsForWholeSitePermitAndKeepsPhysicalAssignments(t *testing.T) {
	c := testController(t, nil)
	waiting, retry := holdSiteRetries(c)
	p := selectionProfile()
	if err := c.Start(context.Background(), p, "null"); err != nil {
		t.Fatal(err)
	}
	before := c.Status()
	oldControl, oldVoice := selectionWorker(c, "control-1"), selectionWorker(c, "voice-1")
	// /status still reports tx:false. The native quiescence result, including
	// a permit on the OTHER worker, must control the switch decision.
	busy := selectionFlag(t, oldControl, "busy-permit")
	if err := c.SwitchControl(context.Background(), "voice-1"); err != nil {
		t.Fatal(err)
	}
	nextSiteRetry(t, waiting, 500*time.Millisecond)
	if c.Status().ActiveControlChannelID != "control-1" || !oldControl.alive() || !oldVoice.alive() || c.Status().FNEPID != before.FNEPID {
		t.Fatal("manual switch retired an RF process while a Site permit was busy")
	}
	if _, err := os.Stat(filepath.Join(oldControl.dir, "quiesced")); err != nil {
		t.Fatal("old control authority was not fenced before checking other workers")
	}
	if err := os.Remove(busy); err != nil {
		t.Fatal(err)
	}
	retry <- struct{}{}
	waitSelected(t, c, "voice-1")
	if selectedChannel(c, "voice-1").Role != "control" || selectedChannel(c, "control-1").Role != "voice" {
		t.Fatal("promotion did not swap operating roles")
	}
	assertSelectionUnchanged(t, c, p)
	if err := c.SwitchControl(context.Background(), "control-1"); err != nil {
		t.Fatal(err)
	}
	waitSelected(t, c, "control-1")
	assertSelectionUnchanged(t, c, p)
}

func TestControlSelectionStaticOutageRecoversAssignedControl(t *testing.T) {
	c := testController(t, nil)
	waiting, retry := holdSiteRetries(c)
	p := selectionProfile()
	p.Site.ControlPolicy.Mode = "static"
	if err := c.Start(context.Background(), p, "null"); err != nil {
		t.Fatal(err)
	}
	old := selectionWorker(c, "control-1")
	if err := syscall.Kill(old.cmd.Process.Pid, syscall.SIGKILL); err != nil {
		t.Fatal(err)
	}
	nextSiteRetry(t, waiting, time.Second)
	retry <- struct{}{}
	waitSelected(t, c, "control-1")
	if selectedChannel(c, "voice-1").Role != "voice" {
		t.Fatal("static failure promoted a voice channel")
	}
	assertSelectionUnchanged(t, c, p)
}

func TestControlSelectionDynamicOutagePromotesAndKeepsReplacement(t *testing.T) {
	c := testController(t, nil)
	waiting, retry := holdSiteRetries(c)
	p := selectionProfile()
	if err := c.Start(context.Background(), p, "null"); err != nil {
		t.Fatal(err)
	}
	oldControl, oldVoice := selectionWorker(c, "control-1"), selectionWorker(c, "voice-1")
	busy := selectionFlag(t, oldVoice, "busy-permit")
	if err := syscall.Kill(oldControl.cmd.Process.Pid, syscall.SIGKILL); err != nil {
		t.Fatal(err)
	}
	nextSiteRetry(t, waiting, 500*time.Millisecond)
	if !oldVoice.alive() || selectedChannel(c, "voice-1").PID != oldVoice.cmd.Process.Pid {
		t.Fatal("dynamic outage preempted the busy voice worker")
	}
	if err := os.Remove(busy); err != nil {
		t.Fatal(err)
	}
	retry <- struct{}{}
	waitSelected(t, c, "voice-1")
	recoveredControl := selectedChannel(c, "control-1")
	if recoveredControl.Role != "voice" || !recoveredControl.Ready || recoveredControl.PID == oldControl.cmd.Process.Pid {
		t.Fatal("original control did not return as an independent voice resource")
	}
	c.scheduleControlRotation(time.Now().Add(48 * time.Hour))
	if c.Status().ActiveControlChannelID != "voice-1" || c.Status().ControlSwitchTarget != "" {
		t.Fatal("a recovered preferred modem automatically displaced the replacement")
	}
	assertSelectionUnchanged(t, c, p)
}

func TestControlSelectionRotationUsesSuccessfulActivationAndSkipsStoppedCandidate(t *testing.T) {
	c := testController(t, nil)
	t.Setenv("RADIO_DELAY_CHANNEL", "control-1")
	t.Setenv("RADIO_STARTUP_DELAY", "150ms")
	p := selectionProfileThree()
	p.Site.ControlPolicy.RotationEnabled = true
	if err := c.Start(context.Background(), p, "null"); err != nil {
		t.Fatal(err)
	}
	before := c.Status()
	if !before.ControlChangedAt.After(before.StartedAt.Add(100*time.Millisecond)) || before.NextControlRotation.Sub(before.ControlChangedAt) != 24*time.Hour {
		t.Fatalf("rotation timer did not start with successful control activation: %+v", before)
	}
	if err := c.StopChannel(context.Background(), "voice-1"); err != nil {
		t.Fatal(err)
	}
	c.scheduleControlRotation(before.NextControlRotation.Add(-time.Nanosecond))
	if c.Status().ControlSwitchTarget != "" {
		t.Fatal("rotation began before its interval expired")
	}
	c.scheduleControlRotation(before.NextControlRotation)
	waitSelected(t, c, "voice-2")
	if selectedChannel(c, "voice-1").State != "stopped" || selectedChannel(c, "voice-1").PID != 0 {
		t.Fatal("rotation selected or restarted an intentionally stopped voice channel")
	}
	after := c.Status()
	if after.NextControlRotation.Sub(after.ControlChangedAt) != 24*time.Hour || !after.ControlChangedAt.After(before.ControlChangedAt) {
		t.Fatal("successful rotation did not reset the next 24-hour interval")
	}
	assertSelectionUnchanged(t, c, p)
}

func TestControlSelectionStopCancelsWaitingManualSwitch(t *testing.T) {
	c := testController(t, nil)
	waiting, _ := holdSiteRetries(c)
	if err := c.Start(context.Background(), selectionProfile(), "null"); err != nil {
		t.Fatal(err)
	}
	selectionFlag(t, selectionWorker(c, "voice-1"), "busy-permit")
	if err := c.SwitchControl(context.Background(), "voice-1"); err != nil {
		t.Fatal(err)
	}
	nextSiteRetry(t, waiting, 500*time.Millisecond)
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	if err := c.Stop(ctx); err != nil {
		t.Fatal(err)
	}
	waitRadio(t, "pending control switch survived Stop", func() bool {
		c.mu.Lock()
		defer c.mu.Unlock()
		return c.controlSwitch == nil
	})
	if c.Status().State != "stopped" || c.Status().ActiveControlChannelID != "" || c.Status().ControlSwitchTarget != "" {
		t.Fatal("explicit Stop left selected or pending control activity")
	}
}

func TestControlSelectionRejectsUnknownIneligibleDisabledAndAlreadyActiveTargets(t *testing.T) {
	c := testController(t, nil)
	p := selectionProfile()
	p.Site.Channels[1].ControlEligible = false
	p.Site.Channels = append(p.Site.Channels, Channel{ID: "disabled", Role: "voice", ControlEligible: true})
	if err := c.Start(context.Background(), p, "null"); err != nil {
		t.Fatal(err)
	}
	before := c.Status()
	for _, target := range []string{"unknown", "voice-1", "disabled", "control-1"} {
		if err := c.SwitchControl(context.Background(), target); err == nil {
			t.Errorf("runtime target %q was accepted", target)
		}
	}
	if c.Status().ControlSwitchTarget != "" || c.Status().ActiveControlChannelID != "control-1" || c.Status().FNEPID != before.FNEPID {
		t.Fatal("a rejected runtime selection changed the active Site")
	}
}

func TestControlSelectionRequiresExplicitNativeIdleConfirmation(t *testing.T) {
	for _, marker := range []string{"unknown-quiesce", "reject-quiesce"} {
		t.Run(marker, func(t *testing.T) {
			c := testController(t, nil)
			waiting, _ := holdSiteRetries(c)
			if err := c.Start(context.Background(), selectionProfile(), "null"); err != nil {
				t.Fatal(err)
			}
			before := c.Status()
			oldControl, oldVoice := selectionWorker(c, "control-1"), selectionWorker(c, "voice-1")
			selectionFlag(t, oldVoice, marker)
			if err := c.SwitchControl(context.Background(), "voice-1"); err != nil {
				t.Fatal(err)
			}
			nextSiteRetry(t, waiting, time.Second)
			if !oldControl.alive() || !oldVoice.alive() || c.Status().FNEPID != before.FNEPID || c.Status().ActiveControlChannelID != "control-1" || !strings.Contains(c.Status().ControlSwitchError, "native") {
				t.Fatal("unknown/rejected native idleness retired the active Site or claimed success")
			}
			if err := c.Stop(context.Background()); err != nil {
				t.Fatal(err)
			}
		})
	}
}

func TestControlSelectionPendingManualSwitchSurvivesControlOutage(t *testing.T) {
	c := testController(t, nil)
	waiting, retry := holdSiteRetries(c)
	if err := c.Start(context.Background(), selectionProfile(), "null"); err != nil {
		t.Fatal(err)
	}
	oldControl, oldVoice := selectionWorker(c, "control-1"), selectionWorker(c, "voice-1")
	busy := selectionFlag(t, oldVoice, "busy-permit")
	if err := c.SwitchControl(context.Background(), "voice-1"); err != nil {
		t.Fatal(err)
	}
	nextSiteRetry(t, waiting, 500*time.Millisecond)
	if err := syscall.Kill(oldControl.cmd.Process.Pid, syscall.SIGKILL); err != nil {
		t.Fatal(err)
	}
	waitRadio(t, "pending switch did not observe failed control authority", func() bool { return selectedChannel(c, "control-1").State == "fault" })
	retry <- struct{}{}
	nextSiteRetry(t, waiting, 500*time.Millisecond)
	if !oldVoice.alive() {
		t.Fatal("control loss during a queued manual switch interrupted the busy voice call")
	}
	if err := os.Remove(busy); err != nil {
		t.Fatal(err)
	}
	retry <- struct{}{}
	waitSelected(t, c, "voice-1")
}

func TestControlSelectionDynamicWithoutAlternateRetriesOriginalAfterFailedStartup(t *testing.T) {
	c := testController(t, nil)
	waiting, retry := holdSiteRetries(c)
	p := selectionProfile()
	p.Site.Channels[1].ControlEligible = false
	if err := c.Start(context.Background(), p, "null"); err != nil {
		t.Fatal(err)
	}
	t.Setenv("RADIO_FAIL_CHANNEL", "control-1")
	if err := syscall.Kill(selectionWorker(c, "control-1").cmd.Process.Pid, syscall.SIGKILL); err != nil {
		t.Fatal(err)
	}
	// This path retires TWO complete native lifetimes before retry: the
	// failed old run and the deliberately failed replacement. Race-enabled
	// helper processes each add their own exit delay; this setup deadline
	// does not alter production readiness or recovery/backoff timers.
	select {
	case delay := <-waiting:
		if delay != time.Second {
			t.Fatalf("failed startup retry delay=%v, want 1s", delay)
		}
	case <-time.After(8 * time.Second):
		t.Fatalf("failed startup never reached retry: %+v", c.Status())
	}
	if c.Status().State == "running" {
		t.Fatal("failed original control startup was reported as working")
	}
	t.Setenv("RADIO_FAIL_CHANNEL", "")
	retry <- struct{}{}
	waitSelected(t, c, "control-1")
	assertSelectionUnchanged(t, c, p)
}

func TestControlSelectionConfigurationChangeCancelsWaitAndResumesOldNativeWorkers(t *testing.T) {
	c := testController(t, nil)
	waiting, _ := holdSiteRetries(c)
	p := selectionProfile()
	if err := c.Start(context.Background(), p, "null"); err != nil {
		t.Fatal(err)
	}
	oldControl, oldVoice := selectionWorker(c, "control-1"), selectionWorker(c, "voice-1")
	selectionFlag(t, oldVoice, "busy-permit")
	if err := c.SwitchControl(context.Background(), "voice-1"); err != nil {
		t.Fatal(err)
	}
	nextSiteRetry(t, waiting, 500*time.Millisecond)
	p.Site.ControlPolicy.RotationHours = 48
	if err := c.ApplyPolicy(context.Background(), p); err == nil || !strings.Contains(err.Error(), "stop and start") {
		t.Fatal("edited RF policy did not remain pending explicit Stop/Start")
	}
	waitRadio(t, "configuration change left native call admissions fenced", func() bool {
		_, controlErr := os.Stat(filepath.Join(oldControl.dir, "quiesced"))
		_, voiceErr := os.Stat(filepath.Join(oldVoice.dir, "quiesced"))
		c.mu.Lock()
		pending := c.controlSwitch != nil
		c.mu.Unlock()
		return !pending && os.IsNotExist(controlErr) && os.IsNotExist(voiceErr)
	})
	if !oldControl.alive() || !oldVoice.alive() || c.Status().ActiveControlChannelID != "control-1" || c.Status().State != "running" {
		t.Fatal("saving physical policy replaced the old operating Site")
	}
}

func TestControlSelectionPendingSwitchRetainsLaterVoiceStopAndStart(t *testing.T) {
	for _, action := range []string{"stop", "start"} {
		t.Run(action, func(t *testing.T) {
			c := testController(t, nil)
			waiting, retry := holdSiteRetries(c)
			p := selectionProfileThree()
			if err := c.Start(context.Background(), p, "null"); err != nil {
				t.Fatal(err)
			}
			if action == "start" {
				if err := c.StopChannel(context.Background(), "voice-2"); err != nil {
					t.Fatal(err)
				}
			}
			busy := selectionFlag(t, selectionWorker(c, "control-1"), "busy-permit")
			if err := c.SwitchControl(context.Background(), "voice-1"); err != nil {
				t.Fatal(err)
			}
			nextSiteRetry(t, waiting, 500*time.Millisecond)
			var err error
			if action == "stop" {
				err = c.StopChannel(context.Background(), "voice-2")
			} else {
				err = c.StartChannel(context.Background(), "voice-2")
			}
			if err != nil {
				t.Fatal(err)
			}
			if err := os.Remove(busy); err != nil {
				t.Fatal(err)
			}
			retry <- struct{}{}
			waitSelected(t, c, "voice-1")
			row := selectedChannel(c, "voice-2")
			if action == "stop" && (row.PID != 0 || row.State != "stopped" || row.Ready) {
				t.Fatalf("pending switch replayed stale desired state after explicit Stop: %+v", row)
			}
			if action == "start" && (row.PID == 0 || !row.Ready) {
				t.Fatalf("pending switch lost later explicit Start intent: %+v", row)
			}
			assertSelectionUnchanged(t, c, p)
		})
	}
}

func TestControlSelectionStoppingPendingTargetCancelsSwitch(t *testing.T) {
	c := testController(t, nil)
	waiting, _ := holdSiteRetries(c)
	if err := c.Start(context.Background(), selectionProfileThree(), "null"); err != nil {
		t.Fatal(err)
	}
	control := selectionWorker(c, "control-1")
	selectionFlag(t, control, "busy-permit")
	if err := c.SwitchControl(context.Background(), "voice-1"); err != nil {
		t.Fatal(err)
	}
	nextSiteRetry(t, waiting, 500*time.Millisecond)
	if err := c.StopChannel(context.Background(), "voice-1"); err != nil {
		t.Fatal(err)
	}
	waitRadio(t, "stopping the pending target did not cancel/unfence the switch", func() bool {
		c.mu.Lock()
		pending := c.controlSwitch != nil
		c.mu.Unlock()
		_, err := os.Stat(filepath.Join(control.dir, "quiesced"))
		return !pending && os.IsNotExist(err)
	})
	if !control.alive() || c.Status().ActiveControlChannelID != "control-1" || selectedChannel(c, "voice-1").State != "stopped" || selectedChannel(c, "voice-1").PID != 0 {
		t.Fatal("target Stop was overridden by a previously queued promotion")
	}
}

func TestControlSelectionDynamicOutageReplacesUnavailablePendingManualTarget(t *testing.T) {
	c := testController(t, nil)
	waiting, retry := holdSiteRetries(c)
	p := selectionProfileThree()
	if err := c.Start(context.Background(), p, "null"); err != nil {
		t.Fatal(err)
	}
	control, target, alternative := selectionWorker(c, "control-1"), selectionWorker(c, "voice-1"), selectionWorker(c, "voice-2")
	busy := selectionFlag(t, alternative, "busy-permit")
	if err := c.SwitchControl(context.Background(), "voice-1"); err != nil {
		t.Fatal(err)
	}
	nextSiteRetry(t, waiting, 500*time.Millisecond)
	// A failed target remains unavailable even if its ordinary voice recovery
	// races the pending switch. The third channel is healthy but still busy.
	t.Setenv("RADIO_FAIL_CHANNEL", "voice-1")
	for _, worker := range []*child{control, target} {
		if err := syscall.Kill(worker.cmd.Process.Pid, syscall.SIGKILL); err != nil {
			t.Fatal(err)
		}
	}
	waitRadio(t, "manual switch did not observe failed authority and target", func() bool {
		return selectedChannel(c, "control-1").State == "fault" && !target.alive()
	})
	if err := os.Remove(busy); err != nil {
		t.Fatal(err)
	}
	retry <- struct{}{}
	waitSelected(t, c, "voice-2")
	if selectedChannel(c, "voice-1").Ready {
		t.Fatal("failed manual candidate was shown as ready after alternate promotion")
	}
	assertSelectionUnchanged(t, c, p)
}

func TestControlSelectionDrainingRejectsNewRadioCommands(t *testing.T) {
	c := testController(t, nil)
	waiting, retry := holdSiteRetries(c)
	if err := c.Start(context.Background(), selectionProfile(), "null"); err != nil {
		t.Fatal(err)
	}
	if _, err := c.Command(context.Background(), Command{Action: "radio_check", RID: 10001}); err != nil {
		t.Fatal("ordinary ready control command failed:", err)
	}
	busy := selectionFlag(t, selectionWorker(c, "voice-1"), "busy-permit")
	if err := c.SwitchControl(context.Background(), "voice-1"); err != nil {
		t.Fatal(err)
	}
	nextSiteRetry(t, waiting, 500*time.Millisecond)
	for _, action := range []string{"radio_check", "page", "message", "registration_demand", "affiliation_query", "status_query", "inhibit", "uninhibit", "regroup", "regroup_cancel"} {
		if _, err := c.Command(context.Background(), Command{Action: action, RID: 10001, TGID: 1001, Code: 1}); err == nil || !strings.Contains(err.Error(), "pending control-channel switch") {
			t.Errorf("command %s was not refused while draining: %v", action, err)
		}
	}
	if err := os.Remove(busy); err != nil {
		t.Fatal(err)
	}
	retry <- struct{}{}
	waitSelected(t, c, "voice-1")
	if _, err := c.Command(context.Background(), Command{Action: "radio_check", RID: 10001}); err != nil {
		t.Fatal("commands did not resume with the replacement control:", err)
	}
}

func TestControlSelectionUnscheduledEnteredIntervalRemainsReportedAfterManualSwitch(t *testing.T) {
	c := testController(t, nil)
	p := selectionProfile()
	p.Site.ControlPolicy.RotationEnabled = true
	p.Site.ControlPolicy.RotationHours = 0
	if err := c.Start(context.Background(), p, "null"); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(c.Status().ControlSwitchError, "interval") {
		t.Fatal("unusable entered interval was silently ignored")
	}
	if err := c.SwitchControl(context.Background(), "voice-1"); err != nil {
		t.Fatal(err)
	}
	waitSelected(t, c, "voice-1")
	status := c.Status()
	if !status.NextControlRotation.IsZero() || !strings.Contains(status.ControlSwitchError, "interval") {
		t.Fatal("successful manual switch hid the unscheduled entered interval", status)
	}
	assertSelectionUnchanged(t, c, p)
}

func TestControlSelectionRetainsAliasEditAcceptedWhileWaitingForIdle(t *testing.T) {
	c := testController(t, nil)
	waiting, retry := holdSiteRetries(c)
	p := selectionProfile()
	if err := c.Start(context.Background(), p, "null"); err != nil {
		t.Fatal(err)
	}
	busy := selectionFlag(t, selectionWorker(c, "voice-1"), "busy-permit")
	if err := c.SwitchControl(context.Background(), "voice-1"); err != nil {
		t.Fatal(err)
	}
	nextSiteRetry(t, waiting, 500*time.Millisecond)
	p.System.Radios[0].Alias = "Updated Alias"
	if err := c.ApplyPolicy(context.Background(), p); err != nil {
		t.Fatal(err)
	}
	if err := os.Remove(busy); err != nil {
		t.Fatal(err)
	}
	retry <- struct{}{}
	waitSelected(t, c, "voice-1")
	c.mu.Lock()
	actual := c.profile.System.Radios[0].Alias
	configured := c.configuredProfile.System.Radios[0].Alias
	directory := c.children["voice-1"].dir
	c.mu.Unlock()
	lookup, err := os.ReadFile(filepath.Join(directory, "rid_acl.dat"))
	if err != nil || actual != "Updated Alias" || configured != "Updated Alias" || !strings.Contains(string(lookup), "Updated Alias") {
		t.Fatalf("control replacement resurrected an obsolete alias: actual=%q configured=%q lookup=%s err=%v", actual, configured, lookup, err)
	}
}
