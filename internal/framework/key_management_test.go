package framework

import (
	"bytes"
	"crypto/ecdh"
	"crypto/ed25519"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"net/http"
	"open25.local/open25-fne/internal/p25crypto"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
)

func keyManagementSystem(t *testing.T) *integrationService {
	t.Helper()
	system := storeFixtureSystem()
	return startIntegrationService(t, t.TempDir(), RoleSystem, system.ID, system, ServiceCredentials{Peers: map[string]PeerCredential{}}, system, storeFixtureZone())
}
func pinFixtureSystem(t *testing.T, system *Service, endpoint *Service) ed25519.PublicKey {
	t.Helper()
	signing, e := systemSigningKey(system.Config().StateDir)
	if e != nil {
		t.Fatal(e)
	}
	defer clear(signing)
	public := signing.Public().(ed25519.PublicKey)
	if endpoint != nil {
		creds, e := readCredentials(endpoint.Config().CredentialFile)
		if e != nil {
			t.Fatal(e)
		}
		creds.CryptoSystemPublicKey = hex.EncodeToString(public)
		if e = WritePrivateJSON(endpoint.Config().CredentialFile, creds); e != nil {
			t.Fatal(e)
		}
	}
	return public
}
func publicFixtureKey() trafficKey {
	return trafficKey{AlgorithmID: 132, KeyID: 7, Name: "Shared test key", Enabled: true, Material: strings.Repeat("0123456789abcdef", 4)}
}
func TestTrafficKeysEncryptedAtRestAndRedactedOverOwnerHTTP(t *testing.T) {
	pair := keyManagementSystem(t)
	client := integrationLogin(t, pair, integrationPassword)
	key := publicFixtureKey()
	status, reply := client.request(t, http.MethodPost, "/api/crypto/keys", key)
	if status != 200 {
		t.Fatalf("save key: %d %s", status, reply)
	}
	status, reply = client.request(t, http.MethodGet, "/api/crypto/keys", nil)
	if status != 200 || bytes.Contains(reply, []byte(key.Material)) || bytes.Contains(reply, []byte("material")) {
		t.Fatalf("owner key response exposed material: %d %s", status, reply)
	}
	raw, _, err := pair.service.Store.ReadCache("traffic_keys")
	if err != nil {
		t.Fatal(err)
	}
	if bytes.Contains(raw, []byte(key.Material)) || bytes.Contains(raw, []byte(key.Name)) {
		t.Fatal("database stores plaintext traffic-key inventory")
	}
	plain, err := hex.DecodeString(key.Material)
	if err != nil {
		t.Fatal(err)
	}
	for _, suffix := range []string{"configuration.db", "configuration.db-wal"} {
		content, err := os.ReadFile(filepath.Join(pair.service.Config().StateDir, suffix))
		if err != nil && os.IsNotExist(err) {
			continue
		}
		if err != nil {
			t.Fatal(err)
		}
		if bytes.Contains(content, plain) || bytes.Contains(content, []byte(key.Material)) {
			t.Fatal("traffic key found in database pages", suffix)
		}
	}
	saved, err := pair.service.readTrafficKeys()
	if err != nil || len(saved) != 1 || saved[0] != key {
		t.Fatal("protected store did not round trip", err)
	}
	info, err := os.Stat(filepath.Join(pair.service.Config().StateDir, "crypto", "master.key"))
	if err != nil || info.Mode().Perm() != 0600 {
		t.Fatal("master key is not owner-only", err)
	}
}
func TestTrafficKeyEnvelopeBoundToRecipientAndSystem(t *testing.T) {
	pair := keyManagementSystem(t)
	key := publicFixtureKey()
	if err := pair.service.writeTrafficKeys([]trafficKey{key}); err != nil {
		t.Fatal(err)
	}
	endpoint, err := ecdh.X25519().GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	credentials, err := readCredentials(pair.service.Config().CredentialFile)
	if err != nil {
		t.Fatal(err)
	}
	credentials.CryptoRecipients = map[string]string{"application-1": hex.EncodeToString(endpoint.PublicKey().Bytes())}
	if err = WritePrivateJSON(pair.service.Config().CredentialFile, credentials); err != nil {
		t.Fatal(err)
	}
	envelopes, err := pair.service.keyEnvelopes()
	if err != nil {
		t.Fatal(err)
	}
	env := envelopes["application-1"]
	pin := pinFixtureSystem(t, pair.service, nil)
	plain, err := openKeyEnvelope(endpoint, pair.service.view().System.ID, "application-1", env, pin)
	if err != nil || len(plain) != 1 || plain[0] != key {
		t.Fatal("enrolled recipient cannot open its keys", err)
	}
	raw, _ := json.Marshal(envelopes)
	if bytes.Contains(raw, []byte(key.Material)) || bytes.Contains(raw, []byte(key.Name)) {
		t.Fatal("envelope leaked key inventory")
	}
	wrong, err := ecdh.X25519().GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	for name, test := range map[string]func() error{
		"wrong private key": func() error {
			_, e := openKeyEnvelope(wrong, pair.service.view().System.ID, "application-1", env, pin)
			return e
		},
		"wrong system": func() error {
			_, e := openKeyEnvelope(endpoint, "different-system", "application-1", env, pin)
			return e
		},
		"wrong recipient": func() error {
			_, e := openKeyEnvelope(endpoint, pair.service.view().System.ID, "dispatch-2", env, pin)
			return e
		},
		"tampered ciphertext": func() error {
			v := env
			v.Ciphertext = append([]byte{}, env.Ciphertext...)
			v.Ciphertext[0] ^= 1
			_, e := openKeyEnvelope(endpoint, pair.service.view().System.ID, "application-1", v, pin)
			return e
		},
		"tampered public key": func() error {
			v := env
			v.Public = make([]byte, 32)
			_, e := openKeyEnvelope(endpoint, pair.service.view().System.ID, "application-1", v, pin)
			return e
		},
		"short nonce": func() error {
			v := env
			v.Nonce = nil
			_, e := openKeyEnvelope(endpoint, pair.service.view().System.ID, "application-1", v, pin)
			return e
		},
	} {
		t.Run(name, func(t *testing.T) {
			if test() == nil {
				t.Fatal("invalid envelope accepted")
			}
		})
	}
}
func TestMalformedCryptoCachesFailRatherThanEmptyInventory(t *testing.T) {
	system := keyManagementSystem(t)
	if err := system.service.writeTrafficKeys([]trafficKey{publicFixtureKey()}); err != nil {
		t.Fatal(err)
	}
	for _, raw := range []string{"{", `null`, `{"nonce":"","ciphertext":""}`, `{"unexpected":true}`} {
		if _, err := system.service.Store.db.Exec("UPDATE framework_documents SET document=? WHERE name='cache:traffic_keys'", raw); err != nil {
			t.Fatal(err)
		}
		if keys, err := system.service.readTrafficKeys(); err == nil {
			t.Fatalf("invalid protected cache became empty/successful: %s %+v", raw, keys)
		}
	}
	profile := storeFixtureSystem()
	zone := storeFixtureZone()
	dispatch := startIntegrationService(t, t.TempDir(), RoleApplication, "application-1", ApplicationConfig{Name: "Application Server", ConnectionURL: "http://127.0.0.1:5001"}, ServiceCredentials{Peers: map[string]PeerCredential{}}, profile, zone)
	if envelopes, err := dispatch.service.keyEnvelopes(); err != nil || len(envelopes) != 0 {
		t.Fatal("uninitialized inventory should be empty", err)
	}
	for _, raw := range []string{`null`, `{"application-1":{}}`, `{"application-1":{"recipient":"other"}}`} {
		if err := dispatch.service.Store.WriteCache("key_envelopes", json.RawMessage(raw)); err != nil {
			t.Fatal(err)
		}
		if _, err := dispatch.service.keyEnvelopes(); err == nil {
			t.Fatal("malformed cached envelopes accepted", raw)
		}
	}
	if err := dispatch.service.writeTrafficKeys([]trafficKey{publicFixtureKey()}); err == nil {
		t.Fatal("Application Server took System key custody")
	}
}
func TestCryptoEnrollmentSavesUnusablePublicKeyAndRejectsPrivateFileSymlink(t *testing.T) {
	pair := keyManagementSystem(t)
	client := integrationLogin(t, pair, integrationPassword)
	status, _ := client.request(t, http.MethodPost, "/api/crypto/recipients", map[string]string{"recipient": "application-1", "public_key": strings.Repeat("00", 32)})
	if status != 200 {
		t.Fatal("administrator recipient save was refused", status)
	}
	creds, err := readCredentials(pair.service.Config().CredentialFile)
	if err != nil || creds.CryptoRecipients["application-1"] != strings.Repeat("00", 32) {
		t.Fatal("entered recipient changed", err)
	}
	if _, err = pair.service.keyEnvelopes(); err == nil {
		t.Fatal("unusable recipient encrypted a key envelope")
	}
	file := filepath.Join(t.TempDir(), "key")
	if err := os.WriteFile(file, make([]byte, 32), 0600); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(file, file+"-link"); err != nil {
		t.Fatal(err)
	}
	if _, err := privateBytes(file+"-link", 32, false); err == nil {
		t.Fatal("private key symlink accepted")
	}
	if err := os.Chmod(file, 0644); err != nil {
		t.Fatal(err)
	}
	if _, err := privateBytes(file, 32, false); err == nil {
		t.Fatal("readable private key accepted")
	}
}
func TestInitializeEnrollsTheFinalApplicationPrivateIdentity(t *testing.T) {
	seed, err := decodeFixture(t, initialFixture(t))
	if err != nil {
		t.Fatal(err)
	}
	root := filepath.Join(t.TempDir(), "services")
	receipt, err := Initialize(seed, InitOptions{Root: root})
	if err != nil {
		t.Fatal(err)
	}
	system, err := ReadFileConfig(receipt.ConfigurationFiles[RoleSystem])
	if err != nil {
		t.Fatal(err)
	}
	dispatch, err := ReadFileConfig(receipt.ConfigurationFiles[RoleApplication])
	if err != nil {
		t.Fatal(err)
	}
	credentials, err := readCredentials(system.CredentialFile)
	if err != nil {
		t.Fatal(err)
	}
	private, err := endpointKey(dispatch.StateDir)
	if err != nil {
		t.Fatal(err)
	}
	if credentials.CryptoRecipients[dispatch.InstanceID] != hex.EncodeToString(private.PublicKey().Bytes()) {
		t.Fatal("initialized System enrollment does not match final Application Server private key")
	}
	systemSigning, e := systemSigningKey(system.StateDir)
	if e != nil {
		t.Fatal(e)
	}
	defer clear(systemSigning)
	dispatchCredentials, e := readCredentials(dispatch.CredentialFile)
	if e != nil || dispatchCredentials.CryptoSystemPublicKey != hex.EncodeToString(systemSigning.Public().(ed25519.PublicKey)) {
		t.Fatal("Application Server did not pin final System signing identity", e)
	}
	privatePath := filepath.Join(dispatch.StateDir, "crypto", "endpoint.key")
	info, err := os.Stat(privatePath)
	if err != nil || info.Mode().Perm() != 0600 {
		t.Fatal("final Application Server private identity is unavailable or public", err)
	}
	raw, err := os.ReadFile(system.CredentialFile)
	if err != nil {
		t.Fatal(err)
	}
	if bytes.Contains(raw, []byte(hex.EncodeToString(private.Bytes()))) {
		t.Fatal("System credentials contain Application Server private key")
	}
	for _, role := range []Role{RoleZone, RoleSite} {
		cfg, err := ReadFileConfig(receipt.ConfigurationFiles[role])
		if err != nil {
			t.Fatal(err)
		}
		if _, err = os.Stat(filepath.Join(cfg.StateDir, "crypto", "endpoint.key")); !os.IsNotExist(err) {
			t.Fatal("Zone/Site received a crypto endpoint private key")
		}
	}
}

