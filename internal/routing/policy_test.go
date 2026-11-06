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
	"open25.local/open25-fne/internal/p25rtp"
)

func waitFor(t *testing.T, check func() bool) {
	t.Helper()
	until := time.Now().Add(time.Second)
	for !check() {
		if time.Now().After(until) {
			t.Fatal("timed out waiting for routing state")
		}
		time.Sleep(time.Millisecond)
	}
}

func TestFASTStartAddsBusySiteWithoutReplayingAudio(t *testing.T) {
	ready := &testTarget{id: "ready"}
	busy := &testTarget{id: "busy", beginErr: dispatch.ErrBusy}
	r := newRouter(t, Options{Policy: func(Request) Policy { return Policy{Retry: 10 * time.Millisecond} }, Targets: func(q Request) Selection {
		return Selection{Targets: []Target{{"ready", ready, q.Talkgroups}, {"busy", busy, q.Talkgroups}}}
	}})
	g, err := r.Begin(context.Background(), "owner", request("late", 1001))
	if err != nil {
		t.Fatal(err)
	}
	first := frame(g)
	if err = r.Write(context.Background(), "owner", g, first); err != nil {
		t.Fatal(err)
	}
	busy.mu.Lock()
	busy.beginErr = nil
	busy.mu.Unlock()
	waitFor(t, func() bool {
		return slices.ContainsFunc(r.Delivery("owner", g), func(d Delivery) bool { return d.SiteID == "busy" && d.State == "active" })
	})
	second := frame(g)
	second.Sequence = 2
	if err = r.Write(context.Background(), "owner", g, second); err != nil {
		t.Fatal(err)
	}
	busy.mu.Lock()
	defer busy.mu.Unlock()
	if len(busy.frames) != 1 || busy.frames[0].Sequence != 2 {
		t.Fatalf("late Site received replayed or missing audio: %+v", busy.frames)
	}
}

func TestFASTStartAddsNewRFInterestAndRemovesWithdrawnSite(t *testing.T) {
	target := &testTarget{id: "site"}
	var interested atomic.Bool
	r := newRouter(t, Options{Policy: func(Request) Policy { return Policy{Retry: 10 * time.Millisecond} }, Targets: func(q Request) Selection {
		if interested.Load() {
			return Selection{Targets: []Target{{"site", target, q.Talkgroups}}}
		}
		return Selection{}
	}})
	g, err := r.Begin(context.Background(), "owner", request("interest", 1001))
	if err != nil {
		t.Fatal(err)
	}
	if n, _, _, _ := target.counts(); n != 0 {
		t.Fatal("IP-only call manufactured RF interest")
	}
	interested.Store(true)
	waitFor(t, func() bool {
		return slices.ContainsFunc(r.Delivery("owner", g), func(d Delivery) bool { return d.State == "active" })
	})
	interested.Store(false)
	waitFor(t, func() bool { _, _, _, n := target.counts(); return n == 1 })
	if err = r.Write(context.Background(), "owner", g, frame(g)); err != nil {
		t.Fatal(err)
	}
	_, frames, _, _ := target.counts()
	if frames != 0 {
		t.Fatal("withdrawn Site continued receiving audio")
	}
}

func TestAllStartWaitsAndReleasesPartialAttempts(t *testing.T) {
	ready := &testTarget{id: "ready"}
	busy := &testTarget{id: "busy", beginErr: dispatch.ErrBusy}
	r := newRouter(t, Options{Policy: func(Request) Policy { return Policy{StartMode: AllStart, Retry: 10 * time.Millisecond} }, Targets: func(q Request) Selection {
		return Selection{Targets: []Target{{"ready", ready, q.Talkgroups}, {"busy", busy, q.Talkgroups}}}
	}})
	type result struct {
		g   dispatch.Grant
		err error
	}
	done := make(chan result, 1)
	go func() { g, err := r.Begin(context.Background(), "owner", request("all", 1001)); done <- result{g, err} }()
	waitFor(t, func() bool { n, _, _, ends := ready.counts(); return n >= 2 && ends >= 1 })
	if len(r.Active()) != 0 {
		t.Fatal("AllStart returned a partial call")
	}
	busy.mu.Lock()
	busy.beginErr = nil
	busy.mu.Unlock()
	var got result
	select {
	case got = <-done:
	case <-time.After(time.Second):
		t.Fatal("AllStart did not grant after resources became free")
	}
	if got.err != nil {
		t.Fatal(got.err)
	}
	if len(r.Delivery("owner", got.g)) != 2 {
		t.Fatal("AllStart did not admit both Sites")
	}
	begins, _, _, ends := ready.counts()
	if begins != ends+1 {
		t.Fatalf("partial allocations leaked: begins=%d ends=%d", begins, ends)
	}
}

