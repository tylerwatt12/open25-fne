package framework

import (
	"bytes"
	"encoding/json"
	"reflect"
	"strconv"
	"testing"

	"golang.org/x/crypto/bcrypt"
)

func userFixtureSystem() SystemConfig {
	s := storeFixtureSystem()
	s.Radios = append(s.Radios, Radio{RID: 10002, Alias: "Dispatch", Enabled: true, Kind: "console"}, Radio{RID: 10003, Alias: "Mobile", Enabled: true, Kind: "mobile"}, Radio{RID: 10004, Alias: "Second console", Enabled: true, Kind: "console"})
	s.Talkgroups = append(s.Talkgroups, Talkgroup{TGID: 1002, AdmissionMode: "clear", TransmitMode: "clear", Alias: "Test 2", Enabled: true, SiteAccess: "all", SiteIDs: []string{}}, Talkgroup{TGID: 1099, AdmissionMode: "clear", TransmitMode: "clear", Alias: "Denied", Enabled: false, SiteAccess: "all", SiteIDs: []string{}})
	s.ConsolePositions = []ConsolePosition{{ID: "desk-1", Name: "Dispatch 1", RadioID: 10002, Enabled: true, Talkgroups: []int{1001, 1002}}, {ID: "desk-2", Name: "Dispatch 2", RadioID: 10004, Enabled: true, Talkgroups: []int{1001}}}
	return s
}
func fixtureUser() User {
	return User{ID: "user-1", Username: "dispatcher-one", RadioID: 0, PositionIDs: []string{"desk-1"}, Role: "dispatcher", Enabled: true, Talkgroups: []int{1001, 1002}, Permissions: []string{"ptt", "page"}, Profile: UserProfile{PreferredPositionID: "desk-1", Zones: []ProfileZone{{Name: "Main", Talkgroups: []int{1001, 1002}}}, Receive: []ReceiveSetting{{TalkgroupID: 1001, Volume: 0.8, Pan: 0}}, SelectedTalkgroup: 1001, TransmitTalkgroups: []int{1001}}}
}
func cloneUser(t *testing.T, u User) User {
	t.Helper()
	raw, err := json.Marshal(u)
	if err != nil {
		t.Fatal(err)
	}
	var out User
	if err = json.Unmarshal(raw, &out); err != nil {
		t.Fatal(err)
	}
	return out
}

func userRecordsJSON(t *testing.T, s *Store) []byte {
	t.Helper()
	records, err := s.userRecords()
	if err != nil {
		t.Fatal(err)
	}
	raw, err := json.Marshal(records)
	if err != nil {
		t.Fatal(err)
	}
	return raw
}

func TestApplicationUserAuthenticationAndSafePublicConfiguration(t *testing.T) {
	s := storeOpen(t, RoleSystem)
	u := fixtureUser()
	password := "test-user-password"
	if err := s.SaveUser(u.ID, u, password); err != nil {
		t.Fatal(err)
	}
	for _, login := range []string{u.Username} {
		actual, hash, err := s.LoginUser(login)
		if err != nil || actual.ID != u.ID {
			t.Fatalf("login %q: %#v %v", login, actual, err)
		}
		if err = bcrypt.CompareHashAndPassword(hash, []byte(password)); err != nil {
			t.Fatal("password was not stored as a usable one-way verifier")
		}
	}
	if _, _, err := s.LoginUser("10002"); err == nil {
		t.Fatal("a shared desk ID identified a human account")
	}
	mobile := cloneUser(t, u)
	mobile.ID, mobile.Username, mobile.Role, mobile.RadioID, mobile.PositionIDs, mobile.Profile.PreferredPositionID = "mobile-1", "mobile-one", "mobile", 10003, []string{}, ""
	if err := s.SaveUser(mobile.ID, mobile, password); err != nil {
		t.Fatal(err)
	}
	if actual, _, err := s.LoginUser(strconv.Itoa(mobile.RadioID)); err != nil || actual.ID != mobile.ID {
		t.Fatal("personal mobile Radio ID did not identify its user")
	}
	public, err := s.Users()
	if err != nil {
		t.Fatal(err)
	}
	raw, err := json.Marshal(public)
	if err != nil {
		t.Fatal(err)
	}
	_, hash, _ := s.User(u.ID)
	for _, secret := range []string{password, string(hash), "password_hash", "access_token", "private_key"} {
		if bytes.Contains(raw, []byte(secret)) {
			t.Fatalf("public configuration leaked %q", secret)
		}
	}
	if err = DecodeStrict([]byte(`{"id":"user-2","password_hash":"secret"}`), new(userWrite)); err == nil {
		t.Fatal("user write accepted a password verifier as an imported configuration field")
	}
}

