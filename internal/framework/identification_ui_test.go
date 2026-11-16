package framework

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"testing"
	"time"

	"open25.local/open25-fne/internal/frameworkui"
)

func TestDigitalIdentificationBrowser(t *testing.T) {
	if os.Getenv("OPEN25_ADMIN_BROWSER_TEST") != "1" {
		t.Skip("set OPEN25_ADMIN_BROWSER_TEST=1 for browser checks")
	}
	c := integrationNewCluster(t)
	repo, err := filepath.Abs("../..")
	if err != nil {
		t.Fatal(err)
	}
	evidence := os.Getenv("OPEN25_IDENTIFICATION_BROWSER_EVIDENCE")
	if evidence == "" {
		evidence = t.TempDir()
	}
	roles := []map[string]string{}
	for _, pair := range []*integrationService{c.site, c.otherSite} {
		pair.service.assets = frameworkui.Assets()
		roles = append(roles, map[string]string{"origin": pair.web.URL, "instance": pair.service.Config().InstanceID, "username": "administrator", "password": integrationPassword})
	}
	fixture := filepath.Join(evidence, "fixture.private.json")
	if err := WritePrivateJSON(fixture, map[string]any{"roles": roles}); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.Remove(fixture) })
	ctx, cancel := context.WithTimeout(t.Context(), time.Minute)
	defer cancel()
	output, err := exec.CommandContext(ctx, "node", filepath.Join(repo, "tools/framework/identification-ui-check.mjs"), fixture, filepath.Join(evidence, "screenshots")).CombinedOutput()
	t.Log(string(output))
	if err != nil {
		t.Fatal(err)
	}
	for index, pair := range []*integrationService{c.site, c.otherSite} {
		var site SiteConfig
		if err := pair.service.owned(&site); err != nil {
			t.Fatal(err)
		}
		if site.Identification.Callsign != []string{"W1TEST", "W2TEST"}[index] || site.Identification.BSIProfile != "motorola" || site.Identification.Enabled {
			t.Fatal("digital identification values lost on Save", site.Identification)
		}
	}
}
