//go:build linux

package dispatch

import (
	"bytes"
	"context"
	"crypto/aes"
	"crypto/cipher"
	"crypto/des"
	"crypto/rc4"
	"encoding/binary"
	"encoding/hex"
	"fmt"
	"math"
	"net"
	"open25.local/open25-fne/internal/radio"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"strings"
	"syscall"
	"testing"
	"time"
)

func nativePacketCodec(t *testing.T, keyed bool) *PacketCodec {
	t.Helper()
	binaryPath := os.Getenv("TRUNKSYSTEM_PACKET_CODEC")
	if binaryPath == "" {
		t.Skip("set TRUNKSYSTEM_PACKET_CODEC to a reviewed native dvmbridge")
	}
	keyFile := "-"
	if keyed {
		keyFile = filepath.Join(t.TempDir(), "traffic-keys")
		if err := os.WriteFile(keyFile, []byte("132:7:"+strings.Repeat("0123456789abcdef", 4)+"\n"), 0600); err != nil {
			t.Fatal(err)
		}
	}
	codec, err := NewPacketCodec(PacketCodecOptions{Binary: binaryPath, KeyFile: keyFile})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = codec.Close() })
	if err = codec.Start(context.Background()); err != nil {
		t.Fatal(err)
	}
	return codec
}
func nativeToneFrame(call string, seq uint64, encrypted bool) Frame {
	pcm := make([]byte, FrameBytes)
	for i := 0; i < 160; i++ {
		v := int16(4000 * math.Sin(2*math.Pi*700*float64(int(seq-1)*160+i)/8000))
		binary.LittleEndian.PutUint16(pcm[i*2:], uint16(v))
	}
	f := Frame{CallID: call, TalkgroupID: 1001, SourceID: 10003, Sequence: seq, PCM: pcm, AlgorithmID: 128, Encrypted: encrypted}
	if encrypted {
		f.AlgorithmID = 132
		f.KeyID = 7
		f.AlgorithmID = 132
	}
	return f
}

