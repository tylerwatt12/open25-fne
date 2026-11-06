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

func webSourceTag(w io.Writer, label string) {
	fmt.Fprintf(w, "<span class=source-tag>%s</span>", esc(label))
}

func webReportTime(value time.Time) string {
	if value.IsZero() {
		return "—"
	}
	return value.Format("2006-01-02 15:04:05 MST")
}

// Configured addresses remain editable; only ordinary web addresses become links.
func (s *Service) webAdminAddress(w io.Writer, address, label string) {
	parsed, err := url.Parse(address)
	if err != nil || parsed.Host == "" || (parsed.Scheme != "https" && parsed.Scheme != "http") || parsed.User != nil {
		fmt.Fprint(w, "<span class=muted>Unavailable</span>")
		return
	}
	outside := ""
	current, _ := url.Parse(s.Config().PublicURL)
	if current == nil || parsed.Scheme != current.Scheme || parsed.Host != current.Host {
		outside = adminPortalIcon()
	}
	fmt.Fprintf(w, `<a href="%s">%s%s</a>`, esc(address), esc(label), outside)
}

func webReportMeta(w io.Writer, source, via string, local bool, observed, received time.Time) {
	if local && observed.IsZero() && received.IsZero() {
		return
	}
	fmt.Fprint(w, `<div class=report-meta>`)
	if !local {
		webSourceTag(w, source)
	}
	if !observed.IsZero() {
		fmt.Fprintf(w, `<span>Observed %s</span>`, webTime(observed))
	}
	if !received.IsZero() {
		label := "Received here"
		if via != "" {
			label = "Received by " + via
		}
		fmt.Fprintf(w, `<span>%s %s</span>`, esc(label), webTime(received))
	}
	fmt.Fprint(w, webHelp("Report source", "Observed is when the source measured this state. Received is when the report reached this service. Receipt keeps the original observation time."), `</div>`)
}

// This projection intentionally excludes programmed RF values and native ports.
func webRadioObservations(w io.Writer, status *radio.Status) {
	if status == nil {
		fmt.Fprint(w, "<p class=muted>No observations.</p>")
		return
	}
	webProperties(w, map[string]any{"state": status.State, "mode": status.Mode, "zone_connected": status.ZoneConnected, "fne_ready": status.FNEReady, "started_at": status.StartedAt, "error": status.Error}, []webColumn{{"state", "Radio state"}, {"mode", "Operating mode"}, {"zone_connected", "Zone connected"}, {"fne_ready", "Fixed network ready"}, {"started_at", "Started"}, {"error", "Last error"}})
	webControlObservations(w, *status)
	fmt.Fprint(w, "<h3>Channel observations</h3>")
	channels := make([]map[string]any, 0, len(status.Channels))
	for _, channel := range status.Channels {
		channels = append(channels, map[string]any{"channel": channel.Name, "record_id": channel.ID, "role": channel.Role, "host_state": channel.State, "ready": channel.Ready, "busy": channel.Busy, "radio_id": channel.RID, "talkgroup_id": channel.TGID, "error": channel.Error})
	}
	webTable(w, []webColumn{{"channel", "Channel"}, {"role", "Role"}, {"host_state", "State"}, {"ready", "Ready"}, {"busy", "Busy"}, {"radio_id", "Radio ID"}, {"talkgroup_id", "Talkgroup ID"}, {"error", "Last error"}}, channels)
	fmt.Fprint(w, "<h3>Registration</h3>")
	registrations := make([]map[string]any, 0, len(status.Registrations))
	for _, registration := range status.Registrations {
		registrations = append(registrations, map[string]any{"rid": registration.RID, "at": registration.At})
	}
	webTable(w, []webColumn{{"rid", "Radio ID"}, {"at", "Registered"}}, registrations)
	fmt.Fprint(w, "<h3>Affiliation</h3>")
	affiliations := make([]map[string]any, 0, len(status.Affiliations))
	for _, affiliation := range status.Affiliations {
		affiliations = append(affiliations, map[string]any{"rid": affiliation.RID, "tgid": affiliation.TGID, "at": affiliation.At})
	}
	webTable(w, []webColumn{{"rid", "Radio ID"}, {"tgid", "Talkgroup ID"}, {"at", "Affiliated"}}, affiliations)
}

func (s *Service) webKnownZone() *ZoneConfig {
	var zone ZoneConfig
	if s.Config().Role == RoleZone {
		if s.owned(&zone) != nil {
			return nil
		}
	} else if !s.readCache("zone", &zone) {
		return nil
	}
	return &zone
}

func (s *Service) webInterfaces(w io.Writer) {
	s.webTopologyMap(w)
}

