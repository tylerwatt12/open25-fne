package framework

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/cookiejar"
	"net/http/httptest"
	"path/filepath"
	"reflect"
	"strings"
	"sync"
	"testing"
)

func desktopFixture() DesktopProfile {
	p := emptyDesktopProfile()
	p.ActiveWorkspaceID, p.DefaultWorkspaceID = "day", "day"
	p.Receive = []ReceiveSetting{{TalkgroupID: 1001, Volume: 0.4, Pan: -1, Muted: true}}
	p.Workspaces = []DesktopWorkspace{{ID: "day", Name: "Day shift", Folders: []DesktopFolder{{ID: "north", Name: "North"}}, Placements: []DesktopPlacement{
		{ID: "main", TalkgroupID: 1001, FolderID: "north", Primary: true, Expanded: true},
		{ID: "duplicate", TalkgroupID: 1001, ActivePanel: "audio"},
	}, Multiselects: []DesktopMultiselect{{ID: "announce", Name: "Announcement", Talkgroups: []int{1001}}}}}
	return p
}

func cloneDesktop(p DesktopProfile) DesktopProfile { return cloneDesktopProfile(p) }

func desktopRequest(t *testing.T, s *Service, method string, body any, session Session) *httptest.ResponseRecorder {
	t.Helper()
	var raw []byte
	if body != nil {
		var err error
		raw, err = json.Marshal(body)
		if err != nil {
			t.Fatal(err)
		}
	}
	r := httptest.NewRequest(method, "http://example.test/api/desktop-workspace", bytes.NewReader(raw))
	r.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()
	s.desktopWorkspaceHTTP(w, r, session)
	return w
}

func TestDesktopWorkspaceRoundTripUsesSystemAuthorityOnBothPaths(t *testing.T) {
	for _, viaSite := range []bool{false, true} {
		t.Run(map[bool]string{false: "through-zone", true: "through-site"}[viaSite], func(t *testing.T) {
			c := newProfileChain(t, viaSite)
			before := cloneUser(t, c.user).Profile
			session := Session{UserID: c.user.ID, Username: c.user.Username}
			p := desktopFixture()
			p.Theme, p.Density = "dark", "comfortable"
			put := desktopRequest(t, c.dispatch.service, http.MethodPut, p, session)
			if put.Code != http.StatusOK {
				t.Fatalf("save failed: %d %s", put.Code, put.Body.String())
			}
			var saved struct {
				Saved   bool           `json:"saved"`
				Desktop DesktopProfile `json:"desktop"`
			}
			if err := json.Unmarshal(put.Body.Bytes(), &saved); err != nil || !saved.Saved || !reflect.DeepEqual(saved.Desktop, p) {
				t.Fatalf("entered preferences changed: %s %v", put.Body.String(), err)
			}
			nodes := []*profileTestNode{c.system, c.zone, c.dispatch}
			if viaSite {
				nodes = append(nodes, c.site)
			}
			for _, node := range nodes {
				user, _, err := node.service.Store.User(c.user.ID)
				if err != nil || user.Profile.Desktop == nil || !reflect.DeepEqual(*user.Profile.Desktop, saved.Desktop) {
					t.Fatalf("%s did not retain the central document: %#v %v", node.service.Config().Role, user.Profile.Desktop, err)
				}
				user.Profile.Desktop = nil
				if !reflect.DeepEqual(user.Profile, before) {
					t.Fatalf("desktop changed PTT programming at %s", node.service.Config().Role)
				}
			}
			get := desktopRequest(t, c.dispatch.service, http.MethodGet, nil, session)
			if get.Code != 200 {
				t.Fatal(get.Body.String())
			}
			var exported struct{ Desktop DesktopProfile }
			if err := json.Unmarshal(get.Body.Bytes(), &exported); err != nil || !reflect.DeepEqual(exported.Desktop, saved.Desktop) {
				t.Fatalf("export differs from saved preferences: %s %v", get.Body.String(), err)
			}
			for _, secret := range []string{"password", "proof", "token", "radio_id", "key_id", "device", "transmit_talkgroups"} {
				if bytes.Contains(get.Body.Bytes(), []byte(`"`+secret+`"`)) {
					t.Fatalf("workspace export contains %s", secret)
				}
			}
			if again := desktopRequest(t, c.dispatch.service, http.MethodPut, exported.Desktop, session); again.Code != 200 {
				t.Fatalf("export cannot be reimported: %s", again.Body.String())
			}
		})
	}
}

