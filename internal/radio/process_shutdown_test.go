//go:build linux || darwin

package radio

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

type shutdownLease struct {
	closed atomic.Int32
	check  func()
}

func (l *shutdownLease) Close() error {
	if l.check != nil {
		l.check()
	}
	l.closed.Add(1)
	return nil
}

func currentObservedChild(t *testing.T, c *Controller, id, role string) *child {
	t.Helper()
	c.mu.Lock()
	defer c.mu.Unlock()
	dir := ""
	if c.rootDir != "" {
		dir = filepath.Join(c.rootDir, id)
	}
	source := &child{id: id, role: role, dir: dir, done: make(chan struct{}), groupRetired: true}
	t.Cleanup(func() { close(source.done) })
	if role == "media" {
		c.fne = source
	} else {
		c.children[id] = source
	}
	return source
}

func awaitChild(t *testing.T, done <-chan struct{}, what string) {
	t.Helper()
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal(what)
	}
}

func stopTestChild(t *testing.T, process *child) {
	t.Helper()
	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
		defer cancel()
		if err := process.stop(ctx, false); err != nil {
			t.Error(err)
		}
	})
}

func TestChildShutdownReapsDespiteBlockedObserver(t *testing.T) {
	entered, unblock := make(chan struct{}), make(chan struct{})
	defer close(unblock)
	process, err := launch("/bin/sh", t.TempDir(), "printf 'last native line\\n'", "voice-1", "voice", nil, func(_ *child, _ string) {
		close(entered)
		<-unblock
	})
	if err != nil {
		t.Fatal(err)
	}
	lease := &shutdownLease{}
	process.mu.Lock()
	process.lease = lease
	process.mu.Unlock()
	awaitChild(t, entered, "native observer did not enter")
	awaitChild(t, process.done, "reaped native process was held by its observer")
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	started := time.Now()
	if err = process.stop(ctx, false); err != nil {
		t.Fatal(err)
	}
	if time.Since(started) > 500*time.Millisecond || lease.closed.Load() != 1 || process.alive() {
		t.Fatal("blocked observer retained native ownership")
	}
	select {
	case <-process.outputDone:
		t.Fatal("test observer unexpectedly completed")
	default:
	}
	if _, err = process.output.Stat(); !errors.Is(err, os.ErrClosed) {
		t.Fatalf("owned output reader remained open: %v", err)
	}
	// Once ownership is retired, a repeated stop must not touch that numeric
	// process group again. It may have been reused by the operating system.
	process.cmd.Process = nil
	if err = process.stop(ctx, false); err != nil || lease.closed.Load() != 1 {
		t.Fatal("repeated stop did not preserve retired ownership", err)
	}
}

func TestChildShutdownRetiresInheritedWriterBeforeLeaseRelease(t *testing.T) {
	process, err := launch("/bin/sh", t.TempDir(), "(trap '' INT TERM HUP; exec sleep 30) & printf 'writer ready\\n'", "voice-1", "voice", nil, func(_ *child, _ string) {})
	if err != nil {
		t.Fatal(err)
	}
	stopTestChild(t, process)
	awaitChild(t, process.done, "leader exit waited for inherited stdout")
	if alive, err := processGroupAlive(process.cmd.Process.Pid); err != nil || !alive {
		t.Fatalf("test did not retain its inherited writer: alive=%v err=%v", alive, err)
	}
	lease := &shutdownLease{check: func() {
		if alive, err := processGroupAlive(process.cmd.Process.Pid); err != nil || alive {
			t.Errorf("lease released with a live process-group member: alive=%v err=%v", alive, err)
		}
	}}
	process.mu.Lock()
	process.lease = lease
	process.mu.Unlock()
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	if err = process.stop(ctx, false); err != nil {
		t.Fatal(err)
	}
	if lease.closed.Load() != 1 || !process.groupRetired {
		t.Fatal("inherited writer cleanup retained ownership")
	}
	awaitChild(t, process.outputDone, "owned reader did not close after child exit")
}

