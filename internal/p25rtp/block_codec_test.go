package p25rtp

import (
	"bytes"
	"reflect"
	"testing"
)

func TestStandaloneVoiceAndHeaderCodecsUseIndependentLiterals(t *testing.T) {
	header := hexBytes(t, "0123456789abcdef018412340003e9293010")
	h, err := ParseHeaderWord(header)
	if err != nil {
		t.Fatal(err)
	}
	b, err := MarshalHeaderWord(h)
	if err != nil || !bytes.Equal(b, header) {
		t.Fatalf("header %x %v", b, err)
	}
	for _, literal := range []string{"c2123456789abcb4715990d5aea4", "6a123456789abcb4715990d5aea4a55a06", "6d123456789abcb4715990d5aea40420c43b"} {
		want := hexBytes(t, literal)
		v, err := ParseVoice(want)
		if err != nil {
			t.Fatal(err)
		}
		b, err := MarshalVoice(v)
		if err != nil || !bytes.Equal(b, want) {
			t.Fatalf("voice %x %v", b, err)
		}
		before := v
		clear(want)
		if !reflect.DeepEqual(v, before) {
			t.Fatal("voice aliases input")
		}
	}
	for _, b := range [][]byte{header[:17], append(append([]byte{}, header...), 0), hexBytes(t, "0123456789abcdef018412340003e9293020")} {
		if _, err := ParseHeaderWord(b); err == nil {
			t.Fatal("invalid standalone header accepted")
		}
	}
	if _, err := ParseVoice(hexBytes(t, "6d123456789abcb4715990d5aea40420c451")); err == nil {
		t.Fatal("invalid raw ES status accepted outside RTP")
	}
}

func TestFrameTypeIndexMatchesIndependentCycle(t *testing.T) {
	for i, typ := range []FrameType{0xc2, 0xc3, 0xc4, 0xc5, 0xc6, 0xc7, 0xc8, 0xc9, 0x6a, 0xcb, 0xcc, 0x6d, 0x6e, 0x6f, 0x70, 0x71, 0x72, 0x73} {
		if got, ok := typ.Index(); !ok || got != uint8(i) {
			t.Fatalf("type%x index%d %v", typ, got, ok)
		}
	}
	for _, typ := range []FrameType{0, 0x6b, 0x6c, 0xca, 0xcd, 0xff} {
		if _, ok := typ.Index(); ok {
			t.Fatalf("unsupported type%x", typ)
		}
	}
}
