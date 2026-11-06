package sipvoice

import (
	"context"
	"errors"
	"sync/atomic"
	"testing"
	"time"

	"github.com/emiago/sipgo/sip"
)

func TestActualSourceBindingAndSuccessiveSpurts(t *testing.T) {
	a := startTestNode(t, "source", nil)
	var bindings atomic.Int32
	b := startTestNode(t, "recipient", func(o *Options) {
		o.OnSource = func(_ context.Context, _ *Session, s Source) error {
			if s.Service&0xC0 != 0x40 {
				return ErrUnauthorized
			}
			bindings.Add(1)
			return nil
		}
	})
	s, err := a.n.Invite(t.Context(), b.peer, testCall("source-binding"), endpoints(12000))
	if err != nil {
		t.Fatal(err)
	}
	incoming := <-b.incoming
	for _, source := range []Source{{Epoch: 1, TSN: 2, MFID: 1, Service: 0x40, AlgorithmID: 132, KeyID: 7}, {Epoch: 2, TSN: 4, MFID: 0, Service: 0x47, AlgorithmID: 129, KeyID: 29}} {
		if err = s.SourceReady(t.Context(), source); err != nil {
			t.Fatal(err)
		}
		if err = s.SourceReady(t.Context(), source); err != nil {
			t.Fatal("exact repeat", err)
		}
		changed := source
		changed.KeyID++
		if err = s.SourceReady(t.Context(), changed); !errors.Is(err, ErrRejected) {
			t.Fatal("same spurt changed its key identity", err)
		}
		changed = source
		changed.Service ^= 1
		if err = s.SourceReady(t.Context(), changed); !errors.Is(err, ErrRejected) {
			t.Fatal("same spurt changed its binding", err)
		}
	}
	if bindings.Load() != 2 {
		t.Fatal("duplicate source exchange repeated admission", bindings.Load())
	}
	if err = s.SourceReady(t.Context(), Source{Epoch: 3, TSN: 6, MFID: 1, Service: 4}); !errors.Is(err, ErrRejected) {
		t.Fatal("policy-rejected source accepted", err)
	}
	if err = incoming.SourceReady(t.Context(), Source{Epoch: 1, TSN: 2}); !errors.Is(err, ErrRejected) {
		t.Fatal("recipient originated a source spurt", err)
	}
	if err = s.End(t.Context()); err != nil {
		t.Fatal(err)
	}
	await(t, incoming.Done())
	if err = s.SourceReady(t.Context(), Source{Epoch: 3, TSN: 6}); !errors.Is(err, ErrClosed) {
		t.Fatal("closed call accepted a source", err)
	}
}

func TestSourceReadyWaitsForIncomingACKAndCancellation(t *testing.T) {
	a := startTestNode(t, "a", nil)
	var bindings atomic.Int32
	b := startTestNode(t, "b", func(o *Options) {
		o.OnSource = func(context.Context, *Session, Source) error { bindings.Add(1); return nil }
	})
	ack := make(chan struct{})
	b.n.server.OnAck(func(r *sip.Request, tx sip.ServerTransaction) {
		select {
		case <-ack:
			b.n.onAck(r, tx)
		case <-b.n.ctx.Done():
		}
	})
	s, err := a.n.Invite(t.Context(), b.peer, testCall("ack-order"), endpoints(12000))
	if err != nil {
		t.Fatal(err)
	}
	source := Source{Epoch: 1, TSN: 1, MFID: 1, Service: 4}
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	if err = s.SourceReady(ctx, source); !errors.Is(err, context.Canceled) {
		t.Fatal("canceled source exchange", err)
	}
	done := make(chan error, 1)
	go func() { done <- s.SourceReady(t.Context(), source) }()
	select {
	case err = <-done:
		t.Fatal("source binding did not wait for ACK", err)
	case <-time.After(20 * time.Millisecond):
	}
	if bindings.Load() != 0 {
		t.Fatal("source bound before dialog confirmation")
	}
	close(ack)
	if err = <-done; err != nil {
		t.Fatal(err)
	}
	if bindings.Load() != 1 {
		t.Fatal("source callback count", bindings.Load())
	}
}

func TestSourceEpochAndTSNWrap(t *testing.T) {
	for _, v := range []struct{ old, next uint8 }{{125, 127}, {127, 1}, {124, 126}, {126, 2}} {
		old := Source{Epoch: 10, TSN: v.old}
		next := Source{Epoch: 11, TSN: v.next}
		if !next.follows(&old) {
			t.Fatal("valid parity-preserving TSN progression rejected", v)
		}
		next.TSN++
		if next.follows(&old) {
			t.Fatal("TSN parity changed", v)
		}
	}
}
