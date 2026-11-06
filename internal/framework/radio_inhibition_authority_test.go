package framework

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"open25.local/open25-fne/internal/radio"
)

func inhibitionCluster(t *testing.T) (*Service, *operationRadio, *integrationClient, *integrationService, *integrationClient, OperatorActor, map[string]string) {
	t.Helper()
	site, runtime, siteClient := commandSiteFixture(t)
	zoneConfig := storeFixtureZone()
	zoneConfig.Sites[0].Endpoint = site.Config().InternalURL
	system := storeFixtureSystem()
	token := strings.Repeat("z", 64)
	siteToken := strings.Repeat("s", 64)
	zone := startIntegrationService(t, t.TempDir(), RoleZone, "zone-1", zoneConfig, ServiceCredentials{OutboundID: "zone-main", OutboundToken: token, Peers: map[string]PeerCredential{"site-transport": {Role: RoleSite, InstanceID: "site-1", Token: siteToken}}}, system, zoneConfig)
	if err := WritePrivateJSON(site.Config().CredentialFile, ServiceCredentials{OutboundID: "site-transport", OutboundToken: siteToken, Peers: map[string]PeerCredential{"zone-main": {Role: RoleZone, InstanceID: "zone-1", Token: token}}}); err != nil {
		t.Fatal(err)
	}
	username, hash, err := zone.service.Store.Account()
	if err != nil {
		t.Fatal(err)
	}
	if err = site.Store.SetAccount(username, hash); err != nil {
		t.Fatal(err)
	}
	// Re-authenticate after adopting the Zone's current salted verifier;
	// pre-existing Site sessions correctly become invalid at that boundary.
	status, raw := integrationHTTP(t, siteClient.http, "POST", siteClient.base+"/api/login", map[string]string{"username": username, "password": integrationPassword}, map[string]string{"Origin": siteClient.base})
	if status != 200 {
		t.Fatalf("Site re-login %d %s", status, raw)
	}
	var login struct {
		CSRF string `json:"csrf"`
	}
	if err = json.Unmarshal(raw, &login); err != nil {
		t.Fatal(err)
	}
	siteClient.csrf = login.CSRF
	site.mu.Lock()
	site.config.UpstreamURL = zone.server.URL
	site.client = &http.Client{Timeout: 3 * time.Second}
	site.mu.Unlock()
	proof := sha256.Sum256(hash)
	actor := OperatorActor{Administrator: true, Username: username, Proof: hex.EncodeToString(proof[:])}
	return site, runtime, siteClient, zone, integrationLogin(t, zone, integrationPassword), actor, map[string]string{"X-Service-ID": "zone-main", "Authorization": "Bearer " + token}
}
func requireDesired(t *testing.T, s *Service, want ...int) {
	t.Helper()
	got, err := s.DesiredInhibitedRadios()
	if err != nil || !slices.Equal(got, want) {
		t.Fatalf("desired %v want %v: %v", got, want, err)
	}
}
func TestInhibitionAuthorityRefusesSiteChoicesAndIncompleteZoneCommands(t *testing.T) {
	site, runtime, siteClient, _, _, actor, headers := inhibitionCluster(t)
	for _, action := range []string{"inhibit", "uninhibit"} {
		status, _ := siteClient.request(t, "POST", "/api/radio/command", radioCommandInput{Action: action, RadioID: 10001})
		if status != 403 {
			t.Fatalf("Site administrative choice %s: HTTP%d", action, status)
		}
	}
	for _, envelope := range []radioCommandEnvelope{
		{Input: radioCommandInput{ID: "missing-policy", Action: "inhibit", RadioID: 10001}, Actor: actor},
		{Input: radioCommandInput{ID: "mismatched-policy", Action: "inhibit", RadioID: 10001}, Actor: actor, Inhibition: &radioInhibitionPolicy{Desired: []int{}}},
		{Input: radioCommandInput{ID: "duplicate-policy", Action: "inhibit", RadioID: 10001}, Actor: actor, Inhibition: &radioInhibitionPolicy{Desired: []int{10001, 10001}}},
	} {
		status, _ := integrationHTTP(t, http.DefaultClient, "POST", site.Config().InternalURL+"/internal/radio/command", envelope, headers)
		if status != 400 && status != 403 {
			t.Fatalf("invalid authoritative envelope: HTTP%d", status)
		}
	}
	requireDesired(t, site)
	runtime.mu.Lock()
	count := len(runtime.commands)
	runtime.mu.Unlock()
	if count != 0 {
		t.Fatal("refused choice reached native adapter")
	}
}
func TestInhibitionAuthorityOldRequestIDCannotRestoreAnOppositeChoice(t *testing.T) {
	site, runtime, _, zone, admin, actor, headers := inhibitionCluster(t)
	first := radioCommandInput{ID: "first-inhibit", Action: "inhibit", RadioID: 10001, SiteID: "site-1"}
	status, body := admin.request(t, "POST", "/api/radio/command", first)
	if status != 202 {
		t.Fatalf("inhibit: %d %s", status, body)
	}
	requireDesired(t, zone.service, 10001)
	requireDesired(t, site, 10001)
	status, body = admin.request(t, "POST", "/api/radio/command", radioCommandInput{ID: "next-uninhibit", Action: "uninhibit", RadioID: 10001, SiteID: "site-1"})
	if status != 202 {
		t.Fatalf("uninhibit: %d %s", status, body)
	}
	requireDesired(t, zone.service)
	requireDesired(t, site)
	status, body = admin.request(t, "POST", "/api/radio/command", first)
	if status != 202 {
		t.Fatalf("duplicate: %d %s", status, body)
	}
	requireDesired(t, zone.service)
	requireDesired(t, site)
	// A late transport duplicate cannot apply its stale full-set envelope either.
	status, body = integrationHTTP(t, http.DefaultClient, "POST", zone.service.view().Zone.Sites[0].Endpoint+"/internal/radio/command", radioCommandEnvelope{Input: first, Actor: actor, Inhibition: &radioInhibitionPolicy{Desired: []int{10001}}}, headers)
	if status != 202 {
		t.Fatalf("late duplicate: %d %s", status, body)
	}
	requireDesired(t, site)
	runtime.mu.Lock()
	count := len(runtime.commands)
	runtime.mu.Unlock()
	if count != 2 {
		t.Fatalf("duplicate transmitted: %d commands", count)
	}
}
func TestInhibitionAuthorityPersistsIntentWhenSelectedSiteDoesNotConfirm(t *testing.T) {
	site, runtime, _, zone, admin, _, _ := inhibitionCluster(t)
	failure := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { http.Error(w, "unavailable", 503) }))
	defer failure.Close()
	config := storeFixtureZone()
	config.Sites[0].Endpoint = failure.URL
	if err := zone.service.Store.SaveConfig(storeDoc(t, config)); err != nil {
		t.Fatal(err)
	}
	zone.service.refreshDirectory()
	status, body := admin.request(t, "POST", "/api/radio/command", radioCommandInput{ID: "offline-inhibit", Action: "inhibit", RadioID: 10001, SiteID: "site-1"})
	if status != 503 || !strings.Contains(string(body), "desired_policy_saved") || !strings.Contains(string(body), "did not confirm") {
		t.Fatalf("saved intent/send failure hidden: %d %s", status, body)
	}
	requireDesired(t, zone.service, 10001)
	fresh := &Service{Store: zone.service.Store, config: zone.service.Config()}
	requireDesired(t, fresh, 10001)
	runtime.mu.Lock()
	count := len(runtime.commands)
	runtime.mu.Unlock()
	if count != 0 {
		t.Fatal("failed Site contact invented a native send")
	}
	username, hash, err := zone.service.Store.Account()
	if err != nil {
		t.Fatal(err)
	}
	if err = zone.service.acceptCatalog(catalog{System: storeFixtureSystem(), Administrator: administratorRecord{Username: username, Hash: hash}}); err != nil {
		t.Fatal(err)
	}
	requireDesired(t, zone.service, 10001)
	report := SiteReport{ID: "site-1", Config: storeFixtureSite(), ObservedAt: time.Now().UTC(), Endpoint: failure.URL, AdminURL: site.Config().PublicURL, Radio: &radio.Status{SiteID: "site-1", State: "stopped"}}
	if err = zone.service.acceptSiteReport(PeerCredential{Role: RoleSite, InstanceID: "site-1"}, report); err != nil {
		t.Fatal(err)
	}
	requireDesired(t, zone.service, 10001)
}
func TestInhibitionAuthorityCancelledInternalWaiterCannotReplacePolicy(t *testing.T) {
	site, _, _, _, _, actor, _ := inhibitionCluster(t)
	if err := site.cacheRadioInhibition(radioInhibitionPolicy{Desired: []int{10001}}); err != nil {
		t.Fatal(err)
	}
	raw, _ := json.Marshal(radioCommandEnvelope{Input: radioCommandInput{ID: "cancelled-uninhibit", Action: "uninhibit", RadioID: 10001}, Actor: actor, Inhibition: &radioInhibitionPolicy{Desired: []int{}}})
	ctx, cancel := context.WithCancel(context.Background())
	request := httptest.NewRequest("POST", "/internal/radio/command", strings.NewReader(string(raw))).WithContext(ctx)
	recorder := httptest.NewRecorder()
	done := make(chan struct{})
	site.syncMu.Lock()
	go func() {
		defer close(done)
		site.radioCommandInternal(recorder, request, PeerCredential{Role: RoleZone, InstanceID: "zone-1"})
	}()
	cancel()
	site.syncMu.Unlock()
	select {
	case <-done:
	case <-time.After(time.Second):
		t.Fatal("cancelled waiter did not return")
	}
	if recorder.Code != 503 {
		t.Fatal(recorder.Code)
	}
	requireDesired(t, site, 10001)
}

