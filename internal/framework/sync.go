package framework

import (
	"bytes"
	"context"
	"crypto/subtle"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"open25.local/open25-fne/internal/activity"
	"open25.local/open25-fne/internal/radio"
	"slices"
	"strings"
	"time"
)

type administratorRecord struct {
	Username string `json:"username"`
	Hash     []byte `json:"password_hash"`
}
type catalog struct {
	System          SystemConfig           `json:"system"`
	Zone            *ZoneConfig            `json:"zone,omitempty"`
	Administrator   administratorRecord    `json:"administrator"`
	Users           []userRecord           `json:"users"`
	Automations     []automationRecord     `json:"automations"`
	KeyEnvelopes    map[string]keyEnvelope `json:"key_envelopes"`
	Operator        OperatorSnapshot       `json:"operator"`
	Inhibition      *radioInhibitionPolicy `json:"inhibition,omitempty"`
	Topology        topology               `json:"topology"`
	HardwareReports []SiteHardwareReport   `json:"hardware_reports,omitempty"`
}
type Connection struct {
	Name        string    `json:"name"`
	URL         string    `json:"url"`
	State       string    `json:"state"`
	LastSuccess time.Time `json:"last_success"`
	Error       string    `json:"error"`
}
type SiteReport struct {
	ID         string              `json:"id"`
	Name       string              `json:"name"`
	SiteID     int                 `json:"site_id"`
	RFSSID     int                 `json:"rfss_id"`
	Endpoint   string              `json:"endpoint"`
	AdminURL   string              `json:"admin_url"`
	ObservedAt time.Time           `json:"observed_at"`
	ReceivedAt time.Time           `json:"received_at"`
	Config     SiteConfig          `json:"config"`
	Error      string              `json:"error"`
	RFEnabled  bool                `json:"rf_enabled"`
	Radio      *radio.Status       `json:"radio,omitempty"`
	Operator   OperatorSnapshot    `json:"operator"`
	Hardware   *SiteHardwareReport `json:"hardware,omitempty"`
}
type zoneReport struct {
	ID         string       `json:"id"`
	Endpoint   string       `json:"endpoint"`
	AdminURL   string       `json:"admin_url"`
	ObservedAt time.Time    `json:"observed_at"`
	ReceivedAt time.Time    `json:"received_at"`
	Config     ZoneConfig   `json:"config"`
	Sites      []SiteReport `json:"sites"`
}
type topology struct {
	SystemURL      string            `json:"system_url"`
	ZoneURL        string            `json:"zone_url"`
	ApplicationURL string            `json:"application_url"`
	SiteURLs       map[string]string `json:"site_urls"`
}

func (s *Service) readCache(name string, target any) bool {
	if s.Store == nil {
		return false
	}
	raw, _, err := s.Store.ReadCache(name)
	return err == nil && json.Unmarshal(raw, target) == nil
}
func (s *Service) writeCache(name string, value any) error {
	raw, err := json.Marshal(value)
	if err != nil {
		return err
	}
	return s.Store.WriteCache(name, raw)
}
func (s *Service) owned(target any) error {
	raw, err := s.Store.Config()
	if err != nil {
		return err
	}
	return json.Unmarshal(raw, target)
}
func (s *Service) upstreamURL() string {
	c := s.Config()
	if c.Role == RoleApplication {
		var d ApplicationConfig
		if s.owned(&d) == nil {
			return d.ConnectionURL
		}
		return ""
	}
	return c.UpstreamURL
}

func (s *Service) documents() (SystemConfig, *ZoneConfig, error) {
	c := s.Config()
	var system SystemConfig
	if c.Role == RoleSystem {
		if err := s.owned(&system); err != nil {
			return system, nil, err
		}
	} else if !s.readCache("system", &system) {
		return system, nil, errors.New("system settings have not been received")
	}
	var zone ZoneConfig
	if c.Role == RoleZone {
		if err := s.owned(&zone); err != nil {
			return system, nil, err
		}
	} else if !s.readCache("zone", &zone) {
		return system, nil, nil
	}
	return system, &zone, nil
}

