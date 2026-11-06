package radio

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"path/filepath"
	"time"
)

// StopChannel withdraws one idle voice resource. The dedicated control channel
// is stopped only by Stop, so an administrator cannot strand a live Site by
// treating it as an ordinary voice resource.
func (c *Controller) StopChannel(ctx context.Context, id string) error {
	c.mu.Lock()
	c.c4fmEpoch++
	if test := c.c4fm; test != nil && test.channel.ID == id {
		test.restore = false
		test.cancel()
	}
	if pending := c.controlSwitch; pending != nil && pending.reason == "manual" && pending.target == id {
		pending.cancel()
	}
	c.mu.Unlock()
	c.cancelChannelStart(id)
	c.mu.Lock()
	operating := c.status.State == "running"
	c.mu.Unlock()
	if !operating {
		return errors.New("Site is not operating")
	}
	c.opMu.Lock()
	defer c.opMu.Unlock()
	c.mu.Lock()
	test := c.c4fm
	if test != nil && test.channel.ID == id {
		test.restore = false
		test.cancel()
	} else {
		test = nil
	}
	c.mu.Unlock()
	if test != nil {
		if err := c.finishC4FMTest(ctx, test, nil); err != nil {
			return err
		}
	}
	return c.stopChannel(ctx, id)
}

// stopChannel owns opMu. C4FM reservation uses this same withdrawal/retirement
// path before opening the selected modem for its finite native test.
func (c *Controller) stopChannel(ctx context.Context, id string) error {
	c.mu.Lock()
	if c.status.State != "running" {
		c.mu.Unlock()
		return errors.New("Site is not operating")
	}
	process := c.children[id]
	var row *ChannelStatus
	for i := range c.status.Channels {
		if c.status.Channels[i].ID == id {
			row = &c.status.Channels[i]
			break
		}
	}
	if row == nil || row.Role != "voice" {
		c.mu.Unlock()
		return errors.New("choose a voice channel; stop the whole Site to stop its control channel")
	}
	if process == nil {
		if recovery := c.recovery[id]; recovery != nil {
			recovery.wanted = false
			if recovery.attempt != nil {
				recovery.attempt.cancel()
			}
		}
		row.State, row.Ready, row.Busy, row.PID, row.Error = "stopped", false, false, 0, ""
		c.mu.Unlock()
		return nil
	}
	if row.Busy || row.Identifying {
		c.mu.Unlock()
		return errors.New("wait for the voice transmission or identification to finish")
	}
	c.mu.Unlock()
	client, err := c.ControlClient()
	if err != nil {
		return err
	}
	data, err := client.Get(ctx, "/status")
	if err != nil {
		return err
	}
	var allocated []struct {
		ChannelNo int  `json:"channelNo"`
		TX        bool `json:"tx"`
	}
	_ = json.Unmarshal(data["vcChannels"], &allocated)
	c.mu.Lock()
	channelNumber := 0
	for _, ch := range c.profile.Site.Channels {
		if ch.ID == id {
			channelNumber = ch.ChannelNo
		}
	}
	c.mu.Unlock()
	for _, allocation := range allocated {
		if allocation.ChannelNo == channelNumber && allocation.TX {
			return errors.New("wait for the channel allocation to be released")
		}
	}
	c.mu.Lock()
	if recovery := c.recovery[id]; recovery != nil {
		recovery.wanted = false
		if recovery.attempt != nil {
			recovery.attempt.cancel()
		}
	}
	for i := range c.status.Channels {
		if c.status.Channels[i].ID == id {
			c.status.Channels[i].State = "stopping"
			c.status.Channels[i].Ready = false
		}
	}
	identification := c.profile.Site.Identification.Enabled && c.profile.Site.Identification.Mode == "ham" && c.status.Mode == "hardware"
	c.mu.Unlock()
	if err = process.stop(ctx, identification); err != nil {
		c.mu.Lock()
		for i := range c.status.Channels {
			if c.status.Channels[i].ID == id && c.children[id] == process {
				c.status.Channels[i].State = "fault"
				c.status.Channels[i].Error = err.Error()
			}
		}
		c.mu.Unlock()
		return err
	}
	c.mu.Lock()
	if c.children[id] == process {
		delete(c.children, id)
	}
	for i := range c.status.Channels {
		if c.status.Channels[i].ID == id {
			c.status.Channels[i].State = "stopped"
			c.status.Channels[i].PID = 0
			c.status.Channels[i].Ready = false
			c.status.Channels[i].Busy = false
			c.status.Channels[i].Error = ""
		}
	}
	c.mu.Unlock()
	c.emit(Event{Kind: "channel_stopped", ChannelID: id, Message: "Voice channel stopped; control channel remains operating."})
	return nil
}

