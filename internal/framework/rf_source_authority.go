package framework

import (
	"open25.local/open25-fne/internal/radio"
	"slices"
	"time"
)

// Only semantic facts are durable; receipt times and call IDs are not owners.
type rfAuthorityFact struct {
	SiteID          string    `json:"site_id"`
	GroupID         int       `json:"group_id,omitempty"`
	At              time.Time `json:"at"`
	NativeStartedAt time.Time `json:"native_started_at,omitzero"`
	Ambiguous       bool      `json:"ambiguous,omitempty"`
	SiteAmbiguous   bool      `json:"site_ambiguous,omitempty"`
	Withdrawn       bool      `json:"withdrawn,omitempty"`
}
type rfAuthorityUnit struct {
	Affiliation       rfAuthorityFact `json:"affiliation,omitzero"`
	Registration      rfAuthorityFact `json:"registration,omitzero"`
	Terminal          rfAuthorityFact `json:"terminal,omitzero"`
	NullAffiliation   rfAuthorityFact `json:"null_affiliation,omitzero"`
	MembershipUnknown rfAuthorityFact `json:"membership_unknown,omitzero"`
}
type rfAuthorityDocument struct {
	ZoneID     string                  `json:"zone_id"`
	NativeRuns map[string]time.Time    `json:"native_runs"`
	Units      map[int]rfAuthorityUnit `json:"units"`
}
type rfAuthorityWithdrawal struct {
	RadioID         int       `json:"radio_id"`
	GroupID         int       `json:"group_id"`
	AffiliatedAt    time.Time `json:"affiliated_at"`
	NativeStartedAt time.Time `json:"native_started_at"`
}
type rfAuthorityUpdate struct {
	SiteID           string                    `json:"site_id"`
	NativeStartedAt  time.Time                 `json:"native_started_at"`
	ObservedAt       time.Time                 `json:"observed_at"`
	Affiliations     []radio.Affiliation       `json:"affiliations"`
	Registrations    []radio.Registration      `json:"registrations"`
	Terminals        []radio.Registration      `json:"terminals"`
	NullAffiliations []radio.Registration      `json:"null_affiliations"`
	Withdrawals      []rfAuthorityWithdrawal   `json:"withdrawals"`
	Retired          []radio.RetiredSourceFact `json:"retired"`
}

