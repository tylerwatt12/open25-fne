package dispatch

import (
	"bufio"
	"context"
	"io"
	"net"
	"os"
	"os/exec"
	"strings"
	"testing"
	"time"
)

func TestCodecCompletionWinsWhenDeadlineIsAlreadyReady(t *testing.T) {
	done := make(chan struct{})
	close(done)
	for range 1000 {
		if !waitCodecDone(done, time.Now().Add(-time.Second)) {
			t.Fatal("confirmed process completion was reported as a timeout")
		}
	}
	if waitCodecDone(make(chan struct{}), time.Now().Add(-time.Second)) {
		t.Fatal("an incomplete process was reported as stopped")
	}
}

func shutdownCodec(t *testing.T) *Codec {
	t.Helper()
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	return &Codec{ctx: ctx, cancel: cancel, resources: map[int]*codecResource{}, events: make(chan CodecEvent, 1), frames: make(chan codecReceiveUpdate, 1)}
}

func shutdownResource(t *testing.T) *codecResource {
	t.Helper()
	read, err := net.ListenUDP("udp4", &net.UDPAddr{IP: net.IPv4(127, 0, 0, 1)})
	if err != nil {
		t.Fatal(err)
	}
	write, err := net.DialUDP("udp4", nil, read.LocalAddr().(*net.UDPAddr))
	if err != nil {
		read.Close()
		t.Fatal(err)
	}
	t.Cleanup(func() { read.Close(); write.Close() })
	return &codecResource{tg: 1001, read: read, write: write, done: make(chan struct{}), ready: true}
}

func unreapedProcess(t *testing.T) *os.Process {
	t.Helper()
	// A deliberately nonexistent PID models a child whose Wait has not joined.
	// It cannot signal this test process or another real process group.
	process, err := os.FindProcess(1 << 30)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = process.Release() })
	return process
}

func TestCodecCloseWaitsForDelayedReapWithinItsBudget(t *testing.T) {
	c := shutdownCodec(t)
	r := shutdownResource(t)
	r.command = &exec.Cmd{Process: unreapedProcess(t)}
	c.resources[r.tg] = r
	c.wg.Add(1)
	go func() {
		defer c.wg.Done()
		<-c.ctx.Done()
		close(r.done)
	}()
	if err := c.closeWithTimeouts(time.Second, time.Second); err != nil {
		t.Fatal("a child reaped during shutdown was reported as running:", err)
	}
	if c.Status()[0].Ready {
		t.Fatal("closed bridge remained ready")
	}
}

func TestCodecCloseBoundsAnUnreapedChildAndKeepsItsResult(t *testing.T) {
	c := shutdownCodec(t)
	r := shutdownResource(t)
	r.command = &exec.Cmd{Process: unreapedProcess(t)}
	c.resources[r.tg] = r
	c.wg.Add(1)
	defer func() { close(r.done); c.wg.Done(); c.wg.Wait() }()
	started := time.Now()
	err := c.closeWithTimeouts(0, 25*time.Millisecond)
	if err == nil || !strings.Contains(err.Error(), "process did not stop") {
		t.Fatal("unconfirmed child shutdown was not reported:", err)
	}
	if time.Since(started) > time.Second {
		t.Fatal("shutdown waited unboundedly for the child reaper")
	}
	if repeated := c.Close(); repeated != err {
		t.Fatal("repeated close discarded the original shutdown result:", repeated)
	}
}

func TestCodecCloseBoundsABlockedEventCallback(t *testing.T) {
	c := shutdownCodec(t)
	entered, release, exited := make(chan struct{}), make(chan struct{}), make(chan struct{})
	c.options.OnEvent = func(CodecEvent) { close(entered); <-release }
	c.wg.Add(1)
	go func() { defer close(exited); c.publishEvents() }()
	c.events <- CodecEvent{State: "ready"}
	<-entered
	defer func() { close(release); <-exited }()
	started := time.Now()
	err := c.closeWithTimeouts(0, 25*time.Millisecond)
	if err == nil || !strings.Contains(err.Error(), "workers did not stop") {
		t.Fatal("blocked callback was incorrectly reported as a running native child:", err)
	}
	if time.Since(started) > time.Second {
		t.Fatal("shutdown waited unboundedly for a callback")
	}
}

func TestCodecCloseUnblocksAnInheritedOutputPipe(t *testing.T) {
	c := shutdownCodec(t)
	r := shutdownResource(t)
	reader, writer, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	defer writer.Close() // Remains open until after Close, as an inherited writer would.
	log, err := os.CreateTemp(t.TempDir(), "bridge-log-")
	if err != nil {
		t.Fatal(err)
	}
	r.output = reader
	c.resources[r.tg] = r
	c.wg.Add(1)
	log.Close()
	go c.scanOutput(r, reader, log.Name())
	if err = c.closeWithTimeouts(0, time.Second); err != nil {
		t.Fatal("the retained log writer prevented shutdown:", err)
	}
}

func TestPacketCodecCloseBoundsItsChildAndUnblocksPendingOutput(t *testing.T) {
	reader, writer, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	defer writer.Close()
	c := &PacketCodec{command: &exec.Cmd{Process: unreapedProcess(t)}, output: bufio.NewReader(reader), outputPipe: reader, done: make(chan struct{})}
	readDone := make(chan error, 1)
	go func() { _, err := c.output.ReadByte(); readDone <- err }()
	started := time.Now()
	err = c.closeWithTimeout(25 * time.Millisecond)
	if err == nil || !strings.Contains(err.Error(), "process did not stop") {
		t.Fatal("unreaped packet helper was not reported:", err)
	}
	if time.Since(started) > time.Second {
		t.Fatal("packet helper shutdown exceeded its bound")
	}
	select {
	case err := <-readDone:
		if err == nil || err == io.EOF {
			t.Fatal("the pending binary read was not explicitly closed:", err)
		}
	case <-time.After(time.Second):
		t.Fatal("packet helper shutdown left a binary read blocked")
	}
	if repeated := c.Close(); repeated != err {
		t.Fatal("repeated packet close discarded its first result:", repeated)
	}
	close(c.done)
}
