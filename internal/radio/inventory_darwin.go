//go:build darwin

package radio

import (
	"errors"
	"os"
	"time"
)

func DiscoverHardware(configured []string) HardwareInventory {
	_, hostname, _, err := HardwareHostIdentity()
	devices := make([]HardwareDevice, 0, len(configured))
	seen := map[string]bool{}
	for _, path := range configured {
		if path != "" && !seen[path] {
			seen[path] = true
			devices = append(devices, HardwareDevice{ID: path, Device: path, Paths: []string{path}, Kind: "configured", IdentityKind: "Configured path", Openers: []HardwareProcess{}})
		}
	}
	return HardwareInventory{Hostname: hostname, ObservedAt: time.Now().UTC(), Devices: devices, Error: err.Error()}
}

func HardwareHostIdentity() (id, hostname, bootID string, err error) {
	hostname, _ = os.Hostname()
	err = errors.New("Radio hardware discovery requires Linux.")
	return
}

func HardwareProcessStartID(int) string { return "" }