func TestSealedTrafficKeysSyncOnlyToTheirApplicationRecipient(t *testing.T) {
	c := integrationNewCluster(t)
	key := publicFixtureKey()
	if err := c.system.service.writeTrafficKeys([]trafficKey{key}); err != nil {
		t.Fatal(err)
	}
	private, err := endpointKey(c.dispatch.service.Config().StateDir)
	if err != nil {
		t.Fatal(err)
	}
	other, err := ecdh.X25519().GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	creds, err := readCredentials(c.system.service.Config().CredentialFile)
	if err != nil {
		t.Fatal(err)
	}
	creds.CryptoRecipients = map[string]string{"application-1": hex.EncodeToString(private.PublicKey().Bytes()), "dispatch-2": hex.EncodeToString(other.PublicKey().Bytes())}
	if err = WritePrivateJSON(c.system.service.Config().CredentialFile, creds); err != nil {
		t.Fatal(err)
	}
	integrationSyncAll(t, c)
	zoneEnvelopes, err := c.zone.service.keyEnvelopes()
	if err != nil || len(zoneEnvelopes) != 2 {
		t.Fatal("Zone did not relay sealed recipients", err)
	}
	envelopes, err := c.dispatch.service.keyEnvelopes()
	if err != nil || len(envelopes) != 1 {
		t.Fatal("Application Server received another recipient's envelope", err)
	}
	pin := pinFixtureSystem(t, c.system.service, c.dispatch.service)
	keys, err := openKeyEnvelope(private, c.dispatch.service.view().System.ID, "application-1", envelopes["application-1"], pin)
	if err != nil || len(keys) != 1 || keys[0] != key {
		t.Fatal("Application Server could not open its synchronized inventory", err)
	}
	for _, site := range []*integrationService{c.site, c.otherSite} {
		envelopes, err := site.service.keyEnvelopes()
		if err != nil || len(envelopes) != 0 {
			t.Fatal("Site received traffic-key envelopes", err)
		}
	}
	for _, pair := range []*integrationService{c.zone, c.site, c.otherSite, c.dispatch} {
		rows, err := pair.service.Store.db.Query("SELECT document FROM framework_documents")
		if err != nil {
			t.Fatal(err)
		}
		for rows.Next() {
			var raw []byte
			if err = rows.Scan(&raw); err != nil {
				rows.Close()
				t.Fatal(err)
			}
			if bytes.Contains(raw, []byte(key.Material)) {
				rows.Close()
				t.Fatal("plaintext key found in replicated service database", pair.service.Config().Role)
			}
		}
		rows.Close()
	}
}