func (s *Service) internalPeer(r *http.Request) (PeerCredential, bool) {
	creds, err := readCredentials(s.Config().CredentialFile)
	if err != nil {
		return PeerCredential{}, false
	}
	peer, ok := creds.Peers[r.Header.Get("X-Service-ID")]
	token := strings.TrimPrefix(r.Header.Get("Authorization"), "Bearer ")
	return peer, ok && strings.HasPrefix(r.Header.Get("Authorization"), "Bearer ") && subtle.ConstantTimeCompare([]byte(token), []byte(peer.Token)) == 1
}

func (s *Service) internal(w http.ResponseWriter, r *http.Request) {
	peer, ok := s.internalPeer(r)
	if !ok {
		fail(w, 401, errors.New("service credentials are not accepted"))
		return
	}
	role := s.Config().Role
	if r.URL.Path == "/internal/smtp" {
		s.smtpInternal(w, r, peer)
		return
	}
	if r.URL.Path == "/internal/operator-history" {
		s.operatorHistoryInternal(w, r, peer)
		return
	}
	if strings.HasPrefix(r.URL.Path, "/internal/audio-identity/") {
		s.audioIdentityInternal(w, r, peer)
		return
	}
	if strings.HasPrefix(r.URL.Path, "/internal/operator/") {
		s.operatorInternal(w, r, peer)
		return
	}
	if r.URL.Path == "/internal/radio/command" {
		s.radioCommandInternal(w, r, peer)
		return
	}
	if r.URL.Path == "/internal/radio/commands" {
		s.radioCommandsInternal(w, r, peer)
		return
	}
	if r.URL.Path == "/internal/rf-authority" {
		s.rfAuthorityInternal(w, r, peer)
		return
	}
	if r.URL.Path == "/internal/voice" {
		s.voiceDiscovery(w, r, peer)
		return
	}
	if r.URL.Path == "/internal/profile" {
		s.profileInternal(w, r, peer)
		return
	}
	if r.URL.Path == "/internal/desktop-workspace" {
		s.desktopWorkspaceInternal(w, r, peer)
		return
	}
	if r.URL.Path == "/internal/events" {
		s.activityInternal(w, r, peer)
		return
	}
	switch r.URL.Path {
	case "/internal/catalog":
		if r.Method != "GET" {
			w.WriteHeader(405)
			return
		}
		allowed := (role == RoleSystem && peer.Role == RoleZone) || (role == RoleZone && (peer.Role == RoleSite || peer.Role == RoleApplication)) || (role == RoleSite && peer.Role == RoleApplication)
		if !allowed {
			fail(w, 403, errors.New("this service cannot read that catalogue"))
			return
		}
		var system SystemConfig
		var zone *ZoneConfig
		var err error
		view := s.view()
		if role == RoleSystem {
			system, zone, err = s.documents()
		} else {
			// A downstream service must see the same directory that this
			// controller currently uses to admit calls, rather than an earlier
			// database write from an unfinished catalogue acceptance.
			system = view.System
			if view.Zone != nil {
				copy := *view.Zone
				zone = &copy
			}
			if system.ID == "" {
				err = errors.New("accepted system settings are not available")
			}
		}
		if err != nil {
			fail(w, 503, err)
			return
		}
		if peer.Role == RoleSite {
			if zone == nil {
				fail(w, 503, errors.New("zone settings are unavailable"))
				return
			}
			found := false
			for _, site := range zone.Sites {
				if site.ID == peer.InstanceID {
					found = true
					zone.Sites = []SiteIdentity{site}
					break
				}
			}
			if !found {
				fail(w, 403, errors.New("site is not enrolled in this zone"))
				return
			}
			groups := []Talkgroup{}
			for _, g := range system.Talkgroups {
				if g.SiteAccess == "all" {
					groups = append(groups, g)
					continue
				}
				for _, id := range g.SiteIDs {
					if id == peer.InstanceID {
						g.SiteIDs = []string{id}
						groups = append(groups, g)
						break
					}
				}
			}
			system.Talkgroups = groups
			system.ConsolePositions = slices.Clone(system.ConsolePositions)
			for i := range system.ConsolePositions {
				system.ConsolePositions[i].Talkgroups = slices.DeleteFunc(slices.Clone(system.ConsolePositions[i].Talkgroups), func(id int) bool { return !slices.ContainsFunc(groups, func(g Talkgroup) bool { return g.TGID == id }) })
			}
		}
		username, hash, err := s.Store.Account()
		if err != nil {
			fail(w, 503, errors.New("administrator settings are unavailable"))
			return
		}
		users := []userRecord{}
		if role == RoleSystem {
			users, err = s.Store.userRecords()
		} else {
			for _, record := range view.Users {
				users = append(users, record)
			}
		}
		if err != nil {
			fail(w, 503, errors.New("user directory is unavailable"))
			return
		}
		users = eligibleUsers(users, system)
		automations := []automationRecord{}
		var automationErr error
		if role == RoleSystem {
			automations, automationErr = s.Store.automationRecords()
		} else {
			for _, record := range view.Automations {
				automations = append(automations, record)
			}
		}
		if automationErr != nil {
			fail(w, 503, errors.New("automation directory is unavailable"))
			return
		}
		automations = eligibleAutomations(automations, system)
		var keys map[string]keyEnvelope
		if peer.Role != RoleSite {
			keys, err = s.keyEnvelopes()
			if err != nil {
				fail(w, 503, err)
				return
			}
		}
		if peer.Role == RoleApplication {
			item, found := keys[peer.InstanceID]
			keys = map[string]keyEnvelope{}
			if found {
				keys[peer.InstanceID] = item
			}
		}
		var inhibition *radioInhibitionPolicy
		if role == RoleZone && peer.Role == RoleSite {
			desired, policyErr := s.DesiredInhibitedRadios()
			if policyErr != nil {
				fail(w, 503, policyErr)
				return
			}
			inhibition = &radioInhibitionPolicy{Desired: desired}
		}
		var hardwareReports []SiteHardwareReport
		if role == RoleZone && peer.Role == RoleSite {
			hardwareReports = s.hardwareReportsForSite(peer.InstanceID)
		}
		respond(w, 200, catalog{System: system, Zone: zone, Administrator: administratorRecord{username, hash}, Users: users, Automations: automations, KeyEnvelopes: keys, Operator: s.operatorSnapshot(), Inhibition: inhibition, Topology: s.adminTopology(zone), HardwareReports: hardwareReports})
	case "/internal/report":
		if r.Method != "POST" {
			w.WriteHeader(405)
			return
		}
		if role == RoleSystem && peer.Role == RoleZone {
			var report zoneReport
			if err := readJSON(w, r, &report); err != nil {
				fail(w, 400, err)
				return
			}
			if err := s.acceptZoneReport(peer, report); err != nil {
				fail(w, 400, err)
				return
			}
		} else if role == RoleZone && peer.Role == RoleSite {
			var report SiteReport
			if err := readJSON(w, r, &report); err != nil {
				fail(w, 400, err)
				return
			}
			if err := s.acceptSiteReport(peer, report); err != nil {
				fail(w, 400, err)
				return
			}
		} else {
			fail(w, 403, errors.New("this service cannot submit that report"))
			return
		}
		respond(w, 200, map[string]bool{"accepted": true})
	default:
		http.NotFound(w, r)
	}
}

