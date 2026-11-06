package framework

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"slices"
	"testing"
	"time"

	"open25.local/open25-fne/internal/activity"
	"open25.local/open25-fne/internal/radio"
)

func unitsZoneFixture(t *testing.T) (*Service, operatorAuthority, time.Time) {
	t.Helper()
	s, desk, _ := operatorFixture(t, RoleZone)
	v := s.view()
	v.Zone.Sites = []SiteIdentity{{ID: "site-1", Name: "North"}, {ID: "site-2", Name: "South"}}
	v.System.Radios = []Radio{{RID: 10001, Alias: "Patrol one", Kind: "subscriber", Enabled: true}, {RID: 10004, Alias: "Private unit", Kind: "subscriber", Enabled: true}, {RID: 10005, Alias: "Disabled unit", Kind: "subscriber"}, {RID: 10002, Alias: "Dispatch desk", Kind: "console", Enabled: true}, {RID: 10003, Alias: "Mobile app", Kind: "mobile", Enabled: true}}
	desk.user.Talkgroups = []int{1001}
	now := time.Now().UTC()
	v.Reports = []SiteReport{{ID: "site-1", ReceivedAt: now, Radio: &radio.Status{SiteID: "site-1", State: "running", Affiliations: []radio.Affiliation{
		{RID: 10001, TGID: 1001, At: now.Add(-time.Hour)},
		{RID: 10004, TGID: 1002, At: now.Add(-time.Minute)},
		{RID: 10005, TGID: 1001, At: now.Add(-time.Minute)},
		{RID: 10002, TGID: 1001, At: now.Add(-time.Minute)},
		{RID: 10003, TGID: 1001, At: now.Add(-time.Minute)},
		{RID: 10199, TGID: 1001, At: now.Add(-time.Minute)},
	}}}}
	// Reports retain virtual/unknown rows for the UI filtering assertions;
	// only configured physical subscribers can enter control authority.
	publishFixtureRFState(t, s, "site-1", radio.Status{Affiliations: slices.Clone(v.Reports[0].Radio.Affiliations[:3])})
	return s, desk, now
}

func TestOperatorUnitsShowOnlyCurrentPermittedRFDirectoryEntries(t *testing.T) {
	s, desk, _ := unitsZoneFixture(t)
	units, err := s.operatorUnits(desk, 1001)
	if err != nil || len(units) != 1 || units[0].RadioID != 10001 || units[0].Alias != "Patrol one" || units[0].SiteID != "site-1" || units[0].SiteName != "North" || units[0].TalkgroupID != 1001 || units[0].TalkgroupName == "" || units[0].AffiliatedAt.IsZero() || units[0].ObservedAt.IsZero() {
		t.Fatalf("unit listing lost trusted scope or identities: %+v %v", units, err)
	}
	if _, err := s.operatorUnits(desk, 1002); err == nil {
		t.Fatal("unpermitted resource was accepted")
	}
	all, err := s.operatorUnits(desk, 0)
	if err != nil || len(all) != 1 {
		t.Fatal("omitting the resource filter exposed another talkgroup", all, err)
	}
	// The list is not part of operator state or durable/replicated snapshots.
	state := s.operatorSnapshot()
	if len(state.Units) != 0 {
		t.Fatal("unit directory was mixed into the general operator snapshot")
	}
	units[0].Alias = "edited in caller"
	again, _ := s.operatorUnits(desk, 1001)
	if again[0].Alias != "Patrol one" {
		t.Fatal("a caller changed accepted directory state")
	}
}

func TestOperatorUnitsCannotBecomeRetainedOrReplicatedOperatorState(t *testing.T) {
	s := &Service{Store: storeOpen(t, RoleApplication), config: FileConfig{Role: RoleApplication, InstanceID: "application-1"}}
	if err := s.acceptOperatorSnapshot(OperatorSnapshot{Units: []OperatorUnit{{RadioID: 10001, Alias: "Never retain this directory"}}}); err != nil {
		t.Fatal(err)
	}
	raw, _, err := s.Store.ReadCache("operator-upstream")
	if err != nil || bytes.Contains(raw, []byte("Never retain")) || bytes.Contains(raw, []byte("units")) {
		t.Fatal("an incoming directory became durable operator state", string(raw), err)
	}
	if len(s.operatorSnapshot().Units) != 0 {
		t.Fatal("a cached operator snapshot advertised current affiliations")
	}
}

