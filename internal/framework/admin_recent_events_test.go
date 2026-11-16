package framework

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

func TestOverviewUsesCurrentDiagnosticsWithoutHistoryReads(t *testing.T) {
	c := integrationNewCluster(t)
	for _, node := range []*integrationService{c.system, c.zone, c.site, c.otherSite, c.dispatch} {
		s := node.service
		t.Run(s.Config().InstanceID, func(t *testing.T) {
			if err := s.Store.AddEvent("connection", "Current process diagnostic"); err != nil {
				t.Fatal(err)
			}
			// An accidental historical read now fails, even while current status and
			// diagnostics remain available through their independent owners.
			if err := s.activity.Close(); err != nil {
				t.Fatal(err)
			}
			done := make(chan string, 1)
			go func() {
				response := httptest.NewRecorder()
				s.webDashboard(response, httptest.NewRequest(http.MethodGet, "/dashboard", nil), Session{Username: "administrator"})
				done <- response.Body.String()
			}()
			select {
			case body := <-done:
				if !strings.Contains(body, "Recent diagnostics") || !strings.Contains(body, "Current process diagnostic") || strings.Contains(body, "Activity records unavailable") {
					t.Fatal("Overview depended on historical storage", body)
				}
			case <-time.After(time.Second):
				t.Fatal("Overview waited for history storage")
			}
		})
	}
}
