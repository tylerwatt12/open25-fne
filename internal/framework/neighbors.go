package framework

import (
	"time"

	"open25.local/open25-fne/internal/radio"
)

// The Zone projects its registry and the last authenticated Site reports.
// The recipient learns no other Site's RF configuration, credentials or routes.
func (s *Service) zoneNeighborSites(system SystemConfig, zone ZoneConfig, recipient string, reports []SiteReport) []radio.NeighborSite {
	byID := make(map[string]SiteReport, len(reports))
	for _, report := range reports {
		byID[report.ID] = report
	}
	now := time.Now()
	maxAge := time.Duration(max(10, s.Config().PollSeconds*4)) * time.Second
	neighbors := make([]radio.NeighborSite, 0, len(zone.Sites))
	for _, site := range zone.Sites {
		if site.ID == recipient {
			continue
		}
		neighbor := radio.NeighborSite{ID: site.ID, Name: site.Name}
		report := byID[site.ID]
		if report.RFEnabled && report.Radio != nil && report.Radio.AdjacentSite != nil {
			control := report.Radio.AdjacentSite
			expires := report.ReceivedAt.Add(maxAge)
			if observedExpiry := report.ObservedAt.Add(maxAge); observedExpiry.Before(expires) {
				expires = observedExpiry
			}
			if expires.After(now) && control.WACN == system.WACN && control.SystemID == system.SystemID && control.RFSSID == zone.RFSSID &&
				control.SiteID == site.SiteID && control.LRA == site.LRA {
				neighbor.Control = control
				neighbor.ValidUntil = expires
			}
		}
		neighbors = append(neighbors, neighbor)
	}
	return neighbors
}

func (s *Service) neighborDirectory() []radio.NeighborSite {
	var sites []radio.NeighborSite
	s.readCache("neighbor_directory", &sites)
	return sites
}
