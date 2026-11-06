package framework

import (
	"context"
	"encoding/json"
	"errors"
	"testing"
	"time"

	"open25.local/open25-fne/internal/radio"
)

func TestRadioCommandTracksOnlyMatchingSubscriberResponse(t *testing.T) {
	s, native, client := commandSiteFixture(t)
	input := radioCommandInput{ID: "check-request-1", Action: "radio_check", RadioID: 10001}
	status, raw := client.request(t, "POST", "/api/radio/command", input)
	if status != 202 {
		t.Fatalf("%d %s", status, raw)
	}
	var receipt radioCommandReceipt
	_ = json.Unmarshal(raw, &receipt)
	if receipt.State != "sent" || !receipt.AwaitingAcknowledgment || receipt.ID != input.ID {
		t.Fatalf("%+v", receipt)
	}
	for _, e := range []radio.Event{
		{Kind: "acknowledgement", RID: 10001},
		{Kind: "radio_check_ack", RID: 10002},
		{Kind: "page_ack", RID: 10001},
		{Kind: "radio_check_ack", RID: 10001, Direction: "sent"},
		{Kind: "radio_check_ack", RID: 10001, SiteID: "another-site"},
		{Kind: "radio_check_ack", RID: 10001, At: receipt.AttemptedAt.Add(-time.Second)},
		{Kind: "radio_check_ack", RID: 10001, RequestorID: 10002, Direction: "received"},
		{Kind: "radio_check_ack", RID: 10001, RequestorID: 0, Direction: "received"},
	} {
		s.observeRadioCommand(e)
	}
	rows, _ := s.Store.radioCommands()
	if rows[0].State != "sent" {
		t.Fatal("unrelated response completed request", rows)
	}
	response := radio.Event{ID: "actual-rf-response", At: time.Now().UTC(), Kind: "radio_check_ack", RID: 10001, RequestorID: radio.NativeFNEID, Direction: "received"}
	s.observeRadioCommand(response)
	rows, _ = s.Store.radioCommands()
	v := rows[0]
	if v.State != "acknowledged" || v.AwaitingAcknowledgment || v.ResponseEventID != response.ID || v.ResponseKind != "radio_check_ack" {
		t.Fatalf("%+v", v)
	}
	status, raw = client.request(t, "POST", "/api/radio/command", input)
	native.mu.Lock()
	count := len(native.commands)
	native.mu.Unlock()
	if status != 202 || count != 1 {
		t.Fatal("retry re-sent a completed radio command", status, string(raw), count)
	}
	input.ID = "check-request-2"
	status, _ = client.request(t, "POST", "/api/radio/command", input)
	if status != 503 {
		t.Fatal("overlapping response windows accepted", status)
	}
}
func TestRadioCommandDeadlinePersistsAndNeverReplays(t *testing.T) {
	s, native, client := commandSiteFixture(t)
	input := radioCommandInput{ID: "page-deadline", Action: "page", RadioID: 10001}
	status, raw := client.request(t, "POST", "/api/radio/command", input)
	if status != 202 {
		t.Fatalf("%d %s", status, raw)
	}
	err := s.Store.mutateRadioCommands(func(rows *[]radioCommandReceipt) error {
		(*rows)[0].ExpiresAt = time.Now().Add(-time.Second)
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	restarted := &Service{Store: s.Store, config: s.Config()}
	rows, err := restarted.siteCommandRows()
	if err != nil || len(rows) != 1 || rows[0].State != "timed_out" || rows[0].AwaitingAcknowledgment {
		t.Fatalf("%+v %v", rows, err)
	}
	restarted.observeRadioCommand(radio.Event{Kind: "page_ack", RID: 10001, At: time.Now()})
	rows, _ = s.Store.radioCommands()
	if rows[0].State != "timed_out" {
		t.Fatal("late ACK revived expired request")
	}
	status, _ = client.request(t, "POST", "/api/radio/command", input)
	native.mu.Lock()
	count := len(native.commands)
	native.mu.Unlock()
	if status != 202 || count != 1 {
		t.Fatal("expired request was replayed")
	}
}
func TestRadioCommandUnconfirmedSendIsDurable(t *testing.T) {
	s, native, _, _, client, _, _ := inhibitionCluster(t)
	native.mu.Lock()
	native.commandError = errors.New("native send unconfirmed")
	native.mu.Unlock()
	status, _ := client.request(t, "POST", "/api/radio/command", radioCommandInput{ID: "uncertain-send", Action: "inhibit", RadioID: 10001, SiteID: "site-1"})
	rows, err := s.Store.radioCommands()
	if status != 503 || err != nil || len(rows) != 1 || rows[0].State != "unconfirmed" || rows[0].AttemptedAt.IsZero() {
		t.Fatalf("%d %+v %v", status, rows, err)
	}
}
func TestOperatorCommandHistoryIsScopedToActorIdentity(t *testing.T) {
	s, native := operationFixture(t)
	native.mu.Lock()
	native.status.State = "running"
	native.status.Affiliations = []radio.Affiliation{{RID: 10001, TGID: 1001, At: time.Now().UTC()}}
	native.mu.Unlock()
	actor := operatorAuthority{actor: OperatorActor{UserID: "person", RadioID: 10002}, user: User{ID: "person", RadioID: 10002, Talkgroups: []int{1001}, Permissions: []string{"radio_check"}}}
	result, err := s.operatorRadioCommand(OperatorInput{ID: "scoped-query", Action: "radio_check", RadioID: 10001}, actor)
	if err != nil || len(result.Commands) != 1 {
		t.Fatalf("%+v %v", result, err)
	}
	result, err = s.operatorRadioCommands(actor)
	if err != nil || len(result.Commands) != 1 {
		t.Fatalf("%+v %v", result, err)
	}
	for _, other := range []operatorAuthority{
		{actor: OperatorActor{UserID: "another", RadioID: 10002}, user: actor.user},
		{actor: OperatorActor{UserID: "person", RadioID: 10003}, user: actor.user},
		{actor: actor.actor, user: User{Talkgroups: []int{1002}, Permissions: []string{"radio_check"}}},
		{actor: actor.actor, user: User{Talkgroups: []int{1001}, Permissions: []string{"page"}}},
	} {
		result, err = s.operatorRadioCommands(other)
		if err != nil || len(result.Commands) != 0 {
			t.Fatal("command result leaked", result, err)
		}
	}
	if err = s.activity.Flush(context.Background()); err != nil {
		t.Fatal(err)
	}
}
