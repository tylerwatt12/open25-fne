package main

import (
	"os"
	"path/filepath"
	"testing"

	"golang.org/x/crypto/bcrypt"
	"open25.local/open25-fne/internal/framework"
)

func TestEnrollApplicationCommandCreatesSeparateCurrentInstance(t *testing.T) {
	verifier, err := bcrypt.GenerateFromPassword([]byte("test-only"), bcrypt.MinCost)
	if err != nil {
		t.Fatal(err)
	}
	root := filepath.Join(t.TempDir(), "framework")
	seed := framework.InitialConfig{
		System:        framework.SystemConfig{ID: "system-1"},
		Zone:          framework.ZoneConfig{ID: "zone-1", SystemID: "system-1", Sites: []framework.SiteIdentity{{ID: "site-1"}}},
		Sites:         []framework.SiteConfig{{ID: "site-1"}},
		Administrator: framework.InitialAdministrator{Username: "administrator", PasswordHash: string(verifier)},
	}
	initial, err := framework.Initialize(seed, framework.InitOptions{Root: root})
	if err != nil {
		t.Fatal(err)
	}
	original, err := os.ReadFile(initial.ConfigurationFiles[framework.RoleApplication])
	if err != nil {
		t.Fatal(err)
	}
	if err = run([]string{"enroll-application", "--root", root, "--application-id", "dispatch-2", "--public-url", "http://127.0.0.1:15013", "--internal-url", "http://127.0.0.1:15113"}); err != nil {
		t.Fatal(err)
	}
	cfg, err := framework.ReadFileConfig(filepath.Join(root, "applications", "dispatch-2", "service.json"))
	if err != nil {
		t.Fatal(err)
	}
	if cfg.InstanceID != "dispatch-2" || cfg.Role != framework.RoleApplication || cfg.Listen != "127.0.0.1:15013" || cfg.InternalListen != "127.0.0.1:15113" || cfg.Radio.AllowRF {
		t.Fatalf("new command did not create its independent stopped configuration: %+v", cfg)
	}
	after, err := os.ReadFile(initial.ConfigurationFiles[framework.RoleApplication])
	if err != nil || string(after) != string(original) {
		t.Fatal("new instance changed the original Application Server configuration", err)
	}
}
