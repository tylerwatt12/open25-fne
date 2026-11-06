package siteaudio

import (
	"context"
	"errors"
	"testing"
	"time"
	"open25.local/open25-fne/internal/dispatch"
	"open25.local/open25-fne/internal/p25rtp"
)

func TestGapResumesOnlyAtActualLDUBoundary(t *testing.T) {
	n := &capacityNative{limit: 1}
	codec := &independentCodec{}
	r := capacityRouter(n, codec)
	g, err := r.Begin(t.Context(), request("gap", 1001))
	if err != nil {
		t.Fatal(err)
	}
	word := func(sequence uint64, index uint8, kind p25rtp.FrameType) dispatch.Frame {
		return dispatch.Frame{CallID: g.ID, SourceID: 10002, TalkgroupID: 1001, Sequence: sequence, Encoded: make([]byte, 11), AlgorithmID: 0x80, ServiceOptions: 4, VoiceIndex: index, P25: &dispatch.P25VoiceMetadata{Type: kind}, CapturedAt: time.Now()}
	}
	if err = r.Write(t.Context(), g, word(1, 0, p25rtp.FR1)); err != nil {
		t.Fatal(err)
	}
	if err = r.Gap(t.Context(), dispatch.Grant{ID: g.ID, Talkgroups: []int{1002}}); !errors.Is(err, dispatch.ErrUnavailable) {
		t.Fatal("foreign grant marked a discontinuity", err)
	}
	if err = r.Gap(t.Context(), g); err != nil {
		t.Fatal(err)
	}
	if err = r.Write(t.Context(), g, word(3, 2, p25rtp.FR3)); err != nil {
		t.Fatal(err)
	}
	if err = r.Write(t.Context(), g, word(10, 9, p25rtp.FR10)); err != nil {
		t.Fatal(err)
	}
	codec.mu.Lock()
	if len(codec.frames) != 2 || codec.frames[1].Sequence != 10 || codec.frames[1].VoiceIndex != 9 {
		t.Error("gap fabricated media or resumed a partial LDU", codec.frames)
	}
	codec.mu.Unlock()
	if err = r.End(context.Background(), g); err != nil {
		t.Fatal(err)
	}
	if err = r.Gap(t.Context(), g); err == nil {
		t.Fatal("retired grant accepted a gap")
	}
}
