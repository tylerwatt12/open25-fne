package framework

import (
	"crypto/rand"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"strings"
	"time"

	"golang.org/x/crypto/bcrypt"
	"open25.local/open25-fne/internal/activity"
	"open25.local/open25-fne/internal/radio"
)

func randomToken() (string, error) {
	b := make([]byte, 32)
	if _, err := rand.Read(b); err != nil {
		return "", err
	}
	return hex.EncodeToString(b), nil
}
func respond(w http.ResponseWriter, status int, value any) {
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("Cache-Control", "no-store")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(value)
}
func fail(w http.ResponseWriter, status int, err error) {
	respond(w, status, map[string]string{"error": err.Error()})
}

func readJSON(w http.ResponseWriter, r *http.Request, value any) error {
	if !strings.HasPrefix(strings.ToLower(r.Header.Get("Content-Type")), "application/json") {
		return errors.New("send JSON configuration")
	}
	limit := int64(2 << 20)
	if r.URL.Path == "/api/config" || r.URL.Path == "/internal/report" {
		limit = maxConfigurationBytes
	}
	raw, err := io.ReadAll(http.MaxBytesReader(w, r.Body, limit))
	if err != nil {
		return errors.New("request exceeds the allowed size")
	}
	return DecodeStrict(raw, value)
}

func sameOrigin(r *http.Request) bool {
	if r.Header.Get("Sec-Fetch-Site") == "cross-site" {
		return false
	}
	o := r.Header.Get("Origin")
	if o == "" {
		return true
	}
	u, err := url.Parse(o)
	scheme := "http"
	if r.TLS != nil {
		scheme = "https"
	}
	return err == nil && u.Scheme == scheme && u.Host == r.Host && u.User == nil
}

func (s *Service) session(r *http.Request) (Session, bool) {
	cookie, err := r.Cookie(s.cookieName())
	if err != nil {
		return Session{}, false
	}
	s.mu.Lock()
	v, ok := s.sessions[cookie.Value]
	s.mu.Unlock()
	valid := ok && !time.Now().After(v.Expires)
	if valid && v.UserID != "" {
		u, hash, e := s.Store.User(v.UserID)
		valid = e == nil && u.Enabled && v.Username == u.Username && v.AccountDigest == userDigest(u, hash) && s.Config().Role == RoleApplication
		if valid {
			_, _, e = resolveUserPosition(u, s.view().System, v.PositionID)
			valid = e == nil
		}
	} else if valid {
		username, hash, e := s.Store.Account()
		valid = e == nil && v.Username == username && v.AccountDigest == sha256.Sum256(hash)
	}
	if !valid {
		s.mu.Lock()
		delete(s.sessions, cookie.Value)
		s.mu.Unlock()
		return Session{}, false
	}
	v.Key = cookie.Value
	return v, true
}

func (s *Service) sessionResponse(w http.ResponseWriter, r *http.Request) {
	v, ok := s.session(r)
	respond(w, 200, map[string]any{"authenticated": ok, "username": v.Username, "csrf": v.CSRF, "role": s.Config().Role, "is_administrator": ok && v.UserID == ""})
}

