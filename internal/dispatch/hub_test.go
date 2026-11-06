package dispatch

import (
	"context"
	"errors"
	"slices"
	"sync"
	"testing"
	"time"
)

type testRouter struct {
	mu         sync.Mutex
	begin      []CallRequest
	writes     []Frame
	writeTimes []time.Time
	ends       []Grant
	renewDelay time.Duration
	beginError error
	beginWait  <-chan struct{}
}

func (r *testRouter) Begin(ctx context.Context, request CallRequest) (Grant, error) {
	r.mu.Lock()
	r.begin = append(r.begin, request)
	wait := r.beginWait
	err := r.beginError
	r.mu.Unlock()
	if wait != nil {
		select {
		case <-ctx.Done():
			return Grant{ID: request.ID, Talkgroups: request.Talkgroups, Secure: request.Secure, AlgorithmID: request.AlgorithmID, KeyID: request.KeyID}, ctx.Err()
		case <-wait:
		}
	}
	return Grant{ID: request.ID, Talkgroups: append([]int{}, request.Talkgroups...), Secure: request.Secure, AlgorithmID: request.AlgorithmID, KeyID: request.KeyID}, err
}
func (r *testRouter) Write(ctx context.Context, g Grant, f Frame) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	r.writes = append(r.writes, f)
	r.writeTimes = append(r.writeTimes, time.Now())
	return nil
}
func (r *testRouter) Renew(ctx context.Context, g Grant) error {
	r.mu.Lock()
	delay := r.renewDelay
	r.mu.Unlock()
	if delay > 0 {
		timer := time.NewTimer(delay)
		defer timer.Stop()
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-timer.C:
		}
	}
	return nil
}
func (r *testRouter) End(ctx context.Context, g Grant) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.ends = append(r.ends, g)
	return nil
}
func (r *testRouter) counts() (int, int, int) {
	r.mu.Lock()
	defer r.mu.Unlock()
	return len(r.begin), len(r.writes), len(r.ends)
}
func principal(id int, groups ...int) Principal {
	return Principal{UserID: "operator-" + string(rune(id)), RadioID: id, ReceiveGroups: groups, TransmitGroups: groups, CanTransmit: true}
}
func readyClient(t *testing.T, h *Hub, p Principal) *client {
	t.Helper()
	c, err := h.openClient(p, "session-"+p.UserID)
	if err != nil {
		t.Fatal(err)
	}
	c.connected.Store(true)
	t.Cleanup(func() { h.removeClient(c) })
	return c
}
func waitFor(t *testing.T, condition func() bool) {
	t.Helper()
	deadline := time.Now().Add(3 * time.Second)
	for !condition() {
		if time.Now().After(deadline) {
			t.Fatal("condition did not become true")
		}
		time.Sleep(5 * time.Millisecond)
	}
}
func waitGrant(t *testing.T, h *Hub, tx *transmission) {
	t.Helper()
	select {
	case <-tx.Ready:
	case <-time.After(2 * time.Second):
		t.Fatal("grant timed out")
	}
	h.mu.Lock()
	state, err := tx.State, tx.Error
	h.mu.Unlock()
	if state != "transmitting" {
		t.Fatalf("not transmitting: %s %s", state, err)
	}
}
func waitEnd(t *testing.T, tx *transmission) {
	t.Helper()
	select {
	case <-tx.Done:
	case <-time.After(2 * time.Second):
		t.Fatal("cleanup timed out")
	}
}

