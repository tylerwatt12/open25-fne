package framework

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/cookiejar"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"

	"golang.org/x/net/html"
	"open25.local/open25-fne/internal/frameworkui"
)

func earlyWebLogin(t *testing.T, pair *integrationService) *http.Client {
	t.Helper()
	jar, err := cookiejar.New(nil)
	if err != nil {
		t.Fatal(err)
	}
	client := &http.Client{Jar: jar, Timeout: 10 * time.Second, CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}
	response, err := client.PostForm(pair.web.URL+"/login", url.Values{"username": {"administrator"}, "password": {integrationPassword}})
	if err != nil {
		t.Fatal(err)
	}
	defer response.Body.Close()
	if response.StatusCode != http.StatusSeeOther || response.Header.Get("Location") != "/" {
		body, _ := io.ReadAll(response.Body)
		t.Fatalf("form login: %d %s", response.StatusCode, body)
	}
	return client
}

func earlyWebGet(t *testing.T, client *http.Client, address string, want int) []byte {
	t.Helper()
	response, err := client.Get(address)
	if err != nil {
		t.Fatal(err)
	}
	defer response.Body.Close()
	body, err := io.ReadAll(response.Body)
	if err != nil {
		t.Fatal(err)
	}
	if response.StatusCode != want {
		t.Fatalf("GET %s: %d; want %d: %s", address, response.StatusCode, want, body)
	}
	return body
}

// Read the actual server-rendered form so these tests exercise ordinary form
// submission, including unchanged fields and checkbox/select behavior.
func earlyWebForm(t *testing.T, body []byte, field string) url.Values {
	t.Helper()
	document, err := html.Parse(bytes.NewReader(body))
	if err != nil {
		t.Fatal(err)
	}
	attr := func(n *html.Node, key string) string {
		for _, a := range n.Attr {
			if a.Key == key {
				return a.Val
			}
		}
		return ""
	}
	hasAttr := func(n *html.Node, key string) bool {
		for _, a := range n.Attr {
			if a.Key == key {
				return true
			}
		}
		return false
	}
	var text func(*html.Node) string
	text = func(n *html.Node) string {
		if n.Type == html.TextNode {
			return n.Data
		}
		var s strings.Builder
		for c := n.FirstChild; c != nil; c = c.NextSibling {
			s.WriteString(text(c))
		}
		return s.String()
	}
	var find func(*html.Node) *html.Node
	find = func(n *html.Node) *html.Node {
		if n.Type == html.ElementNode && n.Data == "form" {
			var contains func(*html.Node) bool
			contains = func(c *html.Node) bool {
				if attr(c, "name") == field {
					return true
				}
				for d := c.FirstChild; d != nil; d = d.NextSibling {
					if contains(d) {
						return true
					}
				}
				return false
			}
			if contains(n) {
				return n
			}
		}
		for c := n.FirstChild; c != nil; c = c.NextSibling {
			if found := find(c); found != nil {
				return found
			}
		}
		return nil
	}
	form := find(document)
	if form == nil {
		t.Fatalf("form field %q missing: %s", field, body)
	}
	values := url.Values{}
	var read func(*html.Node)
	read = func(n *html.Node) {
		name := attr(n, "name")
		if name != "" {
			switch n.Data {
			case "input":
				kind := attr(n, "type")
				if kind != "submit" && kind != "button" && (kind != "checkbox" || hasAttr(n, "checked")) {
					values.Add(name, attr(n, "value"))
				}
			case "textarea":
				values.Add(name, text(n))
			case "select":
				first, selected := "", false
				for c := n.FirstChild; c != nil; c = c.NextSibling {
					if c.Data == "option" {
						if !selected {
							first = attr(c, "value")
							selected = true
						}
						if hasAttr(c, "selected") {
							first = attr(c, "value")
							break
						}
					}
				}
				values.Add(name, first)
			}
		}
		for c := n.FirstChild; c != nil; c = c.NextSibling {
			read(c)
		}
	}
	read(form)
	return values
}

func earlyWebPost(t *testing.T, client *http.Client, address string, values url.Values, origin string, want int) {
	t.Helper()
	request, err := http.NewRequest(http.MethodPost, address, strings.NewReader(values.Encode()))
	if err != nil {
		t.Fatal(err)
	}
	request.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	if origin != "" {
		request.Header.Set("Origin", origin)
	}
	response, err := client.Do(request)
	if err != nil {
		t.Fatal(err)
	}
	defer response.Body.Close()
	body, _ := io.ReadAll(response.Body)
	if response.StatusCode != want {
		t.Fatalf("POST %s: %d; want %d: %s", address, response.StatusCode, want, body)
	}
}