func (s *Service) webLocalStatus(w http.ResponseWriter, r *http.Request, session Session, live bool) {
	s.webHeader(w, r, "Local Status", session, live)
	defer webFooter(w)
	webReportMeta(w, s.Config().InstanceID, "", true, time.Time{}, time.Time{})
	fmt.Fprint(w, "<p><a class=button href=\"\">Refresh</a></p><h3>Connections</h3>")
	webTable(w, []webColumn{{"connection", "Connection"}, {"reported_state", "Reported state"}, {"freshness", "Report"}, {"last_success", "Last contact"}, {"error", "Last error"}}, s.webConnectionRows())
	if s.Config().Role == RoleSite {
		fmt.Fprint(w, "<h3>Local radio observations</h3>")
		status := s.radioStatus()
		webRadioObservations(w, &status)
	}
}

func (s *Service) webSiteReports(w http.ResponseWriter, r *http.Request, session Session) {
	s.webHeader(w, r, "Reported Site Status", session, r.URL.Path == "/live-status")
	defer webFooter(w)
	reports := s.siteReports()
	zone := s.webKnownZone()
	identities := []SiteIdentity{}
	if zone != nil {
		identities = zone.Sites
	}
	if s.Config().Role == RoleSite {
		identities = nil
		for _, report := range reports {
			identities = append(identities, SiteIdentity{ID: report.ID, Name: report.Name, AdminURL: report.AdminURL})
		}
	}
	receivedLabel := "Received here"
	if s.Config().Role == RoleSystem {
		receivedLabel = "Received by Zone"
	}
	fmt.Fprintf(w, "<p><a class=button href=\"\">Refresh</a></p><div class=table-wrap tabindex=0 aria-label=\"Scrollable records\"><table class=record-table><thead><tr><th>Source</th><th>Report</th><th>Observed</th><th>%s</th><th>Open</th></tr></thead><tbody>", receivedLabel)
	selected := r.URL.Query().Get("site")
	if selected == "" && len(identities) > 0 {
		selected = identities[0].ID
	}
	for _, identity := range identities {
		name := identity.Name
		if name == "" {
			name = identity.ID
		}
		state := "No report"
		address := identity.AdminURL
		var observed, received time.Time
		for _, report := range reports {
			if report.ID == identity.ID {
				observed, received, state = report.ObservedAt, report.ReceivedAt, "Received"
				if report.AdminURL != "" {
					address = report.AdminURL
				}
				if report.Error != "" {
					state = report.Error
				}
			}
		}
		fmt.Fprintf(w, "<tr><td><a href=\"/reports?site=%s\">%s</a></td><td>%s</td><td>%s</td><td>%s</td><td>", esc(url.QueryEscape(identity.ID)), esc(name), esc(state), webTime(observed), webTime(received))
		s.webAdminAddress(w, address, "Open owner")
		fmt.Fprint(w, "</td></tr>")
	}
	if len(identities) == 0 {
		fmt.Fprint(w, "<tr><td colspan=5>No Site reports.</td></tr>")
	}
	fmt.Fprint(w, "</tbody></table></div>")
	if selected != "" {
		fmt.Fprintf(w, "<section class=record-detail><h3>Site status · %s</h3>", esc(selected))
		var report *SiteReport
		for i := range reports {
			if reports[i].ID == selected {
				report = &reports[i]
			}
		}
		if report == nil {
			webReportMeta(w, selected, "", false, time.Time{}, time.Time{})
			fmt.Fprint(w, "<p class=muted>No report.</p>")
		} else {
			via := ""
			if s.Config().Role == RoleSystem && zone != nil {
				via = zone.Name
				if via == "" {
					via = zone.ID
				}
			}
			webReportMeta(w, report.ID, via, s.Config().Role == RoleSite, report.ObservedAt, report.ReceivedAt)
			if s.Config().Role == RoleSystem {
				var relay zoneReport
				if s.readCache("zone_report", &relay) {
					fmt.Fprintf(w, "<div class=report-meta><span>Relayed here %s</span></div>", webTime(relay.ReceivedAt))
				}
			}
			if report.Error != "" {
				fmt.Fprintf(w, "<p class=notice>%s</p>", esc(report.Error))
			}
			webRadioObservations(w, report.Radio)
		}
	}
	if selected != "" {
		fmt.Fprint(w, "</section>")
	}
}

