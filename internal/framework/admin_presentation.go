package framework

import (
	"fmt"
	"strings"
	"time"
)

func adminPortalIcon() string {
	return `<span class=ts-external role=img aria-label="Another administration portal">` + adminIcon("external-link") + `</span>`
}

// All administrator information uses the same hover/keyboard-focus component.
func webHelp(title, text string) string {
	return `<span class=ts-help tabindex=0 aria-label="` + esc(title) + `">` + adminIcon("info") + `<span class=ts-helpbody role=tooltip><strong>` + esc(strings.TrimPrefix(title, "About ")) + `</strong><span>` + esc(text) + `</span></span></span>`
}

func webTime(at time.Time) string {
	if at.IsZero() {
		return "—"
	}
	return fmt.Sprintf(`<time datetime="%s" title="%s">%s</time>`, esc(at.Format(time.RFC3339Nano)), esc(webReportTime(at)), esc(webTimeAgo(at, time.Now())))
}
func webTimeAgo(at, now time.Time) string {
	seconds := int64(now.Sub(at) / time.Second)
	future := seconds < 0
	if future {
		seconds = -seconds
	}
	if seconds < 60 {
		if future {
			return "in a moment"
		}
		return "just now"
	}
	count, unit := seconds/60, "minute"
	if seconds >= 86400 {
		count, unit = seconds/86400, "day"
	} else if seconds >= 3600 {
		count, unit = seconds/3600, "hour"
	}
	if count != 1 {
		unit += "s"
	}
	if future {
		return fmt.Sprintf("in %d %s", count, unit)
	}
	return fmt.Sprintf("%d %s ago", count, unit)
}

// Colour the actual named observation, never a programmed option or inferred RF quality.
func webStatus(text string) string {
	class := ""
	switch strings.ToLower(text) {
	case "connected", "running", "ready", "acknowledged":
		class = " ts-good"
	case "failed", "disconnected", "rejected", "timed out", "timed_out", "error", "fault", "not connected", "emergency":
		class = " ts-bad"
	case "stale", "pending", "connecting", "starting", "stopping", "cached settings", "recovering", "unconfirmed":
		class = " ts-amber"
	}
	return `<span class="ts-badge ts-status` + class + `">` + esc(text) + `</span>`
}

func webUSBChoices(inventory HardwareInventoryView) []adminChoice {
	choices := []adminChoice{}
	seen := map[string]bool{}
	for _, device := range inventory.Devices {
		if device.Kind != "usb" || !device.PresenceKnown || !device.Present {
			continue
		}
		for _, path := range append([]string{device.Device}, device.Paths...) {
			if !strings.HasPrefix(path, "/dev/serial/by-id/") || seen[path] {
				continue
			}
			seen[path] = true
			choices = append(choices, adminChoice{ID: path, Name: device.Product})
		}
	}
	return choices
}