func TestApplicationRejectsAttackerReplacementKeyEnvelope(t *testing.T) {
	system := keyManagementSystem(t)
	endpoint, e := ecdh.X25519().GenerateKey(rand.Reader)
	if e != nil {
		t.Fatal(e)
	}
	pin := pinFixtureSystem(t, system.service, nil)
	attacker, e := ecdh.X25519().GenerateKey(rand.Reader)
	if e != nil {
		t.Fatal(e)
	}
	systemID := system.service.view().System.ID
	aad := systemID + "/dispatch-1"
	a, e := envelopeAEAD(attacker, endpoint.PublicKey().Bytes(), aad)
	if e != nil {
		t.Fatal(e)
	}
	nonce := make([]byte, a.NonceSize())
	if _, e = rand.Read(nonce); e != nil {
		t.Fatal(e)
	}
	known := publicFixtureKey()
	known.Material = strings.Repeat("ff", 32)
	plain, _ := json.Marshal([]trafficKey{known})
	replacement := keyEnvelope{SystemID: systemID, Recipient: "application-1", Public: attacker.PublicKey().Bytes(), Nonce: nonce, Ciphertext: a.Seal(nil, nonce, plain, []byte(aad))}
	// The replacement is valid encryption to this Application Server, but the untrusted
	// relay cannot sign it as System Administration.
	if _, e = openKeyEnvelope(endpoint, systemID, "application-1", replacement, pin); e == nil {
		t.Fatal("unsigned attacker-chosen traffic key accepted")
	}
	_, attackerSigning, e := ed25519.GenerateKey(rand.Reader)
	if e != nil {
		t.Fatal(e)
	}
	replacement.Signature = ed25519.Sign(attackerSigning, envelopeMessage(replacement))
	if _, e = openKeyEnvelope(endpoint, systemID, "application-1", replacement, pin); e == nil {
		t.Fatal("relay signing identity substituted for pinned System")
	}
}

