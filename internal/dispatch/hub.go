package dispatch

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"regexp"
	"slices"
	"sort"
	"sync"
	"time"
)

var requestPattern = regexp.MustCompile(`^[A-Za-z0-9_-]{1,80}$`)

type ticket struct {
	Principal Principal
	Session   [32]byte
	Expires   time.Time
}
type queuedFrame struct {
	PCM      []byte
	At       time.Time
	Sequence uint64
}
type transmission struct {
	ID, RequestID                               string
	Owner                                       *client
	Groups                                      []int
	State                                       string
	Context                                     context.Context
	Cancel                                      context.CancelFunc
	Queue                                       chan queuedFrame
	Done                                        chan struct{}
	Ready                                       chan struct{}
	ReadyOnce                                   sync.Once
	LeaseUntil, RequestedAt, GrantedAt, LastPCM time.Time
	Grant                                       Grant
	Sequence                                    uint64
	Written                                     uint64
	Error                                       string
	Secure                                      bool
	AlgorithmID, KeyID                          int
}

type Hub struct {
	mu                sync.Mutex
	options           Options
	ctx               context.Context
	cancel            context.CancelFunc
	wg                sync.WaitGroup
	closed            bool
	clients           map[string]*client
	byRadio           map[int]*client
	floors            map[int]*transmission
	calls             map[string]*transmission
	tickets           map[[32]byte]ticket
	events            chan Event
	lastReceiveAudio  time.Time
	unavailableNotice map[string]time.Time
	lastAvailable     bool
}

func New(options Options) *Hub {
	if options.Lease <= 0 {
		options.Lease = time.Second
	}
	if options.MaxTransmit <= 0 {
		options.MaxTransmit = time.Minute
	}
	if options.GrantTimeout <= 0 {
		options.GrantTimeout = 4 * time.Second
	}
	if options.AudioIdle <= 0 {
		options.AudioIdle = 750 * time.Millisecond
	}
	if options.MaxAudioAge <= 0 {
		options.MaxAudioAge = 250 * time.Millisecond
	}
	if options.MaxSessions <= 0 {
		options.MaxSessions = 32
	}
	ctx, cancel := context.WithCancel(context.Background())
	h := &Hub{options: options, ctx: ctx, cancel: cancel, clients: map[string]*client{}, byRadio: map[int]*client{}, floors: map[int]*transmission{}, calls: map[string]*transmission{}, tickets: map[[32]byte]ticket{}, events: make(chan Event, 256)}
	h.lastAvailable = h.available()
	h.wg.Add(2)
	go h.watchdog()
	go h.eventLoop()
	return h
}

func randomID() (string, error) {
	b := make([]byte, 24)
	if _, err := rand.Read(b); err != nil {
		return "", err
	}
	return hex.EncodeToString(b), nil
}
func groupList(groups []int) ([]int, error) {
	if len(groups) > MaxGroups {
		return nil, fmt.Errorf("select no more than %d talkgroups", MaxGroups)
	}
	seen := map[int]bool{}
	out := []int{}
	for _, tg := range groups {
		if tg < 1 || tg > 65534 || seen[tg] {
			return nil, errors.New("select unique talkgroups from 1 to 65534")
		}
		seen[tg] = true
		out = append(out, tg)
	}
	sort.Ints(out)
	return out, nil
}
func validPrincipal(p Principal) (Principal, error) {
	if (p.UserID == "") == (p.AutomationID == "") || len(p.UserID) > 100 || len(p.AutomationID) > 64 || p.RadioID < 1 || p.RadioID > 0xfffffb || (p.AutomationID == "" && p.AutomationAction != "") || (p.AutomationID != "" && p.AutomationAction != "wav" && p.AutomationAction != "tts") {
		return p, ErrPermission
	}
	var err error
	p.ReceiveGroups, err = groupList(p.ReceiveGroups)
	if err != nil {
		return p, err
	}
	p.TransmitGroups, err = groupList(p.TransmitGroups)
	return p, err
}