func (s *Service) webZoneStatus(w http.ResponseWriter, r *http.Request, session Session) {
	s.webHeader(w, r, "Reported Zone Status", session, false)
	defer webFooter(w)
	var report zoneReport
	if !s.readCache("zone_report", &report) {
		webReportMeta(w, "Zone", "", false, time.Time{}, time.Time{})
		fmt.Fprint(w, "<p class=muted>No report.</p>")
		return
	}
	webReportMeta(w, report.ID, "", false, report.ObservedAt, report.ReceivedAt)
	if report.ReceivedAt.IsZero() || time.Since(report.ReceivedAt) > time.Duration(max(10, s.Config().PollSeconds*4))*time.Second {
		webSourceTag(w, "Stale")
	}
	fmt.Fprintf(w, "<p>Site reports received: %d</p><p><a href=/reports>Site status</a></p>", len(report.Sites))
	s.webAdminAddress(w, report.AdminURL, "Open Zone Administration")
}

func (s *Service) webReference(w http.ResponseWriter, r *http.Request, session Session, zoneReference bool) {
	title, owner := "System Reference", "System"
	zone := s.webKnownZone()
	if zoneReference {
		title, owner = "Zone Reference", "Zone"
		if zone != nil {
			owner = zone.Name
			if owner == "" {
				owner = zone.ID
			}
		}
	}
	s.webHeader(w, r, title, session, false)
	defer webFooter(w)
	webSourceTag(w, owner)

	fmt.Fprint(w, webHelp("About reference programming", "Settings received from the named owner. Open that service’s administration page to edit them. This read-only reference contains accepted programming."))
	if s.Config().Role == RoleSystem {
		fmt.Fprint(w, "<p class=muted>No upstream programming.</p>")
		return
	}
	if zoneReference {
		if zone == nil {
			fmt.Fprint(w, "<p class=muted>No programming received.</p>")
			return
		}
		fmt.Fprint(w, "<h3>Band Plan</h3>")
		webTable(w, []webColumn{{"identifier", "Channel Identifier"}, {"base_frequency_hz", "Base (MHz)"}, {"channel_spacing_hz", "Spacing (kHz)"}, {"uplink_offset_hz", "Offset (MHz)"}, {"bandwidth_hz", "Bandwidth (kHz)"}}, zone.ChannelIdentifiers)
		tree := s.adminTopology(zone)
		s.webAdminAddress(w, tree.ZoneURL, "Open Zone Administration")
		return
	}
	var system SystemConfig
	if !s.readCache("system", &system) {
		fmt.Fprint(w, "<p class=muted>No programming received.</p>")
		return
	}
	fmt.Fprint(w, "<h3>Radio directory</h3>")
	webTable(w, []webColumn{{"rid", "Radio ID"}, {"alias", "Name"}, {"kind", "Radio type"}, {"enabled", "Enabled"}}, system.Radios)
	fmt.Fprint(w, "<h3>Talkgroups</h3>")
	groups := make([]map[string]any, 0, len(system.Talkgroups))
	for _, group := range system.Talkgroups {
		groups = append(groups, map[string]any{"talkgroup_id": group.TGID, "name": group.Alias})
	}
	webTable(w, []webColumn{{"talkgroup_id", "Talkgroup ID"}, {"name", "Name"}}, groups)
	tree := s.adminTopology(s.webKnownZone())
	s.webAdminAddress(w, tree.SystemURL, "Open System Administration")
}

// Status views select their fields; future additions to a report are not exposed.
func webObservationRows(value any, keys []string) []map[string]any {
	rows, _ := value.([]any)
	out := make([]map[string]any, 0, len(rows))
	for _, row := range rows {
		record, _ := row.(map[string]any)
		projected := map[string]any{}
		for _, key := range keys {
			if value, ok := record[key]; ok {
				projected[key] = value
			}
		}
		out = append(out, projected)
	}
	return out
}

