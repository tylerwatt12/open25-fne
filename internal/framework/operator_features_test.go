package framework

import (
	"bytes"
	"encoding/json"
	"fmt"
	"math"
	"net/http"
	"net/http/httptest"
	"open25.local/open25-fne/internal/dispatch"
	"open25.local/open25-fne/internal/routing"
	"slices"
	"testing"
	"time"
)

func operatorFixture(t *testing.T, role Role) (*Service, operatorAuthority, operatorAuthority) {
	t.Helper()
	id := "zone-1"
	if role == RoleSite {
		id = "site-1"
	}
	s := &Service{Store: storeOpen(t, role), config: FileConfig{Role: role, InstanceID: id}}
	system, zone := userFixtureSystem(), storeFixtureZone()
	desk := fixtureUser()
	desk.Permissions = []string{"ptt", "secure_ptt", "emergency", "emergency_ack", "emergency_clear", "location_report", "location_view", "patch"}
	mobile := cloneUser(t, desk)
	mobile.ID, mobile.Username, mobile.Role, mobile.RadioID = "mobile-1", "mobile-one", "mobile", 10003
	mobile.PositionIDs, mobile.Profile.PreferredPositionID = []string{}, ""
	hash := []byte("operator-test-verifier")
	s.directory.value.Store(&directorySnapshot{System: system, Zone: &zone, Users: map[string]userRecord{
		desk.ID: {User: desk, PasswordHash: hash}, mobile.ID: {User: mobile, PasswordHash: hash},
	}})
	_ = s.currentRadioInhibition()
	if role == RoleSite {
		_ = s.siteActualInhibition()
	}
	deskAuthority, err := s.operatorActor(OperatorActor{UserID: desk.ID, Proof: userProof(desk, hash), RadioID: 10002})
	if err != nil {
		t.Fatal(err)
	}
	mobileAuthority, err := s.operatorActor(OperatorActor{UserID: mobile.ID, Proof: userProof(mobile, hash), RadioID: 10003})
	if err != nil {
		t.Fatal(err)
	}
	return s, deskAuthority, mobileAuthority
}

func operatorApply(t *testing.T, s *Service, topic string, input OperatorInput, actor operatorAuthority) OperatorSnapshot {
	t.Helper()
	value, err := s.applyOperator(topic, input, actor)
	if err != nil {
		t.Fatal(err)
	}
	return value
}

func TestOperatorEmergencySharedLifecycleAndHistory(t *testing.T) {
	s, desk, mobile := operatorFixture(t, RoleZone)
	declared := operatorApply(t, s, "emergency", OperatorInput{Action: "declare", ID: "incident-1", TalkgroupID: 1001}, mobile)
	if len(declared.Emergencies) != 1 || !s.activeEmergency(10003, 1001) {
		t.Fatal("own emergency was not shared")
	}
	if e := declared.Emergencies[0]; e.DeclaredBy != mobile.user.ID || e.RadioID != 10003 || e.ScopeID != "zone-1" {
		t.Fatalf("lost actor/identity: %#v", e)
	}
	operatorApply(t, s, "emergency", OperatorInput{Action: "acknowledge", ID: "incident-1"}, desk)
	cleared := operatorApply(t, s, "emergency", OperatorInput{Action: "clear", ID: "incident-1"}, desk)
	if s.activeEmergency(10003, 1001) || len(cleared.Emergencies) != 1 || cleared.Emergencies[0].AcknowledgedBy != desk.user.ID || cleared.Emergencies[0].ClearedBy != desk.user.ID {
		t.Fatal("acknowledge/clear lost the incident history")
	}
	merged := mergeOperatorSnapshots(cleared, declared)
	if merged.Emergencies[0].ClearedAt.IsZero() {
		t.Fatal("a stale active snapshot resurrected a cleared emergency")
	}
	if _, err := s.applyOperator("emergency", OperatorInput{Action: "declare", RadioID: 10001, TalkgroupID: 1001}, mobile); err == nil {
		t.Fatal("mobile declared another radio's emergency")
	}
	limited := desk
	limited.user.Talkgroups = []int{1002}
	if _, err := s.applyOperator("emergency", OperatorInput{Action: "clear", ID: "incident-1"}, limited); err == nil {
		t.Fatal("dispatcher cleared an unauthorized group")
	}
	if len(s.filterOperatorState(cleared, limited).Emergencies) != 0 {
		t.Fatal("unauthorized emergency leaked into state")
	}
	operatorApply(t, s, "emergency", OperatorInput{Action: "declare", ID: "incident-2", TalkgroupID: 1001}, mobile)
	if len(s.operatorSnapshot().Emergencies) != 2 {
		t.Fatal("a subsequent emergency overwrote its predecessor")
	}
}