func TestAllStartDeadlineAndOwnerCancellationFreeFloors(t *testing.T) {
	for _, cancelOwner := range []bool{false, true} {
		t.Run(map[bool]string{false: "deadline", true: "owner cancellation"}[cancelOwner], func(t *testing.T) {
			target := &testTarget{id: "busy", beginErr: dispatch.ErrBusy}
			r := newRouter(t, Options{Policy: func(Request) Policy {
				return Policy{StartMode: AllStart, Wait: 60 * time.Millisecond, Retry: 10 * time.Millisecond}
			}, Targets: func(q Request) Selection { return Selection{Targets: []Target{{"busy", target, q.Talkgroups}}} }})
			done := make(chan error, 1)
			go func() { _, err := r.Begin(context.Background(), "owner", request("waiting", 1001)); done <- err }()
			waitFor(t, func() bool { n, _, _, _ := target.counts(); return n > 0 })
			if cancelOwner {
				r.CancelOwner("owner")
			}
			select {
			case err := <-done:
				if cancelOwner && !errors.Is(err, dispatch.ErrUnavailable) {
					t.Fatalf("canceled call: %v", err)
				}
				if !cancelOwner && !errors.Is(err, dispatch.ErrBusy) {
					t.Fatalf("wait timeout: %v", err)
				}
			case <-time.After(time.Second):
				t.Fatal("setup failed to stop")
			}
			target.mu.Lock()
			target.beginErr = nil
			target.mu.Unlock()
			if _, err := r.Begin(context.Background(), "other", request("after-wait", 1001)); err != nil {
				t.Fatalf("pending call leaked its floor: %v", err)
			}
		})
	}
}

func TestOneRadioIDCannotHaveConcurrentOwnersOnDifferentTalkgroups(t *testing.T) {
	r := newRouter(t, Options{})
	g, err := r.Begin(context.Background(), "one", request("one", 1001))
	if err != nil {
		t.Fatal(err)
	}
	if _, err = r.Begin(context.Background(), "two", request("two", 1002)); !errors.Is(err, dispatch.ErrBusy) {
		t.Fatalf("Radio ID had two simultaneous owners: %v", err)
	}
	if err = r.End(context.Background(), "one", g); err != nil {
		t.Fatal(err)
	}
	if _, err = r.Begin(context.Background(), "two", request("two", 1002)); err != nil {
		t.Fatal("released Radio ID stayed reserved")
	}
}

