package dispatch

import (
	"context"
	"sync/atomic"
	"testing"
)

type identityTestRouter struct {
	testRouter
	renews    atomic.Int32
	released  atomic.Int32
	deny      atomic.Bool
	failClaim bool
}

func (r *identityTestRouter) ClaimAudio(context.Context, Principal, string) error {
	if r.failClaim {
		return ErrPermission
	}
	return nil
}
func (r *identityTestRouter) RenewAudio(context.Context, Principal, string) error {
	n := r.renews.Add(1)
	if r.deny.Load() {
		return ErrPermission
	}
	if n == 1 {
		return ErrUnavailable
	}
	return nil
}
func (r *identityTestRouter) ReleaseAudio(context.Context, Principal, string) error {
	r.released.Add(1)
	return nil
}

func TestAudioLeaseToleratesTransportHandoffButRevocationClosesTheClient(t *testing.T) {
	router := &identityTestRouter{}
	h := New(Options{Router: router})
	defer h.Close()
	client := readyClient(t, h, principal(10001, 1001))
	waitFor(t, func() bool { return router.renews.Load() >= 2 })
	if client.ctx.Err() != nil {
		t.Fatal("a temporary controller handoff closed browser audio")
	}
	router.deny.Store(true)
	waitFor(t, func() bool { return client.ctx.Err() != nil })
	waitFor(t, func() bool { return router.released.Load() > 0 })
}

func TestRejectedAudioClaimIsCleanedUpWithoutRetainingTheLocalRID(t *testing.T) {
	router := &identityTestRouter{failClaim: true}
	h := New(Options{Router: router})
	defer h.Close()
	if _, err := h.openClient(principal(10001, 1001), "login"); err == nil {
		t.Fatal("refused remote claim opened audio")
	}
	h.mu.Lock()
	count := len(h.clients) + len(h.byRadio)
	h.mu.Unlock()
	if count != 0 || router.released.Load() != 1 {
		t.Fatal("failed admission did not clean up its identity")
	}
}