func TestInhibitionAuthorityOlderCatalogueCannotOverwriteNewDirectCommand(t *testing.T) {
	site, runtime, _, zone, admin, _, _ := inhibitionCluster(t)
	runtime.mu.Lock()
	runtime.status.SiteID = "site-1"
	runtime.mu.Unlock()
	read := make(chan struct{})
	release := make(chan struct{})
	var first sync.Once
	var releaseOnce sync.Once
	defer releaseOnce.Do(func() { close(release) })
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/internal/catalog" {
			zone.service.InternalHandler().ServeHTTP(w, r)
			return
		}
		snapshot := httptest.NewRecorder()
		zone.service.InternalHandler().ServeHTTP(snapshot, r)
		first.Do(func() { close(read); <-release })
		for k, values := range snapshot.Header() {
			for _, v := range values {
				w.Header().Add(k, v)
			}
		}
		w.WriteHeader(snapshot.Code)
		_, _ = w.Write(snapshot.Body.Bytes())
	}))
	defer upstream.Close()
	site.mu.Lock()
	site.config.UpstreamURL = upstream.URL
	site.mu.Unlock()
	ctx, cancel := context.WithTimeout(context.Background(), 4*time.Second)
	defer cancel()
	syncDone := make(chan error, 1)
	go func() { syncDone <- site.SyncOnce(ctx) }()
	select {
	case <-read:
	case <-ctx.Done():
		t.Fatal("Site did not fetch old catalogue")
	}
	type result struct {
		status int
		body   []byte
	}
	commandDone := make(chan result, 1)
	go func() {
		status, body := admin.request(t, "POST", "/api/radio/command", radioCommandInput{ID: "newer-than-get", Action: "inhibit", RadioID: 10001, SiteID: "site-1"})
		commandDone <- result{status, body}
	}()
	deadline := time.Now().Add(time.Second)
	for {
		got, err := zone.service.DesiredInhibitedRadios()
		if err != nil {
			t.Fatal(err)
		}
		if slices.Equal(got, []int{10001}) {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("Zone did not commit newer intent while GET was pending")
		}
		time.Sleep(time.Millisecond)
	}
	select {
	case got := <-commandDone:
		t.Fatalf("command passed the pending catalogue owner: %+v", got)
	default:
	}
	runtime.mu.Lock()
	before := len(runtime.commands)
	runtime.mu.Unlock()
	if before != 0 {
		t.Fatal("pending GET did not serialize command application")
	}
	releaseOnce.Do(func() { close(release) })
	select {
	case err := <-syncDone:
		if err != nil {
			t.Fatal(err)
		}
	case <-ctx.Done():
		t.Fatal(ctx.Err())
	}
	select {
	case got := <-commandDone:
		if got.status != 202 {
			t.Fatalf("new command %d %s", got.status, got.body)
		}
	case <-ctx.Done():
		t.Fatal(ctx.Err())
	}
	requireDesired(t, zone.service, 10001)
	requireDesired(t, site, 10001)
	runtime.mu.Lock()
	after := len(runtime.commands)
	runtime.mu.Unlock()
	if after != 1 {
		t.Fatalf("expected one actual command, got %d", after)
	}
}

