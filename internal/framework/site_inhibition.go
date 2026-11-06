package framework

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"slices"

	"open25.local/open25-fne/internal/radio"
)

// Actual state is Site-owned recovery protection, independent of current Zone
// intent and operator receipts. It contains no queued/replayable operation.
type siteInhibitionSnapshot struct {
	rows    []radio.InhibitRestore
	blocked map[int]bool
	err     error
}

func siteInhibitionState(rows []radio.InhibitRestore, err error) *siteInhibitionSnapshot {
	state := &siteInhibitionSnapshot{rows: slices.Clone(rows), blocked: map[int]bool{}, err: err}
	for _, r := range rows {
		state.blocked[r.RadioID] = r.Blocked || r.PendingUninhibit
	}
	return state
}

func (s *Service) siteActualInhibition() *siteInhibitionSnapshot {
	if state := s.actualInhibition.Load(); state != nil {
		return state
	}
	rows := []radio.InhibitRestore{}
	var err error
	if s.Store == nil {
		err = errors.New("Site actual inhibit state store is unavailable")
	} else {
		var raw json.RawMessage
		raw, _, err = s.Store.ReadCache("radio-inhibition-actual")
		if errors.Is(err, sql.ErrNoRows) {
			err = nil
		} else if err == nil {
			err = DecodeStrict(raw, &rows)
		}
	}
	if err == nil {
		if rows == nil || len(rows) > radio.InhibitRecordLimit {
			err = errors.New("Site actual inhibit state is invalid")
		}
		seen := map[int]bool{}
		for _, r := range rows {
			if r.RadioID < 1 || r.RadioID > 0xFFFFFB || seen[r.RadioID] || !r.Blocked {
				err = errors.New("Site actual inhibit state is invalid")
			}
			seen[r.RadioID] = true
		}
	}
	next := siteInhibitionState(rows, err)
	if s.actualInhibition.CompareAndSwap(nil, next) {
		return next
	}
	return s.actualInhibition.Load()
}

// Native calls this synchronously while owning publication. A missing or
// failed save cannot be represented as a confirmed actual clear.
func (s *Service) persistSiteInhibition(rows []radio.InhibitRestore) error {
	if s.Config().Role != RoleSite || s.Store == nil {
		return errors.New("actual inhibit state belongs to the Site")
	}
	rows = slices.Clone(rows)
	if rows == nil {
		rows = []radio.InhibitRestore{}
	}
	physical := []radio.Radio{}
	for _, r := range s.view().System.Radios {
		physical = append(physical, radio.Radio{RID: r.RID, Kind: r.Kind})
	}
	if err := radio.ValidateInhibition(physical, radio.InhibitPolicy{Restore: rows}); err != nil {
		return err
	}
	slices.SortFunc(rows, func(a, b radio.InhibitRestore) int { return a.RadioID - b.RadioID })
	if old := s.actualInhibition.Load(); old != nil && old.err == nil && slices.Equal(old.rows, rows) {
		return nil
	}
	raw, err := json.Marshal(rows)
	if err != nil {
		return err
	}
	if err = s.Store.WriteCache("radio-inhibition-actual", raw); err != nil {
		return err
	}
	s.actualInhibition.Store(siteInhibitionState(rows, nil))
	return nil
}

func (s *Service) applySiteInhibitionPolicy(ctx context.Context, desired []int) error {
	if s.Config().Role != RoleSite {
		return errors.New("only a Site can apply cached native inhibit policy")
	}
	current := s.currentRadioInhibition()
	if current.err != nil {
		if s.radio != nil {
			_ = s.radio.Stop(ctx)
		}
		return current.err
	}
	if !slices.Equal(current.desired, desired) {
		return context.Canceled
	}
	if err := ctx.Err(); err != nil {
		// Authority was already durably accepted. An expired HTTP caller must
		// not leave the previous RF policy operating while the worker catches up.
		s.cancelStaleRadioOperation()
		if s.radio != nil {
			return errors.Join(err, s.radio.Stop(ctx))
		}
		return err
	}
	v := s.view()
	state := s.siteActualInhibition()
	if state.err != nil {
		if s.radio != nil {
			_ = s.radio.Stop(ctx)
		}
		return state.err
	}
	physical := make([]radio.Radio, 0, len(v.System.Radios))
	for _, r := range v.System.Radios {
		physical = append(physical, radio.Radio{RID: r.RID, Kind: r.Kind, Enabled: r.Enabled})
	}
	restore := slices.Clone(state.rows)
	for i := range restore {
		if slices.Contains(desired, restore[i].RadioID) {
			restore[i].PendingUninhibit = false
		}
	}
	if err := radio.ValidateInhibition(physical, radio.InhibitPolicy{Desired: desired, Restore: restore}); err != nil {
		if s.radio != nil {
			_ = s.radio.Stop(ctx)
		}
		return err
	}
	if s.radio == nil {
		return nil
	}
	s.cancelStaleRadioOperation()
	return s.radio.ApplyInhibitionPolicy(ctx, desired)
}

// This admission check reads immutable caches only. Registration and native
// extended-function exceptions are handled separately by their protocol owner.
func (s *Service) radioServiceInhibited(rid int) bool {
	policy := s.inhibition.Load()
	if policy == nil || policy.err != nil {
		return true
	}
	_, desired := policy.lookup[rid]
	if desired {
		return true
	}
	if s.Config().Role == RoleSite {
		state := s.actualInhibition.Load()
		return state == nil || state.err != nil || state.blocked[rid]
	}
	return false
}
