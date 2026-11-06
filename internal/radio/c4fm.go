package radio

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"
)

// A test reserves one resource in one existing Site run. Unlike normal voice
// recovery, it is never retried, replayed or saved.
type c4fmTest struct {
	run               string
	epoch             uint64
	channel           Channel
	configuredChannel Channel
	modem             Modem
	ctx               context.Context
	cancel            context.CancelFunc
	process           *child
	startLease        *os.File
	restore           bool
	status            C4FMTestStatus
}

func (c *Controller) C4FMTestStatus() C4FMTestStatus {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.c4fm != nil {
		return c.c4fm.status
	}
	if c.c4fmStatus.State == "" {
		return C4FMTestStatus{State: "idle", Pattern: "PRBS511"}
	}
	return c.c4fmStatus
}

// StartC4FMTest uses saved settings without defaults or programming writes.
// The native helper owns every FIFO write and all RF timing.
func (c *Controller) StartC4FMTest(ctx context.Context, p Profile, request C4FMTestRequest) (err error) {
	if request.DurationSeconds <= 0 || uint64(request.DurationSeconds) > uint64(^uint32(0))/1000 {
		return errors.New("the test duration must fit a positive native millisecond timer")
	}
	c.mu.Lock()
	admissionEpoch, admissionRun, admissionStop := c.c4fmEpoch, c.runID, c.stopEpoch
	c.mu.Unlock()
	path, err := nativeExecutable(c.opts.EnginePath)
	if err != nil {
		return err
	}
	probe, cancel := context.WithTimeout(ctx, 3*time.Second)
	output, err := exec.CommandContext(probe, path, "--c4fm-test-check").CombinedOutput()
	cancel()
	if ctx.Err() != nil {
		return ctx.Err()
	}
	if err != nil || strings.TrimSpace(string(output)) != "C4FM_TEST available" {
		return errors.New("the installed native engine does not provide the C4FM test")
	}
	lease, err := c.startLease()
	if err != nil {
		return err
	}
	c.opMu.Lock()
	defer c.opMu.Unlock()
	c.mu.Lock()
	if c.c4fmEpoch != admissionEpoch || c.runID != admissionRun || c.stopEpoch != admissionStop {
		c.mu.Unlock()
		lease.Close()
		return context.Canceled
	}
	if c.c4fm != nil {
		c.mu.Unlock()
		lease.Close()
		return errors.New("a C4FM test already owns a transmitter")
	}
	if c.closing || c.stopRequests != 0 || c.status.State != "running" || !c.siteWanted || c.runContext == nil || c.runContext.Err() != nil {
		c.mu.Unlock()
		lease.Close()
		return errors.New("start the Site before testing an idle voice transmitter")
	}
	if c.status.Mode != "hardware" || !c.opts.AllowRF {
		c.mu.Unlock()
		lease.Close()
		return errors.New("hardware RF operation is not enabled for this Site")
	}
	if c.controlSwitch != nil {
		c.mu.Unlock()
		lease.Close()
		return errors.New("wait for the control-channel switch to finish")
	}
	channel, found := c4fmChannel(p, request.ChannelID)
	modem, modemFound := findModem(p, channel.ModemID)
	active, activeFound := c4fmChannel(c.profile, request.ChannelID)
	activeModem, _ := findModem(c.profile, active.ModemID)
	configured, configuredFound := c4fmChannel(c.configuredProfile, request.ChannelID)
	configuredModem, _ := findModem(c.configuredProfile, configured.ModemID)
	var row ChannelStatus
	for _, candidate := range c.status.Channels {
		if candidate.ID == request.ChannelID {
			row = candidate
		}
	}
	if p.Site.ID != c.profile.Site.ID || !found || !modemFound || !activeFound || !configuredFound || channel != configured || modem != activeModem || modem != configuredModem || active.Role != "voice" || !active.Enabled || row.Role != "voice" {
		c.mu.Unlock()
		lease.Close()
		return errors.New("choose a saved voice transmitter in the current Site run; restart the Site to apply changed settings")
	}
	recovery := c.recovery[channel.ID]
	if row.Busy || row.Identifying || row.State != "ready" || !row.Ready || recovery == nil || recovery.attempt != nil {
		c.mu.Unlock()
		lease.Close()
		return errors.New("wait for an operating voice channel to become idle")
	}
	operation, stop := context.WithCancel(c.runContext)
	test := &c4fmTest{run: c.runID, epoch: c.stopEpoch, channel: active, configuredChannel: configured, modem: modem, ctx: operation, cancel: stop, startLease: lease, restore: recovery.wanted,
		status: C4FMTestStatus{State: "starting", ChannelID: channel.ID, ChannelName: channel.Name, ModemID: modem.ID, TXHz: channel.TXHz, PowerPercent: channel.PowerPercent, DurationSeconds: request.DurationSeconds, Pattern: "PRBS511"}}
	previous := c.children[channel.ID]
	c.c4fm = test
	// Other requests that were still probing must not become queued tests after
	// this one finishes; a later operation needs a fresh explicit admission.
	c.c4fmEpoch++
	c.mu.Unlock()
	failed, retired := true, false
	defer func() {
		if !failed {
			return
		}
		cleanup, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		if !retired && previous != nil && previous.alive() {
			resume, resumeErr := previous.client.Put(cleanup, "/p25/control-quiesce", map[string]bool{"enabled": false})
			var stillQuiesced bool
			if resumeErr == nil && (json.Unmarshal(resume["quiesced"], &stillQuiesced) != nil || stillQuiesced) {
				resumeErr = errors.New("native voice worker did not confirm resumed admission")
			}
			err = errors.Join(err, resumeErr)
			c.mu.Lock()
			test.restore = false
			if resumeErr != nil {
				message := "Could not resume voice admission; stop and start this channel: " + resumeErr.Error()
				previous.mu.Lock()
				previous.admissionError = message
				previous.mu.Unlock()
				c.c4fmChannelStateLocked(test, "fault", previous.cmd.Process.Pid, message)
			}
			c.mu.Unlock()
		}
		err = errors.Join(err, c.finishC4FMTest(cleanup, test, err))
	}()
	startup, cancel := context.WithCancel(ctx)
	stopRun := context.AfterFunc(test.ctx, cancel)
	defer cancel()
	defer stopRun()
	if previous == nil || previous.client == nil {
		return errors.New("the selected voice worker is unavailable")
	}
	reply, err := previous.client.Put(startup, "/p25/control-quiesce", map[string]bool{"enabled": true})
	if err != nil {
		return err
	}
	reserve, stopReserve := context.WithTimeout(startup, c.opts.StartupTimeout)
	err = waitC4FMIdle(reserve, previous.client, reply)
	stopReserve()
	if err != nil {
		return err
	}
	if err = c.stopChannel(startup, channel.ID); err != nil {
		return err
	}
	retired = true
	if err = startup.Err(); err != nil {
		return err
	}
	modemLease, err := c.acquireModem(startup, modem.Device)
	if modemLease != nil {
		done := make(chan struct{})
		close(done)
		test.process = &child{id: channel.ID, role: "c4fm-test", done: done, groupRetired: true, lease: modemLease, intentional: true}
	}
	if err != nil {
		return err
	}
	if err = c.verifyModem(startup, modemLease, modem.Device, false); err != nil {
		return err
	}
	dir := filepath.Join(c.rootDir, "c4fm-test")
	if err = writeFiles(dir, map[string]string{"config.yml": string(renderC4FMTest(active, modem))}); err != nil {
		return err
	}
	if err = startup.Err(); err != nil {
		return err
	}
	c.mu.Lock()
	if test.ctx.Err() != nil || c.c4fm != test || c.runID != test.run || c.stopEpoch != test.epoch || c.stopRequests != 0 || c.closing || !c.siteWanted {
		c.mu.Unlock()
		return context.Canceled
	}
	process, err := launch(c.opts.EnginePath, dir, "config.yml", channel.ID, "c4fm-test", nil, func(_ *child, line string) { c.observeC4FMTest(test, line) }, "-f", "--c4fm-test", fmt.Sprint(uint64(request.DurationSeconds)*1000))
	if err != nil {
		c.mu.Unlock()
		return err
	}
	process.lease, process.device, process.sameDevice, process.stopGrace = modemLease, modem.Device, c.modem.same, 7*time.Second
	test.process = process
	c.c4fmChannelStateLocked(test, "testing", process.cmd.Process.Pid, "")
	c.mu.Unlock()
	wait, stopWait := context.WithTimeout(startup, c.opts.StartupTimeout)
	err = process.waitReady(wait)
	stopWait()
	if err != nil {
		return err
	}
	if err = startup.Err(); err != nil {
		return err
	}
	failed = false
	go c.watchC4FMTest(test, process)
	return nil
}

