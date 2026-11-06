package framework

import (
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"time"

	"open25.local/open25-fne/internal/radio"
)

func (s *Service) webConnectionRows() []map[string]any {
	rows := []map[string]any{}
	var connection Connection
	if s.readCache("connection", &connection) {
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
	fmt.Fprint(w, `<section class=ts-panel aria-label="Administration topology"><div class=ts-panelhead><h2>Network location</h2></div><div class=ts-network-diagram>`)
	nodes := adminVisibleInstances(s.adminInstances())
	node := func(n adminInstance) {
		current := ""
		if n.Current {
			current = " aria-current=true"
		}
		fmt.Fprintf(w, `<div class=ts-node%s><div class=ts-nodebranch>%s%s</div><strong>%s</strong>`, current, adminIcon(n.Icon), esc(strings.TrimPrefix(adminRoleName(n.Role), "Open25 ")), esc(n.Name))
		if n.Current {
			fmt.Fprintf(w, `<span class="ts-badge ts-selected">You are here</span>`)
		} else if n.URL != "" {
			fmt.Fprintf(w, `<a class=ts-link href="%s">Open administration%s</a>`, esc(n.URL), adminPortalIcon())
		} else {
			fmt.Fprint(w, `<small>Address unavailable</small>`)
		}
		fmt.Fprint(w, `</div>`)
	}
	for _, role := range []Role{RoleSystem, RoleZone, RoleSite} {
		if role != RoleSystem {
			fmt.Fprint(w, `<div class=ts-network-edge aria-hidden=true></div>`)
		}
		fmt.Fprint(w, `<div class=ts-network-tier>`)
		for _, n := range nodes {
			if n.Role == role {
				node(n)
			}
		}
		fmt.Fprint(w, `</div>`)
	}
	fmt.Fprint(w, `<div class=ts-network-app><span class=ts-caption>Application connection</span>`)
	for _, n := range nodes {
		if n.Role == RoleApplication {
			node(n)
		}
	}
	fmt.Fprint(w, `</div></div></section>`)
}

func (s *Service) webDashboard(w http.ResponseWriter, r *http.Request, session Session) {
	s.webHeader(w, r, "Overview", session, false)
	defer webFooter(w)
	fmt.Fprint(w, `<div class=ts-toolbar><a class=ts-button href=/dashboard>Refresh</a></div>`)
	connections := s.webConnectionRows()
	attention := []adminLink{}
	for _, c := range connections {
		if message := webText(c["error"]); message != "" {
			attention = append(attention, adminLink{"connections", webText(c["connection"]) + ": " + message})
		} else if c["freshness"] == "Stale" || c["freshness"] == "No successful contact" {
			attention = append(attention, adminLink{"connections", webText(c["connection"]) + ": " + webText(c["freshness"])})
		}
	}
	var local radio.Status
	if s.Config().Role == RoleSite {
		local = s.radioStatus()
		if local.Error != "" {
			attention = append(attention, adminLink{"live-status", local.Error})
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
	class := "ts-panel"
	if len(attention) > 0 {
		class += " ts-attention"
	}
	fmt.Fprintf(w, `<section class="%s"><div class=ts-panelhead><h2>Needs attention</h2></div><ul class=ts-list>`, class)
	for _, item := range attention {
		fmt.Fprintf(w, `<li><div class=ts-itemline><span>%s</span><a class=ts-link href="/%s">Details</a></div></li>`, esc(item.Label), esc(item.Path))
	}
	if len(attention) == 0 {
		fmt.Fprint(w, `<li class=ts-muted>No issues in the available observations.</li>`)
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
		webTable(w, []webColumn{{"radio_id", "Radio ID"}, {"talkgroup_id", "Talkgroup"}, {"source", "Source"}, {"last_seen", "Last seen"}}, incidents)
	} else {
		fmt.Fprint(w, `<p class=ts-muted>No active incident in the stored observations.</p>`)
	}
	if s.broker != nil && (s.Config().Role == RoleZone || s.Config().Role == RoleApplication) {
		fmt.Fprintf(w, `<p>%d current call allocations here</p>`, len(snapshot.CallDelivery))
	} else {
		fmt.Fprint(w, `<p class=ts-muted>Current call allocation count unavailable here.</p>`)
	}
	fmt.Fprint(w, `<a class=ts-link href=/activity-status>Activity details</a></section></div>`)
	if s.Config().Role == RoleSite {
		fmt.Fprint(w, `<section class=ts-panel><div class=ts-panelhead><h2>Local radio readiness</h2></div>`)
		webProperties(w, map[string]any{"state": local.State, "mode": local.Mode, "error": local.Error}, []webColumn{{"state", "Operating state"}, {"mode", "Mode"}, {"error", "Last error"}})
		rows := []map[string]any{}
		for _, ch := range local.Channels {
			rows = append(rows, map[string]any{"channel": ch.Name, "role": ch.Role, "state": ch.State, "ready": ch.Ready, "busy": ch.Busy, "radio_id": ch.RID, "talkgroup_id": ch.TGID})
		}
		webTable(w, []webColumn{{"channel", "Channel"}, {"role", "Role"}, {"state", "State"}, {"ready", "Ready"}, {"busy", "Busy"}, {"radio_id", "Radio ID"}, {"talkgroup_id", "Talkgroup"}}, rows)
		fmt.Fprint(w, `<a class=ts-link href=/live-status>Channel details</a></section>`)
	} else if s.Config().Role == RoleApplication {
		fmt.Fprint(w, `<section class=ts-panel><div class=ts-panelhead><h2>Application readiness</h2></div><div class=ts-actions><a class=ts-link href=/connections>Controller and media connections</a><a class=ts-link href=/enrollment-status>Key enrollment</a><a class=ts-link href=/dispatch-test>Open25 Dispatch</a><a class=ts-link href=/ptt-test>Open25 PTT</a></div></section>`)
	} else {
		s.webDashboardSites(w)
	}
	fmt.Fprint(w, `<section class=ts-panel><div class=ts-panelhead><h2>Recent activity and outcomes</h2></div>`)
	result := s.webAPI(r, session, "/api/activity", "GET", nil)
	if result.code >= 300 {
		webJSONResult(w, result)
	} else {
		record, _ := webConfig(result.Bytes())
		rows := webObservationRows(record["events"], []string{"at", "label", "radio_id", "site_id", "outcome", "message"})
		if len(rows) > 6 {
			rows = rows[:6]
		}
		webTable(w, []webColumn{{"at", "Time"}, {"label", "Event"}, {"site_id", "Site"}, {"outcome", "Result"}, {"message", "Message"}}, rows)
	}
	fmt.Fprint(w, `<div class=ts-actions><a class=ts-link href=/activity-status>Activity and details</a><a class=ts-link href=/notification-delivery>Notification delivery</a></div></section>`)
	fmt.Fprint(w, `<section class=ts-panel><div class=ts-panelhead><h2>Configuration shortcuts</h2></div><div class=ts-actions>`)
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
