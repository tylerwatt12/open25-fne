package radio

import (
	"context"
	"errors"
	"io"
	"path/filepath"
	"time"
)

// Desired operation belongs to a Site run, independently of intentional
// retirement of any particular process. Nothing here persists radio activity.
type channelRecovery struct {
	wanted   bool
	failures uint
	next     time.Time
	attempt  *channelAttempt
}
type channelAttempt struct {
	run    string
	ctx    context.Context
	cancel context.CancelFunc
}

// A control outage retires the entire old run. This token retains only desired
// configuration and idle voice intent; calls, grants and queued commands never
// cross that boundary. It is not tied to the failed run's cancelled context.
type siteAttempt struct {
	ctx         context.Context
	cancel      context.CancelFunc
	done        chan struct{}
	epoch       uint64
	profile     Profile
	mode        string
	voiceIntent map[string]bool
	controlID   string
}

type modemOperations struct {
	acquire func(context.Context, string) (io.Closer, error)
	verify  func(context.Context, io.Closer, string, bool) error
	same    func(io.Closer, string) bool
	resolve func(string) (string, error)
}

func (c *Controller) resolveModem(path string) (string, error) {
	if c.modem.resolve != nil {
		return c.modem.resolve(path)
	}
	return filepath.EvalSymlinks(path)
}

func (c *Controller) acquireModem(ctx context.Context, path string) (io.Closer, error) {
	if c.modem.acquire != nil {
		return c.modem.acquire(ctx, path)
	}
	return acquireDevice(ctx, path)
}
func (c *Controller) verifyModem(ctx context.Context, lease io.Closer, path string, identification bool) error {
	if c.modem.verify != nil {
		return c.modem.verify(ctx, lease, path, identification)
	}
	return verifyFirmware(ctx, lease, path, identification)
}

// Public Stop/Close signal before opMu: a slow modem/startup cannot prevent the
// shutdown request from cancelling the operation currently holding that mutex.
func (c *Controller) requestRunStop(closing bool) {
	c.mu.Lock()
	c.requestRunStopLocked(closing)
	c.mu.Unlock()
}

func (c *Controller) requestRunStopLocked(closing bool) {
	c.stopEpoch++
	c.stopRequests++
	if closing {
		c.closing = true
	}
	if c.controlSwitch != nil {
		c.controlSwitch.cancel()
		c.controlSwitch = nil
		c.status.ControlSwitchTarget, c.status.ControlSwitchReason = "", ""
	}
	c.withdrawSiteLocked()
	c.cancelRunStartsLocked()
}

func (c *Controller) withdrawSiteLocked() {
	c.siteWanted = false
	if c.siteRecovery != nil {
		c.siteRecovery.cancel()
		c.siteRecovery = nil
	}
}

func (c *Controller) currentSiteAttemptLocked(attempt *siteAttempt) bool {
	return c.siteRecovery == attempt && c.siteWanted && !c.closing &&
		c.stopRequests == 0 && c.stopEpoch == attempt.epoch && attempt.ctx.Err() == nil
}

func (c *Controller) waitSiteRetry(ctx context.Context, delay time.Duration) error {
	if c.retryWait != nil {
		return c.retryWait(ctx, delay)
	}
	timer := time.NewTimer(delay)
	defer timer.Stop()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-timer.C:
		return nil
	}
}

// The monitor only schedules this worker. It must be able to exit immediately
// when stop() joins it; all replacement/lease ownership stays under opMu.
func (c *Controller) recoverSite(attempt *siteAttempt) {
	defer close(attempt.done)
	defer attempt.cancel()
	c.opMu.Lock()
	c.mu.Lock()
	current := c.currentSiteAttemptLocked(attempt)
	c.mu.Unlock()
	if current {
		ctx, cancel := context.WithTimeout(attempt.ctx, 5*time.Second)
		err := c.stop(ctx, false)
		cancel()
		c.mu.Lock()
		c.markSiteRetryLocked(attempt, err)
		c.mu.Unlock()
	}
	c.opMu.Unlock()
	if !current {
		return
	}
	for failures := uint(0); ; {
		if c.waitSiteRetry(attempt.ctx, recoveryDelay(failures)) != nil {
			return
		}
		c.opMu.Lock()
		c.mu.Lock()
		current = c.currentSiteAttemptLocked(attempt)
		c.mu.Unlock()
		if !current {
			c.opMu.Unlock()
			return
		}
		err := c.startSite(attempt.ctx, attempt.profile, attempt.mode, attempt.voiceIntent, attempt.epoch, attempt)
		c.opMu.Unlock()
		if err == nil {
			return
		}
		c.mu.Lock()
		current = c.currentSiteAttemptLocked(attempt)
		if current {
			c.markSiteRetryLocked(attempt, err)
		}
		c.mu.Unlock()
		if !current {
			return
		}
		if failures < 5 {
			failures++
		}
	}
}