func c4fmChannel(p Profile, id string) (Channel, bool) {
	for _, channel := range p.Site.Channels {
		if channel.ID == id {
			return channel, true
		}
	}
	return Channel{}, false
}

// The initial fence response may precede the protocol clock's serial barrier
// and fresh quiet modem observations. Poll only this explicit operation's
// actual runtime result while keeping the admission fence in place.
func waitC4FMIdle(ctx context.Context, client *NativeClient, reply map[string]json.RawMessage) error {
	for {
		var quiesced, idle bool
		if json.Unmarshal(reply["quiesced"], &quiesced) != nil || !quiesced || json.Unmarshal(reply["idle"], &idle) != nil {
			return errors.New("the native voice worker did not confirm its admission fence")
		}
		if idle {
			return nil
		}
		timer := time.NewTimer(100 * time.Millisecond)
		select {
		case <-ctx.Done():
			timer.Stop()
			return fmt.Errorf("waiting for native voice idleness: %w", ctx.Err())
		case <-timer.C:
		}
		var err error
		reply, err = client.Get(ctx, "/p25/control-quiesce")
		if err != nil {
			return err
		}
	}
}

func renderC4FMTest(channel Channel, modem Modem) []byte {
	x := modem.Calibration
	return []byte(fmt.Sprintf("c4fmTest:\n  portType: uart\n  device: %s\n  baud: %d\n  rxFrequencyHz: %d\n  txFrequencyHz: %d\n  powerPercent: %d\n  rxOffsetHz: %d\n  txOffsetHz: %d\n  rxLevel: %v\n  txLevel: %v\n  rxDCOffset: %d\n  txDCOffset: %d\n  rxInvert: %t\n  txInvert: %t\n  pttInvert: %t\n", quoted(modem.Device), modem.Baud, channel.RXHz, channel.TXHz, channel.PowerPercent, x.RXOffsetHz, x.TXOffsetHz, x.RXLevel, x.TXLevel, x.RXDCOffset, x.TXDCOffset, x.RXInvert, x.TXInvert, x.PTTInvert))
}