func TestOperatorLocationOwnIdentityFreshnessAndPrivacy(t *testing.T) {
	s, desk, mobile := operatorFixture(t, RoleZone)
	now := time.Now().UTC()
	input := OperatorInput{Action: "report", TalkgroupID: 1001, Location: &LocationInput{Latitude: 41.5, Longitude: -81.7, Accuracy: 8, FixAt: now}}
	value := operatorApply(t, s, "location", input, mobile)
	if len(value.Locations) != 1 || value.Locations[0].RadioID != 10003 || value.Locations[0].SourceKind != "application" {
		t.Fatal("phone fix did not use its authenticated identity")
	}
	cases := []struct {
		name  string
		input OperatorInput
	}{
		{"other radio", OperatorInput{Action: "report", RadioID: 10001, TalkgroupID: 1001, Location: input.Location}},
		{"disabled talkgroup", OperatorInput{Action: "report", TalkgroupID: 1099, Location: input.Location}},
		{"old fix", OperatorInput{Action: "report", TalkgroupID: 1001, Location: &LocationInput{FixAt: now.Add(-6 * time.Minute)}}},
		{"older than stored", OperatorInput{Action: "report", TalkgroupID: 1001, Location: &LocationInput{FixAt: now.Add(-time.Second)}}},
		{"future fix", OperatorInput{Action: "report", TalkgroupID: 1001, Location: &LocationInput{FixAt: now.Add(time.Minute)}}},
		{"invalid coordinates", OperatorInput{Action: "report", TalkgroupID: 1001, Location: &LocationInput{Latitude: 91}}},
		{"not finite", OperatorInput{Action: "report", TalkgroupID: 1001, Location: &LocationInput{Longitude: math.NaN()}}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if _, err := s.applyOperator("location", tc.input, mobile); err == nil {
				t.Fatal("invalid position accepted")
			}
		})
	}
	viewer := desk
	viewer.user.Permissions = []string{"ptt"}
	if len(s.filterOperatorState(s.operatorSnapshot(), viewer).Locations) != 0 {
		t.Fatal("another radio's coordinates leaked without location permission")
	}
	if len(s.filterOperatorState(s.operatorSnapshot(), desk).Locations) != 1 {
		t.Fatal("authorized dispatcher could not view a permitted radio position")
	}
	input.Location.FixAt = now.Add(time.Second)
	input.Location.Latitude = 42
	operatorApply(t, s, "location", input, mobile)
	if got := s.operatorSnapshot().Locations; len(got) != 1 || got[0].Latitude != 42 {
		t.Fatal("latest fix did not replace the previous point")
	}
}

