// Package sipvoice implements a private, current-only group SIP/TCP segment.
// It is not an ISSI/CSSI implementation, a PBX, or a registrar. RTP talk-spurt
// ownership and native RF outcomes belong to the caller. Transport privacy is
// provided by the deployment VPN; enrolled credentials still authorize calls.
package sipvoice

import (
	"bytes"
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/binary"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net"
	"strings"
	"sync"
	"time"

	"github.com/emiago/sipgo"
	"github.com/emiago/sipgo/sip"
)

const maxBody = 16384

var (
	ErrClosed       = errors.New("SIP voice node closed")
	ErrUnauthorized = errors.New("SIP voice authorization rejected")
	ErrRejected     = errors.New("SIP voice call rejected")
	ErrTimeout      = errors.New("SIP voice transaction timed out")
)

type SourceRole string

const (
	SourceRF      SourceRole = "rf"
	SourceConsole SourceRole = "console"
)

type Call struct {
	ID            string          `json:"id"`
	GroupID       uint16          `json:"group_id"`
	Source        SourceRole      `json:"source"`
	Authorization json.RawMessage `json:"authorization"`
}

func (c Call) String() string {
	return fmt.Sprintf("Call{%s group=%d source=%s authorization=[redacted]}", c.ID, c.GroupID, c.Source)
}
func (c Call) GoString() string { return c.String() }
func (c Call) valid() bool {
	return token(c.ID, 128) && c.GroupID != 0 && (c.Source == SourceRF || c.Source == SourceConsole) && len(c.Authorization) > 0 && len(c.Authorization) <= 8192 && json.Valid(c.Authorization) && bytes.HasPrefix(bytes.TrimSpace(c.Authorization), []byte("{"))
}
func (c Call) clone() Call { c.Authorization = bytes.Clone(c.Authorization); return c }

type Peer struct{ ID, URI string }
type AuthRequest struct {
	Method, PeerID string
	BearerToken    string `json:"-"`
	Call           Call
	Remote         Endpoints
}

func (a AuthRequest) String() string {
	return fmt.Sprintf("AuthRequest{%s peer=%s credentials=[redacted]}", a.Method, a.PeerID)
}
func (a AuthRequest) GoString() string { return a.String() }

type EndReason string

const (
	EndLocal     EndReason = "local"
	EndRemote    EndReason = "remote"
	EndCanceled  EndReason = "canceled"
	EndTimeout   EndReason = "timeout"
	EndRejected  EndReason = "rejected"
	EndClosed    EndReason = "closed"
	EndTransport EndReason = "transport"
)

// Answer owns the accepted incoming allocation. Release must be bounded and
// runs exactly once, including cancellation while OnInvite is completing.
type Answer struct {
	Endpoints       Endpoints
	ResponseContext json.RawMessage
	Release         func()
}

type Options struct {
	PeerID string
	// BearerToken is this node's existing enrolled service credential.
	BearerToken   string `json:"-"`
	AdvertiseHost string
	// Callbacks must honor ctx. Authenticate must validate enrolled identity,
	// endpoint admission and the complete current per-call proof. For ACK/BYE it
	// can distinguish cleanup from a new allocation using Method.
	Authenticate func(context.Context, AuthRequest) error
	// Called only after authenticated, validated INVITE. Allocate endpoints here;
	// do not start media until Session.Ready closes. Return the actual accepted
	// grant as ResponseContext; OnEnd is once-only notification after Release.
	OnInvite    func(context.Context, *Session) (Answer, error)
	OnSource    func(context.Context, *Session, Source) error
	OnRenew     func(context.Context, *Session) error
	OnEnd       func(*Session, EndReason)
	MaxSessions int
}
type Node struct {
	opt       Options
	ua        *sipgo.UserAgent
	client    *sipgo.Client
	server    *sipgo.Server
	ctx       context.Context
	cancel    context.CancelFunc
	mu        sync.Mutex
	listener  net.Listener
	contact   sip.ContactHeader
	sessions  map[string]*Session
	closed    bool
	closeDone chan struct{}
	wg        sync.WaitGroup
}

// Upstream stream-parser errors can include entire packet bytes. Disable the
// dependency's global/default logger before any SIP transport starts; expose
// only typed lifecycle events. Never enable sip.SIPDebug or a raw SIP tracer.
func init() { sip.SetDefaultLogger(slog.New(slog.NewTextHandler(io.Discard, nil))) }

