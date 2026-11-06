package framework

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"
)

func credentialFixture(t *testing.T) (string, InitReceipt) {
	t.Helper()
	seed, err := decodeFixture(t, initialFixture(t))
	if err != nil {
		t.Fatal(err)
	}
	root := filepath.Join(t.TempDir(), "framework")
	receipt, err := Initialize(seed, InitOptions{Root: root})
	if err != nil {
		t.Fatal(err)
	}
	return root, receipt
}

func credentialService(t *testing.T, configPath string) (*Service, *httptest.Server) {
	t.Helper()
	service, err := OpenService(configPath, integrationAssets())
	if err != nil {
		t.Fatal(err)
	}
	server := httptest.NewServer(service.InternalHandler())
	t.Cleanup(func() { server.Close(); _ = service.Close() })
	return service, server
}

func TestInitializedAndEnrolledSitesAuthenticateBothServiceDirections(t *testing.T) {
	root, receipt := credentialFixture(t)
	zoneConfig, err := ReadFileConfig(receipt.ConfigurationFiles[RoleZone])
	if err != nil {
		t.Fatal(err)
	}
	store, err := OpenStore(filepath.Join(zoneConfig.StateDir, "configuration.db"), RoleZone)
	if err != nil {
		t.Fatal(err)
	}
	var zone ZoneConfig
	raw, err := store.Config()
	if err == nil {
		err = json.Unmarshal(raw, &zone)
	}
	if err == nil {
		zone.Sites = append(zone.Sites, SiteIdentity{ID: "site-2", Name: "Second Site", SiteID: 2, NAC: 0x294, Endpoint: "http://127.0.0.1:15102"})
		err = store.SaveConfig(storeDoc(t, zone))
	}
	closeErr := store.Close()
	if err != nil {
		t.Fatal(err)
	}
	if closeErr != nil {
		t.Fatal(closeErr)
	}
	enrolledConfig, err := EnrollSite(root, "site-2", "http://127.0.0.1:15102", "127.0.0.1:15102", "http://127.0.0.1:15202", "127.0.0.1:15202")
	if err != nil {
		t.Fatal(err)
	}
	zoneService, zoneServer := credentialService(t, receipt.ConfigurationFiles[RoleZone])
	zoneCredentials, err := readCredentials(zoneConfig.CredentialFile)
	if err != nil {
		t.Fatal(err)
	}
	for _, test := range []struct{ name, path string }{
		{"initialized", receipt.ConfigurationFiles[RoleSite]},
		{"enrolled", enrolledConfig},
	} {
		t.Run(test.name, func(t *testing.T) {
			site, siteServer := credentialService(t, test.path)
			siteCredentials, err := readCredentials(site.Config().CredentialFile)
			if err != nil {
				t.Fatal(err)
			}
			zonePeer, ok := siteCredentials.Peers[zoneCredentials.OutboundID]
			if !ok || zonePeer != (PeerCredential{RoleZone, zoneService.Config().InstanceID, zoneCredentials.OutboundToken}) {
				t.Fatal("Site does not trust its Zone's outbound credential")
			}
			_, peer, err := zoneService.peerIdentityForSite(site.Config().InstanceID)
			if err != nil {
				t.Fatal(err)
			}
			if peer.Token != siteCredentials.OutboundToken || peer.Token == zoneCredentials.OutboundToken {
				t.Fatal("Site and Zone must retain their separate outbound credentials")
			}
			endpoint, err := zoneService.voice.Load().discover(context.Background(), siteServer.URL, &peer)
			if err != nil {
				t.Fatal("Zone to Site discovery", err)
			}
			if !strings.Contains(endpoint.URI, "site%3A") {
				t.Fatal("enrolled colon ID not preserved in canonical SIP URI")
			}
			if _, err = site.voice.Load().discover(context.Background(), zoneServer.URL, nil); err != nil {
				t.Fatal("Site to Zone discovery", err)
			}

			for _, token := range []string{"", strings.Repeat("x", 64), siteCredentials.OutboundToken} {
				headers := map[string]string{"X-Service-ID": zoneCredentials.OutboundID}
				if token != "" {
					headers["Authorization"] = "Bearer " + token
				}
				status, _ := integrationHTTP(t, http.DefaultClient, http.MethodPost, siteServer.URL+"/internal/voice", zoneService.voice.Load().endpoint, headers)
				if status != http.StatusUnauthorized {
					t.Fatalf("unaccepted Zone token: HTTP %d", status)
				}
			}
			for _, path := range []string{"/internal/media", "/internal/call/begin", "/internal/call/renew", "/internal/call/end"} {
				status, _ := integrationHTTP(t, http.DefaultClient, http.MethodPost, siteServer.URL+path, nil, map[string]string{"X-Service-ID": zoneCredentials.OutboundID, "Authorization": "Bearer " + zoneCredentials.OutboundToken})
				if status != http.StatusNotFound {
					t.Fatalf("obsolete media/call endpoint remains: %s HTTP%d", path, status)
				}
			}

		})
	}
}

