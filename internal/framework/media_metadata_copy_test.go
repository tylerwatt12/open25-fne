package framework

import (
	"context"
	"open25.local/open25-fne/internal/dispatch"
	"open25.local/open25-fne/internal/p25rtp"
	"open25.local/open25-fne/internal/routing"
	"testing"
	"time"
)

func TestMediaQueueOwnsFullRateMetadata(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	c := &voiceCall{source: true, ctx: ctx, cancel: cancel, request: routing.Request{CallRequest: dispatch.CallRequest{RadioID: 10001, Talkgroups: []int{1001}}}, inputs: make(chan voiceInput, 11)}
	f := dispatch.Frame{SourceID: 10001, SourceEpoch: 1, TalkgroupID: 1001, Encoded: make([]byte, 11), AlgorithmID: 0x80, VoiceIndex: 11, ServiceOptions: 4, CapturedAt: time.Now(), P25: &dispatch.P25VoiceMetadata{Type: p25rtp.FR12, ES: &p25rtp.EncryptionSync{Words: [4]uint8{1, 2, 3, 4}}, Header: &p25rtp.HeaderWord{NID: 0x2930, AlgorithmID: 0x80, GroupID: 1001, VoiceBlockBundling: 1}}}
	if err := c.Write(f); err != nil {
		t.Fatal(err)
	}
	f.Encoded[0] = 99
	f.P25.ES.Words[0] = 63
	f.P25.Header.NID = 0xffff
	f.P25.Quality.TotalErrors = 7
	got := (<-c.inputs).frame
	if got.Encoded[0] != 0 || got.P25.ES.Words[0] != 1 || got.P25.Header.NID != 0x2930 || got.P25.Quality.TotalErrors != 0 {
		t.Fatal("queued source aliases mutable input")
	}
}
