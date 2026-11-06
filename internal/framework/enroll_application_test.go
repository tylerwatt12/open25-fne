package framework

import (
	"bytes"
	"encoding/json"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
)

func applicationEnrollmentFixture(t *testing.T, name string) (string, InitReceipt) {
	t.Helper()
	seed, err := decodeFixture(t, initialFixture(t))
	if err != nil {
		t.Fatal(err)
	}
	root := filepath.Join(t.TempDir(), name)
	receipt, err := Initialize(seed, InitOptions{Root: root})
	if err != nil {
		t.Fatal(err)
	}
	return root, receipt
}

func applicationEnrollmentReadConfig(t *testing.T, path string) FileConfig {
	t.Helper()
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	var cfg FileConfig
	if err = DecodeStrict(raw, &cfg); err != nil {
		t.Fatal(err)
	}
	return cfg
}

func applicationEnrollmentSite(t *testing.T, root string, receipt InitReceipt, id string, number int) string {
	t.Helper()
	cfg := applicationEnrollmentReadConfig(t, receipt.ConfigurationFiles[RoleZone])
	store, err := OpenStore(filepath.Join(cfg.StateDir, "configuration.db"), RoleZone)
	if err != nil {
		t.Fatal(err)
	}
	raw, err := store.Config()
	var zone ZoneConfig
	if err == nil {
		err = json.Unmarshal(raw, &zone)
	}
	if err == nil {
		zone.Sites = append(zone.Sites, SiteIdentity{ID: id, Name: id, SiteID: number, NAC: 0x293 + number})
		err = store.SaveConfig(storeDoc(t, zone))
	}
	closeErr := store.Close()
	if err != nil {
		t.Fatal(err)
	}
	if closeErr != nil {
		t.Fatal(closeErr)
	}
	path, err := EnrollSite(root, id, "http://127.0.0.1:15102", "127.0.0.1:15102", "http://127.0.0.1:15202", "127.0.0.1:15202")
	if err != nil {
		t.Fatal(err)
	}
	return path
}

func applicationEnrollmentFiles(t *testing.T, paths ...string) map[string][]byte {
	t.Helper()
	files := make(map[string][]byte, len(paths))
	for _, path := range paths {
		raw, err := os.ReadFile(path)
		if err != nil {
			t.Fatal(err)
		}
		files[path] = raw
	}
	return files
}

func applicationEnrollmentFilesUnchanged(t *testing.T, before map[string][]byte) {
	t.Helper()
	for path, raw := range before {
		current, err := os.ReadFile(path)
		if err != nil || !bytes.Equal(raw, current) {
			t.Fatalf("protected enrollment file changed: %s (%v)", path, err)
		}
	}
}

