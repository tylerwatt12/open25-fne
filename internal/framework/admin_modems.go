package framework

import (
	"fmt"
	"io"
	"net/http"
	"sort"
	"time"

	"open25.local/open25-fne/internal/radio"
)

// This page renders only the modem/channel observations it needs. Neither the
// page nor its fragment returns an editable document, device path or native port.
func (s *Service) webModemStatus(w http.ResponseWriter, r *http.Request, session Session) {
	w.Header().Set("Cache-Control", "no-store")
	if r.URL.Query().Get("fragment") == "1" {
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		s.webModemReports(w, time.Now().UTC())
		return
	}
	s.webHeader(w, r, "Live Modems", session, false)
	defer webFooter(w)
	fmt.Fprint(w, "<script src=/modem-status.js defer></script><div class=modem-controls><button type=button data-modem-pause hidden>Pause updates</button><a class=button href=\"\">Refresh</a><span role=status data-modem-update>Updating automatically</span>"+webHelp("About modem measurements", "TX shows the modem’s transmit indication; RX shows the P25 decoder state. Charts retain recent worker samples and receive reports. Missing samples are unknown. Use external test equipment to measure RF output or modulation quality.")+"</div>")
	s.webModemReports(w, time.Now().UTC())
	fmt.Fprint(w, `<h3>Measurement availability</h3><div class=table-wrap tabindex=0 aria-label="Measurement availability"><table class=record-table><thead><tr><th>Measurement</th><th>Availability</th></tr></thead><tbody><tr><td>RSSI / received power (dBm)</td><td>Unavailable · no qualified RSSI observation in the current telemetry path</td></tr><tr><td>SNR / noise floor</td><td>Unavailable · not reported by the current modem protocol</td></tr><tr><td>Measured symbol rate / frequency error</td><td>Unavailable · no measurement supplied</td></tr><tr><td>TX output / reflected power / SWR</td><td>Unavailable · no measurement supplied</td></tr><tr><td>Overall quality percentage</td><td>Unavailable · no defined measurement; use the reported decoder statistics</td></tr><tr><td>P25 Phase 1 nominal line rate</td><td>9,600 bit/s · C4FM 4,800 symbols/s · protocol reference, not measured throughput</td></tr></tbody></table></div>`)
}