func (s *Service) acceptSiteReport(peer PeerCredential, r SiteReport) error {
	if r.ID != peer.InstanceID || r.Config.ID != r.ID || (r.Radio != nil && r.Radio.SiteID != r.ID) || (r.RFEnabled && (r.Radio == nil || r.Radio.State != "running")) {
		return errors.New("report does not match this Site identity or observed radio state")
	}
	if r.ObservedAt.IsZero() || r.ObservedAt.After(time.Now().Add(time.Minute)) {
		return errors.New("Site report time is missing or ahead of this server")
	}
	_, zone, err := s.documents()
	if err != nil {
		return err
	}
	if zone == nil {
		return errors.New("zone settings unavailable")
	}
	found := false
	for _, site := range zone.Sites {
		if site.ID == r.ID {
			found = true
			r.Name = site.Name
			r.SiteID = site.SiteID
			break
		}
	}
	if !found {
		return errors.New("Site is no longer registered")
	}
	r.RFSSID = zone.RFSSID
	if err = s.mergeSiteOperatorSnapshot(peer.InstanceID, r.Operator); err != nil {
		return err
	}
	r.ReceivedAt = time.Now().UTC()
	if r.Hardware != nil {
		r.Hardware.SiteID = r.ID
		r.Hardware.ObservedAt = r.ObservedAt
		r.Hardware.ReceivedAt = r.ReceivedAt
	}
	s.reportMu.Lock()
	reports := []SiteReport{}
	s.readCache("site_reports", &reports)
	out := []SiteReport{}
	for _, v := range reports {
		if v.ID != r.ID {
			out = append(out, v)
		}
	}
	out = append(out, r)
	err = s.writeCache("site_reports", out)
	s.reportMu.Unlock()
	if err != nil {
		return err
	}
	// Routing consumes the admitted directory, not the durable report cache.
	// Publish this accepted observation without waiting for Zone upstream sync.
	s.refreshDirectory()
	return nil
}

