package framework

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"net/http/httptest"
	"testing"
	"time"

	"open25.local/open25-fne/internal/activity"
	"open25.local/open25-fne/internal/radio"
)

func TestReviewMatchingACKResolvesAnUnconfirmedSendWithinItsWindow(t *testing.T) {
	s, native, _, _, client, _, _ := inhibitionCluster(t)
	native.mu.Lock()
	native.commandError = errors.New("transport response timed out; radio send is uncertain")
	native.mu.Unlock()
	status, _ := client.request(t, "POST", "/api/radio/command", radioCommandInput{ID: "review-uncertain-send", Action: "inhibit", RadioID: 10001, SiteID: "site-1"})
	if status != 503 {
		t.Fatal("fixture did not create an uncertain send")
	}
	s.observeRadioCommand(radio.Event{ID: "review-real-ack", Kind: "inhibit_ack", RID: 10001, RequestorID: radio.NativeFNEID, At: time.Now().UTC(), Direction: "received"})
	rows, err := s.Store.radioCommands()
	if err != nil || len(rows) != 1 || rows[0].State != "acknowledged" || !rows[0].Sent || rows[0].ResponseEventID != "review-real-ack" {
		t.Fatalf("actual radio ACK was discarded after an uncertain send: %+v %v", rows, err)
	}
}

func TestReviewCommandTimeoutIsRetainedInCentralActivityOnce(t *testing.T) {
	s, _, client := commandSiteFixture(t)
	status, _ := client.request(t, "POST", "/api/radio/command", radioCommandInput{ID: "review-timeout", Action: "page", RadioID: 10001})
	if status != 202 {
		t.Fatal(status)
	}
	if err := s.Store.mutateRadioCommands(func(rows *[]radioCommandReceipt) error {
		(*rows)[0].ExpiresAt = time.Now().Add(-time.Second)
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	for i := 0; i < 2; i++ {
		if _, err := s.siteCommandRows(); err != nil {
			t.Fatal(err)
		}
	}
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	if err := s.activity.Flush(ctx); err != nil {
		t.Fatal(err)
	}
	rows, err := s.activity.Events(activity.Query{Type: "radio_command"})
	if err != nil {
		t.Fatal(err)
	}
	timeouts := 0
	for _, e := range rows {
		if e.Outcome == "timed_out" && e.Details["operation_id"] == "review-timeout" {
			timeouts++
		}
	}
	if timeouts != 1 {
		t.Fatalf("expired command outcome missing or duplicated in forwarding history: %d %+v", timeouts, rows)
	}
}

func TestReviewFinalSiteRejectsOperatorCommandAfterTargetTalkgroupChanges(t *testing.T) {
	s, desk, _ := operatorFixture(t, RoleSite)
	view := s.view()
	record := view.Users[desk.actor.UserID]
	record.User.Permissions = append(record.User.Permissions, "radio_check")
	view.Users[desk.actor.UserID] = record
	desk.actor.Proof = userProof(record.User, record.PasswordHash)
	native := &operationRadio{status: radio.Status{State: "running", SiteID: "site-1", Affiliations: []radio.Affiliation{{RID: 10001, TGID: 1002}}}}
	s.radio = native
	body, _ := json.Marshal(radioCommandEnvelope{Input: radioCommandInput{ID: "review-stale-group", Action: "radio_check", RadioID: 10001, SiteID: "site-1"}, Actor: desk.actor, TalkgroupID: 1001})
	r := httptest.NewRequest("POST", "/internal/radio/command", bytes.NewReader(body))
	r.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()
	s.radioCommandInternal(w, r, PeerCredential{Role: RoleZone, InstanceID: "zone-1"})
	native.mu.Lock()
	count := len(native.commands)
	native.mu.Unlock()
	if w.Code == 202 || count != 0 {
		t.Fatalf("Site sent using stale target talkgroup scope: status=%d commands=%d body=%s", w.Code, count, w.Body.String())
	}
}
