//go:build linux

package radio

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
)

func childAttributes() *syscall.SysProcAttr {
	return &syscall.SysProcAttr{Setpgid: true, Pdeathsig: syscall.SIGTERM}
}

func processGroupAlive(pgid int) (bool, error) {
	if err := syscall.Kill(-pgid, 0); errors.Is(err, syscall.ESRCH) {
		return false, nil
	} else if err != nil {
		return false, err
	}
	entries, err := os.ReadDir("/proc")
	if err != nil {
		return false, err
	}
	for _, entry := range entries {
		if _, err := strconv.Atoi(entry.Name()); err != nil {
			continue
		}
		raw, err := os.ReadFile(filepath.Join("/proc", entry.Name(), "stat"))
		if errors.Is(err, os.ErrNotExist) || errors.Is(err, syscall.ESRCH) {
			continue
		}
		if err != nil {
			return false, err
		}
		// The parenthesized command can itself contain spaces and ')'. The
		// state, parent PID and process group follow its final delimiter.
		end := strings.LastIndexByte(string(raw), ')')
		if end < 0 {
			return false, fmt.Errorf("invalid process state for %s", entry.Name())
		}
		fields := strings.Fields(string(raw[end+1:]))
		if len(fields) < 3 {
			return false, fmt.Errorf("incomplete process state for %s", entry.Name())
		}
		group, err := strconv.Atoi(fields[2])
		if err != nil {
			return false, err
		}
		if group == pgid && fields[0] != "Z" && fields[0] != "X" {
			return true, nil
		}
	}
	return false, nil
}
