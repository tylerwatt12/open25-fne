package dispatch

import (
	"context"
	"strings"
	"sync"
	"testing"
	"time"
)

type releaseRaceRouter struct {
	testRouter
	phase    string
	entered  chan struct{}
	complete chan struct{}
	once     sync.Once
}

func (r *releaseRaceRouter) wait(ctx context.Context, phase string) error {
	if r.phase != phase {
		return nil
	}
	r.once.Do(func() { close(r.entered) })
	select {
	case <-r.complete:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}
func (r *releaseRaceRouter) Begin(ctx context.Context, q CallRequest) (Grant, error) {
	g, err := r.testRouter.Begin(ctx, q)
	if err == nil {
		err = r.wait(ctx, "begin")
	}
	return g, err
}
func (r *releaseRaceRouter) Write(ctx context.Context, g Grant, f Frame) error {
	if err := r.wait(ctx, "write"); err != nil {
		return err
	}
	return r.testRouter.Write(ctx, g, f)
}
func (r *releaseRaceRouter) Renew(ctx context.Context, g Grant) error {
	if err := r.wait(ctx, "renew"); err != nil {
		return err
	}
	return r.testRouter.Renew(ctx, g)
}

func TestNormalPTTReleaseDoesNotReportCanceledBeginWriteOrRenew(t *testing.T) {
	for _, phase := range []string{"begin", "write", "renew"} {
		t.Run(phase, func(t *testing.T) {
			router := &releaseRaceRouter{phase: phase, entered: make(chan struct{}), complete: make(chan struct{})}
			h := New(Options{Router: router, Lease: 5 * time.Second, AudioIdle: 5 * time.Second})
			defer h.Close()
			c := readyClient(t, h, principal(10001, 1001))
			tx, err := h.start(c, []int{1001}, "normal-release", false, false, 0, 0)
			if err != nil {
				t.Fatal(err)
			}
			if phase != "begin" {
				waitGrant(t, h, tx)
			}
			if phase == "write" {
				if err = h.acceptPCM(c, make([]byte, FrameBytes), time.Now()); err != nil {
					t.Fatal(err)
				}
			}
			select {
			case <-router.entered:
			case <-time.After(time.Second):
				t.Fatal("route operation was not in flight")
			}
			if err = h.stopRequest(c, tx.RequestID); err != nil {
				t.Fatal(err)
			}
			if phase == "write" {
				close(router.complete)
			} // Drain the actual in-flight word.
			waitEnd(t, tx)
			h.mu.Lock()
			reason := tx.Error
			h.mu.Unlock()
			if reason != "" {
				t.Fatal("normal release became an audio failure:", reason)
			}
			if _, _, ends := router.counts(); ends != 1 {
				t.Fatal("matching grant was not released once", ends)
			}
			for {
				select {
				case value := <-c.send:
					if message, ok := value.(map[string]any); ok && message["type"] == "error" {
						t.Fatal("normal release sent an operator error", message)
					}
				default:
					return
				}
			}
		})
	}
}

func TestIndependentRenewalDeadlineStillEndsActiveTransmission(t *testing.T) {
	router := &releaseRaceRouter{phase: "renew", entered: make(chan struct{})}
	h := New(Options{Router: router, Lease: 5 * time.Second, AudioIdle: 5 * time.Second})
	defer h.Close()
	c := readyClient(t, h, principal(10001, 1001))
	tx, err := h.start(c, []int{1001}, "real-renew-deadline", false, false, 0, 0)
	if err != nil {
		t.Fatal(err)
	}
	waitGrant(t, h, tx)
	waitEnd(t, tx)
	h.mu.Lock()
	reason := tx.Error
	h.mu.Unlock()
	if !strings.Contains(reason, "audio channel lease renewal failed") || !strings.Contains(reason, context.DeadlineExceeded.Error()) {
		t.Fatal("independent renewal deadline was hidden:", reason)
	}
	if tx.Context.Err() == nil {
		t.Fatal("failed call stayed active")
	}
	if _, _, ends := router.counts(); ends != 1 {
		t.Fatal("failed grant was not released exactly once", ends)
	}
}

func TestExplicitCancellationDoesNotReportCanceledWrite(t *testing.T) {
	router := &releaseRaceRouter{phase: "write", entered: make(chan struct{})}
	h := New(Options{Router: router})
	defer h.Close()
	c := readyClient(t, h, principal(10001, 1001))
	tx, err := h.start(c, []int{1001}, "cancel-write", false, false, 0, 0)
	if err != nil {
		t.Fatal(err)
	}
	waitGrant(t, h, tx)
	if err = h.acceptPCM(c, make([]byte, FrameBytes), time.Now()); err != nil {
		t.Fatal(err)
	}
	select {
	case <-router.entered:
	case <-time.After(time.Second):
		t.Fatal("write was not in flight")
	}
	h.stop(tx, "")
	waitEnd(t, tx)
	h.mu.Lock()
	reason := tx.Error
	h.mu.Unlock()
	if reason != "" {
		t.Fatal("cancellation became an audio failure", reason)
	}
	if _, _, ends := router.counts(); ends != 1 {
		t.Fatal("matching grant was not released exactly once")
	}
}
