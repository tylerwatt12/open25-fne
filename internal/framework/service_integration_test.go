package framework

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"io/fs"
	"net/http"
	"net/http/cookiejar"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"testing/fstest"
	"time"

	"golang.org/x/crypto/bcrypt"
)

const integrationPassword = "initial-test-password"

type integrationService struct {
	service    *Service
	server     *httptest.Server
	web        *httptest.Server
	configPath string
}
type integrationCluster struct{ system, zone, site, otherSite, dispatch *integrationService }
type integrationClient struct {
	base, csrf string
	http       *http.Client
}
type integrationState struct {
	Role        Role            `json:"role"`
	Config      json.RawMessage `json:"config"`
	System      SystemConfig    `json:"system"`
	Zone        *ZoneConfig     `json:"zone"`
	Reports     []SiteReport    `json:"site_reports"`
	Connections []Connection    `json:"connections"`
	RFEnabled   bool            `json:"rf_enabled"`
}

type integrationRoundTripFunc func(*http.Request) (*http.Response, error)

func (f integrationRoundTripFunc) RoundTrip(r *http.Request) (*http.Response, error) {
	return f(r)
}

func integrationAssets() fs.FS {
	return fstest.MapFS{"index.html": &fstest.MapFile{Data: []byte("Configuration framework")}}
}

func startIntegrationService(t *testing.T, dir string, role Role, id string, config any, creds ServiceCredentials, system SystemConfig, zone ZoneConfig) *integrationService {
	t.Helper()
	stateDir := filepath.Join(dir, id)
	credentialPath := filepath.Join(stateDir, "credentials.json")
	if err := WritePrivateJSON(credentialPath, creds); err != nil {
		t.Fatal(err)
	}
	c := FileConfig{Role: role, InstanceID: id, Listen: "127.0.0.1:0", InternalListen: "127.0.0.1:0", InternalURL: "http://127.0.0.1:5100", StateDir: stateDir, PublicURL: "http://127.0.0.1:5000", PollSeconds: 1, CredentialFile: credentialPath}
	if role == RoleZone || role == RoleSite {
		c.UpstreamURL = "http://127.0.0.1:5000"
	}
	configPath := filepath.Join(stateDir, "service.json")
	if err := WritePrivateJSON(configPath, c); err != nil {
		t.Fatal(err)
	}
	store, err := OpenStore(filepath.Join(stateDir, "configuration.db"), role)
	if err != nil {
		t.Fatal(err)
	}
	if role != RoleSystem {
		if err = store.WriteCache("system", storeDoc(t, system)); err != nil {
			t.Fatal(err)
		}
	}
	if role == RoleSite || role == RoleApplication {
		if err = store.WriteCache("zone", storeDoc(t, zone)); err != nil {
			t.Fatal(err)
		}
	}
	if err = store.SaveConfig(storeDoc(t, config)); err != nil {
		t.Fatal(err)
	}
	hash, err := bcrypt.GenerateFromPassword([]byte(integrationPassword), bcrypt.MinCost)
	if err != nil {
		t.Fatal(err)
	}
	if err = store.SetAccount("administrator", hash); err != nil {
		t.Fatal(err)
	}
	store.Close()
	if role == RoleZone {
		if err = writeRFAuthority(filepath.Join(stateDir, "rf-source-authority.json"), rfAuthorityDocument{ZoneID: id, NativeRuns: map[string]time.Time{}, Units: map[int]rfAuthorityUnit{}}); err != nil {
			t.Fatal(err)
		}
	}
	s, err := OpenService(configPath, integrationAssets())
	if err != nil {
		t.Fatal(err)
	}
	pair := &integrationService{service: s, configPath: configPath}
	pair.server = httptest.NewServer(s.InternalHandler())
	pair.web = httptest.NewServer(s.Handler())
	settings := s.Config().Settings()
	settings.PublicURL = pair.web.URL
	settings.InternalURL = pair.server.URL
	if err = s.SaveSettings(settings); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { pair.server.Close(); pair.web.Close(); _ = pair.service.Close() })
	return pair
}

