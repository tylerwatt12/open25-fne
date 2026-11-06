package framework

import (
	"encoding/json"
	"reflect"
	"testing"
	"time"

	"open25.local/open25-fne/internal/activity"
	"open25.local/open25-fne/internal/radio"
)

func rfMergeTestTime(second int) time.Time {
	return time.Date(2026, 11, 1, 12, 0, second, 0, time.UTC)
}

func rfMergeTestUpdate(site string, runSecond int, affiliations ...radio.Affiliation) rfAuthorityUpdate {
	return rfAuthorityUpdate{SiteID: site, NativeStartedAt: rfMergeTestTime(runSecond), ObservedAt: rfMergeTestTime(59), Affiliations: affiliations}
}

func rfMergeTestReload(t *testing.T, d rfAuthorityDocument) rfAuthorityDocument {
	t.Helper()
	encoded, err := json.Marshal(d)
	if err != nil {
		t.Fatal(err)
	}
	var reloaded rfAuthorityDocument
	if err := json.Unmarshal(encoded, &reloaded); err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(reloaded, d) {
		t.Fatalf("JSON reload changed authority facts:\n got %#v\nwant %#v", reloaded, d)
	}
	return reloaded
}

func rfMergeTestPermutations(updates []rfAuthorityUpdate, visit func([]rfAuthorityUpdate)) {
	ordered := append([]rfAuthorityUpdate(nil), updates...)
	var permute func(int)
	permute = func(i int) {
		if i == len(ordered) {
			visit(ordered)
			return
		}
		for j := i; j < len(ordered); j++ {
			ordered[i], ordered[j] = ordered[j], ordered[i]
			permute(i + 1)
			ordered[i], ordered[j] = ordered[j], ordered[i]
		}
	}
	permute(0)
}

func rfMergeTestApply(t *testing.T, updates []rfAuthorityUpdate, reload bool) rfAuthorityDocument {
	t.Helper()
	d := rfAuthorityDocument{ZoneID: "zone-a", NativeRuns: map[string]time.Time{}, Units: map[int]rfAuthorityUnit{}}
	for _, update := range updates {
		d, _, _ = mergeRFAuthority(d, update)
		if reload {
			d = rfMergeTestReload(t, d)
		}
	}
	return d
}

func rfMergeTestOwners(t *testing.T, d rfAuthorityDocument, want map[int]affiliationOwner) {
	t.Helper()
	if got := rfAuthorityOwners(d); !reflect.DeepEqual(got, want) {
		t.Fatalf("owners = %#v, want %#v", got, want)
	}
}

func TestRFAuthorityMergeNewerSiteSurvivesReorderingDuplicatesAndReload(t *testing.T) {
	a := rfMergeTestUpdate("site-a", 1, radio.Affiliation{RID: 10001, TGID: 1001, At: rfMergeTestTime(10)})
	b := rfMergeTestUpdate("site-b", 2, radio.Affiliation{RID: 10001, TGID: 1002, At: rfMergeTestTime(20)})
	duplicate := b
	duplicate.ObservedAt = rfMergeTestTime(58)
	duplicate.Affiliations = []radio.Affiliation{{RID: 10001, TGID: 1002, At: rfMergeTestTime(20).In(time.FixedZone("Site local", -4*60*60))}}
	want := map[int]affiliationOwner{10001: {SiteID: "site-b", TGID: 1002, At: rfMergeTestTime(20)}}
	var canonical rfAuthorityDocument
	rfMergeTestPermutations([]rfAuthorityUpdate{a, b, duplicate}, func(ordered []rfAuthorityUpdate) {
		for _, reload := range []bool{false, true} {
			d := rfMergeTestApply(t, ordered, reload)
			rfMergeTestOwners(t, d, want)
			if canonical.Units == nil {
				canonical = d
			} else if !reflect.DeepEqual(d, canonical) {
				t.Fatalf("reordered or duplicate input changed durable facts:\n got %#v\nwant %#v", d, canonical)
			}
			for _, again := range []rfAuthorityUpdate{a, b, duplicate} {
				next, affected, changed := mergeRFAuthority(d, again)
				if changed || len(affected) != 0 || !reflect.DeepEqual(next, d) {
					t.Fatalf("retained input changed facts: changed=%t affected=%v", changed, affected)
				}
			}
		}
	})
}

