package framework

import (
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"time"
)

func (s *Service) webConnectionRows() []map[string]any {
	rows := []map[string]any{}
	var connection Connection
	if s.readCache("connection", &connection) {
		if s.parentIsolated.Load() {
			connection.State = "Test isolation"
			connection.Error = "Parent Zone disconnected for testing"
			connection.URL = s.configuredUpstreamURL()
		}
		freshness := "Last contact"
		if connection.LastSuccess.IsZero() {
			freshness = "No successful contact"
		} else if time.Since(connection.LastSuccess) > time.Duration(max(10, s.Config().PollSeconds*4))*time.Second {
			freshness = "Stale"
		}
		rows = append(rows, map[string]any{"connection": connection.Name, "reported_state": connection.State, "freshness": freshness, "last_success": connection.LastSuccess, "error": connection.Error})
	}
	if s.Config().Role == RoleApplication && s.mediaClient != nil {
		media := s.mediaClient.ConnectionStatus()
		rows = append(rows, map[string]any{"connection": media.Name, "reported_state": media.State, "last_success": media.LastSuccess, "error": media.Error})
	}
	return rows
}

// The map groups known owners, not unmeasured RF or physical cabling.
func (s *Service) webTopologyMap(w io.Writer) {
	fmt.Fprint(w, `<section class="ts-panel location-strip" aria-label="Administration topology"><div class=ts-panelhead><h2>Network location</h2></div><div class=ts-network-diagram>`)
	nodes := adminVisibleInstances(s.adminInstances())
	for _, role := range []Role{RoleSystem, RoleZone, RoleSite, RoleApplication} {
		present := false
		for _, n := range nodes {
			present = present || n.Role == role
		}
		if !present {
			continue
		}
		if role == RoleApplication {
			fmt.Fprint(w, `<div class=ts-network-app><span class=ts-caption>Application connection</span>`)
		} else if role != RoleSystem {
			fmt.Fprint(w, `<div class=ts-network-edge aria-hidden=true></div>`)
		}
		fmt.Fprint(w, `<div class=compact-topology>`)
		for _, n := range nodes {
			if n.Role != role {
				continue
			}
			label := adminInstanceLabel(n)
			switch {
			case n.Current:
				fmt.Fprintf(w, `<a href=/dashboard aria-current=page aria-label="%s — You are here">%s%s</a>`, esc(label), adminIcon(n.Icon), esc(label))
			case n.URL != "":
				fmt.Fprintf(w, `<a href="%s">%s%s%s</a>`, esc(n.URL), adminIcon(n.Icon), esc(label), adminPortalIcon())
			default:
				fmt.Fprintf(w, `<span>%s · Address unavailable</span>`, esc(label))
			}
		}
		fmt.Fprint(w, `</div>`)
		if role == RoleApplication {
			fmt.Fprint(w, `</div>`)
		}
	}
	fmt.Fprint(w, `</div></section>`)
}

