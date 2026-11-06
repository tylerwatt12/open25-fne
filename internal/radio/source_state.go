package radio

import (
	"maps"
	"slices"
	"time"
)

// SourceWithdrawal identifies the accepted membership removed from the native
// table. It supplies no new membership or ownership timestamp.
type SourceWithdrawal struct {
	RadioID         int       `json:"radio_id"`
	GroupID         int       `json:"group_id"`
	AffiliatedAt    time.Time `json:"affiliated_at"`
	NativeStartedAt time.Time `json:"native_started_at"`
}

// RetiredSourceFact preserves non-membership control evidence across a native
// replacement before the independent publisher has observed it.
type RetiredSourceFact struct {
	Kind            string    `json:"kind"`
	RID             int       `json:"rid"`
	At              time.Time `json:"at"`
	NativeStartedAt time.Time `json:"native_started_at"`
}
type retiredSourceKey struct {
	kind string
	rid  int
}

// SourceState is independent of event publication and history storage. Facts
// are the latest accepted observations in the control process lifetime;
// departed affiliations remain paired with their exact withdrawal. Active and
// the departure facts must be checked before treating a fact as live RF proof.
type SourceState struct {
	SiteID           string              `json:"site_id"`
	NativeStartedAt  time.Time           `json:"native_started_at"`
	ObservedAt       time.Time           `json:"observed_at"`
	Revision         uint64              `json:"revision"`
	Active           bool                `json:"active"`
	Affiliations     []Affiliation       `json:"affiliations"`
	Registrations    []Registration      `json:"registrations"`
	NullAffiliations []Registration      `json:"null_affiliations"`
	Terminals        []Registration      `json:"terminals"`
	Withdrawals      []SourceWithdrawal  `json:"withdrawals"`
	Retired          []RetiredSourceFact `json:"retired"`
}

type sourceFacts struct {
	owner            *child
	nativeStartedAt  time.Time
	revision         uint64
	active           bool
	radios           map[int]bool
	groups           map[int]bool
	affiliations     map[int]Affiliation
	registrations    map[int]Registration
	nullAffiliations map[int]Registration
	terminals        map[int]Registration
	withdrawals      map[int]SourceWithdrawal
	retired          map[retiredSourceKey]RetiredSourceFact
}

func (c *Controller) refreshSourceScopeLocked() {
	radios, groups := map[int]bool{}, map[int]bool{}
	for _, r := range c.profile.System.Radios {
		if r.RID > 0 && r.RID <= 0xfffffb && r.Kind == "subscriber" {
			radios[r.RID] = true
		}
	}
	for _, g := range c.profile.System.Talkgroups {
		if g.TGID > 0 && g.TGID <= 65534 {
			groups[g.TGID] = true
		}
	}
	changed := !maps.Equal(c.source.radios, radios) || !maps.Equal(c.source.groups, groups)
	c.source.radios, c.source.groups = radios, groups
	for rid, a := range c.source.affiliations {
		if !radios[rid] || !groups[a.TGID] {
			delete(c.source.affiliations, rid)
		}
	}
	for _, facts := range []map[int]Registration{c.source.registrations, c.source.nullAffiliations, c.source.terminals} {
		for rid := range facts {
			if !radios[rid] {
				delete(facts, rid)
			}
		}
	}
	for rid, w := range c.source.withdrawals {
		if !radios[rid] || !groups[w.GroupID] {
			delete(c.source.withdrawals, rid)
		}
	}
	for key := range c.source.retired {
		if !radios[key.rid] {
			delete(c.source.retired, key)
		}
	}
	if changed {
		c.source.revision++
	}
}

func (c *Controller) sourceOwnerLocked() *child {
	for _, channel := range c.profile.Site.Channels {
		if channel.Role == "control" && channel.Enabled {
			if owner := c.children[channel.ID]; owner != nil && owner.role == "control" {
				return owner
			}
		}
	}
	return nil
}

func (c *Controller) sourceWithdrawalLocked(rid int) bool {
	a, exists := c.source.affiliations[rid]
	if !exists || !c.source.radios[rid] || !c.source.groups[a.TGID] || c.source.nativeStartedAt.IsZero() {
		return false
	}
	w := SourceWithdrawal{RadioID: rid, GroupID: a.TGID, AffiliatedAt: a.At, NativeStartedAt: c.source.nativeStartedAt}
	if old, exists := c.source.withdrawals[rid]; exists && (old == w || old.NativeStartedAt.After(w.NativeStartedAt) || (old.NativeStartedAt.Equal(w.NativeStartedAt) && old.AffiliatedAt.After(w.AffiliatedAt))) {
		return false
	}
	if c.source.withdrawals == nil {
		c.source.withdrawals = map[int]SourceWithdrawal{}
	}
	c.source.withdrawals[rid] = w
	return true
}

