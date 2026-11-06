package framework

import (
	"bytes"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"open25.local/open25-fne/internal/radio"
)

func TestAdminDashboardKeepsCurrentLocationAndReadOnlyScope(t *testing.T) {
	c := integrationNewCluster(t)
	for _, instance := range []*integrationService{c.system, c.zone, c.site, c.otherSite, c.dispatch} {
		s := instance.service
		t.Run(s.Config().InstanceID, func(t *testing.T) {
			before, err := s.Store.Config()
			if err != nil {
				t.Fatal(err)
			}
			result := httptest.NewRecorder()
			s.webDashboard(result, httptest.NewRequest(http.MethodGet, "/", nil), Session{Username: "administrator"})
			body := result.Body.String()
			for _, want := range []string{"Overview", "Administration topology", "You are here", "aria-label=Breadcrumb", "Service connections"} {
				if !strings.Contains(body, want) {
					t.Errorf("dashboard missing %q", want)
				}
			}
			if strings.Count(body, "aria-current=location") != 1 {
				t.Fatal("topology does not identify exactly one current service")
			}
			for _, forbidden := range []string{"View records", "name=config", "name=record", "name=material", "/dev/trunksystem-control", strings.Repeat("z", 64), strings.Repeat("s", 64), "<button class=primary>Save"} {
				if strings.Contains(body, forbidden) {
					t.Errorf("dashboard contains configuration, secret or generic renderer %q", forbidden)
				}
			}
			after, err := s.Store.Config()
			if err != nil {
				t.Fatal(err)
			}
			if !bytes.Equal(before, after) {
				t.Fatal("viewing dashboard changed configuration")
			}
		})
	}
}

func TestAdminDashboardReportsMissingAndStaleEvidenceWithoutInventedHealth(t *testing.T) {
	c := integrationNewCluster(t)
	s := c.system.service
	zone := storeFixtureZone()
	zone.Sites = append(zone.Sites, SiteIdentity{ID: "missing-site", Name: "Unreported Site"})
	if err := s.writeCache("zone", zone); err != nil {
		t.Fatal(err)
	}
	observed := time.Now().Add(-time.Hour).UTC()
	report := SiteReport{ID: zone.Sites[0].ID, ObservedAt: observed, ReceivedAt: observed.Add(time.Minute), Config: SiteConfig{Modems: []Modem{{Device: "never-return-private-usb"}}}, Radio: &radio.Status{State: "running", Error: "Recorded channel failure", Channels: []radio.ChannelStatus{{TXHz: 443459999, RPCPort: 15999}}}}
	if err := s.writeCache("site_reports", []SiteReport{report}); err != nil {
		t.Fatal(err)
	}
	result := httptest.NewRecorder()
	s.webDashboard(result, httptest.NewRequest(http.MethodGet, "/", nil), Session{Username: "administrator"})
	body := result.Body.String()
	for _, want := range []string{"Stale", "Last reported radio state", webReportTime(observed), "No report", "Unknown", "Unreported Site", "Recorded channel failure"} {
		if !strings.Contains(body, want) {
			t.Errorf("dashboard missing %q", want)
		}
	}
	for _, forbidden := range []string{">Healthy<", ">Online<", "never-return-private-usb", "443459999", "15999"} {
		if strings.Contains(body, forbidden) {
			t.Errorf("dashboard invented health or exposed configuration %q", forbidden)
		}
	}
}
