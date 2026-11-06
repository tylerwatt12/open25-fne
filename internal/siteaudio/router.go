// Package siteaudio is the Site-owned audio/RF admission boundary. It
// translates an already authorized network call into a bounded native lease
// only when real RF subscribers need that talkgroup at this Site.
package siteaudio

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"slices"
	"sort"
	"strconv"
	"sync"
	"sync/atomic"
	"time"
	"open25.local/open25-fne/internal/p25crypto"

	"open25.local/open25-fne/internal/dispatch"
	"open25.local/open25-fne/internal/radio"
)

type Codec interface {
	Write(context.Context, dispatch.Frame) error
	Drain(context.Context, string, []int) error
}

type Options struct {
	Control       func() (radio.Native, error)
	Codec         Codec
	HasRFInterest func(int) bool
	AllowTX       func(int, int) bool
	Event         func(Event)
}

type Event struct {
	At           time.Time `json:"at"`
	Kind         string    `json:"kind"`
	CallID       string    `json:"call_id"`
	AllocationID string    `json:"allocation_id"`
	RadioID      int       `json:"radio_id"`
	TalkgroupID  int       `json:"talkgroup_id"`
	Outcome      string    `json:"outcome"`
	Message      string    `json:"message"`
}
type Status struct {
	ActiveCalls int    `json:"active_calls"`
	RFGrants    int    `json:"rf_grants"`
	Fault       string `json:"fault,omitempty"`
}

type nativeReply struct {
	Status    int    `json:"status"`
	Issued    bool   `json:"issued"`
	Accepted  bool   `json:"accepted"`
	State     string `json:"state"`
	Source    int    `json:"srcId"`
	Group     int    `json:"dstId"`
	Lease     string `json:"leaseId"`
	Epoch     string `json:"epoch"`
	ChannelID int    `json:"channelId"`
	ChannelNo int    `json:"channelNo"`
}

type nativeLease struct {
	Source, Group int
	Emergency     bool
	Priority      int
	Secure        bool
	ID, Epoch     string
	Control       radio.Native
	media         sync.RWMutex
	disabled      atomic.Bool
	released      atomic.Bool
}

func (l *nativeLease) identity() map[string]any {
	return map[string]any{"srcId": l.Source, "dstId": l.Group, "leaseId": l.ID, "epoch": l.Epoch, "ttlMs": 1000, "emergency": l.Emergency, "priority": l.Priority, "secure": l.Secure}
}
func (l *nativeLease) matches(r nativeReply) bool {
	epoch, err := strconv.ParseUint(r.Epoch, 10, 64)
	return r.Status == 200 && r.Source == l.Source && r.Group == l.Group && r.Lease == l.ID && err == nil && epoch > 0 && (l.Epoch == "" || l.Epoch == r.Epoch)
}
func readReply(raw map[string]json.RawMessage) nativeReply {
	data, _ := json.Marshal(raw)
	var r nativeReply
	_ = json.Unmarshal(data, &r)
	return r
}
func terminal(state string) bool {
	return state == "released" || state == "expired" || state == "lost" || state == "unavailable" || state == "stopped" || state == "preempted"
}

type call struct {
	mu           sync.Mutex
	media        sync.RWMutex
	request      dispatch.CallRequest
	groups       []int
	rfGroups     []int
	leases       []*nativeLease
	ready        chan struct{}
	endDone      chan struct{}
	state        string
	error        error
	lastSequence uint64
	waitBoundary bool
	lastFrame    dispatch.Frame
	mediaID      string
	mediaEpoch   uint64
}

type Router struct {
	admission sync.Mutex
	mu        sync.Mutex
	options   Options
	calls     map[string]*call
	floors    map[int]string
	history   map[string]bool
	order     []string
	fault     string
	closed    bool
}

func New(options Options) *Router {
	return &Router{options: options, calls: map[string]*call{}, floors: map[int]string{}, history: map[string]bool{}}
}
func normalizedGroups(groups []int) ([]int, error) {
	if len(groups) == 0 || len(groups) > dispatch.MaxGroups {
		return nil, errors.New("select permitted talkgroups")
	}
	out := append([]int{}, groups...)
	sort.Ints(out)
	for i, tg := range out {
		if tg < 1 || tg > 65534 || (i > 0 && out[i-1] == tg) {
			return nil, errors.New("select unique permitted talkgroups")
		}
	}
	return out, nil
}