func (s *Service) acceptZoneReport(peer PeerCredential, r zoneReport) error {
	if r.ID != peer.InstanceID || r.Config.ID != r.ID {
		return errors.New("report does not match the enrolled Zone")
	}
	if r.ObservedAt.IsZero() || r.ObservedAt.After(time.Now().Add(time.Minute)) {
		return errors.New("Zone report time is missing or ahead of this server")
	}
	for _, site := range r.Sites {
		if site.ReceivedAt.IsZero() || site.ObservedAt.IsZero() || site.ReceivedAt.After(time.Now().Add(time.Minute)) || site.ObservedAt.After(time.Now().Add(time.Minute)) {
			return errors.New("Site report time is missing or ahead of this server")
		}
		if site.RFEnabled && (site.Radio == nil || site.Radio.SiteID != site.ID || site.Radio.State != "running") {
			return errors.New("radio state is not supported by its Site report")
		}
		found := false
		for _, identity := range r.Config.Sites {
			if identity.ID == site.ID && site.Config.ID == site.ID {
				found = true
				break
			}
		}
		if !found {
			return errors.New("report contains an unregistered Site")
		}
	}
	raw, err := json.Marshal(r.Config)
	if err != nil {
		return err
	}
	r.ReceivedAt = time.Now().UTC()
	s.reportMu.Lock()
	defer s.reportMu.Unlock()
	if err := s.Store.WriteCache("zone", raw); err != nil {
		return err
	}
	if err := s.writeCache("site_reports", r.Sites); err != nil {
		return err
	}
	return s.writeCache("zone_report", r)
}

func (s *Service) upstreamRequest(ctx context.Context, method, path string, value any, target any) error {
	if err := validateInternalURL(s.upstreamURL(), false); err != nil {
		return err
	}
	c := s.Config()
	creds, err := readCredentials(c.CredentialFile)
	if err != nil {
		return errors.New("could not read local service credentials")
	}
	if creds.OutboundID == "" || len(creds.OutboundToken) < 32 {
		return errors.New("upstream service credentials are missing")
	}
	var body io.Reader
	if value != nil {
		raw, err := json.Marshal(value)
		if err != nil {
			return err
		}
		body = bytes.NewReader(raw)
	}
	req, err := http.NewRequestWithContext(ctx, method, strings.TrimRight(s.upstreamURL(), "/")+path, body)
	if err != nil {
		return err
	}
	req.Header.Set("Authorization", "Bearer "+creds.OutboundToken)
	req.Header.Set("X-Service-ID", creds.OutboundID)
	if value != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	response, err := s.client.Do(req)
	if err != nil {
		return err
	}
	defer response.Body.Close()
	if response.StatusCode != 200 {
		return fmt.Errorf("upstream returned HTTP %d", response.StatusCode)
	}
	if target != nil {
		raw, err := io.ReadAll(io.LimitReader(response.Body, maxConfigurationBytes+1))
		if err != nil {
			return err
		}
		return DecodeStrict(raw, target)
	}
	return nil
}

