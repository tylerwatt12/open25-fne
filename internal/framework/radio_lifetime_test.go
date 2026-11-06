//go:build linux || darwin

package framework

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"os/signal"
	"path/filepath"
	"regexp"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"

	"open25.local/open25-fne/internal/dispatch"
	"open25.local/open25-fne/internal/radio"
	"open25.local/open25-fne/internal/routing"
)

// The actual Controller runs real process groups and authenticated readiness.
// Only the native executable and its RF mode are test fixtures: no modem opens.
type offlineSiteRadio struct {
	*radio.Controller
	stopEntered        chan struct{}
	stopRelease        <-chan struct{}
	stopOnce           sync.Once
	startReturnEntered chan struct{}
	startReturnRelease <-chan struct{}
	startReturnOnce    sync.Once
}

func (r *offlineSiteRadio) Start(ctx context.Context, p radio.Profile, _ string) error {
	err := r.Controller.Start(ctx, p, "null")
	if r.startReturnEntered != nil {
		r.startReturnOnce.Do(func() { close(r.startReturnEntered); <-r.startReturnRelease })
	}
	return err
}

func (r *offlineSiteRadio) Stop(ctx context.Context) error {
	if r.stopEntered != nil {
		r.stopOnce.Do(func() { close(r.stopEntered) })
		select {
		case <-r.stopRelease:
		case <-ctx.Done():
		}
	}
	return r.Controller.Stop(ctx)
}

func TestFrameworkRadioNativeHelper(t *testing.T) {
	if os.Getenv("FRAMEWORK_RADIO_HELPER") != "1" {
		return
	}
	dir, _ := os.Getwd()
	name := filepath.Base(dir)
	codec := strings.HasPrefix(name, "codec-")
	stop := make(chan os.Signal, 1)
	signal.Notify(stop, os.Interrupt, syscall.SIGTERM)
	defer signal.Stop(stop)
	if name == "channel-1" && os.Getenv("FRAMEWORK_DELAY_CONTROL") == "1" || codec && os.Getenv("FRAMEWORK_DELAY_CODEC") == "1" {
		select {
		case <-time.After(30 * time.Second):
		case <-stop:
			return
		}
	}
	if name == "media" || codec {
		if codec {
			fmt.Println("logged into the master successfully")
		} else {
			fmt.Println("FNE is up and running on offline-test")
		}
		<-stop
		return
	}
	config, _ := os.ReadFile("config.yml")
	port := regexp.MustCompile(`restPort: ([0-9]+)`).FindStringSubmatch(string(config))
	secret := regexp.MustCompile(`restPassword: "([a-f0-9]+)"`).FindStringSubmatch(string(config))
	if len(port) < 2 || len(secret) < 2 {
		t.Fatal("native fixture did not receive a rendered configuration")
	}
	sum := sha256.Sum256([]byte(secret[1]))
	token := "offline-" + hex.EncodeToString(sum[:16])
	listener, err := net.Listen("tcp", "127.0.0.1:"+port[1])
	if err != nil {
		t.Fatal(err)
	}
	server := &http.Server{Handler: http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		var body map[string]any
		_ = json.NewDecoder(r.Body).Decode(&body)
		if r.URL.Path == "/auth" {
			if body["auth"] != hex.EncodeToString(sum[:]) {
				_ = json.NewEncoder(w).Encode(map[string]any{"status": 401})
				return
			}
			_ = json.NewEncoder(w).Encode(map[string]any{"status": 200, "token": token})
			return
		}
		if r.Header.Get("X-DVM-Auth-Token") != token {
			_ = json.NewEncoder(w).Encode(map[string]any{"status": 401})
			return
		}
		out := map[string]any{"status": 200}
		switch r.URL.Path {
		case "/status":
			out["p25Enabled"], out["tx"] = true, false
		case "/p25/report-affiliations":
			out["affiliations"] = []any{}
			if os.Getenv("FRAMEWORK_AFFILIATED") == "1" {
				out["affiliations"] = []any{map[string]int{"srcId": 10001, "grpId": 1001}}
			}
		case "/p25/dispatch/grant":
			out["accepted"], out["issued"], out["state"] = true, true, "granted"
			out["srcId"], out["dstId"], out["leaseId"] = body["srcId"], body["dstId"], body["leaseId"]
			out["epoch"], out["channelId"], out["channelNo"] = "1", 2, 81
		case "/p25/site-state":
			out["zoneConnected"] = body["zoneConnected"]
		case "/p25/inhibit-policy":
			out["record_count"], out["record_limit"], out["radios"] = 0, radio.InhibitRecordLimit, []any{}
		default:
			out["status"] = 404
		}
		_ = json.NewEncoder(w).Encode(out)
	})}
	go server.Serve(listener)
	fmt.Println("Host is up and running on offline-test")
	if name == "channel-1" {
		for _, line := range strings.Split(os.Getenv("FRAMEWORK_SOURCE_FACT_LINES"), "\n") {
			if line != "" {
				fmt.Println(line)
			}
		}
	}
	<-stop
	_ = server.Close()
}