type Session struct {
	node              *Node
	call              Call
	peer              Peer
	mu                sync.Mutex
	local, remote     Endpoints
	responseContext   json.RawMessage
	release           func()
	allocationPending bool
	cancelSetup       context.CancelFunc
	cleanupOnce       sync.Once
	ack               chan struct{}
	ackOnce           sync.Once
	request           *sip.Request
	response          *sip.Response
	destination       string
	localFrom         bool
	nextCSeq          uint32
	remoteCSeq        uint32
	lastRemoteBye     uint32
	requestHash       [32]byte
	credentialHash    [32]byte
	ready             chan struct{}
	done              chan struct{}
	readyOnce         sync.Once
	byeOnce           sync.Once
	byeErr            error
	timer             *time.Timer
	closed            bool
	reason            EndReason
	life              context.Context
	cancelLife        context.CancelFunc
	infoGate          chan struct{}
	infoPending       bool
	source            *Source
	sourceCSeq        uint32
	renewCSeq         uint32
	leaseTimer        *time.Timer
	leaseDeadline     time.Time
}

func New(o Options) (*Node, error) {
	if !token(o.PeerID, 128) || !token(o.BearerToken, 4096) || o.Authenticate == nil || o.OnInvite == nil || o.OnEnd == nil || sip.SIPDebug {
		return nil, errors.New("invalid SIP voice options (callbacks and disabled raw SIP tracing required)")
	}
	if o.MaxSessions == 0 {
		o.MaxSessions = 64
	}
	if o.MaxSessions < 1 || o.MaxSessions > 1024 {
		return nil, errors.New("invalid SIP session limit")
	}
	quiet := slog.New(slog.NewTextHandler(io.Discard, nil))
	parser := sip.NewParser()
	parser.MaxMessageLength = maxBody + 8192
	ua, e := sipgo.NewUA(sipgo.WithUserAgent("Open25 FNE"), sipgo.WithUserAgentParser(parser), sipgo.WithUserAgentTransactionLayerOptions(sip.WithTransactionLayerLogger(quiet), sip.WithTransactionLayerTerminateOnConnClose()), sipgo.WithUserAgentTransportLayerOptions(sip.WithTransportLayerLogger(quiet), sip.WithTransportLayerDNSLookupSRV(false), sip.WithTransportLayerTransports(sip.TransportsConfig{TCP: &sip.TransportTCP{WriteTimeout: 2 * time.Second}})))
	if e != nil {
		return nil, errors.New("SIP voice initialization failed")
	}
	cli, e := sipgo.NewClient(ua, sipgo.WithClientLogger(quiet))
	if e != nil {
		ua.Close()
		return nil, errors.New("SIP client initialization failed")
	}
	srv, e := sipgo.NewServer(ua, sipgo.WithServerLogger(quiet))
	if e != nil {
		ua.Close()
		return nil, errors.New("SIP server initialization failed")
	}
	ctx, cancel := context.WithCancel(context.Background())
	n := &Node{opt: o, ua: ua, client: cli, server: srv, ctx: ctx, cancel: cancel, sessions: map[string]*Session{}, closeDone: make(chan struct{})}
	srv.OnInvite(n.onInvite)
	srv.OnAck(n.onAck)
	srv.OnBye(n.onBye)
	srv.OnInfo(n.onInfo)
	srv.OnCancel(func(r *sip.Request, t sip.ServerTransaction) {
		_ = t.Respond(sip.NewResponseFromRequest(r, 481, "Call Does Not Exist", nil))
	})
	return n, nil
}

