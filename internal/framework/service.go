package framework

import (
	"context"
	"crypto/sha256"
	"crypto/tls"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"net"
	"net/http"
	"open25.local/open25-fne/internal/activity"
	"open25.local/open25-fne/internal/dispatch"
	"open25.local/open25-fne/internal/messaging"
	"open25.local/open25-fne/internal/radio"
	"open25.local/open25-fne/internal/siteaudio"
	"os"
	"path/filepath"
	"sync"
	"sync/atomic"
	"syscall"
	"time"
)

type Session struct {
	Username, CSRF string
	Expires        time.Time
	AccountDigest  [32]byte
	UserID         string
	PositionID     string
	Key            string
}
type loginAttempt struct {
	Count int
	Since time.Time
}

type Service struct {
	Store                   *Store
	messages                *messaging.Store
	messageMu               sync.Mutex
	smtpMu                  sync.Mutex
	messageSubscribers      map[string]map[chan struct{}]struct{}
	messageStop             chan struct{}
	messageClose            sync.Once
	configPath              string
	mu                      sync.Mutex
	settingsMu              sync.Mutex
	config                  FileConfig
	sessions                map[string]Session
	attempts                map[string]loginAttempt
	client                  *http.Client
	assets                  fs.FS
	lock                    *os.File
	shutdownDeadline        atomic.Int64
	reportMu                sync.Mutex
	syncMu                  sync.Mutex
	runWG                   sync.WaitGroup
	activity                *activity.Store
	radio                   radioRuntime
	codec                   *dispatch.Codec
	rfRouter                atomic.Pointer[siteaudio.Router]
	hub                     *dispatch.Hub
	broker                  *callBroker
	voice                   atomic.Pointer[voiceRuntime]
	peers                   *mediaPeers
	mediaClient             *mediaClient
	operationCtx            context.Context
	operationCancel         context.CancelFunc
	operationWG             sync.WaitGroup
	operationMu             sync.Mutex
	radioReconcile          sync.Mutex
	operationWake           chan struct{}
	codecSignature          string
	radioCandidateSignature string
	radioAliasError         string
	radioAppliedProfile     radio.Profile
	radioOperation          *radioOperation
	radioStarts             map[string]*radioStartAdmission
	radioChannelStops       map[string]*radioOperation
	radioRunActive          bool
	radioRunStartedAt       time.Time
	radioPolicy             atomic.Pointer[radio.System]
	directory               atomicDirectory
	featureMu               sync.Mutex
	operatorPolicy          atomic.Pointer[operatorPolicySnapshot]
	patchMu                 sync.RWMutex
	crypto                  *keyRuntime
	audioLeaseMu            sync.Mutex
	audioLeases             map[int]audioIdentityLease
	rfAdmissions            *rfAdmissionManager
	rfAuthority             *rfSourceAuthority
	rfAuthorityError        error
	commandMu               sync.Mutex
	inhibitionMu            sync.Mutex
	inhibition              atomic.Pointer[radioInhibitionSnapshot]
	actualInhibition        atomic.Pointer[siteInhibitionSnapshot]
	automationMu            sync.Mutex
	automatedJobs           *automationRuntime
	speechSlots             chan struct{}
}

