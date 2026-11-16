package framework

import (
	"encoding/json"
	"net/http"
	"net/http/cookiejar"
	"testing"
	"time"
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
func TestOperatorHistoricalTransportIsRemoved(t *testing.T) {
	c := newMediaCluster(t)
	client := historyProxyClient(t, c)
	for _, path := range []string{"/api/history/events", "/api/history/calls", "/api/history/locations", "/api/history/statistics"} {
		status, _ := client.request(t, "GET", path, nil)
		if status != http.StatusForbidden {
			t.Fatalf("retired operator history returned %d at %s", status, path)
		}
	}
	credentials, err := readCredentials(c.dispatch.service.Config().CredentialFile)
	if err != nil {
		t.Fatal(err)
	}
	status, _ := integrationHTTP(t, http.DefaultClient, "POST", c.zone.server.URL+"/internal/operator-history", map[string]any{}, map[string]string{"X-Service-ID": credentials.OutboundID, "Authorization": "Bearer " + credentials.OutboundToken})
	if status != http.StatusNotFound {
		t.Fatalf("retired internal history returned %d", status)
	}
}
