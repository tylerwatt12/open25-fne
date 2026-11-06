package framework

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"

	"golang.org/x/sys/unix"
	"open25.local/open25-fne/internal/radio"
)

type maintenanceRadio struct {
	*operationRadio
	entered       chan string
	release       <-chan struct{}
	wanted        bool
	channelStarts int
	channelIDs    []string
}

func (r *maintenanceRadio) Start(ctx context.Context, _ radio.Profile, _ string) error {
	r.mu.Lock()
	r.starts++
	r.wanted, r.status.State = true, "starting"
	r.mu.Unlock()
	if r.entered != nil {
		r.entered <- "site"
	}
	if r.release != nil {
		select {
		case <-r.release:
		case <-ctx.Done():
			return ctx.Err()
		}
	}
	r.mu.Lock()
	r.status.State = "running"
	r.mu.Unlock()
	return nil
}

func (r *maintenanceRadio) Stop(ctx context.Context) error {
	r.mu.Lock()
	r.wanted = false
	r.mu.Unlock()
	return r.operationRadio.Stop(ctx)
}

func (r *maintenanceRadio) StartChannel(ctx context.Context, id string) error {
	r.mu.Lock()
	r.channelStarts++
	r.channelIDs = append(r.channelIDs, id)
	r.mu.Unlock()
	if r.entered != nil {
		r.entered <- id
	}
	if r.release != nil {
		select {
		case <-r.release:
		case <-ctx.Done():
			return ctx.Err()
		}
	}
	return nil
}

func maintenanceService(t *testing.T) (*Service, *maintenanceRadio, string) {
	t.Helper()
	s, original, config := localControlFixture(t)
	runtime := &maintenanceRadio{operationRadio: original.operationRadio, entered: make(chan string, 8)}
	runtime.mu.Lock()
	runtime.status.State = "stopped"
	runtime.status.MaintenanceSupported = true
	runtime.mu.Unlock()
	s.radio = runtime
	return s, runtime, config
}