func integrationNewCluster(t *testing.T) *integrationCluster {
	t.Helper()
	dir := t.TempDir()
	system := storeFixtureSystem()
	system.Talkgroups = []Talkgroup{
		{TGID: 1001, AdmissionMode: "clear", TransmitMode: "clear", Alias: "Every Site", Enabled: true, SiteAccess: "all", SiteIDs: []string{}},
		{TGID: 1002, AdmissionMode: "clear", TransmitMode: "clear", Alias: "Main Site only", Enabled: true, SiteAccess: "selected", SiteIDs: []string{"site-1"}},
		{TGID: 1003, AdmissionMode: "clear", TransmitMode: "clear", Alias: "Other Site only", Enabled: true, SiteAccess: "selected", SiteIDs: []string{"site-2"}},
		{TGID: 1099, AdmissionMode: "clear", TransmitMode: "clear", Alias: "Denied", Enabled: false, SiteAccess: "all", SiteIDs: []string{}},
	}
	zone := storeFixtureZone()
	zone.Sites = append(zone.Sites, SiteIdentity{ID: "site-2", Name: "Other Site", SiteID: 2, NAC: 0x294})
	zoneToken := strings.Repeat("z", 64)
	siteToken := strings.Repeat("s", 64)
	otherToken := strings.Repeat("o", 64)
	dispatchToken := strings.Repeat("d", 64)
	c := &integrationCluster{}
	c.system = startIntegrationService(t, dir, RoleSystem, "system-1", system, ServiceCredentials{Peers: map[string]PeerCredential{"zone-1": {Role: RoleZone, InstanceID: "zone-1", Token: zoneToken}}}, system, zone)
	c.zone = startIntegrationService(t, dir, RoleZone, "zone-1", zone, ServiceCredentials{OutboundID: "zone-1", OutboundToken: zoneToken, Peers: map[string]PeerCredential{"site-1": {Role: RoleSite, InstanceID: "site-1", Token: siteToken}, "site-2": {Role: RoleSite, InstanceID: "site-2", Token: otherToken}, "application-1": {Role: RoleApplication, InstanceID: "application-1", Token: dispatchToken}}}, system, zone)
	settings := c.zone.service.Config().Settings()
	settings.UpstreamURL = c.system.server.URL
	if err := c.zone.service.SaveSettings(settings); err != nil {
		t.Fatal(err)
	}
	c.site = startIntegrationService(t, dir, RoleSite, "site-1", storeFixtureSite(), ServiceCredentials{OutboundID: "site-1", OutboundToken: siteToken, Peers: map[string]PeerCredential{"application-1": {Role: RoleApplication, InstanceID: "application-1", Token: dispatchToken}}}, system, zone)
	otherSite := storeFixtureSite()
	otherSite.ID = "site-2"
	c.otherSite = startIntegrationService(t, dir, RoleSite, "site-2", otherSite, ServiceCredentials{OutboundID: "site-2", OutboundToken: otherToken, Peers: map[string]PeerCredential{}}, system, zone)
	for _, site := range []*integrationService{c.site, c.otherSite} {
		settings = site.service.Config().Settings()
		settings.UpstreamURL = c.zone.server.URL
		if err := site.service.SaveSettings(settings); err != nil {
			t.Fatal(err)
		}
	}
	c.dispatch = startIntegrationService(t, dir, RoleApplication, "application-1", ApplicationConfig{Name: "Dispatch", ConnectionURL: c.zone.server.URL}, ServiceCredentials{OutboundID: "application-1", OutboundToken: dispatchToken, Peers: map[string]PeerCredential{}}, system, zone)
	return c
}

func integrationSync(t *testing.T, pair *integrationService) {
	t.Helper()
	// Mirror the service's polling recovery within one fixture budget. Each
	// request retains its normal client timeout and every failed attempt is logged.
	started := time.Now()
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	for attempt := 1; ; attempt++ {
		if err := ctx.Err(); err != nil {
			t.Fatalf("%s fixture sync did not recover within its 20-second budget: %v", pair.service.Config().Role, err)
		}
		err := pair.service.SyncOnce(ctx)
		role := pair.service.Config().Role
		if err == nil {
			t.Logf("%s fixture sync completed in %s; attempts=%d", role, time.Since(started).Round(time.Millisecond), attempt)
			return
		}
		t.Logf("%s fixture sync attempt %d failed after %s: %v", role, attempt, time.Since(started).Round(time.Millisecond), err)
		timer := time.NewTimer(time.Duration(pair.service.Config().PollSeconds) * time.Second)
		select {
		case <-ctx.Done():
			timer.Stop()
			t.Fatalf("%s fixture sync did not recover within its 20-second budget: %v (last error: %v)", role, ctx.Err(), err)
		case <-timer.C:
		}
	}
}
func integrationSyncAll(t *testing.T, c *integrationCluster) {
	t.Helper()
	integrationSync(t, c.zone)
	integrationSync(t, c.site)
	integrationSync(t, c.otherSite)
	integrationSync(t, c.zone)
	integrationSync(t, c.dispatch)
}