func (c *Controller) markSiteRetryLocked(attempt *siteAttempt, err error) {
	if !c.currentSiteAttemptLocked(attempt) {
		return
	}
	c.status.State = "fault"
	c.status.Error = "Control-channel recovery is waiting for the selected modems."
	if err != nil {
		c.status.Error += " " + err.Error()
	}
	for i := range c.status.Channels {
		row := &c.status.Channels[i]
		if row.Role == "control" {
			// Retired processes have no PID, but the resource remains faulted
			// while desired recovery is pending. The existing Stop Site button
			// then remains available without adding an interface state.
			row.State, row.Ready, row.Busy = "fault", false, false
			row.Error = c.status.Error
		}
	}
	c.emitLocked(Event{Kind: "site_recovery_waiting", Message: c.status.Error})
}

// A recovered control resource may run degraded when a wanted voice worker
// fails warmup, but only after that worker and its descriptor are retired.
func (c *Controller) retireUnavailableVoice(ctx context.Context, process *child, cause error, attempt *siteAttempt) error {
	stop, cancel := context.WithTimeout(ctx, 3*time.Second)
	cleanupErr := process.stop(stop, false)
	cancel()
	if cleanupErr != nil {
		return errors.Join(cause, cleanupErr)
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	if !c.currentSiteAttemptLocked(attempt) || c.children[process.id] != process {
		return context.Canceled
	}
	delete(c.children, process.id)
	for i := range c.status.Channels {
		row := &c.status.Channels[i]
		if row.ID == process.id {
			row.State, row.Ready, row.PID = "fault", false, 0
			row.Error = "Selected voice modem did not become ready: " + cause.Error()
		}
	}
	if voice := c.recovery[process.id]; voice != nil {
		voice.wanted, voice.failures = true, 1
		voice.next = time.Now().Add(recoveryDelay(0))
	}
	c.emitLocked(Event{Kind: "channel_fault", ChannelID: process.id, Message: "Voice startup failed; control recovery is continuing with the remaining resources."})
	return nil
}

func (c *Controller) controlFailed(process *child) {
	if c.dynamicControlFailed(process) {
		return
	}
	c.mu.Lock()
	if c.children[process.id] != process || (c.status.State != "running" && c.status.State != "starting") {
		c.mu.Unlock()
		return
	}
	for i := range c.status.Channels {
		row := &c.status.Channels[i]
		if row.ID == process.id {
			row.State, row.Ready, row.Busy = "fault", false, false
			row.RID, row.TGID = 0, 0
			row.Error = "Native radio engine stopped."
		}
	}
	wanted := c.siteWanted && c.status.State == "running" && c.stopRequests == 0 && !c.closing
	intent := make(map[string]bool, len(c.recovery))
	for id, voice := range c.recovery {
		intent[id] = voice.wanted
	}
	c.status.State, c.status.Error = "fault", "The control-channel engine stopped."
	c.emitLocked(Event{Kind: "channel_fault", ChannelID: process.id, Message: c.status.Error})
	c.cancelRunStartsLocked()
	if wanted {
		ctx, cancel := context.WithCancel(c.ctx)
		attempt := &siteAttempt{ctx: ctx, cancel: cancel, done: make(chan struct{}), epoch: c.stopEpoch,
			profile: cloneProfile(c.configuredProfile), mode: c.status.Mode, voiceIntent: intent, controlID: c.status.ActiveControlChannelID}
		c.siteRecovery = attempt
		c.mu.Unlock()
		go c.recoverSite(attempt)
		return
	}
	failedRun := c.runID
	c.mu.Unlock()
	go func() {
		c.opMu.Lock()
		defer c.opMu.Unlock()
		c.mu.Lock()
		current := c.runID == failedRun && c.children[process.id] == process && c.status.State == "fault"
		c.mu.Unlock()
		if current {
			ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
			defer cancel()
			_ = c.stop(ctx, false)
		}
	}()
}
func (c *Controller) finishRunStop() {
	c.mu.Lock()
	c.stopRequests--
	c.mu.Unlock()
}
func (c *Controller) cancelRunStarts() {
	c.mu.Lock()
	c.cancelRunStartsLocked()
	c.mu.Unlock()
}
func (c *Controller) cancelRunStartsLocked() {
	if c.c4fm != nil {
		c.c4fm.restore = false
		c.c4fm.cancel()
	}
	if c.runCancel != nil {
		c.runCancel()
	}
	for _, recovery := range c.recovery {
		recovery.wanted = false
		if recovery.attempt != nil {
			recovery.attempt.cancel()
		}
	}
}
func (c *Controller) voiceIntent() map[string]bool {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.recovery == nil {
		return nil
	}
	intent := make(map[string]bool, len(c.recovery))
	for id, recovery := range c.recovery {
		intent[id] = recovery.wanted
	}
	return intent
}

func recoveryDelay(failures uint) time.Duration {
	if failures >= 5 {
		return 30 * time.Second
	}
	return time.Second << failures
}

// monitor queues attempts without waiting for a modem or its DSP warmup.
// opMu still serializes every process/lease replacement and explicit operation.
func (c *Controller) recoverVoiceChannels(now time.Time) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.closing || c.status.State != "running" || c.runContext == nil || c.runContext.Err() != nil {
		return
	}
	for _, row := range c.status.Channels {
		recovery := c.recovery[row.ID]
		if (c.c4fm != nil && c.c4fm.channel.ID == row.ID) || row.Role != "voice" || row.State != "fault" || recovery == nil || !recovery.wanted || recovery.attempt != nil || now.Before(recovery.next) {
			continue
		}
		ctx, cancel := context.WithCancel(c.runContext)
		attempt := &channelAttempt{run: c.runID, ctx: ctx, cancel: cancel}
		recovery.attempt = attempt
		go c.recoverVoice(row.ID, attempt)
	}
}
func (c *Controller) recoverVoice(id string, attempt *channelAttempt) {
	c.opMu.Lock()
	err := c.startChannel(attempt.ctx, id, attempt)
	c.opMu.Unlock()
	c.finishChannelAttempt(id, attempt, err)
}
func (c *Controller) finishChannelAttempt(id string, attempt *channelAttempt, err error) {
	cancelled := attempt.ctx.Err() != nil
	attempt.cancel()
	c.mu.Lock()
	defer c.mu.Unlock()
	recovery := c.recovery[id]
	if recovery == nil || recovery.attempt != attempt || c.runID != attempt.run {
		return
	}
	recovery.attempt = nil
	if cancelled {
		recovery.wanted = false
	}
	if err == nil {
		recovery.failures = 0
		recovery.next = time.Time{}
	} else if recovery.wanted && c.status.State == "running" && c.runContext != nil && c.runContext.Err() == nil {
		recovery.next = time.Now().Add(recoveryDelay(recovery.failures))
		if recovery.failures < 5 {
			recovery.failures++
		}
	}
}

