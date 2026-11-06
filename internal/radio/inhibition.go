package radio

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"slices"
	"strings"
)

const InhibitRecordLimit = 32

// Desired is Zone intent. Restore is startup-only actual Site protection; it
// never represents an instruction to replay an operator command.
type InhibitPolicy struct {
	Desired []int            `json:"desired"`
	Restore []InhibitRestore `json:"restore"`
}
type InhibitRestore struct {
	RadioID          int  `json:"radio_id"`
	Blocked          bool `json:"blocked"`
	PendingUninhibit bool `json:"pending_uninhibit"`
}
type InhibitState struct {
	RadioID          int    `json:"radio_id"`
	DesiredInhibited bool   `json:"desired_inhibited"`
	Blocked          bool   `json:"blocked"`
	PendingUninhibit bool   `json:"pending_uninhibit"`
	Procedure        string `json:"procedure"`
	Attempts         int    `json:"attempts"`
}
type inhibitIntent struct {
	RadioID   int  `json:"radio_id"`
	Inhibited bool `json:"inhibited"`
}
type inhibitRequest struct {
	Radios       []inhibitIntent   `json:"radios"`
	ControlState *[]InhibitRestore `json:"control_state,omitempty"`
}

func ValidateInhibition(radios []Radio, p InhibitPolicy) error {
	physical := map[int]bool{}
	for _, r := range radios {
		if r.Kind == "subscriber" {
			physical[r.RID] = true
		}
	}
	if len(p.Desired) > InhibitRecordLimit || len(p.Restore) > InhibitRecordLimit {
		return errors.New("inhibit policy exceeds the 32-radio limit")
	}
	union, desired, restored := map[int]bool{}, map[int]bool{}, map[int]bool{}
	for _, rid := range p.Desired {
		if rid < 1 || rid > 0xFFFFFB || !physical[rid] || desired[rid] {
			return errors.New("inhibit policy must contain unique known physical Radio IDs")
		}
		desired[rid], union[rid] = true, true
	}
	for _, r := range p.Restore {
		if r.RadioID < 1 || r.RadioID > 0xFFFFFB || !physical[r.RadioID] || restored[r.RadioID] || !r.Blocked || (r.PendingUninhibit && desired[r.RadioID]) {
			return errors.New("inhibit recovery must contain unique known blocked physical Radio IDs and valid pending state")
		}
		restored[r.RadioID], union[r.RadioID] = true, true
	}
	if len(union) > InhibitRecordLimit {
		return errors.New("current inhibit protection exceeds the 32-radio limit")
	}
	return nil
}

func renderInhibition(p InhibitPolicy) string {
	var out strings.Builder
	out.WriteString("        inhibitPolicy:\n")
	if len(p.Desired) == 0 {
		out.WriteString("            desired: []\n")
	} else {
		out.WriteString("            desired:\n")
		for _, rid := range p.Desired {
			fmt.Fprintf(&out, "                - radio_id: %d\n                  inhibited: true\n", rid)
		}
	}
	if len(p.Restore) == 0 {
		out.WriteString("            restore: []\n")
	} else {
		out.WriteString("            restore:\n")
		for _, r := range p.Restore {
			fmt.Fprintf(&out, "                - radio_id: %d\n                  blocked: %t\n                  pending_uninhibit: %t\n", r.RadioID, r.Blocked, r.PendingUninhibit)
		}
	}
	return out.String()
}

