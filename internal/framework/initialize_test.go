package framework

import (
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
)

func TestInitializeCreatesIndependentCurrentStoresAndRefusesOverwrite(t *testing.T) {
	seed, err := decodeFixture(t, initialFixture(t))
	if err != nil {
		t.Fatal(err)
	}
	root := filepath.Join(t.TempDir(), "framework")
	receipt, err := Initialize(seed, InitOptions{Root: root, SystemURL: "http://127.0.0.1:15000", ZoneURL: "http://127.0.0.1:15001", SiteURL: "http://127.0.0.1:15002", ApplicationURL: "http://127.0.0.1:15003"})
	if err != nil {
		t.Fatal(err)
	}
	paths := map[string]bool{}
	for _, role := range []Role{RoleSystem, RoleZone, RoleSite, RoleApplication} {
		cfg, err := ReadFileConfig(receipt.ConfigurationFiles[role])
		if err != nil {
			t.Fatal(err)
		}
		if cfg.Role != role || paths[cfg.StateDir] {
			t.Fatal("services share state or use wrong role")
		}
		paths[cfg.StateDir] = true
		store, err := OpenStore(filepath.Join(cfg.StateDir, "configuration.db"), role)
		if err != nil {
			t.Fatal(err)
		}
		if _, err = store.Config(); err != nil {
			t.Fatal(role, err)
		}
		u, hash, err := store.Account()
		if err != nil || u != seed.Administrator.Username || string(hash) != seed.Administrator.PasswordHash {
			t.Fatal("administrator verifier changed")
		}
		if role == RoleApplication {
			var dispatch ApplicationConfig
			raw, err := store.Config()
			if err != nil || json.Unmarshal(raw, &dispatch) != nil || dispatch.ConnectionURL != receipt.InternalURLs[RoleZone] || dispatch.LocalSiteURL != receipt.InternalURLs[RoleSite] {
				t.Fatal("Application Server does not point to the initialized Zone and optional local Site")
			}
		}
		store.Close()
		info, err := os.Stat(cfg.CredentialFile)
		if err != nil || info.Mode().Perm() != 0600 {
			t.Fatal("credentials are not private")
		}
	}
	systemCfg, _ := ReadFileConfig(receipt.ConfigurationFiles[RoleSystem])
	if systemCfg.Listen != "127.0.0.1:15000" {
		t.Fatal("explicit server port was ignored")
	}
	if _, err = Initialize(seed, InitOptions{Root: root}); err == nil {
		t.Fatal("existing framework overwritten")
	}
	if receipt.RFEnabled {
		t.Fatal("initialization enabled RF")
	}
}

func TestEnrollSiteUsesNewStoreAndScopedServiceCredential(t *testing.T) {
	seed, err := decodeFixture(t, initialFixture(t))
	if err != nil {
		t.Fatal(err)
	}
	root := filepath.Join(t.TempDir(), "framework")
	receipt, err := Initialize(seed, InitOptions{Root: root})
	if err != nil {
		t.Fatal(err)
	}
	zoneCfg, _ := ReadFileConfig(receipt.ConfigurationFiles[RoleZone])
	zoneStore, err := OpenStore(filepath.Join(zoneCfg.StateDir, "configuration.db"), RoleZone)
	if err != nil {
		t.Fatal(err)
	}
	var zone ZoneConfig
	raw, _ := zoneStore.Config()
	_ = json.Unmarshal(raw, &zone)
	zone.Sites = append(zone.Sites, SiteIdentity{ID: "site-2", Name: "Second", SiteID: 2, NAC: 0x293, Endpoint: "http://127.0.0.1:15102"})
	raw, _ = json.Marshal(zone)
	if err = zoneStore.SaveConfig(raw); err != nil {
		t.Fatal(err)
	}
	zoneStore.Close()
	path, err := EnrollSite(root, "site-2", "http://127.0.0.1:15102", "127.0.0.1:15102", "http://127.0.0.1:15202", "127.0.0.1:15202")
	if err != nil {
		t.Fatal(err)
	}
	cfg, err := ReadFileConfig(path)
	if err != nil {
		t.Fatal(err)
	}
	if cfg.InstanceID != "site-2" || cfg.StateDir == zoneCfg.StateDir {
		t.Fatal("Site not independent")
	}
	creds, err := readCredentials(cfg.CredentialFile)
	if err != nil {
		t.Fatal(err)
	}
	parentCreds, _ := readCredentials(zoneCfg.CredentialFile)
	p, ok := parentCreds.Peers[creds.OutboundID]
	if !ok || p.InstanceID != "site-2" || p.Role != RoleSite || p.Token != creds.OutboundToken {
		t.Fatal("Site credential is not scoped")
	}
	store, err := OpenStore(filepath.Join(cfg.StateDir, "configuration.db"), RoleSite)
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	if _, err = store.Config(); err != nil {
		t.Fatal(err)
	}
	raw, err = store.Config()
	var enrolled SiteConfig
	if err != nil || json.Unmarshal(raw, &enrolled) != nil || enrolled.ControlPolicy != (ControlPolicy{Mode: "static", RotationHours: 24}) {
		t.Fatalf("newly enrolled Site does not have the approved editable control starting values: %+v (%v)", enrolled.ControlPolicy, err)
	}
	if _, err = EnrollSite(root, "site-2", cfg.PublicURL, cfg.Listen, cfg.InternalURL, cfg.InternalListen); err == nil {
		t.Fatal("Site files overwritten")
	}
}

func TestInitializePreservesMisconfiguredInitialSettings(t *testing.T) {
	seed, err := decodeFixture(t, initialFixture(t))
	if err != nil {
		t.Fatal(err)
	}
	seed.System.WACN = 0
	seed.Sites[0].ControlPolicy = ControlPolicy{Mode: "entered-mode", RotationEnabled: true, RotationHours: -3.5}
	receipt, err := Initialize(seed, InitOptions{Root: filepath.Join(t.TempDir(), "installation")})
	if err != nil {
		t.Fatal(err)
	}
	if receipt.RFEnabled {
		t.Fatal("initialization enabled RF")
	}
	cfg, err := ReadFileConfig(receipt.ConfigurationFiles[RoleSite])
	if err != nil {
		t.Fatal(err)
	}
	store, err := OpenStore(filepath.Join(cfg.StateDir, "configuration.db"), RoleSite)
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	raw, err := store.Config()
	var saved SiteConfig
	if err != nil || json.Unmarshal(raw, &saved) != nil || saved.ControlPolicy != seed.Sites[0].ControlPolicy {
		t.Fatalf("initialization replaced administrator-entered control settings: %+v (%v)", saved.ControlPolicy, err)
	}
}
