//go:build linux

// Package historypriority confines best-effort scheduling changes to history
// workers. Go has no goroutine-level CPU/IO priority; never lower the controller
// process, since it also owns grant and airtime responses.
package historypriority

import (
	"errors"
	"runtime"
	"syscall"
)

// Enter must be called at the start of a dedicated history goroutine. Keep its
// OS thread locked until the goroutine exits, when Go retires that thread; never
// return a lowered thread to the scheduler for realtime/controller work.
func Enter() error {
	runtime.LockOSThread()
	tid := syscall.Gettid()
	cpuErr := syscall.Setpriority(syscall.PRIO_PROCESS, tid, 19)
	_, _, ioErr := syscall.Syscall(syscall.SYS_IOPRIO_SET, 1, uintptr(tid), (2<<13)|7)
	if ioErr != 0 {
		return errors.Join(cpuErr, ioErr)
	}
	return cpuErr
}
