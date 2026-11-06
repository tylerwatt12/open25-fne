package framework

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/cookiejar"
	"reflect"
	"slices"
	"testing"
	"time"

	"open25.local/open25-fne/internal/activity"
)

func historyProxyClient(t *testing.T, c *mediaCluster) *integrationClient {
	t.Helper()
	user := cloneUser(t, c.users[0])
	user.Talkgroups = []int{1001}
	user.Profile.Zones = []ProfileZone{{Name: "Main", Talkgroups: []int{1001}}}
	user.Profile.Receive = []ReceiveSetting{{TalkgroupID: 1001, Volume: 1}}
	user.Profile.TransmitTalkgroups = []int{1001}
	var system SystemConfig
	if err := c.system.service.owned(&system); err != nil {
		t.Fatal(err)
	}
	if err := c.system.service.Store.SaveUser(user.ID, user, "media-integration-password"); err != nil {
		t.Fatal(err)
	}
	integrationSyncAll(t, c.integrationCluster)
	jar, _ := cookiejar.New(nil)
	client := &integrationClient{base: c.dispatch.web.URL, http: &http.Client{Jar: jar, Timeout: 5 * time.Second}}
	status, raw := integrationHTTP(t, client.http, "POST", client.base+"/api/login", map[string]string{"username": user.Username, "password": "media-integration-password"}, map[string]string{"Origin": client.base})
	var login struct {
		CSRF string `json:"csrf"`
	}
	if status != 200 || json.Unmarshal(raw, &login) != nil {
		t.Fatalf("proxy fixture login failed %d", status)
	}
	client.csrf = login.CSRF
	return client
}
func appendHistoryProxyCall(t *testing.T, s *Service, id, site string, tg, rid int) {
	t.Helper()
	now := time.Now().UTC().Add(-time.Minute)
	for _, kind := range []string{"call_start", "call_end"} {
		e := activity.Event{Origin: site, SiteID: site, At: now, CallID: id, Type: kind, RadioID: rid, TalkgroupID: tg, Audience: "dispatcher", SourceKind: "rf"}
		if kind == "call_end" {
			e.At = now.Add(time.Second)
		}
		if err := s.activity.Append(e); err != nil {
			t.Fatal(err)
		}
	}
}

