package framework

import (
	"bytes"
	"encoding/json"
	"io"
	"net/http"
	"strings"
	"testing"
)

func TestAdminNewEntryFormsUseApprovedDefaultsWithoutSaving(t *testing.T) {
	c := integrationNewCluster(t)
	cases := []struct {
		pair         *integrationService
		path, anchor string
		want         map[string]string
		unchecked    []string
	}{
		{c.system, "/radios/edit", "rid", map[string]string{"kind": "subscriber", "enabled": "true", "rid": "", "rid__hex": "", "alias": ""}, nil},
		{c.system, "/talkgroups/edit", "tgid", map[string]string{"enabled": "true", "affiliation_access": "all", "transmit_access": "all", "site_access": "all", "start_policy": "fast_start", "priority": "10", "admission_mode": "clear", "transmit_mode": "clear", "tgid": "", "tgid__hex": "", "alias": "", "algorithm_id": "", "key_id": "", "key_id__hex": "", "affiliation_radio_ids": "", "transmit_radio_ids": "", "site_ids": ""}, nil},
		{c.system, "/positions/edit", "id", map[string]string{"enabled": "true", "id": "", "name": "", "radio_id": "", "talkgroups": ""}, nil},
		{c.system, "/users/edit", "username", map[string]string{"role": "operator", "enabled": "true", "id": "", "username": "", "password": "", "radio_id": "", "position_ids": "", "talkgroups": "", "permissions": ""}, nil},
		{c.system, "/automations/edit", "id", map[string]string{"enabled": "true", "id": "", "name": "", "radio_id": "", "talkgroups": "", "actions": ""}, nil},
		{c.system, "/keys/edit", "key_id", map[string]string{"algorithm_id": "", "key_id": "", "key_id__hex": "", "name": "", "material": ""}, []string{"enabled"}},
		{c.system, "/recipients/edit", "recipient", map[string]string{"recipient": "", "public_key": ""}, nil},
		{c.system, "/status-labels/edit", "id", map[string]string{"id": "", "label": ""}, nil},
		{c.system, "/message-labels/edit", "id", map[string]string{"id": "", "label": ""}, nil},
		{c.zone, "/identifiers/edit", "identifier", map[string]string{"identifier": "", "identifier__hex": "", "base_frequency_hz": "", "channel_spacing_hz": "12.5", "uplink_offset_hz": "", "bandwidth_hz": "12.5"}, nil},
		{c.zone, "/sites/edit", "id", map[string]string{"id": "", "name": "", "site_id": "", "nac": "", "endpoint": "", "admin_url": ""}, nil},
		{c.site, "/modems/edit", "id", map[string]string{"baud": "115200", "calibration.rx_offset_hz": "0", "calibration.tx_offset_hz": "0", "calibration.rx_dc_offset": "0", "calibration.tx_dc_offset": "0", "calibration.rx_level": "", "calibration.tx_level": "", "id": "", "name": "", "device": ""}, []string{"calibration.rx_invert", "calibration.tx_invert", "calibration.ptt_invert"}},
		{c.site, "/channels/edit", "id", map[string]string{"role": "voice", "id": "", "name": "", "identifier": "", "channel_no": "", "power_percent": "", "modem_id": ""}, []string{"enabled"}},
	}
	for _, test := range cases {
		t.Run(test.path, func(t *testing.T) {
			before, err := test.pair.service.Store.Config()
			if err != nil {
				t.Fatal(err)
			}
			client := earlyWebLogin(t, test.pair)
			body := earlyWebGet(t, client, test.pair.web.URL+test.path, http.StatusOK)
			values := earlyWebForm(t, body, test.anchor)
			for name, want := range test.want {
				if !values.Has(name) || values.Get(name) != want {
					t.Errorf("Add field %s = %q (present %v), want %q", name, values.Get(name), values.Has(name), want)
				}
			}
			for _, name := range test.unchecked {
				if values.Has(name) || !bytes.Contains(body, []byte(`name="`+name+`"`)) {
					t.Errorf("Add checkbox %s is checked or missing", name)
				}
			}
			after, err := test.pair.service.Store.Config()
			if err != nil || !bytes.Equal(before, after) {
				t.Fatal("viewing Add changed saved configuration", err)
			}
		})
	}
	users, err := c.system.service.Store.userRecords()
	if err != nil || len(users) != 0 {
		t.Fatal("viewing user Add created a stored user", err)
	}
	automations, err := c.system.service.Store.automationRecords()
	if err != nil || len(automations) != 0 {
		t.Fatal("viewing automation Add created a stored identity", err)
	}
}