// An emergency owns its requested floor only after the previous ordinary call
// has drained and released its exact native allocation. An emergency is never
// a candidate for interruption.
func (r *Router) preemptFloors(ctx context.Context, request dispatch.CallRequest) error {
	r.mu.Lock()
	victims := map[string]*call{}
	for _, tg := range request.Talkgroups {
		id := r.floors[tg]
		if id == "" || id == request.ID {
			continue
		}
		old := r.calls[id]
		if old != nil && old.request.Emergency {
			r.mu.Unlock()
			return dispatch.ErrBusy
		}
		if old != nil {
			victims[id] = old
		}
	}
	r.mu.Unlock()
	for id, old := range victims {
		if err := r.End(ctx, dispatch.Grant{ID: id, Talkgroups: slices.Clone(old.groups), Secure: old.request.Secure, AlgorithmID: old.request.AlgorithmID, KeyID: old.request.KeyID}); err != nil {
			return err
		}
		r.emit(old, 0, "call.preempted", "ended", "Interrupted by an emergency call.")
	}
	return nil
}

func (r *Router) Begin(ctx context.Context, request dispatch.CallRequest) (dispatch.Grant, error) {
	if request.LogicalCallID == "" {
		request.LogicalCallID = request.ID
	}
	r.admission.Lock()
	defer r.admission.Unlock()
	grant := dispatch.Grant{ID: request.ID, Talkgroups: append([]int{}, request.Talkgroups...), Secure: request.Secure, AlgorithmID: request.AlgorithmID, KeyID: request.KeyID}
	if err := ctx.Err(); err != nil {
		return grant, err
	}
	if request.ID == "" || len(request.ID) > 128 || len(request.LogicalCallID) > 128 || request.RadioID < 1 || request.RadioID > 0xfffffb {
		return grant, dispatch.ErrPermission
	}
	groups, err := normalizedGroups(request.Talkgroups)
	if err != nil {
		return grant, err
	}
	grant.Talkgroups = groups
	request.Talkgroups = slices.Clone(groups)
	request.AuthorizationGroups = slices.Clone(request.AuthorizationGroups)
	if request.EncryptionPending && (!request.Secure || request.AlgorithmID != 0 || request.KeyID != 0) {
		return grant, dispatch.ErrPermission
	}
	identityValid := p25crypto.ValidSelection(request.Secure, request.AlgorithmID, request.KeyID) || (!request.Secure && request.AlgorithmID == p25crypto.Clear && request.KeyID == 0)
	if !request.EncryptionPending && !identityValid {
		return grant, dispatch.ErrPermission
	}
	if request.Emergency {
		request.Priority = 1
	} else if request.Priority < 2 || request.Priority > 10 {
		request.Priority = 10
	}
	for _, tg := range groups {
		if r.options.AllowTX == nil || !r.options.AllowTX(request.RadioID, tg) {
			return grant, dispatch.ErrPermission
		}
	}
	rfGroups := []int{}
	for _, tg := range groups {
		if r.options.HasRFInterest != nil && r.options.HasRFInterest(tg) {
			rfGroups = append(rfGroups, tg)
		}
	}
	if request.Emergency {
		if err := r.preemptFloors(ctx, request); err != nil {
			return grant, err
		}
	}
	r.mu.Lock()
	if existing := r.calls[request.ID]; existing != nil {
		r.mu.Unlock()
		if existing.request.LogicalCallID != request.LogicalCallID || existing.request.RadioID != request.RadioID || existing.request.UserID != request.UserID || existing.request.SessionID != request.SessionID || existing.request.Emergency != request.Emergency || existing.request.Priority != request.Priority || existing.request.Secure != request.Secure || existing.request.KeyID != request.KeyID || existing.request.AlgorithmID != request.AlgorithmID || existing.request.EncryptionPending != request.EncryptionPending || !slices.Equal(existing.groups, groups) || !slices.Equal(existing.request.AuthorizationGroups, request.AuthorizationGroups) {
			return grant, dispatch.ErrPermission
		}
		select {
		case <-ctx.Done():
			return grant, ctx.Err()
		case <-existing.ready:
		}
		existing.mu.Lock()
		err := existing.error
		state := existing.state
		existing.mu.Unlock()
		if state != "active" && err == nil {
			err = dispatch.ErrUnavailable
		}
		return grant, err
	}
	if r.closed || r.history[request.ID] {
		r.mu.Unlock()
		return grant, dispatch.ErrUnavailable
	}
	for _, tg := range groups {
		if r.floors[tg] != "" {
			r.mu.Unlock()
			return grant, dispatch.ErrBusy
		}
	}
	for _, active := range r.calls {
		if active.request.RadioID == request.RadioID {
			r.mu.Unlock()
			return grant, dispatch.ErrBusy
		}
	}
	if len(rfGroups) > 0 {
		if r.fault != "" {
			err = errors.New(r.fault)
		}
		if r.options.Codec == nil || r.options.Control == nil {
			err = dispatch.ErrUnavailable
		}
		if err != nil {
			r.mu.Unlock()
			return grant, err
		}
	}
	c := &call{request: request, groups: groups, rfGroups: rfGroups, ready: make(chan struct{}), endDone: make(chan struct{}), state: "starting"}
	r.calls[request.ID] = c
	for _, tg := range groups {
		r.floors[tg] = request.ID
	}
	r.mu.Unlock()
	failed := true
	defer func() {
		if failed {
			c.mu.Lock()
			c.error = err
			c.state = "failed"
			c.mu.Unlock()
		}
		close(c.ready)
		if failed {
			cleanup, cancel := context.WithTimeout(context.Background(), 2*time.Second)
			_ = r.End(cleanup, grant)
			cancel()
		}
	}()
	for _, tg := range rfGroups {
		control, e := r.options.Control()
		if e != nil {
			err = e
			return grant, err
		}
		sum := sha256.Sum256([]byte(request.ID + ":" + strconv.Itoa(tg)))
		lease := &nativeLease{Source: request.RadioID, Group: tg, ID: hex.EncodeToString(sum[:]), Control: control, Emergency: request.Emergency, Priority: request.Priority, Secure: request.Secure}
		c.mu.Lock()
		c.leases = append(c.leases, lease)
		c.mu.Unlock()
		if e = r.acquire(ctx, lease); e != nil {
			err = e
			r.emit(c, tg, "rf.grant", "failed", e.Error())
			return grant, err
		}
		r.emit(c, tg, "rf.grant", "granted", "")
	}
	if err = ctx.Err(); err != nil {
		return grant, err
	}
	c.mu.Lock()
	c.state = "active"
	c.mu.Unlock()
	failed = false
	return grant, nil
}

