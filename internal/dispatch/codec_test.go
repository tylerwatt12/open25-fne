package dispatch

import (
	"bytes"
	"context"
	"encoding/binary"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func wavFixture(pcm []byte) []byte {
	data := make([]byte, 44+len(pcm))
	copy(data, "RIFF")
	binary.LittleEndian.PutUint32(data[4:], uint32(len(data)-8))
	copy(data[8:], "WAVEfmt ")
	binary.LittleEndian.PutUint32(data[16:], 16)
	binary.LittleEndian.PutUint16(data[20:], 1)
	binary.LittleEndian.PutUint16(data[22:], 1)
	binary.LittleEndian.PutUint32(data[24:], 8000)
	binary.LittleEndian.PutUint32(data[28:], 16000)
	binary.LittleEndian.PutUint16(data[32:], 2)
	binary.LittleEndian.PutUint16(data[34:], 16)
	copy(data[36:], "data")
	binary.LittleEndian.PutUint32(data[40:], uint32(len(pcm)))
	copy(data[44:], pcm)
	return data
}

func TestWAVBoundsAndCodecMetadata(t *testing.T) {
	pcm := make([]byte, FrameBytes)
	for n := range pcm {
		pcm[n] = byte(n)
	}
	decoded, err := ParseWAV(wavFixture(pcm))
	if err != nil || !bytes.Equal(decoded, pcm) {
		t.Fatal("WAV PCM boundary failed", err)
	}
	for _, mutate := range []func([]byte){func(v []byte) { v[0] = 'X' }, func(v []byte) { binary.LittleEndian.PutUint32(v[40:], 0xffffffff) }, func(v []byte) { binary.LittleEndian.PutUint32(v[24:], 48000) }, func(v []byte) { binary.LittleEndian.PutUint16(v[22:], 2) }} {
		bad := wavFixture(pcm)
		mutate(bad)
		if _, err = ParseWAV(bad); err == nil {
			t.Fatal("malformed/unsupported WAV accepted")
		}
	}
	frame := Frame{CallID: "call", TalkgroupID: 1001, SourceID: 10003, PCM: pcm}
	packet, err := EncodeCodecPacket(frame)
	if err != nil {
		t.Fatal(err)
	}
	if len(packet) != 332 || binary.BigEndian.Uint32(packet[324:]) != 1001 || binary.BigEndian.Uint32(packet[328:]) != 10003 {
		t.Fatal("native source/group metadata not preserved")
	}
	out, err := DecodeCodecPacket(packet)
	if err != nil || out.SourceID != 10003 || out.TalkgroupID != 1001 || !bytes.Equal(out.PCM, pcm) {
		t.Fatal("native PCM metadata roundtrip failed")
	}
	frame.Encrypted = true
	if _, err = EncodeCodecPacket(frame); err == nil {
		t.Fatal("encrypted frame silently encoded as clear")
	}
	if _, err = DecodeCodecPacket(packet[:324]); err == nil {
		t.Fatal("truncated native packet accepted")
	}
}

func TestCodecRenderUsesMetadataAndIndependentLocalPeer(t *testing.T) {
	o := CodecOptions{VoiceMFID: 1, Identity: "N0TEST", MasterAddress: "127.0.0.1", MasterPort: 60001, MasterPassword: strings.Repeat("a", 64), PeerIDs: map[int]int{1001: 100100385}, RadioID: 10002, WACN: 1, SystemID: 1}
	config := RenderCodecConfig(o, 1001, 50010, 50011)
	for _, required := range []string{"voiceMfid: 1", "id: 100100385", "destinationId: 1001", "overrideSourceIdFromUDP: true", "resetCallForSourceIdChange: true", "udpMetadata: true", "grantDemand: false", "localAudio: false", "enable: false", "udpReceivePort: 50010", "udpSendPort: 50011"} {
		if !strings.Contains(config, required) {
			t.Fatal("missing codec setting", required)
		}
	}
}

func TestCodecFailureStopsItsProcessAndDoesNotLeaveReadyState(t *testing.T) {
	if testing.Short() {
		t.Skip("process supervision")
	}
	script := filepath.Join(t.TempDir(), "fake-codec")
	if err := os.WriteFile(script, []byte("#!/bin/sh\nprintf 'logged into the master successfully\\n'\nexec sleep 30\n"), 0700); err != nil {
		t.Fatal(err)
	}
	c, err := NewCodec(CodecOptions{Binary: script, StateDir: t.TempDir(), Identity: "N0TEST", MasterAddress: "127.0.0.1", MasterPort: 60001, MasterPassword: strings.Repeat("a", 64), PeerIDs: map[int]int{1001: 100100385}, RadioID: 10002, WACN: 1, SystemID: 1, StartupTimeout: time.Second})
	if err != nil {
		t.Fatal(err)
	}
	if err = c.Start(context.Background()); err != nil {
		t.Fatal(err)
	}
	status := c.Status()
	if len(status) != 1 || !status[0].Ready || status[0].PID == 0 {
		t.Fatal("codec readiness not supervised")
	}
	if err = c.Close(); err != nil {
		t.Fatal(err)
	}
	if c.Status()[0].Ready {
		t.Fatal("closed codec still ready")
	}
	if err = c.Close(); err != nil {
		t.Fatal("close is not idempotent")
	}
}
