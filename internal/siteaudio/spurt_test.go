package siteaudio

import (
	"testing"
	"time"
	"open25.local/open25-fne/internal/dispatch"
	"open25.local/open25-fne/internal/p25rtp"
)

func TestSuccessiveSpurtsHaveDistinctMediaAndOneHeldNativeGrant(t *testing.T) {
	n := &capacityNative{limit: 1}
	codec := &independentCodec{}
	r := capacityRouter(n, codec)
	g, err := r.Begin(t.Context(), request("held-allocation", 1001))
	if err != nil {
		t.Fatal(err)
	}
	for epoch := uint64(1); epoch <= 2; epoch++ {
		frame := dispatch.Frame{CallID: g.ID, SourceEpoch: epoch, SourceID: 10002, TalkgroupID: 1001, Sequence: epoch * 19, Encoded: make([]byte, 11), AlgorithmID: 0x80, ServiceOptions: 4, P25: &dispatch.P25VoiceMetadata{Type: p25rtp.FR1}, CapturedAt: time.Now()}
		if err = r.Write(t.Context(), g, frame); err != nil {
			t.Fatal(err)
		}
		if err = r.EndSpurt(t.Context(), g); err != nil {
			t.Fatal(err)
		}
		if err = r.EndSpurt(t.Context(), g); err != nil {
			t.Fatal("duplicate spurt End", err)
		}
		if n.active() != 1 {
			t.Fatal("spurt End released the held allocation")
		}
	}
	codec.mu.Lock()
	if len(codec.frames) != 4 || !codec.frames[1].End || !codec.frames[3].End || codec.frames[0].CallID == codec.frames[2].CallID || codec.frames[0].CallID != codec.frames[1].CallID || codec.frames[2].CallID != codec.frames[3].CallID {
		t.Error("source identity reused, end mismatched or audio fabricated", codec.frames)
	}
	codec.mu.Unlock()
	if err = r.End(t.Context(), g); err != nil || n.active() != 0 {
		t.Fatal("final allocation release", err)
	}
}
