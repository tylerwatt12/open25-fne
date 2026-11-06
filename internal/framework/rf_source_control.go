package framework

import (
	"context"
	"errors"
	"maps"
	"net/http"
	"open25.local/open25-fne/internal/dispatch"
	"open25.local/open25-fne/internal/radio"
	"open25.local/open25-fne/internal/routing"
	"slices"
	"time"
)

func (s *Service) rfAuthorityScope() rfAuthorityScope {
	view := s.view()
	scope := rfAuthorityScope{Sites: map[string]bool{}, Radios: map[int]bool{}, Groups: map[int]bool{}}
	if view.Zone != nil {
		for _, site := range view.Zone.Sites {
			scope.Sites[site.ID] = true
		}
	}
	for _, r := range view.System.Radios {
		if r.Kind == "subscriber" {
			scope.Radios[r.RID] = true
		}
	}
	for _, g := range view.System.Talkgroups {
		scope.Groups[g.TGID] = true
	}
	return scope
}
func (s *Service) rfAuthorityInternal(w http.ResponseWriter, r *http.Request, peer PeerCredential) {
	if s.Config().Role != RoleZone || peer.Role != RoleSite {
		fail(w, 403, dispatch.ErrPermission)
		return
	}
	if r.Method != http.MethodPost {
		w.WriteHeader(405)
		return
	}
	var update rfAuthorityUpdate
	if err := readJSON(w, r, &update); err != nil {
		fail(w, 400, err)
		return
	}
	if update.SiteID != peer.InstanceID {
		fail(w, 403, dispatch.ErrPermission)
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), time.Second)
	defer cancel()
	if err := s.rfAuthority.apply(ctx, update, s.rfAuthorityScope()); err != nil {
		fail(w, 409, err)
		return
	}
	respond(w, 200, map[string]bool{"accepted": true})
}

