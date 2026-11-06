package framework

import (
	"testing"
	"time"

	"open25.local/open25-fne/internal/radio"
)

func retainedRFPair(t *testing.T) (*Service, *Service, EmergencyIncident) {
	t.Helper()
	zone, _, _ := operatorFixture(t, RoleZone)
	site, _, _ := operatorFixture(t, RoleSite)
	if err := site.observeRFEmergency(10001, 1001, true); err != nil {
		t.Fatal(err)
	}
	original := site.operatorLocalSnapshot().Emergencies[0]
	if err := zone.mergeSiteOperatorSnapshot("site-1", site.operatorLocalSnapshot()); err != nil {
		t.Fatal(err)
	}
	return zone, site, original
}

func removeEmergencyAudience(s *Service, cause string) {
	for i := range s.view().System.Talkgroups {
		group := &s.view().System.Talkgroups[i]
		if group.TGID != 1001 {
			continue
		}
		switch cause {
		case "disabled":
			group.Enabled = false
		case "site-access":
			group.SiteAccess, group.SiteIDs = "selected", []string{}
		default:
			s.view().System.Talkgroups = append(s.view().System.Talkgroups[:i], s.view().System.Talkgroups[i+1:]...)
		}
		return
	}
}

func onlyEmergency(t *testing.T, s *Service) EmergencyIncident {
	t.Helper()
	rows := s.operatorSnapshot().Emergencies
	if len(rows) != 1 {
		t.Fatalf("expected retained incident, got %+v", rows)
	}
	return rows[0]
}

func TestRFEmergencyCompletionPropagatesAfterAudienceRemoval(t *testing.T) {
	for _, cause := range []string{"removed", "disabled", "site-access"} {
		t.Run(cause, func(t *testing.T) {
			zone, site, original := retainedRFPair(t)
			observer, _, _ := operatorFixture(t, RoleSite)
			if err := observer.acceptOperatorSnapshot(zone.operatorSnapshot()); err != nil {
				t.Fatal(err)
			}
			for _, s := range []*Service{zone, site, observer} {
				removeEmergencyAudience(s, cause)
			}
			e := radio.ParseEvent("I: (RF) P25, TSDU (Trunking System Data Unit), Emergency Alarm Cancel Accepted, srcId = 10001, dstId = 1001")
			if e == nil {
				t.Fatal("accepted native cancellation did not parse")
			}
			site.radioEvent(*e)
			cancelled := onlyEmergency(t, site)
			if err := zone.mergeSiteOperatorSnapshot("site-1", site.operatorLocalSnapshot()); err != nil {
				t.Fatal(err)
			}
			if got := onlyEmergency(t, zone); !sameRFIncident(got, original) || got.ClearedAt != cancelled.ClearedAt || got.ClearedBy != "radio" {
				t.Fatalf("Site cancellation did not clear its Zone record: %+v", got)
			}
			if err := observer.acceptOperatorSnapshot(zone.operatorSnapshot()); err != nil {
				t.Fatal(err)
			}
			if got := onlyEmergency(t, observer); !sameRFIncident(got, original) || got.ClearedAt != cancelled.ClearedAt {
				t.Fatalf("Zone completion did not reach the retained Site record: %+v", got)
			}
			stale := OperatorSnapshot{Emergencies: []EmergencyIncident{original}}
			if err := zone.mergeSiteOperatorSnapshot("site-1", stale); err != nil {
				t.Fatal(err)
			}
			for _, value := range []OperatorSnapshot{stale, emptyOperatorSnapshot()} {
				if err := observer.acceptOperatorSnapshot(value); err != nil {
					t.Fatal(err)
				}
				if got := onlyEmergency(t, observer); got.ClearedAt != cancelled.ClearedAt {
					t.Fatal("stale/omitted snapshot lost completion", got)
				}
			}
			// Re-enabling the group cannot let a stale active copy revive it.
			observer.view().System = userFixtureSystem()
			if err := observer.acceptOperatorSnapshot(stale); err != nil {
				t.Fatal(err)
			}
			if got := onlyEmergency(t, observer); got.ClearedAt != cancelled.ClearedAt {
				t.Fatal("availability restoration reactivated an old incident", got)
			}
		})
	}
}