func TestDispatchHistoryReadsConnectedZoneWithoutLeakingOtherGroupsOrDirectory(t *testing.T) {
	c := newMediaCluster(t)
	client := historyProxyClient(t, c)
	credentials, err := readCredentials(c.dispatch.service.Config().CredentialFile)
	if err != nil {
		t.Fatal(err)
	}
	user, hash, err := c.dispatch.service.Store.User(c.users[0].ID)
	if err != nil {
		t.Fatal(err)
	}
	for _, check := range []struct {
		input  historyReadEnvelope
		status int
	}{
		{historyReadEnvelope{Actor: OperatorActor{UserID: user.ID, RadioID: 10002, Proof: "forged"}, Path: "/api/history/calls"}, 403},
		{historyReadEnvelope{Actor: OperatorActor{UserID: user.ID, RadioID: 10002, Proof: userProof(user, hash)}, Path: "/api/history/settings"}, 400},
		{historyReadEnvelope{Actor: OperatorActor{UserID: user.ID, RadioID: 10002, Proof: userProof(user, hash)}, Path: "/api/history/calls", Query: "upstream=https://other.example"}, 400},
	} {
		code, _ := integrationHTTP(t, http.DefaultClient, "POST", c.zone.server.URL+"/internal/operator-history", check.input, map[string]string{"X-Service-ID": credentials.OutboundID, "Authorization": "Bearer " + credentials.OutboundToken})
		if code != check.status {
			t.Fatalf("history boundary accepted forged identity or non-read operation: %d", code)
		}
	}
	appendHistoryProxyCall(t, c.zone.service, "actual-rf-call", "site-1", 1001, 10001)
	appendHistoryProxyCall(t, c.zone.service, "other-group-call", "site-2", 1002, 10003)
	// No System dependency: the connected Zone's retained records and cached
	// identity are sufficient for this explicitly scoped history view.
	c.system.server.Close()
	status, raw := client.request(t, "GET", "/api/history/calls", nil)
	var reply struct {
		Calls   []activity.Call `json:"calls"`
		Scope   historyScope    `json:"scope"`
		Aliases map[int]string  `json:"radio_aliases"`
	}
	if status != 200 || json.Unmarshal(raw, &reply) != nil || len(reply.Calls) != 1 || reply.Calls[0].ID != "actual-rf-call" || reply.Scope.Role != RoleZone || reply.Scope.InstanceID != "zone-1" || reply.Scope.Coverage != "connected_controller" {
		t.Fatalf("Dispatch substituted local or unscoped history: HTTP%d %s", status, raw)
	}
	if !reflect.DeepEqual(reply.Aliases, map[int]string{10001: "Portable"}) {
		t.Fatalf("proxy omitted the observed caller name or exposed unrelated names: %+v", reply.Aliases)
	}
	for _, unrelated := range []string{"other-group-call", "Remote portable", "Console two", "password_hash", "proof"} {
		if bytes.Contains(raw, []byte(unrelated)) {
			t.Fatal("history exposed an unrelated resource or private directory field", unrelated)
		}
	}
	local, _ := c.dispatch.service.activity.Calls()
	if len(local) != 0 {
		t.Fatal("fixture accidentally relied on local Dispatch call rows")
	}
	status, raw = client.request(t, "GET", "/api/history/events?format=csv", nil)
	if status != 200 || !bytes.HasPrefix(raw, []byte("Event ID,Time,")) || bytes.Contains(raw, []byte("other-group-call")) {
		t.Fatalf("scoped CSV did not cross controller boundary: %d %s", status, raw)
	}
}

func TestDispatchHistoryFallbackUsesOnlyLocalSiteAndDoesNotFakeDisconnectedHistory(t *testing.T) {
	c := newMediaCluster(t)
	client := historyProxyClient(t, c)
	appendHistoryProxyCall(t, c.site.service, "local-site-call", "site-1", 1001, 10001)
	appendHistoryProxyCall(t, c.site.service, "foreign-site-call", "site-2", 1001, 10003)
	appendHistoryProxyCall(t, c.dispatch.service, "local-buffer-only", "site-1", 1001, 10001)
	c.zone.service.peers.Close()
	c.zone.server.Close()
	mediaWait(t, 5*time.Second, func() bool { return c.dispatch.service.mediaClient.URL() == c.site.server.URL }, "history Site fallback")
	status, raw := client.request(t, "GET", "/api/history/calls?site_id=site-2", nil)
	var reply struct {
		Calls []activity.Call `json:"calls"`
		Scope historyScope    `json:"scope"`
	}
	if status != 200 || json.Unmarshal(raw, &reply) != nil || len(reply.Calls) != 1 || reply.Calls[0].ID != "local-site-call" || reply.Scope.Role != RoleSite || reply.Scope.InstanceID != "site-1" {
		t.Fatalf("fallback leaked another Site or used local console buffer: HTTP%d %s", status, raw)
	}
	if slices.ContainsFunc(reply.Calls[0].Destinations, func(d activity.Destination) bool { return d.SiteID != "site-1" }) {
		t.Fatal("Site fallback returned foreign destinations")
	}
	c.site.service.peers.Close()
	c.site.server.Close()
	mediaWait(t, 5*time.Second, func() bool { return c.dispatch.service.mediaClient.URL() == "" }, "history controller disconnect")
	status, _ = client.request(t, "GET", "/api/history/calls", nil)
	if status != 503 {
		t.Fatal("disconnected console presented its partial local buffer as controller history", status)
	}
}
