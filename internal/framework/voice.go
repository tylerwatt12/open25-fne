package framework

import (
	"context"
	"crypto/subtle"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"net/http"
	"net/netip"
	"net/url"
	"open25.local/open25-fne/internal/dispatch"
	"open25.local/open25-fne/internal/p25rtp"
	"open25.local/open25-fne/internal/routing"
	"open25.local/open25-fne/internal/rtpvoice"
	"open25.local/open25-fne/internal/sipvoice"
	"slices"
	"strconv"
	"strings"
	"sync"
	"time"
)

// Discovery is authenticated management traffic. Every admitted call owns a
// separate SIP dialog and RTP pair; management polling cannot renew that owner.
type voiceEndpoint struct {
	PeerID     string `json:"peer_id"`
	InstanceID string `json:"instance_id"`
	Role       Role   `json:"role"`
	URI        string `json:"uri"`
	MediaIP    string `json:"media_ip"`
	Generation string `json:"generation"`
}
type voicePeer struct {
	endpoint   voiceEndpoint
	credential PeerCredential
	seen       time.Time
}
type voiceEnvelope struct {
	Request routing.Request `json:"request"`
	Receive bool            `json:"receive"`
	Origin  p25rtp.Role     `json:"origin"`
}
type voiceAnswer struct {
	Grant     dispatch.Grant `json:"grant"`
	Emergency bool           `json:"emergency"`
}
type voiceRuntime struct {
	s        *Service
	node     *sipvoice.Node
	listener *net.TCPListener
	endpoint voiceEndpoint
	bind     netip.Addr
	mu       sync.Mutex
	peers    map[string]voicePeer
	calls    map[*voiceCall]bool
	closed   bool
	wg       sync.WaitGroup
}

