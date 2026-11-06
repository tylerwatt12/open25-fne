package p25rtp

import (
	"bytes"
	"fmt"
	"reflect"
	"testing"
)

func TestRejectMalformedPackets(t *testing.T) {
	mutations := map[string]func([]byte) []byte{
		"rtp_version":          func(b []byte) []byte { b[0] = 0x40; return b },
		"rtp_padding":          func(b []byte) []byte { b[0] |= 0x20; return b },
		"rtp_extension":        func(b []byte) []byte { b[0] |= 0x10; return b },
		"rtp_csrc":             func(b []byte) []byte { b[0] |= 1; return b },
		"rtp_marker":           func(b []byte) []byte { b[1] |= 0x80; return b },
		"private_dvm_pt86":     func(b []byte) []byte { b[1] = 86; return b },
		"half_rate_pt101":      func(b []byte) []byte { b[1] = 101; return b },
		"native_pt102":         func(b []byte) []byte { b[1] = 102; return b },
		"unknown_role":         func(b []byte) []byte { b[11] = 16; return b },
		"request_wrong_role":   func(b []byte) []byte { b[11] = 1; return b },
		"compact":              func(b []byte) []byte { b[12] |= 0x40; return b },
		"signal":               func(b []byte) []byte { b[12] |= 0x80; return b },
		"no_blocks":            func(b []byte) []byte { b[12] = 0; return b },
		"too_many_blocks":      func(b []byte) []byte { b[12] = 63; return b },
		"missing_profile_bit":  func(b []byte) []byte { b[13] &= 0x7f; return b },
		"packet_not_first":     func(b []byte) []byte { b[13] = 0x85; return b },
		"packet_offset":        func(b []byte) []byte { b[15] = 4; return b },
		"length_overrun":       func(b []byte) []byte { b[15] = 3; b[16] = 0xff; return b },
		"packet_length":        func(b []byte) []byte { b[16] = 3; return b },
		"rf_offset":            func(b []byte) []byte { b[19] = 4; return b },
		"rf_length":            func(b []byte) []byte { b[20] = 7; return b },
		"header_offset":        func(b []byte) []byte { b[23] = 4; return b },
		"header_length":        func(b []byte) []byte { b[24] = 17; return b },
		"voice_offset":         func(b []byte) []byte { b[27] = 4; return b },
		"voice_length":         func(b []byte) []byte { b[28] = 13; return b },
		"reserved_block":       func(b []byte) []byte { b[25] = 0x82; return b },
		"alias_block":          func(b []byte) []byte { b[25] = 0xa4; return b },
		"vendor_block":         func(b []byte) []byte { b[25] = 0xff; return b },
		"vocoder_mode_block":   func(b []byte) []byte { b[25] = 0xa3; return b },
		"reserved_packet_type": func(b []byte) []byte { b[29] = 11; return b },
		"zero_voice_tsn":       func(b []byte) []byte { b[31] = 0; return b },
		"priority_type":        func(b []byte) []byte { b[40] = 0x30; return b },
		"header_reserved":      func(b []byte) []byte { b[58] |= 1; return b },
		"header_sf":            func(b []byte) []byte { b[58] |= 0x40; return b },
		"header_vbb0":          func(b []byte) []byte { b[58] = 0; return b },
		"header_vbb2":          func(b []byte) []byte { b[58] = 0x20; return b },
		"header_vbb3":          func(b []byte) []byte { b[58] = 0x30; return b },
		"unknown_frame_type":   func(b []byte) []byte { b[59] = 0xca; return b },
		"missing_lsd":          func(b []byte) []byte { b[59] = 0x6a; return b },
		"missing_es":           func(b []byte) []byte { b[59] = 0x6d; return b },
		"quality_reserved":     func(b []byte) []byte { b[72] |= 1; return b },
		"trailing_data":        func(b []byte) []byte { return append(b, 0) },
		"duplicate_header":     func(b []byte) []byte { b[25] = 0x85; return b },
		"header_after_voice": func(b []byte) []byte {
			copy(b[21:29], []byte{0x80, 0, 0, 14, 0x85, 0, 0, 18})
			return append(append(b[:41:41], b[59:73]...), b[41:59]...)
		},
		"duplicate_voice": func(b []byte) []byte {
			b[21], b[24] = 0x80, 14
			return append(append(b[:41:41], b[59:73]...), b[59:73]...)
		},
		"duplicate_packet": func(b []byte) []byte {
			b[17], b[20] = 0x81, 4
			return append(append(b[:33:33], b[29:33]...), b[41:]...)
		},
		"both_source_types": func(b []byte) []byte { b[21] = 0x8f; return b },
	}
	for name, mutate := range mutations {
		t.Run(name, func(t *testing.T) {
			b := mutate(hexBytes(t, requestFixture))
			if p, err := Parse(b); err == nil || !reflect.DeepEqual(p, Packet{}) {
				t.Fatalf("accepted malformed input: %x, %#v, %v", b, p, err)
			}
		})
	}
	for n := 0; n < 73; n++ {
		t.Run(fmt.Sprintf("truncated_%d", n), func(t *testing.T) {
			if _, err := Parse(hexBytes(t, requestFixture)[:n]); err == nil {
				t.Fatal("accepted truncation")
			}
		})
	}
	if _, err := Parse(make([]byte, 94)); err == nil {
		t.Fatal("accepted oversized profile packet")
	}
}

