package dispatch

import (
	"context"
	"os"
	"testing"
	"time"
)

func TestPacketCodecDelayedBookkeepingCannotRewindEncoderTail(t *testing.T) {
	codec := &PacketCodec{calls: map[string]map[[2]int]packetCodecTarget{}}
	frame := Frame{CallID: "same-call", SourceID: 10002, TalkgroupID: 1001, Sequence: 3, VoiceIndex: 2, Encoded: make([]byte, 11), AlgorithmID: 0x80}
	codec.remember(frame, true)
	frame.Sequence, frame.VoiceIndex = 1, 0
	codec.remember(frame, false)
	codec.remember(frame, true)
	target := codec.calls[frame.CallID][[2]int{frame.SourceID, frame.TalkgroupID}]
	if target.encoderTail == nil || target.encoderTail.Sequence != 3 || target.encoderTail.VoiceIndex != 2 || target.frame.Sequence != 3 {
		t.Fatal("a delayed encoder or decoder completion rewound the remembered stream")
	}
}

func tailTestNativeCodec(t *testing.T) *PacketCodec {
	t.Helper()
	path := os.Getenv("TRUNKSYSTEM_PACKET_CODEC")
	if path == "" {
		t.Skip("set TRUNKSYSTEM_PACKET_CODEC to a reviewed native dvmbridge")
	}
	codec, err := NewPacketCodec(PacketCodecOptions{Binary: path, KeyFile: "-"})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := codec.Close(); err != nil {
			t.Error(err)
		}
	})
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	if err := codec.Start(ctx); err != nil {
		t.Fatal(err)
	}
	return codec
}

// This exercises the actual local codec endpoint without a network or RF device.
func TestNativePacketCodecDelayedDecodePreservesEncoderTail(t *testing.T) {
	codec := tailTestNativeCodec(t)
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	var first Frame
	for seq := uint64(1); seq <= 3; seq++ {
		input := Frame{CallID: "delayed-decode-tail", SourceID: 10002, TalkgroupID: 1001, Sequence: seq, PCM: make([]byte, FrameBytes), AlgorithmID: 0x80}
		encoded, err := codec.Encode(ctx, input)
		if err != nil {
			t.Fatalf("encode sequence %d: %v", seq, err)
		}
		if seq == 1 {
			first = encoded
		}
	}
	if _, err := codec.Decode(ctx, first); err != nil {
		t.Fatalf("decode delayed sequence 1 on the same endpoint: %v", err)
	}
	tail, err := codec.Finish(ctx, first.CallID)
	if err != nil {
		t.Fatalf("finish the encoder after delayed loopback decoding: %v", err)
	}
	if len(tail) != 6 {
		t.Fatalf("expected encoder tail sequences 4..9, got %d frames", len(tail))
	}
	for i, frame := range tail {
		if frame.Sequence != uint64(i+4) || frame.VoiceIndex != uint8(i+3) || len(frame.Encoded) != 11 || len(frame.PCM) != 0 {
			t.Fatalf("tail[%d] did not continue the encoder: %+v", i, frame)
		}
	}
	if err := codec.End(ctx, first.CallID); err != nil {
		t.Fatalf("native cleanup: %v", err)
	}
	if _, err := codec.Encode(ctx, Frame{CallID: first.CallID, SourceID: first.SourceID, TalkgroupID: first.TalkgroupID, Sequence: 1, PCM: make([]byte, FrameBytes), AlgorithmID: 0x80}); err != nil {
		t.Fatalf("End did not retire the native encoder context: %v", err)
	}
	if err := codec.End(ctx, first.CallID); err != nil {
		t.Fatal(err)
	}
}

func TestNativePacketCodecDecodeOnlyFinishDoesNotEncodeAudio(t *testing.T) {
	producer := tailTestNativeCodec(t)
	decoder := tailTestNativeCodec(t)
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	frames := []Frame{}
	for _, tg := range []int{1001, 1002} {
		frame, err := producer.Encode(ctx, Frame{CallID: "decode-only", SourceID: 10002, TalkgroupID: tg, Sequence: 1, PCM: make([]byte, FrameBytes), AlgorithmID: 0x80})
		if err != nil {
			t.Fatal(err)
		}
		if _, err := decoder.Decode(ctx, frame); err != nil {
			t.Fatal(err)
		}
		frames = append(frames, frame)
	}
	if tail, err := decoder.Finish(ctx, "decode-only"); err != nil || len(tail) != 0 {
		t.Fatalf("receive-only contexts must not generate encoded tail audio: %d frames, %v", len(tail), err)
	}
	if err := decoder.End(ctx, "decode-only"); err != nil {
		t.Fatal(err)
	}
	// Native sequence validation rejects these repeats unless both contexts ended.
	for _, frame := range frames {
		if _, err := decoder.Decode(ctx, frame); err != nil {
			t.Fatalf("End did not retire talkgroup %d's decoder context: %v", frame.TalkgroupID, err)
		}
	}
	if err := decoder.End(ctx, "decode-only"); err != nil {
		t.Fatal(err)
	}
	if err := producer.End(ctx, "decode-only"); err != nil {
		t.Fatal(err)
	}
}
