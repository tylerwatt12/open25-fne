package framework

import (
	"bytes"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"time"

	"open25.local/open25-fne/internal/radio"
)

// Channel labels are a request-scoped presentation of the existing radio owner.
// They never drive admission, recovery, switching, or transmit decisions.
func adminChannelStatus(site radio.Status, ch radio.ChannelStatus, now time.Time) (string, string) {
	switch ch.State {
	case "stopped":
		return "Offline", ""
	case "stopping":
		return "Stopping", "ts-amber"
	case "starting", "warming":
		return "Starting", "ts-amber"
	case "fault":
		if ch.RecoveryWanted {
			return "Recovering", "ts-amber"
		}
		return "Fault", "ts-bad"
	}
	if site.State == "stopped" && ch.PID == 0 {
		return "Offline", ""
	}
	if site.State != "running" || ch.State != "ready" || !ch.Ready {
		return "Status unavailable", "ts-amber"
	}
	sample, current := adminChannelSample(ch, now)
	if !current || (sample.TX == nil && sample.RX == nil) {
		return "Status unavailable", "ts-amber"
	}
	if ch.Role == "control" {
		return "Active (control)", "ts-good"
	}
	if ch.Role != "voice" {
		return "Status unavailable", "ts-amber"
	}
	if sample.TX != nil && *sample.TX {
		return "Active (voice, transmitting)", "ts-good"
	}
	if sample.RX != nil && *sample.RX {
		return "Active (voice, receiving)", "ts-good"
	}
	if ch.Busy {
		return "Active (voice, in use)", "ts-good"
	}
	if sample.TX == nil || sample.RX == nil {
		return "Active (voice, traffic unknown)", "ts-amber"
	}
	return "Active (voice, idle)", "ts-good"
}
func adminChannelSample(ch radio.ChannelStatus, now time.Time) (radio.ModemSample, bool) {
	samples := ch.Telemetry.Samples
	if len(samples) == 0 {
		return radio.ModemSample{}, false
	}
	sample := samples[len(samples)-1]
	return sample, !sample.At.IsZero() && !modemTimeStale(sample.At, now, 3*time.Second) && ch.Telemetry.StartedAt.Equal(ch.StartedAt)
}
func webChannelStatus(w io.Writer, site radio.Status, ch radio.ChannelStatus, now time.Time) {
	label, class := adminChannelStatus(site, ch, now)
	fmt.Fprintf(w, `<span class="ts-badge ts-status %s">%s</span>`, class, esc(label))
}

// Merge only missing programmed rows for display. Operating values always come
// from radio.Status; an absent worker is offline only when the owner says stopped.
func (s *Service) adminSiteChannels(status radio.Status) []radio.ChannelStatus {
	rows := append([]radio.ChannelStatus(nil), status.Channels...)
	var saved SiteConfig
	if s.owned(&saved) != nil {
		return rows
	}
	for _, ch := range saved.Channels {
		found := false
		for _, row := range rows {
			if row.ID == ch.ID {
				found = true
				break
			}
		}
		if found {
			continue
		}
		state := "unknown"
		if status.State == "stopped" {
			state = "stopped"
		}

		rows = append(rows, radio.ChannelStatus{ID: ch.ID, Name: ch.Name, ModemID: ch.ModemID, State: state})
	}
	return rows
}
func webSiteFacts(w io.Writer, rows [][2]string) {
	fmt.Fprint(w, `<dl class=wide-facts>`)
	for _, row := range rows {
		fmt.Fprintf(w, `<div><dt>%s</dt><dd>%s</dd></div>`, esc(row[0]), row[1])
	}
	fmt.Fprint(w, `</dl>`)
}
func webSitePanel(w io.Writer, name, action string) {
	fmt.Fprintf(w, `<section class=ts-panel><div class=ts-panelhead><h2>%s</h2>%s</div>`, esc(name), action)
}
func adminSiteLink(path, text string) string {
	return `<a class=ts-link href="` + esc(path) + `">` + esc(text) + `</a>`
}

