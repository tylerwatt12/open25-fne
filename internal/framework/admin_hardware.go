package framework

import (
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"path/filepath"
	"time"

	"open25.local/open25-fne/internal/radio"
)

func (s *Service) webHardware(w http.ResponseWriter, r *http.Request, session Session) {
	if r.Method != http.MethodGet {
		w.WriteHeader(http.StatusMethodNotAllowed)
		return
	}
	w.Header().Set("Cache-Control", "no-store")
	var site SiteConfig
	if err := s.owned(&site); err != nil {
		http.Error(w, "Saved modem assignments unavailable", http.StatusInternalServerError)
		return
	}
	now := time.Now().UTC()
	status := s.radioStatus()
	report := SiteReport{Config: site, Radio: &status, ObservedAt: now}
	zone := s.webKnownZone()
	if r.URL.Query().Get("fragment") == "activity" {
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		webHardwareActivity(w, report, zone, now, r.URL.Query().Get("detail"), r.URL.Query().Get("chart"))
		return
	}
	inventory := s.hardwareInventory()
	if r.URL.Query().Get("fragment") == "inventory" {
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		webHardwareInventory(w, inventory, report, zone, now)
		return
	}
	s.webHardwarePage(w, r, session, inventory, report, zone, now)
}

func (s *Service) webHardwarePage(w http.ResponseWriter, r *http.Request, session Session, inventory HardwareInventoryView, report SiteReport, zone *ZoneConfig, now time.Time) {
	s.webHeader(w, r, "Hardware & activity", session, false)
	defer webFooter(w)
	fmt.Fprint(w, `<script src=/hardware-activity.js defer></script><div class="ts-toolbar hardware-controls"><span class=ts-caption role=status data-hardware-update>Activity updated just now</span><div class=ts-actions><button type=button data-hardware-pause hidden>Pause activity</button><a class=ts-button href=/hardware data-hardware-refresh>`+adminIcon("refresh-cw")+`Refresh hardware</a></div></div><p class=ts-caption role=status data-hardware-refreshed hidden></p>`)
	webHardwareInventory(w, inventory, report, zone, now)
	webHardwareMeasurements(w)
}