func TestOperatorUnitsRejectStaleOfflineAndUnconfirmedReports(t *testing.T) {
	for _, test := range []struct {
		name  string
		alter func(*directorySnapshot, time.Time)
	}{
		{"stale report", func(v *directorySnapshot, now time.Time) { v.Reports[0].ReceivedAt = now.Add(-5 * time.Second) }},
		{"missing report time", func(v *directorySnapshot, _ time.Time) { v.Reports[0].ReceivedAt = time.Time{} }},
		{"future report", func(v *directorySnapshot, now time.Time) { v.Reports[0].ReceivedAt = now.Add(10 * time.Second) }},
		{"offline site", func(v *directorySnapshot, _ time.Time) { v.Reports[0].Radio.State = "stopped" }},
		{"missing radio state", func(v *directorySnapshot, _ time.Time) { v.Reports[0].Radio = nil }},
		{"failed report", func(v *directorySnapshot, _ time.Time) { v.Reports[0].Error = "unreachable" }},
		{"different Site radio", func(v *directorySnapshot, _ time.Time) { v.Reports[0].Radio.SiteID = "site-2" }},
		{"missing affiliation time", func(v *directorySnapshot, _ time.Time) { v.Reports[0].Radio.Affiliations[0].At = time.Time{} }},
		{"future affiliation", func(v *directorySnapshot, now time.Time) {
			v.Reports[0].Radio.Affiliations[0].At = now.Add(2 * time.Minute)
		}},
		{"unknown site", func(v *directorySnapshot, _ time.Time) { v.Reports[0].ID = "unenrolled-site" }},
		{"restricted site", func(v *directorySnapshot, _ time.Time) {
			v.System.Talkgroups[0].SiteAccess, v.System.Talkgroups[0].SiteIDs = "selected", []string{"site-2"}
		}},
	} {
		t.Run(test.name, func(t *testing.T) {
			s, desk, now := unitsZoneFixture(t)
			test.alter(s.view(), now)
			units, err := s.operatorUnits(desk, 1001)
			if err != nil || len(units) != 0 {
				t.Fatalf("unconfirmed/offline subscriber appeared current: %+v %v", units, err)
			}
		})
	}
	s, desk, _ := unitsZoneFixture(t)
	s.rfAuthority.close()
	if _, err := s.operatorUnits(desk, 1001); err == nil {
		t.Fatal("missing ownership state was reported as a current directory")
	}
}

