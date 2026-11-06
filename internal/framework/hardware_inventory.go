package framework

import (
	"os"
	"sort"
	"time"

	"open25.local/open25-fne/internal/radio"
)

// SiteHardwareReport contains only the identities and observations needed to
// explain host-local assignment and ownership. It is never configuration input.
type SiteHardwareReport struct {
	SiteID            string                      `json:"site_id"`
	HostID            string                      `json:"host_id"`
	Hostname          string                      `json:"hostname"`
	BootID            string                      `json:"boot_id,omitempty"`
	ControllerPID     int                         `json:"controller_pid"`
	ControllerStartID string                      `json:"controller_start_id,omitempty"`
	ObservedAt        time.Time                   `json:"observed_at"`
	ReceivedAt        time.Time                   `json:"received_at"`
	Modems            []HardwareModemAssignment   `json:"modems"`
	Workers           []HardwareWorkerObservation `json:"workers"`
}

type HardwareWorkerObservation struct {
	ModemID string `json:"modem_id"`
	Device  string `json:"device"`
	HardwareChannelAssignment
}

type HardwareModemAssignment struct {
	ID       string                      `json:"id"`
	Name     string                      `json:"name"`
	Device   string                      `json:"device"`
	Channels []HardwareChannelAssignment `json:"channels"`
}

type HardwareChannelAssignment struct {
	ID             string    `json:"id"`
	Name           string    `json:"name"`
	Role           string    `json:"role"`
	RuntimeRole    string    `json:"runtime_role"`
	Enabled        bool      `json:"enabled"`
	State          string    `json:"state"`
	PID            int       `json:"pid"`
	StartID        string    `json:"start_id,omitempty"`
	Ready          bool      `json:"ready"`
	Error          string    `json:"error,omitempty"`
	RecoveryWanted bool      `json:"recovery_wanted"`
	RecoveryAt     time.Time `json:"recovery_at,omitempty"`
}

type HardwareInventoryView struct {
	HostID     string               `json:"host_id"`
	Hostname   string               `json:"hostname"`
	ObservedAt time.Time            `json:"observed_at"`
	Error      string               `json:"error,omitempty"`
	Devices    []HardwareDeviceView `json:"devices"`
}

type HardwareDeviceView struct {
	radio.HardwareDevice
	Assignments  []HardwareAssignmentView `json:"assignments"`
	Observations []HardwareAssignmentView `json:"observations"`
	Owners       []HardwareOwner          `json:"owners"`
}

type HardwareAssignmentView struct {
	SiteID         string    `json:"site_id"`
	ModemID        string    `json:"modem_id"`
	ModemName      string    `json:"modem_name"`
	ChannelID      string    `json:"channel_id"`
	ChannelName    string    `json:"channel_name"`
	Role           string    `json:"role"`
	RuntimeRole    string    `json:"runtime_role"`
	State          string    `json:"state"`
	Error          string    `json:"error"`
	PID            int       `json:"pid"`
	Ready          bool      `json:"ready"`
	Enabled        bool      `json:"enabled"`
	Local          bool      `json:"local"`
	Stale          bool      `json:"stale"`
	RecoveryWanted bool      `json:"recovery_wanted"`
	ObservedAt     time.Time `json:"observed_at"`
	ReceivedAt     time.Time `json:"received_at"`
	RecoveryAt     time.Time `json:"recovery_at"`
}

type HardwareOwner struct {
	PID         int       `json:"pid"`
	Name        string    `json:"name"`
	SiteID      string    `json:"site_id"`
	ChannelID   string    `json:"channel_id"`
	ChannelName string    `json:"channel_name"`
	Role        string    `json:"role"`
	Local       bool      `json:"local"`
	Stale       bool      `json:"stale"`
	ObservedAt  time.Time `json:"observed_at"`
	ReceivedAt  time.Time `json:"received_at"`
}

