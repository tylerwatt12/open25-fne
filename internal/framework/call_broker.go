package framework

import (
	"context"
	"errors"
	"fmt"
	"open25.local/open25-fne/internal/p25crypto"
	"slices"
	"strings"
	"sync"
	"time"

	"open25.local/open25-fne/internal/activity"
	"open25.local/open25-fne/internal/dispatch"
	"open25.local/open25-fne/internal/routing"
)

type callBroker struct {
	s         *Service
	router    *routing.Router
	mu        sync.Mutex
	rfRetired []string
	rfCalls   map[string]*rfStream
	owners    map[string]string
	epoch     uint64
	paused    bool
	closed    bool
	ctx       context.Context
	cancel    context.CancelFunc
	wg        sync.WaitGroup
	stopMu    sync.Mutex
}
type rfStream struct {
	media                   sync.Mutex // Serializes media admission with retirement of this stream.
	request                 routing.Request
	local                   dispatch.Grant
	remoteCall              *voiceCall
	pending                 []dispatch.Frame
	sourceEpoch             uint64
	remote                  dispatch.Grant
	remoteURL               string
	remoteGeneration        uint64
	lastFrame               time.Time
	receiveUntil            time.Time
	receiveApplicationID    string
	receiveHeader           dispatch.Frame
	lastMediaAt             time.Time
	lastNativeVoiceSequence uint64
	lastReceiveControl      uint64
	ready                   bool
}