func TestInhibitionAuthorityCatalogueReadDoesNotWaitForZoneCommandMutex(t *testing.T) {
	site, _, _, zone, _, _, _ := inhibitionCluster(t)
	zone.service.commandMu.Lock()
	defer zone.service.commandMu.Unlock()
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	done := make(chan error, 1)
	go func() {
		var incoming catalog
		err := site.upstreamRequest(ctx, "GET", "/internal/catalog", nil, &incoming)
		if err == nil && incoming.Inhibition == nil {
			err = errors.New("catalogue omitted authoritative empty policy")
		}
		done <- err
	}()
	select {
	case err := <-done:
		if err != nil {
			t.Fatal(err)
		}
	case <-ctx.Done():
		t.Fatal("catalogue read waited for Zone command mutex")
	}
}

func TestInhibitionAuthorityPageAdmissionUsesDesiredAndRetainedActualState(t *testing.T) {
	for _, actual := range []bool{false, true} {
		t.Run(map[bool]string{false: "desired", true: "pending-uninhibit"}[actual], func(t *testing.T) {
			site, runtime, client := commandSiteFixture(t)
			if actual {
				if err := site.persistSiteInhibition([]radio.InhibitRestore{{RadioID: 10001, Blocked: true, PendingUninhibit: true}}); err != nil {
					t.Fatal(err)
				}
			} else {
				if err := site.cacheRadioInhibition(radioInhibitionPolicy{Desired: []int{10001}}); err != nil {
					t.Fatal(err)
				}
			}
			status, body := client.request(t, "POST", "/api/radio/command", radioCommandInput{ID: "blocked-page", Action: "page", RadioID: 10001})
			if status != 503 || !strings.Contains(string(body), "inhibited") {
				t.Fatalf("blocked page: %d %s", status, body)
			}
			status, body = client.request(t, "POST", "/api/radio/command", radioCommandInput{ID: "permitted-check", Action: "radio_check", RadioID: 10001})
			if status != 202 {
				t.Fatalf("check exception: %d %s", status, body)
			}
			runtime.mu.Lock()
			commands := slices.Clone(runtime.commands)
			runtime.mu.Unlock()
			if len(commands) != 1 || commands[0].Action != "radio_check" {
				t.Fatalf("blocked target reached adapter: %+v", commands)
			}
		})
	}
}

