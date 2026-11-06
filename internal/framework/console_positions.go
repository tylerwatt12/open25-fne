package framework

import (
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"slices"
	"strings"
)

func validPositionPreference(p UserProfile, user User) error {
	if p.PreferredPositionID != "" && (user.Role == "mobile" || !slices.Contains(user.PositionIDs, p.PreferredPositionID)) {
		return errors.New("preferred console position must be assigned to this user")
	}
	return nil
}

func consoleRadio(system SystemConfig, rid int) bool {
	return slices.ContainsFunc(system.Radios, func(r Radio) bool { return r.RID == rid && r.Enabled && r.Kind == "console" })
}

func validateUserIdentity(u User, system SystemConfig) error {
	if u.Role == "mobile" {
		if len(u.PositionIDs) != 0 || u.Profile.PreferredPositionID != "" {
			return errors.New("mobile users use their personal Radio ID, not a dispatch position")
		}
		if !slices.ContainsFunc(system.Radios, func(r Radio) bool { return r.RID == u.RadioID && r.Enabled && r.Kind == "mobile" }) {
			return errors.New("assign an enabled Mobile PTT Radio ID from the Radio Directory")
		}
		return nil
	}
	if u.RadioID != 0 {
		return errors.New("dispatch users use console positions; personal Radio ID must be zero")
	}
	if len(u.PositionIDs) == 0 || len(u.PositionIDs) > 128 {
		return errors.New("assign one or more console positions")
	}
	seen := map[string]bool{}
	for _, id := range u.PositionIDs {
		if seen[id] || !recordID.MatchString(id) {
			return errors.New("console position assignments must be valid and unique")
		}
		seen[id] = true
		if !slices.ContainsFunc(system.ConsolePositions, func(p ConsolePosition) bool { return p.ID == id }) {
			return fmt.Errorf("console position %s is not available", id)
		}
	}
	return validPositionPreference(u.Profile, u)
}

// availableUserPositions exposes only usable positions assigned to this account.
// The position's talkgroup list is an additional ceiling, not a grant of rights.
func availableUserPositions(u User, system SystemConfig) []ConsolePosition {
	out := []ConsolePosition{}
	if !u.Enabled || u.Role == "mobile" {
		return out
	}
	allowed := enabledGroups(system)
	for id := range allowed {
		if !slices.Contains(u.Talkgroups, id) {
			delete(allowed, id)
		}
	}
	for _, p := range system.ConsolePositions {
		if p.Enabled && slices.Contains(u.PositionIDs, p.ID) && consoleRadio(system, p.RadioID) {
			p.Talkgroups = filterGroups(p.Talkgroups, allowed)
			out = append(out, p)
		}
	}
	return out
}

// resolveUserPosition returns an effective copy for this session. The stored
// account remains a person, with RadioID zero for desk-based operators.
func resolveUserPosition(u User, system SystemConfig, positionID string) (User, *ConsolePosition, error) {
	if !u.Enabled {
		return User{}, nil, errors.New("this application account is disabled")
	}
	if u.Role == "mobile" {
		if positionID != "" || validateUserIdentity(u, system) != nil {
			return User{}, nil, errors.New("the mobile Radio ID is not available")
		}
		u.Talkgroups = filterGroups(u.Talkgroups, enabledGroups(system))
		return u, nil, nil
	}
	available := availableUserPositions(u, system)
	if positionID == "" {
		if slices.ContainsFunc(available, func(p ConsolePosition) bool { return p.ID == u.Profile.PreferredPositionID }) {
			positionID = u.Profile.PreferredPositionID
		} else if len(available) == 1 {
			positionID = available[0].ID
		}
	}
	for _, p := range available {
		if p.ID == positionID {
			allowed := enabledGroups(system)
			for id := range allowed {
				if !slices.Contains(p.Talkgroups, id) {
					delete(allowed, id)
				}
			}
			u.RadioID = p.RadioID
			u.Talkgroups = filterGroups(u.Talkgroups, allowed)
			return u, &p, nil
		}
	}
	return User{}, nil, errors.New("select an enabled console position assigned to this user")
}

func enabledGroups(system SystemConfig) map[int]bool {
	allowed := map[int]bool{}
	for _, group := range system.Talkgroups {
		if group.Enabled {
			allowed[group.TGID] = true
		}
	}
	return allowed
}

// userRadioAllowed verifies a routed identity without trusting a browser's RID.
func userRadioAllowed(u User, system SystemConfig, rid int) bool {
	_, _, err := resolveUserRadio(u, system, rid)
	return err == nil
}

func resolveUserRadio(u User, system SystemConfig, rid int) (User, *ConsolePosition, error) {
	if u.Role == "mobile" {
		resolved, p, err := resolveUserPosition(u, system, "")
		if err == nil && resolved.RadioID == rid {
			return resolved, p, nil
		}
	} else {
		for _, p := range availableUserPositions(u, system) {
			if p.RadioID == rid {
				return resolveUserPosition(u, system, p.ID)
			}
		}
	}
	return User{}, nil, errors.New("this user is not assigned to the requested radio identity")
}

func (s *Store) updateConsolePosition(position ConsolePosition, id string, remove bool) error {
	if s.role != RoleSystem {
		return errors.New("console positions belong to System Administration")
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	raw, err := s.Config()
	if err != nil {
		return err
	}
	var system SystemConfig
	if err = json.Unmarshal(raw, &system); err != nil {
		return err
	}
	index := slices.IndexFunc(system.ConsolePositions, func(p ConsolePosition) bool { return p.ID == id })
	if remove {
		system.ConsolePositions = slices.DeleteFunc(system.ConsolePositions, func(p ConsolePosition) bool { return p.ID == id })
	} else if index >= 0 {
		system.ConsolePositions[index] = position
	} else {
		system.ConsolePositions = append(system.ConsolePositions, position)
	}
	raw, err = json.Marshal(system)
	if err != nil {
		return err
	}
	_, err = s.db.Exec("UPDATE framework_documents SET document=?,at=? WHERE name='config'", []byte(raw), nowStamp())
	return err
}

// positionsHTTP is reached only after administrator authentication and CSRF.
func (s *Service) positionsHTTP(w http.ResponseWriter, r *http.Request) {
	if s.Config().Role != RoleSystem {
		fail(w, 403, errors.New("console positions are managed in System Administration"))
		return
	}
	path := strings.TrimPrefix(r.URL.Path, "/api/positions")
	id := strings.TrimPrefix(path, "/")
	if r.Method == http.MethodGet && path == "" {
		var system SystemConfig
		if err := s.owned(&system); err != nil {
			fail(w, 500, err)
			return
		}
		positions := system.ConsolePositions
		if positions == nil {
			positions = []ConsolePosition{}
		}
		respond(w, 200, map[string]any{"positions": positions})
		return
	}
	if r.Method == http.MethodDelete && path != "" {
		if err := s.Store.updateConsolePosition(ConsolePosition{}, id, true); err != nil {
			fail(w, 400, err)
			return
		}
	} else if (r.Method == http.MethodPost && path == "") || (r.Method == http.MethodPut && path != "") {
		var input ConsolePosition
		if err := readJSON(w, r, &input); err != nil {
			fail(w, 400, err)
			return
		}
		if r.Method == http.MethodPost {
			id = input.ID
		}
		if err := s.Store.updateConsolePosition(input, id, false); err != nil {
			fail(w, 400, err)
			return
		}
	} else {
		w.WriteHeader(http.StatusMethodNotAllowed)
		return
	}
	_ = s.Store.AddEvent("configuration", "Console position saved.")
	respond(w, 200, map[string]bool{"saved": true})
}
