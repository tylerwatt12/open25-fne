package siteaudio

import (
	"context"
	"encoding/json"
	"errors"
	"strconv"
	"sync"
	"testing"
	"time"

	"open25.local/open25-fne/internal/dispatch"
	"open25.local/open25-fne/internal/p25rtp"
	"open25.local/open25-fne/internal/radio"
)

type capacityNative struct {
	mu       sync.Mutex
	limit    int
	next     int
	leases   map[string]nativeReply
	metadata map[string]map[string]any
}

func (n *capacityNative) Get(context.Context, string) (map[string]json.RawMessage, error) {
	return nil, errors.New("unused")
}
func (n *capacityNative) Put(_ context.Context, path string, value any) (map[string]json.RawMessage, error) {
	n.mu.Lock()
	defer n.mu.Unlock()
	if n.leases == nil {
		n.leases = map[string]nativeReply{}
		n.metadata = map[string]map[string]any{}
	}
	body := value.(map[string]any)
	id := body["leaseId"].(string)
	l, known := n.leases[id]
	if path == "/p25/dispatch/grant" && !known {
		active := 0
		for _, old := range n.leases {
			if old.State == "granted" {
				active++
			}
		}
		l = nativeReply{Source: body["srcId"].(int), Group: body["dstId"].(int), Lease: id, State: "rejected", Status: 409}
		if active < n.limit {
			n.next++
			l.Epoch = strconv.Itoa(n.next)
			l.State = "granted"
			l.Status = 200
			l.Issued = true
			l.Accepted = true
			l.ChannelID = 2
			l.ChannelNo = 80 + n.next
		}
		n.leases[id] = l
		n.metadata[id] = body
		known = true
	}
	if !known {
		return rawReply(map[string]any{"status": 404}), &radio.NativeError{Status: 404, HTTPStatus: 404}
	}
	if path == "/p25/dispatch/release" && l.State == "granted" {
		l.State = "released"
		n.leases[id] = l
	}
	data, _ := json.Marshal(l)
	out := map[string]json.RawMessage{}
	_ = json.Unmarshal(data, &out)
	if l.Status >= 400 {
		return out, &radio.NativeError{Status: l.Status, HTTPStatus: 503, Message: "no voice resource is available"}
	}
	return out, nil
}
func (n *capacityNative) active() int {
	n.mu.Lock()
	defer n.mu.Unlock()
	v := 0
	for _, l := range n.leases {
		if l.State == "granted" {
			v++
		}
	}
	return v
}

type independentCodec struct {
	mu     sync.Mutex
	frames []dispatch.Frame
	ended  map[string]bool
}

func (c *independentCodec) Write(_ context.Context, f dispatch.Frame) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.ended[f.CallID] {
		return errors.New("ended")
	}
	c.frames = append(c.frames, f)
	return nil
}
func (c *independentCodec) Drain(_ context.Context, id string, _ []int) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.ended == nil {
		c.ended = map[string]bool{}
	}
	c.ended[id] = true
	return nil
}
func capacityRouter(n *capacityNative, c Codec) *Router {
	return New(Options{Control: func() (radio.Native, error) { return n, nil }, Codec: c, HasRFInterest: func(int) bool { return true }, AllowTX: allowed})
}

