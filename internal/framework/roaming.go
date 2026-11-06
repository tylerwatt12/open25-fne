package framework

import (
	"time"

	"open25.local/open25-fne/internal/activity"
)

type affiliationOwner struct {
	SiteID string
	TGID   int
	At     time.Time
}

// latestAffiliationOwners compares retained accepted registrations,
// affiliations and presence for Zone ownership; allocation checks freshness.
// It assumes comparable Site clocks; conflicting confirmations at the same
// newest instant cannot safely identify a remote RF owner.
func latestAffiliationOwners(reports []SiteReport, presence []activity.Radio, now time.Time) map[int]affiliationOwner {
	type candidate struct {
		affiliationOwner
		ambiguous     bool
		siteAmbiguous bool
	}
	affiliations := map[int]candidate{}
	registrations := map[int]candidate{}
	terminals := map[int]candidate{}
	futureLimit := now.Add(time.Minute)
	consider := func(latest map[int]candidate, rid int, owner affiliationOwner) {
		if rid < 1 || rid > 0xFFFFFB || !recordID.MatchString(owner.SiteID) || owner.At.IsZero() || owner.At.After(futureLimit) {
			return
		}
		owner.At = owner.At.UTC()
		previous, exists := latest[rid]
		if !exists || owner.At.After(previous.At) {
			latest[rid] = candidate{affiliationOwner: owner}
			return
		}
		if !owner.At.Equal(previous.At) {
			return
		}
		if owner.SiteID != previous.SiteID || owner.TGID != previous.TGID {
			previous.ambiguous = true
		}
		if owner.SiteID != previous.SiteID {
			previous.siteAmbiguous = true
		}
		latest[rid] = previous
	}
	for _, report := range reports {
		if report.Radio == nil || report.ReceivedAt.IsZero() || report.ReceivedAt.After(futureLimit) {
			continue
		}
		for _, registration := range report.Radio.Registrations {
			consider(registrations, registration.RID, affiliationOwner{SiteID: report.ID, At: registration.At})
		}
		for _, affiliation := range report.Radio.Affiliations {
			if affiliation.TGID < 1 || affiliation.TGID > 65534 {
				continue
			}
			consider(affiliations, affiliation.RID, affiliationOwner{SiteID: report.ID, TGID: affiliation.TGID, At: affiliation.At})
		}
	}
	for _, observed := range presence {
		target := registrations
		if !observed.Registered {
			target = terminals
		}
		consider(target, observed.RID, affiliationOwner{SiteID: observed.SiteID, At: observed.LastSeen})
	}
	owners := map[int]affiliationOwner{}
	for rid, affiliation := range affiliations {
		registration, exists := registrations[rid]
		if !exists || affiliation.At.After(registration.At) {
			if !affiliation.ambiguous {
				owners[rid] = affiliation.affiliationOwner
			}
			continue
		}
		if registration.At.After(affiliation.At) {
			if !registration.ambiguous {
				owner := registration.affiliationOwner
				terminal, terminated := terminals[rid]
				if !affiliation.ambiguous && owner.SiteID == affiliation.SiteID && (!terminated || affiliation.At.After(terminal.At)) {
					owner.TGID = affiliation.TGID
				}
				owners[rid] = owner
			}
			continue
		}
		if !affiliation.ambiguous && !registration.ambiguous && affiliation.SiteID == registration.SiteID {
			owners[rid] = affiliation.affiliationOwner
		}
	}
	for rid, registration := range registrations {
		if _, exists := affiliations[rid]; !exists && !registration.ambiguous {
			owners[rid] = registration.affiliationOwner
		}
	}
	for rid, terminal := range terminals {
		affiliation, hasAffiliation := affiliations[rid]
		registration, hasRegistration := registrations[rid]
		if (hasAffiliation && affiliation.At.After(terminal.At)) || (hasRegistration && registration.At.After(terminal.At)) {
			continue
		}
		conflictingAffiliation := hasAffiliation && affiliation.At.Equal(terminal.At) && (affiliation.siteAmbiguous || affiliation.SiteID != terminal.SiteID)
		conflictingRegistration := hasRegistration && registration.At.Equal(terminal.At) && (registration.ambiguous || registration.SiteID != terminal.SiteID)
		if terminal.ambiguous || conflictingAffiliation || conflictingRegistration {
			delete(owners, rid)
		} else {
			owners[rid] = terminal.affiliationOwner
		}
	}
	return owners
}
