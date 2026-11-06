package rtpvoice

import (
	"bytes"
	"context"
	"errors"
	"net"
	"net/netip"
	"reflect"
	"sync"
	"testing"
	"time"

	"open25.local/open25-fne/internal/p25rtp"
)

func listen(t *testing.T) *Pair {
	t.Helper()
	p, e := Listen(netip.MustParseAddrPort("127.0.0.1:0"))
	if e != nil {
		t.Fatal(e)
	}
	t.Cleanup(func() { p.mu.Lock(); p.closeLocked(); p.mu.Unlock() })
	return p
}
func options(remote netip.AddrPort) Options {
	return Options{Remote: remote, LocalRole: p25rtp.GroupHome, RemoteRole: p25rtp.GroupServing}
}
func newReceiver(t *testing.T) (*Session, *Pair) {
	t.Helper()
	p, peer := listen(t), listen(t)
	s, e := New(p, options(peer.LocalRTP()))
	if e != nil {
		t.Fatal(e)
	}
	t.Cleanup(func() { s.Close() })
	return s, peer
}
func request(seq uint16) p25rtp.Packet {
	return p25rtp.Packet{Role: p25rtp.GroupServing, Sequence: seq, Timestamp: 0x10203040, Control: p25rtp.PacketTypeControl{Type: p25rtp.Request, TSN: 2, ServiceOptions: 4}, RF: &p25rtp.RFControlWord{Source: p25rtp.SUID{WACN: 1, System: 1, Unit: 10001}, TransmitPriority: 4}}
}
func packet(t *testing.T, p p25rtp.Packet) []byte {
	t.Helper()
	b, e := p25rtp.Marshal(p)
	if e != nil {
		t.Fatal(e)
	}
	return b
}
func write(t *testing.T, p *Pair, dst netip.AddrPort, b []byte) {
	t.Helper()
	if _, e := p.rtp.WriteToUDPAddrPort(b, dst); e != nil {
		t.Fatal(e)
	}
}
func receive(t *testing.T, s *Session) p25rtp.Packet {
	t.Helper()
	ctx, c := context.WithTimeout(context.Background(), time.Second)
	defer c()
	p, e := s.Receive(ctx)
	if e != nil {
		t.Fatal(e)
	}
	return p
}
func eventually(t *testing.T, check func() bool) {
	t.Helper()
	deadline := time.Now().Add(time.Second)
	for !check() {
		if time.Now().After(deadline) {
			t.Fatal("condition not observed")
		}
		time.Sleep(time.Millisecond)
	}
}

func TestLocalUDPWireAndSequenceWrap(t *testing.T) {
	s, peer := newReceiver(t)
	// Force the internal owner to the boundary; no caller sequence API exists.
	s.next = 0xffff
	for _, seq := range []uint16{0xffff, 0, 1} {
		p := p25rtp.Packet{Role: p25rtp.GroupHome, Sequence: 123, Timestamp: 98765, Control: p25rtp.PacketTypeControl{Type: p25rtp.Grant, TSN: 2, ServiceOptions: 4}}
		if e := s.Send(context.Background(), p); e != nil {
			t.Fatal(e)
		}
		var b [maxDatagram + 1]byte
		peer.rtp.SetReadDeadline(time.Now().Add(time.Second))
		n, addr, e := peer.rtp.ReadFromUDPAddrPort(b[:])
		if e != nil {
			t.Fatal(e)
		}
		if addr != s.pair.LocalRTP() || n != 21 {
			t.Fatalf("unexpected raw wire %x from %v", b[:n], addr)
		}
		got, e := p25rtp.Parse(b[:n])
		p.Sequence = seq
		if e != nil || !reflect.DeepEqual(got, p) {
			t.Fatalf("decoded %#v %v, want %#v", got, e, p)
		}
	}
	if s.Counters().Sent != 3 {
		t.Fatal(s.Counters())
	}
}

