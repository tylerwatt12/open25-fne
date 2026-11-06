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

type testTarget struct {
	mu                               sync.Mutex
	id                               string
	beginErr, errorWrite, errorRenew error
	block                            <-chan struct{}
	ignoreCancel                     bool
	endBlock                         <-chan struct{}
	endStarted                       chan<- struct{}
	requests                         []dispatch.CallRequest
	frames                           []dispatch.Frame
	renews                           int
	ends                             []dispatch.Grant
}

func (t *testTarget) Begin(ctx context.Context, q dispatch.CallRequest) (dispatch.Grant, error) {
	t.mu.Lock()
	q.Talkgroups = slices.Clone(q.Talkgroups)
	t.requests = append(t.requests, q)
	err, block, ignoreCancel := t.beginErr, t.block, t.ignoreCancel
	t.mu.Unlock()
	if block != nil {
		if ignoreCancel {
			<-block
		} else {
			select {
			case <-block:
			case <-ctx.Done():
				return dispatch.Grant{}, ctx.Err()
			}
		}
	}
	if err != nil {
		return dispatch.Grant{}, err
	}
	return dispatch.Grant{ID: q.ID + "@" + t.id, Talkgroups: slices.Clone(q.Talkgroups)}, nil
}
func (t *testTarget) Write(_ context.Context, g dispatch.Grant, f dispatch.Frame) error {
	t.mu.Lock()
	defer t.mu.Unlock()
	if f.CallID != g.ID {
		return errors.New("wrong Site leg ID")
	}
	f.PCM = slices.Clone(f.PCM)
	t.frames = append(t.frames, f)
	return t.errorWrite
}
func (t *testTarget) Renew(context.Context, dispatch.Grant) error {
	t.mu.Lock()
	defer t.mu.Unlock()
	t.renews++
	return t.errorRenew
}
func (t *testTarget) End(ctx context.Context, g dispatch.Grant) error {
	t.mu.Lock()
	t.ends = append(t.ends, g)
	block, started := t.endBlock, t.endStarted
	t.mu.Unlock()
	if started != nil {
		started <- struct{}{}
	}
	if block != nil {
		select {
		case <-block:
		case <-ctx.Done():
			return ctx.Err()
		}
	}
	return nil
}
func (t *testTarget) counts() (int, int, int, int) {
	t.mu.Lock()
	defer t.mu.Unlock()
	return len(t.requests), len(t.frames), t.renews, len(t.ends)
}

func request(id string, groups ...int) Request {
	return Request{CallRequest: dispatch.CallRequest{ID: id, SessionID: "session-1", UserID: "user-1", RadioID: 10002, Talkgroups: groups}, SourceKind: "console", Proof: "private-proof"}
}
func frame(g dispatch.Grant) dispatch.Frame {
	return dispatch.Frame{CallID: g.ID, TalkgroupID: g.Talkgroups[0], SourceID: 10002, Sequence: 1, PCM: make([]byte, dispatch.FrameBytes), CapturedAt: time.Now()}
}
func newRouter(t *testing.T, o Options) *Router {
	t.Helper()
	r := New(o)
	t.Cleanup(func() { _ = r.Close() })
	return r
}

func TestRouterOneFloorPerGroupAndIdempotentOwnedRequest(t *testing.T) {
	target := &testTarget{id: "site-1"}
	r := newRouter(t, Options{Targets: func(q Request) Selection {
		return Selection{Targets: []Target{{ID: "site-1", Router: target, Groups: q.Talkgroups}}}
	}})
	q := request("call-one", 1002, 1001)
	g, err := r.Begin(context.Background(), "dispatch-1", q)
	if err != nil {
		t.Fatal(err)
	}
	if !slices.Equal(g.Talkgroups, []int{1001, 1002}) {
		t.Fatal("group set not normalized")
	}
	retry, err := r.Begin(context.Background(), "dispatch-1", q)
	if err != nil || retry.ID != g.ID {
		t.Fatal("same owned request was not idempotent")
	}
	begins, _, _, _ := target.counts()
	if begins != 1 {
		t.Fatal("retry allocated duplicate RF Site leg")
	}
	if _, err = r.Begin(context.Background(), "different-dispatch", q); !errors.Is(err, dispatch.ErrBusy) {
		t.Fatal("different owner took existing call ID")
	}
	if _, err = r.Begin(context.Background(), "dispatch-2", request("call-overlap", 1002)); !errors.Is(err, dispatch.ErrBusy) {
		t.Fatal("two calls owned the same group floor")
	}
	independent := request("call-independent", 1003)
	independent.RadioID = 10003
	if _, err = r.Begin(context.Background(), "dispatch-2", independent); err != nil {
		t.Fatal("unrelated group was blocked")
	}
	if err = r.End(context.Background(), "dispatch-1", g); err != nil {
		t.Fatal(err)
	}
	if _, err = r.Begin(context.Background(), "dispatch-2", request("call-after-release", 1001)); err != nil {
		t.Fatal("group floor leaked after release")
	}
}

