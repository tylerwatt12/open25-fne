package framework

import (
	"bytes"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"slices"
	"strings"
	"testing"
)

func TestCatalogProjectionHasStableIdentityOrderWithoutRewritingOwner(t *testing.T) {
	s, _, zone := observationReportService(t, RoleZone)
	system := automationFixtureSystem()
	if err := s.Store.WriteCache("system", storeDoc(t, system)); err != nil {
		t.Fatal(err)
	}
	hash := []byte("$2a$10$N9qo8uLOickgx2ZMRZoMyeIjZAgcfl7p92ldGxad68LJZdL17lhWy")
	users, automations := []userRecord{}, []automationRecord{}
	for _, id := range []string{"z-last", "a-first", "m-middle"} {
		u := fixtureUser()
		u.ID, u.Username = id, id
		users = append(users, userRecord{User: u, PasswordHash: hash})
		a := automationFixture()
		a.ID = id
		automations = append(automations, automationRecord{Automation: a, TokenHash: bytes.Repeat([]byte{1}, 32)})
	}
	for name, value := range map[string]any{"users": users, "automations": automations} {
		if err := s.writeCache(name, value); err != nil {
			t.Fatal(err)
		}
	}
	if err := s.Store.SetAccount("administrator", hash); err != nil {
		t.Fatal(err)
	}
	s.config.CredentialFile = filepath.Join(t.TempDir(), "credentials.json")
	token := strings.Repeat("s", 64)
	if err := WritePrivateJSON(s.config.CredentialFile, ServiceCredentials{Peers: map[string]PeerCredential{"site-1": {Role: RoleSite, InstanceID: "site-1", Token: token}}}); err != nil {
		t.Fatal(err)
	}
	s.refreshDirectory()
	beforeUsers, usersAt, _ := s.Store.ReadCache("users")
	beforeAutomations, automationsAt, _ := s.Store.ReadCache("automations")
	var previous []byte
	for range 24 {
		r := httptest.NewRequest(http.MethodGet, "http://zone.example/internal/catalog", nil)
		r.Header.Set("X-Service-ID", "site-1")
		r.Header.Set("Authorization", "Bearer "+token)
		response := httptest.NewRecorder()
		s.InternalHandler().ServeHTTP(response, r)
		if response.Code != http.StatusOK {
			t.Fatalf("catalogue response: %d %s", response.Code, response.Body.String())
		}
		var got catalog
		if err := json.Unmarshal(response.Body.Bytes(), &got); err != nil {
			t.Fatal(err)
		}
		userIDs, automationIDs := []string{}, []string{}
		for _, value := range got.Users {
			userIDs = append(userIDs, value.User.ID)
		}
		for _, value := range got.Automations {
			automationIDs = append(automationIDs, value.Automation.ID)
		}
		want := []string{"a-first", "m-middle", "z-last"}
		if !slices.Equal(userIDs, want) || !slices.Equal(automationIDs, want) {
			t.Fatalf("map iteration escaped into catalogue order: users=%v automations=%v", userIDs, automationIDs)
		}
		projection := storeDoc(t, struct {
			Users       []userRecord
			Automations []automationRecord
		}{got.Users, got.Automations})
		if previous != nil && !bytes.Equal(projection, previous) {
			t.Fatal("unchanged identities produced a different wire projection")
		}
		previous = projection
	}
	afterUsers, afterUsersAt, _ := s.Store.ReadCache("users")
	afterAutomations, afterAutomationsAt, _ := s.Store.ReadCache("automations")
	if !bytes.Equal(beforeUsers, afterUsers) || !usersAt.Equal(afterUsersAt) || !bytes.Equal(beforeAutomations, afterAutomations) || !automationsAt.Equal(afterAutomationsAt) {
		t.Fatal("reading a stable catalogue reordered or rewrote its owner's records")
	}
	if s.view().Zone.ID != zone.ID {
		t.Fatal("catalogue projection changed the owning Zone")
	}
}

func TestCatalogUnchangedNilZoneBypassesWriterAndRetainsHomeGuard(t *testing.T) {
	hash := []byte("$2a$10$N9qo8uLOickgx2ZMRZoMyeIjZAgcfl7p92ldGxad68LJZdL17lhWy")
	for _, retained := range []bool{false, true} {
		t.Run(fmt.Sprintf("retained=%t", retained), func(t *testing.T) {
			s := storeOpen(t, RoleZone)
			system := storeDoc(t, storeFixtureSystem())
			var zone json.RawMessage
			if retained {
				zone = storeDoc(t, storeFixtureZone())
			}
			if err := s.AcceptCatalog(system, zone, "administrator", hash, nil, nil, nil); err != nil {
				t.Fatal(err)
			}
			withOccupiedConfigurationWriter(t, s, func() error {
				return s.AcceptCatalog(system, nil, "administrator", hash, nil, nil, nil)
			})
			if retained {
				foreign := storeFixtureZone()
				foreign.SystemID = "another-system"
				if err := s.WriteCache("zone", storeDoc(t, foreign)); err != nil {
					t.Fatal(err)
				}
				withOccupiedConfigurationWriter(t, s, func() error {
					if err := s.AcceptCatalog(system, nil, "administrator", hash, nil, nil, nil); err == nil {
						return fmt.Errorf("unchanged catalogue bypassed retained Zone home-System guard")
					}
					return nil
				})
			}
		})
	}
}

