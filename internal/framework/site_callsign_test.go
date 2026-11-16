package framework

import (
	"bytes"
	"encoding/json"
	"net/http"
	"reflect"
	"strings"
	"testing"

	"open25.local/open25-fne/internal/radio"
)

func TestSiteCallsignSavesIndependentlyAndRequiresRestart(t *testing.T) {
	c := integrationNewCluster(t)
	systemBefore, _ := c.system.service.Store.Config()
	for i, pair := range []*integrationService{c.site, c.otherSite} {
		// Save/restart display assertions use a fixed observation double.
		pair.service.operationCancel()
		pair.service.operationWG.Wait()
		profile, err := pair.service.profileRadio()
		if err != nil {
			t.Fatal(err)
		}
		pair.service.radio = &operationRadio{applications: make(chan policyApplication, 128), status: radio.Status{State: "running", AppliedRFConfiguration: radio.RFConfigurationSignature(profile)}}
		client := earlyWebLogin(t, pair)
		before, _ := pair.service.Store.Config()
		var expected SiteConfig
		if err := json.Unmarshal(before, &expected); err != nil {
			t.Fatal(err)
		}
		value := []string{"K9TEST", "W2TEST"}[i]
		page := earlyWebGet(t, client, pair.web.URL+"/identification", http.StatusOK)
		form := earlyWebForm(t, page, "callsign")
		if form.Get("callsign") != "N0TEST" {
			t.Fatal("Site form did not show its saved callsign")
		}
		form.Set("callsign", value)
		earlyWebPost(t, client, pair.web.URL+"/identification", form, pair.web.URL, http.StatusSeeOther)
		expected.Identification.Callsign = value
		var saved SiteConfig
		if err := pair.service.owned(&saved); err != nil || !reflect.DeepEqual(saved, expected) {
			t.Fatal("callsign Save changed unrelated Site programming", err)
		}
		profile, err = pair.service.profileRadio()
		if err != nil || profile.Site.Identification.Callsign != value {
			t.Fatal("runtime profile did not use the owning Site callsign", err)
		}
		page = earlyWebGet(t, client, pair.web.URL+"/identification", http.StatusOK)
		if !bytes.Contains(page, []byte("data-rf-pending")) {
			t.Fatal("saved callsign did not show configured-but-awaiting-restart")
		}
		other := c.otherSite
		otherWant := "N0TEST"
		if i == 1 {
			other, otherWant = c.site, "K9TEST"
		}
		var untouched SiteConfig
		if err := other.service.owned(&untouched); err != nil || untouched.Identification.Callsign != otherWant {
			t.Fatal("callsign Save changed another Site", err)
		}
	}
	systemAfter, _ := c.system.service.Store.Config()
	if !bytes.Equal(systemBefore, systemAfter) {
		t.Fatal("Site callsign Save changed System settings")
	}
	page := earlyWebGet(t, earlyWebLogin(t, c.system), c.system.web.URL+"/system", http.StatusOK)
	if strings.Contains(string(page), `name="callsign"`) || strings.Contains(string(page), "Callsign") {
		t.Fatal("System still exposes a callsign setting")
	}
}