func TestRouterMultiselectAdmissionFailureRollsBackEveryReservedFloorAndLeg(t *testing.T) {
	first := &testTarget{id: "site-1"}
	busy := &testTarget{id: "site-2", beginErr: dispatch.ErrBusy}
	var failed atomic.Bool
	failed.Store(true)
	r := newRouter(t, Options{Targets: func(q Request) Selection {
		if failed.Load() {
			return Selection{Targets: []Target{{ID: "site-1", Router: first, Groups: []int{1001}}, {ID: "site-2", Router: busy, Groups: []int{1002}}}}
		}
		return Selection{}
	}})
	if _, err := r.Begin(context.Background(), "dispatch-1", request("batch", 1001, 1002)); !errors.Is(err, dispatch.ErrBusy) {
		t.Fatal("partial multiselect was silently accepted")
	}
	_, _, _, ends := first.counts()
	if ends != 1 {
		t.Fatal("successful partial Site allocation not released")
	}
	if len(r.Active()) != 0 {
		t.Fatal("failed admission became active")
	}
	failed.Store(false)
	for _, tg := range []int{1001, 1002} {
		q := request("retry-"+string(rune(tg)), tg)
		q.RadioID = tg + 10000
		if _, err := r.Begin(context.Background(), "dispatch-2", q); err != nil {
			t.Fatal("failed transaction leaked group reservation")
		}
	}
}

func TestRouterSingleGroupBusySiteIsOmittedAndReported(t *testing.T) {
	ready := &testTarget{id: "site-ready"}
	busy := &testTarget{id: "site-busy", beginErr: dispatch.ErrBusy}
	var mu sync.Mutex
	events := []string{}
	r := newRouter(t, Options{Targets: func(q Request) Selection {
		return Selection{Targets: []Target{{ID: "site-ready", Router: ready, Groups: q.Talkgroups}, {ID: "site-busy", Router: busy, Groups: q.Talkgroups}}}
	}, Event: func(_ Request, kind, message string) {
		mu.Lock()
		events = append(events, kind+":"+message)
		mu.Unlock()
	}})
	g, err := r.Begin(context.Background(), "dispatch-1", request("one-group", 1001))
	if err != nil {
		t.Fatal(err)
	}
	if err = r.Write(context.Background(), "dispatch-1", g, frame(g)); err != nil {
		t.Fatal(err)
	}
	_, writes, _, _ := ready.counts()
	if writes != 1 {
		t.Fatal("ready Site did not receive audio")
	}
	_, writes, _, _ = busy.counts()
	if writes != 0 {
		t.Fatal("omitted Site received audio")
	}
	mu.Lock()
	defer mu.Unlock()
	if !slices.ContainsFunc(events, func(e string) bool {
		return len(e) >= len("site_omitted:site-busy") && e[:len("site_omitted:site-busy")] == "site_omitted:site-busy"
	}) {
		t.Fatal("busy Site was silently omitted")
	}
}