func TestPatchExpansionReservesSharedFloorButAuthorizesOriginalGroup(t *testing.T) {
	var enabled atomic.Bool
	enabled.Store(true)
	var mu sync.Mutex
	seen := []int{}
	r := newRouter(t, Options{
		Authorize: func(q Request) error {
			if !slices.Equal(q.Talkgroups, []int{1001}) {
				return dispatch.ErrPermission
			}
			return nil
		},
		Expand: func(q Request) (Request, error) {
			if enabled.Load() {
				q.Talkgroups = []int{1001, 1002}
			}
			return q, nil
		},
		Publish: func(_ Request, f dispatch.Frame) {
			if !f.End {
				mu.Lock()
				seen = append(seen, f.TalkgroupID)
				mu.Unlock()
			}
		},
	})
	g, err := r.Begin(context.Background(), "owner", request("patch", 1001))
	if err != nil {
		t.Fatal(err)
	}
	if !slices.Equal(g.Talkgroups, []int{1001, 1002}) {
		t.Fatal("patch did not reserve both member floors")
	}
	active := r.Active()
	if len(active) != 1 || !slices.Equal(active[0].AuthorizationGroups, []int{1001}) {
		t.Fatal("source group was lost")
	}
	if err = r.Write(context.Background(), "owner", g, frame(g)); err != nil {
		t.Fatal(err)
	}
	mu.Lock()
	received := slices.Clone(seen)
	mu.Unlock()
	if !slices.Equal(received, []int{1001, 1002}) {
		t.Fatalf("member calls were not routed through patch: %v", received)
	}
	enabled.Store(false)
	if err = r.Write(context.Background(), "owner", g, frame(g)); err == nil {
		t.Fatal("deactivated patch kept forwarding audio")
	}
	if len(r.Active()) != 0 {
		t.Fatal("stale patch retained its active call")
	}
}

func TestExpansionCannotReplaceCallerIdentity(t *testing.T) {
	r := newRouter(t, Options{Expand: func(q Request) (Request, error) { q.RadioID++; return q, nil }})
	if _, err := r.Begin(context.Background(), "owner", request("replace", 1001)); !errors.Is(err, dispatch.ErrPermission) {
		t.Fatalf("expansion substituted source identity: %v", err)
	}
}

type capacityTarget struct {
	mu       sync.Mutex
	capacity int
	active   map[string]dispatch.CallRequest
	ended    []string
}

func (c *capacityTarget) Begin(_ context.Context, q dispatch.CallRequest) (dispatch.Grant, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if _, ok := c.active[q.ID]; !ok && len(c.active) >= c.capacity {
		return dispatch.Grant{}, dispatch.ErrBusy
	}
	c.active[q.ID] = q
	return dispatch.Grant{ID: q.ID, Talkgroups: slices.Clone(q.Talkgroups)}, nil
}
func (c *capacityTarget) Write(context.Context, dispatch.Grant, dispatch.Frame) error { return nil }
func (c *capacityTarget) Renew(context.Context, dispatch.Grant) error                 { return nil }
func (c *capacityTarget) End(_ context.Context, g dispatch.Grant) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	delete(c.active, g.ID)
	c.ended = append(c.ended, g.ID)
	return nil
}

func TestEmergencyPreemptsLowestOrdinaryCallAtBusySite(t *testing.T) {
	target := &capacityTarget{capacity: 2, active: map[string]dispatch.CallRequest{}}
	r := newRouter(t, Options{Targets: func(q Request) Selection { return Selection{Targets: []Target{{"site", target, q.Talkgroups}}} }})
	high := request("high", 1001)
	high.Priority = 2
	low := request("low", 1002)
	low.RadioID = 10003
	low.Priority = 10
	if _, err := r.Begin(context.Background(), "high", high); err != nil {
		t.Fatal(err)
	}
	if _, err := r.Begin(context.Background(), "low", low); err != nil {
		t.Fatal(err)
	}
	target.mu.Lock()
	lowID := ""
	for id, q := range target.active {
		if q.RadioID == low.RadioID {
			lowID = id
		}
	}
	target.mu.Unlock()
	emergency := request("emergency", 1003)
	emergency.RadioID = 10004
	emergency.Emergency = true
	if _, err := r.Begin(context.Background(), "emergency", emergency); err != nil {
		t.Fatal(err)
	}
	target.mu.Lock()
	defer target.mu.Unlock()
	if lowID == "" || !slices.Equal(target.ended, []string{lowID}) {
		t.Fatalf("wrong preemption victim: %v", target.ended)
	}
	propagated := false
	for _, q := range target.active {
		if q.RadioID == emergency.RadioID && q.Emergency && q.Priority == 1 {
			propagated = true
		}
	}
	if len(target.active) != 2 || !propagated {
		t.Fatal("emergency priority was not propagated to Site")
	}
}

