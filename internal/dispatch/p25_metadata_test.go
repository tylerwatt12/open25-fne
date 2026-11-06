package dispatch

import (
	"bytes"
	"context"
	"encoding/binary"
	"encoding/hex"
	"errors"
	"reflect"
	"testing"
	"open25.local/open25-fne/internal/p25rtp"
)

// Synthetic metadata for software fixtures, never a receiver observation.
func testP25Metadata(index uint8) *P25VoiceMetadata {
	types := [...]p25rtp.FrameType{p25rtp.FR1, p25rtp.FR2, p25rtp.FR3, p25rtp.FR4, p25rtp.FR5, p25rtp.FR6, p25rtp.FR7, p25rtp.FR8, p25rtp.FR9, p25rtp.FR10, p25rtp.FR11, p25rtp.FR12, p25rtp.FR13, p25rtp.FR14, p25rtp.FR15, p25rtp.FR16, p25rtp.FR17, p25rtp.FR18}
	m := &P25VoiceMetadata{Type: types[index]}
	if index == 8 || index == 17 {
		m.LSD = &p25rtp.LowSpeedData{}
	}
	if index >= 11 && index <= 16 {
		m.ES = &p25rtp.EncryptionSync{}
	}
	return m
}

func TestFullRateOpaqueBodyMatchesIndependentLiteral(t *testing.T) {
	f := packetFixture()
	f.KeyID = 0x1234
	f.MessageIndicator = []byte{1, 0x23, 0x45, 0x67, 0x89, 0xab, 0xcd, 0xef, 1}
	f.Encoded, _ = hex.DecodeString("123456789abcb4715990d5")
	f.P25 = &P25VoiceMetadata{Type: p25rtp.FR1, Quality: p25rtp.Quality{TotalErrors: 5, ScaledErrors: 3, Mute: true, U4Errors: 1, U1Errors: 2, Superframe: 1}, Header: &p25rtp.HeaderWord{AlgorithmID: 0x84, KeyID: 0x1234, GroupID: 1001, NID: 0x2930, VoiceBlockBundling: 1}}
	copy(f.P25.Header.MI[:], f.MessageIndicator)
	b, err := EncodeOpaqueCodecPacket(f)
	if err != nil {
		t.Fatal(err)
	}
	want, _ := hex.DecodeString("0123456789abcdef018412340003e9293010c2123456789abcb4715990d5aea4")
	if b[4] != 18 || binary.BigEndian.Uint16(b[10:12]) != 2 || !bytes.Equal(b[64:], want) {
		t.Fatalf("wrong strict current body: %x", b)
	}
	got, err := DecodeOpaqueCodecPacket(b)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(got.Encoded, f.Encoded) || !reflect.DeepEqual(got.P25, f.P25) {
		t.Fatalf("metadata changed: %#v", got.P25)
	}
	clear(b)
	if !bytes.Equal(got.Encoded, f.Encoded) || !reflect.DeepEqual(got.P25, f.P25) {
		t.Fatal("decoded metadata aliases packet")
	}
}

func TestFullRateRawSpecialFieldsAndAbsentHeaderPreserved(t *testing.T) {
	for _, index := range []uint8{8, 11, 17} {
		t.Run(string(rune('A'+index)), func(t *testing.T) {
			f := packetFixture()
			f.VoiceIndex = index
			f.P25 = testP25Metadata(index)
			if f.P25.LSD != nil {
				f.P25.LSD = &p25rtp.LowSpeedData{Words: [2]byte{0xa5, 0x5a}, Status: [2]uint8{1, 3}}
			}
			if f.P25.ES != nil {
				f.P25.ES = &p25rtp.EncryptionSync{Words: [4]uint8{1, 2, 3, 4}, Status: 59}
			}
			b, err := EncodeOpaqueCodecPacket(f)
			if err != nil {
				t.Fatal(err)
			}
			got, err := DecodeOpaqueCodecPacket(b)
			if err != nil || !reflect.DeepEqual(got.P25, f.P25) {
				t.Fatalf("raw fields changed: %#v %v", got.P25, err)
			}
			if got.P25.Header != nil || binary.BigEndian.Uint16(b[10:12]) != 0 {
				t.Fatal("invented an observed header")
			}
		})
	}
}

