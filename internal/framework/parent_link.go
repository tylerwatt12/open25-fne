package framework

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"strings"
	"time"

	"open25.local/open25-fne/internal/activity"
)

type parentIsolation struct {
	Isolated      bool      `json:"isolated"`
	Since         time.Time `json:"since,omitempty"`
	Administrator string    `json:"administrator,omitempty"`
}

func (s *Service) loadParentIsolation() error {
	if s.Config().Role != RoleSite {
		return nil
	}
	var raw []byte
	err := s.Store.db.QueryRow("SELECT document FROM framework_documents WHERE name='site-parent-link'").Scan(&raw)
	if errors.Is(err, sql.ErrNoRows) {
		return nil
	}
	if err != nil {
		return err
	}
	var v parentIsolation
	if err = json.Unmarshal(raw, &v); err != nil {
		return err
	}
	s.parentIsolated.Store(v.Isolated)
	return nil
}
func (s *Service) parentRequestContext(ctx context.Context, address string) (context.Context, context.CancelFunc, error) {
	if s.Config().Role != RoleSite || strings.TrimRight(address, "/") != strings.TrimRight(s.configuredUpstreamURL(), "/") {
		return ctx, func() {}, nil
	}
	s.parentLinkMu.Lock()
	defer s.parentLinkMu.Unlock()
	if s.parentIsolated.Load() {
		return ctx, func() {}, errors.New("parent Zone disconnected for testing")
	}
	if s.parentContext == nil {
		s.parentContext, s.parentCancel = context.WithCancel(context.Background())
	}
	child, cancel := context.WithCancel(ctx)
	stop := context.AfterFunc(s.parentContext, cancel)
	return child, func() { stop(); cancel() }, nil
}
func (s *Service) setParentIsolation(ctx context.Context, isolated bool, administrator string) error {
	s.parentActionMu.Lock()
	defer s.parentActionMu.Unlock()
	if s.Config().Role != RoleSite {
		return errors.New("only a Site owns its parent connection")
	}
	if isolated == s.parentIsolated.Load() {
		return nil
	}
	v := parentIsolation{Isolated: isolated, Administrator: administrator}
	if isolated {
		v.Since = time.Now().UTC()
	}
	raw, err := json.Marshal(v)
	if err != nil {
		return err
	}
	if _, err = s.Store.db.Exec("INSERT OR REPLACE INTO framework_documents(name,document,at) VALUES('site-parent-link',?,?)", raw, nowStamp()); err != nil {
		return err
	}
	s.parentLinkMu.Lock()
	s.parentIsolated.Store(isolated)
	if s.parentCancel != nil {
		s.parentCancel()
	}
	s.parentContext = nil
	s.parentCancel = nil
	s.parentLinkMu.Unlock()
	if isolated {
		if s.radio != nil {
			s.radio.SetZoneConnected(false)
		}
		if s.mediaClient != nil {
			s.mediaClient.selectEndpoint("", voiceEndpoint{})
		}
		// A canceled discovery/sync cannot commit a late parent observation after
		// this action has returned. Local RF processes are deliberately preserved.
		s.syncMu.Lock()
		s.syncMu.Unlock()
	} else {
		// Normal parent traffic resumes. Deliver queued sign-out through that same
		// path; request-time authentication checks revocation before accepting access.
		_ = s.flushSessionRevocations(ctx)
	}
	if s.operationWake != nil {
		select {
		case s.operationWake <- struct{}{}:
		default:
		}
	}
	message := "Parent Zone reconnection requested."
	if isolated {
		message = "Parent Zone disconnected for test isolation; local RF remains under Site control."
	}
	s.emit(activity.Event{Type: "parent_connection", Audience: "operator", Severity: "notice", Outcome: "accepted", Message: message, Details: map[string]any{"isolated": isolated, "administrator": administrator}})
	return nil
}
func (s *Service) parentLinkHTTP(w http.ResponseWriter, r *http.Request, v Session) {
	if s.Config().Role != RoleSite || v.UserID != "" {
		fail(w, 403, errors.New("Site administrator required"))
		return
	}
	if r.Method != "POST" {
		w.WriteHeader(405)
		return
	}
	isolated := r.URL.Path == "/api/site/disconnect-parent"
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	if err := s.setParentIsolation(ctx, isolated, v.Username); err != nil {
		fail(w, 500, err)
		return
	}
	respond(w, 200, map[string]any{"isolated": s.parentIsolated.Load(), "radio": s.radioStatus()})
}
func (s *Service) webParentIsolationNotice(w http.ResponseWriter) {
	if s.Config().Role == RoleSite && s.parentIsolated.Load() {
		fmt.Fprint(w, `<div class=ts-dirty role=status data-parent-isolation><strong>Test isolation — disconnected from parent Zone.</strong> Parent management and media links are blocked. <a href=/parent-zone>Parent Zone connection</a></div>`)
	}
}
func (s *Service) webParentLink(w http.ResponseWriter, r *http.Request, v Session) {
	if r.Method == "POST" {
		path := ""
		switch r.FormValue("action") {
		case "disconnect":
			path = "/api/site/disconnect-parent"
		case "reconnect":
			path = "/api/site/reconnect-parent"
		default:
			http.Error(w, "Choose Disconnect or Reconnect.", 400)
			return
		}
		result := s.webAPI(r, v, path, "POST", map[string]any{})
		if result.code < 300 {
			http.Redirect(w, r, "/parent-zone", 303)
			return
		}
		s.webHeader(w, r, "Parent Zone connection", v, false)
		webJSONResult(w, result)
		webFooter(w)
		return
	}
	s.webHeader(w, r, "Parent Zone connection", v, false)
	defer webFooter(w)
	mode := s.radioStatus().TrunkingMode
	fmt.Fprintf(w, `<p>Observed trunking mode: <strong>%s</strong></p>`, esc(strings.ReplaceAll(mode, "_", " ")))
	if s.parentIsolated.Load() {
		fmt.Fprint(w, `<p>Test isolation is enabled. It remains enabled through service restarts until Reconnect is requested.</p>`)
	} else {
		fmt.Fprint(w, `<p>Test isolation is off. The Site automatically supervises and retries its parent connection.</p>`)
	}
	fmt.Fprint(w, `<p>Disconnect blocks this Site’s parent management and media links for a Site Trunking test. Local RF and local administration remain available. Reconnect resumes the parent links without restarting RF.</p><form method=post>`)
	webHidden(w, v.CSRF)
	fmt.Fprint(w, `<div class=ts-actions><button name=action value=disconnect>Disconnect from parent Zone</button><button name=action value=reconnect>Reconnect</button></div></form>`)
}