func TestAdminNewEntryDefaultsDoNotReplaceExistingZeroFalseOrBlank(t *testing.T) {
	c := integrationNewCluster(t)
	var system SystemConfig
	raw, _ := c.system.service.Store.Config()
	if err := json.Unmarshal(raw, &system); err != nil {
		t.Fatal(err)
	}
	system.Talkgroups[0] = Talkgroup{TGID: 1001}
	if err := c.system.service.Store.SaveConfig(storeDoc(t, system)); err != nil {
		t.Fatal(err)
	}
	client := earlyWebLogin(t, c.system)
	form := earlyWebForm(t, earlyWebGet(t, client, c.system.web.URL+"/talkgroups/edit?row=0", http.StatusOK), "tgid")
	for field, want := range map[string]string{"tgid": "1001", "priority": "0", "affiliation_access": "", "transmit_access": "", "site_access": "", "start_policy": "", "admission_mode": "", "transmit_mode": "", "algorithm_id": "0", "key_id": "0"} {
		if form.Get(field) != want {
			t.Errorf("existing %s became %q, want %q", field, form.Get(field), want)
		}
	}
	if form.Has("enabled") {
		t.Fatal("existing disabled talkgroup was enabled")
	}
	if err := c.system.service.Store.SaveUser("blank-role", User{ID: "blank-role", Username: "Existing"}, "password"); err != nil {
		t.Fatal(err)
	}
	form = earlyWebForm(t, earlyWebGet(t, client, c.system.web.URL+"/users/edit?row=0", http.StatusOK), "username")
	if form.Get("role") != "" || form.Has("enabled") || form.Get("radio_id") != "0" {
		t.Fatal("existing user was given new-entry defaults", form)
	}
}