func (s *Service) openVoice() error {
	c := s.Config()
	if c.Role == RoleSystem {
		return nil
	}
	creds, err := readCredentials(c.CredentialFile)
	if err != nil {
		return err
	}
	// An unenrolled service can still serve its management interface.
	if creds.OutboundID == "" {
		return nil
	}
	listen, address := c.Media.VoiceListen, c.Media.VoiceAddress
	if listen == "" {
		host, _, _ := net.SplitHostPort(c.InternalListen)
		u, _ := url.Parse(c.InternalURL)
		listen, address = net.JoinHostPort(host, "0"), net.JoinHostPort(u.Hostname(), "0")
	}
	advertised, advertisedPort, err := net.SplitHostPort(address)
	if err != nil {
		return err
	}
	ips, err := net.DefaultResolver.LookupNetIP(s.operationCtx, "ip4", advertised)
	if err != nil || len(ips) == 0 {
		return errors.New("voice address needs a reachable IPv4 host")
	}
	ip := ips[0].Unmap()
	if ip.IsUnspecified() || ip.IsMulticast() {
		return errors.New("voice address needs a unicast IPv4 host")
	}
	a, err := net.ResolveTCPAddr("tcp4", listen)
	if err != nil {
		return err
	}
	l, err := net.ListenTCP("tcp4", a)
	if err != nil {
		return fmt.Errorf("voice listener: %w", err)
	}
	if advertisedPort != "0" && advertisedPort != strconv.Itoa(l.Addr().(*net.TCPAddr).Port) {
		l.Close()
		return errors.New("voice address and listener need the same port")
	}
	generation, err := randomToken()
	if err != nil {
		l.Close()
		return err
	}
	bind := netip.IPv4Unspecified()
	if a.IP != nil {
		if b, ok := netip.AddrFromSlice(a.IP); ok {
			bind = b.Unmap()
		}
	}
	v := &voiceRuntime{s: s, listener: l, bind: bind, peers: map[string]voicePeer{}, calls: map[*voiceCall]bool{}}
	uri, err := sipvoice.PeerURI(creds.OutboundID, net.JoinHostPort(ip.String(), strconv.Itoa(l.Addr().(*net.TCPAddr).Port)))
	if err != nil {
		l.Close()
		return err
	}
	v.endpoint = voiceEndpoint{PeerID: creds.OutboundID, InstanceID: c.InstanceID, Role: c.Role, MediaIP: ip.String(), Generation: generation, URI: uri}
	v.node, err = sipvoice.New(sipvoice.Options{PeerID: creds.OutboundID, BearerToken: creds.OutboundToken, AdvertiseHost: ip.String(), Authenticate: v.authenticate, OnInvite: v.invite, OnSource: v.sourceReady, OnRenew: v.renew, OnEnd: func(*sipvoice.Session, sipvoice.EndReason) {}})
	if err != nil {
		l.Close()
		return err
	}
	s.voice.Store(v)
	v.wg.Add(1)
	go func() {
		defer v.wg.Done()
		if err := v.node.ServeTCP(l); err != nil && s.operationCtx.Err() == nil {
			v.mu.Lock()
			v.closed = true
			for call := range v.calls {
				call.cancel()
			}
			v.mu.Unlock()
			s.runtimeError(err)
		}
	}()
	return nil
}
func (e voiceEndpoint) valid(id string, p PeerCredential) bool {
	ip, err := netip.ParseAddr(e.MediaIP)
	if err != nil || !ip.Is4() || ip.IsUnspecified() || ip.IsMulticast() || e.PeerID != id || e.InstanceID != p.InstanceID || e.Role != p.Role || e.Generation == "" || len(e.Generation) > 128 {
		return false
	}
	suffix := ";transport=tcp"
	if !strings.HasSuffix(e.URI, suffix) {
		return false
	}
	at := strings.LastIndex(e.URI, "@")
	if at < 0 {
		return false
	}
	address := strings.TrimSuffix(e.URI[at+1:], suffix)
	canonical, err := sipvoice.PeerURI(id, address)
	if err != nil || canonical != e.URI {
		return false
	}
	host, _, err := net.SplitHostPort(address)
	return err == nil && host == ip.String()
}
func voiceRoles(local, peer Role) bool {
	return (local == RoleZone && (peer == RoleSite || peer == RoleApplication)) || (local == RoleSite && (peer == RoleZone || peer == RoleApplication)) || (local == RoleApplication && (peer == RoleZone || peer == RoleSite))
}
func (s *Service) voiceDiscovery(w http.ResponseWriter, r *http.Request, p PeerCredential) {
	v := s.voice.Load()
	if v == nil || !voiceRoles(s.Config().Role, p.Role) {
		fail(w, 403, dispatch.ErrPermission)
		return
	}
	if r.Method == http.MethodPost {
		var e voiceEndpoint
		if err := readJSON(w, r, &e); err != nil {
			fail(w, 400, err)
			return
		}
		if !e.valid(r.Header.Get("X-Service-ID"), p) {
			fail(w, 403, dispatch.ErrPermission)
			return
		}
		v.remember(e, p)
	} else if r.Method != http.MethodGet {
		w.WriteHeader(405)
		return
	}
	v.mu.Lock()
	closed := v.closed
	v.mu.Unlock()
	if closed {
		fail(w, 503, dispatch.ErrUnavailable)
		return
	}
	respond(w, 200, v.endpoint)
}
func (v *voiceRuntime) remember(e voiceEndpoint, p PeerCredential) {
	v.mu.Lock()
	old := v.peers[e.PeerID]
	v.peers[e.PeerID] = voicePeer{e, p, time.Now()}
	v.mu.Unlock()
	if old.endpoint.Generation != "" && (old.endpoint.Generation != e.Generation || old.endpoint.URI != e.URI) {
		v.cancelPeer(e.PeerID)
	}
}
func (v *voiceRuntime) discover(ctx context.Context, address string, recipient *PeerCredential) (voiceEndpoint, error) {
	var e voiceEndpoint
	if err := v.s.requestAt(ctx, address, http.MethodPost, "/internal/voice", v.endpoint, &e, recipient); err != nil {
		return e, err
	}
	creds, err := readCredentials(v.s.Config().CredentialFile)
	if err != nil {
		return e, err
	}
	p, ok := creds.Peers[e.PeerID]
	if !ok || !e.valid(e.PeerID, p) || !voiceRoles(v.s.Config().Role, p.Role) || (recipient != nil && p != *recipient) {
		return voiceEndpoint{}, dispatch.ErrPermission
	}
	v.remember(e, p)
	return e, nil
}
func (v *voiceRuntime) authenticate(ctx context.Context, a sipvoice.AuthRequest) error {
	creds, err := readCredentials(v.s.Config().CredentialFile)
	if err != nil {
		return dispatch.ErrPermission
	}
	p, ok := creds.Peers[a.PeerID]
	if !ok || subtle.ConstantTimeCompare([]byte(p.Token), []byte(a.BearerToken)) != 1 || !voiceRoles(v.s.Config().Role, p.Role) {
		return dispatch.ErrPermission
	}
	if a.Method != "INVITE" {
		return nil
	} // Cleanup remains possible after policy revocation.
	v.mu.Lock()
	known := v.peers[a.PeerID]
	closed := v.closed
	v.mu.Unlock()
	ip, err := netip.ParseAddr(known.endpoint.MediaIP)
	if closed || err != nil || known.credential != p || a.Remote.RTP.Addr() != ip || a.Remote.RTCP.Addr() != ip {
		return dispatch.ErrPermission
	}
	var e voiceEnvelope
	if DecodeStrict(a.Call.Authorization, &e) != nil {
		return dispatch.ErrPermission
	}
	return v.validate(a.PeerID, p, a.Call, e)
}
func (v *voiceRuntime) validate(peerID string, p PeerCredential, c sipvoice.Call, e voiceEnvelope) error {
	q := e.Request
	if q.ID != c.ID || len(q.Talkgroups) == 0 || q.Talkgroups[0] != int(c.GroupID) || q.RadioID < 1 || q.RadioID > 0xfffffb {
		return dispatch.ErrPermission
	}
	rf := q.SourceKind == "rf"
	if q.EncryptionPending && (!rf || !q.Secure || q.AlgorithmID != 0 || q.KeyID != 0) {
		return dispatch.ErrPermission
	}
	if rf != (c.Source == sipvoice.SourceRF) || (!rf && q.SourceKind != "application" && q.SourceKind != "automation") {
		return dispatch.ErrPermission
	}
	role := v.s.Config().Role
	if e.Receive {
		if role != RoleApplication || (p.Role != RoleZone && p.Role != RoleSite) || e.Origin != p25rtp.GroupHome || !v.s.dispatchReceiveAllowed(int(c.GroupID)) || v.s.mediaClient == nil {
			return dispatch.ErrPermission
		}
		v.s.mediaClient.mu.Lock()
		active := v.s.mediaClient.endpoint.PeerID == peerID && v.s.mediaClient.activeURL != ""
		v.s.mediaClient.mu.Unlock()
		if !active {
			return dispatch.ErrPermission
		}
		return nil
	}
	if role == RoleApplication {
		return dispatch.ErrPermission
	}
	expected := p25rtp.GroupServing
	if p.Role == RoleZone {
		expected = p25rtp.GroupHome
	}
	if e.Origin != expected {
		return dispatch.ErrPermission
	}
	return v.s.validateVoiceSource(p, q)
}

