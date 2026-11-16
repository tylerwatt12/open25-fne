package framework

import (
	"context"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"testing"
	"time"

	"open25.local/open25-fne/internal/frameworkui"
)

func TestSiteStartupBrowserFixture(t *testing.T) {
	if os.Getenv("OPEN25_STARTUP_BROWSER_TEST") != "1" {
		t.Skip("set OPEN25_STARTUP_BROWSER_TEST=1 for startup form browser checks")
	}
	repo, err := filepath.Abs("../..")
	if err != nil {
		t.Fatal(err)
	}
	evidence := os.Getenv("OPEN25_STARTUP_BROWSER_EVIDENCE")
	if evidence == "" {
		evidence = t.TempDir()
	}
	c := integrationNewCluster(t)
	roles := []map[string]string{}
	for _, pair := range []*integrationService{c.site, c.otherSite} {
		pair.service.assets = frameworkui.Assets()
		roles = append(roles, map[string]string{"instance": pair.service.Config().InstanceID, "origin": pair.web.URL, "username": "administrator", "password": integrationPassword})
	}
	fixture := filepath.Join(evidence, "fixture.private.json")
	if err := WritePrivateJSON(fixture, map[string]any{"roles": roles}); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.Remove(fixture) })
	ctx, cancel := context.WithTimeout(t.Context(), 2*time.Minute)
	defer cancel()
	command := exec.CommandContext(ctx, "node", filepath.Join(repo, "tools/framework/site-startup-check.mjs"), fixture, evidence)
	output, err := command.CombinedOutput()
	t.Logf("Startup browser evidence: %s", output)
	if err != nil {
		t.Fatal(err)
	}
	for _, pair := range []*integrationService{c.site, c.otherSite} {
		var site SiteConfig
		if err := pair.service.owned(&site); err != nil || site.StartOnServiceStart {
			t.Fatal("browser did not restore startup preference to off", err)
		}
		if pair.service.radioStatus().State != "stopped" {
			t.Fatal("browser Save started RF")
		}
	}
	// The Site-only setting is not offered by the other service owners.
	for _, pair := range []*integrationService{c.system, c.zone, c.dispatch} {
		client := earlyWebLogin(t, pair)
		earlyWebGet(t, client, pair.web.URL+"/control-policy", http.StatusNotFound)
	}
}
