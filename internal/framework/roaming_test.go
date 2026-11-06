package framework

import (
	"reflect"
	"testing"
	"time"

	"open25.local/open25-fne/internal/activity"
	"open25.local/open25-fne/internal/radio"
)

func roamingReport(site string, received time.Time, affiliations ...radio.Affiliation) SiteReport {
	return SiteReport{ID: site, ReceivedAt: received, Radio: &radio.Status{Affiliations: affiliations}}
}

func TestLatestAffiliationOwnersFollowAcceptedRoamingObservations(t *testing.T) {
	now := time.Date(2026, 10, 29, 12, 0, 0, 0, time.UTC)
	old, newer, newest := now.Add(-time.Hour), now.Add(-2*time.Second), now.Add(-time.Second)
	a := roamingReport("site-a", now, radio.Affiliation{RID: 10001, TGID: 1001, At: old})
	b := roamingReport("site-b", now.Add(-3*time.Second), radio.Affiliation{RID: 10001, TGID: 1001, At: newer})
	duplicate := roamingReport("site-b", now, radio.Affiliation{RID: 10001, TGID: 1001, At: newer.In(time.FixedZone("Site local", -4*60*60))})
	for _, test := range []struct {
		name    string
		reports []SiteReport
		owner   affiliationOwner
	}{
		{"old A and new B", []SiteReport{a, b}, affiliationOwner{"site-b", 1001, newer}},
		{"different talkgroup", []SiteReport{a, roamingReport("site-b", now, radio.Affiliation{RID: 10001, TGID: 1002, At: newer})}, affiliationOwner{"site-b", 1002, newer}},
		{"genuine accepted roam back", []SiteReport{b, roamingReport("site-a", now, radio.Affiliation{RID: 10001, TGID: 1001, At: newest})}, affiliationOwner{"site-a", 1001, newest}},
		{"retained newer owner blocks older fresh Site", []SiteReport{a, roamingReport("site-b", now.Add(-5*time.Second), radio.Affiliation{RID: 10001, TGID: 1001, At: now.Add(-6 * time.Second)})}, affiliationOwner{"site-b", 1001, now.Add(-6 * time.Second)}},
		{"retained report remains an ownership observation", []SiteReport{roamingReport("site-a", now.Add(-time.Hour), radio.Affiliation{RID: 10001, TGID: 1001, At: old})}, affiliationOwner{"site-a", 1001, old}},
		{"duplicate observation", []SiteReport{b, duplicate}, affiliationOwner{"site-b", 1001, newer}},
		{"duplicate in reverse order", []SiteReport{duplicate, b}, affiliationOwner{"site-b", 1001, newer}},
	} {
		t.Run(test.name, func(t *testing.T) {
			owners := latestAffiliationOwners(test.reports, nil, now)
			if len(owners) != 1 || owners[10001] != test.owner {
				t.Fatalf("owners = %#v, want only %#v", owners, test.owner)
			}
		})
	}
}

func TestLatestAffiliationOwnersRejectInvalidObservations(t *testing.T) {
	now := time.Date(2026, 10, 29, 12, 0, 0, 0, time.UTC)
	valid := radio.Affiliation{RID: 10001, TGID: 1001, At: now}
	for _, test := range []struct {
		name   string
		report SiteReport
	}{
		{"missing radio", SiteReport{ID: "site-a", ReceivedAt: now}},
		{"missing received time", roamingReport("site-a", time.Time{}, valid)},
		{"future report", roamingReport("site-a", now.Add(time.Minute+time.Nanosecond), valid)},
		{"missing accepted time", roamingReport("site-a", now, radio.Affiliation{RID: 10001, TGID: 1001})},
		{"future accepted time", roamingReport("site-a", now, radio.Affiliation{RID: 10001, TGID: 1001, At: now.Add(time.Minute + time.Nanosecond)})},
		{"zero RID", roamingReport("site-a", now, radio.Affiliation{RID: 0, TGID: 1001, At: now})},
		{"RID too large", roamingReport("site-a", now, radio.Affiliation{RID: 0xFFFFFC, TGID: 1001, At: now})},
		{"zero TGID", roamingReport("site-a", now, radio.Affiliation{RID: 10001, TGID: 0, At: now})},
		{"TGID too large", roamingReport("site-a", now, radio.Affiliation{RID: 10001, TGID: 65535, At: now})},
	} {
		t.Run(test.name, func(t *testing.T) {
			if owners := latestAffiliationOwners([]SiteReport{test.report}, nil, now); len(owners) != 0 {
				t.Fatalf("invalid observation assigned owners: %#v", owners)
			}
		})
	}
	report := roamingReport("site-a", now.Add(time.Minute), radio.Affiliation{RID: 1, TGID: 1, At: now.Add(time.Minute)}, radio.Affiliation{RID: 0xFFFFFB, TGID: 65534, At: now})
	if owners := latestAffiliationOwners([]SiteReport{report}, nil, now); len(owners) != 2 {
		t.Fatal("valid ID and clock-tolerance boundaries were rejected")
	}
}