func TestTrafficKeysUseAlgorithmAndKeyIDPairWithoutFallback(t *testing.T) {
	pair := keyManagementSystem(t)
	client := integrationLogin(t, pair, integrationPassword)
	keys := []trafficKey{
		{AlgorithmID: 129, KeyID: 7, Name: "DES fixture", Enabled: true, Material: strings.Repeat("12", 8)},
		{AlgorithmID: 132, KeyID: 7, Name: "AES fixture", Enabled: true, Material: strings.Repeat("34", 32)},
		{AlgorithmID: 170, KeyID: 7, Name: "ADP fixture", Enabled: true, Material: strings.Repeat("56", 5)},
	}
	for _, key := range keys {
		if status, reply := client.request(t, http.MethodPost, "/api/crypto/keys", key); status != 200 {
			t.Fatalf("save %d: %d %s", key.AlgorithmID, status, reply)
		}
	}
	saved, err := pair.service.readTrafficKeys()
	if err != nil || len(saved) != 3 {
		t.Fatalf("same KID lost an algorithm: %d %v", len(saved), err)
	}
	if status, reply := client.request(t, http.MethodGet, "/api/crypto/keys", nil); status != 200 || !bytes.Contains(reply, []byte(`"algorithms"`)) || bytes.Contains(reply, []byte(`"material"`)) {
		t.Fatalf("sanitized catalog response: %d %s", status, reply)
	}
	recipient, err := ecdh.X25519().GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	credentials, err := readCredentials(pair.service.Config().CredentialFile)
	if err != nil {
		t.Fatal(err)
	}
	credentials.CryptoRecipients = map[string]string{"application-1": hex.EncodeToString(recipient.PublicKey().Bytes())}
	if err = WritePrivateJSON(pair.service.Config().CredentialFile, credentials); err != nil {
		t.Fatal(err)
	}
	envelopes, err := pair.service.keyEnvelopes()
	if err != nil {
		t.Fatal(err)
	}
	opened, err := openKeyEnvelope(recipient, pair.service.view().System.ID, "application-1", envelopes["application-1"], pinFixtureSystem(t, pair.service, nil))
	if err != nil || len(opened) != len(keys) {
		t.Fatal("multi-algorithm envelope lost pairs", err)
	}
	for i, key := range keys {
		if opened[i] != key {
			t.Fatal("sealed inventory altered algorithm/key identity")
		}
	}
	if status, _ := client.request(t, http.MethodDelete, "/api/crypto/keys/129/7", nil); status != 200 {
		t.Fatalf("delete DES: %d", status)
	}
	saved, err = pair.service.readTrafficKeys()
	if err != nil || len(saved) != 2 || saved[0].AlgorithmID != 132 || saved[1].AlgorithmID != 170 {
		t.Fatal("deleting DES removed same-KID keys", err)
	}
}

