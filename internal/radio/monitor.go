package radio

import (
	"context"
	"encoding/json"
	"errors"
	"path/filepath"
	"strings"
	"time"

	"open25.local/open25-fne/internal/nativearchive"
)

type confirmationKey struct {
	Kind string
	RID  int
}
type confirmation struct {
	TGID int
	At   time.Time
}

func (c *Controller) observe(source *child, line string) {
	line = nativearchive.Redact(line, c.secret)
	id := source.id
	// Every reader writes only to its own run, even if a filesystem write or
	// callback finishes after that process has been stopped and replaced.
	dir := ""
	if source.dir != "" {
		dir = filepath.Dir(source.dir)
	}
	if dir != "" && c.archive != nil {
		// Admission runs after synchronous observation, including stale-run early returns.
		// The captured path never follows a replacement run.
		defer c.archive.Copy(filepath.Join(dir, "native-"+id+".log"), line, true)
	}
	e := ParseEvent(line)
	c.mu.Lock()
	current := c.children[id] == source
	if source.role == "media" {
		current = c.fne == source
	}
	if !current || c.rootDir != dir {
		c.mu.Unlock()
		return
	}
	for i := range c.status.Channels {
		if c.status.Channels[i].ID == id {
			if source.acceptsObservations() && (c.status.State == "running" || c.status.State == "starting") {
				c.status.Channels[i].Telemetry.begin(source.started)
				c.status.Channels[i].Telemetry.observe(line, time.Now().UTC())
			}
			if state, ok := identificationLine(line); ok {
				c.status.Channels[i].IdentificationState = state
				c.status.Channels[i].Identifying = state == "identifying" || state == "restoring"
			}
			if strings.Contains(line, "CW, identification failed") {
				c.status.Channels[i].Error = "Station identification failed."
			}
		}
	}
	if e == nil || !source.acceptsObservations() || (c.status.State != "running" && c.status.State != "starting") {
		c.mu.Unlock()
		return
	}
	e.SiteID = c.profile.Site.ID
	e.ChannelID = id
	if e.Kind == "uninhibit_ack" && e.RequestorID == NativeFNEID && source.role == "control" {
		source.mu.Lock()
		if !source.inhibitionInitialized {
			if source.inhibitionClears == nil {
				source.inhibitionClears = map[int]bool{}
			}
			source.inhibitionClears[e.RID] = true
		}
		source.mu.Unlock()
	}
	if source.role == "control" && (e.Kind == "registered" || e.Kind == "affiliated") {
		key := confirmationKey{Kind: e.Kind, RID: e.RID}
		if strings.Contains(line, "(HOST) P25 Affiliation,") {
			// A first accepted RF response is followed by a table-change log.
			// Count that transaction once, while retaining table-only changes.
			if previous, ok := c.confirmations[key]; ok {
				delete(c.confirmations, key)
				age := e.At.Sub(previous.At)
				if previous.TGID == e.TGID && age >= 0 && age < 250*time.Millisecond {
					c.mu.Unlock()
					return
				}
			}
		} else if strings.Contains(line, "(RF)") {
			c.confirmations[key] = confirmation{TGID: e.TGID, At: e.At}
		}
	}
	c.observeSourceLocked(source, *e)
	switch e.Kind {
	case "registered":
		c.registrations[e.RID] = Registration{RID: e.RID, At: e.At}
	case "deregistered":
		delete(c.registrations, e.RID)
		delete(c.affiliations, e.RID)
	case "affiliated":
		c.affiliations[e.RID] = Affiliation{RID: e.RID, TGID: e.TGID, At: e.At}
	case "unaffiliated":
		delete(c.affiliations, e.RID)
	case "call_started":
		for i := range c.status.Channels {
			if c.status.Channels[i].ID == id {
				ch := &c.status.Channels[i]
				if ch.Busy && ch.RID == e.RID && ch.TGID == e.TGID {
					c.mu.Unlock()
					return
				}
				ch.Busy = true
				ch.RID = e.RID
				ch.TGID = e.TGID
			}
		}
	case "call_ended":
		for i := range c.status.Channels {
			if c.status.Channels[i].ID == id {
				ch := &c.status.Channels[i]
				if !ch.Busy {
					c.mu.Unlock()
					return
				}
				e.RID = ch.RID
				e.TGID = ch.TGID
				ch.Busy = false
				ch.RID = 0
				ch.TGID = 0
			}
		}
	}
	c.emitLocked(*e)
	c.mu.Unlock()
}