func TestEnrollApplicationCreatesIndependentStateWithoutGrantingTrafficKeys(t *testing.T) {
	for _, rootName := range []string{"framework", "[lab] ?"} {
		t.Run(rootName, func(t *testing.T) {
			root, receipt := applicationEnrollmentFixture(t, rootName)
			site2Path := applicationEnrollmentSite(t, root, receipt, "site-2", 2)
			zoneCfg := applicationEnrollmentReadConfig(t, receipt.ConfigurationFiles[RoleZone])
			original := applicationEnrollmentReadConfig(t, receipt.ConfigurationFiles[RoleApplication])
			systemCfg := applicationEnrollmentReadConfig(t, receipt.ConfigurationFiles[RoleSystem])
			zoneCreds, err := readCredentials(zoneCfg.CredentialFile)
			if err != nil {
				t.Fatal(err)
			}
			remote := PeerCredential{RoleSite, "remote-site", strings.Repeat("r", 64)}
			zoneCreds.Peers["site:remote-site"] = remote
			if err = WritePrivateJSON(zoneCfg.CredentialFile, zoneCreds); err != nil {
				t.Fatal(err)
			}
			zoneCfg.TLSCertificate = filepath.Join(root, "declared-certificate.pem")
			zoneCfg.TLSKey = filepath.Join(root, "declared-private-key.pem")
			zoneCfg.PublicURL = "https://127.0.0.1:5001"
			if err = WritePrivateJSON(receipt.ConfigurationFiles[RoleZone], zoneCfg); err != nil {
				t.Fatal(err)
			}
			system, _ := credentialService(t, receipt.ConfigurationFiles[RoleSystem])
			if err = system.writeTrafficKeys([]trafficKey{publicFixtureKey()}); err != nil {
				t.Fatal(err)
			}
			keysBefore, _, err := system.Store.ReadCache("traffic_keys")
			if err != nil {
				t.Fatal(err)
			}
			protected := applicationEnrollmentFiles(t, receipt.ConfigurationFiles[RoleSystem], systemCfg.CredentialFile,
				filepath.Join(systemCfg.StateDir, "crypto", "system-signing.key"), filepath.Join(systemCfg.StateDir, "crypto", "master.key"),
				receipt.ConfigurationFiles[RoleApplication], original.CredentialFile, filepath.Join(original.StateDir, "crypto", "endpoint.key"),
				receipt.ConfigurationFiles[RoleZone], receipt.ConfigurationFiles[RoleSite], site2Path)
			hidden := filepath.Join(root, "sites", ".site-init-held", "service.json")
			if err = WritePrivateJSON(hidden, FileConfig{Role: RoleSystem}); err != nil {
				t.Fatal(err)
			}
			hiddenBefore := applicationEnrollmentFiles(t, hidden)
			path, err := EnrollApplication(root, "dispatch-2", "https://127.0.0.1:5013", "127.0.0.1:5013", "http://127.0.0.1:5113", "127.0.0.1:5113")
			if err != nil {
				t.Fatal(err)
			}
			cfg, err := ReadFileConfig(path)
			if err != nil {
				t.Fatal(err)
			}
			if cfg.InstanceID != "dispatch-2" || cfg.StateDir == original.StateDir || cfg.Role != RoleApplication {
				t.Fatal("new Application lacks independent identity/state")
			}
			if cfg.TLSCertificate != zoneCfg.TLSCertificate || cfg.TLSKey != zoneCfg.TLSKey || cfg.Radio.AllowRF || cfg.Radio.Engine != "" || cfg.Media.VoiceListen != "127.0.0.1:0" || cfg.Media.VoiceAddress != "127.0.0.1:0" {
				t.Fatal("declared TLS or independent media/RF settings changed")
			}
			creds, err := readCredentials(cfg.CredentialFile)
			if err != nil {
				t.Fatal(err)
			}
			oldCreds, _ := readCredentials(original.CredentialFile)
			if creds.OutboundID != "application:dispatch-2" || len(creds.OutboundToken) < 32 || creds.OutboundToken == oldCreds.OutboundToken || creds.CryptoSystemPublicKey != "" || len(creds.CryptoRecipients) != 0 {
				t.Fatal("identity was copied or key authorization granted implicitly")
			}
			if creds.Peers[zoneCreds.OutboundID] != (PeerCredential{RoleZone, zoneCfg.InstanceID, zoneCreds.OutboundToken}) || creds.Peers["site:remote-site"] != remote {
				t.Fatal("Zone/remote-Site peer lost")
			}
			for _, file := range []string{receipt.ConfigurationFiles[RoleSite], site2Path} {
				siteCfg := applicationEnrollmentReadConfig(t, file)
				siteCreds, e := readCredentials(siteCfg.CredentialFile)
				if e != nil || creds.Peers[siteCreds.OutboundID] != (PeerCredential{RoleSite, siteCfg.InstanceID, siteCreds.OutboundToken}) || siteCreds.Peers[creds.OutboundID] != (PeerCredential{RoleApplication, cfg.InstanceID, creds.OutboundToken}) {
					t.Fatal("local Site reciprocal trust missing", e)
				}
			}
			keyPath := filepath.Join(cfg.StateDir, "crypto", "endpoint.key")
			newKey := applicationEnrollmentFiles(t, keyPath)[keyPath]
			if len(newKey) != 32 || bytes.Equal(newKey, protected[filepath.Join(original.StateDir, "crypto", "endpoint.key")]) {
				t.Fatal("private endpoint identity was copied")
			}
			store, err := OpenStore(filepath.Join(cfg.StateDir, "configuration.db"), RoleApplication)
			if err != nil {
				t.Fatal(err)
			}
			raw, err := store.Config()
			var applicationCfg ApplicationConfig
			if err != nil || json.Unmarshal(raw, &applicationCfg) != nil || applicationCfg.Name != "dispatch-2" || applicationCfg.ConnectionURL != zoneCfg.InternalURL || applicationCfg.LocalSiteURL != "" {
				t.Fatal("new configuration not initialized explicitly", err)
			}
			user, verifier, err := store.Account()
			zoneStore, e := OpenStore(filepath.Join(zoneCfg.StateDir, "configuration.db"), RoleZone)
			if e != nil {
				t.Fatal(e)
			}
			zoneUser, zoneVerifier, e := zoneStore.Account()
			zoneStore.Close()
			if err != nil || e != nil || user != zoneUser || !bytes.Equal(verifier, zoneVerifier) {
				t.Fatal("accepted administrator verifier changed")
			}
			for _, name := range []string{"system", "zone", "topology"} {
				if _, _, err = store.ReadCache(name); err != nil {
					t.Fatal("accepted cache missing", name, err)
				}
			}
			for _, name := range []string{"traffic_keys", "key_envelopes"} {
				if _, _, err = store.ReadCache(name); err == nil {
					t.Fatal("new endpoint copied key cache", name)
				}
			}
			store.Close()
			for _, file := range []string{path, cfg.CredentialFile, keyPath} {
				info, e := os.Stat(file)
				if e != nil || info.Mode().Perm() != 0600 {
					t.Fatal("private enrollment file not owner-only", file, e)
				}
			}
			applicationEnrollmentFilesUnchanged(t, protected)
			applicationEnrollmentFilesUnchanged(t, hiddenBefore)
			keysAfter, _, err := system.Store.ReadCache("traffic_keys")
			if err != nil || !bytes.Equal(keysBefore, keysAfter) {
				t.Fatal("System traffic-key cache changed", err)
			}
		})
	}
}