// ServeTCP owns l after success. Only plain TCP is exposed by this profile.
func (n *Node) ServeTCP(l *net.TCPListener) error {
	if l == nil {
		return errors.New("missing SIP listener")
	}
	host, port, e := net.SplitHostPort(l.Addr().String())
	if e != nil {
		return errors.New("invalid SIP listener")
	}
	n.mu.Lock()
	if n.closed || n.listener != nil {
		n.mu.Unlock()
		return ErrClosed
	}
	if n.opt.AdvertiseHost != "" {
		host = n.opt.AdvertiseHost
	}
	ip := net.ParseIP(host)
	if host == "" || (ip != nil && ip.IsUnspecified()) {
		n.mu.Unlock()
		return errors.New("SIP advertised host must be explicit")
	}
	contactURI, err := PeerURI(n.opt.PeerID, net.JoinHostPort(host, port))
	uri := sip.Uri{}
	if err != nil || sip.ParseUri(contactURI, &uri) != nil {
		n.mu.Unlock()
		return errors.New("invalid SIP contact")
	}
	n.contact = sip.ContactHeader{Address: uri}
	n.listener = l
	tl := n.listener
	n.wg.Add(1)
	n.mu.Unlock()
	defer n.wg.Done()
	e = n.server.ServeTCP(tl)
	if n.ctx.Err() != nil {
		return nil
	}
	if e != nil {
		return errors.New("SIP TCP listener stopped")
	}
	return nil
}
func (n *Node) Close() error {
	n.mu.Lock()
	if n.closed {
		done := n.closeDone
		n.mu.Unlock()
		<-done
		return nil
	}
	defer close(n.closeDone)
	n.closed = true
	n.cancel()
	l := n.listener
	ss := make([]*Session, 0, len(n.sessions))
	for _, s := range n.sessions {
		ss = append(ss, s)
	}
	n.mu.Unlock()
	if l != nil {
		_ = l.Close()
	}
	for _, s := range ss {
		s.finish(EndClosed)
	}
	n.wg.Wait()
	_ = n.ua.Close()
	for _, s := range ss {
		s.mu.Lock()
		if s.timer != nil {
			s.timer.Stop()
		}
		s.mu.Unlock()
	}
	n.mu.Lock()
	clear(n.sessions)
	n.mu.Unlock()
	return nil
}
func (n *Node) begin() bool {
	n.mu.Lock()
	defer n.mu.Unlock()
	if n.closed {
		return false
	}
	n.wg.Add(1)
	return true
}
func (n *Node) contactCopy() (sip.ContactHeader, error) {
	n.mu.Lock()
	defer n.mu.Unlock()
	if n.closed || n.listener == nil {
		return sip.ContactHeader{}, ErrClosed
	}
	return *n.contact.Clone(), nil
}
func (n *Node) add(s *Session) error {
	n.mu.Lock()
	defer n.mu.Unlock()
	if n.closed {
		return ErrClosed
	}
	if len(n.sessions) >= n.opt.MaxSessions {
		return errors.New("SIP session capacity reached")
	}
	id := s.request.CallID().Value()
	if _, ok := n.sessions[id]; ok {
		return ErrRejected
	}
	n.sessions[id] = s
	return nil
}
func (n *Node) find(r *sip.Request) *Session {
	if r.CallID() == nil {
		return nil
	}
	n.mu.Lock()
	defer n.mu.Unlock()
	return n.sessions[r.CallID().Value()]
}
func (s *Session) Call() Call     { return s.call.clone() }
func (s *Session) PeerID() string { return s.peer.ID }
func (s *Session) Media() (Endpoints, Endpoints) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.local, s.remote
}
func (s *Session) Ready() <-chan struct{} { return s.ready }
func (s *Session) Done() <-chan struct{}  { return s.done }
func (s *Session) Reason() EndReason      { s.mu.Lock(); defer s.mu.Unlock(); return s.reason }
func (s *Session) markReady() {
	s.mu.Lock()
	defer s.mu.Unlock()
	if !s.closed && s.life.Err() == nil {
		s.readyOnce.Do(func() { close(s.ready) })
		if !s.localFrom && s.node.opt.OnRenew != nil && s.leaseTimer == nil {
			s.resetLeaseLocked()
		}
	}
}
func (s *Session) finish(reason EndReason) {
	s.mu.Lock()
	complete := s.finishLocked(reason)
	s.mu.Unlock()
	if complete != nil {
		complete()
	}
}

// mu makes expiry and renewal one ownership decision. INFO callbacks relinquish
// their allocation before Release/OnEnd and Done can complete.
func (s *Session) finishLocked(reason EndReason) func() {
	if s.closed {
		return nil
	}
	s.closed, s.reason = true, reason
	clear(s.request.Body())
	if s.response != nil {
		clear(s.response.Body())
	}
	cancel, pending := s.cancelSetup, s.allocationPending || s.infoPending
	if s.leaseTimer != nil {
		s.leaseTimer.Stop()
		s.leaseTimer = nil
	}
	return func() {
		s.cancelLife()
		if cancel != nil {
			cancel()
		}
		if !pending {
			s.cleanup()
		}
	}
}
func (s *Session) cleanup() {
	s.cleanupOnce.Do(func() {
		s.mu.Lock()
		release, reason := s.release, s.reason
		s.mu.Unlock()
		if release != nil {
			release()
		}
		s.node.opt.OnEnd(s, reason)
		close(s.done)
		// Bounded TU tombstone prevents a duplicate completed transaction from
		// allocating or cleaning up twice after the reliable transaction is gone.
		s.mu.Lock()
		s.timer = time.AfterFunc(64*sip.T1, func() { s.node.mu.Lock(); delete(s.node.sessions, s.request.CallID().Value()); s.node.mu.Unlock() })
		s.mu.Unlock()
	})
}
func (s *Session) setAnswer(a Answer) {
	s.mu.Lock()
	s.local = a.Endpoints
	s.release = a.Release
	s.responseContext = bytes.Clone(a.ResponseContext)
	s.allocationPending = false
	closed := s.closed && !s.infoPending
	s.mu.Unlock()
	if closed {
		s.cleanup()
	}
}

