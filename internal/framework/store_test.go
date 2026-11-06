package framework

import (
	"bytes"
	"database/sql"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"testing"

	"golang.org/x/crypto/bcrypt"
)

func storeFixtureSystem() SystemConfig {
	return SystemConfig{ID: "system-1", Name: "Laboratory", WACN: 1, SystemID: 1, Callsign: "N0TEST", Radios: []Radio{{RID: 10001, Alias: "Portable", Enabled: true, Kind: "subscriber"}}, Talkgroups: []Talkgroup{{TGID: 1001, AdmissionMode: "clear", TransmitMode: "clear", Alias: "Test 1", Enabled: true, SiteAccess: "all", SiteIDs: []string{}}}}
}
func storeFixtureZone() ZoneConfig {
	return ZoneConfig{ID: "zone-1", SystemID: "system-1", Name: "Zone 1", RFSSID: 1, ChannelIdentifiers: []ChannelIdentifier{{Identifier: 2, BaseHz: 443437500, SpacingHz: 12500, OffsetHz: 5000000, BandwidthHz: 12500}}, Sites: []SiteIdentity{{ID: "site-1", Name: "Site 1", SiteID: 1, NAC: 0x293, Endpoint: "http://localhost:5102", AdminURL: "https://localhost:5002"}}}
}
func storeFixtureSite() SiteConfig {
	return SiteConfig{BroadcastBandPlan: true, ID: "site-1", Modems: []Modem{{ID: "modem-1", Name: "Control modem", Device: "/dev/trunksystem-control", Baud: 115200}}, Channels: []Channel{{ID: "channel-1", Name: "Control", Role: "control", Identifier: 2, ChannelNo: 1, PowerPercent: 95, ModemID: "modem-1", Enabled: true}}, Identification: Identification{Mode: "ham", WPM: 20, QuietSeconds: 60}}
}
func storeDoc(t *testing.T, value any) json.RawMessage {
	t.Helper()
	raw, err := json.Marshal(value)
	if err != nil {
		t.Fatal(err)
	}
	return raw
}
func storeOpen(t *testing.T, role Role) *Store {
	t.Helper()
	s, err := OpenStore(filepath.Join(t.TempDir(), "state.db"), role)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { s.Close() })
	return s
}

func TestFrameworkStoreRetainsLocalConfigurationAndAccount(t *testing.T) {
	path := filepath.Join(t.TempDir(), "site.db")
	s, err := OpenStore(path, RoleSite)
	if err != nil {
		t.Fatal(err)
	}
	if _, err = s.Config(); !errors.Is(err, sql.ErrNoRows) {
		t.Fatalf("unseeded configuration: %v", err)
	}
	if err = s.WriteCache("system", storeDoc(t, storeFixtureSystem())); err != nil {
		t.Fatal(err)
	}
	if err = s.WriteCache("zone", storeDoc(t, storeFixtureZone())); err != nil {
		t.Fatal(err)
	}
	if err = s.SaveConfig(storeDoc(t, storeFixtureSite())); err != nil {
		t.Fatal(err)
	}
	hash, err := bcrypt.GenerateFromPassword([]byte("temporary-test-password"), bcrypt.MinCost)
	if err != nil {
		t.Fatal(err)
	}
	if err = s.SetAccount("administrator", hash); err != nil {
		t.Fatal(err)
	}
	before, at, err := s.ReadCache("zone")
	if err != nil || at.IsZero() {
		t.Fatalf("cache timestamp: %s %v", at, err)
	}
	if err = s.Close(); err != nil {
		t.Fatal(err)
	}
	s, err = OpenStore(path, RoleSite)
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	if _, err = s.Config(); err != nil {
		t.Fatal(err)
	}
	after, afterAt, err := s.ReadCache("zone")
	if err != nil || !bytes.Equal(before, after) || !at.Equal(afterAt) {
		t.Fatalf("cached settings changed on restart: %v", err)
	}
	name, gotHash, err := s.Account()
	if err != nil || name != "administrator" || !bytes.Equal(hash, gotHash) {
		t.Fatalf("account not retained: %v", err)
	}
	info, err := os.Stat(path)
	if err != nil || info.Mode().Perm() != 0600 {
		t.Fatalf("database privacy: %v %v", info, err)
	}
	events, err := s.Events(20)
	if err != nil || len(events) != 1 || events[0].Kind != "configuration" {
		t.Fatalf("saved event: %#v %v", events, err)
	}
}