func TestOperatorPatchSurvivesServiceAndRequiresEveryMemberPermission(t *testing.T) {
	s, desk, _ := operatorFixture(t, RoleZone)
	input := OperatorInput{Action: "activate", ID: "patch-1", Patch: &PatchInput{Name: "Test patch", Talkgroups: []int{1002, 1001}}}
	limited := desk
	limited.user.Talkgroups = []int{1001}
	if _, err := s.applyOperator("patch", input, limited); err == nil {
		t.Fatal("one authorized member allowed an unauthorized patch member")
	}
	value := operatorApply(t, s, "patch", input, desk)
	if len(value.Patches) != 1 || value.Patches[0].CreatedBy != desk.user.ID || value.Patches[0].CreatedRadioID != 10002 || value.Patches[0].PositionID != "desk-1" {
		t.Fatal("patch actor and desk were not retained")
	}
	restarted := &Service{Store: s.Store, config: s.Config()}
	restarted.directory.value.Store(s.view())
	// Normal service startup loads retained call policy before admitting audio.
	restarted.refreshOperatorPolicy()
	groups, err := restarted.expandPatchedGroups([]int{1001})
	if err != nil || !slices.Equal(groups, []int{1001, 1002}) {
		t.Fatal("patch depended on browser or Dispatch lifetime", groups, err)
	}
	input.ID = "patch-overlap"
	if _, err = s.applyOperator("patch", input, desk); err == nil {
		t.Fatal("overlapping or nested patch accepted")
	}
	ended := operatorApply(t, s, "patch", OperatorInput{Action: "end", ID: "patch-1"}, desk)
	if len(s.activePatches()) != 0 || ended.Patches[0].EndedAt.IsZero() {
		t.Fatal("explicit end did not stop routing")
	}
	if got := mergeOperatorSnapshots(ended, value); got.Patches[0].Active {
		t.Fatal("stale active patch resurrected after end")
	}
}

func TestOperatorPatchSharedKeyPolicy(t *testing.T) {
	s, desk, _ := operatorFixture(t, RoleZone)
	for i := range s.view().System.Talkgroups {
		if s.view().System.Talkgroups[i].Enabled {
			s.view().System.Talkgroups[i].AdmissionMode = "secure"
			s.view().System.Talkgroups[i].TransmitMode = "secure"
			s.view().System.Talkgroups[i].KeyID = 7
			s.view().System.Talkgroups[i].AlgorithmID = 132
		}
	}
	input := OperatorInput{Action: "activate", ID: "patch-secure", Patch: &PatchInput{Name: "Shared encrypted", Talkgroups: []int{1001, 1002}, AlgorithmID: 132, KeyID: 7}}
	clear := input
	clear.Patch = &PatchInput{Name: "Clear", Talkgroups: []int{1001, 1002}}
	if _, err := s.applyOperator("patch", clear, desk); err == nil {
		t.Fatal("secure-only groups were patched in clear")
	}
	noSecure := desk
	noSecure.user.Permissions = []string{"patch"}
	if _, err := s.applyOperator("patch", input, noSecure); err == nil {
		t.Fatal("encrypted patch bypassed secure permission")
	}
	s.view().System.Talkgroups[1].KeyID = 8
	operatorApply(t, s, "patch", input, desk)
	q := routing.Request{CallRequest: dispatch.CallRequest{Talkgroups: []int{1001}, Secure: true, AlgorithmID: 132, KeyID: 7}}
	if _, err := (&callBroker{s: s}).expandPatch(q); err != nil {
		t.Fatal("console default changed actual patch key admission", err)
	}
	q.KeyID = 8
	if _, err := (&callBroker{s: s}).expandPatch(q); err == nil {
		t.Fatal("patch translated between actual different keys")
	}
}