func (c *Controller) observeC4FMTest(test *c4fmTest, line string) {
	at := strings.Index(line, "C4FM_TEST ")
	if at < 0 {
		return
	}
	marker := strings.TrimSpace(line[at+len("C4FM_TEST "):])
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.c4fm != test {
		return
	}
	switch marker {
	case "ready":
		if test.status.State == "starting" {
			test.status.State = "ready"
		}
	case "transmitting":
		if !test.status.TXObserved {
			test.status.StartedAt = time.Now().UTC()
			test.status.EndsAt = test.status.StartedAt.Add(time.Duration(test.status.DurationSeconds) * time.Second)
		}
		test.status.TXObserved = true
		if !test.status.StopConfirmed && test.status.State != "draining" && test.status.State != "failed" {
			test.status.State = "transmitting"
		}
	case "draining":
		if test.status.State != "failed" {
			test.status.State = "draining"
		}
	case "stopped confirmed":
		test.status.StopConfirmed = true
	case "stopped unconfirmed":
		test.status.Error = "The native engine could not confirm modem stop."
	default:
		if strings.HasPrefix(marker, "failed") {
			test.status.State = "failed"
			test.status.Error = strings.TrimSpace(strings.TrimPrefix(marker, "failed"))
		}
	}
}

func (c *Controller) watchC4FMTest(test *c4fmTest, process *child) {
	watchdog := time.NewTimer(time.Duration(test.status.DurationSeconds)*time.Second + 15*time.Second)
	defer watchdog.Stop()
	var cause error
	select {
	case <-process.done:
	case <-test.ctx.Done():
	case <-watchdog.C:
		cause = errors.New("native C4FM test exceeded its duration and drain allowance")
	}
	c.opMu.Lock()
	defer c.opMu.Unlock()
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	_ = c.finishC4FMTest(ctx, test, cause)
}

