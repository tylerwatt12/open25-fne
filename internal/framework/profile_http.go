package framework

import (
	"crypto/subtle"
	"errors"
	"net/http"
	"slices"
)

type profileUpdate struct {
	UserID  string      `json:"user_id"`
	Proof   string      `json:"proof"`
	Profile UserProfile `json:"profile"`
}

func emptyUserProfile() UserProfile {
	return UserProfile{Zones: []ProfileZone{}, Receive: []ReceiveSetting{}, TransmitTalkgroups: []int{}}
}

func (s *Service) profileGroups(user User) ([]int, error) {
	system, _, err := s.documents()
	if err != nil {
		return nil, err
	}
	groups := []int{}
	for _, group := range system.Talkgroups {
		if group.Enabled && slices.Contains(user.Talkgroups, group.TGID) {
			groups = append(groups, group.TGID)
		}
	}
	return groups, nil
}

// profileHTTP is called only after the application's session and CSRF checks.
// A successful write means System Administration has persisted the profile.
func (s *Service) profileHTTP(w http.ResponseWriter, r *http.Request, session Session) {
	if s.Config().Role != RoleApplication {
		fail(w, http.StatusForbidden, errors.New("application profiles belong to Application Server accounts"))
		return
	}
	if session.UserID == "" {
		if r.Method != http.MethodGet {
			fail(w, http.StatusForbidden, errors.New("sign in with an application account to save an operator profile"))
			return
		}
		profile := emptyUserProfile()
		respond(w, http.StatusOK, map[string]any{"user": User{Username: session.Username, Role: "administrator", Enabled: true, Talkgroups: []int{}, Permissions: []string{}, Profile: profile}, "profile": profile})
		return
	}
	user, hash, err := s.Store.User(session.UserID)
	if err != nil || !user.Enabled || session.Username != user.Username {
		fail(w, http.StatusUnauthorized, errors.New("this application account is no longer available"))
		return
	}
	if r.Method == http.MethodGet {
		system, _, err := s.documents()
		if err != nil {
			fail(w, http.StatusServiceUnavailable, errors.New("accepted system settings are not available"))
			return
		}
		resolved, position, err := resolveUserPosition(user, system, session.PositionID)
		if err != nil {
			fail(w, http.StatusForbidden, err)
			return
		}
		respond(w, http.StatusOK, map[string]any{"user": resolved, "profile": user.Profile, "position": position, "positions": availableUserPositions(user, system)})
		return
	}
	if r.Method != http.MethodPut {
		w.WriteHeader(http.StatusMethodNotAllowed)
		return
	}
	var profile UserProfile
	if err := readJSON(w, r, &profile); err != nil {
		fail(w, http.StatusBadRequest, err)
		return
	}
	// Desktop has its own central partial-write endpoint. Radio programming
	// cannot overwrite it with an omitted or stale exported profile section.
	profile.Desktop = nil
	update := profileUpdate{UserID: user.ID, Proof: userProof(user, hash), Profile: profile}
	if err := s.upstreamRequest(r.Context(), http.MethodPost, "/internal/profile", update, nil); err != nil {
		fail(w, http.StatusServiceUnavailable, errors.New("System Administration could not save this profile; keep the form open and try again when the connection returns"))
		return
	}
	if err := s.Store.SaveProfile(user.ID, profile); err != nil {
		fail(w, http.StatusServiceUnavailable, errors.New("the profile was saved centrally, but this console could not refresh its local copy; reload before editing again"))
		return
	}
	respond(w, http.StatusOK, map[string]bool{"saved": true})
}

func profilePeerAllowed(role Role, peer PeerCredential) bool {
	switch role {
	case RoleSystem:
		return peer.Role == RoleZone
	case RoleZone:
		return peer.Role == RoleApplication || peer.Role == RoleSite
	case RoleSite:
		return peer.Role == RoleApplication
	default:
		return false
	}
}

