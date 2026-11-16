package framework

import (
	"bytes"
	"encoding/json"
	"errors"
	"net/http"
	"open25.local/open25-fne/internal/frameworkui"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"open25.local/open25-fne/internal/radio"
)

func TestAdminGeneratedChannelIdentityIsHiddenStableAndIndependentOnClone(t *testing.T) {
	c := integrationNewCluster(t)
	client := earlyWebLogin(t, c.site)
	page := earlyWebGet(t, client, c.site.web.URL+"/channels/edit", 200)
	form := earlyWebForm(t, page, "name")
	id := form.Get("id")
	if id == "" || bytes.Contains(page, []byte("Record ID</label>")) || form.Get("power_percent") != "95" {
		t.Fatal("new channel did not receive managed identity/defaults", form)
	}
	form.Set("id", "attempted-visible-identity")
	form.Set("name", "Second voice")
	form.Set("identifier", "2")
	form.Set("channel_no", "61")
	form.Set("modem_id", "modem-1")
	earlyWebPost(t, client, c.site.web.URL+"/channels/edit", form, c.site.web.URL, http.StatusSeeOther)
	raw, _ := c.site.service.Store.Config()
	config, _ := webConfig(raw)
	rows := config["channels"].([]any)
	saved := rows[len(rows)-1].(map[string]any)
	earlyWebPost(t, client, c.site.web.URL+"/channels/edit", form, c.site.web.URL, http.StatusSeeOther)
	retried, _ := c.site.service.Store.Config()
	retryConfig, _ := webConfig(retried)
	if len(retryConfig["channels"].([]any)) != len(rows) {
		t.Fatal("retry duplicated an internal entity")
	}
	if saved["id"] != id {
		t.Fatal("internal identity was altered by an editable field", saved)
	}
	cloned := earlyWebForm(t, earlyWebGet(t, client, c.site.web.URL+"/channels/edit?clone=1", 200), "name")
	if cloned.Get("id") == "" || cloned.Get("id") == id {
		t.Fatal("clone retained its source's internal identity")
	}
	edit := earlyWebForm(t, earlyWebGet(t, client, c.site.web.URL+"/channels/edit?row=1", 200), "name")
	edit.Set("id", "attempted-rename")
	edit.Set("name", "Renamed voice")
	earlyWebPost(t, client, c.site.web.URL+"/channels/edit?row=1", edit, c.site.web.URL, http.StatusSeeOther)
	raw, _ = c.site.service.Store.Config()
	config, _ = webConfig(raw)
	if config["channels"].([]any)[1].(map[string]any)["id"] != id {
		t.Fatal("editing changed the stable key")
	}
}

func TestAdminNamedReferencesHideOpaqueKeys(t *testing.T) {
	c := integrationNewCluster(t)
	client := earlyWebLogin(t, c.site)
	page := earlyWebGet(t, client, c.site.web.URL+"/channels/edit?row=0", 200)
	if !bytes.Contains(page, []byte("data-relation-display>Control modem</output>")) || !bytes.Contains(page, []byte(`type=hidden name="modem_id"`)) {
		t.Fatal("modem relationship is not a named control")
	}
	var site SiteConfig
	if err := c.site.service.owned(&site); err != nil {
		t.Fatal(err)
	}
	commands := earlyWebGet(t, client, c.site.web.URL+"/radio-commands", 200)
	if !bytes.Contains(commands, []byte(`type=hidden name="site_id"`)) {
		t.Fatal("radio command requires an opaque Site key")
	}
	history := earlyWebGet(t, earlyWebLogin(t, c.system), c.system.web.URL+"/history", 200)
	if !bytes.Contains(history, []byte(`type=hidden name="site_id"`)) {
		t.Fatal("history filter requires an opaque Site key")
	}
	projected := c.site.service.adminNamedPresentation(map[string]any{"modem_id": site.Modems[0].ID}).(map[string]any)
	if projected["modem_id"] != "Control modem" {
		t.Fatal("read-only projection exposed a key", projected)
	}
}