func integrationHTTP(t *testing.T, client *http.Client, method, address string, body any, headers map[string]string) (int, []byte) {
	t.Helper()
	var raw []byte
	var err error
	if body != nil {
		raw, err = json.Marshal(body)
		if err != nil {
			t.Fatal(err)
		}
	}
	r, err := http.NewRequest(method, address, bytes.NewReader(raw))
	if err != nil {
		t.Fatal(err)
	}
	if body != nil {
		r.Header.Set("Content-Type", "application/json")
	}
	for k, v := range headers {
		r.Header.Set(k, v)
	}
	response, err := client.Do(r)
	if err != nil {
		t.Fatal(err)
	}
	defer response.Body.Close()
	reply, err := io.ReadAll(io.LimitReader(response.Body, 4<<20))
	if err != nil {
		t.Fatal(err)
	}
	return response.StatusCode, reply
}
func integrationLogin(t *testing.T, pair *integrationService, password string) *integrationClient {
	t.Helper()
	jar, _ := cookiejar.New(nil)
	c := &integrationClient{base: pair.web.URL, http: &http.Client{Jar: jar, Timeout: 3 * time.Second}}
	status, raw := integrationHTTP(t, c.http, "POST", c.base+"/api/login", map[string]string{"username": "administrator", "password": password}, map[string]string{"Origin": c.base})
	if status != 200 {
		t.Fatalf("login: HTTP %d %s", status, raw)
	}
	var login struct {
		CSRF string `json:"csrf"`
	}
	if err := json.Unmarshal(raw, &login); err != nil {
		t.Fatal(err)
	}
	c.csrf = login.CSRF
	return c
}
func (c *integrationClient) request(t *testing.T, method, path string, body any) (int, []byte) {
	return integrationHTTP(t, c.http, method, c.base+path, body, map[string]string{"Origin": c.base, "X-CSRF-Token": c.csrf})
}
func (c *integrationClient) state(t *testing.T) integrationState {
	t.Helper()
	status, raw := c.request(t, "GET", "/api/state", nil)
	if status != 200 {
		t.Fatalf("read state: HTTP %d %s", status, raw)
	}
	var s integrationState
	if err := json.Unmarshal(raw, &s); err != nil {
		t.Fatal(err)
	}
	return s
}

func TestConfigurationSyncRetriesLostReportAcknowledgement(t *testing.T) {
	c := integrationNewCluster(t)
	integrationSyncAll(t, c)
	if c.site.service.client.Timeout != 5*time.Second {
		t.Fatal("fixture changed the normal upstream request timeout")
	}
	var previous Connection
	if !c.site.service.readCache("connection", &previous) || previous.State != "Connected" || previous.LastSuccess.IsZero() {
		t.Fatal("fixture has no accepted connection to retain")
	}
	if err := c.site.service.observeRFEmergency(10001, 1001, true); err != nil {
		t.Fatal(err)
	}
	incident := c.site.service.operatorLocalSnapshot().Emergencies[0]
	base := c.site.service.client.Transport
	defer func() { c.site.service.client.Transport = base }()
	reports, observedCached := 0, false
	var lostAt time.Time
	c.site.service.client.Transport = integrationRoundTripFunc(func(r *http.Request) (*http.Response, error) {
		report := r.Method == http.MethodPost && r.URL.Path == "/internal/report"
		if report {
			reports++
			if reports == 2 {
				var failed Connection
				if !c.site.service.readCache("connection", &failed) || failed.State != "Cached settings" ||
					!failed.LastSuccess.Equal(previous.LastSuccess) || !strings.Contains(failed.Error, "injected lost report acknowledgement") {
					t.Error("lost acknowledgement did not retain the last accepted connection", failed)
				}
				observedCached = true
				if time.Since(lostAt) < time.Duration(c.site.service.Config().PollSeconds)*time.Second {
					t.Error("fixture retried before its configured polling interval")
				}
			}
		}
		response, err := base.RoundTrip(r)
		if err != nil || !report || reports != 1 {
			return response, err
		}
		if response.StatusCode != http.StatusOK {
			t.Errorf("report was not accepted before acknowledgement loss: HTTP%d", response.StatusCode)
			return response, nil
		}
		accepted := c.zone.service.operatorLocalSnapshot().Emergencies
		if len(accepted) != 1 || !sameRFIncident(accepted[0], incident) {
			t.Error("receiver had not committed the report before acknowledgement loss")
		}
		_, readErr := io.Copy(io.Discard, response.Body)
		closeErr := response.Body.Close()
		if readErr != nil || closeErr != nil {
			return nil, errors.Join(readErr, closeErr)
		}
		lostAt = time.Now()
		return nil, errors.New("injected lost report acknowledgement")
	})
	integrationSync(t, c.site)
	var connected Connection
	if reports != 2 || !observedCached || !c.site.service.readCache("connection", &connected) || connected.State != "Connected" ||
		connected.Error != "" || !connected.LastSuccess.After(previous.LastSuccess) {
		t.Fatal("retry did not restore a confirmed connection", reports, connected)
	}
	retainedReports := c.zone.service.siteReports()
	siteCount := 0
	for _, report := range retainedReports {
		if report.ID == c.site.service.Config().InstanceID {
			siteCount++
		}
	}
	retained := c.zone.service.operatorLocalSnapshot().Emergencies
	if len(retainedReports) != 2 || siteCount != 1 || len(retained) != 1 || !sameRFIncident(retained[0], incident) || !retained[0].LastSeen.Equal(incident.LastSeen) {
		t.Fatal("report retry duplicated a Site or changed its retained incident identity")
	}
}

