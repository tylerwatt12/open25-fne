package dispatch

import (
	"bufio"
	"bytes"
	"context"
	"encoding/binary"
	"errors"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func packetFixture() Frame {
	return Frame{CallID: "opaque-call", TalkgroupID: 1001, SourceID: 10001, Sequence: 1, P25: testP25Metadata(0), Encoded: bytes.Repeat([]byte{0x55}, 11), AlgorithmID: 0x84, KeyID: 7, Encrypted: true, ServiceOptions: 0x44, MessageIndicator: []byte{1, 2, 3, 4, 5, 6, 7, 8, 9}}
}
func TestP25MediaValidationRejectsMixedAndIncompleteEncryption(t *testing.T) {
	f := packetFixture()
	if !f.MediaValid() {
		t.Fatal("valid AES frame rejected")
	}
	for name, mutate := range map[string]func(*Frame){
		"mixed":     func(f *Frame) { f.PCM = make([]byte, FrameBytes) },
		"algorithm": func(f *Frame) { f.AlgorithmID = 0x82 },
		"flag":      func(f *Frame) { f.Encrypted = false },
		"index":     func(f *Frame) { f.VoiceIndex = 18 },
		"length":    func(f *Frame) { f.Encoded = f.Encoded[:10] },
	} {
		t.Run(name, func(t *testing.T) {
			v := packetFixture()
			mutate(&v)
			if v.MediaValid() {
				t.Fatal("invalid P25 media accepted")
			}
		})
	}
	f = Frame{PCM: make([]byte, FrameBytes)}
	if !f.MediaValid() {
		t.Fatal("raw endpoint PCM rejected")
	}
	f.Encrypted = true
	if f.MediaValid() {
		t.Fatal("plaintext labeled encrypted accepted")
	}
	f = Frame{Encoded: make([]byte, 11), P25: testP25Metadata(0), AlgorithmID: 0x80}
	if !f.MediaValid() {
		t.Fatal("clear IMBE rejected")
	}
	f.KeyID = 7
	f.AlgorithmID = 132
	if f.MediaValid() {
		t.Fatal("clear IMBE carrying KID accepted")
	}
}
func TestOpaqueP25WireRoundTripPreservesCiphertextAndMetadata(t *testing.T) {
	f := packetFixture()
	f.VoiceIndex = 17
	f.P25 = testP25Metadata(17)
	packet, err := EncodeOpaqueCodecPacket(f)
	if err != nil {
		t.Fatal(err)
	}
	if len(packet) != 81 || bytes.Contains(packet, []byte("secret-key-material")) {
		t.Fatal("unexpected wire payload")
	}
	got, err := DecodeCodecPacket(packet)
	if err != nil {
		t.Fatal(err)
	}
	if !got.Encrypted || got.AlgorithmID != 132 || got.KeyID != 7 || got.VoiceIndex != 17 || got.Sequence != 1 || got.SourceID != 10001 || got.TalkgroupID != 1001 || !bytes.Equal(got.Encoded, f.Encoded) || len(got.MessageIndicator) != 0 || len(got.PCM) != 0 {
		t.Fatalf("P25 metadata changed: %+v", got)
	}
	for _, mutate := range []func([]byte){func(p []byte) { p[48] = 1 }, func(p []byte) { p[4] = 1 }, func(p []byte) { p[5] = 1 }, func(p []byte) { p[7] = 18 }, func(p []byte) { p[6] = 130 }, func(p []byte) { binary.BigEndian.PutUint16(p[58:], 10) }, func(p []byte) { binary.BigEndian.PutUint16(p[10:], 2) }} {
		p := append([]byte{}, packet...)
		mutate(p)
		if _, err := DecodeOpaqueCodecPacket(p); err == nil {
			t.Fatal("invalid opaque packet accepted")
		}
	}
	f.End = true
	f.Encoded = nil
	f.P25 = nil
	packet, err = EncodeOpaqueCodecPacket(f)
	if err != nil {
		t.Fatal(err)
	}
	if len(packet) != 64 {
		t.Fatal("end retained audio")
	}
	got, err = DecodeOpaqueCodecPacket(packet)
	if err != nil || !got.End {
		t.Fatal("opaque end lost", err)
	}
}
func TestPacketCodecResponseCannotChangeIdentityOrSecurity(t *testing.T) {
	f := packetFixture()
	packet, err := marshalPacketFrame(3, f)
	if err != nil {
		t.Fatal(err)
	}
	packet[4] = 0x81
	packet[61] = f.ServiceOptions
	if _, err = unmarshalPacketFrame(packet, f, 1); err != nil {
		t.Fatal(err)
	}
	for _, offset := range []int{6, 8, 12, 16, 24, 32} {
		bad := append([]byte{}, packet...)
		bad[offset] ^= 1
		if _, err = unmarshalPacketFrame(bad, f, 1); err == nil {
			t.Fatalf("changed field at %d accepted", offset)
		}
	}
}
func TestPacketCodecPrivateFileAndOpaqueConfig(t *testing.T) {
	binaryPath := filepath.Join(t.TempDir(), "codec")
	if err := os.WriteFile(binaryPath, []byte("#!/bin/sh\nexit 0\n"), 0700); err != nil {
		t.Fatal(err)
	}
	key := filepath.Join(t.TempDir(), "keys")
	if err := os.WriteFile(key, []byte("132:7:"+strings.Repeat("ab", 32)+"\n"), 0644); err != nil {
		t.Fatal(err)
	}
	if _, err := NewPacketCodec(PacketCodecOptions{Binary: binaryPath, KeyFile: key}); err == nil {
		t.Fatal("world-readable key file accepted")
	}
	if err := os.Chmod(key, 0600); err != nil {
		t.Fatal(err)
	}
	if _, err := NewPacketCodec(PacketCodecOptions{Binary: binaryPath, KeyFile: key}); err != nil {
		t.Fatal(err)
	}
	link := key + "-link"
	if err := os.Symlink(key, link); err != nil {
		t.Fatal(err)
	}
	if _, err := NewPacketCodec(PacketCodecOptions{Binary: binaryPath, KeyFile: link}); err == nil {
		t.Fatal("symlink key file accepted")
	}
	config := RenderCodecConfig(CodecOptions{Opaque: true, PeerIDs: map[int]int{1001: 123}}, 1001, 10000, 10001)
	if !strings.Contains(config, "opaqueP25: true") || strings.Contains(config, "132:7:") {
		t.Fatal("Site configuration is not keyless opaque mode")
	}
}

func TestPacketCodecRejectsBadProcessProtocolAndStopsHelper(t *testing.T) {
	path := filepath.Join(t.TempDir(), "echo-codec")
	if err := os.WriteFile(path, []byte("#!/bin/sh\nexec cat\n"), 0700); err != nil {
		t.Fatal(err)
	}
	codec, err := NewPacketCodec(PacketCodecOptions{Binary: path, KeyFile: "-"})
	if err != nil {
		t.Fatal(err)
	}
	defer codec.Close()
	if err = codec.Start(context.Background()); err == nil || codec.Alive() {
		t.Fatal("bad helper response did not fail closed during startup")
	}
}

func TestEncryptedZeroKeyIDIsNotClearMedia(t *testing.T) {
	for _, algorithm := range []uint8{129, 132, 170} {
		f := packetFixture()
		f.AlgorithmID = algorithm
		f.KeyID = 0
		if !f.MediaValid() {
			t.Fatalf("encrypted key0 rejected for %x", algorithm)
		}
		packet, err := EncodeOpaqueCodecPacket(f)
		if err != nil {
			t.Fatal(err)
		}
		got, err := DecodeOpaqueCodecPacket(packet)
		if err != nil || !got.Encrypted || got.AlgorithmID != algorithm || got.KeyID != 0 {
			t.Fatal("zero key lost encrypted identity", err)
		}
	}
}

func TestP25VoiceMFIDWirePreservationAndBounds(t *testing.T) {
	for _, mfid := range []uint8{0, 1} {
		frame := packetFixture()
		frame.ManufacturerID = mfid
		packet, err := EncodeOpaqueCodecPacket(frame)
		if err != nil {
			t.Fatal(err)
		}
		if packet[57] != mfid {
			t.Fatal("MFID missing from packet byte 57")
		}
		decoded, err := DecodeOpaqueCodecPacket(packet)
		if err != nil || decoded.ManufacturerID != mfid {
			t.Fatal("opaque MFID changed", err)
		}
		response, err := marshalPacketFrame(3, frame)
		if err != nil {
			t.Fatal(err)
		}
		response[4] = 0x81
		response[61] = frame.ServiceOptions
		if _, err = unmarshalPacketFrame(response, frame, 1); err != nil {
			t.Fatal(err)
		}
		response[57] ^= 1
		if _, err = unmarshalPacketFrame(response, frame, 1); err == nil {
			t.Fatal("native response changed MFID silently")
		}
	}
	for _, mfid := range []uint8{2, 0x90, 0xa4, 0xff} {
		frame := packetFixture()
		frame.ManufacturerID = mfid
		if frame.MediaValid() {
			t.Fatalf("unsupported standard voice MFID %X accepted", mfid)
		}
		if _, err := EncodeOpaqueCodecPacket(frame); err == nil {
			t.Fatal("unsupported MFID encoded")
		}
		frame.ManufacturerID = 0
		packet, _ := EncodeOpaqueCodecPacket(frame)
		packet[57] = mfid
		if _, err := DecodeOpaqueCodecPacket(packet); err == nil {
			t.Fatal("unsupported MFID decoded")
		}
	}
}

func TestOpaqueEmergencyMetadataWireContract(t *testing.T) {
	for _, emergency := range []bool{false, true} {
		f := packetFixture()
		f = f.WithEmergency(emergency)
		packet, err := EncodeOpaqueCodecPacket(f)
		if err != nil {
			t.Fatal(err)
		}
		expected := byte(0)
		if emergency {
			expected = 1
		}
		if packet[60] != expected || packet[61] != f.ServiceOptions || !bytes.Equal(packet[62:64], []byte{0, 0}) || binary.BigEndian.Uint16(packet[10:12]) != 0 {
			t.Fatal("private emergency changed flags/reserved fields")
		}
		got, err := DecodeOpaqueCodecPacket(packet)
		if err != nil || got.Emergency != emergency {
			t.Fatal("opaque emergency lost", got, err)
		}
		for _, offset := range []int{60, 61, 62, 63} {
			bad := append([]byte{}, packet...)
			bad[offset] = 2
			if _, err = DecodeOpaqueCodecPacket(bad); err == nil {
				t.Fatal("malformed private emergency accepted", offset)
			}
		}
		response := append([]byte{}, packet...)
		response[4] = 0x81
		response[61] = f.ServiceOptions
		if _, err = unmarshalPacketFrame(response, f, 1); err != nil {
			t.Fatal(err)
		}
		response[60] ^= 1
		if _, err = unmarshalPacketFrame(response, f, 1); err == nil {
			t.Fatal("codec changed emergency metadata")
		}
		f.End = true
		f.Encoded = nil
		f.P25 = nil
		end, err := EncodeOpaqueCodecPacket(f)
		if err != nil {
			t.Fatal(err)
		}
		got, err = DecodeOpaqueCodecPacket(end)
		if err != nil || !got.End || got.Emergency != emergency || len(end) != 64 {
			t.Fatal("End lost owned metadata", got, err)
		}
	}
}

// This exercises the real Go binary protocol and cleanup owner using a pipe
// peer. It does not qualify native public-sync decoding or cryptography.
func TestPacketCodecPendingSyncStateIsEnded(t *testing.T) {
	requestsRead, requestsWrite := io.Pipe()
	responsesRead, responsesWrite := io.Pipe()
	observed := make(chan []byte, 2)
	done := make(chan struct{})
	go func() {
		defer close(done)
		defer responsesWrite.Close()
		for n := 0; n < 2; n++ {
			var size [4]byte
			if _, err := io.ReadFull(requestsRead, size[:]); err != nil {
				return
			}
			packet := make([]byte, binary.BigEndian.Uint32(size[:]))
			if _, err := io.ReadFull(requestsRead, packet); err != nil {
				return
			}
			observed <- append([]byte(nil), packet...)
			response := append([]byte(nil), packet[:packetHeaderSize]...)
			response[4] |= 0x80
			response[58], response[59] = 0, 0
			if n == 0 {
				response[5] = 7
			}
			binary.BigEndian.PutUint32(size[:], uint32(len(response)))
			if _, err := responsesWrite.Write(append(size[:], response...)); err != nil {
				return
			}
		}
	}()
	t.Cleanup(func() {
		requestsWrite.Close()
		requestsRead.Close()
		responsesRead.Close()
		responsesWrite.Close()
		select {
		case <-done:
		case <-time.After(time.Second):
			t.Error("pipe peer did not stop")
		}
	})
	codec := &PacketCodec{command: &exec.Cmd{}, input: requestsWrite, output: bufio.NewReader(responsesRead), gate: make(chan struct{}, 1), calls: map[string]map[[2]int]packetCodecTarget{}}
	frame := packetFixture()
	frame.MessageIndicator = nil
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	if _, err := codec.Decode(ctx, frame); !errors.Is(err, ErrP25SynchronizationPending) {
		t.Fatalf("pending result: %v", err)
	}
	first := <-observed
	if first[4] != 2 || !bytes.Equal(first[48:57], make([]byte, 9)) {
		t.Fatal("raw decode request retained a common MI")
	}
	if len(codec.calls[frame.CallID]) != 1 {
		t.Fatal("pending native source lost its cleanup owner")
	}
	if err := codec.End(ctx, frame.CallID); err != nil {
		t.Fatal(err)
	}
	second := <-observed
	if second[4] != 1 || binary.BigEndian.Uint16(second[10:12]) != packetFlagEnd || len(second) != packetHeaderSize {
		t.Fatal("pending cleanup emitted media or used the wrong operation")
	}
	if len(codec.calls) != 0 {
		t.Fatal("pending cleanup retained call")
	}
}

func TestRawServiceOptionsPriorityAndReservedObservations(t *testing.T) {
	for _, service := range []uint8{0, 2, 7, 0x38, 0x3f, 0x40, 0x42, 0x47, 0x78, 0xC0, 0xFF} {
		frame := packetFixture()
		frame.ServiceOptions = service
		frame.Emergency = service&0x80 != 0
		frame.Encrypted = service&0x40 != 0
		if !frame.Encrypted {
			frame.AlgorithmID = 0x80
			frame.KeyID = 0
			frame.MessageIndicator = nil
		}
		wire, err := EncodeOpaqueCodecPacket(frame)
		if err != nil {
			t.Fatal(service, err)
		}
		decoded, err := DecodeOpaqueCodecPacket(wire)
		if err != nil || decoded.ServiceOptions != service || wire[61] != service {
			t.Fatal("raw service changed", service, decoded, err)
		}
	}
}
