//go:build linux || darwin

package radio

import (
	"context"
	"errors"
	"syscall"
	"testing"
	"time"
)

func mediaOwner(c *Controller) *child {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.fne
}

func beginDelayedMediaReplacement(t *testing.T, c *Controller) (*child, Status) {
	t.Helper()
	before := c.Status()
	t.Setenv("RADIO_DELAY_CHANNEL", "media")
	t.Setenv("RADIO_STARTUP_DELAY", "30s")
	if err := syscall.Kill(before.FNEPID, syscall.SIGKILL); err != nil {
		t.Fatal(err)
	}
	waitRadio(t, "media replacement was not owned during readiness", func() bool {
		status := c.Status()
		return status.FNEPID != 0 && status.FNEPID != before.FNEPID && !status.FNEReady
	})
	owner := mediaOwner(c)
	if owner == nil || owner.cmd.Process.Pid != c.Status().FNEPID || owner.readyNow() {
		t.Fatal("replacement was not tracked before readiness")
	}
	return owner, before
}

func TestMediaRecoveryPreservesHealthyRFEngines(t *testing.T) {
	c := testController(t, nil)
	if err := c.Start(context.Background(), fixtureProfile(), "null"); err != nil {
		t.Fatal(err)
	}
	before := c.Status()
	if err := syscall.Kill(before.FNEPID, syscall.SIGKILL); err != nil {
		t.Fatal(err)
	}
	waitRadio(t, "media master did not recover", func() bool {
		status := c.Status()
		return status.FNEReady && status.FNEPID != before.FNEPID
	})
	after := c.Status()
	if after.State != "running" || after.Channels[0].PID != before.Channels[0].PID || voice(c).PID != before.Channels[1].PID {
		t.Fatal("media recovery replaced an RF engine")
	}
}

func TestMediaReplacementCleanupFailureRetainsOwnerAndRetries(t *testing.T) {
	c := testController(t, nil)
	if err := c.Start(context.Background(), fixtureProfile(), "null"); err != nil {
		t.Fatal(err)
	}
	owner, before := beginDelayedMediaReplacement(t, c)
	// FNE normally owns no modem lease. This private failing Close exercises
	// the same stop-error boundary as an unretired process group, without
	// depending on OS permissions or leaving an unkillable test process.
	lease := &recoveryLease{}
	lease.fail.Store(true)
	t.Cleanup(func() { lease.fail.Store(false) })
	owner.mu.Lock()
	owner.lease = lease
	owner.mu.Unlock()
	if err := syscall.Kill(owner.cmd.Process.Pid, syscall.SIGKILL); err != nil {
		t.Fatal(err)
	}
	waitRadio(t, "replacement cleanup was not attempted", func() bool { return lease.calls.Load() >= 2 })
	if mediaOwner(c) != owner || c.Status().FNEPID != owner.cmd.Process.Pid || c.Status().FNEReady || lease.closed.Load() {
		t.Fatal("failed cleanup dropped its owned replacement")
	}
	// Repeated cleanup failure must not launch another process on its ports.
	time.Sleep(600 * time.Millisecond)
	if mediaOwner(c) != owner {
		t.Fatal("a new media worker bypassed failed cleanup")
	}
	t.Setenv("RADIO_DELAY_CHANNEL", "")
	lease.fail.Store(false)
	waitRadio(t, "retained media owner was not retired and retried", func() bool {
		return c.Status().FNEReady && mediaOwner(c) != owner
	})
	if !lease.closed.Load() || c.Status().Channels[0].PID != before.Channels[0].PID || voice(c).PID != before.Channels[1].PID {
		t.Fatal("media cleanup retry changed RF ownership")
	}
}

func TestMediaReplacementStopAndCloseCancelAndJoinReadiness(t *testing.T) {
	for _, closeService := range []bool{false, true} {
		t.Run(map[bool]string{false: "stop", true: "close"}[closeService], func(t *testing.T) {
			c := testController(t, nil)
			if err := c.Start(context.Background(), fixtureProfile(), "null"); err != nil {
				t.Fatal(err)
			}
			owner, _ := beginDelayedMediaReplacement(t, c)
			c.mu.Lock()
			done := c.monitorDone
			c.mu.Unlock()
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
				limit = 4 * time.Second // normal grace for race-instrumented RF helpers
			}
			if time.Since(begin) >= limit {
				t.Fatal("shutdown waited for the media readiness timeout")
			}
			select {
			case <-done:
			default:
				t.Fatal("media replacement attempt was not joined")
			}
			if owner.alive() || c.Status().FNEPID != 0 || mediaOwner(c) != nil {
				t.Fatal("stopped media replacement retained a live or forgotten owner")
			}
		})
	}
}

func TestMediaFailedCleanupPreventsCloseFromDroppingOwnership(t *testing.T) {
	c := testController(t, nil)
	if err := c.Start(context.Background(), fixtureProfile(), "null"); err != nil {
		t.Fatal(err)
	}
	owner, _ := beginDelayedMediaReplacement(t, c)
	lease := &recoveryLease{}
	lease.fail.Store(true)
	t.Cleanup(func() { lease.fail.Store(false) })
	owner.mu.Lock()
	owner.lease = lease
	owner.mu.Unlock()
	if err := c.Close(); err == nil {
		t.Fatal("close hid the failed replacement retirement")
	}
	if mediaOwner(c) != owner || c.lock == nil || c.Status().FNEPID != owner.cmd.Process.Pid || lease.closed.Load() {
		t.Fatal("failed close dropped replacement or controller ownership")
	}
	if duplicate, err := New(c.opts); err == nil {
		_ = duplicate.Close()
		t.Fatal("failed media retirement released the controller lock")
	}
	lease.fail.Store(false)
	if err := c.Close(); err != nil {
		t.Fatal(err)
	}
	if !lease.closed.Load() || mediaOwner(c) != nil || c.Status().FNEPID != 0 {
		t.Fatal("later close did not retire retained replacement")
	}
}

func TestOldMediaAttemptCannotPublishIntoReplacementRun(t *testing.T) {
	c := testController(t, nil)
	if err := c.Start(context.Background(), fixtureProfile(), "null"); err != nil {
		t.Fatal(err)
	}
	old := mediaOwner(c)
	c.mu.Lock()
	oldRun, oldContext := c.runID, c.runContext
	c.mu.Unlock()
	if err := c.Stop(context.Background()); err != nil {
		t.Fatal(err)
	}
	if err := c.Start(context.Background(), fixtureProfile(), "null"); err != nil {
		t.Fatal(err)
	}
	before := c.Status()
	sequence := c.sequence.Load()
	c.restartFNE(oldContext, old)
	c.fneReplacementFailed(old, oldRun, errors.New("late old media cleanup"), true)
	c.observe(old, "I: (HOST) P25 Affiliation, group affiliation, srcId = 10001, dstId = 1001")
	after := c.Status()
	if after.FNEPID != before.FNEPID || !after.FNEReady || after.Error != before.Error || len(c.Affiliations()) != 0 || c.sequence.Load() != sequence {
		t.Fatal("stale media attempt changed a replacement run")
	}
}
