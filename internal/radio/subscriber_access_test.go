package radio

import (
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"
)

// The external contract runner feeds these actual rendered files to the pinned
// native YAML parser and policy classes, rather than a second Go YAML parser.
func TestNativeYAMLContractFixtures(t *testing.T) {
	directory := os.Getenv("TRUNKSYSTEM_NATIVE_YAML_EVIDENCE")
	if directory == "" {
		t.Skip("run tools/native_fixture/test_yaml_contract.py with the pinned native source")
	}
	profile := fixtureProfile()
	profile.System.Radios = append(profile.System.Radios,
		Radio{RID: 10003, Kind: "subscriber", Enabled: true},
		Radio{RID: 10004, Kind: "subscriber", Enabled: false},
		Radio{RID: 10005, Kind: "mobile", Enabled: true},
		Radio{RID: 10006, Kind: "automation", Enabled: true})
	all := profile.System.Talkgroups[0]
	selected, empty := all, all
	selected.TGID, selected.AffiliationAccess, selected.TransmitAccess = 1002, "selected", "selected"
	selected.AffiliationRadioIDs, selected.TransmitRadioIDs = []int{10003, 10001}, []int{10003}
	selected.StartPolicy = "all_start"
	empty.TGID, empty.AffiliationAccess, empty.TransmitAccess = 1003, "selected", "selected"
	profile.System.Talkgroups = []Talkgroup{all, selected, empty}
	profile.Inhibition = InhibitPolicy{Desired: []int{10001}, Restore: []InhibitRestore{{RadioID: 10003, Blocked: true, PendingUninhibit: true}}}
	// Explicitly synthetic credentials; these files cannot address a modem.
	network := Network{Address: "127.0.0.1", Port: 60001, Secret: strings.Repeat("a", 64), RPCSecret: strings.Repeat("b", 64), ControlPeerID: 70000, VoicePeerIDs: map[string]int{"voice-1": 70001}, ConsolePeerIDs: map[int]int{1001: 71001, 1002: 71002, 1003: 71003}}
	write := func(name string, p Profile, n Network) {
		t.Helper()
		files, err := RenderFNE(p, n)
		if err != nil {
			t.Fatal(err)
		}
		control, err := RenderChannel(p, p.Site.Channels[0], n, 18000, 18001, "null")
		if err != nil {
			t.Fatal(err)
		}
		files["control.yml"] = control["config.yml"]
		path := filepath.Join(directory, name)
		if err = os.MkdirAll(path, 0700); err != nil {
			t.Fatal(err)
		}
		for file, value := range files {
			if err = os.WriteFile(filepath.Join(path, file), []byte(value), 0600); err != nil {
				t.Fatal(err)
			}
		}
	}
	write("populated", profile, network)
	profile.System.Radios = []Radio{{RID: 10002, Kind: "console", Enabled: true}}
	profile.Inhibition = InhibitPolicy{}
	profile.Site.Channels = profile.Site.Channels[:1]
	for i := range profile.System.Talkgroups {
		profile.System.Talkgroups[i].AffiliationAccess, profile.System.Talkgroups[i].TransmitAccess = "all", "all"
		profile.System.Talkgroups[i].AffiliationRadioIDs, profile.System.Talkgroups[i].TransmitRadioIDs = nil, nil
		profile.System.Talkgroups[i].StartPolicy = ""
	}
	network.VoicePeerIDs = map[string]int{}
	write("empty-physical", profile, network)
	profile.System.Talkgroups = nil
	if err := os.WriteFile(filepath.Join(directory, "empty-group-rules.yml"), []byte(RenderLookups(profile)["talkgroup_rules.yml"]), 0600); err != nil {
		t.Fatal(err)
	}
}