func newCallBroker(s *Service) *callBroker {
	b := &callBroker{s: s, rfCalls: map[string]*rfStream{}, owners: map[string]string{}}
	parent := s.operationCtx
	if parent == nil {
		parent = context.Background()
	}
	b.ctx, b.cancel = context.WithCancel(parent)
	b.router = routing.New(routing.Options{Authorize: b.authorize, Targets: b.targets, Policy: b.callPolicy, Expand: b.expandPatch, EndSpurt: func(q routing.Request) { s.peers.EndSpurt(q) }, Publish: func(request routing.Request, f dispatch.Frame) {
		// Keep the stream/allocation identity used by codecs and recordings;
		// separately publish its authoritative conversation for history links.
		f.LogicalCallID = request.LogicalCallID
		f.SourceKind = request.SourceKind
		// Caller names come from the accepted System directory, never the
		// microphone or an upstream media frame's supplied display name.
		f.SourceAlias = ""
		for _, known := range s.view().System.Radios {
			if known.RID == f.SourceID {
				f.SourceAlias = known.Alias
				break
			}
		}
		s.peers.Publish(request, f)
	}, Event: func(r routing.Request, kind, message string) {
		if kind == "call_end" {
			b.mu.Lock()
			delete(b.owners, r.ID)
			b.mu.Unlock()
		}
		for _, tg := range r.Talkgroups {
			s.emit(activity.Event{RadioID: r.RadioID, TalkgroupID: tg, CallID: r.LogicalCallID, Type: kind, Message: message, Audience: "dispatcher", SourceKind: r.SourceKind, Outcome: "accepted", Details: map[string]any{"allocation_id": r.ID}})
		}
	}})
	return b
}
func (b *callBroker) authorize(q routing.Request) error {
	if err := b.authorizePolicy(q); err != nil {
		return err
	}
	if q.SourceKind == "rf" && !q.ZoneAuthorized {
		if b.s.Config().Role == RoleZone {
			if b.s.rfAuthority == nil || b.s.rfAuthority.ctx.Err() != nil {
				return dispatch.ErrUnavailable
			}
			state := b.s.rfAuthority.value.Load()
			owner, known := state.owners[q.RadioID]
			known = known && !state.fenced[q.RadioID]
			groups := q.AuthorizationGroups
			if len(groups) == 0 {
				groups = q.Talkgroups
			}
			if !known || len(groups) != 1 || owner.SiteID != q.OriginSite || owner.TGID != groups[0] {
				return dispatch.ErrPermission
			}
			if !state.document.NativeRuns[q.OriginSite].Equal(q.RFSource.NativeStartedAt) {
				return dispatch.ErrPermission
			}
		} else if b.s.Config().Role == RoleSite {
			groups := q.AuthorizationGroups
			if len(groups) == 0 {
				groups = q.Talkgroups
			}
			if b.s.radio == nil || len(groups) != 1 || !b.s.radio.SourceAllowed(q.RadioID, groups[0], q.RFSource.NativeStartedAt) {
				return dispatch.ErrPermission
			}
		}
	}
	return nil
}
func (b *callBroker) authorizePolicy(q routing.Request) error {
	v := b.s.view()
	radioOK := false
	kind := ""
	for _, r := range v.System.Radios {
		if r.RID == q.RadioID && r.Enabled {
			radioOK = true
			kind = r.Kind
		}
	}
	if !radioOK {
		return dispatch.ErrPermission
	}
	if kind == "subscriber" && b.s.radioServiceInhibited(q.RadioID) {
		return dispatch.ErrPermission
	}
	current, err := b.s.prepareCall(q.CallRequest, q.SourceKind, q.ZoneAuthorized)
	if err != nil || current.Secure != q.Secure || (current.KeyID != q.KeyID || current.AlgorithmID != q.AlgorithmID) {
		return dispatch.ErrPermission
	}
	checkGroups := q.Talkgroups
	if len(q.AuthorizationGroups) > 0 && !q.ZoneAuthorized {
		checkGroups = q.AuthorizationGroups
		expanded, err := b.s.expandPatchedGroups(checkGroups)
		if err != nil {
			return err
		}
		for _, tg := range q.Talkgroups {
			if !slices.Contains(expanded, tg) {
				return dispatch.ErrPermission
			}
		}
	}
	for _, id := range q.Talkgroups {
		allowed := false
		for _, g := range v.System.Talkgroups {
			if g.TGID == id && g.Enabled {
				allowed = true
			}
		}
		if !allowed {
			return dispatch.ErrPermission
		}
		if b.s.Config().Role == RoleSite && !groupAtSite(v.System, id, b.s.Config().InstanceID) {
			return dispatch.ErrPermission
		}
	}
	if q.ZoneAuthorized {
		if b.s.Config().Role != RoleSite {
			return dispatch.ErrPermission
		}
		if q.SourceKind == "rf" {
			if kind != "subscriber" || q.OriginSite == "" || q.OriginSite == b.s.Config().InstanceID || len(q.AuthorizationGroups) != 1 {
				return dispatch.ErrPermission
			}
			return nil
		}
		if q.SourceKind == "automation" {
			if kind != "automation" || q.UserID != "" || q.AutomationID == "" || (q.AutomationAction != "wav" && q.AutomationAction != "tts") || q.Proof == "" {
				return dispatch.ErrPermission
			}
			return nil
		}
		if q.SourceKind != "application" || q.AutomationID != "" || (kind != "console" && kind != "mobile") || q.UserID == "" || q.Proof == "" {
			return dispatch.ErrPermission
		}
		return nil
	}
	if q.SourceKind == "rf" {
		if kind != "subscriber" || q.UserID != "" || q.AutomationID != "" || q.AutomationAction != "" || q.OriginSite == "" || len(checkGroups) != 1 {
			return dispatch.ErrPermission
		}
		// Only the selected source group requires direct subscriber permission.
		// The current, operator-authorized patch owns its destination fanout.
		if !subscriberGroupAllowed(v.System, q.RadioID, checkGroups[0], true) {
			return dispatch.ErrPermission
		}
		if b.s.Config().Role == RoleZone && (v.Zone == nil || !slices.ContainsFunc(v.Zone.Sites, func(site SiteIdentity) bool { return site.ID == q.OriginSite }) || !groupAtSite(v.System, checkGroups[0], q.OriginSite)) {
			return dispatch.ErrPermission
		}
		return nil
	}
	if q.SourceKind == "automation" {
		if kind != "automation" || !b.s.automationCallAllowed(q.CallRequest, q.Proof, checkGroups) {
			return dispatch.ErrPermission
		}
		return nil
	}
	if q.SourceKind != "application" || q.AutomationID != "" || q.AutomationAction != "" {
		return dispatch.ErrPermission
	}
	u, ok := v.Users[q.UserID]
	if !ok || !u.User.Enabled || userProof(u.User, u.PasswordHash) != q.Proof || !slices.Contains(u.User.Permissions, "ptt") {
		return dispatch.ErrPermission
	}
	effective, _, err := resolveUserRadio(u.User, v.System, q.RadioID)
	if err != nil {
		return dispatch.ErrPermission
	}
	for _, tg := range checkGroups {
		if !userAllows(effective, tg) {
			return dispatch.ErrPermission
		}
	}
	return nil
}
func (b *callBroker) targets(q routing.Request) routing.Selection {
	s := b.s
	c := s.Config()
	selection := routing.Selection{Excluded: map[string]string{}}
	out := []routing.Target{}
	if c.Role == RoleSite {
		selection.Reason = "Site RF audio routing is unavailable."
		router := s.rfRouter.Load()
		if router != nil {
			groups := slices.Clone(q.Talkgroups)
			if q.SourceKind == "rf" && q.OriginSite == c.InstanceID {
				groups = slices.DeleteFunc(groups, func(tg int) bool { return slices.Contains(q.AuthorizationGroups, tg) })
			}
			if len(groups) > 0 {
				out = append(out, routing.Target{ID: c.InstanceID, Router: router, Groups: groups})
			} else {
				selection.Reason = "Source Site has no additional receiving talkgroups."
			}
		}
		selection.Targets = out
		return selection
	}
	if c.Role != RoleZone {
		selection.Reason = "This service does not own receiving Site routes."
		selection.Targets = out
		return selection
	}
	v := s.view()
	if v.Zone == nil {
		selection.Reason = "Zone directory is unavailable."
		return selection
	}
	if s.rfAuthority == nil || s.rfAuthority.ctx.Err() != nil {
		selection.Reason = "RF source authority is unavailable."
		return selection
	}
	selection.Reason = "Site is no longer configured for this Zone."
	authority := s.rfAuthority.value.Load()
	for _, identity := range v.Zone.Sites {
		if identity.ID == q.OriginSite {
			selection.Excluded[identity.ID] = "Source Site is excluded from receiving its own transmission."
			continue
		}
		groups := []int{}
		reasons := []string{}
		for _, tg := range q.Talkgroups {
			if !groupAtSite(v.System, tg, identity.ID) {
				reasons = append(reasons, fmt.Sprintf("TG%d policy does not enable this Site.", tg))
				continue
			}
			interest := false
			matching, ownerMismatch, inhibited, denied := 0, 0, 0, 0
			for rid, owner := range authority.owners {
				if owner.TGID != tg || owner.SiteID != identity.ID {
					continue
				}
				matching++
				if authority.fenced[rid] {
					ownerMismatch++
					continue
				}
				blocked := s.radioServiceInhibited(rid)
				if blocked {
					inhibited++
				} else if subscriberGroupAllowed(v.System, rid, tg, false) {
					interest = true
				} else {
					denied++
				}
			}
			if interest {
				groups = append(groups, tg)
			} else {
				reasons = append(reasons, fmt.Sprintf("TG%d has no eligible RF affiliation (matching %d; owner mismatch %d; inhibited %d; subscriber denied %d).", tg, matching, ownerMismatch, inhibited, denied))
			}
		}
		if len(reasons) > 0 {
			selection.Excluded[identity.ID] = strings.Join(reasons, " ")
		}
		if len(groups) > 0 {
			peerID, peer, err := s.peerIdentityForSite(identity.ID)
			if err == nil {
				out = append(out, routing.Target{ID: identity.ID, Router: &siteLeg{s: s, id: identity.ID, peerID: peerID, address: identity.Endpoint, peer: peer, request: q}, Groups: groups})
			} else {
				selection.Excluded[identity.ID] = "Site peer credentials are unavailable: " + err.Error()
			}
		}
	}
	selection.Targets = out
	return selection
}
func groupAtSite(system SystemConfig, tg int, id string) bool {
	for _, g := range system.Talkgroups {
		if g.TGID == tg && g.Enabled {
			return talkgroupAtSite(g, id)
		}
	}
	return false
}

