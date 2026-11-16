package framework

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
	"time"

	"open25.local/open25-fne/internal/radio"
)

func TestSiteControlPolicyFormsPreserveEnteredPolicyAndAssignments(t *testing.T) {
	c := integrationNewCluster(t)
	s := c.site.service
	config, _ := webConfig(storeDoc(t, storeFixtureSite()))
	config["control_policy"] = map[string]any{"mode": "future-entered-mode", "rotation_hours": json.Number("-0.125"), "keep": "unrelated policy field"}
	beforeChannels, _ := json.Marshal(config["channels"])
	beforeModems, _ := json.Marshal(config["modems"])
	if err := s.Store.SaveConfig(storeDoc(t, config)); err != nil {
		t.Fatal(err)
	}
	before, _ := s.Store.Config()
	client := earlyWebLogin(t, c.site)
	page := earlyWebGet(t, client, c.site.web.URL+"/control-policy", http.StatusOK)
	form := earlyWebForm(t, page, "control_policy.mode")
	if form.Get("control_policy.mode") != "future-entered-mode" || form.Get("control_policy.rotation_hours") != "-0.125" {
		t.Fatal("opening an existing policy changed its saved values")
	}
	if !strings.Contains(string(page), "class=ts-editor") || strings.Contains(string(page), "class=admin-record-layout") || strings.Contains(string(page), "http-equiv=refresh") {
		t.Fatal("control policy is not an explicit main-pane settings form")
	}
	for _, forbidden := range []string{" min=", " max=", " required", " disabled", " hidden name=\"control_policy"} {
		if strings.Contains(string(page), forbidden) {
			t.Fatalf("policy fields acquired a configuration restriction: %q", forbidden)
		}
	}
	afterOpen, _ := s.Store.Config()
	if !bytes.Equal(before, afterOpen) {
		t.Fatal("opening control policy saved configuration")
	}
	form.Set("control_policy.mode", "dynamic")
	form.Set("control_policy.rotation_hours", "0.3333333333333333")
	earlyWebPost(t, client, c.site.web.URL+"/control-policy", form, c.site.web.URL, http.StatusSeeOther)
	raw, _ := s.Store.Config()
	saved, _ := webConfig(raw)
	policy := saved["control_policy"].(map[string]any)
	channels, _ := json.Marshal(saved["channels"])
	modems, _ := json.Marshal(saved["modems"])
	if policy["mode"] != "dynamic" || webText(policy["rotation_hours"]) != "0.3333333333333333" || policy["keep"] != "unrelated policy field" || !bytes.Equal(channels, beforeChannels) || !bytes.Equal(modems, beforeModems) {
		t.Fatal("policy Save changed entered values, unknown policy fields or hardware assignments")
	}
	if s.radioStatus().State != "stopped" {
		t.Fatal("saving a policy started RF")
	}
	for _, pair := range []*integrationService{c.system, c.zone, c.dispatch} {
		other := earlyWebLogin(t, pair)
		earlyWebGet(t, other, pair.web.URL+"/control-policy", http.StatusNotFound)
		earlyWebGet(t, other, pair.web.URL+"/hardware", http.StatusNotFound)
	}
}

func TestSiteNewControlPolicyShowsApprovedStartingValuesWithoutSaving(t *testing.T) {
	c := integrationNewCluster(t)
	s := c.site.service
	raw, _ := s.Store.Config()
	config, _ := webConfig(raw)
	delete(config, "control_policy")
	if err := s.Store.SaveConfig(storeDoc(t, config)); err != nil {
		t.Fatal(err)
	}
	before, _ := s.Store.Config()
	client := earlyWebLogin(t, c.site)
	form := earlyWebForm(t, earlyWebGet(t, client, c.site.web.URL+"/control-policy", http.StatusOK), "control_policy.mode")
	if form.Get("control_policy.mode") != "static" || form.Get("control_policy.rotation_hours") != "0" || form.Has("control_policy.rotation_enabled") {
		t.Fatal("new policy did not show approved static/off/24-hour initial choices")
	}
	after, _ := s.Store.Config()
	if !bytes.Equal(before, after) {
		t.Fatal("opening a new policy saved its starting values")
	}
	config["control_policy"] = map[string]any{"mode": "dynamic"}
	if err := s.Store.SaveConfig(storeDoc(t, config)); err != nil {
		t.Fatal(err)
	}
	form = earlyWebForm(t, earlyWebGet(t, client, c.site.web.URL+"/control-policy", http.StatusOK), "control_policy.mode")
	if form.Get("control_policy.rotation_hours") != "" {
		t.Fatal("a missing field in an existing policy received a starting value")
	}
}