// This uses Go's standard-library AES/OFB implementation, separately from DVM's
// AES and P25 crypto implementation. The IMBE placement covers both LDUs and
// the two synchronization-bit holes in each 101-byte interval.
func independentP25AES(key, mi, plain []byte, index int) []byte {
	iv := make([]byte, 16)
	copy(iv, mi[:8])
	lfsr := binary.BigEndian.Uint64(mi[:8])
	for i := 0; i < 64; i++ {
		feedback := ((lfsr >> 63) ^ (lfsr >> 61) ^ (lfsr >> 45) ^ (lfsr >> 37) ^ (lfsr >> 26) ^ (lfsr >> 14)) & 1
		lfsr = (lfsr << 1) | feedback
	}
	binary.BigEndian.PutUint64(iv[8:], lfsr)
	block, _ := aes.NewCipher(key)
	stream := make([]byte, 240)
	cipher.NewOFB(block, iv).XORKeyStream(stream, stream)
	offset := 16 + 11 + (index%9)*11
	if index >= 9 {
		offset += 101
	}
	if index%9 == 8 {
		offset += 2
	}
	out := append([]byte{}, plain...)
	for i := range out {
		out[i] ^= stream[offset+i]
	}
	return out
}
func TestNativePacketCodecAESKnownAnswers(t *testing.T) {
	codec := nativePacketCodec(t, true)
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	key := bytes.Repeat([]byte{1, 0x23, 0x45, 0x67, 0x89, 0xab, 0xcd, 0xef}, 4)
	for index := 0; index < 18; index++ {
		f := packetFixture()
		f.CallID = "known-answer"
		f.Sequence = uint64(index + 1)
		f.VoiceIndex = uint8(index)
		f.P25 = testP25Metadata(f.VoiceIndex)
		got, err := codec.transform(ctx, 3, f)
		if err != nil {
			t.Fatal(err)
		}
		want := independentP25AES(key, f.MessageIndicator, f.Encoded, index)
		if !bytes.Equal(got.Encoded, want) {
			t.Fatalf("AES keystream mismatch at voice index %d: got %x want %x", index, got.Encoded, want)
		}
	}
}
func TestNativePacketCodecEncryptedVoiceAndCallIsolation(t *testing.T) {
	codec := nativePacketCodec(t, true)
	clearDecoder := nativePacketCodec(t, false)
	secureDecoder := nativePacketCodec(t, true)
	key := bytes.Repeat([]byte{1, 0x23, 0x45, 0x67, 0x89, 0xab, 0xcd, 0xef}, 4)
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	var firstMI []byte
	changed := false
	nonzero := false
	for seq := uint64(1); seq <= 37; seq++ {
		clear, err := codec.Encode(ctx, nativeToneFrame("clear", seq, false))
		if err != nil {
			t.Fatal(err)
		}
		secure, err := codec.Encode(ctx, nativeToneFrame("secure", seq, true))
		if err != nil {
			t.Fatal(err)
		}
		if seq == 1 {
			firstMI = append([]byte{}, secure.MessageIndicator...)
		}
		if seq == 19 {
			changed = !bytes.Equal(firstMI, secure.MessageIndicator)
		}
		if bytes.Equal(clear.Encoded, secure.Encoded) || len(secure.PCM) != 0 || !secure.Encrypted || secure.VoiceIndex != uint8((seq-1)%18) {
			t.Fatal("encoded encryption metadata or payload invalid")
		}
		// The independent clear oracle needs clear Header/ESS, not copied cipher metadata.
		plain := clear
		plain.CallID = "independent-plain"
		plain.P25 = clear.P25.Clone()
		plain.Encoded = independentP25AES(key, secure.MessageIndicator, secure.Encoded, int(secure.VoiceIndex))
		plain.MessageIndicator = nil
		if !plain.MediaValid() || !secure.MediaValid() {
			t.Fatal("independent clear oracle or original encrypted frame is invalid")
		}
		clearPCM, err := clearDecoder.Decode(ctx, plain)
		if err != nil {
			t.Fatal(err)
		}
		securePCM, err := secureDecoder.Decode(ctx, secure)
		if err != nil {
			t.Fatal(err)
		}
		if !bytes.Equal(clearPCM.PCM, securePCM.PCM) || securePCM.Encrypted || len(securePCM.Encoded) != 0 {
			t.Fatal("AES decode differs from identical clear IMBE audio")
		}
		for _, v := range securePCM.PCM {
			nonzero = nonzero || v != 0
		}
	}
	if !changed || !nonzero {
		t.Fatal("MI did not advance or decoder returned silence")
	}
	tail, err := codec.Finish(ctx, "secure")
	if err != nil {
		t.Fatal(err)
	}
	if len(tail) != 8 || tail[len(tail)-1].VoiceIndex != 8 {
		t.Fatalf("encrypted LDU tail not completed: %d frames", len(tail))
	}
	for _, f := range tail {
		if !f.Encrypted || len(f.Encoded) != 11 || len(f.PCM) != 0 {
			t.Fatal("plaintext escaped in encrypted tail")
		}
	}
	if err = codec.End(ctx, "secure"); err != nil {
		t.Fatal(err)
	}
	if _, err = codec.Encode(ctx, nativeToneFrame("secure", 1, true)); err != nil {
		t.Fatal("end did not clear sequence state", err)
	}
	if _, err = codec.Encode(ctx, nativeToneFrame("secure", 1, true)); err == nil {
		t.Fatal("duplicate frame sequence accepted")
	}
}
func TestNativePacketCodecMissingKeyFailsClosed(t *testing.T) {
	codec := nativePacketCodec(t, false)
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if _, err := codec.Encode(ctx, nativeToneFrame("missing-key", 1, true)); err == nil || !strings.Contains(err.Error(), "key is unavailable") {
		t.Fatal("missing key did not fail closed", err)
	}
	if _, err := codec.Encode(ctx, nativeToneFrame("clear-still-works", 1, false)); err != nil {
		t.Fatal("missing-key rejection broke clear codec", err)
	}
}

