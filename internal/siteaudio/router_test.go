package siteaudio

import (
	"context"
	"encoding/json"
	"errors"
	"slices"
	"sync"
	"testing"
	"time"

	"open25.local/open25-fne/internal/dispatch"
	"open25.local/open25-fne/internal/radio"
)

type fakeCodec struct {
	mu        sync.Mutex
	frames    []dispatch.Frame
	drained   bool
	drainWait <-chan struct{}
}

func (c *fakeCodec) Write(ctx context.Context, frame dispatch.Frame) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.drained {
		return errors.New("write after drain")
	}
	c.frames = append(c.frames, frame)
	return ctx.Err()
}
func (c *fakeCodec) Drain(ctx context.Context, id string, groups []int) error {
	if c.drainWait != nil {
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-c.drainWait:
		}
	}
	c.mu.Lock()
	c.drained = true
	c.mu.Unlock()
	return nil
}

type fakeNative struct {
	mu             sync.Mutex
	calls          []string
	grant          map[string]any
	ambiguous      bool
	badEpoch       bool
	statusFailure  bool
	releaseFailure bool
	released       bool
	holdGrant      <-chan struct{}
}

func rawReply(v map[string]any) map[string]json.RawMessage {
	out := map[string]json.RawMessage{}
	for k, v := range v {
		out[k], _ = json.Marshal(v)
	}
	return out
}
func (n *fakeNative) Get(context.Context, string) (map[string]json.RawMessage, error) {
	return nil, errors.New("unused")
}
func (n *fakeNative) Put(ctx context.Context, path string, value any) (map[string]json.RawMessage, error) {
	n.mu.Lock()
	n.calls = append(n.calls, path)
	encoded, _ := json.Marshal(value)
	body := map[string]any{}
	_ = json.Unmarshal(encoded, &body)
	hold := n.holdGrant
	n.mu.Unlock()
	if path == "/p25/dispatch/grant" && hold != nil {
		select {
		case <-ctx.Done():
			return nil, &radio.NativeError{Possible: false, Message: "cancelled before send"}
		case <-hold:
		}
	}
	n.mu.Lock()
	defer n.mu.Unlock()
	if path == "/p25/dispatch/grant" {
		n.grant = body
		if n.ambiguous {
			return nil, &radio.NativeError{Possible: true, Message: "reply lost"}
		}
	}
	if path == "/p25/dispatch/status" && n.statusFailure {
		return nil, &radio.NativeError{Possible: true, Message: "status unavailable"}
	}
	if path == "/p25/dispatch/release" && n.releaseFailure {
		return nil, &radio.NativeError{Possible: true, Message: "release unavailable"}
	}
	if path == "/p25/dispatch/status" && n.grant == nil {
		return rawReply(map[string]any{"status": 404}), &radio.NativeError{Status: 404, HTTPStatus: 404, Message: "unknown lease"}
	}
	state := "granted"
	if path == "/p25/dispatch/release" {
		n.released = true
	}
	if n.released {
		state = "released"
	}
	epoch := "17"
	if n.badEpoch {
		epoch = "0"
	}
	reply := map[string]any{"status": 200, "issued": true, "accepted": true, "state": state, "srcId": body["srcId"], "dstId": body["dstId"], "leaseId": body["leaseId"], "epoch": epoch, "channelId": 2, "channelNo": 81}
	return rawReply(reply), nil
}
func (n *fakeNative) paths() []string {
	n.mu.Lock()
	defer n.mu.Unlock()
	return append([]string{}, n.calls...)
}
func request(id string, groups ...int) dispatch.CallRequest {
	return dispatch.CallRequest{ID: id, SessionID: "console-session", UserID: "dispatcher", RadioID: 10002, Talkgroups: groups}
}
func frame(id string, seq uint64) dispatch.Frame {
	return dispatch.Frame{CallID: id, SourceID: 10002, Sequence: seq, PCM: make([]byte, dispatch.FrameBytes), CapturedAt: time.Now()}
}
func allowed(int, int) bool { return true }

func TestIPOnlyCallDoesNotTouchNativeOrCodec(t *testing.T) {
	n := &fakeNative{}
	c := &fakeCodec{}
	controls := 0
	r := New(Options{Control: func() (radio.Native, error) { controls++; return n, nil }, Codec: c, HasRFInterest: func(int) bool { return false }, AllowTX: allowed})
	g, err := r.Begin(context.Background(), request("ip-only", 1001, 1002))
	if err != nil {
		t.Fatal(err)
	}
	if err = r.Write(context.Background(), g, frame(g.ID, 1)); err != nil {
		t.Fatal(err)
	}
	if err = r.Renew(context.Background(), g); err != nil {
		t.Fatal(err)
	}
	if err = r.End(context.Background(), g); err != nil {
		t.Fatal(err)
	}
	if controls != 0 || len(n.paths()) != 0 || len(c.frames) != 0 || c.drained {
		t.Fatal("IP listening interest created an RF leg")
	}
}

