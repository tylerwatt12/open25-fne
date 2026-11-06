package rtpvoice

import (
	"context"
	"errors"
	"io"
	"testing"
	"time"
	"open25.local/open25-fne/internal/p25rtp"
)

func TestPacedEighteenWordsDoNotAcquireFortyMillisecondCadence(t *testing.T) {
	testPacedVoiceWords(t, 18)
}

func TestSustainedPacedVoiceClockDoesNotAccumulateSchedulerDelay(t *testing.T) {
	testPacedVoiceWords(t, 180)
}

func testPacedVoiceWords(t *testing.T, words int) {
	t.Helper()
	home, serving := engineSessionPair(t)
	spurt := engineFixture(t, p25rtp.GroupHome)
	receiver, receiverDone := engineStart(t, serving, spurt)
	sender, senderDone := engineStart(t, home, spurt)
	engineReady(t, receiver)
	engineReady(t, sender)
	type outcome struct {
		frames []Delivery
		err    error
	}
	deliveries := make(chan outcome, 1)
	go func() {
		ctx, cancel := context.WithTimeout(t.Context(), time.Duration(words)*20*time.Millisecond+2*time.Second)
		defer cancel()
		var frames []Delivery
		for {
			d, err := receiver.Receive(ctx)
			if err != nil {
				if errors.Is(err, io.EOF) {
					err = nil
				}
				deliveries <- outcome{frames, err}
				return
			}
			frames = append(frames, d)
		}
	}()
	// Input need not share the actor ticker's phase. Scheduling jitter must not
	// turn the20ms media clock into40ms steps.
	time.Sleep(5 * time.Millisecond)
	tick := time.NewTicker(20 * time.Millisecond)
	defer tick.Stop()
	for position := uint64(0); position < uint64(words); position++ {
		if position != 0 {
			<-tick.C
		}
		if err := sender.Push(engineFrame(t, position)); err != nil {
			t.Fatal(err)
		}
	}
	ctx, cancel := context.WithTimeout(t.Context(), time.Second)
	defer cancel()
	if err := sender.End(ctx); err != nil {
		t.Fatal(err)
	}
	received := <-deliveries
	if received.err != nil {
		t.Fatal(received.err)
	}
	result := received.frames
	if len(result) != words+1 || receiver.Stats().ReceivedFrames != uint64(words) || receiver.Stats().IPGaps != 0 {
		t.Fatalf("paced words lost through scheduler drift: delivered%d stats%+v", len(result), receiver.Stats())
	}
	for i := range words {
		if result[i].Kind != FrameDelivery || result[i].Position != uint64(i) {
			t.Fatal("actual word replaced by IP gap", i, result[i])
		}
	}
	if err := <-senderDone; err != nil {
		t.Fatal(err)
	}
	if err := <-receiverDone; err != nil {
		t.Fatal(err)
	}
}