func TestEarlyWebAuthenticatedFormSavesPreserveUneditedConfiguration(t *testing.T) {
	c := integrationNewCluster(t)
	cases := []struct {
		pair                *integrationService
		route, field, value string
	}{
		{c.system, "/system", "callsign", "  entered lowercase  "},
		{c.zone, "/zone", "system_id", "unresolved-system"},
		{c.site, "/modems/edit?row=0", "name", "  Control modem renamed  "},
		{c.dispatch, "/application-settings", "connection_url", "unavailable controller"},
	}
	for _, test := range cases {
		t.Run(string(test.pair.service.Config().Role), func(t *testing.T) {
			pair := test.pair
			pair.service.assets = frameworkui.Assets()
			original, _ := pair.service.Store.Config()
			config, err := webConfig(original)
			if err != nil {
				t.Fatal(err)
			}
			config["qa_unedited"] = map[string]any{"keep": "  original text  ", "ids": []any{json.Number("9"), json.Number("9")}}
			if pair == c.site {
				modems := config["modems"].([]any)
				modems[0].(map[string]any)["qa_unedited"] = "retain modem field"
				modems = append(modems, map[string]any{"id": "modem-voice", "name": "Voice", "device": "/dev/trunksystem-voice", "baud": json.Number("115200")})
				config["modems"] = modems
			}
			if err = pair.service.Store.SaveConfig(storeDoc(t, config)); err != nil {
				t.Fatal(err)
			}
			before, _ := pair.service.Store.Config()
			client := earlyWebLogin(t, pair)
			values := earlyWebForm(t, earlyWebGet(t, client, pair.web.URL+test.route, 200), test.field)
			values.Set(test.field, test.value)
			if pair == c.system {
				values.Set("wacn", "0")
			}
			if pair == c.site {
				values.Set("calibration.rx_level", "101.5")
			}
			earlyWebPost(t, client, pair.web.URL+test.route, values, pair.web.URL, http.StatusSeeOther)
			savedRaw, _ := pair.service.Store.Config()
			saved, err := webConfig(savedRaw)
			if err != nil {
				t.Fatal(err)
			}
			if !reflect.DeepEqual(saved["qa_unedited"], config["qa_unedited"]) {
				t.Fatal("save erased an unedited configuration field")
			}
			switch pair {
			case c.system:
				if saved["callsign"] != test.value || webText(saved["wacn"]) != "0" {
					t.Fatal("entered values were changed")
				}
			case c.zone:
				if saved["system_id"] != test.value {
					t.Fatal("unresolved system reference was rewritten")
				}
			case c.dispatch:
				if saved["connection_url"] != test.value {
					t.Fatal("unavailable address was rewritten")
				}
			case c.site:
				modems := saved["modems"].([]any)
				control := modems[0].(map[string]any)
				voice := modems[1].(map[string]any)
				if control["name"] != test.value || control["device"] != "/dev/trunksystem-control" || voice["device"] != "/dev/trunksystem-voice" || control["qa_unedited"] != "retain modem field" {
					t.Fatal("modem identity, role assignment, or unedited field changed")
				}
				oldConfig, _ := webConfig(before)
				if !reflect.DeepEqual(saved["channels"], oldConfig["channels"]) {
					t.Fatal("editing modem text changed channel assignments")
				}
			}
			// Loading an editor and following Cancel performs only reads.
			editor := earlyWebGet(t, client, pair.web.URL+test.route, 200)
			if !bytes.Contains(editor, []byte("Cancel")) {
				t.Fatal("ordinary Cancel link missing")
			}
			cancelRoute := strings.Split(test.route, "?")[0]
			cancelRoute = strings.TrimSuffix(cancelRoute, "/edit")
			earlyWebGet(t, client, pair.web.URL+cancelRoute, 200)
			afterCancel, _ := pair.service.Store.Config()
			if !bytes.Equal(savedRaw, afterCancel) {
				t.Fatal("Cancel changed saved configuration")
			}
			bad := earlyWebForm(t, editor, test.field)
			bad.Del("csrf")
			earlyWebPost(t, client, pair.web.URL+test.route, bad, pair.web.URL, 403)
			bad.Set("csrf", values.Get("csrf"))
			earlyWebPost(t, client, pair.web.URL+test.route, bad, "https://untrusted.invalid", 403)
			afterRejected, _ := pair.service.Store.Config()
			if !bytes.Equal(savedRaw, afterRejected) {
				t.Fatal("rejected form changed configuration")
			}
		})
	}
}