func (s *Service) webSiteConnection(w io.Writer, status radio.Status, details bool) {
	link := adminSiteLink("/live-status", "Connection details")
	if details {
		link = adminSiteLink("/parent-zone", "Parent Zone operation")
	}
	webSitePanel(w, "Zone connection", link)
	state, contact, issue := "Status unavailable", "No successful contact", ""
	for _, row := range s.webConnectionRows() {
		state, issue = webText(row["reported_state"]), webText(row["error"])
		if at, ok := row["last_success"].(time.Time); ok && !at.IsZero() {
			contact = webTime(at)
		}
		if row["freshness"] == "Stale" {
			state = "Contact stale"
		}
	}
	mode := strings.ReplaceAll(status.TrunkingMode, "_", " ")
	if mode == "" {
		mode = "Unavailable"
	} else {
		mode = strings.ToUpper(mode[:1]) + mode[1:]
	}
	webSiteFacts(w, [][2]string{{"Parent Zone", webStatus(state)}, {"Trunking mode", esc(mode)}, {"Last Zone contact", contact}})
	if issue != "" {
		fmt.Fprintf(w, `<p class=channel-sub>%s</p>`, esc(issue))
	}
	fmt.Fprint(w, `</section>`)
}
func (s *Service) webSiteAttention(w io.Writer, status radio.Status) {
	issues := []string{}
	s.operationMu.Lock()
	if s.radioStartupCancel != nil {
		issue := "Automatic Site startup pending"
		if s.radioStartupError != "" {
			issue = "Automatic Site startup waiting to retry: " + s.radioStartupError
		}
		issues = append(issues, issue)
	}
	s.operationMu.Unlock()
	for _, row := range s.webConnectionRows() {
		if issue := webText(row["error"]); issue != "" {
			issues = append(issues, issue)
		} else if row["freshness"] == "Stale" || row["freshness"] == "No successful contact" {
			issues = append(issues, "Parent Zone: "+webText(row["freshness"]))
		}
	}
	if len(s.webConnectionRows()) == 0 {
		issues = append(issues, "Zone connection observations unavailable")
	}
	if status.Error != "" {
		issues = append(issues, status.Error)
	}
	for _, ch := range s.adminSiteChannels(status) {
		label, _ := adminChannelStatus(status, ch, time.Now())
		if label == "Fault" || label == "Recovering" || label == "Status unavailable" {
			issue := ch.Name + ": " + label
			if ch.Error != "" {
				issue += " — " + ch.Error
			}
			issues = append(issues, issue)
		}
	}
	class := "ts-panel summary-strip"
	if len(issues) > 0 {
		class += " ts-attention"
	}
	fmt.Fprintf(w, `<section class="%s"><h2>Needs attention</h2>`, class)
	if len(issues) == 0 {
		fmt.Fprint(w, `<span>No current faults in the available observations.</span>`)
	} else {
		fmt.Fprint(w, `<ul class=ts-list>`)
		for _, issue := range issues {
			fmt.Fprintf(w, `<li>%s</li>`, esc(issue))
		}
		fmt.Fprint(w, `</ul>`)
	}
	fmt.Fprint(w, `</section>`)
}
func (s *Service) webSiteChannelTable(w io.Writer, status radio.Status) {
	fmt.Fprint(w, `<div class=table-wrap tabindex=0 aria-label="RF channels"><table class="record-table channel-table"><thead><tr><th scope=col>Channel / modem</th><th scope=col>Channel status</th><th scope=col>Frequency (MHz)</th><th scope=col>Current traffic / issue</th></tr></thead><tbody>`)
	now := time.Now()
	rows := s.adminSiteChannels(status)
	for _, ch := range rows {
		fmt.Fprintf(w, `<tr><td><a class="ts-link channel-name" href="/live-status?channel=%s">%s</a><span class=channel-sub>%s</span></td><td>`, esc(url.QueryEscape(ch.ID)), esc(ch.Name), esc(ch.OperatingModemName))
		webChannelStatus(w, status, ch, now)
		fmt.Fprint(w, `</td><td><div class=frequency-stack>`)
		if ch.TXHz != 0 || ch.RXHz != 0 {
			fmt.Fprintf(w, `<span><b>TX</b><span class=ts-numeric>%s</span></span><span><b>RX</b><span class=ts-numeric>%s</span></span>`, esc(webUnitText("tx_frequency_hz", ch.TXHz)), esc(webUnitText("rx_frequency_hz", ch.RXHz)))
		} else {
			fmt.Fprint(w, `Operating frequencies unavailable`)
		}
		fmt.Fprint(w, `</div></td><td>`)
		webSiteChannelTraffic(w, status, ch, now)
		fmt.Fprint(w, `</td></tr>`)
	}
	if len(rows) == 0 {
		fmt.Fprint(w, `<tr><td colspan=4>No programmed channels or channel observations.</td></tr>`)
	}
	fmt.Fprint(w, `</tbody></table></div>`)
}
func webSiteChannelTraffic(w io.Writer, status radio.Status, ch radio.ChannelStatus, now time.Time) {
	label, _ := adminChannelStatus(status, ch, now)
	sample, current := adminChannelSample(ch, now)
	switch {
	case ch.Error != "":
		fmt.Fprint(w, esc(ch.Error))
	case label == "Offline":
		fmt.Fprint(w, `Channel stopped`)
	case label == "Starting" || label == "Stopping" || label == "Recovering":
		fmt.Fprint(w, esc(label))
		if !ch.RecoveryAt.IsZero() {
			fmt.Fprint(w, ` · Retry `, webTime(ch.RecoveryAt))
		}
	case label == "Status unavailable":
		fmt.Fprint(w, `Current traffic unconfirmed`)
		if !sample.At.IsZero() {
			fmt.Fprint(w, `<span class=channel-sub>Last observation `, webTime(sample.At), `</span>`)
		}
	case ch.RID != 0 || ch.TGID != 0:
		if ch.RID != 0 {
			fmt.Fprint(w, `Radio `)
			webDataField(w, "radio_id", ch.RID)
		}
		if ch.TGID != 0 {
			fmt.Fprint(w, `<span class=channel-sub>Talkgroup `)
			webDataField(w, "talkgroup_id", ch.TGID)
			fmt.Fprint(w, `</span>`)
		}
	case ch.Role == "control" && current && sample.TX != nil && *sample.TX:
		fmt.Fprint(w, `Control signaling`)
	case label == "Active (voice, idle)":
		fmt.Fprint(w, `No call`)
	default:
		fmt.Fprint(w, `No call identity reported`)
	}
}

