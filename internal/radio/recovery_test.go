//go:build linux || darwin

package radio

import (
	"context"
	"errors"
	"io"
	"sync"
	"sync/atomic"
	"syscall"
	"testing"
	"time"
)

type recoveryLease struct {
	closed atomic.Bool
	fail   atomic.Bool
	calls  atomic.Int32
}

func (l *recoveryLease) Close() error {
	l.calls.Add(1)
	if l.fail.Load() {
		return errors.New("offline lease cleanup failed")
	}
	l.closed.Store(true)
	return nil
}
func waitRadio(t *testing.T, what string, condition func() bool) {
	t.Helper()
	deadline := time.Now().Add(4 * time.Second)
	for time.Now().Before(deadline) {
		if condition() {
			return
		}
		time.Sleep(5 * time.Millisecond)
	}
	t.Fatal(what)
}
func forceVoiceRetry(c *Controller) {
	c.mu.Lock()
	next := c.recovery["voice-1"].next
	c.mu.Unlock()
	c.recoverVoiceChannels(next.Add(time.Millisecond))
}
func voice(c *Controller) ChannelStatus {
	for _, row := range c.Status().Channels {
		if row.ID == "voice-1" {
			return row
		}
	}
	return ChannelStatus{}
}

func TestVoiceRecoveryWaitsForSameModemBacksOffAndHonorsExplicitStop(t *testing.T) {
	c := testController(t, nil)
	var available atomic.Bool
	var attempts atomic.Int32
	var mu sync.Mutex
	var paths []string
	c.modem = modemOperations{
		acquire: func(ctx context.Context, path string) (io.Closer, error) {
			attempts.Add(1)
			mu.Lock()
			paths = append(paths, path)
			mu.Unlock()
			if !available.Load() {
				return nil, errors.New("selected modem is unavailable")
			}
			return &recoveryLease{}, ctx.Err()
		},
		verify: func(ctx context.Context, _ io.Closer, _ string, _ bool) error { return ctx.Err() },
		same:   func(lease io.Closer, _ string) bool { return available.Load() && !lease.(*recoveryLease).closed.Load() },
	}
	if err := c.Start(context.Background(), fixtureProfile(), "null"); err != nil {
		t.Fatal(err)
	}
	before := c.Status()
	// Existing native helpers stay on unopened/null RF. Only lease acquisition
	// for subsequent replacements uses the offline missing/reappearing modem.
	c.mu.Lock()
	c.status.Mode = "hardware"
	c.mu.Unlock()
	_ = syscall.Kill(voice(c).PID, syscall.SIGKILL)
	waitRadio(t, "failed voice did not become faulted", func() bool { return voice(c).State == "fault" })
	for failure := uint(1); failure <= 6; failure++ {
		c.mu.Lock()
		next := c.recovery["voice-1"].next
		c.mu.Unlock()
		count := attempts.Load()
		c.recoverVoiceChannels(next.Add(-time.Nanosecond))
		time.Sleep(5 * time.Millisecond)
		if attempts.Load() != count {
			t.Fatal("retry ran before its backoff")
		}
		forceVoiceRetry(c)
		waitRadio(t, "missing-modem retry did not finish", func() bool {
			c.mu.Lock()
			defer c.mu.Unlock()
			return attempts.Load() > count && c.recovery["voice-1"].attempt == nil
		})
		c.mu.Lock()
		remaining := time.Until(c.recovery["voice-1"].next)
		c.mu.Unlock()
		want := recoveryDelay(failure)
		if remaining < want-200*time.Millisecond || remaining > want {
			t.Fatalf("retry %d backoff=%v want %v", failure, remaining, want)
		}
	}
	available.Store(true)
	forceVoiceRetry(c)
	waitRadio(t, "reappearing modem did not recover", func() bool { return voice(c).Ready && voice(c).PID != before.Channels[1].PID })
	after := c.Status()
	if after.FNEPID != before.FNEPID || after.Channels[0].PID != before.Channels[0].PID || after.State != "running" {
		t.Fatal("voice recovery replaced unrelated engines")
	}
	mu.Lock()
	for _, path := range paths {
		if path != "/dev/serial/by-id/voice" {
			t.Errorf("recovery changed stable role: %s", path)
		}
	}
	mu.Unlock()
	if err := c.StopChannel(context.Background(), "voice-1"); err != nil {
		t.Fatal(err)
	}
	count := attempts.Load()
	c.recoverVoiceChannels(time.Now().Add(time.Hour))
	time.Sleep(30 * time.Millisecond)
	if attempts.Load() != count || voice(c).State != "stopped" || voice(c).PID != 0 {
		t.Fatal("explicitly stopped voice was restarted")
	}
}

