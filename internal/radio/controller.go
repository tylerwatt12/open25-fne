package radio

import (
	"bufio"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"os"
	"path/filepath"
	"slices"
	"sort"
	"strings"
	"sync"
	"sync/atomic"
	"syscall"
	"time"

	"open25.local/open25-fne/internal/nativearchive"
)

type Controller struct {
	opts              Options
	opMu              sync.Mutex
	inhibitionMu      sync.Mutex
	mu                sync.Mutex
	lock              *os.File
	secret            string
	profile           Profile
	configuredProfile Profile
	network           Network
	status            Status
	children          map[string]*child
	fne               *child
	rootDir           string
	runID             string
	sequence          atomic.Uint64
	zone              atomic.Bool
	ctx               context.Context
	cancel            context.CancelFunc
	monitorCancel     context.CancelFunc
	monitorDone       chan struct{}
	events            chan Event
	registrations     map[int]Registration
	affiliations      map[int]Affiliation
	source            sourceFacts
	confirmations     map[confirmationKey]confirmation
	closing           bool
	runContext        context.Context
	runCancel         context.CancelFunc
	recovery          map[string]*channelRecovery
	siteWanted        bool
	siteRecovery      *siteAttempt
	retryWait         func(context.Context, time.Duration) error
	modem             modemOperations
	stopEpoch         uint64
	stopRequests      uint64
	inhibitDesired    []int
	inhibitConfigured bool
	archive           *nativearchive.Writer
	controlSwitch     *controlSwitchAttempt
	c4fm              *c4fmTest
	c4fmStatus        C4FMTestStatus
	c4fmEpoch         uint64
}

func New(opts Options) (*Controller, error) {
	if opts.StateDir == "" {
		return nil, errors.New("radio data directory is required")
	}
	if opts.StartupTimeout == 0 {
		opts.StartupTimeout = 45 * time.Second
	}
	if opts.StartupTimeout < time.Second || opts.StartupTimeout > 2*time.Minute {
		return nil, errors.New("native startup timeout must be 1-120 seconds")
	}
	if err := os.MkdirAll(opts.StateDir, 0700); err != nil {
		return nil, err
	}
	lock, err := os.OpenFile(filepath.Join(opts.StateDir, "radio.lock"), os.O_CREATE|os.O_RDWR, 0600)
	if err != nil {
		return nil, err
	}
	if err = syscall.Flock(int(lock.Fd()), syscall.LOCK_EX|syscall.LOCK_NB); err != nil {
		lock.Close()
		return nil, errors.New("another Site owns this radio data directory")
	}
	secretPath := filepath.Join(opts.StateDir, "media-secret")
	raw, err := os.ReadFile(secretPath)
	if errors.Is(err, os.ErrNotExist) {
		var secret string
		secret, err = newSecret()
		if err == nil {
			err = os.WriteFile(secretPath, []byte(secret), 0600)
			raw = []byte(secret)
		}
	}
	if err != nil || !validSecret(string(raw)) {
		lock.Close()
		return nil, errors.New("private media credential is unavailable or invalid")
	}
	info, err := os.Stat(secretPath)
	if err != nil || info.Mode().Perm()&0077 != 0 {
		lock.Close()
		return nil, errors.New("media credential must be private to its owner")
	}
	ctx, cancel := context.WithCancel(context.Background())
	_, engineErr := nativeExecutable(opts.EnginePath)
	_, mediaErr := nativeExecutable(opts.FNEPath)
	c := &Controller{opts: opts, lock: lock, secret: string(raw), ctx: ctx, cancel: cancel, children: map[string]*child{}, events: make(chan Event, 256), registrations: map[int]Registration{}, affiliations: map[int]Affiliation{}, confirmations: map[confirmationKey]confirmation{}, status: Status{Available: engineErr == nil && mediaErr == nil, State: "stopped", Channels: []ChannelStatus{}, Affiliations: []Affiliation{}, Registrations: []Registration{}, TrunkingMode: "stopped"}}
	c.archive = nativearchive.New()
	go c.publish()
	return c, nil
}

func (c *Controller) publish() {
	for {
		select {
		case <-c.ctx.Done():
			return
		case e := <-c.events:
			if c.opts.OnEvent != nil {
				func() { defer func() { _ = recover() }(); c.opts.OnEvent(e) }()
			}
		}
	}
}
func (c *Controller) emit(e Event) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.emitLocked(e)
}

// emitLocked preserves the same run ownership as the state change that created
// the event. Its bounded queue never invokes application callbacks under mu.
func (c *Controller) emitLocked(e Event) {
	if e.At.IsZero() {
		e.At = time.Now().UTC()
	}
	if e.SiteID == "" {
		e.SiteID = c.profile.Site.ID
	}
	run := c.runID
	if run == "" {
		run = "service"
	}
	e.ID = fmt.Sprintf("%s:%d", run, c.sequence.Add(1))
	if e.Audience == "" {
		e.Audience = "system_operator"
	}
	select {
	case c.events <- e:
	default:
	}
}

