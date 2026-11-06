package radio

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"golang.org/x/sys/unix"
)

func maintenanceOwner(t *testing.T, path string) *os.File {
	t.Helper()
	file, err := os.OpenFile(path, os.O_CREATE|os.O_RDWR, 0600)
	if err != nil {
		t.Fatal(err)
	}
	if err = unix.Flock(int(file.Fd()), unix.LOCK_EX|unix.LOCK_NB); err != nil {
		_ = file.Close()
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = file.Close() })
	return file
}

func TestMaintenanceProcessHolder(t *testing.T) {
	path := os.Getenv("TS_TEST_MAINTENANCE_HOLDER")
	if path == "" {
		return
	}
	file := maintenanceOwner(t, path)
	defer file.Close()
	_, _ = os.Stdout.WriteString("maintenance-ready\n")
	var release [1]byte
	_, _ = os.Stdin.Read(release[:])
}

func TestMaintenanceSharedDescriptorsAndExclusiveOwner(t *testing.T) {
	path := filepath.Join(t.TempDir(), "radio-maintenance.lock")
	one, err := AcquireStartLease(path)
	if err != nil {
		t.Fatal(err)
	}
	defer one.Close()
	two, err := AcquireStartLease(path)
	if err != nil {
		t.Fatal(err)
	}
	defer two.Close()
	owner, err := os.OpenFile(path, os.O_RDWR, 0)
	if err != nil {
		t.Fatal(err)
	}
	defer owner.Close()
	blocked := func() {
		t.Helper()
		if err := unix.Flock(int(owner.Fd()), unix.LOCK_EX|unix.LOCK_NB); !errors.Is(err, unix.EWOULDBLOCK) {
			t.Fatalf("exclusive owner passed an active starter: %v", err)
		}
	}
	blocked()
	_ = one.Close()
	blocked()
	_ = two.Close()
	if err = unix.Flock(int(owner.Fd()), unix.LOCK_EX|unix.LOCK_NB); err != nil {
		t.Fatal(err)
	}
	if _, err = AcquireStartLease(path); !errors.Is(err, ErrMaintenance) {
		t.Fatalf("maintenance rejection lost its sentinel: %v", err)
	}
	_ = owner.Close()
	lease, err := AcquireStartLease(path)
	if err != nil {
		t.Fatal(err)
	}
	_ = lease.Close()
}

func TestMaintenanceExclusiveOwnerInAnotherProcess(t *testing.T) {
	path := filepath.Join(t.TempDir(), "radio-maintenance.lock")
	command := exec.Command(os.Args[0], "-test.run=^TestMaintenanceProcessHolder$")
	command.Env = append(os.Environ(), "TS_TEST_MAINTENANCE_HOLDER="+path)
	input, err := command.StdinPipe()
	if err != nil {
		t.Fatal(err)
	}
	output, err := command.StdoutPipe()
	if err != nil {
		t.Fatal(err)
	}
	command.Stderr = os.Stderr
	if err = command.Start(); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = input.Close(); _ = command.Process.Kill(); _ = command.Wait() })
	ready := make(chan bool, 1)
	go func() { line, _ := bufio.NewReader(output).ReadString('\n'); ready <- line == "maintenance-ready\n" }()
	select {
	case okay := <-ready:
		if !okay {
			t.Fatal("separate maintenance owner did not become ready")
		}
	case <-time.After(5 * time.Second):
		t.Fatal("separate maintenance owner timed out")
	}
	if _, err = AcquireStartLease(path); !errors.Is(err, ErrMaintenance) {
		t.Fatalf("another process did not exclude startup: %v", err)
	}
	_ = input.Close()
	if err = command.Wait(); err != nil {
		t.Fatal(err)
	}
	lease, err := AcquireStartLease(path)
	if err != nil {
		t.Fatal(err)
	}
	_ = lease.Close()
}

func TestMaintenanceRejectsUnsafeInodesWithoutChangingThem(t *testing.T) {
	for _, kind := range []string{"public-file", "symlink", "hardlink", "directory", "writable-parent"} {
		t.Run(kind, func(t *testing.T) {
			dir := t.TempDir()
			path := filepath.Join(dir, "radio-maintenance.lock")
			if err := os.WriteFile(path, []byte("preserve"), 0600); err != nil {
				t.Fatal(err)
			}
			switch kind {
			case "public-file":
				_ = os.Chmod(path, 0644)
			case "symlink":
				_ = os.Rename(path, path+".original")
				_ = os.Symlink(path+".original", path)
			case "hardlink":
				if err := os.Link(path, path+".second"); err != nil {
					t.Fatal(err)
				}
			case "directory":
				_ = os.Remove(path)
				_ = os.Mkdir(path, 0700)
			case "writable-parent":
				_ = os.Chmod(dir, 0770)
			}
			if file, err := AcquireStartLease(path); err == nil {
				_ = file.Close()
				t.Fatal("unsafe lock accepted")
			}
			if kind != "directory" {
				raw, err := os.ReadFile(path)
				if err != nil || string(raw) != "preserve" {
					t.Fatal("rejected lock was changed")
				}
			}
		})
	}
}