func TestZoneSiteRequestsRequireAnEnrolledRecipient(t *testing.T) {
	_, receipt := credentialFixture(t)
	zone, _ := credentialService(t, receipt.ConfigurationFiles[RoleZone])
	site, _ := credentialService(t, receipt.ConfigurationFiles[RoleSite])
	_, peer, err := zone.peerIdentityForSite(site.Config().InstanceID)
	if err != nil {
		t.Fatal(err)
	}
	requests := make(chan http.Header, 8)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		requests <- r.Header.Clone()
		w.WriteHeader(http.StatusOK)
	}))
	defer server.Close()
	for _, test := range []struct {
		name   string
		caller *Service
		peer   PeerCredential
	}{
		{"unknown Site", zone, PeerCredential{RoleSite, "missing-site", peer.Token}},
		{"changed credential", zone, PeerCredential{RoleSite, peer.InstanceID, strings.Repeat("x", 64)}},
		{"wrong recipient role", zone, PeerCredential{RoleApplication, peer.InstanceID, peer.Token}},
		{"wrong caller role", site, peer},
	} {
		t.Run(test.name, func(t *testing.T) {
			if err := test.caller.requestAt(context.Background(), server.URL, http.MethodPost, "/internal/voice", nil, nil, &test.peer); err == nil {
				t.Fatal("unenrolled recipient accepted")
			}
		})
	}
	select {
	case <-requests:
		t.Fatal("request sent to an unenrolled recipient")
	default:
	}
	if err = zone.requestAt(context.Background(), server.URL, http.MethodPost, "/internal/voice", nil, nil, &peer); err != nil {
		t.Fatal(err)
	}
	headers := <-requests
	credentials, err := readCredentials(zone.Config().CredentialFile)
	if err != nil {
		t.Fatal(err)
	}
	if headers.Get("X-Service-ID") != credentials.OutboundID || headers.Get("Authorization") != "Bearer "+credentials.OutboundToken {
		t.Fatal("Zone request did not use its own outbound identity and token")
	}
}

func TestSitePeerIdentityUsesTheEnrollmentKey(t *testing.T) {
	path := filepath.Join(t.TempDir(), "credentials.json")
	peer := PeerCredential{Role: RoleSite, InstanceID: "site-1", Token: strings.Repeat("s", 64)}
	credentials := ServiceCredentials{Peers: map[string]PeerCredential{
		"site-1":      peer,
		"site:site-1": {Role: RoleSite, InstanceID: "another-site", Token: strings.Repeat("o", 64)},
		"wrong-role":  {Role: RoleApplication, InstanceID: "site-1", Token: strings.Repeat("d", 64)},
	}}
	if err := WritePrivateJSON(path, credentials); err != nil {
		t.Fatal(err)
	}
	service := &Service{config: FileConfig{CredentialFile: path}}
	identity, actual, err := service.peerIdentityForSite("site-1")
	if err != nil {
		t.Fatal(err)
	}
	if identity != "site-1" || actual != peer {
		t.Fatal("Site lookup did not return its enrolled identity and credential")
	}

	if identity, _, err = service.peerIdentityForSite("missing-site"); err == nil || identity != "" {
		t.Fatal("lookup returned an identity for an unenrolled Site")
	}
}
