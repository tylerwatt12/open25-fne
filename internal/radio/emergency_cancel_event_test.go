package radio

import "testing"

func TestEmergencyCancelRequiresAcceptedNativeOutcome(t *testing.T) {
	accepted := "I: (RF) P25, TSDU (Trunking System Data Unit), Emergency Alarm Cancel Accepted, srcId = 10001, dstId = 1001"
	e := ParseEvent(accepted)
	if e == nil || e.Kind != "emergency_cancel" || e.RID != 10001 || e.TGID != 1001 || e.Direction != "received" {
		t.Fatalf("accepted cancellation lost its actual identity: %+v", e)
	}
	for _, line := range []string{
		"I: (RF) P25, TSDU (Cancel Service Request), srcId = 10001, dstId = 1001, serviceType = $27",
		"I: (RF) P25, TSDU (Trunking System Data Unit), Emergency Alarm Cancel Accepted, srcId = 10001, dstId = 0",
		"I: (RF) P25, TSDU (Trunking System Data Unit), Emergency Alarm Cancel Accepted, srcId = 10001, dstId = 65535",
		"I: (NET) P25, TSDU (Trunking System Data Unit), Emergency Alarm Cancel Accepted, srcId = 10001, dstId = 1001",
		"W: (RF) P25, Emergency Alarm Cancel Denial, srcId = 10001, dstId = 1001, reason = $10",
	} {
		if e := ParseEvent(line); e != nil && e.Kind == "emergency_cancel" {
			t.Fatalf("nonaccepted cancellation became a clear: %s", line)
		}
	}
}