func (s *Service) webModemReports(w io.Writer, now time.Time) {
	fmt.Fprint(w, `<div data-modem-reports>`)
	defer fmt.Fprint(w, `</div>`)
	role := s.Config().Role
	var reports []SiteReport
	if role == RoleSite {
		var site SiteConfig
		if err := s.owned(&site); err != nil {
			webError(w, err)
			return
		}
		status := s.radioStatus()
		reports = []SiteReport{{ID: site.ID, Name: site.ID, Config: site, Radio: &status, ObservedAt: now}}
	} else {
		reports = s.siteReports()
	}
	zone := s.webKnownZone()
	via := ""
	var relayed time.Time
	if role == RoleSystem {
		var relay zoneReport
		if s.readCache("zone_report", &relay) {
			relayed = relay.ReceivedAt
		}
		if zone != nil {
			via = zone.Name
			if via == "" {
				via = zone.ID
			}
		}
	}
	// Preserve missing registered Sites as missing, including before the first
	// report. Registry entries provide identity, never invented runtime rows.
	if role != RoleSite && zone != nil {
		for _, identity := range zone.Sites {
			found := false
			for _, report := range reports {
				found = found || report.ID == identity.ID
			}
			if !found {
				reports = append(reports, SiteReport{ID: identity.ID, Name: identity.Name, AdminURL: identity.AdminURL})
			}
		}
	}
	sort.SliceStable(reports, func(i, j int) bool { return reports[i].ID < reports[j].ID })
	if len(reports) == 0 {
		fmt.Fprint(w, `<p>No Site reports available.</p>`)
	}
	for index, report := range reports {
		name := report.Name
		if name == "" {
			name = report.ID
		}
		fmt.Fprintf(w, `<section class=modem-site><h3>%s</h3>`, esc(name))
		webReportMeta(w, report.ID, via, role == RoleSite, report.ObservedAt, report.ReceivedAt)
		threshold := time.Duration(max(10, s.Config().PollSeconds*4)) * time.Second
		stale := role != RoleSite && (modemTimeStale(report.ObservedAt, now, threshold) || modemTimeStale(report.ReceivedAt, now, threshold))
		if role == RoleSystem {
			stale = stale || modemTimeStale(relayed, now, threshold)
			fmt.Fprintf(w, `<div class=report-meta><span>Relayed here %s</span></div>`, webTime(relayed))
		}
		if report.Error != "" {
			fmt.Fprintf(w, `<p class=notice>%s</p>`, esc(report.Error))
		}
		if stale {
			webSourceTag(w, "Stale · current state unknown")
		}
		if role != RoleSite {
			fmt.Fprint(w, `<p>`)
			s.webAdminAddress(w, report.AdminURL, "Open Site Administration")
			fmt.Fprint(w, `</p>`)
		}
		if report.Radio != nil {
			fmt.Fprintf(w, `<p><span class=source-tag>%s</span> <span class=source-tag>%s</span></p>`, esc(report.Radio.State), esc(report.Radio.Mode))
			if report.Radio.Error != "" {
				fmt.Fprintf(w, `<p class=notice>%s</p>`, esc(report.Radio.Error))
			}
		}
		rows := modemRows(report, zone)
		fmt.Fprint(w, `<div class=table-wrap tabindex=0 aria-label="Per-modem TX and RX observations"><table class="record-table modem-table"><thead><tr><th scope=col>Modem / channel</th><th scope=col>TX / RX frequency (MHz)</th><th scope=col>TX</th><th scope=col>RX</th><th scope=col>Last received-call BER</th><th scope=col>FEC / decode errors</th><th scope=col>Observed</th></tr></thead><tbody>`)
		for rowIndex, row := range rows {
			id := fmt.Sprintf("modem-chart-%d-%d", index, rowIndex)
			txFrequency, rxFrequency := "Unavailable", "Unavailable"
			if row.Programming != "Unassigned" && row.FrequenciesKnown {
				txFrequency, rxFrequency = webUnitText("tx_frequency_hz", row.TXHz), webUnitText("rx_frequency_hz", row.RXHz)
			}
			fmt.Fprintf(w, `<tr><td><a href="#%s">%s</a><br><span class=muted>%s · %s</span><br>%s</td><td><span class=nowrap>TX %s</span><br><span class=nowrap>RX %s</span><br><span class=muted>%s</span></td>`, id, esc(row.Name), esc(row.ID), esc(row.Role), esc(row.ChannelName), esc(txFrequency), esc(rxFrequency), esc(row.Programming))
			var sample radio.ModemSample
			if len(row.Telemetry.Samples) > 0 {
				sample = row.Telemetry.Samples[len(row.Telemetry.Samples)-1]
			}
			operating := report.Radio != nil && (report.Radio.State == "running" || report.Radio.State == "starting")
			current := operating && !stale && row.Ready && row.State == "ready" && row.Telemetry.StartedAt.Equal(row.StartedAt) && !modemTimeStale(sample.At, now, 3*time.Second)
			tx, rx := "Unavailable", "Unavailable"
			if current {
				if sample.TX != nil {
					tx = "Idle"
					if *sample.TX {
						tx = "Transmitting"
					}
				}
				if sample.RX != nil {
					rx = sample.RXState
				}
			}
			fmt.Fprintf(w, `<td data-modem-live-state>%s</td><td data-modem-live-state>%s</td>`, esc(tx), esc(rx))
			if count := len(row.Telemetry.Receive); count > 0 {
				last := row.Telemetry.Receive[count-1]
				fmt.Fprintf(w, `<td>%s%%<br><span class=muted>%s</span></td><td>%d reported bit errors<br>%d undecodable LC<br><span class=muted>%d frames · %d protected bits</span></td>`, esc(fmt.Sprintf("%.4f", last.BERPercent)), webTime(last.At), last.Errors, last.UndecodableLC, last.Frames, last.ProtectedBits)
			} else {
				fmt.Fprint(w, `<td>Unavailable · no completed RX report</td><td>Unavailable · no completed RX report</td>`)
			}
			fmt.Fprintf(w, `<td>%s<br><span class=muted>%s</span>`, webTime(sample.At), esc(row.State))
			if row.Error != "" {
				fmt.Fprintf(w, `<br>%s`, esc(row.Error))
			}
			fmt.Fprint(w, `</td></tr>`)
		}
		if len(rows) == 0 {
			fmt.Fprint(w, `<tr><td colspan=7>No modem observations available.</td></tr>`)
		}
		fmt.Fprint(w, `</tbody></table></div><div class=modem-charts>`)
		for rowIndex, row := range rows {
			fmt.Fprintf(w, `<section class=modem-chart-card id="modem-chart-%d-%d"><h4>%s · %s</h4>`, index, rowIndex, esc(row.Name), esc(row.ChannelName))
			webModemActivityChart(w, row.Telemetry.Samples)
			webModemReceiveChart(w, row.Telemetry.Receive, true)
			webModemReceiveChart(w, row.Telemetry.Receive, false)
			if len(row.Telemetry.Receive) > 0 {
				fmt.Fprint(w, `<details class=modem-receive-history><summary>Received-call reports</summary><div class=table-wrap tabindex=0 aria-label="Received-call decoder reports"><table class=record-table><thead><tr><th>Site observed</th><th>BER estimate (%)</th><th>Reported bit errors</th><th>Undecodable LC</th><th>Frames / protected bits</th></tr></thead><tbody>`)
				for _, stats := range row.Telemetry.Receive {
					fmt.Fprintf(w, `<tr><td>%s</td><td>%.4f</td><td>%d</td><td>%d</td><td>%d / %d</td></tr>`, webTime(stats.At), stats.BERPercent, stats.Errors, stats.UndecodableLC, stats.Frames, stats.ProtectedBits)
				}
				fmt.Fprint(w, `</tbody></table></div></details>`)
			}
			if !row.Telemetry.StartedAt.IsZero() {
				fmt.Fprintf(w, `<p class=muted>Worker started %s · RX buffer overflows %d · TX buffer overflows %d</p>`, webTime(row.Telemetry.StartedAt), row.Telemetry.RXOverflows, row.Telemetry.TXOverflows)
			} else {
				fmt.Fprint(w, `<p class=muted>Overflow counts unavailable · no worker observations</p>`)
			}
			fmt.Fprint(w, `</section>`)
		}
		fmt.Fprint(w, `</div></section>`)
	}
}