// These are the former HTTP allocation checks, now applied before SIP admission.
func (s *Service) validateVoiceSource(peer PeerCredential, input routing.Request) error {
	role := s.Config().Role
	if !((role == RoleZone && (peer.Role == RoleSite || peer.Role == RoleApplication)) || (role == RoleSite && (peer.Role == RoleZone || peer.Role == RoleApplication))) {
		return dispatch.ErrPermission
	}
	if input.SourceKind != "rf" {
		return nil
	}
	groups := input.Talkgroups
	if role == RoleSite && peer.Role == RoleZone {
		groups = input.AuthorizationGroups
	}
	if len(groups) != 1 || input.OriginSite == "" {
		return dispatch.ErrPermission
	}
	if role == RoleZone {
		view := s.view()
		if peer.Role != RoleSite || input.OriginSite != peer.InstanceID || !groupAtSite(view.System, input.Talkgroups[0], peer.InstanceID) {
			return dispatch.ErrPermission
		}
		if err := s.validateRFObservation(input); err != nil {
			return err
		}
	} else {
		zone := s.view().Zone
		if peer.Role != RoleZone || zone == nil || peer.InstanceID != zone.ID || input.OriginSite == s.Config().InstanceID {
			return dispatch.ErrPermission
		}
	}
	return nil
}
func (s *Service) dispatchReceiveAllowed(group int) bool {
	if s.hub == nil {
		return false
	}
	v := s.view()
	s.audioLeaseMu.Lock()
	defer s.audioLeaseMu.Unlock()
	for rid, l := range s.audioLeases {
		if time.Now().Before(l.Expires) {
			if u, ok := v.Users[strings.TrimPrefix(l.UserID, "user:")]; ok {
				effective, _, err := resolveUserRadio(u.User, v.System, rid)
				if err == nil && userAllows(effective, group) {
					return true
				}
			}
		}
	}
	return false
}
func (s *Service) receiveOwnerAllowed(owner string, group int) bool {
	v := s.view()
	s.audioLeaseMu.Lock()
	defer s.audioLeaseMu.Unlock()
	for rid, l := range s.audioLeases {
		if l.Owner == owner && time.Now().Before(l.Expires) {
			if u, ok := v.Users[strings.TrimPrefix(l.UserID, "user:")]; ok {
				effective, _, err := resolveUserRadio(u.User, v.System, rid)
				if err == nil && userAllows(effective, group) {
					return true
				}
			}
		}
	}
	return false
}
func (v *voiceRuntime) pair() (*rtpvoice.Pair, sipvoice.Endpoints, error) {
	p, err := rtpvoice.Listen(netip.AddrPortFrom(v.bind, 0))
	if err != nil {
		return nil, sipvoice.Endpoints{}, err
	}
	ip, _ := netip.ParseAddr(v.endpoint.MediaIP)
	return p, sipvoice.Endpoints{RTP: netip.AddrPortFrom(ip, p.LocalRTP().Port()), RTCP: netip.AddrPortFrom(ip, p.LocalRTCP().Port())}, nil
}
func (v *voiceRuntime) invite(ctx context.Context, s *sipvoice.Session) (sipvoice.Answer, error) {
	var e voiceEnvelope
	if DecodeStrict(s.Call().Authorization, &e) != nil {
		return sipvoice.Answer{}, dispatch.ErrPermission
	}
	creds, err := readCredentials(v.s.Config().CredentialFile)
	if err != nil {
		return sipvoice.Answer{}, err
	}
	peer, ok := creds.Peers[s.PeerID()]
	if !ok {
		return sipvoice.Answer{}, dispatch.ErrPermission
	}
	if err = v.validate(s.PeerID(), peer, s.Call(), e); err != nil {
		return sipvoice.Answer{}, err
	}
	q := e.Request
	if !(v.s.Config().Role == RoleSite && peer.Role == RoleZone) && !e.Receive {
		q.LogicalCallID = q.ID
		q.AuthorizationGroups = slices.Clone(q.Talkgroups)
	}
	g := dispatch.Grant{ID: q.ID, Talkgroups: slices.Clone(q.Talkgroups), Secure: q.Secure, AlgorithmID: q.AlgorithmID, KeyID: q.KeyID}
	if !e.Receive {
		q.CallRequest, err = v.s.prepareCall(q.CallRequest, q.SourceKind, peer.Role == RoleZone)
		if err != nil {
			return sipvoice.Answer{}, err
		}
		if q.SourceKind == "rf" && v.s.Config().Role == RoleZone {
			if err = v.s.acceptRFObservation(ctx, q); err != nil {
				return sipvoice.Answer{}, err
			}
		}
		g, err = v.s.broker.Begin(ctx, s.PeerID(), q)
		if err != nil {
			return sipvoice.Answer{}, err
		}
	}
	pair, endpoints, err := v.pair()
	if err != nil {
		if !e.Receive {
			_ = v.s.broker.End(ctx, s.PeerID(), g)
		}
		return sipvoice.Answer{}, err
	}
	c := v.newCall(s.PeerID(), peer, q, g, e.Receive, false, e.Origin, pair)
	c.sip = s
	if err = v.add(c); err != nil {
		pair.Close()
		if !e.Receive {
			_ = v.s.broker.End(ctx, s.PeerID(), g)
		}
		return sipvoice.Answer{}, err
	}
	raw, _ := json.Marshal(voiceAnswer{g, q.Emergency})
	go c.run()
	return sipvoice.Answer{Endpoints: endpoints, ResponseContext: raw, Release: c.release}, nil
}
func (v *voiceRuntime) begin(ctx context.Context, e voiceEndpoint, q routing.Request, receive bool) (*voiceCall, error) {
	if q.SourceKind == "rf" && v.s.Config().Role == RoleSite && !receive && q.RFSource == (routing.RFSourceObservation{}) {
		var err error
		q, err = v.s.observeRFSource(q)
		if err != nil {
			return nil, err
		}
	}
	creds, err := readCredentials(v.s.Config().CredentialFile)
	if err != nil {
		return nil, err
	}
	peer, ok := creds.Peers[e.PeerID]
	if !ok || !e.valid(e.PeerID, peer) {
		return nil, dispatch.ErrPermission
	}
	pair, endpoints, err := v.pair()
	if err != nil {
		return nil, err
	}
	origin := p25rtp.GroupServing
	if v.s.Config().Role == RoleZone || receive {
		origin = p25rtp.GroupHome
	}
	if origin == p25rtp.GroupServing {
		q.LogicalCallID = q.ID
		q.AuthorizationGroups = slices.Clone(q.Talkgroups)
	}
	source := sipvoice.SourceConsole
	if q.SourceKind == "rf" {
		source = sipvoice.SourceRF
	}
	raw, _ := json.Marshal(voiceEnvelope{q, receive, origin})
	session, err := v.node.Invite(ctx, sipvoice.Peer{ID: e.PeerID, URI: e.URI}, sipvoice.Call{ID: q.ID, GroupID: uint16(q.Talkgroups[0]), Source: source, Authorization: raw}, endpoints)
	if err != nil {
		pair.Close()
		return nil, err
	}
	cleanup := func() {
		pair.Close()
		c, cancel := context.WithTimeout(context.Background(), 2*time.Second)
		defer cancel()
		_ = session.End(c)
	}
	_, remote := session.Media()
	ip, _ := netip.ParseAddr(e.MediaIP)
	var answer voiceAnswer
	if remote.RTP.Addr() != ip || remote.RTCP.Addr() != ip || DecodeStrict(session.ResponseContext(), &answer) != nil || answer.Grant.ID != q.ID || len(answer.Grant.Talkgroups) == 0 || answer.Grant.Secure != q.Secure || answer.Grant.AlgorithmID != q.AlgorithmID || answer.Grant.KeyID != q.KeyID || (q.Emergency && !answer.Emergency) {
		cleanup()
		return nil, dispatch.ErrPermission
	}
	q.Emergency = answer.Emergency
	c := v.newCall(e.PeerID, peer, q, answer.Grant, receive, true, origin, pair)
	c.sip = session
	if err = v.add(c); err != nil {
		cleanup()
		return nil, err
	}
	go c.run()
	return c, nil
}
func (v *voiceRuntime) add(c *voiceCall) error {
	v.mu.Lock()
	defer v.mu.Unlock()
	if v.closed || len(v.calls) >= 64 {
		return dispatch.ErrUnavailable
	}
	v.calls[c] = true
	v.wg.Add(1)
	return nil
}
func (v *voiceRuntime) busy() bool { v.mu.Lock(); defer v.mu.Unlock(); return len(v.calls) > 0 }
func (v *voiceRuntime) cancelPeer(id string) {
	v.mu.Lock()
	list := []*voiceCall{}
	for c := range v.calls {
		if c.peerID == id {
			list = append(list, c)
		}
	}
	v.mu.Unlock()
	for _, c := range list {
		c.cancel()
	}
}
func (v *voiceRuntime) Close() {
	v.mu.Lock()
	v.closed = true
	for c := range v.calls {
		c.cancel()
	}
	v.mu.Unlock()
	_ = v.node.Close()
	_ = v.listener.Close()
	v.wg.Wait()
}

