package dispatch

import (
	"errors"
	"sync/atomic"
	"testing"
	"time"
)

func TestReadinessChangesReachConsoleAndFenceOnlyNewCalls(t *testing.T) {
	var ready atomic.Bool
	router := &testRouter{}
	h := New(Options{Router: router, Available: ready.Load, Lease: 5 * time.Second, AudioIdle: 5 * time.Second})
	defer h.Close()
	c := readyClient(t, h, principal(10002, 1001))
	if h.Overview(c.principal).Available {
		t.Fatal("unavailable source was advertised as ready")
	}
	if _, err := h.start(c, []int{1001}, "unavailable-request", false, false, 0, 0); !errors.Is(err, ErrUnavailable) {
		t.Fatal("unavailable source accepted PTT", err)
	}
	if begins, _, _ := router.counts(); begins != 0 {
		t.Fatal("unavailable PTT reached allocation")
	}
	waitState := func(available bool) {
		t.Helper()
		deadline := time.NewTimer(2 * time.Second)
		defer deadline.Stop()
		for {
			select {
			case message := <-c.send:
				if value, ok := message.(map[string]any); ok && value["type"] == "state" {
					if state, ok := value["state"].(State); ok && state.Available == available {
						return
					}
				}
			case <-deadline.C:
				t.Fatal("console did not receive readiness change")
			}
		}
	}
	ready.Store(true)
	waitState(true)
	tx, err := h.start(c, []int{1001}, "ready-request", false, false, 0, 0)
	if err != nil {
		t.Fatal(err)
	}
	waitGrant(t, h, tx)
	ready.Store(false)
	waitState(false)
	select {
	case <-tx.Done:
		t.Fatal("new-call readiness revoked an independently owned active call")
	default:
	}
	if _, err = h.start(c, []int{1001}, "later-request", false, false, 0, 0); !errors.Is(err, ErrUnavailable) {
		t.Fatal("readiness loss accepted another PTT", err)
	}
	h.stop(tx, "released")
	waitEnd(t, tx)
}
