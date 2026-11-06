package sipvoice

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"net"
	"net/netip"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/emiago/sipgo"
	"github.com/emiago/sipgo/sip"
)

type testNode struct {
	n         *Node
	peer      Peer
	incoming  chan *Session
	ends      atomic.Int32
	invites   atomic.Int32
	acks      atomic.Int32
	mu        sync.Mutex
	allocated map[*Session][]*net.UDPConn
}

func startTestNode(t *testing.T, id string, setup func(*Options)) *testNode {
	t.Helper()
	x := &testNode{incoming: make(chan *Session, 8), allocated: map[*Session][]*net.UDPConn{}}
	o := Options{PeerID: id, BearerToken: id + "-test-secret", Authenticate: func(ctx context.Context, a AuthRequest) error {
		if a.BearerToken != a.PeerID+"-test-secret" || a.Call.ID == "denied" {
			return ErrUnauthorized
		}
		return nil
	}, OnInvite: func(ctx context.Context, s *Session) (Answer, error) {
		x.invites.Add(1)
		e, cs := udpPair(t)
		x.mu.Lock()
		x.allocated[s] = cs
		x.mu.Unlock()
		x.incoming <- s
		return Answer{Endpoints: e, ResponseContext: json.RawMessage(`{"id":"accepted-grant","talkgroups":[1001]}`), Release: func() {
			x.mu.Lock()
			for _, c := range x.allocated[s] {
				c.Close()
			}
			delete(x.allocated, s)
			x.mu.Unlock()
		}}, nil
	}, OnEnd: func(s *Session, r EndReason) {
		x.ends.Add(1)
		x.mu.Lock()
		for _, c := range x.allocated[s] {
			_ = c.Close()
		}
		delete(x.allocated, s)
		x.mu.Unlock()
	}}
	if setup != nil {
		setup(&o)
	}
	n, e := New(o)
	if e != nil {
		t.Fatal(e)
	}
	x.n = n
	n.server.OnAck(func(r *sip.Request, tx sip.ServerTransaction) { x.acks.Add(1); n.onAck(r, tx) })
	l, e := net.ListenTCP("tcp4", &net.TCPAddr{IP: net.IPv4(127, 0, 0, 1)})
	if e != nil {
		t.Fatal(e)
	}
	x.peer = Peer{ID: id, URI: "sip:" + id + "@" + l.Addr().String() + ";transport=tcp"}
	served := make(chan error, 1)
	go func() { served <- n.ServeTCP(l) }()
	deadline := time.NewTimer(time.Second)
	defer deadline.Stop()
	for {
		if _, err := n.contactCopy(); err == nil {
			break
		}
		select {
		case <-deadline.C:
			t.Fatal("listener not ready")
		case <-time.After(time.Millisecond):
		}
	}
	t.Cleanup(func() {
		if e := n.Close(); e != nil {
			t.Error(e)
		}
		select {
		case e := <-served:
			if e != nil {
				t.Error(e)
			}
		case <-time.After(time.Second):
			t.Error("listener did not close")
		}
		x.mu.Lock()
		defer x.mu.Unlock()
		if len(x.allocated) != 0 {
			t.Error("media allocation leaked")
		}
	})
	return x
}
func udpPair(t *testing.T) (Endpoints, []*net.UDPConn) {
	t.Helper()
	for i := 0; i < 32; i++ {
		a, e := net.ListenUDP("udp4", &net.UDPAddr{IP: net.ParseIP("127.0.0.1")})
		if e != nil {
			t.Fatal(e)
		}
		p := a.LocalAddr().(*net.UDPAddr).Port
		if p%2 != 0 || p > 65532 {
			a.Close()
			continue
		}
		b, e := net.ListenUDP("udp4", &net.UDPAddr{IP: net.ParseIP("127.0.0.1"), Port: p + 1})
		if e != nil {
			a.Close()
			continue
		}
		return endpoints(uint16(p)), []*net.UDPConn{a, b}
	}
	t.Fatal("no loopback UDP pair")
	return Endpoints{}, nil
}
func testCall(id string) Call {
	return Call{ID: id, GroupID: 1001, Source: SourceConsole, Authorization: json.RawMessage(`{"proof":"loopback-only"}`)}
}
func await(t *testing.T, c <-chan struct{}) {
	t.Helper()
	select {
	case <-c:
	case <-time.After(2 * time.Second):
		t.Fatal("bounded session event missing")
	}
}
func waitCount(t *testing.T, f func() bool) {
	t.Helper()
	deadline := time.Now().Add(2 * time.Second)
	for !f() {
		if time.Now().After(deadline) {
			t.Fatal("bounded count condition missing")
		}
		time.Sleep(time.Millisecond)
	}
}
func TestTCPInviteAckByeAndDuplicates(t *testing.T) {
	a := startTestNode(t, "a", nil)
	var admissions atomic.Int32
	b := startTestNode(t, "b", func(o *Options) {
		original := o.Authenticate
		o.Authenticate = func(ctx context.Context, a AuthRequest) error {
			if a.Method == "INVITE" && admissions.Add(1) > 1 {
				return ErrUnauthorized
			}
			return original(ctx, a)
		}
	})
	ctx, cancel := context.WithTimeout(t.Context(), 5*time.Second)
	defer cancel()
	s, e := a.n.Invite(ctx, b.peer, testCall("happy"), endpoints(12000))
	if e != nil {
		t.Fatal(e)
	}
	incoming := <-b.incoming
	await(t, incoming.Ready())
	proof := s.ResponseContext()
	if !bytes.Contains(proof, []byte("accepted-grant")) {
		t.Fatal("accepted grant context missing")
	}
	proof[0] = 'x'
	if s.ResponseContext()[0] == 'x' {
		t.Fatal("answer context accessor aliases")
	}
	local, remote := s.Media()
	blocal, bremote := incoming.Media()
	if local != bremote || remote != blocal {
		t.Fatal("directional endpoints differ")
	}
	// Exact SIP retransmission after the first ACK must keep the same dialog,
	// produce another ACK, and never allocate a second receiver/media owner.
	if e = a.n.client.WriteRequest(s.request.Clone()); e != nil {
		t.Fatal(e)
	}
	waitCount(t, func() bool { return b.acks.Load() >= 2 })
	if b.invites.Load() != 1 {
		t.Fatal("duplicate INVITE allocated media")
	}
	if admissions.Load() != 1 {
		t.Fatal("SIP duplicate consumed allocation proof again")
	}
	if e = s.End(ctx); e != nil {
		t.Fatal(e)
	}
	await(t, incoming.Done())
	if e = s.End(ctx); e != nil {
		t.Fatal(e)
	}
	bye, e := s.dialogRequest(sip.BYE)
	if e != nil {
		t.Fatal(e)
	}
	bye.CSeq().SeqNo-- // duplicate the same completed BYE transaction
	tx, e := a.n.client.TransactionRequest(ctx, bye, sipgo.ClientRequestAddVia)
	if e != nil {
		t.Fatal(e)
	}
	defer tx.Terminate()
	select {
	case r := <-tx.Responses():
		if r.StatusCode != 200 {
			t.Fatal("duplicate BYE not acknowledged")
		}
	case <-ctx.Done():
		t.Fatal("duplicate BYE timed out")
	}
	if a.ends.Load() != 1 || b.ends.Load() != 1 {
		t.Fatal("cleanup not once")
	}
}
func TestTCPAuthorizationBeforeAllocation(t *testing.T) {
	a := startTestNode(t, "a", nil)
	b := startTestNode(t, "b", nil)
	ctx, cancel := context.WithTimeout(t.Context(), 3*time.Second)
	defer cancel()
	if _, e := a.n.Invite(ctx, b.peer, testCall("denied"), endpoints(12000)); e == nil {
		t.Fatal("unauthorized call accepted")
	}
	if b.invites.Load() != 0 {
		t.Fatal("allocated before authorization")
	}
}
func TestTCPCancelPendingInvite(t *testing.T) {
	entered := make(chan struct{})
	b := startTestNode(t, "b", func(o *Options) {
		o.OnInvite = func(ctx context.Context, s *Session) (Answer, error) {
			close(entered)
			<-ctx.Done()
			return Answer{}, ctx.Err()
		}
	})
	a := startTestNode(t, "a", nil)
	ctx, cancel := context.WithCancel(t.Context())
	result := make(chan error, 1)
	go func() { _, e := a.n.Invite(ctx, b.peer, testCall("cancel"), endpoints(12000)); result <- e }()
	await(t, entered)
	cancel()
	select {
	case e := <-result:
		if !errors.Is(e, context.Canceled) {
			t.Fatalf("cancel result %v", e)
		}
	case <-time.After(time.Second):
		t.Fatal("cancel blocked")
	}
	waitCount(t, func() bool { return b.ends.Load() == 1 })
}