// This is a passive view of the existing inventory. Allocation is shown for the
// serving Site; opening or refreshing this page never assigns or opens hardware.
func webHardwareInventory(w io.Writer, inventory HardwareInventoryView, report SiteReport, zone *ZoneConfig, now time.Time) {
	fmt.Fprint(w, `<div class="table-wrap hardware-inventory" data-hardware-inventory><table class="record-table hardware-table"><thead><tr><th scope=col>Device / modem</th><th scope=col>Serial device path</th><th scope=col>Hardware status</th><th scope=col>Use</th><th scope=col>Activity</th></tr></thead><tbody>`)
	rows := modemRows(report, zone)
	for index, device := range inventory.Devices {
		name := device.Product
		if name == "" {
			name = filepath.Base(device.Device)
		}
		if name == "." {
			name = "Serial device"
		}
		var matches []modemRow
		for _, row := range rows {
			if hardwarePathMatches(device.HardwareDevice, row.Device) {
				matches = append(matches, row)
			}
		}
		if len(matches) > 0 {
			name = matches[0].Name
		} else if len(device.Assignments) > 0 && device.Assignments[0].ModemName != "" {
			name = device.Assignments[0].ModemName
		}
		aliases, _ := json.Marshal(append([]string{device.Device}, device.Paths...))
		activityPath := ""
		if len(matches) > 0 {
			activityPath = matches[0].Device
		}
		fmt.Fprintf(w, `<tr class=hardware-device data-hardware-paths="%s" data-activity-path="%s"><th scope=row><span data-hardware-name>%s</span>`, esc(string(aliases)), esc(activityPath), esc(name))
		if device.UID != "" {
			fmt.Fprint(w, `<span class="channel-sub ts-numeric">`, esc(device.UID), `</span>`)
		}
		presence, node := "Unavailable", "—"
		if device.PresenceKnown && device.Present {
			presence = "Present"
			if device.ResolvedDevice != "" {
				node = device.ResolvedDevice
			}
		}
		local, elsewhere := false, false
		for _, allocation := range device.Assignments {
			local = local || allocation.Local
			elsewhere = elsewhere || !allocation.Local
		}
		// A running owner still occupies its device after its saved assignment
		// changes. Keep that existing observation without exposing process details.
		for _, owner := range device.Owners {
			local = local || owner.Local
			elsewhere = elsewhere || !owner.Local
		}
		local = local || len(matches) > 0
		use := "Unallocated"
		if local {
			use = "Allocated"
		} else if elsewhere {
			use = "Used elsewhere"
		}
		fmt.Fprintf(w, `</th><td><span class=hardware-mobile-label>Serial device path</span><span class=ts-numeric>%s</span></td><td><span class=hardware-mobile-label>Hardware status</span>%s</td><td><span class=hardware-mobile-label>Use</span>%s`, esc(node), webStatus(presence), esc(use))
		if !local && elsewhere {
			for _, a := range device.Assignments {
				if !a.Local && a.SiteName != "" {
					fmt.Fprint(w, `<span class=channel-sub>`, esc(a.SiteName), `</span>`)
					break
				}
			}
		}
		fmt.Fprint(w, `</td><td><span class=hardware-mobile-label>Activity</span>`)
		if local || len(matches) > 0 {
			fmt.Fprintf(w, `<button type=button class="ts-link hardware-activity-toggle" data-hardware-toggle aria-expanded=false aria-controls="hardware-detail-%d">%s<span data-hardware-live-status>`, index, adminIcon("chevron-down"))
			webHardwareStatuses(w, report, matches, now)
			fmt.Fprintf(w, `</span></button></td></tr><tr class=hardware-detail-row id="hardware-detail-%d" hidden><td colspan=5></td></tr>`, index)
		} else {
			fmt.Fprint(w, `<span class=ts-caption>—</span></td></tr>`)
		}
	}
	if len(inventory.Devices) == 0 {
		message := "No hardware devices found."
		if inventory.Error != "" {
			message = "Hardware inventory unavailable."
		}
		fmt.Fprintf(w, `<tr><td colspan=5>%s</td></tr>`, message)
	}
	fmt.Fprint(w, `</tbody></table></div>`)
}

func webControlObservations(w io.Writer, status radio.Status, heading ...bool) {
	if len(heading) == 0 || heading[0] {
		fmt.Fprint(w, "<h3>Control channel</h3>")
	}
	view := webPresentationValue(status).(map[string]any)
	for _, key := range []string{"active_control_channel_id", "control_switch_target"} {
		id := webText(view[key])
		if id == "" {
			continue
		}
		view[key] = "Unavailable channel"
		for _, ch := range status.Channels {
			if ch.ID == id {
				view[key] = ch.Name
			}
		}
	}
	webProperties(w, view, []webColumn{{"active_control_channel_id", "Active channel"}, {"control_changed_at", "Control active since"}, {"next_control_rotation", "Next scheduled rotation"}, {"control_switch_target", "Requested replacement"}, {"control_switch_reason", "Switch status"}, {"control_switch_error", "Last switch error"}})
}

func (s *Service) webControlSwitchForm(w io.Writer, r *http.Request, session Session) {
	fmt.Fprint(w, "<section class=detail-section><div class=section-heading>"+webHelp("About manual control switching", "Requests another enabled channel from this Site’s operating control pool. The backend waits for calls and identification to finish, then switches the workers. Saved programming is unchanged. Pool edits take effect after Stop Site and Start Site.")+"</div>")
	fmt.Fprint(w, `<form method=post>`)
	webHidden(w, session.CSRF)
	s.adminEditorFields(w, "radio-operation", []webField{{Name: "target_channel_id", Label: "Target channel", Kind: "text"}}, map[string]any{"target_channel_id": r.FormValue("target_channel_id")})
	fmt.Fprint(w, `<div class=ts-actions><button name=action value=switch-control>Switch control channel</button></div></form></section>`)

}