func TestEmergencyInterruptsOrdinaryFloorButDoesNotInterruptEmergency(t *testing.T) {
	target := &testTarget{id: "site"}
	r := newRouter(t, Options{Targets: func(q Request) Selection { return Selection{Targets: []Target{{"site", target, q.Talkgroups}}} }})
	if _, err := r.Begin(context.Background(), "ordinary", request("ordinary", 1001)); err != nil {
		t.Fatal(err)
	}
	emergency := request("emergency", 1001)
	emergency.RadioID = 10003
	emergency.Emergency = true
	if _, err := r.Begin(context.Background(), "emergency", emergency); err != nil {
		t.Fatal(err)
	}
	second := request("second-emergency", 1001)
	second.RadioID = 10004
	second.Emergency = true
	if _, err := r.Begin(context.Background(), "second", second); !errors.Is(err, dispatch.ErrBusy) {
		t.Fatalf("another emergency was preempted: %v", err)
	}
	_, _, _, ends := target.counts()
	if ends != 1 {
		t.Fatalf("unexpected preemption count: %d", ends)
	}
}

func TestEncryptedMediaRemainsOpaqueAndMustMatchGrantedKey(t *testing.T) {
	target := &testTarget{id: "site"}
	var received dispatch.Frame
	r := newRouter(t, Options{Targets: func(q Request) Selection { return Selection{Targets: []Target{{"site", target, q.Talkgroups}}} }, Publish: func(_ Request, f dispatch.Frame) {
		if !f.End {
			received = f
		}
	}})
	q := request("secure", 1001)
	q.Secure = true
	q.KeyID = 7
	q.AlgorithmID = 132
	g, err := r.Begin(context.Background(), "owner", q)
	if err != nil {
		t.Fatal(err)
	}
	f := frame(g)
	f.PCM = nil
	f.Encoded = make([]byte, 11)
	f.Encrypted = true
	f.ServiceOptions = 0x44
	f.P25 = &dispatch.P25VoiceMetadata{Type: p25rtp.FR1}
	f.ManufacturerID = 1
	f.AlgorithmID = 0x84
	f.KeyID = 7
	f.AlgorithmID = 132
	f.MessageIndicator = []byte{0x12, 0x34, 0x56, 0x78, 0x90, 0xAB, 0xCD, 0xEF, 0x00}
	if err = r.Write(context.Background(), "owner", g, f); err != nil {
		t.Fatal(err)
	}
	if !received.Encrypted || len(received.PCM) != 0 || !slices.Equal(received.Encoded, f.Encoded) || received.KeyID != 7 || received.ManufacturerID != 1 {
		t.Fatal("opaque encrypted voice was altered")
	}
	f.AlgorithmID = 129
	if err = r.Write(context.Background(), "owner", g, f); err == nil {
		t.Fatal("ungranted algorithm with matching KID accepted")
	}
	f.KeyID = 8
	f.AlgorithmID = 132
	if err = r.Write(context.Background(), "owner", g, f); err == nil {
		t.Fatal("ungranted key was accepted")
	}
	if err = r.Write(context.Background(), "owner", g, frame(g)); err == nil {
		t.Fatal("secure call silently downgraded to clear audio")
	}
}

