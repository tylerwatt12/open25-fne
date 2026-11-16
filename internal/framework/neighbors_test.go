package framework

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"testing"
	"time"

	"open25.local/open25-fne/internal/frameworkui"
	"open25.local/open25-fne/internal/radio"
)

type neighborReportRadio struct {
	operationRadio
	neighbors []radio.NeighborSite
}

func (r *neighborReportRadio) ApplyNeighborSites(_ context.Context, sites []radio.NeighborSite) error {
	r.neighbors = sites
	return nil
}

func TestNeighborAnnouncementsSiteEditorAndZoneExchange(t *testing.T) {
	c := integrationNewCluster(t)
	integrationSyncAll(t, c)
	var zone ZoneConfig
	var system SystemConfig
	if err := c.zone.service.owned(&zone); err != nil {
		t.Fatal(err)
	}
	if err := c.system.service.owned(&system); err != nil {
		t.Fatal(err)
	}
	source, recipient := zone.Sites[0], zone.Sites[1]
	a := &neighborReportRadio{}
	a.status = radio.Status{State: "running", Mode: "hardware", SiteID: source.ID, AdjacentSite: &radio.AdjacentSite{
		WACN: system.WACN, SystemID: system.SystemID, RFSSID: zone.RFSSID, SiteID: source.SiteID,
		LRA: source.LRA, Identifier: 2, ChannelNo: 13, TXHz: 434200000, RXHz: 439200000, ServiceClass: 0x60,
	}}
	b := &neighborReportRadio{}
	// The normal integration fixture's background operations would otherwise
	// drive the fake radio while we explicitly synchronize these two Sites.
	for _, node := range []*integrationService{c.site, c.otherSite} {
		node.service.operationCancel()
		node.service.operationWG.Wait()
	}
	c.site.service.radio, c.otherSite.service.radio = a, b

	ctx := context.Background()
	if err := c.site.service.SyncOnce(ctx); err != nil {
		t.Fatal(err)
	}
	if err := c.otherSite.service.SyncOnce(ctx); err != nil {
		t.Fatal(err)
	}
	if len(b.neighbors) != 1 || b.neighbors[0].ID != source.ID || b.neighbors[0].Control == nil ||
		*b.neighbors[0].Control != *a.status.AdjacentSite || !b.neighbors[0].ValidUntil.After(time.Now()) {
		t.Fatalf("reported control did not reach the same-Zone Site: %#v", b.neighbors)
	}
	if len(a.neighbors) != 1 || a.neighbors[0].ID != recipient.ID || a.neighbors[0].Control != nil {
		t.Fatalf("the unavailable Site must remain selectable without an RF announcement: %#v", a.neighbors)
	}
	a.status.AdjacentSite.ChannelNo = 22
	if err := c.site.service.SyncOnce(ctx); err != nil {
		t.Fatal(err)
	}
	if err := c.otherSite.service.SyncOnce(ctx); err != nil {
		t.Fatal(err)
	}
	if b.neighbors[0].Control == nil || b.neighbors[0].Control.ChannelNo != 22 {
		t.Fatal("control-channel change did not reach the other Site")
	}
	a.status.State = "stopped"
	if err := c.site.service.SyncOnce(ctx); err != nil {
		t.Fatal(err)
	}
	if err := c.otherSite.service.SyncOnce(ctx); err != nil {
		t.Fatal(err)
	}
	if b.neighbors[0].Control != nil {
		t.Fatal("stopped Site still had a control announcement")
	}

	client := earlyWebLogin(t, c.otherSite)
	address := c.otherSite.web.URL + "/neighbor-announcements"
	before, _ := c.otherSite.service.Store.Config()
	form := earlyWebForm(t, earlyWebGet(t, client, address, http.StatusOK), "neighbor_mode")
	if form.Get("neighbor_mode") != "all" {
		t.Fatalf("default was %q", form.Get("neighbor_mode"))
	}
	afterGet, _ := c.otherSite.service.Store.Config()
	if !bytes.Equal(before, afterGet) {
		t.Fatal("opening the editor saved the default")
	}
	var choices adminChoicePage
	body := earlyWebGet(t, client, c.otherSite.web.URL+"/chooser?kind=neighbor-sites", http.StatusOK)
	if err := json.Unmarshal(body, &choices); err != nil || len(choices.Items) != 1 || choices.Items[0].ID != source.ID || choices.Items[0].Name != source.Name {
		t.Fatalf("unexpected same-Zone chooser: %s, %v", body, err)
	}
	form.Set("neighbor_mode", "selected")
	form.Set("neighbor_site_ids", source.ID)
	earlyWebPost(t, client, address, form, c.otherSite.web.URL, http.StatusSeeOther)
	var saved, untouched SiteConfig
	if err := c.otherSite.service.owned(&saved); err != nil {
		t.Fatal(err)
	}
	if err := c.site.service.owned(&untouched); err != nil {
		t.Fatal(err)
	}
	if saved.NeighborMode != "selected" || !slices.Equal(saved.NeighborSiteIDs, []string{source.ID}) ||
		untouched.NeighborMode != "" || b.starts != 0 || b.stops != 0 {
		t.Fatalf("unexpected saved neighbor policy: %+v", saved.NeighborSiteIDs)
	}
	earlyWebGet(t, earlyWebLogin(t, c.zone), c.zone.web.URL+"/neighbor-announcements", http.StatusNotFound)
}

func TestNeighborAnnouncementsBrowser(t *testing.T) {
	if os.Getenv("OPEN25_ADMIN_BROWSER_TEST") != "1" {
		t.Skip("set OPEN25_ADMIN_BROWSER_TEST=1 for the local Site browser check")
	}
	c := integrationNewCluster(t)
	integrationSyncAll(t, c)
	type portal struct {
		Origin     string `json:"origin"`
		Password   string `json:"password"`
		NeighborID string `json:"neighborId"`
	}
	for _, site := range []*integrationService{c.site, c.otherSite} {
		site.service.assets = frameworkui.Assets()
	}
	dir := t.TempDir()
	fixture := filepath.Join(dir, "browser.private.json")
	if err := WritePrivateJSON(fixture, []portal{
		{c.site.web.URL, integrationPassword, c.otherSite.service.Config().InstanceID},
		{c.otherSite.web.URL, integrationPassword, c.site.service.Config().InstanceID},
	}); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(t.Context(), time.Minute)
	defer cancel()
	command := exec.CommandContext(ctx, "node", "../../tools/framework/neighbor-announcements-ui-check.mjs", fixture, dir)
	if output, err := command.CombinedOutput(); err != nil {
		t.Fatalf("%v: %s", err, output)
	}
}