func TestSiteControlSwitchFormListsConfiguredChoicesAndPreservesSelection(t *testing.T) {
	c := integrationNewCluster(t)
	s := c.site.service
	var site SiteConfig
	if err := s.owned(&site); err != nil {
		t.Fatal(err)
	}
	site.Channels = []Channel{{ID: "first", Name: "First <control>", Enabled: true}, {ID: "candidate", Name: "Candidate voice", Enabled: true}, {ID: "disabled", Name: "Disabled voice"}}
	if err := s.Store.SaveConfig(storeDoc(t, site)); err != nil {
		t.Fatal(err)
	}
	var output bytes.Buffer
	r := httptest.NewRequest(http.MethodPost, "/radio-operation", strings.NewReader(url.Values{"target_channel_id": {"candidate"}}.Encode()))
	r.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	s.webControlSwitchForm(&output, r, Session{Username: "administrator", CSRF: "csrf-fixture"})
	form := earlyWebForm(t, output.Bytes(), "target_channel_id")
	if form.Get("target_channel_id") != "candidate" || form.Get("csrf") != "csrf-fixture" {
		t.Fatal("manual switch selection or CSRF was not retained")
	}
	for _, want := range []string{`data-chooser-kind="channels"`, `data-chooser-target="target_channel_id"`, "name=action value=switch-control"} {
		if !strings.Contains(output.String(), want) {
			t.Fatalf("manual switch presentation missing %q", want)
		}
	}
}

func TestHardwareInventoryPresentationShowsOnlyDevicePresenceAndSiteUse(t *testing.T) {
	for _, test := range []struct {
		name              string
		present, known    bool
		assignments       []HardwareAssignmentView
		owners            []HardwareOwner
		status, use, node string
	}{
		{"current Site", true, true, []HardwareAssignmentView{{Local: true}}, nil, "Present", "Allocated", "/dev/ttyACM2"},
		{"another Site", true, true, []HardwareAssignmentView{{Local: false}}, nil, "Present", "Used elsewhere", "/dev/ttyACM2"},
		{"unallocated", true, true, nil, nil, "Present", "Unallocated", "/dev/ttyACM2"},
		{"missing allocated device", false, true, []HardwareAssignmentView{{Local: true}}, nil, "Unavailable", "Allocated", "—"},
		{"presence unknown", true, false, nil, nil, "Unavailable", "Unallocated", "—"},
		{"running local owner after assignment edit", true, true, nil, []HardwareOwner{{Local: true, PID: 4321, Name: "private-process"}}, "Present", "Allocated", "/dev/ttyACM2"},
		{"another running owner", true, true, nil, []HardwareOwner{{PID: 4321, Name: "private-process"}}, "Present", "Used elsewhere", "/dev/ttyACM2"},
	} {
		t.Run(test.name, func(t *testing.T) {
			inventory := HardwareInventoryView{Hostname: "private-host", ObservedAt: time.Now(), Devices: []HardwareDeviceView{{HardwareDevice: radio.HardwareDevice{ID: "private-device-key", Device: "/dev/serial/by-id/saved-device", ResolvedDevice: "/dev/ttyACM2", UID: "hardware-serial", Product: "Modem <&>", Present: test.present, PresenceKnown: test.known, Error: "private-error"}, Assignments: test.assignments, Owners: test.owners}}}
			var output bytes.Buffer
			webHardwareInventory(&output, inventory, SiteReport{}, nil, time.Now())
			body := output.String()
			for _, want := range []string{"Modem &lt;&amp;&gt;", "hardware-serial", ">Device / modem</th>", ">Serial device path</th>", ">Hardware status</th>", ">Use</th>", ">" + test.status + "</span>", ">" + test.use, ">" + test.node + "</span>"} {
				if !strings.Contains(body, want) {
					t.Errorf("USB inventory missing %q", want)
				}
			}
			for _, forbidden := range []string{"Details", "Process use", "Saved assignment", "Modem assignments", "Channel operation", "Local host", "private-host", "private-process", "private-error", "4321", "name=config", "<input", "http-equiv=refresh"} {
				if strings.Contains(body, forbidden) {
					t.Errorf("USB inventory retains removed information %q", forbidden)
				}
			}
			if test.status == "Unavailable" && strings.Contains(body, "/dev/ttyACM2") {
				t.Fatal("absent device showed an old node as current")
			}
		})
	}
	var output bytes.Buffer
	webHardwareInventory(&output, HardwareInventoryView{Error: "passive discovery failed"}, SiteReport{}, nil, time.Now())
	if !strings.Contains(output.String(), "Hardware inventory unavailable.") {
		t.Fatal("discovery failure represented as an empty successful observation")
	}
}