func TestOperatorSiteOutageAndZoneReconciliation(t *testing.T) {
	zone, zoneDesk, zoneMobile := operatorFixture(t, RoleZone)
	site, siteDesk, _ := operatorFixture(t, RoleSite)
	initial := operatorApply(t, zone, "emergency", OperatorInput{Action: "declare", ID: "zone-incident", TalkgroupID: 1001}, zoneMobile)
	if err := site.acceptOperatorSnapshot(initial); err != nil {
		t.Fatal(err)
	}
	operatorApply(t, site, "emergency", OperatorInput{Action: "clear", ID: "zone-incident"}, siteDesk)
	if err := zone.mergeSiteOperatorSnapshot("site-1", site.operatorLocalSnapshot()); err != nil {
		t.Fatal(err)
	}
	if zone.activeEmergency(10003, 1001) {
		t.Fatal("Site emergency clear was lost on reconnection")
	}
	operatorApply(t, site, "patch", OperatorInput{Action: "activate", ID: "site-patch", Patch: &PatchInput{Name: "Local outage", Talkgroups: []int{1001, 1002}}}, siteDesk)
	if len(site.activePatches()) != 1 {
		t.Fatal("local patch did not operate during outage")
	}
	if err := zone.mergeSiteOperatorSnapshot("site-1", site.operatorLocalSnapshot()); err != nil {
		t.Fatal(err)
	}
	if len(zone.activePatches()) != 0 {
		t.Fatal("a Site patch was promoted into a wide-area patch")
	}
	// A Zone cannot activate a conflicting patch until it explicitly resolves
	// the reported Site patch; here emulate a Zone patch created during isolation.
	wide := RuntimePatch{ID: "zone-patch", Name: "Wide", Talkgroups: []int{1001, 1002}, CreatedBy: zoneDesk.user.ID, CreatedRadioID: 10002, At: time.Now().UTC(), Active: true, ScopeID: "zone-1", State: "active"}
	upstream := zone.operatorSnapshot()
	upstream.Patches = append(upstream.Patches, wide)
	if err := site.acceptOperatorSnapshot(upstream); err != nil {
		t.Fatal(err)
	}
	active := site.activePatches()
	if len(active) != 1 || active[0].ID != "zone-patch" {
		t.Fatalf("overlap routed two independent patches: %#v", active)
	}
	if got := site.operatorLocalSnapshot().Patches[0]; got.State != "conflict" {
		t.Fatal("local conflict was not retained for an administrator")
	}
	if _, err := site.applyOperator("patch", OperatorInput{Action: "end", ID: "zone-patch"}, siteDesk); err == nil {
		t.Fatal("isolated Site ended a Zone-wide patch")
	}
	if err := site.acceptOperatorSnapshot(initial); err != nil {
		t.Fatal(err)
	}
	if site.activeEmergency(10003, 1001) {
		t.Fatal("stale Zone state undid a local emergency clear")
	}
}

func TestOperatorSiteReportCannotForgeOtherScopeOrLeakOtherSites(t *testing.T) {
	zone, _, _ := operatorFixture(t, RoleZone)
	bad := OperatorSnapshot{Emergencies: []EmergencyIncident{{ID: "invented", RadioID: 10003, TalkgroupID: 1001, ScopeID: "zone-1", At: time.Now(), LastSeen: time.Now()}}, Patches: []RuntimePatch{{ID: "forged", Name: "Forged", Talkgroups: []int{1001, 1002}, Active: true, ScopeID: "zone-1", At: time.Now()}}}
	if err := zone.mergeSiteOperatorSnapshot("unknown-site", bad); err == nil {
		t.Fatal("unknown Site submitted state")
	}
	if err := zone.mergeSiteOperatorSnapshot("site-1", bad); err != nil {
		t.Fatal(err)
	}
	if len(zone.operatorSnapshot().Patches) != 0 || len(zone.operatorSnapshot().Emergencies) != 0 {
		t.Fatal("Site created another controller's state")
	}
	site, _, _ := operatorFixture(t, RoleSite)
	bad.Emergencies[0].ScopeID = "site-2"
	bad.Patches[0].ScopeID = "site-2"
	if err := site.acceptOperatorSnapshot(bad); err != nil {
		t.Fatal(err)
	}
	if len(site.operatorSnapshot().Emergencies) != 0 || len(site.operatorSnapshot().Patches) != 0 {
		t.Fatal("other Site data appeared in a local Site console")
	}
}

