package framework

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"open25.local/open25-fne/internal/radio"
)

func TestUnconfirmedRadioCommandHasBoundedResponseWindow(t *testing.T) {
	deadline := time.Date(2026, 10, 31, 12, 0, 0, 0, time.UTC)
	for _, attempted := range []bool{false, true} {
		t.Run(map[bool]string{false: "remote-attempt-unknown", true: "local-attempt"}[attempted], func(t *testing.T) {
			row := radioCommandReceipt{State: "unconfirmed", ExpiresAt: deadline}
			if attempted {
				row.AttemptedAt = deadline.Add(-radioCommandWait)
			}
			rows := []radioCommandReceipt{row}
			if !commandPending(row) {
				t.Fatal("uncertain command lost its pending response window")
			}
			expireRadioCommands(rows, deadline.Add(-time.Nanosecond))
			if rows[0].State != "unconfirmed" {
				t.Fatal("uncertainty ended before its deadline")
			}
			expireRadioCommands(rows, deadline)
			if rows[0].State != "timed_out" || rows[0].Sent || rows[0].AwaitingAcknowledgment || strings.Contains(rows[0].Message, "not sent") {
				t.Fatalf("expiry invented an unsent or confirmed outcome: %+v", rows[0])
			}
			if !strings.Contains(rows[0].Message, "unconfirmed") {
				t.Fatalf("expiry discarded the transport uncertainty: %+v", rows[0])
			}
		})
	}
}

func TestUnconfirmedCommandExpiresAfterRestartWithoutReplay(t *testing.T) {
	s, native := commandLifecycleFixture(t)
	native.complete = func(context.Context, radio.Command) error { return errors.New("native response lost") }
	input := radioCommandInput{ID: "uncertain-restart", Action: "radio_check", RadioID: 10001}
	actor := OperatorActor{Administrator: true}
	got, err := s.sendSiteRadioCommand(context.Background(), input, actor, 0)
	if err == nil || got.State != "unconfirmed" {
		t.Fatalf("expected an uncertain native send: %+v %v", got, err)
	}
	if err := s.Store.mutateRadioCommands(func(rows *[]radioCommandReceipt) error {
		(*rows)[0].ExpiresAt = time.Now().Add(-time.Second)
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	restarted := &Service{Store: s.Store, config: s.Config(), radio: native}
	rows, err := restarted.siteCommandRows()
	if err != nil || len(rows) != 1 || rows[0].State != "timed_out" {
		t.Fatalf("uncertain command did not terminate after restart: %+v %v", rows, err)
	}
	restarted.observeRadioCommand(radio.Event{Kind: "radio_check_ack", RID: 10001, RequestorID: radio.NativeFNEID, At: time.Now().UTC(), Direction: "received"})
	got, err = restarted.sendSiteRadioCommand(context.Background(), input, actor, 0)
	if err != nil || got.State != "timed_out" || len(native.commands) != 1 {
		t.Fatalf("restart/retry revived uncertain request: %+v %v commands=%d", got, err, len(native.commands))
	}
}

func TestRadioCommandResponseAtDeadlineCannotAcknowledge(t *testing.T) {
	s, _ := commandLifecycleFixture(t)
	deadline := time.Now().UTC().Add(time.Second)
	row := radioCommandReceipt{ID: "deadline-boundary", SiteID: "site-1", RadioID: 10001, Action: "radio_check", State: "sent", Sent: true, AttemptedAt: deadline.Add(-radioCommandWait), ExpiresAt: deadline, AwaitingAcknowledgment: true}
	if err := s.Store.mutateRadioCommands(func(rows *[]radioCommandReceipt) error { *rows = append(*rows, row); return nil }); err != nil {
		t.Fatal(err)
	}
	s.observeRadioCommand(radio.Event{Kind: "radio_check_ack", RID: 10001, RequestorID: radio.NativeFNEID, At: deadline, Direction: "received"})
	rows, err := s.Store.radioCommands()
	if err != nil || rows[0].State != "sent" {
		t.Fatalf("ACK admitted at the expired response-window boundary: %+v %v", rows, err)
	}
}