func TestFrameworkStorePreservesRoleAndAllowsEnteredIdentity(t *testing.T) {
	path := filepath.Join(t.TempDir(), "system.db")
	s, err := OpenStore(path, RoleSystem)
	if err != nil {
		t.Fatal(err)
	}
	if err = s.SaveConfig(storeDoc(t, storeFixtureSystem())); err != nil {
		t.Fatal(err)
	}
	other := storeFixtureSystem()
	other.ID = "different-system"
	if err = s.SaveConfig(storeDoc(t, other)); err != nil {
		t.Fatal(err)
	}
	s.Close()
	if wrong, err := OpenStore(path, RoleZone); err == nil {
		wrong.Close()
		t.Fatal("opened system database as zone")
	}
	s, err = OpenStore(path, RoleSystem)
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	if _, err = s.Config(); err != nil {
		t.Fatal(err)
	}
}

func TestFrameworkStoreRefusesUnrelatedDatabase(t *testing.T) {
	path := filepath.Join(t.TempDir(), "live.db")
	db, err := sql.Open("sqlite", path)
	if err != nil {
		t.Fatal(err)
	}
	if _, err = db.Exec("CREATE TABLE existing_live_configuration(value TEXT); INSERT INTO existing_live_configuration(value) VALUES('keep');"); err != nil {
		t.Fatal(err)
	}
	if _, err = OpenStore(path, RoleSystem); err == nil {
		t.Fatal("opened unrelated live database")
	}
	var count int
	if err = db.QueryRow("SELECT count(*) FROM sqlite_master WHERE name LIKE 'framework_%'").Scan(&count); err != nil || count != 0 {
		t.Fatalf("unrelated database mutated: %d %v", count, err)
	}
	var value string
	if err = db.QueryRow("SELECT value FROM existing_live_configuration").Scan(&value); err != nil || value != "keep" {
		t.Fatal("existing data changed")
	}
	db.Close()
}

func TestFrameworkCatalogAcceptanceIsAtomic(t *testing.T) {
	s := storeOpen(t, RoleSite)
	hash, err := bcrypt.GenerateFromPassword([]byte("test-only-account"), bcrypt.MinCost)
	if err != nil {
		t.Fatal(err)
	}
	if err = s.AcceptCatalog(storeDoc(t, storeFixtureSystem()), storeDoc(t, storeFixtureZone()), "administrator", hash, []userRecord{}, []automationRecord{}, &radioInhibitionPolicy{Desired: []int{}}); err != nil {
		t.Fatal(err)
	}
	systemBefore, _, _ := s.ReadCache("system")
	zoneBefore, _, _ := s.ReadCache("zone")
	changedSystem := storeFixtureSystem()
	changedSystem.Name = "Changed system"
	changedZone := storeFixtureZone()
	changedZone.Name = "Changed zone"
	if err = s.AcceptCatalog(storeDoc(t, changedSystem), storeDoc(t, changedZone), "administrator", []byte("not-a-verifier"), []userRecord{}, []automationRecord{}, &radioInhibitionPolicy{Desired: []int{}}); err == nil {
		t.Fatal("accepted invalid account verifier")
	}
	systemAfter, _, _ := s.ReadCache("system")
	zoneAfter, _, _ := s.ReadCache("zone")
	if !bytes.Equal(systemBefore, systemAfter) || !bytes.Equal(zoneBefore, zoneAfter) {
		t.Fatal("rejected bundle changed cached settings")
	}
	_, accountHash, _ := s.Account()
	if !bytes.Equal(accountHash, hash) {
		t.Fatal("rejected bundle changed administrator")
	}
	changedZone.SystemID = "another-system"
	if err = s.AcceptCatalog(storeDoc(t, changedSystem), storeDoc(t, changedZone), "administrator", hash, []userRecord{}, []automationRecord{}, &radioInhibitionPolicy{Desired: []int{}}); err == nil {
		t.Fatal("accepted foreign Zone bundle")
	}
	systemAfter, _, _ = s.ReadCache("system")
	if !bytes.Equal(systemBefore, systemAfter) {
		t.Fatal("foreign Zone rejection partially saved system")
	}
	if err = s.AcceptCatalog(storeDoc(t, changedSystem), nil, "administrator", hash, []userRecord{}, []automationRecord{}, &radioInhibitionPolicy{Desired: []int{}}); err != nil {
		t.Fatal(err)
	}
	zoneAfter, _, _ = s.ReadCache("zone")
	if !bytes.Equal(zoneBefore, zoneAfter) {
		t.Fatal("nil Zone removed retained Zone")
	}
}

