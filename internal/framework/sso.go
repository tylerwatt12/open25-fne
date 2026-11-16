package framework

import (
	"context"
	"crypto/sha256"
	"crypto/subtle"
	"database/sql"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"strings"
	"time"

	"open25.local/open25-fne/internal/historypriority"
)

// Final sessions and revocations use the private document store, indexed only
// by token digest. Unfinished SSO requests and codes belong to this process.
const authTimeFormat = "2006-01-02T15:04:05.000000000Z"

func authName(kind, token string) string {
	sum := sha256.Sum256([]byte(token))
	return "auth:" + kind + ":" + hex.EncodeToString(sum[:])
}
func (s *Store) authPut(kind, token string, value any, expires time.Time) (err error) {
	raw, err := json.Marshal(value)
	if err != nil {
		return err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	turn, err := historypriority.AcquireForeground()
	if err != nil {
		return err
	}
	defer func() { err = errors.Join(err, turn.Release()) }()
	// Expiration and the replacement are one durable change. Apart from avoiding
	// a separate sync for expiration, a failed insert cannot discard old records.
	tx, err := s.db.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback()
	if _, err = tx.Exec("DELETE FROM framework_documents WHERE name LIKE 'auth:%' AND at<=?", time.Now().UTC().Format(authTimeFormat)); err != nil {
		return err
	}
	var count int
	if err = tx.QueryRow("SELECT count(*) FROM framework_documents WHERE name LIKE ? AND name<>?", "auth:"+kind+":%", authName(kind, token)).Scan(&count); err != nil {
		return err
	}
	if count >= 4096 {
		return errors.New("too many active authentication records; try again later")
	}
	if _, err = tx.Exec("INSERT OR REPLACE INTO framework_documents(name,document,at) VALUES(?,?,?)", authName(kind, token), raw, expires.UTC().Format(authTimeFormat)); err != nil {
		return err
	}
	return tx.Commit()
}
func (s *Store) authRead(kind, token string, value any) error {
	var raw []byte
	err := s.readDB.QueryRow("SELECT document FROM framework_documents WHERE name=? AND at>?", authName(kind, token), time.Now().UTC().Format(authTimeFormat)).Scan(&raw)
	if err != nil {
		return err
	}
	return json.Unmarshal(raw, value)
}
func (s *Store) authDelete(kind, token string) (err error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	turn, err := historypriority.AcquireForeground()
	if err != nil {
		return err
	}
	defer func() { err = errors.Join(err, turn.Release()) }()
	_, err = s.db.Exec("DELETE FROM framework_documents WHERE name=?", authName(kind, token))
	return err
}
func (s *Service) createSession(w http.ResponseWriter, r *http.Request, v *Session) error {
	token, err := randomToken()
	if err != nil {
		return err
	}
	csrf, err := randomToken()
	if err != nil {
		return err
	}
	if v.FamilyID == "" {
		v.FamilyID, err = randomToken()
		if err != nil {
			return err
		}
	}
	if _, revoked := s.familyRevocation(v.FamilyID); revoked {
		return errors.New("shared session revoked")
	}
	v.CSRF = csrf
	if err = s.Store.authPut("session", token, v, v.Expires); err != nil {
		return err
	}
	v.Key = token
	http.SetCookie(w, &http.Cookie{Name: s.cookieName(), Value: token, Path: "/", HttpOnly: true, Secure: r.TLS != nil, SameSite: http.SameSiteLaxMode, MaxAge: int(time.Until(v.Expires).Seconds()), Expires: v.Expires})
	return nil
}

type ssoPending struct {
	Verifier  string
	Return    string
	Callback  string
	Authority string
	Expires   time.Time
}
type ssoGrant struct {
	FamilyID       string
	Callback       string
	Challenge      string
	Username       string
	AccountDigest  [32]byte
	SessionExpires time.Time
	Expires        time.Time
}
type ssoExchange struct {
	Code     string `json:"code"`
	Verifier string `json:"verifier"`
	Callback string `json:"callback"`
}
type ssoIdentity struct {
	FamilyID      string    `json:"family_id"`
	Username      string    `json:"username"`
	AccountDigest [32]byte  `json:"account_digest"`
	Expires       time.Time `json:"expires"`
}
type ssoAuthority struct {
	URL string `json:"url"`
}

const ssoLimit = 4096

// The Service owns unfinished sign-in state. Restart deliberately abandons it;
// only a successfully committed final session can survive a restart.
func (s *Service) expireSSOLocked(now time.Time) {
	for key, pending := range s.ssoPending {
		if !now.Before(pending.Expires) {
			delete(s.ssoPending, key)
		}
	}
	for key, grant := range s.ssoCodes {
		if !now.Before(grant.Expires) {
			delete(s.ssoCodes, key)
		}
	}
}

func (s *Service) rememberSSOPending(state string, pending ssoPending) error {
	s.ssoMu.Lock()
	defer s.ssoMu.Unlock()
	s.expireSSOLocked(time.Now())
	key := sha256.Sum256([]byte(state))
	if _, exists := s.ssoPending[key]; !exists && len(s.ssoPending) >= ssoLimit {
		return errors.New("too many pending sign-in requests; try again later")
	}
	if s.ssoPending == nil {
		s.ssoPending = make(map[[32]byte]ssoPending)
	}
	s.ssoPending[key] = pending
	return nil
}

func (s *Service) consumeSSOPending(state, issuer, callback string) (ssoPending, bool) {
	s.ssoMu.Lock()
	defer s.ssoMu.Unlock()
	s.expireSSOLocked(time.Now())
	key := sha256.Sum256([]byte(state))
	pending, exists := s.ssoPending[key]
	if !exists || pending.Authority != issuer || pending.Callback != callback {
		return ssoPending{}, false
	}
	delete(s.ssoPending, key)
	return pending, true
}

func (s *Service) rememberSSOCode(code string, grant ssoGrant) error {
	s.ssoMu.Lock()
	defer s.ssoMu.Unlock()
	s.expireSSOLocked(time.Now())
	key := sha256.Sum256([]byte(code))
	if _, exists := s.ssoCodes[key]; !exists && len(s.ssoCodes) >= ssoLimit {
		return errors.New("too many active sign-in codes; try again later")
	}
	if s.ssoCodes == nil {
		s.ssoCodes = make(map[[32]byte]ssoGrant)
	}
	s.ssoCodes[key] = grant
	return nil
}

func ssoChallenge(verifier string) string {
	sum := sha256.Sum256([]byte(verifier))
	return base64.RawURLEncoding.EncodeToString(sum[:])
}
func ssoPublicURL(address string) bool {
	u, err := url.Parse(address)
	return err == nil && u.Scheme == "https" && u.Host != "" && u.User == nil && u.RawQuery == "" && u.Fragment == "" && (u.Path == "" || u.Path == "/")
}
func ssoContinue(path string) string {
	u, err := url.Parse(path)
	if err == nil && !u.IsAbs() && u.Host == "" && u.Path == "/sso/authorize" {
		return u.RequestURI()
	}
	return ""
}
func ssoReturn(path string) string {
	u, err := url.Parse(path)
	if err == nil && !u.IsAbs() && u.Host == "" && strings.HasPrefix(u.Path, "/") && !strings.HasPrefix(u.Path, "//") && !strings.ContainsAny(path, "\\\r\n") && !strings.HasPrefix(u.Path, "/sso/") && u.Path != "/login" && u.Path != "/logout" {
		return u.RequestURI()
	}
	return "/"
}
func (s *Service) ssoCallbackAllowed(callback string) bool {
	_, zone, err := s.documents()
	if err != nil {
		return false
	}
	tree := s.adminTopology(zone)
	for _, address := range append([]string{tree.ZoneURL, tree.ApplicationURL}, topologySites(tree)...) {
		if ssoPublicURL(address) && callback == strings.TrimRight(address, "/")+"/sso/callback" {
			return true
		}
	}
	return false
}
func (s *Service) ssoInternal(w http.ResponseWriter, r *http.Request, peer PeerCredential) {
	role := s.Config().Role
	allowed := (role == RoleSystem && peer.Role == RoleZone) || (role == RoleZone && (peer.Role == RoleSite || peer.Role == RoleApplication)) || (role == RoleSite && peer.Role == RoleApplication)
	if !allowed {
		fail(w, 403, errors.New("service cannot exchange administrator sign-in"))
		return
	}
	if r.URL.Path == "/internal/sso/revoke" || r.URL.Path == "/internal/sso/status" {
		s.sessionRevocationInternal(w, r)
		return
	}
	switch r.URL.Path {
	case "/internal/sso/authority":
		if r.Method != "GET" {
			w.WriteHeader(405)
			return
		}
		var authority ssoAuthority
		if role == RoleSystem {
			authority.URL = s.Config().PublicURL
		} else if err := s.upstreamRequest(r.Context(), "GET", r.URL.Path, nil, &authority); err != nil {
			fail(w, 503, errors.New("System sign-in is unavailable"))
			return
		}
		if !ssoPublicURL(authority.URL) {
			fail(w, 503, errors.New("System sign-in requires its HTTPS web address"))
			return
		}
		respond(w, 200, authority)
	case "/internal/sso/exchange":
		if r.Method != "POST" {
			w.WriteHeader(405)
			return
		}
		var input ssoExchange
		if err := readJSON(w, r, &input); err != nil {
			fail(w, 400, err)
			return
		}
		var identity ssoIdentity
		var err error
		if role == RoleSystem {
			identity, err = s.redeemSSO(input)
		} else {
			err = s.upstreamRequest(r.Context(), "POST", r.URL.Path, input, &identity)
		}
		if err != nil {
			fail(w, 403, errors.New("shared sign-in could not be verified"))
			return
		}
		respond(w, 200, identity)
	default:
		http.NotFound(w, r)
	}
}
func (s *Service) redeemSSO(input ssoExchange) (ssoIdentity, error) {
	s.ssoMu.Lock()
	s.expireSSOLocked(time.Now())
	key := sha256.Sum256([]byte(input.Code))
	grant, exists := s.ssoCodes[key]
	if len(input.Code) != 64 || len(input.Verifier) != 64 || !exists || input.Callback != grant.Callback || subtle.ConstantTimeCompare([]byte(ssoChallenge(input.Verifier)), []byte(grant.Challenge)) != 1 {
		s.ssoMu.Unlock()
		return ssoIdentity{}, errors.New("invalid code")
	}
	if !validSessionFamily(grant.FamilyID) {
		s.ssoMu.Unlock()
		return ssoIdentity{}, errors.New("invalid session family")
	}
	delete(s.ssoCodes, key)
	s.ssoMu.Unlock()
	// Consuming a proven code is atomic and needs no configuration writer. Still
	// read the current durable revocation/account before granting any identity.
	var revoked sessionRevocation
	if err := s.Store.authRead("revoked", grant.FamilyID, &revoked); err == nil {
		return ssoIdentity{}, errors.New("shared session revoked")
	} else if !errors.Is(err, sql.ErrNoRows) {
		return ssoIdentity{}, errors.New("shared session revocation is unavailable")
	}
	username, hash, err := s.Store.Account()
	if err != nil || username != grant.Username || sha256.Sum256(hash) != grant.AccountDigest || !time.Now().Before(grant.SessionExpires) {
		return ssoIdentity{}, errors.New("account changed or session expired")
	}
	return ssoIdentity{FamilyID: grant.FamilyID, Username: grant.Username, AccountDigest: grant.AccountDigest, Expires: grant.SessionExpires}, nil
}

// Chromium applies a submitted form's policy to the entire redirect chain.
// Permit only the registered destination origin for this System login, while
// keeping every other form and origin under the normal self-only policy.
func (s *Service) ssoLoginPolicy(w http.ResponseWriter, r *http.Request) {
	if s.Config().Role != RoleSystem {
		return
	}
	continuation, err := url.Parse(ssoContinue(r.FormValue("continue")))
	if err != nil || continuation.Path != "/sso/authorize" {
		return
	}
	callback := continuation.Query().Get("redirect_uri")
	if !s.ssoCallbackAllowed(callback) {
		return
	}
	destination, err := url.Parse(callback)
	if err != nil {
		return
	}
	policy := strings.Replace(w.Header().Get("Content-Security-Policy"), "form-action 'self'", "form-action 'self' "+destination.Scheme+"://"+destination.Host, 1)
	w.Header().Set("Content-Security-Policy", policy)
}

func (s *Service) ssoUnavailable(w http.ResponseWriter, r *http.Request, message string) {
	// A form under no-referrer produces Origin: null in Chromium. Preserve
	// only the origin for local POSTs; never forward a callback code or path.
	w.Header().Set("Referrer-Policy", "strict-origin")
	s.webHeader(w, r, "Sign in", Session{}, false)
	fmt.Fprintf(w, `<p role=alert>%s</p>`, esc(message))
	s.webLoginForm(w, r)
	webFooter(w)
}
func (s *Service) ssoWeb(w http.ResponseWriter, r *http.Request) bool {
	if !strings.HasPrefix(r.URL.Path, "/sso/") {
		return false
	}
	w.Header().Set("Cache-Control", "no-store")
	w.Header().Set("Referrer-Policy", "no-referrer")
	if r.Method != "GET" {
		w.WriteHeader(405)
		return true
	}
	switch r.URL.Path {
	case "/sso/start":
		if s.Config().Role == RoleSystem {
			http.Redirect(w, r, "/login", 303)
			break
		}
		if !ssoPublicURL(s.Config().PublicURL) {
			s.ssoUnavailable(w, r, "Shared sign-in requires this service’s HTTPS web address. Use local sign-in below.")
			break
		}
		ctx, cancel := context.WithTimeout(r.Context(), 3*time.Second)
		defer cancel()
		var authority ssoAuthority
		if err := s.upstreamRequest(ctx, "GET", "/internal/sso/authority", nil, &authority); err != nil || !ssoPublicURL(authority.URL) {
			s.ssoUnavailable(w, r, "System sign-in is unavailable. Sign in locally to this service.")
			break
		}
		state, err := randomToken()
		if err != nil {
			http.Error(w, "Could not start sign-in.", 500)
			break
		}
		verifier, err := randomToken()
		if err != nil {
			http.Error(w, "Could not start sign-in.", 500)
			break
		}
		pending := ssoPending{Verifier: verifier, Return: ssoReturn(r.URL.Query().Get("return")), Callback: strings.TrimRight(s.Config().PublicURL, "/") + "/sso/callback", Authority: strings.TrimRight(authority.URL, "/"), Expires: time.Now().Add(5 * time.Minute)}
		if err = s.rememberSSOPending(state, pending); err != nil {
			http.Error(w, "Could not save sign-in request.", 500)
			break
		}
		http.SetCookie(w, &http.Cookie{Name: s.cookieName() + "_sso", Value: state, Path: "/sso/callback", MaxAge: 300, HttpOnly: true, Secure: true, SameSite: http.SameSiteLaxMode})
		q := url.Values{"redirect_uri": {pending.Callback}, "state": {state}, "code_challenge": {ssoChallenge(verifier)}, "code_challenge_method": {"S256"}}
		http.Redirect(w, r, pending.Authority+"/sso/authorize?"+q.Encode(), 303)
	case "/sso/authorize":
		if s.Config().Role != RoleSystem {
			http.NotFound(w, r)
			break
		}
		q := r.URL.Query()
		callback := q.Get("redirect_uri")
		challenge := q.Get("code_challenge")
		decoded, err := base64.RawURLEncoding.DecodeString(challenge)
		if !s.ssoCallbackAllowed(callback) || len(q.Get("state")) != 64 || q.Get("code_challenge_method") != "S256" || err != nil || len(decoded) != 32 {
			http.Error(w, "Unrecognized shared sign-in request.", 400)
			break
		}
		session, ok := s.session(r)
		if !ok {
			http.Redirect(w, r, "/login?continue="+url.QueryEscape(r.URL.RequestURI()), 303)
			break
		}
		if session.UserID != "" {
			http.Error(w, "Administrator sign-in required.", 403)
			break
		}
		code, err := randomToken()
		if err != nil {
			http.Error(w, "Could not authorize sign-in.", 500)
			break
		}
		grant := ssoGrant{FamilyID: session.FamilyID, Callback: callback, Challenge: challenge, Username: session.Username, AccountDigest: session.AccountDigest, SessionExpires: session.Expires, Expires: time.Now().Add(time.Minute)}
		if err = s.rememberSSOCode(code, grant); err != nil {
			http.Error(w, "Could not save authorization.", 500)
			break
		}
		http.Redirect(w, r, callback+"?"+url.Values{"code": {code}, "state": {q.Get("state")}, "iss": {strings.TrimRight(s.Config().PublicURL, "/")}}.Encode(), 303)
	case "/sso/callback":
		state := r.URL.Query().Get("state")
		cookie, err := r.Cookie(s.cookieName() + "_sso")
		if err != nil || len(state) != 64 || subtle.ConstantTimeCompare([]byte(state), []byte(cookie.Value)) != 1 {
			http.Error(w, "Sign-in request expired or does not belong to this browser. Start sign-in again.", 403)
			break
		}
		pending, exists := s.consumeSSOPending(state, r.URL.Query().Get("iss"), strings.TrimRight(s.Config().PublicURL, "/")+"/sso/callback")
		if !exists {
			http.Error(w, "Sign-in request expired or does not belong to this browser. Start sign-in again.", 403)
			break
		}
		http.SetCookie(w, &http.Cookie{Name: s.cookieName() + "_sso", Path: "/sso/callback", MaxAge: -1, HttpOnly: true, Secure: true, SameSite: http.SameSiteLaxMode})
		ctx, cancel := context.WithTimeout(r.Context(), 3*time.Second)
		defer cancel()
		var identity ssoIdentity
		if err = s.upstreamRequest(ctx, "POST", "/internal/sso/exchange", ssoExchange{Code: r.URL.Query().Get("code"), Verifier: pending.Verifier, Callback: pending.Callback}, &identity); err != nil {
			s.ssoUnavailable(w, r, "Shared sign-in is unavailable or expired. Try again or sign in locally.")
			break
		}
		username, hash, err := s.Store.Account()
		if err != nil || username != identity.Username || sha256.Sum256(hash) != identity.AccountDigest || !time.Now().Before(identity.Expires) {
			s.ssoUnavailable(w, r, "Administrator account has not synchronized. Sign in locally or try again after reconnection.")
			break
		}
		v := Session{FamilyID: identity.FamilyID, Username: username, AccountDigest: identity.AccountDigest, Expires: identity.Expires}
		if err = s.createSession(w, r, &v); err != nil {
			http.Error(w, "Could not persist sign-in.", 500)
			break
		}
		http.Redirect(w, r, pending.Return, 303)
	default:
		http.NotFound(w, r)
	}
	return true
}
