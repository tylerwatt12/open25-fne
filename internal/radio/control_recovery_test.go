//go:build linux || darwin

package radio

import (
	"context"
	"errors"
	"io"
	"strings"
	"sync"
	"sync/atomic"
	"syscall"
	"testing"
	"time"
)

// Timer delivery is controlled, but production process ownership, readiness,
// cleanup and authenticated local HTTP handshakes all run unchanged. No device
// is opened: even hardware-mode fixtures replace every modem operation.
func holdSiteRetries(c *Controller) (<-chan time.Duration, chan<- struct{}) {
	waiting := make(chan time.Duration, 16)
	release := make(chan struct{})
	c.retryWait = func(ctx context.Context, delay time.Duration) error {
		select {
		case waiting <- delay:
		case <-ctx.Done():
			return ctx.Err()
		}
		select {
		case <-release:
			return nil
		case <-ctx.Done():
			return ctx.Err()
		}
	}
	return waiting, release
}

func nextSiteRetry(t *testing.T, waiting <-chan time.Duration, want time.Duration) {
	t.Helper()
	select {
	case delay := <-waiting:
		if delay != want {
			t.Fatalf("control recovery backoff %v, want %v", delay, want)
		}
	// Existing cleanup may consume its five-second deadline before this wait.
	case <-time.After(8 * time.Second):
		t.Fatal("control recovery did not reach its bounded retry wait")
	}
}

func controlOwner(c *Controller) *child {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.children["control-1"]
}

func TestControlRecoveryWaitsForSameBoardAndResumesWithMissingVoice(t *testing.T) {
	c := testController(t, nil)
	waiting, retry := holdSiteRetries(c)
	c.opts.AllowRF = true
	var controlAvailable, voiceAvailable atomic.Bool
	var verified atomic.Int32
	var pathMu sync.Mutex
	var paths []string
	available := func(path string) bool {
		return path == "/dev/serial/by-id/control" && controlAvailable.Load() || path == "/dev/serial/by-id/voice" && voiceAvailable.Load()
	}
	c.modem = modemOperations{
		resolve: func(path string) (string, error) { return path, nil },
		acquire: func(ctx context.Context, path string) (io.Closer, error) {
			pathMu.Lock()
			paths = append(paths, path)
			pathMu.Unlock()
			if !available(path) {
				return nil, errors.New("selected board is unavailable")
			}
			return &recoveryLease{}, ctx.Err()
		},
		verify: func(ctx context.Context, _ io.Closer, _ string, _ bool) error { verified.Add(1); return ctx.Err() },
		same: func(lease io.Closer, path string) bool {
			return available(path) && !lease.(*recoveryLease).closed.Load()
		},
	}
	if err := c.Start(context.Background(), fixtureProfile(), "null"); err != nil {
		t.Fatal(err)
	}
	before, oldControl := c.Status(), controlOwner(c)
	c.mu.Lock()
	oldRun := c.runID
	c.status.Mode = "hardware" // helpers remain null; subsequent leases are fake
	c.status.Channels[1].Busy, c.status.Channels[1].RID, c.status.Channels[1].TGID = true, 10001, 1001
	c.registrations[10001] = Registration{RID: 10001, At: time.Now()}
	c.affiliations[10001] = Affiliation{RID: 10001, TGID: 1001, At: time.Now()}
	c.mu.Unlock()
	_ = syscall.Kill(before.Channels[0].PID, syscall.SIGKILL)
	nextSiteRetry(t, waiting, time.Second)
	if c.Status().Channels[0].State != "fault" || c.Status().Channels[0].PID != 0 {
		t.Fatal("pending control retry was shown as intentionally stopped")
	}
	for _, pid := range []int{before.FNEPID, before.Channels[0].PID, before.Channels[1].PID} {
		if syscall.Kill(pid, 0) == nil {
			t.Fatal("old call/media resource survived control retirement")
		}
	}
	if _, err := c.ControlClient(); err == nil {
		t.Fatal("failed Site exposed an operational control client")
	}
	if _, err := c.ChannelClient("voice-1"); err == nil {
		t.Fatal("failed Site exposed an old voice client")
	}
	for failure := uint(1); failure <= 6; failure++ {
		retry <- struct{}{}
		nextSiteRetry(t, waiting, recoveryDelay(failure))
		if c.Status().State != "fault" || c.Status().FNEPID != 0 {
			t.Fatal("missing control board started native resources")
		}
	}
	controlAvailable.Store(true)
	retry <- struct{}{}
	waitRadio(t, "same control board did not resume degraded Site", func() bool { return c.Status().State == "running" })
	after := c.Status()
	c.mu.Lock()
	wanted := c.recovery["voice-1"].wanted
	newRun := c.runID
	c.mu.Unlock()
	if newRun == oldRun || after.Channels[0].PID == before.Channels[0].PID || after.FNEPID == before.FNEPID {
		t.Fatal("control recovery reused the failed lifetime")
	}
	if voice(c).State != "fault" || voice(c).PID != 0 || !wanted || voice(c).Error == "" {
		t.Fatal("missing voice was hidden or lost desired-running intent")
	}
	if len(after.Registrations) != 0 || len(after.Affiliations) != 0 || voice(c).Busy || voice(c).RID != 0 || voice(c).TGID != 0 {
		t.Fatal("transient old-run call state crossed the recovery boundary")
	}
	voiceAvailable.Store(true)
	forceVoiceRetry(c)
	waitRadio(t, "same voice board did not resume independently", func() bool { return voice(c).Ready })
	if c.Status().Channels[0].PID != after.Channels[0].PID || c.Status().FNEPID != after.FNEPID || verified.Load() < 2 {
		t.Fatal("voice retry replaced control/media or skipped identity verification")
	}
	c.nativeExited(oldControl)
	c.observe(oldControl, "P25, RF voice transmission from 10001 to TG 1001")
	if c.Status().State != "running" || c.Status().Channels[0].PID != after.Channels[0].PID {
		t.Fatal("old owner invalidated the recovered Site")
	}
	pathMu.Lock()
	defer pathMu.Unlock()
	for _, path := range paths {
		if path != "/dev/serial/by-id/control" && path != "/dev/serial/by-id/voice" {
			t.Errorf("recovery changed a stable role path: %s", path)
		}
	}
}