func TestChildShutdownPreservesFinalIdentificationOutput(t *testing.T) {
	lines := make(chan string, 8)
	process, err := launch("/bin/sh", t.TempDir(), "trap 'printf \"I: CW, state idle\\n\"; exit 0' INT; printf 'ready\\n'; while :; do sleep 0.05; done", "voice-1", "voice", nil, func(_ *child, line string) { lines <- line })
	if err != nil {
		t.Fatal(err)
	}
	stopTestChild(t, process)
	select {
	case <-lines:
	case <-time.After(2 * time.Second):
		t.Fatal("native helper did not start")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	if err = process.stop(ctx, true); err != nil {
		t.Fatal(err)
	}
	select {
	case line := <-lines:
		if line != "I: CW, state idle" {
			t.Fatal("unexpected final output", line)
		}
	default:
		t.Fatal("final station identification output was discarded")
	}
}

func TestLateNativeObservationCannotMutateReplacementRun(t *testing.T) {
	c := testController(t, nil)
	oldDir, newDir := t.TempDir(), t.TempDir()
	c.mu.Lock()
	c.rootDir = oldDir
	c.runID = "old-run"
	c.profile = fixtureProfile()
	c.status.State = "running"
	c.status.Channels = []ChannelStatus{{ID: "voice-1", State: "ready"}}
	c.mu.Unlock()
	old := currentObservedChild(t, c, "voice-1", "voice")
	c.mu.Lock()
	c.rootDir = newDir
	c.runID = "new-run"
	c.mu.Unlock()
	current := currentObservedChild(t, c, "voice-1", "voice")
	sequence := c.sequence.Load()
	for _, line := range []string{
		"I: CW, state identifying",
		"I: (HOST) P25 Affiliation, group affiliation, srcId = 10001, dstId = 1001",
		"I: (RF) P25 Voice Call, srcId = 10001, dstId = 1001",
	} {
		c.observe(old, line)
	}
	c.applyAffiliations(old, map[string]json.RawMessage{"affiliations": json.RawMessage(`[{"srcId":10001,"grpId":1001}]`)}, c.SourceState().Revision)
	if status := c.Status(); status.Channels[0].Identifying || status.Channels[0].Busy || len(status.Affiliations) != 0 {
		t.Fatal("late native output changed a replacement run", status)
	}
	waitRadio(t, "late output archive did not drain to its original run", func() bool {
		raw, err := os.ReadFile(filepath.Join(oldDir, "native-voice-1.log"))
		return err == nil && strings.Contains(string(raw), "state identifying")
	})
	if raw, err := os.ReadFile(filepath.Join(oldDir, "native-voice-1.log")); err != nil || !strings.Contains(string(raw), "state identifying") {
		t.Fatal("late output did not remain in its original log", err)
	}
	if _, err := os.Stat(filepath.Join(newDir, "native-voice-1.log")); !os.IsNotExist(err) {
		t.Fatal("late output reached replacement run log", err)
	}
	if c.sequence.Load() != sequence {
		t.Fatal("late output emitted a replacement-run event")
	}
	c.observe(current, "I: (HOST) P25 Affiliation, group affiliation, srcId = 10001, dstId = 1001")
	if len(c.Affiliations()) != 1 {
		t.Fatal("current native observation was dropped")
	}
}

func TestStoppedNativeObservationCannotReviveFaultedChannel(t *testing.T) {
	c := testController(t, nil)
	c.mu.Lock()
	c.profile = fixtureProfile()
	c.status.State = "running"
	c.status.Channels = []ChannelStatus{{ID: "voice-1", State: "fault"}}
	c.mu.Unlock()
	source := currentObservedChild(t, c, "voice-1", "voice")
	source.mu.Lock()
	source.intentional = true
	source.mu.Unlock()
	sequence := c.sequence.Load()
	c.observe(source, "I: (RF) P25 Voice Call, srcId = 10001, dstId = 1001")
	c.observe(source, "I: (HOST) P25 Affiliation, group affiliation, srcId = 10001, dstId = 1001")
	c.applyAffiliations(source, map[string]json.RawMessage{"affiliations": json.RawMessage(`[{"srcId":10001,"grpId":1001}]`)}, c.SourceState().Revision)
	c.applyNativeStatus(source, map[string]json.RawMessage{"tx": json.RawMessage(`true`)})
	if status := c.Status(); status.Channels[0].Busy || status.Channels[0].Ready || len(status.Affiliations) != 0 || c.sequence.Load() != sequence {
		t.Fatal("stopped engine output revived a faulted channel", status)
	}
}
