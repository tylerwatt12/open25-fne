package framework

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"
	"time"

	"open25.local/open25-fne/internal/radio"
)

// These cases exercise application receipt ordering, not RF timing. Native
// observations can arrive before the HTTP call that initiated them returns.
type commandLifecycleRadio struct {
	*operationRadio
	complete func(context.Context, radio.Command) error
}

func (r *commandLifecycleRadio) Command(ctx context.Context, command radio.Command) (map[string]json.RawMessage, error) {
	reply, err := r.operationRadio.Command(ctx, command)
	if r.complete != nil {
		err = r.complete(ctx, command)
	}
	return reply, err
}

func commandLifecycleFixture(t *testing.T) (*Service, *commandLifecycleRadio) {
	t.Helper()
	native := &commandLifecycleRadio{operationRadio: &operationRadio{status: radio.Status{State: "running", SiteID: "site-1"}}}
	s := &Service{Store: storeOpen(t, RoleSite), config: FileConfig{Role: RoleSite, InstanceID: "site-1"}, radio: native}
	return s, native
}

func TestRadioCommandSendCompletionCannotReviveTerminalOutcome(t *testing.T) {
	for _, sendError := range []bool{false, true} {
		for _, nativeOutcome := range []bool{false, true} {
			t.Run(strings.Join([]string{map[bool]string{false: "send-ok", true: "send-error"}[sendError], map[bool]string{false: "deadline", true: "native-no-response"}[nativeOutcome]}, "/"), func(t *testing.T) {
				s, native := commandLifecycleFixture(t)
				native.complete = func(context.Context, radio.Command) error {
					if nativeOutcome {
						s.observeRadioCommand(radio.Event{ID: "finished-check", Kind: "radio_check_no_response", RID: 10001, RequestorID: radio.NativeFNEID, Attempts: 5, Direction: "state", At: time.Now().UTC()})
					} else {
						if err := s.Store.mutateRadioCommands(func(rows *[]radioCommandReceipt) error {
							(*rows)[0].ExpiresAt = time.Now().Add(-time.Second)
							return nil
						}); err != nil {
							t.Fatal(err)
						}
						s.expireRadioCommandRecords()
					}
					if sendError {
						return errors.New("late transport failure")
					}
					return nil
				}
				got, _ := s.sendSiteRadioCommand(context.Background(), radioCommandInput{ID: "finished-check", Action: "radio_check", RadioID: 10001}, OperatorActor{Administrator: true}, 0)
				rows, err := s.Store.radioCommands()
				if err != nil || len(rows) != 1 || got.State != "timed_out" || rows[0].State != "timed_out" || got.AwaitingAcknowledgment {
					t.Fatalf("late send completion revived finished request: returned=%+v stored=%+v err=%v", got, rows, err)
				}
				if nativeOutcome && (got.ResponseKind != "radio_check_no_response" || got.ResponseEventID != "finished-check" || !got.Sent) {
					t.Fatalf("native terminal evidence was lost: %+v", got)
				}
			})
		}
	}
}

func TestRadioCommandCancelledBeforeSendIsRetainedAsUnsent(t *testing.T) {
	s, native := commandLifecycleFixture(t)
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	got, err := s.sendSiteRadioCommand(ctx, radioCommandInput{ID: "cancelled-before-send", Action: "radio_check", RadioID: 10001}, OperatorActor{Administrator: true}, 0)
	if !errors.Is(err, context.Canceled) || got.State != "rejected" || got.Sent || got.AwaitingAcknowledgment || !got.AttemptedAt.IsZero() || !strings.Contains(got.Message, "not sent") {
		t.Fatalf("cancelled unsent command remained pending: %+v %v", got, err)
	}
	if len(native.commands) != 0 {
		t.Fatal("cancelled command reached native")
	}
	retry, retryErr := s.sendSiteRadioCommand(context.Background(), radioCommandInput{ID: got.ID, Action: got.Action, RadioID: got.RadioID}, OperatorActor{Administrator: true}, 0)
	if retryErr != nil || retry.State != "rejected" || len(native.commands) != 0 {
		t.Fatalf("same-ID retry replayed unsent cancellation: %+v %v", retry, retryErr)
	}
}

func TestRadioCommandExpiresBeforeSendOwnershipIsAvailable(t *testing.T) {
	for _, finish := range []string{"deadline", "expiry-worker", "cancelled"} {
		t.Run(finish, func(t *testing.T) { testRadioCommandWaitingForSendOwnership(t, finish) })
	}
}

func testRadioCommandWaitingForSendOwnership(t *testing.T, finish string) {
	t.Helper()
	s, native := commandLifecycleFixture(t)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	s.operationMu.Lock()
	locked := true
	defer func() {
		if locked {
			s.operationMu.Unlock()
		}
	}()
	type result struct {
		receipt radioCommandReceipt
		err     error
	}
	done := make(chan result, 1)
	go func() {
		receipt, err := s.sendSiteRadioCommand(ctx, radioCommandInput{ID: "expired-before-send", Action: "radio_check", RadioID: 10001}, OperatorActor{Administrator: true}, 0)
		done <- result{receipt, err}
	}()
	deadline := time.Now().Add(2 * time.Second)
	for {
		rows, err := s.Store.radioCommands()
		if err != nil {
			t.Fatal(err)
		}
		if len(rows) == 1 {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("command admission did not finish")
		}
		time.Sleep(time.Millisecond)
	}
	if finish == "cancelled" {
		cancel()
	} else {
		if err := s.Store.mutateRadioCommands(func(rows *[]radioCommandReceipt) error {
			(*rows)[0].ExpiresAt = time.Now().Add(-time.Second)
			return nil
		}); err != nil {
			t.Fatal(err)
		}
		if finish == "expiry-worker" {
			s.expireRadioCommandRecords()
		}
	}
	s.operationMu.Unlock()
	locked = false
	select {
	case got := <-done:
		if got.err == nil || got.receipt.State != "rejected" || got.receipt.Sent || !got.receipt.AttemptedAt.IsZero() || !strings.Contains(got.receipt.Message, "not sent") || len(native.commands) != 0 {
			t.Fatalf("expired queued command reached native: %+v err=%v commands=%+v", got.receipt, got.err, native.commands)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("expired command did not finish")
	}
}

func TestRadioCommandLateSendCompletionPreservesActualSubscriberResponse(t *testing.T) {
	for _, response := range []struct{ action, kind, state string }{
		{"radio_check", "radio_check_ack", "acknowledged"},
		{"registration_demand", "registered", "response_observed"},
	} {
		t.Run(response.action, func(t *testing.T) {
			s, native := commandLifecycleFixture(t)
			native.complete = func(context.Context, radio.Command) error {
				s.observeRadioCommand(radio.Event{ID: "actual-subscriber-response", Kind: response.kind, RID: 10001, RequestorID: radio.NativeFNEID, Direction: "received", At: time.Now().UTC()})
				return errors.New("HTTP response was lost after subscriber response")
			}
			got, _ := s.sendSiteRadioCommand(context.Background(), radioCommandInput{ID: "responded-command", Action: response.action, RadioID: 10001}, OperatorActor{Administrator: true}, 0)
			if got.State != response.state || !got.Sent || got.AwaitingAcknowledgment || got.ResponseEventID != "actual-subscriber-response" || got.SentAt.IsZero() {
				t.Fatalf("transport completion discarded subscriber evidence: %+v", got)
			}
		})
	}
}
