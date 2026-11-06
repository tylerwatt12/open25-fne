package siteaudio

import (
	"context"
	"testing"
	"time"
	"open25.local/open25-fne/internal/dispatch"
	"open25.local/open25-fne/internal/p25rtp"
)

func TestPendingRFIdentityBindsActualMediaUntilSpurtEnds(t *testing.T) {
	codec := &independentCodec{}
	r := capacityRouter(&capacityNative{limit: 1}, codec)
	q := request("pending-rf", 1001)
	q.Secure, q.EncryptionPending = true, true
	g, err := r.Begin(context.Background(), q)
	if err != nil {
		t.Fatal(err)
	}
	defer r.End(context.Background(), g)
	f := dispatch.Frame{CallID: g.ID, SourceID: q.RadioID, TalkgroupID: 1001, SourceEpoch: 1, Sequence: 1, Encoded: make([]byte, 11), P25: &dispatch.P25VoiceMetadata{Type: p25rtp.FR1}, AlgorithmID: 129, KeyID: 29, Encrypted: true, ServiceOptions: 0x44, CapturedAt: time.Now()}
	if err = r.Write(context.Background(), g, f); err != nil {
		t.Fatal("actual key did not bind", err)
	}
	f.Sequence = 2
	f.KeyID = 30
	if err = r.Write(context.Background(), g, f); err == nil {
		t.Fatal("key changed inside a burst")
	}
	f.SourceEpoch = 2
	if err = r.Write(context.Background(), g, f); err == nil {
		t.Fatal("new source epoch replaced an unfinished burst")
	}
	if err = r.EndSpurt(context.Background(), g); err != nil {
		t.Fatal(err)
	}
	f.SourceEpoch = 1
	if err = r.Write(context.Background(), g, f); err == nil {
		t.Fatal("ended source epoch reopened with a replacement key")
	}
	f.SourceEpoch = 2
	if err = r.Write(context.Background(), g, f); err != nil {
		t.Fatal("new burst could not bind actual key", err)
	}
	f.SourceEpoch = 1
	f.Sequence = 3
	f.KeyID = 29
	if err = r.Write(context.Background(), g, f); err == nil {
		t.Fatal("old burst replayed after replacement")
	}
	codec.mu.Lock()
	defer codec.mu.Unlock()
	var voice []dispatch.Frame
	for _, frame := range codec.frames {
		if !frame.End {
			voice = append(voice, frame)
		}
	}
	if len(voice) != 2 || voice[0].KeyID != 29 || voice[1].KeyID != 30 || voice[0].CallID == voice[1].CallID {
		t.Fatal("actual key metadata or burst identity changed", voice)
	}
}
