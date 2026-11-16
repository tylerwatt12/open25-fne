//go:build linux || darwin

package framework

import (
	"bytes"
	"context"
	"errors"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"

	"open25.local/open25-fne/internal/radio"
)

func saveStartupSetting(t *testing.T, s *Service, enabled bool) {
	t.Helper()
	var site SiteConfig
	if err := s.owned(&site); err != nil {
		t.Fatal(err)
	}
	site.StartOnServiceStart = enabled
	if err := s.saveOwned(storeDoc(t, site)); err != nil {
		t.Fatal(err)
	}
	s.refreshDirectory()
}

func startupTestError(s *Service) string {
	s.operationMu.Lock()
	defer s.operationMu.Unlock()
	return s.radioStartupError
}

func runStartupFixture(t *testing.T, s *Service) func() {
	t.Helper()
	// macOS Unix socket paths must remain shorter than testing's named TempDir.
	directory, err := os.MkdirTemp("/tmp", "open25-start-")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(directory) })
	s.mu.Lock()
	s.config.StateDir = directory
	s.mu.Unlock()
	lock, err := os.OpenFile(filepath.Join(directory, "service.lock"), os.O_CREATE|os.O_RDWR, 0600)
	if err != nil {
		t.Fatal(err)
	}
	if err = syscall.Flock(int(lock.Fd()), syscall.LOCK_EX|syscall.LOCK_NB); err != nil {
		t.Fatal(err)
	}
	s.lock = lock
	s.mu.Lock()
	s.config.Listen, s.config.InternalListen, s.config.PollSeconds = "127.0.0.1:0", "127.0.0.1:0", 1
	s.mu.Unlock()
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- s.Run(ctx) }()
	var once sync.Once
	stop := func() {
		once.Do(func() {
			cancel()
			select {
			case err := <-done:
				if err != nil {
					t.Errorf("service Run: %v", err)
				}
			case <-time.After(8 * time.Second):
				t.Error("service startup worker did not shut down")
			}
			_ = syscall.Flock(int(lock.Fd()), syscall.LOCK_UN)
			_ = lock.Close()
		})
	}
	t.Cleanup(stop)
	t.Cleanup(func() {
		if t.Failed() {
			t.Logf("automatic startup error: %s; radio: %+v", startupTestError(s), s.radioStatus())
		}
	})
	frameworkWait(t, "local control listener did not start", func() bool {
		info, err := os.Stat(filepath.Join(s.Config().StateDir, "control.sock"))
		return err == nil && info.Mode()&os.ModeSocket != 0
	})
	return stop
}

func TestSiteServiceStartupSettingControlsActualRun(t *testing.T) {
	for _, enabled := range []bool{false, true} {
		t.Run(map[bool]string{false: "off", true: "on"}[enabled], func(t *testing.T) {
			s, runtime := radioLifetimeFixture(t, false)
			saveStartupSetting(t, s, enabled)
			runStartupFixture(t, s)
			if !enabled {
				saveStartupSetting(t, s, true)
				time.Sleep(700 * time.Millisecond)
				if runtime.Status().State != "stopped" {
					t.Fatal("Save started RF in the current service run")
				}
				return
			}
			frameworkWait(t, "enabled service did not start native workers", func() bool { return runtime.Status().State == "running" })
			status := runtime.Status()
			if status.FNEPID == 0 || len(status.Channels) == 0 || status.Channels[0].PID == 0 {
				t.Fatal("automatic startup did not use the native Controller", status)
			}
			if err := s.stopRadio(context.Background(), ""); err != nil {
				t.Fatal(err)
			}
			time.Sleep(1200 * time.Millisecond)
			if runtime.Status().State != "stopped" {
				t.Fatal("automatic startup overrode Stop Site")
			}
			var saved SiteConfig
			if err := s.owned(&saved); err != nil || !saved.StartOnServiceStart {
				t.Fatal("Stop changed the saved startup preference", err)
			}
		})
	}
}