func token(s string, max int) bool {
	if len(s) == 0 || len(s) > max {
		return false
	}
	for _, c := range s {
		if c < 33 || c > 126 {
			return false
		}
	}
	return true
}

// tcpURI accepts only the enrolled, explicit TCP endpoint model. SIPgo also
// supports other transports; none are selected or exposed by this package.
func tcpURI(u sip.Uri) bool {
	return u.Scheme == "sip" && !u.Wildcard && !u.HierarhicalSlashes && canonicalPeerUser(u.User) && u.Host != "" && u.Port > 0 && u.Port <= 65535 && u.Password == "" && len(u.Headers) == 0 && len(u.UriParams) == 1 && u.UriParams[0].K == "transport" && u.UriParams[0].V == "tcp"
}

func header(r sip.Message, name string) string {
	hs := r.GetHeaders(name)
	if len(hs) != 1 {
		return ""
	}
	return hs[0].Value()
}
func credentials(r *sip.Request) (string, string) {
	p := header(r, "X-TrunkSystem-Peer")
	v := header(r, "Authorization")
	if !token(p, 128) || !strings.HasPrefix(v, "Bearer ") || !token(strings.TrimPrefix(v, "Bearer "), 4096) {
		return "", ""
	}
	return p, strings.TrimPrefix(v, "Bearer ")
}
func (n *Node) auth(ctx context.Context, r *sip.Request, c Call, remote Endpoints) error {
	p, t := credentials(r)
	if p == "" || r.Transport() != "TCP" || sip.SIPDebug {
		return ErrUnauthorized
	}
	if n.opt.Authenticate(ctx, AuthRequest{Method: r.Method.String(), PeerID: p, BearerToken: t, Call: c.clone(), Remote: remote}) != nil {
		return ErrUnauthorized
	}
	return nil
}
func (n *Node) authHeaders(r *sip.Request) {
	r.AppendHeader(sip.NewHeader("X-TrunkSystem-Peer", n.opt.PeerID))
	r.AppendHeader(sip.NewHeader("Authorization", "Bearer "+n.opt.BearerToken))
}
func (n *Node) newSession(call Call, peer Peer, req *sip.Request, out bool) *Session {
	r := req.Clone()
	r.SetBody(bytes.Clone(req.Body()))
	remoteSeq := req.CSeq().SeqNo
	if out {
		remoteSeq = 0
	}
	life, cancel := context.WithCancel(n.ctx)
	return &Session{node: n, call: call.clone(), peer: peer, request: r, localFrom: out, nextCSeq: req.CSeq().SeqNo, remoteCSeq: remoteSeq, requestHash: sha256.Sum256(req.Body()), credentialHash: sha256.Sum256([]byte(strings.TrimPrefix(header(req, "Authorization"), "Bearer "))), ready: make(chan struct{}), done: make(chan struct{}), ack: make(chan struct{}), allocationPending: !out, life: life, cancelLife: cancel, infoGate: make(chan struct{}, 1)}
}

