package framework

import (
	"fmt"
	"net/http"
	"sort"
	"strings"
)

// Alarm declarations remain planned; SMTP pages use their implemented handlers.
func (s *Service) webNotifications(w http.ResponseWriter, r *http.Request, session Session, path string) {
	if path == "smtp-settings" || path == "notification-actions" || path == "notification-delivery" {
		s.webSMTP(w, r, session, path)
		return
	}
	page, form := adminPlannedPages[path]
	if r.Method == http.MethodPost {
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		w.Header().Set("Cache-Control", "no-store")
		w.WriteHeader(http.StatusNotImplemented)
	}
	title := page.Title
	if !form {
		title = map[string]string{"alarms": "Alarms", "alarm-history": "Alarm history", "notification-delivery": "Notification delivery"}[path]
	}
	s.webHeader(w, r, title, session, false)
	defer webFooter(w)
	fmt.Fprint(w, `<div class=ts-notice role=status><span>Not implemented · Infrastructure alarm monitoring and rules</span><span class="ts-badge ts-amber">Planned</span></div>`)
	if !form {
		fmt.Fprint(w, `<div class=ts-empty><h2>No observations available</h2><p class=ts-muted>Infrastructure alarms and alarm-triggered notification rules are planned. SMTP profiles, email tests and delivery receipts are available.</p><a class=ts-link href=/planned>Feature coverage</a></div>`)
		return
	}
	if r.Method == http.MethodPost {
		fmt.Fprint(w, `<p class=ts-notice role=alert>Nothing was saved or sent. The infrastructure alarm and rule backend is not implemented.</p>`)
	}
	fields := append([]webField(nil), page.Fields...)
	record := map[string]any{}

	fmt.Fprint(w, `<div class=ts-editor><form method=post data-config-form`)
	if r.Method == http.MethodPost {
		fmt.Fprint(w, ` data-admin-unsaved`)
	}
	fmt.Fprint(w, `>`)
	webHidden(w, session.CSRF)
	var submitted = r.PostForm
	if r.Method != http.MethodPost {
		submitted = nil
	}
	s.adminEditorFields(w, path, fields, record, submitted)
	if page.Kind == "planned-operation" {
		fmt.Fprint(w, `<div class=ts-savebar><span class=ts-muted>Explicit operation · Planned</span><button class=ts-primary>Submit operation</button></div>`)
	} else {
		webSaveBar(w, "/"+path)
	}
	fmt.Fprint(w, `</form></div>`)
}

func (s *Service) webInstallation(w http.ResponseWriter, r *http.Request, session Session) {
	s.webHeader(w, r, "Host installation", session, false)
	defer webFooter(w)
	c := s.Config()
	fmt.Fprint(w, `<div class=ts-reference><span class=ts-badge>Host managed</span> <span class=ts-badge>Read only</span></div><section class=ts-panel>`)
	webProperties(w, map[string]any{"product": "Open25 FNE", "executable": "open25-fne", "instance": c.InstanceID, "role": c.Role, "listener": c.Listen, "state": c.StateDir, "web": c.PublicURL, "internal": c.InternalURL}, []webColumn{{"product", "Product"}, {"executable", "Executable"}, {"instance", "Instance"}, {"role", "Service role"}, {"listener", "Listener"}, {"state", "State directory"}, {"web", "Web administration address"}, {"internal", "Internal service address"}})
	fmt.Fprint(w, `</section>`)
}

func (s *Service) webFindSetting(w http.ResponseWriter, r *http.Request, session Session) {
	s.webHeader(w, r, "Find a setting", session, false)
	defer webFooter(w)
	query := strings.ToLower(strings.TrimSpace(r.URL.Query().Get("q")))
	fmt.Fprintf(w, `<form method=get class=ts-toolbar><input type=search name=q value="%s" aria-label="Find a setting"><button>Search</button></form>`, esc(r.URL.Query().Get("q")))
	if query == "" {
		fmt.Fprint(w, `<p class=ts-muted>Search setting names across the known administration instances.</p>`)
		return
	}
	fmt.Fprint(w, `<div class=ts-tablescroll><table><thead><tr><th>Setting</th><th>Page</th><th>Owner</th><th>Open</th></tr></thead><tbody>`)
	count := 0
	for _, instance := range s.adminInstances() {
		for _, group := range []string{"configuration", "notifications"} {
			for _, category := range adminCategories(instance.Role, group) {
				for _, link := range category.Links {
					names := []string{link.Label}
					for _, section := range adminPageSpecs[link.Path].Sections {
						names = append(names, section.Name)
					}
					for _, label := range adminPageSpecs[link.Path].Labels {
						names = append(names, label)
					}
					matches := []string{}
					for _, name := range names {
						if strings.Contains(strings.ToLower(name), query) {
							matches = append(matches, name)
						}
					}
					if len(matches) == 0 {
						continue
					}
					sort.Strings(matches)
					count++
					fmt.Fprintf(w, `<tr><td>%s</td><td>%s</td><td><span class=ts-badge>%s</span></td><td>`, esc(strings.Join(matches, ", ")), esc(link.Label), esc(instance.Name))
					if instance.URL != "" {
						outside := ""
						if !instance.Current {
							outside = adminPortalIcon()
						}
						fmt.Fprintf(w, `<a href="%s%s">Open%s</a>`, esc(instance.URL), esc(link.Path), outside)
					} else {
						fmt.Fprint(w, `Address unavailable`)
					}
					fmt.Fprint(w, `</td></tr>`)
				}
			}
		}
	}
	if count == 0 {
		fmt.Fprint(w, `<tr><td colspan=4>No matching settings.</td></tr>`)
	}
	fmt.Fprint(w, `</tbody></table></div>`)
}

