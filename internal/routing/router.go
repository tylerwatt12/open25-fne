// Package routing owns call floors and eligible receiving Site legs.
package routing

import (
	"context"
	"errors"
	"slices"
	"sort"
	"sync"
	"time"
	"open25.local/open25-fne/internal/p25crypto"

	"open25.local/open25-fne/internal/dispatch"
)

type RFSourceObservation struct {
	NativeStartedAt time.Time `json:"native_started_at"`
	ObservedAt      time.Time `json:"observed_at"`
	AffiliatedAt    time.Time `json:"affiliated_at"`
	RegisteredAt    time.Time `json:"registered_at,omitzero"`
}

type Request struct {
	dispatch.CallRequest
	Proof          string              `json:"proof"`
	SourceKind     string              `json:"source_kind"`
	OriginSite     string              `json:"origin_site"`
	ZoneAuthorized bool                `json:"-"`
	RFSource       RFSourceObservation `json:"rf_source,omitzero"`
}
type Target struct {
	ID     string
	Router dispatch.Router
	Groups []int
}

// Selection records eligibility and its reasons from one evaluation.
type Selection struct {
	Targets  []Target
	Excluded map[string]string
	Reason   string
}
type Options struct {
	Authorize func(Request) error
	Expand    func(Request) (Request, error)
	Policy    func(Request) Policy
	Targets   func(Request) Selection
	Publish   func(Request, dispatch.Frame)
	EndSpurt  func(Request)
	Event     func(Request, string, string)
	Lease     time.Duration
	Now       func() time.Time
}
type leg struct {
	target Target
	grant  dispatch.Grant
}
type call struct {
	original    Request
	request     Request
	owner       string
	grant       dispatch.Grant
	legs        []leg
	policy      Policy
	delivery    map[string]Delivery
	deadline    time.Time
	ready       bool
	ending      bool
	done        chan struct{}
	endError    error
	media       sync.RWMutex
	cleanup     sync.WaitGroup
	ended       bool
	keyEpoch    uint64
	keyClosed   bool
	algorithmID uint8
	keyID       uint16
	ctx         context.Context
	cancel      context.CancelFunc
}
type Router struct {
	mu      sync.Mutex
	calls   map[string]*call
	floors  map[int]string
	options Options
	ctx     context.Context
	cancel  context.CancelFunc
	wg      sync.WaitGroup
}