func TestConfigurationServiceOwnershipCSRFAndNoRF(t *testing.T) {
	c := integrationNewCluster(t)
	integrationSyncAll(t, c)
	status, _ := integrationHTTP(t, http.DefaultClient, "GET", c.site.web.URL+"/api/state", nil, nil)
	if status != 401 {
		t.Fatalf("anonymous state read: HTTP %d", status)
	}
	for _, pair := range []*integrationService{c.system, c.zone, c.site, c.dispatch} {
		client := integrationLogin(t, pair, integrationPassword)
		state := client.state(t)
		if state.Role != pair.service.Config().Role || state.RFEnabled {
			t.Fatal("wrong application role or RF capability")
		}
		status, reply := client.request(t, "PUT", "/api/config", state.Config)
		if status != 200 {
			t.Fatalf("owned save: %d %s", status, reply)
		}
		wrong := storeDoc(t, storeFixtureSystem())
		if state.Role == RoleSystem {
			wrong = storeDoc(t, storeFixtureSite())
		}
		status, _ = client.request(t, "PUT", "/api/config", wrong)
		if status != 200 {
			t.Fatalf("entered document rejected: HTTP %d", status)
		}
		if status, reply = client.request(t, "PUT", "/api/config", state.Config); status != 200 {
			t.Fatalf("restore entered configuration: %d %s", status, reply)
		}
		status, _ = integrationHTTP(t, client.http, "PUT", client.base+"/api/config", state.Config, map[string]string{"Origin": client.base})
		if status != 403 {
			t.Fatalf("missing CSRF: HTTP %d", status)
		}
		status, _ = integrationHTTP(t, client.http, "PUT", client.base+"/api/config", state.Config, map[string]string{"Origin": "https://untrusted.example", "X-CSRF-Token": client.csrf})
		if status != 403 {
			t.Fatalf("cross-origin write: HTTP %d", status)
		}
		for _, path := range []string{"/api/runtime/start", "/api/site/start"} {
			status, _ = client.request(t, "POST", path, map[string]bool{"start": true})
			if status != 404 {
				t.Fatalf("RF/audio route %s exists: HTTP %d", path, status)
			}
		}
		for _, path := range []string{"/api/dispatch/audio", "/api/dispatch/session"} {
			status, _ = client.request(t, "POST", path, map[string]bool{})
			if status != 403 {
				t.Fatalf("administrator without dispatch identity reached %s: HTTP %d", path, status)
			}
		}
		status, _ = client.request(t, "POST", "/api/radio/start", map[string]string{})
		wanted := 403
		if state.Role == RoleSite {
			wanted = 503
		}
		if status != wanted {
			t.Fatalf("unconfigured radio start: HTTP %d, want %d", status, wanted)
		}
		status, raw := integrationHTTP(t, http.DefaultClient, "GET", client.base+"/healthz", nil, nil)
		var health map[string]any
		_ = json.Unmarshal(raw, &health)
		if status != 200 || health["rf_enabled"] != false {
			t.Fatal("health did not identify configuration-only service")
		}
	}
	status, _ = integrationHTTP(t, http.DefaultClient, "POST", c.system.web.URL+"/api/login", map[string]string{"username": "administrator", "password": integrationPassword}, map[string]string{"Origin": "https://untrusted.example"})
	if status != 403 {
		t.Fatalf("cross-origin login: HTTP %d", status)
	}
}