func frameworkWait(t *testing.T, what string, condition func() bool) {
	t.Helper()
	deadline := time.Now().Add(6 * time.Second)
	for time.Now().Before(deadline) {
		if condition() {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatal(what)
}

func radioLifetimeFixture(t *testing.T, withCodec bool) (*Service, *offlineSiteRadio) {
	t.Helper()
	s, _ := operationFixture(t)
	s.operationCancel()
	s.operationWG.Wait()
	t.Setenv("FRAMEWORK_RADIO_HELPER", "1")
	binary, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	script := filepath.Join(t.TempDir(), "native-fixture")
	quoted := "'" + strings.ReplaceAll(binary, "'", "'\\''") + "'"
	if err = os.WriteFile(script, []byte("#!/bin/sh\nexec "+quoted+" -test.run=^TestFrameworkRadioNativeHelper$ -- \"$@\"\n"), 0700); err != nil {
		t.Fatal(err)
	}
	base := 0
	for i := 0; i < 100; i++ {
		l, openErr := net.Listen("tcp", "127.0.0.1:0")
		if openErr != nil {
			t.Fatal(openErr)
		}
		candidate := 18000 + (l.Addr().(*net.TCPAddr).Port%4000)*2
		l.Close()
		first, e1 := net.Listen("tcp", fmt.Sprintf("127.0.0.1:%d", candidate))
		second, e2 := net.Listen("tcp", fmt.Sprintf("127.0.0.1:%d", candidate+1))
		if first != nil {
			first.Close()
		}
		if second != nil {
			second.Close()
		}
		if e1 == nil && e2 == nil {
			base = candidate
			break
		}
	}
	if base == 0 {
		t.Fatal("no free native fixture ports")
	}
	stateDir := t.TempDir()
	c, err := radio.New(radio.Options{StateDir: stateDir, EnginePath: script, FNEPath: script, BaseRPCPort: base, StartupTimeout: 45 * time.Second})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if !t.Failed() {
			return
		}
		paths, _ := filepath.Glob(filepath.Join(stateDir, "run-*", "native-*.log"))
		for _, path := range paths {
			raw, _ := os.ReadFile(path)
			if len(raw) > 4096 {
				raw = raw[len(raw)-4096:]
			}
			t.Logf("native fixture diagnostic %s:\n%s", filepath.Base(path), raw)
		}
	})
	runtime := &offlineSiteRadio{Controller: c}
	s.operationCtx, s.operationCancel = context.WithCancel(context.Background())
	s.radio = runtime
	s.radioRunActive, s.radioRunStartedAt = false, time.Time{}
	s.radioCandidateSignature = ""
	s.mu.Lock()
	if withCodec {
		s.config.Radio.Bridge = script
	}
	s.mu.Unlock()
	// Complete the new controller's stopped-policy initialization before the
	// background worker and the explicit Start under test can compete for it.
	s.reconcileRadio()
	s.operationWG.Add(1)
	go s.operationLoop()
	return s, runtime
}

