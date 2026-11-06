package framework

import (
	"encoding/json"
	"net/http/httptest"
	"testing"
	"time"

	"open25.local/open25-fne/internal/radio"
)

func TestStatusQueryAdministratorPathTracksOnlyMatchingStatusResponse(t *testing.T) {
	s, native, client := commandSiteFixture(t)
	input := radioCommandInput{ID: "status-query-1", Action: "status_query", RadioID: 10001}
	status, raw := client.request(t, "POST", "/api/radio/command", input)
	if status != 202 {
		t.Fatalf("%d %s", status, raw)
	}
	var receipt radioCommandReceipt
	if err := json.Unmarshal(raw, &receipt); err != nil {
		t.Fatal(err)
	}
	if receipt.State != "sent" || receipt.ResponseCode != nil || !receipt.AwaitingAcknowledgment {
		t.Fatalf("dishonest acceptance: %+v", receipt)
	}
	code := 258
	for _, e := range []radio.Event{
		{Kind: "status", RID: 10001, Code: &code},
		{Kind: "status_query_response", RID: 10001, RequestorID: 10002, Code: &code, Direction: "received"},
		{Kind: "status_query_response", RID: 10002, RequestorID: radio.NativeFNEID, Code: &code, Direction: "received"},
		{Kind: "status_query_response", RID: 10001, RequestorID: radio.NativeFNEID, Code: &code, Direction: "sent"},
		{Kind: "status_query_response", RID: 10001, RequestorID: radio.NativeFNEID, Direction: "received"},
		{Kind: "status_query_response", RID: 10001, RequestorID: radio.NativeFNEID, Code: &code, Direction: "received", SiteID: "other"},
		{Kind: "status_query_response", RID: 10001, RequestorID: radio.NativeFNEID, Code: &code, Direction: "received", At: receipt.AttemptedAt.Add(-time.Second)},
		{Kind: "status_query_response", RID: 10001, RequestorID: radio.NativeFNEID, Code: &code, Direction: "received", At: receipt.ExpiresAt},
	} {
		s.observeRadioCommand(e)
	}
	rows, _ := s.Store.radioCommands()
	if rows[0].State != "sent" {
		t.Fatal("unrelated evidence completed query", rows)
	}
	response := radio.Event{ID: "matched-status", Kind: "status_query_response", RID: 10001, RequestorID: radio.NativeFNEID, Code: &code, Direction: "received", At: time.Now().UTC()}
	s.observeRadioCommand(response)
	rows, _ = s.Store.radioCommands()
	if len(rows) != 1 || rows[0].State != "response_observed" || rows[0].ResponseCode == nil || *rows[0].ResponseCode != 258 || rows[0].ResponseEventID != "matched-status" {
		t.Fatalf("wrong response receipt: %+v", rows)
	}
	code = 65535
	s.observeRadioCommand(response)
	rows, _ = s.Store.radioCommands()
	if *rows[0].ResponseCode != 258 {
		t.Fatal("duplicate response rewrote terminal status")
	}
	status, raw = client.request(t, "POST", "/api/radio/command", input)
	native.mu.Lock()
	count := len(native.commands)
	native.mu.Unlock()
	if status != 202 || count != 1 {
		t.Fatalf("same ID replayed query: %d %d %s", status, count, raw)
	}
	input.ID = "status-query-overlap"
	status, _ = client.request(t, "POST", "/api/radio/command", input)
	if status != 503 {
		t.Fatal("overlapping window admitted", status)
	}
	r := httptest.NewRecorder()
	s.radioCommandHTTP(r, httptest.NewRequest("POST", "/api/radio/command", nil), Session{UserID: "dispatcher-user"})
	if r.Code != 403 {
		t.Fatal("operator elevated to administrator query")
	}
}
func TestStatusQueryZeroStatusAndNativeNoResponseRemainDistinct(t *testing.T) {
	for _, zero := range []bool{false, true} {
		t.Run(map[bool]string{false: "no_response", true: "zero_status"}[zero], func(t *testing.T) {
			s, _, client := commandSiteFixture(t)
			status, raw := client.request(t, "POST", "/api/radio/command", radioCommandInput{ID: "query-final", Action: "status_query", RadioID: 10001})
			if status != 202 {
				t.Fatalf("%d %s", status, raw)
			}
			e := radio.Event{Kind: "status_query_no_response", RID: 10001, RequestorID: radio.NativeFNEID, Attempts: 1, Direction: "state", At: time.Now().UTC()}
			if zero {
				code := 0
				e.Kind = "status_query_response"
				e.Direction = "received"
				e.Code = &code
			}
			s.observeRadioCommand(e)
			rows, _ := s.Store.radioCommands()
			if zero {
				if rows[0].State != "response_observed" || rows[0].ResponseCode == nil || *rows[0].ResponseCode != 0 {
					t.Fatal(rows)
				}
			} else if rows[0].State != "timed_out" || rows[0].ResponseCode != nil {
				t.Fatal(rows)
			}
		})
	}
}