func (s *Service) SyncOnce(ctx context.Context) error {
	s.syncMu.Lock()
	defer s.syncMu.Unlock()
	c := s.Config()
	if c.Role == RoleSystem {
		return nil
	}
	var incoming catalog
	err := s.upstreamRequest(ctx, "GET", "/internal/catalog", nil, &incoming)
	if err == nil {
		err = s.acceptCatalog(incoming)
	}
	if err == nil {
		s.refreshDirectory()
		if c.Role == RoleSite {
			desired, policyErr := s.DesiredInhibitedRadios()
			if policyErr == nil {
				policyErr = s.applySiteInhibitionPolicy(ctx, desired)
			}
			err = policyErr
		}
		if err == nil {
			s.uploadActivity(ctx)
		}
	}
	if err == nil && c.Role == RoleSite {
		var report SiteReport
		report, err = s.ownSiteReport()
		if err == nil {
			err = s.upstreamRequest(ctx, "POST", "/internal/report", report, nil)
		}
	}
	if err == nil && c.Role == RoleZone {
		var zone ZoneConfig
		err = s.owned(&zone)
		if err == nil {
			reports := s.siteReports()
			r := zoneReport{ID: c.InstanceID, Endpoint: c.InternalURL, AdminURL: c.PublicURL, Config: zone, Sites: reports, ObservedAt: time.Now().UTC()}
			err = s.upstreamRequest(ctx, "POST", "/internal/report", r, nil)
		}
	}
	if err == nil {
		s.refreshDirectory()
	}
	var previous Connection
	s.readCache("connection", &previous)
	connection := Connection{Name: "Upstream", URL: s.upstreamURL(), LastSuccess: previous.LastSuccess}
	if err == nil {
		connection.State = "Connected"
		connection.LastSuccess = time.Now().UTC()
	} else {
		connection.State = "Cached settings"
		connection.Error = err.Error()
		if previous.LastSuccess.IsZero() {
			connection.State = "Not connected"
		}
	}
	_ = s.writeCache("connection", connection)
	if previous.State != connection.State || previous.Error != connection.Error {
		message := "Upstream connected; accepted settings updated."
		if err != nil {
			message = "Upstream unavailable; retained settings remain in use. " + connection.Error
		}
		_ = s.Store.AddEvent("connection", message)
	}
	return err
}

func (s *Service) acceptCatalog(incoming catalog) error {
	if incoming.System.ID == "" {
		return errors.New("upstream returned no system identity")
	}
	var old SystemConfig
	if s.readCache("system", &old) && old.ID != incoming.System.ID {
		return errors.New("upstream belongs to a different home system")
	}
	raw, err := json.Marshal(incoming.System)
	if err != nil {
		return err
	}
	if incoming.Zone != nil {
		var accepted ZoneConfig
		if s.readCache("zone", &accepted) && accepted.ID != incoming.Zone.ID {
			return errors.New("upstream belongs to a different logical Zone; enroll this service again before moving it")
		}
	}
	if s.Config().Role == RoleSite {
		if incoming.Zone == nil || len(incoming.Zone.Sites) != 1 || incoming.Zone.Sites[0].ID != s.Config().InstanceID {
			return errors.New("upstream did not assign this Site")
		}
	}
	var acceptedInhibition *radioInhibitionPolicy
	if s.Config().Role == RoleSite {
		if incoming.Inhibition == nil {
			return errors.New("Zone catalogue omitted its authoritative inhibit policy")
		}
		value, policyErr := normalizedRadioInhibition(*incoming.Inhibition)
		if policyErr != nil {
			return policyErr
		}
		acceptedInhibition = &value
	} else if incoming.Inhibition != nil {
		return errors.New("this upstream service does not own radio inhibit policy")
	}
	var zoneRaw json.RawMessage
	if incoming.Zone != nil {
		zoneRaw, _ = json.Marshal(incoming.Zone)
	}
	if acceptedInhibition != nil {
		s.inhibitionMu.Lock()
	}
	err = s.Store.AcceptCatalog(raw, zoneRaw, incoming.Administrator.Username, incoming.Administrator.Hash, incoming.Users, incoming.Automations, acceptedInhibition)
	if acceptedInhibition != nil {
		if err == nil {
			s.inhibition.Store(inhibitionSnapshot(*acceptedInhibition, nil))
		}
		s.inhibitionMu.Unlock()
	}
	if err != nil {
		return err
	}
	if incoming.KeyEnvelopes != nil {
		if err = s.writeCache("key_envelopes", incoming.KeyEnvelopes); err != nil {
			return err
		}
	}
	if err := s.writeCache("topology", incoming.Topology); err != nil {
		return err
	}
	if s.Config().Role == RoleSite {
		if err := s.writeCache("hardware_reports", incoming.HardwareReports); err != nil {
			return err
		}
	}
	if err := s.acceptOperatorSnapshot(incoming.Operator); err != nil {
		return err
	}
	return nil
}