func (c *Controller) monitor(ctx context.Context, done chan struct{}) {
	defer close(done)
	tick := time.NewTicker(500 * time.Millisecond)
	defer tick.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-tick.C:
		}
		c.mu.Lock()
		children := make([]*child, 0, len(c.children))
		for _, child := range c.children {
			children = append(children, child)
		}
		fne, fneReady := c.fne, c.status.FNEReady
		c.mu.Unlock()
		for _, child := range children {
			if ctx.Err() != nil {
				return
			}
			child.mu.Lock()
			intentional := child.intentional
			child.mu.Unlock()
			if intentional {
				continue
			}
			if !child.alive() {
				c.nativeExited(child)
				continue
			}
			if !child.validDevice() {
				stop, cancel := context.WithTimeout(context.Background(), 3*time.Second)
				_ = child.stop(stop, false)
				cancel()
				child.mu.Lock()
				child.intentional = false
				child.mu.Unlock()
				c.nativeExited(child)
				continue
			}
			// Start/StartChannel own a bounded wait for native readiness. The
			// modem initialization and DSP warmup take longer than the normal
			// status watchdog, so that watchdog must not reap a starting child.
			if !child.readyNow() {
				continue
			}
			child.mu.Lock()
			zoneNeeded := !child.zoneApplied || child.zoneState != c.zone.Load()
			child.mu.Unlock()
			if child.client.DispatchActive() && !zoneNeeded {
				continue
			}
			request, cancel := context.WithTimeout(ctx, 600*time.Millisecond)
			data, err := child.client.Get(request, "/status")
			if err == nil && zoneNeeded {
				_, err = child.client.Put(request, "/p25/site-state", map[string]bool{"zoneConnected": c.zone.Load()})
				if err == nil {
					child.mu.Lock()
					child.zoneApplied = true
					child.zoneState = c.zone.Load()
					child.mu.Unlock()
				}
			}
			cancel()
			if ctx.Err() != nil {
				return
			}
			if err != nil {
				c.nativeStatusFailed(child, err)
				continue
			}
			child.mu.Lock()
			child.lastSeen = time.Now()
			child.mu.Unlock()
			c.applyNativeStatus(child, data)
			if child.role == "control" {
				if child.client.DispatchActive() {
					continue
				}
				request, cancel = context.WithTimeout(ctx, 350*time.Millisecond)
				policyErr := c.synchronizeInhibition(request, false)
				cancel()
				if policyErr != nil && child.client.DispatchActive() {
					continue
				}
				if policyErr != nil && !errors.Is(policyErr, context.Canceled) {
					c.mu.Lock()
					current := c.children[child.id] == child && c.status.State == "running"
					if current {
						c.status.InhibitionReady = false
						c.withdrawSiteLocked()
					}
					c.mu.Unlock()
					if current {
						c.emit(Event{Kind: "policy_withdrawn", Message: "RF withdrawn because current inhibit protection could not be confirmed: " + policyErr.Error()})
						c.controlFailed(child)
					}
					return
				}
				c.mu.Lock()
				currentSource := c.adoptSourceOwnerLocked(child)
				sourceRevision := c.source.revision
				c.mu.Unlock()
				if !currentSource {
					continue
				}
				request, cancel = context.WithTimeout(ctx, 500*time.Millisecond)
				aff, affErr := child.client.Get(request, "/p25/report-affiliations")
				cancel()
				if affErr == nil {
					c.applyAffiliations(child, aff, sourceRevision)
				}
			}
		}
		c.recoverVoiceChannels(time.Now())
		c.scheduleControlRotation(time.Now())
		if fne != nil && (!fne.alive() || !fneReady) {
			c.restartFNE(ctx, fne)
		}
	}
}

func (c *Controller) nativeExited(child *child) {
	child.mu.Lock()
	intentional := child.intentional
	child.mu.Unlock()
	if intentional {
		return
	}
	if child.role == "control" {
		c.controlFailed(child)
		return
	}
	c.mu.Lock()
	current := c.children[child.id] == child && (c.status.State == "running" || c.status.State == "starting")
	c.mu.Unlock()
	if !current {
		return
	}
	stop, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	cleanupErr := child.stop(stop, false)
	cancel()
	c.mu.Lock()
	if c.children[child.id] != child || (c.status.State != "running" && c.status.State != "starting") {
		c.mu.Unlock()
		return
	}
	first := false
	for i := range c.status.Channels {
		ch := &c.status.Channels[i]
		if ch.ID == child.id {
			first = ch.State != "fault"
			ch.State = "fault"
			ch.Ready = false
			ch.Busy = false
			ch.Error = "Native radio engine stopped."
			if cleanupErr == nil {
				ch.PID = 0
			} else {
				ch.Error = cleanupErr.Error()
			}
		}
	}
	if first && child.role == "voice" {
		if recovery := c.recovery[child.id]; recovery != nil && recovery.wanted && recovery.attempt == nil {
			recovery.next = time.Now().Add(recoveryDelay(0))
			recovery.failures = 1
		}
	}
	if first {
		c.emitLocked(Event{Kind: "channel_fault", ChannelID: child.id, Message: "Native radio engine stopped."})
	}
	c.mu.Unlock()
}

