package framework

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"open25.local/open25-fne/internal/dispatch"
	"os"
	"path/filepath"
	"sync"
	"sync/atomic"
	"time"
)

type rfAuthoritySnapshot struct {
	document rfAuthorityDocument
	owners   map[int]affiliationOwner
	fenced   map[int]bool
	version  uint64
	err      error
}
type rfSourceAuthority struct {
	path    string
	value   atomic.Pointer[rfAuthoritySnapshot]
	mu      sync.Mutex // staging/publication only, never disk I/O
	desired rfAuthorityDocument
	version uint64
	changed chan struct{}
	wake    chan struct{}
	waiters chan struct{}
	ctx     context.Context
	cancel  context.CancelFunc
	done    chan struct{}
	write   func(string, any) error
}

func writeRFAuthority(path string, value any) error {
	raw, err := json.MarshalIndent(value, "", "  ")
	if err != nil {
		return err
	}
	if len(raw)+1 > 32<<20 {
		return errors.New("RF source state exceeds its bounded storage size")
	}
	if err := os.MkdirAll(filepath.Dir(path), 0700); err != nil {
		return err
	}
	f, err := os.CreateTemp(filepath.Dir(path), ".rf-source-*")
	if err != nil {
		return err
	}
	defer os.Remove(f.Name())
	if err = f.Chmod(0600); err == nil {
		_, err = f.Write(append(raw, '\n'))
	}
	if err == nil {
		err = f.Sync()
	}
	closeErr := f.Close()
	if err == nil {
		err = closeErr
	}
	if err != nil {
		return err
	}
	if err := os.Rename(f.Name(), path); err != nil {
		return err
	}
	dir, err := os.Open(filepath.Dir(path))
	if err != nil {
		return err
	}
	defer dir.Close()
	return dir.Sync()
}
func openRFSourceAuthority(path, zoneID string) (*rfSourceAuthority, error) {
	info, err := os.Lstat(path)
	if err != nil {
		return nil, fmt.Errorf("RF source state unavailable; prepare it offline: %w", err)
	}
	if !info.Mode().IsRegular() || info.Mode().Perm()&0077 != 0 {
		return nil, errors.New("RF source state must be an owner-only regular file")
	}
	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	raw, err := io.ReadAll(io.LimitReader(f, (32<<20)+1))
	if err != nil {
		return nil, err
	}
	var d rfAuthorityDocument
	if len(raw) > 32<<20 || DecodeStrict(raw, &d) != nil || d.ZoneID != zoneID || d.NativeRuns == nil || d.Units == nil {
		return nil, errors.New("RF source state is corrupt or belongs to another Zone")
	}
	if err := validateRFAuthorityDocument(d); err != nil {
		return nil, err
	}
	ctx, cancel := context.WithCancel(context.Background())
	a := &rfSourceAuthority{path: path, desired: d, changed: make(chan struct{}), wake: make(chan struct{}, 1), waiters: make(chan struct{}, 64), ctx: ctx, cancel: cancel, done: make(chan struct{}), write: writeRFAuthority}
	a.value.Store(&rfAuthoritySnapshot{document: d, owners: rfAuthorityOwners(d), fenced: map[int]bool{}})
	go a.run()
	return a, nil
}
func (a *rfSourceAuthority) lookup(rid int) (affiliationOwner, bool) {
	if a == nil || a.ctx.Err() != nil {
		return affiliationOwner{}, false
	}
	v := a.value.Load()
	if v == nil || v.fenced[rid] {
		return affiliationOwner{}, false
	}
	owner, ok := v.owners[rid]
	return owner, ok
}
func (a *rfSourceAuthority) close() {
	if a != nil {
		a.cancel()
		<-a.done
	}
}

type rfAuthorityScope struct {
	Sites  map[string]bool
	Radios map[int]bool
	Groups map[int]bool
}

