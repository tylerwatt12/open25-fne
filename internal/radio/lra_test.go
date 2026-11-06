package radio

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestLocationRegistrationAreaReachesEveryNativeChannel(t *testing.T) {
	network := Network{Address: "127.0.0.1", Port: 62031, Secret: strings.Repeat("a", 64), RPCSecret: strings.Repeat("b", 64), ControlPeerID: 100, VoicePeerIDs: map[string]int{"voice-1": 101}}
	for _, lra := range []int{0, 1, 165, 255} {
		t.Run(fmt.Sprint(lra), func(t *testing.T) {
			profile := fixtureProfile()
			profile.Zone.Sites[0].LRA = lra
			for index, channel := range profile.Site.Channels {
				files, err := RenderChannel(profile, channel, network, 25100+2*index, 25101+2*index, "null")
				if err != nil {
					t.Fatal(err)
				}
				if !strings.Contains(files["config.yml"], fmt.Sprintf("        lra: %d\n", lra)) {
					t.Fatal("native channel lost the Zone-owned LRA", channel.ID)
				}
				// A separate native fixture can consume the actual renderer output.
				if directory := os.Getenv("TRUNKSYSTEM_LRA_EVIDENCE"); directory != "" {
					if err := os.MkdirAll(directory, 0700); err != nil {
						t.Fatal(err)
					}
					name := fmt.Sprintf("%s-%d.yml", channel.Role, lra)
					if err := os.WriteFile(filepath.Join(directory, name), []byte(files["config.yml"]), 0600); err != nil {
						t.Fatal(err)
					}
				}
			}
			if profile.Zone.Sites[0].LRA != lra {
				t.Fatal("rendering changed administrator configuration")
			}
		})
	}
}

func TestLocationRegistrationAreaUnrepresentableOperationProducesNoNativeFiles(t *testing.T) {
	network := Network{Address: "127.0.0.1", Port: 62031, Secret: strings.Repeat("a", 64), RPCSecret: strings.Repeat("b", 64), ControlPeerID: 100, VoicePeerIDs: map[string]int{"voice-1": 101}}
	for _, lra := range []int{-1, 256, 257, 1 << 32} {
		t.Run(fmt.Sprint(lra), func(t *testing.T) {
			profile := fixtureProfile()
			profile.Zone.Sites[0].LRA = lra
			for index, channel := range profile.Site.Channels {
				files, err := RenderChannel(profile, channel, network, 25100+2*index, 25101+2*index, "null")
				if err == nil || !strings.Contains(err.Error(), fmt.Sprint(lra)) || files != nil {
					t.Fatalf("unrepresentable LRA produced native files or lost the attempted value: %v, %v", files, err)
				}
			}
			if profile.Zone.Sites[0].LRA != lra {
				t.Fatal("failed native encoding changed the administrator's entry")
			}
		})
	}
}

func TestLocationRegistrationAreaChangeUsesExistingPhysicalSettingsOwner(t *testing.T) {
	profile := fixtureProfile()
	profile.Zone.Sites = append(profile.Zone.Sites, SiteIdentity{ID: "site-2", SiteID: 2, LRA: 3})
	initial := physicalDigest(profile)
	profile.Zone.Sites[1].LRA = 4
	if physicalDigest(profile) != initial {
		t.Fatal("another Site's LRA replaced this Site's physical settings")
	}
	profile.Zone.Sites[0].LRA = 165
	if physicalDigest(profile) == initial {
		t.Fatal("local LRA change was omitted from native settings replacement")
	}
}