func TestStopChannelCancelsConcurrentManualStartup(t *testing.T) {
	c := testController(t, nil)
	if err := c.Start(context.Background(), fixtureProfile(), "null"); err != nil {
		t.Fatal(err)
	}
	before := c.Status()
	if err := c.StopChannel(context.Background(), "voice-1"); err != nil {
		t.Fatal(err)
	}
	t.Setenv("RADIO_DELAY_CHANNEL", "voice-1")
	t.Setenv("RADIO_STARTUP_DELAY", "30s")
	c.opts.StartupTimeout = 45 * time.Second
	started := make(chan error, 1)
	go func() { started <- c.StartChannel(context.Background(), "voice-1") }()
	waitRadio(t, "replacement never entered startup", func() bool { return voice(c).State == "starting" && voice(c).PID != 0 })
	if err := c.StartChannel(context.Background(), "voice-1"); err == nil {
		t.Fatal("concurrent manual start was duplicated")
	}
	begin := time.Now()
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	if err := c.StopChannel(ctx, "voice-1"); err != nil {
		t.Fatal(err)
	}
	if time.Since(begin) > 1500*time.Millisecond {
		t.Fatal("channel stop waited for startup timeout")
	}
	select {
	case err := <-started:
		if err == nil {
			t.Fatal("cancelled startup reported success")
		}
	case <-time.After(time.Second):
		t.Fatal("startup was not cancelled")
	}
	if status := c.Status(); status.State != "running" || voice(c).State != "stopped" || status.FNEPID != before.FNEPID || status.Channels[0].PID != before.Channels[0].PID {
		t.Fatal("voice startup cancellation changed other engines")
	}
	c.recoverVoiceChannels(time.Now().Add(time.Hour))
	if voice(c).State != "stopped" {
		t.Fatal("cancelled manual start revived stopped channel")
	}
}

func TestSiteStopAndCloseInterruptStartupBeforeOperationMutex(t *testing.T) {
	for _, closeService := range []bool{false, true} {
		t.Run(map[bool]string{false: "stop", true: "close"}[closeService], func(t *testing.T) {
			c := testController(t, nil)
			if err := c.Start(context.Background(), fixtureProfile(), "null"); err != nil {
				t.Fatal(err)
			}
			if err := c.StopChannel(context.Background(), "voice-1"); err != nil {
				t.Fatal(err)
			}
			t.Setenv("RADIO_DELAY_CHANNEL", "voice-1")
			t.Setenv("RADIO_STARTUP_DELAY", "30s")
			c.opts.StartupTimeout = 45 * time.Second
			started := make(chan error, 1)
			go func() { started <- c.StartChannel(context.Background(), "voice-1") }()
			waitRadio(t, "startup not reached", func() bool { return voice(c).State == "starting" })
			begin := time.Now()
			var err error
			if closeService {
				err = c.Close()
			} else {
				ctx, cancel := context.WithTimeout(context.Background(), time.Second)
				err = c.Stop(ctx)
				cancel()
			}
			if err != nil {
				t.Fatal(err)
			}
			limit := 1500 * time.Millisecond
			if closeService {
				limit = 5 * time.Second // race-instrumented helpers retain their normal exit grace
			}
			if time.Since(begin) > limit {
				t.Fatal("shutdown waited for startup timeout")
			}
			if err := <-started; err == nil {
				t.Fatal("cancelled channel startup succeeded")
			}
			if c.Status().FNEPID != 0 || voice(c).PID != 0 {
				t.Fatal("shutdown forgot native owner")
			}
		})
	}
}