func (s *Service) webSiteDashboard(w http.ResponseWriter, r *http.Request, session Session) {
	s.webHeader(w, r, "Overview", session, false)
	defer webFooter(w)
	status := s.radioStatus()
	s.webSiteAttention(w, status)
	s.webTopologyMap(w)
	s.webSiteConnection(w, status, false)
	webSitePanel(w, "RF channels", adminSiteLink("/live-status", "Site status"))
	s.webSiteChannelTable(w, status)
	fmt.Fprint(w, `</section>`)
	fmt.Fprint(w, `<div class=ts-overview>`)
	webSitePanel(w, "Calls and emergencies", "")
	incidents := []map[string]any{}
	for _, e := range s.operatorSnapshot().Emergencies {
		if e.ClearedAt.IsZero() {
			incidents = append(incidents, map[string]any{"radio_id": e.RadioID, "talkgroup_id": e.TalkgroupID, "last_seen": e.LastSeen})
		}
	}
	if len(incidents) > 0 {
		s.webTable(w, []webColumn{{"radio_id", "Radio ID"}, {"talkgroup_id", "Talkgroup"}, {"last_seen", "Last seen"}}, incidents)
	} else {
		fmt.Fprint(w, `<p>No active radio emergency in the stored observations.</p>`)
	}
	fmt.Fprint(w, `<p class=channel-sub>Current call-allocation count unavailable at this Site.</p></section>`)
	webSitePanel(w, "Recent diagnostics", "")
	events, err := s.Store.Events(6)
	if err != nil {
		fmt.Fprint(w, `<p>Activity records unavailable.</p>`)
	} else if len(events) == 0 {
		fmt.Fprint(w, `<p>No recorded outcomes.</p>`)
	} else {
		s.webTable(w, []webColumn{{"at", "Time"}, {"kind", "Event"}, {"message", "Message"}}, events)
	}

	fmt.Fprint(w, `<div class=ts-actions>`, adminSiteLink("/command-results", "Command results"), adminSiteLink("/notification-delivery", "Email delivery"), `</div></section></div>`)
	s.webHistoryDelivery(w)
	webSitePanel(w, "Configuration", "")
	fmt.Fprint(w, `<div class=ts-actions>`)
	for _, item := range []adminLink{{"channels", "RF channels"}, {"modems", "Modem setup"}, {"control-policy", "Control channel behavior"}} {
		fmt.Fprintf(w, `<a class=ts-button href="/%s">%s</a>`, item.Path, item.Label)
	}
	fmt.Fprint(w, `</div></section>`)
}
func (s *Service) webSiteStatus(w http.ResponseWriter, r *http.Request, session Session) {
	status := s.radioStatus()
	selected := r.URL.Query().Get("channel")
	if selected != "" {
		s.webSiteChannelDetail(w, r, session, status, selected)
		return
	}
	s.webHeader(w, r, "Site status", session, true)
	defer webFooter(w)
	s.webSiteAttention(w, status)
	s.webSiteConnection(w, status, true)
	webSitePanel(w, "RF channels", "")
	s.webSiteChannelTable(w, status)
	fmt.Fprint(w, `</section>`)
	webSitePanel(w, "Control channel", adminSiteLink("/radio-operation", "Channel operation"))
	webControlObservations(w, status, false)
	fmt.Fprint(w, `</section>`)
	webSitePanel(w, "Registered radios", "")
	if len(status.Registrations) == 0 {
		fmt.Fprint(w, `<p>No radios have registered in the current Site observations.</p>`)
	} else {
		s.webTable(w, []webColumn{{"rid", "Radio ID"}, {"at", "Registered"}}, status.Registrations)
	}
	fmt.Fprint(w, `</section>`)
	webSitePanel(w, "Talkgroup affiliations", "")
	if len(status.Affiliations) == 0 {
		fmt.Fprint(w, `<p>No subscriber talkgroup affiliations in the current Site observations.</p>`)
	} else {
		s.webTable(w, []webColumn{{"rid", "Radio ID"}, {"tgid", "Talkgroup ID"}, {"at", "Affiliated"}}, status.Affiliations)
	}
	fmt.Fprint(w, `</section>`)
}
func (s *Service) webSiteChannelDetail(w http.ResponseWriter, r *http.Request, session Session, status radio.Status, id string) {
	s.webHeader(w, r, "Channel details", session, false)
	defer webFooter(w)
	fmt.Fprint(w, `<div class=section-links>`, adminSiteLink("/live-status", "Back to Site status"), `</div>`)
	for _, ch := range s.adminSiteChannels(status) {
		if ch.ID != id {
			continue
		}
		edit := ""
		var saved SiteConfig
		if s.owned(&saved) == nil {
			for i, c := range saved.Channels {
				if c.ID == id {
					edit = adminSiteLink(fmt.Sprintf("/channels/edit?row=%d", i), "Channel setup")
					break
				}
			}
		}
		webSitePanel(w, ch.Name, edit)
		fmt.Fprint(w, `<div class=status-line>`)
		webChannelStatus(w, status, ch, time.Now())
		fmt.Fprint(w, `<span>`, esc(ch.OperatingModemName), `</span></div><div class=detail-section>`)
		tx, rx := "Unavailable", "Unavailable"
		if ch.TXHz != 0 || ch.RXHz != 0 {
			tx = esc(webUnitText("tx_frequency_hz", ch.TXHz))
			rx = esc(webUnitText("rx_frequency_hz", ch.RXHz))
		}
		var traffic bytes.Buffer
		webSiteChannelTraffic(&traffic, status, ch, time.Now())
		webSiteFacts(w, [][2]string{{"Site TX (MHz)", `<span class=ts-numeric>` + tx + `</span>`}, {"Site RX (MHz)", `<span class=ts-numeric>` + rx + `</span>`}, {"Current traffic", traffic.String()}})
		fmt.Fprint(w, `</div><div class=detail-section>`)
		sample, current := adminChannelSample(ch, time.Now())
		txState, rxState := "Unavailable", "Unavailable"
		if current {
			if sample.TX != nil {
				txState = "Idle"
				if *sample.TX {
					txState = "Transmitting"
				}
			}
			if sample.RX != nil {
				rxState = sample.RXState
			}
		}
		rows := [][2]string{{"Last observed", webTime(sample.At)}, {"Transmit indication", esc(txState)}, {"Receive decoder", esc(rxState)}}
		if !ch.Telemetry.StartedAt.IsZero() {
			rows = append(rows, [2]string{"RX / TX buffer overflows", fmt.Sprintf("%d / %d", ch.Telemetry.RXOverflows, ch.Telemetry.TXOverflows)})
		}
		webSiteFacts(w, rows)
		fmt.Fprint(w, `</div><details class=ts-advanced><summary>`, adminIcon("chevron-down"), `Technical observations</summary>`)
		s.webProperties(w, ch, []webColumn{{"pid", "Native process ID"}, {"state", "Native process state"}, {"ready", "Modem available"}, {"started_at", "Worker started"}, {"recovery_wanted", "Recovery requested"}, {"recovery_at", "Retry scheduled for"}, {"error", "Last error"}, {"device", "USB device identity"}})
		fmt.Fprint(w, `</details></section>`)
		return
	}
	fmt.Fprint(w, `<p role=status>No matching channel in the current Site observations.</p>`)
}