func modemTimeStale(at, now time.Time, limit time.Duration) bool {
	return at.IsZero() || now.Sub(at) > limit || at.After(now.Add(time.Second))
}

type modemRow struct {
	ID, Name, ChannelName, Role, Programming string
	TXHz, RXHz                               int64
	FrequenciesKnown                         bool
	radio.ChannelStatus
}

func modemRows(report SiteReport, zones ...*ZoneConfig) []modemRow {
	var zone *ZoneConfig
	if len(zones) > 0 {
		zone = zones[0]
	}
	rows := []modemRow{}
	bound := map[string]bool{}
	if report.Radio != nil {
		for _, ch := range report.Radio.Channels {
			// Runtime identity is separate from a possibly edited saved binding.
			id, name := ch.OperatingModemID, ch.OperatingModemName
			if id == "" {
				continue
			}
			if name == "" {
				name = id
			}
			rows = append(rows, modemRow{ID: id, Name: name, ChannelName: ch.Name, Role: ch.Role, Programming: "Operating programming", FrequenciesKnown: true, TXHz: ch.TXHz, RXHz: ch.RXHz, ChannelStatus: ch})
			bound[id+"\x00"+ch.ID] = true
		}
	}
	for _, modem := range report.Config.Modems {
		name := modem.Name
		if name == "" {
			name = modem.ID
		}
		assigned := false
		for _, ch := range report.Config.Channels {
			if ch.ModemID != modem.ID {
				continue
			}
			assigned = true
			if !bound[modem.ID+"\x00"+ch.ID] {
				tx, rx, known := bandPlanFrequencies(zone, ch.Identifier, ch.ChannelNo)
				rows = append(rows, modemRow{ID: modem.ID, Name: name, ChannelName: ch.Name, Role: ch.Role, Programming: "Saved programming", FrequenciesKnown: known, TXHz: tx, RXHz: rx, ChannelStatus: radio.ChannelStatus{ID: ch.ID, State: "No operating observation"}})
			}
		}
		if !assigned {
			active := false
			for _, row := range rows {
				active = active || row.ID == modem.ID
			}
			if !active {
				rows = append(rows, modemRow{ID: modem.ID, Name: name, ChannelName: "Unassigned", Programming: "Unassigned", ChannelStatus: radio.ChannelStatus{State: "No operating observation"}})
			}
		}
	}
	return rows
}

