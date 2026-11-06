package framework

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/cookiejar"
	"net/http/httptest"
	"slices"
	"testing"
	"time"

	"open25.local/open25-fne/internal/dispatch"
	"open25.local/open25-fne/internal/radio"
	"open25.local/open25-fne/internal/routing"
)

func featureCluster(t *testing.T) (*mediaCluster, User) {
	t.Helper()
	c := newMediaCluster(t)
	var system SystemConfig
	if err := c.system.service.owned(&system); err != nil {
		t.Fatal(err)
	}
	system.Radios = append(system.Radios, Radio{RID: 10005, Alias: "Mobile", Enabled: true, Kind: "mobile"})
	if err := c.system.service.Store.SaveConfig(storeDoc(t, system)); err != nil {
		t.Fatal(err)
	}
	permissions := []string{"ptt", "secure_ptt", "patch", "emergency", "emergency_ack", "emergency_clear", "location_view", "location_report"}
	for i := range c.users {
		c.users[i].Permissions = slices.Clone(permissions)
		if err := c.system.service.Store.SaveUser(c.users[i].ID, c.users[i], ""); err != nil {
			t.Fatal(err)
		}
	}
	mobile := cloneUser(t, c.users[0])
	mobile.ID, mobile.Username, mobile.Role, mobile.RadioID = "mobile-user", "mobile-user", "mobile", 10005
	mobile.PositionIDs = []string{}
	mobile.Profile.PreferredPositionID = ""
	if err := c.system.service.Store.SaveUser(mobile.ID, mobile, "media-integration-password"); err != nil {
		t.Fatal(err)
	}
	integrationSyncAll(t, c.integrationCluster)
	return c, mobile
}
func featureLogin(t *testing.T, node *integrationService, user User) *integrationClient {
	t.Helper()
	jar, _ := cookiejar.New(nil)
	client := &integrationClient{base: node.web.URL, http: &http.Client{Jar: jar, Timeout: 4 * time.Second}}
	status, raw := integrationHTTP(t, client.http, http.MethodPost, client.base+"/api/login", map[string]string{"username": user.Username, "password": "media-integration-password"}, map[string]string{"Origin": client.base})
	if status != 200 {
		t.Fatalf("feature login: %d %s", status, raw)
	}
	var login struct {
		CSRF string `json:"csrf"`
	}
	if err := json.Unmarshal(raw, &login); err != nil {
		t.Fatal(err)
	}
	client.csrf = login.CSRF
	return client
}
func featureAction(t *testing.T, client *integrationClient, topic string, input OperatorInput) OperatorSnapshot {
	t.Helper()
	status, raw := client.request(t, http.MethodPost, "/api/operator/"+topic, input)
	if status != 200 {
		t.Fatalf("%s action: %d %s", topic, status, raw)
	}
	var state OperatorSnapshot
	if err := json.Unmarshal(raw, &state); err != nil {
		t.Fatal(err)
	}
	return state
}
func featureState(t *testing.T, client *integrationClient) OperatorSnapshot {
	t.Helper()
	status, raw := client.request(t, http.MethodGet, "/api/operator/state", nil)
	if status != 200 {
		t.Fatalf("operator state: %d %s", status, raw)
	}
	var state OperatorSnapshot
	if err := json.Unmarshal(raw, &state); err != nil {
		t.Fatal(err)
	}
	return state
}
func featureAffiliations(t *testing.T, c *mediaCluster) {
	t.Helper()
	reports := []SiteReport{}
	for i, node := range []*integrationService{c.site, c.otherSite} {
		id := node.service.Config().InstanceID
		runtime := c.radios[id]
		runtime.mu.Lock()
		rid, tg := 10001, 1002
		if i == 1 {
			rid, tg = 10003, 1001
		}
		runtime.status.Affiliations = []radio.Affiliation{{RID: rid, TGID: tg, At: time.Now()}}
		runtime.mu.Unlock()
		publishFixtureRFState(t, c.zone.service, id, runtime.Status())
		report, err := node.service.ownSiteReport()
		if err != nil {
			t.Fatal(err)
		}
		report.ReceivedAt = time.Now()
		reports = append(reports, report)
	}
	if err := c.zone.service.writeCache("site_reports", reports); err != nil {
		t.Fatal(err)
	}
	c.zone.service.refreshDirectory()
}
func featurePatchedRF(t *testing.T, c *mediaCluster, source *integrationService, rid, tg, destinationTG int, destination string) {
	t.Helper()
	featureAffiliations(t, c)
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	id := "patch-rf-" + source.service.Config().InstanceID
	request := routing.Request{CallRequest: dispatch.CallRequest{ID: id, RadioID: rid, Talkgroups: []int{tg}}, SourceKind: "rf", OriginSite: source.service.Config().InstanceID}
	endpoint, err := source.service.voice.Load().discover(ctx, c.zone.server.URL, nil)
	if err != nil {
		t.Fatal(err)
	}
	call, err := source.service.voice.Load().begin(ctx, endpoint, request, false)
	if err != nil {
		t.Fatal("authenticated RF patch admission", err)
	}
	defer func() {
		cleanup, cancel := context.WithTimeout(context.Background(), 3*time.Second)
		defer cancel()
		if err := call.End(cleanup); err != nil {
			t.Error("RF patch release", err)
		}
		mediaWait(t, 2*time.Second, func() bool {
			return len(c.zone.service.broker.router.Active()) == 0 && len(c.site.service.broker.router.Active()) == 0 && len(c.otherSite.service.broker.router.Active()) == 0
		}, "previous patched SIP allocation fully released")
	}()
	frame := sipTestFrame(call.grant.ID, rid, tg, 1, 0)
	if err = call.Write(frame); err != nil {
		t.Fatal("RF patch media", err)
	}

	select {
	case got := <-c.codecs[destination].frames:
		if got.SourceID != rid || got.TalkgroupID != destinationTG {
			t.Fatalf("wrong patch destination: %+v", got)
		}
	case <-ctx.Done():
		t.Fatal("patch did not reach destination Site")
	}
}
func TestPersistentPatchRoutesRestrictedSitesWhileDispatchStoppedAndAfterReconnect(t *testing.T) {
	c, _ := featureCluster(t)
	desk := featureLogin(t, c.dispatch, c.users[0])
	featureAction(t, desk, "patch", OperatorInput{Action: "activate", ID: "persistent-patch", Patch: &PatchInput{Name: "Common to main", Talkgroups: []int{1001, 1002}}})
	integrationSyncAll(t, c.integrationCluster)
	path := c.dispatch.configPath
	c.dispatch.server.Close()
	if err := c.dispatch.service.Close(); err != nil {
		t.Fatal(err)
	}
	if len(c.zone.service.activePatches()) != 1 || len(c.site.service.activePatches()) != 1 {
		t.Fatal("Dispatch shutdown removed controller-owned patch")
	}
	// TG1002 exists only at Site1. Neither the Site2 directory nor its user
	// directory contains the foreign source policy; the Zone authorizes it.
	featurePatchedRF(t, c, c.otherSite, 10003, 1001, 1002, "site-1")
	featurePatchedRF(t, c, c.site, 10001, 1002, 1001, "site-2")
	reopened, err := OpenService(path, integrationAssets())
	if err != nil {
		t.Fatal(err)
	}
	c.dispatch.service = reopened
	c.dispatch.web.Close()
	c.dispatch.server = httptest.NewServer(reopened.InternalHandler())
	c.dispatch.web = httptest.NewServer(reopened.Handler())
	settings := reopened.Config().Settings()
	settings.PublicURL = c.dispatch.web.URL
	settings.InternalURL = c.dispatch.server.URL
	if err = reopened.SaveSettings(settings); err != nil {
		t.Fatal(err)
	}
	reopened.mediaClient.Ensure()
	mediaWait(t, 4*time.Second, func() bool { return reopened.mediaClient.URL() == c.zone.server.URL }, "restarted Dispatch connection")
	reconnected := featureLogin(t, c.dispatch, c.users[0])
	if state := featureState(t, reconnected); len(state.Patches) != 1 || !state.Patches[0].Active {
		t.Fatal("reconnected console lost persistent patch")
	}
	featureAction(t, reconnected, "patch", OperatorInput{Action: "end", ID: "persistent-patch"})
	for _, node := range []*integrationService{c.zone, c.site, c.otherSite, c.zone, c.dispatch} {
		if err := node.service.SyncOnce(context.Background()); err != nil {
			t.Fatalf("final sync %s: %v", node.service.Config().InstanceID, err)
		}
	}
	if len(c.zone.service.activePatches()) != 0 || len(c.site.service.activePatches()) != 0 {
		t.Fatal("explicit patch end did not reach controllers")
	}
}
func TestEmergencyDeclarationAcknowledgmentAndClearAreSharedByLiveSessions(t *testing.T) {
	c, mobile := featureCluster(t)
	one, two, phone := featureLogin(t, c.dispatch, c.users[0]), featureLogin(t, c.dispatch, c.users[1]), featureLogin(t, c.dispatch, mobile)
	featureAction(t, phone, "emergency", OperatorInput{Action: "declare", ID: "mobile-emergency", TalkgroupID: 1001})
	for _, client := range []*integrationClient{one, two} {
		state := featureState(t, client)
		if len(state.Emergencies) != 1 || state.Emergencies[0].RadioID != 10005 {
			t.Fatal("emergency was not shared with permitted desk")
		}
	}
	featureAction(t, one, "emergency", OperatorInput{Action: "acknowledge", ID: "mobile-emergency"})
	if state := featureState(t, two); state.Emergencies[0].AcknowledgedBy != c.users[0].ID {
		t.Fatal("second desk did not see acknowledgment")
	}
	featureAction(t, two, "emergency", OperatorInput{Action: "clear", ID: "mobile-emergency"})
	for _, client := range []*integrationClient{one, phone} {
		state := featureState(t, client)
		if len(state.Emergencies) != 1 || state.Emergencies[0].ClearedAt.IsZero() || state.Emergencies[0].ClearedBy != c.users[1].ID {
			t.Fatal("clear or incident history was not shared")
		}
	}
	if c.zone.service.activeEmergency(10005, 1001) {
		t.Fatal("cleared emergency still affects priority")
	}
}
func TestApplicationGPSIsScopedRejectsBadFixesAndHonorsRevocation(t *testing.T) {
	c, mobile := featureCluster(t)
	limited := cloneUser(t, c.users[1])
	limited.Talkgroups = []int{1002}
	limited.Profile.Zones = []ProfileZone{{Name: "Restricted", Talkgroups: []int{1002}}}
	limited.Profile.Receive = nil
	limited.Profile.SelectedTalkgroup = 1002
	limited.Profile.TransmitTalkgroups = []int{1002}
	var system SystemConfig
	if err := c.system.service.owned(&system); err != nil {
		t.Fatal(err)
	}
	if err := c.system.service.Store.SaveUser(limited.ID, limited, ""); err != nil {
		t.Fatal(err)
	}
	integrationSyncAll(t, c.integrationCluster)
	phone, viewer, other := featureLogin(t, c.dispatch, mobile), featureLogin(t, c.dispatch, c.users[0]), featureLogin(t, c.dispatch, limited)
	fix := &LocationInput{Latitude: 41.5, Longitude: -81.7, Accuracy: 8, FixAt: time.Now().UTC()}
	featureAction(t, phone, "location", OperatorInput{Action: "report", TalkgroupID: 1001, Location: fix})
	if state := featureState(t, viewer); len(state.Locations) != 1 || state.Locations[0].RadioID != 10005 {
		t.Fatal("authorized desk cannot see mobile position")
	}
	if state := featureState(t, other); len(state.Locations) != 0 {
		t.Fatal("foreign talkgroup leaked coordinates")
	}
	for _, bad := range []*LocationInput{{Latitude: 91}, {Longitude: 181}, {Accuracy: -1}, {FixAt: time.Now().Add(-10 * time.Minute)}} {
		if status, _ := phone.request(t, http.MethodPost, "/api/operator/location", OperatorInput{Action: "report", TalkgroupID: 1001, Location: bad}); status == 200 {
			t.Fatal("malformed/stale location accepted")
		}
	}
	if status, _ := phone.request(t, http.MethodPost, "/api/operator/location", OperatorInput{Action: "report", RadioID: 10001, TalkgroupID: 1001, Location: fix}); status == 200 {
		t.Fatal("mobile forged a subscriber location")
	}
	revoked := cloneUser(t, c.users[0])
	revoked.Permissions = slices.DeleteFunc(revoked.Permissions, func(p string) bool { return p == "location_view" })
	if err := c.system.service.Store.SaveUser(revoked.ID, revoked, ""); err != nil {
		t.Fatal(err)
	}
	integrationSyncAll(t, c.integrationCluster)
	status, raw := viewer.request(t, http.MethodGet, "/api/operator/state", nil)
	if status == 200 {
		var state OperatorSnapshot
		if err := json.Unmarshal(raw, &state); err != nil {
			t.Fatal(err)
		}
		if len(state.Locations) != 0 {
			t.Fatal("revoked viewer retained coordinates")
		}
	} else if status != 401 && status != 403 {
		t.Fatalf("unexpected revocation response: %d %s", status, raw)
	}
}
func TestPatchSecurityAndZoneDestinationTrustCannotBeForged(t *testing.T) {
	c, _ := featureCluster(t)
	var system SystemConfig
	if err := c.system.service.owned(&system); err != nil {
		t.Fatal(err)
	}
	for i := range system.Talkgroups {
		if system.Talkgroups[i].TGID == 1001 || system.Talkgroups[i].TGID == 1002 {
			system.Talkgroups[i].AdmissionMode = "secure"
			system.Talkgroups[i].TransmitMode = "secure"
			system.Talkgroups[i].KeyID = 7
			system.Talkgroups[i].AlgorithmID = 132
		}
	}
	if err := c.system.service.Store.SaveConfig(storeDoc(t, system)); err != nil {
		t.Fatal(err)
	}
	integrationSyncAll(t, c.integrationCluster)
	desk := featureLogin(t, c.dispatch, c.users[0])
	status, _ := desk.request(t, http.MethodPost, "/api/operator/patch", OperatorInput{Action: "activate", ID: "clear-patch", Patch: &PatchInput{Name: "Clear patch", Talkgroups: []int{1001, 1002}}})
	if status == 200 {
		t.Fatal("encrypted-only groups accepted a clear patch")
	}
	featureAction(t, desk, "patch", OperatorInput{Action: "activate", ID: "same-key", Patch: &PatchInput{Name: "Shared key", Talkgroups: []int{1001, 1002}, AlgorithmID: 132, KeyID: 7}})
	for _, q := range []dispatch.CallRequest{{Talkgroups: []int{1001}}, {Talkgroups: []int{1001}, Secure: true, AlgorithmID: 132, KeyID: 8}} {
		if _, err := c.zone.service.broker.expandPatch(routing.Request{CallRequest: q}); err == nil {
			t.Fatal("patch accepted actual clear or mismatched-key media")
		}
	}
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	endpoint, err := c.dispatch.service.voice.Load().discover(ctx, c.site.server.URL, nil)
	if err != nil {
		t.Fatal(err)
	}
	forged := routing.Request{CallRequest: dispatch.CallRequest{ID: "forged-zone-trust", RadioID: 10003, Talkgroups: []int{1001}, Secure: true, AlgorithmID: 132, KeyID: 7}, SourceKind: "rf", OriginSite: "site-1"}
	if call, err := c.dispatch.service.voice.Load().begin(ctx, endpoint, forged, false); err == nil {
		_ = call.End(context.Background())
		t.Fatal("Dispatch acquired Zone-only RF source trust")
	}
}