func (n *Node) onInvite(r *sip.Request, tx sip.ServerTransaction) {
	if !n.begin() {
		_ = tx.Respond(sip.NewResponseFromRequest(r, 503, "Unavailable", nil))
		return
	}
	defer n.wg.Done()
	reject := func(code int) { _ = tx.Respond(sip.NewResponseFromRequest(r, code, "Call Rejected", nil)) }
	if r.CallID() == nil || r.CSeq() == nil || r.CSeq().MethodName != sip.INVITE || r.From() == nil || !r.From().Params.Has("tag") || r.To() == nil || r.Contact() == nil || !tcpURI(r.Contact().Address) || !tcpURI(r.Recipient) || r.Recipient.User != peerUser(n.opt.PeerID) || len(r.GetHeaders("Route")) != 0 || len(r.GetHeaders("Record-Route")) != 0 {
		reject(400)
		return
	}
	deadline, stop := context.WithTimeout(n.ctx, 64*sip.T1)
	defer stop()
	ctx, cancel := context.WithCancelCause(deadline)
	defer cancel(nil)
	if !tx.OnCancel(func(*sip.Request) { cancel(sip.ErrTransactionCanceled) }) {
		return
	}
	_ = tx.Respond(sip.NewResponseFromRequest(r, 100, "Trying", nil))

	// An already admitted SIP transaction/dialog is not a fresh framework
	// allocation. Authenticate the accepted credential/body binding without
	// consuming the framework's one-use allocation proof again.
	if old := n.find(r); old != nil {
		peer, credential := credentials(r)
		old.mu.Lock()
		res := responseCopy(old.response)
		ok := r.Transport() == "TCP" && !sip.SIPDebug && old.peer.ID == peer && old.credentialHash == sha256.Sum256([]byte(credential)) && old.request.CSeq().SeqNo == r.CSeq().SeqNo && old.request.From().Value() == r.From().Value() && old.request.To().Value() == r.To().Value() && old.requestHash == sha256.Sum256(r.Body())
		closed := old.closed
		old.mu.Unlock()
		if !ok || closed {
			reject(481)
			return
		}
		if res == nil {
			return
		}
		res.RemoveHeader("Via")
		sip.CopyHeaders("Via", r, res)
		res.SetDestination(r.Source())
		_ = tx.Respond(res)
		return
	}

	call, raw, e := parseBody(header(r, "Content-Type"), r.Body())
	if e != nil {
		reject(488)
		return
	}
	remote, e := parseSDP(raw)
	clear(raw)
	if e != nil {
		reject(488)
		return
	}
	if n.auth(ctx, r, call, remote) != nil {
		reject(403)
		return
	}

	if r.To().Params.Has("tag") {
		reject(488)
		return
	} // no re-INVITE/renegotiation in this private profile
	if ctx.Err() != nil {
		return
	}
	s := n.newSession(call, Peer{ID: header(r, "X-TrunkSystem-Peer")}, r, false)
	s.destination = r.Source()
	s.remote = remote
	s.cancelSetup = func() { cancel(context.Canceled) }
	if n.add(s) != nil {
		s.cancelLife()
		reject(486)
		return
	}
	answer, e := n.opt.OnInvite(ctx, s)
	endpoints := answer.Endpoints
	s.setAnswer(answer)
	if e != nil || ctx.Err() != nil {
		s.finish(EndCanceled)
		if context.Cause(ctx) != sip.ErrTransactionCanceled && n.ctx.Err() == nil {
			reject(480)
		}
		return
	}
	if !endpoints.valid() || !validContext(answer.ResponseContext) || answer.Release == nil {
		s.finish(EndRejected)
		reject(488)
		return
	}
	s.mu.Lock()
	if s.closed || ctx.Err() != nil {
		s.mu.Unlock()
		s.finish(EndCanceled)
		if context.Cause(ctx) != sip.ErrTransactionCanceled && n.ctx.Err() == nil {
			reject(480)
		}
		return
	}
	s.local = endpoints
	s.mu.Unlock()
	id, e := randomID()
	if e != nil {
		s.finish(EndRejected)
		reject(488)
		return
	}
	body, e := marshalSDP(endpoints, id)
	if e != nil {
		s.finish(EndRejected)
		reject(488)
		return
	}
	ct, body, e := makeAnswerBody(call.ID, answer.ResponseContext, body)
	if e != nil {
		s.finish(EndRejected)
		reject(488)
		return
	}
	res := sip.NewResponseFromRequest(r, 200, "OK", body)
	contact, e := n.contactCopy()
	if e != nil {
		s.finish(EndClosed)
		return
	}
	res.AppendHeader(&contact)
	res.AppendHeader(sip.NewHeader("Content-Type", ct))
	res.AppendHeader(sip.NewHeader("X-TrunkSystem-Peer", n.opt.PeerID))
	if packages := n.infoPackages(); packages != "" {
		res.AppendHeader(sip.NewHeader("Recv-Info", packages))
	}
	s.mu.Lock()
	s.response = responseCopy(res)
	s.nextCSeq = uint32(id>>32) & 0x7fffffff
	s.mu.Unlock()
	if tx.Respond(res) != nil {
		s.finish(EndTransport)
		return
	}
	// RFC3261 13.3.1.4: absolute64*T1 limit, not a freshly restarted timeout
	// each time a 2xx retransmission is sent. SIPgo handles parsing/transactions.
	timeout := time.NewTimer(64 * sip.T1)
	defer timeout.Stop()
	retry := time.NewTimer(sip.T1)
	defer retry.Stop()
	interval := sip.T1
	for {
		select {
		case <-s.ack:
			s.mu.Lock()
			closed := s.closed
			s.mu.Unlock()
			if closed {
				c, stop := context.WithTimeout(n.ctx, 2*time.Second)
				_ = s.sendBye(c)
				stop()
			}
			return
		case <-n.ctx.Done():
			s.finish(EndClosed)
			return
		case <-timeout.C:
			s.finish(EndTimeout)
			c, stop := context.WithTimeout(n.ctx, 2*time.Second)
			_ = s.sendBye(c)
			stop()
			return
		case <-retry.C:
			if tx.Respond(res.Clone()) != nil {
				s.finish(EndTransport)
				return
			}
			interval = min(2*interval, sip.T2)
			retry.Reset(interval)
		}
	}
}

