package framework

import (
	"crypto/subtle"
	"errors"
	"net/http"
	"slices"
)

type desktopWorkspaceUpdate struct {
	UserID  string         `json:"user_id"`
	Proof   string         `json:"proof"`
	Desktop DesktopProfile `json:"desktop"`
}

func (s *Service) desktopWorkspaceHTTP(w http.ResponseWriter, r *http.Request, session Session) {
	if s.Config().Role != RoleApplication || session.UserID == "" {
		fail(w, http.StatusForbidden, errors.New("sign in with an application account to use desktop workspaces"))
		return
	}
	if r.Method != http.MethodGet && r.Method != http.MethodPut {
		w.WriteHeader(http.StatusMethodNotAllowed)
		return
	}
	user, hash, err := s.Store.User(session.UserID)
	if err != nil || !user.Enabled || user.Username != session.Username || user.Role == "mobile" {
		fail(w, http.StatusForbidden, errors.New("desktop workspaces require an enabled dispatch account"))
		return
	}
	groups, err := s.profileGroups(user)
	if err != nil {
		fail(w, http.StatusServiceUnavailable, errors.New("accepted system settings are not available"))
		return
	}
	if r.Method == http.MethodGet {
		respond(w, http.StatusOK, map[string]any{"desktop": scopedDesktopProfile(user.Profile.Desktop, groups)})
		return
	}
	var desktop DesktopProfile
	if err = readJSON(w, r, &desktop); err != nil {
		fail(w, http.StatusBadRequest, err)
		return
	}
	update := desktopWorkspaceUpdate{UserID: user.ID, Proof: userProof(user, hash), Desktop: desktop}
	if err = s.upstreamRequest(r.Context(), http.MethodPost, "/internal/desktop-workspace", update, nil); err != nil {
		fail(w, http.StatusServiceUnavailable, errors.New("System Administration could not save this workspace; keep the form open and try again when the connection returns"))
		return
	}
	if err = s.Store.SaveDesktopProfile(user.ID, desktop); err != nil {
		fail(w, http.StatusServiceUnavailable, errors.New("the workspace was saved centrally, but this console could not refresh its copy; reload before editing again"))
		return
	}
	respond(w, http.StatusOK, map[string]any{"saved": true, "desktop": desktop})
}

func (s *Service) desktopWorkspaceInternal(w http.ResponseWriter, r *http.Request, peer PeerCredential) {
	if r.Method != http.MethodPost {
		w.WriteHeader(http.StatusMethodNotAllowed)
		return
	}
	role := s.Config().Role
	if !profilePeerAllowed(role, peer) {
		fail(w, http.StatusForbidden, errors.New("this service cannot submit desktop workspaces here"))
		return
	}
	if role == RoleZone && peer.Role == RoleSite {
		var zone ZoneConfig
		if err := s.owned(&zone); err != nil || !slices.ContainsFunc(zone.Sites, func(site SiteIdentity) bool { return site.ID == peer.InstanceID }) {
			fail(w, http.StatusForbidden, errors.New("this Site is not enrolled in the Zone"))
			return
		}
	}
	var update desktopWorkspaceUpdate
	if err := readJSON(w, r, &update); err != nil {
		fail(w, http.StatusBadRequest, err)
		return
	}
	user, hash, err := s.Store.User(update.UserID)
	if err != nil || !user.Enabled || user.Role == "mobile" || subtle.ConstantTimeCompare([]byte(update.Proof), []byte(userProof(user, hash))) != 1 {
		fail(w, http.StatusForbidden, errors.New("the dispatch account or its authorization has changed"))
		return
	}
	if role != RoleSystem {
		if err = s.upstreamRequest(r.Context(), http.MethodPost, "/internal/desktop-workspace", update, nil); err != nil {
			fail(w, http.StatusServiceUnavailable, errors.New("the workspace authority is unavailable; no local-only update was saved"))
			return
		}
	}
	if err = s.Store.SaveDesktopProfile(user.ID, update.Desktop); err != nil {
		fail(w, http.StatusServiceUnavailable, errors.New("could not save the desktop workspace"))
		return
	}
	if role == RoleSystem {
		_ = s.Store.AddEvent("profile", "Desktop workspace saved.")
	}
	respond(w, http.StatusOK, map[string]bool{"saved": true})
}
