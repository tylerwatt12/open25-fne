//go:build linux || darwin

package radio

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"os/signal"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"
)

func c4fmNativeCheck() {
	if gate := os.Getenv("RADIO_C4FM_CHECK_GATE"); gate != "" {
		_ = os.WriteFile(gate+".entered", []byte("offline native check"), 0600)
		for {
			if _, err := os.Stat(gate); err == nil {
				break
			}
			time.Sleep(5 * time.Millisecond)
		}
	}
	fmt.Println("C4FM_TEST available")
}

// This executable fixture emits protocol observations only. It never opens a
// modem or generates RF; native PTY tests qualify the real command writer.
func c4fmNativeHelper() {
	stop := make(chan os.Signal, 1)
	signal.Notify(stop, os.Interrupt, syscall.SIGTERM)
	defer signal.Stop(stop)
	_ = os.WriteFile("c4fm-helper-entered", []byte("offline fixture"), 0600)
	mode := os.Getenv("RADIO_C4FM_MODE")
	if mode == "hold-ready" {
		<-stop
		fmt.Println("C4FM_TEST draining")
		fmt.Println("C4FM_TEST stopped confirmed")
		return
	}
	fmt.Println("C4FM_TEST ready")
	if mode != "no-tx" {
		fmt.Println("C4FM_TEST transmitting")
	}
	duration := time.Second
	for i, arg := range os.Args {
		if arg == "--c4fm-test" && i+1 < len(os.Args) {
			ms, _ := strconv.Atoi(os.Args[i+1])
			duration = time.Duration(ms) * time.Millisecond
		}
	}
	if mode == "no-tx" || mode == "unconfirmed" {
		duration = 100 * time.Millisecond
	}
	select {
	case <-time.After(duration):
	case <-stop:
	}
	if mode == "no-tx" {
		fmt.Println("C4FM_TEST failed transmit state not observed")
	}
	fmt.Println("C4FM_TEST draining")
	if mode == "unconfirmed" {
		fmt.Println("C4FM_TEST stopped unconfirmed")
		os.Exit(1)
	}
	fmt.Println("C4FM_TEST stopped confirmed")
	if mode == "no-tx" {
		os.Exit(1)
	}
}

func c4fmController(t *testing.T) (*Controller, Profile, func() []*recoveryLease) {
	t.Helper()
	c := testController(t, nil)
	p := fixtureProfile()
	if err := c.Start(context.Background(), p, "null"); err != nil {
		t.Fatal(err)
	}
	var mu sync.Mutex
	var leases []*recoveryLease
	c.modem = modemOperations{
		acquire: func(ctx context.Context, _ string) (io.Closer, error) {
			lease := &recoveryLease{}
			mu.Lock()
			leases = append(leases, lease)
			mu.Unlock()
			return lease, ctx.Err()
		},
		verify: func(ctx context.Context, _ io.Closer, _ string, _ bool) error { return ctx.Err() },
		same: func(lease io.Closer, _ string) bool {
			return !lease.(*recoveryLease).closed.Load()
		},
		resolve: func(path string) (string, error) { return path, nil },
	}
	c.mu.Lock()
	c.opts.AllowRF = true
	c.status.Mode = "hardware" // only fake native processes and in-memory leases
	c.mu.Unlock()
	return c, p, func() []*recoveryLease {
		mu.Lock()
		defer mu.Unlock()
		return append([]*recoveryLease(nil), leases...)
	}
}

func waitC4FMFinished(t *testing.T, c *Controller) C4FMTestStatus {
	t.Helper()
	waitRadio(t, "C4FM operation did not retire", func() bool {
		c.mu.Lock()
		defer c.mu.Unlock()
		return c.c4fm == nil
	})
	return c.C4FMTestStatus()
}

func TestC4FMNaturalCompletionAndExplicitStopRestoreOnlySelectedVoice(t *testing.T) {
	for _, mode := range []string{"natural", "stop", "canceled-stop-context"} {
		t.Run(mode, func(t *testing.T) {
			c, p, leases := c4fmController(t)
			before := c.Status()
			oldVoice := voice(c)
			if err := c.StartC4FMTest(context.Background(), p, C4FMTestRequest{ChannelID: "voice-1", DurationSeconds: 1}); err != nil {
				t.Fatal(err)
			}
			waitRadio(t, "native transmit observation was not published", func() bool { return c.C4FMTestStatus().TXObserved })
			if err := c.StartChannel(context.Background(), "voice-1"); err == nil || !strings.Contains(err.Error(), "reserved") {
				t.Fatal("ordinary Start reclaimed the test transmitter", err)
			}
			if mode != "natural" {
				stop := context.Background()
				if mode == "canceled-stop-context" {
					canceled, cancel := context.WithCancel(stop)
					cancel()
					stop = canceled
				}
				if err := c.StopC4FMTest(stop); err != nil {
					t.Fatal(err)
				}
			}
			status := waitC4FMFinished(t, c)
			waitRadio(t, "confirmed test did not restore voice", func() bool { return voice(c).Ready })
			after := c.Status()
			if status.State != "stopped" || !status.TXObserved || !status.StopConfirmed || status.Error != "" || status.StartedAt.IsZero() || status.EndsAt.Sub(status.StartedAt) != time.Second {
				t.Fatalf("test completion lost actual observations: %+v", status)
			}
			if before.StartedAt != after.StartedAt || before.FNEPID != after.FNEPID || selectedChannel(c, "control-1").PID != selectedPID(before, "control-1") || voice(c).PID == oldVoice.PID {
				t.Fatal("restoration replaced an unrelated Site/control/media lifetime")
			}
			if len(leases()) < 2 || !leases()[0].closed.Load() {
				t.Fatal("test modem lease was not released before voice reacquisition")
			}
		})
	}
}

