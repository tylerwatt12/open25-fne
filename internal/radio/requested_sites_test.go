package radio

import (
	"bytes"
	"slices"
	"testing"
)

func TestRequestedSitesSupplyOnlyLocalEnabledRFInterest(t *testing.T) {
	c := testController(t, nil)
	c.profile = fixtureProfile()
	c.status.InhibitionReady = true
	group := &c.profile.System.Talkgroups[0]
	if c.HasRFInterest(group.TGID) {
		t.Fatal("ordinary group gained interest without an affiliation")
	}
	group.RequestedSiteIDs = []string{"other-site"}
	if c.HasRFInterest(group.TGID) {
		t.Fatal("a different Site's request supplied local interest")
	}
	group.RequestedSiteIDs = []string{c.profile.Site.ID}
	if !c.HasRFInterest(group.TGID) || len(c.affiliations) != 0 {
		t.Fatal("requested Site did not gain interest independently of affiliations")
	}
	group.SiteAccess, group.SiteIDs = "selected", []string{"other-site"}
	if c.HasRFInterest(group.TGID) {
		t.Fatal("requested Site bypassed Site access")
	}
	group.SiteIDs = []string{c.profile.Site.ID}
	group.Enabled = false
	if c.HasRFInterest(group.TGID) {
		t.Fatal("disabled group supplied requested interest")
	}
	group.Enabled = true
	c.status.InhibitionReady = false
	if c.HasRFInterest(group.TGID) {
		t.Fatal("requested interest bypassed native inhibition readiness")
	}
}

func TestRequestedSitesRenderCloneAndPolicyRefresh(t *testing.T) {
	profile := fixtureProfile()
	original := PolicySignature(profile)
	profile.System.Talkgroups[0].RequestedSiteIDs = []string{profile.Site.ID}
	if PolicySignature(profile) == original {
		t.Fatal("request change did not replace native policy")
	}
	rendered := RenderLookups(profile)
	if !bytes.Contains([]byte(rendered["talkgroup_rules.yml"]), []byte("requested: true")) {
		t.Fatal("requested Site was absent from native rule")
	}
	copy := cloneProfile(profile)
	profile.System.Talkgroups[0].RequestedSiteIDs[0] = "elsewhere"
	if !slices.Equal(copy.System.Talkgroups[0].RequestedSiteIDs, []string{profile.Site.ID}) {
		t.Fatal("running policy shared a mutable request list")
	}
	if PolicySignature(profile) != original {
		t.Fatal("a request for a different Site changed local policy")
	}
}