func TestRenderedSubscriberRulesKeepIndependentPhysicalSelections(t *testing.T) {
	profile := fixtureProfile()
	profile.System.Radios = append(profile.System.Radios, Radio{RID: 10003, Kind: "subscriber", Alias: "Other", Enabled: true}, Radio{RID: 10004, Kind: "mobile", Alias: "Mobile app", Enabled: true}, Radio{RID: 10005, Kind: "automation", Alias: "Announcement", Enabled: true}, Radio{RID: 10006, Kind: "console", Alias: "Disabled desk"})
	group := &profile.System.Talkgroups[0]
	group.AffiliationAccess = "selected"
	group.AffiliationRadioIDs = []int{10003, 10001}
	group.TransmitAccess = "selected"
	group.TransmitRadioIDs = []int{}
	if err := Validate(profile); err != nil {
		t.Fatal(err)
	}
	rules := RenderLookups(profile)["talkgroup_rules.yml"]
	for _, expected := range []string{
		"rid_permitted: []",
		"affiliation:\n          restricted: true\n          radio_ids:\n            - 10001\n            - 10003",
		"transmit:\n          restricted: true\n          radio_ids: []",
	} {
		if !strings.Contains(rules, expected) {
			t.Fatalf("missing native subscriber rule %q:\n%s", expected, rules)
		}
	}
	for _, appID := range []string{"10002", "10004", "10005", "10006"} {
		if strings.Contains(rules, appID) {
			t.Fatal("application identity entered a physical subscriber rule")
		}
	}
	if !slices.Equal(group.AffiliationRadioIDs, []int{10003, 10001}) {
		t.Fatal("renderer mutated the saved subscriber list")
	}
	group.AffiliationAccess = "all"
	group.AffiliationRadioIDs = nil
	if !strings.Contains(RenderLookups(profile)["talkgroup_rules.yml"], "affiliation:\n          restricted: true\n          radio_ids:\n            - 10001\n            - 10003") {
		t.Fatal("All did not resolve to the enabled physical inventory")
	}
}

func TestNativeSubscriberRuleValidationRejectsUnknownApplicationAndDuplicateIDs(t *testing.T) {
	for _, ids := range [][]int{{0}, {0xFFFFFC}, {999}, {10002}, {10001, 10001}} {
		for _, affiliation := range []bool{false, true} {
			profile := fixtureProfile()
			g := &profile.System.Talkgroups[0]
			if affiliation {
				g.AffiliationAccess = "selected"
				g.AffiliationRadioIDs = ids
			} else {
				g.TransmitAccess = "selected"
				g.TransmitRadioIDs = ids
			}
			if err := Validate(profile); err == nil {
				t.Fatalf("invalid subscriber IDs accepted: %v", ids)
			}
		}
	}
	profile := fixtureProfile()
	profile.System.Talkgroups[0].AffiliationAccess = "selected"
	profile.System.Talkgroups[0].TransmitAccess = "selected"
	if err := Validate(profile); err != nil {
		t.Fatal("explicit deny-all rule rejected", err)
	}
}

func TestNativeRFInterestIgnoresNewlyDeniedAffiliationButKeepsReceiveOnly(t *testing.T) {
	controller := testController(t, nil)
	controller.profile = fixtureProfile()
	controller.status.InhibitionReady = true
	controller.affiliations[10001] = Affiliation{RID: 10001, TGID: 1001}
	controller.profile.System.Talkgroups[0].TransmitAccess = "selected"
	if !controller.HasRFInterest(1001) {
		t.Fatal("transmit denial removed an allowed receiving subscriber")
	}
	controller.profile.System.Talkgroups[0].AffiliationAccess = "selected"
	if controller.HasRFInterest(1001) {
		t.Fatal("denied retained affiliation counted as RF demand")
	}
	if len(controller.affiliations) != 1 {
		t.Fatal("permission change erased the underlying affiliation observation")
	}
	controller.profile.System.Talkgroups[0].AffiliationRadioIDs = []int{10001}
	if !controller.HasRFInterest(1001) {
		t.Fatal("permitted affiliation did not restore RF demand")
	}
}

func TestSubscriberPolicySignatureCanonicalizesSelectionOrderAndDefaults(t *testing.T) {
	profile := fixtureProfile()
	base := PolicySignature(profile)
	profile.System.Talkgroups[0].AffiliationAccess = "all"
	profile.System.Talkgroups[0].TransmitAccess = "all"
	if PolicySignature(profile) != base {
		t.Fatal("ordinary all-access defaults forced native replacement")
	}
	profile.System.Talkgroups[0].AffiliationAccess = "selected"
	profile.System.Talkgroups[0].AffiliationRadioIDs = []int{10003, 10001}
	first := PolicySignature(profile)
	profile.System.Talkgroups[0].AffiliationRadioIDs = []int{10001, 10003}
	if PolicySignature(profile) != first {
		t.Fatal("selection order forced native replacement")
	}
	profile.System.Talkgroups[0].AffiliationRadioIDs = []int{10001}
	if PolicySignature(profile) == first {
		t.Fatal("changed affiliation selection did not replace policy")
	}
}