func TestRFAuthorityMergeRegistrationAndTerminalFenceOlderAffiliation(t *testing.T) {
	a := rfMergeTestUpdate("site-a", 1, radio.Affiliation{RID: 10001, TGID: 1001, At: rfMergeTestTime(10)})
	b := rfMergeTestUpdate("site-b", 2)
	b.Registrations = []radio.Registration{{RID: 10001, At: rfMergeTestTime(20)}}
	terminal := rfMergeTestUpdate("site-b", 2)
	terminal.Terminals = []radio.Registration{{RID: 10001, At: rfMergeTestTime(30)}}
	for _, test := range []struct {
		name    string
		updates []rfAuthorityUpdate
		at      time.Time
	}{
		{"registration only", []rfAuthorityUpdate{a, b, a}, rfMergeTestTime(20)},
		{"later deregistration", []rfAuthorityUpdate{a, b, terminal, a}, rfMergeTestTime(30)},
	} {
		t.Run(test.name, func(t *testing.T) {
			rfMergeTestPermutations(test.updates, func(ordered []rfAuthorityUpdate) {
				d := rfMergeTestApply(t, ordered, true)
				rfMergeTestOwners(t, d, map[int]affiliationOwner{10001: {SiteID: "site-b", At: test.at}})
			})
		})
	}

	d := rfMergeTestApply(t, []rfAuthorityUpdate{a, b, terminal}, true)
	reregistered := rfMergeTestUpdate("site-a", 1)
	reregistered.Registrations = []radio.Registration{{RID: 10001, At: rfMergeTestTime(40)}}
	d, _, _ = mergeRFAuthority(d, reregistered)
	rfMergeTestOwners(t, d, map[int]affiliationOwner{10001: {SiteID: "site-a", At: rfMergeTestTime(40)}})
	d, _, _ = mergeRFAuthority(d, rfMergeTestUpdate("site-a", 1, radio.Affiliation{RID: 10001, TGID: 1003, At: rfMergeTestTime(50)}))
	rfMergeTestOwners(t, d, map[int]affiliationOwner{10001: {SiteID: "site-a", TGID: 1003, At: rfMergeTestTime(50)}})
}

func TestRFAuthorityMergeEqualTimeConflictsRemainUntilNewerMembership(t *testing.T) {
	a := rfMergeTestUpdate("site-a", 1, radio.Affiliation{RID: 10001, TGID: 1001, At: rfMergeTestTime(20)})
	for _, test := range []struct {
		name          string
		conflict      rfAuthorityUpdate
		siteAmbiguous bool
	}{
		{"different Site", rfMergeTestUpdate("site-b", 2, radio.Affiliation{RID: 10001, TGID: 1001, At: rfMergeTestTime(20)}), true},
		{"different talkgroup", rfMergeTestUpdate("site-a", 1, radio.Affiliation{RID: 10001, TGID: 1002, At: rfMergeTestTime(20)}), false},
	} {
		t.Run(test.name, func(t *testing.T) {
			older := rfMergeTestUpdate("site-c", 3, radio.Affiliation{RID: 10001, TGID: 1003, At: rfMergeTestTime(10)})
			rfMergeTestPermutations([]rfAuthorityUpdate{a, test.conflict, a, older}, func(ordered []rfAuthorityUpdate) {
				d := rfMergeTestApply(t, ordered, true)
				rfMergeTestOwners(t, d, map[int]affiliationOwner{})
				fact := d.Units[10001].Affiliation
				if !fact.Ambiguous || fact.SiteAmbiguous != test.siteAmbiguous || !fact.At.Equal(rfMergeTestTime(20)) {
					t.Fatalf("conflict was not durably retained: %#v", fact)
				}
				next, affected, changed := mergeRFAuthority(d, a)
				if changed || len(affected) != 0 || !reflect.DeepEqual(next, d) {
					t.Fatalf("duplicate resolved a conflict: changed=%t affected=%v", changed, affected)
				}
				d, _, _ = mergeRFAuthority(d, rfMergeTestUpdate("site-c", 3, radio.Affiliation{RID: 10001, TGID: 1003, At: rfMergeTestTime(21)}))
				d = rfMergeTestReload(t, d)
				rfMergeTestOwners(t, d, map[int]affiliationOwner{10001: {SiteID: "site-c", TGID: 1003, At: rfMergeTestTime(21)}})
				if fact := d.Units[10001].Affiliation; fact.Ambiguous || fact.SiteAmbiguous {
					t.Fatalf("genuinely newer membership retained an obsolete conflict: %#v", fact)
				}
			})
		})
	}
}