func (h *Hub) IssueTicket(p Principal, sessionKey string) (string, error) {
	var err error
	p, err = validPrincipal(p)
	if err != nil || sessionKey == "" || p.AutomationID != "" {
		return "", ErrPermission
	}
	id, err := randomID()
	if err != nil {
		return "", err
	}
	h.mu.Lock()
	defer h.mu.Unlock()
	if h.closed {
		return "", ErrUnavailable
	}
	now := time.Now()
	for k, v := range h.tickets {
		if now.After(v.Expires) {
			delete(h.tickets, k)
		}
	}
	if len(h.tickets) >= 128 {
		return "", errors.New("too many pending audio connections")
	}
	h.tickets[sha256.Sum256([]byte(id))] = ticket{p, sha256.Sum256([]byte(sessionKey)), now.Add(30 * time.Second)}
	return id, nil
}

func (h *Hub) consumeTicket(token string, p Principal, key string) error {
	h.mu.Lock()
	defer h.mu.Unlock()
	hash := sha256.Sum256([]byte(token))
	v, ok := h.tickets[hash]
	delete(h.tickets, hash)
	if !ok || h.closed || time.Now().After(v.Expires) || key == "" || v.Session != sha256.Sum256([]byte(key)) || v.Principal.UserID != p.UserID || v.Principal.RadioID != p.RadioID || !slices.Equal(v.Principal.ReceiveGroups, p.ReceiveGroups) || !slices.Equal(v.Principal.TransmitGroups, p.TransmitGroups) || v.Principal.CanTransmit != p.CanTransmit || v.Principal.CanSecure != p.CanSecure {
		return ErrPermission
	}
	return nil
}

func (h *Hub) openClient(p Principal, sessionKey string) (*client, error) {
	var err error
	p, err = validPrincipal(p)
	if err != nil || sessionKey == "" {
		return nil, ErrPermission
	}
	id, err := randomID()
	if err != nil {
		return nil, err
	}
	ctx, cancel := context.WithCancel(h.ctx)
	c := &client{id: id, principal: p, session: sha256.Sum256([]byte(sessionKey)), hub: h, ctx: ctx, cancel: cancel, send: make(chan any, 64), created: time.Now(), subscriptions: map[int]bool{}, usedRequests: map[string]bool{}, tracks: map[int]*rxTrack{}}
	h.mu.Lock()
	if h.closed || len(h.clients) >= h.options.MaxSessions {
		h.mu.Unlock()
		cancel()
		return nil, errors.New("audio session capacity is reached")
	}
	if h.byRadio[p.RadioID] != nil {
		h.mu.Unlock()
		cancel()
		return nil, errors.New("this radio ID already has an active audio session")
	}
	h.clients[id] = c
	h.byRadio[p.RadioID] = c
	if sessions, ok := h.options.Router.(AudioSessions); ok {
		h.wg.Add(1)
		h.mu.Unlock()
		ctx, done := context.WithTimeout(c.ctx, time.Second)
		err := sessions.ClaimAudio(ctx, p, id)
		done()
		if err != nil {
			h.removeClient(c)
			cleanup, cancelCleanup := context.WithTimeout(context.Background(), time.Second)
			_ = sessions.ReleaseAudio(cleanup, p, id)
			cancelCleanup()
			h.wg.Done()
			return nil, err
		}
		go h.leaseAudio(c, sessions)
	} else {
		h.mu.Unlock()
	}
	return c, nil
}
func (h *Hub) leaseAudio(c *client, sessions AudioSessions) {
	defer h.wg.Done()
	defer func() {
		ctx, done := context.WithTimeout(context.Background(), time.Second)
		defer done()
		_ = sessions.ReleaseAudio(ctx, c.principal, c.id)
	}()
	ticker := time.NewTicker(time.Second)
	defer ticker.Stop()
	lastSuccess := time.Now()
	for {
		select {
		case <-c.ctx.Done():
			return
		case <-ticker.C:
			ctx, done := context.WithTimeout(c.ctx, time.Second)
			err := sessions.RenewAudio(ctx, c.principal, c.id)
			done()
			if err != nil {
				if errors.Is(err, ErrUnavailable) && time.Since(lastSuccess) < 5*time.Second {
					continue
				}
				c.enqueue(map[string]any{"type": "error", "message": "Audio identity ownership was lost. Reconnect audio."})
				h.removeClient(c)
				return
			}
			lastSuccess = time.Now()
		}
	}
}

func (h *Hub) removeClient(c *client) {
	c.cancel()
	c.closePeer()
	h.mu.Lock()
	if h.clients[c.id] == c {
		delete(h.clients, c.id)
		if h.byRadio[c.principal.RadioID] == c {
			delete(h.byRadio, c.principal.RadioID)
		}
	}
	var tx *transmission
	for _, v := range h.calls {
		if v.Owner == c {
			tx = v
			break
		}
	}
	h.mu.Unlock()
	if tx != nil {
		h.stop(tx, "Audio session disconnected.")
	}
	h.broadcastState()
}