func validateRFAuthorityDocument(d rfAuthorityDocument) error {
	if !recordID.MatchString(d.ZoneID) {
		return errors.New("invalid RF source Zone identity")
	}
	for site, run := range d.NativeRuns {
		if !recordID.MatchString(site) || run.IsZero() || run.After(time.Now().Add(time.Minute)) {
			return errors.New("invalid RF source native run")
		}
	}
	for rid, unit := range d.Units {
		if rid < 1 || rid > 0xfffffb {
			return errors.New("invalid RF source Radio ID")
		}
		for kind, f := range []rfAuthorityFact{unit.Affiliation, unit.Registration, unit.Terminal, unit.NullAffiliation, unit.MembershipUnknown} {
			if f.At.IsZero() {
				if f != (rfAuthorityFact{}) {
					return errors.New("incomplete RF source fact")
				}
				continue
			}
			if !recordID.MatchString(f.SiteID) || f.At.After(time.Now().Add(time.Minute)) || f.NativeStartedAt.After(f.At) || f.NativeStartedAt.After(d.NativeRuns[f.SiteID]) || (f.SiteAmbiguous && !f.Ambiguous) {
				return errors.New("invalid RF source fact")
			}
			if kind == 0 {
				if f.GroupID < 1 || f.GroupID > 65534 {
					return errors.New("invalid RF source talkgroup")
				}
			} else if f.GroupID != 0 || f.Withdrawn {
				return errors.New("invalid RF source terminal or registration")
			}
		}
	}
	return nil
}
func validateRFUpdate(u rfAuthorityUpdate, d rfAuthorityDocument, scope rfAuthorityScope) error {
	now := time.Now()
	if !scope.Sites[u.SiteID] || u.NativeStartedAt.IsZero() || u.NativeStartedAt.After(u.ObservedAt) || u.NativeStartedAt.Before(d.NativeRuns[u.SiteID]) || u.ObservedAt.IsZero() || u.ObservedAt.After(now.Add(time.Minute)) || now.Sub(u.ObservedAt) > 4*time.Second {
		return errors.New("RF source Site/run observation is invalid or stale")
	}
	if len(u.Affiliations) > len(scope.Radios) || len(u.Registrations) > len(scope.Radios) || len(u.Terminals) > len(scope.Radios) || len(u.NullAffiliations) > len(scope.Radios) || len(u.Withdrawals) > len(scope.Radios) || len(u.Retired) > 3*len(scope.Radios) {
		return errors.New("RF source fact set exceeds configured identities")
	}
	valid := func(rid int, at time.Time) bool {
		return scope.Radios[rid] && !at.IsZero() && !at.Before(u.NativeStartedAt) && !at.After(u.ObservedAt)
	}
	seen := map[int]bool{}
	for _, r := range u.Affiliations {
		if seen[r.RID] || !valid(r.RID, r.At) || !scope.Groups[r.TGID] {
			return errors.New("invalid RF source affiliation")
		}
		seen[r.RID] = true
	}
	seen = map[int]bool{}
	for _, r := range u.Registrations {
		if seen[r.RID] || !valid(r.RID, r.At) {
			return errors.New("invalid RF source registration")
		}
		seen[r.RID] = true
	}
	seen = map[int]bool{}
	for _, r := range u.Terminals {
		if seen[r.RID] || !valid(r.RID, r.At) {
			return errors.New("invalid RF source terminal")
		}
		seen[r.RID] = true
	}
	seen = map[int]bool{}
	for _, n := range u.NullAffiliations {
		if seen[n.RID] || !valid(n.RID, n.At) {
			return errors.New("invalid RF source null affiliation")
		}
		seen[n.RID] = true
	}
	retiredSeen := map[struct {
		rid  int
		kind string
	}]bool{}
	for _, r := range u.Retired {
		key := struct {
			rid  int
			kind string
		}{r.RID, r.Kind}
		if retiredSeen[key] || !scope.Radios[r.RID] || r.NativeStartedAt.IsZero() || r.NativeStartedAt.After(u.NativeStartedAt) || r.At.Before(r.NativeStartedAt) || r.At.After(u.ObservedAt) || (r.Kind != "registration" && r.Kind != "null_affiliation" && r.Kind != "deregistration") {
			return errors.New("invalid retired RF control fact")
		}
		retiredSeen[key] = true
	}
	seen = map[int]bool{}
	for _, w := range u.Withdrawals {
		if seen[w.RadioID] || !scope.Radios[w.RadioID] || !scope.Groups[w.GroupID] || w.NativeStartedAt.IsZero() || w.NativeStartedAt.After(u.NativeStartedAt) || w.AffiliatedAt.Before(w.NativeStartedAt) || w.AffiliatedAt.After(u.ObservedAt) {
			return errors.New("invalid RF source membership withdrawal")
		}
		seen[w.RadioID] = true
	}
	return nil
}
func changesRFAuthority(d rfAuthorityDocument, u rfAuthorityUpdate) bool {
	if u.NativeStartedAt.After(d.NativeRuns[u.SiteID]) {
		return true
	}
	for _, r := range u.Affiliations {
		old := d.Units[r.RID].Affiliation
		if old != mergeRFFact(old, rfAuthorityFact{SiteID: u.SiteID, GroupID: r.TGID, At: r.At.UTC(), NativeStartedAt: u.NativeStartedAt.UTC()}) {
			return true
		}
	}
	for _, r := range u.Registrations {
		old := d.Units[r.RID].Registration
		if old != mergeRFFact(old, rfAuthorityFact{SiteID: u.SiteID, At: r.At.UTC(), NativeStartedAt: u.NativeStartedAt.UTC()}) {
			return true
		}
	}
	for _, r := range u.Terminals {
		old := d.Units[r.RID].Terminal
		if old != mergeRFFact(old, rfAuthorityFact{SiteID: u.SiteID, At: r.At.UTC(), NativeStartedAt: u.NativeStartedAt.UTC()}) {
			return true
		}
	}
	for _, n := range u.NullAffiliations {
		old := d.Units[n.RID].NullAffiliation
		if old != mergeRFFact(old, rfAuthorityFact{SiteID: u.SiteID, At: n.At.UTC(), NativeStartedAt: u.NativeStartedAt.UTC()}) {
			return true
		}
	}
	for _, r := range u.Retired {
		old := d.Units[r.RID]
		if old != mergeRFRetired(old, u.SiteID, r) {
			return true
		}
	}
	for _, w := range u.Withdrawals {
		old := d.Units[w.RadioID].Affiliation
		if old != mergeRFWithdrawal(old, u.SiteID, w) {
			return true
		}
	}
	return false
}
func updateRFFenced(v *rfAuthoritySnapshot, u rfAuthorityUpdate) bool {
	for _, r := range u.Affiliations {
		if v.fenced[r.RID] {
			return true
		}
	}
	for _, r := range u.Registrations {
		if v.fenced[r.RID] {
			return true
		}
	}
	for _, r := range u.Terminals {
		if v.fenced[r.RID] {
			return true
		}
	}
	for _, n := range u.NullAffiliations {
		if v.fenced[n.RID] {
			return true
		}
	}
	for _, r := range u.Retired {
		if v.fenced[r.RID] {
			return true
		}
	}
	for _, r := range u.Withdrawals {
		if v.fenced[r.RadioID] {
			return true
		}
	}
	return false
}
func rfAuthorityDifference(accepted, desired rfAuthorityDocument) map[int]bool {
	fenced := map[int]bool{}
	for rid, unit := range desired.Units {
		if accepted.Units[rid] != unit || !accepted.NativeRuns[unit.Affiliation.SiteID].Equal(desired.NativeRuns[unit.Affiliation.SiteID]) {
			fenced[rid] = true
		}
	}
	return fenced
}
func (a *rfSourceAuthority) apply(ctx context.Context, u rfAuthorityUpdate, scope rfAuthorityScope) error {
	if a == nil || a.ctx.Err() != nil {
		return dispatch.ErrUnavailable
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	accepted := a.value.Load()
	if err := validateRFUpdate(u, accepted.document, scope); err != nil {
		return err
	}
	if !changesRFAuthority(accepted.document, u) && !updateRFFenced(accepted, u) {
		return nil
	}
	select {
	case a.waiters <- struct{}{}:
		defer func() { <-a.waiters }()
	case <-ctx.Done():
		return ctx.Err()
	case <-a.ctx.Done():
		return dispatch.ErrUnavailable
	default:
		return errors.New("RF source admission capacity reached")
	}
	a.mu.Lock()
	if a.ctx.Err() != nil || ctx.Err() != nil {
		a.mu.Unlock()
		return dispatch.ErrUnavailable
	}
	if err := validateRFUpdate(u, a.desired, scope); err != nil {
		a.mu.Unlock()
		return err
	}
	next, _, changed := mergeRFAuthority(a.desired, u)
	if changed {
		a.desired = next
		a.version++
	}
	version := a.version
	current := a.value.Load()
	a.value.Store(&rfAuthoritySnapshot{document: current.document, owners: current.owners, fenced: rfAuthorityDifference(current.document, a.desired), version: current.version})
	select {
	case a.wake <- struct{}{}:
	default:
	}
	for {
		wait := a.changed
		a.mu.Unlock()
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-a.ctx.Done():
			return dispatch.ErrUnavailable
		case <-wait:
		}
		a.mu.Lock()
		current = a.value.Load()
		if current.version >= version || current.err != nil {
			err := current.err
			if current.version >= version {
				err = nil
			}
			a.mu.Unlock()
			if ctx.Err() != nil {
				return ctx.Err()
			}
			return err
		}
	}
}
func (a *rfSourceAuthority) run() {
	defer close(a.done)
	for {
		select {
		case <-a.ctx.Done():
			return
		case <-a.wake:
		}
		a.mu.Lock()
		current := a.value.Load()
		if current.version == a.version && current.err == nil {
			a.mu.Unlock()
			continue
		}
		doc, version := cloneRFAuthority(a.desired), a.version
		a.mu.Unlock()
		err := a.write(a.path, doc)
		a.mu.Lock()
		if a.ctx.Err() == nil {
			old := a.value.Load()
			if err == nil {
				a.value.Store(&rfAuthoritySnapshot{document: doc, owners: rfAuthorityOwners(doc), fenced: rfAuthorityDifference(doc, a.desired), version: version})
			} else {
				a.value.Store(&rfAuthoritySnapshot{document: old.document, owners: old.owners, fenced: rfAuthorityDifference(old.document, a.desired), version: old.version, err: fmt.Errorf("RF source state commit failed: %w", err)})
			}
			close(a.changed)
			a.changed = make(chan struct{})
		}
		a.mu.Unlock()
	}
}