func (c *Controller) StopC4FMTest(ctx context.Context) error {
	c.mu.Lock()
	c.c4fmEpoch++
	test := c.c4fm
	if test != nil {
		test.cancel()
	}
	c.mu.Unlock()
	if test == nil {
		return nil
	}
	c.opMu.Lock()
	defer c.opMu.Unlock()
	// Once accepted, stop survives a disconnected browser/request timeout.
	cleanup, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	return c.finishC4FMTest(cleanup, test, nil)
}

// finishC4FMTest owns opMu. Failed cleanup retains reservation and leases for
// explicit Stop; reaped leaders alone do not prove modem stop.
func (c *Controller) finishC4FMTest(ctx context.Context, expected *c4fmTest, cause error) error {
	c.mu.Lock()
	test := c.c4fm
	if test == nil || (expected != nil && test != expected) {
		c.mu.Unlock()
		return nil
	}
	test.cancel()
	if test.status.State != "failed" {
		test.status.State = "draining"
	}
	c.mu.Unlock()
	var cleanupErr error
	if test.process != nil {
		cleanupErr = test.process.stop(ctx, false)
	}
	if cleanupErr == nil {
		cleanupErr = test.startLease.Close()
	}
	c.mu.Lock()
	if cause != nil {
		if test.status.Error == "" {
			test.status.Error = cause.Error()
		} else {
			test.status.Error = cause.Error() + "; " + test.status.Error
		}
	}
	if cleanupErr != nil {
		test.status.State = "failed"
		test.status.Error = errors.Join(cause, cleanupErr).Error()
		pid := 0
		if test.process != nil && test.process.cmd != nil && test.process.cmd.Process != nil {
			pid = test.process.cmd.Process.Pid
		}
		c.c4fmChannelStateLocked(test, "fault", pid, test.status.Error)
		c.mu.Unlock()
		return cleanupErr
	}
	if test.process != nil {
		test.process.mu.Lock()
		exitErr := test.process.exitErr
		test.process.mu.Unlock()
		if exitErr != nil && test.status.Error == "" {
			test.status.Error = "Native test exited: " + exitErr.Error()
		}
	}
	if !test.status.StopConfirmed && test.status.Error == "" {
		test.status.Error = "The native engine exited without confirmed modem stop."
	}
	test.status.State = "stopped"
	if test.status.Error != "" {
		test.status.State = "failed"
	}
	test.status.FinishedAt = time.Now().UTC()
	active, found := c4fmChannel(c.profile, test.channel.ID)
	modem, _ := findModem(c.profile, active.ModemID)
	configured, configuredFound := c4fmChannel(c.configuredProfile, test.channel.ID)
	configuredModem, _ := findModem(c.configuredProfile, configured.ModemID)
	restore := test.restore && test.status.StopConfirmed && c.runID == test.run && c.stopEpoch == test.epoch && c.siteWanted && !c.closing && c.stopRequests == 0 && c.status.State == "running" && c.runContext != nil && c.runContext.Err() == nil && found && active == test.channel && modem == test.modem && configuredFound && configured == test.configuredChannel && configuredModem == test.modem
	if test.process != nil {
		c.c4fmChannelStateLocked(test, "stopped", 0, "")
	}
	c.c4fmStatus = test.status
	c.c4fm = nil
	c.mu.Unlock()
	if restore {
		attempt, err := c.prepareChannelAttempt(context.Background(), test.channel.ID)
		if err != nil {
			return err
		}
		err = c.startChannel(attempt.ctx, test.channel.ID, attempt)
		c.finishChannelAttempt(test.channel.ID, attempt, err)
		if err != nil {
			c.mu.Lock()
			c.c4fmStatus.Error = "modem stop confirmed; restoring the voice channel failed: " + err.Error()
			c.mu.Unlock()
		}
		return err
	}
	return nil
}

func (c *Controller) c4fmChannelStateLocked(test *c4fmTest, state string, pid int, message string) {
	if c.runID != test.run {
		return
	}
	for i := range c.status.Channels {
		row := &c.status.Channels[i]
		if row.ID == test.channel.ID {
			row.State, row.PID, row.Ready, row.Busy, row.Error = state, pid, false, false, message
		}
	}
}