func TestDesktopWorkspaceOutageDoesNotSaveOnlyAtConsoleOrSite(t *testing.T) {
	c := newProfileChain(t, true)
	before := map[*profileTestNode][]byte{}
	for _, node := range []*profileTestNode{c.dispatch, c.site, c.zone} {
		before[node], _, _ = node.service.Store.ReadCache("users")
	}
	c.system.server.Close()
	w := desktopRequest(t, c.dispatch.service, http.MethodPut, desktopFixture(), Session{UserID: c.user.ID, Username: c.user.Username})
	if w.Code != http.StatusServiceUnavailable {
		t.Fatalf("outage looked saved: %d %s", w.Code, w.Body.String())
	}
	for node, expected := range before {
		after, _, _ := node.service.Store.ReadCache("users")
		if !bytes.Equal(expected, after) {
			t.Fatalf("%s made a local-only save", node.service.Config().Role)
		}
	}
}

func ptrDesktop(p DesktopProfile) *DesktopProfile { return &p }

func TestDesktopWorkspaceImportRejectsLiveStateAndUnsupportedFields(t *testing.T) {
	c := newProfileChain(t, false)
	for _, field := range []string{`"transmit_talkgroups":[1001]`, `"patches":[]`, `"emergency":true`, `"command":"inhibit"`, `"user_id":"another-user"`, `"token":"secret"`, `"revision":1`} {
		r := httptest.NewRequest(http.MethodPut, "http://example.test/api/desktop-workspace", strings.NewReader(`{`+field+`}`))
		r.Header.Set("Content-Type", "application/json")
		w := httptest.NewRecorder()
		c.dispatch.service.desktopWorkspaceHTTP(w, r, Session{UserID: c.user.ID, Username: c.user.Username})
		if w.Code != http.StatusBadRequest {
			t.Fatalf("import accepted %s: %d %s", field, w.Code, w.Body.String())
		}
	}
	p := desktopFixture()
	p.Workspaces[0].Placements[0].TalkgroupID = 1002
	if w := desktopRequest(t, c.dispatch.service, http.MethodPut, p, Session{UserID: c.user.ID, Username: c.user.Username}); w.Code != 200 {
		t.Fatalf("workspace preference was rejected: %s", w.Body.String())
	}
}