func (s *Service) webDashboard(w http.ResponseWriter, r *http.Request, session Session) {
	if s.Config().Role == RoleSite {
		s.webSiteDashboard(w, r, session)
		return
	}
	s.webHeader(w, r, "Overview", session, false)
	defer webFooter(w)
	connections := s.webConnectionRows()
	attention := []adminLink{}
	for _, c := range connections {
		if message := webText(c["error"]); message != "" {
			attention = append(attention, adminLink{"connections", webText(c["connection"]) + ": " + message})
		} else if c["freshness"] == "Stale" || c["freshness"] == "No successful contact" {
			attention = append(attention, adminLink{"connections", webText(c["connection"]) + ": " + webText(c["freshness"])})
		}
	}
	if len(connections) == 0 && s.Config().Role != RoleSystem {
		attention = append(attention, adminLink{"connections", "Connection observations unavailable"})
	}
	if s.Config().Role != RoleSite && s.Config().Role != RoleApplication {
		zone := s.webKnownZone()
		if zone == nil {
			attention = append(attention, adminLink{"reports", "Site programming and reports unavailable"})
		} else {
			reports := s.siteReports()
			for _, site := range zone.Sites {
				found := false
				for _, report := range reports {
					if report.ID == site.ID {
						found = true
						if report.ObservedAt.IsZero() || time.Since(report.ObservedAt) > time.Duration(max(10, s.Config().PollSeconds*4))*time.Second {
							attention = append(attention, adminLink{"reports?site=" + url.QueryEscape(site.ID), site.Name + ": stale report"})
						} else if report.Radio != nil && report.Radio.Error != "" {
							attention = append(attention, adminLink{"reports?site=" + url.QueryEscape(site.ID), site.Name + ": " + report.Radio.Error})
						} else if report.Error != "" {
							attention = append(attention, adminLink{"reports?site=" + url.QueryEscape(site.ID), site.Name + ": " + report.Error})
						}
					}
				}
				if !found {
					attention = append(attention, adminLink{"reports?site=" + url.QueryEscape(site.ID), site.Name + ": no report received"})
				}
			}
		}
	}
	class := "ts-panel summary-strip"
	if len(attention) > 0 {
		class += " ts-attention"
	}
	fmt.Fprintf(w, `<section class="%s"><h2>Needs attention</h2><ul class=ts-list>`, class)
	for _, item := range attention {
		fmt.Fprintf(w, `<li><div class=ts-itemline><span>%s</span><a class=ts-link href="/%s">Details</a></div></li>`, esc(item.Label), esc(item.Path))
	}
	if len(attention) == 0 {
		fmt.Fprint(w, `<li class=ts-muted>No current faults in the available observations.</li>`)
	}
	fmt.Fprint(w, `</ul></section>`)
	s.webTopologyMap(w)
	fmt.Fprint(w, `<div class=ts-overview><section class=ts-panel><div class=ts-panelhead><h2>Service connections</h2></div>`)
	if len(connections) == 0 {
		fmt.Fprint(w, `<p class=ts-muted>No connection observations available.</p>`)
	} else {
		fmt.Fprint(w, `<ul class=ts-list>`)
		for _, c := range connections {
			fmt.Fprintf(w, `<li><div class=ts-itemline><strong>%s</strong>%s</div><p class=ts-caption>%s · `, esc(webText(c["connection"])), webStatus(webText(c["reported_state"])), esc(webText(c["freshness"])))
			webScalar(w, "last_success", webPresentationValue(c["last_success"]))
			fmt.Fprint(w, `</p></li>`)
		}
		fmt.Fprint(w, `</ul>`)
	}
	fmt.Fprint(w, `<a class=ts-link href=/connections>Connection details</a></section><section class=ts-panel><div class=ts-panelhead><h2>Calls and emergencies</h2></div>`)
	snapshot := s.operatorSnapshot()
	incidents := []map[string]any{}
	for _, e := range snapshot.Emergencies {
		if e.ClearedAt.IsZero() {
			incidents = append(incidents, map[string]any{"radio_id": e.RadioID, "talkgroup_id": e.TalkgroupID, "source": e.ScopeID, "last_seen": e.LastSeen})
		}
	}
	if len(incidents) > 0 {
		s.webTable(w, []webColumn{{"radio_id", "Radio ID"}, {"talkgroup_id", "Talkgroup"}, {"source", "Source"}, {"last_seen", "Last seen"}}, incidents)
	} else {
		fmt.Fprint(w, `<p class=ts-muted>No active incident in the stored observations.</p>`)
	}
	if s.broker != nil && (s.Config().Role == RoleZone || s.Config().Role == RoleApplication) {
		fmt.Fprintf(w, `<p>%d current call allocations here</p>`, len(snapshot.CallDelivery))
	} else {
		fmt.Fprint(w, `<p class=ts-muted>Current call allocation count unavailable here.</p>`)
	}
	fmt.Fprint(w, `</section></div>`)
	if s.Config().Role == RoleApplication {
		fmt.Fprint(w, `<section class=ts-panel><div class=ts-panelhead><h2>Application readiness</h2></div><div class=ts-actions><a class=ts-link href=/connections>Controller and media connections</a><a class=ts-link href=/enrollment-status>Key enrollment</a><a class=ts-link href=/dispatch-test>Open25 Dispatch</a><a class=ts-link href=/ptt-test>Open25 PTT</a></div></section>`)
	} else {
		s.webDashboardSites(w)
	}
	fmt.Fprint(w, `<section class=ts-panel><div class=ts-panelhead><h2>Recent diagnostics</h2></div>`)
	events, err := s.Store.Events(6)
	if err != nil {
		fmt.Fprint(w, `<p>Activity records unavailable.</p>`)
	} else {
		s.webTable(w, []webColumn{{"at", "Time"}, {"kind", "Event"}, {"message", "Message"}}, events)
	}

	fmt.Fprint(w, `<div class=ts-actions><a class=ts-link href=/notification-delivery>Notification delivery</a></div></section>`)
	if s.Config().Role != RoleSystem {
		s.webHistoryDelivery(w)
	}
	fmt.Fprint(w, `<section class=ts-panel><div class=ts-panelhead><h2>Configuration</h2></div><div class=ts-actions>`)
	for _, path := range adminPrimaryPages[s.Config().Role] {
		fmt.Fprintf(w, `<a class=ts-button href="/%s">%s%s</a>`, path, adminIcon(adminIcons[path]), esc(adminLabel(s.Config().Role, path, path)))
	}
	fmt.Fprint(w, `</div></section>`)
}

func (s *Service) webDashboardSites(w io.Writer) {
	zone := s.webKnownZone()
	if zone == nil || len(zone.Sites) == 0 {
		return
	}
	reports := s.siteReports()
	fmt.Fprint(w, "<section class=ts-panel><div class=section-heading><h2>Site readiness</h2>")
	fmt.Fprint(w, "</div><div class=table-wrap tabindex=0 aria-label=\"Scrollable records\"><table class=record-table><thead><tr><th>Site</th><th>Report</th><th>Last reported radio state</th><th>Observed</th><th>Last reported issue</th><th>Details</th></tr></thead><tbody>")
	for _, site := range zone.Sites {
		name := site.Name
		if name == "" {
			name = site.ID
		}
		freshness, state, issue := "No report", "Unknown", "—"
		var observed time.Time
		for _, report := range reports {
			if report.ID != site.ID {
				continue
			}
			freshness, observed = "Received", report.ObservedAt
			if report.Error != "" {
				freshness, issue = "Error reported", report.Error
			}
			if report.ObservedAt.IsZero() || time.Since(report.ObservedAt) > time.Duration(max(10, s.Config().PollSeconds*4))*time.Second || strings.Contains(strings.ToLower(report.Error), "stale") {
				freshness = "Stale"
			}
			if report.Radio != nil && report.Radio.State != "" {
				state = report.Radio.State
			}
			if report.Radio != nil && report.Radio.Error != "" && report.Radio.Error != report.Error {
				if issue == "—" {
					issue = report.Radio.Error
				} else {
					issue += "; " + report.Radio.Error
				}
			}
		}
		fmt.Fprintf(w, "<tr><td>%s</td><td>%s</td><td>%s</td><td class=time>%s</td><td>%s</td><td><a href=\"/reports?site=%s\">Details</a></td></tr>", esc(name), webStatus(freshness), webStatus(state), webTime(observed), esc(issue), esc(url.QueryEscape(site.ID)))
	}
	fmt.Fprint(w, "</tbody></table></div></section>")
}