func (h *Hub) Overview(p Principal) State {
	p, _ = validPrincipal(p)
	h.mu.Lock()
	defer h.mu.Unlock()
	return h.stateLocked(p, nil)
}
func (h *Hub) available() bool {
	return h.options.Router != nil && (h.options.Available == nil || h.options.Available())
}

func (h *Hub) stateLocked(p Principal, c *client) State {
	s := State{RadioID: p.RadioID, ReceiveGroups: append([]int{}, p.ReceiveGroups...), TransmitGroups: append([]int{}, p.TransmitGroups...), Subscriptions: []int{}, TX: TXState{State: "idle", Talkgroups: []int{}}, Floors: []Floor{}, Available: !h.closed && h.available()}
	if c != nil {
		s.SessionID = c.id
		s.MediaConnected = c.connected.Load()
		s.EndedRequestID = c.endedRequest
		for tg := range c.subscriptions {
			s.Subscriptions = append(s.Subscriptions, tg)
		}
		sort.Ints(s.Subscriptions)
		for _, tx := range h.calls {
			if tx.Owner == c {
				s.TX = TXState{ID: tx.ID, RequestID: tx.RequestID, Talkgroups: append([]int{}, tx.Groups...), State: tx.State, Secure: tx.Grant.Secure, AlgorithmID: tx.Grant.AlgorithmID, KeyID: tx.Grant.KeyID}
				break
			}
		}
	}
	for tg, tx := range h.floors {
		if slices.Contains(p.ReceiveGroups, tg) || slices.Contains(p.TransmitGroups, tg) {
			s.Floors = append(s.Floors, Floor{tx.ID, tx.Owner.principal.RadioID, tg, tx.State})
		}
	}
	sort.Slice(s.Floors, func(i, j int) bool { return s.Floors[i].TalkgroupID < s.Floors[j].TalkgroupID })
	return s
}

func (h *Hub) sendState(c *client) {
	h.mu.Lock()
	if h.clients[c.id] == c {
		c.enqueue(map[string]any{"type": "state", "state": h.stateLocked(c.principal, c)})
	}
	h.mu.Unlock()
}
func (h *Hub) broadcastState() {
	h.mu.Lock()
	for _, c := range h.clients {
		c.enqueue(map[string]any{"type": "state", "state": h.stateLocked(c.principal, c)})
	}
	h.mu.Unlock()
}

func (h *Hub) subscribe(c *client, groups []int) error {
	groups, err := groupList(groups)
	if err != nil {
		return err
	}
	for _, tg := range groups {
		if !slices.Contains(c.principal.ReceiveGroups, tg) {
			return ErrPermission
		}
	}
	h.mu.Lock()
	if h.clients[c.id] != c {
		h.mu.Unlock()
		return ErrPermission
	}
	c.subscriptions = map[int]bool{}
	for _, tg := range groups {
		c.subscriptions[tg] = true
	}
	h.mu.Unlock()
	h.sendState(c)
	return nil
}