func (s *Service) RunSync(ctx context.Context) {
	if s.Config().Role == RoleSystem {
		<-ctx.Done()
		return
	}
	interval := time.Duration(s.Config().PollSeconds) * time.Second
	ticker := time.NewTicker(interval)
	defer ticker.Stop()
	for {
		if ctx.Err() != nil {
			return
		}
		_ = s.SyncOnce(ctx)
		if next := time.Duration(s.Config().PollSeconds) * time.Second; next != interval {
			interval = next
			ticker.Reset(interval)
		}
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
		}
	}
}

func (s *Service) ownSiteReport() (SiteReport, error) {
	c := s.Config()
	var site SiteConfig
	if err := s.owned(&site); err != nil {
		return SiteReport{}, err
	}
	r := SiteReport{ID: site.ID, Name: site.ID, Config: site, Endpoint: c.InternalURL, AdminURL: c.PublicURL, ObservedAt: time.Now().UTC(), ReceivedAt: time.Now().UTC(), Operator: s.operatorLocalSnapshot()}
	status := s.radioStatus()
	r.Radio = &status
	hardware := s.ownHardwareReport(site, status)
	r.Hardware = &hardware
	r.RFEnabled = status.State == "running" && status.Mode == "hardware"
	var zone ZoneConfig
	if s.readCache("zone", &zone) {
		r.RFSSID = zone.RFSSID
		for _, identity := range zone.Sites {
			if identity.ID == site.ID {
				r.Name = identity.Name
				r.SiteID = identity.SiteID
			}
		}
	}
	return r, nil
}

func (s *Service) siteReports() []SiteReport {
	if s.Config().Role == RoleSite {
		r, err := s.ownSiteReport()
		if err == nil {
			return []SiteReport{r}
		}
		return []SiteReport{}
	}
	reports := []SiteReport{}
	s.readCache("site_reports", &reports)
	var zone ZoneConfig
	if s.Config().Role == RoleZone {
		_ = s.owned(&zone)
	} else {
		s.readCache("zone", &zone)
	}
	allowed := map[string]bool{}
	for _, site := range zone.Sites {
		allowed[site.ID] = true
	}
	out := []SiteReport{}
	for _, r := range reports {
		if !allowed[r.ID] {
			continue
		}
		if time.Since(r.ReceivedAt) > time.Duration(max(10, s.Config().PollSeconds*4))*time.Second {
			r.Error = "Report is stale; current Site state is unknown."
		}
		out = append(out, r)
	}
	return out
}

func (s *Service) State(session Session) (map[string]any, error) {
	if session.UserID != "" {
		return s.operatorState(session), nil
	}
	c := s.Config()
	raw, err := s.Store.Config()
	if err != nil {
		return nil, err
	}
	system, zone, _ := s.documents()
	connections := []Connection{}
	if c.Role != RoleSystem {
		var connection Connection
		if s.readCache("connection", &connection) {
			actualURL := s.upstreamURL()
			if connection.URL != actualURL {
				connection.State = "Not connected"
				connection.LastSuccess = time.Time{}
				connection.Error = "This connection has not been checked yet."
			}
			connection.URL = actualURL
			if connection.State == "Connected" && time.Since(connection.LastSuccess) > time.Duration(max(10, c.PollSeconds*4))*time.Second {
				connection.State = "Cached settings"
				connection.Error = "Waiting for fresh contact with the upstream server."
			}
			connections = append(connections, connection)
		} else {
			connections = append(connections, Connection{Name: "Upstream", URL: s.upstreamURL(), State: "Not connected"})
		}
	}
	if c.Role == RoleApplication && s.mediaClient != nil {
		connections = append(connections, s.mediaClient.ConnectionStatus())
	}
	tree := s.adminTopology(zone)
	events, err := s.Store.Events(50)
	if err != nil {
		return nil, err
	}
	status := s.radioStatus()
	return map[string]any{"role": c.Role, "instance_id": c.InstanceID, "username": session.Username, "csrf": session.CSRF, "is_administrator": true, "radio": status, "capabilities": map[string]bool{"radio": c.Role == RoleSite && s.radio != nil, "activity": true, "users": c.Role == RoleSystem, "dispatch": c.Role == RoleApplication, "dispatch_wav": c.Role == RoleApplication}, "config": raw, "system": system, "zone": zone, "site_reports": s.siteReports(), "connections": connections, "settings": c.Settings(), "events": events, "topology": tree, "rf_enabled": status.State == "running"}, nil
}

