package sipvoice

import (
	"context"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

func TestIndependentSourceCallbackQuiescesBeforeDialogRelease(t *testing.T) {
	a := startTestNode(t, "source-owner-review", nil)
	entered, canceled, exited := make(chan struct{}), make(chan struct{}), make(chan struct{})
	proceed := make(chan struct{})
	var proceedOnce sync.Once
	releaseCallback := func() { proceedOnce.Do(func() { close(proceed) }) }
	t.Cleanup(releaseCallback)
	var active atomic.Bool
	released := make(chan bool, 1)
	b := startTestNode(t, "source-recipient-review", func(o *Options) {
		original := o.OnInvite
		o.OnInvite = func(ctx context.Context, s *Session) (Answer, error) {
			answer, err := original(ctx, s)
			release := answer.Release
			answer.Release = func() { released <- active.Load(); release() }
			return answer, err
		}
		o.OnSource = func(ctx context.Context, _ *Session, _ Source) error {
			active.Store(true)
			close(entered)
			<-ctx.Done()
			close(canceled)
			<-proceed
			active.Store(false)
			close(exited)
			return ctx.Err()
		}
	})
	s, err := a.n.Invite(t.Context(), b.peer, testCall("independent-source-close"), endpoints(12000))
	if err != nil {
		t.Fatal(err)
	}
	incoming := <-b.incoming
	readyResult := make(chan error, 1)
	go func() { readyResult <- s.SourceReady(t.Context(), Source{Epoch: 1, TSN: 1, MFID: 0, Service: 4}) }()
	await(t, entered)
	if err := s.End(t.Context()); err != nil {
		t.Fatal(err)
	}
	select {
	case <-canceled:
	case <-time.After(150 * time.Millisecond):
		t.Error("BYE released dialog without canceling active source callback context")
	}
	select {
	case overlap := <-released:
		t.Errorf("allocation released before source callback completed; active=%v", overlap)
	default:
	}
	select {
	case <-incoming.Done():
		t.Error("dialog Done closed before active source callback relinquished allocation")
	default:
	}
	releaseCallback()
	if active.Load() {
		_ = b.n.Close()
	} // Baseline cleanup also cancels its node-scoped context.
	await(t, exited)
	await(t, incoming.Done())
	select {
	case err := <-readyResult:
		if err == nil {
			t.Error("closed dialog admitted source")
		}
	case <-time.After(time.Second):
		t.Error("outgoing source request did not retire")
	}
}
