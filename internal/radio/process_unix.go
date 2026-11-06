//go:build linux || darwin

package radio

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"syscall"
	"time"
)

type child struct {
	cmd                   *exec.Cmd
	done                  chan struct{}
	ready                 chan struct{}
	readyOnce             sync.Once
	stopMu                sync.Mutex
	groupRetired          bool
	output                *os.File
	outputDone            chan struct{}
	exitErr               error
	mu                    sync.Mutex
	id, role, dir         string
	client                *NativeClient
	started               time.Time
	lease                 io.Closer
	intentional           bool
	lastSeen              time.Time
	zoneApplied           bool
	zoneState             bool
	inhibitionApplied     bool
	inhibitionMirror      []InhibitRestore
	inhibitionDesired     []int
	inhibitionInitialized bool
	inhibitionClears      map[int]bool
	device                string
	sameDevice            func(io.Closer, string) bool
	stopGrace             time.Duration
	admissionError        string
}

func nativeExecutable(binary string) (string, error) {
	if binary == "" {
		return "", errors.New("native executable is not configured")
	}
	path, err := filepath.Abs(binary)
	if err != nil {
		return "", err
	}
	info, err := os.Stat(path)
	if err != nil || !info.Mode().IsRegular() || info.Mode().Perm()&0111 == 0 {
		return "", errors.New("native executable is missing or not executable")
	}
	return path, nil
}

func launch(binary, dir, config, id, role string, client *NativeClient, line func(*child, string), extraArgs ...string) (*child, error) {
	path, err := nativeExecutable(binary)
	if err != nil {
		return nil, err
	}
	read, write, err := os.Pipe()
	if err != nil {
		return nil, err
	}
	cmd := exec.Command(path, append([]string{"-c", config}, extraArgs...)...)
	cmd.Dir = dir
	cmd.Stdout = write
	cmd.Stderr = write
	cmd.SysProcAttr = childAttributes()
	if err = cmd.Start(); err != nil {
		read.Close()
		write.Close()
		return nil, err
	}
	write.Close()
	c := &child{cmd: cmd, done: make(chan struct{}), ready: make(chan struct{}), output: read, outputDone: make(chan struct{}), id: id, role: role, dir: dir, client: client, started: time.Now().UTC()}
	go func() {
		defer read.Close()
		defer close(c.outputDone)
		collectLines(read, func(text string) {
			if strings.Contains(text, "Host is up and running on ") || strings.Contains(text, "FNE is up and running on ") || (role == "c4fm-test" && strings.Contains(text, "C4FM_TEST ready")) {
				c.readyOnce.Do(func() { close(c.ready) })
			}
			line(c, text)
		})
	}()
	go func() {
		err := cmd.Wait()
		c.mu.Lock()
		c.exitErr = err
		c.mu.Unlock()
		// Ownership follows the OS process, not a scanner callback or a pipe
		// inherited by another process. Neither can delay reporting its exit.
		close(c.done)
		c.finishOutput(time.Now().Add(100 * time.Millisecond))
	}()
	return c, nil
}

func (c *child) alive() bool {
	select {
	case <-c.done:
		return false
	default:
		return true
	}
}

func (c *child) acceptsObservations() bool {
	c.mu.Lock()
	defer c.mu.Unlock()
	return !c.intentional && c.alive()
}

func (c *child) readyNow() bool {
	select {
	case <-c.ready:
		return c.alive()
	default:
		return false
	}
}
func (c *child) waitReady(ctx context.Context) error {
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-c.done:
		c.mu.Lock()
		defer c.mu.Unlock()
		if c.exitErr == nil {
			return errors.New("native process exited during startup")
		}
		return fmt.Errorf("native process exited during startup: %w", c.exitErr)
	case <-c.ready:
		if !c.alive() {
			return errors.New("native process exited after startup")
		}
		return nil
	}
}
func (c *child) stop(ctx context.Context, identification bool) error {
	c.stopMu.Lock()
	defer c.stopMu.Unlock()
	c.mu.Lock()
	c.intentional = true
	c.mu.Unlock()
	if c.groupRetired {
		return c.releaseLease()
	}
	var deadline time.Time
	if c.alive() {
		// SIGINT permits bounded final station identification; SIGTERM is the
		// fail-closed path for failed startup or a revoked policy.
		signal := syscall.SIGTERM
		grace := 2 * time.Second
		if c.stopGrace > 0 {
			grace = c.stopGrace
		}
		if identification {
			signal = syscall.SIGINT
			grace = 9 * time.Second
		}
		_ = syscall.Kill(-c.cmd.Process.Pid, signal)
		timer := time.NewTimer(grace)
		defer timer.Stop()
		select {
		case <-c.done:
		case <-ctx.Done():
			_ = syscall.Kill(-c.cmd.Process.Pid, syscall.SIGKILL)
		case <-timer.C:
			_ = syscall.Kill(-c.cmd.Process.Pid, syscall.SIGKILL)
		}
		deadline = time.Now().Add(2 * time.Second)
		reaped := time.NewTimer(time.Until(deadline))
		defer reaped.Stop()
		select {
		case <-c.done:
		case <-reaped.C:
			if c.alive() {
				return errors.New("native process could not be reaped; modem ownership is retained")
			}
		}
	} else {
		deadline = time.Now().Add(2 * time.Second)
	}
	// A reaped leader can leave a process in its group holding an inherited
	// modem or output descriptor. Retire that group before releasing its lease.
	for {
		alive, err := processGroupAlive(c.cmd.Process.Pid)
		if err != nil {
			return fmt.Errorf("native process group could not be checked; modem ownership is retained: %w", err)
		}
		if !alive {
			c.groupRetired = true
			c.finishOutput(minTime(deadline, time.Now().Add(100*time.Millisecond)))
			return c.releaseLease()
		}
		if err = syscall.Kill(-c.cmd.Process.Pid, syscall.SIGKILL); err != nil && !errors.Is(err, syscall.ESRCH) {
			return fmt.Errorf("native process group could not be stopped; modem ownership is retained: %w", err)
		}
		if time.Now().After(deadline) {
			return errors.New("native process group did not stop; modem ownership is retained")
		}
		time.Sleep(10 * time.Millisecond)
	}
}

func (c *child) finishOutput(deadline time.Time) {
	if c.output == nil {
		return
	}
	left := time.Until(deadline)
	if left > 0 {
		timer := time.NewTimer(left)
		select {
		case <-c.outputDone:
		case <-timer.C:
		}
		timer.Stop()
	}
	_ = c.output.Close()
}

func minTime(a, b time.Time) time.Time {
	if a.Before(b) {
		return a
	}
	return b
}

func (c *child) releaseLease() error {
	c.mu.Lock()
	lease := c.lease
	c.mu.Unlock()
	if lease == nil {
		return nil
	}
	if err := lease.Close(); err != nil {
		return fmt.Errorf("modem lease could not be released; ownership is retained: %w", err)
	}
	c.mu.Lock()
	c.lease = nil
	c.mu.Unlock()
	return nil
}

func (c *child) validDevice() bool {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.lease == nil {
		return true
	}
	if c.sameDevice != nil {
		return c.sameDevice(c.lease, c.device)
	}
	return deviceSame(c.lease, c.device)
}