func TestDesktopWorkspaceOwnAccountAndPeerAuthorization(t *testing.T) {
	c := newProfileChain(t, false)
	for _, session := range []Session{{Username: "owner"}, {UserID: "missing", Username: "unknown"}, {UserID: c.user.ID, Username: "different"}} {
		if w := desktopRequest(t, c.dispatch.service, http.MethodGet, nil, session); w.Code != 403 {
			t.Fatalf("unassigned account read workspace: %d %s", w.Code, w.Body.String())
		}
	}
	if w := desktopRequest(t, c.site.service, http.MethodGet, nil, Session{UserID: c.user.ID, Username: c.user.Username}); w.Code != 403 {
		t.Fatal("Site exposed a second workspace editor")
	}
	u, hash, _ := c.zone.service.Store.User(c.user.ID)
	base := desktopWorkspaceUpdate{UserID: u.ID, Proof: userProof(u, hash), Desktop: desktopFixture()}
	cases := []struct {
		peer   PeerCredential
		change func(*desktopWorkspaceUpdate)
		want   int
	}{
		{PeerCredential{Role: RoleApplication, InstanceID: "application-1"}, func(u *desktopWorkspaceUpdate) { u.Proof = "forged" }, 403},
		{PeerCredential{Role: RoleApplication, InstanceID: "application-1"}, func(u *desktopWorkspaceUpdate) { u.UserID = "other" }, 403},
		{PeerCredential{Role: RoleSite, InstanceID: "other-site"}, func(*desktopWorkspaceUpdate) {}, 403},
		{PeerCredential{Role: RoleSystem, InstanceID: "system-1"}, func(*desktopWorkspaceUpdate) {}, 403},
		{PeerCredential{Role: RoleApplication, InstanceID: "application-1"}, func(u *desktopWorkspaceUpdate) { u.Desktop.Receive[0].TalkgroupID = 1002 }, 200},
	}
	for i, tc := range cases {
		update := base
		update.Desktop = cloneDesktop(base.Desktop)
		tc.change(&update)
		raw, _ := json.Marshal(update)
		r := httptest.NewRequest(http.MethodPost, "http://example.test/internal/desktop-workspace", bytes.NewReader(raw))
		r.Header.Set("Content-Type", "application/json")
		w := httptest.NewRecorder()
		c.zone.service.desktopWorkspaceInternal(w, r, tc.peer)
		if w.Code != tc.want {
			t.Fatalf("case %d: %d %s", i, w.Code, w.Body.String())
		}
		if bytes.Contains(w.Body.Bytes(), []byte(base.Proof)) {
			t.Fatal("workspace error exposed authorization proof")
		}
	}
}

func TestDesktopWorkspacePublicEndpointRequiresSessionAndCSRF(t *testing.T) {
	c := newProfileChain(t, false)
	c.dispatch.service.refreshDirectory()
	server := httptest.NewServer(c.dispatch.service.Handler())
	defer server.Close()
	jar, _ := cookiejar.New(nil)
	client := &http.Client{Jar: jar}
	if status, _ := integrationHTTP(t, client, http.MethodGet, server.URL+"/api/desktop-workspace", nil, nil); status != 401 {
		t.Fatalf("anonymous workspace read: %d", status)
	}
	status, raw := integrationHTTP(t, client, http.MethodPost, server.URL+"/api/login", map[string]string{"username": c.user.Username, "password": "profile-test-password"}, nil)
	if status != 200 {
		t.Fatalf("sign-in: %d %s", status, raw)
	}
	var login struct{ CSRF string }
	if err := json.Unmarshal(raw, &login); err != nil || login.CSRF == "" {
		t.Fatalf("missing session proof: %s %v", raw, err)
	}
	if status, raw = integrationHTTP(t, client, http.MethodPut, server.URL+"/api/desktop-workspace", desktopFixture(), nil); status != 403 {
		t.Fatalf("save without CSRF: %d %s", status, raw)
	}
	if status, raw = integrationHTTP(t, client, http.MethodPut, server.URL+"/api/desktop-workspace", desktopFixture(), map[string]string{"Origin": "https://untrusted.example", "X-CSRF-Token": login.CSRF}); status != 403 {
		t.Fatalf("cross-origin workspace save: %d %s", status, raw)
	}
	if status, raw = integrationHTTP(t, client, http.MethodPut, server.URL+"/api/desktop-workspace", desktopFixture(), map[string]string{"Origin": server.URL, "X-CSRF-Token": login.CSRF}); status != 200 {
		t.Fatalf("authenticated central workspace save: %d %s", status, raw)
	}
}

