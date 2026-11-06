package sipvoice

import (
	"context"
	"errors"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/emiago/sipgo"
	"github.com/emiago/sipgo/sip"
)

func reviewInfo(t *testing.T, s *Session, pkg, body string) *sip.Request {
	t.Helper()
	r, err := s.dialogRequest(sip.INFO)
	if err != nil {
		t.Fatal(err)
	}
	r.AppendHeader(sip.NewHeader("Info-Package", pkg))
	r.AppendHeader(sip.NewHeader("Content-Disposition", "Info-Package"))
	if pkg == sourcePackage {
		r.AppendHeader(sip.NewHeader("Content-Type", sourceContentType))
	}
	if body != "" {
		r.SetBody([]byte(body))
	}
	return r
}
func reviewInfoResponse(t *testing.T, s *Session, r *sip.Request) *sip.Response {
	t.Helper()
	r.RemoveHeader("Via") // A new transaction reaches the dialog's replay decision.
	ctx, cancel := context.WithTimeout(t.Context(), 2*time.Second)
	defer cancel()
	tx, err := s.node.client.TransactionRequest(ctx, r, sipgo.ClientRequestAddVia)
	if err != nil {
		t.Fatal(err)
	}
	defer tx.Terminate()
	return finalResponse(t, tx)
}
func reviewDeadline(s *Session) time.Time { s.mu.Lock(); defer s.mu.Unlock(); return s.leaseDeadline }

