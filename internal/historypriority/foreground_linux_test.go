//go:build linux

package historypriority

import (
	"os"
	"path/filepath"
	"runtime"
	"syscall"
	"testing"
)

func TestForegroundRuntimeFilesArePrivateAndCallerPriorityIsUnchanged(t *testing.T) {
	parent := t.TempDir()
	if err := os.Chmod(parent, 0770); err != nil {
		t.Fatal(err)
	}
	path, err := runtimeGatePath(parent)
	if err != nil {
		t.Fatal(err)
	}
	runtime.LockOSThread()
	defer runtime.UnlockOSThread()
	tid := syscall.Gettid()
	before, err := syscall.Getpriority(syscall.PRIO_PROCESS, tid)
	if err != nil {
		t.Fatal(err)
	}
	ioBefore, _, ioErr := syscall.Syscall(syscall.SYS_IOPRIO_GET, 1, uintptr(tid), 0)
	if ioErr != 0 {
		t.Fatal(ioErr)
	}
	turn, err := AcquireForegroundPath(path)
	if err != nil {
		t.Fatal(err)
	}
	if err := turn.Release(); err != nil {
		t.Fatal(err)
	}
	after, err := syscall.Getpriority(syscall.PRIO_PROCESS, tid)
	if err != nil {
		t.Fatal(err)
	}
	ioAfter, _, ioErr := syscall.Syscall(syscall.SYS_IOPRIO_GET, 1, uintptr(tid), 0)
	if ioErr != 0 || before != after || ioBefore != ioAfter {
		t.Fatal("foreground changed the caller's thread priority", before, after, ioBefore, ioAfter, ioErr)
	}
	for name, want := range map[string]os.FileMode{parent: 0770, filepath.Dir(path): 0700, path: 0600, path + ".foreground": 0600} {
		info, err := os.Lstat(name)
		if err != nil || info.Mode().Perm() != want {
			t.Fatal("unexpected runtime permissions", name, info, err)
		}
	}
}