// A manual stop of a starting voice channel must interrupt its readiness wait
// before it can acquire opMu. Busy/identifying ready channels keep the existing
// explicit-stop checks and do not lose their desired intent on a rejected stop.
func (c *Controller) cancelChannelStart(id string) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.status.State != "running" {
		return
	}
	for _, row := range c.status.Channels {
		if row.ID != id || row.Role != "voice" || row.Busy || row.Identifying {
			continue
		}
		if recovery := c.recovery[id]; recovery != nil && recovery.attempt != nil {
			recovery.wanted = false
			recovery.attempt.cancel()
		}
	}
}

func (c *Controller) currentAttemptLocked(id string, attempt *channelAttempt) bool {
	recovery := c.recovery[id]
	return (c.c4fm == nil || c.c4fm.channel.ID != id) && !c.closing && c.status.State == "running" && c.runID == attempt.run &&
		recovery != nil && recovery.wanted && recovery.attempt == attempt && attempt.ctx.Err() == nil
}

func (c *Controller) prepareChannelAttempt(ctx context.Context, id string) (*channelAttempt, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.c4fm != nil && c.c4fm.channel.ID == id {
		return nil, errors.New("the voice channel is reserved for a C4FM test")
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if c.closing || c.status.State != "running" || c.runContext == nil || c.runContext.Err() != nil {
		return nil, errors.New("start the Site before starting an individual voice channel")
	}
	recovery := c.recovery[id]
	if recovery == nil {
		return nil, errors.New("choose a configured voice channel")
	}
	for _, row := range c.status.Channels {
		if row.ID == id && row.State == "stopping" {
			return nil, errors.New("voice channel is stopping")
		}
	}
	if recovery.attempt != nil {
		return nil, errors.New("voice channel is already starting")
	}
	operation, cancel := context.WithCancel(ctx)
	stopRun := context.AfterFunc(c.runContext, cancel)
	attempt := &channelAttempt{run: c.runID, ctx: operation, cancel: func() { stopRun(); cancel() }}
	recovery.wanted = true
	recovery.failures = 0
	recovery.next = time.Time{}
	recovery.attempt = attempt
	return attempt, nil
}
