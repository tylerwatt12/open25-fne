package p25rtp

import (
	"bytes"
	"encoding/hex"
	"fmt"
	"reflect"
	"sync"
	"testing"
)

// Manually derived from the original PDF figures by an independent reviewer,
// before using Marshal. See README.md for sources and synthetic-field limits.
const requestFixture = "80641234010203040000000204810000048b000008850000128000000e00c4040000001001002711040123456789abcdef018412340003e9293010c2123456789abcb4715990d5aea4"

func hexBytes(t testing.TB, s string) []byte {
	t.Helper()
	b, err := hex.DecodeString(s)
	if err != nil {
		t.Fatal(err)
	}
	return b
}

func requestPacket() Packet {
	return Packet{
		Role: GroupServing, Sequence: 0x1234, Timestamp: 0x01020304,
		Control: PacketTypeControl{Type: Request, ServiceOptions: 0xc4, TSN: 2},
		RF:      &RFControlWord{Source: SUID{WACN: 1, System: 1, Unit: 10001}, TransmitPriority: 4},
		Header:  &HeaderWord{MI: [9]byte{1, 0x23, 0x45, 0x67, 0x89, 0xab, 0xcd, 0xef, 1}, AlgorithmID: 0x84, KeyID: 0x1234, GroupID: 1001, NID: 0x2930, VoiceBlockBundling: 1},
		Voice: &FullRateVoice{Type: FR1, IMBE: [11]byte{0x12, 0x34, 0x56, 0x78, 0x9a, 0xbc, 0xb4, 0x71, 0x59, 0x90, 0xd5},
			Quality: Quality{TotalErrors: 5, ScaledErrors: 3, Mute: true, U4Errors: 1, U1Errors: 2, Superframe: 1}},
	}
}

func assertWire(t *testing.T, p Packet, expected []byte) {
	t.Helper()
	got, err := Marshal(p)
	if err != nil || !bytes.Equal(got, expected) {
		t.Fatalf("Marshal %x %v; want %x", got, err, expected)
	}
	parsed, err := Parse(expected)
	if err != nil || !reflect.DeepEqual(parsed, p) {
		t.Fatalf("Parse %#v %v; want %#v", parsed, err, p)
	}
}

func TestManualRequestHeaderVoiceFixture(t *testing.T) {
	expected := hexBytes(t, requestFixture)
	if len(expected) != 73 {
		t.Fatal("independent fixture size")
	}
	// U0..7 = 123,456,789,ABC,5A3,456,321,55 with bit widths
	// 12,12,12,12,11,11,11,7. Quality AE A4 = Et5/Er3/M1/L0/E4=1/E1=2/SF1.
	assertWire(t, requestPacket(), expected)
}

func TestCorpusControlFixtureCandidates(t *testing.T) {
	for _, tc := range []struct {
		name, wire string
		packet     Packet
	}{
		{"request", "80641234000000000000000202810000048b000008000404000000100100271104", Packet{Role: GroupServing, Sequence: 0x1234, Control: PacketTypeControl{Type: Request, ServiceOptions: 4, TSN: 2}, RF: &RFControlWord{Source: SUID{WACN: 1, System: 1, Unit: 10001}, TransmitPriority: 4}}},
		{"grant_echo_even_tsn", "806412350000000000000001818100000401040400", Packet{Role: GroupHome, Sequence: 0x1235, Control: PacketTypeControl{Type: Grant, ServiceOptions: 4, TSN: 2}}},
		{"end", "806412360000000000000002018100000403040400", Packet{Role: GroupServing, Sequence: 0x1236, Control: PacketTypeControl{Type: End, ServiceOptions: 4, TSN: 2}}},
		{"heartbeat", "80641237000000000000000201810000040904000a", Packet{Role: GroupServing, Sequence: 0x1237, Control: PacketTypeControl{Type: Heartbeat, ServiceOptions: 4, Interval: 10}}},
	} {
		t.Run(tc.name, func(t *testing.T) { assertWire(t, tc.packet, hexBytes(t, tc.wire)) })
	}
}

func TestManualLSDAndEncryptionSyncFixtures(t *testing.T) {
	for _, tc := range []struct {
		name, header, block string
		typ                 FrameType
		lsd                 *LowSpeedData
		es                  *EncryptionSync
	}{
		{"lsd", "80000011", "6a123456789abcb4715990d5aea4a55a06", FR9, &LowSpeedData{Words: [2]byte{0xa5, 0x5a}, Status: [2]uint8{1, 2}}, nil},
		{"encryption_sync", "80000012", "6d123456789abcb4715990d5aea40420c43b", FR12, nil, &EncryptionSync{Words: [4]uint8{1, 2, 3, 4}, Status: 59}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			// ES 1,2,3,4 packs as 04 20 C4. ST0..3=2,1,0,2 gives
			// 2+3+0+54=59 (3B), independently of any MI or cipher.
			want := hexBytes(t, "8064fffeffffffff0000000283810000048b000008"+tc.header+"02c404000000100100271104"+tc.block)
			p := requestPacket()
			p.Sequence, p.Timestamp, p.Control.Type, p.Header = 0xfffe, 0xffffffff, Progress, nil
			p.Voice.Type, p.Voice.LSD, p.Voice.ES = tc.typ, tc.lsd, tc.es
			assertWire(t, p, want)
		})
	}
}