func TestEncryptedKeyIDZeroRemainsAlgorithmScoped(t *testing.T) {
	pair := keyManagementSystem(t)
	client := integrationLogin(t, pair, integrationPassword)
	for _, algorithm := range p25crypto.Algorithms() {
		if !algorithm.Supported {
			continue
		}
		material := strings.Repeat("ab", algorithm.KeyBytes)
		if algorithm.ID == 0x83 {
			// AAAD-B-2015 B.4 (PDF46/printed36): three independent DES
			// subkeys are required; a repeated-octet bundle is not option1.
			material = "0123456789ABCDEF23456789ABCDEF01456789ABCDEF0123"
		}
		key := trafficKey{AlgorithmID: algorithm.ID, KeyID: 0, Name: algorithm.Name + " zero", Enabled: true, Material: material}
		if status, reply := client.request(t, http.MethodPost, "/api/crypto/keys", key); status != 200 {
			t.Fatalf("zero key %d save: %d %s", algorithm.ID, status, reply)
		}
	}
	keys, err := pair.service.readTrafficKeys()
	if err != nil || len(keys) != 4 {
		t.Fatal("encrypted zero IDs collided", err)
	}
	if status, _ := client.request(t, http.MethodDelete, "/api/crypto/keys/129/0", nil); status != 200 {
		t.Fatal("zero ID delete failed")
	}
	keys, err = pair.service.readTrafficKeys()
	if err != nil || len(keys) != 3 {
		t.Fatal("zero ID delete removed other algorithms", err)
	}
}