func TestSiteAutomaticStartupStopBeforeWorkerAndDuringWarmup(t *testing.T) {
	for _, phase := range []string{"before worker", "warmup"} {
		t.Run(phase, func(t *testing.T) {
			s, runtime := radioLifetimeFixture(t, false)
			saveStartupSetting(t, s, true)
			parent, cancel := context.WithCancel(context.Background())
			defer cancel()
			defer func() {
				if t.Failed() {
					t.Logf("automatic startup error: %s", startupTestError(s))
				}
			}()
			ctx := s.radioStartupContext(parent)
			if phase == "before worker" {
				if err := s.stopRadio(context.Background(), ""); err != nil {
					t.Fatal(err)
				}
				s.runRadioStartup(ctx)
			} else {
				t.Setenv("FRAMEWORK_DELAY_CONTROL", "1")
				done := make(chan struct{})
				go func() { defer close(done); s.runRadioStartup(ctx) }()
				frameworkWait(t, "automatic startup did not enter warmup", func() bool {
					status := runtime.Status()
					return status.State == "starting" && len(status.Channels) > 0 && status.Channels[0].PID != 0
				})
				if err := s.stopRadio(context.Background(), ""); err != nil {
					t.Fatal(err)
				}
				select {
				case <-done:
				case <-time.After(3 * time.Second):
					t.Fatal("Stop did not retire automatic startup")
				}
			}
			if ctx.Err() == nil || runtime.Status().State != "stopped" {
				t.Fatal("Stop did not cancel this service's startup")
			}
		})
	}
}

// The existing runtime interface supplies a failed native startup without RF.
type failingStartupRadio struct {
	*operationRadio
	attempts         chan struct{}
	recoverOnAttempt int
}

func (r *failingStartupRadio) Start(context.Context, radio.Profile, string) error {
	r.mu.Lock()
	r.starts++
	recovered := r.recoverOnAttempt > 0 && r.starts >= r.recoverOnAttempt
	if recovered {
		r.status.State, r.status.StartedAt = "running", time.Now().UTC()
	}
	r.mu.Unlock()
	r.attempts <- struct{}{}
	if recovered {
		return nil
	}
	return errors.New("test modem is temporarily unavailable")
}

func TestSiteAutomaticStartupRecoversAfterTemporaryFailure(t *testing.T) {
	var runtime *failingStartupRadio
	s, _ := operationConfiguredFixture(t, func(s *Service) {
		base := s.radio.(*operationRadio)
		base.status.State = "stopped"
		runtime = &failingStartupRadio{operationRadio: base, attempts: make(chan struct{}, 8), recoverOnAttempt: 2}
		s.radio = runtime
	})
	saveStartupSetting(t, s, true)
	ctx := s.radioStartupContext(context.Background())
	done := make(chan struct{})
	go func() { defer close(done); s.runRadioStartup(ctx) }()
	select {
	case <-done:
	case <-time.After(4 * time.Second):
		t.Fatal("automatic startup did not recover")
	}
	if runtime.Status().State != "running" || len(runtime.attempts) != 2 {
		t.Fatal("failed startup was not retried through the existing Start path")
	}
}

func TestSiteAutomaticStartupMaintenanceRefusalNeverReplays(t *testing.T) {
	var runtime *failingStartupRadio
	s, _ := operationConfiguredFixture(t, func(s *Service) {
		base := s.radio.(*operationRadio)
		base.status.State = "stopped"
		runtime = &failingStartupRadio{operationRadio: base, attempts: make(chan struct{}, 8)}
		s.radio = runtime
	})
	saveStartupSetting(t, s, true)
	lock, err := os.OpenFile(filepath.Join(s.Config().StateDir, "radio-maintenance.lock"), os.O_CREATE|os.O_RDWR, 0600)
	if err != nil {
		t.Fatal(err)
	}
	defer lock.Close()
	if err := syscall.Flock(int(lock.Fd()), syscall.LOCK_EX|syscall.LOCK_NB); err != nil {
		t.Fatal(err)
	}
	defer syscall.Flock(int(lock.Fd()), syscall.LOCK_UN)
	ctx := s.radioStartupContext(context.Background())
	done := make(chan struct{})
	go func() { defer close(done); s.runRadioStartup(ctx) }()
	select {
	case <-done:
	case <-time.After(time.Second):
		t.Fatal("maintenance refusal was queued for retry")
	}
	if err := syscall.Flock(int(lock.Fd()), syscall.LOCK_UN); err != nil {
		t.Fatal(err)
	}
	time.Sleep(300 * time.Millisecond)
	if len(runtime.attempts) != 0 {
		t.Fatal("maintenance refusal replayed a radio start")
	}
}