func TestNativeIdentityAndSourceMetadataArePreserved(t *testing.T) {
	n := &fakeNative{}
	c := &fakeCodec{}
	r := New(Options{Control: func() (radio.Native, error) { return n, nil }, Codec: c, HasRFInterest: func(tg int) bool { return tg == 1001 }, AllowTX: allowed})
	g, err := r.Begin(context.Background(), request("rf-and-ip", 1001, 1002))
	if err != nil {
		t.Fatal(err)
	}
	if r.Status().RFGrants != 1 {
		t.Fatal("missing RF grant")
	}
	if err = r.Write(context.Background(), g, frame(g.ID, 1)); err != nil {
		t.Fatal(err)
	}
	if err = r.Renew(context.Background(), g); err != nil {
		t.Fatal(err)
	}
	if len(c.frames) != 1 || c.frames[0].TalkgroupID != 1001 || c.frames[0].SourceID != 10002 {
		t.Fatal("RF metadata changed or extra RF leg created")
	}
	if err = r.End(context.Background(), g); err != nil {
		t.Fatal(err)
	}
	if !c.drained || !n.released {
		t.Fatal("tail was not drained before grant release")
	}
	if !slices.Equal(n.paths(), []string{"/p25/dispatch/grant", "/p25/dispatch/renew", "/p25/dispatch/release"}) {
		t.Fatal(n.paths())
	}
	if err = r.End(context.Background(), g); err != nil {
		t.Fatal("cleanup is not idempotent", err)
	}
}

func TestMultiselectAllocatesOneNativeLeasePerInterestedGroup(t *testing.T) {
	n := &fakeNative{}
	r := New(Options{Control: func() (radio.Native, error) { return n, nil }, Codec: &fakeCodec{}, HasRFInterest: func(int) bool { return true }, AllowTX: allowed})
	g, err := r.Begin(context.Background(), request("multi-rf", 1001, 1002))
	if err != nil {
		t.Fatal(err)
	}
	if len(n.paths()) != 2 || r.Status().RFGrants != 2 {
		t.Fatal("each interested group needs its own voice allocation", n.paths(), r.Status())
	}
	if err = r.End(context.Background(), g); err != nil {
		t.Fatal(err)
	}
	if r.Status().ActiveCalls != 0 {
		t.Fatal("ended request retained resources")
	}

}

func TestAmbiguousGrantUsesStatusWithoutAnotherGrant(t *testing.T) {
	n := &fakeNative{ambiguous: true}
	r := New(Options{Control: func() (radio.Native, error) { return n, nil }, Codec: &fakeCodec{}, HasRFInterest: func(int) bool { return true }, AllowTX: allowed})
	g, err := r.Begin(context.Background(), request("recover-same-lease", 1001))
	if err != nil {
		t.Fatal(err)
	}
	if !slices.Equal(n.paths(), []string{"/p25/dispatch/grant", "/p25/dispatch/status"}) {
		t.Fatal("grant was retried or not resolved", n.paths())
	}
	if err = r.End(context.Background(), g); err != nil {
		t.Fatal(err)
	}
}

func TestUnconfirmedGrantFailsClosedButDoesNotBlockIPCalls(t *testing.T) {
	n := &fakeNative{ambiguous: true, statusFailure: true}
	r := New(Options{Control: func() (radio.Native, error) { return n, nil }, Codec: &fakeCodec{}, HasRFInterest: func(tg int) bool { return tg == 1001 }, AllowTX: allowed})
	if _, err := r.Begin(context.Background(), request("unknown", 1001)); err == nil {
		t.Fatal("unconfirmed grant admitted")
	}
	if r.Status().Fault == "" || r.Status().ActiveCalls != 0 {
		t.Fatal("unknown grant did not clean up/fail closed")
	}
	before := len(n.paths())
	if _, err := r.Begin(context.Background(), request("retry-rf", 1001)); err == nil {
		t.Fatal("new RF grant admitted after unknown outcome")
	}
	if len(n.paths()) != before {
		t.Fatal("RF request retried after ambiguous grant")
	}
	g, err := r.Begin(context.Background(), request("ip-still-works", 1002))
	if err != nil {
		t.Fatal("IP-only service was disabled", err)
	}
	if err = r.End(context.Background(), g); err != nil {
		t.Fatal(err)
	}
}

func TestInvalidEpochOrSourceNeverReachesCodec(t *testing.T) {
	n := &fakeNative{badEpoch: true}
	c := &fakeCodec{}
	r := New(Options{Control: func() (radio.Native, error) { return n, nil }, Codec: c, HasRFInterest: func(int) bool { return true }, AllowTX: allowed})
	if _, err := r.Begin(context.Background(), request("bad-epoch", 1001)); err == nil {
		t.Fatal("zero epoch accepted")
	}
	if len(c.frames) > 0 {
		t.Fatal("audio entered an unconfirmed grant")
	}
	n = &fakeNative{}
	c = &fakeCodec{}
	r = New(Options{Control: func() (radio.Native, error) { return n, nil }, Codec: c, HasRFInterest: func(int) bool { return true }, AllowTX: allowed})
	g, err := r.Begin(context.Background(), request("source-check", 1001))
	if err != nil {
		t.Fatal(err)
	}
	f := frame(g.ID, 1)
	f.SourceID = 10003
	if err = r.Write(context.Background(), g, f); err == nil {
		t.Fatal("spoofed source accepted")
	}
	f = frame(g.ID, 1)
	f.Encrypted = true
	if err = r.Write(context.Background(), g, f); err == nil {
		t.Fatal("encrypted data transmitted clear")
	}
	if len(c.frames) > 0 {
		t.Fatal("unpermitted audio reached codec")
	}
	_ = r.End(context.Background(), g)
}