func TestRFAuthorityMergeNativeRestartRetainsSiteFenceWithoutAdvancingMembership(t *testing.T) {
	a := rfMergeTestUpdate("site-a", 1, radio.Affiliation{RID: 10001, TGID: 1001, At: rfMergeTestTime(10)})
	b1 := rfMergeTestUpdate("site-b", 2, radio.Affiliation{RID: 10001, TGID: 1002, At: rfMergeTestTime(20)})
	b2 := rfMergeTestUpdate("site-b", 25)
	before := rfMergeTestApply(t, []rfAuthorityUpdate{a, b1}, true)
	after, affected, changed := mergeRFAuthority(before, b2)
	if !changed || !reflect.DeepEqual(affected, []int{10001}) || after.Units[10001] != before.Units[10001] {
		t.Fatalf("empty native restart did not durably affect retained membership: changed=%t affected=%v", changed, affected)
	}
	rfMergeTestPermutations([]rfAuthorityUpdate{a, b1, b2, b1}, func(ordered []rfAuthorityUpdate) {
		d := rfMergeTestApply(t, ordered, true)
		rfMergeTestOwners(t, d, map[int]affiliationOwner{10001: {SiteID: "site-b", At: rfMergeTestTime(20)}})
		fact := d.Units[10001].Affiliation
		if !fact.At.Equal(rfMergeTestTime(20)) || !fact.NativeStartedAt.Equal(rfMergeTestTime(2)) || !d.NativeRuns["site-b"].Equal(rfMergeTestTime(25)) {
			t.Fatalf("restart advanced or discarded membership time: fact=%#v runs=%#v", fact, d.NativeRuns)
		}
		next, affected, changed := mergeRFAuthority(d, b2)
		if changed || len(affected) != 0 || !reflect.DeepEqual(next, d) {
			t.Fatalf("duplicate empty native run changed facts: changed=%t affected=%v", changed, affected)
		}
		d, _, _ = mergeRFAuthority(d, rfMergeTestUpdate("site-b", 25, radio.Affiliation{RID: 10001, TGID: 1002, At: rfMergeTestTime(30)}))
		rfMergeTestOwners(t, d, map[int]affiliationOwner{10001: {SiteID: "site-b", TGID: 1002, At: rfMergeTestTime(30)}})
	})
}