func TestRFEmergencyAfterUpstreamClearCreatesNewIncident(t *testing.T) {
	for name, firstActive := range map[string]bool{"alarm": true, "cancellation": false} {
		t.Run(name, func(t *testing.T) {
			zone, desk, _ := operatorFixture(t, RoleZone)
			site, _, _ := operatorFixture(t, RoleSite)
			if err := site.observeRFEmergency(10001, 1001, true); err != nil {
				t.Fatal(err)
			}
			original := onlyEmergency(t, site)
			if err := zone.mergeSiteOperatorSnapshot("site-1", site.operatorLocalSnapshot()); err != nil {
				t.Fatal(err)
			}
			operatorApply(t, zone, "emergency", OperatorInput{Action: "clear", ID: original.ID}, desk)
			completed := onlyEmergency(t, zone)
			if err := site.acceptOperatorSnapshot(zone.operatorSnapshot()); err != nil {
				t.Fatal(err)
			}
			if !site.operatorLocalSnapshot().Emergencies[0].ClearedAt.IsZero() || site.activeEmergency(10001, 1001) {
				t.Fatal("fixture did not retain the stale local copy and completed upstream record")
			}
			// A new RF alarm must not reuse an identity already completed by Dispatch.
			if err := site.observeRFEmergency(10001, 1001, firstActive); err != nil {
				t.Fatal(err)
			}
			rows := site.operatorLocalSnapshot().Emergencies
			if !firstActive {
				if len(rows) != 1 || rows[0] != completed {
					t.Fatalf("late own cancellation changed the upstream completion: %+v", rows)
				}
				if err := site.observeRFEmergency(10001, 1001, true); err != nil {
					t.Fatal(err)
				}
				rows = site.operatorLocalSnapshot().Emergencies
			}
			if len(rows) != 2 || !site.activeEmergency(10001, 1001) {
				t.Fatalf("new RF alarm reused a completed incident: %+v", rows)
			}
			if rows[0] != completed || rows[1].ID == original.ID || !rows[1].ClearedAt.IsZero() {
				t.Fatalf("new RF alarm changed history or did not create an active identity: %+v", rows)
			}
			fresh := rows[1]
			if err := site.observeRFEmergency(10001, 1001, true); err != nil {
				t.Fatal(err)
			}
			if got := site.operatorLocalSnapshot().Emergencies; len(got) != 2 || got[1].ID != fresh.ID {
				t.Fatalf("repeated RF alarm created another incident: %+v", got)
			}
			if err := zone.mergeSiteOperatorSnapshot("site-1", site.operatorLocalSnapshot()); err != nil {
				t.Fatal(err)
			}
			if err := site.acceptOperatorSnapshot(zone.operatorSnapshot()); err != nil {
				t.Fatal(err)
			}
			if err := site.observeRFEmergency(10001, 1001, false); err != nil {
				t.Fatal(err)
			}
			rows = site.operatorLocalSnapshot().Emergencies
			if rows[0] != completed || rows[1].ID != fresh.ID || rows[1].ClearedAt.IsZero() || rows[1].ClearedBy != "radio" || site.activeEmergency(10001, 1001) {
				t.Fatalf("own cancellation did not complete only the new incident: %+v", rows)
			}
			cancelled := rows[1]
			if err := site.observeRFEmergency(10001, 1001, false); err != nil {
				t.Fatal(err)
			}
			if err := site.acceptOperatorSnapshot(OperatorSnapshot{Emergencies: []EmergencyIncident{original, fresh}}); err != nil {
				t.Fatal(err)
			}
			for _, got := range site.operatorSnapshot().Emergencies {
				if got.ID == original.ID && got != completed || got.ID == fresh.ID && got != cancelled {
					t.Fatalf("retry or stale snapshot changed completed history: %+v", got)
				}
			}
		})
	}
}