func TestIndependentTalkgroupFloorsAndAtomicMultiselect(t *testing.T) {
	r := &testRouter{}
	h := New(Options{Router: r})
	defer h.Close()
	a := readyClient(t, h, principal(10001, 1001))
	b := readyClient(t, h, principal(10002, 1002))
	c := readyClient(t, h, principal(10003, 1002, 1003))
	d := readyClient(t, h, principal(10004, 1003))
	one, err := h.start(a, []int{1001}, "one", false, false, 0, 0)
	if err != nil {
		t.Fatal(err)
	}
	two, err := h.start(b, []int{1002}, "two", false, false, 0, 0)
	if err != nil {
		t.Fatal(err)
	}
	waitGrant(t, h, one)
	waitGrant(t, h, two)
	if _, err = h.start(c, []int{1002, 1003}, "multiple", false, false, 0, 0); !errors.Is(err, ErrBusy) {
		t.Fatalf("expected busy, got %v", err)
	}
	three, err := h.start(d, []int{1003}, "three", false, false, 0, 0)
	if err != nil {
		t.Fatal("partial multiselect reserved an unrelated group:", err)
	}
	waitGrant(t, h, three)
	h.stop(one, "")
	h.stop(two, "")
	h.stop(three, "")
	waitEnd(t, one)
	waitEnd(t, two)
	waitEnd(t, three)
	_, _, ended := r.counts()
	if ended != 3 {
		t.Fatalf("ended %d grants", ended)
	}
}

func TestPermissionSessionIdentityAndRequestReplay(t *testing.T) {
	r := &testRouter{}
	h := New(Options{Router: r})
	defer h.Close()
	p := principal(10001, 1001)
	c := readyClient(t, h, p)
	if _, err := h.openClient(p, "different-session"); err == nil {
		t.Fatal("duplicate active RID accepted")
	}
	if _, err := h.start(c, []int{1002}, "unauthorized", false, false, 0, 0); !errors.Is(err, ErrPermission) {
		t.Fatal("unpermitted TG accepted")
	}
	tx, err := h.start(c, []int{1001}, "request", false, false, 0, 0)
	if err != nil {
		t.Fatal(err)
	}
	waitGrant(t, h, tx)
	if err = h.stopRequest(c, "other-request"); err == nil {
		t.Fatal("stale stop affected another request")
	}
	h.stop(tx, "")
	waitEnd(t, tx)
	if _, err = h.start(c, []int{1001}, "request", false, false, 0, 0); err == nil {
		t.Fatal("ended request replayed")
	}
	if err = h.stopRequest(c, "request"); err != nil {
		t.Fatal("idempotent stop failed", err)
	}
}

func TestLeaseAndCancellationReleaseMatchingGrant(t *testing.T) {
	r := &testRouter{}
	h := New(Options{Router: r, Lease: 100 * time.Millisecond, AudioIdle: time.Second})
	defer h.Close()
	c := readyClient(t, h, principal(10001, 1001))
	tx, err := h.start(c, []int{1001}, "lost-browser", false, false, 0, 0)
	if err != nil {
		t.Fatal(err)
	}
	waitGrant(t, h, tx)
	waitEnd(t, tx)
	_, _, ended := r.counts()
	if ended != 1 {
		t.Fatal("lease did not release exactly one grant")
	}
	h.mu.Lock()
	reason := tx.Error
	remaining := len(h.floors)
	h.mu.Unlock()
	if reason == "" || remaining != 0 {
		t.Fatal("expired floor retained")
	}
}

func TestNormalPTTReleaseDrainsAcceptedMicrophoneFrames(t *testing.T) {
	r := &testRouter{}
	h := New(Options{Router: r})
	defer h.Close()
	c := readyClient(t, h, principal(10001, 1001))
	tx, err := h.start(c, []int{1001}, "release-drain", false, false, 0, 0)
	if err != nil {
		t.Fatal(err)
	}
	waitGrant(t, h, tx)
	// Block the route so at least two accepted frames remain queued at release.
	func() {
		r.mu.Lock()
		defer r.mu.Unlock()
		for i := range 3 {
			pcm := make([]byte, FrameBytes)
			pcm[0] = byte(i + 1)
			if err := h.acceptPCM(c, pcm, time.Now()); err != nil {
				t.Fatal(err)
			}
		}
		for range 2 {
			if err := h.stopRequest(c, "release-drain"); err != nil {
				t.Fatal(err)
			}
		}
		if err := h.acceptPCM(c, make([]byte, FrameBytes), time.Now()); err == nil {
			t.Fatal("microphone input was accepted after release")
		}
	}()
	waitEnd(t, tx)
	r.mu.Lock()
	defer r.mu.Unlock()
	if len(r.writes) != 3 || len(r.ends) != 1 || r.ends[0].ID != tx.ID {
		t.Fatalf("normal release wrote %d of 3 admitted frames and ended %d grants", len(r.writes), len(r.ends))
	}
	for i, frame := range r.writes {
		if frame.Sequence != uint64(i+1) || frame.PCM[0] != byte(i+1) {
			t.Fatal("release changed admitted frame order or audio")
		}
	}
}

