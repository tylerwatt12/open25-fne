package framework

import (
	"bytes"
	"encoding/json"
	"net/http"
	"reflect"
	"strconv"
	"testing"
)

func TestLocationRegistrationAreaZoneFormAndScopedSitePropagation(t *testing.T) {
	c := integrationNewCluster(t)
	client := earlyWebLogin(t, c.zone)
	address := c.zone.web.URL + "/sites/edit?row=0"
	body := earlyWebGet(t, client, address, http.StatusOK)
	for _, want := range []string{"Location Registration Area (LRA)", `name="lra"`, `name="lra__hex"`, "data-number-decimal", "data-number-hex"} {
		if !bytes.Contains(body, []byte(want)) {
			t.Fatal("LRA editor is missing its ordinary number pair/help", want)
		}
	}
	beforeRaw, err := c.zone.service.Store.Config()
	if err != nil {
		t.Fatal(err)
	}
	before, err := webConfig(beforeRaw)
	if err != nil {
		t.Fatal(err)
	}
	form := earlyWebForm(t, body, "lra")
	form.Set("lra__base", "hex")
	form.Set("lra__hex", "A5")
	webUnitsSubmit(t, client, address, c.zone.web.URL, form, http.StatusSeeOther)
	stored, err := c.zone.service.Store.Config()
	if err != nil {
		t.Fatal(err)
	}
	after, err := webConfig(stored)
	if err != nil {
		t.Fatal(err)
	}
	want := before["sites"].([]any)[0].(map[string]any)
	want["lra"] = json.Number("165")
	if !reflect.DeepEqual(before, after) {
		t.Fatal("LRA Save changed another configuration value")
	}
	zone := storeFixtureZone()
	if err := json.Unmarshal(stored, &zone); err != nil {
		t.Fatal(err)
	}
	zone.Sites[1].LRA = 61
	if err := c.zone.service.Store.SaveConfig(storeDoc(t, zone)); err != nil {
		t.Fatal(err)
	}
	integrationSyncAll(t, c)
	for _, target := range []struct {
		pair *integrationService
		want int
	}{{c.site, 165}, {c.otherSite, 61}} {
		profile, err := target.pair.service.profileRadio()
		if err != nil || len(profile.Zone.Sites) != 1 || profile.Zone.Sites[0].LRA != target.want {
			t.Fatalf("owning Zone's LRA did not reach only its Site: %#v, %v", profile.Zone.Sites, err)
		}
		earlyWebGet(t, earlyWebLogin(t, target.pair), target.pair.web.URL+"/sites/edit?row=0", http.StatusNotFound)
	}
	// Save preserves the entered number even when it cannot fit the CAI octet.
	// This does not assert an RF outcome for that misconfiguration.
	client = earlyWebLogin(t, c.zone)
	for _, lra := range []int{257, -1} {
		form = earlyWebForm(t, earlyWebGet(t, client, address, http.StatusOK), "lra")
		form.Set("lra__base", "decimal")
		form.Set("lra", strconv.Itoa(lra))
		webUnitsSubmit(t, client, address, c.zone.web.URL, form, http.StatusSeeOther)
		stored, err = c.zone.service.Store.Config()
		if err != nil {
			t.Fatal(err)
		}
		if err := json.Unmarshal(stored, &zone); err != nil || zone.Sites[0].LRA != lra || zone.Sites[1].LRA != 61 {
			t.Fatal("LRA Save restricted/corrected the entry or changed another Site", err)
		}
	}
}
