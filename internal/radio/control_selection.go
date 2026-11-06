package radio

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"sort"
	"sync/atomic"
	"time"
)

// A switch is transient operator/runtime intent. It is cancelled by Stop,
// shutdown or a new configuration; it is never saved as a modem assignment.
type controlSwitchAttempt struct {
	ctx                  context.Context
	cancel               context.CancelFunc
	epoch                uint64
	profile              Profile
	mode, target, reason string
	previous, run        string
	lastFailed           string
	voiceIntent          map[string]bool
	retired              bool
	quiesced             map[*child]bool
	outage               atomic.Bool
}

func promotedProfile(p Profile, controlID string) Profile {
	p = cloneProfile(p)
	for i := range p.Site.Channels {
		channel := &p.Site.Channels[i]
		if channel.Enabled {
			channel.Role = "voice"
			if channel.ID == controlID {
				channel.Role = "control"
			}
		}
	}
	return p
}

func controlCandidates(p Profile) []Channel {
	channels := []Channel{}
	for _, channel := range p.Site.Channels {
		if channel.Enabled && channel.ControlEligible {
			channels = append(channels, channel)
		}
	}
	sort.Slice(channels, func(i, j int) bool {
		if channels[i].ControlPriority != channels[j].ControlPriority {
			return channels[i].ControlPriority < channels[j].ControlPriority
		}
		return channels[i].ID < channels[j].ID
	})
	return channels
}

func rotationInterval(policy ControlPolicy) (time.Duration, error) {
	hours := policy.RotationHours
	if math.IsNaN(hours) || math.IsInf(hours, 0) || hours <= 0 || hours > float64(math.MaxInt64)/float64(time.Hour) {
		return 0, errors.New("the configured control rotation interval cannot be scheduled")
	}
	duration := time.Duration(hours * float64(time.Hour))
	if duration <= 0 {
		return 0, errors.New("the configured control rotation interval cannot be scheduled")
	}
	return duration, nil
}

func (c *Controller) updateNextRotationLocked() {
	c.status.NextControlRotation = time.Time{}
	if !c.profile.Site.ControlPolicy.RotationEnabled || c.status.ControlChangedAt.IsZero() {
		return
	}
	interval, err := rotationInterval(c.profile.Site.ControlPolicy)
	if err != nil {
		c.status.ControlSwitchError = err.Error()
		return
	}
	c.status.NextControlRotation = c.status.ControlChangedAt.Add(interval)
}

// SwitchControl accepts a manual request. Native workers, rather than browser
// status, establish idleness before any process is retired.
func (c *Controller) SwitchControl(ctx context.Context, channelID string) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	lease, err := c.startLease()
	if err != nil {
		return err
	}
	defer lease.Close()
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.c4fm != nil {
		return errors.New("stop the C4FM test before selecting a control channel")
	}
	if c.status.State != "running" || !c.siteWanted || c.runContext == nil || c.runContext.Err() != nil || c.stopRequests != 0 || c.closing {
		return errors.New("start the Site before selecting a control channel")
	}
	if c.controlSwitch != nil {
		return errors.New("a control-channel switch is already pending")
	}
	if channelID == c.status.ActiveControlChannelID {
		return errors.New("that channel is already selected as control")
	}
	found := false
	for _, channel := range controlCandidates(c.configuredProfile) {
		if channel.ID == channelID {
			found = true
		}
	}
	if !found {
		return errors.New("the selected channel is not enabled and control eligible in this Site's operating configuration")
	}
	c.queueControlSwitchLocked(channelID, "manual")
	return nil
}

