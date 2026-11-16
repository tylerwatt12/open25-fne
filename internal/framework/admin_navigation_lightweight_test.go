package framework

import (
	"bytes"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

func TestAdminNavigationDoesNotWaitForOperatorPublication(t *testing.T) {
	c := integrationNewCluster(t)
	s := c.site.service
	var page bytes.Buffer
	done := make(chan struct{})
	s.featureMu.Lock()
	go func() {
		defer close(done)
		result := httptest.NewRecorder()
		s.webHeader(result, httptest.NewRequest("GET", "/networking", nil), "Connections", Session{Username: "administrator"}, false)
		webFooter(result)
		page.Write(result.Body.Bytes())
		// Named table cells reuse the same lightweight administration identities.
		_ = s.adminNames()
	}()
	select {
	case <-done:
		s.featureMu.Unlock()
	case <-time.After(time.Second):
		s.featureMu.Unlock()
		<-done
		t.Fatal("navigation waited for the operator publication lock")
	}
	for _, want := range []string{"Site 1", "Laboratory", "Zone 1", "aria-current=location", "/channels", "/hardware"} {
		if !strings.Contains(page.String(), want) {
			t.Errorf("navigation lost %q", want)
		}
	}
}

func TestAdminTopologyUsesSmallAcceptedAddressesAndCurrentRegistry(t *testing.T) {
	s := &Service{Store: storeOpen(t, RoleZone), config: FileConfig{Role: RoleZone, InstanceID: "zone-1", PublicURL: "https://zone.example"}}
	zone := storeFixtureZone()
	zone.Sites = []SiteIdentity{
		{ID: "reported", Name: "Reported Site", AdminURL: "https://registry.example"},
		{ID: "unreported", Name: "Unreported Site", AdminURL: "https://unreported.example"},
		{ID: "blank", Name: "Blank Site", AdminURL: "https://old.example"},
		{ID: "internal-only", Name: "Internal only", Endpoint: "http://private.example:5102"},
	}
	if err := s.writeCache("topology", topology{SystemURL: "https://system.example", SiteURLs: map[string]string{
		"reported": "https://reported.example", "blank": "", "removed": "https://removed.example",
	}}); err != nil {
		t.Fatal(err)
	}
	tree := s.adminTopology(&zone)
	if tree.SystemURL != "https://system.example" || tree.ZoneURL != s.Config().PublicURL {
		t.Fatal("navigation lost explicit parent/current web addresses", tree)
	}
	want := map[string]string{"reported": "https://reported.example", "unreported": "https://unreported.example", "blank": "", "internal-only": ""}
	if len(tree.SiteURLs) != len(want) {
		t.Fatal("navigation retained an unregistered Site", tree.SiteURLs)
	}
	for id, address := range want {
		if got, ok := tree.SiteURLs[id]; !ok || got != address {
			t.Errorf("Site %s address=%q present=%v; want %q", id, got, ok, address)
		}
	}
	// A render owns its decoded map; filtering it must not change the retained
	// topology that another accepted configuration can use.
	var retained topology
	if !s.readCache("topology", &retained) || retained.SiteURLs["removed"] != "https://removed.example" {
		t.Fatal("navigation mutated the retained address projection")
	}
}

func TestAdminTopologyKeepsLocalSiteAddressWithoutReadingLocalReport(t *testing.T) {
	s := &Service{Store: storeOpen(t, RoleSite), config: FileConfig{Role: RoleSite, InstanceID: "site-1", PublicURL: "https://current.example"}}
	zone := storeFixtureZone()
	if err := s.writeCache("topology", topology{SiteURLs: map[string]string{"site-1": "https://previous.example"}}); err != nil {
		t.Fatal(err)
	}
	s.featureMu.Lock()
	defer s.featureMu.Unlock()
	if got := s.adminTopology(&zone).SiteURLs["site-1"]; got != s.Config().PublicURL {
		t.Fatal("local configured address was not used", got)
	}
}
