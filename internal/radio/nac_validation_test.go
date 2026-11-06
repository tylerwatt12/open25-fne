package radio

import (
	"fmt"
	"strings"
	"testing"
)

func TestRendererPreservesFixedTwelveBitNACAndRejectsSpecialValues(t *testing.T) {
	base := fixtureProfile()
	controller := testController(t, nil)
	network, err := controller.NetworkFor(base)
	if err != nil {
		t.Fatal(err)
	}
	network.RPCSecret = strings.Repeat("b", 64)
	for _, test := range []struct {
		nac   int
		valid bool
	}{{0, true}, {0xF7D, true}, {0xF7E, false}, {0xF7F, false}, {0xF80, true}, {0xFFF, true}, {0x1000, false}, {-1, false}} {
		t.Run(fmt.Sprintf("%X", test.nac), func(t *testing.T) {
			profile := fixtureProfile()
			profile.Zone.Sites[0].NAC = test.nac
			files, err := RenderChannel(profile, profile.Site.Channels[0], network, 41000, 41001, "null")
			if (err == nil) != test.valid {
				t.Fatalf("fixed NAC %X accepted=%v, expected=%v: %v", test.nac, err == nil, test.valid, err)
			}
			if test.valid && !strings.Contains(files["config.yml"], fmt.Sprintf("nac: \"%03X\"", test.nac)) {
				t.Fatal("rendered NAC was truncated or replaced")
			}
		})
	}
}
