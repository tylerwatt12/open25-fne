//go:build linux || darwin

package dispatch

import (
	"os"
	"path/filepath"
	"syscall"
	"testing"
	"time"

	"open25.local/open25-fne/internal/nativearchive"
)

func TestCodecArchiveCannotDelayCurrentReadyObservation(t *testing.T) {
	c := shutdownCodec(t)
	r := shutdownResource(t)
	r.ready = false
	c.resources[r.tg] = r
	c.archive = nativearchive.New()
	c.archiveReaders = 1
	path := filepath.Join(t.TempDir(), "codec.fifo")
	if err := syscall.Mkfifo(path, 0600); err != nil {
		t.Fatal(err)
	}
	fd, err := syscall.Open(path, syscall.O_RDWR|syscall.O_NONBLOCK, 0)
	if err != nil {
		t.Fatal(err)
	}
	buffer := make([]byte, 4096)
	filled := 0
	for {
		n, err := syscall.Write(fd, buffer)
		if n > 0 {
			filled += n
		}
		if err == syscall.EAGAIN {
			break
		}
		if err != nil {
			t.Fatal(err)
		}
	}
	if filled == 0 {
		t.Fatal("archive FIFO was not filled")
	}
	if err := syscall.SetNonblock(fd, false); err != nil {
		t.Fatal(err)
	}
	archiveFile := os.NewFile(uintptr(fd), path)
	reader, writer, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	r.output = reader
	defer func() {
		stop, drained := make(chan struct{}), make(chan struct{})
		go func() {
			defer close(drained)
			data := make([]byte, 16384)
			for {
				_ = syscall.SetNonblock(fd, true)
				_, _ = syscall.Read(fd, data)
				select {
				case <-stop:
					return
				default:
					time.Sleep(time.Millisecond)
				}
			}
		}()
		writer.Close()
		if err := c.closeWithTimeouts(time.Second, time.Second); err != nil {
			t.Error(err)
		}
		close(stop)
		<-drained
		archiveFile.Close()
	}()
	c.wg.Add(1)
	go c.scanOutput(r, reader, path)
	if _, err := writer.WriteString("logged into the master successfully\n"); err != nil {
		t.Fatal(err)
	}
	deadline := time.NewTimer(500 * time.Millisecond)
	defer deadline.Stop()
	tick := time.NewTicker(time.Millisecond)
	defer tick.Stop()
	for !c.Status()[0].Ready {
		select {
		case <-deadline.C:
			t.Fatal("full diagnostic FIFO delayed synchronous codec-ready observation")
		case <-tick.C:
		}
	}
}

func TestCodecArchiveDrainFailureIsNotOperationalFailure(t *testing.T) {
	c := shutdownCodec(t)
	c.archive = nativearchive.New()
	path := filepath.Join(t.TempDir(), "blocked.fifo")
	if err := syscall.Mkfifo(path, 0600); err != nil {
		t.Fatal(err)
	}
	c.archive.Copy(path, "line", false)
	defer func() {
		fd, err := syscall.Open(path, syscall.O_RDONLY|syscall.O_NONBLOCK, 0)
		if err != nil {
			t.Error(err)
			return
		}
		defer syscall.Close(fd)
		buffer := make([]byte, 16384)
		limit := time.Now().Add(2 * time.Second)
		for !c.DiagnosticArchiveStatus().DrainComplete && time.Now().Before(limit) {
			_, _ = syscall.Read(fd, buffer)
			time.Sleep(time.Millisecond)
		}
		if !c.DiagnosticArchiveStatus().DrainComplete {
			t.Error("owned archive did not finish after sink release")
		}
	}()
	if err := c.closeWithTimeouts(0, 25*time.Millisecond); err != nil {
		t.Fatal("archive-only stall became an operational codec error:", err)
	}
	status := c.DiagnosticArchiveStatus()
	if status.DrainComplete || status.LastDrainError == "" {
		t.Fatalf("archive drain failure was reported as graceful success: %+v", status)
	}
}