func TestMultipleCallsUseIndependentNativeAllocations(t *testing.T) {
	n := &capacityNative{limit: 2}
	codec := &independentCodec{}
	r := capacityRouter(n, codec)
	a, err := r.Begin(context.Background(), request("first", 1001))
	if err != nil {
		t.Fatal(err)
	}
	q := request("second", 1002)
	q.RadioID = 10003
	b, err := r.Begin(context.Background(), q)
	if err != nil {
		t.Fatal(err)
	}
	if n.active() != 2 || r.Status().RFGrants != 2 {
		t.Fatal("independent channels not allocated")
	}
	if err = r.End(context.Background(), a); err != nil {
		t.Fatal(err)
	}
	f := frame(b.ID, 1)
	f.SourceID = q.RadioID
	if err = r.Write(context.Background(), b, f); err != nil {
		t.Fatal("ending another channel stopped this call", err)
	}
	if err = r.Renew(context.Background(), b); err != nil {
		t.Fatal(err)
	}
	if n.active() != 1 {
		t.Fatal("one call cleanup retired another allocation")
	}
	if err = r.End(context.Background(), b); err != nil {
		t.Fatal(err)
	}
}
func TestPartialMultiselectReleasesEarlierLeasesOnCapacityFailure(t *testing.T) {
	n := &capacityNative{limit: 1}
	r := capacityRouter(n, &independentCodec{})
	if _, err := r.Begin(context.Background(), request("patch", 1001, 1002)); !errors.Is(err, dispatch.ErrBusy) {
		t.Fatalf("capacity failure must be recognizable: %v", err)
	}
	if n.active() != 0 || r.Status().ActiveCalls != 0 || r.Status().Fault != "" {
		t.Fatalf("partial call stranded allocation or latched a fault: %+v", r.Status())
	}
	if _, err := r.Begin(context.Background(), request("next", 1001)); err != nil {
		t.Fatal("capacity was not returned", err)
	}
}
func TestEmergencyDrainsItsOrdinaryFloorButNeverAnotherEmergency(t *testing.T) {
	n := &capacityNative{limit: 1}
	codec := &independentCodec{}
	r := capacityRouter(n, codec)
	ordinary, err := r.Begin(context.Background(), request("ordinary", 1001))
	if err != nil {
		t.Fatal(err)
	}
	q := request("urgent", 1001)
	q.RadioID = 10003
	q.Emergency = true
	emergency, err := r.Begin(context.Background(), q)
	if err != nil {
		t.Fatal(err)
	}
	if err = r.Write(context.Background(), ordinary, frame(ordinary.ID, 1)); err == nil {
		t.Fatal("preempted source retained the floor")
	}
	if n.active() != 1 {
		t.Fatal("ordinary allocation not released before emergency")
	}
	other := q
	other.ID = "other-emergency"
	other.RadioID = 10004
	if _, err = r.Begin(context.Background(), other); !errors.Is(err, dispatch.ErrBusy) {
		t.Fatal("interrupted another emergency", err)
	}
	if err = r.End(context.Background(), emergency); err != nil {
		t.Fatal(err)
	}
}
func TestSecureEncodedMediaAndNativeGrantFlagsArePreserved(t *testing.T) {
	n := &capacityNative{limit: 1}
	codec := &independentCodec{}
	r := capacityRouter(n, codec)
	q := request("secure", 1001)
	q.Secure = true
	q.KeyID = 7
	q.AlgorithmID = 132
	q.Emergency = true
	g, err := r.Begin(context.Background(), q)
	if err != nil {
		t.Fatal(err)
	}
	f := dispatch.Frame{ManufacturerID: 1, CallID: g.ID, SourceID: q.RadioID, TalkgroupID: 1001, Sequence: 1, Encoded: make([]byte, 11), AlgorithmID: 0x84, KeyID: 7, MessageIndicator: []byte{0x12, 0x34, 0x56, 0x78, 0x90, 0xAB, 0xCD, 0xEF, 0x00}, Encrypted: true, CapturedAt: time.Now()}
	f.ServiceOptions = 0x44
	f.P25 = &dispatch.P25VoiceMetadata{Type: p25rtp.FR1}
	if err = r.Write(context.Background(), g, f); err != nil {
		t.Fatal(err)
	}
	n.mu.Lock()
	for _, m := range n.metadata {
		if m["secure"] != true || m["emergency"] != true || m["priority"] != 1 {
			t.Fatal("native grant flags missing", m)
		}
	}
	n.mu.Unlock()
	f.Sequence = 2
	f.AlgorithmID = 129
	if err = r.Write(context.Background(), g, f); !errors.Is(err, dispatch.ErrPermission) {
		t.Fatal("different algorithm with same KID admitted", err)
	}
	f.KeyID = 8
	f.AlgorithmID = 132
	if err = r.Write(context.Background(), g, f); !errors.Is(err, dispatch.ErrPermission) {
		t.Fatal("different key admitted", err)
	}
	clear := frame(g.ID, 2)
	if err = r.Write(context.Background(), g, clear); !errors.Is(err, dispatch.ErrPermission) {
		t.Fatal("secure call transmitted clear", err)
	}
	codec.mu.Lock()
	if len(codec.frames) != 1 || codec.frames[0].KeyID != 7 || codec.frames[0].ManufacturerID != 1 {
		t.Fatal("opaque media changed")
	}
	codec.mu.Unlock()
	if err = r.End(context.Background(), g); err != nil {
		t.Fatal(err)
	}
}
func TestOneRadioCannotOwnDifferentSiteCallsAtOnce(t *testing.T) {
	r := capacityRouter(&capacityNative{limit: 2}, &independentCodec{})
	if _, err := r.Begin(context.Background(), request("one", 1001)); err != nil {
		t.Fatal(err)
	}
	if _, err := r.Begin(context.Background(), request("two", 1002)); !errors.Is(err, dispatch.ErrBusy) {
		t.Fatal("radio identity has two simultaneous owners", err)
	}
}
