package radio

import (
	"strings"
	"testing"
)

func TestDigitalIdentificationConfigurationOnEveryChannel(t *testing.T) {
	network := Network{Address: "127.0.0.1", Port: 62031, Secret: strings.Repeat("a", 64), RPCSecret: strings.Repeat("b", 64), PeerBase: 100, ChannelPeerIDs: map[string]int{"control-1": 100, "voice-1": 101}}
	for _, enabled := range []bool{false, true} {
		for _, mode := range []string{"null", "hardware"} {
			p := fixtureProfile()
			p.Site.Identification.Enabled = enabled
			p.Site.Identification.BSIProfile = "motorola"
			for i, channel := range p.Site.Channels {
				files, err := RenderChannel(p, channel, network, 25100+2*i, 25101+2*i, mode)
				if err != nil {
					t.Fatal(err)
				}
				config := files["config.yml"]
				if strings.Contains(config, "cwId:") || strings.Contains(config, "quietSeconds:") || strings.Contains(config, "drainSeconds:") {
					t.Fatal("retired CW settings reached a native worker", channel.ID)
				}
				want := "    bsi:\n        enable: false\n        profile: \"motorola\""
				if enabled && mode == "hardware" {
					want = strings.Replace(want, "enable: false", "enable: true", 1)
				}
				if !strings.Contains(config, want) {
					t.Fatal("native BSI switch lost the Site setting or operation mode", channel.ID, enabled, mode)
				}
				if !strings.Contains(config, `identity: "N0TEST"`) {
					t.Fatal("native station identity lost its Site callsign")
				}
			}
		}
	}
}

func TestDigitalIdentificationUsesOnlyOwningSiteCallsign(t *testing.T) {
	network := Network{Address: "127.0.0.1", Port: 62031, Secret: strings.Repeat("a", 64), RPCSecret: strings.Repeat("b", 64), PeerBase: 100, ChannelPeerIDs: map[string]int{"control-1": 100, "voice-1": 101}}
	for _, callsign := range []string{"W1TEST", "W2TEST", "W1TEST"} {
		p := fixtureProfile()
		p.Site.Identification.Callsign = callsign
		p.Site.Identification.Enabled = true
		for i, channel := range p.Site.Channels {
			files, err := RenderChannel(p, channel, network, 25100+2*i, 25101+2*i, "hardware")
			if err != nil {
				t.Fatal(err)
			}
			if !strings.Contains(files["config.yml"], `identity: "`+callsign+`"`) {
				t.Fatal("worker did not use its owning Site callsign", channel.ID, callsign)
			}
		}
	}
}