func TestInhibitionAuthorityTerminalRetriesNeverClearActualProtection(t *testing.T) {
	for _, action := range []string{"inhibit", "uninhibit"} {
		t.Run(action, func(t *testing.T) {
			site, _, _, _, admin, _, _ := inhibitionCluster(t)
			status, body := admin.request(t, "POST", "/api/radio/command", radioCommandInput{ID: "terminal-" + action, Action: action, RadioID: 10001, SiteID: "site-1"})
			if status != 202 {
				t.Fatalf("admit %d %s", status, body)
			}
			if err := site.persistSiteInhibition([]radio.InhibitRestore{{RadioID: 10001, Blocked: true, PendingUninhibit: action == "uninhibit"}}); err != nil {
				t.Fatal(err)
			}
			base := radio.Event{Kind: action + "_no_response", RID: 10001, RequestorID: radio.NativeFNEID, Direction: "state", Attempts: 5, At: time.Now().UTC()}
			for _, change := range []func(*radio.Event){func(e *radio.Event) { e.RequestorID = 1 }, func(e *radio.Event) { e.Attempts = 4 }, func(e *radio.Event) { e.Direction = "received" }, func(e *radio.Event) { e.RID = 10002 }, func(e *radio.Event) { e.SiteID = "other-site" }, func(e *radio.Event) { e.At = e.At.Add(-time.Minute) }} {
				event := base
				change(&event)
				site.observeRadioCommand(event)
				rows, err := site.Store.radioCommands()
				if err != nil || len(rows) != 1 || rows[0].State != "sent" {
					t.Fatal("unrelated terminal completed receipt", rows, err)
				}
			}
			base.ID = "native-terminal"
			site.observeRadioCommand(base)
			rows, err := site.Store.radioCommands()
			if err != nil || rows[0].State != "timed_out" || rows[0].AwaitingAcknowledgment || rows[0].ResponseEventID != base.ID || !strings.Contains(rows[0].Message, "inhibition remains") {
				t.Fatal(rows, err)
			}
			if !site.radioServiceInhibited(10001) {
				t.Fatal("terminal receipt cleared actual protection")
			}
			site.observeRadioCommand(radio.Event{Kind: action + "_ack", RID: 10001, RequestorID: radio.NativeFNEID, Direction: "received", At: time.Now().UTC()})
			rows, _ = site.Store.radioCommands()
			if rows[0].State != "timed_out" || !site.radioServiceInhibited(10001) {
				t.Fatal("late receipt response changed terminal or actual state", rows)
			}
		})
	}
}
