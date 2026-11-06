package radio

import (
	"fmt"
	"strings"
	"testing"
)

func TestNativeProfileSeparatesStandardVoiceMFIDFromBSIProfile(t *testing.T) {
	controller := testController(t, nil)
	network, err := controller.NetworkFor(fixtureProfile())
	if err != nil {
		t.Fatal(err)
	}
	network.RPCSecret = strings.Repeat("b", 64)
	for _, mfid := range []int{0, 1} {
		profile := fixtureProfile()
		profile.System.VoiceMFID = mfid
		profile.System.BSIProfile = "motorola"
		files, err := RenderChannel(profile, profile.Site.Channels[0], network, 41000, 41001, "null")
		if err != nil {
			t.Fatal(err)
		}
		if !strings.Contains(files["config.yml"], fmt.Sprintf("voiceMfid: %d", mfid)) || !strings.Contains(files["config.yml"], "profile: \"motorola\"") {
			t.Fatal("voice MFID and feature profile were not independently rendered")
		}
	}
	for _, mfid := range []int{-1, 0x90, 0xa4, 256} {
		profile := fixtureProfile()
		profile.System.VoiceMFID = mfid
		if err := Validate(profile); err == nil {
			t.Fatalf("unsupported MFID %X accepted", mfid)
		}
	}
	profile := fixtureProfile()
	profile.System.BSIProfile = "harris"
	if err := Validate(profile); err == nil {
		t.Fatal("unsupported BSI profile accepted")
	}
}
