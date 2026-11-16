package framework

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/cookiejar"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
	"time"
)

func sharedLoginClient(t *testing.T, c *integrationCluster, transport *http.Client) *http.Client {
	t.Helper()
	tree := c.system.service.adminTopology(c.system.service.webKnownZone())
	tree.ApplicationURL = c.dispatch.web.URL
	if err := c.system.service.writeCache("topology", tree); err != nil {
		t.Fatal(err)
	}
	client := *transport
	client.Jar, _ = cookiejar.New(nil)
	status, _ := integrationHTTP(t, &client, "POST", c.system.web.URL+"/api/login", map[string]string{"username": "administrator", "password": integrationPassword}, map[string]string{"Origin": c.system.web.URL})
	if status != 200 {
		t.Fatal("central login failed", status)
	}
	for _, node := range []*integrationService{c.zone, c.site, c.otherSite, c.dispatch} {
		response, err := client.Get(node.web.URL + "/")
		if err != nil {
			t.Fatal(err)
		}
		response.Body.Close()
		if response.Request.URL.String() != node.web.URL+"/" {
			t.Fatal("shared sign-in failed", response.Request.URL.Host, response.Request.URL.Path)
		}
	}
	return &client
}
func sharedSession(t *testing.T, client *http.Client, node *integrationService, want bool) map[string]any {
	t.Helper()
	response, err := client.Get(node.web.URL + "/api/session")
	if err != nil {
		t.Fatal(err)
	}
	defer response.Body.Close()
	var value map[string]any
	if err = json.NewDecoder(response.Body).Decode(&value); err != nil {
		t.Fatal(err)
	}
	if value["authenticated"] != want {
		t.Fatalf("%s authentication=%v wanted %v", node.service.Config().Role, value["authenticated"], want)
	}
	return value
}
func TestSharedLogoutRevokesConnectedAndReconnectedSites(t *testing.T) {
	c, transport := ssoTLSCluster(t)
	client := sharedLoginClient(t, c, transport)
	independent := sharedLoginClient(t, c, transport)
	beforeRadio := c.otherSite.service.radioStatus()
	ctx := context.Background()
	if err := c.otherSite.service.setParentIsolation(ctx, true, "administrator"); err != nil {
		t.Fatal(err)
	}
	isolated := sharedSession(t, client, c.otherSite, true)
	status, _ := integrationHTTP(t, client, "POST", c.site.web.URL+"/api/logout", map[string]any{}, map[string]string{"Origin": c.site.web.URL, "X-CSRF-Token": sharedSession(t, client, c.site, true)["csrf"].(string)})
	if status != 200 {
		t.Fatal("logout failed", status)
	}
	for _, node := range []*integrationService{c.system, c.zone, c.site, c.dispatch} {
		sharedSession(t, client, node, false)
	}
	sharedSession(t, client, c.otherSite, true) // No invented remote revocation while physically gated.
	for _, node := range []*integrationService{c.system, c.site, c.otherSite} {
		sharedSession(t, independent, node, true)
	}
	if err := c.otherSite.service.setParentIsolation(ctx, false, "administrator"); err != nil {
		t.Fatal(err)
	}
	sharedSession(t, client, c.otherSite, false)
	// A stale CSRF value and cookie cannot repeat a protected operation after reconnect.
	status, _ = integrationHTTP(t, client, "POST", c.otherSite.web.URL+"/api/site/disconnect-parent", map[string]any{}, map[string]string{"Origin": c.otherSite.web.URL, "X-CSRF-Token": isolated["csrf"].(string)})
	if status != 401 {
		t.Fatal("revoked session performed an operation", status)
	}
	afterRadio := c.otherSite.service.radioStatus()
	if beforeRadio.FNEPID != afterRadio.FNEPID || beforeRadio.State != afterRadio.State {
		t.Fatal("parent actions restarted RF")
	}
}
func TestIsolatedLogoutQueuesDurablyAndRevokesAfterReconnect(t *testing.T) {
	c, transport := ssoTLSCluster(t)
	client := sharedLoginClient(t, c, transport)
	c.site.service.setParentIsolation(context.Background(), true, "administrator")
	before := sharedSession(t, client, c.site, true)
	status, raw := integrationHTTP(t, client, "POST", c.site.web.URL+"/api/logout", map[string]any{}, map[string]string{"Origin": c.site.web.URL, "X-CSRF-Token": before["csrf"].(string)})
	if status != 200 || !bytes.Contains(raw, []byte(`"revocation_pending":true`)) {
		t.Fatal("offline logout was not queued", status, string(raw))
	}
	sharedSession(t, client, c.site, false)
	sharedSession(t, client, c.system, true)
	// Reopen the real SQLite store to establish durable pending delivery and test state.
	store := c.site.service.Store
	cfg := c.site.service.Config()
	var pending int
	if err := store.db.QueryRow(`SELECT count(*) FROM framework_documents WHERE name LIKE 'auth:revoked:%' AND json_extract(document,'$.pending')=1`).Scan(&pending); err != nil || pending != 1 {
		t.Fatal("pending sign-out missing", pending, err)
	}
	reopened, err := OpenStore(cfg.StateDir+"/configuration.db", cfg.Role)
	if err != nil {
		t.Fatal(err)
	}
	defer reopened.Close()
	recovery := &Service{Store: reopened, config: cfg, client: c.site.service.client}
	if err = recovery.loadParentIsolation(); err != nil || !recovery.parentIsolated.Load() {
		t.Fatal("isolation did not persist", err)
	}
	if err = recovery.setParentIsolation(context.Background(), false, "administrator"); err != nil {
		t.Fatal(err)
	}
	for _, node := range []*integrationService{c.system, c.zone, c.otherSite, c.dispatch} {
		sharedSession(t, client, node, false)
	}
}
func TestParentIsolationCancelsRealRequestsAndPreservesProgramming(t *testing.T) {
	c := integrationNewCluster(t)
	integrationSyncAll(t, c)
	original, _ := c.site.service.Store.Config()
	entered := make(chan struct{})
	canceled := make(chan struct{})
	parent := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { close(entered); <-r.Context().Done(); close(canceled) }))
	defer parent.Close()
	settings := c.site.service.Config().Settings()
	settings.UpstreamURL = parent.URL
	c.site.service.SaveSettings(settings)
	done := make(chan error, 1)
	go func() {
		done <- c.site.service.upstreamRequest(context.Background(), "GET", "/internal/catalog", nil, nil)
	}()
	<-entered
	if err := c.site.service.setParentIsolation(context.Background(), false, "administrator"); err != nil {
		t.Fatal(err)
	}
	select {
	case <-canceled:
		t.Fatal("repeated Reconnect canceled a healthy parent request")
	case <-time.After(50 * time.Millisecond):
	}
	if err := c.site.service.setParentIsolation(context.Background(), true, "administrator"); err != nil {
		t.Fatal(err)
	}
	select {
	case <-canceled:
	case <-time.After(time.Second):
		t.Fatal("parent request survived isolation")
	}
	if err := <-done; err == nil {
		t.Fatal("canceled parent request succeeded")
	}
	if err := c.site.service.upstreamRequest(context.Background(), "GET", "/internal/catalog", nil, nil); err == nil {
		t.Fatal("isolated Site contacted parent")
	}
	after, _ := c.site.service.Store.Config()
	if !bytes.Equal(original, after) {
		t.Fatal("isolation edited RF programming")
	}
	// Action endpoints retain role, authentication and CSRF guards.
	login := integrationLogin(t, c.site, integrationPassword)
	status, _ := integrationHTTP(t, login.http, "POST", c.site.web.URL+"/api/site/reconnect-parent", map[string]any{}, map[string]string{"Origin": c.site.web.URL})
	if status != 403 {
		t.Fatal("missing CSRF accepted", status)
	}
	response, err := login.http.Get(c.site.web.URL + "/parent-zone")
	if err != nil {
		t.Fatal(err)
	}
	body, _ := io.ReadAll(response.Body)
	response.Body.Close()
	if !strings.Contains(string(body), "data-parent-isolation") || !strings.Contains(string(body), "Disconnect from parent Zone") {
		t.Fatal("isolation actions/banner missing")
	}
}

