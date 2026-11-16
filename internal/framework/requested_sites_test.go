package framework

import (
	"bytes"
	"encoding/json"
	"open25.local/open25-fne/internal/frameworkui"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"testing"

	"open25.local/open25-fne/internal/dispatch"
	"open25.local/open25-fne/internal/routing"
)

func TestRequestedSitesZoneSelectionWithoutAffiliations(t *testing.T) {
	c := integrationNewCluster(t)
	system := storeFixtureSystem()
	system.Talkgroups[0].SiteAccess = "all"
	system.Talkgroups[0].RequestedSiteIDs = []string{"site-1"}
	if err := c.system.service.Store.SaveConfig(storeDoc(t, system)); err != nil {
		t.Fatal(err)
	}
	integrationSyncAll(t, c)
	zone := c.zone.service
	request := routing.Request{CallRequest: dispatch.CallRequest{ID: "requested", RadioID: 10002, Talkgroups: []int{1001}}, SourceKind: "application"}
	selection := zone.broker.targets(request)
	if len(selection.Targets) != 1 || selection.Targets[0].ID != "site-1" {
		t.Fatalf("requested Site selection with zero affiliations = %+v", selection)
	}
	if len(zone.rfAuthority.value.Load().owners) != 0 {
		t.Fatal("Site request created a radio affiliation")
	}
	system.Talkgroups[0].RequestedSiteIDs = []string{"site-1", "site-2"}
	if err := c.system.service.Store.SaveConfig(storeDoc(t, system)); err != nil {
		t.Fatal(err)
	}
	integrationSyncAll(t, c)
	selection = zone.broker.targets(request)
	if len(selection.Targets) != 2 {
		t.Fatalf("both requested Sites = %+v", selection)
	}
	system.Talkgroups[0].SiteAccess, system.Talkgroups[0].SiteIDs = "selected", []string{"site-2"}
	if err := c.system.service.Store.SaveConfig(storeDoc(t, system)); err != nil {
		t.Fatal(err)
	}
	integrationSyncAll(t, c)
	selection = zone.broker.targets(request)
	if len(selection.Targets) != 1 || selection.Targets[0].ID != "site-2" {
		t.Fatal("requested routing bypassed Site access", selection)
	}
	system.Talkgroups[0].RequestedSiteIDs = nil
	if err := c.system.service.Store.SaveConfig(storeDoc(t, system)); err != nil {
		t.Fatal(err)
	}
	integrationSyncAll(t, c)
	if selection = zone.broker.targets(request); len(selection.Targets) != 0 {
		t.Fatal("clearing requested Sites retained RF demand")
	}
}

func TestRequestedSitesAdminSaveAndBulkPreserveExplicitSelection(t *testing.T) {
	c := integrationNewCluster(t)
	client := earlyWebLogin(t, c.system)
	address := c.system.web.URL + "/talkgroups/edit?row=0"
	page := earlyWebGet(t, client, address, 200)
	if !bytes.Contains(page, []byte("Requested Sites")) || !bytes.Contains(page, []byte(`data-chooser-kind="sites"`)) {
		t.Fatal("Requested Sites and the shared site chooser are missing")
	}
	form := earlyWebForm(t, page, "requested_site_ids")
	form.Set("requested_site_ids", "site-1, site-2, unlisted-site")
	earlyWebPost(t, client, address, form, c.system.web.URL, 303)
	raw, _ := c.system.service.Store.Config()
	var system SystemConfig
	if err := json.Unmarshal(raw, &system); err != nil {
		t.Fatal(err)
	}
	if !slices.Equal(system.Talkgroups[0].RequestedSiteIDs, []string{"site-1", "site-2", "unlisted-site"}) {
		t.Fatal("Save altered the administrator's site selection")
	}
	page = earlyWebGet(t, client, c.system.web.URL+"/talkgroups/bulk?selection=1001", 200)
	form = earlyWebForm(t, page, "requested_site_ids")
	form["apply"] = []string{"requested_site_ids"}
	form.Set("requested_site_ids", "site-1")
	form.Set("action", "save")
	earlyWebPost(t, client, c.system.web.URL+"/talkgroups/bulk", form, c.system.web.URL, 303)
	raw, _ = c.system.service.Store.Config()
	_ = json.Unmarshal(raw, &system)
	if !slices.Equal(system.Talkgroups[0].RequestedSiteIDs, []string{"site-1"}) {
		t.Fatal("bulk Save did not apply Requested Sites")
	}
}

func TestRequestedSitesBrowser(t *testing.T) {
	script := os.Getenv("OPEN25_REQUESTED_BROWSER_SCRIPT")
	if script == "" {
		t.Skip("set the Requested Sites browser script for the disposable UI check")
	}
	c := integrationNewCluster(t)
	integrationSyncAll(t, c)
	c.system.service.assets = frameworkui.Assets()
	dir := t.TempDir()
	fixture := filepath.Join(dir, "fixture.json")
	if err := WritePrivateJSON(fixture, map[string]string{"origin": c.system.web.URL, "username": "administrator", "password": integrationPassword}); err != nil {
		t.Fatal(err)
	}
	command := exec.CommandContext(t.Context(), "node", script, fixture, os.Getenv("OPEN25_REQUESTED_BROWSER_OUTPUT"))
	if output, err := command.CombinedOutput(); err != nil {
		t.Fatalf("%v: %s", err, output)
	}
}