// Public programming references arrive through this service's accepted backend view.
func (s *Service) webProgrammingReference(w http.ResponseWriter, kind string) {
	if s.Config().Role != RoleSite {
		return
	}
	zone := s.webKnownZone()
	system := s.view().System
	switch kind {
	case "channels":
		fmt.Fprint(w, `<section class=ts-reference><div class=ts-itemline><strong>Band Plan reference</strong><span class=ts-badge>Zone · Read only</span></div>`)
		if zone != nil {
			webTable(w, []webColumn{{"identifier", "Channel Identifier"}, {"base_frequency_hz", "Base (MHz)"}, {"channel_spacing_hz", "Spacing (kHz)"}, {"uplink_offset_hz", "Offset (MHz)"}}, zone.ChannelIdentifiers)
		} else {
			fmt.Fprint(w, `<p>Programming unavailable</p>`)
		}
		fmt.Fprint(w, `<a class=ts-link href=/reference-zone>Zone Band Plan</a></section>`)
	case "identification":
		fmt.Fprint(w, `<section class=ts-reference><div class=ts-itemline><strong>Shared identification</strong><span class=ts-badge>System · Read only</span></div>`)
		webProperties(w, map[string]any{"callsign": system.Callsign, "profile": system.BSIProfile}, []webColumn{{"callsign", "Callsign"}, {"profile", "BSI signaling profile"}})
		fmt.Fprint(w, `<a class=ts-link href=/reference-system>System identity</a></section>`)
	case "site-identity":
		fmt.Fprint(w, `<section class=ts-reference><div class=ts-itemline><strong>P25 Site identity</strong><span class=ts-badge>Zone · Read only</span></div>`)
		if zone != nil {
			for _, site := range zone.Sites {
				if site.ID == s.Config().InstanceID {
					webProperties(w, site, []webColumn{{"site_id", "P25 Site ID"}, {"nac", "NAC"}, {"lra", "Location Registration Area"}})
				}
			}
		}
		fmt.Fprint(w, `<a class=ts-link href=/reference-zone>Zone Site registry</a></section>`)
	}
}

func webFeatureCoverage(w http.ResponseWriter) {
	rows := []map[string]any{}
	for _, feature := range [][4]string{
		{"Radio identities and talkgroups", "System", "Implemented", "Names, participation, Site access and encryption rules"},
		{"Band Plan and Site registry", "Zone", "Implemented", "Channel numbering and P25 Site programming"},
		{"RF channels and USB calibration", "Site", "Implemented", "Local channel roles, frequencies and stable modem paths"},
		{"Operator profiles", "System", "Implemented", "Zones, selected groups and receive preferences"},
		{"Radio commands", "Zone / Site", "Implemented; individual RF outcomes require qualification", "Explicit requests with separate delivery and radio response results"},
		{"Channel start, stop and rotation", "Site", "Implemented", "Saved eligibility and current operating roles"},
		{"Traffic keys and enrollment", "System / Application", "Implemented", "Deliberate loading and public recipient identities"},
		{"In-call OTA alias", "System / Site", "Implemented; RF qualification separate", "Directory names sent through Phase 1 User Alias signaling"},
		{"Multiple Zones", "System", "Planned", "Reports and routing across several Zones"},
		{"Band Plan Subsets / standby", "Zone", "Planned", "Additional frequency-plan and controller arrangements"},
		{"Inbound OTA alias and contact download", "System", "Planned", "Received aliases and subscriber contact programming"},
		{"Map view", "Application", "Planned", "Received location observations shown on a map"},
		{"Expanded radio diagnostics", "Site", "Planned", "Additional radio measurements and diagnostic operations"},
		{"Infrastructure alarms", "All services", "Planned", "Conditions, severity, acknowledgment, recovery and history"},
		{"SMTP notifications", "Owning service / parent", "Implemented", "Saved relay profiles, parent-sent inheritance, explicit test/retry and SMTP acceptance receipts; alarm-triggered rules remain planned"},
		{"Private web messaging", "Application", "Implemented; RF text delivery planned", "Conversations shared between Dispatch and PTT"},
	} {
		rows = append(rows, map[string]any{"feature": feature[0], "owner": feature[1], "status": feature[2], "purpose": feature[3]})
	}
	webTable(w, []webColumn{{"feature", "Capability"}, {"owner", "Owner"}, {"status", "Implementation"}, {"purpose", "Purpose"}}, rows)
}
