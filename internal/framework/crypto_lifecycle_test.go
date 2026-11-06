package framework

import (
	"context"
	"errors"
	"fmt"
	"open25.local/open25-fne/internal/dispatch"
	"os"
	"testing"
	"time"
)

func TestReceiveCodecOwnershipSurvivesReplacementUntilEndAndIsBounded(t *testing.T) {
	runtime := &keyRuntime{}
	first, second := &dispatch.PacketCodec{}, &dispatch.PacketCodec{}
	now := time.Now()
	frame := dispatch.Frame{CallID: "one-use-call", SourceID: 10001, TalkgroupID: 1001}
	if owner, err := runtime.receiveOwner(frame, first, now); err != nil || owner != first {
		t.Fatal("first process did not own call", err)
	}
	if _, err := runtime.receiveOwner(frame, second, now.Add(time.Second)); err == nil {
		t.Fatal("existing receive stream moved to a replacement process")
	}
	if _, err := runtime.receiveOwner(frame, first, now.Add(2*time.Second)); err == nil {
		t.Fatal("blocked call resumed after original pointer returned")
	}
	fresh := frame
	fresh.CallID = "another-one-use-call"
	if owner, err := runtime.receiveOwner(fresh, second, now.Add(2*time.Second)); err != nil || owner != second {
		t.Fatal("distinct call was blocked by old ownership", err)
	}
	if owner := runtime.endReceive(frame); owner != first {
		t.Fatal("End lost old process owner")
	}
	if owner, err := runtime.receiveOwner(frame, second, now.Add(3*time.Second)); err != nil || owner != second {
		t.Fatal("End did not release binding", err)
	}
	// The identity includes source and talkgroup, not only a logical call token.
	different := frame
	different.SourceID++
	if _, err := runtime.receiveOwner(different, first, now.Add(3*time.Second)); err != nil {
		t.Fatal("different source inherited a binding", err)
	}
	different = frame
	different.TalkgroupID++
	if _, err := runtime.receiveOwner(different, first, now.Add(3*time.Second)); err != nil {
		t.Fatal("different group inherited a binding", err)
	}
	for i := len(runtime.receive); i < maxReceiveCodecBindings; i++ {
		v := frame
		v.CallID = fmt.Sprintf("bounded-%d", i)
		if _, err := runtime.receiveOwner(v, first, now.Add(3*time.Second)); err != nil {
			t.Fatal(err)
		}
	}
	overflow := frame
	overflow.CallID = "capacity-refused"
	if _, err := runtime.receiveOwner(overflow, first, now.Add(4*time.Second)); err == nil {
		t.Fatal("receive ownership exceeded fixed capacity")
	}
	if !runtime.receiveOwned(frame, second) {
		t.Fatal("capacity pressure evicted active ownership")
	}
	if _, err := runtime.receiveOwner(overflow, second, now.Add(receiveCodecIdle+5*time.Second)); err != nil {
		t.Fatal("expired bindings did not release bounded capacity", err)
	}
}

func TestCodecReplacementRetiresTransmitGrantOnWriteAndRenew(t *testing.T) {
	binary := os.Getenv("TRUNKSYSTEM_PACKET_CODEC")
	if binary == "" {
		t.Skip("set reviewed native packet codec executable")
	}
	for _, operation := range []string{"write", "renew"} {
		t.Run(operation, func(t *testing.T) {
			c := newMediaCluster(t)
			c.affiliations(t, true)
			codec, err := dispatch.NewPacketCodec(dispatch.PacketCodecOptions{Binary: binary, KeyFile: "-"})
			if err != nil {
				t.Fatal(err)
			}
			if err = codec.Start(context.Background()); err != nil {
				t.Fatal(err)
			}
			t.Cleanup(func() { _ = codec.Close() })
			c.dispatch.service.crypto.mu.Lock()
			c.dispatch.service.crypto.codec = codec
			c.dispatch.service.crypto.mu.Unlock()
			router := &networkRouter{service: c.dispatch.service}
			user := c.users[0]
			request := dispatch.CallRequest{ID: "process-owner-" + operation, SessionID: "codec-owner-session", UserID: user.ID, RadioID: 10002, Talkgroups: []int{1001}}
			ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
			defer cancel()
			principal := dispatch.Principal{UserID: user.ID, RadioID: 10002, CanTransmit: true, TransmitGroups: []int{1001}, ReceiveGroups: []int{1001}}
			if err := router.ClaimAudio(ctx, principal, request.SessionID); err != nil {
				t.Fatal(err)
			}
			defer router.ReleaseAudio(context.Background(), principal, request.SessionID)
			grant, err := router.Begin(ctx, request)
			if err != nil {
				t.Fatal(err)
			}
			if router.route(grant).Codec != codec {
				t.Fatal("call lost its admitted native process")
			}
			// The admitted SIP allocation must not adopt a replacement native process.
			replacement := &dispatch.PacketCodec{}
			c.dispatch.service.crypto.mu.Lock()
			c.dispatch.service.crypto.codec = replacement
			c.dispatch.service.crypto.mu.Unlock()
			if operation == "write" {
				err = router.Write(ctx, grant, dispatch.Frame{CallID: grant.ID, SourceID: 10002, TalkgroupID: 1001, Sequence: 1, PCM: make([]byte, dispatch.FrameBytes), CapturedAt: time.Now()})
			} else {
				err = router.Renew(ctx, grant)
			}
			if !errors.Is(err, dispatch.ErrUnavailable) {
				t.Fatal("changed process retained call", err)
			}
			if router.route(grant).Address != "" {
				t.Fatal("changed process retained its local grant")
			}
			mediaWait(t, time.Second, func() bool { return len(c.zone.service.broker.router.Active()) == 0 }, "changed codec remote SIP grant released")
		})
	}
}