func TestReceivingAttemptsHaveDistinctIDsAcrossRetryAndRejoin(t *testing.T) {
	target := &testTarget{id: "one-use-site"}
	var interest atomic.Bool
	interest.Store(true)
	r := newRouter(t, Options{Policy: func(Request) Policy { return Policy{Retry: 10 * time.Millisecond} }, Targets: func(q Request) Selection {
		if interest.Load() {
			return Selection{Targets: []Target{{"site", target, q.Talkgroups}}}
		}
		return Selection{}
	}})
	g, err := r.Begin(context.Background(), "owner", request("origin-stays-stable", 1001))
	if err != nil {
		t.Fatal(err)
	}
	interest.Store(false)
	waitFor(t, func() bool { _, _, _, ends := target.counts(); return ends == 1 })
	interest.Store(true)
	waitFor(t, func() bool { begins, _, _, _ := target.counts(); return begins == 2 })
	target.mu.Lock()
	first, second := target.requests[0].ID, target.requests[1].ID
	target.mu.Unlock()
	if first == second || first == g.ID || second == g.ID {
		t.Fatal("receiving attempt reused an ended or original identity")
	}
	if err = r.Write(context.Background(), "owner", g, frame(g)); err != nil {
		t.Fatal(err)
	}
}
func TestExpandCannotReplaceTrustedAuthorizationGroups(t *testing.T) {
	r := newRouter(t, Options{Expand: func(q Request) (Request, error) {
		q.AuthorizationGroups = []int{1002}
		q.Talkgroups = []int{1001, 1002}
		return q, nil
	}})
	q := request("trusted-scope", 1001)
	q.AuthorizationGroups = []int{1001}
	if _, err := r.Begin(context.Background(), "owner", q); !errors.Is(err, dispatch.ErrPermission) {
		t.Fatal("patch changed original authorization scope", err)
	}
}

func TestOriginSiteReceivesPatchMembersWithoutRepeatingItsRFSource(t *testing.T) {
	target := &testTarget{id: "origin"}
	r := newRouter(t, Options{Expand: func(q Request) (Request, error) { q.Talkgroups = []int{1001, 1002}; return q, nil }, Targets: func(q Request) Selection { return Selection{Targets: []Target{{"origin", target, q.Talkgroups}}} }})
	q := request("rf-patch", 1001)
	q.SourceKind = "rf"
	q.OriginSite = "origin"
	g, err := r.Begin(context.Background(), "site", q)
	if err != nil {
		t.Fatal(err)
	}
	target.mu.Lock()
	requests := slices.Clone(target.requests)
	target.mu.Unlock()
	if len(requests) != 1 || !slices.Equal(requests[0].Talkgroups, []int{1002}) {
		t.Fatalf("RF source looped back or patch member was lost: %+v", requests)
	}
	if err = r.Write(context.Background(), "site", g, frame(g)); err != nil {
		t.Fatal(err)
	}
}