func TestRFAuthorityMergeDepartureRetainsExactOriginalTuple(t *testing.T) {
	base := rfMergeTestApply(t, []rfAuthorityUpdate{
		rfMergeTestUpdate("site-a", 1,
			radio.Affiliation{RID: 10001, TGID: 1001, At: rfMergeTestTime(20)},
			radio.Affiliation{RID: 10002, TGID: 1002, At: rfMergeTestTime(21)}),
		rfMergeTestUpdate("site-b", 2),
	}, true)
	valid := rfAuthorityWithdrawal{RadioID: 10001, GroupID: 1001, AffiliatedAt: rfMergeTestTime(20), NativeStartedAt: rfMergeTestTime(1)}
	update := rfMergeTestUpdate("site-a", 1)
	update.Withdrawals = []rfAuthorityWithdrawal{valid}
	d, affected, changed := mergeRFAuthority(base, update)
	wantFact := base.Units[10001].Affiliation
	wantFact.Withdrawn = true
	if !changed || !reflect.DeepEqual(affected, []int{10001}) || d.Units[10001].Affiliation != wantFact || base.Units[10001].Affiliation.Withdrawn {
		t.Fatalf("exact withdrawal was not isolated to its fact: changed=%t affected=%v units=%#v", changed, affected, d.Units)
	}
	d = rfMergeTestReload(t, d)
	rfMergeTestOwners(t, d, map[int]affiliationOwner{
		10001: {SiteID: "site-a", At: rfMergeTestTime(20)},
		10002: {SiteID: "site-a", TGID: 1002, At: rfMergeTestTime(21)},
	})
	for _, duplicate := range []rfAuthorityUpdate{update, rfMergeTestUpdate("site-a", 1, radio.Affiliation{RID: 10001, TGID: 1001, At: rfMergeTestTime(20)})} {
		next, affected, changed := mergeRFAuthority(d, duplicate)
		if changed || len(affected) != 0 || !reflect.DeepEqual(next, d) {
			t.Fatalf("duplicate cleared or changed withdrawal: changed=%t affected=%v", changed, affected)
		}
	}
	d, _, _ = mergeRFAuthority(d, rfMergeTestUpdate("site-a", 1, radio.Affiliation{RID: 10001, TGID: 1003, At: rfMergeTestTime(22)}))
	rfMergeTestOwners(t, d, map[int]affiliationOwner{
		10001: {SiteID: "site-a", TGID: 1003, At: rfMergeTestTime(22)},
		10002: {SiteID: "site-a", TGID: 1002, At: rfMergeTestTime(21)},
	})
	next, affected, changed := mergeRFAuthority(d, update)
	if changed || len(affected) != 0 || !reflect.DeepEqual(next, d) {
		t.Fatalf("delayed exact withdrawal affected a newer membership: changed=%t affected=%v", changed, affected)
	}
}

func TestRFAuthorityMergeUnseenNewerDepartureFencesOlderSiteAtOriginalTime(t *testing.T) {
	a := rfMergeTestUpdate("site-a", 1, radio.Affiliation{RID: 10001, TGID: 1001, At: rfMergeTestTime(10)})
	// B1 accepted at 20 but was replaced before publication. B2 sends the
	// original departed tuple alongside its empty native-run fence at 25.
	b := rfMergeTestUpdate("site-b", 25)
	b.Withdrawals = []rfAuthorityWithdrawal{{RadioID: 10001, GroupID: 1002, AffiliatedAt: rfMergeTestTime(20), NativeStartedAt: rfMergeTestTime(2)}}
	wantFact := rfAuthorityFact{SiteID: "site-b", GroupID: 1002, At: rfMergeTestTime(20), NativeStartedAt: rfMergeTestTime(2), Withdrawn: true}
	rfMergeTestPermutations([]rfAuthorityUpdate{a, b, b}, func(ordered []rfAuthorityUpdate) {
		d := rfMergeTestApply(t, ordered, true)
		rfMergeTestOwners(t, d, map[int]affiliationOwner{10001: {SiteID: "site-b", At: rfMergeTestTime(20)}})
		if d.Units[10001].Affiliation != wantFact || !d.NativeRuns["site-b"].Equal(rfMergeTestTime(25)) {
			t.Fatalf("unseen departure lost its original accepted tuple or native fence: %#v", d)
		}
		for _, duplicate := range []rfAuthorityUpdate{a, b} {
			next, affected, changed := mergeRFAuthority(d, duplicate)
			if changed || len(affected) != 0 || !reflect.DeepEqual(next, d) {
				t.Fatalf("duplicate changed learned departure: changed=%t affected=%v", changed, affected)
			}
		}
		newer := rfMergeTestUpdate("site-c", 3, radio.Affiliation{RID: 10001, TGID: 1003, At: rfMergeTestTime(30)})
		d, _, _ = mergeRFAuthority(d, newer)
		next, affected, changed := mergeRFAuthority(d, b)
		if changed || len(affected) != 0 || !reflect.DeepEqual(next, d) {
			t.Fatalf("delayed departure erased newer accepted membership: changed=%t affected=%v", changed, affected)
		}
		rfMergeTestOwners(t, next, map[int]affiliationOwner{10001: {SiteID: "site-c", TGID: 1003, At: rfMergeTestTime(30)}})
	})
}

