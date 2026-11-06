package framework

import (
	"context"
	"errors"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"syscall"
	"testing"

	"open25.local/open25-fne/internal/radio"
)

type localTestRadio struct {
	*operationRadio
	startedChannels, stoppedChannels []string
	startError                       error
}

func (r *localTestRadio) Start(ctx context.Context, p radio.Profile, mode string) error {
	if r.startError != nil {
		return r.startError
	}
	return r.operationRadio.Start(ctx, p, mode)
}
func (r *localTestRadio) StartChannel(_ context.Context, id string) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.startedChannels = append(r.startedChannels, id)
	return nil
}
func (r *localTestRadio) StopChannel(_ context.Context, id string) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.stoppedChannels = append(r.stoppedChannels, id)
	return nil
}

func localControlFixture(t *testing.T) (*Service, *localTestRadio, string) {
	t.Helper()
	s, original := operationFixture(t)
	s.operationCancel()
	s.operationWG.Wait()
	// Stop the background reconciler while keeping this endpoint fixture's
	// service/request lifetime live. Start now observes that lifetime too.
	s.operationCtx, s.operationCancel = context.WithCancel(context.Background())
	// Unix socket path limits are short on macOS. Keep this owned test path
	// short instead of depending on testing's long named temporary directory.
	directory, err := os.MkdirTemp("/tmp", "ts-control-")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(directory) })
	s.mu.Lock()
	s.config.StateDir = directory
	s.config.Listen = "127.0.0.1:5002"
	s.config.InternalListen = "127.0.0.1:5102"
	s.config.InternalURL = "http://127.0.0.1:5102"
	s.config.UpstreamURL = "http://127.0.0.1:5001"
	s.config.CredentialFile = filepath.Join(directory, "service-credentials.json")
	s.config.PollSeconds = 2
	c := s.config
	s.mu.Unlock()
	lock, err := os.OpenFile(filepath.Join(directory, "service.lock"), os.O_CREATE|os.O_RDWR, 0600)
	if err != nil {
		t.Fatal(err)
	}
	if err = syscall.Flock(int(lock.Fd()), syscall.LOCK_EX|syscall.LOCK_NB); err != nil {
		_ = lock.Close()
		t.Fatal(err)
	}
	s.lock = lock
	t.Cleanup(func() { _ = syscall.Flock(int(lock.Fd()), syscall.LOCK_UN); _ = lock.Close(); s.lock = nil })
	runtime := &localTestRadio{operationRadio: original}
	s.radio = runtime
	path := filepath.Join(directory, "service.json")
	if err = WritePrivateJSON(path, c); err != nil {
		t.Fatal(err)
	}
	return s, runtime, path
}