func (c *Controller) NetworkFor(p Profile) (Network, error) {
	identity, err := siteIdentity(p)
	if err != nil {
		return Network{}, err
	}
	n := p.Network
	if n.Address == "" {
		n.Address = "127.0.0.1"
	}
	if ip := net.ParseIP(n.Address); ip == nil || !ip.IsLoopback() {
		return Network{}, errors.New("Site media master must be local to the Site Controller")
	}
	if n.Port == 0 {
		n.Port = 60000 + identity.SiteID
		if c.opts.FNEPort != 0 {
			n.Port = c.opts.FNEPort
		}
	}
	if n.Secret == "" {
		n.Secret = c.secret
	}
	if !validSecret(n.Secret) || n.Port < 1024 || n.Port > 65535 {
		return Network{}, errors.New("local media assignment is invalid")
	}
	n.PeerBase = 100000000 + p.Zone.RFSSID*100000 + identity.SiteID*256
	n.ControlPeerID = n.PeerBase
	n.VoicePeerIDs = map[string]int{}
	n.ConsolePeerIDs = map[int]int{}
	voices := orderedChannels(p)
	index := 1
	for _, ch := range voices {
		if ch.Role == "voice" {
			n.VoicePeerIDs[ch.ID] = n.PeerBase + index
			index++
		}
	}
	index = 128
	for _, g := range allowedGroups(p) {
		if !g.Enabled {
			continue
		}
		if index > 255 {
			return Network{}, errors.New("at most 128 local codec talkgroups are supported")
		}
		n.ConsolePeerIDs[g.TGID] = n.PeerBase + index
		index++
	}
	return n, nil
}

func cloneNetwork(n Network) Network {
	copy := n
	copy.VoicePeerIDs = map[string]int{}
	for k, v := range n.VoicePeerIDs {
		copy.VoicePeerIDs[k] = v
	}
	copy.ConsolePeerIDs = map[int]int{}
	for k, v := range n.ConsolePeerIDs {
		copy.ConsolePeerIDs[k] = v
	}
	return copy
}
func (c *Controller) Network() Network {
	c.mu.Lock()
	defer c.mu.Unlock()
	return cloneNetwork(c.network)
}
func (c *Controller) Status() Status {
	c.mu.Lock()
	defer c.mu.Unlock()
	v := c.status
	v.MaintenanceSupported = c.opts.MaintenanceLockPath != ""
	v.Channels = append([]ChannelStatus{}, c.status.Channels...)
	for i := range v.Channels {
		row := &v.Channels[i]
		row.Telemetry = row.Telemetry.clone()
		for _, operating := range c.profile.Site.Channels {
			if operating.ID == row.ID {
				row.OperatingModemID = operating.ModemID
				if modem, ok := findModem(c.profile, operating.ModemID); ok {
					row.OperatingModemName = modem.Name
				}
			}
		}
		for _, configured := range c.configuredProfile.Site.Channels {
			if configured.ID == row.ID {
				row.ConfiguredRole, row.ModemID, row.ControlEligible = configured.Role, configured.ModemID, configured.ControlEligible
				if modem, ok := findModem(c.configuredProfile, configured.ModemID); ok {
					row.Device = modem.Device
				}
			}
		}
		if recovery := c.recovery[row.ID]; recovery != nil {
			row.RecoveryWanted, row.RecoveryAt = recovery.wanted && row.State == "fault", recovery.next
		}
	}
	v.Inhibition = append([]InhibitState{}, c.status.Inhibition...)
	v.ZoneConnected = c.zone.Load()
	v.Registrations = []Registration{}
	v.Affiliations = []Affiliation{}
	for _, r := range c.registrations {
		v.Registrations = append(v.Registrations, r)
	}
	for _, a := range c.affiliations {
		v.Affiliations = append(v.Affiliations, a)
	}
	sort.Slice(v.Registrations, func(i, j int) bool { return v.Registrations[i].RID < v.Registrations[j].RID })
	sort.Slice(v.Affiliations, func(i, j int) bool { return v.Affiliations[i].RID < v.Affiliations[j].RID })
	if v.State == "running" {
		v.TrunkingMode = "site_trunking"
		if v.ZoneConnected {
			v.TrunkingMode = "wide_area"
		}
	} else if v.State == "starting" {
		v.TrunkingMode = "starting"
	} else {
		v.TrunkingMode = v.State
	}
	return v
}
func (c *Controller) Affiliations() []Affiliation { return c.Status().Affiliations }
func (c *Controller) HasRFInterest(tgid int) bool {
	c.mu.Lock()
	defer c.mu.Unlock()
	if !c.status.InhibitionReady {
		return false
	}
	for _, a := range c.affiliations {
		if a.TGID == tgid && !inhibitionBlocked(c.status.Inhibition, a.RID) && !slices.Contains(c.profile.Inhibition.Desired, a.RID) {
			for _, r := range c.profile.System.Radios {
				if r.RID == a.RID && r.Enabled && r.Kind == "subscriber" {
					for _, g := range c.profile.System.Talkgroups {
						if g.TGID == tgid && g.Enabled && groupAllowedAtSite(g, c.profile.Site.ID) && (g.AffiliationAccess == "" || g.AffiliationAccess == "all" || (g.AffiliationAccess == "selected" && slices.Contains(g.AffiliationRadioIDs, a.RID))) {
							return true
						}
					}
				}
			}
		}
	}
	return false
}

func (c *Controller) Start(ctx context.Context, p Profile, mode string) error {
	lease, err := c.startLease()
	if err != nil {
		return err
	}
	defer lease.Close()
	epoch, err := c.prepareSiteStart(ctx)
	if err != nil {
		return err
	}
	c.opMu.Lock()
	defer c.opMu.Unlock()
	return c.startAtEpoch(ctx, p, mode, nil, epoch)
}