func talkgroupAtSite(group Talkgroup, id string) bool {
	return group.Enabled && (group.SiteAccess == "all" || slices.Contains(group.SiteIDs, id))
}
func (b *callBroker) Begin(ctx context.Context, owner string, q routing.Request) (dispatch.Grant, error) {
	b.s.patchMu.RLock()
	defer b.s.patchMu.RUnlock()
	trustedZone := false
	if credentials, err := readCredentials(b.s.Config().CredentialFile); err == nil {
		peer := credentials.Peers[owner]
		zone := b.s.view().Zone
		trustedZone = b.s.Config().Role == RoleSite && peer.Role == RoleZone && zone != nil && peer.InstanceID == zone.ID
		if b.s.Config().Role == RoleSite && peer.Role == RoleZone && !trustedZone {
			return dispatch.Grant{}, dispatch.ErrPermission
		}
	}
	q.ZoneAuthorized = trustedZone
	if !trustedZone {
		// Originating applications and RF Sites own their new conversation ID.
		// An upstream sender cannot attach its call to someone else's history.
		q.LogicalCallID = q.ID
		q.AuthorizationGroups = slices.Clone(q.Talkgroups)
	} else if q.LogicalCallID == "" || len(q.LogicalCallID) > 128 {
		return dispatch.Grant{}, dispatch.ErrPermission
	}
	var err error
	q.CallRequest, err = b.s.prepareCall(q.CallRequest, q.SourceKind, trustedZone)
	if err != nil {
		return dispatch.Grant{}, err
	}
	b.mu.Lock()
	if b.closed || b.paused {
		b.mu.Unlock()
		return dispatch.Grant{}, dispatch.ErrUnavailable
	}
	epoch := b.epoch
	b.mu.Unlock()
	g, err := b.router.Begin(ctx, owner, q)
	if err != nil {
		return g, err
	}
	// Recheck after all resource/lock waits, immediately before publication.
	b.mu.Lock()
	if err = ctx.Err(); err == nil {
		err = b.authorize(q)
	}
	if err == nil && (epoch != b.epoch || b.closed || b.paused) {
		err = dispatch.ErrUnavailable
	}
	if err != nil {
		b.mu.Unlock()
		_ = b.router.End(context.Background(), owner, g)
		return dispatch.Grant{}, err
	}
	b.owners[g.ID] = owner
	b.mu.Unlock()
	return g, nil
}
func (b *callBroker) Write(ctx context.Context, owner string, g dispatch.Grant, f dispatch.Frame) error {
	return b.router.Write(ctx, owner, g, f)
}
func (b *callBroker) EndSpurt(ctx context.Context, owner string, g dispatch.Grant) error {
	return b.router.EndSpurt(ctx, owner, g)
}
func (b *callBroker) Gap(ctx context.Context, owner string, g dispatch.Grant) error {
	return b.router.Gap(ctx, owner, g)
}
func (b *callBroker) Renew(ctx context.Context, owner string, g dispatch.Grant) error {
	return b.router.Renew(ctx, owner, g)
}
func (b *callBroker) End(ctx context.Context, owner string, g dispatch.Grant) error {
	err := b.router.End(ctx, owner, g)
	if err == nil {
		b.mu.Lock()
		delete(b.owners, g.ID)
		b.mu.Unlock()
	}
	return err
}
func (b *callBroker) CancelOwner(owner string) {
	b.router.CancelOwner(owner)
	b.mu.Lock()
	for id, known := range b.owners {
		if known == owner {
			delete(b.owners, id)
		}
	}
	b.mu.Unlock()
}
func (b *callBroker) CancelAll() {
	b.stopMu.Lock()
	defer b.stopMu.Unlock()
	b.mu.Lock()
	b.epoch++
	b.paused = true
	streams := []*rfStream{}
	for _, stream := range b.rfCalls {
		streams = append(streams, stream)
	}
	b.mu.Unlock()
	for _, stream := range streams {
		b.s.endRFStream(stream)
	}
	// The routing table also includes admissions still awaiting a Site grant;
	// they may not yet have an entry in owners.
	b.router.CancelAll()
	b.mu.Lock()
	b.owners = map[string]string{}
	b.paused = false
	b.mu.Unlock()
}
func (b *callBroker) Close() {
	b.mu.Lock()
	b.closed = true
	if b.cancel != nil {
		b.cancel()
	}
	b.mu.Unlock()
	b.CancelAll()
	_ = b.router.Close()
	b.wg.Wait()
}