func TestControlRecoveryPreservesExplicitVoiceStop(t *testing.T) {
	c := testController(t, nil)
	waiting, retry := holdSiteRetries(c)
	if err := c.Start(context.Background(), fixtureProfile(), "null"); err != nil {
		t.Fatal(err)
	}
	if err := c.StopChannel(context.Background(), "voice-1"); err != nil {
		t.Fatal(err)
	}
	old := controlOwner(c)
	_ = syscall.Kill(old.cmd.Process.Pid, syscall.SIGKILL)
	nextSiteRetry(t, waiting, time.Second)
	retry <- struct{}{}
	waitRadio(t, "control did not recover", func() bool { return c.Status().State == "running" })
	c.recoverVoiceChannels(time.Now().Add(time.Hour))
	c.mu.Lock()
	wanted := c.recovery["voice-1"].wanted
	c.mu.Unlock()
	if wanted || voice(c).State != "stopped" || voice(c).PID != 0 {
		t.Fatal("control recovery resumed a manually stopped voice resource")
	}
}

func TestControlRecoveryIdentityLossAndRejectedFirmwareProbe(t *testing.T) {
	c := testController(t, nil)
	waiting, retry := holdSiteRetries(c)
	c.opts.AllowRF = true
	var present, qualified atomic.Bool
	present.Store(true)
	var leaseMu sync.Mutex
	var leases []*recoveryLease
	c.modem = modemOperations{
		resolve: func(path string) (string, error) { return path, nil },
		acquire: func(ctx context.Context, path string) (io.Closer, error) {
			if path == "/dev/serial/by-id/control" && !present.Load() {
				return nil, errors.New("same control board has not returned")
			}
			lease := &recoveryLease{}
			leaseMu.Lock()
			leases = append(leases, lease)
			leaseMu.Unlock()
			return lease, ctx.Err()
		},
		verify: func(ctx context.Context, _ io.Closer, path string, _ bool) error {
			if path == "/dev/serial/by-id/control" && !qualified.Load() {
				return errors.New("selected board firmware identity is not qualified")
			}
			return ctx.Err()
		},
		same: func(lease io.Closer, path string) bool {
			return (path != "/dev/serial/by-id/control" || present.Load()) && !lease.(*recoveryLease).closed.Load()
		},
	}
	if err := c.Start(context.Background(), fixtureProfile(), "null"); err != nil {
		t.Fatal(err)
	}
	old := controlOwner(c)
	oldLease := &recoveryLease{}
	old.mu.Lock()
	old.lease, old.sameDevice = oldLease, c.modem.same
	old.mu.Unlock()
	c.mu.Lock()
	c.status.Mode = "hardware"
	c.mu.Unlock()
	// A USB reset/re-enumeration invalidates the old descriptor even when its
	// native process has not yet reported an I/O error or exited by itself.
	present.Store(false)
	nextSiteRetry(t, waiting, time.Second)
	if old.alive() || !oldLease.closed.Load() {
		t.Fatal("identity loss did not retire the old descriptor and engine")
	}
	present.Store(true)
	retry <- struct{}{}
	nextSiteRetry(t, waiting, 2*time.Second)
	leaseMu.Lock()
	if len(leases) != 1 || !leases[0].closed.Load() {
		leaseMu.Unlock()
		t.Fatal("rejected identity probe retained an unowned descriptor")
	}
	leaseMu.Unlock()
	if c.Status().State != "fault" || c.Status().FNEPID != 0 {
		t.Fatal("unqualified firmware was allowed to launch the Site")
	}
	qualified.Store(true)
	retry <- struct{}{}
	waitRadio(t, "qualified same board did not recover after reset", func() bool { return c.Status().State == "running" })
}