func (c *Controller) prepareSiteStart(ctx context.Context) (uint64, error) {
	if err := ctx.Err(); err != nil {
		return 0, err
	}
	c.mu.Lock()
	if c.c4fm != nil {
		c.mu.Unlock()
		return 0, errors.New("stop the C4FM test before starting the Site")
	}
	stopping := c.stopRequests != 0 || c.closing
	if !stopping && c.siteRecovery != nil {
		// An explicit Start supersedes the failed run's settings, including a
		// recovery that is currently waiting for native/modem readiness.
		c.stopEpoch++
		c.withdrawSiteLocked()
		c.cancelRunStartsLocked()
	}
	epoch := c.stopEpoch
	c.mu.Unlock()
	if stopping {
		return 0, context.Canceled
	}
	return epoch, nil
}
func (c *Controller) start(ctx context.Context, p Profile, mode string) error {
	lease, err := c.startLease()
	if err != nil {
		return err
	}
	defer lease.Close()
	epoch, err := c.prepareSiteStart(ctx)
	if err != nil {
		return err
	}
	return c.startAtEpoch(ctx, p, mode, nil, epoch)
}
func (c *Controller) startAtEpoch(ctx context.Context, p Profile, mode string, voiceIntent map[string]bool, epoch uint64) (err error) {
	return c.startSite(ctx, p, mode, voiceIntent, epoch, nil)
}

// Explicit startup remains strict. Only recovery of an already desired Site
// may leave an unavailable voice resource faulted for its own retry loop.
func (c *Controller) startSite(ctx context.Context, p Profile, mode string, voiceIntent map[string]bool, epoch uint64, recovering *siteAttempt) (err error) {
	controlID := ""
	if recovering != nil {
		controlID = recovering.controlID
	}
	return c.startSiteOnControl(ctx, p, mode, voiceIntent, epoch, recovering, controlID)
}

