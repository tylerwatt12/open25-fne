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
	"syscall"
	"testing"
	"time"
)

func foregroundGateChild(t *testing.T, path string, foreground, hold bool) (*exec.Cmd, io.WriteCloser, <-chan string) {
	t.Helper()
	command := exec.Command(os.Args[0], "-test.run=^TestGateSubprocess$", "-test.v=false")
	bit := func(value bool) int {
		if value {
			return 1
		}
		return 0
	}
	command.Env = append(os.Environ(), "OPEN25_GATE_TEST_PATH="+path, fmt.Sprintf("OPEN25_GATE_TEST_HOLD=%d", bit(hold)), fmt.Sprintf("OPEN25_GATE_TEST_FOREGROUND=%d", bit(foreground)))
	input, err := command.StdinPipe()
	if err != nil {
		t.Fatal(err)
	}
	output, err := command.StdoutPipe()
	if err != nil {
		t.Fatal(err)
	}
	var stderr bytes.Buffer
	command.Stderr = &stderr
	if err := command.Start(); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = input.Close(); _ = command.Process.Kill(); _ = command.Wait() })
	lines := make(chan string, 8)
	go func() {
		defer close(lines)
		scanner := bufio.NewScanner(output)
		for scanner.Scan() {
			lines <- scanner.Text()
		}
	}()
	return command, input, lines
}

func awaitGateChild(t *testing.T, lines <-chan string) {
	t.Helper()
	select {
	case line := <-lines:
		if line != "acquired" {
			t.Fatalf("child did not acquire: %q", line)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("waiting process made no progress")
	}
}

func awaitForegroundReservation(t *testing.T, path string) {
	t.Helper()
	file, err := openGate(path + ".foreground")
	if err != nil {
		t.Fatal(err)
	}
	defer file.Close()
	deadline := time.Now().Add(5 * time.Second)
	for {
		err := syscall.Flock(int(file.Fd()), syscall.LOCK_EX|syscall.LOCK_NB)
		if errors.Is(err, syscall.EWOULDBLOCK) || errors.Is(err, syscall.EAGAIN) {
			return
		}
		if err != nil {
			t.Fatal(err)
		}
		if err := syscall.Flock(int(file.Fd()), syscall.LOCK_UN); err != nil {
			t.Fatal(err)
		}
		if time.Now().After(deadline) {
			t.Fatal("foreground never reserved its queued turn")
		}
		time.Sleep(time.Millisecond)
	}
}

func TestForegroundReservationPrecedesCompetingBackgroundProcesses(t *testing.T) {
	path := filepath.Join(t.TempDir(), "mutation.lock")
	current, err := AcquirePath(context.Background(), path)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = current.Release() })
	foreground, input, entered := foregroundGateChild(t, path, true, true)
	awaitForegroundReservation(t, path)
	var background []*exec.Cmd
	var outputs []<-chan string
	for range 3 {
		child, in, lines := foregroundGateChild(t, path, false, false)
		_ = in.Close()
		background = append(background, child)
		outputs = append(outputs, lines)
	}
	if err := current.Release(); err != nil {
		t.Fatal(err)
	}
	awaitGateChild(t, entered)
	for _, lines := range outputs {
		select {
		case line := <-lines:
			t.Fatalf("background overtook a reserved foreground turn: %q", line)
		default:
		}
	}
	_ = input.Close()
	if err := foreground.Wait(); err != nil {
		t.Fatal(err)
	}
	for i, lines := range outputs {
		awaitGateChild(t, lines)
		if err := background[i].Wait(); err != nil {
			t.Fatal(err)
		}
	}
}

func TestQueuedForegroundCrashReleasesReservation(t *testing.T) {
	path := filepath.Join(t.TempDir(), "mutation.lock")
	current, err := AcquirePath(context.Background(), path)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = current.Release() })
	foreground, _, _ := foregroundGateChild(t, path, true, true)
	awaitForegroundReservation(t, path)
	if err := foreground.Process.Kill(); err != nil {
		t.Fatal(err)
	}
	_ = foreground.Wait()
	if err := current.Release(); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	next, err := AcquirePath(ctx, path)
	if err != nil {
		t.Fatal("dead foreground left scheduling intent", err)
	}
	if err := next.Release(); err != nil {
		t.Fatal(err)
	}
}

func TestActiveForegroundCrashReleasesBothLocks(t *testing.T) {
	path := filepath.Join(t.TempDir(), "mutation.lock")
	foreground, _, entered := foregroundGateChild(t, path, true, true)
	awaitGateChild(t, entered)
	if err := foreground.Process.Kill(); err != nil {
		t.Fatal(err)
	}
	_ = foreground.Wait()
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	next, err := AcquirePath(ctx, path)
	if err != nil {
		t.Fatal("dead foreground retained its turn", err)
	}
	if err := next.Release(); err != nil {
		t.Fatal(err)
	}
}

func TestForegroundReleaseHasNoHistoryRestAndCanceledBackgroundLeavesNoIntent(t *testing.T) {
	path := filepath.Join(t.TempDir(), "mutation.lock")
	foreground, err := AcquireForegroundPath(path)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = foreground.Release() })
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Millisecond)
	defer cancel()
	if turn, err := AcquirePath(ctx, path); turn != nil || !errors.Is(err, context.DeadlineExceeded) {
		t.Fatal("background ignored foreground reservation or cancellation", turn, err)
	}
	started := time.Now()
	if err := foreground.Release(); err != nil {
		t.Fatal(err)
	}
	if elapsed := time.Since(started); elapsed >= 200*time.Millisecond {
		t.Fatal("foreground inherited history quiet/yield", elapsed)
	}
	next, err := AcquirePath(context.Background(), path)
	if err != nil {
		t.Fatal(err)
	}
	if err := next.Release(); err != nil {
		t.Fatal(err)
	}
}

func TestForegroundReservationRefusesInsecureFilesWithoutRepair(t *testing.T) {
	for _, kind := range []string{"shared", "symlink", "hardlink"} {
		t.Run(kind, func(t *testing.T) {
			root := t.TempDir()
			path := filepath.Join(root, "mutation.lock")
			reservation := path + ".foreground"
			target := filepath.Join(root, "preserve")
			if err := os.WriteFile(target, []byte("unchanged"), 0600); err != nil {
				t.Fatal(err)
			}
			switch kind {
			case "shared":
				if err := os.WriteFile(reservation, []byte("unchanged"), 0644); err != nil {
					t.Fatal(err)
				}
			case "symlink":
				if err := os.Symlink(target, reservation); err != nil {
					t.Fatal(err)
				}
			case "hardlink":
				if err := os.Link(target, reservation); err != nil {
					t.Fatal(err)
				}
			}
			if turn, err := AcquireForegroundPath(path); err == nil {
				t.Fatal("foreground accepted insecure reservation", turn)
			}
			if turn, err := AcquirePath(context.Background(), path); err == nil {
				t.Fatal("background accepted insecure reservation", turn)
			}
			if bytes, err := os.ReadFile(target); err != nil || string(bytes) != "unchanged" {
				t.Fatal("refusal changed the existing target", err)
			}
		})
	}
}