func TestRejectFrameSpecificReservedAndStatusValues(t *testing.T) {
	for _, tc := range []struct{ name, block string }{
		{"lsd_reserved", "6a123456789abcb4715990d5aea4a55a16"},
		{"es_reserved", "6d123456789abcb4715990d5aea40420c4bb"},
		{"es_status_over80", "6d123456789abcb4715990d5aea40420c451"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			b := hexBytes(t, "80640001000000000000000283810000048b00000880000000"+"020404000000100100271104"+tc.block)
			b[24] = byte(len(hexBytes(t, tc.block)))
			if _, err := Parse(b); err == nil {
				t.Fatalf("accepted %x", b)
			}
		})
	}
}

func TestRejectInvalidTypedFields(t *testing.T) {
	for name, change := range map[string]func(*Packet){
		"role":               func(p *Packet) { p.Role = 0 },
		"type":               func(p *Packet) { p.Control.Type = 11 },
		"tsn_overflow":       func(p *Packet) { p.Control.TSN = 128 },
		"voice_tsn0":         func(p *Packet) { p.Control.TSN = 0 },
		"request_home":       func(p *Packet) { p.Role = GroupHome },
		"missing_source":     func(p *Packet) { p.RF = nil },
		"both_sources":       func(p *Packet) { p.Console = &ConsoleControlWord{} },
		"wacn":               func(p *Packet) { p.RF.Source.WACN = 0x100000 },
		"system":             func(p *Packet) { p.RF.Source.System = 0x1000 },
		"unit":               func(p *Packet) { p.RF.Source.Unit = 0x1000000 },
		"priority_type":      func(p *Packet) { p.RF.TransmitPriority = 0x30 },
		"header_sf":          func(p *Packet) { p.Header.Superframe = 1 },
		"header_vbb0":        func(p *Packet) { p.Header.VoiceBlockBundling = 0 },
		"header_vbb2":        func(p *Packet) { p.Header.VoiceBlockBundling = 2 },
		"header_vbb3":        func(p *Packet) { p.Header.VoiceBlockBundling = 3 },
		"frame_type":         func(p *Packet) { p.Voice.Type = 0xca },
		"Et":                 func(p *Packet) { p.Voice.Quality.TotalErrors = 8 },
		"Er":                 func(p *Packet) { p.Voice.Quality.ScaledErrors = 8 },
		"E4":                 func(p *Packet) { p.Voice.Quality.U4Errors = 2 },
		"E1":                 func(p *Packet) { p.Voice.Quality.U1Errors = 8 },
		"SF":                 func(p *Packet) { p.Voice.Quality.Superframe = 4 },
		"extra_lsd":          func(p *Packet) { p.Voice.LSD = &LowSpeedData{} },
		"extra_es":           func(p *Packet) { p.Voice.ES = &EncryptionSync{} },
		"missing_lsd":        func(p *Packet) { p.Voice.Type = FR9 },
		"missing_es":         func(p *Packet) { p.Voice.Type = FR12 },
		"lsd_status":         func(p *Packet) { p.Voice.Type = FR9; p.Voice.LSD = &LowSpeedData{Status: [2]uint8{4, 0}} },
		"es_symbol":          func(p *Packet) { p.Voice.Type = FR12; p.Voice.ES = &EncryptionSync{Words: [4]uint8{64, 0, 0, 0}} },
		"es_status":          func(p *Packet) { p.Voice.Type = FR12; p.Voice.ES = &EncryptionSync{Status: 81} },
		"console_descriptor": func(p *Packet) { p.RF = nil; p.Console = &ConsoleControlWord{TransmissionDescriptor: 1} },
		"start_with_voice":   func(p *Packet) { p.Role = GroupHome; p.Control.Type = Start },
	} {
		t.Run(name, func(t *testing.T) {
			p := requestPacket()
			change(&p)
			if b, err := Marshal(p); err == nil || b != nil {
				t.Fatalf("accepted %#v => %x, %v", p, b, err)
			}
		})
	}
	for _, p := range []Packet{
		{Role: GroupServing, Control: PacketTypeControl{Type: HeartbeatQuery, TSN: 2}},
		{Role: GroupServing, Control: PacketTypeControl{Type: Grant, TSN: 2}},
		{Role: GroupServing, Control: PacketTypeControl{Type: End, TSN: 2}, RF: &RFControlWord{}},
	} {
		if _, err := Marshal(p); err == nil {
			t.Fatalf("accepted %#v", p)
		}
	}
}

func FuzzParse(f *testing.F) {
	f.Add(hexBytes(f, requestFixture))
	f.Add(hexBytes(f, "80641237000000000000000201810000040904000a"))
	f.Add([]byte{})
	f.Fuzz(func(t *testing.T, b []byte) {
		p, err := Parse(b)
		if err != nil {
			return
		}
		out, err := Marshal(p)
		if err != nil || !bytes.Equal(out, b) {
			t.Fatalf("accepted packet not lossless: %x => %x, %v", b, out, err)
		}
	})
}
