package radio

import (
	"context"
	"slices"
	"testing"
)

func TestRandomControlPoolBalancesTurnsAndIgnoresStorageOrder(t *testing.T) {
	p := selectionProfileThree()
	p.Site.ControlPolicy.Mode = "random"
	c := &Controller{}
	ids := []string{"control-1", "voice-1", "voice-2"}
	previous := ""
	counts := map[string]int{}
	for round := 0; round < 40; round++ {
		seen := map[string]bool{}
		for turn := 0; turn < 3; turn++ {
			id := c.chooseControlLocked(p, ids, previous)
			if id == "" || id == previous || seen[id] {
				t.Fatalf("round %d repeated or skipped a candidate: %q %+v", round, id, seen)
			}
			seen[id] = true
			counts[id]++
			previous = id
		}
	}
	for _, id := range ids {
		if counts[id] != 40 {
			t.Fatal("available candidates did not get equal turns", counts)
		}
	}
	// A missing resource cannot block other resources and can rejoin later.
	for i := 0; i < 8; i++ {
		previous = c.chooseControlLocked(p, ids[:2], previous)
		if !slices.Contains(ids[:2], previous) {
			t.Fatal("unavailable candidate selected")
		}
	}
	p.Site.ControlPolicy.CandidateIDs = []string{"voice-2"}
	if got := c.chooseControlLocked(p, []string{"voice-2"}, ""); got != "voice-2" {
		t.Fatal("changed pool retained stale turns", got)
	}
}

func TestStaticControlStartFallsBackWithoutChangingProgramming(t *testing.T) {
	c := testController(t, nil)
	p := selectionProfile()
	t.Setenv("RADIO_FAIL_CHANNEL", "control-1")
	if err := c.Start(context.Background(), p, "null"); err != nil {
		t.Fatal("static startup did not fall back", err)
	}
	if status := c.Status(); status.ActiveControlChannelID != "voice-1" || !selectedChannel(c, "voice-1").Ready || selectedChannel(c, "control-1").Ready {
		t.Fatal("fallback did not expose degraded channel readiness", status)
	}
	assertSelectionUnchanged(t, c, p)
}

func TestStaticSelectionDoesNotScheduleRotation(t *testing.T) {
	c := testController(t, nil)
	p := selectionProfile()
	p.Site.ControlPolicy.RotationHours = 0.1
	if err := c.Start(context.Background(), p, "null"); err != nil {
		t.Fatal(err)
	}
	if !c.Status().NextControlRotation.IsZero() {
		t.Fatal("static selection scheduled random rotation")
	}
}

func TestControlPoolOrderIsNotProgrammingPriority(t *testing.T) {
	p := selectionProfileThree()
	before := RFConfigurationSignature(p)
	slices.Reverse(p.Site.ControlPolicy.CandidateIDs)
	if RFConfigurationSignature(p) != before {
		t.Fatal("reordering an unordered pool changed RF programming")
	}
}
