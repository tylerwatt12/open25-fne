package framework

import (
	"fmt"
	"io"
	"strings"
	"time"

	"open25.local/open25-fne/internal/radio"
)

func webHardwareStatuses(w io.Writer, report SiteReport, rows []modemRow, now time.Time) {
	labels := map[string]bool{}
	for _, row := range rows {
		label := "Status unavailable"
		if report.Radio != nil {
			label, _ = adminChannelStatus(*report.Radio, row.ChannelStatus, now)
		}
		if labels[label] {
			continue
		}
		labels[label] = true
		if len(labels) > 1 {
			fmt.Fprint(w, `<br>`)
		}
		at := ""
		if strings.HasPrefix(label, "Active (") {
			sample, _ := adminChannelSample(row.ChannelStatus, now)
			at = sample.At.Format(time.RFC3339Nano)
		}
		fmt.Fprintf(w, `<span data-hardware-current="%s">%s</span>`, esc(at), esc(label))
	}
	if len(rows) == 0 {
		fmt.Fprint(w, `<span data-hardware-current="">Status unavailable</span>`)
	}
}

// Live requests read the existing radio snapshot and saved bindings. They never
// enumerate hardware, inspect process descriptors, or perform a radio operation.
func webHardwareActivity(w io.Writer, report SiteReport, zone *ZoneConfig, now time.Time, detail, chart string) {
	fmt.Fprint(w, `<div data-hardware-updates>`)
	for _, row := range modemRows(report, zone) {
		if row.Device == "" {
			continue
		}
		fmt.Fprintf(w, `<template data-hardware-path="%s"><span data-hardware-name>%s</span><span data-hardware-live-status>`, esc(row.Device), esc(row.Name))
		webHardwareStatuses(w, report, []modemRow{row}, now)
		fmt.Fprint(w, `</span>`)
		if detail == row.Device {
			webHardwareDetail(w, report, row, now, chart)
		}
		fmt.Fprint(w, `</template>`)
	}
	fmt.Fprint(w, `</div>`)
}

func webHardwareDetail(w io.Writer, report SiteReport, row modemRow, now time.Time, chart string) {
	fmt.Fprintf(w, `<section class=hardware-detail aria-label="Activity for %s"><div class=hardware-detail-heading><h2>%s</h2></div>`, esc(row.Name), esc(row.ChannelName))
	if row.FrequenciesKnown && row.Programming != "Unassigned" {
		fmt.Fprintf(w, `<div class="frequency-stack hardware-frequency"><span><b>TX</b><span class=ts-numeric>%s</span> MHz</span><span><b>RX</b><span class=ts-numeric>%s</span> MHz</span></div>`, esc(webUnitText("tx_frequency_hz", row.TXHz)), esc(webUnitText("rx_frequency_hz", row.RXHz)))
	} else {
		fmt.Fprint(w, `<p class=ts-caption>Operating frequencies unavailable</p>`)
	}
	if row.Programming == "Saved programming" {
		fmt.Fprint(w, `<p class=ts-caption>Saved programming · no operating observation</p>`)
	}
	if row.Error != "" {
		fmt.Fprintf(w, `<p class=notice>%s</p>`, esc(row.Error))
	}
	tx, rx := "Unavailable", "Unavailable"
	sample, current := adminChannelSample(row.ChannelStatus, now)
	if current && report.Radio != nil && report.Radio.State == "running" && row.State == "ready" && row.Ready {
		if sample.TX != nil {
			tx = "Idle"
			if *sample.TX {
				tx = "Transmitting"
			}
		}
		if sample.RX != nil {
			rx = sample.RXState
			if rx == "" {
				if *sample.RX {
					rx = "Receiving"
				} else {
					rx = "Listening"
				}
			}
		}
	}
	at := ""
	if current {
		at = sample.At.Format(time.RFC3339Nano)
	}
	fmt.Fprintf(w, `<div class=hardware-receiver data-hardware-observed="%s"><span><b>Transmit</b><span data-hardware-live-state>%s</span></span><span><b>Receive decoder</b><span data-hardware-live-state>%s</span></span></div>`, esc(at), esc(tx), esc(rx))
	fmt.Fprint(w, `<div class=hardware-detail-heading><h3>Received-call reports</h3></div>`)
	if len(row.Telemetry.Receive) == 0 {
		fmt.Fprint(w, `<p class=ts-caption>No received-call report.</p>`)
	} else {
		fmt.Fprint(w, `<div class="table-wrap hardware-history-wrap" tabindex=0 aria-label="Received-call reports"><table class="record-table hardware-history"><thead><tr><th scope=col>Received</th><th scope=col>Bit error rate</th><th scope=col>Bit errors</th><th scope=col>Undecodable control</th><th scope=col>Frames / protected bits</th></tr></thead><tbody>`)
		for i := len(row.Telemetry.Receive) - 1; i >= 0; i-- {
			stats := row.Telemetry.Receive[i]
			fmt.Fprintf(w, `<tr><td>%s</td><td class=ts-numeric>%.4f%%</td><td class=ts-numeric>%d</td><td class=ts-numeric>%d</td><td class=ts-numeric>%d / %d</td></tr>`, webTime(stats.At), stats.BERPercent, stats.Errors, stats.UndecodableLC, stats.Frames, stats.ProtectedBits)
		}
		fmt.Fprint(w, `</tbody></table></div>`)
	}
	fmt.Fprintf(w, `<details class=hardware-disclosure data-hardware-section=charts><summary>%sCharts</summary><div class=hardware-chart-head><span class=ts-caption>Recent worker observations</span><select data-hardware-chart aria-label="Chart for %s">`, adminIcon("chevron-down"), esc(row.Name))
	for _, option := range [][2]string{{"activity", "TX / RX activity"}, {"ber", "Received-call bit error rate (%)"}, {"errors", "Received-call bit errors"}} {
		selected := ""
		if chart == option[0] || (chart == "" && option[0] == "activity") {
			selected = " selected"
		}
		fmt.Fprintf(w, `<option value="%s"%s>%s</option>`, option[0], selected, option[1])
	}
	fmt.Fprint(w, `</select></div><div class=hardware-plot>`)
	// Closed charts carry no sample marks. The first explicit expansion requests
	// the selected chart, and later live requests refresh that chart only.
	if chart != "" {
		webHardwareChart(w, row.Telemetry, chart)
	}
	fmt.Fprintf(w, `</div></details><details class=hardware-disclosure data-hardware-section=diagnostics><summary>%sDiagnostics</summary><dl class=hardware-diagnostics>`, adminIcon("chevron-down"))
	if row.Telemetry.StartedAt.IsZero() {
		fmt.Fprint(w, `<div><dt>Worker observations</dt><dd>Unavailable</dd></div>`)
	} else {
		fmt.Fprintf(w, `<div><dt>Worker started</dt><dd>%s</dd></div><div><dt>RX buffer overflows</dt><dd class=ts-numeric>%d</dd></div><div><dt>TX buffer overflows</dt><dd class=ts-numeric>%d</dd></div>`, webTime(row.Telemetry.StartedAt), row.Telemetry.RXOverflows, row.Telemetry.TXOverflows)
	}
	fmt.Fprint(w, `</dl></details></section>`)
}