func (s *Service) ownHardwareReport(site SiteConfig, status radio.Status) SiteHardwareReport {
	hostID, hostname, bootID, _ := radio.HardwareHostIdentity()
	now := time.Now().UTC()
	report := SiteHardwareReport{SiteID: s.Config().InstanceID, HostID: hostID, Hostname: hostname, BootID: bootID, ControllerPID: os.Getpid(), ControllerStartID: radio.HardwareProcessStartID(os.Getpid()), ObservedAt: now, ReceivedAt: now, Modems: []HardwareModemAssignment{}, Workers: []HardwareWorkerObservation{}}
	for _, runtime := range status.Channels {
		observation := HardwareChannelAssignment{ID: runtime.ID, Name: runtime.Name, Role: runtime.ConfiguredRole, RuntimeRole: runtime.Role, State: runtime.State, PID: runtime.PID, Ready: runtime.Ready, Error: runtime.Error, RecoveryWanted: runtime.RecoveryWanted, RecoveryAt: runtime.RecoveryAt}
		if runtime.PID > 0 {
			observation.StartID = radio.HardwareProcessStartID(runtime.PID)
		}
		report.Workers = append(report.Workers, HardwareWorkerObservation{ModemID: runtime.ModemID, Device: runtime.Device, HardwareChannelAssignment: observation})
	}
	for _, modem := range site.Modems {
		assignment := HardwareModemAssignment{ID: modem.ID, Name: modem.Name, Device: modem.Device, Channels: []HardwareChannelAssignment{}}
		for _, channel := range site.Channels {
			if channel.ModemID != modem.ID {
				continue
			}
			observation := HardwareChannelAssignment{ID: channel.ID, Name: channel.Name, Role: channel.Role, Enabled: channel.Enabled, State: "No observation"}
			if !status.Available {
				observation.State, observation.Error = "unavailable", status.Error
			} else if status.State == "stopped" {
				observation.State = "stopped"
			}
			for _, runtime := range report.Workers {
				if runtime.ID == channel.ID && runtime.Device != "" && runtime.Device == modem.Device {
					observation.State, observation.PID, observation.Ready, observation.StartID = runtime.State, runtime.PID, runtime.Ready, runtime.StartID
					observation.RuntimeRole, observation.Error = runtime.RuntimeRole, runtime.Error
					observation.RecoveryWanted, observation.RecoveryAt = runtime.RecoveryWanted, runtime.RecoveryAt
					break
				}
			}
			assignment.Channels = append(assignment.Channels, observation)
		}
		report.Modems = append(report.Modems, assignment)
	}
	return report
}

// hardwareReportsForSite uses the existing authenticated Zone report cache.
// Neither the browser nor the Site reads sibling configuration or databases.
func (s *Service) hardwareReportsForSite(siteID string) []SiteHardwareReport {
	reports := s.siteReports()
	hostID := ""
	for _, report := range reports {
		if report.ID == siteID && report.Hardware != nil {
			hostID = report.Hardware.HostID
			break
		}
	}
	out := []SiteHardwareReport{}
	if hostID == "" {
		return out
	}
	for _, report := range reports {
		if report.Hardware != nil && report.Hardware.HostID == hostID {
			out = append(out, *report.Hardware)
		}
	}
	return out
}

// hardwareInventory performs its passive descriptor scan only for an explicit
// hardware-page request. Ordinary two-second reports do not enumerate /proc.
func (s *Service) hardwareInventory() HardwareInventoryView {
	var site SiteConfig
	if s.Config().Role != RoleSite {
		return HardwareInventoryView{Error: "Local hardware observations belong to a Site Controller.", Devices: []HardwareDeviceView{}}
	}
	if err := s.owned(&site); err != nil {
		return HardwareInventoryView{Error: "Saved modem assignments are unavailable.", Devices: []HardwareDeviceView{}}
	}
	local := s.ownHardwareReport(site, s.radioStatus())
	reports := []SiteHardwareReport{local}
	var received []SiteHardwareReport
	s.readCache("hardware_reports", &received)
	for _, report := range received {
		if report.SiteID != local.SiteID && local.HostID != "" && report.HostID == local.HostID {
			reports = append(reports, report)
		}
	}
	paths := []string{}
	for _, report := range reports {
		for _, modem := range report.Modems {
			paths = append(paths, modem.Device)
		}
		for _, worker := range report.Workers {
			paths = append(paths, worker.Device)
		}
	}
	inventory := radio.DiscoverHardware(paths)
	return buildHardwareInventoryView(inventory, local.SiteID, reports, time.Duration(max(10, s.Config().PollSeconds*4))*time.Second)
}