func OpenService(configPath string, assets fs.FS) (*Service, error) {
	c, err := ReadFileConfig(configPath)
	if err != nil {
		return nil, err
	}
	if err = os.MkdirAll(c.StateDir, 0700); err != nil {
		return nil, err
	}
	lock, err := os.OpenFile(filepath.Join(c.StateDir, "service.lock"), os.O_CREATE|os.O_RDWR, 0600)
	if err != nil {
		return nil, err
	}
	if err = syscall.Flock(int(lock.Fd()), syscall.LOCK_EX|syscall.LOCK_NB); err != nil {
		lock.Close()
		return nil, errors.New("another service already uses this data directory")
	}
	closeLock := func() { _ = syscall.Flock(int(lock.Fd()), syscall.LOCK_UN); _ = lock.Close() }
	if _, err = readCredentials(c.CredentialFile); err != nil {
		closeLock()
		return nil, err
	}
	store, err := OpenStore(filepath.Join(c.StateDir, "configuration.db"), c.Role)
	if err != nil {
		closeLock()
		return nil, err
	}
	if _, err = store.Config(); err != nil {
		store.Close()
		closeLock()
		return nil, fmt.Errorf("initialize this service before starting: %w", err)
	}
	var identity struct {
		ID string `json:"id"`
	}
	if c.Role != RoleApplication {
		raw, _ := store.Config()
		_ = json.Unmarshal(raw, &identity)
		if identity.ID != c.InstanceID {
			store.Close()
			closeLock()
			return nil, errors.New("service identity does not match its saved configuration")
		}
	}
	if _, _, err = store.Account(); err != nil {
		store.Close()
		closeLock()
		return nil, fmt.Errorf("administrator account is missing: %w", err)
	}
	client := newHTTPClient()
	s := &Service{Store: store, config: c, configPath: configPath, client: client, assets: assets, lock: lock, sessions: map[string]Session{}, attempts: map[string]loginAttempt{}}
	if c.Role == RoleApplication {
		s.messages, err = messaging.Open(filepath.Join(c.StateDir, "messages.db"))
		if err != nil {
			store.Close()
			closeLock()
			return nil, fmt.Errorf("open application messages: %w", err)
		}
		s.messageStop = make(chan struct{})
	}
	if err = s.openOperations(); err != nil {
		s.closeOperations()
		if s.messages != nil {
			_ = s.messages.Close()
		}
		store.Close()
		closeLock()
		return nil, err
	}
	_ = store.AddEvent("service", "Service started. Radio operation requires an explicit Site start.")
	return s, nil
}

func (s *Service) Config() FileConfig { s.mu.Lock(); defer s.mu.Unlock(); return s.config }
func (s *Service) shutdownContext() (context.Context, context.CancelFunc) {
	s.shutdownDeadline.CompareAndSwap(0, time.Now().Add(5*time.Second).UnixNano())
	return context.WithDeadline(context.Background(), time.Unix(0, s.shutdownDeadline.Load()))
}
func releaseServiceLock(lock *os.File) {
	if lock != nil {
		_ = syscall.Flock(int(lock.Fd()), syscall.LOCK_UN)
		_ = lock.Close()
	}
}
func (s *Service) Close() error {
	shutdown, cancel := s.shutdownContext()
	defer cancel()
	s.messageClose.Do(func() {
		if s.messageStop != nil {
			close(s.messageStop)
		}
	})
	s.closeOperations()
	if s.client != nil {
		s.client.CloseIdleConnections()
	}
	lock := s.lock
	s.lock = nil
	var authorityErr error
	if s.rfAuthority != nil {
		select {
		case <-s.rfAuthority.done:
		case <-shutdown.Done():
			authorityErr = shutdown.Err()
		}
	}
	if authorityErr == nil {
		releaseServiceLock(lock)
	} else {
		// Keep the singleton lifetime until an uninterruptible filesystem call exits.
		// New admission is already cancelled; a replacement cannot race this writer.
		go func() { <-s.rfAuthority.done; releaseServiceLock(lock) }()
	}
	var messageErr error
	if s.messages != nil {
		messageErr = s.messages.Close()
	}
	return errors.Join(authorityErr, messageErr, s.Store.Close())
}
func (s *Service) cookieName() string {
	c := s.Config()
	sum := sha256.Sum256([]byte(c.StateDir))
	return "tsf_" + string(c.Role) + "_" + hex.EncodeToString(sum[:4])
}