type siteLeg struct {
	s                   *Service
	id, address, peerID string
	peer                PeerCredential
	request             routing.Request
	mu                  sync.Mutex
	call                *voiceCall
}

func (l *siteLeg) Begin(ctx context.Context, q dispatch.CallRequest) (dispatch.Grant, error) {
	r := l.request
	r.CallRequest = q
	endpoint, err := l.s.voice.Load().discover(ctx, l.address, &l.peer)
	if err != nil {
		return dispatch.Grant{}, err
	}
	call, err := l.s.voice.Load().begin(ctx, endpoint, r, false)
	if err != nil {
		return dispatch.Grant{}, err
	}
	l.mu.Lock()
	l.call = call
	l.mu.Unlock()
	return call.grant, nil
}
func (l *siteLeg) current(g dispatch.Grant) *voiceCall {
	l.mu.Lock()
	defer l.mu.Unlock()
	if l.call != nil && l.call.grant.ID == g.ID {
		return l.call
	}
	return nil
}
func (l *siteLeg) Write(ctx context.Context, g dispatch.Grant, f dispatch.Frame) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	c := l.current(g)
	if c == nil {
		return dispatch.ErrUnavailable
	}
	return c.Write(f.WithGroup(c.request.Talkgroups[0]))
}
func (l *siteLeg) EndSpurt(ctx context.Context, g dispatch.Grant) error {
	if c := l.current(g); c != nil {
		return c.EndSpurt(ctx)
	}
	return dispatch.ErrUnavailable
}
func (l *siteLeg) Renew(ctx context.Context, g dispatch.Grant) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	if c := l.current(g); c != nil {
		return c.Renew()
	}
	return dispatch.ErrUnavailable
}
func (l *siteLeg) End(ctx context.Context, g dispatch.Grant) error {
	if c := l.current(g); c != nil {
		return c.End(ctx)
	}
	return nil
}

