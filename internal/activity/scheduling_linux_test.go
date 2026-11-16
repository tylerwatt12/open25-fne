//go:build linux

package activity

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"syscall"
	"testing"

	"open25.local/open25-fne/internal/historypriority"
)

func TestHistoryInitializationMutationAndCloseUseOneLowPriorityWorker(t *testing.T) {
	runtime.LockOSThread()
	defer runtime.UnlockOSThread()
	caller := syscall.Gettid()
	before, err := syscall.Getpriority(syscall.PRIO_PROCESS, caller)
	if err != nil {
		t.Fatal(err)
	}
	type observed struct{ tid, nice, io int }
	turns := []observed{}
	s, err := open(filepath.Join(t.TempDir(), "activity.db"), true, func(ctx context.Context) (*historypriority.Turn, error) {
		tid := syscall.Gettid()
		raw, err := os.ReadFile(fmt.Sprintf("/proc/self/task/%d/stat", tid))
		if err != nil {
			return nil, err
		}
		fields := strings.Fields(string(raw)[strings.LastIndex(string(raw), ") ")+2:])
		nice, err := strconv.Atoi(fields[16])
		if err != nil {
			return nil, err
		}
		io, _, errno := syscall.Syscall(syscall.SYS_IOPRIO_GET, 1, uintptr(tid), 0)
		if errno != 0 {
			return nil, errno
		}
		turns = append(turns, observed{tid, nice, int(io)})
		return historypriority.Acquire(ctx)
	})
	if err != nil {
		t.Fatal(err)
	}
	if err = s.Append(context.Background(), Event{ID: "worker-owned", Origin: "site-1", Type: "status"}); err != nil {
		t.Fatal(err)
	}
	if err = s.Close(); err != nil {
		t.Fatal(err)
	}
	after, err := syscall.Getpriority(syscall.PRIO_PROCESS, caller)
	if err != nil || before != after {
		t.Fatalf("history changed the caller thread priority: before=%d after=%d error=%v", before, after, err)
	}
	if len(turns) < 3 {
		t.Fatalf("initialization, mutation or close bypassed scheduling: %+v", turns)
	}
	for _, turn := range turns {
		if turn.tid == caller || turn.tid != turns[0].tid || turn.nice != 19 || turn.io != (2<<13)|7 {
			t.Fatalf("history lifecycle escaped its one low-priority worker: caller=%d turns=%+v", caller, turns)
		}
	}
}
