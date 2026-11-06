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

func TestMessagingBrowserRealApplicationServer(t *testing.T) {
	if os.Getenv("TRUNKSYSTEM_MESSAGING_BROWSER_TEST") != "1" {
		t.Skip("set TRUNKSYSTEM_MESSAGING_BROWSER_TEST=1 for real Application Server browser messaging checks")
	}
	f := newMessagingHTTPFixture(t)
	f.cluster.dispatch.service.assets = frameworkui.Assets()
	repo, err := filepath.Abs("../..")
	if err != nil {
		t.Fatal(err)
	}
	evidence := os.Getenv("TRUNKSYSTEM_MESSAGING_BROWSER_EVIDENCE")
	if evidence == "" {
		evidence = filepath.Join(repo, "build", "messaging-ui-test", "browser")
	}
	if err = os.MkdirAll(evidence, 0700); err != nil {
		t.Fatal(err)
	}
	fixture := struct {
		Origin   string          `json:"origin"`
		Password string          `json:"password"`
		People   []messagePerson `json:"people"`
	}{Origin: f.cluster.dispatch.web.URL, Password: messagingHTTPPassword, People: []messagePerson{{ID: f.desk.ID, Username: f.desk.Username}, {ID: f.ptt.ID, Username: f.ptt.Username}, {ID: f.outsider.ID, Username: f.outsider.Username}}}
	fixturePath := filepath.Join(evidence, "fixture.private.json")
	if err = WritePrivateJSON(fixturePath, fixture); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.Remove(fixturePath) })
	ctx, cancel := context.WithTimeout(t.Context(), 3*time.Minute)
	defer cancel()
	command := exec.CommandContext(ctx, "node", filepath.Join(repo, "tools", "framework", "messaging-ui-check.mjs"), fixturePath, evidence)
	output, err := command.CombinedOutput()
	t.Logf("Real Application Server messaging browser checks: %s", output)
	if err != nil {
		t.Fatal(err)
	}
}