type mediaRoute struct {
	Call           *voiceCall
	VoiceMFID      uint8
	Codec          *dispatch.PacketCodec
	Address        string
	Generation     uint64
	Secure         bool
	ServiceOptions uint8
	AlgorithmID    int
	KeyID          int
}

type networkRouter struct {
	service     *Service
	mu          sync.Mutex
	routes      map[string]mediaRoute
	audioRoutes map[string]audioIdentityRoute
}

func (n *networkRouter) Begin(ctx context.Context, q dispatch.CallRequest) (dispatch.Grant, error) {
	s := n.service
	address, generation := s.mediaClient.Link()
	if address == "" {
		return dispatch.Grant{}, dispatch.ErrUnavailable
	}
	var securityErr error
	kind := "application"
	if q.AutomationID != "" {
		kind = "automation"
	}
	q, securityErr = s.prepareTransmit(q)
	if securityErr == nil {
		q, securityErr = s.prepareCall(q, kind, false)
	}
	if securityErr != nil {
		return dispatch.Grant{}, securityErr
	}
	principal := dispatch.Principal{UserID: q.UserID, AutomationID: q.AutomationID, AutomationAction: q.AutomationAction, RadioID: q.RadioID, CanTransmit: true, CanSecure: q.Secure, TransmitGroups: q.Talkgroups}
	if err := n.ensureAudioIdentity(ctx, principal, q.SessionID); err != nil {
		return dispatch.Grant{}, err
	}
	if current, gen := s.mediaClient.Link(); current != address || gen != generation {
		return dispatch.Grant{}, dispatch.ErrUnavailable
	}
	voiceMFID := s.view().System.VoiceMFID
	if voiceMFID != 0 && voiceMFID != 1 {
		return dispatch.Grant{}, dispatch.ErrPermission
	}
	codec, keys := s.packetCodec()
	if codec == nil || !codec.Alive() {
		return dispatch.Grant{}, dispatch.ErrUnavailable
	}
	if q.Secure {
		if codec == nil || !keys[p25crypto.Pair(q.AlgorithmID, q.KeyID)] {
			return dispatch.Grant{}, errors.New("the encryption key is not available at Dispatch")
		}
	}
	actor, actorErr := s.applicationAudioActor(principal)
	if actorErr != nil {
		return dispatch.Grant{}, actorErr
	}
	request := routing.Request{CallRequest: q, Proof: actor.Proof, SourceKind: kind}
	endpoint, err := s.mediaClient.endpointAt(generation)
	if err != nil {
		return dispatch.Grant{}, err
	}
	call, err := s.voice.Load().begin(ctx, endpoint, request, false)
	if err != nil {
		return dispatch.Grant{}, err
	}
	grant := call.grant
	current, currentGeneration := s.mediaClient.Link()
	if current != address || currentGeneration != generation || !s.currentPacketCodec(codec) {
		cleanup, cancel := context.WithTimeout(context.Background(), 2*time.Second)
		defer cancel()
		_ = call.End(cleanup)
		return dispatch.Grant{}, dispatch.ErrUnavailable
	}
	n.mu.Lock()
	if n.routes == nil {
		n.routes = map[string]mediaRoute{}
	}
	n.routes[grant.ID] = mediaRoute{Call: call, VoiceMFID: uint8(voiceMFID), Codec: codec, Address: address, Generation: generation, Secure: q.Secure, ServiceOptions: generatedVoiceService(call.request.Emergency, q.Secure), AlgorithmID: q.AlgorithmID, KeyID: q.KeyID}
	n.mu.Unlock()
	return grant, nil
}

