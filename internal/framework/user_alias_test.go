package framework

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"reflect"
	"strings"
	"testing"
	"time"

	"open25.local/open25-fne/internal/radio"
)

func TestSystemUserAliasFormatAndExistingConfigurationPreservation(t *testing.T) {
	c := integrationNewCluster(t)
	client := earlyWebLogin(t, c.system)
	body := earlyWebGet(t, client, c.system.web.URL+"/radios/edit?row=0", http.StatusOK)
	if !strings.Contains(string(body), `maxlength=14 pattern="[\x20-\x7E]{1,14}"`) {
		t.Fatal("radio Name did not expose the requested native HTML format")
	}
	original, err := c.system.service.Store.Config()
	if err != nil {
		t.Fatal(err)
	}
	for _, name := range []string{"", "123456789012345", "Engine\t12", "Engine\n12", "Enginé 12", "Engine\x7f12"} {
		config, _ := webConfig(original)
		config["radios"].([]any)[0].(map[string]any)["alias"] = name
		if err = c.system.service.saveOwned(storeDoc(t, config)); err == nil {
			t.Fatalf("accepted an unencodable radio name %q", name)
		}
		after, _ := c.system.service.Store.Config()
		if !bytes.Equal(after, original) {
			t.Fatal("a rejected name changed saved configuration")
		}
	}
	config, _ := webConfig(original)
	name := `Engine, "12"`
	config["radios"].([]any)[0].(map[string]any)["alias"] = name
	config["future_setting"] = "preserve"
	if err = c.system.service.saveOwned(storeDoc(t, config)); err != nil {
		t.Fatal(err)
	}
	saved, _ := c.system.service.Store.Config()
	var result SystemConfig
	if err = json.Unmarshal(saved, &result); err != nil || result.Radios[0].Alias != name || !bytes.Contains(saved, []byte(`"future_setting":"preserve"`)) {
		t.Fatal("valid punctuation or unrelated settings were changed", err)
	}
	// Installation and reads do not normalize an old saved name. Runtime has a
	// separate bounded omission path; only an explicit System Save checks it.
	config["radios"].([]any)[0].(map[string]any)["alias"] = "An existing longer directory name"
	legacy := storeDoc(t, config)
	if err = c.system.service.Store.SaveConfig(legacy); err != nil {
		t.Fatal(err)
	}
	earlyWebGet(t, client, c.system.web.URL+"/radios/edit?row=0", http.StatusOK)
	unchanged, _ := c.system.service.Store.Config()
	if !bytes.Equal(unchanged, legacy) {
		t.Fatal("reading the existing entry rewrote its name")
	}
}

func TestRadioAliasFormErrorRetainsEnteredNameWithoutAddingRecord(t *testing.T) {
	c := integrationNewCluster(t)
	client := earlyWebLogin(t, c.system)
	original, _ := c.system.service.Store.Config()
	config, _ := webConfig(original)
	config["future_setting"] = "preserve through retry"
	config["radios"].([]any)[0].(map[string]any)["future_radio_setting"] = map[string]any{"untouched": true}
	if err := c.system.service.Store.SaveConfig(storeDoc(t, config)); err != nil {
		t.Fatal(err)
	}
	address := c.system.web.URL + "/radios/edit"
	form := earlyWebForm(t, earlyWebGet(t, client, address, http.StatusOK), "rid")
	form.Set("rid", "12345")
	form.Set("alias", "Name is much too long")
	original, _ = c.system.service.Store.Config()
	body := adminFormResponse(t, client, address, c.system.web.URL, form, http.StatusOK)
	retry := earlyWebForm(t, body, "rid")
	if retry.Get("alias") != form.Get("alias") || !bytes.Contains(body, []byte("1–14 printable ASCII")) {
		t.Fatal("the failed form did not retain the name and format explanation")
	}
	unchanged, _ := c.system.service.Store.Config()
	if !bytes.Equal(unchanged, original) {
		t.Fatal("the rejected form changed configuration")
	}
	table := userAliasRecordTable(t, earlyWebGet(t, client, c.system.web.URL+"/radios", http.StatusOK))
	if strings.Contains(table, form.Get("alias")) || strings.Count(table, "<tr>") != 2 || strings.Contains(table, "/radios/edit?row=1") {
		t.Fatal("the rejected Add appeared as a saved record or changed table indexes")
	}
	retry.Set("alias", "Engine 12")
	adminFormResponse(t, client, address, c.system.web.URL, retry, http.StatusSeeOther)
	saved, _ := c.system.service.Store.Config()
	var result SystemConfig
	if err := json.Unmarshal(saved, &result); err != nil {
		t.Fatal(err)
	}
	count := 0
	for _, entry := range result.Radios {
		if entry.RID == 12345 {
			count++
			if entry.Alias != "Engine 12" {
				t.Fatal("a rejected name survived the corrected retry")
			}
		}
	}
	if count != 1 {
		t.Fatalf("corrected Add created %d records", count)
	}
	savedConfig, _ := webConfig(saved)
	if savedConfig["future_setting"] != config["future_setting"] || !reflect.DeepEqual(savedConfig["radios"].([]any)[0].(map[string]any)["future_radio_setting"], config["radios"].([]any)[0].(map[string]any)["future_radio_setting"]) {
		t.Fatal("the corrected retry lost unrelated or per-radio unknown fields")
	}
}