func initialInhibition(p InhibitPolicy) []InhibitState {
	rows := map[int]InhibitState{}
	for _, r := range p.Restore {
		rows[r.RadioID] = InhibitState{RadioID: r.RadioID, Blocked: r.Blocked, PendingUninhibit: r.Blocked, Procedure: "idle"}
	}
	for _, rid := range p.Desired {
		rows[rid] = InhibitState{RadioID: rid, DesiredInhibited: true, Blocked: true, Procedure: "idle"}
	}
	out := make([]InhibitState, 0, len(rows))
	for _, r := range rows {
		out = append(out, r)
	}
	slices.SortFunc(out, func(a, b InhibitState) int { return a.RadioID - b.RadioID })
	return out
}
func inhibitionRestore(rows []InhibitState) []InhibitRestore {
	out := []InhibitRestore{}
	for _, r := range rows {
		if r.Blocked || r.PendingUninhibit {
			out = append(out, InhibitRestore{r.RadioID, r.Blocked, r.PendingUninhibit})
		}
	}
	return out
}
func inhibitionBlocked(rows []InhibitState, rid int) bool {
	return slices.ContainsFunc(rows, func(r InhibitState) bool {
		return r.RadioID == rid && (r.Blocked || r.PendingUninhibit || r.DesiredInhibited)
	})
}
func inhibitionRequest(desired []int) inhibitRequest {
	r := inhibitRequest{Radios: []inhibitIntent{}}
	for _, rid := range desired {
		r.Radios = append(r.Radios, inhibitIntent{rid, true})
	}
	return r
}

// Decode every safety-bearing field explicitly. Missing booleans must not
// silently turn into a false/clear observation.
func decodeInhibition(data map[string]json.RawMessage, p Profile) ([]InhibitState, error) {
	var count, limit int
	if json.Unmarshal(data["record_count"], &count) != nil || json.Unmarshal(data["record_limit"], &limit) != nil || limit != InhibitRecordLimit || count < 0 || count > limit {
		return nil, errors.New("native inhibit record bounds are invalid")
	}
	var raw []json.RawMessage
	if len(data["radios"]) == 0 || bytes.Equal(data["radios"], []byte("null")) || json.Unmarshal(data["radios"], &raw) != nil || len(raw) != count {
		return nil, errors.New("native inhibit snapshot is incomplete")
	}
	seen := map[int]bool{}
	rows := make([]InhibitState, 0, count)
	for _, value := range raw {
		var r struct {
			RadioID   *int    `json:"radio_id"`
			Desired   *bool   `json:"desired_inhibited"`
			Blocked   *bool   `json:"blocked"`
			Pending   *bool   `json:"pending_uninhibit"`
			Procedure *string `json:"procedure"`
			Attempts  *int    `json:"attempts"`
		}
		decoder := json.NewDecoder(bytes.NewReader(value))
		decoder.DisallowUnknownFields()
		if decoder.Decode(&r) != nil || r.RadioID == nil || r.Desired == nil || r.Blocked == nil || r.Pending == nil || r.Procedure == nil || r.Attempts == nil {
			return nil, errors.New("native inhibit record is incomplete")
		}
		rid := *r.RadioID
		if seen[rid] || !slices.ContainsFunc(p.System.Radios, func(v Radio) bool { return v.RID == rid && v.Kind == "subscriber" }) || *r.Desired != slices.Contains(p.Inhibition.Desired, rid) || (*r.Desired && (!*r.Blocked || *r.Pending)) || (*r.Pending && (!*r.Blocked || *r.Desired)) || (!*r.Desired && *r.Blocked && !*r.Pending) || *r.Attempts < 0 || *r.Attempts > 5 || !slices.Contains([]string{"idle", "queued", "awaiting", "acknowledged", "no_response", "cancelled"}, *r.Procedure) {
			return nil, errors.New("native inhibit state disagrees with current physical policy")
		}
		seen[rid] = true
		rows = append(rows, InhibitState{rid, *r.Desired, *r.Blocked, *r.Pending, *r.Procedure, *r.Attempts})
	}
	for _, rid := range p.Inhibition.Desired {
		if !seen[rid] {
			return nil, errors.New("native inhibit snapshot omitted current desired protection")
		}
	}
	slices.SortFunc(rows, func(a, b InhibitState) int { return a.RadioID - b.RadioID })
	return rows, nil
}