func (c *Controller) queueControlSwitchLocked(target, reason string) {
	ctx, cancel := context.WithCancel(c.ctx)
	intent := map[string]bool{}
	for id, recovery := range c.recovery {
		intent[id] = recovery.wanted
	}
	// A working control resource returns as a wanted voice resource after
	// promotion, without changing its saved Role or frequency pair.
	intent[c.status.ActiveControlChannelID] = true
	attempt := &controlSwitchAttempt{ctx: ctx, cancel: cancel, epoch: c.stopEpoch, profile: cloneProfile(c.configuredProfile), mode: c.status.Mode, target: target, reason: reason, previous: c.status.ActiveControlChannelID, run: c.runID, voiceIntent: intent, quiesced: map[*child]bool{}}
	c.controlSwitch = attempt
	attempt.outage.Store(reason == "failover")
	c.status.ControlSwitchTarget, c.status.ControlSwitchReason = target, reason
	c.status.ControlSwitchError = "Waiting for the Site to become idle."
	c.emitLocked(Event{Kind: "control_switch_pending", ChannelID: target, Message: "Control-channel " + reason + " requested; waiting for all Site calls to finish."})
	go c.runControlSwitch(attempt)
}

func (c *Controller) currentControlSwitchLocked(attempt *controlSwitchAttempt) bool {
	return c.controlSwitch == attempt && attempt.ctx.Err() == nil && c.siteWanted && !c.closing && c.stopRequests == 0 && c.stopEpoch == attempt.epoch
}

func (c *Controller) scheduleControlRotation(now time.Time) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.c4fm != nil || c.controlSwitch != nil || c.status.State != "running" || !c.siteWanted || c.stopRequests != 0 || c.closing || c.status.NextControlRotation.IsZero() || now.Before(c.status.NextControlRotation) {
		return
	}
	channels := controlCandidates(c.configuredProfile)
	if len(channels) == 0 {
		c.status.ControlSwitchError = "Control rotation is waiting for an eligible channel."
		return
	}
	start := -1
	for i, ch := range channels {
		if ch.ID == c.status.ActiveControlChannelID {
			start = i
			break
		}
	}
	for offset := 1; offset <= len(channels); offset++ {
		channel := channels[(start+offset)%len(channels)]
		if channel.ID == c.status.ActiveControlChannelID {
			continue
		}
		if recovery := c.recovery[channel.ID]; recovery != nil && !recovery.wanted {
			continue
		}
		process := c.children[channel.ID]
		if process == nil || !process.alive() || !process.readyNow() || !process.validDevice() {
			continue
		}
		c.queueControlSwitchLocked(channel.ID, "rotation")
		return
	}
	c.status.ControlSwitchError = "Control rotation is waiting for another wanted eligible channel."
}

// A dynamic control outage keeps healthy voice workers alive until their
// actual native call/permit state is idle. It never preempts them to promote.
func (c *Controller) dynamicControlFailed(process *child) bool {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.profile.Site.ControlPolicy.Mode != "dynamic" || c.children[process.id] != process || c.status.State != "running" || !c.siteWanted || c.closing || c.stopRequests != 0 {
		return false
	}
	if c.c4fm != nil {
		c.c4fm.restore = false
		c.c4fm.cancel()
	}
	c.status.InhibitionReady = false
	c.withdrawSourceFactsLocked()
	c.status.Error = "The control channel is unavailable; waiting to promote an idle eligible resource."
	for i := range c.status.Channels {
		if c.status.Channels[i].ID == process.id {
			c.status.Channels[i].State = "fault"
			c.status.Channels[i].Ready = false
			c.status.Channels[i].Busy = false
			c.status.Channels[i].Error = c.status.Error
		}
	}
	if c.controlSwitch != nil {
		c.controlSwitch.outage.Store(true)
		c.status.ControlSwitchReason = "failover"
		return true
	}
	c.queueControlSwitchLocked("", "failover")
	return true
}