func TestSystemCatalogProjectionPreservesEqualIdentityOrderAndLastOwner(t *testing.T) {
	pair := keyManagementSystem(t)
	s := pair.service
	if err := s.Store.SaveConfig(storeDoc(t, automationFixtureSystem())); err != nil {
		t.Fatal(err)
	}
	hash := []byte("$2a$10$N9qo8uLOickgx2ZMRZoMyeIjZAgcfl7p92ldGxad68LJZdL17lhWy")
	users, automations := []userRecord{}, []automationRecord{}
	wantUsers, wantAutomations := map[string][]string{}, map[string][]string{}
	for i := range 64 {
		id := fmt.Sprintf("identity-%d", (i*7+3)%9)
		u := fixtureUser()
		u.ID, u.Username = id, fmt.Sprintf("user-%02d", i)
		users = append(users, userRecord{User: u, PasswordHash: hash})
		wantUsers[id] = append(wantUsers[id], u.Username)
		a := automationFixture()
		a.ID, a.Name = id, fmt.Sprintf("automation-%02d", i)
		automations = append(automations, automationRecord{Automation: a, TokenHash: bytes.Repeat([]byte{byte(i)}, 32)})
		wantAutomations[id] = append(wantAutomations[id], a.Name)
	}
	// Existing persisted arrays can contain duplicate programmed identities.
	// Build that exact input without adding a new Save restriction or repair.
	for name, value := range map[string]any{"users": users, "automations": automations} {
		if _, err := s.Store.db.Exec("INSERT INTO framework_documents(name,document,at) VALUES(?,?,?)", name, storeDoc(t, value), nowStamp()); err != nil {
			t.Fatal(err)
		}
	}
	creds, err := readCredentials(s.Config().CredentialFile)
	if err != nil {
		t.Fatal(err)
	}
	token := strings.Repeat("z", 64)
	creds.Peers["zone-1"] = PeerCredential{Role: RoleZone, InstanceID: "zone-1", Token: token}
	if err = WritePrivateJSON(s.Config().CredentialFile, creds); err != nil {
		t.Fatal(err)
	}
	s.refreshDirectory()
	r := httptest.NewRequest(http.MethodGet, "http://system.example/internal/catalog", nil)
	r.Header.Set("X-Service-ID", "zone-1")
	r.Header.Set("Authorization", "Bearer "+token)
	response := httptest.NewRecorder()
	s.InternalHandler().ServeHTTP(response, r)
	if response.Code != http.StatusOK {
		t.Fatalf("catalogue response: %d %s", response.Code, response.Body.String())
	}
	var got catalog
	if err = json.Unmarshal(response.Body.Bytes(), &got); err != nil {
		t.Fatal(err)
	}
	gotUsers, gotAutomations := map[string][]string{}, map[string][]string{}
	previous := ""
	for _, record := range got.Users {
		if record.User.ID < previous {
			t.Fatal("user projection is not ordered by identity")
		}
		previous = record.User.ID
		gotUsers[record.User.ID] = append(gotUsers[record.User.ID], record.User.Username)
	}
	previous = ""
	for _, record := range got.Automations {
		if record.Automation.ID < previous {
			t.Fatal("automation projection is not ordered by identity")
		}
		previous = record.Automation.ID
		gotAutomations[record.Automation.ID] = append(gotAutomations[record.Automation.ID], record.Automation.Name)
	}
	for id, order := range wantUsers {
		if !slices.Equal(order, gotUsers[id]) || !slices.Equal(wantAutomations[id], gotAutomations[id]) {
			t.Fatalf("equal identity %s changed relative order: users=%v want=%v automations=%v want=%v", id, gotUsers[id], order, gotAutomations[id], wantAutomations[id])
		}
	}
	zone := &Service{Store: storeOpen(t, RoleZone), config: FileConfig{Role: RoleZone, InstanceID: "zone-1"}}
	if err = zone.Store.SaveConfig(storeDoc(t, storeFixtureZone())); err != nil {
		t.Fatal(err)
	}
	if err = zone.acceptCatalog(got); err != nil {
		t.Fatal(err)
	}
	zone.refreshDirectory()
	for id, order := range wantUsers {
		if zone.view().Users[id].User.Username != order[len(order)-1] || zone.view().Automations[id].Automation.Name != wantAutomations[id][len(wantAutomations[id])-1] {
			t.Fatalf("downstream identity %s selected a different duplicate owner", id)
		}
	}
}