func TestRouterIPOnlyCallPublishesWithoutRFAllocation(t *testing.T) {
	var mu sync.Mutex
	published := []dispatch.Frame{}
	r := newRouter(t, Options{Targets: func(Request) Selection { return Selection{} }, Publish: func(_ Request, f dispatch.Frame) { mu.Lock(); published = append(published, f); mu.Unlock() }})
	g, err := r.Begin(context.Background(), "dispatch-1", request("ip-only", 1001, 1002))
	if err != nil {
		t.Fatal(err)
	}
	if err = r.Write(context.Background(), "dispatch-1", g, frame(g)); err != nil {
		t.Fatal(err)
	}
	mu.Lock()
	if len(published) != 2 || published[0].TalkgroupID != 1001 || published[1].TalkgroupID != 1002 {
		t.Fatal("IP-only listeners did not receive their group legs")
	}
	mu.Unlock()
	if err = r.End(context.Background(), "dispatch-1", g); err != nil {
		t.Fatal(err)
	}
	mu.Lock()
	defer mu.Unlock()
	if len(published) != 4 || !published[2].End || !published[3].End {
		t.Fatal("IP-only end was not delivered")
	}
}

func TestRouterOriginSiteIsExcludedFromReceivingLegs(t *testing.T) {
	origin := &testTarget{id: "origin"}
	other := &testTarget{id: "other"}
	r := newRouter(t, Options{Targets: func(q Request) Selection {
		return Selection{Targets: []Target{{ID: "site-origin", Router: origin, Groups: q.Talkgroups}, {ID: "site-other", Router: other, Groups: q.Talkgroups}}}
	}})
	q := request("rf-source", 1001)
	q.OriginSite = "site-origin"
	q.SourceKind = "subscriber"
	g, err := r.Begin(context.Background(), "site-origin", q)
	if err != nil {
		t.Fatal(err)
	}
	if err = r.Write(context.Background(), "site-origin", g, frame(g)); err != nil {
		t.Fatal(err)
	}
	b, w, _, _ := origin.counts()
	if b != 0 || w != 0 {
		t.Fatal("RF origin was looped back as a destination")
	}
	b, w, _, _ = other.counts()
	if b != 1 || w != 1 {
		t.Fatal("remote eligible Site was not routed")
	}
}

func TestRouterRejectsWrongSourceEncryptedAndStaleFramesBeforePublication(t *testing.T) {
	var published atomic.Int32
	r := newRouter(t, Options{Publish: func(Request, dispatch.Frame) { published.Add(1) }})
	g, err := r.Begin(context.Background(), "dispatch-1", request("media-validation", 1001))
	if err != nil {
		t.Fatal(err)
	}
	cases := []struct {
		name   string
		change func(*dispatch.Frame)
	}{{"wrong source", func(f *dispatch.Frame) { f.SourceID = 10001 }}, {"wrong call", func(f *dispatch.Frame) { f.CallID = "other-call" }}, {"encrypted", func(f *dispatch.Frame) { f.Encrypted = true }}, {"expired", func(f *dispatch.Frame) { f.CapturedAt = time.Now().Add(-time.Second) }}, {"future", func(f *dispatch.Frame) { f.CapturedAt = time.Now().Add(time.Minute) }}, {"missing timestamp", func(f *dispatch.Frame) { f.CapturedAt = time.Time{} }}, {"short frame", func(f *dispatch.Frame) { f.PCM = f.PCM[:2] }}}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			f := frame(g)
			tc.change(&f)
			if err := r.Write(context.Background(), "dispatch-1", g, f); err == nil {
				t.Fatal("invalid media was accepted")
			}
		})
	}
	if published.Load() != 0 {
		t.Fatal("rejected media reached listeners")
	}
	if err = r.Write(context.Background(), "dispatch-1", g, frame(g)); err != nil {
		t.Fatal("valid audio rejected")
	}
	if published.Load() != 1 {
		t.Fatal("valid audio not published")
	}
}

func TestRouterOnlyOwnerCanRenewWriteOrEndCall(t *testing.T) {
	target := &testTarget{id: "site-1"}
	r := newRouter(t, Options{Targets: func(q Request) Selection {
		return Selection{Targets: []Target{{ID: "site-1", Router: target, Groups: q.Talkgroups}}}
	}})
	g, err := r.Begin(context.Background(), "owner", request("owned-call", 1001))
	if err != nil {
		t.Fatal(err)
	}
	for _, op := range []func() error{func() error { return r.Write(context.Background(), "other", g, frame(g)) }, func() error { return r.Renew(context.Background(), "other", g) }, func() error { return r.End(context.Background(), "other", g) }} {
		if !errors.Is(op(), dispatch.ErrPermission) {
			t.Fatal("another service controlled the call")
		}
	}
	if len(r.Active()) != 1 {
		t.Fatal("wrong owner ended the call")
	}
	if err = r.Renew(context.Background(), "owner", g); err != nil {
		t.Fatal(err)
	}
	if err = r.End(context.Background(), "owner", g); err != nil {
		t.Fatal(err)
	}
	if err = r.End(context.Background(), "owner", g); err != nil {
		t.Fatal("repeat end was not idempotent")
	}
	_, _, renews, ends := target.counts()
	if renews != 1 || ends != 1 {
		t.Fatal("leg ownership/cleanup was not exact")
	}
}