func TestIndependentAllocationLeaseExpiresWithoutRenewal(t *testing.T) {
	a := startTestNode(t, "lease-caller", nil)
	var renews atomic.Int32
	b := startTestNode(t, "lease-callee", func(o *Options) { o.OnRenew = func(context.Context, *Session) error { renews.Add(1); return nil } })
	s, err := a.n.Invite(t.Context(), b.peer, testCall("lease-expiry"), endpoints(12000))
	if err != nil {
		t.Fatal(err)
	}
	incoming := <-b.incoming
	await(t, incoming.Ready())
	await(t, incoming.Done())
	await(t, s.Done())
	if incoming.Reason() != EndTimeout || b.ends.Load() != 1 || renews.Load() != 0 {
		t.Fatalf("lease outcome %v ends%d renews%d", incoming.Reason(), b.ends.Load(), renews.Load())
	}
	if err = s.Renew(t.Context()); !errors.Is(err, ErrClosed) {
		t.Fatalf("closed renewal: %v", err)
	}
}
func TestIndependentRenewKeepsHeldAllocationWithoutMedia(t *testing.T) {
	a := startTestNode(t, "held-caller", nil)
	var renews atomic.Int32
	b := startTestNode(t, "held-callee", func(o *Options) { o.OnRenew = func(context.Context, *Session) error { renews.Add(1); return nil } })
	s, err := a.n.Invite(t.Context(), b.peer, testCall("held-no-spoken-spurt"), endpoints(12000))
	if err != nil {
		t.Fatal(err)
	}
	incoming := <-b.incoming
	await(t, incoming.Ready())
	for range 4 {
		time.Sleep(400 * time.Millisecond)
		if err = s.Renew(t.Context()); err != nil {
			t.Fatal(err)
		}
	}
	select {
	case <-incoming.Done():
		t.Fatal("held allocation expired despite current renewals")
	default:
	}
	if renews.Load() != 4 {
		t.Fatal("renewal callback count", renews.Load())
	}
	if err = incoming.Renew(t.Context()); !errors.Is(err, ErrRejected) {
		t.Fatal("callee renewed caller allocation", err)
	}
	if err = s.End(t.Context()); err != nil {
		t.Fatal(err)
	}
	await(t, incoming.Done())
	if b.ends.Load() != 1 {
		t.Fatal("release not once")
	}
}
func TestIndependentRenewDuplicatesAndDenialsDoNotMoveDeadline(t *testing.T) {
	a := startTestNode(t, "replay-caller", nil)
	var renews atomic.Int32
	var denied atomic.Bool
	b := startTestNode(t, "replay-callee", func(o *Options) {
		o.OnRenew = func(context.Context, *Session) error {
			renews.Add(1)
			if denied.Load() {
				return ErrUnauthorized
			}
			return nil
		}
	})
	s, err := a.n.Invite(t.Context(), b.peer, testCall("renew-replay"), endpoints(12000))
	if err != nil {
		t.Fatal(err)
	}
	incoming := <-b.incoming
	await(t, incoming.Ready())
	request := reviewInfo(t, s, renewPackage, "")
	if got := reviewInfoResponse(t, s, request).StatusCode; got != 200 {
		t.Fatal(got)
	}
	deadline := reviewDeadline(incoming)
	for range 3 {
		time.Sleep(250 * time.Millisecond)
		if got := reviewInfoResponse(t, s, request.Clone()).StatusCode; got != 200 {
			t.Fatal("exact duplicate", got)
		}
		if !reviewDeadline(incoming).Equal(deadline) {
			t.Fatal("duplicate refreshed lease")
		}
	}
	stale := request.Clone()
	stale.CSeq().SeqNo--
	if got := reviewInfoResponse(t, s, stale).StatusCode != 481; got {
		t.Fatal("stale renewal accepted")
	}
	denied.Store(true)
	if err = s.Renew(t.Context()); err == nil {
		t.Fatal("policy-rejected renewal accepted")
	}
	if !reviewDeadline(incoming).Equal(deadline) {
		t.Fatal("denied renewal refreshed lease")
	}
	if renews.Load() != 2 {
		t.Fatal("duplicate/stale repeated callback", renews.Load())
	}
	await(t, incoming.Done())
	await(t, s.Done())
	if b.ends.Load() != 1 {
		t.Fatal("expired allocation released repeatedly")
	}
	if got := reviewInfoResponse(t, s, request.Clone()).StatusCode; got != 481 {
		t.Fatal("closed dialog duplicate revived lease", got)
	}
}
func TestIndependentSourceAndRenewShareWireCSeqOrder(t *testing.T) {
	a := startTestNode(t, "ordered-caller", nil)
	entered, proceed := make(chan struct{}), make(chan struct{})
	var once sync.Once
	var renews atomic.Int32
	b := startTestNode(t, "ordered-callee", func(o *Options) {
		o.OnSource = func(ctx context.Context, _ *Session, _ Source) error {
			once.Do(func() { close(entered) })
			select {
			case <-proceed:
				return nil
			case <-ctx.Done():
				return ctx.Err()
			}
		}
		o.OnRenew = func(context.Context, *Session) error { renews.Add(1); return nil }
	})
	s, err := a.n.Invite(t.Context(), b.peer, testCall("ordered-info"), endpoints(12000))
	if err != nil {
		t.Fatal(err)
	}
	<-b.incoming
	source := make(chan error, 1)
	renew := make(chan error, 1)
	go func() { source <- s.SourceReady(t.Context(), Source{Epoch: 1, TSN: 1, Service: 4}) }()
	await(t, entered)
	go func() { renew <- s.Renew(t.Context()) }()
	select {
	case err := <-renew:
		t.Fatal("renew overtook pending source", err)
	case <-time.After(20 * time.Millisecond):
	}
	if renews.Load() != 0 {
		t.Fatal("renew reached callback ahead of source")
	}
	close(proceed)
	if err = <-source; err != nil {
		t.Fatal(err)
	}
	if err = <-renew; err != nil {
		t.Fatal(err)
	}
	for epoch := uint64(2); epoch <= 6; epoch++ {
		start := make(chan struct{})
		sresult, rresult := make(chan error, 1), make(chan error, 1)
		go func() {
			<-start
			sresult <- s.SourceReady(t.Context(), Source{Epoch: epoch, TSN: uint8(epoch*2 - 1), MFID: 1, Service: 0x44})
		}()
		go func() { <-start; rresult <- s.Renew(t.Context()) }()
		close(start)
		if err = <-sresult; err != nil {
			t.Fatal("concurrent source", err)
		}
		if err = <-rresult; err != nil {
			t.Fatal("concurrent renewal", err)
		}
	}
	if renews.Load() != 6 {
		t.Fatal("lost fresh renewal", renews.Load())
	}
	if err = s.End(t.Context()); err != nil {
		t.Fatal(err)
	}
}
func TestIndependentRenewLeaseStartsAtReceivedACK(t *testing.T) {
	a := startTestNode(t, "ack-lease-caller", nil)
	b := startTestNode(t, "ack-lease-callee", func(o *Options) { o.OnRenew = func(context.Context, *Session) error { return nil } })
	ack := make(chan struct{})
	var once sync.Once
	t.Cleanup(func() { once.Do(func() { close(ack) }) })
	b.n.server.OnAck(func(r *sip.Request, tx sip.ServerTransaction) {
		select {
		case <-ack:
			b.n.onAck(r, tx)
		case <-b.n.ctx.Done():
		}
	})
	s, err := a.n.Invite(t.Context(), b.peer, testCall("ack-starts-lease"), endpoints(12000))
	if err != nil {
		t.Fatal(err)
	}
	incoming := <-b.incoming
	select {
	case <-incoming.Done():
		t.Fatal("lease started before ACK")
	case <-time.After(1600 * time.Millisecond):
	}
	if !reviewDeadline(incoming).IsZero() {
		t.Fatal("unconfirmed dialog had allocation lease")
	}
	once.Do(func() { close(ack) })
	await(t, incoming.Ready())
	if err = s.Renew(t.Context()); err != nil {
		t.Fatal(err)
	}
	if err = s.End(t.Context()); err != nil {
		t.Fatal(err)
	}
	await(t, incoming.Done())
}
func TestIndependentNodeCloseJoinsActiveRenewBeforeRelease(t *testing.T) {
	a := startTestNode(t, "close-renew-caller", nil)
	entered, canceled, proceed := make(chan struct{}), make(chan struct{}), make(chan struct{})
	var once sync.Once
	t.Cleanup(func() { once.Do(func() { close(proceed) }) })
	var active atomic.Bool
	released := make(chan bool, 1)
	b := startTestNode(t, "close-renew-callee", func(o *Options) {
		original := o.OnInvite
		o.OnInvite = func(ctx context.Context, s *Session) (Answer, error) {
			answer, err := original(ctx, s)
			release := answer.Release
			answer.Release = func() { released <- active.Load(); release() }
			return answer, err
		}
		o.OnRenew = func(ctx context.Context, _ *Session) error {
			active.Store(true)
			close(entered)
			<-ctx.Done()
			close(canceled)
			<-proceed
			active.Store(false)
			return ctx.Err()
		}
	})
	s, err := a.n.Invite(t.Context(), b.peer, testCall("node-close-renew"), endpoints(12000))
	if err != nil {
		t.Fatal(err)
	}
	incoming := <-b.incoming
	result := make(chan error, 1)
	go func() { result <- s.Renew(t.Context()) }()
	await(t, entered)
	closed := make(chan struct{})
	go func() { b.n.Close(); close(closed) }()
	await(t, canceled)
	select {
	case <-closed:
		t.Fatal("Close returned during callback")
	case <-released:
		t.Fatal("Release ran during callback")
	case <-time.After(20 * time.Millisecond):
	}
	once.Do(func() { close(proceed) })
	await(t, closed)
	await(t, incoming.Done())
	if overlap := <-released; overlap {
		t.Fatal("Release overlapped callback")
	}
	if err = <-result; err == nil {
		t.Fatal("closed renewal succeeded")
	}
}