func (n *Node) onAck(r *sip.Request, tx sip.ServerTransaction) {
	if !n.begin() {
		return
	}
	defer n.wg.Done()
	s := n.find(r)
	if s == nil {
		return
	}
	if !s.matches(r) || r.CSeq().SeqNo != s.request.CSeq().SeqNo || n.auth(n.ctx, r, s.call, s.remoteEndpoints()) != nil {
		return
	}
	s.ackOnce.Do(func() { close(s.ack) })
	s.markReady()
}
func (n *Node) onBye(r *sip.Request, tx sip.ServerTransaction) {
	if !n.begin() {
		return
	}
	defer n.wg.Done()
	s := n.find(r)
	if s == nil || !s.matches(r) || n.auth(n.ctx, r, s.call, s.remoteEndpoints()) != nil {
		_ = tx.Respond(sip.NewResponseFromRequest(r, 481, "Call Does Not Exist", nil))
		return
	}
	s.mu.Lock()
	valid := r.CSeq().SeqNo > s.remoteCSeq || (s.closed && r.CSeq().SeqNo == s.lastRemoteBye)
	if valid {
		s.remoteCSeq = r.CSeq().SeqNo
		s.lastRemoteBye = r.CSeq().SeqNo
	}
	s.mu.Unlock()
	if !valid {
		_ = tx.Respond(sip.NewResponseFromRequest(r, 500, "Out Of Order", nil))
		return
	}
	_ = tx.Respond(sip.NewResponseFromRequest(r, 200, "OK", nil))
	s.finish(EndRemote)
}
func (s *Session) matches(r *sip.Request) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	if r.From() == nil || r.To() == nil || r.CSeq() == nil || r.CSeq().MethodName != r.Method || s.response == nil || header(r, "X-TrunkSystem-Peer") != s.peer.ID {
		return false
	}
	from, to := s.request.From(), s.response.To()
	if !s.localFrom {
		a, b := to.AsFrom(), from.AsTo()
		return r.From().Params.GetOr("tag", "") == b.Params.GetOr("tag", "") && r.To().Params.GetOr("tag", "") == a.Params.GetOr("tag", "")
	}
	return r.From().Params.GetOr("tag", "") == to.Params.GetOr("tag", "") && r.To().Params.GetOr("tag", "") == from.Params.GetOr("tag", "")
}

