package radio

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func inventoryFixture(t *testing.T) hardwarePaths {
	t.Helper()
	dir := t.TempDir()
	paths := hardwarePaths{byID: filepath.Join(dir, "by-id"), gpio: []string{filepath.Join(dir, "serial0"), filepath.Join(dir, "ttyAMA0")}, sysTTY: filepath.Join(dir, "sys-tty"), proc: filepath.Join(dir, "proc"), machineID: filepath.Join(dir, "machine-id"), bootID: filepath.Join(dir, "boot-id"), hostname: func() (string, error) { return "fixture-host", nil }}
	for _, path := range []string{paths.byID, paths.sysTTY, paths.proc} {
		if err := os.MkdirAll(path, 0700); err != nil {
			t.Fatal(err)
		}
	}
	inventoryWrite(t, paths.machineID, "private-machine-identity\n")
	inventoryWrite(t, paths.bootID, "boot-lifetime\n")
	inventoryWrite(t, filepath.Join(paths.proc, "mounts"), "proc "+paths.proc+" proc rw 0 0\n")
	return paths
}

func inventoryWrite(t *testing.T, path, value string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(value), 0600); err != nil {
		t.Fatal(err)
	}
}

func inventoryLink(t *testing.T, target, path string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(target, path); err != nil {
		t.Fatal(err)
	}
}

func inventoryProcess(t *testing.T, paths hardwarePaths, pid, name, start string, devices ...string) {
	t.Helper()
	base := filepath.Join(paths.proc, pid)
	inventoryWrite(t, filepath.Join(base, "comm"), name+"\n")
	fields := make([]string, 20)
	for i := range fields {
		fields[i] = "0"
	}
	fields[0], fields[19] = "S", start
	inventoryWrite(t, filepath.Join(base, "stat"), pid+" (a process (name)) "+strings.Join(fields, " "))
	if err := os.MkdirAll(filepath.Join(base, "fd"), 0700); err != nil {
		t.Fatal(err)
	}
	for i, device := range devices {
		inventoryLink(t, device, filepath.Join(base, "fd", string(rune('0'+i))))
	}
}

func TestHardwareDiscoveryPassivelyEnumeratesStableIdentitiesAndOpeners(t *testing.T) {
	paths := inventoryFixture(t)
	first := filepath.Join(paths.byID, "usb-first")
	alias := filepath.Join(paths.byID, "usb-second-alias")
	missing := filepath.Join(paths.byID, "usb-configured-missing")
	inventoryLink(t, "/dev/null", first)
	inventoryLink(t, "/dev/null", alias)
	inventoryLink(t, "/dev/zero", paths.gpio[0])
	inventoryLink(t, "/dev/zero", paths.gpio[1])
	usb := filepath.Join(filepath.Dir(paths.sysTTY), "usb-device")
	inventoryWrite(t, filepath.Join(usb, "idVendor"), "1eaf\n")
	inventoryWrite(t, filepath.Join(usb, "idProduct"), "0004\n")
	inventoryWrite(t, filepath.Join(usb, "serial"), "stable-board-uid\n")
	inventoryWrite(t, filepath.Join(usb, "product"), "Fixture USB modem\n")
	inventoryLink(t, usb, filepath.Join(paths.sysTTY, "null", "device"))
	inventoryProcess(t, paths, "321", "dvmhost", "9001", "/dev/null", "/dev/null")
	inventoryProcess(t, paths, "123", "controller", "8001", "/dev/null", "/dev/zero")
	now := time.Date(2026, 11, 4, 12, 0, 0, 0, time.UTC)
	got := discoverHardware(paths, []string{first, missing, missing}, now)
	if got.HostID == "" || strings.Contains(got.HostID, "private-machine") || got.Hostname != "fixture-host" || got.BootID != "boot-lifetime" || !got.ObservedAt.Equal(now) || got.Error != "" {
		t.Fatalf("wrong host observation: %+v", got)
	}
	if len(got.Devices) != 3 {
		t.Fatalf("aliases or missing configuration were mishandled: %+v", got.Devices)
	}
	byPath := map[string]HardwareDevice{}
	for _, device := range got.Devices {
		byPath[device.Device] = device
	}
	device := byPath[first]
	if !device.Present || !device.PresenceKnown || !device.OwnershipKnown || len(device.Paths) != 2 || device.UID != "stable-board-uid" || device.VendorID != "1eaf" || device.ProductID != "0004" || device.Product != "Fixture USB modem" {
		t.Fatalf("wrong USB observation: %+v", device)
	}
	if len(device.Openers) != 2 || device.Openers[0].PID != 123 || device.Openers[1].PID != 321 || device.Openers[1].StartID != "9001" || device.Openers[1].Name != "dvmhost" {
		t.Fatalf("wrong actual openers or duplicate descriptors: %+v", device.Openers)
	}
	if absent := byPath[missing]; absent.Present || !absent.PresenceKnown || absent.OwnershipKnown || len(absent.Openers) != 0 {
		t.Fatalf("missing hardware became available: %+v", absent)
	}
	if gpio := byPath[paths.gpio[0]]; !gpio.Present || gpio.Kind != "gpio" || len(gpio.Paths) != 2 || len(gpio.Openers) != 1 {
		t.Fatalf("wrong GPIO aliases: %+v", gpio)
	}
}