func (h *Hub) start(c *client, groups []int, requestID string, file, secure bool, algorithmID, keyID int) (*transmission, error) {
	if !requestPattern.MatchString(requestID) {
		return nil, errors.New("a unique request ID is required")
	}
	groups, err := groupList(groups)
	if err != nil || len(groups) == 0 {
		return nil, errors.New("select at least one talkgroup")
	}
	if !c.principal.CanTransmit {
		return nil, ErrPermission
	}
	for _, tg := range groups {
		if !slices.Contains(c.principal.TransmitGroups, tg) {
			return nil, ErrPermission
		}
	}
	if !file && !c.connected.Load() {
		return nil, errors.New("connect audio before transmitting")
	}
	if !h.available() {
		return nil, ErrUnavailable
	}
	id, err := randomID()
	if err != nil {
		return nil, err
	}
	h.mu.Lock()
	if h.closed || h.clients[c.id] != c {
		h.mu.Unlock()
		return nil, ErrUnavailable
	}
	if c.usedRequests[requestID] {
		h.mu.Unlock()
		return nil, errors.New("this transmission request has already been used")
	}
	if len(c.usedRequests) >= 1024 {
		h.mu.Unlock()
		return nil, errors.New("reconnect audio before starting another transmission")
	}
	for _, active := range h.calls {
		if active.Owner == c {
			h.mu.Unlock()
			return nil, errors.New("release PTT before starting another transmission")
		}
	}
	for _, tg := range groups {
		if h.floors[tg] != nil {
			h.mu.Unlock()
			return nil, ErrBusy
		}
	}
	ctx, cancel := context.WithCancel(h.ctx)
	now := time.Now()
	tx := &transmission{ID: id, RequestID: requestID, Owner: c, Groups: groups, State: "requesting", Context: ctx, Cancel: cancel, Queue: make(chan queuedFrame, 8), Done: make(chan struct{}), Ready: make(chan struct{}), RequestedAt: now, LeaseUntil: now.Add(h.options.Lease), Secure: secure, AlgorithmID: algorithmID, KeyID: keyID}
	c.usedRequests[requestID] = true
	h.calls[id] = tx
	for _, tg := range groups {
		h.floors[tg] = tx
	}
	h.wg.Add(1)
	h.mu.Unlock()
	h.emit(tx, "call.requested", "pending", "")
	h.broadcastState()
	go h.run(tx)
	return tx, nil
}

func (h *Hub) run(tx *transmission) {
	defer h.wg.Done()
	defer close(tx.Done)
	var grant Grant
	var runError error
	defer func() {
		tx.Cancel()
		h.mu.Lock()
		if runError != nil {
			tx.Error = runError.Error()
		}
		h.mu.Unlock()
		tx.ReadyOnce.Do(func() { close(tx.Ready) })
		if grant.ID != "" {
			ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
			err := h.options.Router.End(ctx, grant)
			cancel()
			if runError == nil {
				runError = err
			}
		}
		h.mu.Lock()
		if h.calls[tx.ID] == tx {
			delete(h.calls, tx.ID)
			for _, tg := range tx.Groups {
				if h.floors[tg] == tx {
					delete(h.floors, tg)
				}
			}
			tx.Owner.endedRequest = tx.RequestID
		}
		if runError != nil {
			tx.Error = runError.Error()
		}
		reason := tx.Error
		h.mu.Unlock()
		outcome := "completed"
		if reason != "" {
			outcome = "ended"
		}
		h.emit(tx, "call.ended", outcome, reason)
		if reason != "" {
			tx.Owner.enqueue(map[string]any{"type": "error", "action": "ptt_end", "request_id": tx.RequestID, "message": reason})
		}
		h.broadcastState()
	}()
	ctx, cancel := context.WithTimeout(tx.Context, h.options.GrantTimeout)
	var err error
	grant, err = h.options.Router.Begin(ctx, CallRequest{ID: tx.ID, SessionID: tx.Owner.id, UserID: tx.Owner.principal.UserID, AutomationID: tx.Owner.principal.AutomationID, AutomationAction: tx.Owner.principal.AutomationAction, RadioID: tx.Owner.principal.RadioID, Talkgroups: append([]int{}, tx.Groups...), Secure: tx.Secure, AlgorithmID: tx.AlgorithmID, KeyID: tx.KeyID})
	cancel()
	if err != nil {
		if tx.Context.Err() == nil {
			runError = err
		}
		return
	}
	if grant.ID == "" || !containsGrantedGroups(grant.Talkgroups, tx.Groups) {
		runError = errors.New("audio route returned an invalid grant")
		return
	}
	if grant.Secure && !tx.Owner.principal.CanSecure {
		runError = ErrPermission
		return
	}
	h.mu.Lock()
	if tx.Context.Err() != nil {
		h.mu.Unlock()
		return
	}
	tx.Grant = grant
	tx.Secure, tx.AlgorithmID, tx.KeyID = grant.Secure, grant.AlgorithmID, grant.KeyID
	tx.State = "transmitting"
	tx.GrantedAt = time.Now()
	tx.LastPCM = tx.GrantedAt
	h.mu.Unlock()
	tx.ReadyOnce.Do(func() { close(tx.Ready) })
	h.emit(tx, "call.granted", "granted", "")
	h.broadcastState()
	renewFailures := make(chan error, 1)
	renewDone := make(chan struct{})
	go func() {
		defer close(renewDone)
		ticker := time.NewTicker(250 * time.Millisecond)
		defer ticker.Stop()
		for {
			select {
			case <-tx.Context.Done():
				return
			case <-ticker.C:
				ctx, cancel := context.WithTimeout(tx.Context, 500*time.Millisecond)
				err := h.options.Router.Renew(ctx, grant)
				cancel()
				if err != nil {
					if tx.Context.Err() != nil {
						return
					}
					select {
					case renewFailures <- err:
					default:
					}
					return
				}
			}
		}
	}()
	defer func() { tx.Cancel(); <-renewDone }()
	pace := time.NewTicker(FrameDuration)
	defer pace.Stop()
	for {
		select {
		case <-tx.Context.Done():
			return
		case err := <-renewFailures:
			if tx.Context.Err() == nil {
				runError = fmt.Errorf("audio channel lease renewal failed: %w", err)
			}
			return
		case <-pace.C:
			h.mu.Lock()
			drained := tx.State == "stopping" && len(tx.Queue) == 0
			h.mu.Unlock()
			if drained {
				return
			}
			var frame queuedFrame
			select {
			case frame = <-tx.Queue:
			default:
				continue
			}
			if age := time.Since(frame.At); age > h.options.MaxAudioAge || age < -time.Second {
				continue
			}
			h.mu.Lock()
			valid := h.calls[tx.ID] == tx && tx.Context.Err() == nil
			h.mu.Unlock()
			if !valid {
				return
			}
			deadline := frame.At.Add(h.options.MaxAudioAge)
			if latest := time.Now().Add(h.options.MaxAudioAge); deadline.After(latest) {
				deadline = latest
			}
			ctx, cancel := context.WithDeadline(tx.Context, deadline)
			err := h.options.Router.Write(ctx, grant, Frame{CallID: tx.ID, SourceID: tx.Owner.principal.RadioID, Sequence: frame.Sequence, PCM: frame.PCM, CapturedAt: frame.At})
			cancel()
			if err != nil {
				if tx.Context.Err() == nil {
					runError = fmt.Errorf("audio transmission failed at frame %d: %w", frame.Sequence, err)
				}
				return
			}
			h.mu.Lock()
			tx.Written = frame.Sequence
			h.mu.Unlock()
		}
	}
}