func (v *voiceRuntime) peerBusy(id string) bool {
	v.mu.Lock()
	defer v.mu.Unlock()
	for c := range v.calls {
		if c.peerID == id {
			return true
		}
	}
	return false
}

// SourceReady binds observed/generated voice fields on the existing allocation.
// E/P and the immutable source/group belong to the accepted SIP request.
func (v *voiceRuntime) sourceReady(ctx context.Context, s *sipvoice.Session, source sipvoice.Source) error {
	v.mu.Lock()
	var call *voiceCall
	for c := range v.calls {
		if c.sip == s {
			call = c
			break
		}
	}
	v.mu.Unlock()
	if call == nil || call.source {
		return dispatch.ErrPermission
	}
	return call.bind(ctx, source)
}

// A live receiving allocation is checked by its Site's actual native lease and
// current RF interest. Management report age is an admission hint, not a revoke.
func (v *voiceRuntime) liveGroups(site, logical string) []int {
	v.mu.Lock()
	defer v.mu.Unlock()
	for c := range v.calls {
		if c.source && !c.receive && c.peer.Role == RoleSite && c.peer.InstanceID == site && c.request.LogicalCallID == logical && c.ctx.Err() == nil {
			return slices.Clone(c.grant.Talkgroups)
		}
	}
	return nil
}

