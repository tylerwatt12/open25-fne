package main

import (
	"bytes"
	"crypto/ed25519"
	"encoding/hex"
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"testing"

	"golang.org/x/crypto/bcrypt"
	"open25.local/open25-fne/internal/framework"
)

func TestOneOffPreparationPreservesOwnerAndSiteState(t *testing.T) {
	hash, err := bcrypt.GenerateFromPassword([]byte("preparation-fixture"), bcrypt.MinCost)
	if err != nil {
		t.Fatal(err)
	}
	seed := framework.InitialConfig{System: framework.SystemConfig{ID: "system-1", Name: "Test system", WACN: 1, SystemID: 1, Callsign: "N0TEST", Radios: []framework.Radio{{RID: 10002, Alias: "Console", Kind: "console", Enabled: true}}, Talkgroups: []framework.Talkgroup{{TGID: 1001, Alias: "Test", Enabled: true, SiteAccess: "all"}}}, Zone: framework.ZoneConfig{ID: "zone-1", SystemID: "system-1", Name: "Zone", RFSSID: 1, ChannelIdentifiers: []framework.ChannelIdentifier{{Identifier: 2, BaseHz: 443437500, SpacingHz: 12500, OffsetHz: 5000000, BandwidthHz: 12500}}, Sites: []framework.SiteIdentity{{ID: "site-1", Name: "Site", SiteID: 1, NAC: 659}}}, Sites: []framework.SiteConfig{{BroadcastBandPlan: true, ID: "site-1", Modems: []framework.Modem{{ID: "modem-1", Name: "Modem", Device: "/dev/test-only", Baud: 115200}}, Channels: []framework.Channel{{ID: "control", Name: "Control", Role: "control", Identifier: 2, ChannelNo: 1, PowerPercent: 95, ModemID: "modem-1", Enabled: true}}, Identification: framework.Identification{Mode: "ham", WPM: 20, QuietSeconds: 60}}}, Application: framework.ApplicationConfig{Name: "Application Server"}, Administrator: framework.InitialAdministrator{Username: "owner", PasswordHash: string(hash)}}
	root := filepath.Join(t.TempDir(), "state")
	receipt, err := framework.Initialize(seed, framework.InitOptions{Root: root})
	if err != nil {
		t.Fatal(err)
	}
	// Model the current-format test state, before crypto enrollment was
	// activated. The preparation tool must create these identities once.
	sysBefore, e := framework.ReadFileConfig(receipt.ConfigurationFiles[framework.RoleSystem])
	if e != nil {
		t.Fatal(e)
	}
	if e = os.Remove(filepath.Join(sysBefore.StateDir, "crypto", "system-signing.key")); e != nil {
		t.Fatal(e)
	}
	applicationBefore, e := framework.ReadFileConfig(receipt.ConfigurationFiles[framework.RoleApplication])
	if e != nil {
		t.Fatal(e)
	}
	credentialsBeforeRaw, e := privateRead(applicationBefore.CredentialFile)
	if e != nil {
		t.Fatal(e)
	}
	var credentialsBefore framework.ServiceCredentials
	if e = json.Unmarshal(credentialsBeforeRaw, &credentialsBefore); e != nil {
		t.Fatal(e)
	}
	credentialsBefore.CryptoSystemPublicKey = ""
	if e = framework.WritePrivateJSON(applicationBefore.CredentialFile, credentialsBefore); e != nil {
		t.Fatal(e)
	}
	before := map[string][]byte{}
	for _, role := range []framework.Role{framework.RoleSite, framework.RoleZone} {
		cfg, err := framework.ReadFileConfig(receipt.ConfigurationFiles[role])
		if err != nil {
			t.Fatal(err)
		}
		for _, path := range []string{receipt.ConfigurationFiles[role], filepath.Join(cfg.StateDir, "configuration.db")} {
			before[path], err = os.ReadFile(path)
			if err != nil {
				t.Fatal(err)
			}
		}
	}
	codec := filepath.Join(t.TempDir(), "codec")
	if err = os.WriteFile(codec, []byte("#!/bin/sh\nexit 0\n"), 0700); err != nil {
		t.Fatal(err)
	}
	for _, apply := range []bool{false, true, true} {
		args := []string{"run", ".", "--root", root, "--codec", codec}
		if apply {
			args = append(args, "--apply")
		}
		command := exec.Command("go", args...)
		command.Env = append(os.Environ(), "GOFLAGS=-mod=readonly")
		output, err := command.CombinedOutput()
		if err != nil {
			t.Fatalf("preparation failed: %v %s", err, output)
		}
		if bytes.Contains(output, hash) || bytes.Contains(output, []byte("preparation-fixture")) {
			t.Fatal("preparation printed owner credentials")
		}
	}
	cfg, err := framework.ReadFileConfig(receipt.ConfigurationFiles[framework.RoleSystem])
	if err != nil {
		t.Fatal(err)
	}
	store, err := framework.OpenStore(filepath.Join(cfg.StateDir, "configuration.db"), framework.RoleSystem)
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	username, actual, err := store.Account()
	if err != nil || username != "owner" || !bytes.Equal(actual, hash) {
		t.Fatal("existing owner verifier changed", err)
	}
	raw, err := store.Config()
	if err != nil {
		t.Fatal(err)
	}
	var system framework.SystemConfig
	if err = json.Unmarshal(raw, &system); err != nil {
		t.Fatal(err)
	}
	if len(system.ConsolePositions) != 1 || system.ConsolePositions[0].RadioID != 10002 {
		t.Fatal("console position not derived once")
	}
	application, err := framework.ReadFileConfig(receipt.ConfigurationFiles[framework.RoleApplication])
	if err != nil || application.Media.CodecBinary != codec {
		t.Fatal("Application Server codec path not prepared", err)
	}
	signSeed, e := privateRead(filepath.Join(cfg.StateDir, "crypto", "system-signing.key"))
	if e != nil {
		t.Fatal(e)
	}
	defer clear(signSeed)
	signing := ed25519.NewKeyFromSeed(signSeed)
	defer clear(signing)
	credentialsRaw, e := privateRead(application.CredentialFile)
	if e != nil {
		t.Fatal(e)
	}
	var endpointCredentials framework.ServiceCredentials
	if e = json.Unmarshal(credentialsRaw, &endpointCredentials); e != nil || endpointCredentials.CryptoSystemPublicKey != hex.EncodeToString(signing.Public().(ed25519.PublicKey)) {
		t.Fatal("Application Server did not pin local System origin", e)
	}
	for path, content := range before {
		after, err := os.ReadFile(path)
		if err != nil || !bytes.Equal(content, after) {
			t.Fatal("Site/Zone state changed", path, err)
		}
	}
}