func (c *Controller) startSiteOnControl(ctx context.Context, p Profile, mode string, voiceIntent map[string]bool, epoch uint64, recovering *siteAttempt, controlID string) (err error) {
	lease, err := c.startLease()
	if err != nil {
		c.mu.Lock()
		if errors.Is(err, ErrMaintenance) && recovering != nil && c.currentSiteAttemptLocked(recovering) {
			c.withdrawSiteLocked()
			c.cancelRunStartsLocked()
			c.status.Error = err.Error()
			c.emitLocked(Event{Kind: "site_recovery_cancelled", Message: err.Error()})
		}
		c.mu.Unlock()
		return err
	}
	defer lease.Close()
	if err := ctx.Err(); err != nil {
		return err
	}
	p = cloneProfile(p)
	configured := cloneProfile(p)
	if controlID != "" {
		p = promotedProfile(p, controlID)
	}
	slices.Sort(p.Inhibition.Desired)
	c.mu.Lock()
	if c.inhibitConfigured {
		p.Inhibition.Desired = slices.Clone(c.inhibitDesired)
	}
	// Current observations supersede the caller's older recovery cache.
	if c.profile.System.ID != "" {
		p.Inhibition.Restore = inhibitionRestore(c.status.Inhibition)
	}
	c.mu.Unlock()
	if mode != "hardware" && mode != "null" {
		return errors.New("choose hardware or null radio operation")
	}
	if mode == "hardware" && !c.opts.AllowRF {
		return errors.New("RF operation is not enabled for this Site service")
	}
	if policy := p.Site.ControlPolicy.Mode; policy != "" && policy != "static" && policy != "dynamic" {
		return errors.New("the configured control assignment policy is not implemented")
	}
	if _, err := nativeExecutable(c.opts.EnginePath); err != nil {
		return err
	}
	if _, err := nativeExecutable(c.opts.FNEPath); err != nil {
		return err
	}
	if err := Validate(p); err != nil {
		return err
	}
	c.mu.Lock()
	if !c.inhibitConfigured {
		c.inhibitDesired, c.inhibitConfigured = slices.Clone(p.Inhibition.Desired), true
	}
	active := c.status.State == "running" || c.status.State == "starting" || c.status.State == "stopping"
	reserved := c.c4fm != nil
	closed := c.closing
	interrupted := c.stopEpoch != epoch || c.stopRequests != 0
	resources := len(c.children) > 0 || c.fne != nil
	c.mu.Unlock()
	if reserved {
		return errors.New("stop the C4FM test before replacing the Site run")
	}
	if active {
		return errors.New("Site is already operating")
	}
	if closed {
		return errors.New("radio service is closed")
	}
	if interrupted {
		return context.Canceled
	}
	if resources {
		if err := c.stop(ctx, false); err != nil {
			return err
		}
	}
	n, err := c.NetworkFor(p)
	if err != nil {
		return err
	}
	n.RPCSecret, err = newSecret()
	if err != nil {
		return err
	}
	run, err := newSecret()
	if err != nil {
		return err
	}
	run = run[:16]
	runContext, runCancel := context.WithCancel(c.ctx)
	c.mu.Lock()
	if c.closing || c.stopEpoch != epoch || c.stopRequests != 0 {
		c.mu.Unlock()
		runCancel()
		return context.Canceled
	}
	c.runContext, c.runCancel = runContext, runCancel
	c.mu.Unlock()
	operation, operationCancel := context.WithCancel(ctx)
	stopRun := context.AfterFunc(runContext, operationCancel)
	defer operationCancel()
	defer stopRun()
	ctx = operation
	startedRun := false
	defer func() {
		if !startedRun {
			runCancel()
		}
	}()
	wanted := func(ch Channel) bool {
		return ch.Role != "voice" || voiceIntent == nil || voiceIntent[ch.ID]
	}
	identity, _ := siteIdentity(p)
	base := c.opts.BaseRPCPort
	if base == 0 {
		base = 18000 + identity.SiteID*32
	}
	channels := orderedChannels(p)
	if base < 1024 || base+2*len(channels) > 65535 {
		return errors.New("native control port range is invalid")
	}
	leases := map[string]io.Closer{}
	unavailable := map[string]error{}
	if mode == "hardware" {
		resolved := map[string]bool{}
		for _, ch := range channels {
			if !wanted(ch) {
				continue
			}
			m, _ := findModem(p, ch.ModemID)
			if err := ctx.Err(); err != nil {
				return errors.Join(err, c.releaseStartupLeases(leases, p))
			}
			path, err := c.resolveModem(m.Device)
			if err == nil && resolved[path] {
				return errors.Join(errors.New("two RF channels resolve to the same modem"), c.releaseStartupLeases(leases, p))
			}
			if err == nil {
				resolved[path] = true
				var lease io.Closer
				lease, err = c.acquireModem(ctx, m.Device)
				if lease != nil {
					leases[ch.ID] = lease
				}
				if err == nil {
					err = c.verifyModem(ctx, lease, m.Device, p.Site.Identification.Enabled && p.Site.Identification.Mode == "ham")
				}
			}
			if err != nil {
				if recovering != nil && ch.Role == "voice" && ctx.Err() == nil {
					if lease := leases[ch.ID]; lease != nil {
						delete(leases, ch.ID)
						if cleanupErr := c.releaseStartupLeases(map[string]io.Closer{ch.ID: lease}, p); cleanupErr != nil {
							return errors.Join(err, cleanupErr, c.releaseStartupLeases(leases, p))
						}
					}
					unavailable[ch.ID] = err
					continue
				}
				return errors.Join(err, c.releaseStartupLeases(leases, p))
			}
		}
	}
	willStart := func(ch Channel) bool { return wanted(ch) && unavailable[ch.ID] == nil }
	dir := filepath.Join(c.opts.StateDir, "run-"+run)
	if err = os.MkdirAll(dir, 0700); err != nil {
		return errors.Join(err, c.releaseStartupLeases(leases, p))
	}
	// Persist all startup protection before any RF owner is launched. In
	// particular, a later desired=false cache write must not lose a desired
	// inhibit that this run has already established.
	if c.opts.OnInhibition != nil {
		if err = c.opts.OnInhibition(inhibitionRestore(initialInhibition(p.Inhibition))); err != nil {
			return errors.Join(err, c.releaseStartupLeases(leases, p))
		}
	}
	c.mu.Lock()
	if ctx.Err() != nil || c.closing || c.stopEpoch != epoch || c.stopRequests != 0 {
		c.mu.Unlock()
		return errors.Join(context.Canceled, c.releaseStartupLeases(leases, p))
	}
	c.withdrawSourceFactsLocked()
	c.profile = p
	c.configuredProfile = configured
	c.refreshSourceScopeLocked()
	c.network = n
	c.rootDir = dir
	c.runID = run
	c.status.State = "starting"
	c.status.Mode = mode
	c.status.SiteID = p.Site.ID
	c.status.StartedAt = time.Now().UTC()
	c.status.ActiveControlChannelID = ""
	if c.controlSwitch == nil {
		c.status.ControlSwitchTarget, c.status.ControlSwitchReason, c.status.ControlSwitchError = "", "", ""
	}
	for _, channel := range channels {
		if channel.Role == "control" {
			c.status.ActiveControlChannelID = channel.ID
		}
	}
	c.status.NextControlRotation = time.Time{}
	c.status.Error = ""
	c.status.FNEPID = 0
	c.status.FNEReady = false
	c.status.InhibitionReady = false
	c.status.Inhibition = initialInhibition(p.Inhibition)
	c.status.Channels = []ChannelStatus{}
	c.children = map[string]*child{}
	c.registrations = map[int]Registration{}
	c.affiliations = map[int]Affiliation{}
	c.confirmations = map[confirmationKey]confirmation{}
	c.recovery = map[string]*channelRecovery{}
	for i, ch := range channels {
		state := "queued"
		if ch.Role == "voice" {
			c.recovery[ch.ID] = &channelRecovery{wanted: wanted(ch)}
			if !wanted(ch) {
				state = "stopped"
			} else if unavailable[ch.ID] != nil {
				state = "fault"
				c.recovery[ch.ID].next = time.Now().Add(recoveryDelay(0))
				c.recovery[ch.ID].failures = 1
			}
		}
		row := ChannelStatus{ID: ch.ID, Name: ch.Name, Role: ch.Role, State: state, TXHz: ch.TXHz, RXHz: ch.RXHz, PowerPercent: ch.PowerPercent, RPCPort: base + 2*i, RESTPort: base + 2*i + 1}
		if unavailable[ch.ID] != nil {
			row.Error = "Selected voice modem is unavailable: " + unavailable[ch.ID].Error()
		}
		c.status.Channels = append(c.status.Channels, row)
	}
	c.mu.Unlock()
	c.emit(Event{Kind: "site_starting", Message: "Starting the Site's local media master and RF channels."})
	c.reportUserAliasOmissions(p, nil)
	failed := true
	defer func() {
		if failed {
			stopCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
			if ctx.Err() != nil {
				cancel()
			}
			cleanupErr := c.stop(stopCtx, false)
			cancel()
			err = errors.Join(err, cleanupErr, c.releaseStartupLeases(leases, p))
		}
	}()
	files, err := RenderFNE(p, n)
	if err != nil {
		return c.startFailure(err)
	}
	fneDir := filepath.Join(dir, "media")
	if err = writeFiles(fneDir, files); err != nil {
		return c.startFailure(err)
	}
	c.mu.Lock()
	if ctx.Err() != nil || c.closing || c.stopEpoch != epoch || c.stopRequests != 0 {
		c.mu.Unlock()
		return context.Canceled
	}
	fne, err := launch(c.opts.FNEPath, fneDir, "fne.yml", "media", "media", nil, c.observe)
	if err != nil {
		c.mu.Unlock()
		return c.startFailure(err)
	}
	c.fne = fne
	c.status.FNEPID = fne.cmd.Process.Pid
	c.mu.Unlock()
	wait, cancel := context.WithTimeout(ctx, c.opts.StartupTimeout)
	err = fne.waitReady(wait)
	cancel()
	if err != nil {
		return c.startFailure(err)
	}
	c.mu.Lock()
	c.status.FNEReady = true
	c.mu.Unlock()
	// Ready voice endpoints exist before a control channel can advertise them.
	for pass := 0; pass < 2; pass++ {
		for i, ch := range channels {
			if !willStart(ch) || (pass == 0 && ch.Role == "control") || (pass == 1 && ch.Role != "control") {
				continue
			}
			files, err = RenderChannel(p, ch, n, base+2*i, base+2*i+1, mode)
			if err != nil {
				return c.startFailure(err)
			}
			channelDir := filepath.Join(dir, ch.ID)
			if err = writeFiles(channelDir, files); err != nil {
				return c.startFailure(err)
			}
			client, err := NewNativeClient(net.JoinHostPort("127.0.0.1", fmt.Sprint(base+2*i+1)), n.RPCSecret)
			if err != nil {
				return c.startFailure(err)
			}
			c.mu.Lock()
			if ctx.Err() != nil || c.closing || c.stopEpoch != epoch || c.stopRequests != 0 {
				c.mu.Unlock()
				return context.Canceled
			}
			child, err := launch(c.opts.EnginePath, channelDir, "config.yml", ch.ID, ch.Role, client, c.observe)
			if err != nil {
				c.mu.Unlock()
				return c.startFailure(err)
			}
			child.lease = leases[ch.ID]
			modem, _ := findModem(p, ch.ModemID)
			child.device = modem.Device
			child.sameDevice = c.modem.same
			delete(leases, ch.ID)
			c.children[ch.ID] = child
			if ch.Role == "control" {
				c.adoptSourceOwnerLocked(child)
			}
			c.status.Channels[i].State = "starting"
			c.status.Channels[i].PID = child.cmd.Process.Pid
			c.status.Channels[i].StartedAt = child.started
			c.mu.Unlock()
		}
		for _, ch := range channels {
			if !willStart(ch) || (pass == 0 && ch.Role == "control") || (pass == 1 && ch.Role != "control") {
				continue
			}
			c.mu.Lock()
			child := c.children[ch.ID]
			c.mu.Unlock()
			wait, cancel := context.WithTimeout(ctx, c.opts.StartupTimeout)
			err = child.waitReady(wait)
			if err == nil {
				_, err = child.client.Get(wait, "/status")
			}
			if err == nil {
				_, err = child.client.Put(wait, "/p25/site-state", map[string]bool{"zoneConnected": c.zone.Load()})
				if err == nil {
					child.mu.Lock()
					child.zoneApplied = true
					child.zoneState = c.zone.Load()
					child.lastSeen = time.Now()
					child.mu.Unlock()
				}
			}
			cancel()
			if err != nil {
				if recovering != nil && ch.Role == "voice" && ctx.Err() == nil {
					if cleanupErr := c.retireUnavailableVoice(ctx, child, err, recovering); cleanupErr == nil {
						unavailable[ch.ID] = err
						continue
					} else {
						err = cleanupErr
					}
				}
				return c.startFailure(err)
			}
			c.mu.Lock()
			for i := range c.status.Channels {
				if c.status.Channels[i].ID == ch.ID {
					c.status.Channels[i].State = "ready"
					c.status.Channels[i].Ready = true
				}
			}
			c.mu.Unlock()
		}
	}
	if err = c.synchronizeInhibition(ctx, true); err != nil {
		return c.startFailure(err)
	}
	c.mu.Lock()
	if ctx.Err() != nil || c.closing || c.stopEpoch != epoch || c.stopRequests != 0 || (recovering != nil && c.siteRecovery != recovering) {
		c.mu.Unlock()
		return context.Canceled
	}
	c.status.State = "running"
	c.status.ControlChangedAt = time.Now().UTC()
	c.updateNextRotationLocked()
	c.siteWanted = true
	if recovering != nil {
		c.siteRecovery = nil
	}
	monitorCtx, monitorCancel := context.WithCancel(runContext)
	c.monitorCancel = monitorCancel
	c.monitorDone = make(chan struct{})
	done := c.monitorDone
	c.mu.Unlock()
	go c.monitor(monitorCtx, done)
	failed = false
	startedRun = true
	c.emit(Event{Kind: "site_started", Message: "Site radio operation started.", Direction: "state"})
	return nil
}