func (n *networkRouter) route(g dispatch.Grant) mediaRoute {
	n.mu.Lock()
	defer n.mu.Unlock()
	return n.routes[g.ID]
}
func (n *networkRouter) Write(ctx context.Context, g dispatch.Grant, f dispatch.Frame) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	route := n.route(g)
	address, generation := n.service.mediaClient.Link()
	if route.Address == "" || route.Address != address || route.Generation != generation {
		return dispatch.ErrUnavailable
	}
	if !n.service.currentPacketCodec(route.Codec) {
		_ = n.End(ctx, g)
		return errAudioProcessChanged
	}
	f.SourceEpoch = 1
	f.ManufacturerID = route.VoiceMFID
	f.ServiceOptions = route.ServiceOptions
	f.Emergency = route.ServiceOptions&0x80 != 0
	if codec := route.Codec; codec != nil {
		if len(g.Talkgroups) == 0 {
			return dispatch.ErrPermission
		}
		f.TalkgroupID = g.Talkgroups[0]
		f.AlgorithmID = 128
		f.KeyID = 0
		f.Encrypted = route.Secure
		if route.Secure {
			f.AlgorithmID = uint8(route.AlgorithmID)
			f.KeyID = uint16(route.KeyID)
		}
		var err error
		f, err = codec.Encode(ctx, f)
		if err != nil {
			if !n.service.currentPacketCodec(route.Codec) {
				_ = n.End(ctx, g)
			}
			return err
		}
	} else if route.Secure {
		return dispatch.ErrUnavailable
	}
	if !n.service.currentPacketCodec(route.Codec) {
		_ = n.End(ctx, g)
		return errAudioProcessChanged
	}
	if n.route(g) != route {
		return dispatch.ErrUnavailable
	}
	f.SourceEpoch = 1
	return route.Call.Write(f)
}
func (n *networkRouter) Renew(ctx context.Context, g dispatch.Grant) error {
	route := n.route(g)
	address, generation := n.service.mediaClient.Link()
	if route.Address == "" || route.Address != address || route.Generation != generation {
		return dispatch.ErrUnavailable
	}
	if !n.service.currentPacketCodec(route.Codec) {
		_ = n.End(ctx, g)
		return errAudioProcessChanged
	}
	return route.Call.Renew()
}
func (n *networkRouter) End(ctx context.Context, g dispatch.Grant) error {
	n.mu.Lock()
	route := n.routes[g.ID]
	delete(n.routes, g.ID)
	n.mu.Unlock()
	if route.Address == "" {
		return nil
	}
	var mediaErr error
	// Only the process admitted with this call may finish it. A replacement
	// must never generate a fresh MI or silence padding for the old grant.
	if codec := route.Codec; codec != nil {
		if n.service.currentPacketCodec(codec) {
			if frames, finishErr := codec.Finish(ctx, g.ID); finishErr == nil {
				for _, frame := range frames {
					if !n.service.currentPacketCodec(codec) {
						mediaErr = dispatch.ErrUnavailable
						break
					}
					frame.SourceEpoch = 1
					if err := route.Call.Write(frame); err != nil {
						mediaErr = err
						break
					}
				}
			} else {
				mediaErr = finishErr
			}
		}
		mediaErr = errors.Join(mediaErr, codec.End(ctx, g.ID))
	}
	err := route.Call.End(ctx)
	return errors.Join(mediaErr, err)
}