func TestInitialSiteStartStillRequiresEveryWantedVoiceModem(t *testing.T) {
	c := testController(t, nil)
	c.opts.AllowRF = true
	lease := &recoveryLease{}
	c.modem = modemOperations{
		resolve: func(path string) (string, error) { return path, nil },
		acquire: func(ctx context.Context, path string) (io.Closer, error) {
			if path == "/dev/serial/by-id/voice" {
				return nil, errors.New("selected voice modem is unavailable")
			}
			return lease, ctx.Err()
		},
		verify: func(ctx context.Context, _ io.Closer, _ string, _ bool) error { return ctx.Err() },
	}
	if err := c.Start(context.Background(), fixtureProfile(), "hardware"); err == nil {
		t.Fatal("explicit initial Start silently chose degraded operation")
	}
	c.mu.Lock()
	wanted, pending, owners := c.siteWanted, c.siteRecovery, len(c.children)
	c.mu.Unlock()
	if !lease.closed.Load() || wanted || pending != nil || owners != 0 || c.Status().FNEPID != 0 {
		t.Fatal("failed initial Start retained desired operation or resources")
	}
}

func TestControlRecoveryContinuesAfterVoiceWarmupFails(t *testing.T) {
	c := testController(t, nil)
	waiting, retry := holdSiteRetries(c)
	c.opts.StartupTimeout = time.Second
	if err := c.Start(context.Background(), fixtureProfile(), "null"); err != nil {
		t.Fatal(err)
	}
	t.Setenv("RADIO_DELAY_CHANNEL", "voice-1")
	t.Setenv("RADIO_STARTUP_DELAY", "30s")
	_ = syscall.Kill(controlOwner(c).cmd.Process.Pid, syscall.SIGKILL)
	nextSiteRetry(t, waiting, time.Second)
	retry <- struct{}{}
	waitRadio(t, "failed voice warmup blocked healthy control recovery", func() bool { return c.Status().State == "running" })
	before := c.Status()
	if voice(c).State != "fault" || voice(c).PID != 0 || voice(c).Error == "" {
		t.Fatal("failed voice warmup did not retain truthful fault state")
	}
	c.mu.Lock()
	wanted, owner := c.recovery["voice-1"].wanted, c.children["voice-1"]
	c.mu.Unlock()
	if !wanted || owner != nil {
		t.Fatal("voice warmup lost desired intent or retained an unretired worker")
	}
	t.Setenv("RADIO_DELAY_CHANNEL", "")
	forceVoiceRetry(c)
	waitRadio(t, "wanted voice did not recover after warmup failure", func() bool { return voice(c).Ready })
	if c.Status().FNEPID != before.FNEPID || c.Status().Channels[0].PID != before.Channels[0].PID {
		t.Fatal("voice warmup retry replaced healthy control/media")
	}
}

