package framework

import (
	"testing"

	"open25.local/open25-fne/internal/radio"
)

func TestNativeEmergencyEventAdmissionUpdatesSiteIncidents(t *testing.T) {
	for _, test := range []struct {
		name string
		line string
		want int
	}{
		{"accepted", "I: (RF) P25, (Emergency Alarm Request), si1 = $01, si2 = $02, srcId = 10001, dstId = 1001", 1},
		{"denied", "W: (RF) P25, emergency alarm denial, reason = $30, srcId = 10001, dstId = 1001", 0},
	} {
		t.Run(test.name, func(t *testing.T) {
			s, _, _ := operatorFixture(t, RoleSite)
			event := radio.ParseEvent(test.line)
			if event == nil {
				t.Fatal("the native event was not parsed")
			}
			s.radioEvent(*event)
			incidents := s.operatorSnapshot().Emergencies
			if len(incidents) != test.want {
				t.Fatalf("got %d incidents, want %d", len(incidents), test.want)
			}
			if test.want > 0 && (incidents[0].RadioID != 10001 || incidents[0].TalkgroupID != 1001 || incidents[0].SourceKind != "rf") {
				t.Fatalf("unexpected incident: %+v", incidents[0])
			}
		})
	}
}