func TestConcurrentSendsOwnOneSequence(t *testing.T) {
	s, peer := newReceiver(t)
	s.next = 65520
	p := p25rtp.Packet{Role: p25rtp.GroupHome, Control: p25rtp.PacketTypeControl{Type: p25rtp.Grant, TSN: 2}}
	var wg sync.WaitGroup
	errs := make(chan error, 32)
	for range 8 {
		wg.Go(func() {
			for range 4 {
				errs <- s.Send(context.Background(), p)
			}
		})
	}
	wg.Wait()
	close(errs)
	for err := range errs {
		if err != nil {
			t.Fatal(err)
		}
	}
	peer.rtp.SetReadDeadline(time.Now().Add(time.Second))
	for i := 0; i < 32; i++ {
		var b [maxDatagram + 1]byte
		n, _, err := peer.rtp.ReadFromUDPAddrPort(b[:])
		if err != nil {
			t.Fatal(err)
		}
		got, err := p25rtp.Parse(b[:n])
		if err != nil || got.Sequence != uint16(65520+i) {
			t.Fatalf("sequence %#v %v", got, err)
		}
	}
}

func TestMaximumProfilePacket(t *testing.T) {
	s, peer := newReceiver(t)
	p := request(99)
	p.Control.Type = p25rtp.Progress
	p.RF = nil
	p.Console = &p25rtp.ConsoleControlWord{ConsoleTXPriority: 255}
	p.Header = &p25rtp.HeaderWord{VoiceBlockBundling: 1, AlgorithmID: 0x84, MFID: 1, KeyID: 0x1234, NID: 0x2930, GroupID: 1001}
	p.Voice = &p25rtp.FullRateVoice{Type: p25rtp.FR12, ES: &p25rtp.EncryptionSync{Words: [4]uint8{63, 62, 61, 60}, Status: 80}}
	b := packet(t, p)
	if len(b) != 93 {
		t.Fatal("maximum wire size", len(b))
	}
	write(t, peer, s.pair.LocalRTP(), b)
	if got := receive(t, s); !reflect.DeepEqual(got, p) {
		t.Fatalf("metadata lost: %#v", got)
	}
}

func TestDuplicateAndEndpointBinding(t *testing.T) {
	s, peer := newReceiver(t)
	valid := packet(t, request(25))
	dst := s.pair.LocalRTP()
	wrong := listen(t)
	write(t, wrong, dst, valid)
	malformed := bytes.Clone(valid)
	malformed[12] = 0
	write(t, peer, dst, malformed)
	// Wrong endpoint and invalid payload do not consume a sequence.
	write(t, peer, dst, valid)
	if got := receive(t, s); !reflect.DeepEqual(got, request(25)) {
		t.Fatal(got)
	}
	write(t, peer, dst, valid)
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Millisecond)
	defer cancel()
	if _, e := s.Receive(ctx); !errors.Is(e, context.DeadlineExceeded) {
		t.Fatal(e)
	}
	if x := s.Counters(); x.Received != 1 || x.WrongEndpoint != 1 || x.InvalidPayload != 1 || x.StaleOrDuplicate != 1 {
		t.Fatal(x)
	}
}

func TestWrongSSRCMalformedOversizedAndProtectedPacketRejected(t *testing.T) {
	s, peer := newReceiver(t)
	dst := s.pair.LocalRTP()
	p := p25rtp.Packet{Role: p25rtp.GroupHome, Sequence: 1, Control: p25rtp.PacketTypeControl{Type: p25rtp.Grant, TSN: 2}}
	write(t, peer, dst, packet(t, p))
	write(t, peer, dst, make([]byte, maxDatagram+20))
	plain := packet(t, request(3))
	plain[12] = 0
	write(t, peer, dst, plain)
	// No SRTP tag or downgrade fallback parser is accepted.
	write(t, peer, dst, append(packet(t, request(4)), make([]byte, 16)...))
	write(t, peer, dst, packet(t, request(5)))
	if got := receive(t, s); got.Sequence != 5 {
		t.Fatal(got)
	}
	if x := s.Counters(); x.InvalidHeader != 1 || x.Oversized != 1 || x.InvalidPayload != 2 {
		t.Fatal(x)
	}
}