func TestCancelledAmbiguousBeginStillGetsIdentityBoundCleanup(t *testing.T) {
	r := &testRouter{beginWait: make(chan struct{})}
	h := New(Options{Router: r, GrantTimeout: 60 * time.Millisecond})
	defer h.Close()
	c := readyClient(t, h, principal(10001, 1001))
	tx, err := h.start(c, []int{1001}, "ambiguous", false, false, 0, 0)
	if err != nil {
		t.Fatal(err)
	}
	waitEnd(t, tx)
	r.mu.Lock()
	defer r.mu.Unlock()
	if len(r.ends) != 1 || r.ends[0].ID != tx.ID {
		t.Fatal("ambiguous grant was not released by its call ID")
	}
}

func TestStaleOrPregrantAudioNeverReachesRouter(t *testing.T) {
	allow := make(chan struct{})
	r := &testRouter{beginWait: allow}
	h := New(Options{Router: r})
	defer h.Close()
	c := readyClient(t, h, principal(10001, 1001))
	tx, err := h.start(c, []int{1001}, "fresh-only", false, false, 0, 0)
	if err != nil {
		t.Fatal(err)
	}
	if err = h.acceptPCM(c, make([]byte, FrameBytes), time.Now()); err == nil {
		t.Fatal("pregrant audio accepted")
	}
	close(allow)
	waitGrant(t, h, tx)
	if err = h.acceptPCM(c, make([]byte, FrameBytes), time.Now().Add(-time.Second)); err == nil {
		t.Fatal("stale audio accepted")
	}
	if _, written, _ := r.counts(); written != 0 {
		t.Fatal("stale audio transmitted")
	}
	h.stop(tx, "")
	waitEnd(t, tx)
}

func TestSlowLeaseRenewDoesNotPauseAudioPacing(t *testing.T) {
	r := &testRouter{renewDelay: 130 * time.Millisecond}
	h := New(Options{Router: r, AudioIdle: time.Second})
	defer h.Close()
	c := readyClient(t, h, principal(10001, 1001))
	tx, err := h.start(c, []int{1001}, "slow-control", false, false, 0, 0)
	if err != nil {
		t.Fatal(err)
	}
	waitGrant(t, h, tx)
	for n := 0; n < 38; n++ {
		if n%8 == 0 {
			if err = h.renew(c, "slow-control"); err != nil {
				t.Fatal(err)
			}
		}
		if err = h.acceptPCM(c, make([]byte, FrameBytes), time.Now()); err != nil {
			t.Fatal(err)
		}
		time.Sleep(FrameDuration)
	}
	waitFor(t, func() bool { _, n, _ := r.counts(); return n == 38 })
	h.stop(tx, "")
	waitEnd(t, tx)
	r.mu.Lock()
	defer r.mu.Unlock()
	for n := 1; n < len(r.writeTimes); n++ {
		if gap := r.writeTimes[n].Sub(r.writeTimes[n-1]); gap > 100*time.Millisecond {
			t.Fatalf("control renewal blocked audio for %s", gap)
		}
	}
}