func mergeRFFact(old, next rfAuthorityFact) rfAuthorityFact {
	if old.At.IsZero() || next.At.After(old.At) {
		return next
	}
	if !next.At.Equal(old.At) {
		return old
	}
	ambiguous := old.Ambiguous || next.Ambiguous || old.SiteID != next.SiteID || old.GroupID != next.GroupID
	siteAmbiguous := old.SiteAmbiguous || next.SiteAmbiguous || old.SiteID != next.SiteID
	withdrawn := old.Withdrawn || next.Withdrawn
	if old.SiteID == next.SiteID && old.GroupID == next.GroupID {
		if next.NativeStartedAt.After(old.NativeStartedAt) {
			withdrawn = next.Withdrawn
		}
		if next.NativeStartedAt.Before(old.NativeStartedAt) {
			withdrawn = old.Withdrawn
		}
	}

	if next.SiteID < old.SiteID || (next.SiteID == old.SiteID && next.GroupID < old.GroupID) {
		old = next
	} else if next.SiteID == old.SiteID && next.GroupID == old.GroupID && next.NativeStartedAt.After(old.NativeStartedAt) {
		old.NativeStartedAt = next.NativeStartedAt
	}
	old.Ambiguous, old.SiteAmbiguous, old.Withdrawn = ambiguous, siteAmbiguous, withdrawn
	return old
}
func mergeRFWithdrawal(old rfAuthorityFact, site string, w rfAuthorityWithdrawal) rfAuthorityFact {
	if old.SiteID == site && old.GroupID == w.GroupID && old.At.Equal(w.AffiliatedAt) && w.NativeStartedAt.Before(old.NativeStartedAt) {
		return old
	}
	return mergeRFFact(old, rfAuthorityFact{SiteID: site, GroupID: w.GroupID, At: w.AffiliatedAt.UTC(), NativeStartedAt: w.NativeStartedAt.UTC(), Withdrawn: true})
}
func mergeRFRetired(unit rfAuthorityUnit, site string, r radio.RetiredSourceFact) rfAuthorityUnit {
	fact := rfAuthorityFact{SiteID: site, At: r.At.UTC(), NativeStartedAt: r.NativeStartedAt.UTC()}
	switch r.Kind {
	case "registration":
		unit.Registration = mergeRFFact(unit.Registration, fact)
	case "null_affiliation":
		unit.NullAffiliation = mergeRFFact(unit.NullAffiliation, fact)
	case "deregistration":
		unit.Terminal = mergeRFFact(unit.Terminal, fact)
	}
	return unit
}
func cloneRFAuthority(d rfAuthorityDocument) rfAuthorityDocument {
	next := rfAuthorityDocument{ZoneID: d.ZoneID, NativeRuns: make(map[string]time.Time, len(d.NativeRuns)), Units: make(map[int]rfAuthorityUnit, len(d.Units))}
	for site, run := range d.NativeRuns {
		next.NativeRuns[site] = run
	}
	for rid, unit := range d.Units {
		next.Units[rid] = unit
	}
	return next
}
func mergeRFAuthority(d rfAuthorityDocument, update rfAuthorityUpdate) (rfAuthorityDocument, []int, bool) {
	next := cloneRFAuthority(d)
	if update.NativeStartedAt.After(next.NativeRuns[update.SiteID]) {
		next.NativeRuns[update.SiteID] = update.NativeStartedAt.UTC()
	}
	for _, a := range update.Affiliations {
		unit := next.Units[a.RID]
		unit.Affiliation = mergeRFFact(unit.Affiliation, rfAuthorityFact{SiteID: update.SiteID, GroupID: a.TGID, At: a.At.UTC(), NativeStartedAt: update.NativeStartedAt.UTC()})
		next.Units[a.RID] = unit
	}
	for _, r := range update.Registrations {
		unit := next.Units[r.RID]
		unit.Registration = mergeRFFact(unit.Registration, rfAuthorityFact{SiteID: update.SiteID, At: r.At.UTC(), NativeStartedAt: update.NativeStartedAt.UTC()})
		next.Units[r.RID] = unit
	}
	for _, t := range update.Terminals {
		unit := next.Units[t.RID]
		unit.Terminal = mergeRFFact(unit.Terminal, rfAuthorityFact{SiteID: update.SiteID, At: t.At.UTC(), NativeStartedAt: update.NativeStartedAt.UTC()})
		next.Units[t.RID] = unit
	}
	for _, n := range update.NullAffiliations {
		unit := next.Units[n.RID]
		unit.NullAffiliation = mergeRFFact(unit.NullAffiliation, rfAuthorityFact{SiteID: update.SiteID, At: n.At.UTC(), NativeStartedAt: update.NativeStartedAt.UTC()})
		next.Units[n.RID] = unit
	}
	for _, r := range update.Retired {
		next.Units[r.RID] = mergeRFRetired(next.Units[r.RID], update.SiteID, r)
	}
	for _, w := range update.Withdrawals {
		unit := next.Units[w.RadioID]
		unit.Affiliation = mergeRFWithdrawal(unit.Affiliation, update.SiteID, w)
		next.Units[w.RadioID] = unit
	}
	affected := []int{}
	for rid, unit := range next.Units {
		if previous, exists := d.Units[rid]; !exists || previous != unit || !d.NativeRuns[unit.Affiliation.SiteID].Equal(next.NativeRuns[unit.Affiliation.SiteID]) {
			affected = append(affected, rid)
		}
	}
	slices.Sort(affected)
	changed := len(affected) != 0 || len(next.NativeRuns) != len(d.NativeRuns)
	for site, run := range next.NativeRuns {
		changed = changed || !run.Equal(d.NativeRuns[site])
	}
	return next, affected, changed
}

func rfAuthorityOwners(d rfAuthorityDocument) map[int]affiliationOwner {
	owners := make(map[int]affiliationOwner, len(d.Units))
	for rid, unit := range d.Units {
		a, r, terminal := unit.Affiliation, unit.Registration, mergeRFFact(unit.Terminal, mergeRFFact(unit.NullAffiliation, unit.MembershipUnknown))
		var owner affiliationOwner
		if !a.At.IsZero() {
			switch {
			case r.At.IsZero() || a.At.After(r.At):
				if !a.Ambiguous {
					owner = affiliationOwner{a.SiteID, a.GroupID, a.At}
				}
			case r.At.After(a.At):
				if !r.Ambiguous {
					owner = affiliationOwner{SiteID: r.SiteID, At: r.At}
					if !a.Ambiguous && a.SiteID == r.SiteID && (terminal.At.IsZero() || a.At.After(terminal.At)) {
						owner.TGID = a.GroupID
					}
				}
			case !a.Ambiguous && !r.Ambiguous && a.SiteID == r.SiteID:
				owner = affiliationOwner{a.SiteID, a.GroupID, a.At}
			}
		} else if !r.At.IsZero() && !r.Ambiguous {
			owner = affiliationOwner{SiteID: r.SiteID, At: r.At}
		}
		if !terminal.At.IsZero() && !a.At.After(terminal.At) && !r.At.After(terminal.At) {
			if terminal.Ambiguous || (a.At.Equal(terminal.At) && (a.SiteAmbiguous || a.SiteID != terminal.SiteID)) || (r.At.Equal(terminal.At) && (r.Ambiguous || r.SiteID != terminal.SiteID)) {
				owner = affiliationOwner{}
			} else {
				owner = affiliationOwner{SiteID: terminal.SiteID, At: terminal.At}
			}
		}
		if owner.TGID != 0 && owner.SiteID == a.SiteID && (a.Withdrawn || a.NativeStartedAt.IsZero() || a.NativeStartedAt.Before(d.NativeRuns[a.SiteID])) {
			owner.TGID = 0
		}
		if owner.SiteID != "" {
			owners[rid] = owner
		}
	}
	return owners
}