func (s *Service) SaveSettings(v Settings) error {
	s.settingsMu.Lock()
	defer s.settingsMu.Unlock()
	c := s.Config()
	c.PublicURL = v.PublicURL
	c.InternalURL = v.InternalURL
	c.UpstreamURL = v.UpstreamURL
	c.PollSeconds = v.PollSeconds
	if err := WritePrivateJSON(s.configPath, c); err != nil {
		return err
	}
	s.mu.Lock()
	s.config = c
	s.mu.Unlock()
	return s.Store.AddEvent("configuration", "Server connection settings saved.")
}

func (s *Service) Run(ctx context.Context) error {
	c := s.Config()
	web := &http.Server{Addr: c.Listen, Handler: s.Handler(), ReadHeaderTimeout: 5 * time.Second, ReadTimeout: 15 * time.Second, WriteTimeout: 75 * time.Second, IdleTimeout: 60 * time.Second, MaxHeaderBytes: 16384}
	internal := &http.Server{Addr: c.InternalListen, Handler: s.InternalHandler(), ReadHeaderTimeout: 5 * time.Second, ReadTimeout: 15 * time.Second, WriteTimeout: 75 * time.Second, IdleTimeout: 60 * time.Second, MaxHeaderBytes: 16384}
	if c.TLSCertificate != "" {
		web.TLSConfig = &tls.Config{MinVersion: tls.VersionTLS12}
	}
	internalListener, err := net.Listen("tcp", c.InternalListen)
	if err != nil {
		return err
	}
	loopCtx, cancel := context.WithCancel(ctx)
	closeControl, err := s.startLocalControl(loopCtx)
	if err != nil {
		cancel()
		_ = internalListener.Close()
		return err
	}
	defer func() {
		cancel()
		closeControl()
		_ = internalListener.Close()
		_ = web.Close()
		_ = internal.Close()
		s.runWG.Wait()
	}()
	result := make(chan error, 1)
	s.runWG.Add(4)
	go func() { defer s.runWG.Done(); s.runSMTP(loopCtx) }()
	go func() { defer s.runWG.Done(); s.serveWeb(loopCtx, web, c) }()
	go func() { defer s.runWG.Done(); result <- internal.Serve(internalListener) }()
	go func() { defer s.runWG.Done(); s.RunSync(loopCtx) }()
	fmt.Printf("%s service: %s (internal %s)\n", c.Role, c.PublicURL, c.InternalURL)
	select {
	case <-ctx.Done():
	case err = <-result:
	}
	cancel()
	shutdown, stop := s.shutdownContext()
	defer stop()
	// Both listeners share one shutdown budget, rather than sequential waits.
	closed := make(chan error, 2)
	go func() { closed <- web.Shutdown(shutdown) }()
	go func() { closed <- internal.Shutdown(shutdown) }()
	for range 2 {
		if shutdownErr := <-closed; shutdownErr != nil && (err == nil || errors.Is(err, http.ErrServerClosed)) {
			err = shutdownErr
		}
	}
	if errors.Is(err, http.ErrServerClosed) {
		return nil
	}
	return err
}

func (s *Service) serveWeb(ctx context.Context, server *http.Server, c FileConfig) {
	lastError := ""
	for ctx.Err() == nil {
		var err error
		if c.TLSCertificate != "" {
			_, err = tls.LoadX509KeyPair(c.TLSCertificate, c.TLSKey)
		}
		if err == nil {
			var listener net.Listener
			listener, err = net.Listen("tcp", c.Listen)
			if err == nil {
				lastError = ""
				if c.TLSCertificate != "" {
					err = server.ServeTLS(listener, c.TLSCertificate, c.TLSKey)
				} else {
					err = server.Serve(listener)
				}
				_ = listener.Close()
			}
		}
		if ctx.Err() != nil || errors.Is(err, http.ErrServerClosed) {
			return
		}
		if err.Error() != lastError {
			fmt.Fprintf(os.Stderr, "%s web administration unavailable: %v; retrying\n", c.Role, err)
			lastError = err.Error()
		}
		select {
		case <-ctx.Done():
			return
		case <-time.After(time.Second):
		}
	}
}