// Two real keyless Site bridges exchange encoded DES/AES/ADP voice through an isolated
// loopback FNE. No host engine, modem, transmitter, or traffic key is configured
// in either bridge. This verifies the current raw FullRate carrier mapping, not RF qualification.
func TestNativeOpaqueP25ThroughPrivateFNE(t *testing.T) {
	codecBinary := os.Getenv("TRUNKSYSTEM_PACKET_CODEC")
	fneBinary := os.Getenv("TRUNKSYSTEM_PACKET_FNE")
	if codecBinary == "" || fneBinary == "" {
		t.Skip("set reviewed packet codec and FNE executable paths")
	}
	state := t.TempDir()
	if evidence := os.Getenv("TRUNKSYSTEM_NATIVE_EVIDENCE"); evidence != "" {
		if err := os.MkdirAll(evidence, 0700); err != nil {
			t.Fatal(err)
		}
		var err error
		state, err = os.MkdirTemp(evidence, "opaque-fne-")
		if err != nil {
			t.Fatal(err)
		}
		t.Logf("private opaque FNE evidence: %s", state)
	}
	reserved, err := net.ListenUDP("udp4", &net.UDPAddr{IP: net.IPv4(127, 0, 0, 1)})
	if err != nil {
		t.Fatal(err)
	}
	port := reserved.LocalAddr().(*net.UDPAddr).Port
	reserved.Close()
	profile := nativeCodecProfile()
	controller, err := radio.New(radio.Options{StateDir: filepath.Join(state, "controller"), FNEPort: port, AllowRF: false})
	if err != nil {
		t.Fatal(err)
	}
	defer controller.Close()
	network, err := controller.NetworkFor(profile)
	if err != nil {
		t.Fatal(err)
	}
	files, err := radio.RenderFNE(profile, network)
	if err != nil {
		t.Fatal(err)
	}
	for name, value := range files {
		if err = os.WriteFile(filepath.Join(state, name), []byte(value), 0600); err != nil {
			t.Fatal(err)
		}
	}
	logPath := filepath.Join(state, "fne.log")
	log, err := os.Create(logPath)
	if err != nil {
		t.Fatal(err)
	}
	defer log.Close()
	command := exec.Command(fneBinary, "-c", filepath.Join(state, "fne.yml"))
	command.Dir = state
	command.Stdout = log
	command.Stderr = log
	command.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	if err = command.Start(); err != nil {
		t.Fatal(err)
	}
	done := make(chan error, 1)
	go func() { done <- command.Wait() }()
	defer func() {
		_ = syscall.Kill(-command.Process.Pid, syscall.SIGTERM)
		select {
		case <-done:
		case <-time.After(3 * time.Second):
			_ = syscall.Kill(-command.Process.Pid, syscall.SIGKILL)
			<-done
		}
	}()
	deadline := time.Now().Add(10 * time.Second)
	for {
		text, _ := os.ReadFile(logPath)
		if strings.Contains(string(text), "FNE is up and running") {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("private FNE did not start", string(text))
		}
		time.Sleep(20 * time.Millisecond)
	}
	frames := make(chan Frame, 160)
	options := func(peer int, identity string) CodecOptions {
		return CodecOptions{Binary: codecBinary, StateDir: filepath.Join(state, identity), Identity: identity, MasterAddress: network.Address, MasterPort: network.Port, MasterPassword: network.Secret, PeerIDs: map[int]int{1001: peer}, RadioID: 10002, WACN: 1, SystemID: 1, Opaque: true, StartupTimeout: 15 * time.Second}
	}
	receiveOptions := options(network.ConsolePeerIDs[1001], "opaque-receive")
	receiveOptions.OnFrame = func(f Frame) { frames <- f }
	receiver, err := NewCodec(receiveOptions)
	if err != nil {
		t.Fatal(err)
	}
	defer receiver.Close()
	sendOptions := options(network.VoicePeerIDs["voice"], "opaque-send")
	sendOptions.RadioID = 10001 // Physical voice ingress uses the subscriber population.
	sender, err := NewCodec(sendOptions)
	if err != nil {
		t.Fatal(err)
	}
	defer sender.Close()
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	connectedAt := time.Now()
	if err = receiver.Start(ctx); err != nil {
		t.Fatal(err)
	}
	if err = sender.Start(ctx); err != nil {
		t.Fatal(err)
	}
	if elapsed := time.Since(connectedAt); elapsed > 3*time.Second {
		t.Fatalf("initial private FNE connections took %v; first attempts must not wait for the reconnect timer", elapsed)
	}
	trusted := nativeMultiAlgorithmCodec(t)
	expected := []Frame{}
	// These are explicit synthetic receiver-report/status values for carrier
	// fidelity, not RF observations. Actual native ciphertext and Header/ES
	// synchronization words are unchanged; HDU MFID stays separate from LC.
	prepare := func(f Frame) Frame {
		f.P25 = f.P25.Clone()
		f.P25.Quality.TotalErrors, f.P25.Quality.ScaledErrors = 3, 2
		f.P25.Quality.U4Errors, f.P25.Quality.U1Errors = 1, 1
		f.P25.Quality.Mute = f.VoiceIndex == 3
		f.P25.Quality.Lost = f.VoiceIndex == 4
		if f.P25.LSD != nil {
			f.P25.LSD.Words = [2]byte{0x12, 0x34}
			f.P25.LSD.Status = [2]uint8{1, 2}
		}
		if f.P25.ES != nil {
			f.P25.ES.Status = 5
		}
		if f.P25.Header != nil {
			f.P25.Header.MFID = 0x90
		}
		f.CapturedAt = time.Now()
		return f
	}
	end := func(last Frame) {
		t.Helper()
		last.End, last.PCM, last.Encoded, last.P25, last.MessageIndicator = true, nil, nil, nil, nil
		last.CapturedAt = time.Now()
		packet, err := EncodeCodecPacket(last)
		if err != nil || len(packet) != packetHeaderSize || packet[4] != 18 || !bytes.Equal(packet[48:57], make([]byte, 9)) {
			t.Fatal("explicit last-owner End is not the current empty op18 contract", err)
		}
		if err := sender.Write(ctx, last); err != nil {
			t.Fatal(err)
		}
		if err := sender.Drain(ctx, last.CallID, []int{1001}); err != nil {
			t.Fatal(err)
		}
	}
	ticker := time.NewTicker(FrameDuration)
	defer ticker.Stop()
	for _, algorithm := range []uint8{0x84, 0x81, 0xaa} {
		callID := fmt.Sprintf("opaque-integration-%d", algorithm)
		var last Frame
		for seq := uint64(1); seq <= 36; seq++ {
			<-ticker.C
			input := nativeToneFrame(callID, seq, true)
			input.SourceID, input.AlgorithmID, input.ServiceOptions = 10001, algorithm, 0x18
			if algorithm != 0x84 {
				input.ManufacturerID = 1
			}
			if algorithm == 0xaa {
				input.KeyID = 0
			}
			encoded, err := trusted.Encode(ctx, input)
			if err != nil {
				t.Fatal(err)
			}
			encoded = prepare(encoded)
			last = encoded
			expected = append(expected, encoded)
			if err = sender.Write(ctx, encoded); err != nil {
				t.Fatal(err)
			}
		}
		end(last)
	}
	// A new clear media owner follows encryption on the same source and keyless Site.
	var last Frame
	for seq := uint64(1); seq <= 18; seq++ {
		<-ticker.C
		input := nativeToneFrame("opaque-clear-integration", seq, false)
		input.SourceID, input.ServiceOptions = 10001, 0x18
		input.ManufacturerID = 1
		encoded, err := trusted.Encode(ctx, input)
		if err != nil {
			t.Fatal(err)
		}
		encoded = prepare(encoded)
		last = encoded
		expected = append(expected, encoded)
		if err = sender.Write(ctx, encoded); err != nil {
			t.Fatal(err)
		}
	}
	end(last)
	actual := []Frame{}
	ends := 0
	terminalByCall := map[string]int{}
	lastByCall := map[string]Frame{}
	timer := time.NewTimer(3 * time.Second)
	defer timer.Stop()
collect:
	for {
		select {
		case f := <-frames:
			if f.End {
				terminalByCall[f.CallID]++
				previous, known := lastByCall[f.CallID]
				if !known || terminalByCall[f.CallID] != 1 || f.Sequence <= previous.Sequence || f.SourceID != previous.SourceID || f.TalkgroupID != previous.TalkgroupID || f.ServiceOptions != previous.ServiceOptions || f.ManufacturerID != previous.ManufacturerID || len(f.PCM) != 0 || len(f.Encoded) != 0 || f.P25 != nil || len(f.MessageIndicator) != 0 {
					t.Fatal("native terminal duplicated, changed owner or fabricated media", f)
				}
				ends++
				if ends == 4 {
					break collect
				}
				continue
			}
			lastByCall[f.CallID] = f
			actual = append(actual, f)
		case <-timer.C:
			break collect
		}
	}
	if len(actual) != len(expected) || ends != 4 {
		receiveLog, _ := os.ReadFile(filepath.Join(receiver.runDir, "talkgroup-1001.log"))
		sendLog, _ := os.ReadFile(filepath.Join(sender.runDir, "talkgroup-1001.log"))
		fneLog, _ := os.ReadFile(logPath)
		t.Fatalf("opaque native transport got %d of %d frames and %d of 4 terminals\nreceiver: %s\nsender: %s\nfne: %s", len(actual), len(expected), ends, receiveLog, sendLog, fneLog)
	}
	for i, got := range actual {
		want := expected[i]
		if len(got.PCM) != 0 || got.SourceID != want.SourceID || got.TalkgroupID != want.TalkgroupID || got.ManufacturerID != want.ManufacturerID || got.Encrypted != want.Encrypted || got.AlgorithmID != want.AlgorithmID || got.KeyID != want.KeyID || got.VoiceIndex != want.VoiceIndex || len(got.MessageIndicator) != 0 || got.Emergency != want.Emergency || got.ServiceOptions != want.ServiceOptions || !reflect.DeepEqual(got.P25, want.P25) || !bytes.Equal(got.Encoded, want.Encoded) {
			t.Fatalf("opaque FNE changed ciphertext or metadata at %d: %+v want %+v", i, got, want)
		}
	}
	if len(terminalByCall) != 4 || len(lastByCall) != 4 {
		t.Fatal("new media IDs did not retain four distinct receive owners")
	}
	// These same live bridges just forwarded all valid frames and terminals.
	// A physical Voice peer still cannot borrow an enabled console identity.
	for seq := uint64(1); seq <= 18; seq++ {
		<-ticker.C
		input := nativeToneFrame("forbidden-console-on-voice-peer", seq, false)
		input.SourceID = 10002
		encoded, err := trusted.Encode(ctx, input)
		if err != nil {
			t.Fatal(err)
		}
		if err = sender.Write(ctx, encoded); err != nil {
			t.Fatal(err)
		}
		last = encoded
	}
	end(last)
	denied := time.NewTimer(500 * time.Millisecond)
	select {
	case frame := <-frames:
		denied.Stop()
		t.Fatalf("physical voice peer forwarded a console identity: source %d end %v", frame.SourceID, frame.End)
	case <-denied.C:
	}
	t.Log("console RID on physical Voice peer produced no forwarded frame or terminal; rejection checked after live subscriber forwarding")
	t.Logf("%d real clear/encrypted IMBE words, raw Header/quality/LSD/ES/service/LC-MFID and four unique terminals survived the keyless Site/FNE path; common network MI absent, no RF hardware", len(actual))
}

