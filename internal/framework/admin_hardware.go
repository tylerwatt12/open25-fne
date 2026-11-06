package framework

import (
	"fmt"
	"io"
	"net/http"
	"net/url"
	"path/filepath"
	"strings"

	"open25.local/open25-fne/internal/radio"
)

func (s *Service) webHardware(w http.ResponseWriter, r *http.Request, session Session) {
	s.webHeader(w, r, "Radio Hardware", session, false)
	defer webFooter(w)
	webHardwareInventory(w, r, s.hardwareInventory())
}

// The inventory page presents passive observations. Device discovery and
// ownership inspection never select, open, assign or start a radio resource.
func webHardwareInventory(w io.Writer, r *http.Request, inventory HardwareInventoryView) {
	fmt.Fprint(w, "<div class=report-meta>")
	webSourceTag(w, "Local host")

	fmt.Fprint(w, "<span>Observed ", webTime(inventory.ObservedAt), "</span>"+webHelp("About hardware discovery and ownership", "Lists USB identities and observed process use without opening a modem. Saved assignments show programming; process observations show actual use. Unknown or stale ownership needs a fresh observation.")+"</div><p><a class=button href=\"\">Refresh</a> <a class=button href=/modems>Modem assignments</a> <a class=button href=/radio-operation>Channel operation</a></p>")
	webProperties(w, inventory, []webColumn{{"hostname", "Host"}})
	if inventory.Error != "" {
		fmt.Fprintf(w, "<p class=notice role=status>%s</p>", esc(inventory.Error))
	}
	fmt.Fprint(w, "<div class=table-wrap tabindex=0 aria-label=\"Scrollable radio hardware\"><table class=record-table><thead><tr><th scope=col>Device identity</th><th scope=col>Presence</th><th scope=col>Saved Site / channel</th><th scope=col>Observed process use</th><th scope=col>Connection / recovery</th><th scope=col>Details</th></tr></thead><tbody>")
	for _, device := range inventory.Devices {
		name := device.Product
		if name == "" {
			name = filepath.Base(device.Device)
		}
		fmt.Fprintf(w, "<tr><td>%s", esc(name))
		if device.UID != "" {
			fmt.Fprintf(w, "<br><span class=nowrap>%s</span>", esc(device.UID))
		}
		presence := "Unknown"
		if device.PresenceKnown {
			presence = "Missing"
			if device.Present {
				presence = "Present"
			}
		}
		fmt.Fprintf(w, "</td><td>%s</td><td>", presence)
		if len(device.Assignments) == 0 {
			fmt.Fprint(w, "No assignment reported")
		}
		for i, assignment := range device.Assignments {
			if i > 0 {
				fmt.Fprint(w, "<br>")
			}
			channel := assignment.ChannelName
			if channel == "" {
				channel = assignment.ChannelID
			}
			if channel == "" {
				channel = "No channel"
			}
			fmt.Fprintf(w, "%s / %s ", esc(assignment.SiteID), esc(channel))
			webHardwareFreshness(w, assignment.Local, assignment.Stale)
		}
		fmt.Fprint(w, "</td><td>")
		if len(device.Owners) == 0 {
			if device.OwnershipKnown {
				fmt.Fprint(w, "No process observed")
			} else {
				fmt.Fprint(w, "Unknown")
			}
		}
		for i, owner := range device.Owners {
			if i > 0 {
				fmt.Fprint(w, "<br>")
			}
			fmt.Fprintf(w, "PID %d · %s", owner.PID, esc(owner.Name))
			if owner.SiteID != "" {
				fmt.Fprintf(w, "<br>%s / %s ", esc(owner.SiteID), esc(owner.ChannelID))
				webHardwareFreshness(w, owner.Local, owner.Stale)
			}
		}
		if !device.OwnershipKnown && len(device.Owners) > 0 {
			fmt.Fprint(w, "<br><span class=source-tag>Incomplete visibility</span>")
		}
		fmt.Fprint(w, "</td><td>")
		observations := device.Observations
		if len(observations) == 0 {
			// Saved rows may carry an actual stopped/unavailable Site status,
			// but never inherit a worker's state from another device binding.
			observations = device.Assignments
		}
		if len(observations) == 0 {
			fmt.Fprint(w, "No channel observation")
		}
		for i, assignment := range observations {
			if i > 0 {
				fmt.Fprint(w, "<br>")
			}
			state := assignment.State
			if state == "" {
				state = "Unknown"
			}
			fmt.Fprintf(w, "%s · %s", esc(assignment.SiteID), esc(state))
			if assignment.ChannelName != "" {
				fmt.Fprintf(w, " · %s", esc(assignment.ChannelName))
			}
			if assignment.Ready {
				fmt.Fprint(w, " · Ready")
			}
			if assignment.RecoveryWanted && assignment.State == "fault" {
				fmt.Fprint(w, " · Recovery requested")
			}
			if !assignment.RecoveryAt.IsZero() {
				fmt.Fprintf(w, "<br>Retry after %s", webTime(assignment.RecoveryAt))
			}
			if assignment.Error != "" {
				fmt.Fprintf(w, "<br>%s", esc(assignment.Error))
			}
			fmt.Fprint(w, " ")
			webHardwareFreshness(w, assignment.Local, assignment.Stale)
		}
		if device.Error != "" {
			fmt.Fprintf(w, "<br>%s", esc(device.Error))
		}
		fmt.Fprintf(w, "</td><td><a href=\"/hardware?device=%s\">Details</a></td></tr>", esc(url.QueryEscape(device.ID)))
	}
	if len(inventory.Devices) == 0 {
		fmt.Fprint(w, "<tr><td colspan=6 class=empty>No modem identities were discovered or configured.</td></tr>")
	}
	fmt.Fprint(w, "</tbody></table></div>")
	selected := r.URL.Query().Get("device")
	if selected == "" {
		return
	}
	for _, device := range inventory.Devices {
		if device.ID != selected {
			continue
		}
		fmt.Fprint(w, "<section class=record-detail><h3>Device details</h3>")
		webProperties(w, device, []webColumn{{"device", "Device path"}, {"identity_kind", "Identity kind"}, {"paths", "Identity paths"}, {"resolved_device", "Current device node"}, {"uid", "Hardware serial identity"}, {"kind", "Connection kind"}, {"product", "Product"}, {"vendor_id", "USB vendor ID"}, {"product_id", "USB product ID"}, {"presence_known", "Presence observed"}, {"present", "Present"}, {"ownership_known", "Complete process visibility"}, {"error", "Observation issue"}})
		fmt.Fprint(w, "<h3>Saved assignments</h3>")
		rows := make([]map[string]any, 0, len(device.Assignments))
		for _, assignment := range device.Assignments {
			row := webObject(assignment)
			row["freshness"] = webHardwareFreshnessText(assignment.Local, assignment.Stale)
			rows = append(rows, row)
		}
		webTable(w, []webColumn{{"site_id", "Site"}, {"modem_name", "Modem"}, {"channel_id", "Channel"}, {"role", "Configured role"}, {"enabled", "Enabled"}, {"freshness", "Source"}, {"observed_at", "Observed"}, {"received_at", "Received"}}, rows)
		fmt.Fprint(w, "<h3>Observed channels</h3>")
		observations := make([]map[string]any, 0, len(device.Observations))
		for _, observation := range device.Observations {
			row := webObject(observation)
			row["freshness"] = webHardwareFreshnessText(observation.Local, observation.Stale)
			observations = append(observations, row)
		}
		webTable(w, []webColumn{{"site_id", "Site"}, {"channel_id", "Channel"}, {"runtime_role", "Operating role"}, {"state", "State"}, {"ready", "Ready"}, {"recovery_wanted", "Recovery requested"}, {"recovery_at", "Retry after"}, {"error", "Last error"}, {"freshness", "Source"}, {"observed_at", "Observed"}, {"received_at", "Received"}}, observations)
		fmt.Fprint(w, "<h3>Observed process owners</h3>")
		owners := make([]map[string]any, 0, len(device.Owners))
		for _, owner := range device.Owners {
			row := webObject(owner)
			row["freshness"] = webHardwareFreshnessText(owner.Local, owner.Stale)
			owners = append(owners, row)
		}
		webTable(w, []webColumn{{"pid", "Process ID"}, {"name", "Process"}, {"site_id", "Site"}, {"channel_id", "Channel"}, {"role", "Role"}, {"freshness", "Source"}, {"observed_at", "Observed"}}, owners)
		fmt.Fprint(w, "</section>")
		return
	}
	fmt.Fprint(w, "<p role=status>The selected device is not in this observation. Refresh to observe the host again.</p>")
}

