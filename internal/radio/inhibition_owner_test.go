package radio

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"slices"
	"strings"
	"sync"
	"sync/atomic"
	"syscall"
	"testing"
	"time"
)

func inhibitReply(rows []InhibitState) map[string]any {
	if rows == nil {
		rows = []InhibitState{}
	}
	return map[string]any{"status": 200, "record_count": len(rows), "record_limit": InhibitRecordLimit, "radios": rows}
}

func inhibitEndpoint(t *testing.T, fn func(http.ResponseWriter, *http.Request)) *NativeClient {
	t.Helper()
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		if r.URL.Path == "/auth" {
			_ = json.NewEncoder(w).Encode(map[string]any{"status": 200, "token": "inhibit-test"})
			return
		}
		if r.Header.Get("X-DVM-Auth-Token") != "inhibit-test" {
			t.Error("missing native authentication")
		}
		fn(w, r)
	}))
	t.Cleanup(server.Close)
	client, err := NewNativeClient(strings.TrimPrefix(server.URL, "http://"), "private-test")
	if err != nil {
		t.Fatal(err)
	}
	return client
}

func inhibitOwner(id, role string, client *NativeClient) *child {
	ready := make(chan struct{})
	close(ready)
	return &child{id: id, role: role, client: client, done: make(chan struct{}), ready: ready}
}

func inhibitController(t *testing.T, control, voice *NativeClient, p InhibitPolicy) *Controller {
	t.Helper()
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	profile := fixtureProfile()
	profile.System.Radios = append(profile.System.Radios, Radio{RID: 10003, Kind: "subscriber", Enabled: true})
	profile.Inhibition = p
	return &Controller{ctx: ctx, runContext: ctx, runCancel: cancel, runID: "current-inhibit-run", profile: profile, children: map[string]*child{"control-1": inhibitOwner("control-1", "control", control), "voice-1": inhibitOwner("voice-1", "voice", voice)}, events: make(chan Event, 32), status: Status{State: "running", Mode: "null", Inhibition: initialInhibition(p), InhibitionReady: true}, inhibitConfigured: true, inhibitDesired: slices.Clone(p.Desired)}
}

func TestInhibitionSerializesCurrentControlReadbackBeforeCompleteVoiceMirror(t *testing.T) {
	var mu sync.Mutex
	rows := initialInhibition(InhibitPolicy{Desired: []int{10001}, Restore: []InhibitRestore{{10003, true, true}}})
	events := []string{}
	control := inhibitEndpoint(t, func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		defer mu.Unlock()
		events = append(events, "control "+r.Method)
		if r.Method == "PUT" {
			var request inhibitRequest
			if err := json.NewDecoder(r.Body).Decode(&request); err != nil {
				t.Error(err)
			}
			if request.ControlState != nil {
				t.Error("control received a voice-only mirror")
			}
			desired := []int{}
			for _, v := range request.Radios {
				if v.Inhibited {
					desired = append(desired, v.RadioID)
				}
			}
			rows = initialInhibition(InhibitPolicy{Desired: desired, Restore: inhibitionRestore(rows)})
		}
		_ = json.NewEncoder(w).Encode(inhibitReply(rows))
	})
	var mirrors [][]InhibitRestore
	voice := inhibitEndpoint(t, func(w http.ResponseWriter, r *http.Request) {
		var request inhibitRequest
		if err := json.NewDecoder(r.Body).Decode(&request); err != nil {
			t.Error(err)
		}
		if request.ControlState == nil {
			t.Error("voice mirror omitted complete actual state")
			_ = json.NewEncoder(w).Encode(map[string]int{"status": 400})
			return
		}
		mu.Lock()
		defer mu.Unlock()
		events = append(events, "voice PUT")
		mirrors = append(mirrors, slices.Clone(*request.ControlState))
		desired := []int{}
		for _, v := range request.Radios {
			if v.Inhibited {
				desired = append(desired, v.RadioID)
			}
		}
		_ = json.NewEncoder(w).Encode(inhibitReply(initialInhibition(InhibitPolicy{Desired: desired, Restore: *request.ControlState})))
	})
	c := inhibitController(t, control, voice, InhibitPolicy{Desired: []int{10001}, Restore: []InhibitRestore{{10003, true, true}}})
	c.opts.OnInhibition = func(actual []InhibitRestore) error {
		mu.Lock()
		defer mu.Unlock()
		events = append(events, "save actual")
		return nil
	}
	if err := c.ApplyInhibitionPolicy(context.Background(), []int{}); err != nil {
		t.Fatal(err)
	}
	if got := c.Status(); !got.InhibitionReady || len(got.Inhibition) != 2 || !got.Inhibition[0].PendingUninhibit || !got.Inhibition[1].PendingUninhibit {
		t.Fatalf("omitted desire erased actual protection: %+v", got.Inhibition)
	}
	mu.Lock()
	if !slices.Equal(events, []string{"control PUT", "control GET", "voice PUT", "save actual"}) {
		t.Errorf("incorrect management order: %v", events)
	}
	if len(mirrors) != 1 || len(mirrors[0]) != 2 {
		t.Error("voice did not receive both pending actual records")
	}
	rows = []InhibitState{}
	events = nil
	mu.Unlock()
	if err := c.synchronizeInhibition(context.Background(), false); err != nil {
		t.Fatal(err)
	}
	if got := c.Status(); !got.InhibitionReady || len(got.Inhibition) != 0 {
		t.Fatalf("current owner clear did not converge: %+v", got)
	}
	mu.Lock()
	defer mu.Unlock()
	if len(mirrors) != 2 || len(mirrors[1]) != 0 || !slices.Equal(events, []string{"control GET", "voice PUT", "save actual"}) {
		t.Fatalf("actual clear was not a complete authenticated mirror: mirrors=%v order=%v", mirrors, events)
	}
}

