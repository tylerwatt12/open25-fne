package radio

import (
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"time"
)

// HardwareInventory is a passive operating-system observation. Discovery never
// opens a terminal, writes to a modem, assigns a role, or starts radio operation.
type HardwareInventory struct {
	HostID     string           `json:"host_id"`
	Hostname   string           `json:"hostname"`
	BootID     string           `json:"boot_id,omitempty"`
	ObservedAt time.Time        `json:"observed_at"`
	Devices    []HardwareDevice `json:"devices"`
	Error      string           `json:"error,omitempty"`
}

type HardwareDevice struct {
	ID             string            `json:"id"`
	Device         string            `json:"device"`
	Paths          []string          `json:"paths"`
	Kind           string            `json:"kind"`
	IdentityKind   string            `json:"identity_kind"`
	ResolvedDevice string            `json:"resolved_device,omitempty"`
	UID            string            `json:"uid,omitempty"`
	VendorID       string            `json:"vendor_id,omitempty"`
	ProductID      string            `json:"product_id,omitempty"`
	Product        string            `json:"product,omitempty"`
	Present        bool              `json:"present"`
	PresenceKnown  bool              `json:"presence_known"`
	OwnershipKnown bool              `json:"ownership_known"`
	Openers        []HardwareProcess `json:"openers"`
	Error          string            `json:"error,omitempty"`
}

// StartID and BootID distinguish a reported process from a later reuse of its
// PID. Name is /proc/comm, never a command line or environment.
type HardwareProcess struct {
	PID     int    `json:"pid"`
	Name    string `json:"name"`
	StartID string `json:"start_id,omitempty"`
}

type hardwarePaths struct {
	byID, byPath, dev, sysTTY, proc, machineID, bootID string
	gpio                                               []string
	hostname                                           func() (string, error)
}

func hardwareHostIdentity(paths hardwarePaths) (id, hostname, bootID string, err error) {
	raw, readErr := os.ReadFile(paths.machineID)
	if readErr != nil || strings.TrimSpace(string(raw)) == "" {
		err = errors.New("Host identity is unavailable; other hosts cannot be correlated.")
	} else {
		sum := sha256.Sum256([]byte("trunksystem-hardware-host\x00" + strings.TrimSpace(string(raw))))
		id = hex.EncodeToString(sum[:])
	}
	if paths.hostname != nil {
		hostname, _ = paths.hostname()
	}
	if raw, readErr = os.ReadFile(paths.bootID); readErr == nil {
		bootID = strings.TrimSpace(string(raw))
	}
	return
}

func hardwareProcessStartID(proc string, pid int) string {
	raw, err := os.ReadFile(filepath.Join(proc, strconv.Itoa(pid), "stat"))
	if err != nil {
		return ""
	}
	// The parenthesized comm field can contain spaces and parentheses. Fields
	// after its closing parenthesis begin at field 3; starttime is field 22.
	at := strings.LastIndexByte(string(raw), ')')
	if at < 0 {
		return ""
	}
	fields := strings.Fields(string(raw[at+1:]))
	if len(fields) < 20 {
		return ""
	}
	return fields[19]
}

func discoverHardware(paths hardwarePaths, configured []string, now time.Time) HardwareInventory {
	id, hostname, bootID, identityErr := hardwareHostIdentity(paths)
	out := HardwareInventory{HostID: id, Hostname: hostname, BootID: bootID, ObservedAt: now.UTC(), Devices: []HardwareDevice{}}
	if identityErr != nil {
		out.Error = identityErr.Error()
	}
	type candidate struct{ path, kind, identityKind string }
	candidates := []candidate{}
	entries, err := os.ReadDir(paths.byID)
	if err != nil && !errors.Is(err, os.ErrNotExist) {
		out.Error = appendHardwareError(out.Error, "USB identity directory could not be read.")
	}
	for _, entry := range entries {
		candidates = append(candidates, candidate{filepath.Join(paths.byID, entry.Name()), "usb", "USB identity path"})
	}
	if paths.byPath != "" {
		entries, err = os.ReadDir(paths.byPath)
		if err != nil && !errors.Is(err, os.ErrNotExist) {
			out.Error = appendHardwareError(out.Error, "USB connection-path directory could not be read.")
		}
		for _, entry := range entries {
			candidates = append(candidates, candidate{filepath.Join(paths.byPath, entry.Name()), "usb", "USB connection path"})
		}
	}
	// Include USB tty nodes lacking identity links without probing arbitrary ttys.
	if paths.dev != "" {
		entries, err = os.ReadDir(paths.sysTTY)
		if err != nil && !errors.Is(err, os.ErrNotExist) {
			out.Error = appendHardwareError(out.Error, "USB tty identities could not be read.")
		}
		for _, entry := range entries {
			device := HardwareDevice{ResolvedDevice: filepath.Join(paths.dev, entry.Name())}
			readHardwareUSB(paths, &device)
			if device.VendorID != "" {
				candidates = append(candidates, candidate{device.ResolvedDevice, "usb", "Current USB device node"})
			}
		}
	}
	for _, path := range paths.gpio {
		if _, err := os.Lstat(path); err == nil || !errors.Is(err, os.ErrNotExist) {
			candidates = append(candidates, candidate{path, "gpio", "GPIO path"})
		}
	}
	for _, path := range configured {
		if path != "" {
			candidates = append(candidates, candidate{path, "configured", "Configured path"})
		}
	}
	infos := []os.FileInfo{}
	seen := map[string]bool{}
	for _, candidate := range candidates {
		if seen[candidate.path] {
			continue
		}
		seen[candidate.path] = true
		device := HardwareDevice{ID: candidate.path, Device: candidate.path, Paths: []string{candidate.path}, Kind: candidate.kind, IdentityKind: candidate.identityKind, Openers: []HardwareProcess{}}
		info, statErr := os.Stat(candidate.path)
		if statErr != nil {
			device.PresenceKnown = errors.Is(statErr, os.ErrNotExist)
			if !device.PresenceKnown {
				device.Error = "Device presence could not be observed."
			}
		} else {
			device.PresenceKnown = true
			device.Present = info.Mode()&os.ModeCharDevice != 0
			if !device.Present {
				device.Error = "The configured path is not a character device."
			}
			device.ResolvedDevice, _ = filepath.EvalSymlinks(candidate.path)
		}
		merged := false
		if device.Present {
			for i, previous := range infos {
				if previous != nil && os.SameFile(info, previous) {
					out.Devices[i].Paths = append(out.Devices[i].Paths, candidate.path)
					merged = true
					break
				}
			}
			if !merged {
				readHardwareUSB(paths, &device)
			}
		}
		if !merged {
			out.Devices = append(out.Devices, device)
			infos = append(infos, info)
		}
	}
	observeHardwareOpeners(paths.proc, out.Devices, infos)
	for i := range out.Devices {
		sort.Strings(out.Devices[i].Paths)
	}
	sort.Slice(out.Devices, func(i, j int) bool { return out.Devices[i].Device < out.Devices[j].Device })
	return out
}