// ApplyInhibitionPolicy caches intent while stopped. It never starts RF or an
// operator command. A changed choice cancels a startup built from older intent.
func (c *Controller) ApplyInhibitionPolicy(ctx context.Context, desired []int) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	desired = slices.Clone(desired)
	slices.Sort(desired)
	if len(desired) > InhibitRecordLimit {
		return errors.New("inhibit policy exceeds the 32-radio limit")
	}
	for i, rid := range desired {
		if rid < 1 || rid > 0xFFFFFB || (i > 0 && desired[i-1] == rid) {
			return errors.New("inhibit policy must contain unique unreserved Radio IDs")
		}
	}
	c.mu.Lock()
	if c.closing || ctx.Err() != nil {
		c.mu.Unlock()
		return context.Canceled
	}
	changed := !slices.Equal(c.inhibitDesired, desired) || !c.inhibitConfigured
	c.inhibitDesired, c.inhibitConfigured = desired, true
	interrupted := changed && (c.status.State == "starting" || c.siteRecovery != nil || (c.status.State != "running" && c.runContext != nil && c.runContext.Err() == nil))
	if changed && c.status.State != "running" {
		c.stopEpoch++
		c.withdrawSiteLocked()
		c.cancelRunStartsLocked()
	}
	if changed {
		c.status.InhibitionReady = false
		for _, recovery := range c.recovery {
			if recovery.attempt != nil {
				recovery.attempt.cancel()
			}
		}
	}
	c.mu.Unlock()
	c.opMu.Lock()
	defer c.opMu.Unlock()
	if err := ctx.Err(); err != nil {
		c.mu.Lock()
		current := slices.Equal(desired, c.inhibitDesired) && c.status.State == "running" && !c.status.InhibitionReady
		c.mu.Unlock()
		if current {
			return errors.Join(err, c.withdrawInhibition(context.Background(), err))
		}
		return err
	}
	c.mu.Lock()
	if !slices.Equal(desired, c.inhibitDesired) || c.closing {
		c.mu.Unlock()
		return context.Canceled
	}
	p := cloneProfile(c.profile)
	p.Inhibition.Desired = slices.Clone(desired)
	p.Inhibition.Restore = inhibitionRestore(c.status.Inhibition)
	// A new inhibit choice supersedes an older pending re-enable flag.
	for i := range p.Inhibition.Restore {
		if slices.Contains(desired, p.Inhibition.Restore[i].RadioID) {
			p.Inhibition.Restore[i].PendingUninhibit = false
		}
	}
	err := error(nil)
	if p.System.ID != "" {
		err = ValidateInhibition(p.System.Radios, p.Inhibition)
	}
	running := c.status.State == "running"
	alreadyApplied := !changed && c.status.InhibitionReady
	if err == nil {
		c.profile.Inhibition = p.Inhibition
	}
	c.mu.Unlock()
	if interrupted || err != nil {
		return errors.Join(err, c.stop(ctx, false))
	}
	if !running || alreadyApplied {
		return nil
	}
	if err = c.synchronizeInhibition(ctx, true); err != nil {
		c.mu.Lock()
		current := slices.Equal(c.inhibitDesired, desired)
		c.mu.Unlock()
		if !current {
			return err
		}
		return errors.Join(err, c.withdrawInhibition(ctx, err))
	}
	return nil
}