func webHardwareMeasurements(w io.Writer) {
	fmt.Fprintf(w, `<details class=hardware-availability><summary>%sMeasurement availability</summary><dl><div><dt>Reported</dt><dd>TX/RX indications, received-call error estimates and buffer overflow counts.</dd></div><div><dt>Not reported</dt><dd>Signal strength, signal-to-noise ratio, measured symbol rate or frequency error, RF output/reflected power, SWR and an overall quality score.</dd></div><div><dt>P25 Phase 1 reference</dt><dd>9,600 bit/s · C4FM 4,800 symbols/s</dd></div></dl></details>`, adminIcon("chevron-down"))
}

// Percentage positions keep the plot fluid while text and marks retain their
// normal CSS pixel sizes. Per-call observations are never interpolated.
func webHardwareChart(w io.Writer, telemetry radio.ModemTelemetry, chart string) {
	label := "Recent sampled transmit and receive indications"
	if chart == "ber" {
		label = "Bit error rate of recent received calls"
	} else if chart == "errors" {
		label = "Reported bit errors of recent received calls"
	}
	fmt.Fprintf(w, `<svg width="100%%" height=175 role=img aria-label="%s">`, label)
	var first, last time.Time
	x := func(at time.Time) float64 {
		if !last.After(first) {
			return 50
		}
		return 16 + 80*float64(at.Sub(first))/float64(last.Sub(first))
	}
	if chart == "activity" {
		fmt.Fprint(w, `<line class=chart-grid x1="16%" x2="96%" y1="42" y2="42"/><line class=chart-grid x1="16%" x2="96%" y1="99" y2="99"/><text x=8 y=40>TX</text><text x=8 y=97>RX</text>`)
		if len(telemetry.Samples) > 0 {
			first, last = telemetry.Samples[0].At, telemetry.Samples[len(telemetry.Samples)-1].At
		}
		for _, point := range telemetry.Samples {
			for i, value := range []*bool{point.TX, point.RX} {
				if value == nil {
					continue
				}
				y, state := 54+i*57, "idle"
				if *value {
					y -= 24
					state = "active"
				}
				fmt.Fprintf(w, `<circle class="chart-series-%d %s" cx="%.3f%%" cy=%d r=3><title>%s · %s %s</title></circle>`, i, state, x(point.At), y, esc(webReportTime(point.At)), []string{"TX", "RX"}[i], state)
			}
		}
	} else {
		maximum := 1.0
		for _, point := range telemetry.Receive {
			value := float64(point.Errors)
			if chart == "ber" {
				value = point.BERPercent
			}
			maximum = max(maximum, value)
		}
		unit := ""
		if chart == "ber" {
			unit = "%"
		}
		fmt.Fprintf(w, `<line class=chart-grid x1="16%%" x2="16%%" y1="20" y2="126"/><line class=chart-grid x1="16%%" x2="96%%" y1="126" y2="126"/><text x="13%%" y=24 text-anchor=end>%.4g%s</text><text x="13%%" y=130 text-anchor=end>0</text>`, maximum, unit)
		if len(telemetry.Receive) > 0 {
			first, last = telemetry.Receive[0].At, telemetry.Receive[len(telemetry.Receive)-1].At
		}
		for _, point := range telemetry.Receive {
			value := float64(point.Errors)
			if chart == "ber" {
				value = point.BERPercent
			}
			fmt.Fprintf(w, `<circle class=chart-series-0 cx="%.3f%%" cy="%.3f" r=4><title>%s · %.4f%s</title></circle>`, x(point.At), 126-106*value/maximum, esc(webReportTime(point.At)), value, unit)
		}
	}
	if first.IsZero() {
		fmt.Fprint(w, `<text x="50%" y=76 text-anchor=middle>No observations</text>`)
	} else {
		fmt.Fprintf(w, `<text x="16%%" y=153>%s</text><text x="96%%" y=153 text-anchor=end>%s</text>`, first.UTC().Format("15:04:05"), last.UTC().Format("15:04:05 UTC"))
	}
	fmt.Fprint(w, `</svg>`)
	if chart == "activity" {
		fmt.Fprint(w, `<p class=ts-caption>Higher dots: active · lower dots: idle · missing dots: unavailable</p>`)
	}
}
