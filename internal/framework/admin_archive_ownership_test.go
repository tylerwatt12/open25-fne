package framework

import (
	"net/http"
	"strings"
	"testing"
)

func TestSourceAdministrationLinksToSystemArchiveWithoutLocalHistory(t *testing.T) {
	c := integrationNewCluster(t)
	integrationSyncAll(t, c)
	for _, node := range []*integrationService{c.zone, c.site, c.otherSite, c.dispatch} {
		t.Run(node.service.Config().InstanceID, func(t *testing.T) {
			client := earlyWebLogin(t, node)
			body := string(earlyWebGet(t, client, node.web.URL+"/activity-status", http.StatusOK))
			for _, label := range []string{"History delivery", "Awaiting acknowledgment", "Outbox limit", "Oldest pending", "100000"} {
				if !strings.Contains(body, label) {
					t.Errorf("missing %q", label)
				}
			}
			if !strings.Contains(body, `href="`+c.system.web.URL+`/history"`) {
				t.Fatal("historical navigation did not use owning System address")
			}
			for _, path := range []string{"/history", "/history-settings"} {
				earlyWebGet(t, client, node.web.URL+path, http.StatusNotFound)
			}
			for _, forbidden := range []string{"History storage and capture", "name=retention_days", "Download CSV", "href=\"/history\"", "href=\"/history-settings\""} {
				if strings.Contains(body, forbidden) {
					t.Errorf("source exposes local history %q", forbidden)
				}
			}
		})
	}
}