func TestFrameworkSiteStopInterruptsNativeAndCodecWarmup(t *testing.T) {
	for _, phase := range []string{"native", "codec"} {
		t.Run(phase, func(t *testing.T) {
			s, runtime := radioLifetimeFixture(t, phase == "codec")
			if phase == "native" {
				t.Setenv("FRAMEWORK_DELAY_CONTROL", "1")
			} else {
				t.Setenv("FRAMEWORK_DELAY_CODEC", "1")
			}
			started := make(chan *httptest.ResponseRecorder, 1)
			go func() {
				response := httptest.NewRecorder()
				s.localRadioHandler().ServeHTTP(response, httptest.NewRequest("POST", "/start", strings.NewReader(`{}`)))
				started <- response
			}()
			frameworkWait(t, "product Start did not enter warmup", func() bool {
				if phase == "native" {
					status := runtime.Status()
					return status.State == "starting" && len(status.Channels) > 0 && status.Channels[0].PID != 0
				}
				s.operationMu.Lock()
				defer s.operationMu.Unlock()
				return s.codec != nil && s.radioOperation != nil
			})
			ctx, cancel := context.WithTimeout(context.Background(), 4*time.Second)
			defer cancel()
			begin := time.Now()
			response := httptest.NewRecorder()
			s.localRadioHandler().ServeHTTP(response, httptest.NewRequest("POST", "/stop", strings.NewReader(`{}`)).WithContext(ctx))
			if response.Code != http.StatusOK || time.Since(begin) > 5*time.Second {
				t.Fatalf("product Stop could not cancel warmup: %d %s", response.Code, response.Body.String())
			}
			select {
			case response = <-started:
				if response.Code == http.StatusOK {
					t.Fatal("cancelled product Start reported success")
				}
			case <-time.After(time.Second):
				t.Fatal("product Start remained blocked")
			}
			status := runtime.Status()
			if status.State != "stopped" || status.Error != "" || status.FNEPID != 0 || status.Channels[0].PID != 0 {
				t.Fatal("product Stop left an active or waiting Site", status)
			}
			s.operationMu.Lock()
			retained := s.codec != nil || s.rfRouter.Load() != nil || s.radioOperation != nil
			s.operationMu.Unlock()
			if retained {
				t.Fatal("stopped caller retained startup/media ownership")
			}
		})
	}
}

func TestFrameworkSavedSettingsCancelInitialWarmup(t *testing.T) {
	s, runtime := radioLifetimeFixture(t, false)
	t.Setenv("FRAMEWORK_DELAY_CONTROL", "1")
	started := make(chan error, 1)
	go func() { started <- s.startRadio(context.Background(), "") }()
	frameworkWait(t, "initial Site did not enter warmup", func() bool {
		status := runtime.Status()
		return status.State == "starting" && len(status.Channels) > 0 && status.Channels[0].PID != 0
	})
	config := storeFixtureSite()
	config.Channels[0].PowerPercent = 65
	if err := s.Store.SaveConfig(storeDoc(t, config)); err != nil {
		t.Fatal(err)
	}
	s.refreshDirectory()
	select {
	case err := <-started:
		if err == nil {
			t.Fatal("stale settings reached running")
		}
	case <-time.After(5 * time.Second):
		t.Fatal("configuration wakeup did not interrupt initial warmup")
	}
	if runtime.Status().State == "running" || runtime.Status().FNEPID != 0 {
		t.Fatal("cancelled initial startup retained native resources")
	}
	t.Setenv("FRAMEWORK_DELAY_CONTROL", "")
	frameworkWait(t, "caller operation did not retire", func() bool { s.operationMu.Lock(); defer s.operationMu.Unlock(); return s.radioOperation == nil })
	if err := s.startRadio(context.Background(), ""); err != nil {
		t.Fatal(err)
	}
	if runtime.Status().Channels[0].PowerPercent != 65 {
		t.Fatal("fresh product Start used stale settings")
	}
}