func TestControlRecoveryRetainsFailedLeaseOwnerBeforeReplacement(t *testing.T) {
	c := testController(t, nil)
	waiting, retry := holdSiteRetries(c)
	if err := c.Start(context.Background(), fixtureProfile(), "null"); err != nil {
		t.Fatal(err)
	}
	old := controlOwner(c)
	lease := &recoveryLease{}
	lease.fail.Store(true)
	t.Cleanup(func() { lease.fail.Store(false) })
	old.mu.Lock()
	old.lease, old.sameDevice = lease, func(io.Closer, string) bool { return true }
	old.mu.Unlock()
	_ = syscall.Kill(old.cmd.Process.Pid, syscall.SIGKILL)
	nextSiteRetry(t, waiting, time.Second)
	if controlOwner(c) != old || lease.closed.Load() {
		t.Fatal("failed lease release forgot its owner")
	}
	retry <- struct{}{}
	nextSiteRetry(t, waiting, 2*time.Second)
	if controlOwner(c) != old || lease.calls.Load() < 2 || c.Status().State != "fault" || old.alive() {
		t.Fatal("new process bypassed failed retirement")
	}
	lease.fail.Store(false)
	retry <- struct{}{}
	waitRadio(t, "safe lease retirement did not allow recovery", func() bool { return c.Status().State == "running" })
	if !lease.closed.Load() || controlOwner(c) == old {
		t.Fatal("recovery replaced a modem before retiring its ownership")
	}
}

func TestControlRecoveryCancellationAndFreshStart(t *testing.T) {
	for _, phase := range []string{"retry-wait", "warmup"} {
		for _, action := range []string{"stop", "close", "configuration"} {
			t.Run(phase+"/"+action, func(t *testing.T) {
				c := testController(t, nil)
				waiting, retry := holdSiteRetries(c)
				c.opts.StartupTimeout = 45 * time.Second
				p := fixtureProfile()
				if err := c.Start(context.Background(), p, "null"); err != nil {
					t.Fatal(err)
				}
				t.Setenv("RADIO_DELAY_CHANNEL", "control-1")
				t.Setenv("RADIO_STARTUP_DELAY", "30s")
				_ = syscall.Kill(controlOwner(c).cmd.Process.Pid, syscall.SIGKILL)
				nextSiteRetry(t, waiting, time.Second)
				c.mu.Lock()
				attempt := c.siteRecovery
				c.mu.Unlock()
				if phase == "warmup" {
					retry <- struct{}{}
					waitRadio(t, "control did not enter recovery warmup", func() bool {
						s := c.Status()
						return s.State == "starting" && s.Channels[0].PID != 0
					})
				}
				if err := c.StopChannel(context.Background(), "voice-1"); err == nil {
					t.Fatal("voice Stop falsely reported accepted during whole-Site recovery")
				}
				c.mu.Lock()
				voiceWanted := c.siteRecovery == attempt && attempt.voiceIntent["voice-1"]
				c.mu.Unlock()
				if !voiceWanted {
					t.Fatal("rejected voice Stop changed desired recovery intent")
				}
				ctx, cancel := context.WithTimeout(context.Background(), time.Second)
				defer cancel()
				begin := time.Now()
				var err error
				switch action {
				case "stop":
					err = c.Stop(ctx)
				case "close":
					err = c.Close()
				case "configuration":
					p.Site.Channels[1].PowerPercent = 60
					err = c.ApplyPolicy(ctx, p)
				}
				if err != nil {
					t.Fatal(err)
				}
				if time.Since(begin) > 1500*time.Millisecond {
					t.Fatal("cancellation waited for native startup timeout")
				}
				select {
				case <-attempt.done:
				case <-time.After(time.Second):
					t.Fatal("cancelled recovery worker did not exit")
				}
				c.mu.Lock()
				wanted, pending := c.siteWanted, c.siteRecovery
				owners := len(c.children)
				c.mu.Unlock()
				if wanted || pending != nil || owners != 0 || c.Status().FNEPID != 0 || c.Status().State == "running" {
					t.Fatal("cancelled recovery retained desired RF or native resources")
				}
				if action != "configuration" && (c.Status().State != "stopped" || c.Status().Error != "") {
					t.Fatal("successful explicit Stop/Close retained a recovery-waiting fault")
				}
				if action != "configuration" && c.Status().Channels[0].Error != "" {
					t.Fatal("successful explicit Stop/Close retained a per-channel recovery-waiting error")
				}
				if action != "close" {
					t.Setenv("RADIO_DELAY_CHANNEL", "")
					if err := c.Start(context.Background(), p, "null"); err != nil {
						t.Fatal("fresh explicit Start was blocked", err)
					}
					if c.Status().State != "running" || c.Status().Channels[1].PowerPercent != p.Site.Channels[1].PowerPercent {
						t.Fatal("new Start did not use fresh settings")
					}
				}
			})
		}
	}
}