func rawInvite(t *testing.T, a, b *testNode, raw []byte) (*sip.Request, sip.ClientTransaction) {
	t.Helper()
	ct, body, e := makeBody(testCall("raw"), raw)
	if e != nil {
		t.Fatal(e)
	}
	var uri sip.Uri
	if sip.ParseUri(b.peer.URI, &uri) != nil {
		t.Fatal("fixture URI")
	}
	req := sip.NewRequest(sip.INVITE, uri)
	req.SetTransport("TCP")
	contact, e := a.n.contactCopy()
	if e != nil {
		t.Fatal(e)
	}
	req.AppendHeader(&contact)
	req.AppendHeader(sip.NewHeader("Content-Type", ct))
	a.n.authHeaders(req)
	req.SetBody(body)
	tx, e := a.n.client.TransactionRequest(t.Context(), req)
	if e != nil {
		t.Fatal("raw fixture INVITE failed")
	}
	t.Cleanup(tx.Terminate)
	return req, tx
}
func finalResponse(t *testing.T, tx sip.ClientTransaction) *sip.Response {
	t.Helper()
	timeout := time.NewTimer(2 * time.Second)
	defer timeout.Stop()
	for {
		select {
		case r := <-tx.Responses():
			if r == nil {
				t.Fatal("response stream ended")
			}
			if !r.IsProvisional() {
				return r
			}
		case <-tx.Done():
			t.Fatal("transaction ended before final response")
		case <-timeout.C:
			t.Fatal("final response timed out")
		}
	}
}
func TestTCPMalformedAndUnsupportedSDPRejectsBeforeMedia(t *testing.T) {
	a := startTestNode(t, "a", nil)
	b := startTestNode(t, "b", nil)
	wire, _ := marshalSDP(endpoints(12000), 123)
	for _, raw := range [][]byte{[]byte(strings.Replace(string(wire), "c=IN IP4 127.0.0.1", "c=IN IP4 255.255.255.255", 1)), []byte(strings.Replace(string(wire), "o=- 123 0 IN IP4 127.0.0.1", "o=- 123 0 IN IP4 255.255.255.255", 1)), []byte("not an SDP"), []byte(strings.Replace(string(wire), "RTP/AVP", "RTP/SAVP", 1)), []byte(strings.Replace(string(wire), "X-TIA-P25-FullRate", "X-TIA-P25-IMBE", 1)), []byte(string(wire) + "a=crypto:1 AEAD_AES_128_GCM inline:unused|2^31\r\n")} {
		_, tx := rawInvite(t, a, b, raw)
		if r := finalResponse(t, tx); r.StatusCode != 488 {
			t.Fatal("unsupported profile not rejected")
		}
		tx.Terminate()
	}
	if b.invites.Load() != 0 {
		t.Fatal("media allocation before SDP admission")
	}
}
func TestTCPOnlyPeerURI(t *testing.T) {
	a := startTestNode(t, "a", nil)
	b := startTestNode(t, "b", nil)
	for name, uri := range map[string]string{
		"sips":            strings.Replace(b.peer.URI, "sip:", "sips:", 1),
		"udp":             strings.Replace(b.peer.URI, "transport=tcp", "transport=udp", 1),
		"tls":             strings.Replace(b.peer.URI, "transport=tcp", "transport=tls", 1),
		"unspecified":     strings.TrimSuffix(b.peer.URI, ";transport=tcp"),
		"extra_parameter": b.peer.URI + ";lr",
	} {
		t.Run(name, func(t *testing.T) {
			if _, e := a.n.Invite(t.Context(), Peer{ID: b.peer.ID, URI: uri}, testCall("bad-transport"), endpoints(12000)); !errors.Is(e, ErrRejected) {
				t.Fatal("non-profile SIP URI accepted")
			}
		})
	}
	if b.invites.Load() != 0 {
		t.Fatal("unsupported URI allocated media")
	}
}
func TestTCPCloseCancelsPendingAllocationAndReleasesReturnedPair(t *testing.T) {
	entered := make(chan struct{})
	var allocated Endpoints
	var conns []*net.UDPConn
	var released atomic.Int32
	b := startTestNode(t, "b", func(o *Options) {
		o.OnInvite = func(ctx context.Context, s *Session) (Answer, error) {
			allocated, conns = udpPair(t)
			close(entered)
			<-ctx.Done()
			return Answer{Endpoints: allocated, Release: func() {
				for _, c := range conns {
					c.Close()
				}
			}}, ctx.Err()
		}
		o.OnEnd = func(s *Session, r EndReason) {
			local, _ := s.Media()
			if local != allocated {
				t.Error("cleanup lost returned allocation")
			}
			for _, c := range conns {
				_ = c.Close()
			}
			released.Add(1)
		}
	})
	a := startTestNode(t, "a", nil)
	result := make(chan error, 1)
	go func() { _, e := a.n.Invite(t.Context(), b.peer, testCall("close"), endpoints(12000)); result <- e }()
	await(t, entered)
	ended := make(chan struct{})
	go func() { _ = b.n.Close(); close(ended) }()
	await(t, ended)
	if released.Load() != 1 {
		t.Fatal("cleanup did not release once")
	}
	for _, p := range []netip.AddrPort{allocated.RTP, allocated.RTCP} {
		c, e := net.ListenUDP("udp4", net.UDPAddrFromAddrPort(p))
		if e != nil {
			t.Fatal("allocated port remains occupied")
		}
		c.Close()
	}
	select {
	case <-result:
	case <-time.After(2 * time.Second):
		t.Fatal("caller survived transport close")
	}
}
func TestTCPMissingAckHasAbsoluteRFCDeadline(t *testing.T) {
	a := startTestNode(t, "a", nil)
	b := startTestNode(t, "b", nil)
	wire, _ := marshalSDP(endpoints(12000), 123)
	_, tx := rawInvite(t, a, b, wire)
	if r := finalResponse(t, tx); r.StatusCode != 200 {
		t.Fatal("fixture not accepted")
	}
	s := <-b.incoming
	start := time.Now()
	select {
	case <-s.Done():
	case <-time.After(64*sip.T1 + 3*time.Second):
		t.Fatal("ACK wait restarted with each retransmission")
	}
	if time.Since(start) < 64*sip.T1-time.Second || s.Reason() != EndTimeout {
		t.Fatal("ACK deadline was not the RFC absolute limit")
	}
	select {
	case <-s.Ready():
		t.Fatal("unacknowledged dialog became ready")
	default:
	}
}