func TestFrameworkSavedSettingsReachControlRecovery(t *testing.T) {
	s, runtime := radioLifetimeFixture(t, false)
	if err := s.startRadio(context.Background(), ""); err != nil {
		t.Fatal(err)
	}
	t.Setenv("FRAMEWORK_DELAY_CONTROL", "1")
	_ = syscall.Kill(runtime.Status().Channels[0].PID, syscall.SIGKILL)
	frameworkWait(t, "control failure was not reported", func() bool { return runtime.Status().State == "fault" })
	config := storeFixtureSite()
	config.Channels[0].PowerPercent = 66
	if err := s.Store.SaveConfig(storeDoc(t, config)); err != nil {
		t.Fatal(err)
	}
	s.refreshDirectory()
	frameworkWait(t, "saved settings did not cancel desired control recovery", func() bool {
		status := runtime.Status()
		return status.FNEPID == 0 && strings.Contains(status.Error, "settings changed")
	})
	time.Sleep(1100 * time.Millisecond)
	if runtime.Status().State == "running" || runtime.Status().FNEPID != 0 {
		t.Fatal("stale recovery resumed after saved settings changed")
	}
	t.Setenv("FRAMEWORK_DELAY_CONTROL", "")
	if err := s.startRadio(context.Background(), ""); err != nil {
		t.Fatal(err)
	}
	if runtime.Status().Channels[0].PowerPercent != 66 {
		t.Fatal("explicit restart did not select current settings")
	}
}

func TestFrameworkSameProfileNewRunRetiresAndRecreatesMedia(t *testing.T) {
	s, runtime := radioLifetimeFixture(t, true)
	if err := s.startRadio(context.Background(), ""); err != nil {
		t.Fatal(err)
	}
	s.operationMu.Lock()
	oldCodec, oldRouter := s.codec, s.rfRouter.Load()
	s.operationMu.Unlock()
	if oldCodec == nil || oldRouter == nil {
		t.Fatal("fixture did not establish real codec/router ownership")
	}
	credentials := filepath.Join(t.TempDir(), "zone-credentials.json")
	if err := WritePrivateJSON(credentials, ServiceCredentials{Peers: map[string]PeerCredential{"zone:actual": {Role: RoleZone, InstanceID: "zone-1", Token: strings.Repeat("z", 64)}}}); err != nil {
		t.Fatal(err)
	}
	s.mu.Lock()
	s.config.CredentialFile = credentials
	s.mu.Unlock()
	if _, err := s.broker.Begin(context.Background(), "zone:actual", routing.Request{CallRequest: dispatch.CallRequest{ID: "old-floor", LogicalCallID: "old-floor", AuthorizationGroups: []int{1001}, RadioID: 10001, Talkgroups: []int{1001}}, SourceKind: "rf", OriginSite: "other"}); err != nil {
		t.Fatal(err)
	}
	before := runtime.Status()
	_ = syscall.Kill(before.Channels[0].PID, syscall.SIGKILL)
	frameworkWait(t, "new native lifetime did not rebind same-profile media", func() bool {
		status := runtime.Status()
		s.operationMu.Lock()
		defer s.operationMu.Unlock()
		return status.State == "running" && !status.StartedAt.Equal(before.StartedAt) && s.codec != nil && s.codec != oldCodec && s.rfRouter.Load() != nil && s.rfRouter.Load() != oldRouter && s.radioRunStartedAt.Equal(status.StartedAt)
	})
	if oldRouter.Status().ActiveCalls != 0 {
		t.Fatal("old call floor crossed the new Site lifetime")
	}
	if len(s.broker.router.Active()) != 0 {
		t.Fatal("old broker allocation crossed the new Site lifetime")
	}
	if _, err := oldRouter.Begin(context.Background(), dispatch.CallRequest{ID: "old-router-replay", RadioID: 10001, Talkgroups: []int{1001}}); err == nil {
		t.Fatal("retired router admitted a new call")
	}
	for _, row := range oldCodec.Status() {
		if row.Ready || row.PID != 0 && syscall.Kill(row.PID, 0) == nil {
			t.Fatal("old codec resource was not retired", row)
		}
	}
	s.operationMu.Lock()
	active := s.rfRouter.Load().Status().ActiveCalls
	s.operationMu.Unlock()
	if active != 0 {
		t.Fatal("old floor was replayed into replacement media")
	}
}

