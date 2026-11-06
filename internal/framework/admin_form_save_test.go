package framework

import (
	"testing"
)

func TestEarlyWebSaveUsesSubmittedConfigurationAfterAnotherSaveOrDelete(t *testing.T) {
	c := integrationNewCluster(t)
	client := earlyWebLogin(t, c.system)
	form := earlyWebForm(t, earlyWebGet(t, client, c.system.web.URL+"/radios/edit?row=0", 200), "rid")
	current, _ := c.system.service.Store.Config()
	config, err := webConfig(current)
	if err != nil {
		t.Fatal(err)
	}
	config["radios"] = []any{}
	config["callsign"] = "another administrator"
	if err = c.system.service.Store.SaveConfig(storeDoc(t, config)); err != nil {
		t.Fatal(err)
	}
	form.Set("alias", "submitted last")
	earlyWebPost(t, client, c.system.web.URL+"/radios/edit?row=0", form, c.system.web.URL, 303)
	raw, _ := c.system.service.Store.Config()
	saved, err := webConfig(raw)
	if err != nil {
		t.Fatal(err)
	}
	radios := saved["radios"].([]any)
	if len(radios) != 1 || radios[0].(map[string]any)["alias"] != "submitted last" || saved["callsign"] != "another administrator" {
		t.Fatal("Save lost the submitted record or changed settings outside that form")
	}
	form.Set("action", "delete")
	for range 2 {
		earlyWebPost(t, client, c.system.web.URL+"/radios?row=0", form, c.system.web.URL, 303)
	}
	raw, _ = c.system.service.Store.Config()
	saved, _ = webConfig(raw)
	if len(saved["radios"].([]any)) != 0 {
		t.Fatal("submitted delete did not persist")
	}
}