func (c *Controller) StartChannel(ctx context.Context, id string) error {
	lease, err := c.startLease()
	if err != nil {
		return err
	}
	defer lease.Close()
	attempt, err := c.prepareChannelAttempt(ctx, id)
	if err != nil {
		return err
	}
	c.opMu.Lock()
	err = c.startChannel(attempt.ctx, id, attempt)
	c.opMu.Unlock()
	c.finishChannelAttempt(id, attempt, err)
	return err
}

// startChannel owns opMu through startup and cleanup. Stop/Close can cancel its
// attempt before waiting for that mutex; no untracked child can outlive failure.
func (c *Controller) startChannel(ctx context.Context, id string, attempt *channelAttempt) (err error) {
	lease, err := c.startLease()
	if err != nil {
		c.mu.Lock()
		if recovery := c.recovery[id]; errors.Is(err, ErrMaintenance) && recovery != nil && recovery.attempt == attempt && c.runID == attempt.run {
			recovery.wanted = false
			recovery.next = time.Time{}
			c.emitLocked(Event{Kind: "channel_recovery_cancelled", ChannelID: id, Message: err.Error()})
		}
		c.mu.Unlock()
		return err
	}
	defer lease.Close()
	c.mu.Lock()
	if !c.currentAttemptLocked(id, attempt) {
		c.mu.Unlock()
		return context.Canceled
	}
	p, n, mode, dir := c.profile, c.network, c.status.Mode, c.rootDir
	previous := c.children[id]
	if previous != nil && previous.alive() {
		c.mu.Unlock()
		return errors.New("voice channel is already operating")
	}
	rpcPort, restPort := 0, 0
	for _, row := range c.status.Channels {
		if row.ID == id && row.Role == "voice" {
			rpcPort, restPort = row.RPCPort, row.RESTPort
		}
	}
	c.mu.Unlock()
	var process *child
	var leaseCloser interface{ Close() error }
	failed := true
	var previousCleanupErr error
	defer func() {
		if !failed {
			return
		}
		cleanupErr := previousCleanupErr
		if cleanupErr != nil {
			// Do not forget an owner merely because its leader already exited.
		} else if process != nil {
			stop, cancel := context.WithTimeout(context.Background(), 3*time.Second)
			if ctx.Err() != nil {
				cancel() // cancelled startup needs immediate forced retirement
			}
			cleanupErr = process.stop(stop, false)
			cancel()
		} else if leaseCloser != nil {
			cleanupErr = leaseCloser.Close()
			if cleanupErr != nil {
				// A failed pre-launch Close still needs an owned retry target.
				done := make(chan struct{})
				close(done)
				process = &child{id: id, role: "voice", dir: filepath.Join(dir, id), done: done, groupRetired: true, lease: leaseCloser, intentional: true}
			}
		}
		if previousCleanupErr == nil {
			err = errors.Join(err, cleanupErr)
		}
		c.mu.Lock()
		if cleanupErr != nil && process != nil {
			c.children[id] = process
		} else if process != nil && c.children[id] == process {
			delete(c.children, id)
		}
		if c.runID == attempt.run {
			for i := range c.status.Channels {
				row := &c.status.Channels[i]
				if row.ID == id {
					row.State, row.Ready, row.Busy = "fault", false, false
					row.PID = 0
					if cleanupErr != nil && process != nil && process.cmd != nil && process.cmd.Process != nil {
						row.PID = process.cmd.Process.Pid
					}
					if err != nil {
						row.Error = err.Error()
					}
				}
			}
		}
		c.mu.Unlock()
	}()
	if previous != nil {
		if err = previous.stop(ctx, false); err != nil {
			process, previousCleanupErr = previous, err
			return err // keep the previous owner and lease in children
		}
		c.mu.Lock()
		if c.children[id] == previous {
			delete(c.children, id)
		}
		c.mu.Unlock()
	}
	var channel Channel
	found := false
	for _, ch := range p.Site.Channels {
		if ch.ID == id && ch.Enabled && ch.Role == "voice" {
			channel, found = ch, true
		}
	}
	if !found || rpcPort == 0 {
		return errors.New("voice channel is not in the operating Site configuration")
	}
	modem, _ := findModem(p, channel.ModemID)
	if mode == "hardware" {
		if leaseCloser, err = c.acquireModem(ctx, modem.Device); err != nil {
			return err
		}
		if err = c.verifyModem(ctx, leaseCloser, modem.Device, p.Site.Identification.Enabled && p.Site.Identification.Mode == "ham"); err != nil {
			return err
		}
	}
	if err = ctx.Err(); err != nil {
		return err
	}
	files, err := RenderChannel(p, channel, n, rpcPort, restPort, mode)
	if err != nil {
		return err
	}
	channelDir := filepath.Join(dir, id)
	if err = writeFiles(channelDir, files); err != nil {
		return err
	}
	client, err := NewNativeClient(net.JoinHostPort("127.0.0.1", fmt.Sprint(restPort)), n.RPCSecret)
	if err != nil {
		return err
	}
	if err = ctx.Err(); err != nil {
		return err
	}
	process, err = launch(c.opts.EnginePath, channelDir, "config.yml", id, "voice", client, c.observe)
	if err != nil {
		return err
	}
	process.lease, process.device, process.sameDevice = leaseCloser, modem.Device, c.modem.same
	c.mu.Lock()
	c.children[id] = process
	for i := range c.status.Channels {
		if c.status.Channels[i].ID == id {
			row := &c.status.Channels[i]
			row.State, row.Ready, row.Error = "starting", false, ""
			row.PID, row.StartedAt = process.cmd.Process.Pid, process.started
		}
	}
	c.mu.Unlock()
	wait, cancel := context.WithTimeout(ctx, c.opts.StartupTimeout)
	err = process.waitReady(wait)
	if err == nil {
		_, err = client.Get(wait, "/status")
	}
	if err == nil {
		_, err = client.Put(wait, "/p25/site-state", map[string]bool{"zoneConnected": c.zone.Load()})
	}
	if err == nil {
		err = c.synchronizeInhibition(wait, false)
	}
	cancel()
	if err != nil {
		return err
	}
	process.mu.Lock()
	process.zoneApplied, process.zoneState, process.lastSeen = true, c.zone.Load(), time.Now()
	process.mu.Unlock()
	c.mu.Lock()
	if !c.currentAttemptLocked(id, attempt) || c.children[id] != process {
		c.mu.Unlock()
		return context.Canceled
	}
	for i := range c.status.Channels {
		if c.status.Channels[i].ID == id {
			c.status.Channels[i].State, c.status.Channels[i].Ready = "ready", true
		}
	}
	failed = false
	c.emitLocked(Event{Kind: "channel_started", ChannelID: id, Message: "Voice channel started; the other RF channels were not restarted."})
	c.mu.Unlock()
	return nil
}