func TestFrameworkStopBarrierPreventsMediaRecreationAndNewStart(t *testing.T) {
	s, runtime := radioLifetimeFixture(t, true)
	if err := s.startRadio(context.Background(), ""); err != nil {
		t.Fatal(err)
	}
	runtime.stopEntered = make(chan struct{})
	release := make(chan struct{})
	runtime.stopRelease = release
	stopped := make(chan error, 1)
	go func() { stopped <- s.stopRadio(context.Background(), "") }()
	select {
	case <-runtime.stopEntered:
	case <-time.After(4 * time.Second):
		t.Fatal("Stop never reached adapter")
	}
	// The RF adapter is deliberately still running in this window, after the
	// caller drained media. Reconciliation must not rebuild it before Stop.
	s.reconcileRadio()
	s.operationMu.Lock()
	recreated := s.codec != nil || s.rfRouter.Load() != nil
	s.operationMu.Unlock()
	if recreated {
		t.Fatal("reconciliation recreated media across pending Stop")
	}
	if err := s.startRadio(context.Background(), ""); err == nil {
		t.Fatal("Start passed pending product Stop")
	}
	close(release)
	if err := <-stopped; err != nil {
		t.Fatal(err)
	}
}

func TestFrameworkStopAndNewSettingsInterruptPolicyWarmup(t *testing.T) {
	for _, action := range []string{"stop", "configuration"} {
		t.Run(action, func(t *testing.T) {
			s, runtime := radioLifetimeFixture(t, false)
			if err := s.startRadio(context.Background(), ""); err != nil {
				t.Fatal(err)
			}
			t.Setenv("FRAMEWORK_DELAY_CONTROL", "1")
			system := storeFixtureSystem()
			system.Radios[0].Enabled = false
			if err := s.Store.WriteCache("system", storeDoc(t, system)); err != nil {
				t.Fatal(err)
			}
			s.refreshDirectory()
			frameworkWait(t, "policy replacement did not enter native warmup", func() bool {
				status := runtime.Status()
				return status.State == "starting" && len(status.Channels) > 0 && status.Channels[0].PID != 0
			})
			if action == "stop" {
				ctx, cancel := context.WithTimeout(context.Background(), 4*time.Second)
				defer cancel()
				if err := s.stopRadio(ctx, ""); err != nil {
					t.Fatal(err)
				}
			} else {
				config := storeFixtureSite()
				config.Channels[0].PowerPercent = 67
				if err := s.Store.SaveConfig(storeDoc(t, config)); err != nil {
					t.Fatal(err)
				}
				s.refreshDirectory()
			}
			frameworkWait(t, "old policy warmup did not retire after cancellation", func() bool {
				status := runtime.Status()
				s.operationMu.Lock()
				defer s.operationMu.Unlock()
				return status.State != "running" && status.State != "starting" && status.FNEPID == 0 && s.radioOperation == nil
			})
			if action == "stop" && (runtime.Status().State != "stopped" || runtime.Status().Error != "") {
				t.Fatal("Stop retained stale policy-start fault")
			}
			if action == "configuration" {
				t.Setenv("FRAMEWORK_DELAY_CONTROL", "")
				frameworkWait(t, "new saved policy did not settle", func() bool {
					p, err := s.profileRadio()
					s.operationMu.Lock()
					defer s.operationMu.Unlock()
					return err == nil && s.radioOperation == nil && s.radioCandidateSignature == profileSignature(p)
				})
				if err := s.startRadio(context.Background(), ""); err != nil {
					t.Fatal(err)
				}
				if runtime.Status().Channels[0].PowerPercent != 67 {
					t.Fatal("new Start replayed older policy settings")
				}
			}
		})
	}
}