func TestAdminNewEntryDefaultsNeverReapplyOnSubmittedForms(t *testing.T) {
	c := integrationNewCluster(t)
	client := earlyWebLogin(t, c.system)
	values := earlyWebForm(t, earlyWebGet(t, client, c.system.web.URL+"/talkgroups/edit", http.StatusOK), "tgid")
	values.Set("alias", "Keep my submitted name")
	for _, field := range []string{"affiliation_access", "transmit_access", "site_access", "start_policy", "priority", "admission_mode", "transmit_mode", "algorithm_id", "key_id"} {
		values.Set(field, "")
	}
	values.Del("enabled")
	values.Set("tgid__base", "hex")
	values.Set("tgid__hex", "G")
	before, _ := c.system.service.Store.Config()
	request, err := http.NewRequest(http.MethodPost, c.system.web.URL+"/talkgroups/edit", strings.NewReader(values.Encode()))
	if err != nil {
		t.Fatal(err)
	}
	request.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	request.Header.Set("Origin", c.system.web.URL)
	response, err := client.Do(request)
	if err != nil {
		t.Fatal(err)
	}
	body, err := io.ReadAll(response.Body)
	response.Body.Close()
	if err != nil || response.StatusCode != http.StatusOK {
		t.Fatal("failed form was not shown", response.StatusCode, err)
	}
	failed := earlyWebForm(t, body, "tgid")
	for _, field := range []string{"priority", "affiliation_access", "transmit_access", "site_access", "start_policy", "admission_mode", "transmit_mode", "algorithm_id", "key_id"} {
		if failed.Get(field) != "" {
			t.Errorf("failed submission defaulted %s to %q", field, failed.Get(field))
		}
	}
	if failed.Has("enabled") || failed.Get("tgid__hex") != "G" || failed.Get("alias") != values.Get("alias") {
		t.Fatal("failed submission changed entered values", failed)
	}
	after, _ := c.system.service.Store.Config()
	if !bytes.Equal(before, after) {
		t.Fatal("failed submission changed saved configuration")
	}
	failed.Set("tgid__hex", "0BB8")
	earlyWebPost(t, client, c.system.web.URL+"/talkgroups/edit", failed, c.system.web.URL, http.StatusSeeOther)
	raw, _ := c.system.service.Store.Config()
	var saved SystemConfig
	if err := json.Unmarshal(raw, &saved); err != nil {
		t.Fatal(err)
	}
	var original SystemConfig
	if err := json.Unmarshal(before, &original); err != nil {
		t.Fatal(err)
	}
	if len(saved.Talkgroups) != len(original.Talkgroups)+1 {
		t.Fatalf("corrected Add created %d records, want one", len(saved.Talkgroups)-len(original.Talkgroups))
	}
	for _, group := range saved.Talkgroups {
		if group.TGID == 3000 {
			if group.Enabled || group.Priority != 0 || group.StartPolicy != "" || group.SiteAccess != "" || group.AffiliationAccess != "" || group.TransmitAccess != "" || group.AdmissionMode != "" || group.TransmitMode != "" || group.Alias != values.Get("alias") {
				t.Fatal("saving the submitted blank/false settings reinserted defaults", group)
			}
			return
		}
	}
	t.Fatal("submitted new talkgroup was not saved")
}

func TestAdminNewEntryDirectoryFailureKeepsSubmittedValues(t *testing.T) {
	c := integrationNewCluster(t)
	client := earlyWebLogin(t, c.system)
	values := earlyWebForm(t, earlyWebGet(t, client, c.system.web.URL+"/users/edit", http.StatusOK), "username")
	values.Set("id", "submitted-user")
	values.Set("username", "Submitted username")
	values.Set("role", "")
	values.Set("permissions", "future_permission")
	values.Set("radio_id__base", "hex")
	values.Set("radio_id__hex", "G")
	values.Del("enabled")
	request, err := http.NewRequest(http.MethodPost, c.system.web.URL+"/users/edit", strings.NewReader(values.Encode()))
	if err != nil {
		t.Fatal(err)
	}
	request.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	request.Header.Set("Origin", c.system.web.URL)
	response, err := client.Do(request)
	if err != nil {
		t.Fatal(err)
	}
	body, err := io.ReadAll(response.Body)
	response.Body.Close()
	if err != nil || response.StatusCode != http.StatusOK {
		t.Fatal("failed user form was not shown", response.StatusCode, err)
	}
	failed := earlyWebForm(t, body, "username")
	if failed.Get("role") != "" || failed.Has("enabled") || failed.Get("username") != values.Get("username") || failed.Get("radio_id__hex") != "G" || failed.Get("permissions") != "future_permission" {
		t.Fatal("failed directory submission was defaulted or changed", failed)
	}
	users, err := c.system.service.Store.userRecords()
	if err != nil || len(users) != 0 {
		t.Fatal("failed Add created a stored user", err)
	}
	failed.Set("radio_id__hex", "000000")
	earlyWebPost(t, client, c.system.web.URL+"/users/edit", failed, c.system.web.URL, http.StatusSeeOther)
	user, _, err := c.system.service.Store.User("submitted-user")
	if err != nil || user.Enabled || user.Role != "" || user.RadioID != 0 || len(user.Permissions) != 1 || user.Permissions[0] != "future_permission" {
		t.Fatal("corrected user submission was defaulted or filtered", user, err)
	}
}