// AAAD-B B.4, PDF46/printed36, public three-key key bundle. Exact document
// citations and transcription are in the p25crypto test fixtures.
const publishedTDEAKey = "0123456789ABCDEF23456789ABCDEF01456789ABCDEF0123"

// Public test-only alternate: the B.4 key components in K3/K2/K1 order.
// Its expected ciphertext is calculated by Go TripleDES, not a published vector.
const alternateTDEAKey = "456789ABCDEF012323456789ABCDEF010123456789ABCDEF"

// TDEA placement and its normative oracle are AAAD-B Annex B.4 (PDF46-47,
// printed36-37), using A.4 plaintext (PDF42/printed32). The optional voice
// module supplies supporting DES/AES/ADP evidence only. The first LDU skips
// synchronization material, the second LDU skips 101 bytes, and each ninth
// voice word follows two additional sync bytes.
func independentP25Traffic(algorithm int, key, mi, plain []byte, index int) []byte {
	if algorithm == 0x84 {
		return independentP25AES(key, mi, plain, index)
	}
	stream := make([]byte, 512)
	offset := 19
	switch algorithm {
	case 0x81, 0x83:
		var block cipher.Block
		var err error
		if algorithm == 0x83 {
			block, err = des.NewTripleDESCipher(key)
		} else {
			block, err = des.NewCipher(key)
		}
		if err != nil {
			panic(err)
		}
		cipher.NewOFB(block, mi[:8]).XORKeyStream(stream, stream)
	case 0xaa:
		seed := append(append([]byte(nil), key...), mi[:8]...)
		c, err := rc4.NewCipher(seed)
		if err != nil {
			panic(err)
		}
		c.XORKeyStream(stream, stream)
		offset = 267
	default:
		panic("unsupported reference algorithm")
	}
	offset += (index % 9) * 11
	if index >= 9 {
		offset += 101
	}
	if index%9 == 8 {
		offset += 2
	}
	out := append([]byte(nil), plain...)
	for i := range out {
		out[i] ^= stream[offset+i]
	}
	return out
}
func nativeMultiAlgorithmCodec(t *testing.T) *PacketCodec {
	t.Helper()
	binaryPath := os.Getenv("TRUNKSYSTEM_PACKET_CODEC")
	if binaryPath == "" {
		t.Skip("set TRUNKSYSTEM_PACKET_CODEC to a reviewed native dvmbridge")
	}
	path := filepath.Join(t.TempDir(), "traffic-keys")
	var content strings.Builder
	for _, key := range []struct {
		algorithm int
		material  string
	}{{129, strings.Repeat("0123456789abcdef", 1)}, {131, publishedTDEAKey}, {132, strings.Repeat("0123456789abcdef", 4)}, {170, "0123456789"}} {
		zeroKey := key.material
		if key.algorithm == 131 {
			zeroKey = alternateTDEAKey
		}
		fmt.Fprintf(&content, "%d:7:%s\n%d:0:%s\n", key.algorithm, key.material, key.algorithm, zeroKey)
	}
	if err := os.WriteFile(path, []byte(content.String()), 0600); err != nil {
		t.Fatal(err)
	}
	codec, err := NewPacketCodec(PacketCodecOptions{Binary: binaryPath, KeyFile: path})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = codec.Close() })
	if err = codec.Start(context.Background()); err != nil {
		t.Fatal(err)
	}
	return codec
}
func TestNativePacketCodecAllTrafficAlgorithmsAgainstIndependentReferences(t *testing.T) {
	codec := nativeMultiAlgorithmCodec(t)
	tdeaKey, _ := hex.DecodeString(publishedTDEAKey)
	keys := map[int][]byte{131: tdeaKey, 129: {1, 0x23, 0x45, 0x67, 0x89, 0xab, 0xcd, 0xef}, 132: bytes.Repeat([]byte{1, 0x23, 0x45, 0x67, 0x89, 0xab, 0xcd, 0xef}, 4), 170: {1, 0x23, 0x45, 0x67, 0x89}}
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	for algorithm, key := range keys {
		t.Run(fmt.Sprintf("algorithm-%02x", algorithm), func(t *testing.T) {
			for index := 0; index < 18; index++ {
				f := packetFixture()
				f.CallID = fmt.Sprintf("reference-%d", algorithm)
				f.AlgorithmID = uint8(algorithm)
				f.Sequence = uint64(index + 1)
				f.VoiceIndex = uint8(index)
				f.P25 = testP25Metadata(f.VoiceIndex)
				got, err := codec.transform(ctx, 3, f)
				if err != nil {
					t.Fatal(err)
				}
				want := independentP25Traffic(algorithm, key, f.MessageIndicator, f.Encoded, index)
				if !bytes.Equal(got.Encoded, want) {
					t.Fatalf("algorithm %02x index %d differs from independent reference", algorithm, index)
				}
			}
		})
	}
	t.Run("tdea-zero-key-selects-distinct-loaded-key", func(t *testing.T) {
		key, _ := hex.DecodeString(alternateTDEAKey)
		for index := 0; index < 18; index++ {
			f := packetFixture()
			f.CallID = "reference-tdea-zero-key"
			f.AlgorithmID, f.KeyID = 0x83, 0
			f.Sequence, f.VoiceIndex = uint64(index+1), uint8(index)
			f.P25 = testP25Metadata(f.VoiceIndex)
			got, err := codec.transform(ctx, 3, f)
			if err != nil {
				t.Fatal(err)
			}
			want := independentP25Traffic(0x83, key, f.MessageIndicator, f.Encoded, index)
			if got.KeyID != 0 || !bytes.Equal(got.Encoded, want) {
				t.Fatalf("TDEA KID0 index %d did not select its distinct loaded key", index)
			}
		}
	})
	for _, algorithm := range []uint8{0, 0x82, 0x85, 0x89, 0x9f} {
		f := packetFixture()
		f.CallID = fmt.Sprintf("unsupported-%d", algorithm)
		f.AlgorithmID = algorithm
		if _, err := codec.transform(ctx, 3, f); err == nil {
			t.Fatalf("unimplemented algorithm %02x accepted", algorithm)
		}
	}
	for _, algorithm := range []uint8{129, 131} {
		f := packetFixture()
		f.CallID = fmt.Sprintf("missing-pair-%d", algorithm)
		f.AlgorithmID = algorithm
		f.KeyID = 8
		if _, err := codec.transform(ctx, 3, f); err == nil || !strings.Contains(err.Error(), "key is unavailable") {
			t.Fatalf("missing algorithm/KID pair %02x: %v", algorithm, err)
		}
	}
}
func TestNativePacketCodecEachAlgorithmRoundTripAndMIRotation(t *testing.T) {
	for _, algorithm := range []uint8{129, 131, 132, 170} {
		t.Run(fmt.Sprintf("algorithm-%02x", algorithm), func(t *testing.T) {
			encoder := nativeMultiAlgorithmCodec(t)
			decoder := nativeMultiAlgorithmCodec(t)
			referenceDecoder := nativePacketCodec(t, false)
			key := []byte{1, 0x23, 0x45, 0x67, 0x89}
			if algorithm == 129 {
				key = append(key, 0xab, 0xcd, 0xef)
			}
			if algorithm == 131 {
				key, _ = hex.DecodeString(publishedTDEAKey)
			}
			if algorithm == 132 {
				key = bytes.Repeat([]byte{1, 0x23, 0x45, 0x67, 0x89, 0xab, 0xcd, 0xef}, 4)
			}
			ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
			defer cancel()
			var firstMI []byte
			changed, nonzero := false, false
			for sequence := uint64(1); sequence <= 37; sequence++ {
				f := nativeToneFrame("secure-multi", sequence, true)
				f.AlgorithmID = algorithm
				encoded, err := encoder.Encode(ctx, f)
				if err != nil {
					t.Fatal(err)
				}
				if sequence == 1 {
					firstMI = append([]byte(nil), encoded.MessageIndicator...)
				}
				if sequence == 19 {
					changed = !bytes.Equal(firstMI, encoded.MessageIndicator)
				}
				if !encoded.Encrypted || encoded.AlgorithmID != algorithm || encoded.KeyID != 7 || encoded.VoiceIndex != uint8((sequence-1)%18) || len(encoded.PCM) != 0 {
					t.Fatal("encrypted metadata changed")
				}
				// Obtain real clear Header/ESS while retaining the independent IMBE oracle.
				clearInput := f
				clearInput.CallID = "reference-multi"
				clearInput.Encrypted, clearInput.AlgorithmID, clearInput.KeyID = false, 128, 0
				plain, err := referenceDecoder.Encode(ctx, clearInput)
				if err != nil {
					t.Fatal(err)
				}
				plain.P25 = plain.P25.Clone()
				plain.Encoded = independentP25Traffic(int(algorithm), key, encoded.MessageIndicator, encoded.Encoded, int(encoded.VoiceIndex))
				plain.MessageIndicator = nil
				if !plain.MediaValid() || !encoded.MediaValid() {
					t.Fatal("independent clear oracle or original encrypted frame is invalid")
				}
				reference, err := referenceDecoder.Decode(ctx, plain)
				if err != nil {
					t.Fatal(err)
				}
				decoded, err := decoder.Decode(ctx, encoded)
				if err != nil {
					t.Fatal(err)
				}
				if !bytes.Equal(decoded.PCM, reference.PCM) || decoded.Encrypted || len(decoded.Encoded) != 0 {
					t.Fatal("decrypted PCM differs from independently decrypted voice")
				}
				for _, value := range decoded.PCM {
					nonzero = nonzero || value != 0
				}
			}
			if !changed || !nonzero {
				t.Fatal("MI did not rotate or actual decoded audio absent")
			}
			switched := nativeToneFrame("secure-multi", 38, true)
			switched.AlgorithmID = 129
			if algorithm == 129 {
				switched.AlgorithmID = 132
			}
			if _, err := encoder.Encode(ctx, switched); err == nil {
				t.Fatal("active call changed algorithm while retaining same KID")
			}
			tail, err := encoder.Finish(ctx, "secure-multi")
			if err != nil || len(tail) != 8 {
				t.Fatalf("encrypted tail: %d %v", len(tail), err)
			}
			for _, frame := range tail {
				if frame.AlgorithmID != algorithm || !frame.Encrypted || len(frame.Encoded) != 11 || len(frame.PCM) != 0 {
					t.Fatal("tail changed algorithm or leaked PCM")
				}
			}
			if err = encoder.End(ctx, "secure-multi"); err != nil {
				t.Fatal(err)
			}
			restarted := nativeToneFrame("secure-multi", 1, true)
			restarted.AlgorithmID = algorithm
			if _, err = encoder.Encode(ctx, restarted); err != nil {
				t.Fatal("ended algorithm call retained sequence state", err)
			}
			if _, err = encoder.Encode(ctx, restarted); err == nil {
				t.Fatal("duplicate sequence accepted")
			}
		})
	}
}