func sameGroups(a, b []int) bool {
	a, err := groupList(a)
	if err != nil {
		return false
	}
	b, err = groupList(b)
	return err == nil && slices.Equal(a, b)
}

// The trusted router can expand an authorized source group through a patch.
func containsGrantedGroups(granted, requested []int) bool {
	groups, err := groupList(granted)
	if err != nil || len(groups) == 0 {
		return false
	}
	return !slices.ContainsFunc(requested, func(tg int) bool { return !slices.Contains(groups, tg) })
}
func (h *Hub) renew(c *client, id string) error {
	h.mu.Lock()
	defer h.mu.Unlock()
	for _, tx := range h.calls {
		if tx.Owner == c && tx.RequestID == id && tx.Context.Err() == nil {
			tx.LeaseUntil = time.Now().Add(h.options.Lease)
			return nil
		}
	}
	return errors.New("this PTT request is no longer active")
}
func (h *Hub) stopRequest(c *client, id string) error {
	h.mu.Lock()
	var found *transmission
	for _, tx := range h.calls {
		if tx.Owner == c && tx.RequestID == id {
			found = tx
			break
		}
	}
	ended := c.endedRequest == id
	if found != nil && found.Context.Err() == nil && (found.State == "transmitting" || found.State == "stopping") {
		found.State = "stopping"
		h.mu.Unlock()
		return nil
	}
	h.mu.Unlock()
	if found != nil {
		h.stop(found, "")
		return nil
	}
	if ended {
		return nil
	}
	return errors.New("this PTT request is not owned by this audio session")
}
func (h *Hub) stop(tx *transmission, reason string) {
	h.mu.Lock()
	if reason != "" && tx.Error == "" {
		tx.Error = reason
	}
	h.mu.Unlock()
	tx.Cancel()
}