func TestFailedReplacementCleanupRetainsLeaseAndControllerOwnership(t *testing.T) {
	c := testController(t, nil)
	lease := &recoveryLease{}
	lease.fail.Store(true)
	t.Cleanup(func() { lease.fail.Store(false) })
	c.modem = modemOperations{
		acquire: func(context.Context, string) (io.Closer, error) { return lease, nil },
		verify:  func(context.Context, io.Closer, string, bool) error { return nil },
		same:    func(io.Closer, string) bool { return true },
	}
	if err := c.Start(context.Background(), fixtureProfile(), "null"); err != nil {
		t.Fatal(err)
	}
	if err := c.StopChannel(context.Background(), "voice-1"); err != nil {
		t.Fatal(err)
	}
	c.mu.Lock()
	c.status.Mode = "hardware"
	c.mu.Unlock()
	t.Setenv("RADIO_FAIL_CHANNEL", "voice-1")
	if err := c.StartChannel(context.Background(), "voice-1"); err == nil {
		t.Fatal("failed replacement reported success")
	}
	c.mu.Lock()
	owner := c.children["voice-1"]
	c.mu.Unlock()
	if owner == nil || owner.lease != lease || lease.closed.Load() || voice(c).PID == 0 {
		t.Fatal("failed cleanup dropped process or lease ownership")
	}
	if err := c.Close(); err == nil {
		t.Fatal("close hid retained modem ownership")
	}
	if _, err := New(c.opts); err == nil {
		t.Fatal("failed close released controller owner lock")
	}
	c.mu.Lock()
	if c.children["voice-1"] != owner {
		t.Error("failed close forgot replacement")
	}
	c.mu.Unlock()
	lease.fail.Store(false)
	if err := c.Close(); err != nil {
		t.Fatal(err)
	}
	if !lease.closed.Load() || voice(c).PID != 0 {
		t.Fatal("later cleanup did not retire retained lease")
	}
}

func TestQueuedRecoveryFromOldRunCannotLaunchIntoReplacement(t *testing.T) {
	c := testController(t, nil)
	if err := c.Start(context.Background(), fixtureProfile(), "null"); err != nil {
		t.Fatal(err)
	}
	_ = syscall.Kill(voice(c).PID, syscall.SIGKILL)
	waitRadio(t, "voice never faulted", func() bool { return voice(c).State == "fault" })
	c.opMu.Lock()
	forceVoiceRetry(c) // worker waits for opMu while owning only its attempt token
	c.mu.Lock()
	old := c.recovery["voice-1"].attempt
	c.mu.Unlock()
	c.requestRunStop(false)
	if err := c.stop(context.Background(), false); err != nil {
		c.opMu.Unlock()
		t.Fatal(err)
	}
	c.finishRunStop()
	if err := c.start(context.Background(), fixtureProfile(), "null"); err != nil {
		c.opMu.Unlock()
		t.Fatal(err)
	}
	before := c.Status()
	c.opMu.Unlock()
	waitRadio(t, "old recovery context remained live", func() bool { return old.ctx.Err() != nil })
	time.Sleep(30 * time.Millisecond)
	after := c.Status()
	if after.State != "running" || after.Channels[0].PID != before.Channels[0].PID || voice(c).PID != before.Channels[1].PID || after.FNEPID != before.FNEPID {
		t.Fatal("old recovery changed replacement run")
	}
}

func TestControlFailureCancelsVoiceWarmupAndWithdrawsOldRun(t *testing.T) {
	c := testController(t, nil)
	waiting, _ := holdSiteRetries(c)
	if err := c.Start(context.Background(), fixtureProfile(), "null"); err != nil {
		t.Fatal(err)
	}
	before := c.Status()
	if err := c.StopChannel(context.Background(), "voice-1"); err != nil {
		t.Fatal(err)
	}
	t.Setenv("RADIO_DELAY_CHANNEL", "voice-1")
	t.Setenv("RADIO_STARTUP_DELAY", "30s")
	c.opts.StartupTimeout = 45 * time.Second
	started := make(chan error, 1)
	go func() { started <- c.StartChannel(context.Background(), "voice-1") }()
	waitRadio(t, "voice never entered warmup", func() bool { return voice(c).State == "starting" })
	_ = syscall.Kill(before.Channels[0].PID, syscall.SIGKILL)
	waitRadio(t, "control loss did not safely retire Site", func() bool { return c.Status().State == "fault" && c.Status().FNEPID == 0 && voice(c).PID == 0 })
	if err := <-started; err == nil {
		t.Fatal("voice startup survived control-channel failure")
	}
	<-waiting
	c.recoverVoiceChannels(time.Now().Add(time.Hour))
	if c.Status().State != "fault" || voice(c).PID != 0 {
		t.Fatal("voice recovery escaped the failed control run")
	}
}