func TestFrameworkStopJoinsDelayedCallerBeforeNewStart(t *testing.T) {
	s, runtime := radioLifetimeFixture(t, true)
	runtime.startReturnEntered = make(chan struct{})
	release := make(chan struct{})
	runtime.startReturnRelease = release
	started := make(chan error, 1)
	go func() { started <- s.startRadio(context.Background(), "") }()
	select {
	case <-runtime.startReturnEntered:
	case <-time.After(4 * time.Second):
		t.Fatal("Start never reached delayed return")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 200*time.Millisecond)
	defer cancel()
	if err := s.stopRadio(ctx, ""); err == nil {
		t.Fatal("Stop claimed to join a caller that has not returned")
	}
	s.operationMu.Lock()
	barrier := s.radioOperation != nil && s.radioOperation.stopping
	s.operationMu.Unlock()
	if !barrier || runtime.Status().State == "running" {
		t.Fatal("timed-out join lost its barrier or left RF running")
	}
	if err := s.startRadio(context.Background(), ""); err == nil {
		t.Fatal("new Start passed an unjoined old caller")
	}
	close(release)
	if err := <-started; err == nil {
		t.Fatal("cancelled delayed Start reported success")
	}
	frameworkWait(t, "joined caller did not release Stop barrier", func() bool { s.operationMu.Lock(); defer s.operationMu.Unlock(); return s.radioOperation == nil })
	if err := s.startRadio(context.Background(), ""); err != nil {
		t.Fatal("fresh Start failed after old caller retired", err)
	}
	s.operationMu.Lock()
	ready := s.codec != nil && s.rfRouter.Load() != nil && s.radioOperation == nil
	s.operationMu.Unlock()
	if !ready || runtime.Status().State != "running" {
		t.Fatal("old completion damaged replacement media")
	}
}

func TestFrameworkOldRouterCannotAcquireReplacementControlClient(t *testing.T) {
	s, runtime := radioLifetimeFixture(t, true)
	if err := s.startRadio(context.Background(), ""); err != nil {
		t.Fatal(err)
	}
	s.operationMu.Lock()
	defer s.operationMu.Unlock()
	oldRouter := s.rfRouter.Load()
	before := runtime.Status()
	t.Setenv("FRAMEWORK_AFFILIATED", "1")
	_ = syscall.Kill(before.Channels[0].PID, syscall.SIGKILL)
	// Deliberately pause framework reconciliation while the real radio
	// supervisor replaces the Site. The old router must retain its old client.
	frameworkWait(t, "replacement controller did not become ready in polling gap", func() bool {
		status := runtime.Status()
		return status.State == "running" && !status.StartedAt.Equal(before.StartedAt) && runtime.HasRFInterest(1001)
	})
	client, err := runtime.ControlClient()
	if err != nil {
		t.Fatal(err)
	}
	if _, err = client.Put(context.Background(), "/p25/dispatch/grant", map[string]any{"srcId": 10001, "dstId": 1001, "leaseId": "current-owner-fixture"}); err != nil {
		t.Fatal("replacement fixture did not accept its current client", err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	if _, err = oldRouter.Begin(ctx, dispatch.CallRequest{ID: "old-router-new-run", RadioID: 10001, Talkgroups: []int{1001}}); err == nil {
		t.Fatal("old router acquired the replacement allocator/secret before reconciliation")
	}
}
