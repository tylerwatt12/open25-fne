package framework

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"golang.org/x/crypto/bcrypt"
	"open25.local/open25-fne/internal/activity"
	"open25.local/open25-fne/internal/radio"
)

func commandSiteFixture(t *testing.T) (*Service, *operationRadio, *integrationClient) {
	t.Helper()
	site, runtime := operationFixture(t)
	hash, err := bcrypt.GenerateFromPassword([]byte(integrationPassword), bcrypt.MinCost)
	if err != nil {
		t.Fatal(err)
	}
	if err = site.Store.SetAccount("administrator", hash); err != nil {
		t.Fatal(err)
	}
	site.sessions = map[string]Session{}
	site.attempts = map[string]loginAttempt{}
	site.mu.Lock()
	site.config.CredentialFile = filepath.Join(t.TempDir(), "service-credentials.json")
	site.mu.Unlock()
	if err = WritePrivateJSON(site.Config().CredentialFile, ServiceCredentials{Peers: map[string]PeerCredential{}}); err != nil {
		t.Fatal(err)
	}
	server := httptest.NewServer(site.InternalHandler())
	t.Cleanup(server.Close)
	web := httptest.NewServer(site.Handler())
	t.Cleanup(web.Close)
	site.mu.Lock()
	site.config.InternalURL, site.config.PublicURL = server.URL, web.URL
	site.mu.Unlock()
	client := integrationLogin(t, &integrationService{service: site, server: server, web: web}, integrationPassword)
	return site, runtime, client
}

