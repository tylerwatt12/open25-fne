//go:build linux || darwin

package radio

import (
	"os"
	"path/filepath"
	"syscall"
	"testing"
	"time"
)

// This test body also runs against the unmodified emergency-source baseline.
func TestNativeArchiveCannotDelayCurrentRegistrationObservation(t *testing.T) {
	c := testController(t, nil)
	dir := t.TempDir()
	path := filepath.Join(dir, "native-control-1.log")
	if err := syscall.Mkfifo(path, 0600); err != nil {
		t.Fatal(err)
	}
	source := &child{id: "control-1", role: "control", dir: filepath.Join(dir, "control-1"), done: make(chan struct{}), groupRetired: true}
	c.mu.Lock()
	c.rootDir, c.runID, c.profile, c.status.State = dir, "archive-test", fixtureProfile(), "running"
	c.children[source.id] = source
	c.mu.Unlock()
	observed := make(chan struct{})
	defer func() {
		fd, err := syscall.Open(path, syscall.O_RDONLY|syscall.O_NONBLOCK, 0)
		if err != nil {
			t.Error(err)
			return
		}
		stop, drained := make(chan struct{}), make(chan struct{})
		go func() {
			defer close(drained)
			buffer := make([]byte, 8192)
			for {
				_, _ = syscall.Read(fd, buffer)
				select {
				case <-stop:
					return
				default:
					time.Sleep(time.Millisecond)
				}
			}
		}()
		select {
		case <-observed:
		case <-time.After(2 * time.Second):
			t.Error("observer did not return after archive release")
		}
		if err := c.Close(); err != nil {
			t.Error(err)
		}
		close(stop)
		<-drained
		_ = syscall.Close(fd)
		close(source.done)
	}()
	go func() {
		c.observe(source, "I: (HOST) P25 Affiliation, unit registration, srcId = 10001")
		close(observed)
	}()
	select {
	case <-observed:
	case <-time.After(500 * time.Millisecond):
		t.Fatal("diagnostic archive FIFO delayed synchronous registration observation")
	}
	if len(c.Status().Registrations) != 1 {
		t.Fatal("registration observation was deferred behind its archive copy")
	}
	if info, err := os.Stat(path); err != nil || info.Mode()&os.ModeNamedPipe == 0 {
		t.Fatal("fixture did not retain a real stalled FIFO", err)
	}
}
