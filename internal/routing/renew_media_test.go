package routing

import (
	"context"
	"errors"
	"slices"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"open25.local/open25-fne/internal/dispatch"
)

type renewalTarget struct {
	*testTarget
	renew func(context.Context, dispatch.Grant) error
	began chan dispatch.Grant
	ended chan dispatch.Grant
}

func (s *renewalTarget) Begin(ctx context.Context, q dispatch.CallRequest) (dispatch.Grant, error) {
	g, err := s.testTarget.Begin(ctx, q)
	if err == nil && s.began != nil {
		s.began <- g
	}
	return g, err
}
func (s *renewalTarget) Renew(ctx context.Context, g dispatch.Grant) error {
	if s.renew != nil {
		return s.renew(ctx, g)
	}
	return s.testTarget.Renew(ctx, g)
}
func (s *renewalTarget) End(ctx context.Context, g dispatch.Grant) error {
	err := s.testTarget.End(ctx, g)
	if s.ended != nil {
		s.ended <- g
	}
	return err
}
func receiveRenewalResult[T any](t *testing.T, ch <-chan T) T {
	t.Helper()
	select {
	case value := <-ch:
		return value
	case <-time.After(3 * time.Second):
		t.Fatal("timed out waiting for call lifecycle barrier")
		var zero T
		return zero
	}
}

func TestSlowSiteRenewalDoesNotBlockLiveWriteOrIPPublish(t *testing.T) {
	entered := make(chan struct{})
	release := make(chan struct{})
	var once sync.Once
	target := &renewalTarget{testTarget: &testTarget{id: "site"}, renew: func(ctx context.Context, _ dispatch.Grant) error {
		close(entered)
		select {
		case <-release:
			return nil
		case <-ctx.Done():
			return ctx.Err()
		}
	}}
	published := make(chan dispatch.Frame, 1)
	r := New(Options{Targets: func(q Request) Selection {
		return Selection{Targets: []Target{{ID: "site", Router: target, Groups: q.Talkgroups}}}
	}, Publish: func(_ Request, f dispatch.Frame) {
		if !f.End {
			published <- f
		}
	}})
	t.Cleanup(func() { once.Do(func() { close(release) }); _ = r.Close() })
	g, err := r.Begin(context.Background(), "owner", request("slow-renew-media", 1001))
	if err != nil {
		t.Fatal(err)
	}
	renewed := make(chan error, 1)
	go func() { renewed <- r.Renew(context.Background(), "owner", g) }()
	receiveRenewalResult(t, entered)
	wrote := make(chan error, 1)
	go func() { wrote <- r.Write(context.Background(), "owner", g, frame(g)) }()
	if err = receiveRenewalResult(t, wrote); err != nil {
		t.Fatal(err)
	}
	if got := receiveRenewalResult(t, published); got.CallID != g.ID {
		t.Fatal("wrong IP call published")
	}
	_, frames, _, _ := target.counts()
	if frames != 1 {
		t.Fatal("live media did not reach the receiving Site")
	}
	select {
	case <-renewed:
		t.Fatal("fixture lease RPC unexpectedly completed")
	default:
	}
	once.Do(func() { close(release) })
	if err = receiveRenewalResult(t, renewed); err != nil {
		t.Fatal(err)
	}
}

func TestEndCancelsAndWaitsForInFlightSiteRenewal(t *testing.T) {
	entered := make(chan struct{})
	cancelled := make(chan struct{})
	release := make(chan struct{})
	var once sync.Once
	target := &renewalTarget{testTarget: &testTarget{id: "site"}, renew: func(ctx context.Context, _ dispatch.Grant) error {
		close(entered)
		<-ctx.Done()
		close(cancelled)
		<-release
		return ctx.Err()
	}}
	r := New(Options{Targets: func(q Request) Selection {
		return Selection{Targets: []Target{{ID: "site", Router: target, Groups: q.Talkgroups}}}
	}})
	t.Cleanup(func() { once.Do(func() { close(release) }); _ = r.Close() })
	g, err := r.Begin(context.Background(), "owner", request("end-renew", 1001))
	if err != nil {
		t.Fatal(err)
	}
	renewed := make(chan error, 1)
	go func() { renewed <- r.Renew(context.Background(), "owner", g) }()
	receiveRenewalResult(t, entered)
	ended := make(chan error, 1)
	go func() { ended <- r.End(context.Background(), "owner", g) }()
	receiveRenewalResult(t, cancelled)
	select {
	case <-ended:
		t.Fatal("End returned before the in-flight renewal retired")
	default:
	}
	if _, err = r.Begin(context.Background(), "other", request("before-renew-retired", 1001)); !errors.Is(err, dispatch.ErrBusy) {
		t.Fatal("floor reused before renewal retired", err)
	}
	once.Do(func() { close(release) })
	if err = receiveRenewalResult(t, renewed); !errors.Is(err, context.Canceled) {
		t.Fatal("End did not cancel renewal context", err)
	}
	if err = receiveRenewalResult(t, ended); err != nil {
		t.Fatal(err)
	}
	if len(r.Active()) != 0 || len(r.Delivery("owner", g)) != 0 {
		t.Fatal("late renewal restored an ended call")
	}
	_, _, _, ends := target.counts()
	if ends != 1 {
		t.Fatal("exact Site allocation was not finalized once", ends)
	}
}

