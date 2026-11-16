package framework

import (
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/sha256"
	"crypto/tls"
	"crypto/x509"
	"encoding/json"
	"io"
	"math/big"
	"net"
	"net/http"
	"net/http/cookiejar"
	"net/http/httptest"
	"net/url"
	"strings"
	"sync"
	"testing"
	"time"
)

func ssoTLSCluster(t *testing.T) (*integrationCluster, *http.Client) {
	t.Helper()
	c := integrationNewCluster(t)
	certificate, key, _ := splitTLSFixture(t)
	roots := x509.NewCertPool()
	keypair, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	template := &x509.Certificate{SerialNumber: big.NewInt(1), DNSNames: []string{"system.example.test", "zone.example.test", "site.example.test", "other.example.test", "app.example.test"}, NotBefore: time.Now().Add(-time.Hour), NotAfter: time.Now().Add(time.Hour), KeyUsage: x509.KeyUsageDigitalSignature, ExtKeyUsage: []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth}, BasicConstraintsValid: true, IsCA: true}
	der, err := x509.CreateCertificate(rand.Reader, template, template, &keypair.PublicKey, keypair)
	if err != nil {
		t.Fatal(err)
	}
	cert, err := x509.ParseCertificate(der)
	if err != nil {
		t.Fatal(err)
	}
	roots.AddCert(cert)
	hosts := []string{"system.example.test", "zone.example.test", "site.example.test", "other.example.test", "app.example.test"}
	for i, node := range []*integrationService{c.system, c.zone, c.site, c.otherSite, c.dispatch} {
		node.web.Close()
		node.web = httptest.NewUnstartedServer(node.service.Handler())
		node.web.TLS = &tls.Config{Certificates: []tls.Certificate{{Certificate: [][]byte{der}, PrivateKey: keypair}}}
		node.web.StartTLS()
		parsed, _ := url.Parse(node.web.URL)
		node.web.URL = "https://" + net.JoinHostPort(hosts[i], parsed.Port())
		settings := node.service.Config().Settings()
		settings.PublicURL = node.web.URL
		if err := node.service.SaveSettings(settings); err != nil {
			t.Fatal(err)
		}
		node.service.config.TLSCertificate = certificate
		node.service.config.TLSKey = key
		if err := WritePrivateJSON(node.configPath, node.service.Config()); err != nil {
			t.Fatal(err)
		}
	}
	integrationSyncAll(t, c)
	jar, _ := cookiejar.New(nil)
	transport := c.system.web.Client().Transport.(*http.Transport).Clone()
	transport.TLSClientConfig.RootCAs = roots
	transport.Proxy = nil
	transport.DialContext = func(ctx context.Context, network, address string) (net.Conn, error) {
		_, port, err := net.SplitHostPort(address)
		if err != nil {
			return nil, err
		}
		return (&net.Dialer{}).DialContext(ctx, network, net.JoinHostPort("127.0.0.1", port))
	}
	client := &http.Client{Jar: jar, Transport: transport, Timeout: 10 * time.Second}
	t.Cleanup(client.CloseIdleConnections)
	return c, client
}
func TestSSOAcrossSitesPersistentAndDisconnected(t *testing.T) {
	c, client := ssoTLSCluster(t)
	response, err := client.Get(c.site.web.URL + "/channels")
	if err != nil {
		t.Fatal(err)
	}
	body, _ := io.ReadAll(response.Body)
	response.Body.Close()
	if response.Request.URL.Host != strings.TrimPrefix(c.system.web.URL, "https://") || !strings.Contains(string(body), "name=continue") {
		t.Fatalf("Site did not offer central login: %s", body)
	}
	if policy := response.Header.Get("Content-Security-Policy"); !strings.Contains(policy, "form-action 'self' "+c.site.web.URL) || strings.Contains(policy, c.otherSite.web.URL) {
		t.Fatalf("central form redirect policy has wrong destination: %s", policy)
	}
	continuation := earlyWebForm(t, body, "username")
	continuation.Set("username", "administrator")
	continuation.Set("password", integrationPassword)
	response, err = client.PostForm(c.system.web.URL+"/login", continuation)
	if err != nil {
		t.Fatal(err)
	}
	io.Copy(io.Discard, response.Body)
	response.Body.Close()
	if response.Request.URL.String() != c.site.web.URL+"/channels" {
		t.Fatalf("did not return to channel page: %s", response.Request.URL)
	}
	response, err = client.Get(c.otherSite.web.URL + "/channels")
	if err != nil {
		t.Fatal(err)
	}
	body, _ = io.ReadAll(response.Body)
	response.Body.Close()
	if response.Request.URL.String() != c.otherSite.web.URL+"/channels" || strings.Contains(string(body), "autocomplete=current-password") {
		t.Fatalf("second Site asked for login: %s", body)
	}
	// Reopen the real Store and Service, retaining only browser cookies.
	node := c.otherSite
	node.web.Close()
	node.server.Close()
	if err = node.service.Close(); err != nil {
		t.Fatal(err)
	}
	reopened, err := OpenService(node.configPath, integrationAssets())
	if err != nil {
		t.Fatal(err)
	}
	node.service = reopened
	node.web = httptest.NewTLSServer(node.service.Handler())
	// Cookie host/name and configured public URL are unchanged; direct handler
	// invocation avoids changing the callback registration after this restart.
	request := httptest.NewRequest("GET", c.otherSite.service.Config().PublicURL+"/api/session", nil)
	oldURL, _ := url.Parse(c.otherSite.service.Config().PublicURL)
	for _, cookie := range client.Jar.Cookies(oldURL) {
		request.AddCookie(cookie)
	}
	w := httptest.NewRecorder()
	node.service.Handler().ServeHTTP(w, request)
	var session map[string]any
	json.Unmarshal(w.Body.Bytes(), &session)
	if session["authenticated"] != true {
		t.Fatalf("restart lost persistent session: %s", w.Body)
	}
	// Block the actual upstream service connection, not browser cookie sharing.
	c.site.service.operationCancel()
	c.site.service.operationWG.Wait()
	c.site.service.mediaClient.Close()
	c.site.service.client = &http.Client{Transport: integrationRoundTripFunc(func(*http.Request) (*http.Response, error) { return nil, io.ErrClosedPipe })}
	disconnected := *client
	disconnected.Jar, _ = cookiejar.New(nil)
	disconnected.CheckRedirect = func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }
	response, err = disconnected.PostForm(c.site.web.URL+"/login", url.Values{"username": {"administrator"}, "password": {integrationPassword}})
	if err != nil {
		t.Fatal(err)
	}
	response.Body.Close()
	if response.StatusCode != 303 {
		t.Fatal("offline local login failed")
	}
	earlyWebGet(t, &disconnected, c.site.web.URL+"/channels", 200)
	noSession := *client
	noSession.Jar, _ = cookiejar.New(nil)
	response, err = noSession.Get(c.site.web.URL + "/channels")
	if err != nil {
		t.Fatal(err)
	}
	body, _ = io.ReadAll(response.Body)
	response.Body.Close()
	if response.Header.Get("Referrer-Policy") != "strict-origin" {
		t.Fatal("offline login form must preserve its POST Origin without disclosing callback paths")
	}
	if !strings.Contains(string(body), "System sign-in is unavailable") || !strings.Contains(string(body), "autocomplete=current-password") {
		t.Fatalf("disconnected Site did not offer local login: %s", body)
	}
}
func TestSSOCodeGuardsAndSessionRevocation(t *testing.T) {
	c, client := ssoTLSCluster(t)
	for _, path := range []string{"/dispatch-test", "/ptt-test"} {
		response, err := client.Get(c.dispatch.web.URL + path)
		if err != nil {
			t.Fatal(err)
		}
		response.Body.Close()
		if response.Request.URL.String() != c.dispatch.web.URL+"/login" {
			t.Fatal("operator entry was redirected to administrator SSO")
		}
	}
	username, hash, _ := c.system.service.Store.Account()
	code := strings.Repeat("c", 64)
	verifier := strings.Repeat("v", 64)
	grant := ssoGrant{FamilyID: strings.Repeat("f", 64), Callback: c.site.web.URL + "/sso/callback", Challenge: ssoChallenge(verifier), Username: username, AccountDigest: sha256.Sum256(hash), SessionExpires: time.Now().Add(time.Hour), Expires: time.Now().Add(time.Minute)}
	if err := c.system.service.rememberSSOCode(code, grant); err != nil {
		t.Fatal(err)
	}
	for _, input := range []ssoExchange{{code, strings.Repeat("x", 64), grant.Callback}, {code, verifier, c.otherSite.web.URL + "/sso/callback"}} {
		if _, err := c.system.service.redeemSSO(input); err == nil {
			t.Fatal("accepted wrong verifier or audience")
		}
	}
	var wg sync.WaitGroup
	results := make(chan bool, 2)
	for range 2 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			_, err := c.system.service.redeemSSO(ssoExchange{code, verifier, grant.Callback})
			results <- err == nil
		}()
	}
	wg.Wait()
	close(results)
	successes := 0
	for ok := range results {
		if ok {
			successes++
		}
	}
	if successes != 1 {
		t.Fatalf("single-use redemption successes=%d", successes)
	}
	grant.Expires = time.Now().Add(-time.Second)
	c.system.service.rememberSSOCode(code, grant)
	if _, err := c.system.service.redeemSSO(ssoExchange{code, verifier, grant.Callback}); err == nil {
		t.Fatal("expired code accepted")
	}
	if c.system.service.ssoCallbackAllowed("https://evil.example/sso/callback") || c.system.service.ssoCallbackAllowed(c.site.web.URL+"/sso/callback/extra") {
		t.Fatal("open redirect allowed")
	}
	w := httptest.NewRecorder()
	c.site.service.Handler().ServeHTTP(w, httptest.NewRequest("GET", "/sso/callback?state="+strings.Repeat("s", 64), nil))
	if w.Code != 403 {
		t.Fatal("callback without browser state accepted")
	}
	login := *client
	login.Jar, _ = cookiejar.New(nil)
	response, err := login.PostForm(c.site.web.URL+"/login", url.Values{"username": {"administrator"}, "password": {integrationPassword}})
	if err != nil {
		t.Fatal(err)
	}
	response.Body.Close()
	u, _ := url.Parse(c.site.web.URL)
	var token string
	for _, cookie := range login.Jar.Cookies(u) {
		if cookie.Name == c.site.service.cookieName() {
			token = cookie.Value
		}
	}
	var raw []byte
	if err := c.site.service.Store.db.QueryRow("SELECT document FROM framework_documents WHERE name=?", authName("session", token)).Scan(&raw); err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(raw), token) {
		t.Fatal("raw bearer persisted")
	}
	request := httptest.NewRequest("GET", "/api/session", nil)
	request.AddCookie(&http.Cookie{Name: c.site.service.cookieName(), Value: token})
	if _, ok := c.site.service.session(request); !ok {
		t.Fatal("fresh session rejected")
	}
	// Cross-origin mutations and missing CSRF remain rejected after SSO.
	forged := httptest.NewRequest("POST", "/api/logout", nil)
	forged.Header.Set("Origin", "https://attacker.example")
	forged.AddCookie(&http.Cookie{Name: c.site.service.cookieName(), Value: token})
	denied := httptest.NewRecorder()
	c.site.service.Handler().ServeHTTP(denied, forged)
	if denied.Code != 403 {
		t.Fatal("cross-origin logout accepted")
	}
	var stored Session
	c.site.service.Store.authRead("session", token, &stored)
	logout := httptest.NewRequest("POST", "/api/logout", nil)
	logout.AddCookie(&http.Cookie{Name: c.site.service.cookieName(), Value: token})
	logout.Header.Set("X-CSRF-Token", stored.CSRF)
	out := httptest.NewRecorder()
	c.site.service.Handler().ServeHTTP(out, logout)
	if out.Code != 200 {
		t.Fatalf("logout failed: %s", out.Body)
	}
	if _, ok := c.site.service.session(request); ok {
		t.Fatal("logout retained persisted session")
	}
	c.site.service.Store.authPut("session", token, stored, time.Now().Add(-time.Second))
	if _, ok := c.site.service.session(request); ok {
		t.Fatal("expired persisted session accepted")
	}
	c.site.service.Store.authPut("session", token, stored, stored.Expires)
	// Password/account changes invalidate persistent sessions without a restart.
	c.site.service.Store.SetAccount("replacement", hash)
	if _, ok := c.site.service.session(request); ok {
		t.Fatal("changed account retained session")
	}

}

func TestSSOLoginPolicyDoesNotPermitUnregisteredOrigins(t *testing.T) {
	c, client := ssoTLSCluster(t)
	continuation := "/sso/authorize?redirect_uri=" + url.QueryEscape("https://evil.example/sso/callback")
	response, err := client.Get(c.system.web.URL + "/login?continue=" + url.QueryEscape(continuation))
	if err != nil {
		t.Fatal(err)
	}
	defer response.Body.Close()
	if policy := response.Header.Get("Content-Security-Policy"); !strings.HasSuffix(policy, "form-action 'self'") || strings.Contains(policy, "evil.example") {
		t.Fatal("unregistered origin expanded login CSP")
	}
}