func TestRouterGrantTimeoutAndCanceledSetupReleaseReservations(t *testing.T) {
	blocked := make(chan struct{})
	target := &testTarget{id: "blocked", block: blocked}
	r := newRouter(t, Options{Targets: func(q Request) Selection {
		return Selection{Targets: []Target{{ID: "site-1", Router: target, Groups: q.Talkgroups}}}
	}})
	ctx, cancel := context.WithTimeout(context.Background(), 25*time.Millisecond)
	defer cancel()
	if _, err := r.Begin(ctx, "owner", request("canceled-setup", 1001)); !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("setup timeout: %v", err)
	}
	if len(r.Active()) != 0 {
		t.Fatal("canceled setup became active")
	}
	close(blocked)
	if _, err := r.Begin(context.Background(), "other", request("after-cancel", 1001)); err != nil {
		t.Fatal("canceled setup leaked its floor")
	}
}

func TestRouterLeaseRenewalAndOwnerCancellationCleanLegs(t *testing.T) {
	target := &testTarget{id: "site-1"}
	var current atomic.Int64
	current.Store(time.Now().UnixNano())
	now := func() time.Time { return time.Unix(0, current.Load()) }
	r := newRouter(t, Options{Lease: time.Second, Now: now, Targets: func(q Request) Selection {
		return Selection{Targets: []Target{{ID: "site-1", Router: target, Groups: q.Talkgroups}}}
	}})
	g, err := r.Begin(context.Background(), "owner", request("lease", 1001))
	if err != nil {
		t.Fatal(err)
	}
	current.Add(int64(800 * time.Millisecond))
	if err = r.Renew(context.Background(), "owner", g); err != nil {
		t.Fatal(err)
	}
	current.Add(int64(500 * time.Millisecond))
	time.Sleep(120 * time.Millisecond)
	if len(r.Active()) != 1 {
		t.Fatal("renewed call expired at original deadline")
	}
	current.Add(int64(time.Second))
	deadline := time.Now().Add(time.Second)
	for len(r.Active()) > 0 && time.Now().Before(deadline) {
		time.Sleep(10 * time.Millisecond)
	}
	if len(r.Active()) != 0 {
		t.Fatal("expired lease remained active")
	}
	deadline = time.Now().Add(time.Second)
	for time.Now().Before(deadline) {
		_, _, _, ends := target.counts()
		if ends == 1 {
			break
		}
		time.Sleep(time.Millisecond)
	}
	_, _, _, ends := target.counts()
	if ends != 1 {
		t.Fatal("lease expiry did not release Site leg")
	}
	if _, err = r.Begin(context.Background(), "owner", request("cancel-owner", 1001)); err != nil {
		t.Fatal(err)
	}
	r.CancelOwner("owner")
	if len(r.Active()) != 0 {
		t.Fatal("disconnected owner retained floor")
	}
	_, _, _, ends = target.counts()
	if ends != 2 {
		t.Fatal("owner cancellation did not release Site leg")
	}
}

func TestRouterIdempotenceRequiresCompleteCallIdentity(t *testing.T) {
	r := newRouter(t, Options{})
	q := request("identity", 1001)
	if _, err := r.Begin(context.Background(), "owner", q); err != nil {
		t.Fatal(err)
	}
	for _, tc := range []struct {
		name   string
		change func(*Request)
	}{
		{"session", func(q *Request) { q.SessionID = "another-session" }},
		{"user", func(q *Request) { q.UserID = "another-user" }},
		{"radio", func(q *Request) { q.RadioID++ }},
		{"proof", func(q *Request) { q.Proof = "another-proof" }},
		{"source", func(q *Request) { q.SourceKind = "rf" }},
		{"origin", func(q *Request) { q.OriginSite = "another-site" }},
	} {
		t.Run(tc.name, func(t *testing.T) {
			changed := q
			tc.change(&changed)
			if _, err := r.Begin(context.Background(), "owner", changed); !errors.Is(err, dispatch.ErrBusy) {
				t.Fatalf("changed identity reused a call: %v", err)
			}
		})
	}
}