func TestLatestAffiliationOwnersOmitNewestConflictsRegardlessOfReportOrder(t *testing.T) {
	now := time.Date(2026, 10, 29, 12, 0, 0, 0, time.UTC)
	accepted := now.Add(-time.Second)
	a := roamingReport("site-a", now, radio.Affiliation{RID: 10001, TGID: 1001, At: accepted})
	for _, conflict := range []SiteReport{
		roamingReport("site-b", now, radio.Affiliation{RID: 10001, TGID: 1001, At: accepted}),
		roamingReport("site-a", now, radio.Affiliation{RID: 10001, TGID: 1002, At: accepted}),
	} {
		for _, resolved := range []bool{false, true} {
			last := a
			wanted := map[int]affiliationOwner{}
			if resolved {
				last = roamingReport("site-c", now, radio.Affiliation{RID: 10001, TGID: 1003, At: now})
				wanted[10001] = affiliationOwner{"site-c", 1003, now}
			}
			for _, reports := range [][]SiteReport{{a, conflict, last}, {a, last, conflict}, {conflict, a, last}, {conflict, last, a}, {last, a, conflict}, {last, conflict, a}} {
				if owners := latestAffiliationOwners(reports, nil, now); !reflect.DeepEqual(owners, wanted) {
					t.Fatalf("order-dependent or ambiguous owner: %#v, want %#v", owners, wanted)
				}
			}
		}
	}
}