// Failed pre-launch lease release must retain a cleanup owner too; otherwise
// an unsuccessful startup could silently lose the only descriptor reference.
func (c *Controller) releaseStartupLeases(leases map[string]io.Closer, p Profile) error {
	var result error
	for id, lease := range leases {
		if err := lease.Close(); err != nil {
			result = errors.Join(result, err)
			done := make(chan struct{})
			close(done)
			role := "voice"
			for _, channel := range p.Site.Channels {
				if channel.ID == id {
					role = channel.Role
				}
			}
			c.mu.Lock()
			c.children[id] = &child{id: id, role: role, done: done, groupRetired: true, lease: lease, intentional: true}
			c.status.State = "fault"
			c.status.Error = "Modem ownership is retained after failed cleanup: " + err.Error()
			c.mu.Unlock()
		}
		delete(leases, id)
	}
	return result
}
func (c *Controller) startFailure(err error) error {
	c.mu.Lock()
	c.status.Error = err.Error()
	c.status.State = "fault"
	c.mu.Unlock()
	c.emit(Event{Kind: "site_fault", Message: "Site startup failed: " + err.Error()})
	return err
}
func (c *Controller) Stop(ctx context.Context) error {
	c.requestRunStop(false)
	c.opMu.Lock()
	defer c.opMu.Unlock()
	defer c.finishRunStop()
	return c.stop(ctx, true)
}

