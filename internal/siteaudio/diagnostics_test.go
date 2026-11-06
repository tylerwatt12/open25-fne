package siteaudio

import (
	"context"
	"encoding/json"
	"errors"
	"slices"
	"strings"
	"sync"
	"testing"

	"open25.local/open25-fne/internal/radio"
)

type rejectedRenewalNative struct {
	base        fakeNative
	mu          sync.Mutex
	paths       []string
	state       string
	lookupError bool
}

func (n *rejectedRenewalNative) Get(ctx context.Context, path string) (map[string]json.RawMessage, error) {
	return n.base.Get(ctx, path)
}
func (n *rejectedRenewalNative) Put(ctx context.Context, path string, value any) (map[string]json.RawMessage, error) {
	n.mu.Lock()
	n.paths = append(n.paths, path)
	n.mu.Unlock()
	if path == "/p25/dispatch/renew" {
		return rawReply(map[string]any{"status": 409}), &radio.NativeError{HTTPStatus: 409, Status: 409, Message: "Native radio control rejected the request."}
	}
	if path == "/p25/dispatch/status" {
		if n.lookupError {
			return nil, &radio.NativeError{Message: "status lookup unavailable", Possible: true}
		}
		raw, _ := json.Marshal(value)
		var body map[string]any
		_ = json.Unmarshal(raw, &body)
		return rawReply(map[string]any{"status": 200, "accepted": true, "state": n.state, "srcId": body["srcId"], "dstId": body["dstId"], "leaseId": body["leaseId"], "epoch": body["epoch"]}), nil
	}
	return n.base.Put(ctx, path, value)
}

func TestRenewalFailureRecordsControllerStatusAndConfirmedLeaseState(t *testing.T) {
	for _, state := range []string{"released", "expired", "granted", "unexpected-private-data"} {
		t.Run(state, func(t *testing.T) {
			native := &rejectedRenewalNative{state: state}
			codec := &fakeCodec{}
			events := []Event{}
			router := New(Options{Control: func() (radio.Native, error) { return native, nil }, Codec: codec, HasRFInterest: func(int) bool { return true }, AllowTX: allowed, Event: func(e Event) { events = append(events, e) }})
			grant, err := router.Begin(context.Background(), request("renew-diagnostic", 1001))
			if err != nil {
				t.Fatal(err)
			}
			err = router.Renew(context.Background(), grant)
			var failure *radio.NativeError
			if !errors.As(err, &failure) || failure.HTTPStatus != 409 {
				t.Fatalf("native failure identity was lost: %v", err)
			}
			expected := "state " + diagnosticState(state)
			if !strings.Contains(err.Error(), "HTTP 409, controller 409") || !strings.Contains(err.Error(), expected) {
				t.Fatalf("missing renewal reason: %v", err)
			}
			if strings.Contains(err.Error(), "unexpected-private-data") {
				t.Fatal("unsanitized native state reached the operator")
			}
			if len(events) != 2 || events[1].Kind != "rf.renew" || events[1].Outcome != "failed" || events[1].Message != err.Error() {
				t.Fatalf("renewal failure not recorded: %+v", events)
			}
			if err = router.End(context.Background(), grant); err != nil {
				t.Fatal(err)
			}
			native.mu.Lock()
			paths := slices.Clone(native.paths)
			native.mu.Unlock()
			if count := len(slices.DeleteFunc(slices.Clone(paths), func(p string) bool { return p != "/p25/dispatch/renew" })); count != 1 {
				t.Fatal("renewal was retried")
			}
			if terminal(state) && slices.Contains(paths, "/p25/dispatch/release") {
				t.Fatal("already confirmed terminal lease was released again")
			}
		})
	}
}

func TestRenewalDiagnosticLookupFailureKeepsOriginalRejection(t *testing.T) {
	native := &rejectedRenewalNative{lookupError: true}
	router := New(Options{Control: func() (radio.Native, error) { return native, nil }, Codec: &fakeCodec{}, HasRFInterest: func(int) bool { return true }, AllowTX: allowed})
	grant, err := router.Begin(context.Background(), request("unavailable-status", 1001))
	if err != nil {
		t.Fatal(err)
	}
	err = router.Renew(context.Background(), grant)
	if err == nil || !strings.Contains(err.Error(), "HTTP 409, controller 409, state unknown") || strings.Contains(err.Error(), "status lookup unavailable") {
		t.Fatalf("diagnostic hid the real error: %v", err)
	}
}