func TestUnavailableRFCompletionRequiresExactRetainedIdentity(t *testing.T) {
	changes := map[string]func(*EmergencyIncident){
		"id":          func(e *EmergencyIncident) { e.ID += "-new" },
		"scope":       func(e *EmergencyIncident) { e.ScopeID = "site-2" },
		"radio":       func(e *EmergencyIncident) { e.RadioID = 10003 },
		"group":       func(e *EmergencyIncident) { e.TalkgroupID = 1002 },
		"origin-time": func(e *EmergencyIncident) { e.At = e.At.Add(time.Nanosecond) },
		"source-kind": func(e *EmergencyIncident) { e.SourceKind = "application" },
	}
	for name, change := range changes {
		t.Run(name, func(t *testing.T) {
			zone, site, original := retainedRFPair(t)
			observer, _, _ := operatorFixture(t, RoleSite)
			if err := observer.acceptOperatorSnapshot(zone.operatorSnapshot()); err != nil {
				t.Fatal(err)
			}
			for _, s := range []*Service{zone, site, observer} {
				s.view().System.Talkgroups = nil
			}
			forged := original
			forged.AcknowledgedAt, forged.AcknowledgedBy = time.Now().UTC(), "wrong-owner"
			forged.ClearedAt, forged.ClearedBy = time.Now().UTC(), "wrong-owner"
			change(&forged)
			value := OperatorSnapshot{Emergencies: []EmergencyIncident{forged}}
			if err := zone.mergeSiteOperatorSnapshot("site-1", value); err != nil {
				t.Fatal(err)
			}
			if err := observer.acceptOperatorSnapshot(value); err != nil {
				t.Fatal(err)
			}
			for _, s := range []*Service{zone, observer} {
				got := onlyEmergency(t, s)
				if !sameRFIncident(got, original) || !got.ClearedAt.IsZero() || !got.AcknowledgedAt.IsZero() {
					t.Fatalf("identity substitution changed a retained RF incident: %+v", got)
				}
			}
		})
	}
}

func TestUnavailableRFUpdatesCopyOnlyMonotonicCompletion(t *testing.T) {
	zone, _, original := retainedRFPair(t)
	observer, _, _ := operatorFixture(t, RoleSite)
	if err := observer.acceptOperatorSnapshot(zone.operatorSnapshot()); err != nil {
		t.Fatal(err)
	}
	for _, s := range []*Service{zone, observer} {
		removeEmergencyAudience(s, "removed")
		for i := range s.view().System.Radios {
			if s.view().System.Radios[i].RID == original.RadioID {
				s.view().System.Radios[i].Enabled = false
			}
		}
	}
	update := original
	update.AcknowledgedAt, update.AcknowledgedBy = original.At.Add(time.Second), "operator-1"
	update.LastSeen, update.DeclaredBy, update.PositionID = original.At.Add(time.Hour), "forged-declaration", "forged-position"
	apply := func(value OperatorSnapshot) {
		t.Helper()
		if err := zone.mergeSiteOperatorSnapshot("site-1", value); err != nil {
			t.Fatal(err)
		}
		if err := observer.acceptOperatorSnapshot(value); err != nil {
			t.Fatal(err)
		}
	}
	apply(OperatorSnapshot{Emergencies: []EmergencyIncident{update}})
	for _, s := range []*Service{zone, observer} {
		got := onlyEmergency(t, s)
		if got.AcknowledgedAt != update.AcknowledgedAt || got.AcknowledgedBy != "operator-1" || !got.ClearedAt.IsZero() || got.LastSeen != original.LastSeen || got.DeclaredBy != original.DeclaredBy || got.PositionID != original.PositionID {
			t.Fatal("completion replaced non-completion metadata", got)
		}
	}
	update.ClearedAt, update.ClearedBy = original.At.Add(2*time.Second), "radio"
	apply(OperatorSnapshot{Emergencies: []EmergencyIncident{update}})
	for _, s := range []*Service{zone, observer} {
		if got := onlyEmergency(t, s); got.ClearedAt != update.ClearedAt || got.ClearedBy != "radio" {
			t.Fatal("clear did not follow acknowledgement", got)
		}
	}
	update.ClearedAt = update.ClearedAt.Add(time.Hour)
	apply(OperatorSnapshot{Emergencies: []EmergencyIncident{update}})
	apply(OperatorSnapshot{Emergencies: []EmergencyIncident{original}})
	for _, s := range []*Service{zone, observer} {
		if got := onlyEmergency(t, s); got.ClearedAt != original.At.Add(2*time.Second) {
			t.Fatal("stale retry changed completion time or reactivated incident", got)
		}
	}
}

