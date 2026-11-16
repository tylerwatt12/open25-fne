package radio

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"
)

func TestRestrictedWACNStartDoesNotLaunchNativeOrTouchModems(t *testing.T) {
	for _, mode := range []string{"null", "hardware"} {
		for _, pendingRecovery := range []bool{false, true} {
			t.Run(fmt.Sprintf("%s/recovery-%t", mode, pendingRecovery), func(t *testing.T) {
				c := testController(t, nil)
				c.opts.AllowRF = true
				c.modem = modemOperations{
					resolve: func(string) (string, error) {
						t.Error("restricted startup attempted modem discovery")
						return "", errors.New("unexpected modem discovery")
					},
					acquire: func(context.Context, string) (io.Closer, error) {
						t.Error("restricted startup attempted modem acquisition")
						return nil, errors.New("unexpected modem acquisition")
					},
				}
				var recovery *siteAttempt
				var voice *channelRecovery
				if pendingRecovery {
					ctx, cancel := context.WithCancel(context.Background())
					t.Cleanup(cancel)
					done := make(chan struct{})
					close(done) // This sentinel has no running worker.
					recovery = &siteAttempt{ctx: ctx, cancel: cancel, done: done, epoch: 7}
					voice = &channelRecovery{wanted: true, failures: 2, next: time.Date(2025, 1, 1, 0, 0, 0, 0, time.UTC), attempt: &channelAttempt{ctx: ctx, cancel: cancel}}
					c.siteRecovery, c.siteWanted, c.stopEpoch = recovery, true, 7
					c.recovery = map[string]*channelRecovery{"voice-1": voice}
				}
				c.controlTurns = []string{"unchanged-control-turn"}
				beforeStatus := c.Status()
				for wacn := 0xBEE00; wacn <= 0xBEEFF; wacn++ {
					p := fixtureProfile()
					p.System.WACN = wacn
					before, err := json.Marshal(p)
					if err != nil {
						t.Fatal(err)
					}
					err = c.Start(context.Background(), p, mode)
					want := fmt.Sprintf("WACN %05X is blocked for operation: BEE00-BEEFF is restricted to reduce unintended subscriber roaming risk", wacn)
					if err == nil || !strings.Contains(err.Error(), want) {
						t.Fatalf("startup did not return the policy error: got %v, want %q", err, want)
					}
					after, marshalErr := json.Marshal(p)
					if marshalErr != nil || !bytes.Equal(after, before) {
						t.Fatal("refused startup changed the caller's programming")
					}
					if _, err := os.Lstat(filepath.Join(c.opts.StateDir, "radio-maintenance.lock")); !errors.Is(err, os.ErrNotExist) {
						t.Fatal("refused startup acquired or created the maintenance lock", err)
					}
					c.mu.Lock()
					changed := c.rootDir != "" || c.fne != nil || len(c.children) != 0 || c.siteWanted != pendingRecovery || c.siteRecovery != recovery || !reflect.DeepEqual(c.controlTurns, []string{"unchanged-control-turn"})
					if pendingRecovery {
						changed = changed || c.stopEpoch != 7 || recovery.ctx.Err() != nil || c.recovery["voice-1"] != voice || !voice.wanted || voice.failures != 2 || voice.attempt.ctx.Err() != nil || !voice.next.Equal(time.Date(2025, 1, 1, 0, 0, 0, 0, time.UTC))
					}
					c.mu.Unlock()
					if changed || !reflect.DeepEqual(beforeStatus, c.Status()) {
						t.Fatal("restricted startup changed native ownership, control selection or recovery")
					}
				}
			})
		}
	}
}

func TestRestrictedWACNInternalStartupRejectsBeforeMaintenanceOrControlSelection(t *testing.T) {
	for _, entry := range []string{"start", "startSite", "startSiteOnControl"} {
		t.Run(entry, func(t *testing.T) {
			c := testController(t, nil)
			c.controlTurns = []string{"unchanged-control-turn"}
			for _, wacn := range []int{0xBEE00, 0xBEE80, 0xBEEFF} {
				p := fixtureProfile()
				p.System.WACN = wacn
				var err error
				switch entry {
				case "start":
					err = c.start(context.Background(), p, "null")
				case "startSite":
					err = c.startSite(context.Background(), p, "null", nil, 0, nil)
				case "startSiteOnControl":
					err = c.startSiteOnControl(context.Background(), p, "null", nil, 0, nil, "control-1")
				}
				if err == nil || !strings.Contains(err.Error(), "BEE00-BEEFF") {
					t.Fatal("internal startup did not return the policy error", err)
				}
				if _, err := os.Lstat(filepath.Join(c.opts.StateDir, "radio-maintenance.lock")); !errors.Is(err, os.ErrNotExist) {
					t.Fatal("internal startup acquired or created the maintenance lock", err)
				}
				c.mu.Lock()
				changed := c.rootDir != "" || c.fne != nil || len(c.children) != 0 || c.siteWanted || c.siteRecovery != nil || !reflect.DeepEqual(c.controlTurns, []string{"unchanged-control-turn"})
				c.mu.Unlock()
				if changed {
					t.Fatal("internal startup changed native ownership, control selection or recovery")
				}
			}
		})
	}
}
