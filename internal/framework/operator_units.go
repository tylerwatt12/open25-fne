package framework

import (
	"errors"
	"sort"
	"time"

	"open25.local/open25-fne/internal/activity"
	"open25.local/open25-fne/internal/radio"
)

// OperatorUnit describes an accepted, currently reported RF affiliation. It is
// computed for this read only; it is not a second directory or a replicated
// operator preference. Listening application sessions are never RF units.
type OperatorUnit struct {
	RadioID       int       `json:"radio_id"`
	Alias         string    `json:"alias"`
	TalkgroupID   int       `json:"talkgroup_id"`
	TalkgroupName string    `json:"talkgroup_name"`
	SiteID        string    `json:"site_id"`
	SiteName      string    `json:"site_name"`
	AffiliatedAt  time.Time `json:"affiliated_at"`
	ObservedAt    time.Time `json:"observed_at"`
}

const operatorUnitReportAge = 4 * time.Second

func currentUnitReport(report SiteReport, now time.Time) bool {
	return report.Error == "" && report.Radio != nil && report.Radio.SiteID == report.ID && report.Radio.State == "running" &&
		!report.ReceivedAt.IsZero() && !report.ReceivedAt.After(now.Add(time.Second)) &&
		now.Sub(report.ReceivedAt) <= operatorUnitReportAge
}

func (s *Service) operatorUnits(authority operatorAuthority, group int) ([]OperatorUnit, error) {
	if group < 0 || group > 65534 {
		return nil, errors.New("select a permitted talkgroup")
	}
	view, config, now := s.view(), s.Config(), time.Now().UTC()
	reports := view.Reports
	if config.Role == RoleZone {
		if view.Zone == nil || s.rfAuthority == nil || s.rfAuthority.ctx.Err() != nil {
			return nil, errors.New("current radio ownership is unavailable")
		}
	} else if config.Role == RoleSite {
		status := s.radioStatus()
		if status.State != "running" {
			return nil, errors.New("the Site radio service is not running")
		}
		reports = []SiteReport{{ID: config.InstanceID, ReceivedAt: now, Radio: &status}}
	} else {
		return nil, errors.New("current radio affiliations belong to Zone and Site controllers")
	}

	radios := map[int]Radio{}
	for _, record := range view.System.Radios {
		if record.Enabled && record.Kind == "subscriber" {
			radios[record.RID] = record
		}
	}
	permittedGroups := map[int]bool{}
	for _, id := range authority.user.Talkgroups {
		permittedGroups[id] = true
	}
	groups := map[int]Talkgroup{}
	for _, record := range view.System.Talkgroups {
		if record.Enabled && (authority.admin || permittedGroups[record.TGID]) && (config.Role != RoleSite || talkgroupAtSite(record, config.InstanceID)) {
			groups[record.TGID] = record
		}
	}
	if group != 0 {
		if _, allowed := groups[group]; !allowed {
			return nil, errors.New("select a permitted talkgroup")
		}
	}
	sites := map[string]string{}
	if view.Zone != nil {
		for _, identity := range view.Zone.Sites {
			sites[identity.ID] = identity.Name
		}
	}
	if config.Role == RoleSite {
		if _, ok := sites[config.InstanceID]; !ok {
			sites[config.InstanceID] = config.InstanceID
		}
	}
	accepted := []SiteReport{}
	for _, report := range reports {
		if _, known := sites[report.ID]; known {
			accepted = append(accepted, report)
		}
	}
	owners := map[int]affiliationOwner{}
	if config.Role == RoleZone {
		state := s.rfAuthority.value.Load()
		for rid, owner := range state.owners {
			if !state.fenced[rid] {
				owners[rid] = owner
			}
		}
	} else {
		presence := []activity.Radio{}
		for _, row := range view.Presence {
			if _, known := sites[row.SiteID]; known && row.Kind == "rf" {
				presence = append(presence, row)
			}
		}
		owners = latestAffiliationOwners(accepted, presence, now)
	}
	units := []OperatorUnit{}
	seen := map[int]bool{}
	for _, report := range accepted {
		if !currentUnitReport(report, now) {
			continue
		}
		latest := map[int]radio.Affiliation{}
		for _, affiliation := range report.Radio.Affiliations {
			previous, known := latest[affiliation.RID]
			if !known || affiliation.At.After(previous.At) {
				latest[affiliation.RID] = affiliation
			}
		}
		for _, affiliation := range latest {
			r, allowed := radios[affiliation.RID]
			owner, owned := owners[affiliation.RID]
			resource, permitted := groups[affiliation.TGID]
			if !allowed || !owned || !permitted || !subscriberGroupAllowed(view.System, affiliation.RID, affiliation.TGID, false) || seen[r.RID] || owner.SiteID != report.ID || owner.TGID != affiliation.TGID ||
				(group != 0 && affiliation.TGID != group) || !talkgroupAtSite(resource, report.ID) ||
				affiliation.At.IsZero() || affiliation.At.After(now.Add(time.Minute)) {
				continue
			}
			seen[r.RID] = true
			units = append(units, OperatorUnit{RadioID: r.RID, Alias: r.Alias, TalkgroupID: affiliation.TGID, TalkgroupName: resource.Alias, SiteID: report.ID, SiteName: sites[report.ID], AffiliatedAt: affiliation.At, ObservedAt: report.ReceivedAt})
		}
	}
	sort.Slice(units, func(i, j int) bool { return units[i].RadioID < units[j].RadioID })
	return units, nil
}