func TestManualConsoleStartFixture(t *testing.T) {
	// Figure 50: three 56-bit SUIDs each followed by its separate octet.
	want := hexBytes(t, "80641357000000000000000182810000048f00001804840600abcde1234567891312345678abcdef0000000000000000f0")
	p := Packet{Role: GroupHome, Sequence: 0x1357, Control: PacketTypeControl{Type: Start, ServiceOptions: 0x84, TSN: 3}, Console: &ConsoleControlWord{
		Console: SUID{WACN: 0xabcde, System: 0x123, Unit: 0x456789}, TransmitPriority: 0x13,
		PhysicalConsole: SUID{WACN: 0x12345, System: 0x678, Unit: 0xabcdef}, ConsoleTXPriority: 0xf0,
	}}
	assertWire(t, p, want)
}

func TestAllFullRateFrameTypesAndQualityBounds(t *testing.T) {
	// Literal types/lengths from Table38/Figures44..46, not voiceLength.
	for _, tc := range []struct{ typ, size uint8 }{{0xc2, 14}, {0xc3, 14}, {0xc4, 14}, {0xc5, 14}, {0xc6, 14}, {0xc7, 14}, {0xc8, 14}, {0xc9, 14}, {0x6a, 17}, {0xcb, 14}, {0xcc, 14}, {0x6d, 18}, {0x6e, 18}, {0x6f, 18}, {0x70, 18}, {0x71, 18}, {0x72, 18}, {0x73, 17}} {
		t.Run(fmt.Sprintf("%02x", tc.typ), func(t *testing.T) {
			p := Packet{Role: GroupServing, Sequence: 0xffff, Timestamp: 0xfffffff0, Control: PacketTypeControl{Type: Progress, TSN: 127, ServiceOptions: 0xff, Mute: true, Losing: true}, RF: &RFControlWord{Source: SUID{WACN: 0xfffff, System: 0xfff, Unit: 0xffffff}, TransmitPriority: 0x2f}, Voice: &FullRateVoice{Type: FrameType(tc.typ), Quality: Quality{TotalErrors: 7, ScaledErrors: 7, Mute: true, Lost: true, U4Errors: 1, U1Errors: 7, Superframe: 3}}}
			if tc.size == 17 {
				p.Voice.LSD = &LowSpeedData{Status: [2]uint8{3, 3}}
			}
			if tc.size == 18 {
				p.Voice.ES = &EncryptionSync{Words: [4]uint8{63, 63, 63, 63}, Status: 80}
			}
			got, err := Marshal(p)
			if err != nil {
				t.Fatal(err)
			}
			if got[24] != tc.size || len(got) != 37+int(tc.size) || got[49] != 0xff || got[50] != 0xfc {
				t.Fatalf("wrong length or max quality: %x", got)
			}
			parsed, err := Parse(got)
			if err != nil || !reflect.DeepEqual(parsed, p) {
				t.Fatalf("Parse %#v %v; want %#v", parsed, err, p)
			}
		})
	}
}

func TestPacketTypesSignalRolesAndHeartbeatTSN(t *testing.T) {
	for typ := Request; typ <= HeartbeatQuery; typ++ {
		t.Run(fmt.Sprint(typ), func(t *testing.T) {
			p := Packet{Role: GroupHome, Control: PacketTypeControl{Type: typ, TSN: 2, ServiceOptions: 4}}
			if typ == Request {
				p.Role = GroupServing
			}
			if typ == Request || typ == Start || typ == Progress {
				p.RF = &RFControlWord{}
			}
			if typ == HeartbeatQuery {
				p.Control.TSN = 0
			}
			got, err := Marshal(p)
			if err != nil {
				t.Fatal(err)
			}
			expectedSignal := typ == 1 || typ == 2 || typ == 4 || typ == 5 || typ == 6
			if (got[12]&0x80 != 0) != expectedSignal {
				t.Fatalf("wrong S-bit: %x", got)
			}
			parsed, err := Parse(got)
			if err != nil || !reflect.DeepEqual(parsed, p) {
				t.Fatalf("Parse %#v %v", parsed, err)
			}
		})
	}
}

func TestMaximumProfilePacket(t *testing.T) {
	p := requestPacket()
	p.RF, p.Console = nil, &ConsoleControlWord{ConsoleTXPriority: 0xff}
	p.Voice.Type, p.Voice.ES = FR12, &EncryptionSync{}
	b, err := Marshal(p)
	if err != nil || len(b) != 93 {
		t.Fatalf("max packet length %d %v", len(b), err)
	}
	got, err := Parse(b)
	if err != nil || !reflect.DeepEqual(got, p) {
		t.Fatalf("Parse %#v %v", got, err)
	}
}

func TestIndependentOwnershipAndStatelessSequence(t *testing.T) {
	b := hexBytes(t, requestFixture)
	p, err := Parse(b)
	if err != nil {
		t.Fatal(err)
	}
	clear(b)
	if !reflect.DeepEqual(p, requestPacket()) {
		t.Fatal("parsed fields alias input")
	}
	// No state: session owner handles duplicate/reordered packets and wrap.
	var wg sync.WaitGroup
	for range 16 {
		wg.Go(func() {
			for _, seq := range []uint16{0xffff, 0, 0, 1} {
				q := p
				q.Sequence, q.Timestamp = seq, uint32(seq)*160
				b, err := Marshal(q)
				if err != nil {
					t.Error(err)
					return
				}
				got, err := Parse(b)
				if err != nil || !reflect.DeepEqual(got, q) {
					t.Errorf("Parse %#v %v", got, err)
				}
			}
		})
	}
	wg.Wait()
}
