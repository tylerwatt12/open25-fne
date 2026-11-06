//go:build darwin

package radio

import (
	"syscall"

	"golang.org/x/sys/unix"
)

func childAttributes() *syscall.SysProcAttr { return &syscall.SysProcAttr{Setpgid: true} }

func processGroupAlive(pgid int) (bool, error) {
	processes, err := unix.SysctlKinfoProcSlice("kern.proc.pgrp", pgid)
	if err != nil {
		return false, err
	}
	for _, process := range processes {
		// Darwin's SZOMB state is 5. Zombies no longer own file descriptors.
		if int(process.Eproc.Pgid) == pgid && process.Proc.P_stat != 5 {
			return true, nil
		}
	}
	return false, nil
}
