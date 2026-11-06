package rtpvoice

import (
	"context"
	"errors"
	"io"
	"testing"
	"time"
	"open25.local/open25-fne/internal/p25rtp"
)

func TestSeparatedSchedulerStallsDoNotPermanentlyDelayVoice(t *testing.T) {
	home, serving := engineSessionPair(t)
	spurt := engineFixture(t, p25rtp.GroupHome)
	receiver, receiverDone := engineStart(t, serving, spurt)
	sender, err := NewEngine(home, spurt)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = sender.Close() })
	ctx, cancel := context.WithTimeout(t.Context(), 3*time.Second)
	defer cancel()
	var initial []p25rtp.PacketType
	sender.state, initial = newEngineState(p25rtp.GroupHome, spurt.Origin, time.Now())
	if err = sender.sendControls(ctx, initial, time.Now()); err != nil {
		t.Fatal(err)
	}
	engineReady(t, receiver)
	const words = 50
	frames := make([]Frame, words)
	for i := range frames {
		frames[i] = engineFrame(t, uint64(i))
	}
	if err = sender.Push(frames[0]); err != nil {
		t.Fatal(err)
	}
	if _, err = sender.drive(ctx, time.Now()); err != nil {
		t.Fatal(err)
	}
	anchor := sender.txAnchor
	produced := make(chan error, 1)
	go func() {
		for i := 1; i < words; i++ {
			timer := time.NewTimer(time.Until(anchor.Add(time.Duration(i) * framePeriod)))
			select {
			case <-timer.C:
			case <-ctx.Done():
				timer.Stop()
				produced <- ctx.Err()
				return
			}
			if err := sender.Push(frames[i]); err != nil {
				produced <- err
				return
			}
		}
		produced <- nil
	}()
	type result struct {
		frames []Delivery
		err    error
	}
	delivered := make(chan result, 1)
	go func() {
		var out []Delivery
		for {
			d, err := receiver.Receive(ctx)
			if err != nil {
				if errors.Is(err, io.EOF) {
					err = nil
				}
				delivered <- result{out, err}
				return
			}
			out = append(out, d)
		}
	}()
	// Drive the real UDP sender ourselves so only its scheduling stops. The
	// independent 20ms producer and receiver continue through each stall.
	var stalls []time.Duration
	var lagAfterRecovery []time.Duration
	finishedInput, ending := false, false
	stalled := [2]bool{}
	recovered := [2]bool{}
	for {
		now := time.Now()
		for i, at := range []time.Duration{100 * time.Millisecond, 300 * time.Millisecond} {
			if !stalled[i] && !now.Before(anchor.Add(at)) {
				began := time.Now()
				time.Sleep(26 * time.Millisecond)
				stalls = append(stalls, time.Since(began))
				stalled[i] = true
				now = time.Now()
			}
		}
		select {
		case err := <-produced:
			if err != nil {
				t.Fatal(err)
			}
			finishedInput = true
		default:
		}
		sender.txMu.Lock()
		empty := len(sender.tx) == 0
		sender.txMu.Unlock()
		if finishedInput && empty && !ending {
			sender.beginEnding(ReasonLocalEnd, now, false)
			ending = true
		}
		before := sender.Stats().SentFrames
		complete, err := sender.drive(ctx, now)
		if err != nil {
			t.Fatal(err)
		}
		if sent := sender.Stats().SentFrames; sent != before {
			for i, after := range []uint64{10, 20} {
				if sent >= after && !recovered[i] {
					lagAfterRecovery = append(lagAfterRecovery, time.Since(anchor)-time.Duration(sent-1)*framePeriod)
					recovered[i] = true
				}
			}
		}
		if complete {
			break
		}
		select {
		case <-ctx.Done():
			t.Fatal(ctx.Err())
		case <-time.After(time.Millisecond):
		}
	}
	out := <-delivered
	t.Logf("injected stalls=%v, sender lag after recovery=%v, received=%+v", stalls, lagAfterRecovery, receiver.Stats())
	if len(stalls) != 2 || stalls[0] < framePeriod || stalls[1] < framePeriod {
		t.Fatalf("did not exercise two independent scheduler stalls: %v", stalls)
	}
	if out.err != nil {
		t.Fatal(out.err)
	}
	if err := <-receiverDone; err != nil {
		t.Fatalf("receiver failed while fresh in-order voice continued: %v", err)
	}
	if len(out.frames) != words+1 || receiver.Stats().ReceivedFrames != words || receiver.Stats().IPGaps != 0 {
		t.Fatalf("scheduler stalls permanently displaced voice: deliveries%d stats%+v", len(out.frames), receiver.Stats())
	}
	for i := range words {
		if out.frames[i].Kind != FrameDelivery || out.frames[i].Position != uint64(i) {
			t.Fatalf("word%d was lost or reordered: %#v", i, out.frames[i])
		}
	}
	if out.frames[words].Kind != EndDelivery || out.frames[words].Reason != ReasonRemoteEnd {
		t.Fatalf("unexpected final delivery: %#v", out.frames[words])
	}
	for _, lag := range lagAfterRecovery {
		if lag > 12*time.Millisecond {
			t.Fatalf("sender retained scheduling delay after catch-up: %v", lagAfterRecovery)
		}
	}
}

func TestSchedulerCatchUpCannotSendExpiredQueuedWord(t *testing.T) {
	home, _ := engineSessionPair(t)
	sender, err := NewEngine(home, engineFixture(t, p25rtp.GroupHome))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = sender.Close() })
	sender.state, _ = newEngineState(p25rtp.GroupHome, p25rtp.GroupHome, time.Now())
	if err := sender.Push(engineFrame(t, 0)); err != nil {
		t.Fatal(err)
	}
	sender.txMu.Lock()
	sender.tx[0].at = time.Now().Add(-maximumQueueAge - time.Millisecond)
	sender.txMu.Unlock()
	if _, err := sender.drive(t.Context(), time.Now()); err != nil {
		t.Fatal(err)
	}
	if sender.state.reason != ReasonQueue || sender.Stats().SentFrames != 0 || sender.Stats().DroppedFrames != 1 {
		t.Fatalf("catch-up sent stale media: reason%s stats%+v", sender.state.reason, sender.Stats())
	}
}
