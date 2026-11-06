package framework

import (
	"bytes"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"open25.local/open25-fne/internal/frameworkui"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestAdminChooserLargeDirectoriesBoundPagesAndKeepAllIdentities(t *testing.T) {
	c := integrationNewCluster(t)
	s := c.system.service
	system := storeFixtureSystem()
	zone := storeFixtureZone()
	const count = 50000
	system.Radios = nil
	system.Talkgroups = nil
	zone.Sites = nil
	users := make([]userRecord, 0, count)
	for i := 0; i < count; i++ {
		system.Radios = append(system.Radios, Radio{RID: 100000 + i, Alias: fmt.Sprintf("Unit %05d", i), Kind: "subscriber", Group: "Existing Fleet", Enabled: i%2 == 0})
		system.Talkgroups = append(system.Talkgroups, Talkgroup{TGID: 100000 + i, Alias: fmt.Sprintf("Group %05d", i), Enabled: i%2 == 0})
		zone.Sites = append(zone.Sites, SiteIdentity{ID: fmt.Sprintf("site-%05d", i), Name: fmt.Sprintf("Site %05d", i)})
		users = append(users, userRecord{User: User{ID: fmt.Sprintf("login-%05d", i), Username: fmt.Sprintf("Person %05d", i)}, PasswordHash: []byte("private-verifier-never-in-chooser")})
	}
	if err := s.Store.SaveConfig(storeDoc(t, system)); err != nil {
		t.Fatal(err)
	}
	if err := s.Store.WriteCache("zone", storeDoc(t, zone)); err != nil {
		t.Fatal(err)
	}
	raw, _ := json.Marshal(users)
	if _, err := s.Store.db.Exec("INSERT INTO framework_documents(name,document,at) VALUES('users',?,?) ON CONFLICT(name) DO UPDATE SET document=excluded.document", raw, nowStamp()); err != nil {
		t.Fatal(err)
	}
	s.refreshDirectory()
	before, _ := s.Store.Config()
	for _, kind := range []string{"radios", "talkgroups", "sites", "users"} {
		for _, offset := range []int{0, 50, count - 50} {
			start := time.Now()
			w := httptest.NewRecorder()
			s.webChooser(w, httptest.NewRequest("GET", fmt.Sprintf("/chooser?kind=%s&offset=%d", kind, offset), nil))
			var page adminChoicePage
			if w.Code != 200 || json.Unmarshal(w.Body.Bytes(), &page) != nil || page.Total != count || page.Offset != offset || len(page.Items) != 50 {
				t.Fatalf("%s page %d: HTTP%d / %#v", kind, offset, w.Code, page)
			}
			if w.Body.Len() > 16384 || bytes.Contains(w.Body.Bytes(), []byte("password")) || bytes.Contains(w.Body.Bytes(), []byte("private-verifier")) || bytes.Contains(w.Body.Bytes(), []byte("profile")) {
				t.Fatal("unbounded/private chooser response", kind, w.Body.Len())
			}
			t.Logf("%s offset=%d response=%d bytes elapsed=%s", kind, offset, w.Body.Len(), time.Since(start))
		}
	}
	w := httptest.NewRecorder()
	s.webChooser(w, httptest.NewRequest("GET", "/chooser?kind=radios&q=uNIt+49999", nil))
	var page adminChoicePage
	_ = json.Unmarshal(w.Body.Bytes(), &page)
	if len(page.Items) != 1 || page.Items[0].ID != "149999" {
		t.Fatal("name search lost disabled radio", w.Body.String())
	}
	// The race detector instruments the large JSON fixture too; keep that
	// diagnostic work separate from the timings recorded below.
	client := earlyWebLogin(t, c.system)
	client.Timeout = time.Minute
	for _, path := range []string{"/talkgroups/edit?row=0", "/radios?q=49999", "/users?q=49999", "/profiles/edit?identity=login-49999"} {
		start := time.Now()
		html := earlyWebGet(t, client, c.system.web.URL+path, 200)
		if len(html) > 128<<10 || bytes.Contains(html, []byte("private-verifier")) {
			t.Fatal("form embedded directory/credentials", path, len(html))
		}
		if path == "/talkgroups/edit?row=0" && bytes.Contains(html, []byte("Unit 49999")) {
			t.Fatal("chooser options embedded in form")
		}
		t.Logf("page %s response=%d bytes elapsed=%s", path, len(html), time.Since(start))
	}
	if evidence := os.Getenv("OPEN25_CHOOSER_BROWSER_EVIDENCE"); evidence != "" {
		s.assets = frameworkui.Assets()
		if err := s.writeTrafficKeys([]trafficKey{{AlgorithmID: 132, KeyID: 17, Name: "Example loaded key", Enabled: false, Material: strings.Repeat("11", 32)}}); err != nil {
			t.Fatal(err)
		}
		if err := os.MkdirAll(evidence, 0700); err != nil {
			t.Fatal(err)
		}
		fixture := filepath.Join(evidence, "fixture.private.json")
		data, _ := json.Marshal(map[string]any{"origin": c.system.web.URL, "password": integrationPassword})
		if err := os.WriteFile(fixture, data, 0600); err != nil {
			t.Fatal(err)
		}
		command := exec.Command("node", "../../tools/framework/admin-chooser-check.mjs", fixture, evidence)
		command.Stdout = os.Stdout
		command.Stderr = os.Stderr
		if err := command.Run(); err != nil {
			t.Fatal("chooser browser", err)
		}
	}

	after, _ := s.Store.Config()
	if !bytes.Equal(before, after) {
		t.Fatal("search/page load changed configuration")
	}
}

func TestAdminChooserAuthenticationOwnershipAndMalformedPages(t *testing.T) {
	c := integrationNewCluster(t)
	unauth := &http.Client{CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}
	earlyWebGet(t, unauth, c.system.web.URL+"/chooser?kind=radios", http.StatusSeeOther)
	client := earlyWebLogin(t, c.system)
	earlyWebGet(t, client, c.system.web.URL+"/chooser?kind=radios&offset=-1", 400)
	earlyWebGet(t, client, c.system.web.URL+"/chooser?kind=modems", 404)
	earlyWebGet(t, earlyWebLogin(t, c.site), c.site.web.URL+"/chooser?kind=users", 404)
	request := httptest.NewRequest("POST", "/chooser?kind=radios", strings.NewReader("{}"))
	w := httptest.NewRecorder()
	c.system.service.webChooser(w, request)
	if w.Code != 405 {
		t.Fatal("chooser accepted mutation")
	}
}

func TestBandPlanFrequenciesAndPerSiteBroadcastPolicy(t *testing.T) {
	zone := storeFixtureZone()
	for _, row := range []struct {
		id, ch int
		tx, rx int64
	}{{2, 0, 443437500, 448437500}, {2, 1, 443450000, 448450000}, {2, 81, 444450000, 449450000}} {
		tx, rx, ok := bandPlanFrequencies(&zone, row.id, row.ch)
		if !ok || tx != row.tx || rx != row.rx {
			t.Fatal("Phase1 equation", row, tx, rx, ok)
		}
	}
	zone.ChannelIdentifiers[0].BaseHz = 851006250
	zone.ChannelIdentifiers[0].SpacingHz = 6250
	zone.ChannelIdentifiers[0].OffsetHz = -45000000
	tx, rx, ok := bandPlanFrequencies(&zone, 2, 160)
	if !ok || tx != 852006250 || rx != 807006250 {
		t.Fatal("700/800 implicit duplex calculation")
	}
	if _, _, ok = bandPlanFrequencies(nil, 2, 1); ok {
		t.Fatal("missing plan fabricated frequencies")
	}
	c := integrationNewCluster(t)
	client := earlyWebLogin(t, c.site)
	address := c.site.web.URL + "/control-policy"
	form := earlyWebForm(t, earlyWebGet(t, client, address, 200), "broadcast_band_plan")
	if form.Get("broadcast_band_plan") != "true" {
		t.Fatal("normal policy not enabled")
	}
	form.Del("broadcast_band_plan")
	earlyWebPost(t, client, address, form, c.site.web.URL, 303)
	p, err := c.site.service.profileRadio()
	if err != nil || p.Site.BroadcastBandPlan {
		t.Fatal("Site policy missing from native profile", err)
	}
	var other SiteConfig
	if err = c.otherSite.service.owned(&other); err != nil || !other.BroadcastBandPlan {
		t.Fatal("one Site changed the other Site", err)
	}
}