func ptrSystem(v SystemConfig) *SystemConfig { return &v }
func ptrZone(v ZoneConfig) *ZoneConfig       { return &v }

func TestFrameworkCacheUnchangedWritesSkipUpdates(t *testing.T) {
	s := storeOpen(t, RoleSite)
	if err := s.WriteCache("unchanged-fixture", json.RawMessage(`{"value":1}`)); err != nil {
		t.Fatal(err)
	}
	before, beforeAt, _ := s.ReadCache("unchanged-fixture")
	if _, err := s.db.Exec(`CREATE TRIGGER reject_cache_update BEFORE UPDATE ON framework_documents
		WHEN NEW.name='cache:unchanged-fixture' BEGIN SELECT RAISE(FAIL, 'unexpected cache update'); END`); err != nil {
		t.Fatal(err)
	}
	if err := s.WriteCache("unchanged-fixture", json.RawMessage(`{ "value": 1 }`)); err != nil {
		t.Fatal("unchanged cache attempted an UPDATE:", err)
	}
	after, afterAt, err := s.ReadCache("unchanged-fixture")
	if err != nil || !bytes.Equal(before, after) || !beforeAt.Equal(afterAt) {
		t.Fatal("unchanged cache changed its document or saved timestamp", err)
	}
	if err := s.WriteCache("unchanged-fixture", json.RawMessage(`{"value":2}`)); err == nil {
		t.Fatal("failed changed-cache write was accepted")
	}
	if _, err := s.db.Exec("DROP TRIGGER reject_cache_update"); err != nil {
		t.Fatal(err)
	}
	if err := s.WriteCache("unchanged-fixture", json.RawMessage(`{"value":2}`)); err != nil {
		t.Fatal(err)
	}
	after, afterAt, err = s.ReadCache("unchanged-fixture")
	if err != nil || string(after) != `{"value":2}` || beforeAt.Equal(afterAt) {
		t.Fatal("changed cache did not retain its new value and timestamp", err)
	}
}