func (s *Service) webSiteOperation(w http.ResponseWriter, r *http.Request, session Session) {
	var result *webResult
	if r.Method == http.MethodPost {
		action := r.FormValue("action")
		switch action {
		case "start", "stop":
			result = s.webAPI(r, session, "/api/radio/"+action, http.MethodPost, map[string]string{"channel_id": r.FormValue("channel_id")})
		case "switch-control":
			result = s.webAPI(r, session, "/api/radio/switch-control", http.MethodPost, map[string]string{"channel_id": r.FormValue("target_channel_id")})
		default:
			http.Error(w, "Unknown channel operation", http.StatusBadRequest)
			return
		}
		if result.code < 300 {
			http.Redirect(w, r, "/radio-operation", http.StatusSeeOther)
			return
		}
	}
	s.webHeader(w, r, "Channel operation", session, false)
	defer webFooter(w)
	if result != nil {
		webJSONResult(w, result)
	}
	status := s.radioStatus()
	webSitePanel(w, "Site operation", "")
	webSiteFacts(w, [][2]string{{"Site operation", webStatus(status.State)}, {"Operating mode", esc(status.Mode)}, {"Last error", esc(status.Error)}})
	s.webRFConfigurationNotice(w)
	fmt.Fprint(w, `<form method=post class="ts-actions detail-section">`)
	webHidden(w, session.CSRF)
	fmt.Fprint(w, `<input type=hidden name=channel_id value=""><button name=action value=start>Start Site</button><button class=ts-bad-action name=action value=stop>Stop Site</button></form></section>`)
	webSitePanel(w, "Individual channels", "")
	fmt.Fprint(w, `<div class=table-wrap tabindex=0 aria-label="Individual channel operations"><table class=record-table><thead><tr><th scope=col>Channel</th><th scope=col>Channel status</th><th scope=col>Actions</th></tr></thead><tbody>`)
	for _, ch := range s.adminSiteChannels(status) {
		fmt.Fprint(w, `<tr><td>`, esc(ch.Name), `</td><td>`)
		webChannelStatus(w, status, ch, time.Now())
		fmt.Fprint(w, `</td><td><form method=post class=ts-actions>`)
		webHidden(w, session.CSRF)
		fmt.Fprintf(w, `<input type=hidden name=channel_id value="%s"><button name=action value=start>Start channel</button><button name=action value=stop>Stop channel</button></form></td></tr>`, esc(ch.ID))
	}
	fmt.Fprint(w, `</tbody></table></div></section>`)
	webSitePanel(w, "Switch control channel", "")
	webControlObservations(w, status, false)
	s.webControlSwitchForm(w, r, session)
	fmt.Fprint(w, `</section>`)
}