func (r *Router) acquire(ctx context.Context, l *nativeLease) error {
	raw, err := l.Control.Put(ctx, "/p25/dispatch/grant", l.identity())
	reply := readReply(raw)
	if l.matches(reply) {
		l.Epoch = reply.Epoch
	}
	if err == nil && l.matches(reply) && reply.Accepted && reply.State == "granted" && reply.ChannelNo > 0 && reply.ChannelNo <= 4095 && reply.ChannelID >= 0 && reply.ChannelID <= 15 {
		return nil
	}
	knownRejection := raw != nil && !reply.Accepted && !reply.Issued && reply.Status >= 400
	possible := err == nil
	var native *radio.NativeError
	if errors.As(err, &native) {
		possible = native.Possible
	}
	if knownRejection {
		possible = false
		l.released.Store(true)
	}
	if possible {
		lookup, cancel := context.WithTimeout(context.Background(), 1200*time.Millisecond)
		status, e := l.Control.Put(lookup, "/p25/dispatch/status", l.identity())
		cancel()
		found := readReply(status)
		if e == nil && l.matches(found) {
			l.Epoch = found.Epoch
			if found.Accepted && found.State == "granted" && found.ChannelNo > 0 && found.ChannelNo <= 4095 && found.ChannelID >= 0 && found.ChannelID <= 15 && ctx.Err() == nil {
				return nil
			}
			if terminal(found.State) {
				return errors.New("native voice grant ended before audio could start")
			}
		}
		if l.Epoch == "" {
			r.setFault("An RF grant outcome is unconfirmed. Restart Site radio operation before another RF transmission.")
		}
	}
	if reply.Status == 409 || reply.Status == 503 {
		return fmt.Errorf("%w: native voice capacity is unavailable", dispatch.ErrBusy)
	}
	if err != nil {
		return err
	}
	return errors.New("native RF grant was not positively confirmed")
}