func maintenanceExclusive(t *testing.T, s *Service) *os.File {
	t.Helper()
	file, err := os.OpenFile(filepath.Join(s.Config().StateDir, "radio-maintenance.lock"), os.O_CREATE|os.O_RDWR, 0600)
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

func maintenanceHTTP(t *testing.T, s *Service, action, body string) *httptest.ResponseRecorder {
	t.Helper()
	response := httptest.NewRecorder()
	request := httptest.NewRequest(http.MethodPost, "/api/radio/"+action, strings.NewReader(body))
	request.Header.Set("Content-Type", "application/json")
	if !s.operationsHTTP(response, request, Session{}) {
		t.Fatal("radio operation was not handled")
	}
	return response
}

func TestRadioMaintenanceHTTPRejectsBeforeAcceptanceAndNeverReplays(t *testing.T) {
	s, runtime, _ := maintenanceService(t)
	owner := maintenanceExclusive(t, s)
	for _, body := range []string{`{}`, `{"channel_id":"channel-voice"}`} {
		response := maintenanceHTTP(t, s, "start", body)
		if response.Code != http.StatusServiceUnavailable || !strings.Contains(response.Body.String(), radio.ErrMaintenance.Error()) {
			t.Fatalf("start accepted during maintenance: HTTP%d %s", response.Code, response.Body.String())
		}
	}
	response := maintenanceHTTP(t, s, "stop", `{}`)
	if response.Code != http.StatusAccepted {
		t.Fatalf("Stop unavailable during maintenance: %d", response.Code)
	}
	s.operationWG.Wait()
	_ = owner.Close()
	// A later configuration wakeup may update stopped policy, but must not
	// find or replay a Start that was rejected before asynchronous acceptance.
	s.operationMu.Lock()
	s.radioCandidateSignature = "saved-configuration-changed-during-maintenance"
	s.operationMu.Unlock()
	s.reconcileRadio()
	runtime.mu.Lock()
	defer runtime.mu.Unlock()
	if runtime.starts != 0 || runtime.channelStarts != 0 || runtime.wanted || runtime.status.State != "stopped" || runtime.stops != 1 {
		t.Fatalf("release/configuration replayed rejected work: starts%d channels%d wanted%v state%s stops%d", runtime.starts, runtime.channelStarts, runtime.wanted, runtime.status.State, runtime.stops)
	}
}

func TestRadioMaintenanceLocalControlStopAndStatusRemainAvailable(t *testing.T) {
	s, runtime, config := maintenanceService(t)
	closeControl, err := s.startLocalControl(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	defer closeControl()
	owner := maintenanceExclusive(t, s)
	for _, channel := range []string{"", "channel-voice"} {
		if _, err = ControlRadio(context.Background(), config, "start", channel); err == nil || !strings.Contains(err.Error(), radio.ErrMaintenance.Error()) {
			t.Fatalf("local Start passed maintenance: %v", err)
		}
	}
	status, err := ControlRadio(context.Background(), config, "status", "")
	if err != nil || !status.MaintenanceSupported {
		t.Fatalf("maintenance capability/status unavailable: %+v %v", status, err)
	}
	if _, err = ControlRadio(context.Background(), config, "stop", ""); err != nil {
		t.Fatalf("local Stop passed through unavailable guard: %v", err)
	}
	_ = owner.Close()
	s.reconcileRadio()
	runtime.mu.Lock()
	defer runtime.mu.Unlock()
	if runtime.starts != 0 || runtime.channelStarts != 0 || runtime.wanted {
		t.Fatal("local rejected Start was replayed")
	}
}

func TestRadioMaintenanceHTTPHoldsSharedLeaseThroughoutAsyncStartup(t *testing.T) {
	for _, channel := range []bool{false, true} {
		t.Run(map[bool]string{false: "site", true: "channel"}[channel], func(t *testing.T) {
			s, runtime, _ := maintenanceService(t)
			release := make(chan struct{})
			runtime.release = release
			body := `{}`
			if channel {
				body = `{"channel_id":"channel-voice"}`
			}
			response := maintenanceHTTP(t, s, "start", body)
			if response.Code != http.StatusAccepted {
				t.Fatalf("start HTTP%d", response.Code)
			}
			select {
			case <-runtime.entered:
			case <-time.After(2 * time.Second):
				t.Fatal("fake adapter never entered startup")
			}
			path := filepath.Join(s.Config().StateDir, "radio-maintenance.lock")
			owner, err := os.OpenFile(path, os.O_RDWR, 0)
			if err != nil {
				t.Fatal(err)
			}
			defer owner.Close()
			if err = unix.Flock(int(owner.Fd()), unix.LOCK_EX|unix.LOCK_NB); !errors.Is(err, unix.EWOULDBLOCK) {
				t.Fatalf("maintenance entered during startup: %v", err)
			}
			close(release)
			s.operationWG.Wait()
			if err = unix.Flock(int(owner.Fd()), unix.LOCK_EX|unix.LOCK_NB); err != nil {
				t.Fatalf("startup leaked shared lease: %v", err)
			}
			if err = s.stopRadio(context.Background(), ""); err != nil {
				t.Fatalf("Stop blocked under EX: %v", err)
			}
		})
	}
}

func TestRadioMaintenanceReconcileAndRestartShareTheSameInode(t *testing.T) {
	s, runtime, _ := maintenanceService(t)
	owner := maintenanceExclusive(t, s)
	s.operationMu.Lock()
	s.radioCandidateSignature = "new-policy"
	s.operationMu.Unlock()
	s.reconcileRadio()
	select {
	case <-runtime.applications:
		t.Fatal("reconciliation entered adapter under EX")
	default:
	}
	// A newly constructed Site wrapper must observe the same file lock,
	// independently of the previous wrapper's in-memory operation state.
	restarted := &Service{config: s.Config(), radio: runtime}
	if err := restarted.startRadio(context.Background(), ""); !errors.Is(err, radio.ErrMaintenance) {
		t.Fatalf("restarted Site passed EX: %v", err)
	}
	if !restarted.radioStatus().MaintenanceSupported {
		t.Fatal("guarded Site lost capability")
	}
	_ = owner.Close()
	s.reconcileRadio()
	runtime.mu.Lock()
	defer runtime.mu.Unlock()
	if runtime.starts != 0 || runtime.wanted || runtime.status.State != "stopped" {
		t.Fatal("reconcile/restart replayed activity after release")
	}
}

func TestRadioMaintenanceReconcileKeepsLeaseUntilAdapterReturns(t *testing.T) {
	s, runtime, _ := maintenanceService(t)
	blocked := make(chan struct{})
	runtime.applyWait = blocked
	s.operationMu.Lock()
	s.radioCandidateSignature = "new-policy"
	s.operationMu.Unlock()
	done := make(chan struct{})
	go func() { s.reconcileRadio(); close(done) }()
	select {
	case <-runtime.applications:
	case <-time.After(2 * time.Second):
		t.Fatal("reconcile did not reach adapter")
	}
	owner, err := os.OpenFile(filepath.Join(s.Config().StateDir, "radio-maintenance.lock"), os.O_RDWR, 0)
	if err != nil {
		t.Fatal(err)
	}
	defer owner.Close()
	if err = unix.Flock(int(owner.Fd()), unix.LOCK_EX|unix.LOCK_NB); !errors.Is(err, unix.EWOULDBLOCK) {
		t.Fatalf("maintenance entered policy startup: %v", err)
	}
	if err = s.stopRadio(context.Background(), ""); err != nil {
		t.Fatal(err)
	}
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("Stop failed to cancel policy warmup")
	}
	if err = unix.Flock(int(owner.Fd()), unix.LOCK_EX|unix.LOCK_NB); err != nil {
		t.Fatalf("cancelled policy leaked lease: %v", err)
	}
}

func TestRadioMaintenanceWrapperDoesNotInventAdapterSupport(t *testing.T) {
	s, runtime, _ := maintenanceService(t)
	runtime.mu.Lock()
	runtime.status.MaintenanceSupported = false
	runtime.mu.Unlock()
	if s.radioStatus().MaintenanceSupported {
		t.Fatal("wrapper upgraded unsupported adapter")
	}
}

func admissionService(t *testing.T) (*Service, *maintenanceRadio) {
	t.Helper()
	s, runtime, _ := maintenanceService(t)
	site := storeFixtureSite()
	for i, suffix := range []string{"a", "b"} {
		id := "modem-voice-" + suffix
		site.Modems = append(site.Modems, Modem{ID: id, Name: "Voice " + suffix, Device: "/offline/" + id, Baud: 115200})
		channel := site.Channels[0]
		channel.ID, channel.Name, channel.Role, channel.ModemID = "voice-"+suffix, "Voice "+suffix, "voice", id
		channel.ChannelNo += i + 1
		site.Channels = append(site.Channels, channel)
	}
	if err := s.Store.SaveConfig(storeDoc(t, site)); err != nil {
		t.Fatal(err)
	}
	s.refreshDirectory()
	return s, runtime
}

func testAdmission(t *testing.T, s *Service, channel string) *radioStartAdmission {
	t.Helper()
	admission, err := s.admitRadioStart(context.Background(), channel)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		select {
		case <-admission.done:
		default:
			s.finishRadioStart(admission)
		}
	})
	return admission
}