func TestOperatorUnitsFollowNewestOwnershipWithoutRevivingOldSites(t *testing.T) {
	s, desk, now := unitsZoneFixture(t)
	v := s.view()
	v.Reports = append(v.Reports, SiteReport{ID: "site-2", ReceivedAt: now, Radio: &radio.Status{SiteID: "site-2", State: "running", Affiliations: []radio.Affiliation{{RID: 10001, TGID: 1001, At: now.Add(-time.Second)}}}})
	publishFixtureRFState(t, s, "site-2", *v.Reports[1].Radio)
	units, err := s.operatorUnits(desk, 1001)
	if err != nil || len(units) != 1 || units[0].SiteID != "site-2" {
		t.Fatal("roaming unit was duplicated or assigned to its old Site", units, err)
	}
	v.Reports[1].ReceivedAt = now.Add(-5 * time.Second)
	units, err = s.operatorUnits(desk, 1001)
	if err != nil || len(units) != 0 {
		t.Fatal("lost latest Site revived retained old affiliation", units, err)
	}
	v.Reports[1].ReceivedAt = now
	v.Presence = []activity.Radio{{RID: 10001, SiteID: "site-2", Kind: "rf", Registered: false, LastSeen: now}}
	units, err = s.operatorUnits(desk, 1001)
	if err != nil || len(units) != 1 || units[0].SiteID != "site-2" {
		t.Fatal("history upload changed accepted physical ownership", units, err)
	}
	v.Presence = []activity.Radio{{RID: 10001, SiteID: "site-2", Kind: "application", Registered: false, LastSeen: now}}
	units, err = s.operatorUnits(desk, 1001)
	if err != nil || len(units) != 1 || units[0].SiteID != "site-2" {
		t.Fatal("application evidence changed physical ownership", units, err)
	}
	terminalAt := time.Now().UTC()
	update := rfAuthorityUpdate{SiteID: "site-2", NativeStartedAt: s.rfAuthority.value.Load().document.NativeRuns["site-2"], ObservedAt: terminalAt, Terminals: []radio.Registration{{RID: 10001, At: terminalAt}}}
	if err := s.rfAuthority.apply(context.Background(), update, s.rfAuthorityScope()); err != nil {
		t.Fatal(err)
	}
	units, err = s.operatorUnits(desk, 1001)
	if err != nil || len(units) != 0 {
		t.Fatal("accepted control deregistration left subscriber current", units, err)
	}
	v.Presence = nil
	v.Reports[1].Radio.Affiliations[0].At = time.Now().UTC()
	publishFixtureRFState(t, s, "site-2", *v.Reports[1].Radio)
	units, err = s.operatorUnits(desk, 1001)
	if err != nil || len(units) != 1 || units[0].SiteID != "site-2" {
		t.Fatal("new accepted control membership did not restore the unit", units, err)
	}
	v.Reports[0].Radio.Affiliations[0].At = v.Reports[1].Radio.Affiliations[0].At
	publishFixtureRFState(t, s, "site-1", radio.Status{Affiliations: []radio.Affiliation{v.Reports[0].Radio.Affiliations[0]}})
	units, err = s.operatorUnits(desk, 1001)
	if err != nil || len(units) != 0 {
		t.Fatal("ambiguous newest affiliation chose an arbitrary Site", units, err)
	}
}

func TestZoneOperatorUnitsDoNotFallBackToReportsWhenAuthorityIsUnavailableOrFenced(t *testing.T) {
	for _, test := range []struct {
		name  string
		alter func(*Service)
	}{
		{"missing authority", func(s *Service) { s.rfAuthority = nil }},
		{"closed authority", func(s *Service) { s.rfAuthority.close() }},
		{"pending source fence", func(s *Service) {
			accepted := s.rfAuthority.value.Load()
			// Explicit immutable fault fixture: accepted owners remain visible
			// internally, but pending commit fencing must exclude this RID.
			pending := *accepted
			pending.fenced = map[int]bool{10001: true}
			s.rfAuthority.value.Store(&pending)
		}},
	} {
		t.Run(test.name, func(t *testing.T) {
			s, desk, _ := unitsZoneFixture(t)
			test.alter(s)
			units, err := s.operatorUnits(desk, 1001)
			unavailable := test.name != "pending source fence"
			if len(units) != 0 || (unavailable && err == nil) || (!unavailable && err != nil) {
				t.Fatalf("current report substituted for unavailable/fenced authority: %+v %v", units, err)
			}
		})
	}
}

func TestSiteOperatorUnitsStayLocalAndRejectStoppedRadio(t *testing.T) {
	s, native := operationFixture(t)
	native.mu.Lock()
	native.status.Affiliations = []radio.Affiliation{{RID: 10001, TGID: 1001, At: time.Now().UTC()}}
	native.mu.Unlock()
	authority := operatorAuthority{user: User{Talkgroups: []int{1001}}}
	units, err := s.operatorUnits(authority, 1001)
	if err != nil || len(units) != 1 || units[0].SiteID != "site-1" {
		t.Fatal("local current affiliation was unavailable", units, err)
	}
	native.mu.Lock()
	native.status.State = "stopped"
	native.mu.Unlock()
	if _, err := s.operatorUnits(authority, 1001); err == nil {
		t.Fatal("stopped Site advertised retained units")
	}
}