func (r *Router) Write(ctx context.Context, grant dispatch.Grant, frame dispatch.Frame) error {
	c, err := r.lookup(grant)
	if err != nil {
		return err
	}
	if !c.media.TryRLock() {
		return dispatch.ErrUnavailable
	}
	defer c.media.RUnlock()
	c.mu.Lock()
	if c.state != "active" {
		c.mu.Unlock()
		return dispatch.ErrUnavailable
	}
	if frame.SourceID != c.request.RadioID || frame.CallID != c.request.ID || frame.End || !frame.MediaValid() || frame.Encrypted != c.request.Secure || (frame.Encrypted && !c.request.EncryptionPending && (int(frame.KeyID) != c.request.KeyID || int(frame.AlgorithmID) != c.request.AlgorithmID)) {
		c.mu.Unlock()
		return dispatch.ErrPermission
	}
	if c.request.EncryptionPending && (frame.SourceEpoch == 0 || (c.lastFrame.CallID == "" && c.mediaEpoch != 0 && frame.SourceEpoch == c.mediaEpoch) || (c.lastFrame.CallID != "" && (frame.AlgorithmID != c.lastFrame.AlgorithmID || frame.KeyID != c.lastFrame.KeyID))) {
		c.mu.Unlock()
		return dispatch.ErrPermission
	}
	if frame.Sequence == 0 || frame.Sequence <= c.lastSequence {
		c.mu.Unlock()
		return errors.New("audio sequence is stale or duplicated")
	}
	if frame.CapturedAt.IsZero() || time.Since(frame.CapturedAt) > 250*time.Millisecond || frame.CapturedAt.After(time.Now().Add(time.Second)) {
		c.mu.Unlock()
		return errors.New("audio frame is stale")
	}
	mediaID := voiceMediaID(c.request.ID, frame.SourceEpoch)
	if frame.SourceEpoch < c.mediaEpoch || (mediaID != c.mediaID && c.lastFrame.CallID != "") {
		c.mu.Unlock()
		return dispatch.ErrPermission
	}
	c.lastSequence = frame.Sequence
	if len(frame.Encoded) != 0 && ((mediaID != c.mediaID && frame.VoiceIndex != 0) || (c.waitBoundary && frame.VoiceIndex != 0 && frame.VoiceIndex != 9)) {
		c.mu.Unlock()
		return nil
	}
	c.waitBoundary = false
	c.mediaID, c.mediaEpoch = mediaID, frame.SourceEpoch
	c.lastFrame = frame
	c.lastFrame.CallID = mediaID
	c.lastFrame.PCM, c.lastFrame.Encoded, c.lastFrame.P25 = nil, nil, nil
	leases := append([]*nativeLease{}, c.leases...)
	c.mu.Unlock()
	for _, tg := range c.groups {
		if r.options.AllowTX == nil || !r.options.AllowTX(c.request.RadioID, tg) {
			return dispatch.ErrPermission
		}
	}
	for _, lease := range leases {
		if lease.disabled.Load() || (r.options.HasRFInterest != nil && !r.options.HasRFInterest(lease.Group)) {
			continue
		}
		if lease.Epoch == "" {
			return dispatch.ErrUnavailable
		}
		if !lease.media.TryRLock() {
			continue
		}
		if lease.disabled.Load() {
			lease.media.RUnlock()
			continue
		}
		out := frame.WithGroup(lease.Group).WithEmergency(c.request.Emergency) // accepted call owner, never microphone metadata
		out.CallID = mediaID
		err = r.options.Codec.Write(ctx, out)
		lease.media.RUnlock()
		if err != nil {
			return err
		}
	}
	return nil
}

