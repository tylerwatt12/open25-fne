package framework

import (
	"errors"
	"net/http"
	"strings"
)

func (s *Service) automationsHTTP(w http.ResponseWriter, r *http.Request, session Session) {
	if s.Config().Role != RoleSystem || session.UserID != "" {
		fail(w, http.StatusForbidden, errors.New("automation accounts are managed in System Administration"))
		return
	}
	path := strings.TrimPrefix(r.URL.Path, "/api/automations")
	if path == "" && r.Method == http.MethodGet {
		identities, err := s.Store.Automations()
		if err != nil {
			fail(w, 500, errors.New("automation accounts are unavailable"))
			return
		}
		respond(w, 200, map[string]any{"automations": identities})
		return
	}
	id := strings.TrimPrefix(path, "/")
	if r.Method == http.MethodPost && strings.HasSuffix(id, "/token") {
		id = strings.TrimSuffix(id, "/token")
		token, err := s.Store.RotateAutomationToken(id)
		if err != nil {
			fail(w, 400, errors.New("could not rotate this automation credential"))
			return
		}
		s.refreshDirectory()
		_ = s.Store.AddEvent("automation", "Automation credential replaced.")
		respond(w, 200, map[string]any{"id": id, "token": token})
		return
	}
	if r.Method == http.MethodDelete && path != "" {
		if err := s.Store.DeleteAutomation(id); err != nil {
			fail(w, 400, errors.New("could not remove this automation account"))
			return
		}
		s.refreshDirectory()
		_ = s.Store.AddEvent("automation", "Automation account removed.")
		respond(w, 200, map[string]bool{"saved": true})
		return
	}
	create := r.Method == http.MethodPost && path == ""
	if !create && !(r.Method == http.MethodPut && path != "") {
		w.WriteHeader(http.StatusMethodNotAllowed)
		return
	}
	var input Automation
	if err := readJSON(w, r, &input); err != nil {
		fail(w, 400, err)
		return
	}
	if create {
		id = input.ID
	}
	token, err := s.Store.SaveAutomation(id, input, create)
	if err != nil {
		fail(w, 400, err)
		return
	}
	s.refreshDirectory()
	_ = s.Store.AddEvent("automation", "Automation account saved.")
	result := map[string]any{"saved": true}
	if token != "" {
		result["token"] = token
	}
	respond(w, 200, result)
}