func TestEarlyWebCollectionCRUDAndServiceOwnership(t *testing.T) {
	c := integrationNewCluster(t)
	client := earlyWebLogin(t, c.system)
	original, _ := c.system.service.Store.Config()
	values := earlyWebForm(t, earlyWebGet(t, client, c.system.web.URL+"/radios/edit", 200), "rid")
	values.Set("rid", "99999999")
	values.Set("alias", "  unready  ")
	values.Set("kind", "future")
	values.Set("enabled", "true")
	earlyWebPost(t, client, c.system.web.URL+"/radios/edit", values, c.system.web.URL, 303)
	var saved SystemConfig
	raw, _ := c.system.service.Store.Config()
	if err := json.Unmarshal(raw, &saved); err != nil {
		t.Fatal(err)
	}
	if len(saved.Radios) != 2 || saved.Radios[1].RID != 99999999 || saved.Radios[1].Kind != "future" || saved.Radios[1].Alias != "  unready  " {
		t.Fatal("create changed entered radio settings")
	}
	values = earlyWebForm(t, earlyWebGet(t, client, c.system.web.URL+"/radios/edit?row=1", 200), "rid")
	values.Set("alias", "renamed")
	earlyWebPost(t, client, c.system.web.URL+"/radios/edit?row=1", values, c.system.web.URL, 303)
	raw, _ = c.system.service.Store.Config()
	if err := json.Unmarshal(raw, &saved); err != nil || saved.Radios[1].Alias != "renamed" {
		t.Fatal("edit was not saved", err)
	}
	values.Set("action", "delete")
	earlyWebPost(t, client, c.system.web.URL+"/radios?row=1", values, c.system.web.URL, 303)
	raw, _ = c.system.service.Store.Config()
	initialConfig, _ := webConfig(original)
	finalConfig, _ := webConfig(raw)
	if !reflect.DeepEqual(initialConfig, finalConfig) {
		t.Fatal("create/edit/delete changed an unrelated record")
	}
	for _, pair := range []*integrationService{c.zone, c.site, c.dispatch} {
		other := earlyWebLogin(t, pair)
		earlyWebGet(t, other, pair.web.URL+"/radios/edit", 404)
		earlyWebGet(t, other, pair.web.URL+"/users/edit", 404)
	}
}

