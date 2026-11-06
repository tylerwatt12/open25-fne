package framework

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"strings"
	"time"
)

type webColumn struct{ Key, Label string }

// Each page chooses its columns. A new backend field never becomes a new column.
func webTable(w io.Writer, columns []webColumn, value any) {
	rows, _ := webPresentationValue(value).([]any)
	fmt.Fprint(w, "<div class=table-wrap tabindex=0 aria-label=\"Scrollable records\"><table class=record-table><thead><tr>")
	for _, column := range columns {
		fmt.Fprintf(w, "<th scope=col>%s</th>", esc(column.Label))
	}
	fmt.Fprint(w, "</tr></thead><tbody>")
	for _, row := range rows {
		record, _ := row.(map[string]any)
		fmt.Fprint(w, "<tr>")
		for _, column := range columns {
			class := "description"
			if strings.HasSuffix(column.Key, "_at") || column.Key == "at" {
				class = "time"
			} else if strings.HasSuffix(column.Key, "_id") || column.Key == "rid" || column.Key == "tgid" {
				class = "identity"
			}
			fmt.Fprintf(w, "<td class=%s>", class)
			webDataField(w, column.Key, webValue(record, column.Key))
			fmt.Fprint(w, "</td>")
		}
		fmt.Fprint(w, "</tr>")
	}
	if len(rows) == 0 {
		fmt.Fprintf(w, "<tr><td class=empty colspan=%d>No records.</td></tr>", len(columns))
	}
	fmt.Fprint(w, "</tbody></table></div>")
}

func webProperties(w io.Writer, value any, columns []webColumn) {
	record, _ := webPresentationValue(value).(map[string]any)
	fmt.Fprint(w, "<dl class=observation-list>")
	for _, column := range columns {
		fmt.Fprintf(w, "<div><dt>%s</dt><dd>", esc(column.Label))
		webDataField(w, column.Key, webValue(record, column.Key))
		fmt.Fprint(w, "</dd></div>")
	}
	fmt.Fprint(w, "</dl>")
}

func webPresentationValue(value any) any {
	raw, err := json.Marshal(value)
	if err != nil {
		return nil
	}
	var result any
	decoder := json.NewDecoder(bytes.NewReader(raw))
	decoder.UseNumber()
	_ = decoder.Decode(&result)
	return result
}

func webScalar(w io.Writer, key string, value any) {
	if value == nil || value == "" {
		fmt.Fprint(w, "—")
		return
	}
	if text, ok := value.(string); ok {
		if key == "_details" {
			fmt.Fprintf(w, "<a href=\"%s\">Details</a>", esc(text))
			return
		}
		if at, err := time.Parse(time.RFC3339Nano, text); err == nil {
			if at.IsZero() {
				fmt.Fprint(w, "—")
			} else {
				fmt.Fprint(w, webTime(at))
			}
			return
		}
		if key == "action" || key == "state" || key == "outcome" || key == "reported_state" {
			labels := map[string]string{"radio_check": "Radio check", "registration_demand": "Request registration", "affiliation_query": "Request affiliation", "status_query": "Request status", "page": "Call alert", "message": "Coded message", "inhibit": "Disable radio", "uninhibit": "Enable radio", "timed_out": "Timed out", "acknowledged": "Acknowledged", "sent": "Sent", "accepted": "Accepted", "rejected": "Rejected", "unconfirmed": "Unconfirmed", "running": "Running", "stopped": "Stopped", "failed": "Failed"}
			if label := labels[text]; label != "" {
				text = label
			}
		}
		switch key {
		case "state", "reported_state", "host_state", "outcome", "freshness":
			fmt.Fprint(w, webStatus(text))
		default:
			fmt.Fprint(w, esc(text))
		}
		return
	}
	if flag, ok := value.(bool); ok {
		if flag {
			if key == "ready" || key == "zone_connected" || key == "fne_ready" {
				label := "Ready"
				if key == "zone_connected" {
					label = "Connected"
				}
				fmt.Fprint(w, webStatus(label))
				return
			}
			fmt.Fprint(w, "Yes")
		} else {
			fmt.Fprint(w, "No")
		}
		return
	}
	fmt.Fprint(w, esc(value))
}