func TestLatestAffiliationOwnersRegistrationPreservesOnlyTheLatestSiteTalkgroup(t *testing.T) {
	now := time.Date(2026, 10, 29, 12, 0, 0, 0, time.UTC)
	old, registered := now.Add(-time.Hour), now.Add(-time.Second)
	a := roamingReport("site-a", now, radio.Affiliation{RID: 10001, TGID: 1001, At: old})
	b := roamingReport("site-b", now)
	b.Radio.Registrations = []radio.Registration{{RID: 10001, At: registered}}
	fresh := roamingReport("site-b", now, radio.Affiliation{RID: 10001, TGID: 1002, At: now})
	sameTime := roamingReport("site-b", now, radio.Affiliation{RID: 10001, TGID: 1002, At: registered})
	otherRegistration := roamingReport("site-a", now)
	otherRegistration.Radio.Registrations = []radio.Registration{{RID: 10001, At: registered}}
	bAffiliated := roamingReport("site-b", now, radio.Affiliation{RID: 10001, TGID: 1002, At: registered.Add(-time.Second)})
	retainedRegistration := roamingReport("site-b", now.Add(-10*time.Second))
	retainedRegistration.Radio.Registrations = []radio.Registration{{RID: 10001, At: now.Add(-11 * time.Second)}}
	for _, test := range []struct {
		name    string
		reports []SiteReport
		wanted  map[int]affiliationOwner
	}{
		{"A affiliation then B registration", []SiteReport{a, b}, map[int]affiliationOwner{10001: {"site-b", 0, registered}}},
		{"periodic registration preserves same Site talkgroup", []SiteReport{b, roamingReport("site-b", now, radio.Affiliation{RID: 10001, TGID: 1001, At: old})}, map[int]affiliationOwner{10001: {"site-b", 1001, registered}}},
		{"return registration cannot revive older A talkgroup", []SiteReport{a, bAffiliated, otherRegistration}, map[int]affiliationOwner{10001: {"site-a", 0, registered}}},
		{"periodic B registration retains latest B talkgroup", []SiteReport{a, bAffiliated, b}, map[int]affiliationOwner{10001: {"site-b", 1002, registered}}},
		{"retained registration blocks older fresh A affiliation", []SiteReport{a, retainedRegistration}, map[int]affiliationOwner{10001: {"site-b", 0, now.Add(-11 * time.Second)}}},
		{"B fresh affiliation confirms talkgroup", []SiteReport{a, b, fresh}, map[int]affiliationOwner{10001: {"site-b", 1002, now}}},
		{"same time same Site prefers affiliation", []SiteReport{b, sameTime}, map[int]affiliationOwner{10001: {"site-b", 1002, registered}}},
		{"same time across Site registration and affiliation", []SiteReport{otherRegistration, sameTime}, map[int]affiliationOwner{}},
		{"same time across Site registrations", []SiteReport{otherRegistration, b}, map[int]affiliationOwner{}},
		{"registration cannot resolve conflicting talkgroups", []SiteReport{b, sameTime, roamingReport("site-b", now, radio.Affiliation{RID: 10001, TGID: 1003, At: registered})}, map[int]affiliationOwner{}},
	} {
		t.Run(test.name, func(t *testing.T) {
			for shift := range len(test.reports) {
				ordered := append(append([]SiteReport{}, test.reports[shift:]...), test.reports[:shift]...)
				for range 2 {
					if owners := latestAffiliationOwners(ordered, nil, now); !reflect.DeepEqual(owners, test.wanted) {
						t.Fatalf("owners = %#v, want %#v", owners, test.wanted)
					}
					for i, j := 0, len(ordered)-1; i < j; i, j = i+1, j-1 {
						ordered[i], ordered[j] = ordered[j], ordered[i]
					}
				}
			}
		})
	}
}

func TestLatestAffiliationOwnersRejectInvalidRegistrations(t *testing.T) {
	now := time.Date(2026, 10, 29, 12, 0, 0, 0, time.UTC)
	for _, test := range []struct {
		name     string
		received time.Time
		accepted time.Time
		rid      int
	}{
		{"missing report time", time.Time{}, now, 10001},
		{"future report", now.Add(time.Minute + time.Nanosecond), now, 10001},
		{"missing registration time", now, time.Time{}, 10001},
		{"future registration", now, now.Add(time.Minute + time.Nanosecond), 10001},
		{"zero RID", now, now, 0},
		{"RID too large", now, now, 0xFFFFFC},
	} {
		t.Run(test.name, func(t *testing.T) {
			report := roamingReport("site-a", test.received)
			report.Radio.Registrations = []radio.Registration{{RID: test.rid, At: test.accepted}}
			if owners := latestAffiliationOwners([]SiteReport{report}, nil, now); len(owners) != 0 {
				t.Fatalf("invalid registration assigned owners: %#v", owners)
			}
		})
	}
}