func New(o Options) *Router {
	if o.Lease <= 0 {
		o.Lease = 1500 * time.Millisecond
	}
	if o.Now == nil {
		o.Now = time.Now
	}
	ctx, cancel := context.WithCancel(context.Background())
	r := &Router{calls: map[string]*call{}, floors: map[int]string{}, options: o, ctx: ctx, cancel: cancel}
	r.wg.Add(1)
	go r.watch()
	return r
}
func groups(ids []int) ([]int, error) {
	out := slices.Clone(ids)
	sort.Ints(out)
	if len(out) == 0 || len(out) > 16 {
		return nil, errors.New("select one to sixteen talkgroups")
	}
	for i, g := range out {
		if g < 1 || g > 65534 || (i > 0 && g == out[i-1]) {
			return nil, errors.New("talkgroups must be unique and valid")
		}
	}
	return out, nil
}
func (r *Router) Begin(ctx context.Context, owner string, q Request) (dispatch.Grant, error) {
	if err := ctx.Err(); err != nil {
		return dispatch.Grant{}, err
	}
	ids, err := groups(q.Talkgroups)
	if err != nil {
		return dispatch.Grant{}, err
	}
	q.Talkgroups = ids
	if len(q.AuthorizationGroups) == 0 {
		q.AuthorizationGroups = slices.Clone(ids)
	}
	if q.LogicalCallID == "" {
		q.LogicalCallID = q.ID
	}
	if q.ID == "" || len(q.ID) > 128 || owner == "" || q.RadioID < 1 || q.RadioID > 0xFFFFFB {
		return dispatch.Grant{}, dispatch.ErrPermission
	}
	if len(q.LogicalCallID) > 128 {
		return dispatch.Grant{}, dispatch.ErrPermission
	}
	if q.EncryptionPending && (q.SourceKind != "rf" || !q.Secure || q.AlgorithmID != 0 || q.KeyID != 0) {
		return dispatch.Grant{}, dispatch.ErrPermission
	}
	identityValid := p25crypto.ValidSelection(q.Secure, q.AlgorithmID, q.KeyID) || (!q.Secure && q.AlgorithmID == p25crypto.Clear && q.KeyID == 0)
	if !q.EncryptionPending && !identityValid {
		return dispatch.Grant{}, dispatch.ErrPermission
	}
	if r.options.Authorize != nil {
		if err = r.options.Authorize(q); err != nil {
			return dispatch.Grant{}, err
		}
	}
	if err = ctx.Err(); err != nil {
		return dispatch.Grant{}, err
	}
	original := cloneRequest(q)
	if r.options.Expand != nil {
		q, err = r.expand(q)
		if err != nil {
			return dispatch.Grant{}, err
		}
	}
	ids = q.Talkgroups
	policy := r.policy(original)
	for {
		r.mu.Lock()
		if r.ctx.Err() != nil {
			r.mu.Unlock()
			return dispatch.Grant{}, dispatch.ErrUnavailable
		}
		if old := r.calls[q.ID]; old != nil {
			if old.owner == owner && old.ready && sameRequest(old.original, original) && slices.Equal(old.request.Talkgroups, q.Talkgroups) {
				g := cloneGrant(old.grant)
				r.mu.Unlock()
				return g, nil
			}
			r.mu.Unlock()
			return dispatch.Grant{}, dispatch.ErrBusy
		}
		for _, old := range r.calls {
			if old.original.RadioID == original.RadioID {
				r.mu.Unlock()
				return dispatch.Grant{}, dispatch.ErrBusy
			}
		}
		conflicts := map[string]*call{}
		for _, tg := range ids {
			if old := r.calls[r.floors[tg]]; old != nil {
				conflicts[old.request.ID] = old
			}
		}
		if len(conflicts) != 0 {
			victims := []*call{}
			if original.Emergency {
				for _, old := range conflicts {
					if !old.original.Emergency && !old.ending {
						victims = append(victims, old)
					}
				}
			}
			r.mu.Unlock()
			if len(victims) != len(conflicts) {
				return dispatch.Grant{}, dispatch.ErrBusy
			}
			for _, old := range victims {
				r.event(old.request, "call_preempted", "Interrupted by an emergency call.")
				if err = r.End(ctx, old.owner, old.grant); err != nil {
					return dispatch.Grant{}, err
				}
			}
			continue
		}
		callCtx, cancel := context.WithCancel(r.ctx)
		c := &call{original: original, request: cloneRequest(q), owner: owner, grant: dispatch.Grant{ID: q.ID, Talkgroups: slices.Clone(ids), Secure: q.Secure, AlgorithmID: q.AlgorithmID, KeyID: q.KeyID}, policy: policy, delivery: map[string]Delivery{}, ctx: callCtx, cancel: cancel, deadline: r.options.Now().Add(policy.Wait + r.options.Lease), done: make(chan struct{})}
		r.calls[q.ID] = c
		for _, tg := range ids {
			r.floors[tg] = q.ID
		}
		r.mu.Unlock()
		failed := true
		defer func() {
			if failed {
				_ = r.End(context.Background(), owner, c.grant)
			}
		}()
		if err = r.admit(ctx, c); err != nil {
			return c.grant, err
		}
		if err = ctx.Err(); err != nil {
			return c.grant, err
		}
		r.mu.Lock()
		if r.calls[q.ID] != c || c.ending || r.ctx.Err() != nil {
			r.mu.Unlock()
			return c.grant, dispatch.ErrUnavailable
		}
		c.ready = true
		c.deadline = r.options.Now().Add(r.options.Lease)
		r.wg.Add(1)
		go r.follow(c)
		r.mu.Unlock()
		failed = false
		r.event(c.request, "call_start", "")
		return cloneGrant(c.grant), nil
	}
}

func sameRequest(a, b Request) bool {
	return a.ID == b.ID && a.LogicalCallID == b.LogicalCallID && a.UserID == b.UserID && a.AutomationID == b.AutomationID && a.AutomationAction == b.AutomationAction && a.SessionID == b.SessionID && a.RadioID == b.RadioID && a.Emergency == b.Emergency && a.Priority == b.Priority && a.Secure == b.Secure && a.KeyID == b.KeyID && a.AlgorithmID == b.AlgorithmID && a.EncryptionPending == b.EncryptionPending && a.Proof == b.Proof && a.SourceKind == b.SourceKind && a.OriginSite == b.OriginSite && a.ZoneAuthorized == b.ZoneAuthorized && a.RFSource == b.RFSource && slices.Equal(a.Talkgroups, b.Talkgroups) && slices.Equal(a.AuthorizationGroups, b.AuthorizationGroups)
}