func TestCallerCancellationDoesNotDetachHealthySite(t *testing.T) {
	entered := make(chan struct{})
	target := &renewalTarget{testTarget: &testTarget{id: "site"}, renew: func(ctx context.Context, _ dispatch.Grant) error {
		close(entered)
		<-ctx.Done()
		return ctx.Err()
	}}
	r := newRouter(t, Options{Targets: func(q Request) Selection {
		return Selection{Targets: []Target{{ID: "site", Router: target, Groups: q.Talkgroups}}}
	}})
	g, err := r.Begin(context.Background(), "owner", request("caller-cancel-renew", 1001))
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	renewed := make(chan error, 1)
	go func() { renewed <- r.Renew(ctx, "owner", g) }()
	receiveRenewalResult(t, entered)
	cancel()
	if err = receiveRenewalResult(t, renewed); !errors.Is(err, context.Canceled) {
		t.Fatal(err)
	}
	delivery := r.Delivery("owner", g)
	if len(delivery) != 1 || delivery[0].State != "active" {
		t.Fatal("caller cancellation was treated as a Site failure", delivery)
	}
	if err = r.Write(context.Background(), "owner", g, frame(g)); err != nil {
		t.Fatal(err)
	}
	_, frames, _, ends := target.counts()
	if frames != 1 || ends != 0 {
		t.Fatal("healthy Site was detached after caller cancellation", frames, ends)
	}
}

func TestLateFailedRenewalCannotRemoveReplacementSiteAllocation(t *testing.T) {
	entered := make(chan struct{})
	release := make(chan struct{})
	var once sync.Once
	target := &renewalTarget{testTarget: &testTarget{id: "site"}, began: make(chan dispatch.Grant, 4), ended: make(chan dispatch.Grant, 4), renew: func(ctx context.Context, _ dispatch.Grant) error {
		close(entered)
		select {
		case <-release:
			return dispatch.ErrUnavailable
		case <-ctx.Done():
			return ctx.Err()
		}
	}}
	var interested atomic.Bool
	interested.Store(true)
	events := make(chan string, 8)
	r := New(Options{Targets: func(q Request) Selection {
		if !interested.Load() {
			return Selection{}
		}
		return Selection{Targets: []Target{{ID: "site", Router: target, Groups: q.Talkgroups}}}
	}, Event: func(_ Request, kind, message string) {
		if kind == "site_omitted" {
			events <- message
		}
	}})
	t.Cleanup(func() { once.Do(func() { close(release) }); _ = r.Close() })
	g, err := r.Begin(context.Background(), "owner", request("replace-during-renew", 1001))
	if err != nil {
		t.Fatal(err)
	}
	old := receiveRenewalResult(t, target.began)
	renewed := make(chan error, 1)
	go func() { renewed <- r.Renew(context.Background(), "owner", g) }()
	receiveRenewalResult(t, entered)
	interested.Store(false)
	if retired := receiveRenewalResult(t, target.ended); retired.ID != old.ID {
		t.Fatal("wrong initial Site allocation retired")
	}
	interested.Store(true)
	replacement := receiveRenewalResult(t, target.began)
	if replacement.ID == old.ID {
		t.Fatal("replacement reused an old Site allocation")
	}
	once.Do(func() { close(release) })
	if err = receiveRenewalResult(t, renewed); err != nil {
		t.Fatal("stale Site failure ended the logical call", err)
	}
	delivery := r.Delivery("owner", g)
	if len(delivery) != 1 || delivery[0].State != "active" {
		t.Fatal("stale failed renewal overwrote current Site delivery", delivery)
	}
	c, err := r.find("owner", g)
	if err != nil {
		t.Fatal(err)
	}
	c.media.RLock()
	legs := slices.Clone(c.legs)
	c.media.RUnlock()
	if len(legs) != 1 || legs[0].grant.ID != replacement.ID {
		t.Fatal("failed old renewal removed or restored an incorrect Site allocation")
	}
	if err = r.Write(context.Background(), "owner", g, frame(g)); err != nil {
		t.Fatal(err)
	}
	target.mu.Lock()
	frames := slices.Clone(target.frames)
	target.mu.Unlock()
	if len(frames) != 1 || frames[0].CallID != replacement.ID {
		t.Fatal("live audio did not retain the replacement Site allocation")
	}
	select {
	case event := <-events:
		t.Fatal("stale failure emitted an unavailable Site event", event)
	default:
	}
	if err = r.End(context.Background(), "owner", g); err != nil {
		t.Fatal(err)
	}
	_, _, _, ends := target.counts()
	if ends != 2 {
		t.Fatal("old and replacement allocations were not each finalized once", ends)
	}
}
