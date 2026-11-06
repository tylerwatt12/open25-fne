package radio

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"testing"
)

func TestStatusQueryEventsRequireMatchedEndpointAndPreserveFullCode(t *testing.T) {
	for _, code := range []int{0, 258, 65535} {
		line := fmt.Sprintf("I: (RF) P25, TSDU (Trunking System Data Unit), TSBKO, ISP_STS_Q_RSP (Status Query Response), status = $%04X, srcId = 10001, dstId = 16777212", code)
		e := ParseEvent(line)
		if e == nil || e.Kind != "status_query_response" || e.RID != 10001 || e.RequestorID != NativeFNEID || e.TGID != 0 || e.Code == nil || *e.Code != code {
			t.Fatalf("status response lost fields: %+v", e)
		}
	}
	for _, line := range []string{
		"I: (RF) P25, (Status Query Response), status = 65536, srcId = 10001, dstId = 16777212",
		"I: (RF) P25, (Status Query Response), srcId = 10001, dstId = 16777212",
		"I: (RF) P25, (Status Query Response), status = 258, srcId = 10001, dstId = 10002",
		"I: (NET) P25, (Status Query Response), status = 258, srcId = 10001, dstId = 16777212",
		"I: (HOST) P25 Status Query, no response, arg = 10002, tgt = 10001, attempts = 1",
		"I: (HOST) P25 Status Query, no response, arg = 16777212, tgt = 10001, attempts = 5",
	} {
		if e := ParseEvent(line); e != nil {
			t.Fatalf("invalid query evidence accepted: %s %+v", line, e)
		}
	}
	e := ParseEvent("I: (HOST) P25 Status Query, no response, arg = 16777212, tgt = 10001, attempts = 1")
	if e == nil || e.Kind != "status_query_no_response" || e.RID != 10001 || e.TGID != 0 || e.Direction != "state" || e.RequestorID != NativeFNEID || e.Attempts != 1 {
		t.Fatalf("wrong timeout: %+v", e)
	}
}

func TestStatusQueryUsesCurrentAuthenticatedControlEndpoint(t *testing.T) {
	seen := make(chan map[string]any, 1)
	endpoint := inhibitEndpoint(t, func(w http.ResponseWriter, r *http.Request) {
		if r.Method != "PUT" || r.URL.Path != "/p25/rid" {
			t.Errorf("wrong command path %s %s", r.Method, r.URL.Path)
		}
		var body map[string]any
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			t.Error(err)
		}
		seen <- body
		_ = json.NewEncoder(w).Encode(map[string]any{"status": 200})
	})
	c := inhibitController(t, endpoint, endpoint, InhibitPolicy{})
	if _, err := c.Command(context.Background(), Command{Action: "status_query", RID: 10001}); err != nil {
		t.Fatal(err)
	}
	body := <-seen
	if body["command"] != "status-query" || body["dstId"] != float64(10001) || len(body) != 2 {
		t.Fatalf("wrong native status command: %+v", body)
	}
	c.mu.Lock()
	c.status.State = "stopped"
	c.mu.Unlock()
	if _, err := c.Command(context.Background(), Command{Action: "status_query", RID: 10001}); err == nil {
		t.Fatal("stopped Site accepted status query")
	}
}