// Console Header NID0 is the explicit virtual-source generation policy. A
// receiving Site supplies its own configured CAI NAC/HDU DUID0. Observed RF
// headers are never stamped or inferred from their numeric NID value.
func (s *Service) stampGeneratedVoiceHeader(frame *dispatch.Frame) error {
	if frame.SourceKind == "rf" {
		return nil
	}
	if frame.SourceKind != "application" && frame.SourceKind != "automation" {
		return dispatch.ErrP25MetadataNotSupported
	}
	zone := s.view().Zone
	if zone == nil {
		return dispatch.ErrP25MetadataNotSupported
	}
	for _, site := range zone.Sites {
		if site.ID == s.Config().InstanceID {
			frame.P25 = frame.P25.Clone()
			frame.P25.Header.NID = uint16(site.NAC << 4)
			return nil
		}
	}
	return dispatch.ErrP25MetadataNotSupported
}

func (v *voiceRuntime) renew(ctx context.Context, session *sipvoice.Session) error {
	v.mu.Lock()
	var call *voiceCall
	for c := range v.calls {
		if c.sip == session {
			call = c
			break
		}
	}
	v.mu.Unlock()
	if call == nil || call.source || call.ctx.Err() != nil {
		return dispatch.ErrPermission
	}
	if call.receive {
		if !v.s.dispatchReceiveAllowed(call.request.Talkgroups[0]) {
			return dispatch.ErrPermission
		}
		if v.s.mediaClient == nil || v.s.mediaClient.endpointPeer() != call.peerID {
			return dispatch.ErrPermission
		}
		return nil
	}
	return v.s.broker.Renew(ctx, call.peerID, call.grant)
}