func TestOperatorPrivateHTTPAuthenticationAndInputContract(t *testing.T) {
	s, desk, _ := operatorFixture(t, RoleZone)
	input := operatorEnvelope{Actor: desk.actor, Input: OperatorInput{Action: "declare", ID: "from-http", TalkgroupID: 1001}}
	call := func(envelope operatorEnvelope, peer PeerCredential) *httptest.ResponseRecorder {
		raw, _ := json.Marshal(envelope)
		req := httptest.NewRequest(http.MethodPost, "/internal/operator/emergency", bytes.NewReader(raw))
		req.Header.Set("Content-Type", "application/json")
		w := httptest.NewRecorder()
		s.operatorInternal(w, req, peer)
		return w
	}
	peer := PeerCredential{Role: RoleApplication, InstanceID: "application-1"}
	w := call(input, peer)
	if w.Code != 200 {
		t.Fatal(w.Code, w.Body.String())
	}
	if bytes.Contains(w.Body.Bytes(), []byte(desk.actor.Proof)) || bytes.Contains(w.Body.Bytes(), []byte("proof")) {
		t.Fatal("state exposed the private actor verifier")
	}
	input.Actor.Proof = "wrong"
	if got := call(input, peer); got.Code != 403 {
		t.Fatal("forged person proof accepted", got.Code)
	}
	input.Actor = desk.actor
	if got := call(input, PeerCredential{Role: RoleSystem, InstanceID: "system-1"}); got.Code != 403 {
		t.Fatal("configuration authority submitted operator mutation")
	}
	req := httptest.NewRequest(http.MethodPost, "/internal/operator/emergency", bytes.NewBufferString(`{"actor":{"user_id":"user-1"},"input":{"action":"declare","radio_id":10001,"talkgroup_id":1001,"permissions":["emergency"]}}`))
	req.Header.Set("Content-Type", "application/json")
	w = httptest.NewRecorder()
	s.operatorInternal(w, req, peer)
	if w.Code != 400 {
		t.Fatal("client supplied operator permissions in input", w.Code)
	}
	user := s.view().Users[desk.user.ID]
	user.User.Permissions = []string{"ptt"}
	s.view().Users[desk.user.ID] = user
	if got := call(input, peer); got.Code == 200 {
		t.Fatal("cached identity proof retained removed operation permissions", got.Code)
	}
}

func TestOperatorRFEmergencyRequiresLocalSubscriber(t *testing.T) {
	s, _, _ := operatorFixture(t, RoleSite)
	if err := s.observeRFEmergency(10003, 1001, true); err == nil {
		t.Fatal("mobile app identity treated as RF observation")
	}
	if err := s.observeRFEmergency(10001, 1099, true); err == nil {
		t.Fatal("disabled talkgroup accepted")
	}
	if err := s.observeRFEmergency(10001, 1001, true); err != nil {
		t.Fatal(err)
	}
	if !s.activeEmergency(10001, 1001) {
		t.Fatal("RF emergency not visible")
	}
	if err := s.observeRFEmergency(10001, 1001, false); err != nil {
		t.Fatal(err)
	}
	if s.activeEmergency(10001, 1001) || len(s.operatorSnapshot().Emergencies) != 1 {
		t.Fatal("RF clear did not preserve incident history")
	}
}