func TestSiteRadioCommandsRequireAdministratorAndReportOnlySendAcceptance(t *testing.T) {
	site, runtime, client := commandSiteFixture(t)
	payload := radioCommandInput{Action: "radio_check", RadioID: 10001}
	status, _ := integrationHTTP(t, http.DefaultClient, "POST", client.base+"/api/radio/command", payload, map[string]string{"Origin": client.base})
	if status != 401 {
		t.Fatalf("anonymous command accepted: %d", status)
	}
	status, _ = integrationHTTP(t, client.http, "POST", client.base+"/api/radio/command", payload, map[string]string{"Origin": client.base})
	if status != 403 {
		t.Fatalf("command without CSRF accepted: %d", status)
	}
	for _, action := range []string{"radio_check", "page", "registration_demand", "affiliation_query"} {
		payload.Action = action
		status, raw := client.request(t, "POST", "/api/radio/command", payload)
		if status != 202 {
			t.Fatalf("%s: %d %s", action, status, raw)
		}
		var receipt radioCommandReceipt
		if err := json.Unmarshal(raw, &receipt); err != nil {
			t.Fatal(err)
		}
		if !receipt.Sent || !receipt.AwaitingAcknowledgment || receipt.SiteID != "site-1" || receipt.RadioID != 10001 || receipt.Action != action {
			t.Fatalf("dishonest command receipt: %s", raw)
		}
		var fields map[string]any
		_ = json.Unmarshal(raw, &fields)
		if _, exists := fields["success"]; exists {
			t.Fatal("transport acceptance was reported as radio success")
		}
	}
	runtime.mu.Lock()
	commands := append([]radio.Command{}, runtime.commands...)
	runtime.mu.Unlock()
	if len(commands) != 4 {
		t.Fatal("allowed commands did not reach the Site-owned radio adapter")
	}
	for i, action := range []string{"radio_check", "page", "registration_demand", "affiliation_query"} {
		if commands[i].Action != action || commands[i].RID != 10001 {
			t.Fatal("native command target or action changed")
		}
	}
	before := len(commands)
	system := storeFixtureSystem()
	system.Radios = append(system.Radios, Radio{RID: 10002, Alias: "Console", Enabled: true, Kind: "console"}, Radio{RID: 10003, Alias: "Mobile", Enabled: true, Kind: "mobile"})
	if err := site.Store.WriteCache("system", storeDoc(t, system)); err != nil {
		t.Fatal(err)
	}
	site.refreshDirectory()
	for _, payload := range []radioCommandInput{{Action: "regroup", RadioID: 10001}, {Action: "location_poll", RadioID: 10001}, {Action: "emergency_clear", RadioID: 10001}, {Action: "radio_check", RadioID: 12345}, {Action: "page", RadioID: 10001, SiteID: "site-2"}, {Action: "page", RadioID: 10002}, {Action: "page", RadioID: 10003}, {Action: "page", RadioID: 0xFFFFFC}} {
		status, _ = client.request(t, "POST", "/api/radio/command", payload)
		if status != 400 {
			t.Fatalf("unsupported or invalid target accepted: %+v HTTP%d", payload, status)
		}
	}
	runtime.mu.Lock()
	after := len(runtime.commands)
	runtime.mu.Unlock()
	if after != before {
		t.Fatal("a rejected command reached the radio adapter")
	}
	recorder := httptest.NewRecorder()
	request := httptest.NewRequest("POST", "/api/radio/command", nil)
	site.radioCommandHTTP(recorder, request, Session{UserID: "dispatcher-user"})
	if recorder.Code != 403 {
		t.Fatal("dispatcher reached administrator-only command route")
	}
	runtime.mu.Lock()
	runtime.commandError = errors.New("native command outcome is unconfirmed")
	runtime.mu.Unlock()
	if err := site.Store.mutateRadioCommands(func(rows *[]radioCommandReceipt) error {
		for i := range *rows {
			(*rows)[i].ExpiresAt = time.Now().Add(-time.Second)
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	status, raw := client.request(t, "POST", "/api/radio/command", radioCommandInput{Action: "page", RadioID: 10001})
	if status != 503 || !strings.Contains(string(raw), "unconfirmed") {
		t.Fatalf("unconfirmed native send was hidden: %d %s", status, raw)
	}
	runtime.mu.Lock()
	runtime.status.State = "stopped"
	before = len(runtime.commands)
	runtime.mu.Unlock()
	status, _ = client.request(t, "POST", "/api/radio/command", radioCommandInput{Action: "page", RadioID: 10001})
	runtime.mu.Lock()
	after = len(runtime.commands)
	runtime.mu.Unlock()
	if status != 503 || after != before {
		t.Fatal("a stopped control channel accepted a radio command")
	}
}

func TestZoneForwardsRadioCommandsOnlyToItsEnrolledSite(t *testing.T) {
	site, runtime, _ := commandSiteFixture(t)
	zoneConfig := storeFixtureZone()
	zoneConfig.Sites[0].Endpoint = site.Config().InternalURL
	system := storeFixtureSystem()
	token := strings.Repeat("z", 64)
	siteToken := strings.Repeat("s", 64)
	zone := startIntegrationService(t, t.TempDir(), RoleZone, "zone-1", zoneConfig, ServiceCredentials{OutboundID: "zone-main", OutboundToken: token, Peers: map[string]PeerCredential{"site-transport": {Role: RoleSite, InstanceID: "site-1", Token: siteToken}}}, system, zoneConfig)
	if err := WritePrivateJSON(site.Config().CredentialFile, ServiceCredentials{OutboundID: "site-transport", OutboundToken: siteToken, Peers: map[string]PeerCredential{"zone-main": {Role: RoleZone, InstanceID: "zone-1", Token: token}}}); err != nil {
		t.Fatal(err)
	}
	// The standalone Site fixture normally has no service credential file;
	// give it the explicit enrollment used by this local HTTP test.
	administrator := integrationLogin(t, zone, integrationPassword)
	username, hash, err := zone.service.Store.Account()
	if err != nil {
		t.Fatal(err)
	}
	if err = site.Store.SetAccount(username, hash); err != nil {
		t.Fatal(err)
	}
	status, body := administrator.request(t, "POST", "/api/radio/command", radioCommandInput{Action: "page", RadioID: 10001, SiteID: "site-1"})
	if status != 202 {
		t.Fatalf("Zone forwarding failed: %d %s", status, body)
	}
	messageCode := 65535
	status, body = administrator.request(t, "POST", "/api/radio/command", radioCommandInput{Action: "message", RadioID: 10001, SiteID: "site-1", Code: &messageCode})
	var messageReceipt radioCommandReceipt
	if err := json.Unmarshal(body, &messageReceipt); err != nil || status != 202 || messageReceipt.Code == nil || *messageReceipt.Code != messageCode {
		t.Fatalf("Zone lost coded-message data: %d %s %v", status, body, err)
	}
	runtime.mu.Lock()
	count := len(runtime.commands)
	message := runtime.commands[count-1]
	runtime.mu.Unlock()
	if count != 2 || message.Action != "message" || message.Code != messageCode {
		t.Fatal("Zone did not forward both commands exactly once with their original data")
	}
	for _, target := range []string{"", "not-enrolled"} {
		status, _ = administrator.request(t, "POST", "/api/radio/command", radioCommandInput{Action: "page", RadioID: 10001, SiteID: target})
		if status != 400 {
			t.Fatalf("missing/unknown Site accepted: %q HTTP%d", target, status)
		}
	}
	wrongHeaders := map[string]string{"X-Service-ID": "zone-main", "Authorization": "Bearer wrong-token"}
	status, _ = integrationHTTP(t, http.DefaultClient, "POST", site.Config().InternalURL+"/internal/radio/command", radioCommandInput{Action: "inhibit", RadioID: 10001}, wrongHeaders)
	if status != 401 {
		t.Fatal("Site accepted an unauthenticated Zone command")
	}
}

func TestSignalingLabelsAreCapturedWithRawNumericIDs(t *testing.T) {
	site, runtime := operationFixture(t)
	system := storeFixtureSystem()
	system.StatusLabels = []SignalLabel{{ID: 1, Label: "READY"}, {ID: 2, Label: "BUSY"}, {ID: 258, Label: "EN ROUTE"}}
	system.MessageLabels = []SignalLabel{{ID: 1, Label: "TESTMSG"}}
	if err := site.Store.WriteCache("system", storeDoc(t, system)); err != nil {
		t.Fatal(err)
	}
	site.refreshDirectory()
	now := time.Now().UTC()
	one, two, unknown, combined := 1, 2, 44, 258
	for i, event := range []radio.Event{{Kind: "status", Code: &one}, {Kind: "status", Code: &two}, {Kind: "message", Code: &one}, {Kind: "message", Code: &unknown}, {Kind: "status", Code: &combined}} {
		event.ID = string(rune('a' + i))
		event.At = now.Add(time.Duration(i) * time.Millisecond)
		event.RID = 10001
		event.Audience = "operator"
		site.radioEvent(event)
	}
	if err := site.activity.Flush(context.Background()); err != nil {
		t.Fatal(err)
	}
	rows, err := site.activity.Events(activity.Query{Restricted: true, Groups: []int{1001}})
	if err != nil {
		t.Fatal(err)
	}
	labels := map[string]string{}
	codes := map[string]int{}
	for _, row := range rows {
		labels[row.ID] = row.Label
		codes[row.ID] = int(row.Details["code"].(float64))
		if row.ID == "e" && (row.Details["user_status"] != float64(1) || row.Details["unit_status"] != float64(2)) {
			t.Fatalf("P25 status components were lost or reversed: %+v", row.Details)
		}
	}
	if labels["a"] != "READY" || labels["b"] != "BUSY" || labels["c"] != "TESTMSG" || labels["d"] != "Message 44" || labels["e"] != "EN ROUTE" || codes["a"] != 1 || codes["b"] != 2 || codes["c"] != 1 || codes["d"] != 44 || codes["e"] != 258 {
		t.Fatalf("labels and IDs were not retained independently: %+v %+v", labels, codes)
	}
	system.StatusLabels[0].Label = "AVAILABLE"
	if err = site.Store.WriteCache("system", storeDoc(t, system)); err != nil {
		t.Fatal(err)
	}
	site.refreshDirectory()
	rows, _ = site.activity.Events(activity.Query{Restricted: true, Groups: []int{1001}})
	for _, row := range rows {
		if row.ID == "a" && row.Label != "READY" {
			t.Fatal("later configuration silently rewrote historical event labels")
		}
	}
	site.reconcileRadio()
	select {
	case <-runtime.applications:
		t.Fatal("display metadata was applied as an RF policy change")
	default:
	}
}
