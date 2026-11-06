package radio

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"testing"
)

func TestCodedMessageUsesExistingControlEndpointWithoutLosingZero(t *testing.T) {
	seen := make(chan map[string]any, 2)
	endpoint := inhibitEndpoint(t, func(w http.ResponseWriter, r *http.Request) {
		if r.Method != "PUT" || r.URL.Path != "/p25/rid" {
			t.Errorf("wrong native operation %s %s", r.Method, r.URL.Path)
		}
		var body map[string]any
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			t.Error(err)
		}
		seen <- body
		_ = json.NewEncoder(w).Encode(map[string]any{"status": 200})
	})
	c := inhibitController(t, endpoint, endpoint, InhibitPolicy{})
	for _, code := range []int{0, 65535} {
		if _, err := c.Command(context.Background(), Command{Action: "message", RID: 10001, Code: code}); err != nil {
			t.Fatal(err)
		}
		body := <-seen
		if body["command"] != "message" || body["dstId"] != float64(10001) || body["message"] != float64(code) || len(body) != 3 {
			t.Fatalf("wrong message fields: %+v", body)
		}
	}
	for _, code := range []int{-1, 65536} {
		if _, err := c.Command(context.Background(), Command{Action: "message", RID: 10001, Code: code}); err == nil {
			t.Fatal("unrepresentable code reached native transport", code)
		}
	}
}

func TestCodedMessageEventsRequireMatchedNativeOutcome(t *testing.T) {
	for _, code := range []int{0, 258, 65535} {
		for _, outcome := range []string{"acknowledged", "no response"} {
			line := fmt.Sprintf("I: (HOST) P25 Message Update, %s, arg = 16777212, tgt = 10001, message = $%04X, attempts = 1", outcome, code)
			e := ParseEvent(line)
			if e == nil || e.RID != 10001 || e.RequestorID != NativeFNEID || e.Code == nil || *e.Code != code || e.Attempts != 1 {
				t.Fatalf("message evidence lost: %+v", e)
			}
			if outcome == "acknowledged" && (e.Kind != "message_ack" || e.Direction != "received") || outcome == "no response" && (e.Kind != "message_no_response" || e.Direction != "state") {
				t.Fatalf("wrong outcome: %+v", e)
			}
		}
	}
	for _, line := range []string{
		"I: (HOST) P25 Message Update, acknowledged, arg = 10002, tgt = 10001, message = 1, attempts = 1",
		"I: (HOST) P25 Message Update, acknowledged, arg = 16777212, tgt = 10001, message = 65536, attempts = 1",
		"I: (HOST) P25 Message Update, acknowledged, arg = 16777212, tgt = 10001, attempts = 1",
		"I: (HOST) P25 Message Update, acknowledged, arg = 16777212, tgt = 10001, message = 1, attempts = 5",
	} {
		if e := ParseEvent(line); e != nil {
			t.Fatalf("unmatched evidence accepted: %s %+v", line, e)
		}
	}
}
