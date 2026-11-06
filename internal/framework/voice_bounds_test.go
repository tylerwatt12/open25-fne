package framework

import (
	"context"
	"errors"
	"net/netip"
	"open25.local/open25-fne/internal/dispatch"
	"open25.local/open25-fne/internal/p25rtp"
	"open25.local/open25-fne/internal/routing"
	"open25.local/open25-fne/internal/rtpvoice"
	"open25.local/open25-fne/internal/sipvoice"
	"testing"
	"time"
)

func TestVoicePreReadyBoundAndMissingMetadata(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	c := &voiceCall{source: true, ctx: ctx, cancel: cancel, request: routing.Request{CallRequest: dispatch.CallRequest{RadioID: 10001}}, inputs: make(chan voiceInput, 11)}
	f := sipTestFrame("bounded", 10001, 1001, 1, 0)
	missing := f
	missing.P25 = nil
	if err := c.Write(missing); !errors.Is(err, dispatch.ErrP25MetadataNotSupported) {
		t.Fatal("missing metadata generated", err)
	}
	for i := 0; i < 11; i++ {
		if err := c.Write(sipTestFrame("bounded", 10001, 1001, 1, uint64(i))); err != nil {
			t.Fatal(err)
		}
	}
	if err := c.Write(sipTestFrame("bounded", 10001, 1001, 1, 11)); !errors.Is(err, rtpvoice.ErrQueueFull) || ctx.Err() == nil {
		t.Fatal("unbounded pre-ready queue", err)
	}
}
func TestVoiceOrdinalUsesActualSFAndFTInsteadOfPacketCounter(t *testing.T) {
	pair, err := rtpvoice.Listen(netip.MustParseAddrPort("127.0.0.1:0"))
	if err != nil {
		t.Fatal(err)
	}
	session, err := rtpvoice.New(pair, rtpvoice.Options{Remote: pair.LocalRTP(), LocalRole: p25rtp.GroupHome, RemoteRole: p25rtp.GroupServing})
	if err != nil {
		t.Fatal(err)
	}
	defer session.Close()
	engine, err := rtpvoice.NewEngine(session, rtpvoice.Spurt{Origin: p25rtp.GroupHome, TSN: 1, GroupID: 1001, ServiceOptions: 4, RF: &p25rtp.RFControlWord{Source: p25rtp.SUID{WACN: 1, System: 1, Unit: 10001}}})
	if err != nil {
		t.Fatal(err)
	}
	defer engine.Close()
	c := &voiceCall{request: routing.Request{CallRequest: dispatch.CallRequest{Talkgroups: []int{1001}}}}
	b := &voiceBurst{source: sipvoice.Source{Epoch: 1, TSN: 1, MFID: 0, Service: 4, AlgorithmID: 0x80}, engine: engine}
	first := sipTestFrame("ordinal", 10001, 1001, 1, 71)
	first.Sequence = 10
	if err = c.push(b, first); err != nil {
		t.Fatal(err)
	}
	next := sipTestFrame("ordinal", 10001, 1001, 1, 72)
	next.Sequence = 10000
	if err = c.push(b, next); err != nil || b.position != 72 {
		t.Fatal("packet counter replaced voice ordinal", b.position, err)
	}
	b.lastAt = time.Now().Add(-2 * time.Second)
	if err = c.push(b, sipTestFrame("ordinal", 10001, 1001, 1, 73)); !errors.Is(err, rtpvoice.ErrFramePosition) {
		t.Fatal("ambiguous cycle inferred", err)
	}
}