func modemChartX(at, first, last time.Time) float64 {
	if !last.After(first) {
		return 320
	}
	return 90 + 500*float64(at.Sub(first))/float64(last.Sub(first))
}

func webModemChartTimes(w io.Writer, first, last time.Time) {
	fmt.Fprintf(w, `<text x=90 y=124>%s</text><text x=590 y=124 text-anchor=end>%s</text>`, esc(first.UTC().Format("15:04:05 UTC")), esc(last.UTC().Format("15:04:05 UTC")))
}

func webModemActivityChart(w io.Writer, samples []radio.ModemSample) {
	fmt.Fprint(w, `<div class=modem-chart tabindex=0 aria-label="Scrollable TX and RX activity chart"><p>Sampled TX / RX activity</p><svg viewBox="0 0 620 140" role=img aria-label="Individual TX and RX status samples; active samples are higher than idle samples"><path class=chart-grid d="M90 34H590 M90 76H590"/><text x=12 y=32>TX</text><text x=12 y=74>RX</text>`)
	if len(samples) == 0 {
		fmt.Fprint(w, `<text x=320 y=55 text-anchor=middle>Unavailable · no status samples</text>`)
	} else {
		first, last := samples[0].At, samples[len(samples)-1].At
		for _, sample := range samples {
			for i, value := range []*bool{sample.TX, sample.RX} {
				if value == nil {
					continue
				}
				y, state := 42+i*42, "idle"
				if *value {
					y, state = y-16, "active"
				}
				fmt.Fprintf(w, `<circle class="chart-series-%d %s" cx="%.2f" cy=%d r=3><title>%s · %s %s</title></circle>`, i, state, modemChartX(sample.At, first, last), y, esc(webReportTime(sample.At)), []string{"TX", "RX"}[i], state)
			}
		}
		webModemChartTimes(w, first, last)
	}
	fmt.Fprint(w, `</svg><p class="muted chart-caption">Higher dots: active · lower dots: idle · missing dots: unavailable</p></div>`)
}

func webModemReceiveChart(w io.Writer, receive []radio.ReceiveStatistics, ber bool) {
	label := "Reported FEC bit errors per received call"
	if ber {
		label = "Native voice BER estimate per received call (%)"
	}
	fmt.Fprintf(w, `<div class=modem-chart tabindex=0 aria-label="Scrollable %s chart"><p>%s</p><svg viewBox="0 0 620 140" role=img aria-label="%s"><path class=chart-grid d="M90 20V96H590"/>`, label, label, label)
	if len(receive) == 0 {
		fmt.Fprint(w, `<text x=340 y=60 text-anchor=middle>Unavailable · no completed RX report</text>`)
	} else {
		maximum := 1.0
		for _, point := range receive {
			value := float64(point.Errors)
			if ber {
				value = point.BERPercent
			}
			maximum = max(maximum, value)
		}
		fmt.Fprintf(w, `<text x=80 y=25 text-anchor=end>%.4g</text><text x=80 y=100 text-anchor=end>0</text>`, maximum)
		first, last := receive[0].At, receive[len(receive)-1].At
		for _, point := range receive {
			value := float64(point.Errors)
			if ber {
				value = point.BERPercent
			}
			fmt.Fprintf(w, `<circle class=chart-series-0 cx="%.2f" cy="%.2f" r=4><title>%s · %.4f</title></circle>`, modemChartX(point.At, first, last), 96-76*value/maximum, esc(webReportTime(point.At)), value)
		}
		webModemChartTimes(w, first, last)
	}
	fmt.Fprint(w, `</svg></div>`)
}
