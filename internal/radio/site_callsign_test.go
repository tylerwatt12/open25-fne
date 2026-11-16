package radio

import (
	"strings"
	"testing"
)

func TestSiteCallsignReachesEveryNativeWorker(t *testing.T) {
	network := Network{Address: "127.0.0.1", Port: 62031, Secret: strings.Repeat("a", 64), RPCSecret: strings.Repeat("b", 64), PeerBase: 100, ChannelPeerIDs: map[string]int{"control-1": 100, "voice-1": 101}}
	for _, callsign := range []string{"N0TEST", "K9TEST"} {
		t.Run(callsign, func(t *testing.T) {
			p := fixtureProfile()
			before := RFConfigurationSignature(p)
			p.Site.Identification.Callsign = callsign
			if callsign != "N0TEST" && RFConfigurationSignature(p) == before {
				t.Fatal("callsign edit did not require explicit RF restart")
			}
			for i, ch := range p.Site.Channels {
				files, err := RenderChannel(p, ch, network, 25100+2*i, 25101+2*i, "null")
				if err != nil {
					t.Fatal(err)
				}
				if !strings.Contains(files["config.yml"], `identity: "`+callsign+`"`) {
					t.Fatal("native station and BSI callsigns did not use the owning Site", ch.ID)
				}
			}
			files, err := RenderFNE(p, network)
			if err != nil || !strings.Contains(files["fne.yml"], `"`+callsign+`"`) {
				t.Fatal("Site-local media identity lost callsign", err)
			}
		})
	}
}