func TestDispatchUnitsUseAuthenticatedControllerScopeAndRejectForgedQueries(t *testing.T) {
	c := newMediaCluster(t)
	client := historyProxyClient(t, c)
	now := time.Now().UTC()
	view := *c.zone.service.view()
	view.Reports = []SiteReport{{ID: "site-1", ReceivedAt: now, Radio: &radio.Status{SiteID: "site-1", State: "running", Affiliations: []radio.Affiliation{{RID: 10001, TGID: 1001, At: now}}}}, {ID: "site-2", ReceivedAt: now, Radio: &radio.Status{SiteID: "site-2", State: "running", Affiliations: []radio.Affiliation{{RID: 10003, TGID: 1002, At: now}}}}}
	view.Presence = nil
	c.zone.service.directory.value.Store(&view)
	for _, report := range view.Reports {
		publishFixtureRFState(t, c.zone.service, report.ID, *report.Radio)
	}
	status, raw := client.request(t, "GET", "/api/operator/units?talkgroup_id=1001", nil)
	var reply OperatorSnapshot
	if status != 200 || json.Unmarshal(raw, &reply) != nil || len(reply.Units) != 1 || reply.Units[0].RadioID != 10001 {
		t.Fatalf("authenticated scoped list did not reach the connected Zone: %d %s", status, raw)
	}
	for _, secret := range []string{"Remote portable", "Console two", "password_hash", "proof", "token", "coordinates"} {
		if bytes.Contains(raw, []byte(secret)) {
			t.Fatal("unrelated or private directory data escaped", secret)
		}
	}
	for _, query := range []string{"?talkgroup_id=1002", "?talkgroup_id=65535", "?talkgroup_id=0", "?talkgroup_id=1001&talkgroup_id=1002", "?site_id=site-2", "?talkgroup_id=oops"} {
		if code, _ := client.request(t, "GET", "/api/operator/units"+query, nil); code == 200 {
			t.Fatal("unpermitted or malformed directory query accepted", query)
		}
	}
	// A service's token is not the person's current authority.
	credentials, _ := readCredentials(c.dispatch.service.Config().CredentialFile)
	status, _ = integrationHTTP(t, http.DefaultClient, "POST", c.zone.server.URL+"/internal/operator/units", operatorEnvelope{Actor: OperatorActor{UserID: c.users[0].ID, RadioID: 10002, Proof: "forged"}, Input: OperatorInput{TalkgroupID: 1001}}, map[string]string{"X-Service-ID": credentials.OutboundID, "Authorization": "Bearer " + credentials.OutboundToken})
	if status != 403 {
		t.Fatal("forged directory actor accepted", status)
	}
	// The same read follows the actual connected controller during Site
	// Trunking. A disconnected Dispatch never substitutes a retained unit list.
	local := c.radios["site-1"]
	local.mu.Lock()
	local.status.Affiliations = []radio.Affiliation{{RID: 10001, TGID: 1001, At: time.Now().UTC()}}
	local.mu.Unlock()
	c.zone.service.peers.Close()
	c.zone.server.Close()
	c.system.server.Close()
	mediaWait(t, 5*time.Second, func() bool { return c.dispatch.service.mediaClient.URL() == c.site.server.URL }, "unit directory Site fallback")
	status, raw = client.request(t, "GET", "/api/operator/units?talkgroup_id=1001", nil)
	if status != 200 || json.Unmarshal(raw, &reply) != nil || len(reply.Units) != 1 || reply.Units[0].SiteID != "site-1" {
		t.Fatalf("fallback list did not use its current local Site: %d %s", status, raw)
	}
	c.site.service.peers.Close()
	c.site.server.Close()
	mediaWait(t, 5*time.Second, func() bool { return c.dispatch.service.mediaClient.URL() == "" }, "unit directory controller disconnect")
	if code, _ := client.request(t, "GET", "/api/operator/units?talkgroup_id=1001", nil); code != 503 {
		t.Fatal("disconnected Dispatch advertised stale units", code)
	}
}

