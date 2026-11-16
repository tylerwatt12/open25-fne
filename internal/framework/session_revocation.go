package framework

import (
	"context"
	"encoding/hex"
	"encoding/json"
	"errors"
	"net/http"
	"strings"
	"time"

	"open25.local/open25-fne/internal/historypriority"
)

type sessionRevocation struct {
	FamilyID string    `json:"family_id"`
	Expires  time.Time `json:"expires"`
	Pending  bool      `json:"pending,omitempty"`
}
type sessionFamilyStatus struct {
	Revoked bool      `json:"revoked"`
	Expires time.Time `json:"expires,omitempty"`
	Pending bool      `json:"pending,omitempty"`
}

// Revocation delivery waits for mandatory durable writes at each authenticated
// parent. This budget belongs only to administrator sign-out delivery, never
// control/status requests, RF deadlines or authentication/session lifetimes.
const revocationDeliveryBudget = 30 * time.Second

func validSessionFamily(id string) bool {
	b, err := hex.DecodeString(id)
	return err == nil && len(b) == 32
}
func (s *Service) familyRevocation(id string) (sessionRevocation, bool) {
	var v sessionRevocation
	err := s.Store.authRead("revoked", id, &v)
	return v, err == nil
}

// Tombstones and pending delivery share one durable record. Revocation is
// monotonic: reconnect merges it; a stale account/catalog cannot resurrect it.
func (s *Service) recordRevocation(v sessionRevocation) error {
	if !validSessionFamily(v.FamilyID) || !v.Expires.After(time.Now()) || v.Expires.After(time.Now().Add(8*time.Hour+time.Minute)) {
		return errors.New("invalid session revocation")
	}
	s.Store.mu.Lock()
	old, exists := s.familyRevocation(v.FamilyID)
	if exists {
		if !old.Expires.Before(v.Expires) && (!v.Pending || old.Pending) {
			s.Store.mu.Unlock()
			return nil
		}
		if old.Expires.After(v.Expires) {
			v.Expires = old.Expires
		}
		v.Pending = v.Pending || old.Pending
	}
	raw, err := json.Marshal(v)
	if err != nil {
		s.Store.mu.Unlock()
		return err
	}
	turn, err := historypriority.AcquireForeground()
	if err != nil {
		s.Store.mu.Unlock()
		return err
	}
	tx, err := s.Store.db.Begin()
	if err != nil {
		err = errors.Join(err, turn.Release())
		s.Store.mu.Unlock()
		return err
	}
	var names []string
	commit := func() error {
		if _, err := tx.Exec("DELETE FROM framework_documents WHERE name LIKE 'auth:%' AND at<=?", time.Now().UTC().Format(authTimeFormat)); err != nil {
			return err
		}
		var count int
		if err := tx.QueryRow("SELECT count(*) FROM framework_documents WHERE name LIKE 'auth:revoked:%' AND name<>?", authName("revoked", v.FamilyID)).Scan(&count); err != nil {
			return err
		}
		if count >= 4096 {
			return errors.New("too many retained session revocations")
		}
		if _, err := tx.Exec("INSERT OR REPLACE INTO framework_documents(name,document,at) VALUES(?,?,?)", authName("revoked", v.FamilyID), raw, v.Expires.UTC().Format(authTimeFormat)); err != nil {
			return err
		}
		rows, err := tx.Query(`SELECT name FROM framework_documents WHERE name LIKE 'auth:session:%' AND json_extract(document,'$.FamilyID')=?`, v.FamilyID)
		if err != nil {
			return err
		}
		for rows.Next() {
			var name string
			if err = rows.Scan(&name); err != nil {
				break
			}
			names = append(names, name)
		}
		if err == nil {
			err = rows.Err()
		}
		rows.Close()
		if err != nil {
			return err
		}
		if _, err = tx.Exec(`DELETE FROM framework_documents WHERE name LIKE 'auth:session:%' AND json_extract(document,'$.FamilyID')=?`, v.FamilyID); err != nil {
			return err
		}
		return tx.Commit()
	}
	err = commit()
	if err != nil {
		_ = tx.Rollback()
	}
	err = errors.Join(err, turn.Release())
	s.Store.mu.Unlock()
	if err != nil {
		return err
	}
	s.ssoMu.Lock()
	for key, grant := range s.ssoCodes {
		if grant.FamilyID == v.FamilyID {
			delete(s.ssoCodes, key)
		}
	}
	s.ssoMu.Unlock()
	// The hub stores token digests; invalidate their digest-matching sessions without
	// persisting or returning those keys from the session store.
	if s.hub != nil {
		for _, name := range names {
			raw, err := hex.DecodeString(strings.TrimPrefix(name, "auth:session:"))
			if err == nil && len(raw) == 32 {
				var digest [32]byte
				copy(digest[:], raw)
				s.hub.InvalidateSessionDigest(digest)
			}
		}
	}
	return nil
}
func (s *Service) deliverRevocation(ctx context.Context, v sessionRevocation) bool {
	if s.Config().Role == RoleSystem {
		return true
	}
	var reply sessionFamilyStatus
	if err := s.upstreamRequest(ctx, "POST", "/internal/sso/revoke", sessionRevocation{FamilyID: v.FamilyID, Expires: v.Expires}, &reply); err != nil || !reply.Revoked {
		return false
	}
	// A parent acknowledgment means it durably owns onward delivery, even if the
	// authority is currently unreachable from that parent.
	v.Pending = false
	return s.Store.authPut("revoked", v.FamilyID, v, v.Expires) == nil && !reply.Pending
}
func (s *Service) flushSessionRevocations(ctx context.Context) error {
	rows, err := s.Store.readDB.QueryContext(ctx, `SELECT document FROM framework_documents WHERE name LIKE 'auth:revoked:%' AND json_extract(document,'$.pending')=1 AND at>? LIMIT 32`, time.Now().UTC().Format(authTimeFormat))
	if err != nil {
		return err
	}
	var pending []sessionRevocation
	for rows.Next() {
		var raw []byte
		if err = rows.Scan(&raw); err != nil {
			break
		}
		var v sessionRevocation
		if err = json.Unmarshal(raw, &v); err != nil {
			break
		}
		pending = append(pending, v)
	}
	if err == nil {
		err = rows.Err()
	}
	rows.Close()
	if err != nil {
		return err
	}
	for _, v := range pending {
		if !s.deliverRevocation(ctx, v) {
			return errors.New("shared sign-out delivery is pending")
		}
	}
	return nil
}
func (s *Service) familyStatus(ctx context.Context, id string) sessionFamilyStatus {
	if v, ok := s.familyRevocation(id); ok {
		return sessionFamilyStatus{Revoked: true, Expires: v.Expires}
	}
	if s.Config().Role == RoleSystem || s.parentIsolated.Load() {
		return sessionFamilyStatus{}
	}
	var reply sessionFamilyStatus
	if err := s.upstreamRequest(ctx, "POST", "/internal/sso/status", struct {
		FamilyID string `json:"family_id"`
	}{id}, &reply); err == nil && reply.Revoked {
		if s.recordRevocation(sessionRevocation{FamilyID: id, Expires: reply.Expires}) == nil {
			return reply
		}
		// Failure to cache a confirmed revocation never makes the session valid.
		return reply
	}
	return sessionFamilyStatus{}
}
func (s *Service) sharedSessionRevoked(id string) bool {
	if !validSessionFamily(id) {
		return true
	}
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	return s.familyStatus(ctx, id).Revoked
}
func (s *Service) signOutSession(v Session) (bool, error) {
	revocation := sessionRevocation{FamilyID: v.FamilyID, Expires: v.Expires, Pending: s.Config().Role != RoleSystem}
	if err := s.recordRevocation(revocation); err != nil {
		return false, err
	}
	if s.hub != nil {
		s.hub.InvalidateSession(v.Key)
	}
	ctx, cancel := context.WithTimeout(context.Background(), revocationDeliveryBudget)
	defer cancel()
	return s.deliverRevocation(ctx, revocation), nil
}
func (s *Service) sessionRevocationInternal(w http.ResponseWriter, r *http.Request) {
	if r.Method != "POST" {
		w.WriteHeader(405)
		return
	}
	if r.URL.Path == "/internal/sso/status" {
		var input struct {
			FamilyID string `json:"family_id"`
		}
		if err := readJSON(w, r, &input); err != nil || !validSessionFamily(input.FamilyID) {
			fail(w, 400, errors.New("invalid session family"))
			return
		}
		respond(w, 200, s.familyStatus(r.Context(), input.FamilyID))
		return
	}
	var input sessionRevocation
	if err := readJSON(w, r, &input); err != nil {
		fail(w, 400, err)
		return
	}
	input.Pending = s.Config().Role != RoleSystem
	if err := s.recordRevocation(input); err != nil {
		fail(w, 500, errors.New("could not record shared sign-out"))
		return
	}
	complete := s.deliverRevocation(r.Context(), input)
	respond(w, 200, sessionFamilyStatus{Revoked: true, Expires: input.Expires, Pending: !complete})
}

func (s *Service) sessionRevocations() ([]sessionRevocation, error) {
	rows, err := s.Store.readDB.Query(`SELECT document FROM framework_documents WHERE name LIKE 'auth:revoked:%' AND at>?`, time.Now().UTC().Format(authTimeFormat))
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []sessionRevocation{}
	for rows.Next() {
		var raw []byte
		if err = rows.Scan(&raw); err != nil {
			return nil, err
		}
		var v sessionRevocation
		if err = json.Unmarshal(raw, &v); err != nil {
			return nil, err
		}
		v.Pending = false
		out = append(out, v)
	}
	return out, rows.Err()
}