// Public fixture pairs checked with the optional voice module at source commit
// 701d031b. These are independent recorded input/output data, not its GPL-3
// implementation. The production codec uses the existing DVM crypto routines.
func TestNativePacketCodecPublicModuleOracleVectors(t *testing.T) {
	binaryPath := os.Getenv("TRUNKSYSTEM_PACKET_CODEC")
	if binaryPath == "" {
		t.Skip("set reviewed native packet codec executable")
	}
	keyFile := filepath.Join(t.TempDir(), "keys")
	inventory := "129:7:465BA1A7F83D10C2\n132:7:D59DA7112B0D0AF44544C055F381F2D3900896D1C4456078E8ABCD33B1CBADE0\n170:7:DEADBEEF69\n"
	if err := os.WriteFile(keyFile, []byte(inventory), 0600); err != nil {
		t.Fatal(err)
	}
	codec, err := NewPacketCodec(PacketCodecOptions{Binary: binaryPath, KeyFile: keyFile})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = codec.Close() })
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	if err = codec.Start(ctx); err != nil {
		t.Fatal(err)
	}
	for _, vector := range []struct {
		algorithm     uint8
		mi            string
		index         uint8
		cipher, plain string
	}{
		{129, "AB8E0CCFB65CCCC500", 0, "2E68FAB6B700E5E55D4792", "A8131773BCB700032CDF21"},
		{129, "AB8E0CCFB65CCCC500", 1, "AB086418F6CA01B89792C8", "18700F0F39F400DE4CA162"},
		{129, "AB8E0CCFB65CCCC500", 2, "C93FEEF8854BC038630514", "181C5797A2F700AF7F5BF3"},
		{132, "E6E63447BF74FBFD00", 0, "338B693221091716C618F1", "A87827B224B3000386AD41"},
		{170, "A3FD5550D287CD8E00", 0, "7FAC5FE5EBA4CEFE923858", "040CFD7BFB7DF37B399E4C"},
		{170, "A3FD5550D287CD8E00", 1, "3CE11C9E802C6870546A03", "040CFD7BFB7DF37B2D9E44"},
	} {
		frame := packetFixture()
		frame.CallID = fmt.Sprintf("public-oracle-%d", vector.algorithm)
		frame.AlgorithmID = vector.algorithm
		frame.Sequence = uint64(vector.index) + 1
		frame.VoiceIndex = vector.index
		frame.P25 = testP25Metadata(frame.VoiceIndex)
		frame.MessageIndicator, _ = hex.DecodeString(vector.mi)
		frame.Encoded, _ = hex.DecodeString(vector.cipher)
		expected, _ := hex.DecodeString(vector.plain)
		got, err := codec.transform(ctx, 3, frame)
		if err != nil {
			t.Fatal(err)
		}
		if !bytes.Equal(got.Encoded, expected) {
			t.Fatalf("module oracle mismatch at algorithm %02x index %d", vector.algorithm, vector.index)
		}
	}
}