func awaitAdmissionCancel(t *testing.T, admission *radioStartAdmission) {
	t.Helper()
	select {
	case <-admission.ctx.Done():
	case <-time.After(time.Second):
		t.Fatal("accepted Start was not canceled")
	}
}

func awaitStop(t *testing.T, done <-chan error) {
	t.Helper()
	select {
	case err := <-done:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("Stop did not join retired admission")
	}
}

func TestRadioAdmissionLaterStopJoinsCanceledWorkerAndSharedLease(t *testing.T) {
	s, runtime := admissionService(t)
	admission := testAdmission(t, s, "")
	done := make(chan error, 1)
	go func() { done <- s.stopRadio(context.Background(), "") }()
	awaitAdmissionCancel(t, admission)
	select {
	case <-done:
		t.Fatal("Stop returned before admitted worker/SH retirement")
	default:
	}
	owner, err := os.OpenFile(filepath.Join(s.Config().StateDir, "radio-maintenance.lock"), os.O_RDWR, 0)
	if err != nil {
		t.Fatal(err)
	}
	defer owner.Close()
	if err = unix.Flock(int(owner.Fd()), unix.LOCK_EX|unix.LOCK_NB); !errors.Is(err, unix.EWOULDBLOCK) {
		t.Fatalf("EX overtook admitted worker: %v", err)
	}
	if err = s.startRadioAdmitted(admission.ctx, admission); !errors.Is(err, context.Canceled) {
		t.Fatalf("older accepted Start ran after Stop: %v", err)
	}
	s.finishRadioStart(admission)
	awaitStop(t, done)
	if err = unix.Flock(int(owner.Fd()), unix.LOCK_EX|unix.LOCK_NB); err != nil {
		t.Fatalf("Stop returned before SH close: %v", err)
	}
	runtime.mu.Lock()
	defer runtime.mu.Unlock()
	if runtime.starts != 0 || runtime.wanted || runtime.status.State != "stopped" {
		t.Fatal("canceled acceptance restored running intent")
	}
}