func TestLocalRadioSocketControlsOnlyTheRunningSite(t *testing.T) {
	s, runtime, configPath := localControlFixture(t)
	closeControl, err := s.startLocalControl(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	defer closeControl()
	info, err := os.Stat(filepath.Join(s.Config().StateDir, "control.sock"))
	if err != nil || info.Mode().Perm() != 0600 || info.Mode()&os.ModeSocket == 0 {
		t.Fatalf("socket is not owner-only: %v %v", info, err)
	}
	status, err := ControlRadio(context.Background(), configPath, "status", "")
	if err != nil || status.SiteID != "site-1" || status.State != "running" {
		t.Fatalf("read running Site: %+v %v", status, err)
	}
	for _, command := range []struct{ action, channel string }{{"start", ""}, {"start", "channel-voice"}, {"stop", "channel-voice"}, {"stop", ""}} {
		if _, err = ControlRadio(context.Background(), configPath, command.action, command.channel); err != nil {
			t.Fatalf("%s %s: %v", command.action, command.channel, err)
		}
	}
	runtime.mu.Lock()
	defer runtime.mu.Unlock()
	if runtime.starts != 1 || runtime.stops != 1 || !reflect.DeepEqual(runtime.startedChannels, []string{"channel-voice"}) || !reflect.DeepEqual(runtime.stoppedChannels, []string{"channel-voice"}) {
		t.Fatalf("wrong Site operations: starts %d stops %d channels %v/%v", runtime.starts, runtime.stops, runtime.startedChannels, runtime.stoppedChannels)
	}
}

func TestLocalRadioSocketReturnsOperationErrors(t *testing.T) {
	s, runtime, configPath := localControlFixture(t)
	runtime.startError = errors.New("modem is already in use")
	closeControl, err := s.startLocalControl(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	defer closeControl()
	if _, err = ControlRadio(context.Background(), configPath, "start", ""); err == nil || !strings.Contains(err.Error(), "modem is already in use") {
		t.Fatalf("start error lost: %v", err)
	}
}

func TestLocalRadioSocketRecoversStaleSocketOnlyWithServiceLock(t *testing.T) {
	s, _, configPath := localControlFixture(t)
	path := filepath.Join(s.Config().StateDir, "control.sock")
	stale, err := net.ListenUnix("unix", &net.UnixAddr{Name: path, Net: "unix"})
	if err != nil {
		t.Fatal(err)
	}
	stale.SetUnlinkOnClose(false)
	_ = stale.Close()
	lock := s.lock
	s.lock = nil
	if _, err = s.startLocalControl(context.Background()); err == nil {
		t.Fatal("stale socket removed without service lock")
	}
	if _, err = os.Lstat(path); err != nil {
		t.Fatal("unlocked attempt removed the socket")
	}
	s.lock = lock
	closeControl, err := s.startLocalControl(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if _, err = ControlRadio(context.Background(), configPath, "status", ""); err != nil {
		t.Fatal(err)
	}
	closeControl()
	if _, err = os.Lstat(path); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("closed service left socket: %v", err)
	}
}

func TestLocalRadioSocketPreservesUnexpectedFiles(t *testing.T) {
	s, _, _ := localControlFixture(t)
	path := filepath.Join(s.Config().StateDir, "control.sock")
	if err := os.WriteFile(path, []byte("preserve me"), 0600); err != nil {
		t.Fatal(err)
	}
	if _, err := s.startLocalControl(context.Background()); err == nil {
		t.Fatal("overwrote a non-socket file")
	}
	raw, err := os.ReadFile(path)
	if err != nil || string(raw) != "preserve me" {
		t.Fatal("unexpected file changed")
	}
	if err = os.Remove(path); err != nil {
		t.Fatal(err)
	}
	if err = os.Symlink(filepath.Join(s.Config().StateDir, "service.json"), path); err != nil {
		t.Fatal(err)
	}
	if _, err = s.startLocalControl(context.Background()); err == nil {
		t.Fatal("followed a socket symlink")
	}
	if info, err := os.Lstat(path); err != nil || info.Mode()&os.ModeSymlink == 0 {
		t.Fatal("socket symlink changed")
	}
}

func TestLocalRadioHandlerRejectsInvalidCommands(t *testing.T) {
	s, runtime, _ := localControlFixture(t)
	for _, tc := range []struct {
		method, path, body string
		status             int
	}{{"GET", "/start", "", 405}, {"POST", "/status", "{}", 405}, {"POST", "/anything", "{}", 404}, {"POST", "/start", `{"frequency":444.45}`, 400}, {"POST", "/start", `{} {}`, 400}, {"POST", "/start", strings.Repeat(" ", 2048), 400}} {
		w := httptest.NewRecorder()
		s.localRadioHandler().ServeHTTP(w, httptest.NewRequest(tc.method, tc.path, strings.NewReader(tc.body)))
		if w.Code != tc.status {
			t.Fatalf("%s %s: got %d want %d", tc.method, tc.path, w.Code, tc.status)
		}
	}
	runtime.mu.Lock()
	defer runtime.mu.Unlock()
	if runtime.starts != 0 || runtime.stops != 0 {
		t.Fatal("invalid command changed radio operation")
	}
}

func TestLocalRadioClientRejectsNonSiteAndInvalidFlags(t *testing.T) {
	s, _, path := localControlFixture(t)
	for _, pair := range [][2]string{{"erase", ""}, {"status", "voice"}} {
		if _, err := ControlRadio(context.Background(), path, pair[0], pair[1]); err == nil {
			t.Fatal("invalid radio command accepted")
		}
	}
	c := s.Config()
	c.Role = RoleSystem
	c.UpstreamURL = ""
	if err := WritePrivateJSON(path, c); err != nil {
		t.Fatal(err)
	}
	if _, err := ControlRadio(context.Background(), path, "status", ""); err == nil || !strings.Contains(err.Error(), "Site service") {
		t.Fatalf("non-Site command: %v", err)
	}
	s.mu.Lock()
	s.config.Role = RoleSystem
	s.mu.Unlock()
	w := httptest.NewRecorder()
	s.localRadioHandler().ServeHTTP(w, httptest.NewRequest(http.MethodGet, "/status", nil))
	if w.Code != http.StatusNotFound {
		t.Fatal("non-Site exposed radio status")
	}
}