// StopRun withdraws only the native lifetime that created a failed caller
// resource. Qualification and cancellation are atomic with replacement/Stop;
// a delayed media failure can never stop a newer Site run.
func (c *Controller) StopRun(ctx context.Context, startedAt time.Time) error {
	c.mu.Lock()
	if c.status.State != "running" || !c.status.StartedAt.Equal(startedAt) {
		c.mu.Unlock()
		return nil
	}
	c.requestRunStopLocked(false)
	c.mu.Unlock()
	c.opMu.Lock()
	defer c.opMu.Unlock()
	defer c.finishRunStop()
	return c.stop(ctx, true)
}
func (c *Controller) stop(ctx context.Context, graceful bool) error {
	c.cancelRunStarts()
	testErr := c.finishC4FMTest(ctx, nil, nil)
	c.mu.Lock()
	wasFault := c.status.State == "fault"
	if c.monitorCancel != nil {
		c.monitorCancel()
		c.monitorCancel = nil
	}
	monitorDone := c.monitorDone
	c.monitorDone = nil
	children := make([]*child, 0, len(c.children))
	for _, child := range c.children {
		children = append(children, child)
	}
	fne := c.fne
	identification := c.profile.Site.Identification.Enabled && c.profile.Site.Identification.Mode == "ham" && c.status.Mode == "hardware" && graceful
	c.status.State = "stopping"
	c.withdrawSourceFactsLocked()
	c.status.InhibitionReady = false
	c.mu.Unlock()
	if monitorDone != nil {
		select {
		case <-monitorDone:
		case <-ctx.Done():
		}
	}
	sort.Slice(children, func(i, j int) bool { return children[i].role == "control" && children[j].role != "control" })
	var result error = testErr
	errorsCh := make(chan error, len(children))
	var stopping sync.WaitGroup
	for _, native := range children {
		stopping.Add(1)
		go func(process *child) { defer stopping.Done(); errorsCh <- process.stop(ctx, identification) }(native)
	}
	stopping.Wait()
	close(errorsCh)
	for err := range errorsCh {
		result = errors.Join(result, err)
	}
	if fne != nil {
		if err := fne.stop(ctx, false); err != nil {
			result = errors.Join(result, err)
		}
	}
	c.mu.Lock()
	if result == nil {
		c.children = map[string]*child{}
		c.fne = nil
		c.status.FNEPID = 0
		c.status.FNEReady = false
		c.status.ActiveControlChannelID = ""
		c.status.NextControlRotation = time.Time{}
		for i := range c.status.Channels {
			c.status.Channels[i].PID = 0
			c.status.Channels[i].State = "stopped"
			c.status.Channels[i].Ready = false
			c.status.Channels[i].Busy = false
			c.status.Channels[i].RID = 0
			c.status.Channels[i].TGID = 0
			if graceful {
				c.status.Channels[i].Error = ""
			}
		}
		c.registrations = map[int]Registration{}
		c.affiliations = map[int]Affiliation{}
		c.confirmations = map[confirmationKey]confirmation{}
		c.status.State = "stopped"
		if wasFault && !graceful {
			c.status.State = "fault"
		}
		if graceful {
			c.status.Error = ""
		}
	} else {
		c.status.State = "fault"
		c.status.Error = result.Error()
	}
	c.mu.Unlock()
	c.emit(Event{Kind: "site_stopped", Message: "Site radio operation stopped.", Direction: "state"})
	return result
}

func (c *Controller) Close() error {
	c.requestRunStop(true)
	c.opMu.Lock()
	defer c.opMu.Unlock()
	defer c.finishRunStop()
	c.mu.Lock()
	c.closing = true
	c.mu.Unlock()
	ctx, cancel := context.WithTimeout(context.Background(), 12*time.Second)
	defer cancel()
	err := c.stop(ctx, true)
	if err != nil {
		return err
	}
	c.cancel()
	if c.lock != nil {
		_ = syscall.Flock(int(c.lock.Fd()), syscall.LOCK_UN)
		_ = c.lock.Close()
		c.lock = nil
	}
	if c.archive != nil {
		// Diagnostic-copy outcome is separate from confirmed native/radio shutdown.
		_ = c.archive.Close(ctx)
	}
	return nil
}