func (s *Service) webActivityStatus(w http.ResponseWriter, r *http.Request, session Session) {
	s.webHeader(w, r, "Activity Status", session, r.URL.Query().Get("live") == "1")
	defer webFooter(w)

	fmt.Fprint(w, "<p><a class=button href=\"\">Refresh</a> <a class=button href=/activity-status?live=1>Auto-refresh</a></p>")
	result := s.webAPI(r, session, "/api/activity", "GET", nil)
	if result.code >= 300 {
		webJSONResult(w, result)
		return
	}
	activity, err := webConfig(result.Bytes())
	if err != nil {
		webError(w, err)
		return
	}
	fmt.Fprint(w, "<h3>Presence</h3>")
	webTable(w, []webColumn{{"rid", "Radio ID"}, {"alias", "Name"}, {"registered", "Registered"}, {"affiliated_talkgroup", "Affiliated talkgroup"}, {"site_id", "Site"}, {"last_seen", "Last seen"}}, webObservationRows(activity["radios"], []string{"rid", "alias", "registered", "affiliated_talkgroup", "site_id", "last_seen"}))
	fmt.Fprint(w, "<h3>Calls</h3>")
	calls := webObservationRows(activity["calls"], []string{"id", "radio_id", "started_at", "ended_at", "source_kind", "lifetime_ms", "speech_ms"})
	webActivityList(w, "calls", calls, []webColumn{{"started_at", "Started"}, {"radio_id", "Radio ID"}, {"source_kind", "Source"}, {"lifetime_ms", "Call lifetime (seconds)"}, {"speech_ms", "Speech (seconds)"}})
	fmt.Fprint(w, "<h3>Events</h3>")
	events := webObservationRows(activity["events"], []string{"id", "at", "received_at", "origin", "site_id", "channel_id", "radio_id", "talkgroup_id", "call_id", "type", "label", "message", "outcome", "source_kind", "severity"})
	webActivityList(w, "events", events, []webColumn{{"at", "Time"}, {"label", "Event"}, {"radio_id", "Radio ID"}, {"site_id", "Site"}, {"outcome", "Result"}, {"message", "Message"}})
	selected, kind := r.URL.Query().Get("id"), r.URL.Query().Get("kind")
	if selected == "" || (kind != "calls" && kind != "events") {
		return
	}
	rows, columns := events, []webColumn{{"id", "Event ID"}, {"at", "Observed"}, {"received_at", "Received"}, {"origin", "Source service"}, {"site_id", "Site"}, {"channel_id", "Channel"}, {"radio_id", "Radio ID"}, {"talkgroup_id", "Talkgroup ID"}, {"call_id", "Call ID"}, {"type", "Event type"}, {"label", "Event"}, {"message", "Message"}, {"outcome", "Result"}, {"source_kind", "Source"}, {"severity", "Severity"}}
	if kind == "calls" {
		rows = calls
		columns = []webColumn{{"id", "Call ID"}, {"radio_id", "Radio ID"}, {"started_at", "Started"}, {"ended_at", "Ended"}, {"source_kind", "Source"}, {"lifetime_ms", "Call lifetime (seconds)"}, {"speech_ms", "Speech (seconds)"}}
	}
	fmt.Fprint(w, "<section class=record-detail><h3>Selected record</h3>")
	defer fmt.Fprint(w, "</section>")
	for _, row := range rows {
		if webText(row["id"]) != selected {
			continue
		}
		webProperties(w, row, columns)
		if kind == "calls" {
			for _, raw := range webObservationRows(activity["calls"], []string{"id", "destinations"}) {
				if webText(raw["id"]) == selected {
					fmt.Fprint(w, "<h4>Destinations</h4>")
					webTable(w, []webColumn{{"site_id", "Site"}, {"talkgroup_id", "Talkgroup ID"}, {"channel_id", "Channel"}, {"outcome", "Result"}, {"allocated_ms", "Allocated (seconds)"}, {"speech_ms", "Speech (seconds)"}}, webObservationRows(raw["destinations"], []string{"site_id", "talkgroup_id", "channel_id", "outcome", "allocated_ms", "speech_ms"}))
				}
			}
		}
		return
	}
	fmt.Fprint(w, "<p class=muted>Record unavailable.</p>")
}

func webActivityList(w io.Writer, kind string, rows []map[string]any, columns []webColumn) {
	fmt.Fprint(w, "<div class=table-wrap tabindex=0 aria-label=\"Scrollable records\"><table class=record-table><thead><tr>")
	for _, column := range columns {
		fmt.Fprintf(w, "<th>%s</th>", esc(column.Label))
	}
	fmt.Fprint(w, "<th>Details</th></tr></thead><tbody>")
	for _, row := range rows {
		fmt.Fprint(w, "<tr>")
		for _, column := range columns {
			class := "cell-text"
			if strings.HasSuffix(column.Key, "_at") || column.Key == "at" {
				class = "time"
			} else if strings.HasSuffix(column.Key, "_id") || column.Key == "rid" {
				class = "identity"
			}
			fmt.Fprintf(w, "<td class=%s>", class)
			webDataField(w, column.Key, row[column.Key])
			fmt.Fprint(w, "</td>")
		}
		fmt.Fprintf(w, "<td><a href=\"/activity-status?kind=%s&amp;id=%s\">Details</a></td></tr>", esc(kind), esc(url.QueryEscape(webText(row["id"]))))
	}
	if len(rows) == 0 {
		fmt.Fprintf(w, "<tr><td class=empty colspan=%d>No records.</td></tr>", len(columns)+1)
	}
	fmt.Fprint(w, "</tbody></table></div>")
}