// Caller holds inhibitionMu while taking and publishing a complete current-CC
// snapshot. Every reply is checked against the run and exact child pointers.
func (c *Controller) synchronizeInhibition(ctx context.Context, apply bool) error {
	c.inhibitionMu.Lock()
	defer c.inhibitionMu.Unlock()
	c.mu.Lock()
	if (c.status.State != "running" && c.status.State != "starting") || c.runContext == nil || c.runContext.Err() != nil {
		c.mu.Unlock()
		return context.Canceled
	}
	run, runCtx, p := c.runID, c.runContext, cloneProfile(c.profile)
	if c.inhibitConfigured && !slices.Equal(c.inhibitDesired, p.Inhibition.Desired) {
		c.mu.Unlock()
		return context.Canceled
	}
	var control *child
	voices := []*child{}
	for _, v := range c.children {
		if !v.readyNow() || !v.acceptsObservations() {
			continue
		}
		if v.role == "control" {
			control = v
		} else if v.role == "voice" {
			voices = append(voices, v)
		}
	}
	c.mu.Unlock()
	if control == nil {
		return errors.New("inhibit policy requires the current control owner")
	}
	operation, cancel := context.WithCancel(ctx)
	stop := context.AfterFunc(runCtx, cancel)
	defer cancel()
	defer stop()
	current := func() bool {
		c.mu.Lock()
		defer c.mu.Unlock()
		return operation.Err() == nil && c.runID == run && c.children[control.id] == control && c.runContext == runCtx && slices.Equal(c.profile.Inhibition.Desired, p.Inhibition.Desired) && (!c.inhibitConfigured || slices.Equal(c.inhibitDesired, p.Inhibition.Desired)) && (c.status.State == "running" || c.status.State == "starting") && control.acceptsObservations()
	}
	if !current() {
		return context.Canceled
	}
	request := inhibitionRequest(p.Inhibition.Desired)
	if apply {
		if _, err := control.client.Put(operation, "/p25/inhibit-policy", request); err != nil {
			return err
		}
		if !current() {
			return context.Canceled
		}
	}
	data, err := control.client.Get(operation, "/p25/inhibit-policy")
	if err != nil {
		return err
	}
	if !current() {
		return context.Canceled
	}
	rows, err := decodeInhibition(data, p)
	if err != nil {
		return err
	}
	control.mu.Lock()
	for _, r := range p.Inhibition.Restore {
		if !control.inhibitionInitialized && r.Blocked && !inhibitionBlocked(rows, r.RadioID) && !control.inhibitionClears[r.RadioID] {
			control.mu.Unlock()
			return errors.New("new native control owner omitted restored inhibition without a current re-enable acknowledgment")
		}
	}
	control.mu.Unlock()
	restore := inhibitionRestore(rows)
	for _, v := range voices {
		if !current() {
			return context.Canceled
		}
		c.mu.Lock()
		valid := c.children[v.id] == v
		c.mu.Unlock()
		if !valid {
			return context.Canceled
		}
		v.mu.Lock()
		mirrored := slices.Equal(v.inhibitionMirror, restore) && slices.Equal(v.inhibitionDesired, p.Inhibition.Desired) && v.inhibitionApplied
		v.mu.Unlock()
		if !mirrored || apply {
			request.ControlState = &restore
			var voiceData map[string]json.RawMessage
			if voiceData, err = v.client.Put(operation, "/p25/inhibit-policy", request); err != nil {
				return err
			}
			voiceRows, voiceErr := decodeInhibition(voiceData, p)
			if voiceErr != nil || !slices.Equal(inhibitionRestore(voiceRows), restore) {
				return errors.New("voice owner did not confirm the complete current control inhibit state")
			}
			if !current() {
				return context.Canceled
			}
			c.mu.Lock()
			valid = c.children[v.id] == v
			c.mu.Unlock()
			if !valid {
				return context.Canceled
			}
			v.mu.Lock()
			v.inhibitionMirror = slices.Clone(restore)
			v.inhibitionDesired = slices.Clone(p.Inhibition.Desired)
			v.inhibitionApplied = true
			v.mu.Unlock()
		}
	}
	if !current() {
		return context.Canceled
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	if operation.Err() != nil || c.runID != run || c.children[control.id] != control || !slices.Equal(c.profile.Inhibition.Desired, p.Inhibition.Desired) || (c.inhibitConfigured && !slices.Equal(c.inhibitDesired, p.Inhibition.Desired)) {
		return context.Canceled
	}
	if c.opts.OnInhibition != nil && !slices.Equal(inhibitionRestore(c.status.Inhibition), restore) {
		if err = c.opts.OnInhibition(slices.Clone(restore)); err != nil {
			return fmt.Errorf("save actual inhibit protection: %w", err)
		}
	}
	c.status.Inhibition = rows
	c.status.InhibitionReady = true
	c.profile.Inhibition.Restore = restore
	control.mu.Lock()
	control.inhibitionInitialized = true
	control.inhibitionClears = nil
	control.mu.Unlock()
	return nil
}

func (c *Controller) commandInhibition(ctx context.Context, command Command) (map[string]json.RawMessage, error) {
	c.opMu.Lock()
	defer c.opMu.Unlock()
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	c.mu.Lock()
	if c.status.State != "running" || c.runContext == nil || c.runContext.Err() != nil || !c.status.InhibitionReady || !c.zone.Load() {
		c.mu.Unlock()
		return nil, errors.New("a current enforced policy and connected Zone are required for inhibit commands")
	}
	if !slices.ContainsFunc(c.profile.System.Radios, func(r Radio) bool { return r.RID == command.RID && r.Kind == "subscriber" }) || slices.Contains(c.profile.Inhibition.Desired, command.RID) != (command.Action == "inhibit") {
		c.mu.Unlock()
		return nil, errors.New("inhibit command disagrees with the current physical policy")
	}
	// An explicit re-enable may establish bounded pending state even if this
	// Site never observed an earlier inhibit. Persist conservatively first.
	if command.Action == "uninhibit" && !inhibitionBlocked(c.status.Inhibition, command.RID) {
		if len(c.status.Inhibition) >= InhibitRecordLimit {
			c.mu.Unlock()
			return nil, errors.New("current inhibit protection exceeds the 32-radio limit")
		}
		next := append(slices.Clone(c.status.Inhibition), InhibitState{RadioID: command.RID, Blocked: true, PendingUninhibit: true, Procedure: "idle"})
		if c.opts.OnInhibition != nil {
			if err := c.opts.OnInhibition(inhibitionRestore(next)); err != nil {
				c.mu.Unlock()
				return nil, err
			}
		}
		c.status.Inhibition = next
		c.profile.Inhibition.Restore = inhibitionRestore(next)
	}
	runCtx, run := c.runContext, c.runID
	desired := slices.Clone(c.profile.Inhibition.Desired)
	c.mu.Unlock()
	client, err := c.ControlClient()
	if err != nil {
		return nil, err
	}
	operation, cancel := context.WithCancel(ctx)
	stop := context.AfterFunc(runCtx, cancel)
	defer cancel()
	defer stop()
	reply, commandErr := client.Put(operation, "/p25/rid", map[string]any{"command": command.Action, "dstId": command.RID})
	if err = c.synchronizeInhibition(operation, false); err != nil {
		c.mu.Lock()
		current := c.runID == run && c.runContext == runCtx && slices.Equal(c.inhibitDesired, desired)
		c.mu.Unlock()
		if current {
			err = errors.Join(err, c.withdrawInhibition(ctx, err))
		}
		return reply, errors.Join(commandErr, err)
	}
	if commandErr == nil {
		c.emit(Event{Kind: "command_sent", Audience: "operator", RID: command.RID, Direction: "sent", Message: command.Action + " sent; awaiting a subscriber response."})
	}
	return reply, commandErr
}

func (c *Controller) withdrawInhibition(ctx context.Context, cause error) error {
	c.mu.Lock()
	c.status.InhibitionReady = false
	c.status.Error = "RF withdrawn: inhibit policy could not be enforced: " + cause.Error()
	c.withdrawSiteLocked()
	c.cancelRunStartsLocked()
	c.mu.Unlock()
	return c.stop(ctx, false)
}