func (c *Controller) DiagnosticArchiveStatus() nativearchive.Status {
	if c.archive == nil {
		return nativearchive.Status{}
	}
	return c.archive.Status()
}
func (c *Controller) SetZoneConnected(connected bool) { c.zone.Store(connected) }
func (c *Controller) ControlClient() (*NativeClient, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.status.State != "running" || c.runContext == nil || c.runContext.Err() != nil || !c.status.InhibitionReady {
		return nil, errors.New("control channel is not ready")
	}
	for _, child := range c.children {
		if child.role == "control" && child.alive() && child.readyNow() {
			return child.client, nil
		}
	}
	return nil, errors.New("control channel is not ready")
}
func (c *Controller) ChannelClient(id string) (*NativeClient, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.status.State != "running" || c.runContext == nil || c.runContext.Err() != nil || !c.status.InhibitionReady {
		return nil, errors.New("RF channel is not ready")
	}
	if child := c.children[id]; child != nil && child.alive() && child.readyNow() {
		return child.client, nil
	}
	return nil, errors.New("RF channel is not ready")
}

func (c *Controller) ApplyPolicy(ctx context.Context, p Profile) error {
	lease, err := c.startLease()
	if err != nil {
		return err
	}
	defer lease.Close()
	p = cloneProfile(p)
	configured := cloneProfile(p)
	c.mu.Lock()
	if c.status.ActiveControlChannelID != "" {
		p = promotedProfile(p, c.status.ActiveControlChannelID)
	}
	if c.controlSwitch != nil && recoverySettingsDigest(c.configuredProfile) != recoverySettingsDigest(configured) {
		c.controlSwitch.cancel()
	}
	c.mu.Unlock()
	validationErr := Validate(p)
	// A saved configuration must be able to interrupt recovery's warmup
	// before waiting for opMu. A fresh explicit Start selects the new snapshot.
	c.mu.Lock()
	recoveryCancelled := c.siteRecovery != nil && (validationErr != nil || recoverySettingsDigest(c.configuredProfile) != recoverySettingsDigest(configured))
	if recoveryCancelled {
		c.stopEpoch++
		c.withdrawSiteLocked()
		c.cancelRunStartsLocked()
	}
	epoch := c.stopEpoch
	c.mu.Unlock()
	c.opMu.Lock()
	defer c.opMu.Unlock()
	c.mu.Lock()
	old := c.profile
	running := c.status.State == "running"
	mode := c.status.Mode
	c.mu.Unlock()
	if recoveryCancelled {
		// The cancelled worker may not have acquired opMu for retirement yet.
		// This caller must retire any remaining owners before returning.
		if err := c.stop(ctx, false); err != nil {
			return errors.Join(validationErr, err)
		}
		running = false
	}
	if validationErr != nil {
		if running && PolicySignature(old) != PolicySignature(p) {
			c.mu.Lock()
			c.withdrawSiteLocked()
			c.mu.Unlock()
			c.emit(Event{Kind: "policy_withdrawn", Message: "RF stopped because the updated radio or talkgroup policy cannot be applied."})
			if err := c.stop(ctx, false); err != nil {
				return errors.Join(validationErr, err)
			}
		}
		return validationErr
	}
	if !running {
		if recoveryCancelled {
			c.mu.Lock()
			c.status.Error = "Automatic recovery cancelled because Site settings changed; start the Site to apply the current settings."
			c.emitLocked(Event{Kind: "site_recovery_cancelled", Message: c.status.Error})
			c.mu.Unlock()
		} else {
			c.mu.Lock()
			// Metadata-only directory updates are accepted even while no native
			// owner is running. Keep desired retries current without applying
			// changed RF/admission settings or starting another operation.
			if recoverySettingsDigest(c.configuredProfile) == recoverySettingsDigest(configured) {
				c.profile.System = configured.System
				c.configuredProfile.System = configured.System
				if c.siteRecovery != nil {
					c.siteRecovery.profile.System = configured.System
				}
				if c.controlSwitch != nil {
					c.controlSwitch.profile.System = configured.System
				}
			}
			c.mu.Unlock()
		}
		return nil
	}
	if physicalDigest(old) != physicalDigest(p) {
		c.mu.Lock()
		c.withdrawSiteLocked()
		c.mu.Unlock()
		if PolicySignature(old) != PolicySignature(p) {
			c.emit(Event{Kind: "policy_withdrawn", Message: "RF stopped because radio or talkgroup policy changed alongside pending RF settings."})
			if err := c.stop(ctx, false); err != nil {
				return err
			}
		}
		return errors.New("RF identity or channel settings changed; stop and start the Site to apply them")
	}
	if PolicySignature(old) == PolicySignature(p) {
		if err := c.applyUserAliases(ctx, old, p); err != nil {
			return err
		}
		c.mu.Lock()
		c.profile.System = p.System
		c.configuredProfile.System = configured.System
		c.refreshSourceScopeLocked()
		c.mu.Unlock()
		return nil
	}
	// Actual protection and the latest separately serialized Zone choice must
	// survive an unrelated provisioning restart.
	c.mu.Lock()
	p.Inhibition = c.profile.Inhibition
	c.mu.Unlock()
	c.emit(Event{Kind: "policy_applying", Message: "Applying radio and talkgroup policy; pausing RF before replacing the settings."})
	intent := c.voiceIntent()
	if err := c.stop(ctx, false); err != nil {
		return err
	}
	if err := c.startAtEpoch(ctx, p, mode, intent, epoch); err != nil {
		return err
	}
	c.mu.Lock()
	c.configuredProfile = configured
	c.mu.Unlock()
	c.emit(Event{Kind: "policy_applied", Message: "Radio and talkgroup policy applied."})
	return nil
}