func TestHardwareDiscoveryDoesNotClaimFreeWhenProcOrHostIdentityUnavailable(t *testing.T) {
	paths := inventoryFixture(t)
	device := filepath.Join(paths.byID, "usb-observed")
	inventoryLink(t, "/dev/null", device)
	if err := os.Remove(paths.machineID); err != nil {
		t.Fatal(err)
	}
	paths.proc = filepath.Join(t.TempDir(), "unavailable-proc")
	got := discoverHardware(paths, nil, time.Now())
	if got.HostID != "" || got.Error == "" || len(got.Devices) != 1 || !got.Devices[0].Present || got.Devices[0].OwnershipKnown || got.Devices[0].Error == "" {
		t.Fatalf("unavailable observations were presented as known: %+v", got)
	}
}

func TestHardwareDiscoveryPreservesInvalidConfiguredPathsWithoutOpeningThem(t *testing.T) {
	paths := inventoryFixture(t)
	regular := filepath.Join(t.TempDir(), "administrator-entered-path")
	inventoryWrite(t, regular, "unchanged-content")
	got := discoverHardware(paths, []string{regular, ""}, time.Now())
	if len(got.Devices) != 1 || got.Devices[0].Device != regular || got.Devices[0].Present || !got.Devices[0].PresenceKnown || got.Devices[0].Error == "" {
		t.Fatalf("configured path was hidden or repaired: %+v", got)
	}
	raw, _ := os.ReadFile(regular)
	if string(raw) != "unchanged-content" {
		t.Fatal("discovery wrote to a configured path")
	}
}

func TestHardwareDiscoveryHiddenProcessesDoNotEstablishUnusedDevice(t *testing.T) {
	paths := inventoryFixture(t)
	device := filepath.Join(paths.byID, "usb-observed")
	inventoryLink(t, "/dev/null", device)
	inventoryWrite(t, filepath.Join(paths.proc, "mounts"), "proc "+paths.proc+" proc rw,hidepid=2 0 0\n")
	got := discoverHardware(paths, nil, time.Now())
	if len(got.Devices) != 1 || got.Devices[0].OwnershipKnown || got.Devices[0].Error == "" {
		t.Fatalf("hidden process namespace was presented as unused: %+v", got)
	}
}

func TestHardwareDiscoveryIncludesUSBWithoutByIDAndLabelsConnectionIdentity(t *testing.T) {
	paths := inventoryFixture(t)
	paths.byPath = filepath.Join(filepath.Dir(paths.byID), "by-path")
	paths.dev = "/dev"
	path := filepath.Join(paths.byPath, "platform-usb-1-2")
	inventoryLink(t, "/dev/null", path)
	usb := filepath.Join(filepath.Dir(paths.sysTTY), "usb-without-serial")
	inventoryWrite(t, filepath.Join(usb, "idVendor"), "1234")
	inventoryWrite(t, filepath.Join(usb, "idProduct"), "5678")
	inventoryLink(t, usb, filepath.Join(paths.sysTTY, "null", "device"))
	got := discoverHardware(paths, nil, time.Now())
	if len(got.Devices) != 1 || got.Devices[0].Device != path || got.Devices[0].IdentityKind != "USB connection path" || got.Devices[0].UID != "" || len(got.Devices[0].Paths) != 2 {
		t.Fatalf("USB device without a hardware serial was hidden or mislabeled: %+v", got)
	}
	if err := os.Remove(path); err != nil {
		t.Fatal(err)
	}
	got = discoverHardware(paths, nil, time.Now())
	if len(got.Devices) != 1 || got.Devices[0].Device != "/dev/null" || got.Devices[0].IdentityKind != "Current USB device node" || got.Devices[0].VendorID != "1234" {
		t.Fatalf("USB sysfs device without either identity link was hidden: %+v", got)
	}
}