func TestSiteAutomaticStartupRetryStopsAndDoesNotApplyChangedProgramming(t *testing.T) {
	for _, action := range []string{"stop", "changed programming", "shutdown"} {
		t.Run(action, func(t *testing.T) {
			var runtime *failingStartupRadio
			s, _ := operationConfiguredFixture(t, func(s *Service) {
				base := s.radio.(*operationRadio)
				base.status.State = "stopped"
				runtime = &failingStartupRadio{operationRadio: base, attempts: make(chan struct{}, 8)}
				s.radio = runtime
			})
			saveStartupSetting(t, s, true)
			parent, cancel := context.WithCancel(context.Background())
			defer cancel()
			ctx := s.radioStartupContext(parent)
			done := make(chan struct{})
			go func() { defer close(done); s.runRadioStartup(ctx) }()
			for i := 0; i < 2; i++ {
				select {
				case <-runtime.attempts:
				case <-time.After(3 * time.Second):
					t.Fatalf("startup did not retry: %s", startupTestError(s))
				}
			}
			var out bytes.Buffer
			frameworkWait(t, "retry reason not visible", func() bool {
				out.Reset()
				s.webSiteAttention(&out, runtime.Status())
				return strings.Contains(out.String(), "Automatic Site startup waiting to retry: test modem is temporarily unavailable")
			})
			switch action {
			case "stop":
				if err := s.stopRadio(context.Background(), ""); err != nil {
					t.Fatal(err)
				}
			case "shutdown":
				cancel()
			case "changed programming":
				var site SiteConfig
				if err := s.owned(&site); err != nil {
					t.Fatal(err)
				}
				site.NAC++
				if err := s.saveOwned(storeDoc(t, site)); err != nil {
					t.Fatal(err)
				}
			}
			select {
			case <-done:
			case <-time.After(4 * time.Second):
				t.Fatal("startup intent survived cancellation or changed programming")
			}
			select {
			case <-runtime.attempts:
				t.Fatal("startup attempted RF after cancellation or changed programming")
			default:
			}
		})
	}
}

func TestSiteStartupFormSavesOnlyOwnedPreferenceWithoutRF(t *testing.T) {
	c := integrationNewCluster(t)
	for _, pair := range []*integrationService{c.site, c.otherSite} {
		s := pair.service
		client := earlyWebLogin(t, pair)
		initial, err := s.profileRadio()
		if err != nil {
			t.Fatal(err)
		}
		page := earlyWebGet(t, client, pair.web.URL+"/control-policy", http.StatusOK)
		if !bytes.Contains(page, []byte("Start radios when Site Controller starts")) {
			t.Fatal("missing startup label")
		}
		form := earlyWebForm(t, page, "start_on_service_start")
		if form.Has("start_on_service_start") {
			t.Fatal("automatic startup was enabled by default")
		}
		form.Set("start_on_service_start", "true")
		earlyWebPost(t, client, pair.web.URL+"/control-policy", form, pair.web.URL, http.StatusSeeOther)
		var saved SiteConfig
		if err := s.owned(&saved); err != nil || !saved.StartOnServiceStart {
			t.Fatal("startup preference did not persist", err)
		}
		after, err := s.profileRadio()
		if err != nil || profileSignature(initial) != profileSignature(after) {
			t.Fatal("startup preference changed the RF programming fingerprint", err)
		}
		if s.radioStatus().State != "stopped" {
			t.Fatal("saving the preference started RF")
		}
		page = earlyWebGet(t, client, pair.web.URL+"/control-policy", http.StatusOK)
		form = earlyWebForm(t, page, "start_on_service_start")
		if !form.Has("start_on_service_start") {
			t.Fatal("saved preference was not checked")
		}
		form.Del("start_on_service_start")
		earlyWebPost(t, client, pair.web.URL+"/control-policy", form, pair.web.URL, http.StatusSeeOther)
		if err := s.owned(&saved); err != nil || saved.StartOnServiceStart {
			t.Fatal("unchecking preference did not persist", err)
		}
	}
	if c.system.service.radioStartupContext(context.Background()) != nil || c.zone.service.radioStartupContext(context.Background()) != nil || c.dispatch.service.radioStartupContext(context.Background()) != nil {
		t.Fatal("non-Site service scheduled radio startup")
	}
}