func TestRadioAdmissionChannelStopPreservesOtherChannels(t *testing.T) {
	s, runtime := admissionService(t)
	a, b := testAdmission(t, s, "voice-a"), testAdmission(t, s, "voice-b")
	done := make(chan error, 1)
	go func() { done <- s.stopRadio(context.Background(), "voice-a") }()
	awaitAdmissionCancel(t, a)
	if b.ctx.Err() != nil {
		t.Fatal("channel Stop canceled unrelated channel")
	}
	if err := s.startRadioAdmitted(a.ctx, a); !errors.Is(err, context.Canceled) {
		t.Fatal(err)
	}
	s.finishRadioStart(a)
	awaitStop(t, done)
	if b.ctx.Err() != nil {
		t.Fatal("channel Stop completion canceled unrelated intent")
	}
	if err := s.startRadioAdmitted(b.ctx, b); err != nil {
		t.Fatal(err)
	}
	s.finishRadioStart(b)
	runtime.mu.Lock()
	defer runtime.mu.Unlock()
	if !reflect.DeepEqual(runtime.channelIDs, []string{"voice-b"}) || runtime.starts != 0 {
		t.Fatalf("wrong accepted work ran: %v", runtime.channelIDs)
	}
}

func TestRadioAdmissionChannelStopCancelsPendingWholeSite(t *testing.T) {
	s, runtime := admissionService(t)
	admission := testAdmission(t, s, "")
	done := make(chan error, 1)
	go func() { done <- s.stopRadio(context.Background(), "voice-a") }()
	awaitAdmissionCancel(t, admission)
	if err := s.startRadioAdmitted(admission.ctx, admission); !errors.Is(err, context.Canceled) {
		t.Fatal(err)
	}
	s.finishRadioStart(admission)
	awaitStop(t, done)
	runtime.mu.Lock()
	defer runtime.mu.Unlock()
	if runtime.starts != 0 {
		t.Fatal("whole-Site Start restarted stopped channel")
	}
}

func TestRadioAdmissionWholeStopJoinsEveryChannel(t *testing.T) {
	s, _ := admissionService(t)
	a, b := testAdmission(t, s, "voice-a"), testAdmission(t, s, "voice-b")
	done := make(chan error, 1)
	go func() { done <- s.stopRadio(context.Background(), "") }()
	awaitAdmissionCancel(t, a)
	awaitAdmissionCancel(t, b)
	s.finishRadioStart(a)
	select {
	case <-done:
		t.Fatal("Stop ignored another admitted channel")
	default:
	}
	if _, err := s.admitRadioStart(context.Background(), "voice-a"); err == nil {
		t.Fatal("new admission crossed Stop barrier")
	}
	s.finishRadioStart(b)
	awaitStop(t, done)
}

func TestRadioAdmissionDeadlineRetainsBarrierUntilWorkerRetires(t *testing.T) {
	s, _ := admissionService(t)
	admission := testAdmission(t, s, "")
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Millisecond)
	defer cancel()
	if err := s.stopRadio(ctx, ""); !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("Stop deadline: %v", err)
	}
	if _, err := s.admitRadioStart(context.Background(), ""); err == nil {
		t.Fatal("deadline released pending Stop barrier")
	}
	s.operationMu.Lock()
	barrier := s.radioOperation
	s.operationMu.Unlock()
	if barrier == nil || !barrier.stopping {
		t.Fatal("missing retirement barrier")
	}
	s.finishRadioStart(admission)
	select {
	case <-barrier.done:
	case <-time.After(time.Second):
		t.Fatal("retired worker did not release barrier")
	}
	owner := maintenanceExclusive(t, s)
	_ = owner.Close()
}