func TestReceiveSubscriptionNeverCreatesCallOrRFInterest(t *testing.T) {
	r := &testRouter{}
	h := New(Options{Router: r})
	defer h.Close()
	c, err := h.openClient(principal(10001, 1001), "login")
	if err != nil {
		t.Fatal(err)
	}
	defer h.removeClient(c)
	if err = h.subscribe(c, []int{1002}); !errors.Is(err, ErrPermission) {
		t.Fatal("unpermitted subscription accepted")
	}
	if err = h.subscribe(c, []int{1001}); err != nil {
		t.Fatal(err)
	}
	h.Publish(Frame{CallID: "rf-call-segment", LogicalCallID: "rf-conversation", TalkgroupID: 1001, SourceID: 10002, SourceAlias: "Portable", SourceKind: "rf", PCM: make([]byte, FrameBytes), CapturedAt: time.Now()})
	h.Publish(Frame{CallID: "wrong", TalkgroupID: 1002, SourceID: 10003, PCM: make([]byte, FrameBytes), CapturedAt: time.Now()})
	active := false
	for len(c.send) > 0 {
		message := (<-c.send).(map[string]any)
		if message["type"] == "rx" {
			if message["talkgroup_id"] != 1001 {
				t.Fatal("unpermitted audio event")
			}
			if message["source_alias"] != "Portable" || message["source_id"] != 10002 || message["source_kind"] != "rf" || message["call_id"] != "rf-call-segment" || message["logical_call_id"] != "rf-conversation" {
				t.Fatal("authorized caller metadata was not preserved")
			}
			active = true
		}
	}
	if !active {
		t.Fatal("receive event missing")
	}
	begin, write, end := r.counts()
	if begin != 0 || write != 0 || end != 0 {
		t.Fatal("listening caused routing admission")
	}
	h.Publish(Frame{TalkgroupID: 1001, SourceID: 10002, Encrypted: true, PCM: make([]byte, FrameBytes)})
	if len(c.send) != 0 {
		t.Fatal("encrypted audio was exposed")
	}
}

func TestTicketBindsAccountSessionAndPermissions(t *testing.T) {
	h := New(Options{})
	defer h.Close()
	p := principal(10001, 1001)
	ticket, err := h.IssueTicket(p, "login")
	if err != nil {
		t.Fatal(err)
	}
	if err = h.consumeTicket(ticket, p, "wrong"); err == nil {
		t.Fatal("ticket replaced login")
	}
	if err = h.consumeTicket(ticket, p, "login"); err == nil {
		t.Fatal("ticket reused after failed consumption")
	}
	ticket, _ = h.IssueTicket(p, "login")
	altered := p
	altered.ReceiveGroups = []int{1001, 1002}
	if err = h.consumeTicket(ticket, altered, "login"); err == nil {
		t.Fatal("changed permission snapshot accepted")
	}
	ticket, _ = h.IssueTicket(p, "login")
	if err = h.consumeTicket(ticket, p, "login"); err != nil {
		t.Fatal(err)
	}
	if err = h.consumeTicket(ticket, p, "login"); err == nil {
		t.Fatal("ticket replayed")
	}
}

func TestClearWAVUsesSameFloorAndDrainsFinalFrame(t *testing.T) {
	r := &testRouter{}
	h := New(Options{Router: r})
	defer h.Close()
	pcm := make([]byte, FrameBytes*13)
	for n := range pcm {
		pcm[n] = byte(n)
	}
	frames, err := h.PlayPCM(context.Background(), principal(10001, 1001, 1002), "login", []int{1001, 1002}, pcm, false, 0, 0)
	if err != nil {
		t.Fatal(err)
	}
	if frames != 13 {
		t.Fatal("wrong audio length")
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	if len(r.begin) != 1 || !slices.Equal(r.begin[0].Talkgroups, []int{1001, 1002}) || len(r.writes) != 13 || len(r.ends) != 1 {
		t.Fatal("WAV bypassed the normal floor or dropped final audio")
	}
	for n, f := range r.writes {
		if !slices.Equal(f.PCM, pcm[n*FrameBytes:(n+1)*FrameBytes]) {
			t.Fatal("PCM changed")
		}
	}
}
