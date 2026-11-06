package rtpvoice

import (
	"context"
	"testing"
	"time"
	"open25.local/open25-fne/internal/p25rtp"
)

func TestSuccessiveSpurtsKeepAdmittedRTPCallAndDropOldEnd(t *testing.T) {
	home, serving := engineSessionPair(t)
	for burst := range 3 {
		spurt := engineFixture(t, p25rtp.GroupHome)
		spurt.TSN = uint8(1 + 2*burst)
		receiver, receiverDone := engineStart(t, serving, spurt)
		sender, senderDone := engineStart(t, home, spurt)
		engineReady(t, sender)
		engineReady(t, receiver)
		for _, position := range []uint64{18, 19, 20} {
			if err := sender.Push(engineFrame(t, position)); err != nil {
				t.Fatal(err)
			}
		}
		ending := make(chan error, 1)
		go func() {
			ctx, cancel := context.WithTimeout(t.Context(), time.Second)
			defer cancel()
			ending <- sender.End(ctx)
		}()
		deliveries := engineDrain(t, receiver)
		if len(deliveries) != 4 || deliveries[0].Kind != FrameDelivery || deliveries[3].Kind != EndDelivery || receiver.Stats().ReceivedFrames != 3 {
			t.Fatalf("burst%d lost media or consumed an old spurt's End: %#v", burst, deliveries)
		}
		if err := <-ending; err != nil {
			t.Fatal(err)
		}
		if err := <-senderDone; err != nil {
			t.Fatal(err)
		}
		if err := <-receiverDone; err != nil {
			t.Fatal(err)
		}
		for _, session := range []*Session{home, serving} {
			select {
			case <-session.done:
				t.Fatal("spurt End closed the admitted call's RTP session")
			default:
			}
		}
	}
}