func TestFullRateMissingMetadataAndOldWireFailClosed(t *testing.T) {
	f := packetFixture()
	f.P25 = nil
	if _, err := EncodeOpaqueCodecPacket(f); !errors.Is(err, ErrP25MetadataNotSupported) {
		t.Fatalf("missing metadata: %v", err)
	}
	if _, err := marshalPacketFrame(2, f); !errors.Is(err, ErrP25MetadataNotSupported) {
		t.Fatalf("decode boundary: %v", err)
	}
	if _, err := marshalPacketFrame(3, f); !errors.Is(err, ErrP25MetadataNotSupported) {
		t.Fatalf("crypt boundary: %v", err)
	}
	f.P25 = testP25Metadata(0)
	b, err := EncodeOpaqueCodecPacket(f)
	if err != nil {
		t.Fatal(err)
	}
	b[4] = 16
	if _, err = DecodeOpaqueCodecPacket(b); err == nil {
		t.Fatal("accepted old opaque operation")
	}
	b[4] = 18
	b = append(b[:64], f.Encoded...)
	binary.BigEndian.PutUint16(b[58:60], 11)
	if _, err = DecodeOpaqueCodecPacket(b); !errors.Is(err, ErrP25MetadataNotSupported) {
		t.Fatalf("accepted metadata-less old body: %v", err)
	}
	f.End, f.Encoded, f.P25 = true, nil, nil
	if _, err = marshalPacketFrame(16, f); err == nil {
		t.Fatal("old opaque operation accepted for End")
	}
}

func TestFullRateTransitNeedsRawMetadataNotCommonMI(t *testing.T) {
	f := packetFixture()
	f.MessageIndicator = nil
	if !f.MediaValid() {
		t.Fatal("raw encrypted transit required a fabricated common MI")
	}
	for _, op := range []byte{2, opaqueFullRateOperation} {
		wire, err := marshalPacketFrame(op, f)
		if err != nil {
			t.Fatal(err)
		}
		if !bytes.Equal(wire[48:57], make([]byte, 9)) {
			t.Fatal("common MI leaked into raw transit")
		}
	}
	f.P25 = nil
	if f.MediaValid() {
		t.Fatal("metadata-less encoded transit accepted")
	}
}

func TestFullRateCodecResponseRetainsIndexAndUnsupportedStatus(t *testing.T) {
	f := packetFixture()
	b, err := marshalPacketFrame(3, f)
	if err != nil {
		t.Fatal(err)
	}
	b[4] = 0x83
	if _, err = unmarshalPacketFrame(b, f, 3); err != nil {
		t.Fatal(err)
	}
	b[7], b[64] = 1, byte(p25rtp.FR2)
	if _, err = unmarshalPacketFrame(b, f, 3); err == nil {
		t.Fatal("native response changed both FT and voice index")
	}
	b[5] = 6
	if _, err = unmarshalPacketFrame(b, f, 3); !errors.Is(err, ErrP25MetadataNotSupported) {
		t.Fatalf("unsupported native metadata: %v", err)
	}
}

func TestFullRateHeaderFlagsAndIndexBounds(t *testing.T) {
	f := packetFixture()
	b, err := EncodeOpaqueCodecPacket(f)
	if err != nil {
		t.Fatal(err)
	}
	for name, change := range map[string]func([]byte){
		"type_index":       func(b []byte) { b[7] = 1 },
		"missing_header":   func(b []byte) { binary.BigEndian.PutUint16(b[10:12], 2) },
		"reserved_flag":    func(b []byte) { binary.BigEndian.PutUint16(b[10:12], 4) },
		"header_end":       func(b []byte) { binary.BigEndian.PutUint16(b[10:12], 3) },
		"reserved_octet":   func(b []byte) { b[61] = 1 },
		"reserved_quality": func(b []byte) { b[77] |= 1 },
	} {
		t.Run(name, func(t *testing.T) {
			bad := append([]byte{}, b...)
			change(bad)
			if _, err := DecodeOpaqueCodecPacket(bad); err == nil {
				t.Fatal("accepted malformed FullRate metadata")
			}
		})
	}
	f.End = true
	f.Encoded = nil
	if _, err = EncodeOpaqueCodecPacket(f); err == nil {
		t.Fatal("End accepted voice metadata")
	}
	f.P25 = nil
	end, err := EncodeOpaqueCodecPacket(f)
	if err != nil {
		t.Fatal(err)
	}
	if len(end) != 64 || binary.BigEndian.Uint16(end[10:12]) != 1 {
		t.Fatal("End is not empty/current")
	}
}