func TestTrafficKeySavePreservesUnusableIdentityWithoutValidation(t *testing.T) {
	pair := keyManagementSystem(t)
	client := integrationLogin(t, pair, integrationPassword)
	original := publicFixtureKey()
	if err := pair.service.writeTrafficKeys([]trafficKey{original}); err != nil {
		t.Fatal(err)
	}
	input := map[string]any{"algorithm_id": 999, "key_id": -7, "name": "entered", "enabled": true, "material": "invalid material"}
	for _, path := range []string{"/api/crypto/keys/132/7", "/api/crypto/keys/999/-7"} {
		if status, _ := client.request(t, http.MethodPut, path, input); status != 200 {
			t.Fatal("submitted key save refused", status)
		}
	}
	saved, err := pair.service.readTrafficKeys()
	if err != nil || len(saved) != 1 || saved[0].AlgorithmID != 999 || saved[0].KeyID != -7 || saved[0].Material != "invalid material" {
		t.Fatal("submitted key settings were changed", err)
	}
	for range 2 {
		if status, _ := client.request(t, http.MethodDelete, "/api/crypto/keys/999/-7", nil); status != 200 {
			t.Fatal("repeat delete refused", status)
		}
	}
}

func TestProtectedTrafficKeyConfigurationPreservesEnteredValues(t *testing.T) {
	pair := keyManagementSystem(t)
	keys := []trafficKey{{AlgorithmID: 159, KeyID: -1, Name: "  Future key  ", Enabled: true, Material: "not hexadecimal"}, {AlgorithmID: 159, KeyID: -1, Name: "duplicate"}}
	if err := pair.service.writeTrafficKeys(keys); err != nil {
		t.Fatal(err)
	}
	saved, err := pair.service.readTrafficKeys()
	if err != nil || !reflect.DeepEqual(saved, keys) {
		t.Fatal("protected settings changed", err)
	}
	client := integrationLogin(t, pair, integrationPassword)
	if status, _ := client.request(t, http.MethodPost, "/api/crypto/keys", map[string]any{"algorithm_id": 159, "key_id": 70000, "name": "  Unimplemented  ", "material": "future"}); status != 200 {
		t.Fatalf("entered key rejected: %d", status)
	}
	if status, _ := client.request(t, http.MethodPut, "/api/crypto/keys/159/70000", map[string]any{"algorithm_id": 159, "key_id": 70000, "name": "  Unimplemented  ", "material": ""}); status != 200 {
		t.Fatalf("empty entered material rejected: %d", status)
	}
	saved, err = pair.service.readTrafficKeys()
	if err != nil || len(saved) != 3 || saved[2].Material != "" || saved[2].Name != "  Unimplemented  " {
		t.Fatal("entered empty material or spacing changed", err)
	}
}