func readHardwareUSB(paths hardwarePaths, device *HardwareDevice) {
	node, err := filepath.EvalSymlinks(filepath.Join(paths.sysTTY, filepath.Base(device.ResolvedDevice), "device"))
	if err != nil {
		return
	}
	read := func(path string) string {
		raw, _ := os.ReadFile(path)
		return strings.TrimSpace(string(raw))
	}
	for here := node; here != "" && here != filepath.Dir(here); here = filepath.Dir(here) {
		vendor := read(filepath.Join(here, "idVendor"))
		if vendor == "" {
			continue
		}
		device.Kind = "usb"
		device.VendorID = vendor
		device.ProductID = read(filepath.Join(here, "idProduct"))
		device.Product = read(filepath.Join(here, "product"))
		device.UID = read(filepath.Join(here, "serial"))
		return
	}
}

func observeHardwareOpeners(proc string, devices []HardwareDevice, infos []os.FileInfo) {
	complete := hardwareProcVisibilityComplete(proc)
	pids, err := os.ReadDir(proc)
	if err != nil {
		complete = false
	}
	for _, entry := range pids {
		pid, parseErr := strconv.Atoi(entry.Name())
		if parseErr != nil || pid <= 0 {
			continue
		}
		directory := filepath.Join(proc, entry.Name(), "fd")
		fds, readErr := os.ReadDir(directory)
		if readErr != nil {
			if !errors.Is(readErr, os.ErrNotExist) {
				complete = false
			}
			continue
		}
		matched := map[int]bool{}
		for _, fd := range fds {
			opened, statErr := os.Stat(filepath.Join(directory, fd.Name()))
			if statErr != nil {
				if !errors.Is(statErr, os.ErrNotExist) {
					complete = false
				}
				continue
			}
			for i, info := range infos {
				if info != nil && devices[i].Present && os.SameFile(info, opened) {
					matched[i] = true
				}
			}
		}
		if len(matched) == 0 {
			continue
		}
		name, _ := os.ReadFile(filepath.Join(proc, entry.Name(), "comm"))
		process := HardwareProcess{PID: pid, Name: strings.TrimSpace(string(name)), StartID: hardwareProcessStartID(proc, pid)}
		for i := range matched {
			devices[i].Openers = append(devices[i].Openers, process)
		}
	}
	for i := range devices {
		devices[i].OwnershipKnown = devices[i].Present && complete
		if devices[i].Present && !complete {
			devices[i].Error = appendHardwareError(devices[i].Error, "Some process descriptors could not be inspected; ownership may be incomplete.")
		}
		sort.Slice(devices[i].Openers, func(a, b int) bool { return devices[i].Openers[a].PID < devices[i].Openers[b].PID })
	}
}

func hardwareProcVisibilityComplete(proc string) bool {
	raw, err := os.ReadFile(filepath.Join(proc, "mounts"))
	if err != nil {
		return false
	}
	for _, line := range strings.Split(string(raw), "\n") {
		fields := strings.Fields(line)
		if len(fields) < 4 || fields[2] != "proc" || fields[1] != proc {
			continue
		}
		for _, option := range strings.Split(fields[3], ",") {
			if value, found := strings.CutPrefix(option, "hidepid="); found && value != "0" && value != "off" {
				return false
			}
		}
		return true
	}
	return false
}

func appendHardwareError(previous, next string) string {
	if previous == "" {
		return next
	}
	return fmt.Sprintf("%s %s", previous, next)
}