// withdrawSourceFactsLocked runs before legacy active maps are cleared. The
// retained fact/withdrawal pair remains deliverable while the control is down.
func (c *Controller) withdrawSourceFactsLocked() {
	changed := c.source.active
	c.source.active = false
	for rid := range c.source.affiliations {
		changed = c.sourceWithdrawalLocked(rid) || changed
	}
	if changed {
		c.source.revision++
	}
}

func (c *Controller) retireSourceFactsLocked() {
	if c.source.nativeStartedAt.IsZero() {
		return
	}
	if c.source.retired == nil {
		c.source.retired = map[retiredSourceKey]RetiredSourceFact{}
	}
	for _, set := range []struct {
		kind string
		rows map[int]Registration
	}{{"registration", c.source.registrations}, {"null_affiliation", c.source.nullAffiliations}, {"deregistration", c.source.terminals}} {
		for _, r := range set.rows {
			key := retiredSourceKey{set.kind, r.RID}
			old := c.source.retired[key]
			if r.At.After(old.At) || (r.At.Equal(old.At) && c.source.nativeStartedAt.After(old.NativeStartedAt)) {
				c.source.retired[key] = RetiredSourceFact{Kind: set.kind, RID: r.RID, At: r.At, NativeStartedAt: c.source.nativeStartedAt}
			}
		}
	}
}

func (c *Controller) adoptSourceOwnerLocked(owner *child) bool {
	if owner == nil || owner.role != "control" || owner.started.IsZero() || owner.started.Before(c.source.nativeStartedAt) || c.sourceOwnerLocked() != owner || c.closing || c.stopRequests != 0 || !owner.acceptsObservations() || (c.status.State != "starting" && c.status.State != "running") {
		return false
	}
	if c.source.owner != owner {
		c.retireSourceFactsLocked()
		c.withdrawSourceFactsLocked()
		c.refreshSourceScopeLocked()
		c.source.owner = owner
		c.source.nativeStartedAt = owner.started.UTC()
		c.source.affiliations = map[int]Affiliation{}
		c.source.registrations = map[int]Registration{}
		c.source.nullAffiliations = map[int]Registration{}
		c.source.terminals = map[int]Registration{}
		if c.source.withdrawals == nil {
			c.source.withdrawals = map[int]SourceWithdrawal{}
		}
		c.source.revision++
	}
	active := c.status.State == "running" && c.status.InhibitionReady && owner.readyNow()
	if c.source.active != active {
		c.source.active = active
		c.source.revision++
	}
	return true
}

func (c *Controller) sourceAffiliationLiveLocked(a Affiliation) bool {
	if w, exists := c.source.withdrawals[a.RID]; exists && w.GroupID == a.TGID && w.AffiliatedAt.Equal(a.At) && w.NativeStartedAt.Equal(c.source.nativeStartedAt) {
		return false
	}
	return c.source.nullAffiliations[a.RID].At.Before(a.At) && c.source.terminals[a.RID].At.Before(a.At)
}

func (c *Controller) sourceAffiliationLocked(a Affiliation) bool {
	old, exists := c.source.affiliations[a.RID]
	if exists && !a.At.After(old.At) {
		return false
	}
	c.source.affiliations[a.RID] = a
	if w, exists := c.source.withdrawals[a.RID]; exists && w.NativeStartedAt.Equal(c.source.nativeStartedAt) && a.At.After(w.AffiliatedAt) {
		delete(c.source.withdrawals, a.RID)
	}
	if a.At.After(c.source.nullAffiliations[a.RID].At) {
		delete(c.source.nullAffiliations, a.RID)
	}
	if a.At.After(c.source.terminals[a.RID].At) {
		delete(c.source.terminals, a.RID)
	}
	return true
}

func (c *Controller) observeSourceLocked(owner *child, e Event) {
	if owner.role != "control" || !c.adoptSourceOwnerLocked(owner) || !c.source.radios[e.RID] || e.At.Before(c.source.nativeStartedAt) {
		return
	}
	changed := false
	switch e.Kind {
	case "registered":
		if e.At.After(c.source.registrations[e.RID].At) {
			c.source.registrations[e.RID] = Registration{RID: e.RID, At: e.At}
			if e.At.After(c.source.terminals[e.RID].At) {
				delete(c.source.terminals, e.RID)
			}
			changed = true
		}
	case "affiliated":
		if c.source.groups[e.TGID] {
			changed = c.sourceAffiliationLocked(Affiliation{RID: e.RID, TGID: e.TGID, At: e.At})
			break
		}
		if e.TGID != 0 {
			break
		}
		fallthrough
	case "unaffiliated":
		if e.At.After(c.source.nullAffiliations[e.RID].At) {
			c.source.nullAffiliations[e.RID] = Registration{RID: e.RID, At: e.At}
			changed = true
		}
		changed = c.sourceWithdrawalLocked(e.RID) || changed
	case "deregistered":
		if e.At.After(c.source.terminals[e.RID].At) {
			c.source.terminals[e.RID] = Registration{RID: e.RID, At: e.At}
			changed = true
		}
		changed = c.sourceWithdrawalLocked(e.RID) || changed
	}
	if changed {
		c.source.revision++
	}
}