func TestConfigurationServicesSyncScopedSettingsAndReports(t *testing.T) {
	c := integrationNewCluster(t)
	integrationSyncAll(t, c)
	for _, test := range []struct {
		pair            *integrationService
		wanted, blocked int
		site            string
	}{{c.site, 1002, 1003, "site-1"}, {c.otherSite, 1003, 1002, "site-2"}} {
		var received SystemConfig
		if !test.pair.service.readCache("system", &received) {
			t.Fatal("system not received")
		}
		groups := map[int]Talkgroup{}
		for _, g := range received.Talkgroups {
			groups[g.TGID] = g
		}
		if _, ok := groups[test.wanted]; !ok {
			t.Fatal("permitted selected-Site group missing")
		}
		if _, ok := groups[test.blocked]; ok {
			t.Fatal("other Site's group leaked into scoped catalogue")
		}
		if groups[1099].Enabled {
			t.Fatal("disabled group became allowed")
		}
		var zone ZoneConfig
		if !test.pair.service.readCache("zone", &zone) || len(zone.Sites) != 1 || zone.Sites[0].ID != test.site {
			t.Fatal("Site received another Site's configuration identity")
		}
	}
	for _, pair := range []*integrationService{c.system, c.zone} {
		state, err := pair.service.State(Session{Username: "administrator"})
		if err != nil {
			t.Fatal(err)
		}
		reports := state["site_reports"].([]SiteReport)
		if len(reports) != 2 {
			t.Fatalf("parent did not retain two authenticated Site reports: %#v", reports)
		}
		for _, r := range reports {
			if r.RFEnabled || r.ReceivedAt.IsZero() || len(r.Config.Channels) != 1 {
				t.Fatal("parent Site report lost configuration/freshness or enabled RF")
			}
		}
	}
	var dispatchSystem SystemConfig
	if !c.dispatch.service.readCache("system", &dispatchSystem) || len(dispatchSystem.Talkgroups) != 4 {
		t.Fatal("Dispatch did not receive the central catalogue")
	}
	updated := storeFixtureSystem()
	if err := c.system.service.owned(&updated); err != nil {
		t.Fatal(err)
	}
	updated.Radios[0].Alias = "Updated portable"
	updated.Talkgroups[0].Alias = "Updated group"
	if err := c.system.service.Store.SaveConfig(storeDoc(t, updated)); err != nil {
		t.Fatal(err)
	}
	integrationSyncAll(t, c)
	for _, pair := range []*integrationService{c.zone, c.site, c.otherSite, c.dispatch} {
		var got SystemConfig
		pair.service.readCache("system", &got)
		if got.Radios[0].Alias != "Updated portable" || got.Talkgroups[0].Alias != "Updated group" {
			t.Fatal("automatic catalogue chain retained older settings")
		}
	}
}

func TestConfigurationSiteRetainsEditableSettingsWithoutZone(t *testing.T) {
	c := integrationNewCluster(t)
	integrationSyncAll(t, c)
	client := integrationLogin(t, c.site, integrationPassword)
	cacheBefore, _, _ := c.site.service.Store.ReadCache("system")
	c.zone.server.Close()
	if err := c.zone.service.Close(); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	err := c.site.service.SyncOnce(ctx)
	cancel()
	if err == nil {
		t.Fatal("Zone outage reported as connected")
	}
	state := client.state(t)
	if len(state.Connections) != 1 || state.Connections[0].State != "Cached settings" || state.Connections[0].LastSuccess.IsZero() {
		t.Fatalf("outage state lost accepted settings: %#v", state.Connections)
	}
	var local SiteConfig
	_ = json.Unmarshal(state.Config, &local)
	local.Channels[0].Name = "Saved while isolated"
	status, reply := client.request(t, "PUT", "/api/config", local)
	if status != 200 {
		t.Fatalf("local save during Zone outage: %d %s", status, reply)
	}
	c.site.server.Close()
	if err = c.site.service.Close(); err != nil {
		t.Fatal(err)
	}
	restarted, err := OpenService(c.site.configPath, integrationAssets())
	if err != nil {
		t.Fatal(err)
	}
	c.site.service = restarted
	c.site.web.Close()
	c.site.server = httptest.NewServer(restarted.InternalHandler())
	c.site.web = httptest.NewServer(restarted.Handler())
	settings := restarted.Config().Settings()
	settings.PublicURL = c.site.web.URL
	settings.InternalURL = c.site.server.URL
	if err = restarted.SaveSettings(settings); err != nil {
		t.Fatal(err)
	}
	client = integrationLogin(t, c.site, integrationPassword)
	state = client.state(t)
	_ = json.Unmarshal(state.Config, &local)
	if local.Channels[0].Name != "Saved while isolated" {
		t.Fatal("restart lost locally saved settings")
	}
	cacheAfter, _, _ := restarted.Store.ReadCache("system")
	if !bytes.Equal(cacheBefore, cacheAfter) {
		t.Fatal("outage/restart replaced accepted central settings")
	}
}

