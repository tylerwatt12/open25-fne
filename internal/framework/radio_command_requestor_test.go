package framework

import (
	"testing"
	"time"

	"open25.local/open25-fne/internal/radio"
)

func TestExtendedFunctionACKMustAddressTheOriginatingFNE(t *testing.T) {
	for _, action := range []string{"radio_check", "inhibit", "uninhibit"} {
		t.Run(action, func(t *testing.T) {
			s, _, _, _, client, _, _ := inhibitionCluster(t)
			status, raw := client.request(t, "POST", "/api/radio/command", radioCommandInput{ID: "addressed-" + action, Action: action, RadioID: 10001, SiteID: "site-1"})
			if status != 202 {
				t.Fatalf("admission failed: %d %s", status, raw)
			}
			for _, other := range []int{0, 10002, 0xFFFFFB} {
				s.observeRadioCommand(radio.Event{Kind: action + "_ack", RID: 10001, RequestorID: other, At: time.Now(), Direction: "received"})
				rows, err := s.Store.radioCommands()
				if err != nil || len(rows) != 1 || rows[0].State != "sent" {
					t.Fatalf("other requestor completed this command: %+v %v", rows, err)
				}
			}
			s.observeRadioCommand(radio.Event{ID: "addressed-response", Kind: action + "_ack", RID: 10001, RequestorID: radio.NativeFNEID, At: time.Now(), Direction: "received"})
			rows, err := s.Store.radioCommands()
			if err != nil || rows[0].State != "acknowledged" || rows[0].ResponseEventID != "addressed-response" {
				t.Fatalf("addressed response not retained: %+v %v", rows, err)
			}
		})
	}
}

func TestNativeRadioCheckRetryExhaustionCompletesOnlyItsPendingRequest(t *testing.T) {
	s, _, client := commandSiteFixture(t)
	status, raw := client.request(t, "POST", "/api/radio/command", radioCommandInput{ID: "retry-exhausted", Action: "radio_check", RadioID: 10001})
	if status != 202 {
		t.Fatalf("admission failed: %d %s", status, raw)
	}
	base := radio.Event{Kind: "radio_check_no_response", RID: 10001, RequestorID: radio.NativeFNEID, Attempts: 5, At: time.Now(), Direction: "state"}
	for _, alter := range []func(*radio.Event){
		func(e *radio.Event) { e.RequestorID = 10002 },
		func(e *radio.Event) { e.RID = 10002 },
		func(e *radio.Event) { e.Attempts = 4 },
		func(e *radio.Event) { e.Direction = "received" },
		func(e *radio.Event) { e.SiteID = "another-site" },
		func(e *radio.Event) { e.At = e.At.Add(-time.Minute) },
	} {
		e := base
		alter(&e)
		s.observeRadioCommand(e)
		rows, err := s.Store.radioCommands()
		if err != nil || rows[0].State != "sent" {
			t.Fatalf("unrelated terminal outcome completed request: %+v %v", rows, err)
		}
	}
	base.ID = "native-retries-exhausted"
	s.observeRadioCommand(base)
	rows, err := s.Store.radioCommands()
	if err != nil || rows[0].State != "timed_out" || rows[0].AwaitingAcknowledgment || rows[0].ResponseEventID != base.ID || rows[0].Message != "No subscriber response after the Site's radio-check retries." {
		t.Fatalf("terminal retry result not retained honestly: %+v %v", rows, err)
	}
	// A late raw acknowledgment cannot rewrite an already terminal outcome.
	s.observeRadioCommand(radio.Event{Kind: "radio_check_ack", RID: 10001, RequestorID: radio.NativeFNEID, At: time.Now(), Direction: "received"})
	rows, _ = s.Store.radioCommands()
	if rows[0].State != "timed_out" {
		t.Fatalf("late ACK rewrote terminal outcome: %+v", rows[0])
	}
}

func TestNativeRetryTimeoutCannotOverwriteAnAcknowledgedCheck(t *testing.T) {
	s, _, client := commandSiteFixture(t)
	status, raw := client.request(t, "POST", "/api/radio/command", radioCommandInput{ID: "check-acknowledged", Action: "radio_check", RadioID: 10001})
	if status != 202 {
		t.Fatalf("admission failed: %d %s", status, raw)
	}
	s.observeRadioCommand(radio.Event{Kind: "radio_check_ack", RID: 10001, RequestorID: radio.NativeFNEID, At: time.Now(), Direction: "received"})
	s.observeRadioCommand(radio.Event{Kind: "radio_check_no_response", RID: 10001, RequestorID: radio.NativeFNEID, Attempts: 5, At: time.Now(), Direction: "state"})
	rows, err := s.Store.radioCommands()
	if err != nil || rows[0].State != "acknowledged" {
		t.Fatalf("terminal check overwritten: %+v %v", rows, err)
	}
}
