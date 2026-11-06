package dispatch

import (
	"encoding/binary"
	"os"
	"strings"
	"sync"
	"testing"
	"time"
)

// The wire bytes are built independently of the new control decoder. Replay
// ordinary nine-word LDU bursts, then the observed approximately one-second
// delivery gap. A heartbeat contains no voice and cannot replace a word.
func TestCodecOpaqueCapturedDeliveryGapRetainsApplicationCall(t *testing.T) {
	binaryPath, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	c, err := NewCodec(CodecOptions{Binary: binaryPath, StateDir: t.TempDir(), Identity: "offline-gap-replay", MasterAddress: "127.0.0.1", MasterPort: 60001, MasterPassword: strings.Repeat("a", 64), PeerIDs: map[int]int{1001: 123}, RadioID: 10002, WACN: 1, SystemID: 1, Opaque: true})
	if err != nil {
		t.Fatal(err)
	}
	r := shutdownResource(t)
	c.options.Opaque = true
	var mu sync.Mutex
	var frames []Frame
	c.options.OnFrame = func(f Frame) { mu.Lock(); frames = append(frames, f); mu.Unlock() }
	c.wg.Add(2)
	go c.receive(r)
	go c.publishFrames()
	t.Cleanup(func() { c.cancel(); r.read.Close(); c.wg.Wait() })
	f := Frame{CallID: "captured-native-call", TalkgroupID: 1001, SourceID: 10001, AlgorithmID: 0x80, Encoded: make([]byte, 11)}
	var packet []byte
	burst := func() {
		for range 9 {
			f.Sequence++
			f.VoiceIndex = uint8((f.Sequence - 1) % 18)
			f.P25 = testP25Metadata(f.VoiceIndex)
			f.Encoded[0] = byte(f.Sequence)
			var err error
			packet, err = EncodeOpaqueCodecPacket(f)
			if err != nil {
				t.Fatal(err)
			}
			if _, err = r.write.Write(packet); err != nil {
				t.Fatal(err)
			}
		}
	}
	burst()
	time.Sleep(180 * time.Millisecond)
	burst()
	deadline := time.Now().Add(time.Second)
	for {
		mu.Lock()
		count := len(frames)
		mu.Unlock()
		if count >= 18 {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("initial two LDU bursts were not delivered")
		}
		time.Sleep(time.Millisecond)
	}
	lastVoice := f.Sequence
	for seq := uint64(1); seq <= 10; seq++ {
		time.Sleep(100 * time.Millisecond)
		h := append([]byte{}, packet[:64]...)
		h[4], h[7] = 17, 0
		binary.BigEndian.PutUint16(h[58:60], 0)
		binary.BigEndian.PutUint32(h[20:24], uint32(1500-seq*100))
		binary.BigEndian.PutUint64(h[24:32], seq)
		binary.BigEndian.PutUint64(h[48:56], lastVoice)
		h[56] = 0
		if _, err := r.write.Write(h); err != nil {
			t.Fatal(err)
		}
	}
	burst()
	time.Sleep(70 * time.Millisecond)
	mu.Lock()
	defer mu.Unlock()
	if len(frames) != 27 {
		t.Fatalf("delivery gap changed the received voice/call stream: got%d frames, want27 voice words and no End", len(frames))
	}
	for i, frame := range frames {
		if frame.End || frame.CallID != frames[0].CallID || frame.Sequence != uint64(i+1) || frame.Encoded[0] != byte(i+1) {
			t.Fatalf("word%d lost its application identity, sequence or payload: End=%t sequence=%d", i+1, frame.End, frame.Sequence)
		}
	}
}