func TestInhibitionLateControlReplyCannotPublishIntoReplacementOwner(t *testing.T) {
	entered, release := make(chan struct{}), make(chan struct{})
	control := inhibitEndpoint(t, func(w http.ResponseWriter, r *http.Request) {
		close(entered)
		<-release
		_ = json.NewEncoder(w).Encode(inhibitReply(nil))
	})
	var mirrored, saved atomic.Int32
	voice := inhibitEndpoint(t, func(w http.ResponseWriter, r *http.Request) {
		mirrored.Add(1)
		_ = json.NewEncoder(w).Encode(inhibitReply(nil))
	})
	c := inhibitController(t, control, voice, InhibitPolicy{Restore: []InhibitRestore{{10001, true, true}}})
	c.children["control-1"].inhibitionInitialized = true
	c.opts.OnInhibition = func([]InhibitRestore) error { saved.Add(1); return nil }
	result := make(chan error, 1)
	go func() { result <- c.synchronizeInhibition(context.Background(), false) }()
	select {
	case <-entered:
	case <-time.After(time.Second):
		t.Fatal("control snapshot did not start")
	}
	c.mu.Lock()
	c.children["control-1"] = inhibitOwner("control-1", "control", control)
	c.mu.Unlock()
	close(release)
	select {
	case err := <-result:
		if !errors.Is(err, context.Canceled) {
			t.Fatalf("stale owner returned %v", err)
		}
	case <-time.After(time.Second):
		t.Fatal("stale response did not finish")
	}
	if mirrored.Load() != 0 || saved.Load() != 0 || !inhibitionBlocked(c.Status().Inhibition, 10001) {
		t.Fatal("old owner cleared or mirrored replacement protection")
	}
}

func TestInhibitionRunCancellationInterruptsNativeReadback(t *testing.T) {
	entered := make(chan struct{})
	control := inhibitEndpoint(t, func(w http.ResponseWriter, r *http.Request) { close(entered); <-r.Context().Done() })
	voice := inhibitEndpoint(t, func(w http.ResponseWriter, r *http.Request) { t.Error("cancelled readback reached a voice owner") })
	c := inhibitController(t, control, voice, InhibitPolicy{})
	result := make(chan error, 1)
	go func() { result <- c.synchronizeInhibition(context.Background(), false) }()
	select {
	case <-entered:
	case <-time.After(time.Second):
		t.Fatal("readback did not start")
	}
	c.runCancel()
	select {
	case err := <-result:
		if err == nil {
			t.Fatal("cancelled owner returned success")
		}
	case <-time.After(time.Second):
		t.Fatal("run cancellation waited for native request deadline")
	}
}