func TestMaintenanceControllerAdmissionDoesNotCreateDesiredIntent(t *testing.T) {
	c := testController(t, nil)
	c.opts.MaintenanceLockPath = filepath.Join(c.opts.StateDir, "radio-maintenance.lock")
	owner := maintenanceOwner(t, c.opts.MaintenanceLockPath)
	if err := c.Start(context.Background(), fixtureProfile(), "null"); !errors.Is(err, ErrMaintenance) {
		t.Fatalf("Site start passed maintenance: %v", err)
	}
	if err := c.StartChannel(context.Background(), "voice-1"); !errors.Is(err, ErrMaintenance) {
		t.Fatalf("channel start passed maintenance: %v", err)
	}
	c.mu.Lock()
	wanted, attempt, channels := c.siteWanted, c.siteRecovery, len(c.recovery)
	c.mu.Unlock()
	if wanted || attempt != nil || channels != 0 || c.Status().State != "stopped" {
		t.Fatal("rejected start recorded desired operation")
	}
	if err := c.Stop(context.Background()); err != nil {
		t.Fatalf("Stop blocked by maintenance: %v", err)
	}
	_ = owner.Close()
	c.recoverVoiceChannels(time.Now().Add(time.Hour))
	if c.Status().State != "stopped" {
		t.Fatal("maintenance release started the Site")
	}
	if err := c.Start(context.Background(), fixtureProfile(), "null"); err != nil {
		t.Fatalf("fresh explicit start failed: %v", err)
	}
}

func TestMaintenanceCapabilityRequiresConfiguredControllerPath(t *testing.T) {
	c := testController(t, nil)
	if c.Status().MaintenanceSupported {
		t.Fatal("standalone default advertised Site maintenance pairing")
	}
	c.opts.MaintenanceLockPath = filepath.Join(c.opts.StateDir, "radio-maintenance.lock")
	raw, err := json.Marshal(c.Status())
	if err != nil || !c.Status().MaintenanceSupported || !strings.Contains(string(raw), `"maintenance_supported":true`) {
		t.Fatalf("configured support missing: %s %v", raw, err)
	}
}

func TestMaintenanceControlRecoveryWithdrawsInsteadOfReplaying(t *testing.T) {
	c := testController(t, nil)
	owner := maintenanceOwner(t, filepath.Join(c.opts.StateDir, "radio-maintenance.lock"))
	ctx, cancel := context.WithCancel(c.ctx)
	attempt := &siteAttempt{ctx: ctx, cancel: cancel, done: make(chan struct{}), epoch: c.stopEpoch, profile: fixtureProfile(), mode: "null"}
	c.mu.Lock()
	c.siteWanted, c.siteRecovery = true, attempt
	c.status.State = "fault"
	c.mu.Unlock()
	c.retryWait = func(ctx context.Context, _ time.Duration) error { return ctx.Err() }
	go c.recoverSite(attempt)
	select {
	case <-attempt.done:
	case <-time.After(3 * time.Second):
		t.Fatal("maintenance did not terminate control retry")
	}
	c.mu.Lock()
	wanted, retry := c.siteWanted, c.siteRecovery
	c.mu.Unlock()
	if wanted || retry != nil || ctx.Err() == nil || c.Status().FNEPID != 0 {
		t.Fatal("maintenance retained replayable control intent")
	}
	_ = owner.Close()
	if c.Status().State == "running" {
		t.Fatal("maintenance release replayed control recovery")
	}
}

func TestMaintenanceVoiceRetryWithdrawsOnlyForExclusiveOwner(t *testing.T) {
	for _, busy := range []bool{false, true} {
		t.Run(map[bool]string{false: "ordinary-guard-error-retains-intent", true: "exclusive-owner-withdraws-intent"}[busy], func(t *testing.T) {
			c := testController(t, nil)
			path := filepath.Join(c.opts.StateDir, "radio-maintenance.lock")
			if busy {
				maintenanceOwner(t, path)
			} else if err := os.WriteFile(path, nil, 0644); err != nil {
				t.Fatal(err)
			}
			ctx, cancel := context.WithCancel(c.ctx)
			defer cancel()
			attempt := &channelAttempt{run: "retry", ctx: ctx, cancel: cancel}
			c.mu.Lock()
			c.runID, c.runContext, c.status.State = "retry", ctx, "running"
			c.recovery = map[string]*channelRecovery{"voice-1": {wanted: true, attempt: attempt}}
			c.mu.Unlock()
			err := c.startChannel(ctx, "voice-1", attempt)
			if err == nil || errors.Is(err, ErrMaintenance) != busy {
				t.Fatalf("wrong startup rejection: %v", err)
			}
			c.finishChannelAttempt("voice-1", attempt, err)
			c.mu.Lock()
			wanted := c.recovery["voice-1"].wanted
			c.mu.Unlock()
			if wanted == busy {
				t.Fatalf("wrong desired retry after %v: %v", err, wanted)
			}
		})
	}
}

func TestMaintenanceCancelledChannelCallerCannotBecomeAutomaticRetry(t *testing.T) {
	for _, cancelled := range []bool{false, true} {
		t.Run(map[bool]string{false: "ordinary-failure-retains-desired", true: "cancelled-caller-withdraws-desired"}[cancelled], func(t *testing.T) {
			c := testController(t, nil)
			run, stop := context.WithCancel(c.ctx)
			defer stop()
			ctx, cancel := context.WithCancel(run)
			attempt := &channelAttempt{run: "owned", ctx: ctx, cancel: cancel}
			c.mu.Lock()
			c.runID, c.runContext, c.status.State = "owned", run, "running"
			c.recovery = map[string]*channelRecovery{"voice-1": {wanted: true, attempt: attempt}}
			c.mu.Unlock()
			if cancelled {
				cancel()
			}
			c.finishChannelAttempt("voice-1", attempt, errors.New("startup failed"))
			c.mu.Lock()
			wanted := c.recovery["voice-1"].wanted
			next := c.recovery["voice-1"].next
			c.mu.Unlock()
			if wanted == cancelled || (!cancelled && next.IsZero()) {
				t.Fatalf("wrong retry intent: wanted%v next%v", wanted, next)
			}
		})
	}
}
