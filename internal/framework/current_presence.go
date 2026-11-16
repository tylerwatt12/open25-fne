package framework

import (
	"slices"
	"time"

	"open25.local/open25-fne/internal/radio"
)

// currentRadio is an on-demand view of fresh operational observations. It is
// never persisted, retained after expiry or used as RF admission authority.
type currentRadio struct {
	RID                 int       `json:"rid"`
	Alias               string    `json:"alias"`
	Kind                string    `json:"kind"`
	SiteID              string    `json:"site_id"`
	Registered          bool      `json:"registered"`
	AffiliatedTalkgroup int       `json:"affiliated_talkgroup"`
	LastSeen            time.Time `json:"last_seen"`
}

func (s *Service) currentRadioPresence() []currentRadio {
	view, config, now := s.view(), s.Config(), time.Now().UTC()
	reports := view.Reports
	if config.Role == RoleSite {
		if s.radio == nil {
			return []currentRadio{}
		}
		state := s.radio.SourceState()
		if !state.Active || state.SiteID != config.InstanceID {
			return []currentRadio{}
		}
		reports = []SiteReport{{ID: state.SiteID, ReceivedAt: state.ObservedAt, Radio: &radio.Status{SiteID: state.SiteID, State: "running", Affiliations: state.Affiliations, Registrations: state.Registrations}}}
	}
	knownSites := map[string]bool{}
	if view.Zone != nil {
		for _, site := range view.Zone.Sites {
			knownSites[site.ID] = true
		}
	}
	if config.Role == RoleSite {
		knownSites[config.InstanceID] = true
	}
	directory := map[int]Radio{}
	for _, record := range view.System.Radios {
		if record.Enabled && record.Kind == "subscriber" {
			directory[record.RID] = record
		}
	}
	rows := []currentRadio{}
	for _, report := range reports {
		if !knownSites[report.ID] || !currentUnitReport(report, now) {
			continue
		}
		observed := map[int]currentRadio{}
		for _, registration := range report.Radio.Registrations {
			if registration.At.IsZero() || registration.At.After(now.Add(time.Minute)) {
				continue
			}
			observed[registration.RID] = currentRadio{RID: registration.RID, SiteID: report.ID, Registered: true, LastSeen: report.ReceivedAt}
		}
		for _, affiliation := range report.Radio.Affiliations {
			if affiliation.At.IsZero() || affiliation.At.After(now.Add(time.Minute)) || affiliation.TGID < 1 || affiliation.TGID > 65534 {
				continue
			}
			row := observed[affiliation.RID]
			row.RID, row.SiteID, row.Registered, row.AffiliatedTalkgroup, row.LastSeen = affiliation.RID, report.ID, true, affiliation.TGID, report.ReceivedAt
			observed[affiliation.RID] = row
		}
		for rid, row := range observed {
			if record, ok := directory[rid]; ok {
				row.Alias, row.Kind = record.Alias, record.Kind
				rows = append(rows, row)
			}
		}
	}
	slices.SortFunc(rows, func(a, b currentRadio) int {
		if a.RID != b.RID {
			return a.RID - b.RID
		}
		if a.SiteID < b.SiteID {
			return -1
		}
		if a.SiteID > b.SiteID {
			return 1
		}
		return 0
	})
	return rows
}