// profileInternal accepts only enrolled downstream services. The proof binds
// the update to the current user identity/password state without publishing a
// password verifier in any browser response or event.
func (s *Service) profileInternal(w http.ResponseWriter, r *http.Request, peer PeerCredential) {
	if r.Method != http.MethodPost {
		w.WriteHeader(http.StatusMethodNotAllowed)
		return
	}
	role := s.Config().Role
	if !profilePeerAllowed(role, peer) {
		fail(w, http.StatusForbidden, errors.New("this service cannot submit a profile update here"))
		return
	}
	if role == RoleZone && peer.Role == RoleSite {
		var zone ZoneConfig
		if err := s.owned(&zone); err != nil || !slices.ContainsFunc(zone.Sites, func(site SiteIdentity) bool { return site.ID == peer.InstanceID }) {
			fail(w, http.StatusForbidden, errors.New("this Site is not enrolled in the Zone"))
			return
		}
	}
	var update profileUpdate
	if err := readJSON(w, r, &update); err != nil {
		fail(w, http.StatusBadRequest, err)
		return
	}
	user, hash, err := s.Store.User(update.UserID)
	if err != nil || !user.Enabled || subtle.ConstantTimeCompare([]byte(update.Proof), []byte(userProof(user, hash))) != 1 {
		fail(w, http.StatusForbidden, errors.New("the application account or its authorization has changed"))
		return
	}
	update.Profile.Desktop = nil
	if role != RoleSystem {
		if err := s.upstreamRequest(r.Context(), http.MethodPost, "/internal/profile", update, nil); err != nil {
			fail(w, http.StatusServiceUnavailable, errors.New("the profile authority is unavailable; no local-only update was saved"))
			return
		}
	}
	if err := s.Store.SaveProfile(user.ID, update.Profile); err != nil {
		fail(w, http.StatusServiceUnavailable, errors.New("could not save the profile"))
		return
	}
	if role == RoleSystem {
		_ = s.Store.AddEvent("profile", "Application profile saved.")
	}
	respond(w, http.StatusOK, map[string]bool{"saved": true})
}

// operatorState deliberately excludes server configuration, other accounts,
// password verifiers, device assignments, topology and management settings.
func (s *Service) operatorState(session Session) map[string]any {
	user, _, err := s.Store.User(session.UserID)
	result := map[string]any{"role": RoleApplication, "is_administrator": false, "username": session.Username, "csrf": session.CSRF, "user_role": user.Role, "user": user, "profile": user.Profile, "capabilities": map[string]bool{"dispatch": true, "dispatch_wav": true, "activity": true, "users": false, "radio": false}, "events": []Event{}, "connections": []Connection{}}
	if s.mediaClient != nil {
		result["connections"] = []Connection{s.mediaClient.ConnectionStatus()}
	}
	if err != nil || !user.Enabled {
		result["validation_error"] = "This application account is no longer available."
		result["user"] = nil
		result["profile"] = emptyUserProfile()
		result["system"] = map[string]any{"talkgroups": []Talkgroup{}}
		return result
	}
	system := s.view().System
	if system.ID == "" {
		result["validation_error"] = "System information is not available."
		result["system"] = map[string]any{"talkgroups": []Talkgroup{}}
		return result
	}
	resolved, position, err := resolveUserPosition(user, system, session.PositionID)
	if err != nil {
		result["validation_error"] = err.Error()
		result["system"] = map[string]any{"talkgroups": []Talkgroup{}}
		return result
	}
	user = resolved
	result["user"] = user
	result["position"] = position
	result["positions"] = availableUserPositions(user, system)
	groups := []map[string]any{}
	for _, group := range system.Talkgroups {
		if group.Enabled && slices.Contains(user.Talkgroups, group.TGID) {
			groups = append(groups, map[string]any{"tgid": group.TGID, "alias": group.Alias, "enabled": true, "start_policy": group.StartPolicy, "priority": group.Priority, "admission_mode": group.AdmissionMode, "transmit_mode": group.TransmitMode, "algorithm_id": group.AlgorithmID, "key_id": group.KeyID})
		}
	}
	result["system"] = map[string]any{"id": system.ID, "name": system.Name, "wacn": system.WACN, "system_id": system.SystemID, "callsign": system.Callsign, "talkgroups": groups}
	return result
}