func TestJoinedSiteFailureLeavesHealthySitesAndIPAudioActive(t *testing.T) {
	faulty := &testTarget{id: "faulty"}
	healthy := &testTarget{id: "healthy"}
	var published atomic.Int32
	r := newRouter(t, Options{Policy: func(Request) Policy { return Policy{Retry: 10 * time.Millisecond} }, Targets: func(q Request) Selection {
		return Selection{Targets: []Target{{"faulty", faulty, q.Talkgroups}, {"healthy", healthy, q.Talkgroups}}}
	}, Publish: func(Request, dispatch.Frame) { published.Add(1) }})
	g, err := r.Begin(context.Background(), "owner", request("isolation", 1001))
	if err != nil {
		t.Fatal(err)
	}
	faulty.mu.Lock()
	faulty.errorWrite = errors.New("Site link lost")
	faulty.beginErr = dispatch.ErrUnavailable
	faulty.mu.Unlock()
	if err = r.Write(context.Background(), "owner", g, frame(g)); err != nil {
		t.Fatal("one Site stopped the call", err)
	}
	if published.Load() != 1 {
		t.Fatal("one Site stopped IP audio")
	}
	waitFor(t, func() bool { _, _, _, ends := faulty.counts(); return ends == 1 })
	delivery := r.Delivery("owner", g)
	if !slices.ContainsFunc(delivery, func(d Delivery) bool { return d.SiteID == "faulty" && d.State == "unavailable" && d.Message != "" }) {
		t.Fatal("failed Site was not reported", delivery)
	}
	if err = r.Renew(context.Background(), "owner", g); err != nil {
		t.Fatal("healthy call could not renew", err)
	}
	f := frame(g)
	f.Sequence = 2
	if err = r.Write(context.Background(), "owner", g, f); err != nil {
		t.Fatal(err)
	}
	_, frames, _, ends := healthy.counts()
	if frames != 2 || ends != 0 || published.Load() != 2 {
		t.Fatal("healthy Site/IP audio stopped")
	}
	faulty.mu.Lock()
	faulty.beginErr = nil
	faulty.errorWrite = nil
	faulty.mu.Unlock()
	waitFor(t, func() bool {
		return slices.ContainsFunc(r.Delivery("owner", g), func(d Delivery) bool { return d.SiteID == "faulty" && d.State == "active" })
	})
	f = frame(g)
	f.Sequence = 3
	if err = r.Write(context.Background(), "owner", g, f); err != nil {
		t.Fatal(err)
	}
	faulty.mu.Lock()
	defer faulty.mu.Unlock()
	if len(faulty.frames) != 2 || faulty.frames[1].Sequence != 3 {
		t.Fatal("rejoined Site replayed missed speech")
	}
}
func TestSiteRenewalFailureDetachesOnlyThatSiteAndEndWaitsForItsDrain(t *testing.T) {
	drain := make(chan struct{})
	started := make(chan struct{}, 1)
	faulty := &testTarget{id: "faulty", endBlock: drain, endStarted: started}
	healthy := &testTarget{id: "healthy"}
	r := newRouter(t, Options{Targets: func(q Request) Selection {
		return Selection{Targets: []Target{{"faulty", faulty, q.Talkgroups}, {"healthy", healthy, q.Talkgroups}}}
	}})
	g, err := r.Begin(context.Background(), "owner", request("renew-isolation", 1001))
	if err != nil {
		t.Fatal(err)
	}
	faulty.mu.Lock()
	faulty.errorRenew = dispatch.ErrUnavailable
	faulty.beginErr = dispatch.ErrUnavailable
	faulty.mu.Unlock()
	if err = r.Renew(context.Background(), "owner", g); err != nil {
		t.Fatal("Site failure cancelled healthy calls", err)
	}
	select {
	case <-started:
	case <-time.After(time.Second):
		t.Fatal("detached Site cleanup not started")
	}
	ended := make(chan error, 1)
	go func() { ended <- r.End(context.Background(), "owner", g) }()
	select {
	case <-ended:
		t.Fatal("call ended before detached RF drained")
	case <-time.After(20 * time.Millisecond):
	}
	if _, err = r.Begin(context.Background(), "other", request("before-cleanup", 1001)); !errors.Is(err, dispatch.ErrBusy) {
		t.Fatal("floor reused before detached cleanup", err)
	}
	close(drain)
	if err = <-ended; err != nil {
		t.Fatal(err)
	}
	_, _, _, healthyEnds := healthy.counts()
	if healthyEnds != 1 {
		t.Fatal("healthy Site was not finalized exactly once")
	}
}

func TestFASTStartBlackholedFirstSiteDoesNotPreventHealthySiteAdmission(t *testing.T) {
	blocked := make(chan struct{})
	bad := &testTarget{id: "blackholed", block: blocked}
	good := &testTarget{id: "healthy"}
	r := newRouter(t, Options{Targets: func(q Request) Selection {
		return Selection{Targets: []Target{{"blackholed", bad, q.Talkgroups}, {"healthy", good, q.Talkgroups}}}
	}})
	ctx, done := context.WithTimeout(context.Background(), 2*time.Second)
	defer done()
	started := time.Now()
	g, err := r.Begin(ctx, "owner", request("independent-sites", 1001))
	if err != nil {
		t.Fatal(err)
	}
	if time.Since(started) > 1500*time.Millisecond {
		t.Fatal("blackholed Site consumed the caller's full deadline")
	}
	if !slices.ContainsFunc(r.Delivery("owner", g), func(d Delivery) bool { return d.SiteID == "healthy" && d.State == "active" }) {
		t.Fatal("healthy Site was not admitted")
	}
	if err = r.Write(context.Background(), "owner", g, frame(g)); err != nil {
		t.Fatal(err)
	}
	_, frames, _, _ := good.counts()
	if frames != 1 {
		t.Fatal("healthy Site did not receive live audio")
	}
}
