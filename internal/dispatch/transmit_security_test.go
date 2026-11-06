package dispatch

import (
	"context"
	"encoding/json"
	"testing"
	"time"
)

type strappedTestRouter struct {
	testRouter
	actualSecure bool
}

func (r *strappedTestRouter) Begin(ctx context.Context, q CallRequest) (Grant, error) {
	g, err := r.testRouter.Begin(ctx, q)
	g.Secure = r.actualSecure
	if g.Secure {
		g.AlgorithmID, g.KeyID = 132, 7
	} else {
		g.AlgorithmID, g.KeyID = 0, 0
	}
	return g, err
}
func TestHubReportsResolvedTransmitModeAndAppliesActualPermission(t *testing.T) {
	for _, test := range []struct {
		name                                  string
		requested, actual, permission, denied bool
	}{
		{"forced secure from clear intent", false, true, true, false},
		{"forced clear from secure intent", true, false, true, false},
		{"forced clear needs no encryption permission", true, false, false, false},
		{"forced secure needs encryption permission", false, true, false, true},
	} {
		t.Run(test.name, func(t *testing.T) {
			router := &strappedTestRouter{actualSecure: test.actual}
			h := New(Options{Router: router})
			defer h.Close()
			p := principal(10002, 1001)
			p.CanSecure = test.permission
			c := readyClient(t, h, p)
			tx, err := h.start(c, []int{1001}, "strapped-call", false, test.requested, 0, 0)
			if err != nil {
				t.Fatal(err)
			}
			select {
			case <-tx.Ready:
			case <-time.After(time.Second):
				t.Fatal("grant result missing")
			}
			h.mu.Lock()
			actual, state, failure := tx.Secure, h.stateLocked(p, c).TX, tx.Error
			h.mu.Unlock()
			if test.denied {
				if failure != ErrPermission.Error() {
					t.Fatalf("actual secure call was not denied: %s", failure)
				}
				waitFor(t, func() bool { _, _, ended := router.counts(); return ended == 1 })
				return
			}
			if failure != "" || actual != test.actual || state.Secure != test.actual || state.State != "transmitting" {
				t.Fatalf("reported requested instead of actual mode: %+v, %s", state, failure)
			}
			algorithm, key := 0, 0
			if test.actual {
				algorithm, key = 132, 7
			}
			if state.AlgorithmID != algorithm || state.KeyID != key {
				t.Fatalf("actual transmit key lost: %+v", state)
			}
			raw, err := json.Marshal(state)
			if err != nil {
				t.Fatal(err)
			}
			var fields map[string]any
			if err = json.Unmarshal(raw, &fields); err != nil {
				t.Fatal(err)
			}
			for _, name := range []string{"secure", "algorithm_id", "key_id"} {
				if _, ok := fields[name]; !ok {
					t.Fatalf("actual mode field %s missing", name)
				}
			}
			router.mu.Lock()
			requested := router.begin[0].Secure
			router.mu.Unlock()
			if requested != test.requested {
				t.Fatal("requested and actual modes not distinguished")
			}
		})
	}
}
func TestWAVRequestedEncryptionDefersToResolvedClearGrant(t *testing.T) {
	router := &strappedTestRouter{}
	h := New(Options{Router: router})
	defer h.Close()
	frames, err := h.PlayPCM(context.Background(), principal(10002, 1001), "session", []int{1001}, make([]byte, FrameBytes), true, 0, 0)
	if err != nil || frames != 1 {
		t.Fatalf("forced-clear WAV rejected by requested encryption: %d %v", frames, err)
	}
	router.actualSecure = true
	if _, err = h.PlayPCM(context.Background(), principal(10002, 1001), "session", []int{1001}, make([]byte, FrameBytes), false, 0, 0); err == nil || err.Error() != ErrPermission.Error() {
		t.Fatalf("forced-secure WAV bypassed actual permission: %v", err)
	}
}
