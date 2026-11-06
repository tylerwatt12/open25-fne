package radio

import (
	"fmt"
	"testing"
)

// AABC-E 6.1.6 plus E-3 Table 6.1.6-1. Native CAI parsing is tested
// separately; this verifies that observed SI fields survive the log adapter.
func TestEmergencyInformationPreservesZeroAndAdvancedCauses(t *testing.T) {
	for _, info := range [][2]int{{0, 0}, {1, 0}, {2, 0}, {4, 0}, {8, 0}, {128, 1}, {128, 255}} {
		line := fmt.Sprintf("I: 2026-10-30 06:00:00.000 (RF) P25 TSDU, TSBKO, ISP_EMERG_ALRM_REQ (Emergency Alarm Request), si1 = $%02X, si2 = $%02X, srcId = 10001, dstId = 1001", info[0], info[1])
		e := ParseEvent(line)
		if e == nil || e.Kind != "emergency" || e.Direction != "received" || e.RID != 10001 || e.TGID != 1001 || e.EmergencyInfo1 == nil || e.EmergencyInfo2 == nil || *e.EmergencyInfo1 != info[0] || *e.EmergencyInfo2 != info[1] {
			t.Fatalf("emergency information lost: %v %+v", info, e)
		}
	}
}

func TestOutboundDenialDoesNotDeclareAnEmergency(t *testing.T) {
	line := "I: 2026-10-30 06:00:00.000 (NET) P25 TSDU, TSBKO, OSP_DENY_RSP (Deny Response), AIV = 1, reason = $31, srcId = 10001, dstId = 1001"
	if e := ParseEvent(line); e != nil && e.Kind == "emergency" {
		t.Fatal("downlink denial became an emergency", e)
	}
}

func TestExtendedFunctionResponseRetainsItsAddressedRequestor(t *testing.T) {
	for _, code := range []int{0x80, 0xFE, 0xFF} {
		line := fmt.Sprintf("I: 2026-10-30 06:00:00.000 (RF) P25 TSDU, TSBKO, IOSP_EXT_FNCT (Extended Function Response), op = $%02X, arg = 16777212, tgt = 10001", code)
		e := ParseEvent(line)
		if e == nil || e.RID != 10001 || e.RequestorID != NativeFNEID || e.Code == nil || *e.Code != code {
			t.Fatalf("response addresses lost: %+v", e)
		}
	}
}

func TestAffiliationQueryMismatchDoesNotBecomeAcceptedMembership(t *testing.T) {
	line := "W: 2026-10-30 06:00:00.000 (RF) P25 TSDU, affiliation query mismatch, srcId = 10001, dstId = 1002, anncId = 65534"
	e := ParseEvent(line)
	if e == nil || e.Kind != "request_denied" || e.Audience != "system_operator" || e.RID != 10001 || e.TGID != 1002 || e.Message != "Radio affiliation does not match the Site's record." {
		t.Fatalf("query mismatch was discarded or accepted: %+v", e)
	}
}

func TestRadioCheckRetryOutcomeIsNotASubscriberResponse(t *testing.T) {
	line := "W: 2026-10-30 06:00:10.000 (HOST) P25 Radio Check, no response, arg = 16777212, tgt = 10001, attempts = 5"
	e := ParseEvent(line)
	if e == nil || e.Kind != "radio_check_no_response" || e.Direction != "state" || e.RID != 10001 || e.RequestorID != NativeFNEID || e.Attempts != 5 || e.TGID != 0 {
		t.Fatalf("native retry outcome lost or misidentified: %+v", e)
	}
	for _, suffix := range []string{"arg = 10002, tgt = 10001, attempts = 5", "arg = 16777212, tgt = 10001, attempts = 4", "tgt = 10001, attempts = 5", "arg = 16777212, attempts = 5"} {
		if e := ParseEvent("W: (HOST) P25 Radio Check, no response, " + suffix); e != nil {
			t.Fatalf("incomplete/unrelated terminal outcome accepted: %+v", e)
		}
	}
}