func TestParentIsolationDropsLiveSIPLinkAndDoesNotReplay(t *testing.T) {
	c := newMediaCluster(t)
	c.affiliations(t, true)
	call, q := sipRFCall(t, c, "parent-isolation-live-call", false, false)
	if err := call.Write(sipRFFrame(call, q, 1, 0)); err != nil {
		t.Fatal(err)
	}
	source := c.site.service
	original, _ := source.Store.Config()
	if err := source.setParentIsolation(context.Background(), true, "administrator"); err != nil {
		t.Fatal(err)
	}
	if source.mediaClient.URL() != "" {
		t.Fatal("isolated Site retained its parent media link")
	}
	if c.otherSite.service.mediaClient.URL() != c.zone.server.URL {
		t.Fatal("isolation disconnected the other Site")
	}
	select {
	case <-call.done:
	case <-time.After(3 * time.Second):
		t.Fatal("upstream call survived deliberate isolation")
	}
	if err := call.Write(sipRFFrame(call, q, 1, 1)); err == nil {
		t.Fatal("old call accepted audio after isolation")
	}
	if err := source.setParentIsolation(context.Background(), false, "administrator"); err != nil {
		t.Fatal(err)
	}
	mediaWait(t, 5*time.Second, func() bool { return source.mediaClient.URL() == c.zone.server.URL }, "parent reconnection")
	after, _ := source.Store.Config()
	if !bytes.Equal(original, after) {
		t.Fatal("parent operation edited programming")
	}
	if len(c.zone.service.broker.router.Active()) != 0 {
		t.Fatal("reconnection replayed a call")
	}
}
func TestSharedLogoutCancelsPreviouslyIssuedAuthorizationCode(t *testing.T) {
	c, transport := ssoTLSCluster(t)
	client := sharedLoginClient(t, c, transport)
	u, _ := url.Parse(c.system.web.URL)
	var token string
	for _, cookie := range client.Jar.Cookies(u) {
		if cookie.Name == c.system.service.cookieName() {
			token = cookie.Value
		}
	}
	var v Session
	if err := c.system.service.Store.authRead("session", token, &v); err != nil {
		t.Fatal(err)
	}
	code := strings.Repeat("d", 64)
	verifier := strings.Repeat("e", 64)
	grant := ssoGrant{FamilyID: v.FamilyID, Username: v.Username, AccountDigest: v.AccountDigest, SessionExpires: v.Expires, Expires: time.Now().Add(time.Minute), Callback: c.otherSite.web.URL + "/sso/callback", Challenge: ssoChallenge(verifier)}
	if err := c.system.service.rememberSSOCode(code, grant); err != nil {
		t.Fatal(err)
	}
	if _, err := c.system.service.signOutSession(v); err != nil {
		t.Fatal(err)
	}
	if _, err := c.system.service.redeemSSO(ssoExchange{Code: code, Verifier: verifier, Callback: grant.Callback}); err == nil {
		t.Fatal("pre-logout authorization code resurrected session")
	}
}