func (c *Controller) switchTargetLocked(attempt *controlSwitchAttempt) string {
	if attempt.target != "" && attempt.reason == "manual" {
		// An ordinary manual request stays pinned to its explicit target.
		// After a dynamic outage, an unavailable/failed target must not prevent
		// another eligible resource from restoring control.
		if !attempt.outage.Load() {
			return attempt.target
		}
		usable := attempt.lastFailed != attempt.target
		if !attempt.retired {
			process := c.children[attempt.target]
			usable = usable && process != nil && process.alive() && process.readyNow() && process.validDevice()
		}
		if usable && attempt.mode == "hardware" {
			for _, channel := range attempt.profile.Site.Channels {
				if channel.ID == attempt.target {
					modem, _ := findModem(attempt.profile, channel.ModemID)
					_, err := c.resolveModem(modem.Device)
					usable = err == nil
				}
			}
		}
		if usable {
			return attempt.target
		}
	}
	channels := controlCandidates(attempt.profile)
	start := 0
	after := attempt.lastFailed
	if after == "" && attempt.reason == "rotation" {
		after = attempt.previous
	}
	for i, channel := range channels {
		if channel.ID == after {
			start = (i + 1) % len(channels)
			break
		}
	}
	if len(channels) > 0 {
		channels = append(append([]Channel{}, channels[start:]...), channels[:start]...)
	}
	for _, channel := range channels {
		if channel.ID == attempt.previous {
			continue
		}
		if wanted, exists := attempt.voiceIntent[channel.ID]; exists && !wanted {
			continue
		}
		if !attempt.retired {
			child := c.children[channel.ID]
			if child == nil || !child.alive() || !child.readyNow() || !child.validDevice() {
				continue
			}
		}
		if attempt.mode == "hardware" {
			modem, _ := findModem(attempt.profile, channel.ModemID)
			if _, err := c.resolveModem(modem.Device); err != nil {
				continue
			}
		}
		return channel.ID
	}
	// If no alternate can operate, ordinary same-device recovery remains
	// possible. A successful replacement is never automatically returned.
	return attempt.previous
}