func (h *Hub) acceptPCM(c *client, pcm []byte, at time.Time) error {
	if len(pcm) != FrameBytes {
		return errors.New("audio must contain one 20 ms PCM frame")
	}
	if at.IsZero() {
		at = time.Now()
	}
	if age := time.Since(at); age > h.options.MaxAudioAge || age < -time.Second {
		return errors.New("audio frame is stale")
	}
	h.mu.Lock()
	var tx *transmission
	for _, v := range h.calls {
		if v.Owner == c && v.State == "transmitting" && v.Context.Err() == nil {
			tx = v
			break
		}
	}
	if tx == nil {
		h.mu.Unlock()
		return errors.New("no granted transmission")
	}
	if at.Before(tx.GrantedAt.Add(-20 * time.Millisecond)) {
		h.mu.Unlock()
		return errors.New("audio was captured before grant")
	}
	tx.Sequence++
	seq := tx.Sequence
	tx.LastPCM = time.Now()
	frame := queuedFrame{append([]byte(nil), pcm...), at, seq}
	select {
	case tx.Queue <- frame:
		h.mu.Unlock()
		return nil
	default:
		h.mu.Unlock()
		h.stop(tx, "The microphone sent audio faster than it can be transmitted.")
		return errors.New("audio queue is full")
	}
}

// Publish accepts decoded clear audio from the authenticated route boundary.
// It does not request grants or manufacture affiliation interest.
func (h *Hub) Publish(frame Frame) {
	if frame.Encrypted || frame.TalkgroupID < 1 || frame.TalkgroupID > 65534 || frame.SourceID < 1 || frame.SourceID > 0xfffffb || (!frame.End && len(frame.PCM) != FrameBytes) {
		return
	}
	if frame.CapturedAt.IsZero() {
		frame.CapturedAt = time.Now()
	}
	if !frame.End {
		if age := time.Since(frame.CapturedAt); age > h.options.MaxAudioAge || age < -time.Second {
			return
		}
	}
	frame.PCM = append([]byte(nil), frame.PCM...)
	frame.P25 = frame.P25.Clone()
	h.mu.Lock()
	defer h.mu.Unlock()
	for _, c := range h.clients {
		if frame.SourceID == c.principal.RadioID || !c.subscriptions[frame.TalkgroupID] || !slices.Contains(c.principal.ReceiveGroups, frame.TalkgroupID) {
			continue
		}
		c.enqueue(map[string]any{"type": "rx", "talkgroup_id": frame.TalkgroupID, "source_id": frame.SourceID, "source_alias": frame.SourceAlias, "source_kind": frame.SourceKind, "call_id": frame.CallID, "logical_call_id": frame.LogicalCallID, "active": !frame.End, "encrypted": frame.ReceivedEncrypted, "algorithm_id": frame.ReceivedAlgorithmID, "key_id": frame.ReceivedKeyID})
		if !frame.End && c.connected.Load() {
			c.enqueueAudio(frame)
			h.lastReceiveAudio = time.Now()
		}
	}
}

// PublishUnavailable reports a permitted radio call without inventing intelligible audio.
func (h *Hub) PublishUnavailable(frame Frame, reason string) {
	h.mu.Lock()
	defer h.mu.Unlock()
	if h.unavailableNotice == nil {
		h.unavailableNotice = map[string]time.Time{}
	}
	key := fmt.Sprintf("%s:%d:%s", frame.CallID, frame.TalkgroupID, reason)
	now := time.Now()
	if !frame.End && now.Sub(h.unavailableNotice[key]) < time.Second {
		return
	}
	h.unavailableNotice[key] = now
	for id, at := range h.unavailableNotice {
		if frame.End && id == key || now.Sub(at) > time.Minute {
			delete(h.unavailableNotice, id)
		}
	}
	for _, c := range h.clients {
		if frame.SourceID == c.principal.RadioID || !c.subscriptions[frame.TalkgroupID] || !slices.Contains(c.principal.ReceiveGroups, frame.TalkgroupID) {
			continue
		}
		c.enqueue(map[string]any{"type": "rx", "talkgroup_id": frame.TalkgroupID, "source_id": frame.SourceID, "source_alias": frame.SourceAlias, "source_kind": frame.SourceKind, "call_id": frame.CallID, "logical_call_id": frame.LogicalCallID, "active": !frame.End, "encrypted": frame.Encrypted, "algorithm_id": frame.AlgorithmID, "key_id": frame.KeyID, "audio_unavailable": reason})
	}
}

