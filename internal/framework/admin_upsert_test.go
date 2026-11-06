package framework

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"net/url"
	"reflect"
	"strings"
	"testing"

	"golang.org/x/crypto/bcrypt"
)

func adminMutation(t *testing.T, service *Service, method, path string, value any) *httptest.ResponseRecorder {
	t.Helper()
	raw, err := json.Marshal(value)
	if err != nil {
		t.Fatal(err)
	}
	r := httptest.NewRequest(method, path, bytes.NewReader(raw))
	r.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()
	switch {
	case strings.HasPrefix(path, "/api/users"):
		service.usersHTTP(w, r)
	case strings.HasPrefix(path, "/api/positions"):
		service.positionsHTTP(w, r)
	default:
		service.automationsHTTP(w, r, Session{})
	}
	if w.Code != http.StatusOK {
		t.Fatalf("%s %s: HTTP %d", method, path, w.Code)
	}
	return w
}

func TestAdministratorUserUpsertSavesSubmittedProfileAndPreservesOnlyExistingPassword(t *testing.T) {
	s := storeOpen(t, RoleSystem)
	service := &Service{Store: s, config: FileConfig{Role: RoleSystem}}
	u := fixtureUser()
	u.Profile.Desktop = ptrDesktop(desktopFixture())
	adminMutation(t, service, http.MethodPost, "/api/users", userWrite{User: u, Password: "admin-upsert-password"})
	_, hash, _ := s.User(u.ID)
	u.Profile.Name = "Submitted profile"
	u.Profile.Desktop = ptrDesktop(emptyDesktopProfile())
	adminMutation(t, service, http.MethodPost, "/api/users", userWrite{User: u})
	saved, after, err := s.User(u.ID)
	if err != nil || !reflect.DeepEqual(saved, u) || !bytes.Equal(hash, after) {
		t.Fatal("duplicate add did not save the submitted profile with its existing password")
	}
	oldID := u.ID
	u.ID = "renamed user / 1"
	adminMutation(t, service, http.MethodPut, "/api/users/"+url.PathEscape(oldID), userWrite{User: u})
	_, after, err = s.User(u.ID)
	if err != nil || !bytes.Equal(hash, after) {
		t.Fatal("rename lost the existing password")
	}
	for i := 0; i < 2; i++ {
		adminMutation(t, service, http.MethodDelete, "/api/users/"+url.PathEscape(u.ID), nil)
	}
	adminMutation(t, service, http.MethodPut, "/api/users/"+url.PathEscape(u.ID), userWrite{User: u})
	saved, after, err = s.User(u.ID)
	if err != nil || !reflect.DeepEqual(saved, u) || len(after) != 0 || bcrypt.CompareHashAndPassword(after, []byte("admin-upsert-password")) == nil {
		t.Fatal("deleted edit was refused, altered, or fabricated a lost password")
	}
}

func TestAdministratorAutomationUpsertDoesNotInventDeletedCredential(t *testing.T) {
	s := storeOpen(t, RoleSystem)
	service := &Service{Store: s, config: FileConfig{Role: RoleSystem}}
	a := automationFixture()
	a.ID = "Source with spaces / 1"
	created := adminMutation(t, service, http.MethodPost, "/api/automations", a)
	var credential struct{ Token string }
	if err := json.Unmarshal(created.Body.Bytes(), &credential); err != nil || len(credential.Token) != 64 {
		t.Fatal("explicit creation did not produce a credential")
	}
	records, _ := s.automationRecords()
	hash := bytes.Clone(records[0].TokenHash)
	a.Name = "Submitted configuration"
	updated := adminMutation(t, service, http.MethodPost, "/api/automations", a)
	records, err := s.automationRecords()
	if err != nil || len(records) != 1 || !reflect.DeepEqual(records[0].Automation, a) || !bytes.Equal(records[0].TokenHash, hash) || bytes.Contains(updated.Body.Bytes(), []byte(`"token"`)) {
		t.Fatal("duplicate add changed the saved credential or refused the settings")
	}
	path := "/api/automations/" + url.PathEscape(a.ID)
	for i := 0; i < 2; i++ {
		adminMutation(t, service, http.MethodDelete, path, nil)
	}
	adminMutation(t, service, http.MethodPut, path, a)
	records, err = s.automationRecords()
	if err != nil || len(records) != 1 || !reflect.DeepEqual(records[0].Automation, a) || len(records[0].TokenHash) != 0 {
		t.Fatal("deleted edit was refused, altered, or fabricated a credential")
	}
	adminMutation(t, service, http.MethodPost, path+"/token", nil)
	records, err = s.automationRecords()
	if err != nil || len(records[0].TokenHash) != 32 {
		t.Fatal("explicit credential replacement did not work with the saved identity")
	}
}

func TestAdministratorConsolePositionUpsertAndMissingDelete(t *testing.T) {
	s := storeOpen(t, RoleSystem)
	if err := s.SaveConfig(storeDoc(t, userFixtureSystem())); err != nil {
		t.Fatal(err)
	}
	service := &Service{Store: s, config: FileConfig{Role: RoleSystem}}
	position := ConsolePosition{ID: "desk-1", Name: "Submitted position", RadioID: 0, Talkgroups: []int{999, 999}}
	adminMutation(t, service, http.MethodPost, "/api/positions", position)
	for i := 0; i < 2; i++ {
		adminMutation(t, service, http.MethodDelete, "/api/positions/desk-1", nil)
	}
	adminMutation(t, service, http.MethodPut, "/api/positions/desk-1", position)
	var config SystemConfig
	raw, _ := s.Config()
	if err := json.Unmarshal(raw, &config); err != nil {
		t.Fatal(err)
	}
	if len(config.ConsolePositions) != 2 || !reflect.DeepEqual(config.ConsolePositions[1], position) {
		t.Fatal("deleted position edit did not save the submitted settings")
	}
}

func TestAdministratorEmptyRecordIdentityRemainsEditable(t *testing.T) {
	s := storeOpen(t, RoleSystem)
	if err := s.SaveConfig(storeDoc(t, userFixtureSystem())); err != nil {
		t.Fatal(err)
	}
	service := &Service{Store: s, config: FileConfig{Role: RoleSystem}}
	for _, resource := range []string{"users", "automations", "positions"} {
		path := "/api/" + resource
		field := "name"
		if resource == "users" {
			field = "username"
		}
		adminMutation(t, service, http.MethodPost, path, map[string]any{"id": "", field: "Entered value"})
		adminMutation(t, service, http.MethodPut, path+"/", map[string]any{"id": "renamed", field: "Updated value"})
		for i := 0; i < 2; i++ {
			adminMutation(t, service, http.MethodDelete, path+"/renamed", nil)
		}
	}
}