func TestEarlyWebBrowserFixture(t *testing.T) {
	if os.Getenv("OPEN25_ADMIN_BROWSER_TEST") != "1" {
		t.Skip("set OPEN25_ADMIN_BROWSER_TEST=1 for local four-role browser checks")
	}
	repo, err := filepath.Abs("../..")
	if err != nil {
		t.Fatal(err)
	}
	evidence := filepath.Join(repo, "build", "admin-ui-test", "browser")
	if target := os.Getenv("OPEN25_ADMIN_BROWSER_EVIDENCE"); target != "" {
		evidence = target
	}
	c := integrationNewCluster(t)
	var browserZone ZoneConfig
	if err := c.zone.service.owned(&browserZone); err != nil {
		t.Fatal(err)
	}
	zoneCredentials, err := readCredentials(c.zone.service.Config().CredentialFile)
	if err != nil {
		t.Fatal(err)
	}
	for _, pair := range []*integrationService{c.site, c.otherSite} {
		id := pair.service.Config().InstanceID
		for i := range browserZone.Sites {
			if browserZone.Sites[i].ID == id {
				browserZone.Sites[i].Endpoint = pair.server.URL
			}
		}
		credentials, err := readCredentials(pair.service.Config().CredentialFile)
		if err != nil {
			t.Fatal(err)
		}
		credentials.Peers[zoneCredentials.OutboundID] = PeerCredential{Role: RoleZone, InstanceID: zoneCredentials.OutboundID, Token: zoneCredentials.OutboundToken}
		if err := WritePrivateJSON(pair.service.Config().CredentialFile, credentials); err != nil {
			t.Fatal(err)
		}
	}
	if err := c.zone.service.Store.SaveConfig(storeDoc(t, browserZone)); err != nil {
		t.Fatal(err)
	}
	user := fixtureUser()
	user.Profile.Name = "Daily operations"
	user.Profile.Zones = []ProfileZone{{Name: "LAB", Talkgroups: []int{1001, 1002}}}
	user.Profile.Receive = []ReceiveSetting{{TalkgroupID: 1001, Volume: 0.753, Pan: 0}}
	if err := c.system.service.Store.SaveUser(user.ID, user, "browser-fixture-only"); err != nil {
		t.Fatal(err)
	}
	integrationSyncAll(t, c)
	// Seed stored receipts only: browser checks never submit a radio command.
	now := time.Now().UTC().Truncate(time.Second)
	for _, pair := range []*integrationService{c.site, c.otherSite} {
		id := pair.service.Config().InstanceID
		if err := pair.service.Store.mutateRadioCommands(func(rows *[]radioCommandReceipt) error {
			*rows = []radioCommandReceipt{
				{ID: "ui-radio-check-timeout-62a27936c94ef18b04cf960caa74a6-" + id, State: "timed_out", Sent: true, SiteID: id, RadioID: 10001, Action: "radio_check", ActorUsername: "administrator", CreatedAt: now.Add(-3 * time.Minute), AttemptedAt: now.Add(-3 * time.Minute), SentAt: now.Add(-3 * time.Minute), ExpiresAt: now.Add(-3*time.Minute + 15*time.Second), Message: "No matching subscriber response was observed before this request expired."},
				{ID: "ui-radio-check-acknowledged-7043295c41564958ba118c2c38a7f949-" + id, State: "acknowledged", Sent: true, SiteID: id, RadioID: 10002, Action: "radio_check", ActorUsername: "administrator", CreatedAt: now.Add(-2 * time.Minute), AttemptedAt: now.Add(-2 * time.Minute), SentAt: now.Add(-2 * time.Minute), ExpiresAt: now.Add(-2*time.Minute + 15*time.Second), ResponseAt: now.Add(-2*time.Minute + time.Second), ResponseKind: "radio_check_ack", ResponseEventID: "6ae4da8757f24ce79a118c2c38a7f949", Message: "A matching radio response was observed."},
				{ID: "ui-registration-demand-sent-62a27936c94ef18b04cf960caa74a6-" + id, State: "sent", Sent: true, AwaitingAcknowledgment: true, SiteID: id, RadioID: 10003, Action: "registration_demand", ActorUsername: "administrator", CreatedAt: now.Add(-time.Minute), AttemptedAt: now.Add(-time.Minute), SentAt: now.Add(-time.Minute), ExpiresAt: now.Add(10 * time.Minute), Message: "Request sent; awaiting a matching radio response."},
			}
			return nil
		}); err != nil {
			t.Fatal(err)
		}
	}
	type roleFixture struct {
		Role     Role   `json:"role"`
		Instance string `json:"instance"`
		Origin   string `json:"origin"`
		Username string `json:"username"`
		Password string `json:"password"`
	}
	fixture := struct {
		Roles []roleFixture `json:"roles"`
	}{Roles: []roleFixture{}}
	before := map[*integrationService]json.RawMessage{}
	for _, pair := range []*integrationService{c.system, c.zone, c.site, c.otherSite, c.dispatch} {
		pair.service.assets = frameworkui.Assets()
		fixture.Roles = append(fixture.Roles, roleFixture{pair.service.Config().Role, pair.service.Config().InstanceID, pair.web.URL, "administrator", integrationPassword})
		before[pair], _ = pair.service.Store.Config()
	}
	fixturePath := filepath.Join(evidence, "local-browser-fixture.private.json")
	if err = WritePrivateJSON(fixturePath, fixture); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.Remove(fixturePath) })
	ctx, cancel := context.WithTimeout(t.Context(), 8*time.Minute)
	defer cancel()
	command := exec.CommandContext(ctx, "node", filepath.Join(repo, "tools", "framework", "admin-network-tree-check.mjs"), fixturePath, filepath.Join(evidence, "screenshots"))
	output, err := command.CombinedOutput()
	t.Logf("Browser evidence: %s", output)
	if err != nil {
		t.Fatal(err)
	}
	for pair, raw := range before {
		profile, err := pair.service.smtpProfile()
		if err != nil {
			t.Fatal(err)
		}
		if profile.Host != "mail.example.invalid" || profile.Port != 2525 || profile.Authentication != "password" || profile.Username != "fixture-only-user" || profile.SenderName != "SMTP browser fixture" || profile.Password != "fixture-only-smtp-"+pair.service.Config().InstanceID || profile.Enabled || profile.Inherit != (pair.service.Config().Role != RoleSystem) {
			t.Fatalf("SMTP browser saves or blank-password preservation failed for %s", pair.service.Config().InstanceID)
		}
		deliveries, err := pair.service.Store.smtpDeliveries()
		if err != nil || len(deliveries) != 0 {
			t.Fatalf("SMTP browser Save queued email for %s", pair.service.Config().InstanceID)
		}
		after, _ := pair.service.Store.Config()
		if !bytes.Equal(raw, after) {
			t.Fatalf("browser read/cancel checks changed %s configuration", pair.service.Config().Role)
		}
	}
}