func TestNativePacketCodecEncryptedZeroKeyIDAllAlgorithms(t *testing.T) {
	codec := nativeMultiAlgorithmCodec(t)
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	for _, algorithm := range []uint8{129, 131, 132, 170} {
		input := nativeToneFrame(fmt.Sprintf("zero-key-%d", algorithm), 1, true)
		input.AlgorithmID = algorithm
		input.KeyID = 0
		encoded, err := codec.Encode(ctx, input)
		if err != nil || !encoded.Encrypted || encoded.KeyID != 0 || encoded.AlgorithmID != algorithm {
			t.Fatalf("encrypted zero-key encode %02x: %v", algorithm, err)
		}
		decoded, err := codec.Decode(ctx, encoded)
		if err != nil || len(decoded.PCM) != FrameBytes || decoded.Encrypted || len(decoded.Encoded) != 0 {
			t.Fatalf("encrypted zero-key decode %02x: %v", algorithm, err)
		}
	}
}

func TestNativePacketCodecStandardVoiceMFIDPreservation(t *testing.T) {
	codec := nativeMultiAlgorithmCodec(t)
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	for _, mfid := range []uint8{0, 1} {
		for _, algorithm := range []uint8{0x80, 0x81, 0x83, 0x84, 0xaa} {
			input := nativeToneFrame(fmt.Sprintf("mfid-%d-alg-%d", mfid, algorithm), 1, algorithm != 0x80)
			input.ManufacturerID = mfid
			input.AlgorithmID = algorithm
			encoded, err := codec.Encode(ctx, input)
			if err != nil || encoded.ManufacturerID != mfid {
				t.Fatalf("native encode changed MFID %d: %v", mfid, err)
			}
			decoded, err := codec.Decode(ctx, encoded)
			if err != nil || decoded.ManufacturerID != mfid {
				t.Fatalf("native decode changed MFID %d: %v", mfid, err)
			}
			mismatch := input
			mismatch.Sequence = 2
			mismatch.ManufacturerID ^= 1
			if _, err = codec.Encode(ctx, mismatch); err == nil {
				t.Fatal("active native call changed MFID")
			}
			end := encoded
			end.End = true
			end.Encoded = nil
			end.P25 = nil
			end.ManufacturerID ^= 1
			if _, err = codec.transform(ctx, 1, end); err == nil {
				t.Fatal("mismatched MFID ended the native call")
			}
			// Rejecting a mismatched End must preserve the actual call's sequence state.
			if _, err = codec.Decode(ctx, encoded); err == nil {
				t.Fatal("mismatched End discarded decoder state")
			}
			input.Sequence = 2
			if _, err = codec.Encode(ctx, input); err != nil {
				t.Fatal("MFID mismatch damaged the admitted call", err)
			}
			if err = codec.End(ctx, input.CallID); err != nil {
				t.Fatal(err)
			}
		}
	}
}

func TestNativePacketCodecEmergencyMetadataAndTail(t *testing.T) {
	codec := nativePacketCodec(t, true)
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	for _, secure := range []bool{false, true} {
		input := nativeToneFrame(fmt.Sprintf("emergency-%t", secure), 1, secure)
		input.Emergency = true
		encoded, err := codec.Encode(ctx, input)
		if err != nil || !encoded.Emergency {
			t.Fatal("native encode lost emergency", err)
		}
		decoded, err := codec.Decode(ctx, encoded)
		if err != nil || !decoded.Emergency {
			t.Fatal("native decode lost emergency", err)
		}
		tail, err := codec.Finish(ctx, input.CallID)
		if err != nil || len(tail) == 0 {
			t.Fatal("missing real codec tail", err)
		}
		for _, f := range tail {
			if !f.Emergency || f.Encrypted != secure || f.AlgorithmID != encoded.AlgorithmID || f.KeyID != encoded.KeyID {
				t.Fatal("tail changed owned metadata", f)
			}
		}
		if err = codec.End(ctx, input.CallID); err != nil {
			t.Fatal(err)
		}
	}
}