func TestApplicationUserConfigurationPreservesAssignments(t *testing.T) {
	s := storeOpen(t, RoleSystem)
	u := fixtureUser()
	u.Username = "owner"
	u.Role = "future"
	u.RadioID = 10001
	u.PositionIDs = []string{"missing", "missing"}
	u.Talkgroups = []int{1099, 999}
	u.Permissions = []string{"future", "inhibit"}
	u.Profile.PreferredPositionID = "unassigned"
	if err := s.SaveUser(u.ID, u, "test-user-password"); err != nil {
		t.Fatal(err)
	}
	saved, _, err := s.User(u.ID)
	if err != nil || !reflect.DeepEqual(saved, u) {
		t.Fatal("account settings changed", err)
	}
	other := storeOpen(t, RoleSite)
	if err = other.SaveUser(u.ID, u, "test-user-password"); err == nil {
		t.Fatal("Site became a writable user authority")
	}
}

func TestApplicationUserSavesPreserveEnteredIdentityAndPassword(t *testing.T) {
	s := storeOpen(t, RoleSystem)
	u := fixtureUser()
	if err := s.SaveUser(u.ID, u, "test-user-password"); err != nil {
		t.Fatal(err)
	}
	_, before, _ := s.User(u.ID)
	u.Profile.Name = "Evening console"
	u.Permissions = []string{"inhibit"}
	if err := s.SaveUser(u.ID, u, ""); err != nil {
		t.Fatal(err)
	}
	saved, after, err := s.User(u.ID)
	if err != nil || !bytes.Equal(before, after) || !reflect.DeepEqual(saved, u) {
		t.Fatal("saving settings altered password or configuration", err)
	}
	u.ID = "second-user"
	if err = s.SaveUser(u.ID, u, "second-user-password"); err != nil {
		t.Fatal("duplicate configured identity was rejected", err)
	}
	users, err := s.Users()
	if err != nil || len(users) != 2 {
		t.Fatal("configuration lost a user", err)
	}
}

func TestApplicationProfilePreservesEnteredPreferences(t *testing.T) {
	s := storeOpen(t, RoleSystem)
	u := fixtureUser()
	if err := s.SaveUser(u.ID, u, "test-user-password"); err != nil {
		t.Fatal(err)
	}
	p := u.Profile
	p.SelectedTalkgroup = 1099
	p.TransmitTalkgroups = []int{999}
	p.PreferredPositionID = "missing"
	p.Receive = []ReceiveSetting{{TalkgroupID: 999, Pan: 2, Volume: 1.1}, {TalkgroupID: 999, Volume: -1}}
	if err := s.SaveProfile(u.ID, p); err != nil {
		t.Fatal(err)
	}
	saved, _, err := s.User(u.ID)
	if err != nil || !reflect.DeepEqual(saved.Profile, p) {
		t.Fatal("profile preferences were altered", err)
	}
}

func TestProfileLayoutDoesNotChangeAuthenticationButPermissionChangeDoes(t *testing.T) {
	a := userRecord{User: fixtureUser(), PasswordHash: []byte("opaque-verifier")}
	b := a
	b.User = cloneUser(t, a.User)
	b.User.Profile.Name = "Another layout"
	if !sameUserAuthentication(a, b) {
		t.Fatal("layout-only edit revoked authentication")
	}
	b.User.Permissions = append(b.User.Permissions, "radio_check")
	if sameUserAuthentication(a, b) {
		t.Fatal("permission edit retained old authentication authority")
	}
}

func TestUserIdentityChangePreservesCredentialsAndUnresolvedReferences(t *testing.T) {
	s := storeOpen(t, RoleSystem)
	u := fixtureUser()
	if err := s.SaveUser(u.ID, u, "test-user-password"); err != nil {
		t.Fatal(err)
	}
	_, hash, _ := s.User(u.ID)
	oldID := u.ID
	u.ID = "renamed"
	u.PositionIDs = []string{"missing"}
	if err := s.SaveUser(oldID, u, ""); err != nil {
		t.Fatal(err)
	}
	saved, newHash, err := s.User(u.ID)
	if err != nil || !bytes.Equal(hash, newHash) || !reflect.DeepEqual(saved, u) {
		t.Fatal("rename altered credentials or entered references", err)
	}
	if _, _, err = s.User(oldID); err == nil {
		t.Fatal("rename kept an obsolete account")
	}
}
