package framework

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/url"
	"reflect"
	"strings"
	"testing"
)

func TestAdminGroupsSaveTypedNamesAndOfferExistingNames(t *testing.T) {
	c := integrationNewCluster(t)
	client := earlyWebLogin(t, c.system)
	for _, kind := range []string{"radios", "talkgroups"} {
		address := c.system.web.URL + "/" + kind + "/edit?row=0"
		page := earlyWebGet(t, client, address, 200)
		if !bytes.Contains(page, []byte(`data-record-combo="`+kind+`-groups"`)) {
			t.Fatal("group is not an editable combo", kind)
		}
		form := earlyWebForm(t, page, "group")
		form.Set("group", "  Fleet <West>  ")
		earlyWebPost(t, client, address, form, c.system.web.URL, 303)
		raw, _ := c.system.service.Store.Config()
		config, _ := webConfig(raw)
		if config[kind].([]any)[0].(map[string]any)["group"] != "  Fleet <West>  " {
			t.Fatal("typed group name was altered")
		}
		result := earlyWebGet(t, client, c.system.web.URL+"/chooser?kind="+kind+"-groups", 200)
		var choices adminChoicePage
		if json.Unmarshal(result, &choices) != nil || len(choices.Items) != 1 || choices.Items[0].ID != "  Fleet <West>  " {
			t.Fatal("saved group not offered", kind, string(result))
		}
	}
	earlyWebGet(t, earlyWebLogin(t, c.site), c.site.web.URL+"/radios/bulk", http.StatusNotFound)
}

func TestAdminBulkChangesOnlyCheckedFieldsAndSelectedRecords(t *testing.T) {
	c := integrationNewCluster(t)
	s := c.system.service
	system := storeFixtureSystem()
	system.Radios = append(system.Radios, Radio{RID: 10002, Alias: "Second", Enabled: true, Kind: "console"}, Radio{RID: 10003, Alias: "Third", Enabled: true, Kind: "subscriber"})
	system.Talkgroups = append(system.Talkgroups, Talkgroup{TGID: 1002, Alias: "Second group", Enabled: true, AdmissionMode: "both", TransmitMode: "selectable"})
	if err := s.Store.SaveConfig(storeDoc(t, system)); err != nil {
		t.Fatal(err)
	}
	client := earlyWebLogin(t, c.system)
	before, _ := s.Store.Config()
	start := earlyWebGet(t, client, c.system.web.URL+"/radios", 200)
	selection := earlyWebForm(t, start, "selection")
	selection.Set("selection", "10001, 10002, 999999")
	selection.Set("action", "edit")
	page := adminFormResponse(t, client, c.system.web.URL+"/radios/bulk", c.system.web.URL, selection, 200)
	still, _ := s.Store.Config()
	if !bytes.Equal(before, still) {
		t.Fatal("opening bulk editor wrote settings")
	}
	form := earlyWebForm(t, page, "group")
	form["apply"] = []string{"group", "enabled"}
	form.Set("group", "Fire")
	form.Del("enabled")
	form.Set("alias", "Ignored text")
	form.Set("rid", "1")
	form.Set("action", "save")
	earlyWebPost(t, client, c.system.web.URL+"/radios/bulk", form, c.system.web.URL, 303)
	raw, _ := s.Store.Config()
	var got SystemConfig
	_ = json.Unmarshal(raw, &got)
	want := system
	want.Radios = append([]Radio(nil), system.Radios...)
	for i := 0; i < 2; i++ {
		want.Radios[i].Group = "Fire"
		want.Radios[i].Enabled = false
	}
	if !reflect.DeepEqual(want, got) {
		t.Fatal("bulk edit changed an unchecked field or unselected record", got.Radios)
	}
	// Grouping a talkgroup does not rewrite its call/access/encryption settings.
	page = earlyWebGet(t, client, c.system.web.URL+"/talkgroups/bulk?selection=1002", 200)
	form = earlyWebForm(t, page, "group")
	form["apply"] = []string{"group"}
	form.Set("group", "Works")
	form.Set("action", "save")
	earlyWebPost(t, client, c.system.web.URL+"/talkgroups/bulk", form, c.system.web.URL, 303)
	raw, _ = s.Store.Config()
	_ = json.Unmarshal(raw, &got)
	want.Talkgroups = append([]Talkgroup(nil), system.Talkgroups...)
	want.Talkgroups[1].Group = "Works"
	if !reflect.DeepEqual(want, got) {
		t.Fatal("talkgroup grouping changed runtime policy")
	}
}

func TestAdminCloneIsUnsavedCopyAndDuplicateIdentityRemainsEditable(t *testing.T) {
	c := integrationNewCluster(t)
	s := c.system.service
	client := earlyWebLogin(t, c.system)
	before, _ := s.Store.Config()
	page := earlyWebGet(t, client, c.system.web.URL+"/radios/edit?clone=0", 200)
	form := earlyWebForm(t, page, "rid")
	after, _ := s.Store.Config()
	if !bytes.Equal(before, after) || form.Has("resource") || form.Get("rid") != "10001" || form.Get("alias") != "Portable" {
		t.Fatal("clone changed configuration or automatically changed copied values")
	}
	// Deliberately retain the duplicate identity; Save must not reject it.
	form.Set("alias", "Copied radio")
	earlyWebPost(t, client, c.system.web.URL+"/radios/edit?clone=0", form, c.system.web.URL, 303)
	page = earlyWebGet(t, client, c.system.web.URL+"/radios/edit?row=1", 200)
	form = earlyWebForm(t, page, "alias")
	form.Set("group", "Copy group")
	earlyWebPost(t, client, c.system.web.URL+"/radios/edit?row=1", form, c.system.web.URL, 303)
	raw, _ := s.Store.Config()
	var got SystemConfig
	_ = json.Unmarshal(raw, &got)
	if len(got.Radios) != 2 || got.Radios[0].Alias != "Portable" || got.Radios[0].Group != "" || got.Radios[1].Alias != "Copied radio" || got.Radios[1].Group != "Copy group" {
		t.Fatal("cloned duplicate edit targeted another record", got.Radios)
	}
}

func TestAdminBulkSelectionPageHasBoundedRowsAndCloneLinks(t *testing.T) {
	c := integrationNewCluster(t)
	client := earlyWebLogin(t, c.system)
	page := earlyWebGet(t, client, c.system.web.URL+"/radios", 200)
	for _, want := range []string{"data-bulk-page", "data-bulk-record", `data-chooser-kind="radios"`, "/radios/edit?clone=0", "Edit selected"} {
		if !strings.Contains(string(page), want) {
			t.Fatal("missing bulk/clone affordance", want)
		}
	}
	// Ordinary request parsing and explicit Save apply; a foreign role cannot bulk-write.
	earlyWebPost(t, client, c.system.web.URL+"/channels/bulk", url.Values{"csrf": {earlyWebForm(t, page, "selection").Get("csrf")}, "action": {"save"}}, c.system.web.URL, 404)
}
