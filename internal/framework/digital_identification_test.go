package framework

import (
	"bytes"
	"encoding/json"
	"net/http"
	"testing"
)

func TestDigitalIdentificationSaveOwnsOnlyCurrentSettings(t *testing.T) {
	c := integrationNewCluster(t)
	site := c.site
	raw, err := site.service.Store.Config()
	if err != nil {
		t.Fatal(err)
	}
	var document map[string]any
	if err := json.Unmarshal(raw, &document); err != nil {
		t.Fatal(err)
	}
	identification := document["identification"].(map[string]any)
	// Old persisted settings must be inert; only explicit Save replaces this record.
	identification["mode"], identification["wpm"], identification["quiet_seconds"] = "ham", 20, 60
	old, err := json.Marshal(document)
	if err != nil {
		t.Fatal(err)
	}
	if err := site.service.Store.SaveConfig(old); err != nil {
		t.Fatal(err)
	}
	client := earlyWebLogin(t, site)
	page := earlyWebGet(t, client, site.web.URL+"/identification", http.StatusOK)
	if bytes.Contains(page, []byte(`value="harris"`)) {
		t.Fatal("unimplemented Harris BSI option rendered")
	}
	for _, name := range []string{"mode", "wpm", "quiet_seconds"} {
		if bytes.Contains(page, []byte(`name="`+name+`"`)) {
			t.Fatal("retired CW setting rendered", name)
		}
	}
	afterRead, _ := site.service.Store.Config()
	if !bytes.Equal(old, afterRead) {
		t.Fatal("page access changed saved programming")
	}
	form := earlyWebForm(t, page, "callsign")
	form.Set("callsign", "W1TEST")
	form.Set("enabled", "false")
	form.Set("bsi_profile", "motorola")
	form.Set("mode", "ham")
	form.Set("wpm", "29")
	form.Set("quiet_seconds", "41")
	earlyWebPost(t, client, site.web.URL+"/identification", form, site.web.URL, http.StatusSeeOther)
	saved, err := site.service.Store.Config()
	if err != nil {
		t.Fatal(err)
	}
	var current map[string]any
	if err := json.Unmarshal(saved, &current); err != nil {
		t.Fatal(err)
	}
	got := current["identification"].(map[string]any)
	if len(got) != 3 || got["callsign"] != "W1TEST" || got["bsi_profile"] != "motorola" || got["enabled"] != false {
		t.Fatal("Save did not write only current digital BSI settings", got)
	}
	delete(document, "identification")
	delete(current, "identification")
	wantJSON, _ := json.Marshal(document)
	gotJSON, _ := json.Marshal(current)
	if !bytes.Equal(wantJSON, gotJSON) {
		t.Fatal("identification Save changed unrelated Site programming")
	}
}

func TestDigitalIdentificationCallsignOwnedByEachSite(t *testing.T) {
	c := integrationNewCluster(t)
	parentConfig := map[*integrationService][]byte{}
	for _, parent := range []*integrationService{c.system, c.zone} {
		raw, err := parent.service.Store.Config()
		if err != nil {
			t.Fatal(err)
		}
		parentConfig[parent] = raw
	}
	for _, section := range webSections() {
		for _, field := range section.Fields {
			if field.Name == "callsign" || field.Name == "bsi_profile" {
				if section.Role != RoleSite {
					t.Fatal("BSI editor outside Site", section.Path)
				}
			}
		}
	}
	sites := []*integrationService{c.site, c.otherSite}
	callsigns := []string{"W1TEST", "W2TEST"}
	for i, site := range sites {
		client := earlyWebLogin(t, site)
		page := earlyWebGet(t, client, site.web.URL+"/identification", http.StatusOK)
		form := earlyWebForm(t, page, "callsign")
		form.Set("callsign", callsigns[i])
		form.Set("bsi_profile", "motorola")
		form.Set("enabled", "true")
		earlyWebPost(t, client, site.web.URL+"/identification", form, site.web.URL, http.StatusSeeOther)
	}
	for i, site := range sites {
		var saved SiteConfig
		if err := site.service.owned(&saved); err != nil {
			t.Fatal(err)
		}
		if saved.Identification.Callsign != callsigns[i] {
			t.Fatal("cross-Site callsign overwrite", saved.Identification)
		}
	}
	for parent, before := range parentConfig {
		after, err := parent.service.Store.Config()
		if err != nil {
			t.Fatal(err)
		}
		if !bytes.Equal(before, after) {
			t.Fatal("Site BSI Save changed parent programming")
		}
	}
}