func selectedPID(status Status, id string) int {
	for _, row := range status.Channels {
		if row.ID == id {
			return row.PID
		}
	}
	return 0
}

func TestC4FMDoesNotInventTXOrRestoreAfterUnconfirmedStop(t *testing.T) {
	for _, mode := range []string{"no-tx", "unconfirmed"} {
		t.Run(mode, func(t *testing.T) {
			t.Setenv("RADIO_C4FM_MODE", mode)
			c, p, _ := c4fmController(t)
			if err := c.StartC4FMTest(context.Background(), p, C4FMTestRequest{ChannelID: "voice-1", DurationSeconds: 1}); err != nil {
				t.Fatal(err)
			}
			status := waitC4FMFinished(t, c)
			if status.State != "failed" || status.Error == "" {
				t.Fatalf("actual test failure was hidden: %+v", status)
			}
			if mode == "no-tx" {
				if status.TXObserved || !status.StartedAt.IsZero() || !status.EndsAt.IsZero() {
					t.Fatalf("process startup invented a transmitted signal: %+v", status)
				}
			} else if status.StopConfirmed || voice(c).Ready || voice(c).PID != 0 {
				t.Fatalf("unconfirmed modem stop restored a voice worker: %+v %+v", status, voice(c))
			}
		})
	}
}

func TestC4FMWaitsForActualNativeQuiescenceAndResumesRejectedReservation(t *testing.T) {
	for _, busy := range []bool{false, true} {
		t.Run(fmt.Sprint(busy), func(t *testing.T) {
			c, p, leases := c4fmController(t)
			old := selectionWorker(c, "voice-1")
			selectionFlag(t, old, "delayed-quiesce")
			if busy {
				selectionFlag(t, old, "busy-permit")
			}
			ctx := context.Background()
			cancel := func() {}
			if busy {
				// A race-instrumented executable waits one second before exit;
				// the availability check must finish before this deadline.
				ctx, cancel = context.WithTimeout(ctx, 1500*time.Millisecond)
			}
			defer cancel()
			err := c.StartC4FMTest(ctx, p, C4FMTestRequest{ChannelID: "voice-1", DurationSeconds: 1})
			if busy {
				if err == nil || !old.alive() || voice(c).PID != old.cmd.Process.Pid || len(leases()) != 0 {
					t.Fatal("busy reservation retired voice or acquired its modem", err)
				}
				if _, err := os.Stat(filepath.Join(old.dir, "quiesced")); !os.IsNotExist(err) {
					t.Fatal("rejected reservation retained the native admission fence", err)
				}
			} else {
				if err != nil {
					t.Fatal("initial pending quiescence was treated as rejection", err)
				}
				if err := c.StopC4FMTest(context.Background()); err != nil {
					t.Fatal(err)
				}
			}
		})
	}
}

func TestC4FMRetainsReservationUntilFailedLeaseCleanupCanBeRetried(t *testing.T) {
	c, p, leases := c4fmController(t)
	if err := c.StartC4FMTest(context.Background(), p, C4FMTestRequest{ChannelID: "voice-1", DurationSeconds: 60}); err != nil {
		t.Fatal(err)
	}
	waitRadio(t, "test lease was not acquired", func() bool { return len(leases()) == 1 })
	lease := leases()[0]
	lease.fail.Store(true)
	if err := c.StopC4FMTest(context.Background()); err == nil {
		t.Fatal("failed modem lease cleanup reported success")
	}
	c.mu.Lock()
	retained := c.c4fm != nil
	c.mu.Unlock()
	if !retained || lease.closed.Load() || voice(c).Ready {
		t.Fatal("failed cleanup lost ownership or restored a voice worker")
	}
	lease.fail.Store(false)
	if err := c.StopC4FMTest(context.Background()); err != nil {
		t.Fatal(err)
	}
	if !lease.closed.Load() || !voice(c).Ready {
		t.Fatal("explicit cleanup retry did not release and restore the resource")
	}
}

