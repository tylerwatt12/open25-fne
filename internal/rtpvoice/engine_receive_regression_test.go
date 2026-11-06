package rtpvoice

import (
	"context"
	"testing"
	"time"
	"open25.local/open25-fne/internal/p25rtp"
)

func TestEngineRepeatedStartCannotExtendActualVoiceDeadline(t *testing.T) {
	for _, voiceSeen := range []bool{false, true} {
		at := time.Unix(100, 0)
		state, _ := newEngineState(p25rtp.GroupServing, p25rtp.GroupHome, at)
		state.control(p25rtp.Start, at)
		reason := ReasonFirstMedia
		if voiceSeen {
			state.progress(at.Add(20 * time.Millisecond))
			reason = ReasonMediaLoss
		}
		until := state.mediaUntil
		state.control(p25rtp.Start, until.Add(-time.Millisecond))
		if !state.mediaUntil.Equal(until) {
			t.Fatalf("repeated Start extended the actual-voice deadline (voice seen %v)", voiceSeen)
		}
		state.tick(until)
		if state.phase != phaseEnding || state.reason != reason {
			t.Fatalf("repeated Start prevented expiry (voice seen %v): %s", voiceSeen, state.reason)
		}
	}
}

func TestEngineFreshPacketForPlayedGapCannotBlockLaterVoice(t *testing.T) {
	source, receiving := engineSessionPair(t)
	spurt := engineFixture(t, p25rtp.GroupHome)
	e, done := engineStart(t, receiving, spurt)
	engineRaw(t, source, enginePacket(spurt, p25rtp.Start, 100))
	engineReady(t, e)
	engineRaw(t, source, engineVoicePacket(spurt, engineFrame(t, 18), 101, 1000))
	ctx, cancel := context.WithTimeout(t.Context(), time.Second)
	defer cancel()
	for _, want := range []DeliveryKind{FrameDelivery, GapDelivery} {
		delivery, err := e.Receive(ctx)
		if err != nil || delivery.Kind != want {
			t.Fatalf("initial actual word and missing-word gap: %#v, %v", delivery, err)
		}
	}
	// The RTP sequence is fresh, but this word has already been represented by
	// Gap19 at the playout cursor. It must not obstruct the next timely word.
	engineRaw(t, source, engineVoicePacket(spurt, engineFrame(t, 19), 102, 1160))
	actual := engineFrame(t, 23)
	engineRaw(t, source, engineVoicePacket(spurt, actual, 103, 1800))
	for {
		delivery, err := e.Receive(ctx)
		if err != nil {
			t.Fatal(err)
		}
		if delivery.Kind == EndDelivery || delivery.Position > 23 {
			t.Fatalf("late word blocked timely word23: %#v", delivery)
		}
		if delivery.Position == 23 {
			if delivery.Kind != FrameDelivery || delivery.Frame.Voice.Quality != actual.Voice.Quality {
				t.Fatalf("timely word23 lost or its RF quality changed: %#v", delivery)
			}
			break
		}
	}
	engineRaw(t, source, enginePacket(spurt, p25rtp.End, 104))
	engineDrain(t, e)
	if err := <-done; err != nil {
		t.Fatal(err)
	}
	if e.Stats().LatePackets != 1 || e.Stats().ReceivedFrames != 2 {
		t.Fatal("late playout word was counted as usable audio", e.Stats())
	}
}