func (h *Hub) InvalidateSession(key string) {
	digest := sha256.Sum256([]byte(key))
	h.mu.Lock()
	clients := []*client{}
	for _, c := range h.clients {
		if c.session == digest {
			clients = append(clients, c)
		}
	}
	for k, t := range h.tickets {
		if t.Session == digest {
			delete(h.tickets, k)
		}
	}
	h.mu.Unlock()
	for _, c := range clients {
		h.removeClient(c)
	}
}
func (h *Hub) InvalidateUser(user string) {
	h.mu.Lock()
	clients := []*client{}
	for _, c := range h.clients {
		if c.principal.UserID == user {
			clients = append(clients, c)
		}
	}
	for k, t := range h.tickets {
		if t.Principal.UserID == user {
			delete(h.tickets, k)
		}
	}
	h.mu.Unlock()
	for _, c := range clients {
		h.removeClient(c)
	}
}

func (h *Hub) InvalidateAutomation(id string) {
	h.mu.Lock()
	clients := []*client{}
	for _, c := range h.clients {
		if c.principal.AutomationID == id {
			clients = append(clients, c)
		}
	}
	h.mu.Unlock()
	for _, c := range clients {
		h.removeClient(c)
	}
}

// Busy includes admission, release and recently delivered receive audio so a
// controller handover cannot interrupt a call in either direction.
func (h *Hub) Busy() bool {
	h.mu.Lock()
	defer h.mu.Unlock()
	return len(h.calls) != 0 || (!h.lastReceiveAudio.IsZero() && time.Since(h.lastReceiveAudio) < 500*time.Millisecond)
}

// StopTransmissions releases calls when an upstream route disappears without
// closing listening sessions or replaying microphone audio after reconnection.
func (h *Hub) StopTransmissions(reason string) {
	h.mu.Lock()
	calls := make([]*transmission, 0, len(h.calls))
	for _, call := range h.calls {
		calls = append(calls, call)
	}
	h.mu.Unlock()
	for _, call := range calls {
		h.stop(call, reason)
	}
}

func (h *Hub) watchdog() {
	defer h.wg.Done()
	tick := time.NewTicker(100 * time.Millisecond)
	defer tick.Stop()
	for {
		select {
		case <-h.ctx.Done():
			return
		case now := <-tick.C:
			h.mu.Lock()
			available := h.available()
			changed := available != h.lastAvailable
			h.lastAvailable = available
			stops := []*transmission{}
			clients := []*client{}
			for _, tx := range h.calls {
				reason := ""
				if now.After(tx.LeaseUntil) {
					reason = "PTT was released after the browser stopped responding."
				} else if now.Sub(tx.RequestedAt) > h.options.MaxTransmit {
					reason = "The open microphone time limit was reached."
				} else if tx.State == "transmitting" && now.Sub(tx.LastPCM) > h.options.AudioIdle {
					reason = "The microphone stopped sending audio."
				}
				if reason != "" {
					if tx.Error == "" {
						tx.Error = reason
					}
					stops = append(stops, tx)
				}
			}
			for _, c := range h.clients {
				if c.ctx.Err() != nil || (!c.connected.Load() && !c.file.Load() && now.Sub(c.created) > 20*time.Second) {
					clients = append(clients, c)
				}
			}
			for k, t := range h.tickets {
				if now.After(t.Expires) {
					delete(h.tickets, k)
				}
			}
			h.mu.Unlock()
			if changed {
				h.broadcastState()
			}
			for _, tx := range stops {
				tx.Cancel()
			}
			for _, c := range clients {
				h.removeClient(c)
			}
		}
	}
}
func (h *Hub) emit(tx *transmission, kind, outcome, message string) {
	event := Event{At: time.Now().UTC(), Kind: kind, UserID: tx.Owner.principal.UserID, AutomationID: tx.Owner.principal.AutomationID, RadioID: tx.Owner.principal.RadioID, CallID: tx.ID, Talkgroups: append([]int{}, tx.Groups...), Outcome: outcome, Message: message}
	select {
	case h.events <- event:
	default:
	}
}
func (h *Hub) eventLoop() {
	defer h.wg.Done()
	for {
		select {
		case <-h.ctx.Done():
			return
		case e := <-h.events:
			if h.options.Event != nil {
				h.options.Event(e)
			}
		}
	}
}
func (h *Hub) Close() {
	h.mu.Lock()
	if h.closed {
		h.mu.Unlock()
		return
	}
	h.closed = true
	clients := []*client{}
	for _, c := range h.clients {
		clients = append(clients, c)
	}
	h.mu.Unlock()
	h.cancel()
	for _, c := range clients {
		h.removeClient(c)
	}
	h.wg.Wait()
}