func (r *Router) Renew(ctx context.Context, grant dispatch.Grant) error {
	c, err := r.lookup(grant)
	if err != nil {
		return err
	}
	c.media.RLock()
	defer c.media.RUnlock()
	c.mu.Lock()
	if c.state != "active" {
		c.mu.Unlock()
		return dispatch.ErrUnavailable
	}
	leases := append([]*nativeLease{}, c.leases...)
	mediaID := c.mediaID
	c.mu.Unlock()
	for _, tg := range c.groups {
		if r.options.AllowTX == nil || !r.options.AllowTX(c.request.RadioID, tg) {
			return dispatch.ErrPermission
		}
	}
	for _, l := range leases {
		if l.disabled.Load() {
			continue
		}
		if r.options.HasRFInterest != nil && !r.options.HasRFInterest(l.Group) {
			l.disabled.Store(true)
			l.media.Lock()
			var cleanup error
			if r.options.Codec != nil {
				cleanup = r.options.Codec.Drain(ctx, mediaID, []int{l.Group})
			}
			cleanup = errors.Join(cleanup, r.release(ctx, l))
			l.media.Unlock()
			outcome := "released"
			message := "Last RF affiliation left; IP audio continues."
			if cleanup != nil {
				outcome = "unconfirmed"
				message = cleanup.Error()
			}
			r.emit(c, l.Group, "rf.interest_ended", outcome, message)
			continue
		}
		raw, err := l.Control.Put(ctx, "/p25/dispatch/renew", l.identity())
		reply := readReply(raw)
		if err != nil || !l.matches(reply) || !reply.Accepted || reply.State != "granted" {
			// A rejection may contain only a status code. Read back this exact
			// lease for its terminal reason; never retry or replace the grant.
			lookup, cancel := context.WithTimeout(context.Background(), 200*time.Millisecond)
			status, lookupErr := l.Control.Put(lookup, "/p25/dispatch/status", l.identity())
			cancel()
			state := "unknown"
			if found := readReply(status); lookupErr == nil && l.matches(found) {
				state = diagnosticState(found.State)
				if terminal(found.State) {
					l.released.Store(true)
				}
			}
			failure := renewalFailure(reply, state, err)
			r.emit(c, l.Group, "rf.renew", "failed", failure.Error())
			return failure
		}
	}
	return nil
}

func diagnosticState(state string) string {
	switch state {
	case "granted", "released", "expired", "lost", "unavailable", "stopped", "rejected", "preempted":
		return state
	default:
		return "unknown"
	}
}

func renewalFailure(reply nativeReply, state string, err error) error {
	httpStatus, nativeStatus := 0, reply.Status
	var native *radio.NativeError
	if errors.As(err, &native) {
		httpStatus = native.HTTPStatus
		if nativeStatus == 0 {
			nativeStatus = native.Status
		}
	}
	message := fmt.Sprintf("Voice channel renewal failed (HTTP %d, controller %d, state %s)", httpStatus, nativeStatus, state)
	if err != nil {
		return fmt.Errorf("%s: %w", message, err)
	}
	return errors.New(message)
}

func (r *Router) End(ctx context.Context, grant dispatch.Grant) error {
	r.mu.Lock()
	c := r.calls[grant.ID]
	if c == nil {
		known := r.history[grant.ID]
		r.mu.Unlock()
		if known {
			return nil
		}
		return nil
	}
	if !sameGroupSet(c.groups, grant.Talkgroups) {
		r.mu.Unlock()
		return dispatch.ErrPermission
	}
	r.mu.Unlock()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-c.ready:
	}
	c.media.Lock()
	defer c.media.Unlock()
	c.mu.Lock()
	if c.state == "ending" || c.state == "ended" {
		done := c.endDone
		c.mu.Unlock()
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-done:
		}
		c.mu.Lock()
		err := c.error
		c.mu.Unlock()
		return err
	}
	c.state = "ending"
	leases := append([]*nativeLease{}, c.leases...)
	mediaID := c.mediaID
	c.mu.Unlock()
	var cleanup error
	if len(leases) > 0 && r.options.Codec != nil {
		groups := []int{}
		for _, l := range leases {
			groups = append(groups, l.Group)
		}
		cleanup = r.options.Codec.Drain(ctx, mediaID, groups)
	}
	for _, l := range leases {
		if err := r.release(ctx, l); err != nil {
			cleanup = errors.Join(cleanup, err)
		}
	}
	c.mu.Lock()
	c.state = "ended"
	c.error = cleanup
	close(c.endDone)
	c.mu.Unlock()
	r.mu.Lock()
	delete(r.calls, grant.ID)
	for _, tg := range c.groups {
		if r.floors[tg] == grant.ID {
			delete(r.floors, tg)
		}
	}
	r.history[grant.ID] = true
	r.order = append(r.order, grant.ID)
	for len(r.order) > 2048 {
		delete(r.history, r.order[0])
		r.order = r.order[1:]
	}
	r.mu.Unlock()
	outcome := "released"
	message := ""
	if cleanup != nil {
		outcome = "unconfirmed"
		message = cleanup.Error()
	}
	for _, l := range leases {
		r.emit(c, l.Group, "rf.release", outcome, message)
	}
	return cleanup
}

