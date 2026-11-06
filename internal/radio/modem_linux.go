//go:build linux

package radio

import (
	"context"
	"encoding/hex"
	"errors"
	"fmt"
	"golang.org/x/sys/unix"
	"io"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"time"
)

// acquireDevice performs no UART write, reset or configuration. It refuses
// another process's descriptor and holds the same advisory device lease used
// by the former controller until the new native child is reaped.
func acquireDevice(ctx context.Context, path string) (io.Closer, error) {
	if filepath.Clean(path) != path || (!strings.HasPrefix(path, "/dev/serial/by-id/") && path != "/dev/serial0" && path != "/dev/ttyAMA0") {
		return nil, errors.New("select a stable USB by-id modem or the GPIO UART")
	}
	resolved, err := filepath.EvalSymlinks(path)
	if err != nil {
		return nil, errors.New("selected modem is unavailable")
	}
	info, err := os.Stat(resolved)
	if err != nil || info.Mode()&os.ModeCharDevice == 0 {
		return nil, errors.New("selected modem is not a character device")
	}
	pids, err := os.ReadDir("/proc")
	if err != nil {
		return nil, errors.New("could not check modem ownership")
	}
	for _, pid := range pids {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		number, err := strconv.Atoi(pid.Name())
		if err != nil || number == os.Getpid() {
			continue
		}
		fds, err := os.ReadDir(filepath.Join("/proc", pid.Name(), "fd"))
		if err != nil {
			continue
		}
		for _, fd := range fds {
			target, err := os.Stat(filepath.Join("/proc", pid.Name(), "fd", fd.Name()))
			if err == nil && os.SameFile(info, target) {
				return nil, fmt.Errorf("selected modem is already open by process %d", number)
			}
		}
	}
	f, err := os.OpenFile(resolved, os.O_RDWR|syscall.O_NOCTTY|syscall.O_NONBLOCK, 0)
	if err != nil {
		return nil, errors.New("could not acquire the modem")
	}
	if err = syscall.Flock(int(f.Fd()), syscall.LOCK_EX|syscall.LOCK_NB); err != nil {
		f.Close()
		return nil, errors.New("another controller owns the modem")
	}
	after, err := f.Stat()
	again, linkErr := filepath.EvalSymlinks(path)
	if err != nil || linkErr != nil || again != resolved || !os.SameFile(info, after) {
		f.Close()
		return nil, errors.New("modem changed while acquiring ownership")
	}
	return f, nil
}

func verifyFirmware(ctx context.Context, lease io.Closer, device string, cwid20 bool) error {
	f, ok := lease.(*os.File)
	if !ok {
		return errors.New("modem lease is unavailable")
	}
	fd := int(f.Fd())
	settings, err := unix.IoctlGetTermios(fd, unix.TCGETS)
	if err != nil {
		return errors.New("modem terminal settings are unavailable")
	}
	if settings.Cflag&unix.CBAUD == unix.B1200 {
		return errors.New("refusing a USB modem saved at the bootloader's 1200-baud setting")
	}
	defer unix.IoctlSetTermios(fd, unix.TCSETS, settings)
	raw := *settings
	raw.Iflag = 0
	raw.Oflag = 0
	raw.Lflag = 0
	raw.Cflag = unix.B115200 | unix.CS8 | unix.CREAD | unix.CLOCAL
	raw.Ispeed = unix.B115200
	raw.Ospeed = unix.B115200
	raw.Cc[unix.VMIN] = 0
	raw.Cc[unix.VTIME] = 0
	if err = unix.IoctlSetTermios(fd, unix.TCSETS, &raw); err != nil {
		return errors.New("could not prepare modem identity query")
	}
	deadline := time.Now().Add(2500 * time.Millisecond)
	query := []byte{0xFE, 0x03, 0x00}
	written := 0
	reply := []byte{}
	buffer := make([]byte, 512)
	for time.Now().Before(deadline) {
		if err := ctx.Err(); err != nil {
			return err
		}
		if written < len(query) {
			n, e := unix.Write(fd, query[written:])
			if e != nil && e != unix.EAGAIN && e != unix.EINTR {
				return errors.New("modem identity query failed")
			}
			if n > 0 {
				written += n
			}
		}
		n, e := unix.Read(fd, buffer)
		if e != nil && e != unix.EAGAIN && e != unix.EINTR {
			return errors.New("modem identity reply failed")
		}
		if n > 0 {
			reply = append(reply, buffer[:n]...)
			if len(reply) > 8192 {
				return errors.New("modem identity reply exceeded the limit")
			}
		}
		for at := 0; at+3 <= len(reply); at++ {
			if reply[at] != 0xFE {
				continue
			}
			length := int(reply[at+1])
			if length < 22 || at+length > len(reply) || reply[at+2] != 0x00 {
				continue
			}
			frame := reply[at : at+length]
			if frame[3] != 4 || !strings.Contains(string(frame[21:]), "P25") {
				return errors.New("modem firmware does not provide the qualified P25 protocol")
			}
			if cwid20 && !strings.Contains(string(frame[21:]), "CWID20") {
				return errors.New("this modem needs the qualified 20 WPM identification firmware")
			}
			if strings.HasPrefix(device, "/dev/serial/by-id/") {
				resolved, _ := filepath.EvalSymlinks(device)
				node, _ := filepath.EvalSymlinks(filepath.Join("/sys/class/tty", filepath.Base(resolved), "device"))
				serial := ""
				matched := false
				for here := node; here != "" && here != filepath.Dir(here); here = filepath.Dir(here) {
					vendor, e := os.ReadFile(filepath.Join(here, "idVendor"))
					if e != nil {
						continue
					}
					product, _ := os.ReadFile(filepath.Join(here, "idProduct"))
					serialRaw, _ := os.ReadFile(filepath.Join(here, "serial"))
					serial = strings.TrimSpace(string(serialRaw))
					matched = strings.TrimSpace(string(vendor)) == "1eaf" && strings.TrimSpace(string(product)) == "0004"
					break
				}
				if !matched || len(serial) != 24 || !strings.EqualFold(serial, hex.EncodeToString(frame[5:17])) {
					return errors.New("USB descriptors do not match the modem identity")
				}
			}
			return nil
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(5 * time.Millisecond):
		}
	}
	return errors.New("modem did not answer the bounded identity query")
}
func deviceSame(lease io.Closer, path string) bool {
	f, ok := lease.(*os.File)
	if !ok {
		return false
	}
	owned, err := f.Stat()
	if err != nil {
		return false
	}
	current, err := os.Stat(path)
	return err == nil && os.SameFile(owned, current)
}