func TestDesktopAndRadioProgrammingWritesPreserveEachOtherAndAuthentication(t *testing.T) {
	s := storeOpen(t, RoleSystem)
	u := fixtureUser()
	if err := s.SaveUser(u.ID, u, "desktop-test-password"); err != nil {
		t.Fatal(err)
	}
	before, hash, _ := s.User(u.ID)
	digest := userDigest(before, hash)
	profile := u.Profile
	profile.Name = "New radio programming"
	profile.Receive = []ReceiveSetting{{TalkgroupID: 1001, Volume: 0.9, Pan: 1}}
	desktop := desktopFixture()
	var wg sync.WaitGroup
	errors := make(chan error, 2)
	wg.Add(2)
	go func() { defer wg.Done(); errors <- s.SaveProfile(u.ID, profile) }()
	go func() { defer wg.Done(); errors <- s.SaveDesktopProfile(u.ID, desktop) }()
	wg.Wait()
	close(errors)
	for err := range errors {
		if err != nil {
			t.Fatal(err)
		}
	}
	after, afterHash, _ := s.User(u.ID)
	if after.Profile.Name != profile.Name || after.Profile.Receive[0].Volume != 0.9 || after.Profile.Desktop == nil || after.Profile.Desktop.Receive[0].Volume != 0.4 || !after.Profile.Desktop.Receive[0].Muted {
		t.Fatalf("a profile section overwrote the other: %#v", after.Profile)
	}
	if digest != userDigest(after, afterHash) {
		t.Fatal("saving a workspace changed account identity or permissions")
	}
	profile.Desktop = ptrDesktop(emptyDesktopProfile())
	if err := s.SaveProfile(u.ID, profile); err != nil {
		t.Fatal(err)
	}
	after, _, _ = s.User(u.ID)
	if after.Profile.Desktop.ActiveWorkspaceID != "day" {
		t.Fatal("a stale full-profile export erased the desktop")
	}
}

func TestDesktopWorkspaceReadScopesRevokedResourcesWithoutChangingSavedDocument(t *testing.T) {
	p := desktopFixture()
	p.Receive = append(p.Receive, ReceiveSetting{TalkgroupID: 1002, Volume: 1})
	p.Workspaces[0].Placements = append(p.Workspaces[0].Placements, DesktopPlacement{ID: "revoked", TalkgroupID: 1002})
	p.Workspaces[0].Multiselects[0].Talkgroups = []int{1001, 1002}
	p.Workspaces[0].Multiselects = append(p.Workspaces[0].Multiselects, DesktopMultiselect{ID: "revoked", Name: "Old resource", Talkgroups: []int{1002}})
	before, _ := json.Marshal(p)
	read := scopedDesktopProfile(&p, []int{1001})
	after, _ := json.Marshal(p)
	if !bytes.Equal(before, after) {
		t.Fatal("permission filtering modified a stored document through shared slices")
	}
	if len(read.Receive) != 1 || len(read.Workspaces[0].Placements) != 2 || len(read.Workspaces[0].Multiselects) != 1 || !reflect.DeepEqual(read.Workspaces[0].Multiselects[0].Talkgroups, []int{1001}) {
		t.Fatalf("revoked resources remained in exported layout: %#v", read)
	}
}

func TestDesktopWorkspacePersistsAcrossStoreRestart(t *testing.T) {
	path := filepath.Join(t.TempDir(), "configuration.db")
	s, err := OpenStore(path, RoleSystem)
	if err != nil {
		t.Fatal(err)
	}
	u := fixtureUser()
	if err = s.SaveUser(u.ID, u, "desktop-test-password"); err == nil {
		err = s.SaveDesktopProfile(u.ID, desktopFixture())
	}
	if err != nil {
		s.Close()
		t.Fatal(err)
	}
	if err = s.Close(); err != nil {
		t.Fatal(err)
	}
	s, err = OpenStore(path, RoleSystem)
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	u, _, err = s.User(u.ID)
	if err != nil || u.Profile.Desktop == nil || u.Profile.Desktop.DefaultWorkspaceID != "day" || u.Profile.Receive[0].Volume != 0.8 {
		t.Fatalf("central workspace or independent radio preferences lost: %#v %v", u.Profile, err)
	}
}