func TestNewControlOwnerCannotInferClearFromMissingRestoredState(t *testing.T) {
	control := inhibitEndpoint(t, func(w http.ResponseWriter, r *http.Request) { _ = json.NewEncoder(w).Encode(inhibitReply(nil)) })
	voice := inhibitEndpoint(t, func(w http.ResponseWriter, r *http.Request) { t.Error("unproven new-owner clear reached a VC") })
	c := inhibitController(t, control, voice, InhibitPolicy{Restore: []InhibitRestore{{10001, true, true}}})
	if err := c.synchronizeInhibition(context.Background(), false); err == nil {
		t.Fatal("empty new native state erased recovery protection")
	}
	if !inhibitionBlocked(c.Status().Inhibition, 10001) {
		t.Fatal("failed startup observation cleared recovery protection")
	}
}

func TestNewControlOwnerAcceptsItsObservedReenableAckAndDropsStartupEvidence(t *testing.T) {
	control := inhibitEndpoint(t, func(w http.ResponseWriter, r *http.Request) { _ = json.NewEncoder(w).Encode(inhibitReply(nil)) })
	voice := inhibitEndpoint(t, func(w http.ResponseWriter, r *http.Request) { _ = json.NewEncoder(w).Encode(inhibitReply(nil)) })
	c := inhibitController(t, control, voice, InhibitPolicy{Restore: []InhibitRestore{{10001, true, true}}})
	owner := c.children["control-1"]
	ack := "I: (RF) P25, TSDU (Trunking System Data Unit), (Extended Function Response), op = 254, arg = 16777212, tgt = 10001"
	c.observe(owner, ack)
	if err := c.synchronizeInhibition(context.Background(), false); err != nil {
		t.Fatal("current startup response evidence was not honored", err)
	}
	if inhibitionBlocked(c.Status().Inhibition, 10001) {
		t.Fatal("current native re-enable clear was lost")
	}
	c.observe(owner, ack)
	owner.mu.Lock()
	defer owner.mu.Unlock()
	if !owner.inhibitionInitialized || owner.inhibitionClears != nil {
		t.Fatal("startup response guard retained lifetime false-history entries")
	}
}

func TestInhibitionChangedPolicyCancelsWarmupAndCachesWithoutRestart(t *testing.T) {
	c := testController(t, nil)
	c.opts.StartupTimeout = 45 * time.Second
	t.Setenv("RADIO_DELAY_CHANNEL", "control-1")
	t.Setenv("RADIO_STARTUP_DELAY", "30s")
	started := make(chan error, 1)
	go func() { started <- c.Start(context.Background(), fixtureProfile(), "null") }()
	waitRadio(t, "control warmup did not start", func() bool {
		status := c.Status()
		return status.State == "starting" && len(status.Channels) > 0 && status.Channels[0].PID != 0
	})
	if err := c.ApplyInhibitionPolicy(context.Background(), []int{10001}); err != nil {
		t.Fatal(err)
	}
	select {
	case err := <-started:
		if err == nil {
			t.Fatal("old warmup succeeded")
		}
	case <-time.After(time.Second):
		t.Fatal("policy did not interrupt warmup")
	}
	if c.Status().State == "running" {
		t.Fatal("passive refresh restarted RF")
	}
	t.Setenv("RADIO_DELAY_CHANNEL", "")
	if err := c.Start(context.Background(), fixtureProfile(), "null"); err != nil {
		t.Fatal(err)
	}
	if got := c.Status(); !got.InhibitionReady || !inhibitionBlocked(got.Inhibition, 10001) {
		t.Fatal("fresh Start did not enforce cached choice")
	}
}

func TestInhibitionEnforcementSaveFailureWithdrawsEveryRunOwner(t *testing.T) {
	c := testController(t, nil)
	var fail atomic.Bool
	c.opts.OnInhibition = func([]InhibitRestore) error {
		if fail.Load() {
			return fmt.Errorf("deliberate actual-state save failure")
		}
		return nil
	}
	if err := c.Start(context.Background(), fixtureProfile(), "null"); err != nil {
		t.Fatal(err)
	}
	before := c.Status()
	fail.Store(true)
	if err := c.ApplyInhibitionPolicy(context.Background(), []int{10001}); err == nil {
		t.Fatal("failed current enforcement save reported success")
	}
	if got := c.Status(); got.State == "running" || got.InhibitionReady {
		t.Fatalf("failed durable protection left admission open: %+v", got)
	}
	for _, pid := range []int{before.FNEPID, before.Channels[0].PID, before.Channels[1].PID} {
		if syscall.Kill(pid, 0) == nil {
			t.Fatal("failed enforcement retained a live RF/media process")
		}
	}
}