func TestConfigurationReportsRejectUntrustedIdentitiesAndRF(t *testing.T) {
	c := integrationNewCluster(t)
	integrationSyncAll(t, c)
	report, err := c.site.service.ownSiteReport()
	if err != nil {
		t.Fatal(err)
	}
	headers := map[string]string{"X-Service-ID": "site-1", "Authorization": "Bearer " + strings.Repeat("s", 64)}
	for _, edit := range []func(*SiteReport){func(r *SiteReport) { r.ID = "site-2"; r.Config.ID = "site-2" }, func(r *SiteReport) { r.RFEnabled = true }} {
		forged := report
		edit(&forged)
		status, _ := integrationHTTP(t, http.DefaultClient, "POST", c.zone.server.URL+"/internal/report", forged, headers)
		if status != 400 {
			t.Fatalf("forged Site report accepted: HTTP %d", status)
		}
	}
	futureSite := report
	futureSite.ObservedAt = time.Now().Add(time.Hour)
	status, _ := integrationHTTP(t, http.DefaultClient, "POST", c.zone.server.URL+"/internal/report", futureSite, headers)
	if status != 400 {
		t.Fatalf("future Site report accepted: HTTP %d", status)
	}
	status, _ = integrationHTTP(t, http.DefaultClient, "POST", c.zone.server.URL+"/internal/report", report, nil)
	if status != 401 {
		t.Fatalf("unauthenticated report: HTTP %d", status)
	}
	status, _ = integrationHTTP(t, http.DefaultClient, "GET", c.system.server.URL+"/internal/catalog", nil, headers)
	if status != 401 {
		t.Fatalf("Site credential used against System catalogue: HTTP %d", status)
	}
	var zone ZoneConfig
	_ = c.zone.service.owned(&zone)
	forgedZone := zoneReport{ID: "different-zone", Endpoint: c.zone.server.URL, Config: zone, Sites: []SiteReport{report}}
	status, _ = integrationHTTP(t, http.DefaultClient, "POST", c.system.server.URL+"/internal/report", forgedZone, map[string]string{"X-Service-ID": "zone-1", "Authorization": "Bearer " + strings.Repeat("z", 64)})
	if status != 400 {
		t.Fatalf("forged Zone identity accepted: HTTP %d", status)
	}
	forgedZone.ID = "zone-1"
	forgedZone.ObservedAt = time.Now().Add(time.Hour)
	status, _ = integrationHTTP(t, http.DefaultClient, "POST", c.system.server.URL+"/internal/report", forgedZone, map[string]string{"X-Service-ID": "zone-1", "Authorization": "Bearer " + strings.Repeat("z", 64)})
	if status != 400 {
		t.Fatalf("future Zone report accepted: HTTP %d", status)
	}
	state, err := c.zone.service.State(Session{})
	if err != nil {
		t.Fatal(err)
	}
	for _, r := range state["site_reports"].([]SiteReport) {
		if r.RFEnabled {
			t.Fatal("forged envelope enabled RF")
		}
	}
}

func TestConfigurationPasswordChangeInvalidatesSyncedSessions(t *testing.T) {
	c := integrationNewCluster(t)
	integrationSyncAll(t, c)
	clients := []*integrationClient{}
	for _, pair := range []*integrationService{c.system, c.zone, c.site, c.dispatch} {
		clients = append(clients, integrationLogin(t, pair, integrationPassword))
	}
	status, reply := clients[0].request(t, "PUT", "/api/account", map[string]string{"current_password": integrationPassword, "new_password": "replacement-test-password"})
	if status != 200 {
		t.Fatalf("password change: %d %s", status, reply)
	}
	integrationSyncAll(t, c)
	for i, pair := range []*integrationService{c.system, c.zone, c.site, c.dispatch} {
		status, _ = clients[i].request(t, "GET", "/api/state", nil)
		if status != 401 {
			t.Fatalf("old %s session survived synchronized password change: HTTP %d", pair.service.Config().Role, status)
		}
		integrationLogin(t, pair, "replacement-test-password")
	}
	client := integrationLogin(t, c.site, "replacement-test-password")
	status, _ = client.request(t, "PUT", "/api/account", map[string]string{"current_password": "replacement-test-password", "new_password": "another-test-password"})
	if status != 403 {
		t.Fatalf("Site edited central administrator account: HTTP %d", status)
	}
}