func TestLatestAffiliationOwnersRetainPresenceAcrossEmptySiteReports(t *testing.T) {
	now := time.Date(2026, 10, 29, 12, 0, 0, 0, time.UTC)
	old, retained, fresh := now.Add(-3*time.Second), now.Add(-2*time.Second), now.Add(-time.Second)
	a := roamingReport("site-a", now, radio.Affiliation{RID: 10001, TGID: 1001, At: old})
	negative := activity.Radio{RID: 10001, SiteID: "site-b", LastSeen: retained, AffiliatedTalkgroup: 1002}
	positive := negative
	positive.Registered = true
	freshRegistration := roamingReport("site-a", now)
	freshRegistration.Radio.Registrations = []radio.Registration{{RID: 10001, At: fresh}}
	for _, test := range []struct {
		name     string
		reports  []SiteReport
		presence []activity.Radio
		wanted   map[int]affiliationOwner
	}{
		{"missing B report retains negative owner", []SiteReport{a}, []activity.Radio{negative}, map[int]affiliationOwner{10001: {"site-b", 0, retained}}},
		{"empty B report retains negative owner", []SiteReport{a, roamingReport("site-b", now)}, []activity.Radio{negative}, map[int]affiliationOwner{10001: {"site-b", 0, retained}}},
		{"positive presence does not supply cached talkgroup", []SiteReport{a, roamingReport("site-b", now)}, []activity.Radio{positive}, map[int]affiliationOwner{10001: {"site-b", 0, retained}}},
		{"presence alone has no affiliated demand", nil, []activity.Radio{positive}, map[int]affiliationOwner{10001: {"site-b", 0, retained}}},
		{"new A affiliation reclaims owner", []SiteReport{roamingReport("site-a", now, radio.Affiliation{RID: 10001, TGID: 1001, At: fresh})}, []activity.Radio{negative}, map[int]affiliationOwner{10001: {"site-a", 1001, fresh}}},
		{"new registration cannot revive pre-terminal affiliation", []SiteReport{a, freshRegistration}, []activity.Radio{negative}, map[int]affiliationOwner{10001: {"site-a", 0, fresh}}},
		{"equal same Site terminal suppresses affiliation", []SiteReport{roamingReport("site-b", now, radio.Affiliation{RID: 10001, TGID: 1001, At: retained})}, []activity.Radio{negative}, map[int]affiliationOwner{10001: {"site-b", 0, retained}}},
		{"same Site terminal suppresses tied talkgroups", []SiteReport{roamingReport("site-b", now, radio.Affiliation{RID: 10001, TGID: 1001, At: retained}, radio.Affiliation{RID: 10001, TGID: 1002, At: retained})}, []activity.Radio{negative}, map[int]affiliationOwner{10001: {"site-b", 0, retained}}},
		{"equal different Site terminal is ambiguous", []SiteReport{roamingReport("site-a", now, radio.Affiliation{RID: 10001, TGID: 1001, At: retained})}, []activity.Radio{negative}, map[int]affiliationOwner{}},
		{"equal cross Site presence is ambiguous", []SiteReport{a}, []activity.Radio{negative, {RID: 10001, SiteID: "site-a", Registered: true, LastSeen: retained}}, map[int]affiliationOwner{}},
		{"positive presence retains only real same Site affiliation", []SiteReport{roamingReport("site-b", now, radio.Affiliation{RID: 10001, TGID: 1001, At: old})}, []activity.Radio{positive}, map[int]affiliationOwner{10001: {"site-b", 1001, retained}}},
	} {
		t.Run(test.name, func(t *testing.T) {
			presence := append([]activity.Radio{}, test.presence...)
			for range 2 {
				if owners := latestAffiliationOwners(test.reports, presence, now); !reflect.DeepEqual(owners, test.wanted) {
					t.Fatalf("owners = %#v, want %#v", owners, test.wanted)
				}
				for i, j := 0, len(presence)-1; i < j; i, j = i+1, j-1 {
					presence[i], presence[j] = presence[j], presence[i]
				}
			}
		})
	}
}

func TestLatestAffiliationOwnersRejectInvalidPresence(t *testing.T) {
	now := time.Date(2026, 10, 29, 12, 0, 0, 0, time.UTC)
	for _, observed := range []activity.Radio{
		{RID: 0, SiteID: "site-b", LastSeen: now},
		{RID: 0xFFFFFC, SiteID: "site-b", LastSeen: now},
		{RID: 10001, LastSeen: now},
		{RID: 10001, SiteID: "invalid Site", LastSeen: now},
		{RID: 10001, SiteID: "site-b"},
		{RID: 10001, SiteID: "site-b", LastSeen: now.Add(time.Minute + time.Nanosecond)},
	} {
		if owners := latestAffiliationOwners(nil, []activity.Radio{observed}, now); len(owners) != 0 {
			t.Fatalf("invalid presence assigned owners: %#v", owners)
		}
	}
}