func TestFrameworkCatalogUnchangedWritesSkipUpdatesAndChangesRemainAtomic(t *testing.T) {
	s := storeOpen(t, RoleSite)
	system, zone := automationFixtureSystem(), storeFixtureZone()
	hash, err := bcrypt.GenerateFromPassword([]byte("unchanged-catalog-fixture"), bcrypt.MinCost)
	if err != nil {
		t.Fatal(err)
	}
	originalHash := hash
	username := "administrator"
	users, automations := []userRecord{}, []automationRecord{}
	inhibition := &radioInhibitionPolicy{Desired: []int{}}
	accept := func() error {
		return s.AcceptCatalog(storeDoc(t, system), storeDoc(t, zone), username, hash, users, automations, inhibition)
	}
	snapshot := func() map[string]string {
		values := map[string]string{}
		for _, name := range []string{"system", "zone", "radio-inhibition", "users", "automations"} {
			raw, at, err := s.ReadCache(name)
			if err != nil {
				t.Fatal(err)
			}
			values[name] = string(raw) + "\n" + at.String()
		}
		name, verifier, err := s.Account()
		if err != nil {
			t.Fatal(err)
		}
		values["administrator"] = name + "\n" + string(verifier)
		return values
	}
	if err = accept(); err != nil {
		t.Fatal(err)
	}
	before := snapshot()
	if _, err = s.db.Exec(`CREATE TRIGGER reject_catalog_document_update BEFORE UPDATE ON framework_documents
		BEGIN SELECT RAISE(FAIL, 'unexpected catalog document update'); END;
		CREATE TRIGGER reject_catalog_administrator_update BEFORE UPDATE ON framework_administrator
		BEGIN SELECT RAISE(FAIL, 'unexpected administrator update'); END;`); err != nil {
		t.Fatal(err)
	}
	if err = accept(); err != nil {
		t.Fatal("unchanged catalog attempted an UPDATE:", err)
	}
	for name, value := range snapshot() {
		if value != before[name] {
			t.Fatal("unchanged catalog changed", name)
		}
	}
	if _, err = s.db.Exec("DROP TRIGGER reject_catalog_document_update; DROP TRIGGER reject_catalog_administrator_update;"); err != nil {
		t.Fatal(err)
	}
	username = "renamed-administrator"
	if err = accept(); err != nil {
		t.Fatal(err)
	}
	if name, verifier, accountErr := s.Account(); accountErr != nil || name != username || !bytes.Equal(verifier, originalHash) {
		t.Fatal("username-only catalog change did not retain the password verifier", accountErr)
	}
	hash, err = bcrypt.GenerateFromPassword([]byte("hash-only-catalog-fixture"), bcrypt.MinCost)
	if err != nil {
		t.Fatal(err)
	}
	if err = accept(); err != nil {
		t.Fatal(err)
	}
	if name, verifier, accountErr := s.Account(); accountErr != nil || name != username || !bytes.Equal(verifier, hash) {
		t.Fatal("password-only catalog change did not retain the username", accountErr)
	}
	system.Name, zone.Name, username = "Changed system", "Changed zone", "changed-administrator"
	hash, err = bcrypt.GenerateFromPassword([]byte("changed-catalog-fixture"), bcrypt.MinCost)
	if err != nil {
		t.Fatal(err)
	}
	users = []userRecord{{User: fixtureUser(), PasswordHash: hash}}
	automations = []automationRecord{{Automation: automationFixture(), TokenHash: bytes.Repeat([]byte{1}, 32)}}
	inhibition.Desired = []int{10001}
	if err = accept(); err != nil {
		t.Fatal(err)
	}
	for name, value := range snapshot() {
		if value == before[name] {
			t.Fatal("changed catalog did not update", name)
		}
	}
	var gotSystem SystemConfig
	var gotZone ZoneConfig
	var gotInhibition radioInhibitionPolicy
	for name, target := range map[string]any{"system": &gotSystem, "zone": &gotZone, "radio-inhibition": &gotInhibition} {
		raw, _, err := s.ReadCache(name)
		if err != nil || json.Unmarshal(raw, target) != nil {
			t.Fatal("changed document unavailable:", name, err)
		}
	}
	name, verifier, err := s.Account()
	gotUsers, userErr := s.userRecords()
	gotAutomations, automationErr := s.automationRecords()
	if err != nil || userErr != nil || automationErr != nil || name != username || !bytes.Equal(verifier, hash) ||
		gotSystem.Name != system.Name || gotZone.Name != zone.Name || len(gotInhibition.Desired) != 1 || gotInhibition.Desired[0] != 10001 ||
		len(gotUsers) != 1 || gotUsers[0].User.ID != users[0].User.ID || !bytes.Equal(gotUsers[0].PasswordHash, hash) ||
		len(gotAutomations) != 1 || !bytes.Equal(gotAutomations[0].TokenHash, automations[0].TokenHash) {
		t.Fatal("changed catalog values or credentials were not retained")
	}
	before = snapshot()
	if _, err = s.db.Exec(`CREATE TRIGGER reject_changed_automation BEFORE UPDATE ON framework_documents
		WHEN NEW.name='cache:automations' BEGIN SELECT RAISE(FAIL, 'late changed write failure'); END`); err != nil {
		t.Fatal(err)
	}
	system.Name, zone.Name, username, hash = "Rejected system", "Rejected zone", "rejected-administrator", originalHash
	users[0].PasswordHash = originalHash
	automations[0].TokenHash = bytes.Repeat([]byte{2}, 32)
	inhibition.Desired = []int{}
	if err = accept(); err == nil {
		t.Fatal("failed catalog write was accepted")
	}
	for name, value := range snapshot() {
		if value != before[name] {
			t.Fatal("failed catalog write partially changed", name)
		}
	}
}