func buildHardwareInventoryView(inventory radio.HardwareInventory, localSiteID string, reports []SiteHardwareReport, staleAfter time.Duration) HardwareInventoryView {
	out := HardwareInventoryView{HostID: inventory.HostID, Hostname: inventory.Hostname, ObservedAt: inventory.ObservedAt, Error: inventory.Error, Devices: []HardwareDeviceView{}}
	for _, device := range inventory.Devices {
		view := HardwareDeviceView{HardwareDevice: device, Assignments: []HardwareAssignmentView{}, Observations: []HardwareAssignmentView{}, Owners: []HardwareOwner{}}
		for _, report := range reports {
			local := report.SiteID == localSiteID
			if !local && (inventory.HostID == "" || report.HostID != inventory.HostID) {
				continue
			}
			stale := hardwareReportStale(report, inventory.ObservedAt, staleAfter)
			for _, worker := range report.Workers {
				if hardwarePathMatches(device, worker.Device) {
					view.Observations = append(view.Observations, HardwareAssignmentView{SiteID: report.SiteID, ModemID: worker.ModemID, ChannelID: worker.ID, ChannelName: worker.Name, Role: worker.Role, RuntimeRole: worker.RuntimeRole, State: worker.State, Error: worker.Error, PID: worker.PID, Ready: worker.Ready, RecoveryWanted: worker.RecoveryWanted, RecoveryAt: worker.RecoveryAt, Local: local, Stale: stale, ObservedAt: report.ObservedAt, ReceivedAt: report.ReceivedAt})
				}
			}
			for _, modem := range report.Modems {
				if !hardwarePathMatches(device, modem.Device) {
					continue
				}
				base := HardwareAssignmentView{SiteID: report.SiteID, ModemID: modem.ID, ModemName: modem.Name, Local: local, Stale: stale, ObservedAt: report.ObservedAt, ReceivedAt: report.ReceivedAt}
				if len(modem.Channels) == 0 {
					view.Assignments = append(view.Assignments, base)
				}
				for _, channel := range modem.Channels {
					assignment := base
					assignment.ChannelID, assignment.ChannelName, assignment.Role, assignment.State = channel.ID, channel.Name, channel.Role, channel.State
					assignment.PID, assignment.Ready, assignment.Enabled = channel.PID, channel.Ready, channel.Enabled
					assignment.RuntimeRole, assignment.Error = channel.RuntimeRole, channel.Error
					assignment.RecoveryWanted, assignment.RecoveryAt = channel.RecoveryWanted, channel.RecoveryAt
					view.Assignments = append(view.Assignments, assignment)
				}
			}
		}
		for _, process := range device.Openers {
			owner := HardwareOwner{PID: process.PID, Name: process.Name, ObservedAt: inventory.ObservedAt}
			for _, report := range reports {
				if inventory.HostID == "" || report.HostID != inventory.HostID || inventory.BootID == "" || report.BootID != inventory.BootID || hardwareReportStale(report, inventory.ObservedAt, staleAfter) {
					continue
				}
				if hardwareProcessMatches(process, report.ControllerPID, report.ControllerStartID) {
					owner.SiteID, owner.Role, owner.Local = report.SiteID, "Site Controller", report.SiteID == localSiteID
					owner.ObservedAt, owner.ReceivedAt = report.ObservedAt, report.ReceivedAt
				}
				for _, worker := range report.Workers {
					if hardwareProcessMatches(process, worker.PID, worker.StartID) {
						owner.SiteID, owner.ChannelID, owner.ChannelName, owner.Role, owner.Local = report.SiteID, worker.ID, worker.Name, worker.RuntimeRole, report.SiteID == localSiteID
						owner.ObservedAt, owner.ReceivedAt = report.ObservedAt, report.ReceivedAt
					}
				}
			}
			view.Owners = append(view.Owners, owner)
		}
		sort.Slice(view.Assignments, func(i, j int) bool {
			a, b := view.Assignments[i], view.Assignments[j]
			return a.SiteID+"\x00"+a.ModemID+"\x00"+a.ChannelID < b.SiteID+"\x00"+b.ModemID+"\x00"+b.ChannelID
		})
		out.Devices = append(out.Devices, view)
	}
	return out
}

func hardwarePathMatches(device radio.HardwareDevice, path string) bool {
	if path == device.Device {
		return true
	}
	for _, alias := range device.Paths {
		if alias == path {
			return true
		}
	}
	return false
}

func hardwareProcessMatches(process radio.HardwareProcess, pid int, startID string) bool {
	return pid > 0 && process.PID == pid && process.StartID != "" && process.StartID == startID
}

func hardwareReportStale(report SiteHardwareReport, now time.Time, staleAfter time.Duration) bool {
	return report.ObservedAt.IsZero() || report.ReceivedAt.IsZero() || report.ObservedAt.After(now.Add(time.Minute)) || now.Sub(report.ObservedAt) > staleAfter || now.Sub(report.ReceivedAt) > staleAfter
}