func (s *Service) login(w http.ResponseWriter, r *http.Request) {
	var input struct {
		Username   string `json:"username"`
		Password   string `json:"password"`
		PositionID string `json:"position_id"`
	}
	if err := readJSON(w, r, &input); err != nil {
		fail(w, 400, err)
		return
	}
	ip, _, _ := net.SplitHostPort(r.RemoteAddr)
	now := time.Now()
	s.mu.Lock()
	a := s.attempts[ip]
	if now.Sub(a.Since) > 5*time.Minute {
		a = loginAttempt{Since: now}
	}
	a.Count++
	s.attempts[ip] = a
	for k, v := range s.attempts {
		if now.Sub(v.Since) > 5*time.Minute {
			delete(s.attempts, k)
		}
	}
	s.mu.Unlock()
	if a.Count > 10 {
		fail(w, 429, errors.New("too many login attempts; try again in five minutes"))
		return
	}
	username, hash, err := s.Store.Account()
	userID := ""
	positionID := ""
	digest := sha256.Sum256(hash)
	valid := err == nil && username == input.Username && bcrypt.CompareHashAndPassword(hash, []byte(input.Password)) == nil
	if !valid && s.Config().Role == RoleApplication {
		u, h, e := s.Store.LoginUser(input.Username)
		if e == nil && u.Enabled && bcrypt.CompareHashAndPassword(h, []byte(input.Password)) == nil {
			_, position, resolveErr := resolveUserPosition(u, s.view().System, input.PositionID)
			if resolveErr != nil {
				respond(w, http.StatusConflict, map[string]any{"error": resolveErr.Error(), "positions": availableUserPositions(u, s.view().System)})
				return
			}
			if position != nil {
				positionID = position.ID
			}
			valid = true
			userID = u.ID
			username = u.Username
			digest = userDigest(u, h)
		}
	}
	if !valid {
		fail(w, 401, errors.New("invalid username or password"))
		return
	}
	token, err := randomToken()
	if err != nil {
		fail(w, 500, errors.New("could not create session"))
		return
	}
	csrf, err := randomToken()
	if err != nil {
		fail(w, 500, errors.New("could not create session"))
		return
	}
	v := Session{Username: username, CSRF: csrf, Expires: now.Add(8 * time.Hour), AccountDigest: digest, UserID: userID, PositionID: positionID, Key: token}
	s.mu.Lock()
	delete(s.attempts, ip)
	for k, old := range s.sessions {
		if now.After(old.Expires) {
			delete(s.sessions, k)
		}
	}
	s.sessions[token] = v
	s.mu.Unlock()
	http.SetCookie(w, &http.Cookie{Name: s.cookieName(), Value: token, Path: "/", HttpOnly: true, Secure: r.TLS != nil, SameSite: http.SameSiteStrictMode, MaxAge: 28800})
	s.emit(activity.Event{Type: "login", Message: "User signed in.", Audience: "security", Severity: "info", Outcome: "accepted", SourceKind: "application", Details: map[string]any{"username": username, "user_id": userID}})
	s.sessionResponse(w, withCookie(r, s.cookieName(), token))
}

func withCookie(r *http.Request, name, value string) *http.Request {
	copy := r.Clone(r.Context())
	copy.Header = r.Header.Clone()
	copy.Header.Del("Cookie")
	copy.AddCookie(&http.Cookie{Name: name, Value: value})
	return copy
}

func (s *Service) Handler() http.Handler { return s.handler(false) }

func (s *Service) InternalHandler() http.Handler { return s.handler(true) }

func (s *Service) handler(internalOnly bool) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("X-Content-Type-Options", "nosniff")
		w.Header().Set("Referrer-Policy", "same-origin")
		w.Header().Set("Content-Security-Policy", "default-src 'self'; script-src 'self'; style-src 'self'; img-src 'self' data:; connect-src 'self'; media-src 'self' blob:; worker-src 'self' blob:; frame-ancestors 'none'; base-uri 'none'; form-action 'self'")
		microphone := "()"
		geolocation := "()"
		if s.Config().Role == RoleApplication {
			microphone = "(self)"
			geolocation = "(self)"
		}
		w.Header().Set("Permissions-Policy", "microphone="+microphone+", camera=(), geolocation="+geolocation)
		if !s.allowedClient(r) {
			fail(w, 403, errors.New("this network is not allowed"))
			return
		}
		if r.URL.Path == "/healthz" {
			if r.Method != "GET" {
				w.WriteHeader(405)
				return
			}
			c := s.Config()
			rf := false
			if s.radio != nil {
				rf = s.radio.Status().State == "running"
			}
			respond(w, 200, map[string]any{"role": c.Role, "instance_id": c.InstanceID, "configuration_service": true, "rf_enabled": rf})
			return
		}
		isInternal := r.URL.Path == "/internal" || strings.HasPrefix(r.URL.Path, "/internal/")
		if internalOnly {
			if isInternal {
				s.internal(w, r)
			} else {
				http.NotFound(w, r)
			}
			return
		}
		if isInternal {
			http.NotFound(w, r)
			return
		}
		if !strings.HasPrefix(r.URL.Path, "/api/") {
			s.earlyWeb(w, r)
			return
		}

		if r.Method != "GET" && !sameOrigin(r) {
			fail(w, 403, errors.New("request must originate from this application"))
			return
		}
		if strings.HasPrefix(r.URL.Path, "/api/automation/") {
			s.automationHTTP(w, r)
			return
		}
		if r.URL.Path == "/api/session" && r.Method == "GET" {
			s.sessionResponse(w, r)
			return
		}
		if r.URL.Path == "/api/login" && r.Method == "POST" {
			s.login(w, r)
			return
		}
		v, ok := s.session(r)
		if !ok {
			fail(w, 401, errors.New("sign in to continue"))
			return
		}
		if r.Method != "GET" && subtle.ConstantTimeCompare([]byte(r.Header.Get("X-CSRF-Token")), []byte(v.CSRF)) != 1 {
			fail(w, 403, errors.New("reload this page before saving"))
			return
		}
		if s.messagesHTTP(w, r, v) {
			return
		}
		if s.operationsHTTP(w, r, v) {
			return
		}
		if v.UserID != "" && r.URL.Path != "/api/state" && r.URL.Path != "/api/logout" {
			fail(w, 403, errors.New("administrator sign-in required"))
			return
		}
		switch r.URL.Path {
		case "/api/logout":
			if r.Method != "POST" {
				w.WriteHeader(405)
				return
			}
			if c, err := r.Cookie(s.cookieName()); err == nil {
				if s.hub != nil {
					s.hub.InvalidateSession(c.Value)
				}
				s.mu.Lock()
				delete(s.sessions, c.Value)
				s.mu.Unlock()
			}
			http.SetCookie(w, &http.Cookie{Name: s.cookieName(), Value: "", Path: "/", MaxAge: -1, HttpOnly: true, Secure: r.TLS != nil, SameSite: http.SameSiteStrictMode})
			respond(w, 200, map[string]bool{"ok": true})
		case "/api/state":
			if r.Method != "GET" {
				w.WriteHeader(405)
				return
			}
			state, err := s.State(v)
			if err != nil {
				fail(w, 500, errors.New("could not read saved settings"))
				return
			}
			respond(w, 200, state)
		case "/api/config":
			if r.Method != "PUT" {
				w.WriteHeader(405)
				return
			}
			var raw json.RawMessage
			if err := readJSON(w, r, &raw); err != nil {
				fail(w, 400, err)
				return
			}
			if err := s.saveOwned(raw); err != nil {
				fail(w, 400, err)
				return
			}
			s.refreshDirectory()
			respond(w, 200, map[string]bool{"saved": true})
		case "/api/settings":
			if r.Method != "PUT" {
				w.WriteHeader(405)
				return
			}
			var input Settings
			if err := readJSON(w, r, &input); err != nil {
				fail(w, 400, err)
				return
			}
			if err := s.SaveSettings(input); err != nil {
				fail(w, 400, err)
				return
			}
			respond(w, 200, map[string]bool{"saved": true})
		case "/api/account":
			s.changePassword(w, r)
		default:
			http.NotFound(w, r)
		}
	})
}

