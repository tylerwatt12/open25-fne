package framework

import (
	"context"
	"crypto/x509"
	"encoding/pem"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestInternalAndWebHandlersExposeSeparateRoutes(t *testing.T) {
	for _, role := range []Role{RoleSystem, RoleZone, RoleSite, RoleApplication} {
		t.Run(string(role), func(t *testing.T) {
			credentials := filepath.Join(t.TempDir(), "credentials.json")
			if err := WritePrivateJSON(credentials, ServiceCredentials{Peers: map[string]PeerCredential{}}); err != nil {
				t.Fatal(err)
			}
			s := &Service{config: FileConfig{Role: role, InstanceID: "test", StateDir: t.TempDir(), CredentialFile: credentials}, assets: integrationAssets()}
			for _, test := range []struct {
				name, path string
				handler    http.Handler
				want       int
			}{
				{"web_health", "/healthz", s.Handler(), 200},
				{"internal_health", "/healthz", s.InternalHandler(), 200},
				{"web_signin_redirect", "/", s.Handler(), http.StatusSeeOther},
				{"web_rejects_internal", "/internal/catalog", s.Handler(), 404},
				{"internal_requires_enrollment", "/internal/catalog", s.InternalHandler(), 401},
				{"internal_rejects_login", "/api/login", s.InternalHandler(), 404},
				{"internal_rejects_operator", "/api/state", s.InternalHandler(), 404},
				{"internal_rejects_assets", "/index.html", s.InternalHandler(), 404},
			} {
				t.Run(test.name, func(t *testing.T) {
					r := httptest.NewRequest(http.MethodGet, test.path, nil)
					r.RemoteAddr = "127.0.0.1:1234"
					w := httptest.NewRecorder()
					test.handler.ServeHTTP(w, r)
					if w.Code != test.want {
						t.Fatalf("HTTP %d; want %d", w.Code, test.want)
					}
				})
			}
			s.config.AllowedCIDR = "198.51.100.0/24"
			for _, h := range []http.Handler{s.Handler(), s.InternalHandler()} {
				r := httptest.NewRequest(http.MethodGet, "/healthz", nil)
				r.RemoteAddr = "192.0.2.1:1234"
				w := httptest.NewRecorder()
				h.ServeHTTP(w, r)
				if w.Code != 403 {
					t.Fatal("listener bypassed client network restriction")
				}
			}
		})
	}
}

func splitTLSFixture(t *testing.T) (string, string, *http.Client) {
	t.Helper()
	server := httptest.NewTLSServer(http.NotFoundHandler())
	defer server.Close()
	cert := server.TLS.Certificates[0]
	key, err := x509.MarshalPKCS8PrivateKey(cert.PrivateKey)
	if err != nil {
		t.Fatal(err)
	}
	dir := t.TempDir()
	certPath, keyPath := filepath.Join(dir, "web.crt"), filepath.Join(dir, "web.key")
	if err = os.WriteFile(certPath, pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: cert.Certificate[0]}), 0600); err != nil {
		t.Fatal(err)
	}
	if err = os.WriteFile(keyPath, pem.EncodeToMemory(&pem.Block{Type: "PRIVATE KEY", Bytes: key}), 0600); err != nil {
		t.Fatal(err)
	}
	client := server.Client()
	client.Timeout = 500 * time.Millisecond
	t.Cleanup(client.CloseIdleConnections)
	return certPath, keyPath, client
}
func splitFreeAddress(t *testing.T) string {
	t.Helper()
	l, err := net.Listen("tcp4", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	address := l.Addr().String()
	if err = l.Close(); err != nil {
		t.Fatal(err)
	}
	return address
}
func splitAwaitHealth(t *testing.T, client *http.Client, address string, done <-chan error) {
	t.Helper()
	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		select {
		case err := <-done:
			t.Fatalf("service ended before listeners were ready: %v", err)
		default:
		}
		response, err := client.Get(address + "/healthz")
		if err == nil {
			io.Copy(io.Discard, response.Body)
			response.Body.Close()
			if response.StatusCode == 200 {
				return
			}
		}
		time.Sleep(5 * time.Millisecond)
	}
	t.Fatal("listener did not become ready")
}
func TestServiceDualListenerTLSAndHTTPLifecycle(t *testing.T) {
	certificate, key, webClient := splitTLSFixture(t)
	webAddress, internalAddress := splitFreeAddress(t), splitFreeAddress(t)
	s := &Service{config: FileConfig{Role: RoleSystem, InstanceID: "system-1", Listen: webAddress, InternalListen: internalAddress, PublicURL: "https://" + webAddress, InternalURL: "http://" + internalAddress, TLSCertificate: certificate, TLSKey: key}}
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	done := make(chan error, 1)
	go func() { done <- s.Run(ctx) }()
	internalClient := newHTTPClient()
	internalClient.Timeout = 500 * time.Millisecond
	defer internalClient.CloseIdleConnections()
	splitAwaitHealth(t, webClient, s.config.PublicURL, done)
	splitAwaitHealth(t, internalClient, s.config.InternalURL, done)
	for _, test := range []struct {
		client  *http.Client
		address string
	}{{webClient, s.config.PublicURL + "/internal/catalog"}, {internalClient, s.config.InternalURL + "/api/login"}} {
		response, err := test.client.Get(test.address)
		if err != nil {
			t.Fatal(err)
		}
		io.Copy(io.Discard, response.Body)
		response.Body.Close()
		if response.StatusCode != 404 {
			t.Fatal("listener exposed the other endpoint's routes")
		}
	}
	cancel()
	select {
	case err := <-done:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("listener shutdown did not finish")
	}
	for _, address := range []string{webAddress, internalAddress} {
		l, err := net.Listen("tcp", address)
		if err != nil {
			t.Fatal("owned listener leaked", err)
		}
		l.Close()
	}
}
func TestServiceDualListenerStartupFailureClosesSockets(t *testing.T) {
	certificate, key, _ := splitTLSFixture(t)
	occupied, err := net.Listen("tcp4", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer occupied.Close()
	address := splitFreeAddress(t)
	s := &Service{config: FileConfig{Role: RoleSystem, InstanceID: "system-1", Listen: address, InternalListen: occupied.Addr().String(), TLSCertificate: certificate, TLSKey: key}}
	if err = s.Run(t.Context()); err == nil {
		t.Fatal("occupied internal listener accepted")
	}
	l, err := net.Listen("tcp", address)
	if err != nil {
		t.Fatal("internal startup failure leaked the web address", err)
	}
	l.Close()
}

func TestWebFailureKeepsSiteBackendRunning(t *testing.T) {
	for _, fault := range []string{"occupied_port", "missing_TLS_key"} {
		t.Run(fault, func(t *testing.T) {
			certificate, key, webClient := splitTLSFixture(t)
			s, _, configPath := localControlFixture(t)
			webAddress, internalAddress := splitFreeAddress(t), splitFreeAddress(t)
			var release func() error
			if fault == "occupied_port" {
				occupied, err := net.Listen("tcp", webAddress)
				if err != nil {
					t.Fatal(err)
				}
				defer occupied.Close()
				release = occupied.Close
			} else {
				goodKey, err := os.ReadFile(key)
				if err != nil {
					t.Fatal(err)
				}
				key = filepath.Join(t.TempDir(), "repaired.key")
				release = func() error { return os.WriteFile(key, goodKey, 0600) }
			}
			s.mu.Lock()
			s.config.Listen, s.config.InternalListen = webAddress, internalAddress
			s.config.PublicURL, s.config.InternalURL = "https://"+webAddress, "http://"+internalAddress
			s.config.TLSCertificate, s.config.TLSKey = certificate, key
			cfg := s.config
			s.mu.Unlock()
			if err := WritePrivateJSON(configPath, cfg); err != nil {
				t.Fatal(err)
			}
			ctx, cancel := context.WithCancel(t.Context())
			done := make(chan error, 1)
			stopped := make(chan struct{})
			go func() { defer close(stopped); done <- s.Run(ctx) }()
			t.Cleanup(func() {
				cancel()
				select {
				case <-stopped:
					select {
					case err := <-done:
						if err != nil {
							t.Error(err)
						}
					default:
					}
				case <-time.After(2 * time.Second):
					t.Error("web retry kept the backend from stopping")
				}
			})
			internalClient := newHTTPClient()
			internalClient.Timeout = 500 * time.Millisecond
			defer internalClient.CloseIdleConnections()
			splitAwaitHealth(t, internalClient, cfg.InternalURL, done)
			statusCtx, stop := context.WithTimeout(t.Context(), time.Second)
			status, err := ControlRadio(statusCtx, configPath, "status", "")
			stop()
			if err != nil || status.State != "running" || status.SiteID != "site-1" {
				t.Fatal("web failure interrupted local Site control", status, err)
			}
			plain := &http.Client{Timeout: 100 * time.Millisecond}
			if response, err := plain.Get("http://" + webAddress + "/healthz"); err == nil {
				response.Body.Close()
				if response.StatusCode == http.StatusOK {
					t.Fatal("failed HTTPS listener fell back to plaintext")
				}
			}
			if err := release(); err != nil {
				t.Fatal(err)
			}
			splitAwaitHealth(t, webClient, cfg.PublicURL, done)
		})
	}
}

func TestWebRetryStopsWithBackend(t *testing.T) {
	internalAddress := splitFreeAddress(t)
	s := &Service{config: FileConfig{Role: RoleSystem, InstanceID: "system-1", Listen: splitFreeAddress(t), InternalListen: internalAddress, TLSCertificate: "missing.crt", TLSKey: "missing.key"}}
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	done := make(chan error, 1)
	go func() { done <- s.Run(ctx) }()
	client := newHTTPClient()
	client.Timeout = 500 * time.Millisecond
	defer client.CloseIdleConnections()
	splitAwaitHealth(t, client, "http://"+internalAddress, done)
	cancel()
	select {
	case err := <-done:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(500 * time.Millisecond):
		t.Fatal("shutdown waited for the web retry interval")
	}
	listener, err := net.Listen("tcp", internalAddress)
	if err != nil {
		t.Fatal("backend listener leaked while stopping web retry", err)
	}
	listener.Close()
}

func TestInternalOriginsAndSettingsAreExplicit(t *testing.T) {
	base := FileConfig{Role: RoleZone, InstanceID: "zone-1", Listen: "127.0.0.1:5001", PublicURL: "http://127.0.0.1:5001", InternalListen: "198.51.100.1:5101", InternalURL: "http://198.51.100.1:5101", UpstreamURL: "http://198.51.100.2:5100", PollSeconds: 2, StateDir: t.TempDir(), CredentialFile: "credentials.json"}
	if err := base.Validate(); err != nil {
		t.Fatal(err)
	}
	for _, test := range []struct {
		name   string
		change func(*FileConfig)
	}{
		{"missing_internal_url", func(c *FileConfig) { c.InternalURL = "" }},
		{"missing_internal_listener", func(c *FileConfig) { c.InternalListen = "" }},
		{"secure_internal_url", func(c *FileConfig) { c.InternalURL = "https://198.51.100.1:5101" }},
		{"secure_upstream", func(c *FileConfig) { c.UpstreamURL = "https://198.51.100.2:5100" }},
		{"shared_listener", func(c *FileConfig) { c.InternalListen = c.Listen }},
		{"network_web_http", func(c *FileConfig) { c.PublicURL = "http://198.51.100.1:5001" }},
		{"mismatched_web_tls", func(c *FileConfig) { c.PublicURL = "https://127.0.0.1:5001" }},
	} {
		t.Run(test.name, func(t *testing.T) {
			c := base
			test.change(&c)
			if c.Validate() == nil {
				t.Fatal("unsupported configuration accepted")
			}
		})
	}
	if DecodeStrict([]byte(`{"ca_file":"obsolete.pem"}`), new(FileConfig)) == nil {
		t.Fatal("obsolete internal CA field accepted")
	}
	store := storeOpen(t, RoleZone)
	s := &Service{Store: store, config: base, configPath: filepath.Join(t.TempDir(), "service.json")}
	settings := base.Settings()
	settings.InternalURL = "http://198.51.100.3:5101"
	if err := s.SaveSettings(settings); err != nil {
		t.Fatal(err)
	}
	saved, err := ReadFileConfig(s.configPath)
	if err != nil {
		t.Fatal(err)
	}
	if saved.InternalURL != settings.InternalURL || saved.PublicURL != base.PublicURL || saved.InternalListen != base.InternalListen {
		t.Fatal("saving connection settings changed listener/web identity or lost internal address")
	}
}
func TestReportsAndNavigationKeepAdministrationSeparate(t *testing.T) {
	c := integrationNewCluster(t)
	integrationSyncAll(t, c)
	report, err := c.site.service.ownSiteReport()
	if err != nil {
		t.Fatal(err)
	}
	if report.Endpoint != c.site.server.URL || report.AdminURL != c.site.web.URL || report.Endpoint == report.AdminURL {
		t.Fatal("Site report conflated internal and web addresses")
	}
	for _, node := range []*integrationService{c.system, c.zone, c.site, c.dispatch} {
		_, zone, err := node.service.documents()
		if err != nil {
			t.Fatal(err)
		}
		tree := node.service.adminTopology(zone)
		if tree.SystemURL != c.system.web.URL || tree.ZoneURL != c.zone.web.URL {
			t.Fatalf("%s navigation lost explicit parent administration addresses: %#v", node.service.Config().Role, tree)
		}
		for _, address := range append([]string{tree.SystemURL, tree.ZoneURL, tree.ApplicationURL}, topologySites(tree)...) {
			for _, peer := range []*integrationService{c.system, c.zone, c.site, c.otherSite, c.dispatch} {
				if address == peer.server.URL {
					t.Fatal("internal endpoint became a navigation link")
				}
			}
		}
	}
	zone := storeFixtureZone()
	zone.Sites[0].Endpoint = "http://198.51.100.1:5102"
	zone.Sites[0].AdminURL = ""
	isolated := &Service{Store: storeOpen(t, RoleZone), config: FileConfig{Role: RoleZone, PublicURL: "https://admin.example"}}
	if got := isolated.adminTopology(&zone).SiteURLs["site-1"]; got != "" {
		t.Fatal("missing administration URL fell back to internal endpoint")
	}
	if err := validateInternalURL("http://198.51.100.1:5102", false); err != nil || validateInternalURL("https://198.51.100.1:5102", false) == nil {
		t.Fatal("internal origin scheme changed")
	}
	if !strings.HasPrefix(c.system.service.Config().PublicURL, "http://127.0.0.1:") {
		t.Fatal("software fixture unexpectedly changed its web transport")
	}
}