// This test-only TCP relay delays an already transmitted200. It observes bytes
// without decoding/rewriting SIP; production uses SIPgo's parser/transactions.
func TestTCPCancelRacingCommitted200AcksAndByes(t *testing.T) {
	provisionalForwarded := make(chan struct{})
	a := startTestNode(t, "a", nil)
	b := startTestNode(t, "b", func(o *Options) {
		answer := o.OnInvite
		o.OnInvite = func(ctx context.Context, s *Session) (Answer, error) {
			select {
			case <-provisionalForwarded:
				return answer(ctx, s)
			case <-ctx.Done():
				return Answer{}, ctx.Err()
			}
		}
	})
	l, e := net.Listen("tcp4", "127.0.0.1:0")
	if e != nil {
		t.Fatal(e)
	}
	held := make(chan struct{})
	release := make(chan struct{})
	var releaseOnce sync.Once
	unblock := func() { releaseOnce.Do(func() { close(release) }) }
	cancelSeen := make(chan struct{})
	relayDone := make(chan struct{})
	var once sync.Once
	var canceled sync.Once
	var peersMu sync.Mutex
	var peers []net.Conn
	t.Cleanup(func() {
		unblock()
		l.Close()
		peersMu.Lock()
		for _, c := range peers {
			c.Close()
		}
		peersMu.Unlock()
		select {
		case <-relayDone:
		case <-time.After(2 * time.Second):
			t.Error("TCP relay leaked")
		}
	})
	go func() {
		defer close(relayDone)
		down, e := l.Accept()
		if e != nil {
			return
		}
		up, e := net.Dial("tcp4", b.n.listener.Addr().String())
		if e != nil {
			down.Close()
			return
		}
		peersMu.Lock()
		peers = []net.Conn{down, up}
		peersMu.Unlock()
		defer down.Close()
		defer up.Close()
		forwarded := make(chan struct{})
		go func() {
			defer close(forwarded)
			buf := make([]byte, 32768)
			var tail []byte
			for {
				n, e := down.Read(buf)
				if n > 0 {
					scan := append(tail, buf[:n]...)
					if bytes.Contains(scan, []byte("CANCEL sip:")) {
						canceled.Do(func() { close(cancelSeen) })
					}
					if len(scan) > 32 {
						tail = bytes.Clone(scan[len(scan)-32:])
					} else {
						tail = bytes.Clone(scan)
					}
					if _, w := up.Write(buf[:n]); w != nil {
						return
					}
				}
				if e != nil {
					return
				}
			}
		}()
		buf := make([]byte, 32768)
		provisional := make([]byte, 0, 1024)
		for {
			n, e := up.Read(buf)
			if n > 0 {
				if bytes.Contains(buf[:n], []byte("SIP/2.0 200 OK\r\n")) {
					once.Do(func() { close(held); <-release })
				}
				if written, w := down.Write(buf[:n]); w != nil || written != n {
					return
				}
				if provisional != nil {
					provisional = append(provisional, buf[:n]...)
					if bytes.Contains(provisional, []byte("\r\n\r\n")) {
						if !bytes.HasPrefix(provisional, []byte("SIP/2.0 100 ")) {
							t.Error("relay expected the complete provisional response first")
							return
						}
						// Generate200 only after100 has actually crossed the relay.
						// A coalesced read must not hold100 together with200.
						close(provisionalForwarded)
						provisional = nil
					}
				}
			}
			if e != nil {
				return
			}
		}
	}()
	proxy := Peer{ID: "b", URI: "sip:b@" + l.Addr().String() + ";transport=tcp"}
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	result := make(chan error, 1)
	go func() { _, e := a.n.Invite(ctx, proxy, testCall("late200"), endpoints(12000)); result <- e }()
	await(t, provisionalForwarded)
	await(t, held)
	cancel()
	select {
	case e := <-result:
		if !errors.Is(e, context.Canceled) {
			t.Fatal("caller not canceled")
		}
	case <-time.After(time.Second):
		t.Fatal("caller waited for delayed answer")
	}
	// A provisional was transmitted before the committed200, so CANCEL must be
	// sent even though the200 has already won at the UAS.
	await(t, cancelSeen)
	unblock()
	waitCount(t, func() bool { return b.acks.Load() > 0 && b.ends.Load() == 1 })
	if a.ends.Load() != 1 || b.invites.Load() != 1 {
		t.Fatal("late answer duplicated allocation/cleanup")
	}
}