func cloneGrant(g dispatch.Grant) dispatch.Grant {
	g.Talkgroups = slices.Clone(g.Talkgroups)
	return g
}
func (r *Router) find(owner string, g dispatch.Grant) (*call, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	c := r.calls[g.ID]
	if c == nil || !c.ready || c.owner != owner || !slices.Equal(c.grant.Talkgroups, g.Talkgroups) {
		return nil, dispatch.ErrPermission
	}
	return c, nil
}
func (r *Router) Write(ctx context.Context, owner string, g dispatch.Grant, f dispatch.Frame) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	c, err := r.find(owner, g)
	if err != nil {
		return err
	}
	if !f.MediaValid() || f.End || f.Encrypted != c.original.Secure || f.SourceID != c.original.RadioID || f.CallID != g.ID || f.CapturedAt.IsZero() || time.Since(f.CapturedAt) > 300*time.Millisecond || f.CapturedAt.After(time.Now().Add(time.Second)) {
		return errors.New("invalid, encrypted or delayed media")
	}
	if err = r.validateCall(c, &f); err != nil {
		_ = r.End(context.Background(), owner, g)
		return err
	}
	if err = ctx.Err(); err != nil {
		return err
	}
	c.media.Lock()
	defer c.media.Unlock()
	if c.ended {
		return dispatch.ErrUnavailable
	}
	if f.Encrypted {
		if c.original.EncryptionPending {
			if f.SourceEpoch == 0 || f.SourceEpoch < c.keyEpoch || (f.SourceEpoch == c.keyEpoch && c.keyClosed) {
				return dispatch.ErrPermission
			}
			if f.SourceEpoch > c.keyEpoch {
				if c.keyEpoch != 0 && !c.keyClosed {
					return dispatch.ErrPermission
				}
				c.keyEpoch, c.algorithmID, c.keyID = f.SourceEpoch, f.AlgorithmID, f.KeyID
				c.keyClosed = false
			}
			if f.AlgorithmID != c.algorithmID || f.KeyID != c.keyID {
				return dispatch.ErrPermission
			}
		} else if int(f.KeyID) != c.original.KeyID || int(f.AlgorithmID) != c.original.AlgorithmID {
			return dispatch.ErrPermission
		}
	}
	f = f.WithEmergency(c.request.Emergency) // accepted call mode applies to every Site/IP recipient
	r.retainAvailableLegs(ctx, c, func(leg leg) error {
		copy := f
		copy.CallID = leg.grant.ID
		return leg.target.Router.Write(ctx, leg.grant, copy)
	})
	if err = ctx.Err(); err != nil {
		return err
	}
	if r.options.Publish != nil {
		for _, tg := range c.request.Talkgroups {
			copy := f.WithGroup(tg)
			r.options.Publish(c.request, copy)
		}
	}
	return nil
}
func (r *Router) Renew(ctx context.Context, owner string, g dispatch.Grant) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	c, err := r.find(owner, g)
	if err != nil {
		return err
	}
	if err = r.validateCall(c, nil); err != nil {
		return err
	}
	if err = ctx.Err(); err != nil {
		return err
	}
	ctx, cancel := context.WithCancel(ctx)
	stop := context.AfterFunc(c.ctx, cancel)
	defer func() { stop(); cancel() }()
	c.media.Lock()
	if c.ended || c.ctx.Err() != nil {
		c.media.Unlock()
		return dispatch.ErrUnavailable
	}
	legs := slices.Clone(c.legs)
	// End marks ended under this same lock before waiting. Keep this renewal
	// counted while its RPCs and any detached-allocation cleanup are in flight.
	c.cleanup.Add(1)
	c.media.Unlock()
	defer c.cleanup.Done()
	for _, l := range legs {
		err = l.target.Router.Renew(ctx, l.grant)
		if failure := ctx.Err(); failure != nil {
			return failure
		}
		if err == nil {
			continue
		}
		detached := false
		c.media.Lock()
		if !c.ended && c.ctx.Err() == nil && ctx.Err() == nil {
			index := slices.IndexFunc(c.legs, func(current leg) bool { return current.target.ID == l.target.ID && current.grant.ID == l.grant.ID })
			if index >= 0 {
				c.legs = slices.Delete(c.legs, index, index+1)
				c.delivery[l.target.ID] = Delivery{SiteID: l.target.ID, State: "unavailable", Message: err.Error()}
				c.cleanup.Add(1)
				detached = true
			}
		}
		c.media.Unlock()
		if detached {
			go func(l leg) { defer c.cleanup.Done(); r.release([]leg{l}) }(l)
			r.event(c.request, "site_omitted", l.target.ID+": "+err.Error())
		}
	}
	if err = ctx.Err(); err != nil {
		return err
	}
	r.mu.Lock()
	if r.calls[g.ID] == c && !c.ending {
		c.deadline = r.options.Now().Add(r.options.Lease)
	}
	r.mu.Unlock()
	return nil
}
func (r *Router) End(ctx context.Context, owner string, g dispatch.Grant) error {
	r.mu.Lock()
	c := r.calls[g.ID]
	if c == nil {
		r.mu.Unlock()
		return nil
	}
	if c.owner != owner || !slices.Equal(c.grant.Talkgroups, g.Talkgroups) {
		r.mu.Unlock()
		return dispatch.ErrPermission
	}
	if c.ending {
		done := c.done
		r.mu.Unlock()
		select {
		case <-done:
			return c.endError
		case <-ctx.Done():
			return ctx.Err()
		}
	}
	// Keep every floor reserved until the Site legs have finished draining.
	// Otherwise the next call can acquire a floor while RF is still releasing.
	c.ending = true
	c.ready = false
	c.cancel()
	r.mu.Unlock()
	c.media.Lock()
	c.ended = true
	legs := slices.Clone(c.legs)
	c.media.Unlock()
	var result error
	for _, leg := range legs {
		if err := leg.target.Router.End(ctx, leg.grant); err != nil {
			result = errors.Join(result, err)
		}
	}
	c.cleanup.Wait()
	if r.options.Publish != nil {
		for _, tg := range c.request.Talkgroups {
			r.options.Publish(c.request, dispatch.Frame{CallID: g.ID, TalkgroupID: tg, SourceID: c.request.RadioID, End: true, CapturedAt: r.options.Now()})
		}
	}
	if r.options.Event != nil {
		r.options.Event(c.request, "call_end", "")
	}
	r.mu.Lock()
	if r.calls[g.ID] == c {
		delete(r.calls, g.ID)
		for _, tg := range c.request.Talkgroups {
			if r.floors[tg] == g.ID {
				delete(r.floors, tg)
			}
		}
	}
	c.endError = result
	close(c.done)
	r.mu.Unlock()
	return result
}
func (r *Router) CancelOwner(owner string) {
	r.mu.Lock()
	calls := []dispatch.Grant{}
	for _, c := range r.calls {
		if c.owner == owner {
			calls = append(calls, c.grant)
		}
	}
	r.mu.Unlock()
	for _, g := range calls {
		ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
		_ = r.End(ctx, owner, g)
		cancel()
	}
}
func (r *Router) Active() []Request {
	r.mu.Lock()
	defer r.mu.Unlock()
	out := []Request{}
	for _, c := range r.calls {
		if c.ready {
			q := cloneRequest(c.request)
			q.Proof = ""
			q.Talkgroups = slices.Clone(q.Talkgroups)
			out = append(out, q)
		}
	}
	return out
}

func (r *Router) CancelAll() {
	r.mu.Lock()
	owners := make(map[string]struct{})
	for _, c := range r.calls {
		owners[c.owner] = struct{}{}
	}
	r.mu.Unlock()
	for owner := range owners {
		r.CancelOwner(owner)
	}
}
func (r *Router) watch() {
	defer r.wg.Done()
	ticker := time.NewTicker(100 * time.Millisecond)
	defer ticker.Stop()
	for {
		select {
		case <-r.ctx.Done():
			return
		case <-ticker.C:
			r.mu.Lock()
			expired := []*call{}
			for _, c := range r.calls {
				if !r.options.Now().Before(c.deadline) {
					expired = append(expired, c)
				}
			}
			r.mu.Unlock()
			for _, c := range expired {
				ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
				_ = r.End(ctx, c.owner, c.grant)
				cancel()
			}
		}
	}
}
func (r *Router) Close() error {
	r.mu.Lock()
	r.cancel()
	r.mu.Unlock()
	r.CancelAll()
	r.wg.Wait()
	return nil
}
