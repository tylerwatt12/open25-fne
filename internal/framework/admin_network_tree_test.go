package framework

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"net/url"
	"reflect"
	"strings"
	"testing"
)

func TestStructuredProfileFormSavesOrderGainAndRetainsOtherPreferences(t *testing.T) {
	c := integrationNewCluster(t)
	s := c.system.service
	user := fixtureUser()
	user.Profile.Desktop = ptrDesktop(desktopFixture())
	user.Profile.Name = "Existing profile"
	user.Profile.Zones = []ProfileZone{{Name: "LAB", Talkgroups: []int{1001, 1002}}}
	user.Profile.Receive = []ReceiveSetting{{TalkgroupID: 1001, Volume: 0.753, Pan: -1}}
	if err := s.Store.SaveUser(user.ID, user, "retain-password"); err != nil {
		t.Fatal(err)
	}
	_, password, _ := s.Store.User(user.ID)
	before, _ := s.Store.Config()
	client := earlyWebLogin(t, c.system)
	page := earlyWebGet(t, client, c.system.web.URL+"/profiles/edit?row=0", http.StatusOK)
	form := earlyWebForm(t, page, "selected_talkgroup")
	if form.Get("receive_0_volume") != "75.3" || form.Get("zones_0_talkgroups") != "1001, 1002" {
		t.Fatal("structured form lost stored order or percent", form)
	}
	form.Set("zones_0_name", "New layout")
	form.Set("zones_0_talkgroups", "1002, 98765, 1001, 1002")
	form.Set("receive_0_volume", "125.5")
	form.Set("receive_0_muted", "true")
	form.Set("receive_0_pan", "-3")
	earlyWebPost(t, client, c.system.web.URL+"/profiles/edit?row=0", form, c.system.web.URL, http.StatusSeeOther)
	saved, afterPassword, err := s.Store.User(user.ID)
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(saved.Profile.Zones[0], ProfileZone{Name: "New layout", Talkgroups: []int{1002, 98765, 1001, 1002}}) || saved.Profile.Receive[0].Volume != 1.255 || saved.Profile.Receive[0].Pan != -3 || !saved.Profile.Receive[0].Muted {
		t.Fatal("profile Save filtered or corrected entered preferences", saved.Profile)
	}
	if !reflect.DeepEqual(saved.Profile.Desktop, user.Profile.Desktop) || !bytes.Equal(password, afterPassword) {
		t.Fatal("profile editor changed other preferences or credentials")
	}
	after, _ := s.Store.Config()
	if !bytes.Equal(before, after) {
		t.Fatal("profile editor changed radio/hardware configuration")
	}
}

func TestPermissionCheckboxesPreserveUnknownPermissionAndDoNotFilter(t *testing.T) {
	c := integrationNewCluster(t)
	user := fixtureUser()
	user.Permissions = []string{"ptt", "future_permission"}
	if err := c.system.service.Store.SaveUser(user.ID, user, "retain-password"); err != nil {
		t.Fatal(err)
	}
	client := earlyWebLogin(t, c.system)
	page := earlyWebGet(t, client, c.system.web.URL+"/users/edit?row=0", http.StatusOK)
	form := earlyWebForm(t, page, "username")
	if !strings.Contains(string(page), "future_permission") {
		t.Fatal("unknown permission not editable")
	}
	form["permissions"] = []string{"secure_ptt", "future_permission"}
	earlyWebPost(t, client, c.system.web.URL+"/users/edit?row=0", form, c.system.web.URL, http.StatusSeeOther)
	saved, _, _ := c.system.service.Store.User(user.ID)
	if !reflect.DeepEqual(saved.Permissions, []string{"secure_ptt", "future_permission"}) {
		t.Fatal("permission UI changed submitted selection", saved.Permissions)
	}
}

func TestSMTPSettingsSaveWithoutSendingOrEchoingPassword(t *testing.T) {
	c := integrationNewCluster(t)
	client := earlyWebLogin(t, c.site)
	before, _ := c.site.service.Store.Config()
	page := earlyWebGet(t, client, c.site.web.URL+"/smtp-settings", http.StatusOK)
	form := earlyWebForm(t, page, "host")
	if form.Get("inherit_smtp") != "true" {
		t.Fatal("non-root SMTP source did not start inherited")
	}
	form.Set("host", "smtp-entered.example")
	form.Set("smtp_password", "never-echo-this-password")
	body := adminFormResponse(t, client, c.site.web.URL+"/smtp-settings", c.site.web.URL, form, http.StatusOK)
	if bytes.Contains(body, []byte("never-echo-this-password")) || !bytes.Contains(body, []byte("SMTP settings saved. No email was sent.")) || !bytes.Contains(body, []byte("smtp-entered.example")) {
		t.Fatal("SMTP form lost entered public value or leaked a password")
	}
	profile, err := c.site.service.smtpProfile()
	if err != nil || profile.Host != "smtp-entered.example" || profile.Password != "never-echo-this-password" || !profile.Inherit {
		t.Fatal("SMTP profile was not saved", err)
	}
	form.Set("smtp_password", "")
	_ = adminFormResponse(t, client, c.site.web.URL+"/smtp-settings", c.site.web.URL, form, http.StatusOK)
	profile, err = c.site.service.smtpProfile()
	if err != nil || profile.Password != "never-echo-this-password" {
		t.Fatal("blank replacement erased password")
	}
	after, _ := c.site.service.Store.Config()
	if !bytes.Equal(before, after) {
		t.Fatal("SMTP page altered radio configuration")
	}
}

func TestNumericAlgorithmPairKeepsArbitraryIdentity(t *testing.T) {
	var out bytes.Buffer
	field := webField{Name: "algorithm_id", Label: "Transmit algorithm", Kind: "select-number", Choices: webAlgorithmChoices}
	webFields(&out, []webField{field}, map[string]any{"algorithm_id": json.Number("999")})
	for _, want := range []string{"data-number-decimal", "data-number-hex", "data-number-choice", `value="999"`, `value="3E7"`} {
		if !strings.Contains(out.String(), want) {
			t.Fatal("numeric algorithm presentation missing", want)
		}
	}
	request := httptest.NewRequest("POST", "/talkgroups/edit", nil)
	request.PostForm = url.Values{"algorithm_id": {"999"}, "algorithm_id__base": {"hex"}, "algorithm_id__hex": {"ABC"}}
	record := map[string]any{}
	if err := webReadFields(request, []webField{field}, record); err != nil || webText(record["algorithm_id"]) != "2748" {
		t.Fatal("algorithm choice was restricted", record, err)
	}
}