func TestRouterSnapshotsAndReturnedGrantsCannotMutateCall(t *testing.T) {
	r := newRouter(t, Options{})
	q := request("snapshot", 1001)
	g, err := r.Begin(context.Background(), "owner", q)
	if err != nil {
		t.Fatal(err)
	}
	q.Talkgroups[0] = 1009
	g.Talkgroups[0] = 1010
	active := r.Active()
	if len(active) != 1 || active[0].Proof != "" || !slices.Equal(active[0].Talkgroups, []int{1001}) {
		t.Fatalf("unsafe active snapshot: %+v", active)
	}
	active[0].Talkgroups[0] = 1011
	active = r.Active()
	if !slices.Equal(active[0].Talkgroups, []int{1001}) {
		t.Fatal("editing a snapshot changed the live call")
	}
	retry, err := r.Begin(context.Background(), "owner", request("snapshot", 1001))
	if err != nil || !slices.Equal(retry.Talkgroups, []int{1001}) {
		t.Fatal("returned grant changed the live grant")
	}
	retry.Talkgroups[0] = 1012
	if err = r.Renew(context.Background(), "owner", dispatch.Grant{ID: "snapshot", Talkgroups: []int{1001}}); err != nil {
		t.Fatal(err)
	}
}

func TestRouterCanceledIPOnlyOperationsHaveNoEffect(t *testing.T) {
	var published atomic.Int32
	r := newRouter(t, Options{Publish: func(Request, dispatch.Frame) { published.Add(1) }})
	g, err := r.Begin(context.Background(), "owner", request("cancel-media", 1001))
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if err = r.Write(ctx, "owner", g, frame(g)); !errors.Is(err, context.Canceled) {
		t.Fatalf("canceled write: %v", err)
	}
	if err = r.Renew(ctx, "owner", g); !errors.Is(err, context.Canceled) {
		t.Fatalf("canceled renewal: %v", err)
	}
	if _, err = r.Begin(ctx, "other", request("canceled-begin", 1002)); !errors.Is(err, context.Canceled) {
		t.Fatalf("canceled begin: %v", err)
	}
	if published.Load() != 0 || len(r.Active()) != 1 {
		t.Fatal("canceled operation changed call/media state")
	}
}