func TestFullRateMetadataDeepCopyAndCodecTailOwnership(t *testing.T) {
	f := packetFixture()
	f.VoiceIndex = 11
	f.P25 = testP25Metadata(11)
	f.P25.Header = &p25rtp.HeaderWord{VoiceBlockBundling: 1}
	f.P25.LSD = &p25rtp.LowSpeedData{Words: [2]byte{1, 2}}
	clone := f.P25.Clone()
	codec := &PacketCodec{calls: map[string]map[[2]int]packetCodecTarget{}}
	codec.remember(f, true)
	f.P25.ES.Words[0] = 63
	f.P25.LSD.Words[0] = 99
	f.P25.Header.NID = 0xffff
	f.P25.Quality.TotalErrors = 7
	if clone.ES.Words[0] != 0 || clone.LSD.Words[0] != 1 || clone.Header.NID != 0 || clone.Quality.TotalErrors != 0 {
		t.Fatal("metadata clone aliases source")
	}
	stored := codec.calls[f.CallID][[2]int{f.SourceID, f.TalkgroupID}]
	if !reflect.DeepEqual(stored.frame.P25, clone) || !reflect.DeepEqual(stored.encoderTail.P25, clone) {
		t.Fatal("codec tail retains mutable metadata")
	}
	c := &Codec{ctx: context.Background(), frames: make(chan codecReceiveUpdate, 1)}
	c.queueFrame(f)
	queued := <-c.frames
	f.P25.ES.Status = 80
	if queued.frame.P25.ES.Status != 0 {
		t.Fatal("queued metadata aliases source")
	}
}

func TestFullRateActualHeaderMetadataAndOwnerCoherence(t *testing.T) {
	for _, tc := range []struct {
		name      string
		clear     bool
		mutate    func(*p25rtp.HeaderWord)
		wantError bool
	}{
		{name: "encrypted_distinct_HDU_and_LC_MFID"},
		{name: "clear_preserves_raw_HDU_MFID_KID_MI", clear: true},
		{name: "clear_group_mismatch", clear: true, mutate: func(h *p25rtp.HeaderWord) { h.GroupID++ }, wantError: true},
		{name: "clear_algorithm_mismatch", clear: true, mutate: func(h *p25rtp.HeaderWord) { h.AlgorithmID = 0x84 }, wantError: true},
		{name: "encrypted_key_mismatch", mutate: func(h *p25rtp.HeaderWord) { h.KeyID++ }, wantError: true},
		{name: "encrypted_null_effective_MI", mutate: func(h *p25rtp.HeaderWord) { h.MI = [9]byte{0, 0, 0, 0, 0, 0, 0, 0, 1} }, wantError: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f := packetFixture()
			f.ManufacturerID = 0
			h := p25rtp.HeaderWord{MFID: 0xa4, AlgorithmID: f.AlgorithmID, KeyID: f.KeyID, GroupID: uint16(f.TalkgroupID), NID: 0x2930, VoiceBlockBundling: 1}
			copy(h.MI[:], f.MessageIndicator)
			if tc.clear {
				f.Encrypted, f.AlgorithmID, f.KeyID, f.MessageIndicator = false, 0x80, 0, nil
				f.ServiceOptions &^= 0x40
				h.AlgorithmID, h.KeyID = 0x80, 0x1234
			}
			if tc.mutate != nil {
				tc.mutate(&h)
			}
			f.P25.Header = &h
			wire, err := EncodeOpaqueCodecPacket(f)
			if tc.wantError {
				if err == nil {
					t.Fatal("accepted incoherent media owner")
				}
				return
			}
			if err != nil {
				t.Fatal(err)
			}
			got, err := DecodeOpaqueCodecPacket(wire)
			if err != nil {
				t.Fatal(err)
			}
			if got.ManufacturerID != 0 || got.P25.Header == nil || *got.P25.Header != h || got.KeyID != f.KeyID || len(got.MessageIndicator) != 0 {
				t.Fatal("actual HDU fields were conflated with normalized common LC/crypto context")
			}
		})
	}
}

func TestExplicitGroupAndEmergencyTranslationPreservesSourceFields(t *testing.T) {
	source := packetFixture()
	source.P25.Header = &p25rtp.HeaderWord{AlgorithmID: source.AlgorithmID, KeyID: source.KeyID, MFID: 0x90, GroupID: 1001, NID: 0x2930, VoiceBlockBundling: 1}
	copy(source.P25.Header.MI[:], source.MessageIndicator)
	translated := source.WithGroup(1002).WithEmergency(true)
	if translated.P25.Header.GroupID != 1002 || source.P25.Header.GroupID != 1001 || translated.P25.Header == source.P25.Header {
		t.Fatal("group translation changed source")
	}
	if translated.ServiceOptions != 0xC4 || source.ServiceOptions != 0x44 || !translated.Emergency || source.Emergency {
		t.Fatal("service flags diverged")
	}
	expected := *source.P25.Header
	expected.GroupID = 1002
	if *translated.P25.Header != expected || !bytes.Equal(translated.Encoded, source.Encoded) || !reflect.DeepEqual(translated.P25.Quality, source.P25.Quality) {
		t.Fatal("translation altered source evidence")
	}
	if !translated.MediaValid() {
		t.Fatal("translated current media invalid")
	}
}
