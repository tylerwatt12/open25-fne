package framework

import (
	"context"
	"open25.local/open25-fne/internal/dispatch"
	"open25.local/open25-fne/internal/p25rtp"
	"os"
	"testing"
	"time"
)

// Structural IMBE words and raw quality are independent network fixtures, not
// decoded speech or RF interoperability evidence.
func sipTestFrame(call string, rid, tg int, epoch uint64, ordinal uint64) dispatch.Frame {
	types := [18]p25rtp.FrameType{p25rtp.FR1, p25rtp.FR2, p25rtp.FR3, p25rtp.FR4, p25rtp.FR5, p25rtp.FR6, p25rtp.FR7, p25rtp.FR8, p25rtp.FR9, p25rtp.FR10, p25rtp.FR11, p25rtp.FR12, p25rtp.FR13, p25rtp.FR14, p25rtp.FR15, p25rtp.FR16, p25rtp.FR17, p25rtp.FR18}
	index := ordinal % 18
	m := &dispatch.P25VoiceMetadata{Type: types[index], Quality: p25rtp.Quality{Superframe: uint8(ordinal / 18 % 4), TotalErrors: 3, ScaledErrors: 2, U1Errors: 1}}
	if index == 8 || index == 17 {
		m.LSD = &p25rtp.LowSpeedData{Words: [2]byte{0x12, 0x34}, Status: [2]uint8{1, 2}}
	}
	if index >= 11 && index <= 16 {
		m.ES = &p25rtp.EncryptionSync{Words: [4]uint8{1, 2, 3, 4}, Status: 5}
	}
	if ordinal == 0 {
		m.Header = &p25rtp.HeaderWord{NID: 0x2930, MFID: 1, GroupID: uint16(tg), AlgorithmID: 0x80, KeyID: 0x1234, MI: [9]byte{9, 8, 7, 6, 5, 4, 3, 2, 1}, VoiceBlockBundling: 1}
	}
	return dispatch.Frame{CallID: call, SourceID: rid, TalkgroupID: tg, SourceEpoch: epoch, Sequence: ordinal + 1, Encoded: []byte{1, 2, 3, 4, 5, 6, 7, 8, 9, 10, 11}, P25: m, VoiceIndex: uint8(index), AlgorithmID: 0x80, ServiceOptions: 4, CapturedAt: time.Now()}
}

// PCM sources use the actual trusted native encoder; interservice transport
// always receives structured FullRate words.
func requireMediaPacketCodec(t *testing.T, c *mediaCluster) *dispatch.PacketCodec {
	t.Helper()
	if codec, _ := c.dispatch.service.packetCodec(); codec != nil && codec.Alive() {
		return codec
	}
	binary := os.Getenv("TRUNKSYSTEM_PACKET_CODEC")
	if binary == "" {
		t.Skip("set reviewed native packet codec executable")
	}
	codec, err := dispatch.NewPacketCodec(dispatch.PacketCodecOptions{Binary: binary, KeyFile: "-"})
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	if err = codec.Start(ctx); err != nil {
		t.Fatal(err)
	}
	c.dispatch.service.crypto.mu.Lock()
	c.dispatch.service.crypto.codec = codec
	c.dispatch.service.crypto.mu.Unlock()
	t.Cleanup(func() { _ = codec.Close() })
	return codec
}
