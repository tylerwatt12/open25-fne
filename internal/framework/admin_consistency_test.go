package framework

import (
	"bytes"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

func TestAdminSharedShellAcrossAllOwners(t *testing.T) {
	c := integrationNewCluster(t)
	for _, pair := range []*integrationService{c.system, c.zone, c.site, c.otherSite, c.dispatch} {
		s := pair.service
		t.Run(s.Config().InstanceID, func(t *testing.T) {
			before, _ := s.Store.Config()
			result := httptest.NewRecorder()
			s.webDashboard(result, httptest.NewRequest("GET", "/dashboard", nil), Session{Username: "administrator"})
			body := result.Body.String()
			previous := -1
			for _, label := range []string{"Status &amp; troubleshooting", "Configuration", "Operations", "Email", "Reference"} {
				at := strings.Index(body, "</svg>"+label+"</summary>")
				if at <= previous {
					t.Fatalf("shared navigation order missing %q", label)
				}
				previous = at
			}
			if strings.Count(body, "role=search") != 1 || strings.Contains(body, "ts-site") || strings.Contains(body, "Choose administration instance") {
				t.Fatal("role-specific shell or duplicate instance/search controls")
			}
			for _, want := range []string{"href=/dashboard>Refresh</a>", "class=compact-topology", "data-chooser-kind=instances", "data-admin-theme>Theme", "class=\"ts-panel summary-strip"} {
				if !strings.Contains(body, want) {
					t.Errorf("shared component missing %q", want)
				}
			}
			after, _ := s.Store.Config()
			if !bytes.Equal(before, after) {
				t.Fatal("viewing shell changed programming")
			}
		})
	}
}

func TestReportedChannelsUseSharedStatusesAndRetainTechnicalObservations(t *testing.T) {
	now := time.Now().UTC()
	status := modemObservationFixture(now)
	var out bytes.Buffer
	webRadioObservations(&out, &status)
	body := out.String()
	for _, want := range []string{"Active (control)", "Channel status", "Current traffic / issue", "Technical observations", "Native state", "Ready", "Busy"} {
		if !strings.Contains(body, want) {
			t.Errorf("reported channel missing %q", want)
		}
	}
	status.Channels[0].Telemetry.Samples[1].At = now.Add(-time.Minute)
	out.Reset()
	webRadioObservations(&out, &status)
	if strings.Contains(out.String(), "Active (control)") || !strings.Contains(out.String(), "Status unavailable") {
		t.Fatal("old relayed telemetry presented as current channel activity")
	}
	// Raw radio state stays with its existing owner; rendering cannot repair it.
	if status.Channels[0].State != "ready" || !status.Channels[0].Ready {
		t.Fatal("renderer mutated observations")
	}
}
