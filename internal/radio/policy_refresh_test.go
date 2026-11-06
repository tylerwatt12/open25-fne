package radio

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
)

func TestNativeSecurityPolicyChangeWithdrawsAndReloadsEngines(t *testing.T) {
	for _, cause := range []string{"mode", "affiliation-access", "affiliation-list", "transmit-access", "transmit-list"} {
		t.Run(cause, func(t *testing.T) {
			controller := testController(t, nil)
			profile := fixtureProfile()
			group := &profile.System.Talkgroups[0]
			group.StartPolicy = "fast_start"
			group.Priority = 10
			group.AdmissionMode = "both"
			group.TransmitMode = "selectable"
			group.AlgorithmID = 132
			group.KeyID = 7
			if cause == "affiliation-list" {
				group.AffiliationAccess = "selected"
				group.AffiliationRadioIDs = []int{10001}
			}
			if cause == "transmit-list" {
				group.TransmitAccess = "selected"
				group.TransmitRadioIDs = []int{10001}
			}
			if err := controller.Start(context.Background(), profile, "null"); err != nil {
				t.Fatal(err)
			}
			previous := controller.Status()
			changed := cloneProfile(profile)
			switch cause {
			case "affiliation-access":
				changed.System.Talkgroups[0].AffiliationAccess = "selected"
			case "affiliation-list":
				changed.System.Talkgroups[0].AffiliationRadioIDs = []int{}
			case "transmit-access":
				changed.System.Talkgroups[0].TransmitAccess = "selected"
			case "transmit-list":
				changed.System.Talkgroups[0].TransmitRadioIDs = []int{}
			case "mode":
				changed.System.Talkgroups[0].AdmissionMode = "secure"
				changed.System.Talkgroups[0].TransmitMode = "secure"
			}
			if err := controller.ApplyPolicy(context.Background(), changed); err != nil {
				t.Fatal(err)
			}
			current := controller.Status()
			if current.State != "running" || current.Channels[0].PID == previous.Channels[0].PID {
				t.Fatal("changed native security retained the old process")
			}
			for _, channel := range previous.Channels {
				if syscall.Kill(channel.PID, 0) == nil {
					t.Fatal("an engine with old security survived withdrawal")
				}
			}
			controller.mu.Lock()
			directory := controller.rootDir
			controller.mu.Unlock()
			config, err := os.ReadFile(filepath.Join(directory, changed.Site.Channels[0].ID, "config.yml"))
			if err != nil {
				t.Fatal(err)
			}
			applied := changed.System.Talkgroups[0]
			mode := applied.AdmissionMode
			if mode == "both" {
				mode = "selectable"
			}
			expected := fmt.Sprintf("- talkgroup: %d\n              mode: %s", applied.TGID, quoted(mode))
			if !strings.Contains(string(config), expected) {
				t.Fatal("replacement native configuration retained the old talkgroup security rule")
			}

		})
	}
}

func TestNativePolicySignatureIncludesCallRulesButNotDisplayAliases(t *testing.T) {
	profile := fixtureProfile()
	original := PolicySignature(profile)
	for name, change := range map[string]func(*Talkgroup){
		"affiliation": func(g *Talkgroup) { g.AffiliationAccess = "selected" },
		"transmit":    func(g *Talkgroup) { g.TransmitAccess = "selected" },
		"start":       func(g *Talkgroup) { g.StartPolicy = "all_start" },
		"priority":    func(g *Talkgroup) { g.Priority = 2 },
		"mode":        func(g *Talkgroup) { g.AdmissionMode = "secure"; g.TransmitMode = "secure" },
	} {
		t.Run(name, func(t *testing.T) {
			changed := cloneProfile(profile)
			change(&changed.System.Talkgroups[0])
			if PolicySignature(changed) == original {
				t.Fatal("changed native rule was omitted from policy identity")
			}
		})
	}
	changed := cloneProfile(profile)
	changed.System.Radios[0].Alias = "Renamed radio"
	changed.System.Talkgroups[0].Alias = "Renamed group"
	changed.System.Talkgroups[0].StartPolicy = "fast_start"
	changed.System.Talkgroups[0].Priority = 10
	changed.System.Talkgroups[0].AdmissionMode = "clear"
	changed.System.Talkgroups[0].TransmitMode = "secure"
	changed.System.Talkgroups[0].AlgorithmID = 129
	changed.System.Talkgroups[0].KeyID = 29
	if PolicySignature(changed) != original {
		t.Fatal("display/default normalization caused RF policy replacement")
	}
}

func TestConsoleTransmitChangesKeepNativeEnginesRunning(t *testing.T) {
	controller := testController(t, nil)
	profile := fixtureProfile()
	if err := controller.Start(context.Background(), profile, "null"); err != nil {
		t.Fatal(err)
	}
	before := controller.Status()
	changed := cloneProfile(profile)
	changed.System.Talkgroups[0].TransmitMode = "secure"
	changed.System.Talkgroups[0].AlgorithmID, changed.System.Talkgroups[0].KeyID = 132, 7
	if err := controller.ApplyPolicy(context.Background(), changed); err != nil {
		t.Fatal(err)
	}
	after := controller.Status()
	if after.State != "running" || after.Channels[0].PID != before.Channels[0].PID {
		t.Fatal("console transmit programming interrupted native RF operation")
	}
}