func TestEnrollSiteAfterAdditionalApplicationesUpdatesEveryLocalPeer(t *testing.T) {
	root, receipt := applicationEnrollmentFixture(t, "[lab] ?")
	paths := []string{receipt.ConfigurationFiles[RoleApplication]}
	for _, id := range []string{"dispatch-2", "dispatch-3"} {
		path, err := EnrollApplication(root, id, "http://127.0.0.1:5013", "127.0.0.1:5013", "http://127.0.0.1:5113", "127.0.0.1:5113")
		if err != nil {
			t.Fatal(err)
		}
		paths = append(paths, path)
	}
	before := map[string]ServiceCredentials{}
	for _, path := range paths {
		cfg := applicationEnrollmentReadConfig(t, path)
		creds, err := readCredentials(cfg.CredentialFile)
		if err != nil {
			t.Fatal(err)
		}
		before[path] = creds
	}
	hidden := filepath.Join(root, "applications", ".application-init-held", "service.json")
	if err := WritePrivateJSON(hidden, FileConfig{Role: RoleSystem}); err != nil {
		t.Fatal(err)
	}
	hiddenBefore := applicationEnrollmentFiles(t, hidden)
	sitePath := applicationEnrollmentSite(t, root, receipt, "site-2", 2)
	siteCfg := applicationEnrollmentReadConfig(t, sitePath)
	siteCreds, err := readCredentials(siteCfg.CredentialFile)
	if err != nil {
		t.Fatal(err)
	}
	for _, path := range paths {
		cfg := applicationEnrollmentReadConfig(t, path)
		creds, e := readCredentials(cfg.CredentialFile)
		if e != nil || creds.Peers[siteCreds.OutboundID] != (PeerCredential{RoleSite, siteCfg.InstanceID, siteCreds.OutboundToken}) || siteCreds.Peers[creds.OutboundID] != (PeerCredential{RoleApplication, cfg.InstanceID, creds.OutboundToken}) {
			t.Fatal("later Site reciprocal trust missing", cfg.InstanceID, e)
		}
		delete(creds.Peers, siteCreds.OutboundID)
		if !reflect.DeepEqual(creds, before[path]) {
			t.Fatal("later Site changed an existing Application identity or peer")
		}
	}
	applicationEnrollmentFilesUnchanged(t, hiddenBefore)
}

