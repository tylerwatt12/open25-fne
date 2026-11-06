package framework

import (
	"bytes"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

type profileTestNode struct {
	service *Service
	server  *httptest.Server
}
type profileTestChain struct {
	system, zone, site, dispatch *profileTestNode
	user                         User
}

func profileNode(t *testing.T, role Role, id string, system SystemConfig, zone ZoneConfig, creds ServiceCredentials, records []userRecord) *profileTestNode {
	t.Helper()
	dir := t.TempDir()
	store, err := OpenStore(filepath.Join(dir, "configuration.db"), role)
	if err != nil {
		t.Fatal(err)
	}
	cfg := FileConfig{Role: role, InstanceID: id, StateDir: dir, Listen: "127.0.0.1:0", InternalListen: "127.0.0.1:0", InternalURL: "http://127.0.0.1:5100", PublicURL: "http://127.0.0.1:5000", PollSeconds: 1, CredentialFile: filepath.Join(dir, "credentials.json")}
	if err = WritePrivateJSON(cfg.CredentialFile, creds); err != nil {
		t.Fatal(err)
	}
	var own any
	switch role {
	case RoleSystem:
		own = system
	case RoleZone:
		own = zone
	case RoleSite:
		own = storeFixtureSite()
	case RoleApplication:
		own = ApplicationConfig{Name: "Test dispatch", ConnectionURL: "http://127.0.0.1:5001"}
	}
	if err = store.WriteCache("system", storeDoc(t, system)); err != nil {
		t.Fatal(err)
	}
	if err = store.WriteCache("zone", storeDoc(t, zone)); err != nil {
		t.Fatal(err)
	}
	// user records are protected transport data, not a public configuration.
	raw, err := json.Marshal(records)
	if err != nil {
		t.Fatal(err)
	}
	if role == RoleSystem {
		_, err = store.db.Exec("INSERT INTO framework_documents(name,document,at) VALUES(?,?,?)", store.usersKey(), raw, nowStamp())
	} else {
		err = store.WriteCache("users", raw)
	}
	if err != nil {
		t.Fatal(err)
	}
	if err = store.SaveConfig(storeDoc(t, own)); err != nil {
		t.Fatal(err)
	}
	client := newHTTPClient()
	s := &Service{Store: store, config: cfg, configPath: filepath.Join(dir, "service.json"), client: client, sessions: map[string]Session{}, attempts: map[string]loginAttempt{}}
	n := &profileTestNode{service: s}
	n.server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/internal/profile" && r.URL.Path != "/internal/desktop-workspace" {
			http.NotFound(w, r)
			return
		}
		peer, ok := s.internalPeer(r)
		if !ok {
			fail(w, 401, errors.New("invalid test service credentials"))
			return
		}
		if r.URL.Path == "/internal/desktop-workspace" {
			s.desktopWorkspaceInternal(w, r, peer)
		} else {
			s.profileInternal(w, r, peer)
		}
	}))
	s.config.InternalURL = n.server.URL
	t.Cleanup(func() { n.server.Close(); _ = store.Close() })
	return n
}

func newProfileChain(t *testing.T, viaSite bool) *profileTestChain {
	t.Helper()
	system := userFixtureSystem()
	zone := storeFixtureZone()
	u := fixtureUser()
	u.Talkgroups = []int{1001}
	u.Profile = UserProfile{Name: "Original", Zones: []ProfileZone{{Name: "Main", Talkgroups: []int{1001}}}, Receive: []ReceiveSetting{{TalkgroupID: 1001, Volume: 1, Pan: 0}}, SelectedTalkgroup: 1001, TransmitTalkgroups: []int{}}
	seed := storeOpen(t, RoleSystem)
	if err := seed.SaveUser(u.ID, u, "profile-test-password"); err != nil {
		t.Fatal(err)
	}
	records, err := seed.userRecords()
	if err != nil {
		t.Fatal(err)
	}
	zoneToken, siteToken, dispatchToken := strings.Repeat("z", 64), strings.Repeat("s", 64), strings.Repeat("d", 64)
	c := &profileTestChain{user: u}
	c.system = profileNode(t, RoleSystem, "system-1", system, zone, ServiceCredentials{Peers: map[string]PeerCredential{"zone-1": {Role: RoleZone, InstanceID: "zone-1", Token: zoneToken}}}, records)
	c.zone = profileNode(t, RoleZone, "zone-1", system, zone, ServiceCredentials{OutboundID: "zone-1", OutboundToken: zoneToken, Peers: map[string]PeerCredential{"site-1": {Role: RoleSite, InstanceID: "site-1", Token: siteToken}, "application-1": {Role: RoleApplication, InstanceID: "application-1", Token: dispatchToken}}}, records)
	c.zone.service.config.UpstreamURL = c.system.server.URL
	c.site = profileNode(t, RoleSite, "site-1", system, zone, ServiceCredentials{OutboundID: "site-1", OutboundToken: siteToken, Peers: map[string]PeerCredential{"application-1": {Role: RoleApplication, InstanceID: "application-1", Token: dispatchToken}}}, records)
	c.site.service.config.UpstreamURL = c.zone.server.URL
	c.dispatch = profileNode(t, RoleApplication, "application-1", system, zone, ServiceCredentials{OutboundID: "application-1", OutboundToken: dispatchToken, Peers: map[string]PeerCredential{}}, records)
	upstream := c.zone.server.URL
	if viaSite {
		upstream = c.site.server.URL
	}
	if err = c.dispatch.service.Store.SaveConfig(storeDoc(t, ApplicationConfig{Name: "Test dispatch", ConnectionURL: upstream})); err != nil {
		t.Fatal(err)
	}
	return c
}

