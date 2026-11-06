//go:build linux

package radio

import (
	"os"
	"time"
)

func localHardwarePaths() hardwarePaths {
	return hardwarePaths{byID: "/dev/serial/by-id", byPath: "/dev/serial/by-path", dev: "/dev", gpio: []string{"/dev/serial0", "/dev/ttyAMA0"}, sysTTY: "/sys/class/tty", proc: "/proc", machineID: "/etc/machine-id", bootID: "/proc/sys/kernel/random/boot_id", hostname: os.Hostname}
}

func DiscoverHardware(configured []string) HardwareInventory {
	return discoverHardware(localHardwarePaths(), configured, time.Now())
}

// HardwareHostIdentity is cheap enough for the existing Site report. It never
// enumerates processes or devices and returns no raw machine identifier.
func HardwareHostIdentity() (id, hostname, bootID string, err error) {
	return hardwareHostIdentity(localHardwarePaths())
}

func HardwareProcessStartID(pid int) string { return hardwareProcessStartID("/proc", pid) }
