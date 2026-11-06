package framework

import (
	"fmt"
	"net/http"
	"net/url"
)

func (s *Service) webSectionOverview(w http.ResponseWriter, r *http.Request, session Session, group string) {
	titles := map[string]string{"configuration": "Configuration", "operations": "Operations", "reference": "Reference Information"}
	s.webHeader(w, r, titles[group], session, false)
	defer webFooter(w)
	fmt.Fprint(w, "<div class=ts-overview>")
	for _, category := range adminCategories(s.Config().Role, group) {
		links := []adminLink{}
		for _, link := range category.Links {
			if link.Path != group {
				links = append(links, link)
			}
		}
		if len(links) == 0 {
			continue
		}
		fmt.Fprintf(w, "<section class=ts-panel><h3>%s</h3><ul class=ts-list>", esc(category.Label))
		for _, link := range links {
			fmt.Fprintf(w, "<li><a href=\"/%s\">%s</a></li>", esc(link.Path), esc(link.Label))
		}
		fmt.Fprint(w, "</ul></section>")
	}
	fmt.Fprint(w, "</div>")
}

func (s *Service) webCommandResults(w http.ResponseWriter, r *http.Request, session Session) {
	result := s.webAPI(r, session, "/api/radio/commands", "GET", nil)
	if result.code >= 300 {
		webJSONResult(w, result)
		return
	}
	listing, err := webConfig(result.Bytes())
	if err != nil {
		webError(w, err)
		return
	}
	rows, _ := listing["commands"].([]any)
	for _, raw := range rows {
		record, _ := raw.(map[string]any)
		record["_details"] = "/command-results?id=" + url.QueryEscape(webText(record["id"]))
	}
	webTable(w, []webColumn{{"created_at", "Created"}, {"radio_id", "Radio"}, {"action", "Command"}, {"site_id", "Site"}, {"state", "Result"}, {"_details", "Open"}}, rows)
	selected := r.URL.Query().Get("id")
	if selected != "" {
		found := false
		for _, raw := range rows {
			record, _ := raw.(map[string]any)
			if webText(record["id"]) != selected {
				continue
			}
			found = true
			fmt.Fprint(w, "<section class=record-detail><h3>Command details</h3>")
			webProperties(w, record, []webColumn{{"action", "Command"}, {"radio_id", "Radio ID"}, {"site_id", "Site"}, {"state", "Result"}, {"message", "Reported outcome"}, {"sent", "Sent by Site"}, {"awaiting_acknowledgment", "Awaiting radio response"}, {"created_at", "Created"}, {"attempted_at", "Attempted"}, {"sent_at", "Sent"}, {"response_at", "Radio response"}, {"expires_at", "Response deadline"}, {"response_kind", "Response type"}, {"response_code", "Response code"}, {"actor_username", "Administrator"}, {"id", "Record ID"}, {"response_event_id", "Response record ID"}})
			fmt.Fprint(w, "</section>")
			break
		}
		if !found {
			fmt.Fprint(w, "<p role=status>No matching command record.</p>")
		}
	}
	if sites, ok := listing["sites"].([]any); ok && len(sites) > 0 {
		fmt.Fprint(w, "<h3>Site results</h3>")
		webTable(w, []webColumn{{"site_id", "Site"}, {"state", "Record availability"}, {"message", "Reported outcome"}}, sites)
	}
}