func TestRouterReleaseKeepsFloorUntilEverySiteHasDrained(t *testing.T) {
	unblock := make(chan struct{})
	started := make(chan struct{}, 1)
	target := &testTarget{id: "site", endBlock: unblock, endStarted: started}
	r := newRouter(t, Options{Targets: func(q Request) Selection {
		return Selection{Targets: []Target{{ID: "site", Router: target, Groups: q.Talkgroups}}}
	}})
	g, err := r.Begin(context.Background(), "owner", request("ending", 1001))
	if err != nil {
		t.Fatal(err)
	}
	ended := make(chan error, 1)
	go func() { ended <- r.End(context.Background(), "owner", g) }()
	select {
	case <-started:
	case <-time.After(time.Second):
		t.Fatal("Site release not started")
	}
	if len(r.Active()) != 0 {
		t.Fatal("ending call still accepts audio")
	}
	if _, err = r.Begin(context.Background(), "other", request("too-early", 1001)); !errors.Is(err, dispatch.ErrBusy) {
		t.Fatal("floor released before Site drain")
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if err = r.End(ctx, "owner", g); !errors.Is(err, context.Canceled) {
		t.Fatal("duplicate end did not wait for original cleanup")
	}
	close(unblock)
	if err = <-ended; err != nil {
		t.Fatal(err)
	}
	_, _, _, ends := target.counts()
	if ends != 1 {
		t.Fatal("duplicate end repeated Site release")
	}
	if _, err = r.Begin(context.Background(), "other", request("after-drain", 1001)); err != nil {
		t.Fatal("drained floor did not return to service")
	}
}

func TestRouterCancelAllReleasesEveryOwner(t *testing.T) {
	target := &testTarget{id: "site"}
	r := newRouter(t, Options{Targets: func(q Request) Selection {
		return Selection{Targets: []Target{{ID: "site", Router: target, Groups: q.Talkgroups}}}
	}})
	if _, err := r.Begin(context.Background(), "one", request("one", 1001)); err != nil {
		t.Fatal(err)
	}
	second := request("two", 1002)
	second.RadioID = 10003
	if _, err := r.Begin(context.Background(), "two", second); err != nil {
		t.Fatal(err)
	}
	r.CancelAll()
	if len(r.Active()) != 0 {
		t.Fatal("cancel all left an active owner")
	}
	_, _, _, ends := target.counts()
	if ends != 2 {
		t.Fatalf("released %d Site legs, want 2", ends)
	}
	if _, err := r.Begin(context.Background(), "new", request("after-all", 1001, 1002)); err != nil {
		t.Fatal("cancel all leaked floors")
	}
}

func TestRouterCancellationDuringAdmissionCannotReviveEndedCall(t *testing.T) {
	unblock := make(chan struct{})
	target := &testTarget{id: "site", block: unblock, ignoreCancel: true}
	r := newRouter(t, Options{Targets: func(q Request) Selection {
		return Selection{Targets: []Target{{ID: "site", Router: target, Groups: q.Talkgroups}}}
	}})
	began := make(chan error, 1)
	go func() {
		_, err := r.Begin(context.Background(), "owner", request("pending", 1001))
		began <- err
	}()
	deadline := time.Now().Add(time.Second)
	for time.Now().Before(deadline) {
		begins, _, _, _ := target.counts()
		if begins > 0 {
			break
		}
		time.Sleep(time.Millisecond)
	}
	begins, _, _, _ := target.counts()
	if begins != 1 {
		t.Fatal("Site admission not started")
	}
	r.CancelOwner("owner")
	close(unblock)
	if err := <-began; !errors.Is(err, dispatch.ErrUnavailable) {
		t.Fatalf("canceled admission returned a live grant: %v", err)
	}
	if len(r.Active()) != 0 {
		t.Fatal("canceled admission revived a call")
	}
	_, _, _, ends := target.counts()
	if ends != 1 {
		t.Fatalf("late Site grant released %d times", ends)
	}
	if _, err := r.Begin(context.Background(), "other", request("after-pending", 1001)); err != nil {
		t.Fatal("late grant leaked its floor")
	}
}

func TestRouterCancellationDuringAuthorizationDoesNotAllocateSite(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	target := &testTarget{id: "site"}
	r := newRouter(t, Options{
		Authorize: func(Request) error { cancel(); return nil },
		Targets: func(q Request) Selection {
			return Selection{Targets: []Target{{ID: "site", Router: target, Groups: q.Talkgroups}}}
		},
	})
	if _, err := r.Begin(ctx, "owner", request("cancel-authorize", 1001)); !errors.Is(err, context.Canceled) {
		t.Fatalf("canceled authorization: %v", err)
	}
	begins, _, _, _ := target.counts()
	if begins != 0 || len(r.Active()) != 0 {
		t.Fatal("canceled authorization allocated a Site call")
	}
}

func TestMediaEmergencyUsesAcceptedRouteForSitesAndIP(t *testing.T) {
	for _, emergency := range []bool{false, true} {
		target := &testTarget{id: "site-1"}
		var published []dispatch.Frame
		r := newRouter(t, Options{Targets: func(q Request) Selection {
			return Selection{Targets: []Target{{ID: "site-1", Router: target, Groups: q.Talkgroups}}}
		}, Publish: func(_ Request, f dispatch.Frame) { published = append(published, f) }})
		q := request("accepted-emergency", 1001)
		q.Emergency = emergency
		g, err := r.Begin(context.Background(), "owner", q)
		if err != nil {
			t.Fatal(err)
		}
		f := frame(g)
		f.Emergency = !emergency
		if err = r.Write(context.Background(), "owner", g, f); err != nil {
			t.Fatal(err)
		}
		if len(target.frames) != 1 || target.frames[0].Emergency != emergency || len(published) != 1 || published[0].Emergency != emergency {
			t.Fatal("media changed the accepted route emergency", target.frames, published)
		}
	}
}
