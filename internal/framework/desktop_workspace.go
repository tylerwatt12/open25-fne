package framework

import (
	"database/sql"
	"encoding/json"
	"errors"
	"slices"
)

// DesktopProfile contains portable layout preferences, never live selections,
// grants, patches, commands, device names or credentials. Radio-style PTT
// programming remains in UserProfile's existing Zones and Receive fields.
type DesktopProfile struct {
	Theme              string             `json:"theme"`
	Density            string             `json:"density"`
	ActiveWorkspaceID  string             `json:"active_workspace_id"`
	DefaultWorkspaceID string             `json:"default_workspace_id"`
	Audio              DesktopAudioPolicy `json:"audio"`
	Receive            []ReceiveSetting   `json:"receive"`
	Workspaces         []DesktopWorkspace `json:"workspaces"`
}

type DesktopAudioRoute struct {
	Volume float64 `json:"volume"`
	Pan    int     `json:"pan"`
}

type DesktopAudioPolicy struct {
	Selected   DesktopAudioRoute `json:"selected"`
	Unselected DesktopAudioRoute `json:"unselected"`
}

type DesktopWorkspace struct {
	ID           string               `json:"id"`
	Name         string               `json:"name"`
	Folders      []DesktopFolder      `json:"folders"`
	Placements   []DesktopPlacement   `json:"placements"`
	Multiselects []DesktopMultiselect `json:"multiselects"`
}

type DesktopFolder struct {
	ID    string `json:"id"`
	Name  string `json:"name"`
	Color string `json:"color"`
}

type DesktopPlacement struct {
	ID          string `json:"id"`
	TalkgroupID int    `json:"talkgroup_id"`
	FolderID    string `json:"folder_id"`
	Primary     bool   `json:"primary"`
	Expanded    bool   `json:"expanded"`
	ActivePanel string `json:"active_panel"`
}

// A named multiselect is a reusable destination list. Saving or opening it
// does not select transmit resources or acquire a floor.
type DesktopMultiselect struct {
	ID         string `json:"id"`
	Name       string `json:"name"`
	Talkgroups []int  `json:"talkgroups"`
}

func emptyDesktopProfile() DesktopProfile {
	return DesktopProfile{Theme: "light", Density: "compact", Audio: DesktopAudioPolicy{
		Selected: DesktopAudioRoute{Volume: 1}, Unselected: DesktopAudioRoute{Volume: 0.6},
	}, Receive: []ReceiveSetting{}, Workspaces: []DesktopWorkspace{}}
}

func cloneDesktopProfile(p DesktopProfile) DesktopProfile {
	p.Receive = slices.Clone(p.Receive)
	p.Workspaces = slices.Clone(p.Workspaces)
	for i := range p.Workspaces {
		w := &p.Workspaces[i]
		w.Folders = slices.Clone(w.Folders)
		w.Placements = slices.Clone(w.Placements)
		w.Multiselects = slices.Clone(w.Multiselects)
		for j := range w.Multiselects {
			w.Multiselects[j].Talkgroups = slices.Clone(w.Multiselects[j].Talkgroups)
		}
	}
	return p
}

// Scope reads to current permissions without rewriting a user's saved layout.
// The same document can be exported and imported; it contains no authority.
func scopedDesktopProfile(profile *DesktopProfile, allowed []int) DesktopProfile {
	p := emptyDesktopProfile()
	if profile != nil {
		p = cloneDesktopProfile(*profile)
	}
	p.Receive = slices.DeleteFunc(p.Receive, func(r ReceiveSetting) bool { return !slices.Contains(allowed, r.TalkgroupID) })
	for i := range p.Workspaces {
		w := &p.Workspaces[i]
		w.Placements = slices.DeleteFunc(w.Placements, func(r DesktopPlacement) bool { return !slices.Contains(allowed, r.TalkgroupID) })
		for j := range w.Multiselects {
			w.Multiselects[j].Talkgroups = slices.DeleteFunc(w.Multiselects[j].Talkgroups, func(tg int) bool { return !slices.Contains(allowed, tg) })
		}
		w.Multiselects = slices.DeleteFunc(w.Multiselects, func(m DesktopMultiselect) bool { return len(m.Talkgroups) == 0 })
	}
	return p
}

func (s *Store) SaveDesktopProfile(id string, p DesktopProfile) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	records, err := s.userRecords()
	if err != nil {
		return err
	}
	found := false
	for i, record := range records {
		if record.User.ID != id {
			continue
		}
		if !record.User.Enabled || record.User.Role == "mobile" {
			return errors.New("desktop workspaces require an enabled dispatch account")
		}
		records[i].User.Profile.Desktop = &p
		found = true
		break
	}
	if !found {
		return sql.ErrNoRows
	}
	raw, err := json.Marshal(records)
	if err != nil {
		return err
	}
	_, err = s.db.Exec("UPDATE framework_documents SET document=?,at=? WHERE name=?", raw, nowStamp(), s.usersKey())
	return err
}