func (r *Router) release(ctx context.Context, l *nativeLease) error {
	if l.released.Load() {
		return nil
	}
	if l.Epoch == "" { // Never release some other call by guessing a native epoch.
		lookup, cancel := context.WithTimeout(ctx, 800*time.Millisecond)
		raw, err := l.Control.Put(lookup, "/p25/dispatch/status", l.identity())
		cancel()
		reply := readReply(raw)
		if err == nil && l.matches(reply) {
			l.Epoch = reply.Epoch
			if terminal(reply.State) {
				l.released.Store(true)
				return nil
			}
		}
		if l.Epoch == "" {
			var native *radio.NativeError
			if errors.As(err, &native) && (native.HTTPStatus == 404 || native.Status == 404) {
				return nil
			}
			r.setFault("An RF lease could not be identified for release. Restart Site radio operation before another RF transmission.")
			return errors.New("RF lease release is unconfirmed")
		}
	}
	raw, err := l.Control.Put(ctx, "/p25/dispatch/release", l.identity())
	reply := readReply(raw)
	if err == nil && l.matches(reply) && reply.Accepted && terminal(reply.State) {
		l.released.Store(true)
		return nil
	}
	lookup, cancel := context.WithTimeout(context.Background(), 800*time.Millisecond)
	status, statusErr := l.Control.Put(lookup, "/p25/dispatch/status", l.identity())
	cancel()
	found := readReply(status)
	if statusErr == nil && l.matches(found) && terminal(found.State) {
		l.released.Store(true)
		return nil
	}
	r.setFault("An RF lease release is unconfirmed. Restart Site radio operation before another RF transmission.")
	if err != nil {
		return err
	}
	return errors.New("RF lease release is unconfirmed")
}

func sameGroupSet(a, b []int) bool {
	a, err := normalizedGroups(a)
	if err != nil {
		return false
	}
	b, err = normalizedGroups(b)
	return err == nil && slices.Equal(a, b)
}
func (r *Router) lookup(grant dispatch.Grant) (*call, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	c := r.calls[grant.ID]
	if r.closed || c == nil || !sameGroupSet(c.groups, grant.Talkgroups) {
		return nil, dispatch.ErrUnavailable
	}
	return c, nil
}
func (r *Router) setFault(message string) {
	r.mu.Lock()
	if r.fault == "" {
		r.fault = message
	}
	r.mu.Unlock()
}
func (r *Router) Status() Status {
	r.mu.Lock()
	defer r.mu.Unlock()
	s := Status{ActiveCalls: len(r.calls), Fault: r.fault}
	for _, c := range r.calls {
		c.mu.Lock()
		for _, l := range c.leases {
			if l.Epoch != "" && c.state == "active" && !l.disabled.Load() {
				s.RFGrants++
			}
		}
		c.mu.Unlock()
	}
	return s
}

func (r *Router) RFGrantCount() int { return r.Status().RFGrants }
func (r *Router) Close(ctx context.Context) error {
	r.mu.Lock()
	r.closed = true
	calls := []dispatch.Grant{}
	for id, c := range r.calls {
		calls = append(calls, dispatch.Grant{ID: id, Talkgroups: append([]int{}, c.groups...), Secure: c.request.Secure, AlgorithmID: c.request.AlgorithmID, KeyID: c.request.KeyID})
	}
	r.mu.Unlock()
	var result error
	for _, g := range calls {
		result = errors.Join(result, r.End(ctx, g))
	}
	r.mu.Lock()
	r.closed = true
	r.mu.Unlock()
	return result
}
func (r *Router) emit(c *call, tg int, kind, outcome, message string) {
	if r.options.Event != nil {
		r.options.Event(Event{At: time.Now().UTC(), Kind: kind, CallID: c.request.LogicalCallID, AllocationID: c.request.ID, RadioID: c.request.RadioID, TalkgroupID: tg, Outcome: outcome, Message: message})
	}
}

var _ dispatch.Router = (*Router)(nil)