func TestEndPreventsLateMediaAndCloseRejectsNewCalls(t *testing.T) {
	n := &fakeNative{}
	drain := make(chan struct{})
	c := &fakeCodec{drainWait: drain}
	r := New(Options{Control: func() (radio.Native, error) { return n, nil }, Codec: c, HasRFInterest: func(int) bool { return true }, AllowTX: allowed})
	g, err := r.Begin(context.Background(), request("closing", 1001))
	if err != nil {
		t.Fatal(err)
	}
	endDone := make(chan error, 1)
	go func() { endDone <- r.End(context.Background(), g) }()
	time.Sleep(20 * time.Millisecond)
	writeDone := make(chan error, 1)
	go func() { writeDone <- r.Write(context.Background(), g, frame(g.ID, 1)) }()
	select {
	case err := <-writeDone:
		if err == nil {
			t.Fatal("write raced the release path")
		}
	case <-time.After(20 * time.Millisecond):
		t.Fatal("closing media write should fail promptly")
	}
	close(drain)
	if err = <-endDone; err != nil {
		t.Fatal(err)
	}
	if len(c.frames) > 0 {
		t.Fatal("late audio entered codec")
	}
	if err = r.Close(context.Background()); err != nil {
		t.Fatal(err)
	}
	if _, err = r.Begin(context.Background(), request("after-close", 1002)); err == nil {
		t.Fatal("closed router admitted a call")
	}
}

func TestLastRFAffiliationLeavingDropsOnlyRFLeg(t *testing.T) {
	n := &fakeNative{}
	c := &fakeCodec{}
	var interest sync.Mutex
	needed := true
	r := New(Options{Control: func() (radio.Native, error) { return n, nil }, Codec: c, HasRFInterest: func(int) bool { interest.Lock(); defer interest.Unlock(); return needed }, AllowTX: allowed})
	g, err := r.Begin(context.Background(), request("rf-leaves", 1001))
	if err != nil {
		t.Fatal(err)
	}
	if err = r.Write(context.Background(), g, frame(g.ID, 1)); err != nil {
		t.Fatal(err)
	}
	interest.Lock()
	needed = false
	interest.Unlock()
	if err = r.Write(context.Background(), g, frame(g.ID, 2)); err != nil {
		t.Fatal("IP call should continue", err)
	}
	if len(c.frames) != 1 {
		t.Fatal("RF audio continued without eligible affiliation")
	}
	if err = r.Renew(context.Background(), g); err != nil {
		t.Fatal("loss of RF interest ended IP call", err)
	}
	if !c.drained || !n.released || r.Status().RFGrants != 0 || r.Status().ActiveCalls != 1 {
		t.Fatal("RF leg not released independently")
	}
	if err = r.Write(context.Background(), g, frame(g.ID, 3)); err != nil {
		t.Fatal(err)
	}
	if err = r.Renew(context.Background(), g); err != nil {
		t.Fatal(err)
	}
	if len(n.paths()) != 2 {
		t.Fatal("released RF lease kept renewing", n.paths())
	}
	if err = r.End(context.Background(), g); err != nil {
		t.Fatal(err)
	}
}

func TestRFMediaEmergencyIsOwnedByAcceptedCall(t *testing.T) {
	for _, emergency := range []bool{false, true} {
		t.Run(map[bool]string{false: "ordinary rejects spoof", true: "emergency survives default media"}[emergency], func(t *testing.T) {
			n, c := &fakeNative{}, &fakeCodec{}
			r := New(Options{Control: func() (radio.Native, error) { return n, nil }, Codec: c, HasRFInterest: func(int) bool { return true }, AllowTX: allowed})
			q := request("owned-emergency", 1001)
			q.Emergency = emergency
			g, err := r.Begin(context.Background(), q)
			if err != nil {
				t.Fatal(err)
			}
			f := frame(g.ID, 1)
			f.Emergency = !emergency
			if err = r.Write(context.Background(), g, f); err != nil {
				t.Fatal(err)
			}
			if len(c.frames) != 1 || c.frames[0].Emergency != emergency {
				t.Fatal("media changed accepted call emergency", c.frames)
			}
			if n.grant["emergency"] != emergency {
				t.Fatal("grant and voice owner disagree", n.grant)
			}
			if err = r.End(context.Background(), g); err != nil {
				t.Fatal(err)
			}
		})
	}
}
