package framework

import "slices"

// Scope copies contain only currently usable application identities. Removing a
// permission does not reject the new system policy because an old profile used it.
func scopeUsers(records []userRecord, groups []Talkgroup) []userRecord {
	allowed := map[int]bool{}
	for _, g := range groups {
		if g.Enabled {
			allowed[g.TGID] = true
		}
	}
	out := []userRecord{}
	for _, r := range records {
		if !r.User.Enabled {
			continue
		}
		u := r.User
		u.PositionIDs = slices.Clone(u.PositionIDs)
		u.Talkgroups = slices.Clone(u.Talkgroups)
		u.Permissions = slices.Clone(u.Permissions)
		u.Profile = filterProfile(u.Profile, allowed)
		u.Talkgroups = filterGroups(u.Talkgroups, allowed)
		if u.Profile.Desktop != nil {
			desktop := scopedDesktopProfile(u.Profile.Desktop, u.Talkgroups)
			u.Profile.Desktop = &desktop
		}
		out = append(out, userRecord{u, slices.Clone(r.PasswordHash)})
	}
	return out
}
func filterGroups(ids []int, allowed map[int]bool) []int {
	out := []int{}
	for _, id := range ids {
		if allowed[id] {
			out = append(out, id)
		}
	}
	return out
}
func filterProfile(p UserProfile, allowed map[int]bool) UserProfile {
	out := p
	out.Zones = []ProfileZone{}
	for _, z := range p.Zones {
		out.Zones = append(out.Zones, ProfileZone{z.Name, filterGroups(z.Talkgroups, allowed)})
	}
	out.Receive = []ReceiveSetting{}
	for _, r := range p.Receive {
		if allowed[r.TalkgroupID] {
			out.Receive = append(out.Receive, r)
		}
	}
	out.TransmitTalkgroups = filterGroups(p.TransmitTalkgroups, allowed)
	if !allowed[p.SelectedTalkgroup] {
		out.SelectedTalkgroup = 0
	}
	return out
}
func eligibleUsers(records []userRecord, system SystemConfig) []userRecord {
	out := []userRecord{}
	for _, r := range scopeUsers(records, system.Talkgroups) {
		if r.User.Role == "mobile" {
			if _, _, err := resolveUserPosition(r.User, system, ""); err == nil {
				out = append(out, r)
			}
		} else if len(availableUserPositions(r.User, system)) > 0 {
			out = append(out, r)
		}
	}
	return out
}
