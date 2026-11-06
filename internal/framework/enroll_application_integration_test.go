package framework

import (
	"bytes"
	"context"
	"crypto/ed25519"
	"encoding/hex"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"
)

func applicationEnrollmentService(t *testing.T, path string) *integrationService {
	t.Helper()
	service, server := credentialService(t, path)
	web := httptest.NewServer(service.Handler())
	t.Cleanup(web.Close)
	settings := service.Config().Settings()
	settings.PublicURL = web.URL
	settings.InternalURL = server.URL
	if err := service.SaveSettings(settings); err != nil {
		t.Fatal(err)
	}
	return &integrationService{service: service, server: server, web: web, configPath: path}
}

func TestEnrolledApplicationServicesAuthenticateAndReceiveOnlyExplicitKeyEnvelopes(t *testing.T) {
	root, receipt := applicationEnrollmentFixture(t, "[lab] ?")
	site2Path := applicationEnrollmentSite(t, root, receipt, "site-2", 2)
	// Existing peers are already live when a new Application is enrolled. Their
	// inbound authentication must observe only the new explicit credential.
	system := applicationEnrollmentService(t, receipt.ConfigurationFiles[RoleSystem])
	zone := applicationEnrollmentService(t, receipt.ConfigurationFiles[RoleZone])
	sites := []*integrationService{applicationEnrollmentService(t, receipt.ConfigurationFiles[RoleSite]), applicationEnrollmentService(t, site2Path)}
	original := applicationEnrollmentService(t, receipt.ConfigurationFiles[RoleApplication])
	path, err := EnrollApplication(root, "dispatch-2", "http://127.0.0.1:5013", "127.0.0.1:5013", "http://127.0.0.1:5113", "127.0.0.1:5113")
	if err != nil {
		t.Fatal(err)
	}
	second := applicationEnrollmentService(t, path)
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	readCatalog := func(t *testing.T, caller *integrationService, address string) catalog {
		t.Helper()
		var incoming catalog
		if e := caller.service.requestAt(ctx, address, http.MethodGet, "/internal/catalog", nil, &incoming, nil); e != nil {
			t.Fatal("authenticated catalog", e)
		}
		return incoming
	}
	for _, endpoint := range []*integrationService{original, second} {
		t.Run(endpoint.service.Config().InstanceID, func(t *testing.T) {
			incoming := readCatalog(t, endpoint, zone.server.URL)
			if incoming.System.ID != system.service.Config().InstanceID || incoming.Zone == nil || len(incoming.Zone.Sites) != 2 {
				t.Fatal("separate Application did not receive the real Zone directory")
			}
			if _, e := endpoint.service.voice.Load().discover(ctx, zone.server.URL, nil); e != nil {
				t.Fatal("Application-to-Zone SIP discovery", e)
			}
			if _, e := zone.service.voice.Load().discover(ctx, endpoint.server.URL, nil); e != nil {
				t.Fatal("Zone-to-Application SIP discovery", e)
			}
			for _, site := range sites {
				if readCatalog(t, endpoint, site.server.URL).System.ID != incoming.System.ID {
					t.Fatal("Site fallback catalog has different authority")
				}
				if _, e := endpoint.service.voice.Load().discover(ctx, site.server.URL, nil); e != nil {
					t.Fatal("Application-to-Site SIP discovery", e)
				}
				if _, e := site.service.voice.Load().discover(ctx, endpoint.server.URL, nil); e != nil {
					t.Fatal("Site-to-Application SIP discovery", e)
				}
			}
		})
	}
	secondCreds, err := readCredentials(second.service.Config().CredentialFile)
	if err != nil {
		t.Fatal(err)
	}
	originalCreds, err := readCredentials(original.service.Config().CredentialFile)
	if err != nil {
		t.Fatal(err)
	}
	for _, recipient := range append([]*integrationService{zone}, sites...) {
		status, _ := integrationHTTP(t, http.DefaultClient, http.MethodGet, recipient.server.URL+"/internal/catalog", nil, map[string]string{"X-Service-ID": secondCreds.OutboundID, "Authorization": "Bearer " + originalCreds.OutboundToken})
		if status != http.StatusUnauthorized {
			t.Fatal("a different Application's bearer authenticated the new identity", status)
		}
	}
	status, _ := integrationHTTP(t, http.DefaultClient, http.MethodGet, system.server.URL+"/internal/catalog", nil, map[string]string{"X-Service-ID": secondCreds.OutboundID, "Authorization": "Bearer " + secondCreds.OutboundToken})
	if status != http.StatusUnauthorized {
		t.Fatal("Application acquired direct System service access", status)
	}
	admin := integrationLogin(t, system, "test-only")
	key := publicFixtureKey()
	if status, _ = admin.request(t, http.MethodPost, "/api/crypto/keys", key); status != http.StatusOK {
		t.Fatal("owner key loading failed", status)
	}
	relayKeys := func() {
		incoming := readCatalog(t, zone, system.server.URL)
		if e := zone.service.acceptCatalog(incoming); e != nil {
			t.Fatal(e)
		}
	}
	relayKeys()
	if len(readCatalog(t, second, zone.server.URL).KeyEnvelopes) != 0 {
		t.Fatal("new Application received keys before explicit System enrollment")
	}
	if env := readCatalog(t, original, zone.server.URL).KeyEnvelopes; len(env) != 1 || env[original.service.Config().InstanceID].Recipient != original.service.Config().InstanceID {
		t.Fatal("original Application lost its independently enrolled envelope")
	}
	secondAdmin := integrationLogin(t, second, "test-only")
	status, raw := secondAdmin.request(t, http.MethodGet, "/api/crypto/identity", nil)
	var identity struct {
		Recipient string `json:"recipient"`
		PublicKey string `json:"public_key"`
	}
	if status != http.StatusOK || json.Unmarshal(raw, &identity) != nil || identity.Recipient != "dispatch-2" {
		t.Fatal("new Application public identity unavailable")
	}
	private, err := endpointKey(second.service.Config().StateDir)
	if err != nil {
		t.Fatal(err)
	}
	if identity.PublicKey != hex.EncodeToString(private.PublicKey().Bytes()) || bytes.Contains(raw, []byte(hex.EncodeToString(private.Bytes()))) {
		t.Fatal("public identity mismatches or exposes private material")
	}
	if status, _ = admin.request(t, http.MethodPost, "/api/crypto/recipients", map[string]string{"recipient": identity.Recipient, "public_key": identity.PublicKey}); status != http.StatusOK {
		t.Fatal("explicit recipient enrollment failed", status)
	}
	status, raw = admin.request(t, http.MethodGet, "/api/crypto/identity", nil)
	var systemIdentity struct {
		PublicKey string `json:"public_key"`
	}
	if status != http.StatusOK || json.Unmarshal(raw, &systemIdentity) != nil {
		t.Fatal("trusted System public pin unavailable")
	}
	pin, err := hex.DecodeString(systemIdentity.PublicKey)
	if err != nil || len(pin) != ed25519.PublicKeySize {
		t.Fatal("trusted System public pin malformed")
	}
	secondCreds.CryptoSystemPublicKey = systemIdentity.PublicKey
	if err = WritePrivateJSON(second.service.Config().CredentialFile, secondCreds); err != nil {
		t.Fatal(err)
	}
	relayKeys()
	for _, endpoint := range []*integrationService{original, second} {
		incoming := readCatalog(t, endpoint, zone.server.URL)
		id := endpoint.service.Config().InstanceID
		if len(incoming.KeyEnvelopes) != 1 || incoming.KeyEnvelopes[id].Recipient != id {
			t.Fatal("catalog did not isolate its recipient envelope")
		}
		encoded, _ := json.Marshal(incoming)
		if bytes.Contains(encoded, []byte(key.Material)) {
			t.Fatal("Zone catalog exposed traffic-key bytes")
		}
		ownPrivate, e := endpointKey(endpoint.service.Config().StateDir)
		if e != nil {
			t.Fatal(e)
		}
		opened, e := openKeyEnvelope(ownPrivate, incoming.System.ID, id, incoming.KeyEnvelopes[id], ed25519.PublicKey(pin))
		if e != nil || len(opened) != 1 || opened[0] != key {
			t.Fatal("authorized independent Application cannot open its own sealed inventory", e)
		}
		if endpoint == original {
			if _, e = openKeyEnvelope(private, incoming.System.ID, id, incoming.KeyEnvelopes[id], ed25519.PublicKey(pin)); e == nil {
				t.Fatal("new Application private identity opened the original recipient's inventory")
			}
		}
	}
	for _, site := range sites {
		// The real Site's authenticated catalog request carries no key envelopes.
		if len(readCatalog(t, site, zone.server.URL).KeyEnvelopes) != 0 {
			t.Fatal("Site received a Application traffic-key envelope")
		}
	}
}