func TestReplayWindow(t *testing.T) {
	s, peer := newReceiver(t)
	dst := s.pair.LocalRTP()
	old := packet(t, request(1))
	write(t, peer, dst, old)
	if p := receive(t, s); p.Sequence != 1 {
		t.Fatal(p)
	}
	for n := uint16(2); n <= 132; n++ {
		write(t, peer, dst, packet(t, request(n)))
		if p := receive(t, s); p.Sequence != n {
			t.Fatal(p)
		}
	}
	write(t, peer, dst, old)
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Millisecond)
	defer cancel()
	if _, e := s.Receive(ctx); !errors.Is(e, context.DeadlineExceeded) {
		t.Fatal(e)
	}
	if x := s.Counters(); x.Received != 132 || x.StaleOrDuplicate != 1 {
		t.Fatal(x)
	}
}

func TestReceiveSequenceWrapReorderingAndDuplicate(t *testing.T) {
	s, peer := newReceiver(t)
	dst := s.pair.LocalRTP()
	for _, seq := range []uint16{65534, 65535, 0, 65533, 2, 1} {
		write(t, peer, dst, packet(t, request(seq)))
		if got := receive(t, s); got.Sequence != seq {
			t.Fatal(got)
		}
	}
	for _, seq := range []uint16{65535, 0, 1, 2} {
		write(t, peer, dst, packet(t, request(seq)))
	}
	write(t, peer, dst, packet(t, request(3)))
	if got := receive(t, s); got.Sequence != 3 {
		t.Fatal(got)
	}
	if x := s.Counters(); x.Received != 7 || x.StaleOrDuplicate != 4 {
		t.Fatal(x)
	}
}

func TestTwoSessionsRolesAndClose(t *testing.T) {
	a, b := listen(t), listen(t)
	oa := options(b.LocalRTP())
	ob := options(a.LocalRTP())
	ob.LocalRole, ob.RemoteRole = p25rtp.GroupServing, p25rtp.GroupHome
	sa, e := New(a, oa)
	if e != nil {
		t.Fatal(e)
	}
	sb, e := New(b, ob)
	if e != nil {
		t.Fatal(e)
	}
	t.Cleanup(func() { sa.Close(); sb.Close() })
	if e := sb.Send(context.Background(), request(44)); e != nil {
		t.Fatal(e)
	}
	if p := receive(t, sa); p.Timestamp != request(44).Timestamp {
		t.Fatal(p)
	}
	grant := p25rtp.Packet{Role: p25rtp.GroupHome, Control: p25rtp.PacketTypeControl{Type: p25rtp.Grant, TSN: 2}}
	if e := sa.Send(context.Background(), grant); e != nil {
		t.Fatal(e)
	}
	if p := receive(t, sb); p.Control.Type != p25rtp.Grant {
		t.Fatal(p)
	}
	// The unused companion RTCP port remains exclusively reserved.
	if c, e := net.ListenUDP("udp4", net.UDPAddrFromAddrPort(b.LocalRTCP())); e == nil {
		c.Close()
		t.Fatal("RTCP reservation lost")
	}
	var wg sync.WaitGroup
	for range 8 {
		wg.Go(func() { sa.Close() })
	}
	wg.Wait()
	if e := sa.Send(context.Background(), grant); !errors.Is(e, ErrClosed) {
		t.Fatal(e)
	}
	if _, e := sa.Receive(context.Background()); !errors.Is(e, ErrClosed) {
		t.Fatal(e)
	}
	// Both ports can be rebound after readers joined; no retained socket owner.
	for _, addr := range []netip.AddrPort{a.LocalRTP(), a.LocalRTCP()} {
		c, e := net.ListenUDP("udp4", net.UDPAddrFromAddrPort(addr))
		if e != nil {
			t.Fatal(e)
		}
		c.Close()
	}
}

func TestReceiveCancellationOwnershipAndClose(t *testing.T) {
	s, peer := newReceiver(t)
	ctx, cancel := context.WithCancel(context.Background())
	result := make(chan error, 1)
	go func() { _, e := s.Receive(ctx); result <- e }()
	eventually(t, func() bool { return len(s.receiveGate) == 1 })
	waitCtx, waitCancel := context.WithTimeout(context.Background(), 10*time.Millisecond)
	if _, e := s.Receive(waitCtx); !errors.Is(e, context.DeadlineExceeded) {
		t.Fatal(e)
	}
	waitCancel()
	cancel()
	if e := <-result; !errors.Is(e, context.Canceled) {
		t.Fatal(e)
	}
	// Cancellation's deadline callback cannot poison the following read.
	write(t, peer, s.pair.LocalRTP(), packet(t, request(100)))
	if p := receive(t, s); p.Sequence != 100 {
		t.Fatal(p)
	}
	go func() { _, e := s.Receive(context.Background()); result <- e }()
	eventually(t, func() bool { return len(s.receiveGate) == 1 })
	if e := s.Close(); e != nil {
		t.Fatal(e)
	}
	if e := <-result; !errors.Is(e, ErrClosed) {
		t.Fatal(e)
	}
}