func (c *Controller) nativeStatusFailed(child *child, err error) {
	child.mu.Lock()
	intentional := child.intentional
	child.mu.Unlock()
	if intentional {
		return
	}
	child.mu.Lock()
	last := child.lastSeen
	child.mu.Unlock()
	if contact := child.client.LastContact(); contact.After(last) {
		last = contact
	}
	if last.IsZero() {
		last = child.started
	}
	if time.Since(last) < 5*time.Second {
		return
	}
	c.mu.Lock()
	if c.children[child.id] != child || (c.status.State != "running" && c.status.State != "starting") {
		c.mu.Unlock()
		return
	}
	for i := range c.status.Channels {
		if c.status.Channels[i].ID == child.id {
			c.status.Channels[i].Error = "Native status unavailable; channel will be stopped."
		}
	}
	c.mu.Unlock()
	stop, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	_ = child.stop(stop, false)
	cancel()
	child.mu.Lock()
	child.intentional = false
	child.mu.Unlock()
	c.nativeExited(child)
}

func (c *Controller) applyNativeStatus(child *child, data map[string]json.RawMessage) {
	child.mu.Lock()
	intentional := child.intentional
	admissionError := child.admissionError
	child.mu.Unlock()
	if intentional {
		return
	}
	var tx, identifying bool
	var rid, tg int
	_ = json.Unmarshal(data["tx"], &tx)
	_ = json.Unmarshal(data["isTxCW"], &identifying)
	_ = json.Unmarshal(data["lastSrcId"], &rid)
	_ = json.Unmarshal(data["lastDstId"], &tg)
	var ended *Event
	c.mu.Lock()
	if c.children[child.id] != child || !child.acceptsObservations() || (c.status.State != "running" && c.status.State != "starting") {
		c.mu.Unlock()
		return
	}
	for i := range c.status.Channels {
		ch := &c.status.Channels[i]
		if ch.ID != child.id {
			continue
		}
		if recovery := c.recovery[child.id]; recovery != nil && recovery.attempt != nil {
			continue // readiness belongs to the completing startup handshake
		}
		ch.Telemetry.begin(child.started)
		ch.Telemetry.sample(data, time.Now().UTC())
		ch.Ready = admissionError == ""
		ch.State, ch.Error = "ready", ""
		if admissionError != "" {
			ch.State, ch.Error = "fault", admissionError
		}
		ch.Identifying = identifying
		if child.role == "voice" {
			if ch.Busy && !tx {
				ended = &Event{Kind: "call_ended", Audience: "operator", ChannelID: child.id, RID: ch.RID, TGID: ch.TGID, Message: "Voice transmission ended.", Direction: "state"}
				ch.Busy = false
				ch.RID = 0
				ch.TGID = 0
			}
			if tx && !identifying {
				ch.Busy = true
				if rid > 0 {
					ch.RID = rid
				}
				if tg > 0 {
					ch.TGID = tg
				}
			}
		}
	}
	if ended != nil {
		c.emitLocked(*ended)
	}
	c.mu.Unlock()
}