func profileRequest(t *testing.T, s *Service, method string, body any, session Session) *httptest.ResponseRecorder {
	t.Helper()
	var raw []byte
	if body != nil {
		var err error
		raw, err = json.Marshal(body)
		if err != nil {
			t.Fatal(err)
		}
	}
	r := httptest.NewRequest(method, "http://example.test/api/profile", bytes.NewReader(raw))
	r.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()
	s.profileHTTP(w, r, session)
	return w
}

func TestProfileWriteReachesCentralAuthorityThroughBothServicePaths(t *testing.T) {
	for _, viaSite := range []bool{false, true} {
		name := "through-zone"
		if viaSite {
			name = "through-site-and-zone"
		}
		t.Run(name, func(t *testing.T) {
			c := newProfileChain(t, viaSite)
			p := c.user.Profile
			p.Name = "Saved centrally"
			p.Receive = []ReceiveSetting{{TalkgroupID: 1001, Volume: 0.4, Pan: -1}}
			w := profileRequest(t, c.dispatch.service, http.MethodPut, p, Session{UserID: c.user.ID, Username: c.user.Username})
			if w.Code != 200 {
				t.Fatalf("save: %d %s", w.Code, w.Body.String())
			}
			nodes := []*profileTestNode{c.system, c.zone, c.dispatch}
			if viaSite {
				nodes = append(nodes, c.site)
			}
			for _, node := range nodes {
				u, _, err := node.service.Store.User(c.user.ID)
				if err != nil || u.Profile.Name != p.Name || u.Profile.Receive[0].Pan != -1 {
					t.Fatalf("profile not retained at %s: %#v %v", node.service.Config().Role, u.Profile, err)
				}
			}
			get := profileRequest(t, c.dispatch.service, http.MethodGet, nil, Session{UserID: c.user.ID, Username: c.user.Username})
			if get.Code != 200 || !bytes.Contains(get.Body.Bytes(), []byte("Saved centrally")) {
				t.Fatal("saved profile not readable")
			}
			if bytes.Contains(get.Body.Bytes(), []byte("password_hash")) || bytes.Contains(get.Body.Bytes(), []byte("proof")) {
				t.Fatal("profile read leaked credentials")
			}
		})
	}
}

func TestProfileWriteFailureDoesNotMakeLocalOnlySavedCopy(t *testing.T) {
	c := newProfileChain(t, true)
	before, _, _ := c.dispatch.service.Store.ReadCache("users")
	beforeZone, _, _ := c.zone.service.Store.ReadCache("users")
	c.system.server.Close()
	p := c.user.Profile
	p.Name = "Must not appear saved"
	w := profileRequest(t, c.dispatch.service, http.MethodPut, p, Session{UserID: c.user.ID, Username: c.user.Username})
	if w.Code != 503 {
		t.Fatalf("wanted unavailable: %d %s", w.Code, w.Body.String())
	}
	after, _, _ := c.dispatch.service.Store.ReadCache("users")
	afterZone, _, _ := c.zone.service.Store.ReadCache("users")
	if !bytes.Equal(before, after) || !bytes.Equal(beforeZone, afterZone) {
		t.Fatal("upstream failure replaced a retained local profile")
	}
}