func TestEveryNativeHostReceivesPhysicalInventoryIndependentOfEnabled(t *testing.T) {
	profile := fixtureProfile()
	profile.System.Radios = append(profile.System.Radios, Radio{RID: 10003, Kind: "subscriber", Alias: "Disabled physical"}, Radio{RID: 10004, Kind: "mobile", Alias: "Mobile app", Enabled: true}, Radio{RID: 10005, Kind: "automation", Alias: "Announcement", Enabled: true})
	network := Network{Address: "127.0.0.1", Port: 62031, Secret: strings.Repeat("a", 64), RPCSecret: strings.Repeat("b", 64), ControlPeerID: 100, VoicePeerIDs: map[string]int{"voice-1": 101}}
	for i, ch := range profile.Site.Channels {
		files, err := RenderChannel(profile, ch, network, 25100+i*2, 25101+i*2, "null")
		if err != nil {
			t.Fatal(err)
		}
		config := files["config.yml"]
		if !strings.Contains(config, "dispatchSubscribers:\n            - 10001\n") {
			t.Fatalf("%s omitted physical source inventory", ch.Role)
		}
		start := strings.Index(config, "dispatchSubscribers:")
		end := strings.Index(config[start:], "dispatchTalkgroupPriorities:")
		if end < 0 {
			t.Fatal("test could not locate inventory section")
		}
		inventory := config[start : start+end]
		if !strings.Contains(inventory, "- 10003\n") {
			t.Fatal("disabled physical radio omitted from inhibition inventory")
		}
		for _, id := range []string{"10002", "10004", "10005"} {
			if strings.Contains(inventory, id) {
				t.Fatalf("%s inventory includes app identity %s", ch.Role, id)
			}
		}
	}
}

func TestNativeAllSubscriberAccessWithNoPhysicalRadiosDeniesRawRF(t *testing.T) {
	profile := fixtureProfile()
	profile.System.Radios = profile.System.Radios[1:]
	rules := RenderLookups(profile)["talkgroup_rules.yml"]
	for _, rule := range []string{"affiliation", "transmit"} {
		if !strings.Contains(rules, rule+":\n          restricted: true\n          radio_ids: []") {
			t.Fatal("empty physical inventory became native unrestricted access")
		}
	}
}

func TestSubscriberNativeDenialsNeverCreateAcceptedAffiliationOrRFDemand(t *testing.T) {
	for _, reason := range []string{"physical subscriber denial", "subscriber transmit denial", "subscriber affiliation denial", "subscriber location affiliation denial", "subscriber transmit denial after policy change"} {
		t.Run(reason, func(t *testing.T) {
			events := make(chan Event, 4)
			controller := testController(t, events)
			controller.mu.Lock()
			controller.profile = fixtureProfile()
			controller.status.State = "running"
			controller.mu.Unlock()
			line := fmt.Sprintf("W: (RF) P25, TSDU (Trunking System Data Unit), %s, srcId = 10001, dstId = 1001", reason)
			parsed := ParseEvent(line)
			if parsed == nil || parsed.Kind != "request_denied" || parsed.Audience != "system_operator" || parsed.RID != 10001 || parsed.TGID != 1001 || strings.Contains(parsed.Message, "srcId") || strings.Contains(parsed.Message, "TSDU") {
				t.Fatalf("denial lost its administrator observation: %#v", parsed)
			}
			controller.observe(currentObservedChild(t, controller, "control-1", "control"), line)
			if len(controller.Affiliations()) != 0 || len(controller.Status().Registrations) != 0 || controller.HasRFInterest(1001) {
				t.Fatal("denied request became accepted RF presence")
			}
			select {
			case event := <-events:
				if event.Kind != "request_denied" || event.Audience != "system_operator" {
					t.Fatal("denial changed event category", event)
				}
			case <-time.After(time.Second):
				t.Fatal("denial observation missing")
			}
		})
	}
}

func TestRenderedFNERejectsUnknownIdentitiesEvenOnTrustedConsolePeers(t *testing.T) {
	profile := fixtureProfile()
	files, err := RenderFNE(profile, Network{Address: "127.0.0.1", Port: 62031, Secret: strings.Repeat("a", 64), ControlPeerID: 100, VoicePeerIDs: map[string]int{"voice-1": 101}, ConsolePeerIDs: map[int]int{10002: 102}})
	if err != nil {
		t.Fatal(err)
	}
	parts := strings.SplitN(files["fne.yml"], "master:\n", 2)
	if len(parts) != 2 {
		t.Fatal("native master configuration missing")
	}
	master := strings.SplitN(parts[1], "\npeers:", 2)[0]
	if !strings.Contains(master, "\n    rejectUnknownRID: true\n") || strings.Count(files["fne.yml"], "rejectUnknownRID:") != 1 {
		t.Fatal("unknown-RID protection omitted or placed outside native master options")
	}
}
