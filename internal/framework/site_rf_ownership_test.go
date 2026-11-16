package framework

import (
	"bytes"
	"net/http"
	"testing"

	"open25.local/open25-fne/internal/radio"
)

func TestSiteOwnsBSIAndNACWithoutChangingSystemZoneOrOtherSite(t *testing.T) {
	c := integrationNewCluster(t)
	system, _ := c.system.service.Store.Config()
	zone, _ := c.zone.service.Store.Config()
	other, _ := c.otherSite.service.Store.Config()
	client := earlyWebLogin(t, c.site)
	form := earlyWebForm(t, earlyWebGet(t, client, c.site.web.URL+"/identification", 200), "bsi_profile")
	form.Set("bsi_profile", "harris") // Save preserves unimplemented options.
	earlyWebPost(t, client, c.site.web.URL+"/identification", form, c.site.web.URL, http.StatusSeeOther)
	form = earlyWebForm(t, earlyWebGet(t, client, c.site.web.URL+"/site-identity", 200), "nac")
	form.Set("nac", "682")
	earlyWebPost(t, client, c.site.web.URL+"/site-identity", form, c.site.web.URL, http.StatusSeeOther)
	p, err := c.site.service.profileRadio()
	if err != nil || p.Site.NAC != 682 || p.Site.Identification.BSIProfile != "harris" {
		t.Fatal("Site RF ownership was lost", p.Site, err)
	}
	for _, check := range []struct {
		pair   *integrationService
		before []byte
	}{{c.system, system}, {c.zone, zone}, {c.otherSite, other}} {
		after, _ := check.pair.service.Store.Config()
		if !bytes.Equal(after, check.before) {
			t.Fatal("Site Save changed another owner")
		}
	}
	sysPage := earlyWebGet(t, earlyWebLogin(t, c.system), c.system.web.URL+"/system", 200)
	zonePage := earlyWebGet(t, earlyWebLogin(t, c.zone), c.zone.web.URL+"/sites/edit?row=0", 200)
	if bytes.Contains(sysPage, []byte(`name="bsi_profile"`)) || bytes.Contains(zonePage, []byte(`name="nac"`)) {
		t.Fatal("foreign RF editor remains")
	}
	// A saved NAC must not stamp active audio until explicit Site restart.
	// Join supervision before replacing its runtime with this isolated double.
	c.site.service.operationCancel()
	c.site.service.operationWG.Wait()
	c.site.service.radio = &operationRadio{status: radio.Status{State: "running", NAC: 659}}
	frame := sipTestFrame("site-nac", 10002, 1001, 1, 0)
	frame.SourceKind = "application"
	if err := c.site.service.stampGeneratedVoiceHeader(&frame); err != nil || frame.P25.Header.NID != 659<<4 {
		t.Fatal("saved NAC replaced the active radio snapshot", err)
	}
}
