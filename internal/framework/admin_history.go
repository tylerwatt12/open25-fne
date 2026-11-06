package framework

import (
	"fmt"
	"io"
	"net/http"
	"sort"
)

func webHistoryResults(w io.Writer, r *http.Request, kind string, result *webResult) {
	if result.code >= 300 {
		webJSONResult(w, result)
		return
	}
	page, err := webConfig(result.Bytes())
	if err != nil {
		webError(w, err)
		return
	}
	if kind == "statistics" {
		webProperties(w, page, []webColumn{{"calls", "Calls"}, {"completed_calls", "Completed calls"}, {"site_deliveries", "Site deliveries"}, {"call_lifetime_ms", "Call lifetime (s)"}, {"speech_ms", "Speech (s)"}, {"events", "Events"}})
		counts, _ := page["events_by_type"].(map[string]any)
		keys := make([]string, 0, len(counts))
		for key := range counts {
			keys = append(keys, key)
		}
		sort.Strings(keys)
		rows := []map[string]any{}
		for _, key := range keys {
			rows = append(rows, map[string]any{"type": key, "count": counts[key]})
		}
		fmt.Fprint(w, "<h3>Events by type</h3>")
		webTable(w, []webColumn{{"type", "Event"}, {"count", "Count"}}, rows)
		return
	}
	columns := map[string][]webColumn{
		"events":    {{"at", "Time"}, {"radio_id", "Radio"}, {"site_id", "Site"}, {"label", "Event"}, {"outcome", "Result"}, {"_details", "Open"}},
		"calls":     {{"started_at", "Started"}, {"radio_id", "Radio"}, {"source_kind", "Source"}, {"lifetime_ms", "Lifetime (s)"}, {"speech_ms", "Speech (s)"}, {"_details", "Open"}},
		"locations": {{"fix_at", "Fix time"}, {"radio_id", "Radio"}, {"site_id", "Site"}, {"latitude", "Latitude (°)"}, {"longitude", "Longitude (°)"}, {"stale", "Stale"}, {"_details", "Open"}},
	}
	details := map[string][]webColumn{
		"events":    {{"label", "Event"}, {"message", "Reported outcome"}, {"outcome", "Result"}, {"at", "Observed"}, {"received_at", "Received"}, {"radio_id", "Radio ID"}, {"talkgroup_id", "Talkgroup ID"}, {"site_id", "Site"}, {"channel_id", "Channel"}, {"source_kind", "Source"}, {"origin", "Origin"}, {"severity", "Severity"}, {"call_id", "Call record ID"}, {"id", "Event record ID"}},
		"calls":     {{"radio_id", "Radio ID"}, {"started_at", "Started"}, {"ended_at", "Ended"}, {"source_kind", "Source"}, {"lifetime_ms", "Lifetime (s)"}, {"speech_ms", "Speech (s)"}, {"id", "Call record ID"}},
		"locations": {{"radio_id", "Radio ID"}, {"talkgroup_id", "Talkgroup ID"}, {"site_id", "Site"}, {"latitude", "Latitude (°)"}, {"longitude", "Longitude (°)"}, {"accuracy", "Accuracy (m)"}, {"fix_at", "Fix time"}, {"received_at", "Received"}, {"age_seconds", "Age (s)"}, {"stale", "Stale"}, {"source_kind", "Source"}, {"id", "Location record ID"}},
	}
	rows, _ := page[kind].([]any)
	for _, raw := range rows {
		record, _ := raw.(map[string]any)
		query := r.URL.Query()
		query.Set("id", webText(record["id"]))
		record["_details"] = "/history?" + query.Encode()
	}
	webTable(w, columns[kind], rows)
	selected := r.URL.Query().Get("id")
	if selected == "" {
		return
	}
	for _, raw := range rows {
		record, _ := raw.(map[string]any)
		if webText(record["id"]) != selected {
			continue
		}
		fmt.Fprint(w, "<section class=record-detail><h3>Record details</h3>")
		webProperties(w, record, details[kind])
		if kind == "calls" {
			fmt.Fprint(w, "<h3>Site deliveries</h3>")
			webTable(w, []webColumn{{"site_id", "Site"}, {"talkgroup_id", "Talkgroup"}, {"channel_id", "Channel"}, {"outcome", "Result"}, {"allocated_ms", "Allocated (s)"}, {"speech_ms", "Speech (s)"}}, record["destinations"])
		}
		fmt.Fprint(w, "</section>")
		return
	}
	fmt.Fprint(w, "<p role=status>No matching record on this page.</p>")
}