// The operating configuration is a snapshot owned by this Controller. Callers
// may edit their form/configuration value after Start returns; those edits must
// not silently change the policy used to detect revocations or physical edits.
func cloneProfile(p Profile) Profile {
	p.System.Radios = append([]Radio(nil), p.System.Radios...)
	p.System.Talkgroups = append([]Talkgroup(nil), p.System.Talkgroups...)
	for i := range p.System.Talkgroups {
		p.System.Talkgroups[i].SiteIDs = append([]string(nil), p.System.Talkgroups[i].SiteIDs...)
		p.System.Talkgroups[i].AffiliationRadioIDs = append([]int(nil), p.System.Talkgroups[i].AffiliationRadioIDs...)
		p.System.Talkgroups[i].TransmitRadioIDs = append([]int(nil), p.System.Talkgroups[i].TransmitRadioIDs...)
	}
	p.Zone.ChannelIdentifiers = append([]Identifier(nil), p.Zone.ChannelIdentifiers...)
	p.Zone.Sites = append([]SiteIdentity(nil), p.Zone.Sites...)
	p.Site.Modems = append([]Modem(nil), p.Site.Modems...)
	p.Site.Channels = append([]Channel(nil), p.Site.Channels...)
	p.Network = cloneNetwork(p.Network)
	p.Inhibition.Desired = slices.Clone(p.Inhibition.Desired)
	p.Inhibition.Restore = slices.Clone(p.Inhibition.Restore)
	return p
}

// PolicySignature covers native call/admission rules whose replacement needs
// live grants withdrawn first. Display names never cause an RF restart.
func PolicySignature(p Profile) string {
	type radioRule struct {
		ID      int
		Enabled bool
		Kind    string
	}
	type groupRule struct {
		ID                                    int
		Enabled, SiteAllowed                  bool
		StartPolicy                           string
		Priority                              int
		AdmissionMode                         string
		AffiliationAccess, TransmitAccess     string
		AffiliationRadioIDs, TransmitRadioIDs []int
	}
	value := struct {
		Radios []radioRule
		Groups []groupRule
	}{}
	for _, r := range p.System.Radios {
		value.Radios = append(value.Radios, radioRule{r.RID, r.Enabled, r.Kind})
	}
	for _, g := range p.System.Talkgroups {
		start, mode, priority := g.StartPolicy, g.AdmissionMode, g.Priority
		if start == "" {
			start = "fast_start"
		}
		if priority < 2 || priority > 10 {
			priority = 10
		}
		affiliation, transmit := g.AffiliationAccess, g.TransmitAccess
		if affiliation == "" {
			affiliation = "all"
		}
		if transmit == "" {
			transmit = "all"
		}
		affiliationIDs, transmitIDs := []int{}, []int{}
		if affiliation == "selected" {
			affiliationIDs = append(affiliationIDs, g.AffiliationRadioIDs...)
			slices.Sort(affiliationIDs)
		}
		if transmit == "selected" {
			transmitIDs = append(transmitIDs, g.TransmitRadioIDs...)
			slices.Sort(transmitIDs)
		}
		value.Groups = append(value.Groups, groupRule{g.TGID, g.Enabled, groupAllowedAtSite(g, p.Site.ID), start, priority, mode, affiliation, transmit, affiliationIDs, transmitIDs})
	}
	sort.Slice(value.Radios, func(i, j int) bool { return value.Radios[i].ID < value.Radios[j].ID })
	sort.Slice(value.Groups, func(i, j int) bool { return value.Groups[i].ID < value.Groups[j].ID })
	raw, _ := json.Marshal(value)
	sum := sha256.Sum256(raw)
	return hex.EncodeToString(sum[:])
}

func physicalDigest(p Profile) string {
	p.System.Radios = nil
	p.System.Talkgroups = nil
	p.System.Name = ""
	p.Zone.Name = ""
	p.Network = Network{}
	p.Inhibition = InhibitPolicy{}
	identity, _ := siteIdentity(p)
	identity.Name = ""
	identity.Endpoint = ""
	p.Zone.Sites = []SiteIdentity{identity}
	raw, _ := json.Marshal(p)
	sum := sha256.Sum256(raw)
	return hex.EncodeToString(sum[:])
}

func recoverySettingsDigest(p Profile) string {
	network, _ := json.Marshal(p.Network)
	return physicalDigest(p) + PolicySignature(p) + string(network)
}

func collectLines(reader io.Reader, callback func(string)) {
	scan := bufio.NewScanner(reader)
	scan.Buffer(make([]byte, 4096), 64<<10)
	for scan.Scan() {
		text := strings.TrimSpace(scan.Text())
		if text != "" {
			callback(text)
		}
	}
}
