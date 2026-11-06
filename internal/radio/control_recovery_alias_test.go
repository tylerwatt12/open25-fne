//go:build linux || darwin

package radio

import (
	"context"
	"encoding/csv"
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
	"time"
)

func TestControlRecoveryAliasUpdateDuringRetryReachesRecoveredNativeLookups(t *testing.T) {
	c := testController(t, nil)
	waiting, retry := holdSiteRetries(c)
	p := fixtureProfile()
	p.Site.ControlPolicy.Mode = "static"
	if err := c.Start(context.Background(), p, "null"); err != nil {
		t.Fatal(err)
	}
	oldControl := controlOwner(c)
	if err := syscall.Kill(oldControl.cmd.Process.Pid, syscall.SIGKILL); err != nil {
		t.Fatal(err)
	}
	nextSiteRetry(t, waiting, time.Second)
	if c.Status().State != "fault" {
		t.Fatal("alias update fixture did not enter the between-retry fault state")
	}
	const updatedAlias = "Updated Radio"
	p.System.Radios[0].Alias = updatedAlias
	if err := c.ApplyPolicy(context.Background(), p); err != nil {
		t.Fatal("an alias-only update was rejected during recovery:", err)
	}
	retry <- struct{}{}
	waitRadio(t, "control did not recover after the alias update", func() bool { return c.Status().State == "running" })
	assertRecoveredAliases(t, c, updatedAlias)
}

func TestControlRecoveryAliasUpdateAfterStaticManualPromotionKeepsSelectedControl(t *testing.T) {
	c := testController(t, nil)
	waiting, retry := holdSiteRetries(c)
	p := selectionProfile()
	p.Site.ControlPolicy.Mode = "static"
	if err := c.Start(context.Background(), p, "null"); err != nil {
		t.Fatal(err)
	}
	if err := c.SwitchControl(context.Background(), "voice-1"); err != nil {
		t.Fatal(err)
	}
	waitSelected(t, c, "voice-1")
	promotedControl := selectionWorker(c, "voice-1")
	if err := syscall.Kill(promotedControl.cmd.Process.Pid, syscall.SIGKILL); err != nil {
		t.Fatal(err)
	}
	nextSiteRetry(t, waiting, time.Second)
	const updatedAlias = "Updated Radio"
	// This is the original saved profile: control-1 still has the assigned
	// control role even though voice-1 is the current operating control.
	p.System.Radios[0].Alias = updatedAlias
	if err := c.ApplyPolicy(context.Background(), p); err != nil {
		t.Fatal("an alias-only update was rejected after manual promotion:", err)
	}
	c.mu.Lock()
	recovering := c.siteRecovery != nil && c.siteWanted
	c.mu.Unlock()
	if !recovering {
		t.Fatalf("alias-only update cancelled selected-control recovery: %+v", c.Status())
	}
	select {
	case retry <- struct{}{}:
	case <-time.After(2 * time.Second):
		t.Fatalf("selected-control recovery stopped accepting its retry: %+v", c.Status())
	}
	waitSelected(t, c, "voice-1")
	if selectedChannel(c, "control-1").Role != "voice" {
		t.Fatal("alias refresh silently restored the original assigned control role")
	}
	assertSelectionUnchanged(t, c, p)
	assertRecoveredAliases(t, c, updatedAlias)
}

func assertRecoveredAliases(t *testing.T, c *Controller, updatedAlias string) {
	t.Helper()
	c.mu.Lock()
	activeAlias := c.profile.System.Radios[0].Alias
	configuredAlias := c.configuredProfile.System.Radios[0].Alias
	workers := make(map[string]string, len(c.children))
	for id, worker := range c.children {
		workers[id] = worker.dir
	}
	c.mu.Unlock()
	if activeAlias != updatedAlias || configuredAlias != updatedAlias {
		t.Errorf("recovery replayed an older alias snapshot: active=%q configured=%q, want %q", activeAlias, configuredAlias, updatedAlias)
	}
	for id, directory := range workers {
		raw, err := os.ReadFile(filepath.Join(directory, "rid_acl.dat"))
		if err != nil {
			t.Fatal(err)
		}
		rows, err := csv.NewReader(strings.NewReader(string(raw))).ReadAll()
		if err != nil {
			t.Fatal(err)
		}
		found := false
		for _, row := range rows {
			if len(row) >= 3 && row[0] == "10001" {
				found = true
				if row[2] != updatedAlias {
					t.Errorf("recovered worker %s has RID10001 alias=%q, want %q", id, row[2], updatedAlias)
				}
			}
		}
		if !found {
			t.Errorf("recovered worker %s omitted the configured subscriber", id)
		}
	}
}