func TestEnrollApplicationPreservesIdentityAndSavedOperatingMisconfiguration(t *testing.T) {
	root, receipt := applicationEnrollmentFixture(t, "framework")
	siteCfg := applicationEnrollmentReadConfig(t, receipt.ConfigurationFiles[RoleSite])
	siteCfg.Radio.AllowRF = true
	if err := WritePrivateJSON(receipt.ConfigurationFiles[RoleSite], siteCfg); err != nil {
		t.Fatal(err)
	}
	before := applicationEnrollmentFiles(t, receipt.ConfigurationFiles[RoleSite])
	primary := applicationEnrollmentReadConfig(t, receipt.ConfigurationFiles[RoleApplication])
	originalStore, err := OpenStore(filepath.Join(primary.StateDir, "configuration.db"), RoleApplication)
	if err != nil {
		t.Fatal(err)
	}
	originalRaw, err := originalStore.Config()
	if err == nil {
		err = originalStore.WriteCache("enrollment-local-preference", []byte(`{"selected":1001}`))
	}
	originalStore.Close()
	if err != nil {
		t.Fatal(err)
	}
	path, err := EnrollApplication(root, "dispatch-2", "http://127.0.0.1:5013", "", "http://127.0.0.1:5113", "")
	if err != nil {
		t.Fatal("saved Site operating settings became a compatibility preflight", err)
	}
	applicationEnrollmentFilesUnchanged(t, before)
	cfg := applicationEnrollmentReadConfig(t, path)
	if cfg.Listen != "127.0.0.1:5013" || cfg.InternalListen != "127.0.0.1:5113" {
		t.Fatal("explicit origins did not supply omitted listener arguments")
	}
	store, err := OpenStore(filepath.Join(cfg.StateDir, "configuration.db"), RoleApplication)
	if err != nil {
		t.Fatal(err)
	}
	if _, _, err = store.ReadCache("enrollment-local-preference"); err == nil {
		t.Fatal("another Application's preferences were copied")
	}
	store.Close()
	zoneCfg := applicationEnrollmentReadConfig(t, receipt.ConfigurationFiles[RoleZone])
	protected := applicationEnrollmentFiles(t, cfg.CredentialFile, zoneCfg.CredentialFile, siteCfg.CredentialFile, primary.CredentialFile, filepath.Join(cfg.StateDir, "crypto", "endpoint.key"))
	if _, err = EnrollApplication(root, "dispatch-2", cfg.PublicURL, cfg.Listen, cfg.InternalURL, cfg.InternalListen); err == nil {
		t.Fatal("existing Application files overwritten")
	}
	if _, err = EnrollApplication(root, primary.InstanceID, cfg.PublicURL, cfg.Listen, cfg.InternalURL, cfg.InternalListen); err == nil {
		t.Fatal("existing bearer identity replaced")
	}
	applicationEnrollmentFilesUnchanged(t, protected)
	entries, err := os.ReadDir(filepath.Join(root, "applications"))
	if err != nil || len(entries) != 1 || entries[0].Name() != "dispatch-2" {
		t.Fatal("failed creation left staged or published replacement", err)
	}
	originalStore, err = OpenStore(filepath.Join(primary.StateDir, "configuration.db"), RoleApplication)
	if err != nil {
		t.Fatal(err)
	}
	afterRaw, err := originalStore.Config()
	preference, _, e := originalStore.ReadCache("enrollment-local-preference")
	originalStore.Close()
	if err != nil || e != nil || !bytes.Equal(originalRaw, afterRaw) || string(preference) != `{"selected":1001}` {
		t.Fatal("original Application configuration/preferences changed")
	}
}

func TestEnrollmentCredentialWritesRollbackWhenPublicationFails(t *testing.T) {
	dir := t.TempDir()
	configs := []FileConfig{{CredentialFile: filepath.Join(dir, "zone.json")}, {CredentialFile: filepath.Join(dir, "site.json")}}
	for i, cfg := range configs {
		creds := ServiceCredentials{OutboundID: []string{"zone:one", "site:one"}[i], OutboundToken: strings.Repeat("a", 64), Peers: map[string]PeerCredential{"existing": {RoleApplication, "existing", strings.Repeat("e", 64)}}}
		if err := WritePrivateJSON(cfg.CredentialFile, creds); err != nil {
			t.Fatal(err)
		}
	}
	before := applicationEnrollmentFiles(t, configs[0].CredentialFile, configs[1].CredentialFile)
	stage, destination := filepath.Join(dir, "stage"), filepath.Join(dir, "published")
	for _, p := range []string{stage, destination} {
		if err := os.Mkdir(p, 0700); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.WriteFile(filepath.Join(destination, "existing-instance"), []byte("preserved"), 0600); err != nil {
		t.Fatal(err)
	}
	if err := commitEnrollment(stage, destination, "dispatch:new", PeerCredential{RoleApplication, "new", strings.Repeat("n", 64)}, configs); err == nil {
		t.Fatal("publication over nonempty instance succeeded")
	}
	applicationEnrollmentFilesUnchanged(t, before)
	marker, err := os.ReadFile(filepath.Join(destination, "existing-instance"))
	if err != nil || string(marker) != "preserved" {
		t.Fatal("existing instance changed after failed publication", err)
	}
}
