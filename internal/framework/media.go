package framework

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"open25.local/open25-fne/internal/dispatch"
	"slices"
	"strings"
	"sync"
	"time"
)

// mediaClient tracks the selected controller's authenticated SIP endpoint.
// Discovery is management traffic; an admitted call has its own SIP/RTP lifetime.
type mediaClient struct {
	s                 *Service
	mu                sync.Mutex
	activeURL         string
	endpoint          voiceEndpoint
	running, closed   bool
	generation        uint64
	connectedAt       time.Time
	retryPrimaryAfter time.Time
	cancel            context.CancelFunc
	wg                sync.WaitGroup
}

func newMediaClient(s *Service) *mediaClient { return &mediaClient{s: s} }
func (m *mediaClient) ConnectionStatus() Connection {
	primary := m.s.upstreamURL()
	m.mu.Lock()
	defer m.mu.Unlock()
	c := Connection{Name: "Controller", URL: m.activeURL, State: "Unavailable", LastSuccess: m.connectedAt}
	if m.activeURL != "" {
		c.State = "Connected"
		if m.activeURL != primary {
			c.State = "Site Trunking"
		}
	}
	return c
}
func (m *mediaClient) targets() []string {
	out := []string{}
	if p := m.s.upstreamURL(); p != "" {
		out = append(out, p)
	}
	if m.s.Config().Role == RoleApplication {
		var d ApplicationConfig
		if m.s.owned(&d) == nil && d.LocalSiteURL != "" && !slices.Contains(out, d.LocalSiteURL) {
			out = append(out, d.LocalSiteURL)
		}
	}
	return out
}
func (m *mediaClient) Ensure() {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.closed || m.running || m.s.voice.Load() == nil {
		return
	}
	m.running = true
	ctx, cancel := context.WithCancel(m.s.operationCtx)
	m.cancel = cancel
	m.wg.Add(1)
	go m.run(ctx)
}
func (m *mediaClient) selectEndpoint(address string, e voiceEndpoint) {
	m.mu.Lock()
	changed := m.activeURL != address || m.endpoint.Generation != e.Generation || m.endpoint.URI != e.URI
	old := m.endpoint
	if changed {
		m.generation++
		m.activeURL = address
		m.endpoint = e
		m.connectedAt = time.Now().UTC()
	}
	m.mu.Unlock()
	if changed && old.PeerID != "" {
		m.s.voice.Load().cancelPeer(old.PeerID)
		if m.s.hub != nil {
			m.s.hub.StopTransmissions("The controller connection changed. Release PTT and try again.")
		}
	}
}
func (m *mediaClient) run(ctx context.Context) {
	defer m.wg.Done()
	defer func() { m.mu.Lock(); m.running = false; m.mu.Unlock() }()
	ticker := time.NewTicker(time.Second)
	defer ticker.Stop()
	for {
		targets := m.targets()
		address, _ := m.Link()
		// A primary return is taken only between calls, never by replaying a call.
		if address != "" && slices.Contains(targets, address) && len(targets) > 1 && address != targets[0] && (m.s.hub == nil || !m.s.hub.Busy()) && !m.s.voice.Load().busy() && !time.Now().Before(m.retryPrimaryAfter) {
			m.retryPrimaryAfter = time.Now().Add(10 * time.Second)
			probe, cancel := context.WithTimeout(ctx, 500*time.Millisecond)
			if e, err := m.s.voice.Load().discover(probe, targets[0], nil); err == nil {
				m.selectEndpoint(targets[0], e)
				address = targets[0]
			}
			cancel()
		}
		if address != "" && slices.Contains(targets, address) {
			probe, cancel := context.WithTimeout(ctx, 750*time.Millisecond)
			e, err := m.s.voice.Load().discover(probe, address, nil)
			cancel()
			if err == nil {
				m.selectEndpoint(address, e)
			} else if !m.s.voice.Load().peerBusy(m.endpointPeer()) {
				m.selectEndpoint("", voiceEndpoint{})
			}
		} else {
			m.selectEndpoint("", voiceEndpoint{})
		}
		if current, _ := m.Link(); current == "" {
			for _, target := range targets {
				probe, cancel := context.WithTimeout(ctx, 750*time.Millisecond)
				e, err := m.s.voice.Load().discover(probe, target, nil)
				cancel()
				if err == nil {
					m.selectEndpoint(target, e)
					break
				}
			}
		}
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
		}
	}
}
func (m *mediaClient) endpointPeer() string {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.endpoint.PeerID
}
func (m *mediaClient) URL() string { a, _ := m.Link(); return a }
func (m *mediaClient) Link() (string, uint64) {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.activeURL, m.generation
}
func (m *mediaClient) endpointAt(generation uint64) (voiceEndpoint, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.closed || m.activeURL == "" || m.generation != generation {
		return voiceEndpoint{}, dispatch.ErrUnavailable
	}
	return m.endpoint, nil
}
func (m *mediaClient) Close() {
	m.mu.Lock()
	m.closed = true
	if m.cancel != nil {
		m.cancel()
	}
	e := m.endpoint
	m.activeURL = ""
	m.mu.Unlock()
	if m.s.voice.Load() != nil && e.PeerID != "" {
		m.s.voice.Load().cancelPeer(e.PeerID)
	}
	m.wg.Wait()
}

