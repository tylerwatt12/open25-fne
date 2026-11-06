package routing

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"slices"
	"sort"
	"time"

	"open25.local/open25-fne/internal/dispatch"
)

const (
	FASTStart = "fast_start"
	AllStart  = "all_start"
)

type Policy struct {
	StartMode string
	Wait      time.Duration
	Retry     time.Duration
}

type Delivery struct {
	SiteID  string `json:"site_id"`
	State   string `json:"state"`
	Message string `json:"message,omitempty"`
}

func cloneRequest(q Request) Request {
	q.Talkgroups = slices.Clone(q.Talkgroups)
	q.AuthorizationGroups = slices.Clone(q.AuthorizationGroups)
	return q
}

func (r *Router) event(q Request, kind, message string) {
	if r.options.Event != nil {
		r.options.Event(cloneRequest(q), kind, message)
	}
}

func (r *Router) policy(q Request) Policy {
	p := Policy{StartMode: FASTStart, Wait: 2 * time.Second, Retry: 100 * time.Millisecond}
	if r.options.Policy != nil {
		p = r.options.Policy(cloneRequest(q))
	}
	if p.StartMode != AllStart {
		p.StartMode = FASTStart
	}
	if p.Wait <= 0 || p.Wait > 15*time.Second {
		p.Wait = 2 * time.Second
	}
	if p.Retry < 10*time.Millisecond || p.Retry > time.Second {
		p.Retry = 100 * time.Millisecond
	}
	return p
}

func (r *Router) expand(q Request) (Request, error) {
	original := cloneRequest(q)
	expanded, err := r.options.Expand(cloneRequest(q))
	if err != nil {
		return q, err
	}
	identity := expanded
	identity.Talkgroups = original.Talkgroups
	if !sameRequest(identity, original) {
		return q, dispatch.ErrPermission
	}
	expanded.Talkgroups, err = groups(expanded.Talkgroups)
	if err != nil {
		return q, err
	}
	for _, id := range original.Talkgroups {
		if !slices.Contains(expanded.Talkgroups, id) {
			return q, errors.New("a patch cannot remove its originating talkgroup")
		}
	}
	return expanded, nil
}

func (r *Router) validateCall(c *call, frame *dispatch.Frame) error {
	q := cloneRequest(c.original)
	if q.EncryptionPending && frame != nil {
		q.EncryptionPending = false
		q.AlgorithmID, q.KeyID = int(frame.AlgorithmID), int(frame.KeyID)
	}
	if r.options.Authorize != nil {
		if err := r.options.Authorize(cloneRequest(q)); err != nil {
			return err
		}
	}
	if r.options.Expand != nil {
		q, err := r.expand(q)
		if err != nil {
			return err
		}
		if !slices.Equal(q.Talkgroups, c.request.Talkgroups) {
			return errors.New("the active patch membership changed")
		}
	}
	return nil
}

func priority(q Request) int {
	if q.Emergency {
		return 1
	}
	if q.Priority >= 2 && q.Priority <= 10 {
		return q.Priority
	}
	return 10
}

func (r *Router) targetList(q Request) (Selection, error) {
	if r.options.Targets == nil {
		return Selection{}, nil
	}
	selection := r.options.Targets(cloneRequest(q))
	excluded := make(map[string]string, len(selection.Excluded))
	for id, reason := range selection.Excluded {
		excluded[id] = reason
	}
	selection.Excluded = excluded
	seen := map[string]bool{}
	out := []Target{}
	for _, target := range selection.Targets {
		if target.ID == q.OriginSite {
			source := q.AuthorizationGroups
			if len(source) == 0 {
				source = q.Talkgroups
			}
			target.Groups = slices.DeleteFunc(slices.Clone(target.Groups), func(id int) bool { return slices.Contains(source, id) })
			if len(target.Groups) == 0 {
				selection.Excluded[target.ID] = "Source Site has no additional receiving talkgroups."
			}
		}
		if len(target.Groups) == 0 {
			continue
		}
		if target.ID == "" || target.Router == nil || seen[target.ID] {
			return Selection{}, errors.New("receiving Sites must have distinct identities and routers")
		}
		seen[target.ID] = true
		ids, err := groups(target.Groups)
		if err != nil {
			return Selection{}, err
		}
		for _, id := range ids {
			if !slices.Contains(q.Talkgroups, id) {
				return Selection{}, dispatch.ErrPermission
			}
		}
		target.Groups = ids
		out = append(out, target)
	}
	selection.Targets = out
	return selection, nil
}

func (r *Router) release(legs []leg) {
	for _, l := range legs {
		ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
		_ = l.target.Router.End(ctx, l.grant)
		cancel()
	}
}