func (s *Service) codecFrame(f dispatch.Frame) {
	if s.broker == nil {
		return
	}
	s.broker.mu.Lock()
	epoch := s.broker.epoch
	s.broker.mu.Unlock()
	s.codecFrameAtEpoch(f, epoch)
}
func (s *Service) codecFrameAtEpoch(f dispatch.Frame, epoch uint64) {
	if !s.codecReceiveEpoch(epoch) || !f.MediaValid() || s.radio == nil {
		return
	}
	applicationID, nativeSequence := f.CallID, f.Sequence
	physical := slices.ContainsFunc(s.view().System.Radios, func(r Radio) bool { return r.RID == f.SourceID && r.Enabled && r.Kind == "subscriber" })
	if !physical || s.radioServiceInhibited(f.SourceID) || !subscriberGroupAllowed(s.view().System, f.SourceID, f.TalkgroupID, true) {
		return
	}
	if s.rfAdmissions != nil {
		var accepted bool
		f, accepted = s.rfAdmissions.bind(f)
		if !accepted {
			return
		}
	}
	b := s.broker
	b.mu.Lock()
	if b.closed || b.paused || b.epoch != epoch || slices.Contains(b.rfRetired, f.CallID) {
		b.mu.Unlock()
		return
	}
	stream := b.rfCalls[f.CallID]
	if stream == nil && !f.End {
		request := routing.Request{CallRequest: dispatch.CallRequest{ID: f.CallID, SessionID: "rf:" + s.Config().InstanceID, RadioID: f.SourceID, Talkgroups: []int{f.TalkgroupID}, Emergency: f.Emergency, Secure: f.Encrypted, AlgorithmID: int(f.AlgorithmID), KeyID: int(f.KeyID)}, SourceKind: "rf", OriginSite: s.Config().InstanceID}
		stream = &rfStream{request: request, lastFrame: time.Now()}
		b.rfCalls[f.CallID] = stream
		b.wg.Add(1)
		go func() { defer b.wg.Done(); s.beginRFStream(stream) }()
	}
	if stream != nil {
		stream.lastFrame = time.Now()
		if !f.End {
			if stream.receiveApplicationID != applicationID {
				stream.lastReceiveControl = 0
				stream.sourceEpoch++
			}
			stream.receiveApplicationID = applicationID
			stream.lastMediaAt, stream.lastNativeVoiceSequence = f.CapturedAt, nativeSequence
			stream.receiveHeader = dispatch.Frame{ManufacturerID: f.ManufacturerID, Emergency: f.Emergency, Encrypted: f.Encrypted, AlgorithmID: f.AlgorithmID, KeyID: f.KeyID}
			stream.receiveUntil = time.Time{}
		}
	}
	if stream == nil {
		b.mu.Unlock()
		return
	}
	if f.End {
		b.wg.Add(1)
		b.mu.Unlock()
		go func() { defer b.wg.Done(); s.endRFStream(stream) }()
		return
	}
	f.SourceEpoch = stream.sourceEpoch
	ready := stream.ready
	if !ready {
		if len(stream.pending) >= 12 {
			b.wg.Add(1)
			b.mu.Unlock()
			go func() { defer b.wg.Done(); s.endRFStream(stream) }()
			return
		}
		f.Encoded = slices.Clone(f.Encoded)
		f.P25 = f.P25.Clone()
		stream.pending = append(stream.pending, f)
		b.mu.Unlock()
		return
	}
	b.mu.Unlock()
	stream.media.Lock()
	defer stream.media.Unlock()
	b.mu.Lock()
	active := b.rfCalls[f.CallID] == stream && !b.closed && !b.paused
	b.mu.Unlock()
	if !active {
		return
	}
	ctx, cancel := context.WithTimeout(b.ctx, 200*time.Millisecond)
	defer cancel()
	s.writeRFFrame(ctx, stream, f)
}
func (s *Service) writeRFFrame(ctx context.Context, stream *rfStream, f dispatch.Frame) {
	f = f.WithEmergency(stream.request.Emergency)
	f.SourceKind = "rf"
	if err := s.broker.Write(ctx, "rf:"+s.Config().InstanceID, stream.local, f); err != nil {
		return
	}
	if stream.remoteCall != nil {
		_ = stream.remoteCall.Write(f)
	}
}
func (s *Service) beginRFStream(stream *rfStream) {
	b := s.broker
	ctx, cancel := context.WithTimeout(b.ctx, 2*time.Second)
	defer cancel()
	owner := "rf:" + s.Config().InstanceID
	q := stream.request
	var err error
	q.CallRequest, err = s.prepareCall(q.CallRequest, q.SourceKind, false)
	if err == nil {
		q, err = s.observeRFSource(q)
	}
	if err != nil {
		s.endRFStream(stream)
		return
	}
	local, err := b.Begin(ctx, owner, q)
	if err != nil {
		s.endRFStream(stream)
		s.runtimeError(err)
		return
	}
	var call *voiceCall
	address := ""
	var generation uint64
	if s.mediaClient != nil {
		address, generation = s.mediaClient.Link()
		if address != "" {
			if endpoint, e := s.mediaClient.endpointAt(generation); e == nil {
				call, _ = s.voice.Load().begin(ctx, endpoint, q, false)
			}
		}
	}
	stream.media.Lock()
	b.mu.Lock()
	active := b.rfCalls[q.ID] == stream && !b.closed && !b.paused && b.ctx.Err() == nil
	if active {
		stream.request = q
		stream.local = local
		stream.remoteCall = call
		if call != nil {
			stream.remote = call.grant
			stream.remoteURL = address
			stream.remoteGeneration = generation
		}
		stream.ready = true
	}
	pending := stream.pending
	stream.pending = nil
	b.mu.Unlock()
	if active {
		for _, f := range pending {
			if time.Since(f.CapturedAt) > 240*time.Millisecond {
				active = false
				break
			}
			s.writeRFFrame(ctx, stream, f)
		}
	}
	stream.media.Unlock()
	if !active {
		cleanup, done := context.WithTimeout(context.Background(), 2*time.Second)
		defer done()
		if call != nil {
			_ = call.End(cleanup)
		}
		_ = b.End(cleanup, owner, local)
		s.endRFStream(stream)
		return
	}
	b.mu.Lock()
	if !b.closed {
		b.wg.Add(1)
		go func() { defer b.wg.Done(); s.renewRFStream(stream) }()
	}
	b.mu.Unlock()
}
func (s *Service) renewRFStream(stream *rfStream) {
	ticker := time.NewTicker(400 * time.Millisecond)
	defer ticker.Stop()
	for {
		select {
		case <-s.broker.ctx.Done():
			return
		case <-ticker.C:
			b := s.broker
			b.mu.Lock()
			current := b.rfCalls[stream.request.ID]
			stale := time.Since(stream.lastFrame) > time.Second && !time.Now().Before(stream.receiveUntil)
			local, call := stream.local, stream.remoteCall
			b.mu.Unlock()
			if current != stream {
				return
			}
			if stale {
				s.endRFStream(stream)
				return
			}
			ctx, cancel := context.WithTimeout(b.ctx, 500*time.Millisecond)
			err := b.Renew(ctx, "rf:"+s.Config().InstanceID, local)
			cancel()
			if err != nil {
				s.endRFStream(stream)
				return
			}
			if call != nil && call.Renew() != nil {
				stream.media.Lock()
				b.mu.Lock()
				if stream.remoteCall == call {
					stream.remoteCall = nil
					stream.remote = dispatch.Grant{}
					stream.remoteURL = ""
					stream.remoteGeneration = 0
				}
				b.mu.Unlock()
				stream.media.Unlock()
			}
		}
	}
}
func (s *Service) endRFStream(stream *rfStream) {
	if stream == nil {
		return
	}
	b := s.broker
	stream.media.Lock()
	b.mu.Lock()
	if b.rfCalls[stream.request.ID] != stream {
		b.mu.Unlock()
		stream.media.Unlock()
		return
	}
	delete(b.rfCalls, stream.request.ID)
	b.rfRetired = append(b.rfRetired, stream.request.ID)
	if len(b.rfRetired) > 64 {
		b.rfRetired = b.rfRetired[len(b.rfRetired)-64:]
	}
	local, call := stream.local, stream.remoteCall
	stream.pending = nil
	b.mu.Unlock()
	stream.media.Unlock()
	var retiring sync.WaitGroup
	if call != nil {
		retiring.Add(1)
		go func() {
			defer retiring.Done()
			ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
			defer cancel()
			s.runtimeError(call.End(ctx))
		}()
	}
	if local.ID != "" {
		ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
		s.runtimeError(b.End(ctx, "rf:"+s.Config().InstanceID, local))
		cancel()
	}
	retiring.Wait()
}
func (s *Service) retireRFRemote(call *voiceCall) {
	if call == nil {
		return
	}
	b := s.broker
	b.mu.Lock()
	if b.closed {
		b.mu.Unlock()
		call.cancel()
		return
	}
	b.wg.Add(1)
	b.mu.Unlock()
	go func() {
		defer b.wg.Done()
		ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
		defer cancel()
		s.runtimeError(call.End(ctx))
	}()
}