func (c *Controller) applyAffiliations(source *child, data map[string]json.RawMessage, expectedRevision uint64) {
	var records []struct {
		RID  int `json:"srcId"`
		TGID int `json:"grpId"`
	}
	if json.Unmarshal(data["affiliations"], &records) != nil {
		return
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.children[source.id] != source || c.source.revision != expectedRevision || !source.acceptsObservations() || (c.status.State != "running" && c.status.State != "starting") {
		return
	}
	next := map[int]Affiliation{}
	now := time.Now().UTC()
	sourceRows := make([]Affiliation, 0, len(records))
	for _, row := range records {
		if row.RID < 1 || row.TGID < 1 {
			continue
		}
		at := now
		if old, ok := c.affiliations[row.RID]; ok && old.TGID == row.TGID {
			at = old.At
		}
		next[row.RID] = Affiliation{RID: row.RID, TGID: row.TGID, At: at}
		sourceRows = append(sourceRows, Affiliation{RID: row.RID, TGID: row.TGID})
	}
	c.sourceAffiliationsLocked(source, sourceRows, now)
	c.affiliations = next
}

// The monitor owns and joins this bounded attempt. Publish every launched
// replacement before readiness so even an unsuccessful retirement stays owned.
func (c *Controller) restartFNE(ctx context.Context, old *child) {
	c.mu.Lock()
	if ctx.Err() != nil || c.fne != old || c.status.State != "running" || c.runContext == nil || c.runContext.Err() != nil {
		c.mu.Unlock()
		return
	}
	run, dir := c.runID, old.dir
	first := c.status.FNEReady
	c.status.FNEReady = false
	c.status.Error = "Local audio routing is unavailable; RF engines remain supervised."
	if first {
		c.emitLocked(Event{Kind: "media_fault", Message: "The local media master stopped; attempting to restart it."})
	}
	c.mu.Unlock()
	stop, cancel := context.WithTimeout(ctx, 2*time.Second)
	err := old.stop(stop, false)
	cancel()
	if err != nil {
		c.fneReplacementFailed(old, run, err, false)
		return
	}
	// Native peers reconnect to the same local endpoint. Retirement must
	// succeed before anything can bind those ports again.
	select {
	case <-ctx.Done():
		return
	case <-time.After(time.Second):
	}
	c.mu.Lock()
	if ctx.Err() != nil || c.fne != old || c.runID != run || c.status.State != "running" || c.runContext.Err() != nil {
		c.mu.Unlock()
		return
	}
	// Keep launch and ownership publication together with respect to Stop's
	// snapshot. Output callbacks wait for mu and then see their actual owner.
	process, err := launch(c.opts.FNEPath, dir, "fne.yml", "media", "media", nil, c.observe)
	if err != nil {
		c.status.FNEPID = 0 // the old owner was successfully retired
		c.status.Error = "Local audio routing restart failed: " + err.Error()
		c.mu.Unlock()
		return
	}
	c.fne = process
	c.status.FNEPID = process.cmd.Process.Pid
	c.mu.Unlock()
	wait, cancel := context.WithTimeout(ctx, 5*time.Second)
	err = process.waitReady(wait)
	cancel()
	if err == nil {
		c.mu.Lock()
		if ctx.Err() == nil && c.fne == process && c.runID == run && c.status.State == "running" && c.runContext.Err() == nil {
			c.status.FNEReady = true
			c.status.Error = ""
			c.emitLocked(Event{Kind: "media_recovered", Message: "Local audio routing recovered."})
			c.mu.Unlock()
			return
		}
		c.mu.Unlock()
		err = context.Canceled
	}
	stop, cancel = context.WithTimeout(context.Background(), 3*time.Second)
	if ctx.Err() != nil {
		cancel() // Stop/Close need forced retirement, not another grace wait
	}
	cleanupErr := process.stop(stop, false)
	cancel()
	c.fneReplacementFailed(process, run, errors.Join(err, cleanupErr), cleanupErr == nil)
}

func (c *Controller) fneReplacementFailed(owner *child, run string, err error, retired bool) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.fne != owner || c.runID != run || c.status.State != "running" {
		return
	}
	c.status.FNEReady = false
	if retired {
		c.status.FNEPID = 0
	}
	c.status.Error = "Local audio routing restart failed: " + err.Error()
}

func (c *Controller) Command(ctx context.Context, command Command) (map[string]json.RawMessage, error) {
	if command.RID < 1 || command.RID > 0xFFFFFB {
		return nil, errors.New("choose an unreserved subscriber Radio ID")
	}
	actions := map[string]string{"radio_check": "check", "page": "page", "message": "message", "registration_demand": "unit-reg", "affiliation_query": "group-aff-req", "status_query": "status-query", "inhibit": "inhibit", "uninhibit": "uninhibit", "regroup": "dyn-regrp", "regroup_cancel": "dyn-regrp-cancel"}
	native, ok := actions[command.Action]
	if !ok {
		return nil, errors.New("radio command is not implemented")
	}
	c.mu.Lock()
	switching := c.controlSwitch != nil
	c.mu.Unlock()
	if switching {
		return nil, errors.New("wait for the pending control-channel switch before issuing a radio command")
	}
	if command.Action == "inhibit" || command.Action == "uninhibit" {
		return c.commandInhibition(ctx, command)
	}
	c.mu.Lock()
	mode := c.status.Mode
	running := c.status.State == "running"
	c.mu.Unlock()
	if !running || (mode != "hardware" && mode != "null") {
		return nil, errors.New("Site radio operation is not running")
	}
	client, err := c.ControlClient()
	if err != nil {
		return nil, err
	}
	payload := map[string]any{"command": native, "dstId": command.RID}
	if command.Action == "message" {
		if command.Code < 0 || command.Code > 65535 {
			return nil, errors.New("choose a message code from 0 to 65535")
		}
		payload["message"] = command.Code
	}
	if command.Action == "regroup" {
		if command.TGID < 1 || command.TGID > 65534 {
			return nil, errors.New("choose a valid regroup talkgroup")
		}
		payload["tgId"] = command.TGID
	}
	reply, err := client.Put(ctx, "/p25/rid", payload)
	if err == nil {
		c.emit(Event{Kind: "command_sent", Audience: "operator", RID: command.RID, TGID: command.TGID, Direction: "sent", Message: command.Action + " sent; awaiting a subscriber response."})
	}
	return reply, err
}