// The call's media lock is held by the caller. A failed receiving Site leaves
// the call while healthy Sites and IP listeners continue. Its exact allocation
// drains separately; End still waits for every detached allocation to retire.
func (r *Router) retainAvailableLegs(ctx context.Context, c *call, visit func(leg) error) {
	kept := make([]leg, 0, len(c.legs))
	for i, l := range c.legs {
		err := visit(l)
		if ctx.Err() != nil {
			kept = append(kept, c.legs[i:]...)
			break
		}
		if err == nil {
			kept = append(kept, l)
			continue
		}
		c.delivery[l.target.ID] = Delivery{SiteID: l.target.ID, State: "unavailable", Message: err.Error()}
		c.cleanup.Add(1)
		go func(l leg) { defer c.cleanup.Done(); r.release([]leg{l}) }(l)
		r.event(c.request, "site_omitted", l.target.ID+": "+err.Error())
	}
	c.legs = kept
}

func (r *Router) targetBegin(ctx context.Context, c *call, target Target) (dispatch.Grant, error) {
	q := c.request.CallRequest
	// Receiving allocations are one-use identities. A retry or rejoining Site
	// must not reopen an ended allocation or collide with its stale release.
	var identity [16]byte
	if _, err := rand.Read(identity[:]); err != nil {
		return dispatch.Grant{}, err
	}
	q.ID = hex.EncodeToString(identity[:])
	q.Talkgroups = slices.Clone(target.Groups)
	q.Priority = priority(c.original)
	g, err := target.Router.Begin(ctx, q)
	if errors.Is(err, dispatch.ErrBusy) && c.original.Emergency && r.preemptAtSite(ctx, c, target.ID) {
		if _, e := rand.Read(identity[:]); e != nil {
			return dispatch.Grant{}, e
		}
		q.ID = hex.EncodeToString(identity[:])
		return target.Router.Begin(ctx, q)
	}
	return g, err
}

func (r *Router) admitIndependent(ctx context.Context, c *call, targets []Target) ([]leg, error) {
	type result struct {
		index int
		grant dispatch.Grant
		err   error
	}
	results := make(chan result, len(targets))
	for i, target := range targets {
		go func(i int, target Target) {
			attempt, done := context.WithTimeout(ctx, 750*time.Millisecond)
			defer done()
			grant, err := r.targetBegin(attempt, c, target)
			results <- result{i, grant, err}
		}(i, target)
	}
	ordered := make([]result, len(targets))
	for range targets {
		result := <-results
		ordered[result.index] = result
	}
	admitted := []leg{}
	for i, result := range ordered {
		target := targets[i]
		if result.err != nil {
			r.setDelivery(c, target.ID, "unavailable", result.err.Error())
			r.event(c.request, "site_omitted", target.ID+": "+result.err.Error())
		} else {
			admitted = append(admitted, leg{target, cloneGrant(result.grant)})
		}
	}
	if err := ctx.Err(); err != nil {
		r.release(admitted)
		return nil, err
	}
	return admitted, nil
}

func (r *Router) preemptAtSite(ctx context.Context, incoming *call, site string) bool {
	r.mu.Lock()
	candidates := []*call{}
	for _, c := range r.calls {
		if c != incoming && c.ready && !c.ending && !c.original.Emergency {
			candidates = append(candidates, c)
		}
	}
	r.mu.Unlock()
	sort.Slice(candidates, func(i, j int) bool {
		a, b := priority(candidates[i].original), priority(candidates[j].original)
		if a != b {
			return a > b
		}
		return candidates[i].request.ID < candidates[j].request.ID
	})
	for _, old := range candidates {
		old.media.RLock()
		uses := slices.ContainsFunc(old.legs, func(l leg) bool { return l.target.ID == site })
		old.media.RUnlock()
		if !uses {
			continue
		}
		r.event(old.request, "call_preempted", "Interrupted by an emergency call.")
		return r.End(ctx, old.owner, old.grant) == nil
	}
	return false
}

func (r *Router) setDelivery(c *call, site, state, message string) {
	c.media.Lock()
	c.delivery[site] = Delivery{SiteID: site, State: state, Message: message}
	c.media.Unlock()
}