func webHardwareFreshnessText(local, stale bool) string {
	labels := []string{"Reported"}
	if local {
		labels[0] = "Local"
	}
	if stale {
		labels = append(labels, "Stale")
	}
	return strings.Join(labels, " · ")
}

func webHardwareFreshness(w io.Writer, local, stale bool) {
	webSourceTag(w, webHardwareFreshnessText(local, stale))
}

func webControlObservations(w io.Writer, status radio.Status) {
	fmt.Fprint(w, "<h3>Control channel</h3>")
	webProperties(w, status, []webColumn{{"active_control_channel_id", "Active channel"}, {"control_changed_at", "Control active since"}, {"next_control_rotation", "Next scheduled rotation"}, {"control_switch_target", "Requested replacement"}, {"control_switch_reason", "Switch status"}, {"control_switch_error", "Last switch error"}})
}

func (s *Service) webControlSwitchForm(w io.Writer, r *http.Request, session Session) {
	fmt.Fprint(w, "<section><div class=section-heading><h3>Switch control channel</h3>"+webHelp("About manual control switching", "Requests another enabled, control-eligible channel on this Site. The backend waits for calls and identification to finish, then switches the workers. Saved roles stay as programmed. Changed eligibility takes effect after Stop Site and Start Site.")+"</div>")
	fmt.Fprint(w, `<form method=post>`)
	webHidden(w, session.CSRF)
	s.adminEditorFields(w, "radio-operation", []webField{{Name: "target_channel_id", Label: "Target channel", Kind: "text"}}, map[string]any{"target_channel_id": r.FormValue("target_channel_id")})
	fmt.Fprint(w, `<div class=ts-actions><button name=action value=switch-control>Switch control</button></div></form></section>`)

}