func TestPolicyRefreshPreservesIntentionallyStoppedVoice(t *testing.T) {
	c := testController(t, nil)
	p := fixtureProfile()
	if err := c.Start(context.Background(), p, "null"); err != nil {
		t.Fatal(err)
	}
	if err := c.StopChannel(context.Background(), "voice-1"); err != nil {
		t.Fatal(err)
	}
	p.System.Radios[0].Alias = "policy change"
	p.System.Radios[0].Enabled = false
	if err := c.ApplyPolicy(context.Background(), p); err != nil {
		t.Fatal(err)
	}
	if c.Status().State != "running" || voice(c).State != "stopped" || voice(c).PID != 0 {
		t.Fatal("internal policy restart resumed deliberately stopped voice")
	}
}

func TestStartCannotPassAQueuedSiteStop(t *testing.T) {
	c := testController(t, nil)
	if err := c.Start(context.Background(), fixtureProfile(), "null"); err != nil {
		t.Fatal(err)
	}
	c.opMu.Lock()
	stopped := make(chan error, 1)
	go func() { stopped <- c.Stop(context.Background()) }()
	waitRadio(t, "stop intent not registered", func() bool { c.mu.Lock(); defer c.mu.Unlock(); return c.stopRequests != 0 })
	begin := time.Now()
	if err := c.Start(context.Background(), fixtureProfile(), "null"); err == nil {
		c.opMu.Unlock()
		t.Fatal("new Start passed pending Stop")
	}
	if time.Since(begin) > 500*time.Millisecond {
		c.opMu.Unlock()
		t.Fatal("new Start queued behind pending Stop")
	}
	c.opMu.Unlock()
	if err := <-stopped; err != nil {
		t.Fatal(err)
	}
	if c.Status().FNEPID != 0 || voice(c).PID != 0 {
		t.Fatal("stopped run retained native process")
	}
	if err := c.Start(context.Background(), fixtureProfile(), "null"); err != nil {
		t.Fatal("explicit later Start was blocked", err)
	}
}

func TestSiteStopInterruptsInitialSiteWarmup(t *testing.T) {
	c := testController(t, nil)
	t.Setenv("RADIO_DELAY_CHANNEL", "control-1")
	t.Setenv("RADIO_STARTUP_DELAY", "30s")
	c.opts.StartupTimeout = 45 * time.Second
	started := make(chan error, 1)
	go func() { started <- c.Start(context.Background(), fixtureProfile(), "null") }()
	waitRadio(t, "control never entered initial warmup", func() bool {
		status := c.Status()
		return status.State == "starting" && len(status.Channels) > 0 && status.Channels[0].PID != 0
	})
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	begin := time.Now()
	if err := c.Stop(ctx); err != nil {
		t.Fatal(err)
	}
	if time.Since(begin) > 1500*time.Millisecond {
		t.Fatal("Stop waited for initial startup timeout")
	}
	if err := <-started; err == nil {
		t.Fatal("cancelled initial startup reported success")
	}
	if c.Status().FNEPID != 0 || voice(c).PID != 0 || c.Status().State == "running" {
		t.Fatal("initial startup survived Stop")
	}
}

func TestPrelaunchLeaseCleanupFailureAlsoRetainsAnOwner(t *testing.T) {
	c := testController(t, nil)
	lease := &recoveryLease{}
	lease.fail.Store(true)
	t.Cleanup(func() { lease.fail.Store(false) })
	if err := c.releaseStartupLeases(map[string]io.Closer{"voice-1": lease}, fixtureProfile()); err == nil {
		t.Fatal("lease cleanup failure was hidden")
	}
	c.mu.Lock()
	owner := c.children["voice-1"]
	c.mu.Unlock()
	if owner == nil || owner.lease != lease || !owner.groupRetired || owner.alive() {
		t.Fatal("pre-launch lease lost its cleanup owner")
	}
	if err := c.Start(context.Background(), fixtureProfile(), "null"); err == nil {
		t.Fatal("new Site bypassed retained ownership")
	}
	if c.Status().FNEPID != 0 {
		t.Fatal("new media process launched despite retained lease")
	}
	lease.fail.Store(false)
	if err := c.Close(); err != nil {
		t.Fatal(err)
	}
}
