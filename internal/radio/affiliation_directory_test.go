package radio

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestAffiliationDirectoryKeepsGlobalValidityAndSiteAdmissionSeparate(t *testing.T) {
	profile := fixtureProfile()
	excluded, disabled := profile.System.Talkgroups[0], profile.System.Talkgroups[0]
	excluded.TGID, excluded.Alias, excluded.SiteAccess, excluded.SiteIDs = 1002, "Other Site", "selected", []string{"elsewhere"}
	disabled.TGID, disabled.Alias, disabled.Enabled = 1003, "Disabled", false
	profile.System.Talkgroups = []Talkgroup{profile.System.Talkgroups[0], excluded, disabled}
	rules := RenderLookups(profile)["talkgroup_rules.yml"]
	for _, want := range []string{
		"name: \"Other Site\"\n    alias: \"Other Site\"\n    config:\n      active: false\n      system_valid: true\n",
		"name: \"Disabled\"\n    alias: \"Disabled\"\n    config:\n      active: false\n      system_valid: false\n",
	} {
		if !strings.Contains(rules, want) {
			t.Fatalf("missing classified directory rule %q:\n%s", want, rules)
		}
	}
	if got := allowedGroups(profile); len(got) != 2 || got[0].TGID != 1001 || got[1].TGID != 1003 {
		t.Fatalf("global validity added a Site-excluded group to Site admission: %#v", got)
	}
	// Actual native-parser regression fixtures are generated only on request.
	if directory := os.Getenv("TRUNKSYSTEM_AFFILIATION_DIRECTORY_EVIDENCE"); directory != "" {
		if err := os.MkdirAll(directory, 0700); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(directory, "talkgroup_rules.yml"), []byte(rules), 0600); err != nil {
			t.Fatal(err)
		}
	}
	controller := testController(t, nil)
	controller.profile, controller.status.InhibitionReady = profile, true
	controller.affiliations[10001] = Affiliation{RID: 10001, TGID: 1002}
	if controller.HasRFInterest(1002) {
		t.Fatal("global-only group created local listener demand")
	}
	network := Network{Address: "127.0.0.1", Port: 62031, Secret: strings.Repeat("a", 64), RPCSecret: strings.Repeat("b", 64), ControlPeerID: 100, VoicePeerIDs: map[string]int{"voice-1": 101}}
	files, err := RenderChannel(profile, profile.Site.Channels[0], network, 25100, 25101, "null")
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(files["config.yml"], "talkgroup: 1002") || strings.Count(files["config.yml"], "talkgroup: 1001") != 2 {
		t.Fatal("Site priority/security policies lost their local group boundary")
	}
	fne, err := RenderFNE(profile, network)
	if err != nil {
		t.Fatal(err)
	}
	if directory := os.Getenv("TRUNKSYSTEM_AFFILIATION_DIRECTORY_EVIDENCE"); directory != "" {
		for name, content := range map[string]string{"control.yml": files["config.yml"], "fne.yml": fne["fne.yml"]} {
			if err := os.WriteFile(filepath.Join(directory, name), []byte(content), 0600); err != nil {
				t.Fatal(err)
			}
		}
	}
	if len(profile.System.Talkgroups) != 3 || profile.System.Talkgroups[1].SiteIDs[0] != "elsewhere" {
		t.Fatal("renderer changed saved directory")
	}
}

func TestGlobalAffiliationDirectoryChangesReplaceNativePolicy(t *testing.T) {
	profile := fixtureProfile()
	excluded := profile.System.Talkgroups[0]
	excluded.TGID, excluded.SiteAccess, excluded.SiteIDs = 1002, "selected", []string{"elsewhere"}
	profile.System.Talkgroups = append(profile.System.Talkgroups, excluded)
	initial := PolicySignature(profile)
	profile.System.Talkgroups[len(profile.System.Talkgroups)-1].Alias = "Display only"
	if PolicySignature(profile) != initial {
		t.Fatal("excluded alias changed admission policy")
	}
	profile.System.Talkgroups[len(profile.System.Talkgroups)-1].Enabled = false
	if PolicySignature(profile) == initial {
		t.Fatal("excluded global validity change did not replace native policy")
	}
	profile.System.Talkgroups[len(profile.System.Talkgroups)-1].Enabled = true
	profile.System.Talkgroups[len(profile.System.Talkgroups)-1].SiteIDs = []string{profile.Site.ID}
	if PolicySignature(profile) == initial {
		t.Fatal("Site admission change did not replace native policy")
	}
}