func TestConfigurationCancelsControlRecoveryBeforeInitialRetirement(t *testing.T) {
	c := testController(t, nil)
	holdSiteRetries(c)
	if err := c.Start(context.Background(), fixtureProfile(), "null"); err != nil {
		t.Fatal(err)
	}
	before := c.Status()
	c.opMu.Lock()
	c.controlFailed(controlOwner(c))
	p := fixtureProfile()
	p.System.Radios[0].Enabled = false
	applied := make(chan error, 1)
	go func() { applied <- c.ApplyPolicy(context.Background(), p) }()
	waitRadio(t, "configuration failed to cancel before opMu", func() bool {
		c.mu.Lock()
		defer c.mu.Unlock()
		return c.siteRecovery == nil
	})
	c.opMu.Unlock()
	if err := <-applied; err != nil {
		t.Fatal(err)
	}
	for _, pid := range []int{before.FNEPID, before.Channels[0].PID, before.Channels[1].PID} {
		if syscall.Kill(pid, 0) == nil {
			t.Fatal("cancelled worker abandoned a live old-run resource")
		}
	}
	if !strings.Contains(c.Status().Error, "settings changed") {
		t.Fatal("configuration cancellation was not visible")
	}
}

func TestExplicitStartSupersedesControlRecoveryWarmup(t *testing.T) {
	c := testController(t, nil)
	waiting, retry := holdSiteRetries(c)
	c.opts.StartupTimeout = 45 * time.Second
	p := fixtureProfile()
	if err := c.Start(context.Background(), p, "null"); err != nil {
		t.Fatal(err)
	}
	t.Setenv("RADIO_DELAY_CHANNEL", "control-1")
	t.Setenv("RADIO_STARTUP_DELAY", "30s")
	_ = syscall.Kill(controlOwner(c).cmd.Process.Pid, syscall.SIGKILL)
	nextSiteRetry(t, waiting, time.Second)
	c.mu.Lock()
	attempt := c.siteRecovery
	c.mu.Unlock()
	retry <- struct{}{}
	waitRadio(t, "control recovery did not enter warmup", func() bool {
		return c.Status().State == "starting" && c.Status().Channels[0].PID != 0
	})
	recovering := c.Status()
	t.Setenv("RADIO_DELAY_CHANNEL", "")
	p.Site.Channels[1].PowerPercent = 65
	if err := c.Start(context.Background(), p, "null"); err != nil {
		t.Fatal("explicit Start could not replace pending recovery", err)
	}
	select {
	case <-attempt.done:
	case <-time.After(time.Second):
		t.Fatal("superseded worker did not exit")
	}
	after := c.Status()
	if after.State != "running" || after.Channels[1].PowerPercent != 65 || after.FNEPID == recovering.FNEPID || after.Channels[0].PID == recovering.Channels[0].PID {
		t.Fatal("explicit Start reused stale recovery settings or lifetime")
	}
	for _, row := range recovering.Channels {
		if row.PID != 0 && syscall.Kill(row.PID, 0) == nil {
			t.Fatal("superseded warmup retained a live native process")
		}
	}
}

func TestDelayedRunStopCannotWithdrawReplacement(t *testing.T) {
	c := testController(t, nil)
	if err := c.Start(context.Background(), fixtureProfile(), "null"); err != nil {
		t.Fatal(err)
	}
	old := c.Status().StartedAt
	if err := c.Stop(context.Background()); err != nil {
		t.Fatal(err)
	}
	if err := c.Start(context.Background(), fixtureProfile(), "null"); err != nil {
		t.Fatal(err)
	}
	before := c.Status()
	if err := c.StopRun(context.Background(), old); err != nil {
		t.Fatal(err)
	}
	after := c.Status()
	if after.State != "running" || after.FNEPID != before.FNEPID || after.Channels[0].PID != before.Channels[0].PID {
		t.Fatal("old caller stopped a replacement native lifetime")
	}
	if err := c.StopRun(context.Background(), before.StartedAt); err != nil {
		t.Fatal(err)
	}
	if c.Status().State != "stopped" || c.Status().Error != "" {
		t.Fatal("current-run conditional Stop did not retire its owner")
	}
}