// Independent of catalogue/history publication: SQL or browser fanout cannot
// hold this coalesced, bounded stream of physical control facts.
func (s *Service) publishRFSourceState() {
	tick := time.NewTicker(100 * time.Millisecond)
	defer tick.Stop()
	var accepted uint64
	var run time.Time
	var acceptedScope rfAuthorityScope
	for {
		select {
		case <-s.operationCtx.Done():
			return
		case <-tick.C:
		}
		state := s.radio.SourceState()
		scope := s.rfAuthorityScope()
		if state.NativeStartedAt.IsZero() || (state.Revision == accepted && state.NativeStartedAt.Equal(run) && maps.Equal(scope.Radios, acceptedScope.Radios) && maps.Equal(scope.Groups, acceptedScope.Groups)) {
			continue
		}
		update := rfSourceUpdateInScope(state, scope)
		address := s.Config().UpstreamURL
		if address == "" {
			continue
		}
		ctx, cancel := context.WithTimeout(s.operationCtx, time.Second)
		err := s.requestAt(ctx, address, http.MethodPost, "/internal/rf-authority", update, nil, nil)
		cancel()
		if err == nil {
			accepted, run = state.Revision, state.NativeStartedAt
			acceptedScope = scope
		}
	}
}
func rfSourceUpdateInScope(state radio.SourceState, scope rfAuthorityScope) rfAuthorityUpdate {
	update := rfUpdateFromSource(state)
	// A stopped controller retains its old profile and exclusion facts. Send
	// only identities in the currently accepted System, without changing either.
	update.Affiliations = slices.DeleteFunc(slices.Clone(update.Affiliations), func(a radio.Affiliation) bool {
		return !scope.Radios[a.RID] || !scope.Groups[a.TGID]
	})
	for _, rows := range []*[]radio.Registration{&update.Registrations, &update.Terminals, &update.NullAffiliations} {
		*rows = slices.DeleteFunc(slices.Clone(*rows), func(r radio.Registration) bool { return !scope.Radios[r.RID] })
	}
	update.Retired = slices.DeleteFunc(slices.Clone(update.Retired), func(r radio.RetiredSourceFact) bool { return !scope.Radios[r.RID] })
	update.Withdrawals = slices.DeleteFunc(update.Withdrawals, func(w rfAuthorityWithdrawal) bool {
		return !scope.Radios[w.RadioID] || !scope.Groups[w.GroupID]
	})
	return update
}
func rfUpdateFromSource(state radio.SourceState) rfAuthorityUpdate {
	update := rfAuthorityUpdate{SiteID: state.SiteID, NativeStartedAt: state.NativeStartedAt, ObservedAt: state.ObservedAt, Affiliations: state.Affiliations, Registrations: state.Registrations, Terminals: state.Terminals, NullAffiliations: state.NullAffiliations, Retired: state.Retired}
	for _, w := range state.Withdrawals {
		update.Withdrawals = append(update.Withdrawals, rfAuthorityWithdrawal{RadioID: w.RadioID, GroupID: w.GroupID, AffiliatedAt: w.AffiliatedAt, NativeStartedAt: w.NativeStartedAt})
	}
	return update
}
func (s *Service) observeRFSource(q routing.Request) (routing.Request, error) {
	if s.radio == nil || q.SourceKind != "rf" || q.OriginSite != s.Config().InstanceID || len(q.Talkgroups) != 1 {
		return q, dispatch.ErrPermission
	}
	state := s.radio.SourceState()
	if !state.Active || state.SiteID != q.OriginSite || state.NativeStartedAt.IsZero() || !s.radio.SourceAllowed(q.RadioID, q.Talkgroups[0], state.NativeStartedAt) {
		return q, dispatch.ErrUnavailable
	}
	a, found := findRFAffiliation(state.Affiliations, q.RadioID, q.Talkgroups[0])
	if !found {
		return q, errors.New("current control channel has no matching RF affiliation")
	}
	for _, w := range state.Withdrawals {
		if w.RadioID == a.RID && w.GroupID == a.TGID && w.AffiliatedAt.Equal(a.At) && w.NativeStartedAt.Equal(state.NativeStartedAt) {
			return q, dispatch.ErrPermission
		}
	}
	for _, list := range [][]radio.Registration{state.Terminals, state.NullAffiliations} {
		for _, f := range list {
			if f.RID == a.RID && !a.At.After(f.At) {
				return q, dispatch.ErrPermission
			}
		}
	}
	q.RFSource = routing.RFSourceObservation{NativeStartedAt: state.NativeStartedAt, ObservedAt: state.ObservedAt, AffiliatedAt: a.At}
	for _, r := range state.Registrations {
		if r.RID == q.RadioID {
			q.RFSource.RegisteredAt = r.At
			break
		}
	}
	return q, nil
}
func findRFAffiliation(facts []radio.Affiliation, rid, tg int) (radio.Affiliation, bool) {
	at := slices.IndexFunc(facts, func(a radio.Affiliation) bool { return a.RID == rid && a.TGID == tg })
	if at < 0 {
		return radio.Affiliation{}, false
	}
	return facts[at], true
}
func rfCallUpdate(q routing.Request) rfAuthorityUpdate {
	u := rfAuthorityUpdate{SiteID: q.OriginSite, NativeStartedAt: q.RFSource.NativeStartedAt, ObservedAt: q.RFSource.ObservedAt, Affiliations: []radio.Affiliation{{RID: q.RadioID, TGID: q.Talkgroups[0], At: q.RFSource.AffiliatedAt}}}
	if !q.RFSource.RegisteredAt.IsZero() {
		u.Registrations = []radio.Registration{{RID: q.RadioID, At: q.RFSource.RegisteredAt}}
	}
	return u
}

// Ownership is decided after a valid semantic fact is durable, including
// ties that must fence an existing call. This check authenticates its evidence.
func (s *Service) validateRFObservation(q routing.Request) error {
	if s.rfAuthority == nil || s.rfAuthority.ctx.Err() != nil || len(q.Talkgroups) != 1 {
		return dispatch.ErrUnavailable
	}
	accepted := s.rfAuthority.value.Load()
	update := rfCallUpdate(q)
	if err := validateRFUpdate(update, accepted.document, s.rfAuthorityScope()); err != nil {
		return err
	}

	return nil
}
func (s *Service) acceptRFObservation(ctx context.Context, q routing.Request) error {
	if err := s.broker.authorizePolicy(q); err != nil {
		return err
	}
	if err := s.validateRFObservation(q); err != nil {
		return err
	}
	if err := s.rfAuthority.apply(ctx, rfCallUpdate(q), s.rfAuthorityScope()); err != nil {
		return err
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	return s.broker.authorize(q)
}