func userAliasRecordTable(t *testing.T, body []byte) string {
	t.Helper()
	_, table, found := strings.Cut(string(body), "<table>")
	if !found {
		t.Fatal("radio record table missing")
	}
	table, _, found = strings.Cut(table, "</table>")
	if !found {
		t.Fatal("radio record table incomplete")
	}
	return table
}

func TestRadioAliasRejectedDeletePreservesTableAndRecordIndexes(t *testing.T) {
	c := integrationNewCluster(t)
	system := storeFixtureSystem()
	system.Radios[0].Alias = "An existing longer directory name"
	system.Radios = append(system.Radios,
		Radio{RID: 10002, Alias: "Second", Kind: "subscriber", Enabled: true},
		Radio{RID: 10003, Alias: "Third", Kind: "subscriber", Enabled: true})
	if err := c.system.service.Store.SaveConfig(storeDoc(t, system)); err != nil {
		t.Fatal(err)
	}
	original, _ := c.system.service.Store.Config()
	client := earlyWebLogin(t, c.system)
	form := earlyWebForm(t, earlyWebGet(t, client, c.system.web.URL+"/radios/edit?row=1", http.StatusOK), "record")
	form.Set("action", "delete")
	body := adminFormResponse(t, client, c.system.web.URL+"/radios/edit?row=1", c.system.web.URL, form, http.StatusOK)
	table := userAliasRecordTable(t, earlyWebGet(t, client, c.system.web.URL+"/radios", http.StatusOK))
	if strings.Count(table, "<tr>") != 4 || !strings.Contains(table, ">Second</td>") || !strings.Contains(table, ">Third</td>") || !strings.Contains(table, "/radios/edit?row=2") {
		t.Fatal("a rejected Delete removed a displayed row or shifted the remaining row indexes")
	}
	retry := earlyWebForm(t, body, "record")
	snapshot, err := webConfig([]byte(retry.Get("record")))
	originalConfig, _ := webConfig(original)
	if err != nil || !reflect.DeepEqual(snapshot, originalConfig["radios"].([]any)[1]) {
		t.Fatal("the rejected Delete changed its configuration snapshot")
	}
	unchanged, _ := c.system.service.Store.Config()
	if !bytes.Equal(unchanged, original) {
		t.Fatal("the rejected Delete changed saved configuration")
	}
	// Removing the legacy-invalid first row uses the same index shown in the
	// table. The previously rejected deletion must not also be applied.
	retry = earlyWebForm(t, earlyWebGet(t, client, c.system.web.URL+"/radios/edit?row=0", http.StatusOK), "rid")
	retry.Set("action", "delete")
	adminFormResponse(t, client, c.system.web.URL+"/radios?row=0", c.system.web.URL, retry, http.StatusSeeOther)
	saved, _ := c.system.service.Store.Config()
	if err := json.Unmarshal(saved, &system); err != nil {
		t.Fatal(err)
	}
	if len(system.Radios) != 2 || system.Radios[0].RID != 10002 || system.Radios[1].RID != 10003 {
		t.Fatal("a retry deleted a record other than the selected displayed row", system.Radios)
	}
}

func TestRadioAliasEditRetryPreservesUnknownFields(t *testing.T) {
	c := integrationNewCluster(t)
	raw, _ := c.system.service.Store.Config()
	config, _ := webConfig(raw)
	config["future_setting"] = map[string]any{"keep": "system value"}
	config["radios"].([]any)[0].(map[string]any)["future_radio_setting"] = map[string]any{"keep": "radio value"}
	if err := c.system.service.Store.SaveConfig(storeDoc(t, config)); err != nil {
		t.Fatal(err)
	}
	client := earlyWebLogin(t, c.system)
	address := c.system.web.URL + "/radios/edit?row=0"
	form := earlyWebForm(t, earlyWebGet(t, client, address, http.StatusOK), "rid")
	form.Set("alias", "An invalid edited name")
	body := adminFormResponse(t, client, address, c.system.web.URL, form, http.StatusOK)
	if table := userAliasRecordTable(t, earlyWebGet(t, client, c.system.web.URL+"/radios", http.StatusOK)); !strings.Contains(table, ">Portable</td>") || strings.Contains(table, form.Get("alias")) {
		t.Fatal("the rejected edit replaced the saved name in the table")
	}
	retry := earlyWebForm(t, body, "rid")
	retry.Set("alias", "Engine 12")
	adminFormResponse(t, client, address, c.system.web.URL, retry, http.StatusSeeOther)
	raw, _ = c.system.service.Store.Config()
	saved, _ := webConfig(raw)
	beforeRadio := config["radios"].([]any)[0].(map[string]any)
	afterRadio := saved["radios"].([]any)[0].(map[string]any)
	if len(saved["radios"].([]any)) != 1 || afterRadio["alias"] != "Engine 12" || !reflect.DeepEqual(saved["future_setting"], config["future_setting"]) || !reflect.DeepEqual(afterRadio["future_radio_setting"], beforeRadio["future_radio_setting"]) {
		t.Fatal("the corrected edit lost unknown data or added a record")
	}
}