func TestRFAuthorityMergeEqualTimeDepartureConflictsRemainBlocked(t *testing.T) {
	a := rfMergeTestUpdate("site-a", 1, radio.Affiliation{RID: 10001, TGID: 1001, At: rfMergeTestTime(20)})
	for _, test := range []struct {
		name          string
		site          string
		group         int
		siteAmbiguous bool
	}{
		{"cross Site", "site-b", 1001, true},
		{"cross talkgroup", "site-a", 1002, false},
	} {
		t.Run(test.name, func(t *testing.T) {
			departed := rfMergeTestUpdate(test.site, 1)
			departed.Withdrawals = []rfAuthorityWithdrawal{{RadioID: 10001, GroupID: test.group, AffiliatedAt: rfMergeTestTime(20), NativeStartedAt: rfMergeTestTime(1)}}
			rfMergeTestPermutations([]rfAuthorityUpdate{a, departed, departed}, func(ordered []rfAuthorityUpdate) {
				d := rfMergeTestApply(t, ordered, true)
				rfMergeTestOwners(t, d, map[int]affiliationOwner{})
				fact := d.Units[10001].Affiliation
				if !fact.Ambiguous || fact.SiteAmbiguous != test.siteAmbiguous || !fact.Withdrawn || !fact.At.Equal(rfMergeTestTime(20)) {
					t.Fatalf("equal-time departure conflict was not retained: %#v", fact)
				}
				resolved := rfMergeTestUpdate("site-c", 3, radio.Affiliation{RID: 10001, TGID: 1003, At: rfMergeTestTime(21)})
				d, _, _ = mergeRFAuthority(d, resolved)
				d = rfMergeTestReload(t, d)
				rfMergeTestOwners(t, d, map[int]affiliationOwner{10001: {SiteID: "site-c", TGID: 1003, At: rfMergeTestTime(21)}})
			})
		})
	}
}

func TestRFAuthorityMergeOlderNativeDepartureCannotWithdrawNewerRunMembership(t *testing.T) {
	positive := rfMergeTestUpdate("site-a", 5, radio.Affiliation{RID: 10001, TGID: 1001, At: rfMergeTestTime(20)})
	departed := rfMergeTestUpdate("site-a", 5)
	departed.Withdrawals = []rfAuthorityWithdrawal{{RadioID: 10001, GroupID: 1001, AffiliatedAt: rfMergeTestTime(20), NativeStartedAt: rfMergeTestTime(1)}}
	want := rfMergeTestApply(t, []rfAuthorityUpdate{positive}, true)
	rfMergeTestPermutations([]rfAuthorityUpdate{positive, departed, departed}, func(ordered []rfAuthorityUpdate) {
		d := rfMergeTestApply(t, ordered, true)
		if !reflect.DeepEqual(d, want) {
			t.Fatalf("older-native departure changed same-time newer-run membership:\n got %#v\nwant %#v", d, want)
		}
		rfMergeTestOwners(t, d, map[int]affiliationOwner{10001: {SiteID: "site-a", TGID: 1001, At: rfMergeTestTime(20)}})
		next, affected, changed := mergeRFAuthority(d, departed)
		if changed || len(affected) != 0 || !reflect.DeepEqual(next, d) {
			t.Fatalf("older-native departure affected a newer-run positive: changed=%t affected=%v", changed, affected)
		}
	})
}

