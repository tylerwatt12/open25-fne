package radio

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"

	"golang.org/x/sys/unix"
)

// ErrMaintenance means an exclusive maintenance owner has withheld startup.
// A rejected start is never queued for replay when that owner releases it.
var ErrMaintenance = errors.New("radio maintenance is in progress; start is unavailable")

// AcquireStartLease shares the owner's maintenance inode through startup. Each
// caller opens its own descriptor: closing one concurrent starter cannot drop
// another starter's lock. Stop deliberately does not acquire this lease.
func AcquireStartLease(path string) (*os.File, error) {
	directory, err := unix.Open(filepath.Dir(path), unix.O_RDONLY|unix.O_DIRECTORY|unix.O_NOFOLLOW|unix.O_CLOEXEC, 0)
	if err != nil {
		return nil, fmt.Errorf("radio maintenance directory: %w", err)
	}
	defer unix.Close(directory)
	var info unix.Stat_t
	if err = unix.Fstat(directory, &info); err != nil || info.Uid != uint32(os.Geteuid()) || info.Mode&0022 != 0 {
		return nil, errors.New("radio maintenance requires an owner-controlled state directory")
	}
	fd, err := unix.Openat(directory, filepath.Base(path), unix.O_CREAT|unix.O_RDWR|unix.O_NOFOLLOW|unix.O_CLOEXEC, 0600)
	if err != nil {
		return nil, fmt.Errorf("radio maintenance lock: %w", err)
	}
	file := os.NewFile(uintptr(fd), path)
	if err = unix.Fstat(fd, &info); err != nil || info.Mode&unix.S_IFMT != unix.S_IFREG || info.Mode&0777 != 0600 || info.Uid != uint32(os.Geteuid()) || info.Nlink != 1 {
		_ = file.Close()
		return nil, errors.New("radio maintenance lock must be an owner-only regular file")
	}
	if err = unix.Flock(fd, unix.LOCK_SH|unix.LOCK_NB); err != nil {
		_ = file.Close()
		if errors.Is(err, unix.EWOULDBLOCK) || errors.Is(err, unix.EAGAIN) {
			return nil, ErrMaintenance
		}
		return nil, fmt.Errorf("radio maintenance lock: %w", err)
	}
	return file, nil
}

func (c *Controller) startLease() (*os.File, error) {
	path := c.opts.MaintenanceLockPath
	if path == "" {
		path = filepath.Join(c.opts.StateDir, "radio-maintenance.lock")
	}
	return AcquireStartLease(path)
}
