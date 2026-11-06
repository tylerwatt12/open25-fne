package sipvoice

import (
	"bytes"
	"encoding/json"
	"fmt"
	"net/netip"
	"strings"
	"testing"
)

func endpoints(p uint16) Endpoints {
	a := netip.MustParseAddr("127.0.0.1")
	return Endpoints{netip.AddrPortFrom(a, p), netip.AddrPortFrom(a, p+1)}
}
func TestPlainFullRateSDP(t *testing.T) {
	m := endpoints(12000)
	wire, e := marshalSDP(m, 123)
	if e != nil {
		t.Fatal(e)
	}
	got, e := parseSDP(wire)
	if e != nil {
		t.Fatal(e)
	}
	if got != m {
		t.Fatal("negotiated media mismatch")
	}
	if !bytes.Contains(wire, []byte("m=audio 12000 RTP/AVP 100\r\n")) || !bytes.Contains(wire, []byte("a=rtpmap:100 X-TIA-P25-FullRate/8000\r\n")) {
		t.Fatal("profile mismatch")
	}
	cases := map[string]string{
		"secure_profile":   strings.Replace(string(wire), "RTP/AVP", "RTP/SAVP", 1),
		"wrong_codec":      strings.Replace(string(wire), "X-TIA-P25-FullRate", "X-TIA-P25-IMBE", 1),
		"wrong_clock":      strings.Replace(string(wire), "/8000", "/48000", 1),
		"extra_codec":      strings.Replace(string(wire), "AVP 100", "AVP 100 101", 1),
		"broadcast_origin": strings.Replace(string(wire), "o=- 123 0 IN IP4 127.0.0.1", "o=- 123 0 IN IP4 255.255.255.255", 1),
		"broadcast_media":  strings.Replace(string(wire), "c=IN IP4 127.0.0.1", "c=IN IP4 255.255.255.255", 1),
		"odd_port":         strings.Replace(string(wire), "audio 12000", "audio 12001", 1),
		"inactive_port":    strings.Replace(string(wire), "audio 12000", "audio 65534", 1),
		"SDES":             string(wire) + "a=crypto:1 AEAD_AES_128_GCM inline:secret-key-is-not-for-errors|2^31\r\n",
		"legacy_key":       string(wire) + "k=clear:secret-key-is-not-for-errors\r\n",
		"duplicate_rtpmap": string(wire) + "a=rtpmap:100 X-TIA-P25-FullRate/8000\r\n",
		"unpaired_rtcp":    string(wire) + "a=rtcp:13001 IN IP4 127.0.0.1\r\n",
		"rtcp_mux":         string(wire) + "a=rtcp-mux\r\n",
		"bad_sdp":          "v=0\r\na=crypto:secret-key-is-not-for-errors\r\n",
	}
	for name, raw := range cases {
		t.Run(name, func(t *testing.T) {
			if _, err := parseSDP([]byte(raw)); err == nil {
				t.Fatal("accepted unsupported/malformed SDP")
			} else if strings.Contains(err.Error(), "inline:") || strings.Contains(err.Error(), "secret-key") {
				t.Fatal("secret in diagnostic")
			}
		})
	}
	broadcast := netip.MustParseAddr("255.255.255.255")
	if _, err := marshalSDP(Endpoints{netip.AddrPortFrom(broadcast, 12000), netip.AddrPortFrom(broadcast, 12001)}, 123); err == nil {
		t.Fatal("broadcast local media endpoint accepted")
	}
	paired := append(bytes.Clone(wire), []byte("a=rtcp:12001 IN IP4 127.0.0.1\r\n")...)
	if _, e = parseSDP(paired); e != nil {
		t.Fatal("paired RTCP rejected")
	}
}
func TestCallContextBounds(t *testing.T) {
	c := Call{ID: "call-1", GroupID: 1001, Source: SourceConsole, Authorization: json.RawMessage(`{"proof":"test-only"}`)}
	raw, _ := marshalSDP(endpoints(12000), 1)
	ct, body, e := makeBody(c, raw)
	if e != nil {
		t.Fatal(e)
	}
	got, sdp, e := parseBody(ct, body)
	if e != nil || got.ID != c.ID || !bytes.Equal(sdp, raw) {
		t.Fatal("MIME round trip failed")
	}
	if _, _, e = parseBody("application/sdp", raw); e == nil {
		t.Fatal("missing authorization context accepted")
	}
	c.Authorization = bytes.Repeat([]byte("x"), 8193)
	if _, _, e = makeBody(c, raw); e == nil {
		t.Fatal("oversized proof accepted")
	}
}

func TestAnswerContextCallBindingAndClone(t *testing.T) {
	raw, _ := marshalSDP(endpoints(12000), 1)
	ctx := json.RawMessage(`{"id":"actual-grant","talkgroups":[1001,1002]}`)
	ct, body, e := makeAnswerBody("allocation-1", ctx, raw)
	if e != nil {
		t.Fatal(e)
	}
	got, wire, e := parseAnswerBody(ct, body, "allocation-1")
	if e != nil || !bytes.Equal(wire, raw) || !bytes.Equal(got, ctx) {
		t.Fatal("answer context changed")
	}
	if _, _, e = parseAnswerBody(ct, body, "other-allocation"); e == nil {
		t.Fatal("wrong allocation accepted")
	}
	if _, _, e = makeAnswerBody("allocation-1", json.RawMessage(`null`), raw); e == nil {
		t.Fatal("unbound answer accepted")
	}
	s := &Session{call: testCall("redaction"), peer: Peer{ID: "peer"}}
	for _, v := range []string{fmt.Sprintf("%+v", s), fmt.Sprintf("%#v", s)} {
		if !strings.Contains(v, "redacted") || strings.Contains(v, "loopback-only") {
			t.Fatal("session secret formatting")
		}
	}
}