func TestRadioAdmissionConfigurationCancelsOlderAcceptedWork(t *testing.T) {
	for _, wake := range []bool{false, true} {
		t.Run(map[bool]string{false: "worker-rechecks-without-wakeup", true: "configuration-wakeup-cancels"}[wake], func(t *testing.T) {
			s, runtime := admissionService(t)
			a, b := testAdmission(t, s, "voice-a"), testAdmission(t, s, "voice-b")
			system := s.view().System
			system.Callsign = "K9TEST"
			if err := s.Store.WriteCache("system", storeDoc(t, system)); err != nil {
				t.Fatal(err)
			}
			s.refreshDirectory()
			if wake {
				s.cancelStaleRadioOperation()
				awaitAdmissionCancel(t, a)
				awaitAdmissionCancel(t, b)
			}
			for _, admission := range []*radioStartAdmission{a, b} {
				if err := s.startRadioAdmitted(admission.ctx, admission); !errors.Is(err, context.Canceled) {
					t.Fatal(err)
				}
				s.finishRadioStart(admission)
			}
			runtime.mu.Lock()
			defer runtime.mu.Unlock()
			if runtime.starts != 0 || runtime.channelStarts != 0 {
				t.Fatal("configuration invalidation replayed accepted Start")
			}
		})
	}
}

func TestRadioAdmissionRejectsDuplicateConflictingAndUnboundedWork(t *testing.T) {
	s, _ := admissionService(t)
	whole := testAdmission(t, s, "")
	for _, channel := range []string{"", "voice-a"} {
		if _, err := s.admitRadioStart(context.Background(), channel); err == nil {
			t.Fatal("conflicting admission queued")
		}
	}
	s.finishRadioStart(whole)
	voice := testAdmission(t, s, "voice-a")
	for _, channel := range []string{"", "voice-a"} {
		if _, err := s.admitRadioStart(context.Background(), channel); err == nil {
			t.Fatal("duplicate/channel conflict queued")
		}
	}
	b := testAdmission(t, s, "voice-b")
	extra := testAdmission(t, s, "adapter-rejects-unknown")
	if _, err := s.admitRadioStart(context.Background(), "beyond-configured-capacity"); err == nil {
		t.Fatal("unbounded distinct admissions accepted")
	}
	s.finishRadioStart(voice)
	s.finishRadioStart(b)
	s.finishRadioStart(extra)
}

func TestRadioAdmissionHTTPUsesServiceLifetimeAfterRequestEnds(t *testing.T) {
	s, runtime := admissionService(t)
	release := make(chan struct{})
	runtime.release = release
	ctx, cancel := context.WithCancel(context.Background())
	request := httptest.NewRequest(http.MethodPost, "/api/radio/start", strings.NewReader(`{}`)).WithContext(ctx)
	request.Header.Set("Content-Type", "application/json")
	response := httptest.NewRecorder()
	if !s.operationsHTTP(response, request, Session{}) || response.Code != http.StatusAccepted {
		t.Fatalf("positive startup HTTP%d %s", response.Code, response.Body.String())
	}
	cancel()
	select {
	case <-runtime.entered:
	case <-time.After(time.Second):
		t.Fatal("positive accepted HTTP startup never ran")
	}
	s.operationMu.Lock()
	admission := s.radioStarts[""]
	s.operationMu.Unlock()
	if admission == nil || admission.ctx.Err() != nil {
		t.Fatal("request disconnect canceled service-owned acceptance")
	}
	duplicate := maintenanceHTTP(t, s, "start", `{}`)
	if duplicate.Code != http.StatusServiceUnavailable {
		t.Fatalf("duplicate HTTP%d", duplicate.Code)
	}
	close(release)
	s.operationWG.Wait()
	runtime.mu.Lock()
	defer runtime.mu.Unlock()
	if runtime.starts != 1 || !runtime.wanted || runtime.status.State != "running" {
		t.Fatal("positive HTTP startup did not complete")
	}
}
