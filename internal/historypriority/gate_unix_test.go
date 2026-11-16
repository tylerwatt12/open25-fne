//go:build linux || darwin

package historypriority

import (
	"bufio"
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestGateWaitCancellationLeavesNoFutureReservation(t *testing.T) {
	path := filepath.Join(t.TempDir(), "host.lock")
	first, err := AcquirePath(context.Background(), path)
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Millisecond)
	defer cancel()
	if next, err := AcquirePath(ctx, path); !errors.Is(err, context.DeadlineExceeded) || next != nil {
		t.Fatalf("cancelled waiter acquired a turn: %v %v", next, err)
	}
	if err = first.Release(); err != nil {
		t.Fatal(err)
	}
	next, err := AcquirePath(context.Background(), path)
	if err != nil {
		t.Fatal(err)
	}
	if err = next.Release(); err != nil {
		t.Fatal(err)
	}
}

func TestGateQuietIntervalSurvivesCallerCancellationWithoutChargingWait(t *testing.T) {
	path := filepath.Join(t.TempDir(), "host.lock")
	first, err := AcquirePath(context.Background(), path)
	if err != nil {
		t.Fatal(err)
	}
	type result struct {
		at   time.Time
		hold time.Duration
		err  error
	}
	done := make(chan result, 1)
	entered := make(chan struct{}, 1)
	go func() {
		ctx, cancel := context.WithCancel(context.Background())
		turn, err := AcquirePath(ctx, path)
		if err != nil {
			cancel()
			done <- result{err: err}
			return
		}
		acquired := time.Now()
		entered <- struct{}{}
		cancel() // HTTP callers cancel after receiving their durable result.
		time.Sleep(10 * time.Millisecond)
		err = turn.Release()
		done <- result{at: acquired, hold: time.Since(acquired), err: err}
	}()
	// Make queued wait exceed the intended quiet interval. It must not inflate
	// the next worker's measured work/rest time after the lock is acquired.
	time.Sleep(400 * time.Millisecond)
	if err = first.Release(); err != nil {
		t.Fatal(err)
	}
	select {
	case <-entered:
	case <-time.After(2 * time.Second):
		t.Fatal("waiting owner did not acquire after release")
	}
	short, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
	defer cancel()
	if turn, err := AcquirePath(short, path); err == nil {
		_ = turn.Release()
		t.Fatal("cancelled caller released the host quiet interval early")
	}
	got := <-done
	if got.err != nil || got.hold < 300*time.Millisecond || got.hold > 650*time.Millisecond {
		t.Fatalf("quiet interval included queue wait or was bypassed: hold=%v error=%v", got.hold, got.err)
	}
}

func TestGateRefusesUnownedShapesWithoutRepair(t *testing.T) {
	directory := t.TempDir()
	file := filepath.Join(directory, "target")
	if err := os.WriteFile(file, []byte("unchanged"), 0600); err != nil {
		t.Fatal(err)
	}
	link := filepath.Join(directory, "symlink")
	if err := os.Symlink(file, link); err != nil {
		t.Fatal(err)
	}
	if turn, err := AcquirePath(context.Background(), link); err == nil {
		_ = turn.Release()
		t.Fatal("accepted a symlink gate")
	}
	if err := os.Chmod(file, 0644); err != nil {
		t.Fatal(err)
	}
	if turn, err := AcquirePath(context.Background(), file); err == nil {
		_ = turn.Release()
		t.Fatal("accepted a shared gate")
	}
	if err := os.Chmod(file, 0600); err != nil {
		t.Fatal(err)
	}
	hard := filepath.Join(directory, "hardlink")
	if err := os.Link(file, hard); err != nil {
		t.Fatal(err)
	}
	if turn, err := AcquirePath(context.Background(), hard); err == nil {
		_ = turn.Release()
		t.Fatal("accepted a multiply linked gate")
	}
	if content, err := os.ReadFile(file); err != nil || string(content) != "unchanged" {
		t.Fatal("refusing a gate modified its target", err)
	}
}

// This helper runs in a separate process so exclusion and crash release cannot
// accidentally pass through process-local coordination.
func TestGateSubprocess(t *testing.T) {
	path := os.Getenv("OPEN25_GATE_TEST_PATH")
	if path == "" {
		return
	}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	var turn *Turn
	var err error
	if os.Getenv("OPEN25_GATE_TEST_FOREGROUND") == "1" {
		turn, err = AcquireForegroundPath(path)
	} else {
		turn, err = AcquirePath(ctx, path)
	}
	if err != nil {
		t.Fatal(err)
	}
	fmt.Println("acquired")
	if os.Getenv("OPEN25_GATE_TEST_HOLD") == "1" {
		_, _ = io.Copy(io.Discard, os.Stdin)
	}
	if err = turn.Release(); err != nil {
		t.Fatal(err)
	}
}

func TestGateCrossProcessExclusionProgressAndCrashRelease(t *testing.T) {
	path := filepath.Join(t.TempDir(), "host.lock")
	start := func(hold bool) (*exec.Cmd, io.WriteCloser, <-chan string, *bytes.Buffer) {
		t.Helper()
		command := exec.Command(os.Args[0], "-test.run=^TestGateSubprocess$", "-test.v=false")
		command.Env = append(os.Environ(), "OPEN25_GATE_TEST_PATH="+path, fmt.Sprintf("OPEN25_GATE_TEST_HOLD=%d", map[bool]int{false: 0, true: 1}[hold]))
		in, err := command.StdinPipe()
		if err != nil {
			t.Fatal(err)
		}
		out, err := command.StdoutPipe()
		if err != nil {
			t.Fatal(err)
		}
		stderr := &bytes.Buffer{}
		command.Stderr = stderr
		if err = command.Start(); err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { _ = in.Close(); _ = command.Process.Kill(); _ = command.Wait() })
		lines := make(chan string, 8)
		go func() {
			defer close(lines)
			scanner := bufio.NewScanner(out)
			for scanner.Scan() {
				lines <- scanner.Text()
			}
		}()
		return command, in, lines, stderr
	}
	await := func(lines <-chan string) {
		t.Helper()
		select {
		case line := <-lines:
			if strings.TrimSpace(line) != "acquired" {
				t.Fatalf("subprocess did not acquire: %q", line)
			}
		case <-time.After(5 * time.Second):
			t.Fatal("a waiting process made no progress")
		}
	}
	holder, _, held, _ := start(true)
	await(held)
	processes := []*exec.Cmd{}
	outputs := []<-chan string{}
	for range 4 {
		command, input, lines, _ := start(false)
		_ = input.Close()
		processes = append(processes, command)
		outputs = append(outputs, lines)
	}
	time.Sleep(100 * time.Millisecond)
	for _, lines := range outputs {
		select {
		case line := <-lines:
			t.Fatalf("another process entered a held turn: %q", line)
		default:
		}
	}
	// The holder never executes Release; the kernel must retire its lock.
	if err := holder.Process.Kill(); err != nil {
		t.Fatal(err)
	}
	_ = holder.Wait()
	for i, lines := range outputs {
		await(lines)
		if err := processes[i].Wait(); err != nil {
			t.Fatal(err)
		}
	}
}