func TestCancelledOlderInhibitCallerCannotWithdrawOrRestoreNewerChoice(t *testing.T) {
	c := testController(t, nil)
	p := fixtureProfile()
	p.System.Radios = append(p.System.Radios, Radio{RID: 10003, Kind: "subscriber", Enabled: true})
	if err := c.Start(context.Background(), p, "null"); err != nil {
		t.Fatal(err)
	}
	c.opMu.Lock()
	oldCtx, cancel := context.WithCancel(context.Background())
	old := make(chan error, 1)
	go func() { old <- c.ApplyInhibitionPolicy(oldCtx, []int{10001}) }()
	waitRadio(t, "old caller did not cache its current choice", func() bool { c.mu.Lock(); defer c.mu.Unlock(); return slices.Equal(c.inhibitDesired, []int{10001}) })
	if _, err := c.ControlClient(); err == nil {
		t.Fatal("permits admitted before pending policy mirror converged")
	}
	if err := c.synchronizeInhibition(context.Background(), false); !errors.Is(err, context.Canceled) {
		t.Fatalf("old monitor snapshot could republish readiness: %v", err)
	}
	cancel()
	newer := make(chan error, 1)
	go func() { newer <- c.ApplyInhibitionPolicy(context.Background(), []int{10003}) }()
	waitRadio(t, "new caller did not supersede old choice", func() bool { c.mu.Lock(); defer c.mu.Unlock(); return slices.Equal(c.inhibitDesired, []int{10003}) })
	c.opMu.Unlock()
	if err := <-old; !errors.Is(err, context.Canceled) {
		t.Fatalf("old caller returned %v", err)
	}
	if err := <-newer; err != nil {
		t.Fatal(err)
	}
	got := c.Status()
	if got.State != "running" || !got.InhibitionReady || inhibitionBlocked(got.Inhibition, 10001) || !inhibitionBlocked(got.Inhibition, 10003) {
		t.Fatalf("cancelled caller changed newer RF owner: %+v", got)
	}
}

func TestInFlightOldInhibitReplyCannotWithdrawSupersedingPolicy(t *testing.T) {
	entered, release := make(chan struct{}), make(chan struct{})
	var first atomic.Bool
	var mu sync.Mutex
	rows := []InhibitState{}
	control := inhibitEndpoint(t, func(w http.ResponseWriter, r *http.Request) {
		if r.Method == "PUT" {
			var request inhibitRequest
			_ = json.NewDecoder(r.Body).Decode(&request)
			desired := []int{}
			for _, v := range request.Radios {
				if v.Inhibited {
					desired = append(desired, v.RadioID)
				}
			}
			mu.Lock()
			rows = initialInhibition(InhibitPolicy{Desired: desired})
			mu.Unlock()
			if first.CompareAndSwap(false, true) {
				close(entered)
				<-release
			}
		}
		mu.Lock()
		defer mu.Unlock()
		_ = json.NewEncoder(w).Encode(inhibitReply(rows))
	})
	voice := inhibitEndpoint(t, func(w http.ResponseWriter, r *http.Request) {
		var request inhibitRequest
		_ = json.NewDecoder(r.Body).Decode(&request)
		desired := []int{}
		for _, v := range request.Radios {
			if v.Inhibited {
				desired = append(desired, v.RadioID)
			}
		}
		_ = json.NewEncoder(w).Encode(inhibitReply(initialInhibition(InhibitPolicy{Desired: desired, Restore: *request.ControlState})))
	})
	c := inhibitController(t, control, voice, InhibitPolicy{})
	old := make(chan error, 1)
	go func() { old <- c.ApplyInhibitionPolicy(context.Background(), []int{10001}) }()
	select {
	case <-entered:
	case <-time.After(time.Second):
		t.Fatal("old policy did not reach native owner")
	}
	newer := make(chan error, 1)
	go func() { newer <- c.ApplyInhibitionPolicy(context.Background(), []int{10003}) }()
	waitRadio(t, "new policy did not supersede in-flight caller", func() bool { c.mu.Lock(); defer c.mu.Unlock(); return slices.Equal(c.inhibitDesired, []int{10003}) })
	close(release)
	if err := <-old; !errors.Is(err, context.Canceled) {
		t.Fatalf("old reply returned %v", err)
	}
	if err := <-newer; err != nil {
		t.Fatal(err)
	}
	if got := c.Status(); got.State != "running" || !got.InhibitionReady || !inhibitionBlocked(got.Inhibition, 10003) {
		t.Fatalf("old reply withdrew newer choice: %+v", got)
	}
}