func TestPairOwnershipFailuresAndInputBounds(t *testing.T) {
	if _, e := New(&Pair{}, Options{}); e == nil {
		t.Fatal("zero pair accepted")
	}
	for _, addr := range []string{"127.0.0.1:3", "127.0.0.1:65534", "[::1]:0"} {
		if p, e := Listen(netip.MustParseAddrPort(addr)); e == nil {
			p.Close()
			t.Fatal("accepted", addr)
		}
	}
	p, peer := listen(t), listen(t)
	for name, change := range map[string]func(*Options){
		"remote_zero":           func(o *Options) { o.Remote = netip.MustParseAddrPort("127.0.0.1:0") },
		"remote_odd":            func(o *Options) { o.Remote = netip.MustParseAddrPort("127.0.0.1:3") },
		"remote_inactive_type2": func(o *Options) { o.Remote = netip.MustParseAddrPort("127.0.0.1:65534") },
		"remote_multicast":      func(o *Options) { o.Remote = netip.MustParseAddrPort("239.1.1.1:1000") },
		"roles":                 func(o *Options) { o.RemoteRole = o.LocalRole },
		"timeout":               func(o *Options) { o.WriteTimeout = 21 * time.Millisecond },
		"negative_timeout":      func(o *Options) { o.WriteTimeout = -1 },
	} {
		t.Run(name, func(t *testing.T) {
			o := options(peer.LocalRTP())
			change(&o)
			if s, e := New(p, o); e == nil {
				s.Close()
				t.Fatal("accepted invalid options")
			}
		})
	}
	s, e := New(p, options(peer.LocalRTP()))
	if e != nil {
		t.Fatal(e)
	}
	defer s.Close()
	if _, e := New(p, options(peer.LocalRTP())); e == nil {
		t.Fatal("pair reused")
	}
	if e := p.Close(); e == nil {
		t.Fatal("owner bypassed")
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if e := s.Send(ctx, p25rtp.Packet{}); !errors.Is(e, context.Canceled) {
		t.Fatal(e)
	}
	if e := s.Send(context.Background(), request(1)); e == nil {
		t.Fatal("wrong sender role accepted")
	}
	bad := p25rtp.Packet{Role: p25rtp.GroupHome}
	if e := s.Send(context.Background(), bad); e == nil {
		t.Fatal("invalid payload accepted")
	}
	u := listen(t)
	addr := u.LocalRTP()
	if e := u.Close(); e != nil {
		t.Fatal(e)
	}
	if e := u.Close(); e != nil {
		t.Fatal(e)
	}
	if _, e := New(u, options(peer.LocalRTP())); e == nil {
		t.Fatal("closed pair reused")
	}
	c, e := net.ListenUDP("udp4", net.UDPAddrFromAddrPort(addr))
	if e != nil {
		t.Fatal(e)
	}
	c.Close()
}

func TestCompanionConflictReleasesPartialPair(t *testing.T) {
	p := listen(t)
	rtp, rtcp := p.LocalRTP(), p.LocalRTCP()
	if e := p.Close(); e != nil {
		t.Fatal(e)
	}
	busy, e := net.ListenUDP("udp4", net.UDPAddrFromAddrPort(rtcp))
	if e != nil {
		t.Fatal(e)
	}
	defer busy.Close()
	if q, e := Listen(rtp); e == nil {
		q.Close()
		t.Fatal("accepted occupied companion port")
	}
	probe, e := net.ListenUDP("udp4", net.UDPAddrFromAddrPort(rtp))
	if e != nil {
		t.Fatal("partial RTP reservation leaked", e)
	}
	probe.Close()
}