func (s *Service) allowedClient(r *http.Request) bool {
	c := s.Config()
	if c.AllowedCIDR == "" {
		return true
	}
	host, _, err := net.SplitHostPort(r.RemoteAddr)
	if err != nil {
		return false
	}
	ip := net.ParseIP(host)
	if ip == nil {
		return false
	}
	if ip.IsLoopback() {
		return true
	}
	_, network, err := net.ParseCIDR(c.AllowedCIDR)
	return err == nil && network.Contains(ip)
}

func (s *Service) saveOwned(raw json.RawMessage) error {
	// The owner explicitly requested that System radio names be valid OTA
	// aliases. This format rule is not a configuration compatibility check.
	if s.Config().Role == RoleSystem {
		var names struct {
			Radios []struct {
				Alias string `json:"alias"`
			} `json:"radios"`
		}
		if err := json.Unmarshal(raw, &names); err != nil {
			return err
		}
		for i, entry := range names.Radios {
			if !radio.IsValidUserAlias(entry.Alias) {
				return fmt.Errorf("radio directory entry %d: Name must contain 1–14 printable ASCII characters", i+1)
			}
		}
	}
	return s.Store.SaveConfig(raw)
}

func (s *Service) changePassword(w http.ResponseWriter, r *http.Request) {
	if r.Method != "PUT" {
		w.WriteHeader(405)
		return
	}
	if s.Config().Role != RoleSystem {
		fail(w, 403, errors.New("administrator credentials are managed in System Administration"))
		return
	}
	var input struct {
		Current string `json:"current_password"`
		New     string `json:"new_password"`
	}
	if err := readJSON(w, r, &input); err != nil {
		fail(w, 400, err)
		return
	}
	username, hash, err := s.Store.Account()
	if err != nil || bcrypt.CompareHashAndPassword(hash, []byte(input.Current)) != nil {
		fail(w, 403, errors.New("current password is incorrect"))
		return
	}
	hash, err = bcrypt.GenerateFromPassword([]byte(input.New), bcrypt.DefaultCost)
	if err != nil {
		fail(w, 500, errors.New("could not save password"))
		return
	}
	if err = s.Store.SetAccount(username, hash); err != nil {
		fail(w, 500, errors.New("could not save password"))
		return
	}
	_ = s.Store.AddEvent("account", "Administrator password changed.")
	respond(w, 200, map[string]bool{"saved": true})
}