func (s *Service) uploadActivity(ctx context.Context) {
	if s.activity == nil || s.Config().Role == RoleSystem {
		return
	}
	events, err := s.activity.Pending(128)
	if err != nil || len(events) == 0 {
		return
	}
	var reply struct {
		IDs []string `json:"ids"`
	}
	if s.upstreamRequest(ctx, "POST", "/internal/events", map[string]any{"events": events}, &reply) == nil {
		_ = s.activity.Acknowledge(reply.IDs)
	}
}
func (s *Service) activityInternal(w http.ResponseWriter, r *http.Request, p PeerCredential) {
	role := s.Config().Role
	if r.Method != "POST" || !((role == RoleSystem && p.Role == RoleZone) || (role == RoleZone && (p.Role == RoleSite || p.Role == RoleApplication)) || (role == RoleSite && p.Role == RoleApplication)) {
		fail(w, 403, errors.New("activity origin is not enrolled here"))
		return
	}
	var input struct {
		Events []activity.Event `json:"events"`
	}
	if err := readJSON(w, r, &input); err != nil || len(input.Events) > 256 {
		fail(w, 400, errors.New("invalid activity batch"))
		return
	}
	ids := []string{}
	credentials, err := readCredentials(s.Config().CredentialFile)
	if err != nil {
		fail(w, 503, errors.New("service enrollment is unavailable"))
		return
	}
	for _, event := range input.Events {
		if p.Role == RoleSite {
			forwarded := false
			if event.SourceKind == "application" || event.SourceKind == "automation" {
				for _, known := range credentials.Peers {
					if known.Role == RoleApplication && known.InstanceID == event.Origin {
						forwarded = true
					}
				}
			}
			if event.SiteID != p.InstanceID || (event.Origin != p.InstanceID && !forwarded) {
				fail(w, 403, errors.New("Site cannot report another Site's observations"))
				return
			}
		}
		if p.Role == RoleApplication && event.Origin != p.InstanceID {
			fail(w, 403, errors.New("Application Server cannot report another origin"))
			return
		}
		physicalState := event.Type == "registration" || event.Type == "affiliation" || event.Type == "deregistration"
		if (p.Role == RoleApplication && (physicalState || event.SourceKind == "rf")) || (p.Role == RoleSite && physicalState && event.SourceKind != "rf") {
			fail(w, 403, errors.New("physical radio presence must be reported by its Site"))
			return
		}
		if role == RoleSite && p.Role == RoleApplication {
			event.SiteID = s.Config().InstanceID
		}
		if err := s.activity.Append(event); err != nil {
			fail(w, 400, err)
			return
		}
		ids = append(ids, event.ID)
	}
	s.refreshDirectory()
	respond(w, 200, map[string]any{"ids": ids})
}

func topologySites(t topology) []string {
	out := make([]string, 0, len(t.SiteURLs))
	for _, address := range t.SiteURLs {
		out = append(out, address)
	}
	return out
}

// Navigation is built exclusively from explicit web addresses. Internal
// endpoints never become administration links, including before a report.
func (s *Service) adminTopology(zone *ZoneConfig) topology {
	c := s.Config()
	tree := topology{SiteURLs: map[string]string{}}
	s.readCache("topology", &tree)
	if tree.SiteURLs == nil {
		tree.SiteURLs = map[string]string{}
	}
	switch c.Role {
	case RoleSystem:
		tree.SystemURL = c.PublicURL
		var report zoneReport
		if s.readCache("zone_report", &report) {
			tree.ZoneURL = report.AdminURL
		}
	case RoleZone:
		tree.ZoneURL = c.PublicURL
	case RoleSite:
		tree.SiteURLs[c.InstanceID] = c.PublicURL
	case RoleApplication:
		tree.ApplicationURL = c.PublicURL
	}
	allowed := map[string]bool{}
	if zone != nil {
		for _, site := range zone.Sites {
			allowed[site.ID] = true
			tree.SiteURLs[site.ID] = site.AdminURL
		}
	}
	for _, report := range s.siteReports() {
		if allowed[report.ID] {
			tree.SiteURLs[report.ID] = report.AdminURL
		}
	}
	if c.Role == RoleSite && allowed[c.InstanceID] {
		tree.SiteURLs[c.InstanceID] = c.PublicURL
	}
	for id := range tree.SiteURLs {
		if !allowed[id] {
			delete(tree.SiteURLs, id)
		}
	}
	return tree
}