func TestFrameworkStorePreservesMisconfigurationAndRejectsMalformedJSONAtomically(t *testing.T) {
	s := storeOpen(t, RoleSite)
	config := storeFixtureSite()
	config.Channels[0].Identifier = 99
	config.Channels[0].ModemID = "missing"
	config.Identification.WPM = 25
	raw := storeDoc(t, config)
	if err := s.SaveConfig(raw); err != nil {
		t.Fatal(err)
	}
	before, _ := s.Config()
	if !bytes.Equal(before, raw) {
		t.Fatal("entered configuration changed")
	}
	for _, bad := range []json.RawMessage{json.RawMessage(`{"channels":`), json.RawMessage(`[]`), json.RawMessage(`{} {}`)} {
		if err := s.SaveConfig(bad); err == nil {
			t.Fatal("malformed document saved")
		}
	}
	after, _ := s.Config()
	if !bytes.Equal(before, after) {
		t.Fatal("failed parse changed configuration")
	}
	if err := s.SaveConfig(raw); err != nil {
		t.Fatal(err)
	}
	events, _ := s.Events(100)
	if len(events) != 2 {
		t.Fatal("successful saves were not recorded, or malformed input was recorded")
	}
}

func TestFrameworkUpstreamPlanDoesNotRewriteOrBlockLocalConfiguration(t *testing.T) {
	s := storeOpen(t, RoleSite)
	zone := storeFixtureZone()
	if err := s.WriteCache("zone", storeDoc(t, zone)); err != nil {
		t.Fatal(err)
	}
	if err := s.SaveConfig(storeDoc(t, storeFixtureSite())); err != nil {
		t.Fatal(err)
	}
	before, _ := s.Config()
	zone.ChannelIdentifiers[0].BaseHz += 12500
	if err := s.WriteCache("zone", storeDoc(t, zone)); err != nil {
		t.Fatal(err)
	}
	after, _ := s.Config()
	if !bytes.Equal(before, after) {
		t.Fatal("upstream plan retuned local channel")
	}
	if err := s.SaveConfig(after); err != nil {
		t.Fatal(err)
	}
}

func TestFrameworkConfigurationPreservesEnteredValuesForEveryOwner(t *testing.T) {
	documents := map[Role]json.RawMessage{
		RoleSystem:      json.RawMessage(`{"id":"","wacn":0,"callsign":" n0test ","voice_mfid":256,"bsi_profile":"future","radios":[{"rid":1},{"rid":1}],"talkgroups":[{"tgid":100,"secure_mode":"clear","algorithm_id":132,"key_id":7,"site_ids":["missing"]}],"future_setting":true}`),
		RoleZone:        json.RawMessage(`{"system_id":"missing","rfssid":999,"sites":[{"id":"duplicate","nac":4095},{"id":"duplicate","nac":4094}],"channel_identifiers":[{"identifier":16}]}`),
		RoleSite:        json.RawMessage(`{"id":"","channels":[{"modem_id":"missing","rx_hz":1}],"identification":{"wpm":25}}`),
		RoleApplication: json.RawMessage(`{"name":"","connection_url":"https://unreachable.invalid","future_setting":null}`),
	}
	for role, raw := range documents {
		t.Run(string(role), func(t *testing.T) {
			s := storeOpen(t, role)
			if err := s.SaveConfig(raw); err != nil {
				t.Fatal(err)
			}
			saved, err := s.Config()
			if err != nil || !bytes.Equal(saved, raw) {
				t.Fatalf("entered settings changed: %s %v", saved, err)
			}
			if err = s.WriteCache("system", raw); err != nil {
				t.Fatal(err)
			}
			cached, _, err := s.ReadCache("system")
			if err != nil || !bytes.Equal(cached, raw) {
				t.Fatal("cache changed configuration", err)
			}
		})
	}
}

func TestFrameworkCachePreservesIncompleteSettings(t *testing.T) {
	s := storeOpen(t, RoleSite)
	raw := json.RawMessage(`{"sites":[],"unknown_setting":"retain"}`)
	if err := s.WriteCache("zone", raw); err != nil {
		t.Fatal(err)
	}
	after, _, err := s.ReadCache("zone")
	if err != nil || !bytes.Equal(raw, after) {
		t.Fatal("cache altered entered settings", err)
	}
	if err = s.SaveConfig(storeDoc(t, storeFixtureSite())); err != nil {
		t.Fatal(err)
	}
}