// Each AllStart retry releases partial allocations before trying the complete
// current audience again. A failed setup cannot strand another Site's channel.
func (r *Router) admit(parent context.Context, c *call) error {
	ctx, cancel := context.WithCancel(parent)
	stop := context.AfterFunc(c.ctx, cancel)
	defer func() { stop(); cancel() }()
	if c.policy.StartMode == AllStart {
		var waitCancel context.CancelFunc
		ctx, waitCancel = context.WithTimeout(ctx, c.policy.Wait)
		defer waitCancel()
	}
	for {
		selection, err := r.targetList(c.request)
		if err != nil {
			return err
		}
		targets := selection.Targets
		admitted := []leg{}
		var busy error
		if c.policy.StartMode == FASTStart && len(c.original.Talkgroups) == 1 {
			admitted, err = r.admitIndependent(ctx, c, targets)
		} else {
			for _, target := range targets {
				if err = ctx.Err(); err != nil {
					break
				}
				grant, e := r.targetBegin(ctx, c, target)
				if e != nil {
					r.setDelivery(c, target.ID, "unavailable", e.Error())
					if ctx.Err() != nil {
						err = ctx.Err()
						break
					}
					// Explicit multiselect is atomic, including in FASTStart mode.
					// A single source expanded through a patch may start partially.
					if c.policy.StartMode == AllStart || len(c.original.Talkgroups) > 1 {
						busy = e
						break
					}
					r.event(c.request, "site_omitted", target.ID+": "+e.Error())
					continue
				}
				admitted = append(admitted, leg{target, cloneGrant(grant)})
			}
		}
		if err != nil || busy != nil {
			r.release(admitted)
			if c.ctx.Err() != nil {
				return dispatch.ErrUnavailable
			}
			if err != nil {
				return err
			}
			if c.policy.StartMode != AllStart || (!errors.Is(busy, dispatch.ErrBusy) && !errors.Is(busy, dispatch.ErrUnavailable)) {
				return busy
			}
			select {
			case <-ctx.Done():
				if c.ctx.Err() != nil {
					return dispatch.ErrUnavailable
				}
				if parent.Err() != nil {
					return parent.Err()
				}
				return dispatch.ErrBusy
			case <-time.After(c.policy.Retry):
				if r.options.Authorize != nil {
					if err = r.options.Authorize(c.original); err != nil {
						return err
					}
				}
				continue
			}
		}
		c.media.Lock()
		if c.ended {
			c.media.Unlock()
			r.release(admitted)
			return dispatch.ErrUnavailable
		}
		c.legs = admitted
		for _, l := range admitted {
			c.delivery[l.target.ID] = Delivery{SiteID: l.target.ID, State: "active"}
		}
		c.media.Unlock()
		return nil
	}
}

// Newly available or newly interested Sites receive only subsequent live
// frames. No old speech is buffered or replayed to late arrivals.
func (r *Router) follow(c *call) {
	defer r.wg.Done()
	ticker := time.NewTicker(c.policy.Retry)
	defer ticker.Stop()
	for {
		select {
		case <-c.ctx.Done():
			return
		case <-ticker.C:
			if err := r.validateCall(c, nil); err != nil {
				_ = r.End(context.Background(), c.owner, c.grant)
				return
			}
			selection, err := r.targetList(c.request)
			if err != nil {
				continue
			}
			targets := selection.Targets
			wanted := map[string]Target{}
			for _, target := range targets {
				wanted[target.ID] = target
			}
			c.media.Lock()
			if c.ended {
				c.media.Unlock()
				return
			}
			kept, removed := []leg{}, []leg{}
			reasons := map[string]string{}
			for _, l := range c.legs {
				if target, ok := wanted[l.target.ID]; ok && slices.Equal(target.Groups, l.target.Groups) {
					kept = append(kept, l)
					delete(wanted, target.ID)
				} else {
					reason := selection.Excluded[l.target.ID]
					if target, ok := wanted[l.target.ID]; ok {
						change := fmt.Sprintf("Receiving talkgroups changed from %v to %v.", l.target.Groups, target.Groups)
						if reason != "" {
							change += " " + reason
						}
						reason = change
					} else if reason == "" {
						reason = selection.Reason
					}
					if reason == "" {
						reason = "Site is no longer eligible for this call."
					}
					removed = append(removed, l)
					reasons[l.target.ID] = reason
					c.delivery[l.target.ID] = Delivery{SiteID: l.target.ID, State: "left", Message: reason}
				}
			}
			c.legs = kept
			c.media.Unlock()
			r.release(removed)
			for _, l := range removed {
				r.event(c.request, "site_left", l.target.ID+": "+reasons[l.target.ID])
			}
			for _, target := range targets {
				if _, missing := wanted[target.ID]; !missing {
					continue
				}
				ctx, cancel := context.WithTimeout(c.ctx, 750*time.Millisecond)
				grant, err := r.targetBegin(ctx, c, target)
				cancel()
				if err != nil {
					r.setDelivery(c, target.ID, "unavailable", err.Error())
					continue
				}
				c.media.Lock()
				if c.ended || c.ctx.Err() != nil {
					c.media.Unlock()
					r.release([]leg{{target, grant}})
					return
				}
				c.legs = append(c.legs, leg{target, cloneGrant(grant)})
				c.delivery[target.ID] = Delivery{SiteID: target.ID, State: "active"}
				c.media.Unlock()
				r.event(c.request, "site_joined", target.ID)
			}
		}
	}
}

func (r *Router) Delivery(owner string, g dispatch.Grant) []Delivery {
	c, err := r.find(owner, g)
	if err != nil {
		return nil
	}
	c.media.RLock()
	defer c.media.RUnlock()
	out := make([]Delivery, 0, len(c.delivery))
	for _, d := range c.delivery {
		out = append(out, d)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].SiteID < out[j].SiteID })
	return out
}