func TestUnsupportedSnapshotsCannotCreateIncidentsOrClearForeignSite(t *testing.T) {
	for _, terminal := range []bool{false, true} {
		t.Run(map[bool]string{false: "active", true: "terminal"}[terminal], func(t *testing.T) {
			zone, _, _ := operatorFixture(t, RoleZone)
			site, _, _ := operatorFixture(t, RoleSite)
			zone.view().System.Talkgroups, site.view().System.Talkgroups = nil, nil
			now := time.Now().UTC()
			e := EmergencyIncident{ID: "unknown-rf", RadioID: 10001, TalkgroupID: 1001, ScopeID: "site-1", SourceKind: "rf", At: now, LastSeen: now}
			if terminal {
				e.ClearedAt, e.ClearedBy = now, "radio"
			}
			value := OperatorSnapshot{Emergencies: []EmergencyIncident{e}}
			if err := zone.mergeSiteOperatorSnapshot("site-1", value); err != nil {
				t.Fatal(err)
			}
			if err := site.acceptOperatorSnapshot(value); err != nil {
				t.Fatal(err)
			}
			if len(zone.operatorSnapshot().Emergencies) != 0 || len(site.operatorSnapshot().Emergencies) != 0 {
				t.Fatal("unsupported fresh incident entered retained state")
			}
			zone.view().System, site.view().System = userFixtureSystem(), userFixtureSystem()
			e.ClearedAt, e.ClearedBy = time.Time{}, ""
			value.Emergencies[0] = e
			if err := zone.mergeSiteOperatorSnapshot("site-1", value); err != nil {
				t.Fatal(err)
			}
			if err := site.acceptOperatorSnapshot(value); err != nil {
				t.Fatal(err)
			}
			if !onlyEmergency(t, zone).ClearedAt.IsZero() || !onlyEmergency(t, site).ClearedAt.IsZero() {
				t.Fatal("normal fresh active admission changed")
			}
		})
	}
	zone, _, _ := operatorFixture(t, RoleZone)
	zone.view().Zone.Sites = append(zone.view().Zone.Sites, SiteIdentity{ID: "site-2", SiteID: 2, NAC: 0x293})
	now := time.Now().UTC()
	foreign := EmergencyIncident{ID: "foreign-rf", RadioID: 10001, TalkgroupID: 1001, ScopeID: "site-2", SourceKind: "rf", At: now, LastSeen: now}
	if err := zone.mergeSiteOperatorSnapshot("site-2", OperatorSnapshot{Emergencies: []EmergencyIncident{foreign}}); err != nil {
		t.Fatal(err)
	}
	foreign.ClearedAt, foreign.ClearedBy = now.Add(time.Second), "wrong-site"
	if err := zone.mergeSiteOperatorSnapshot("site-1", OperatorSnapshot{Emergencies: []EmergencyIncident{foreign}}); err != nil {
		t.Fatal(err)
	}
	if got := onlyEmergency(t, zone); !got.ClearedAt.IsZero() {
		t.Fatal("reporting Site cleared another Site's existing incident", got)
	}
}

func TestUnavailableApplicationIncidentDoesNotUseRFCompletionException(t *testing.T) {
	zone, _, _ := operatorFixture(t, RoleZone)
	site, _, _ := operatorFixture(t, RoleSite)
	now := time.Now().UTC()
	original := EmergencyIncident{ID: "app-incident", RadioID: 10003, TalkgroupID: 1001, ScopeID: "site-1", SourceKind: "application", At: now, LastSeen: now}
	if err := zone.mergeSiteOperatorSnapshot("site-1", OperatorSnapshot{Emergencies: []EmergencyIncident{original}}); err != nil {
		t.Fatal(err)
	}
	site.featureMu.Lock()
	err := site.writeOperatorCacheLocked("operator-local", OperatorSnapshot{Emergencies: []EmergencyIncident{original}})
	site.featureMu.Unlock()
	if err != nil {
		t.Fatal(err)
	}
	zone.view().System.Talkgroups, site.view().System.Talkgroups = nil, nil
	update := original
	update.ClearedAt, update.ClearedBy = now.Add(time.Second), "radio"
	if err := zone.mergeSiteOperatorSnapshot("site-1", OperatorSnapshot{Emergencies: []EmergencyIncident{update}}); err != nil {
		t.Fatal(err)
	}
	if err := site.acceptOperatorSnapshot(OperatorSnapshot{Emergencies: []EmergencyIncident{update}}); err != nil {
		t.Fatal(err)
	}
	for _, s := range []*Service{zone, site} {
		if got := onlyEmergency(t, s); !got.ClearedAt.IsZero() || got.SourceKind != "application" {
			t.Fatal("RF exception cleared an unavailable application incident", got)
		}
	}
}