func (s *Service) requestAt(ctx context.Context, address, method, path string, value, target any, recipient *PeerCredential) error {
	if address == "" {
		return dispatch.ErrUnavailable
	}
	if err := validateInternalURL(address, false); err != nil {
		return err
	}
	cfg := s.Config()
	creds, err := readCredentials(cfg.CredentialFile)
	if err != nil {
		return err
	}
	if creds.OutboundID == "" || len(creds.OutboundToken) < 32 {
		return errors.New("outbound service credentials are not configured")
	}
	if recipient != nil {
		enrolled := false
		for _, candidate := range creds.Peers {
			if candidate == *recipient {
				enrolled = true
				break
			}
		}
		if cfg.Role != RoleZone || recipient.Role != RoleSite || !enrolled {
			return errors.New("Site is not enrolled")
		}
	}
	body := []byte{}
	if value != nil {
		var err error
		body, err = json.Marshal(value)
		if err != nil {
			return err
		}
	}
	req, err := http.NewRequestWithContext(ctx, method, strings.TrimRight(address, "/")+path, strings.NewReader(string(body)))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("X-Service-ID", creds.OutboundID)
	req.Header.Set("Authorization", "Bearer "+creds.OutboundToken)
	response, err := s.client.Do(req)
	if err != nil {
		return err
	}
	defer response.Body.Close()
	if response.StatusCode != 200 && response.StatusCode != 202 {
		var failure struct {
			Error string `json:"error"`
		}
		_ = json.NewDecoder(response.Body).Decode(&failure)
		if failure.Error == "" {
			failure.Error = response.Status
		}
		return errors.New(failure.Error)
	}
	if target != nil {
		return json.NewDecoder(response.Body).Decode(target)
	}
	// Consume the small acknowledgment so recurring lease renewals reuse their
	// authenticated HTTP connection. Unexpected larger replies are bounded and
	// simply close without keep-alive; the request's own deadline still applies.
	_, err = io.Copy(io.Discard, io.LimitReader(response.Body, 4096))
	return err
}
func (s *Service) peerIdentityForSite(id string) (string, PeerCredential, error) {
	creds, err := readCredentials(s.Config().CredentialFile)
	if err != nil {
		return "", PeerCredential{}, err
	}
	for key, p := range creds.Peers {
		if p.Role == RoleSite && p.InstanceID == id {
			return key, p, nil
		}
	}
	return "", PeerCredential{}, errors.New("Site is not enrolled")
}
func userAllows(u User, tg int) bool { return u.Enabled && slices.Contains(u.Talkgroups, tg) }