func TestConfigurationMalformedCatalogRetainsLastAcceptedBundle(t *testing.T) {
	c := integrationNewCluster(t)
	integrationSyncAll(t, c)
	before, _, _ := c.site.service.Store.ReadCache("system")
	_, oldHash, _ := c.site.service.Store.Account()
	var system SystemConfig
	c.site.service.readCache("system", &system)
	system.Name = "Must not be accepted"
	var zone ZoneConfig
	c.site.service.readCache("zone", &zone)
	otherZone := zone
	otherZone.ID = "different-logical-zone"
	for _, bad := range []map[string]any{
		{"system": system, "zone": zone, "administrator": map[string]any{"username": "administrator", "password_hash": []byte("invalid verifier")}},
		{"system": system, "zone": zone, "administrator": map[string]any{"username": "administrator", "password_hash": oldHash}, "rf_enabled": true},
		{"system": system, "zone": otherZone, "administrator": map[string]any{"username": "administrator", "password_hash": oldHash}},
	} {
		server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			w.Header().Set("Content-Type", "application/json")
			_ = json.NewEncoder(w).Encode(bad)
		}))
		settings := c.site.service.Config().Settings()
		settings.UpstreamURL = server.URL
		if err := c.site.service.SaveSettings(settings); err != nil {
			t.Fatal(err)
		}
		ctx, cancel := context.WithTimeout(context.Background(), time.Second)
		err := c.site.service.SyncOnce(ctx)
		cancel()
		server.Close()
		if err == nil {
			t.Fatal("malformed upstream catalogue accepted")
		}
		after, _, _ := c.site.service.Store.ReadCache("system")
		_, afterHash, _ := c.site.service.Store.Account()
		if !bytes.Equal(before, after) || !bytes.Equal(oldHash, afterHash) {
			t.Fatal("rejected catalogue partially replaced accepted settings or administrator")
		}
	}
}

func TestConfigurationServicesPropagateWithoutManualRefresh(t *testing.T) {
	c := integrationNewCluster(t)
	ctx, cancel := context.WithCancel(context.Background())
	var running sync.WaitGroup
	for _, pair := range []*integrationService{c.zone, c.site, c.otherSite, c.dispatch} {
		running.Add(1)
		go func(s *Service) { defer running.Done(); s.RunSync(ctx) }(pair.service)
	}
	done := make(chan struct{})
	go func() { running.Wait(); close(done) }()
	t.Cleanup(func() {
		cancel()
		select {
		case <-done:
		case <-time.After(2 * time.Second):
			t.Error("automatic synchronization did not stop after cancellation")
		}
	})
	waitFor := func(description string, condition func() bool) {
		t.Helper()
		deadline := time.Now().Add(6 * time.Second)
		for time.Now().Before(deadline) {
			if condition() {
				return
			}
			time.Sleep(20 * time.Millisecond)
		}
		t.Fatal(description)
	}
	waitFor("services did not connect automatically", func() bool {
		for _, pair := range []*integrationService{c.zone, c.site, c.otherSite, c.dispatch} {
			var connection Connection
			if !pair.service.readCache("connection", &connection) || connection.State != "Connected" {
				return false
			}
		}
		return true
	})
	var system SystemConfig
	if err := c.system.service.owned(&system); err != nil {
		t.Fatal(err)
	}
	system.Radios[0].Alias = "Automatically distributed"
	if err := c.system.service.Store.SaveConfig(storeDoc(t, system)); err != nil {
		t.Fatal(err)
	}
	waitFor("saved System change did not reach all services automatically", func() bool {
		for _, pair := range []*integrationService{c.zone, c.site, c.otherSite, c.dispatch} {
			var received SystemConfig
			if !pair.service.readCache("system", &received) || len(received.Radios) == 0 || received.Radios[0].Alias != "Automatically distributed" {
				return false
			}
		}
		return true
	})
}

func TestRunSyncConsumesWorkWithinPollInterval(t *testing.T) {
	c := integrationNewCluster(t)
	integrationSyncAll(t, c)
	s := c.zone.service
	base := s.client.Transport
	first, next := make(chan struct{}), make(chan time.Time, 1)
	release := make(chan struct{})
	var releaseOnce sync.Once
	var catalogs, acceptedReports, active atomic.Int32
	var overlap atomic.Bool
	s.client.Transport = integrationRoundTripFunc(func(r *http.Request) (*http.Response, error) {
		if r.Method == http.MethodGet && r.URL.Path == "/internal/catalog" {
			if active.Add(1) != 1 {
				overlap.Store(true)
			}
			defer active.Add(-1)
			switch catalogs.Add(1) {
			case 1:
				close(first)
				select {
				case <-release:
				case <-r.Context().Done():
					return nil, r.Context().Err()
				}
			case 2:
				next <- time.Now()
				<-r.Context().Done()
				return nil, r.Context().Err()
			}
		}
		response, err := base.RoundTrip(r)
		if err == nil && r.Method == http.MethodPost && r.URL.Path == "/internal/report" && response.StatusCode == http.StatusOK {
			acceptedReports.Add(1)
		}
		return response, err
	})
	ctx, cancel := context.WithCancel(t.Context())
	done := make(chan struct{})
	t.Cleanup(func() {
		cancel()
		releaseOnce.Do(func() { close(release) })
		select {
		case <-done:
			s.client.Transport = base
		case <-time.After(3 * time.Second):
			t.Error("RunSync did not stop its in-flight request after cancellation")
		}
	})
	go func() { defer close(done); s.RunSync(ctx) }()
	select {
	case <-first:
	case <-time.After(time.Second):
		t.Fatal("first synchronization was not immediate")
	}
	// Keep real catalog HTTP work in flight past the configured one-second period.
	select {
	case <-next:
		t.Fatal("synchronizations overlapped")
	case <-time.After(1200 * time.Millisecond):
	}
	released := time.Now()
	releaseOnce.Do(func() { close(release) })
	select {
	case at := <-next:
		t.Logf("next catalog arrived %s after long synchronization was released", at.Sub(released))
	case <-time.After(750 * time.Millisecond):
		t.Fatal("completed synchronization added a fresh polling interval after its work")
	}
	var connection Connection
	if !s.readCache("connection", &connection) || connection.State != "Connected" || connection.LastSuccess.Before(released) || acceptedReports.Load() != 1 {
		t.Fatal("first synchronization did not finish actual catalog/report acceptance", connection, acceptedReports.Load())
	}
	if catalogs.Load() != 2 || overlap.Load() {
		t.Fatal("polls were not serialized", catalogs.Load())
	}
}