func TestCallAndPatchPreserveActualKeyIndependentlyOfConsoleDefaults(t *testing.T) {
	for _, algorithm := range []int{129, 132, 170} {
		t.Run(fmt.Sprintf("algorithm-%02x", algorithm), func(t *testing.T) {
			s, desk, _ := operatorFixture(t, RoleZone)
			for i := range s.view().System.Talkgroups {
				if s.view().System.Talkgroups[i].Enabled {
					s.view().System.Talkgroups[i].AdmissionMode = "secure"
					s.view().System.Talkgroups[i].TransmitMode = "secure"
					s.view().System.Talkgroups[i].KeyID = 7
					s.view().System.Talkgroups[i].AlgorithmID = algorithm
				}
			}
			q := dispatch.CallRequest{RadioID: 10001, Talkgroups: []int{1001, 1002}, Secure: true, AlgorithmID: algorithm, KeyID: 7}
			got, err := s.prepareCall(q, "rf", false)
			if err != nil || got.AlgorithmID != algorithm || got.KeyID != 7 {
				t.Fatal("matching pair rejected", err)
			}
			different := 129
			if algorithm == 129 {
				different = 132
			}
			q.AlgorithmID = different
			if got, err = s.prepareCall(q, "rf", false); err != nil || got.AlgorithmID != different {
				t.Fatal("RF admission replaced or rejected the actual algorithm", err)
			}
			q.AlgorithmID = algorithm
			s.view().System.Talkgroups[1].AlgorithmID = different
			if got, err = s.prepareCall(q, "rf", false); err != nil || got.AlgorithmID != algorithm {
				t.Fatal("assigned transmit key changed RF call admission", err)
			}
			input := OperatorInput{Action: "activate", ID: "different-algorithms", Patch: &PatchInput{Name: "Algorithm mismatch", Talkgroups: []int{1001, 1002}, AlgorithmID: algorithm, KeyID: 7}}
			if _, err = s.applyOperator("patch", input, desk); err != nil {
				t.Fatal("console default changed shared-key patch activation", err)
			}
			if _, err = (&callBroker{s: s}).expandPatch(routing.Request{CallRequest: q}); err != nil {
				t.Fatal("matching actual algorithm rejected", err)
			}
			q.AlgorithmID = different
			if _, err = (&callBroker{s: s}).expandPatch(routing.Request{CallRequest: q}); err == nil {
				t.Fatal("patch translated actual algorithms sharing a KID")
			}
		})
	}
}

func TestEncryptedZeroKeyCallAndPatchAreNotClear(t *testing.T) {
	s, desk, _ := operatorFixture(t, RoleZone)
	for i := range s.view().System.Talkgroups {
		if s.view().System.Talkgroups[i].Enabled {
			s.view().System.Talkgroups[i].AdmissionMode = "secure"
			s.view().System.Talkgroups[i].TransmitMode = "secure"
			s.view().System.Talkgroups[i].AlgorithmID = 129
			s.view().System.Talkgroups[i].KeyID = 0
		}
	}
	request := dispatch.CallRequest{RadioID: 10001, Talkgroups: []int{1001, 1002}, Secure: true, AlgorithmID: 129, KeyID: 0}
	got, err := s.prepareCall(request, "rf", false)
	if err != nil || !got.Secure || got.AlgorithmID != 129 || got.KeyID != 0 {
		t.Fatal("encrypted zero-key call confused with clear", err)
	}
	request.AlgorithmID = 132
	if got, err = s.prepareCall(request, "rf", false); err != nil || got.AlgorithmID != 132 || got.KeyID != 0 {
		t.Fatal("actual zero-key call identity was changed or rejected", err)
	}
	input := OperatorInput{Action: "activate", ID: "zero-key-patch", Patch: &PatchInput{Name: "Zero DES", Talkgroups: []int{1001, 1002}, AlgorithmID: 129, KeyID: 0}}
	noSecure := desk
	noSecure.user.Permissions = []string{"patch"}
	if _, err = s.applyOperator("patch", input, noSecure); err == nil {
		t.Fatal("zero-key encrypted patch bypassed secure permission")
	}
	value := operatorApply(t, s, "patch", input, desk)
	if len(value.Patches) != 1 || value.Patches[0].AlgorithmID != 129 {
		t.Fatal("zero-key patch became clear")
	}
}

func TestApplicationEmergencyRequiresAcceptedIncident(t *testing.T) {
	s, desk, _ := operatorFixture(t, RoleZone)
	q := dispatch.CallRequest{ID: "client-spoof", RadioID: 10002, Talkgroups: []int{1001}, Emergency: true}
	got, err := s.prepareCall(q, "application", false)
	if err != nil || got.Emergency || got.Priority == 1 {
		t.Fatal("client emergency bypassed incident policy", got, err)
	}
	operatorApply(t, s, "emergency", OperatorInput{Action: "declare", ID: "accepted-owner-emergency", TalkgroupID: 1001}, desk)
	q.Emergency = false
	got, err = s.prepareCall(q, "application", false)
	if err != nil || !got.Emergency || got.Priority != 1 {
		t.Fatal("accepted incident did not own call mode", got, err)
	}
}
