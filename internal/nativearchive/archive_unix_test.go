//go:build linux || darwin

package nativearchive

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
	"time"
)

func releaseFIFO(t *testing.T, path string, done <-chan struct{}) {
	t.Helper()
	fd, err := syscall.Open(path, syscall.O_RDONLY|syscall.O_NONBLOCK, 0)
	if err != nil {
		t.Fatal(err)
	}
	drained := make(chan struct{})
	go func() {
		defer close(drained)
		defer syscall.Close(fd)
		buffer := make([]byte, 16384)
		for {
			_, _ = syscall.Read(fd, buffer)
			select {
			case <-done:
				return
			default:
				time.Sleep(time.Millisecond)
			}
		}
	}()
	select {
	case <-drained:
	case <-time.After(3 * time.Second):
		t.Fatal("archive writer did not retire after FIFO was released")
	}
}

func TestNativeArchiveStallCannotHoldDiagnosticProducer(t *testing.T) {
	path := filepath.Join(t.TempDir(), "archive.fifo")
	if err := syscall.Mkfifo(path, 0600); err != nil {
		t.Fatal(err)
	}
	w := New()
	defer func() { w.Stop(); releaseFIFO(t, path, w.Done()) }()
	started := time.Now()
	for range 256 {
		w.Copy(path, strings.Repeat("x", 256), false)
	}
	if time.Since(started) >= 500*time.Millisecond {
		t.Fatal("archive FIFO held producer admission")
	}
	w.Copy(path, strings.Repeat("x", MaxLineBytes), false)
	ctx, cancel := context.WithTimeout(context.Background(), 25*time.Millisecond)
	err := w.Close(ctx)
	cancel()
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("blocked archive was reported drained: %v", err)
	}
	status := w.Status()
	if status.DrainComplete || status.LastDrainError == "" || status.DroppedFull+status.DroppedBusy == 0 || status.DroppedOversize != 1 || status.Queued > Capacity {
		t.Fatalf("archive outcome is incomplete or unbounded: %+v", status)
	}
}

func TestNativeArchiveKeepsPerRunPathsRotationAndRedaction(t *testing.T) {
	oldPath, newPath := filepath.Join(t.TempDir(), "old.log"), filepath.Join(t.TempDir(), "new.log")
	original := strings.Repeat("a", 2<<20+1)
	if err := os.WriteFile(oldPath, []byte(original), 0600); err != nil {
		t.Fatal(err)
	}
	w := New()
	w.Copy(oldPath, Redact("password=private", ""), true)
	w.Copy(newPath, Redact("raw exact-credential here", "exact-credential"), false)
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	if err := w.Close(ctx); err != nil {
		t.Fatal(err)
	}
	for _, path := range []string{oldPath, newPath} {
		raw, err := os.ReadFile(path)
		if err != nil || string(raw) != "[Private native detail omitted]\n" {
			t.Fatalf("copy escaped redaction or its immutable path: %q %v", raw, err)
		}
	}
	raw, err := os.ReadFile(oldPath + ".previous")
	if err != nil || string(raw) != original {
		t.Fatal("rotation did not preserve previous diagnostic bytes", err)
	}
	w.Copy(oldPath, "late", true)
	if status := w.Status(); status.DroppedClosed != 1 || !status.DrainComplete || status.WriteErrors != 0 {
		t.Fatalf("post-close copy revived writer or hid its loss: %+v", status)
	}
}

func TestNativeArchiveWriteFailureIsSeparateFromDrain(t *testing.T) {
	w := New()
	w.Copy(filepath.Join(t.TempDir(), "missing", "archive.log"), "line", false)
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	if err := w.Close(ctx); err != nil {
		t.Fatal(err)
	}
	status := w.Status()
	if !status.DrainComplete || status.WriteErrors == 0 || status.LastWriteError == "" || status.Written != 0 {
		t.Fatalf("write failure was confused with completed drain: %+v", status)
	}
}