func TestSystemUserAliasPropagatesThroughZoneToBothSiteProfiles(t *testing.T) {
	c := integrationNewCluster(t)
	sites := []*integrationService{c.site, c.otherSite}
	before := make([][]byte, len(sites))
	for index, pair := range sites {
		site := storeFixtureSite()
		site.ID = pair.service.Config().InstanceID
		site.Modems, site.Channels = nil, nil
		for modem := 0; modem < 3; modem++ {
			id := fmt.Sprintf("%s-modem-%d", site.ID, modem)
			role := "voice"
			if modem == 0 {
				role = "control"
			}
			site.Modems = append(site.Modems, Modem{ID: id, Device: "/dev/serial/by-id/test-" + id, Baud: 115200})
			site.Channels = append(site.Channels, Channel{ID: id + "-channel", Role: role, Identifier: 2, ChannelNo: index*3 + modem + 1, ModemID: id, Enabled: true})
		}
		if err := pair.service.Store.SaveConfig(storeDoc(t, site)); err != nil {
			t.Fatal(err)
		}
		before[index], _ = pair.service.Store.Config()
	}
	integrationSyncAll(t, c)
	client := earlyWebLogin(t, c.system)
	address := c.system.web.URL + "/radios/edit?row=0"
	form := earlyWebForm(t, earlyWebGet(t, client, address, http.StatusOK), "rid")
	form.Set("alias", "Engine 12")
	adminFormResponse(t, client, address, c.system.web.URL, form, http.StatusSeeOther)
	integrationSyncAll(t, c)
	for _, pair := range []*integrationService{c.zone, c.site, c.otherSite, c.dispatch} {
		var cached SystemConfig
		if !pair.service.readCache("system", &cached) || len(cached.Radios) != 1 || cached.Radios[0].RID != 10001 || cached.Radios[0].Alias != "Engine 12" {
			t.Fatalf("%s did not receive the System-owned name through its normal connection", pair.service.Config().InstanceID)
		}
	}
	for index, pair := range sites {
		profile, err := pair.service.profileRadio()
		if err != nil || len(profile.System.Radios) != 1 || profile.System.Radios[0].RID != 10001 || profile.System.Radios[0].Alias != "Engine 12" {
			t.Fatalf("%s profile lost the received name: %v", pair.service.Config().InstanceID, err)
		}
		after, err := pair.service.Store.Config()
		if err != nil || !bytes.Equal(before[index], after) {
			t.Fatalf("%s changed local RF/modem configuration during alias propagation: %v", pair.service.Config().InstanceID, err)
		}
		var site SiteConfig
		if err := json.Unmarshal(before[index], &site); err != nil {
			t.Fatal(err)
		}
		var configuredShape SiteConfig
		if err := json.Unmarshal(storeDoc(t, profile.Site), &configuredShape); err != nil {
			t.Fatal(err)
		}
		if !bytes.Equal(storeDoc(t, configuredShape), storeDoc(t, site)) {
			t.Fatalf("%s profile changed USB identities, control/voice roles or local settings", site.ID)
		}
	}
}

func TestSiteRetriesFailedAliasRefresh(t *testing.T) {
	site, runtime := operationFixture(t)
	runtime.mu.Lock()
	runtime.applyError = errors.Join(radio.ErrUserAliasRefresh, errors.New("alias receiver temporarily unavailable"))
	runtime.mu.Unlock()
	system := storeFixtureSystem()
	system.Radios[0].Alias = "Engine 12"
	if err := site.Store.WriteCache("system", storeDoc(t, system)); err != nil {
		t.Fatal(err)
	}
	site.refreshDirectory()
	select {
	case <-runtime.applications:
	case <-time.After(3 * time.Second):
		t.Fatal("the alias refresh was not attempted")
	}
	runtime.mu.Lock()
	runtime.applyError = nil
	runtime.mu.Unlock()
	select {
	case applied := <-runtime.applications:
		if applied.profile.System.Radios[0].Alias != "Engine 12" {
			t.Fatal("retry lost the System alias")
		}
	case <-time.After(3 * time.Second):
		t.Fatal("the unchanged alias candidate was never retried after failure")
	}
	runtime.mu.Lock()
	defer runtime.mu.Unlock()
	if runtime.starts != 0 || runtime.stops != 0 {
		t.Fatal("an alias retry restarted RF")
	}
}