func TestRunSyncReloadsPollIntervalAndCancels(t *testing.T) {
	c := integrationNewCluster(t)
	integrationSyncAll(t, c)
	s := c.zone.service
	settings := s.Config().Settings()
	settings.PollSeconds = 2
	if err := s.SaveSettings(settings); err != nil {
		t.Fatal(err)
	}
	base := s.client.Transport
	first := make(chan struct{})
	secondReport := make(chan time.Time, 1)
	releaseSecondReport := make(chan struct{})
	next := make(chan time.Time, 1)
	release := make(chan struct{})
	var releaseOnce sync.Once
	var catalogs, reports atomic.Int32
	s.client.Transport = integrationRoundTripFunc(func(r *http.Request) (*http.Response, error) {
		if r.Method == http.MethodGet && r.URL.Path == "/internal/catalog" {
			switch catalogs.Add(1) {
			case 1:
				close(first)
				select {
				case <-release:
				case <-r.Context().Done():
					return nil, r.Context().Err()
				}
			case 2:
				next <- time.Now()
			}
		}
		response, err := base.RoundTrip(r)
		if err == nil && r.Method == http.MethodPost && r.URL.Path == "/internal/report" && response.StatusCode == http.StatusOK && reports.Add(1) == 2 {
			secondReport <- time.Now()
			select {
			case <-releaseSecondReport:
			case <-r.Context().Done():
				response.Body.Close()
				return nil, r.Context().Err()
			}
		}
		return response, err
	})
	ctx, cancel := context.WithCancel(t.Context())
	done := make(chan struct{})
	t.Cleanup(func() {
		cancel()
		releaseOnce.Do(func() { close(release) })
		select {
		case <-done:
			s.client.Transport = base
		case <-time.After(3 * time.Second):
			t.Error("RunSync did not stop after cancellation")
		}
	})
	go func() { defer close(done); s.RunSync(ctx) }()
	select {
	case <-first:
	case <-time.After(time.Second):
		t.Fatal("first synchronization was not immediate")
	}
	settings.PollSeconds = 1
	if err := s.SaveSettings(settings); err != nil {
		t.Fatal(err)
	}
	released := time.Now()
	releaseOnce.Do(func() { close(release) })
	select {
	case at := <-next:
		if elapsed := at.Sub(released); elapsed < 900*time.Millisecond {
			t.Fatal("new polling interval was not honored", elapsed)
		}
	case <-time.After(1700 * time.Millisecond):
		t.Fatal("running synchronization did not adopt the saved shorter polling interval")
	}
	var acceptedAt time.Time
	select {
	case acceptedAt = <-secondReport:
	case <-time.After(time.Second):
		t.Fatal("second synchronization did not complete actual report HTTP work")
	}
	// The shorter interval has been observed. Before releasing this response,
	// restore a longer wait so cancellation cannot race a due catch-up poll.
	settings.PollSeconds = 2
	if err := s.SaveSettings(settings); err != nil {
		t.Fatal(err)
	}
	close(releaseSecondReport)
	// HTTP headers precede SyncOnce's trailing refresh and durable connection
	// update. Observe that committed completion before timing cancellation.
	frameworkWait(t, "second synchronization connection state was not committed", func() bool {
		var connection Connection
		return s.readCache("connection", &connection) && connection.State == "Connected" && connection.LastSuccess.After(acceptedAt)
	})
	cancel()
	select {
	case <-done:
	case <-time.After(500 * time.Millisecond):
		t.Fatal("cancellation waited for another polling interval")
	}
	if catalogs.Load() != 2 {
		t.Fatal("unexpected poll after cancellation", catalogs.Load())
	}
}