func TestRFAuthorityMergeObservationTimeAloneDoesNotChangeAuthority(t *testing.T) {
	update := rfMergeTestUpdate("site-a", 1, radio.Affiliation{RID: 10002, TGID: 1002, At: rfMergeTestTime(20)})
	update.Registrations = []radio.Registration{{RID: 10001, At: rfMergeTestTime(21)}}
	update.Terminals = []radio.Registration{{RID: 10003, At: rfMergeTestTime(22)}}
	d, affected, changed := mergeRFAuthority(rfAuthorityDocument{ZoneID: "zone-a"}, update)
	if !changed || !reflect.DeepEqual(affected, []int{10001, 10002, 10003}) {
		t.Fatalf("semantic facts did not report sorted affected IDs: changed=%t affected=%v", changed, affected)
	}
	d = rfMergeTestReload(t, d)
	for _, observed := range []time.Time{rfMergeTestTime(58), rfMergeTestTime(59).Add(time.Hour)} {
		update.ObservedAt = observed
		next, affected, changed := mergeRFAuthority(d, update)
		if changed || len(affected) != 0 || !reflect.DeepEqual(next, d) {
			t.Fatalf("receipt/observation time changed semantic authority: changed=%t affected=%v", changed, affected)
		}
	}
}

func TestRFAuthorityMergeAndCloneDoNotShareCallerMapsOrSlices(t *testing.T) {
	input := rfMergeTestApply(t, []rfAuthorityUpdate{rfMergeTestUpdate("site-a", 1, radio.Affiliation{RID: 10001, TGID: 1001, At: rfMergeTestTime(10)})}, false)
	update := rfMergeTestUpdate("site-b", 2, radio.Affiliation{RID: 10001, TGID: 1002, At: rfMergeTestTime(20)})
	update.Registrations = []radio.Registration{{RID: 10002, At: rfMergeTestTime(21)}}
	update.Terminals = []radio.Registration{{RID: 10003, At: rfMergeTestTime(22)}}
	update.Withdrawals = []rfAuthorityWithdrawal{{RadioID: 10001, GroupID: 1002, AffiliatedAt: rfMergeTestTime(20), NativeStartedAt: rfMergeTestTime(2)}}
	d, _, _ := mergeRFAuthority(input, update)
	want := cloneRFAuthority(d)
	input.NativeRuns["site-a"] = rfMergeTestTime(50)
	input.Units[10001] = rfAuthorityUnit{}
	update.Affiliations[0] = radio.Affiliation{}
	update.Registrations[0] = radio.Registration{}
	update.Terminals[0] = radio.Registration{}
	update.Withdrawals[0] = rfAuthorityWithdrawal{}
	if !reflect.DeepEqual(d, want) {
		t.Fatal("caller mutation changed merged authority")
	}
	copy := cloneRFAuthority(d)
	copy.NativeRuns["site-b"] = rfMergeTestTime(50)
	unit := copy.Units[10001]
	unit.Affiliation.SiteID = "site-c"
	unit.Registration.At = rfMergeTestTime(51)
	unit.Terminal.At = rfMergeTestTime(52)
	copy.Units[10001] = unit
	delete(copy.Units, 10002)
	if !reflect.DeepEqual(d, want) {
		t.Fatal("clone mutation changed stored authority maps or facts")
	}
	unchanged, affected, changed := mergeRFAuthority(d, rfMergeTestUpdate("site-b", 2))
	if changed || len(affected) != 0 {
		t.Fatalf("empty duplicate update changed authority: changed=%t affected=%v", changed, affected)
	}
	unchanged.NativeRuns["site-b"] = rfMergeTestTime(53)
	delete(unchanged.Units, 10001)
	if !reflect.DeepEqual(d, want) {
		t.Fatal("unchanged merge returned caller-owned authority maps")
	}
}