func TestC4FMStopDuringStartupCannotLaterTransmit(t *testing.T) {
	t.Setenv("RADIO_C4FM_MODE", "hold-ready")
	c, p, _ := c4fmController(t)
	started := make(chan error, 1)
	go func() {
		started <- c.StartC4FMTest(context.Background(), p, C4FMTestRequest{ChannelID: "voice-1", DurationSeconds: 60})
	}()
	waitRadio(t, "test helper did not enter startup", func() bool {
		c.mu.Lock()
		dir := filepath.Join(c.rootDir, "c4fm-test", "c4fm-helper-entered")
		c.mu.Unlock()
		_, err := os.Stat(dir)
		return err == nil
	})
	if err := c.StopC4FMTest(context.Background()); err != nil {
		t.Fatal(err)
	}
	select {
	case err := <-started:
		if err == nil {
			t.Fatal("a canceled startup was accepted")
		}
	case <-time.After(2 * time.Second):
		t.Fatal("canceled startup did not retire")
	}
	status := c.C4FMTestStatus()
	if status.TXObserved || !status.StartedAt.IsZero() || !status.EndsAt.IsZero() {
		t.Fatalf("canceled startup became a transmitted signal: %+v", status)
	}
}

func TestC4FMStopCancelsAnUnpublishedNativeCheck(t *testing.T) {
	c, p, leases := c4fmController(t)
	old := selectionWorker(c, "voice-1")
	gate := filepath.Join(t.TempDir(), "allow-native-check")
	t.Setenv("RADIO_C4FM_CHECK_GATE", gate)
	started := make(chan error, 1)
	go func() {
		started <- c.StartC4FMTest(context.Background(), p, C4FMTestRequest{ChannelID: "voice-1", DurationSeconds: 60})
	}()
	waitRadio(t, "native availability check did not block", func() bool {
		_, err := os.Stat(gate + ".entered")
		return err == nil
	})
	if err := c.StopC4FMTest(context.Background()); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(gate, []byte("finish offline check"), 0600); err != nil {
		t.Fatal(err)
	}
	select {
	case err := <-started:
		if err == nil {
			t.Fatal("a Start pending before Stop was accepted afterward")
		}
	case <-time.After(2 * time.Second):
		t.Fatal("canceled availability check did not finish")
	}
	if !old.alive() || voice(c).PID != old.cmd.Process.Pid || len(leases()) != 0 || c.C4FMTestStatus().TXObserved {
		t.Fatal("canceled pending Start reserved or replaced the transmitter")
	}
	if _, err := os.Stat(filepath.Join(old.dir, "quiesced")); !os.IsNotExist(err) {
		t.Fatal("canceled availability check touched native admission", err)
	}
}

func TestC4FMDoesNotRestoreChangedProgramming(t *testing.T) {
	c, p, _ := c4fmController(t)
	if err := c.StartC4FMTest(context.Background(), p, C4FMTestRequest{ChannelID: "voice-1", DurationSeconds: 60}); err != nil {
		t.Fatal(err)
	}
	c.mu.Lock()
	for i := range c.configuredProfile.Site.Modems {
		if c.configuredProfile.Site.Modems[i].ID == "voice" {
			c.configuredProfile.Site.Modems[i].Calibration.TXLevel++
		}
	}
	c.mu.Unlock()
	if err := c.StopC4FMTest(context.Background()); err != nil {
		t.Fatal(err)
	}
	if !c.C4FMTestStatus().StopConfirmed || voice(c).Ready || voice(c).PID != 0 {
		t.Fatal("test restoration reopened changed transmitter programming")
	}
}

func TestC4FMUnconfirmedAdmissionResumeRemainsFaultedUntilExplicitRestart(t *testing.T) {
	c, p, _ := c4fmController(t)
	old := selectionWorker(c, "voice-1")
	unknown := selectionFlag(t, old, "unknown-quiesce")
	if err := c.StartC4FMTest(context.Background(), p, C4FMTestRequest{ChannelID: "voice-1", DurationSeconds: 1}); err == nil {
		t.Fatal("unknown native reservation result was accepted")
	}
	if row := voice(c); row.Ready || row.State != "fault" || !strings.Contains(row.Error, "resume") {
		t.Fatalf("unconfirmed admission resume remained ready: %+v", row)
	}
	c.applyNativeStatus(old, map[string]json.RawMessage{"tx": json.RawMessage(`false`)})
	if row := voice(c); row.Ready || row.State != "fault" {
		t.Fatalf("ordinary idle status cleared an unconfirmed admission fence: %+v", row)
	}
	if err := os.Remove(unknown); err != nil {
		t.Fatal(err)
	}
	if err := c.StopChannel(context.Background(), "voice-1"); err != nil {
		t.Fatal(err)
	}
	if err := c.StartChannel(context.Background(), "voice-1"); err != nil {
		t.Fatal(err)
	}
	if row := voice(c); !row.Ready || row.PID == old.cmd.Process.Pid || row.Error != "" {
		t.Fatalf("fresh explicit worker retained the old admission failure: %+v", row)
	}
}
