package framework

import (
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strconv"
)

// This page submits explicit operations. Refresh only reads the current test;
// neither the form values nor the page's lifetime own a transmission.
func (s *Service) webC4FMTest(w http.ResponseWriter, r *http.Request, session Session) {
	var result *webResult
	var inputError error
	if r.Method == http.MethodPost {
		switch r.FormValue("action") {
		case "start":
			duration, err := strconv.Atoi(r.FormValue("duration_seconds"))
			if err != nil {
				inputError = fmt.Errorf("could not read duration as a whole number of seconds")
			} else {
				result = s.webAPI(r, session, "/api/radio/c4fm-test/start", http.MethodPost, map[string]any{"channel_id": r.FormValue("channel_id"), "duration_seconds": duration})
			}
		case "stop":
			result = s.webAPI(r, session, "/api/radio/c4fm-test/stop", http.MethodPost, map[string]any{})
		default:
			inputError = fmt.Errorf("choose Start test or Stop test")
		}
		if result != nil && result.code >= 200 && result.code < 300 {
			// Redirect so a reload cannot repeat an accepted Start or Stop.
			query := url.Values{}
			for _, field := range []string{"channel_id", "duration_seconds"} {
				if value := r.FormValue(field); value != "" {
					query.Set(field, value)
				}
			}
			destination := "/c4fm-test"
			if len(query) > 0 {
				destination += "?" + query.Encode()
			}
			http.Redirect(w, r, destination, http.StatusSeeOther)
			return
		}
	}
	s.webHeader(w, r, "C4FM Transmit Test", session, false)
	defer webFooter(w)
	if inputError != nil {
		webError(w, inputError)
	}
	if result != nil {
		webJSONResult(w, result)
	}
	fmt.Fprint(w, "<div class=report-meta>")
	webSourceTag(w, "Local")
	fmt.Fprint(w, webHelp("About the C4FM transmit test", "Sends a temporary Phase 1 C4FM test signal with the selected channel’s RF settings. The backend controls its duration. Check RF output and modulation with external receiving or test equipment.")+"</div>")
	fmt.Fprint(w, "<p><a class=button href=\"/c4fm-test\">Refresh status</a> <a href=/radio-operation>Channel operation</a></p><h3>Test status</h3>")
	statusResult := s.webAPI(r, session, "/api/radio/c4fm-test", http.MethodGet, nil)
	if statusResult.code >= 300 {
		webJSONResult(w, statusResult)
	} else if status, err := webConfig(statusResult.Bytes()); err != nil {
		webError(w, err)
	} else {
		webC4FMTestStatus(w, status)
	}
	var site SiteConfig
	if err := s.owned(&site); err != nil {
		webError(w, err)
		return
	}
	webC4FMTestForms(w, r, session, site.Channels)
}

func webC4FMTestStatus(w io.Writer, status map[string]any) {
	columns := []webColumn{{"state", "Test state"}}
	if webText(status["channel_id"]) != "" {
		columns = append(columns, []webColumn{
			{"channel_name", "Channel"}, {"channel_id", "Channel record"}, {"modem_id", "Modem record"},
			{"tx_frequency_hz", "Configured transmit frequency (MHz)"}, {"power_percent", "Configured power (%)"},
			{"pattern", "Test pattern"}, {"duration_seconds", "Requested duration (seconds)"},
			{"started_at", "TX observed at"}, {"ends_at", "Expected end"}, {"finished_at", "Finished"},
			{"tx_observed", "Modem TX observed"}, {"stop_confirmed", "Modem stop confirmed"},
		}...)
	}
	columns = append(columns, webColumn{"error", "Last error"})
	webProperties(w, status, columns)
}

func webC4FMTestForms(w io.Writer, r *http.Request, session Session, channels []Channel) {
	selected := r.FormValue("channel_id")
	fmt.Fprint(w, "<section class=ts-editor><h3>Start a test</h3><form method=post action=/c4fm-test>")
	webHidden(w, session.CSRF)
	fmt.Fprint(w, `<div class=ts-formgrid><div class=ts-field data-field=channel_id><div class=ts-fieldlabel><label for=f-c4fm-channel>Transmitter channel</label>`+webHelp("About transmitter selection", "Uses the selected channel’s saved modem and RF settings. Start attempts the operation and reports its result.")+`</div><div class=ts-fieldbody><div class=ts-relation>`)
	fmt.Fprintf(w, `<input id=f-c4fm-channel name=channel_id value="%s">`, esc(selected))
	webChooserButton(w, "channels", "channel_id", false)
	fmt.Fprint(w, `</div></div></div><div class=ts-field data-field=duration_seconds><div class=ts-fieldlabel><label for=f-c4fm-duration>Duration (seconds)</label>`+webHelp("About test duration", "Enter the requested whole number of seconds. The backend ends the test when its duration expires; Stop test requests an earlier end.")+`</div><div class=ts-fieldbody>`)

	fmt.Fprintf(w, "<input id=f-c4fm-duration type=number name=duration_seconds value=\"%s\" step=1></div></div></div>", esc(r.FormValue("duration_seconds")))
	fmt.Fprint(w, "<div class=ts-actions><button class=ts-primary name=action value=start>Start test</button></div></form></section><section><h3>Stop the test</h3><form method=post action=/c4fm-test>")
	webHidden(w, session.CSRF)
	fmt.Fprint(w, "<button name=action value=stop>Stop test</button></form></section>")
}