// Native RF End terminates an actual burst, while AllStart retains allocation.
func (s *Service) endRFSpurt(stream *rfStream) {
	if stream == nil {
		return
	}
	stream.media.Lock()
	defer stream.media.Unlock()
	b := s.broker
	b.mu.Lock()
	active := b.rfCalls[stream.request.ID] == stream
	local, remote := stream.local, stream.remoteCall
	b.mu.Unlock()
	if !active {
		return
	}
	var wg sync.WaitGroup
	if remote != nil {
		wg.Add(1)
		go func() {
			defer wg.Done()
			ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
			defer cancel()
			s.runtimeError(remote.EndSpurt(ctx))
		}()
	}
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	s.runtimeError(b.EndSpurt(ctx, "rf:"+s.Config().InstanceID, local))
	cancel()
	wg.Wait()
}

// Availability is a snapshot for new PTT only; it never ends an active owner.
func (n *networkRouter) Available() bool {
	s := n.service
	if s.mediaClient == nil {
		return false
	}
	v := s.voice.Load()
	if v == nil {
		return false
	}
	v.mu.Lock()
	closed := v.closed
	v.mu.Unlock()
	if closed {
		return false
	}
	address, _ := s.mediaClient.Link()
	if address == "" {
		return false
	}
	codec, _ := s.packetCodec()
	return codec != nil && codec.Alive()
}