func (c *Controller) sourceAffiliationsLocked(owner *child, rows []Affiliation, now time.Time) {
	if owner.role != "control" || !c.adoptSourceOwnerLocked(owner) {
		return
	}
	present := make(map[int]bool, len(rows))
	changed := false
	for _, row := range rows {
		if !c.source.radios[row.RID] || !c.source.groups[row.TGID] {
			continue
		}
		present[row.RID] = true
		at := now
		if old, exists := c.source.affiliations[row.RID]; exists && old.TGID == row.TGID && c.sourceAffiliationLiveLocked(old) {
			at = old.At
		}
		changed = c.sourceAffiliationLocked(Affiliation{RID: row.RID, TGID: row.TGID, At: at}) || changed
	}
	for rid := range c.source.affiliations {
		if !present[rid] {
			changed = c.sourceWithdrawalLocked(rid) || changed
		}
	}
	if changed {
		c.source.revision++
	}
}

// SourceState clones bounded accepted facts while holding the controller lock.
// Its observation time does not advance the semantic revision or fact clocks.
func (c *Controller) SourceState() SourceState {
	if c == nil {
		return SourceState{}
	}
	c.mu.Lock()
	if !c.adoptSourceOwnerLocked(c.sourceOwnerLocked()) {
		c.withdrawSourceFactsLocked()
	}
	v := SourceState{SiteID: c.profile.Site.ID, NativeStartedAt: c.source.nativeStartedAt, ObservedAt: time.Now().UTC(), Revision: c.source.revision, Active: c.source.active, Affiliations: []Affiliation{}, Registrations: []Registration{}, NullAffiliations: []Registration{}, Terminals: []Registration{}, Withdrawals: []SourceWithdrawal{}, Retired: []RetiredSourceFact{}}
	for _, a := range c.source.affiliations {
		v.Affiliations = append(v.Affiliations, a)
	}
	for _, r := range c.source.registrations {
		v.Registrations = append(v.Registrations, r)
	}
	for _, r := range c.source.nullAffiliations {
		v.NullAffiliations = append(v.NullAffiliations, r)
	}
	for _, r := range c.source.terminals {
		v.Terminals = append(v.Terminals, r)
	}
	for _, r := range c.source.retired {
		v.Retired = append(v.Retired, r)
	}
	for _, w := range c.source.withdrawals {
		v.Withdrawals = append(v.Withdrawals, w)
	}
	c.mu.Unlock()
	slices.SortFunc(v.Affiliations, func(a, b Affiliation) int { return a.RID - b.RID })
	for _, rows := range [][]Registration{v.Registrations, v.NullAffiliations, v.Terminals} {
		slices.SortFunc(rows, func(a, b Registration) int { return a.RID - b.RID })
	}
	slices.SortFunc(v.Retired, func(a, b RetiredSourceFact) int {
		if a.RID != b.RID {
			return a.RID - b.RID
		}
		if a.Kind < b.Kind {
			return -1
		}
		if a.Kind > b.Kind {
			return 1
		}
		return 0
	})
	slices.SortFunc(v.Withdrawals, func(a, b SourceWithdrawal) int { return a.RadioID - b.RadioID })
	return v
}

// SourceAllowed is the allocation-free renewal check for an already captured
// control lifetime. It never waits on callbacks, snapshots, or disk I/O.
func (c *Controller) SourceAllowed(rid, tgid int, nativeRun time.Time) bool {
	if c == nil || nativeRun.IsZero() {
		return false
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	owner := c.source.owner
	if owner == nil || c.children[owner.id] != owner || c.closing || c.stopRequests != 0 || c.status.State != "running" || !c.source.nativeStartedAt.Equal(nativeRun) || !owner.started.Equal(nativeRun) || !c.source.radios[rid] || !c.source.groups[tgid] || !c.status.InhibitionReady || inhibitionBlocked(c.status.Inhibition, rid) || slices.Contains(c.profile.Inhibition.Desired, rid) || !owner.acceptsObservations() || !owner.readyNow() {
		return false
	}
	a, exists := c.source.affiliations[rid]
	return exists && a.TGID == tgid && c.sourceAffiliationLiveLocked(a)
}