func TestRadioReadinessHelpAndIdleIdentities(t *testing.T) {
	var out bytes.Buffer
	webRadioObservations(&out, &radio.Status{Channels: []radio.ChannelStatus{{Name: "Voice", State: "ready", Ready: true}}})
	html := out.String()
	for _, want := range []string{"Starting:", "Fault:", "radio worker startup", ">Yes<"} {
		if !strings.Contains(html, want) {
			t.Fatal("missing readiness explanation", want)
		}
	}
	if strings.Contains(html, "00000000") || strings.Contains(html, "000000 hex") {
		t.Fatal("idle channel rendered an active identity")
	}
	out.Reset()
	webDataField(&out, "radio_id", 10001)
	if !strings.Contains(out.String(), `class=ts-numeric>00010001 / 002711 hex`) {
		t.Fatal("active identity lost its monospace/padded representation", out.String())
	}
}

func TestConfiguredAwaitingRestartIsAnObservationOnly(t *testing.T) {
	s, runtime := operationConfiguredFixture(t, nil)
	profile, err := s.profileRadio()
	if err != nil {
		t.Fatal(err)
	}
	runtime.status.State = "running"
	runtime.status.AppliedRFConfiguration = radio.RFConfigurationSignature(profile)
	var out bytes.Buffer
	s.webRFConfigurationNotice(&out)
	if out.Len() != 0 {
		t.Fatal("matching configuration reported pending")
	}
	var site SiteConfig
	if err := s.owned(&site); err != nil {
		t.Fatal(err)
	}
	site.Channels[0].PowerPercent = 90
	if err := s.Store.SaveConfig(storeDoc(t, site)); err != nil {
		t.Fatal(err)
	}
	s.webRFConfigurationNotice(&out)
	if !strings.Contains(out.String(), "Configured but awaiting restart") || runtime.starts != 0 || runtime.stops != 0 {
		t.Fatal("pending presentation altered radio operation", out.String())
	}
}

func TestAdminSimplificationBrowser(t *testing.T) {
	script := os.Getenv("OPEN25_ADMIN_SIMPLIFICATION_BROWSER")
	if script == "" {
		t.Skip("set OPEN25_ADMIN_SIMPLIFICATION_BROWSER for disposable Chrome checks")
	}
	c := integrationNewCluster(t)
	for _, pair := range []*integrationService{c.system, c.zone, c.site, c.otherSite, c.dispatch} {
		pair.service.assets = frameworkui.Assets()
	}
	for _, pair := range []*integrationService{c.site, c.otherSite} {
		pair.service.operationCancel()
		pair.service.operationWG.Wait()
		profile, err := pair.service.profileRadio()
		if err != nil {
			t.Fatal(err)
		}
		ch := profile.Site.Channels[0]
		pair.service.radio = &operationRadio{applications: make(chan policyApplication, 128), applyError: errors.New("RF settings changed; restart needed"), status: radio.Status{State: "running", Available: true, AppliedRFConfiguration: radio.RFConfigurationSignature(profile), Channels: []radio.ChannelStatus{{ID: ch.ID, Name: ch.Name, Role: ch.Role, Ready: true, State: "ready"}}}}
	}
	fixture := filepath.Join(t.TempDir(), "fixture.json")
	raw, _ := json.Marshal(map[string]any{"system": c.system.web.URL, "zone": c.zone.web.URL, "sites": []string{c.site.web.URL, c.otherSite.web.URL}, "password": integrationPassword})
	if err := os.WriteFile(fixture, raw, 0600); err != nil {
		t.Fatal(err)
	}
	command := exec.Command("node", script, fixture)
	command.Stdout = os.Stdout
	command.Stderr = os.Stderr
	if err := command.Run(); err != nil {
		t.Fatal("administration browser", err)
	}
}
