package framework

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestDeskResolutionPreservesHumanAndIntersectsPermissions(t *testing.T) {
	system := userFixtureSystem()
	alice := fixtureUser()
	alice.PositionIDs = []string{"desk-1", "desk-2"}
	bob := cloneUser(t, alice)
	bob.ID, bob.Username = "person-bob", "bob"
	for _, person := range []User{alice, bob} {
		effective, desk, err := resolveUserPosition(person, system, "desk-2")
		if err != nil {
			t.Fatal(err)
		}
		if effective.ID != person.ID || effective.Username != person.Username || effective.RadioID != 10004 || desk == nil || desk.ID != "desk-2" {
			t.Fatalf("wrong person/desk: %#v %#v", effective, desk)
		}
		if len(effective.Talkgroups) != 1 || effective.Talkgroups[0] != 1001 {
			t.Fatal("desk ceiling did not restrict personal groups")
		}
		if person.RadioID != 0 || len(person.Talkgroups) != 2 {
			t.Fatal("resolving a session modified the stored person")
		}
		if _, _, err = resolveUserRadio(person, system, 10002); err != nil {
			t.Fatal("assigned desk was denied", err)
		}
		if userRadioAllowed(person, system, 10001) {
			t.Fatal("physical subscriber identity was accepted")
		}
	}
	if userProof(alice, []byte("alice-verifier")) == userProof(bob, []byte("bob-verifier")) {
		t.Fatal("desk sharing erased actor authentication")
	}
}

func TestDeskSelectionNeverGuessesOrUsesPersonalLegacyRadioID(t *testing.T) {
	system, user := userFixtureSystem(), fixtureUser()
	user.PositionIDs, user.Profile.PreferredPositionID = []string{"desk-1", "desk-2"}, ""
	if _, _, err := resolveUserPosition(user, system, ""); err == nil {
		t.Fatal("multiple desks were selected implicitly")
	}
	user.Profile.PreferredPositionID = "desk-2"
	if effective, _, err := resolveUserPosition(user, system, ""); err != nil || effective.RadioID != 10004 {
		t.Fatal("preferred desk was not selected")
	}
	user.PositionIDs, user.Profile.PreferredPositionID, user.RadioID = []string{}, "", 10002
	if _, _, err := resolveUserPosition(user, system, ""); err == nil {
		t.Fatal("a personal dispatcher RID acted as compatibility fallback")
	}
	user = fixtureUser()
	if _, _, err := resolveUserPosition(user, system, "desk-2"); err == nil {
		t.Fatal("unassigned desk selected")
	}
	system.ConsolePositions[0].Enabled = false
	if _, _, err := resolveUserPosition(user, system, "desk-1"); err == nil {
		t.Fatal("disabled desk selected")
	}
}

func TestConsolePositionCataloguePreservesConfigurationWithOneAuthority(t *testing.T) {
	store := storeOpen(t, RoleSystem)
	system := userFixtureSystem()
	if err := store.SaveConfig(storeDoc(t, system)); err != nil {
		t.Fatal(err)
	}
	if err := store.SaveUser(fixtureUser().ID, fixtureUser(), "test-user-password"); err != nil {
		t.Fatal(err)
	}
	if err := store.updateConsolePosition(ConsolePosition{}, "desk-1", true); err != nil {
		t.Fatal("assigned position deletion was blocked", err)
	}
	position := ConsolePosition{ID: "desk-3", Name: "  Future desk  ", RadioID: 10001, Enabled: true, Talkgroups: []int{999, 999}}
	if err := store.updateConsolePosition(position, position.ID, false); err != nil {
		t.Fatal(err)
	}
	var saved SystemConfig
	raw, _ := store.Config()
	if err := json.Unmarshal(raw, &saved); err != nil {
		t.Fatal(err)
	}
	if len(saved.ConsolePositions) != 2 || saved.ConsolePositions[1].Name != position.Name || saved.ConsolePositions[1].RadioID != position.RadioID {
		t.Fatal("entered position changed")
	}
	other := storeOpen(t, RoleSite)
	if err := other.updateConsolePosition(position, position.ID, false); err == nil {
		t.Fatal("Site became console-position authority")
	}
}

func TestConsolePositionHTTPKeepsAuthorityAndAllowsIdentityChange(t *testing.T) {
	store := storeOpen(t, RoleSystem)
	if err := store.SaveConfig(storeDoc(t, userFixtureSystem())); err != nil {
		t.Fatal(err)
	}
	service := &Service{Store: store, config: FileConfig{Role: RoleSystem}}
	request := httptest.NewRequest(http.MethodPut, "/api/positions/desk-1", strings.NewReader(`{"id":"other-desk","name":"wrong","radio_id":10002,"enabled":true,"talkgroups":[1001]}`))
	request.Header.Set("Content-Type", "application/json")
	response := httptest.NewRecorder()
	service.positionsHTTP(response, request)
	if response.Code != 200 {
		t.Fatal("desk identity change was rejected")
	}
	service.config.Role = RoleSite
	response = httptest.NewRecorder()
	service.positionsHTTP(response, httptest.NewRequest(http.MethodGet, "/api/positions", nil))
	if response.Code != http.StatusForbidden {
		t.Fatal("position editing leaked outside System")
	}
}