func (c *Controller) runControlSwitch(attempt *controlSwitchAttempt) {
	defer attempt.cancel()
	defer func() {
		c.opMu.Lock()
		c.mu.Lock()
		resume := !attempt.retired && c.runID == attempt.run && !c.closing && c.stopRequests == 0
		processes := []*child{}
		if resume {
			for process := range attempt.quiesced {
				if c.children[process.id] == process && process.alive() {
					processes = append(processes, process)
				}
			}
		}
		if c.controlSwitch == attempt {
			c.controlSwitch = nil
			c.status.ControlSwitchTarget = ""
			c.status.ControlSwitchReason = ""
		}
		c.mu.Unlock()
		for _, process := range processes {
			ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
			_, err := process.client.Put(ctx, "/p25/control-quiesce", map[string]bool{"enabled": false})
			cancel()
			if err != nil {
				c.emit(Event{Kind: "control_switch_resume_failed", ChannelID: process.id, Message: err.Error()})
			}
		}
		c.opMu.Unlock()
	}()
	for failures := uint(0); ; {
		lease, leaseErr := c.startLease()
		if leaseErr != nil {
			c.mu.Lock()
			if c.controlSwitch == attempt {
				c.status.ControlSwitchError = leaseErr.Error()
			}
			c.mu.Unlock()
			return
		}
		c.opMu.Lock()
		unlock := func() { c.opMu.Unlock(); _ = lease.Close() }
		c.mu.Lock()
		current := c.currentControlSwitchLocked(attempt)
		if current && !attempt.retired {
			// Alias/display updates may be applied without cancelling a wait.
			// Select the latest accepted snapshot before retiring its owners.
			attempt.profile = cloneProfile(c.configuredProfile)
			for id, recovery := range c.recovery {
				attempt.voiceIntent[id] = recovery.wanted
			}
			attempt.voiceIntent[attempt.previous] = true
		}
		target := c.switchTargetLocked(attempt)
		if current && target != "" {
			c.status.ControlSwitchTarget = target
		}
		c.mu.Unlock()
		if !current {
			unlock()
			return
		}
		if target == "" {
			unlock()
			if c.waitSiteRetry(attempt.ctx, time.Second) != nil {
				return
			}
			continue
		}
		idle, err := c.quiesceForControlSwitch(attempt)
		if err == nil && !idle {
			c.mu.Lock()
			if c.currentControlSwitchLocked(attempt) {
				c.status.ControlSwitchError = "Waiting for all Site calls, permits and identification to finish."
			}
			c.mu.Unlock()
			unlock()
			if c.waitSiteRetry(attempt.ctx, 500*time.Millisecond) != nil {
				return
			}
			continue
		}
		if err == nil {
			stop, cancel := context.WithTimeout(attempt.ctx, 12*time.Second)
			err = c.stop(stop, false)
			cancel()
			if err == nil {
				attempt.retired = true
			}
		}
		if err == nil {
			c.mu.Lock()
			current = c.currentControlSwitchLocked(attempt)
			var recovery *siteAttempt
			if current {
				recovery = &siteAttempt{ctx: attempt.ctx, cancel: attempt.cancel, done: make(chan struct{}), epoch: attempt.epoch, profile: attempt.profile, mode: attempt.mode, voiceIntent: attempt.voiceIntent, controlID: target}
				c.siteRecovery = recovery
			}
			c.mu.Unlock()
			if !current {
				unlock()
				return
			}
			err = c.startSiteOnControl(attempt.ctx, attempt.profile, attempt.mode, attempt.voiceIntent, attempt.epoch, recovery, target)
		}
		if err == nil {
			c.mu.Lock()
			if c.controlSwitch == attempt {
				c.status.ControlSwitchError = ""
				c.updateNextRotationLocked()
				c.status.ControlSwitchTarget = ""
				c.status.ControlSwitchReason = ""
				c.emitLocked(Event{Kind: "control_channel_changed", ChannelID: target, Message: "Control channel changed after the Site became idle; saved channel assignments are unchanged."})
			}
			c.mu.Unlock()
			unlock()
			return
		}
		attempt.lastFailed = target
		c.mu.Lock()
		current = c.currentControlSwitchLocked(attempt)
		if current {
			c.status.ControlSwitchError = err.Error()
			c.emitLocked(Event{Kind: "control_switch_waiting", ChannelID: target, Message: err.Error()})
		}
		c.mu.Unlock()
		unlock()
		if !current {
			return
		}
		if c.waitSiteRetry(attempt.ctx, recoveryDelay(failures)) != nil {
			return
		}
		if failures < 5 {
			failures++
		}
	}
}

func (c *Controller) quiesceForControlSwitch(attempt *controlSwitchAttempt) (bool, error) {
	c.mu.Lock()
	children := make([]*child, 0, len(c.children))
	for _, process := range c.children {
		children = append(children, process)
	}
	c.mu.Unlock()
	// Gate the control authority first so no new call can race an idle VC.
	sort.Slice(children, func(i, j int) bool { return children[i].role == "control" && children[j].role != "control" })
	idle := true
	for _, process := range children {
		if !process.alive() || !process.validDevice() {
			if attempt.outage.Load() || attempt.retired {
				continue
			}
			return false, errors.New("a Site worker became unavailable while preparing the control switch")
		}
		ctx, cancel := context.WithTimeout(attempt.ctx, 2*time.Second)
		reply, err := process.client.Put(ctx, "/p25/control-quiesce", map[string]bool{"enabled": true})
		cancel()
		if err != nil {
			return false, fmt.Errorf("control switch cannot confirm native idleness: %w", err)
		}
		var quiesced, nativeIdle bool
		if json.Unmarshal(reply["quiesced"], &quiesced) != nil || !quiesced || json.Unmarshal(reply["idle"], &nativeIdle) != nil {
			return false, errors.New("native worker did not confirm control-switch quiescence")
		}
		attempt.quiesced[process] = true
		idle = idle && nativeIdle
	}
	return idle, nil
}
