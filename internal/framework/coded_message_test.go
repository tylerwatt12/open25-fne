package framework

import (
	"context"
	"encoding/json"
	"testing"
	"time"

	"open25.local/open25-fne/internal/radio"
)

func TestCodedMessageDelayedAdmissionKeepsItsOriginalResponseOwner(t *testing.T) {
	s, _, _ := commandSiteFixture(t)
	code := 7
	s.operationMu.Lock()
	locked := true
	defer func() {
		if locked {
			s.operationMu.Unlock()
		}
	}()
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	type outcome struct {
		receipt radioCommandReceipt
		err     error
	}
	done := make(chan outcome, 1)
	go func() {
		receipt, err := s.sendSiteRadioCommand(ctx, radioCommandInput{ID: "delayed-coded-message", Action: "message", RadioID: 10001, Code: &code}, OperatorActor{Administrator: true}, 0)
		done <- outcome{receipt, err}
	}()
	for {
		rows, err := s.Store.radioCommands()
		if err != nil {
			t.Fatal(err)
		}
		if len(rows) != 0 {
			break
		}
		select {
		case <-ctx.Done():
			t.Fatal("request never reached admission")
		case <-time.After(time.Millisecond):
		}
	}
	var oldExpiry time.Time
	if err := s.Store.mutateRadioCommands(func(rows *[]radioCommandReceipt) error {
		// Model time waiting for the existing operation owner without
		// sleeping through a complete response window.
		(*rows)[0].CreatedAt = (*rows)[0].CreatedAt.Add(-4 * time.Second)
		(*rows)[0].ExpiresAt = (*rows)[0].ExpiresAt.Add(-4 * time.Second)
		oldExpiry = (*rows)[0].ExpiresAt
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	s.operationMu.Unlock()
	locked = false
	var result outcome
	select {
	case result = <-done:
	case <-ctx.Done():
		t.Fatal("admitted message did not finish")
	}
	if result.err != nil {
		t.Fatal(result.err)
	}
	late := oldExpiry.Add(500 * time.Millisecond)
	if !late.Before(result.receipt.ExpiresAt) {
		t.Fatal("native response can outlive its application owner")
	}
	s.observeRadioCommand(radio.Event{Kind: "message_ack", At: late, RID: 10001, RequestorID: radio.NativeFNEID, Direction: "received", Code: &code, Attempts: 1})
	rows, err := s.Store.radioCommands()
	if err != nil || len(rows) != 1 || rows[0].State != "acknowledged" {
		t.Fatalf("original response owner lost: %+v %v", rows, err)
	}
	if _, err = s.sendSiteRadioCommand(ctx, radioCommandInput{ID: "overlapping-coded-message", Action: "message", RadioID: 10001, Code: &code}, OperatorActor{Administrator: true}, 0); err == nil {
		t.Fatal("a second command reused the original response window")
	}
}

func TestCodedMessageKeepsZeroAndMatchesOnlyItsNativeOutcome(t *testing.T) {
	s, native, client := commandSiteFixture(t)
	code, other := 0, 1
	input := radioCommandInput{ID: "coded-message-zero", Action: "message", RadioID: 10001, Code: &code}
	status, raw := client.request(t, "POST", "/api/radio/command", input)
	if status != 202 {
		t.Fatalf("%d %s", status, raw)
	}
	var receipt radioCommandReceipt
	if err := json.Unmarshal(raw, &receipt); err != nil {
		t.Fatal(err)
	}
	if receipt.Code == nil || *receipt.Code != 0 || receipt.State != "sent" || !receipt.AwaitingAcknowledgment {
		t.Fatalf("lost code or send outcome: %+v", receipt)
	}
	if receipt.ExpiresAt.Sub(receipt.AttemptedAt) != radioCommandWait {
		t.Fatal("response fence does not cover native admission and reply from the actual attempt", receipt)
	}
	for _, event := range []radio.Event{
		{Kind: "acknowledgement", RID: 10001, RequestorID: radio.NativeFNEID, Direction: "received"},
		{Kind: "page_ack", RID: 10001, RequestorID: radio.NativeFNEID, Direction: "received"},
		{Kind: "message_ack", RID: 10001, RequestorID: 10002, Direction: "received", Code: &code, Attempts: 1},
		{Kind: "message_ack", RID: 10001, RequestorID: radio.NativeFNEID, Direction: "received", Code: &other, Attempts: 1},
		{Kind: "message_ack", RID: 10001, RequestorID: radio.NativeFNEID, Direction: "received", Code: &code, Attempts: 2},
		{Kind: "message_ack", RID: 10001, RequestorID: radio.NativeFNEID, Direction: "state", Code: &code, Attempts: 1},
		{Kind: "message_ack", RID: 10001, SiteID: "another-site", RequestorID: radio.NativeFNEID, Direction: "received", Code: &code, Attempts: 1},
	} {
		s.observeRadioCommand(event)
	}
	rows, err := s.Store.radioCommands()
	if err != nil || rows[0].State != "sent" {
		t.Fatalf("unrelated evidence completed message: %+v %v", rows, err)
	}
	s.observeRadioCommand(radio.Event{ID: "matched-message-reply", Kind: "message_ack", RID: 10001, RequestorID: radio.NativeFNEID, Direction: "received", Code: &code, Attempts: 1})
	rows, err = s.Store.radioCommands()
	if err != nil || rows[0].State != "acknowledged" || rows[0].ResponseEventID != "matched-message-reply" {
		t.Fatalf("actual match lost: %+v %v", rows, err)
	}
	if status, raw = client.request(t, "POST", "/api/radio/command", input); status != 202 {
		t.Fatalf("duplicate result: %d %s", status, raw)
	}
	input.Code = &other
	if status, _ = client.request(t, "POST", "/api/radio/command", input); status != 503 {
		t.Fatal("same ID accepted different code", status)
	}
	native.mu.Lock()
	defer native.mu.Unlock()
	if len(native.commands) != 1 || native.commands[0].Action != "message" || native.commands[0].Code != 0 || native.commands[0].RID != 10001 {
		t.Fatal("wrong or replayed native request", native.commands)
	}
}

func TestCodedMessageRuntimeCodeAndOperatorAuthority(t *testing.T) {
	s, native := operationFixture(t)
	for _, code := range []int{-1, 65536} {
		if err := s.validateRadioCommand(radioCommandInput{Action: "message", RadioID: 10001, Code: &code}); err == nil {
			t.Fatal("unrepresentable on-air code accepted", code)
		}
	}
	if err := s.validateRadioCommand(radioCommandInput{Action: "message", RadioID: 10001}); err == nil {
		t.Fatal("missing code accepted")
	}
	code := 65535
	native.mu.Lock()
	native.status.State = "running"
	native.status.Affiliations = []radio.Affiliation{{RID: 10001, TGID: 1001, At: time.Now().UTC()}}
	native.mu.Unlock()
	actor := operatorAuthority{actor: OperatorActor{UserID: "message-person", RadioID: 10002}, user: User{ID: "message-person", RadioID: 10002, Talkgroups: []int{1001}, Permissions: []string{"message"}}}
	input := OperatorInput{ID: "operator-coded-message", Action: "message", RadioID: 10001, TalkgroupID: 1001, Code: &code}
	result, err := s.operatorRadioCommand(input, actor)
	if err != nil || len(result.Commands) != 1 || result.Commands[0].Code == nil || *result.Commands[0].Code != code {
		t.Fatalf("permitted message: %+v %v", result, err)
	}
	actor.user.Permissions = []string{"page"}
	if _, err = s.operatorRadioCommand(input, actor); err == nil {
		t.Fatal("page permission granted message authority")
	}
	actor.user.Permissions = []string{"message"}
	actor.user.Talkgroups = []int{1002}
	if _, err = s.operatorRadioCommand(input, actor); err == nil {
		t.Fatal("message reached another group's subscriber")
	}
}