func TestResourceScopedCommandRejectsChangedTalkgroupOrSite(t *testing.T) {
	s, native := operationFixture(t)
	now := time.Now().UTC()
	native.mu.Lock()
	native.status.Affiliations = []radio.Affiliation{{RID: 10001, TGID: 1001, At: now}}
	native.mu.Unlock()
	actor := operatorAuthority{actor: OperatorActor{UserID: "person"}, user: User{RadioID: 10002, Talkgroups: []int{1001, 1002}, Permissions: []string{"radio_check"}}}
	units, _ := s.operatorUnits(actor, 1001)
	if len(units) != 1 {
		t.Fatal("no initial selectable subscriber")
	}
	native.mu.Lock()
	native.status.Affiliations[0].TGID = 1002
	native.mu.Unlock()
	if _, err := s.operatorRadioCommand(OperatorInput{Action: "radio_check", RadioID: 10001, TalkgroupID: 1001, SiteID: "site-1"}, actor); err == nil {
		t.Fatal("resource-scoped command followed a subscriber onto another talkgroup")
	}
	native.mu.Lock()
	native.status.Affiliations[0].TGID = 1001
	native.mu.Unlock()
	if _, err := s.operatorRadioCommand(OperatorInput{Action: "radio_check", RadioID: 10001, TalkgroupID: 1001, SiteID: "site-2"}, actor); err == nil {
		t.Fatal("stale Site selection was treated as routing authority")
	}
	native.mu.Lock()
	defer native.mu.Unlock()
	if len(native.commands) != 0 {
		t.Fatal("changed target reached the radio")
	}
}

// Changes after the controller's initial resolver must still fail at the last
// local check, immediately before the native send.
type movingUnitsRadio struct {
	*operationRadio
	reads int
}

func (r *movingUnitsRadio) Status() radio.Status {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.reads++
	copy := r.status
	copy.Affiliations = slices.Clone(r.status.Affiliations)
	if r.reads > 1 {
		copy.Affiliations[0].TGID = 1002
	}
	return copy
}

func TestUnitListSendRaceIsRejectedAtFinalNativeBoundary(t *testing.T) {
	s, native := operationFixture(t)
	s.operationCancel()
	s.operationWG.Wait()
	native.mu.Lock()
	native.status.Affiliations = []radio.Affiliation{{RID: 10001, TGID: 1001, At: time.Now().UTC()}}
	native.mu.Unlock()
	s.radio = &movingUnitsRadio{operationRadio: native}
	actor := operatorAuthority{actor: OperatorActor{UserID: "person"}, user: User{RadioID: 10002, Talkgroups: []int{1001, 1002}, Permissions: []string{"radio_check"}}}
	if _, err := s.operatorRadioCommand(OperatorInput{ID: "changed-before-native", Action: "radio_check", RadioID: 10001, TalkgroupID: 1001, SiteID: "site-1"}, actor); err == nil {
		t.Fatal("radio changed affiliation after admission but the command still sent")
	}
	native.mu.Lock()
	count := len(native.commands)
	native.mu.Unlock()
	if count != 0 {
		t.Fatal("last-moment affiliation change reached native send")
	}
	rows, _ := s.Store.radioCommands()
	if len(rows) != 1 || rows[0].State != "rejected" {
		t.Fatal("final target rejection was not retained", rows)
	}
}

func TestOperatorUnitsNeverAcceptBrowserSuppliedAuthority(t *testing.T) {
	s, desk, _ := unitsZoneFixture(t)
	body, _ := json.Marshal(operatorEnvelope{Actor: desk.actor, Input: OperatorInput{TalkgroupID: 1001}})
	r := httptest.NewRequest("POST", "/internal/operator/units", bytes.NewReader(body))
	r.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()
	s.operatorInternal(w, r, PeerCredential{Role: RoleSystem, InstanceID: "system-1"})
	if w.Code != 403 {
		t.Fatal("non-enrolled operator boundary supplied directory authority", w.Code)
	}
}