func TestTCPInvalidAnswerAcksThenByes(t *testing.T) {
	for _, mode := range []string{"unsupported-SAVP", "SDES", "wrong-allocation"} {
		t.Run(mode, func(t *testing.T) {
			a := startTestNode(t, "a", nil)
			b := startTestNode(t, "b", nil)
			bye := make(chan struct{})
			var byeOnce sync.Once
			b.n.server.OnBye(func(r *sip.Request, tx sip.ServerTransaction) {
				_ = tx.Respond(sip.NewResponseFromRequest(r, 200, "OK", nil))
				byeOnce.Do(func() { close(bye) })
			})
			b.n.server.OnInvite(func(r *sip.Request, tx sip.ServerTransaction) {
				c, raw, e := parseBody(header(r, "Content-Type"), r.Body())
				if e != nil {
					t.Error("fixture request")
					return
				}
				if mode == "wrong-allocation" {
					raw, _ = marshalSDP(endpoints(14000), 2)
					c.ID = "wrong-allocation"
				}
				if mode == "unsupported-SAVP" {
					raw = bytes.Replace(raw, []byte("RTP/AVP"), []byte("RTP/SAVP"), 1)
				}
				if mode == "SDES" {
					raw = append(raw, []byte("a=crypto:1 AEAD_AES_128_GCM inline:unused|2^31\r\n")...)
				}
				ct, body, e := makeAnswerBody(c.ID, json.RawMessage(`{"id":"grant","talkgroups":[1001]}`), raw)
				if e != nil {
					t.Error("fixture response")
					return
				}
				res := sip.NewResponseFromRequest(r, 200, "OK", body)
				contact, _ := b.n.contactCopy()
				res.AppendHeader(&contact)
				res.AppendHeader(sip.NewHeader("Content-Type", ct))
				res.AppendHeader(sip.NewHeader("X-TrunkSystem-Peer", "b"))
				_ = tx.Respond(res)
				select {
				case <-bye:
				case <-t.Context().Done():
				case <-time.After(3 * time.Second):
					t.Error("bad-answer cleanup missing")
				}
			})
			ctx, stop := context.WithTimeout(t.Context(), 4*time.Second)
			defer stop()
			if _, e := a.n.Invite(ctx, b.peer, testCall("bad-answer"), endpoints(12000)); e == nil {
				t.Fatal("invalid answer accepted")
			}
			await(t, bye)
			// SIPgo dispatches ACK and BYE callbacks independently of wire order.
			waitCount(t, func() bool { return b.acks.Load() > 0 && a.ends.Load() > 0 })
			if b.acks.Load() != 1 || a.ends.Load() != 1 {
				t.Fatalf("invalid200 observation: ACK=%d local cleanup=%d", b.acks.Load(), a.ends.Load())
			}
		})
	}
}
func TestTCPLocalEndDuringAllocationReleasesOnce(t *testing.T) {
	entered := make(chan *Session, 1)
	var releases atomic.Int32
	b := startTestNode(t, "b", func(o *Options) {
		o.OnInvite = func(ctx context.Context, s *Session) (Answer, error) {
			e, cs := udpPair(t)
			entered <- s
			<-ctx.Done()
			return Answer{Endpoints: e, ResponseContext: json.RawMessage(`{"id":"canceled"}`), Release: func() {
				for _, c := range cs {
					c.Close()
				}
				releases.Add(1)
			}}, ctx.Err()
		}
	})
	a := startTestNode(t, "a", nil)
	ctx, stop := context.WithTimeout(t.Context(), 4*time.Second)
	defer stop()
	result := make(chan error, 1)
	go func() { _, e := a.n.Invite(ctx, b.peer, testCall("local-end"), endpoints(12000)); result <- e }()
	var pending *Session
	select {
	case pending = <-entered:
	case <-ctx.Done():
		t.Fatal("allocation not entered")
	}
	if e := pending.End(ctx); e != nil {
		t.Fatal(e)
	}
	await(t, pending.Done())
	if releases.Load() != 1 || b.ends.Load() != 1 {
		t.Fatal("returned canceled allocation not released once")
	}
	select {
	case e := <-result:
		if e == nil {
			t.Fatal("ended allocation accepted")
		}
	case <-time.After(2 * time.Second):
		t.Fatal("local pending end left INVITE unresolved")
	}
}