func TestProfileProofAndServiceScopeRejectUntrustedUpdates(t *testing.T) {
	c := newProfileChain(t, false)
	user, hash, _ := c.zone.service.Store.User(c.user.ID)
	valid := profileUpdate{UserID: user.ID, Proof: userProof(user, hash), Profile: user.Profile}
	cases := []struct {
		name   string
		peer   PeerCredential
		change func(*profileUpdate)
		want   int
	}{
		{"wrong proof", PeerCredential{Role: RoleApplication, InstanceID: "application-1"}, func(p *profileUpdate) { p.Proof = strings.Repeat("0", 64) }, 403},
		{"unknown user", PeerCredential{Role: RoleApplication, InstanceID: "application-1"}, func(p *profileUpdate) { p.UserID = "missing-user" }, 403},
		{"forbidden group", PeerCredential{Role: RoleApplication, InstanceID: "application-1"}, func(p *profileUpdate) { p.Profile.SelectedTalkgroup = 1002 }, 200},
		{"unenrolled Site", PeerCredential{Role: RoleSite, InstanceID: "missing-site"}, func(*profileUpdate) {}, 403},
		{"wrong peer role", PeerCredential{Role: RoleSystem, InstanceID: "system-1"}, func(*profileUpdate) {}, 403},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			payload := valid
			payload.Profile = cloneUser(t, c.user).Profile
			tc.change(&payload)
			raw, _ := json.Marshal(payload)
			r := httptest.NewRequest(http.MethodPost, "http://example.test/internal/profile", bytes.NewReader(raw))
			r.Header.Set("Content-Type", "application/json")
			w := httptest.NewRecorder()
			c.zone.service.profileInternal(w, r, tc.peer)
			if w.Code != tc.want {
				t.Fatalf("wanted %d, got %d: %s", tc.want, w.Code, w.Body.String())
			}
			if bytes.Contains(w.Body.Bytes(), []byte(valid.Proof)) {
				t.Fatal("error exposed private proof")
			}
		})
	}
}

func TestProfileAdminReadDoesNotCreateVirtualRadioAndCannotWrite(t *testing.T) {
	c := newProfileChain(t, false)
	admin := Session{Username: "owner"}
	get := profileRequest(t, c.dispatch.service, http.MethodGet, nil, admin)
	if get.Code != 200 {
		t.Fatal(get.Body.String())
	}
	var result struct {
		User User `json:"user"`
	}
	if err := json.Unmarshal(get.Body.Bytes(), &result); err != nil {
		t.Fatal(err)
	}
	if result.User.RadioID != 0 || len(result.User.Talkgroups) > 0 || result.User.Role != "administrator" {
		t.Fatal("administrator inherited a transmitting radio identity")
	}
	if w := profileRequest(t, c.dispatch.service, http.MethodPut, c.user.Profile, admin); w.Code != 403 {
		t.Fatal("administrator wrote an operator profile")
	}
	if w := profileRequest(t, c.site.service, http.MethodGet, nil, Session{UserID: c.user.ID, Username: c.user.Username}); w.Code != 403 {
		t.Fatal("operator profile exposed through Site administration")
	}
}

func TestOperatorStateContainsOnlyOwnIdentityAndPermittedDirectory(t *testing.T) {
	c := newProfileChain(t, false)
	system, _, err := c.dispatch.service.documents()
	if err != nil {
		t.Fatal(err)
	}
	system.Talkgroups[0].StartPolicy = "all_start"
	system.Talkgroups[0].Priority = 3
	system.Talkgroups[0].AdmissionMode = "both"
	system.Talkgroups[0].TransmitMode = "selectable"
	system.Talkgroups[0].KeyID = 71
	system.Talkgroups[0].AlgorithmID = 132
	if err := c.dispatch.service.Store.WriteCache("system", storeDoc(t, system)); err != nil {
		t.Fatal(err)
	}
	c.dispatch.service.refreshDirectory()
	state := c.dispatch.service.operatorState(Session{UserID: c.user.ID, Username: c.user.Username, CSRF: "public-csrf", Expires: time.Now().Add(time.Hour)})
	if state["is_administrator"] != false || state["user_role"] != "dispatcher" {
		t.Fatal("wrong operator state")
	}
	raw, err := json.Marshal(state)
	if err != nil {
		t.Fatal(err)
	}
	for _, field := range []string{"password_hash", "credential_file", "connection_url", "modems", "device", "topology", "private_key", "proof"} {
		if bytes.Contains(raw, []byte(`"`+field+`"`)) {
			t.Fatalf("operator state exposed %s", field)
		}
	}
	var result struct {
		User      User              `json:"user"`
		Position  ConsolePosition   `json:"position"`
		Positions []ConsolePosition `json:"positions"`
		System    struct {
			Talkgroups []Talkgroup `json:"talkgroups"`
		} `json:"system"`
	}
	if err = json.Unmarshal(raw, &result); err != nil {
		t.Fatal(err)
	}
	if len(result.System.Talkgroups) != 1 || result.System.Talkgroups[0].TGID != 1001 {
		t.Fatal("operator received another group's directory entry")
	}
	g := result.System.Talkgroups[0]
	if g.StartPolicy != "all_start" || g.Priority != 3 || g.AdmissionMode != "both" || g.KeyID != 71 {
		t.Fatal("operator did not receive the talkgroup's nonsecret call and encryption policy")
	}
	if result.User.RadioID != 10002 || result.Position.ID != "desk-1" || len(result.Positions) != 1 {
		t.Fatal("person and desk identity were not independently resolved")
	}
	if len(result.Position.Talkgroups) != 1 || result.Position.Talkgroups[0] != 1001 || len(result.Positions[0].Talkgroups) != 1 {
		t.Fatal("desk metadata exposed groups outside the person's permissions")
	}
}