func TestRFAuthorityMergeMatchesOrdinaryRegistrationAffiliationAndDeregistration(t *testing.T) {
	a := rfMergeTestUpdate("site-a", 1, radio.Affiliation{RID: 10001, TGID: 1001, At: rfMergeTestTime(10)})
	b := rfMergeTestUpdate("site-b", 2, radio.Affiliation{RID: 10001, TGID: 1002, At: rfMergeTestTime(20)})
	register := func(site string, second int) rfAuthorityUpdate {
		u := rfMergeTestUpdate(site, 1)
		u.Registrations = []radio.Registration{{RID: 10001, At: rfMergeTestTime(second)}}
		return u
	}
	terminal := func(site string, second int) rfAuthorityUpdate {
		u := rfMergeTestUpdate(site, 1)
		u.Terminals = []radio.Registration{{RID: 10001, At: rfMergeTestTime(second)}}
		return u
	}
	for _, test := range []struct {
		name    string
		updates []rfAuthorityUpdate
		want    map[int]affiliationOwner
	}{
		{"roam affiliation", []rfAuthorityUpdate{a, b}, map[int]affiliationOwner{10001: {"site-b", 1002, rfMergeTestTime(20)}}},
		{"periodic registration retains same Site group", []rfAuthorityUpdate{a, register("site-a", 20)}, map[int]affiliationOwner{10001: {"site-a", 1001, rfMergeTestTime(20)}}},
		{"registration only changes Site", []rfAuthorityUpdate{a, register("site-b", 20)}, map[int]affiliationOwner{10001: {"site-b", 0, rfMergeTestTime(20)}}},
		{"same time same Site affiliation and registration", []rfAuthorityUpdate{b, register("site-b", 20)}, map[int]affiliationOwner{10001: {"site-b", 1002, rfMergeTestTime(20)}}},
		{"same time cross Site affiliation and registration", []rfAuthorityUpdate{b, register("site-a", 20)}, map[int]affiliationOwner{}},
		{"conflicting registrations", []rfAuthorityUpdate{register("site-a", 20), register("site-b", 20)}, map[int]affiliationOwner{}},
		{"deregistration fences old affiliation", []rfAuthorityUpdate{a, terminal("site-b", 20)}, map[int]affiliationOwner{10001: {"site-b", 0, rfMergeTestTime(20)}}},
		{"new registration cannot restore preterminal group", []rfAuthorityUpdate{a, terminal("site-b", 20), register("site-a", 30)}, map[int]affiliationOwner{10001: {"site-a", 0, rfMergeTestTime(30)}}},
		{"same time same Site terminal suppresses group", []rfAuthorityUpdate{b, terminal("site-b", 20)}, map[int]affiliationOwner{10001: {"site-b", 0, rfMergeTestTime(20)}}},
		{"same time cross Site terminal conflicts", []rfAuthorityUpdate{b, terminal("site-a", 20)}, map[int]affiliationOwner{}},
		{"new affiliation after terminal", []rfAuthorityUpdate{a, terminal("site-a", 15), b}, map[int]affiliationOwner{10001: {"site-b", 1002, rfMergeTestTime(20)}}},
	} {
		t.Run(test.name, func(t *testing.T) {
			rfMergeTestPermutations(test.updates, func(ordered []rfAuthorityUpdate) {
				reports := make([]SiteReport, 0, len(ordered))
				var presence []activity.Radio
				for _, u := range ordered {
					reports = append(reports, SiteReport{ID: u.SiteID, ReceivedAt: u.ObservedAt, Radio: &radio.Status{Affiliations: u.Affiliations, Registrations: u.Registrations}})
					for _, r := range u.Terminals {
						presence = append(presence, activity.Radio{RID: r.RID, SiteID: u.SiteID, LastSeen: r.At})
					}
				}
				legacy := latestAffiliationOwners(reports, presence, rfMergeTestTime(59))
				if !reflect.DeepEqual(legacy, test.want) {
					t.Fatalf("ordinary ownership reference = %#v, want %#v", legacy, test.want)
				}
				rfMergeTestOwners(t, rfMergeTestApply(t, ordered, true), legacy)
			})
		})
	}
}