// Invite returns only after a validated200 and successful ACK write. Context
// cancellation releases media immediately while a bounded TU waiter handles
// CANCEL and any racing/late200 with ACK+BYE (RFC3261 9.1/13.2.2.4).
func (n *Node) Invite(ctx context.Context, p Peer, call Call, local Endpoints) (*Session, error) {
	if !n.begin() {
		return nil, ErrClosed
	}
	defer n.wg.Done()
	if !token(p.ID, 128) || !call.valid() || !local.valid() {
		return nil, ErrRejected
	}
	var uri sip.Uri
	if sip.ParseUri(p.URI, &uri) != nil || !tcpURI(uri) || uri.User != peerUser(p.ID) {
		return nil, ErrRejected
	}
	contact, e := n.contactCopy()
	if e != nil {
		return nil, e
	}
	id, e := randomID()
	if e != nil {
		return nil, ErrRejected
	}
	raw, e := marshalSDP(local, id)
	if e != nil {
		return nil, e
	}
	ct, body, e := makeBody(call, raw)
	clear(raw)
	if e != nil {
		return nil, e
	}
	req := sip.NewRequest(sip.INVITE, uri)
	req.SetTransport("TCP")
	req.SetDestination(net.JoinHostPort(uri.Host, fmt.Sprint(uri.Port)))
	req.AppendHeader(&contact)
	req.AppendHeader(sip.NewHeader("Content-Type", ct))
	req.AppendHeader(sip.NewHeader("Accept", "multipart/mixed"))
	req.AppendHeader(sip.NewHeader("Allow", "INVITE, ACK, CANCEL, BYE, INFO"))
	req.AppendHeader(sip.NewHeader("Recv-Info", ""))
	n.authHeaders(req)
	req.SetBody(body)
	setup, stop := context.WithTimeout(n.ctx, 64*sip.T1)
	tx, e := n.client.TransactionRequest(setup, req)
	if e != nil {
		stop()
		return nil, errors.New("SIP TCP INVITE failed")
	}
	s := n.newSession(call, p, req, true)
	s.destination = req.Destination()
	s.local = local
	if n.add(s) != nil {
		s.cancelLife()
		stop()
		tx.Terminate()
		return nil, ErrRejected
	}
	result := make(chan error, 1)
	n.wg.Add(1)
	go func() {
		defer n.wg.Done()
		defer stop()
		err := n.waitInvite(setup, ctx, s, tx)
		if err != nil {
			tx.Terminate()
		} // successful2xx retains SIPgo TimerM/ACK handling
		result <- err
	}()
	select {
	case e = <-result:
		if e != nil {
			return nil, e
		}
		return s, nil
	case <-ctx.Done():
		s.finish(EndCanceled)
		return nil, ctx.Err()
	case <-n.ctx.Done():
		s.finish(EndClosed)
		return nil, ErrClosed
	}
}
func (n *Node) waitInvite(setup, caller context.Context, s *Session, tx sip.ClientTransaction) error {
	canceled := false
	provisional := false
	cancelSent := false
	callerDone := caller.Done()
	sendCancel := func() {
		if cancelSent || !provisional {
			return
		}
		cancelSent = true
		n.wg.Add(1)
		go func() {
			defer n.wg.Done()
			c, stop := context.WithTimeout(setup, 2*time.Second)
			defer stop()
			r := s.cancelRequest()
			ct, e := n.client.TransactionRequest(c, r, func(*sipgo.Client, *sip.Request) error { return nil })
			if e != nil {
				return
			}
			defer ct.Terminate()
			select {
			case <-ct.Responses():
			case <-ct.Done():
			case <-c.Done():
			}
		}()
	}
	for {
		select {
		case <-callerDone:
			canceled = true
			callerDone = nil
			s.finish(EndCanceled)
			sendCancel()
		case <-setup.Done():
			s.finish(EndTimeout)
			return ErrTimeout
		case <-tx.Done():
			s.finish(EndTransport)
			return ErrRejected
		case res, ok := <-tx.Responses():
			if !ok {
				s.finish(EndTransport)
				return ErrRejected
			}
			if res.IsProvisional() {
				provisional = true
				if canceled {
					sendCancel()
				}
				continue
			}
			if !res.IsSuccess() {
				s.finish(EndRejected)
				if canceled {
					return caller.Err()
				}
				return fmt.Errorf("SIP INVITE rejected (%d)", res.StatusCode)
			}
			if !s.validResponse(res) {
				s.finish(EndRejected)
				return ErrRejected
			}
			s.mu.Lock()
			s.response = responseCopy(res)
			if s.closed {
				clear(s.response.Body())
			}
			s.mu.Unlock()
			ack, e := s.dialogRequest(sip.ACK)
			if e != nil {
				s.finish(EndRejected)
				return ErrRejected
			}
			// A 2xx must be acknowledged even if its SDP is rejected or cancellation won.
			// Pin the existing peer destination: Contact never authorizes a new host.
			replay := ack.Clone()
			tx.OnRetransmission(func(r *sip.Response) {
				if r.IsSuccess() {
					_ = n.client.WriteRequest(replay.Clone(), sipgo.ClientRequestAddVia)
				}
			})
			if n.client.WriteRequest(ack, sipgo.ClientRequestAddVia) != nil {
				s.finish(EndTransport)
				return ErrRejected
			}
			reply, raw, e := parseAnswerBody(header(res, "Content-Type"), res.Body(), s.call.ID)
			var remote Endpoints
			if e == nil {
				remote, e = parseSDP(raw)
			}
			clear(raw)
			if header(res, "X-TrunkSystem-Peer") != s.peer.ID || len(res.GetHeaders("Record-Route")) != 0 {
				e = ErrProfile
			}
			if e != nil || canceled || caller.Err() != nil {
				s.finish(EndCanceled)
				c, stop := context.WithTimeout(setup, 2*time.Second)
				_ = s.sendBye(c)
				stop()
				if caller.Err() != nil {
					return caller.Err()
				}
				return ErrSDP
			}
			s.mu.Lock()
			s.remote = remote
			s.responseContext = bytes.Clone(reply)
			s.mu.Unlock()
			s.markReady()
			return nil
		}
	}
}
func (s *Session) cancelRequest() *sip.Request {
	r := sip.NewRequest(sip.CANCEL, s.request.Recipient)
	for _, name := range []string{"Via", "From", "To", "Call-ID", "Max-Forwards"} {
		sip.CopyHeaders(name, s.request, r)
	}
	r.AppendHeader(&sip.CSeqHeader{SeqNo: s.request.CSeq().SeqNo, MethodName: sip.CANCEL})
	r.SetTransport("TCP")
	r.SetDestination(s.destination)
	r.SetBody(nil)
	s.node.authHeaders(r)
	return r
}
func (s *Session) dialogRequest(method sip.RequestMethod) (*sip.Request, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if method == sip.INFO && s.closed {
		return nil, ErrClosed
	}
	if s.response == nil || s.response.To() == nil || !s.response.To().Params.Has("tag") {
		return nil, ErrRejected
	}
	var target sip.Uri
	var from sip.FromHeader
	var to sip.ToHeader
	if s.localFrom {
		if s.response.Contact() == nil {
			return nil, ErrRejected
		}
		target = s.response.Contact().Address
		from = *s.request.From()
		to = *s.response.To()
	} else {
		if s.request.Contact() == nil {
			return nil, ErrRejected
		}
		target = s.request.Contact().Address
		from = s.response.To().AsFrom()
		to = s.request.From().AsTo()
	}
	if !tcpURI(target) {
		return nil, ErrRejected
	}
	r := sip.NewRequest(method, target)
	r.AppendHeader(sip.HeaderClone(&from))
	r.AppendHeader(sip.HeaderClone(&to))
	r.AppendHeader(sip.HeaderClone(s.request.CallID()))
	seq := s.request.CSeq().SeqNo
	if method != sip.ACK {
		s.nextCSeq++
		seq = s.nextCSeq
	}
	r.AppendHeader(&sip.CSeqHeader{SeqNo: seq, MethodName: method})
	mf := sip.MaxForwardsHeader(70)
	r.AppendHeader(&mf)
	r.SetTransport("TCP")
	r.SetDestination(s.destination)
	r.SetBody(nil)
	s.node.authHeaders(r)
	return r, nil
}
func (s *Session) End(ctx context.Context) error {
	s.mu.Lock()
	closed := s.closed
	s.mu.Unlock()
	if closed {
		return nil
	}
	s.finish(EndLocal)
	if !s.localFrom {
		select {
		case <-s.ack:
		default:
			return nil
		}
	} // UAS TU defers BYE until ACK or its absolute timeout.
	return s.sendBye(ctx)
}
func (s *Session) sendBye(ctx context.Context) error {
	s.byeOnce.Do(func() { s.byeErr = s.writeBye(ctx) })
	return s.byeErr
}
func (s *Session) writeBye(ctx context.Context) error {
	r, e := s.dialogRequest(sip.BYE)
	if e != nil {
		return e
	}
	c, stop := context.WithTimeout(ctx, 2*time.Second)
	defer stop()
	tx, e := s.node.client.TransactionRequest(c, r, sipgo.ClientRequestAddVia)
	if e != nil {
		return errors.New("SIP BYE failed")
	}
	defer tx.Terminate()
	for {
		select {
		case res := <-tx.Responses():
			if res == nil {
				return ErrRejected
			}
			if res.IsProvisional() {
				continue
			}
			if res.StatusCode != 200 {
				return fmt.Errorf("SIP BYE rejected (%d)", res.StatusCode)
			}
			return nil
		case <-tx.Done():
			return ErrRejected
		case <-c.Done():
			return ErrTimeout
		}
	}
}
func randomID() (uint64, error) {
	var b [8]byte
	if _, e := rand.Read(b[:]); e != nil {
		return 0, e
	}
	return binary.BigEndian.Uint64(b[:]), nil
}

func responseCopy(r *sip.Response) *sip.Response {
	if r == nil {
		return nil
	}
	c := r.Clone()
	c.SetBody(bytes.Clone(r.Body()))
	return c
}
func (s *Session) remoteEndpoints() Endpoints {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.remote
}

func (s *Session) ResponseContext() json.RawMessage {
	s.mu.Lock()
	defer s.mu.Unlock()
	return bytes.Clone(s.responseContext)
}

func (s *Session) String() string {
	return fmt.Sprintf("Session{%s peer=%s authorization=[redacted]}", s.call.ID, s.peer.ID)
}
func (s *Session) GoString() string             { return s.String() }
func (s *Session) MarshalJSON() ([]byte, error) { return json.Marshal(s.String()) }

func (s *Session) validResponse(r *sip.Response) bool {
	q := s.request
	return r.CallID() != nil && r.CallID().Value() == q.CallID().Value() && r.From() != nil && r.From().Params.GetOr("tag", "") == q.From().Params.GetOr("tag", "") && r.To() != nil && r.To().Params.GetOr("tag", "") != "" && r.CSeq() != nil && r.CSeq().SeqNo == q.CSeq().SeqNo && r.CSeq().MethodName == sip.INVITE
}
