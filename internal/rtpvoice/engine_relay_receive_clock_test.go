package rtpvoice

import (
	"reflect"
	"testing"
	"time"
	"open25.local/open25-fne/internal/p25rtp"
)

func TestRelayReceiverStallsDoNotMakeFinalWordLateDownstream(t *testing.T) {
	ctx := t.Context()
	at := time.Unix(100, 0)
	spurt := engineFixture(t, p25rtp.GroupHome)
	newReceiver := func() *Engine {
		// No worker, socket or wall-clock sleep: exercise actual actor methods
		// at explicitly supplied times.
		e, err := NewEngine(&Session{options: Options{LocalRole: p25rtp.GroupServing, RemoteRole: p25rtp.GroupHome}}, spurt)
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { _ = e.Close() })
		e.state, _ = newEngineState(p25rtp.GroupServing, spurt.Origin, at)
		e.state.control(p25rtp.Start, at)
		return e
	}
	relay, downstream := newReceiver(), newReceiver()
	const words = 9
	const downstreamBudget = 40 * time.Millisecond
	relay.voiceKnown, relay.rxPlay, relay.rxTimestamp = true, at, 1000
	for i := range words {
		// Fresh accepted words remain within the unchanged240ms queue-age
		// bound throughout both stalls. Input loss cannot explain a gap.
		relay.rxTokens <- struct{}{}
		relay.voices = append(relay.voices, queuedDelivery{Delivery: Delivery{Kind: FrameDelivery, Position: uint64(i), Timestamp: 1000 + uint32(i)*p25rtp.FrameTicks, Frame: engineFrame(t, uint64(i)), token: true}, at: at})
	}
	accept := func(packet p25rtp.Packet, now time.Time) {
		downstream.rxTokens <- struct{}{}
		held, err := downstream.packet(ctx, packetArrival{packet: packet, at: now})
		if !held {
			downstream.releaseRX()
		}
		if err != nil {
			t.Fatal(err)
		}
	}
	var received []Delivery
	collect := func() {
		for len(downstream.output) != 0 {
			delivery, err := downstream.Receive(ctx)
			if err != nil {
				t.Fatal(err)
			}
			received = append(received, delivery)
		}
	}
	now := at
	var sentAt []time.Duration
	for i := range words {
		if i == 3 || i == 8 {
			now = relay.rxPlay.Add(26 * time.Millisecond)
		} else if now.Before(relay.rxPlay) {
			now = relay.rxPlay
		}
		if complete, err := relay.drive(ctx, now); err != nil || complete {
			t.Fatalf("relay word%d: complete=%v error=%v", i, complete, err)
		}
		if len(relay.output) != 1 {
			t.Fatalf("fresh queued word%d did not reach the relay output", i)
		}
		delivery, err := relay.Receive(ctx)
		if err != nil || delivery.Kind != FrameDelivery || delivery.Position != uint64(i) || !reflect.DeepEqual(delivery.Frame, engineFrame(t, uint64(i))) {
			t.Fatalf("relay altered actual word%d: %#v, %v", i, delivery, err)
		}
		sentAt = append(sentAt, now.Sub(at))
		// Advance the independent downstream clock before this word arrives.
		// A coincident packet is admitted before its playout deadline.
		for downstream.voiceKnown && downstream.rxPlay.Before(now) {
			if complete, err := downstream.drive(ctx, downstream.rxPlay); err != nil || complete {
				t.Fatalf("downstream playout: complete=%v error=%v", complete, err)
			}
			collect()
		}
		accept(engineVoicePacket(spurt, delivery.Frame, uint16(i+1), delivery.Timestamp), now)
	}
	accept(enginePacket(spurt, p25rtp.End, words+1), now)
	downstream.beginEnding(ReasonRemoteEnd, now, false)
	for attempts := 0; ; attempts++ {
		if attempts > words+1 {
			t.Fatal("ordered downstream End did not complete")
		}
		if now.Before(downstream.rxPlay) {
			now = downstream.rxPlay
		}
		complete, err := downstream.drive(ctx, now)
		if err != nil {
			t.Fatal(err)
		}
		collect()
		if complete {
			break
		}
	}
	stats := downstream.Stats()
	if stats.ReceivedFrames != words || stats.IPGaps != 0 || stats.LatePackets != 0 || len(received) != words+1 {
		t.Fatalf("receiver stalls displaced the final word beyond the existing40ms downstream budget: relay times=%v downstream=%+v deliveries=%d", sentAt, stats, len(received))
	}
	for i := range words {
		if received[i].Kind != FrameDelivery || received[i].Position != uint64(i) {
			t.Fatal("actual word replaced by a gap", i, received[i])
		}
		if sentAt[i] > time.Duration(i)*framePeriod+downstreamBudget {
			t.Fatalf("word%d exceeded the unchanged downstream playout budget: %s", i, sentAt[i])
		}
	}
	if received[words].Kind != EndDelivery || received[words].Reason != ReasonRemoteEnd {
		t.Fatal("End overtook the final actual word", received[words])
	}
	if len(relay.rxTokens) != 0 || len(downstream.rxTokens) != 0 {
		t.Fatal("ordered completion retained receive capacity")
	}
}

func TestReceiverMediaDeadlineWaitsForConsumerWithoutMovingClock(t *testing.T) {
	at := time.Unix(100, 0)
	e := &Engine{state: engineState{phase: phaseActive}, voiceKnown: true, rxPlay: at, output: make(chan Delivery, 1), wake: make(chan struct{}, 1), voices: []queuedDelivery{{}}}
	e.output <- Delivery{Kind: GapDelivery}
	if _, ok := e.mediaDeadline(); ok {
		t.Fatal("full receiver output armed an already-due timer")
	}
	if _, err := e.Receive(t.Context()); err != nil {
		t.Fatal(err)
	}
	if len(e.wake) != 1 {
		t.Fatal("consuming output did not wake the actor")
	}
	if due, ok := e.mediaDeadline(); !ok || !due.Equal(at) {
		t.Fatal("consumer wake did not restore the unchanged playout deadline", due, ok)
	}
	e.voiceKnown = false
	if _, ok := e.mediaDeadline(); ok {
		t.Fatal("receiver armed playout before its first actual voice word")
	}
	e.voiceKnown = true
	e.state.phase = phaseEnding
	if due, ok := e.mediaDeadline(); !ok || !due.Equal(at) {
		t.Fatal("queued final voice lost its playout deadline", due, ok)
	}
	e.voices = nil
	if _, ok := e.mediaDeadline(); ok {
		t.Fatal("ended receiver with no remaining voice armed a playout timer")
	}
}
