package framework

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"open25.local/open25-fne/internal/p25crypto"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync/atomic"
	"time"

	"open25.local/open25-fne/internal/activity"
	"open25.local/open25-fne/internal/dispatch"
	"open25.local/open25-fne/internal/radio"
	"open25.local/open25-fne/internal/siteaudio"
)

type directorySnapshot struct {
	System      SystemConfig
	Zone        *ZoneConfig
	Users       map[string]userRecord
	Automations map[string]automationRecord
	Reports     []SiteReport
	Presence    []activity.Radio
}
type atomicDirectory struct {
	value atomic.Pointer[directorySnapshot]
}

type radioRuntime interface {
	Start(context.Context, radio.Profile, string) error
	Stop(context.Context) error
	StopRun(context.Context, time.Time) error
	Close() error
	Status() radio.Status
	SourceState() radio.SourceState
	SourceAllowed(int, int, time.Time) bool
	SetZoneConnected(bool)
	ApplyPolicy(context.Context, radio.Profile) error
	ApplyInhibitionPolicy(context.Context, []int) error
	Network() radio.Network
	ControlClient() (*radio.NativeClient, error)
	Command(context.Context, radio.Command) (map[string]json.RawMessage, error)
	HasRFInterest(int) bool
	StartChannel(context.Context, string) error
	StopChannel(context.Context, string) error
}

func (s *Service) switchControl(ctx context.Context, channel string) error {
	controller, ok := s.radio.(interface {
		SwitchControl(context.Context, string) error
	})
	if !ok {
		return errors.New("control-channel switching is unavailable at this Site")
	}
	return controller.SwitchControl(ctx, channel)
}

// The caller owns cancellation of a long adapter operation, while the adapter
// owns every RF process/lease. operationMu protects publication, never warmup.
type radioOperation struct {
	signature string
	cancel    context.CancelFunc
	stopping  bool
	done      chan struct{}
}

// An accepted Start owns its cancellation and startup lease before HTTP202.
// It is transient work, never persisted or replayed after cancellation.
type radioStartAdmission struct {
	channel, signature string
	ctx                context.Context
	cancel             context.CancelFunc
	stopService        func() bool
	lease              *os.File
	done               chan struct{}
}

func (s *Service) refreshDirectory() {
	system, zone, _ := s.documents()
	records, _ := s.Store.userRecords()
	users := map[string]userRecord{}
	for _, r := range eligibleUsers(records, system) {
		users[r.User.ID] = r
	}
	automationRecords, _ := s.Store.automationRecords()
	automations := map[string]automationRecord{}
	for _, record := range eligibleAutomations(automationRecords, system) {
		automations[record.Automation.ID] = record
	}
	reports := s.siteReports()
	presence := []activity.Radio{}
	if s.activity != nil {
		rows, _ := s.activity.Presence()
		allowed := map[string]bool{}
		if zone != nil {
			for _, site := range zone.Sites {
				allowed[site.ID] = true
			}
		}
		for _, row := range rows {
			if allowed[row.SiteID] && row.Kind == "rf" {
				presence = append(presence, row)
			}
		}
	}
	next := &directorySnapshot{System: system, Zone: zone, Users: users, Automations: automations, Reports: reports, Presence: presence}
	old := s.directory.value.Swap(next)
	if role := s.Config().Role; role == RoleZone || role == RoleSite {
		_ = s.currentRadioInhibition()
		if role == RoleSite {
			_ = s.siteActualInhibition()
		}
	}
	s.refreshOperatorPolicy()
	if s.operationWake != nil {
		select {
		case s.operationWake <- struct{}{}:
		default:
		}
	}
	if s.hub != nil && old != nil {
		positionsChanged := profileSignature(old.System.ConsolePositions) != profileSignature(system.ConsolePositions)
		for id, u := range old.Users {
			n, ok := users[id]
			if !ok || positionsChanged || !sameUserAuthentication(u, n) {
				s.hub.InvalidateUser(id)
			}
		}
		for id, oldAutomation := range old.Automations {
			current, ok := automations[id]
			if !ok || profileSignature(oldAutomation) != profileSignature(current) {
				s.hub.InvalidateAutomation(id)
			}
		}
	}
}
func (s *Service) view() *directorySnapshot {
	if v := s.directory.value.Load(); v != nil {
		return v
	}
	return &directorySnapshot{Users: map[string]userRecord{}, Automations: map[string]automationRecord{}}
}
func (s *Service) openOperations() error {
	c := s.Config()
	s.operationCtx, s.operationCancel = context.WithCancel(context.Background())
	s.operationWake = make(chan struct{}, 1)
	var err error
	if c.Role == RoleSystem {
		s.activity, err = activity.OpenArchive(filepath.Join(c.StateDir, "activity.db"))
	} else {
		s.activity, err = activity.Open(filepath.Join(c.StateDir, "activity.db"))
	}
	if err != nil {
		return err
	}
	s.refreshDirectory()
	if c.Role == RoleZone {
		s.rfAuthority, s.rfAuthorityError = openRFSourceAuthority(filepath.Join(c.StateDir, "rf-source-authority.json"), c.InstanceID)
		if s.rfAuthorityError != nil {
			s.runtimeError(s.rfAuthorityError)
		}
	}
	if c.Role == RoleSite {
		if state := s.siteActualInhibition(); state.err != nil {
			return state.err
		}
	}
	s.peers = newMediaPeers(s)
	s.broker = newCallBroker(s)
	if err = s.openVoice(); err != nil {
		return err
	}
	if c.Role == RoleSite {
		s.mediaClient = newMediaClient(s)
	}
	if c.Role == RoleApplication {
		s.crypto = &keyRuntime{keys: map[p25crypto.Identity]bool{}}
		s.mediaClient = newMediaClient(s)
		router := &networkRouter{service: s}
		s.hub = dispatch.New(dispatch.Options{Router: router, Available: router.Available, Event: s.dispatchEvent, ICEPortMin: c.Media.ICEPortMin, ICEPortMax: c.Media.ICEPortMax})
		if _, err = s.automationRuntimeForRequest(); err != nil {
			return err
		}
	}
	if c.Role == RoleSite && c.Radio.Engine != "" {
		controller, openErr := radio.New(radio.Options{StateDir: filepath.Join(c.StateDir, "radio"), MaintenanceLockPath: filepath.Join(c.StateDir, "radio-maintenance.lock"), EnginePath: c.Radio.Engine, FNEPath: c.Radio.FNE, AllowRF: c.Radio.AllowRF, BaseRPCPort: c.Radio.BaseRPCPort, FNEPort: c.Radio.FNEPort, OnEvent: s.radioEvent, OnInhibition: s.persistSiteInhibition})
		if openErr != nil {
			return openErr
		}
		s.radio = controller
		s.operationWG.Add(1)
		go func() { defer s.operationWG.Done(); s.publishRFSourceState() }()
		s.mediaClient = newMediaClient(s)
		s.rfAdmissions = &rfAdmissionManager{s: s, entries: map[string]*admittedRF{}, aliases: map[string]string{}, seen: map[string]time.Time{}}
		s.operationWG.Add(1)
		go func() { defer s.operationWG.Done(); s.rfAdmissions.run() }()
	}
	s.operationWG.Add(1)
	go s.operationLoop()
	return nil
}
func (s *Service) closeOperations() {
	if s.operationCancel == nil {
		return
	}
	s.operationCancel()
	if s.rfAuthority != nil {
		s.rfAuthority.cancel()
	}
	s.closeAutomationJobs()
	s.operationWG.Wait()
	if s.hub != nil {
		s.hub.Close()
	}
	if s.crypto != nil {
		s.crypto.reconcileMu.Lock()
		old := s.crypto.detach()
		s.crypto.reconcileMu.Unlock()
		if old != nil {
			_ = old.Close()
		}
	}
	if s.mediaClient != nil {
		s.mediaClient.Close()
	}
	if s.broker != nil {
		s.broker.Close()
	}
	if s.peers != nil {
		s.peers.Close()
	}
	if s.voice.Load() != nil {
		s.voice.Load().Close()
	}
	s.operationMu.Lock()
	ctx, cancel := s.shutdownContext()
	_ = s.closeRFMediaLocked(ctx)
	cancel()
	if s.radio != nil {
		_ = s.radio.Close()
	}
	s.operationMu.Unlock()
	if s.activity != nil {
		_ = s.activity.Close()
	}
	s.operationCancel = nil
}
func (s *Service) operationLoop() {
	defer s.operationWG.Done()
	ticker := time.NewTicker(500 * time.Millisecond)
	defer ticker.Stop()
	lastCryptoError := ""
	for {
		select {
		case <-s.operationCtx.Done():
			return
		case <-ticker.C:
		case <-s.operationWake:
		}
		if s.radio != nil {
			s.expireRadioCommandRecords()
			connected := false
			if s.mediaClient != nil {
				address := s.mediaClient.URL()
				connected = address != "" && address == s.upstreamURL()
			}
			// The heartbeat-protected live link determines wide-area operation.
			// Configuration polling can take longer while writing local storage.
			s.radio.SetZoneConnected(connected)
			s.cancelStaleRadioOperation()
			if s.radioReconcile.TryLock() {
				s.operationWG.Add(1)
				go func() {
					defer s.operationWG.Done()
					defer s.radioReconcile.Unlock()
					s.reconcileRadio()
				}()
			}
		}
		if s.mediaClient != nil {
			s.mediaClient.Ensure()
		}
		if s.crypto != nil {
			if err := s.reconcileCrypto(); err != nil {
				if err.Error() != lastCryptoError {
					s.runtimeError(err)
					lastCryptoError = err.Error()
				}
			} else {
				lastCryptoError = ""
			}
		}
	}
}
func (s *Service) emit(e activity.Event) {
	e.Origin = s.Config().InstanceID
	if e.SiteID == "" && s.Config().Role == RoleSite {
		e.SiteID = s.Config().InstanceID
	}
	if s.activity != nil {
		_ = s.activity.Enqueue(e)
	}
}
func (s *Service) radioEvent(e radio.Event) {
	s.observeRadioCommand(e)
	audience := e.Audience
	if audience == "operator" {
		audience = "dispatcher"
	} else if audience == "system_operator" || audience == "" {
		audience = "operator"
	}
	if audience != "dispatcher" && audience != "operator" && audience != "diagnostic" && audience != "security" {
		audience = "diagnostic"
	}
	kind := e.Kind
	if translated, ok := map[string]string{"registered": "registration", "affiliated": "affiliation", "deregistered": "deregistration", "unaffiliated": "unaffiliation", "call_started": "call_start", "call_ended": "call_end"}[kind]; ok {
		kind = translated
	}
	details := map[string]any{"direction": e.Direction, "encrypted": e.Encrypted}
	talkgroup := e.TGID
	directed := false
	switch e.Kind {
	case "status", "status_query_response", "status_query_no_response", "message", "page_request", "page", "page_ack", "radio_check", "radio_check_ack", "radio_check_no_response", "affiliation_query", "registration_demand", "inhibit", "inhibit_ack", "uninhibit", "uninhibit_ack", "regroup", "regroup_ack", "regroup_cancel", "regroup_cancel_ack", "acknowledgement":
		directed = true
	}
	if e.Kind == "unaffiliated" {
		kind = "affiliation"
		talkgroup = 0
		details["affiliation_state"] = "unaffiliated"
	}
	if directed || talkgroup < 1 || talkgroup > 65534 {
		if talkgroup != 0 {
			details["destination_id"] = talkgroup
		}
		talkgroup = 0
		if e.Kind != "unaffiliated" && e.Kind != "deregistered" && s.radio != nil {
			for _, aff := range s.radio.Status().Affiliations {
				if aff.RID == e.RID {
					talkgroup = aff.TGID
					break
				}
			}
		}
	}
	if e.Code != nil {
		details["code"] = *e.Code
		if e.Kind == "status" || e.Kind == "status_query_response" {
			details["user_status"] = (*e.Code >> 8) & 255
			details["unit_status"] = *e.Code & 255
		}
	}
	if e.RequestorID != 0 {
		details["requestor_id"] = e.RequestorID
	}
	if e.Attempts > 0 {
		details["attempts"] = e.Attempts
	}
	if e.Kind == "emergency" {
		if e.EmergencyInfo1 != nil {
			details["emergency_si1"] = *e.EmergencyInfo1
		}
		if e.EmergencyInfo2 != nil {
			details["emergency_si2"] = *e.EmergencyInfo2
		}
	}
	label := ""
	if e.Code != nil {
		var catalog []SignalLabel
		switch e.Kind {
		case "status", "status_query_response":
			catalog = s.view().System.StatusLabels
		case "message":
			catalog = s.view().System.MessageLabels
		}
		for _, entry := range catalog {
			if entry.ID == *e.Code {
				label = entry.Label
				break
			}
		}
		if label == "" && (e.Kind == "status" || e.Kind == "status_query_response" || e.Kind == "message") {
			prefix := "Status"
			if e.Kind == "message" {
				prefix = "Message"
			}
			label = fmt.Sprintf("%s %d", prefix, *e.Code)
		}
	}
	if e.NativeAt != "" {
		details["native_at"] = e.NativeAt
	}
	if (e.Kind == "emergency" || e.Kind == "emergency_cancel") && talkgroup > 0 {
		if err := s.observeRFEmergency(e.RID, talkgroup, e.Kind == "emergency"); err != nil {
			s.runtimeError(err)
		}
	}
	s.emit(activity.Event{ID: e.ID, At: e.At, SiteID: e.SiteID, ChannelID: e.ChannelID, RadioID: e.RID, TalkgroupID: talkgroup, Type: kind, Label: label, Message: e.Message, Audience: audience, SourceKind: "rf", Details: details})
}
func (s *Service) dispatchEvent(e dispatch.Event) {
	if e.Kind == "call.granted" {
		e.Kind = "call_start"
	} else if e.Kind == "call.ended" {
		e.Kind = "call_end"
	}
	kind := "application"
	details := map[string]any{"user_id": e.UserID, "allocation_id": e.CallID}
	if e.AutomationID != "" {
		kind = "automation"
		details = map[string]any{"automation_id": e.AutomationID, "allocation_id": e.CallID}
	}
	for _, tg := range e.Talkgroups {
		s.emit(activity.Event{At: e.At, RadioID: e.RadioID, TalkgroupID: tg, CallID: e.CallID, Type: e.Kind, Message: e.Message, Audience: "dispatcher", Outcome: e.Outcome, SourceKind: kind, Details: details})
	}
}
func (s *Service) profileRadio() (radio.Profile, error) {
	v := s.view()
	if v.Zone == nil {
		return radio.Profile{}, errors.New("Zone settings have not been received")
	}
	raw, err := s.Store.Config()
	if err != nil {
		return radio.Profile{}, err
	}
	var site SiteConfig
	if err = json.Unmarshal(raw, &site); err != nil {
		return radio.Profile{}, err
	}
	data, _ := json.Marshal(struct {
		System SystemConfig `json:"system"`
		Zone   *ZoneConfig  `json:"zone"`
		Site   SiteConfig   `json:"site"`
	}{v.System, v.Zone, site})
	var p radio.Profile
	err = json.Unmarshal(data, &p)
	if err != nil {
		return p, err
	}
	for i := range p.Site.Channels {
		ch := &p.Site.Channels[i]
		tx, rx, ok := bandPlanFrequencies(v.Zone, ch.Identifier, ch.ChannelNo)
		if !ok {
			if !ch.Enabled {
				continue
			}
			return p, fmt.Errorf("channel %s: cannot calculate frequencies from Band Plan %d and channel %d", ch.ID, ch.Identifier, ch.ChannelNo)
		}
		ch.TXHz, ch.RXHz = tx, rx
	}
	p.Inhibition.Desired, err = s.DesiredInhibitedRadios()
	if err != nil {
		return p, err
	}
	actual := s.siteActualInhibition()
	if actual.err != nil {
		return p, actual.err
	}
	p.Inhibition.Restore = slices.Clone(actual.rows)
	for i := range p.Inhibition.Restore {
		if slices.Contains(p.Inhibition.Desired, p.Inhibition.Restore[i].RadioID) {
			p.Inhibition.Restore[i].PendingUninhibit = false
		}
	}
	return p, err
}
func (s *Service) radioStatus() radio.Status {
	if s.radio == nil {
		return radio.Status{Available: false, State: "stopped", Mode: "none", SiteID: s.Config().InstanceID, Channels: []radio.ChannelStatus{}, Registrations: []radio.Registration{}, Affiliations: []radio.Affiliation{}, Error: "Radio executables are not configured at this Site."}
	}
	status := s.radio.Status()
	status.MaintenanceSupported = status.MaintenanceSupported && s.Config().Role == RoleSite && s.Config().StateDir != ""
	if status.SiteID == "" {
		status.SiteID = s.Config().InstanceID
	}
	return status
}
func (s *Service) startRadio(ctx context.Context, channel string) error {
	admission, err := s.admitRadioStart(ctx, channel)
	if err != nil {
		return err
	}
	defer s.finishRadioStart(admission)
	return s.startRadioAdmitted(admission.ctx, admission)
}

func (s *Service) admitRadioStart(ctx context.Context, channel string) (*radioStartAdmission, error) {
	lease, err := radio.AcquireStartLease(filepath.Join(s.Config().StateDir, "radio-maintenance.lock"))
	if err != nil {
		return nil, err
	}
	s.operationMu.Lock()
	defer s.operationMu.Unlock()
	reject := func(err error) (*radioStartAdmission, error) { _ = lease.Close(); return nil, err }
	if err = ctx.Err(); err != nil {
		return reject(err)
	}
	if s.radio == nil {
		return reject(errors.New("radio executables are not configured"))
	}
	if s.radioOperation != nil || s.radioStarts[""] != nil || s.radioStarts[channel] != nil ||
		(channel == "" && (len(s.radioStarts) != 0 || len(s.radioChannelStops) != 0)) || s.radioChannelStops[channel] != nil {
		return reject(errors.New("a conflicting Site radio operation is already in progress"))
	}
	p, err := s.profileRadio()
	if err != nil {
		return reject(err)
	}
	// One active request per channel, bounded by configured Site resources.
	// The adapter still owns channel identity/role/readiness validation.
	if channel != "" && len(s.radioStarts) >= len(p.Site.Channels) {
		return reject(errors.New("configured Site radio resources already have pending starts"))
	}
	operation, cancel := context.WithCancel(ctx)
	stopService := func() bool { return false }
	if s.operationCtx != nil {
		stopService = context.AfterFunc(s.operationCtx, cancel)
	}
	admission := &radioStartAdmission{channel: channel, signature: profileSignature(p), ctx: operation, cancel: cancel, stopService: stopService, lease: lease, done: make(chan struct{})}
	if s.radioStarts == nil {
		s.radioStarts = make(map[string]*radioStartAdmission)
	}
	s.radioStarts[channel] = admission
	return admission, nil
}

func (s *Service) finishRadioStart(admission *radioStartAdmission) {
	admission.cancel()
	admission.stopService()
	_ = admission.lease.Close()
	s.operationMu.Lock()
	if s.radioStarts[admission.channel] == admission {
		delete(s.radioStarts, admission.channel)
	}
	close(admission.done) // Stop may return only after the SH descriptor closed.
	s.operationMu.Unlock()
}

func (s *Service) startRadioAdmitted(ctx context.Context, admission *radioStartAdmission) error {
	s.operationMu.Lock()
	if s.radioStarts[admission.channel] != admission || admission.ctx.Err() != nil || ctx.Err() != nil {
		s.operationMu.Unlock()
		return context.Canceled
	}
	if s.radio == nil {
		s.operationMu.Unlock()
		return errors.New("radio executables are not configured")
	}
	p, err := s.profileRadio()
	if err != nil || profileSignature(p) != admission.signature {
		admission.cancel()
		s.operationMu.Unlock()
		return context.Canceled
	}
	if admission.channel != "" {
		s.operationMu.Unlock()
		return s.radio.StartChannel(ctx, admission.channel)
	}
	if s.radioOperation != nil {
		s.operationMu.Unlock()
		return errors.New("a Site radio operation is already in progress")
	}
	operation, cancel := context.WithCancel(ctx)
	stopService := func() bool { return false }
	if s.operationCtx != nil {
		stopService = context.AfterFunc(s.operationCtx, cancel)
	}
	defer stopService()
	defer cancel()
	attempt := &radioOperation{signature: profileSignature(p), cancel: cancel, done: make(chan struct{})}
	s.radioOperation = attempt
	s.operationMu.Unlock()
	err = s.radio.Start(operation, p, "hardware")
	s.operationMu.Lock()
	defer s.operationMu.Unlock()
	defer func() {
		if s.radioOperation == attempt {
			s.radioOperation = nil
		}
		close(attempt.done)
	}()
	if err != nil {
		return err
	}
	status := s.radio.Status()
	if operation.Err() != nil || status.State != "running" {
		return context.Canceled
	}
	s.radioAppliedProfile = p
	s.radioPolicy.Store(&p.System)
	s.radioCandidateSignature = profileSignature(p)
	if err = s.ensureCodecLocked(operation, p); err != nil {
		if s.radioOperation == attempt {
			_ = s.closeRFMediaLocked(ctx)
		}
		s.operationMu.Unlock()
		_ = s.radio.StopRun(context.Background(), status.StartedAt)
		s.operationMu.Lock()
		return err
	}
	s.radioRunActive, s.radioRunStartedAt = true, status.StartedAt
	return nil
}

func profileSignature(value any) string {
	if p, ok := value.(radio.Profile); ok {
		// Current actual protection changes through native observations and
		// must not look like a newly saved configuration.
		p.Inhibition.Restore = nil
		value = p
	}
	raw, _ := json.Marshal(value)
	sum := sha256.Sum256(raw)
	return hex.EncodeToString(sum[:])
}

// Configuration wakeups still cancel an in-flight Start/policy warmup when the
// single reconciliation worker is waiting for that adapter operation to finish.
func (s *Service) cancelStaleRadioOperation() {
	s.operationMu.Lock()
	defer s.operationMu.Unlock()
	if s.radioOperation == nil && len(s.radioStarts) == 0 {
		return
	}
	p, err := s.profileRadio()
	for _, admission := range s.radioStarts {
		if err != nil || profileSignature(p) != admission.signature {
			admission.cancel()
		}
	}
	if s.radioOperation != nil && !s.radioOperation.stopping && (err != nil || profileSignature(p) != s.radioOperation.signature) {
		s.radioOperation.cancel()
	}
}

// Cached directory updates are applied by the Site worker, independently of
// request lifetimes. No native lookup can continue allowing a revoked identity
// while the replacement engine is being prepared.
func (s *Service) reconcileRadio() {
	lease, err := radio.AcquireStartLease(filepath.Join(s.Config().StateDir, "radio-maintenance.lock"))
	if err != nil {
		return
	}
	defer lease.Close()
	s.operationMu.Lock()
	if s.radio == nil {
		s.operationMu.Unlock()
		return
	}
	ctx, cancel := context.WithTimeout(s.operationCtx, 60*time.Second)
	defer cancel()
	p, err := s.profileRadio()
	if len(s.radioStarts) != 0 || len(s.radioChannelStops) != 0 {
		for _, admission := range s.radioStarts {
			if err != nil || profileSignature(p) != admission.signature {
				admission.cancel()
			}
		}
		s.operationMu.Unlock()
		return
	}
	if s.radioOperation != nil {
		if !s.radioOperation.stopping && (err != nil || profileSignature(p) != s.radioOperation.signature) {
			s.radioOperation.cancel()
		}
		s.operationMu.Unlock()
		return
	}
	status := s.radio.Status()
	newRun := status.State == "running" && (!s.radioRunActive || !s.radioRunStartedAt.Equal(status.StartedAt))
	if (status.State != "running" || newRun) && (s.radioRunActive || s.rfRouter.Load() != nil || s.codec != nil) {
		// Losing a native lifetime withdraws old calls even if its profile did
		// not change. A failed release RPC to the dead engine cannot preserve
		// an old local floor or disable desired modem recovery.
		s.runtimeError(s.closeRFMediaLocked(ctx))
		s.radioRunActive = false
		s.radioRunStartedAt = time.Time{}
	}
	if err != nil {
		attempt := &radioOperation{stopping: true, cancel: func() {}, done: make(chan struct{})}
		s.radioOperation = attempt
		s.operationMu.Unlock()
		s.runtimeError(err)
		_ = s.radio.Stop(ctx)
		s.finishRadioStop(attempt)
		return
	}
	signature := profileSignature(p)
	if signature == s.radioCandidateSignature && (!newRun || signature == profileSignature(s.radioAppliedProfile)) {
		var attempt *radioOperation
		if newRun {
			attempt = &radioOperation{signature: signature, cancel: cancel, done: make(chan struct{})}
			s.radioOperation = attempt
			err = s.ensureCodecLocked(ctx, p)
			if err == nil {
				s.radioRunActive, s.radioRunStartedAt = true, status.StartedAt
			}
		}
		s.operationMu.Unlock()
		if err != nil {
			s.runtimeError(err)
			_ = s.radio.StopRun(ctx, status.StartedAt)
		}
		if attempt != nil {
			s.operationMu.Lock()
			s.finishRadioOperationLocked(attempt)
			s.operationMu.Unlock()
		}
		return
	}
	s.radioCandidateSignature = signature
	if radio.PolicySignature(p) != radio.PolicySignature(s.radioAppliedProfile) {
		if err = s.closeRFMediaLocked(ctx); err != nil {
			attempt := &radioOperation{stopping: true, cancel: func() {}, done: make(chan struct{})}
			s.radioOperation = attempt
			s.operationMu.Unlock()
			s.runtimeError(err)
			_ = s.radio.Stop(ctx)
			s.finishRadioStop(attempt)
			return
		}
		s.radioRunActive = false
	}
	attempt := &radioOperation{signature: signature, cancel: cancel, done: make(chan struct{})}
	s.radioOperation = attempt
	s.operationMu.Unlock()
	err = s.radio.ApplyInhibitionPolicy(ctx, p.Inhibition.Desired)
	if err == nil {
		err = s.radio.ApplyPolicy(ctx, p)
	}
	s.operationMu.Lock()
	if err != nil {
		report := true
		if s.radioOperation == attempt {
			s.radioOperation = nil
			if errors.Is(err, radio.ErrUserAliasRefresh) {
				s.radioCandidateSignature = ""
				report = s.radioAliasError != err.Error()
				s.radioAliasError = err.Error()
			}
		}
		close(attempt.done)
		s.operationMu.Unlock()
		if report {
			s.runtimeError(err)
		}
		return
	}
	s.radioAliasError = ""
	s.radioAppliedProfile = p
	s.radioPolicy.Store(&p.System)
	status = s.radio.Status()
	if ctx.Err() == nil && status.State == "running" {
		err = s.ensureCodecLocked(ctx, p)
		if err == nil {
			s.radioRunActive, s.radioRunStartedAt = true, status.StartedAt
		}
	}
	s.operationMu.Unlock()
	if err != nil {
		s.runtimeError(err)
		_ = s.radio.StopRun(ctx, status.StartedAt)
	}
	s.operationMu.Lock()
	s.finishRadioOperationLocked(attempt)
	s.operationMu.Unlock()
}

func (s *Service) finishRadioOperationLocked(attempt *radioOperation) {
	if s.radioOperation == attempt {
		s.radioOperation = nil
	}
	close(attempt.done)
}

func (s *Service) ensureCodecLocked(ctx context.Context, p radio.Profile) error {
	status := s.radio.Status()
	network := s.radio.Network()
	signature := profileSignature(struct {
		Network                   radio.Network
		WACN, SystemID, VoiceMFID int
		Callsign                  string
	}{network, p.System.WACN, p.System.SystemID, p.System.VoiceMFID, p.System.Callsign})
	if signature == s.codecSignature && s.codec != nil && s.rfRouter.Load() != nil {
		return nil
	}
	if err := s.closeRFMediaLocked(ctx); err != nil {
		return err
	}
	if len(network.ConsolePeerIDs) == 0 || s.Config().Radio.Bridge == "" {
		return nil
	}
	s.broker.mu.Lock()
	codecEpoch := s.broker.epoch
	s.broker.mu.Unlock()
	codec, err := dispatch.NewCodec(dispatch.CodecOptions{Opaque: true, Binary: s.Config().Radio.Bridge, StateDir: filepath.Join(s.Config().StateDir, "codec"), Identity: p.System.Callsign, MasterAddress: network.Address, MasterPort: network.Port, MasterPassword: network.Secret, PeerIDs: network.ConsolePeerIDs, RadioID: 0xFFFFFB, WACN: p.System.WACN, SystemID: p.System.SystemID, VoiceMFID: p.System.VoiceMFID, OnFrame: func(f dispatch.Frame) { s.codecFrameAtEpoch(f, codecEpoch) }, OnReceiveActivity: func(a dispatch.CodecReceiveActivity) { s.codecReceiveActivityAtEpoch(a, codecEpoch) }, OnEvent: func(e dispatch.CodecEvent) {
		s.emit(activity.Event{At: e.At, TalkgroupID: e.TalkgroupID, Type: "audio_bridge", Message: e.Message, Audience: "operator", Outcome: e.State})
	}})
	if err != nil {
		return err
	}
	// Publish the actual owner before readiness. Stop can close this codec
	// while Start waits, and the caller mutex never delays cancellation.
	s.codec = codec
	s.codecSignature = signature
	s.operationMu.Unlock()
	err = codec.Start(ctx)
	s.operationMu.Lock()
	current := s.radio.Status()
	var owner *radio.NativeClient
	if err == nil && ctx.Err() == nil && s.codec == codec && current.State == "running" && current.StartedAt.Equal(status.StartedAt) {
		owner, err = s.radio.ControlClient()
		current = s.radio.Status()
	}
	if err != nil || owner == nil || ctx.Err() != nil || s.codec != codec || current.State != "running" || !current.StartedAt.Equal(status.StartedAt) {
		s.operationMu.Unlock()
		cleanupErr := codec.Close()
		s.operationMu.Lock()
		if s.codec == codec && cleanupErr == nil {
			s.codec, s.codecSignature = nil, ""
		}
		if err == nil {
			err = context.Canceled
		}
		return errors.Join(err, cleanupErr)
	}
	s.rfRouter.Store(siteaudio.New(siteaudio.Options{Control: func() (radio.Native, error) { return owner, nil }, Codec: codec, HasRFInterest: s.hasRFInterest, AllowTX: s.allowedRadioGroup, Event: func(e siteaudio.Event) {
		s.emit(activity.Event{At: e.At, RadioID: e.RadioID, TalkgroupID: e.TalkgroupID, CallID: e.CallID, Type: e.Kind, Message: e.Message, Audience: "operator", Outcome: e.Outcome, SourceKind: "application", Details: map[string]any{"allocation_id": e.AllocationID}})
	}}))
	return nil
}

func (s *Service) closeRFMediaLocked(ctx context.Context) error {
	router := s.rfRouter.Swap(nil)
	if s.rfAdmissions != nil {
		s.rfAdmissions.retireRun()
	}
	if s.broker != nil {
		s.broker.CancelAll()
	}
	var result error
	if router != nil {
		result = router.Close(ctx)
	}
	if s.codec != nil {
		if err := s.codec.Close(); err != nil {
			result = errors.Join(result, err)
		} else {
			s.codec = nil
		}
	}
	s.codecSignature = ""
	return result
}
func (s *Service) stopRadio(ctx context.Context, channel string) error {
	s.operationMu.Lock()
	retiring := []<-chan struct{}{}
	for _, admission := range s.radioStarts {
		if channel == "" || admission.channel == "" || admission.channel == channel {
			admission.cancel()
			retiring = append(retiring, admission.done)
		}
	}
	if s.radio == nil {
		s.operationMu.Unlock()
		return nil
	}
	if channel != "" {
		if s.radioChannelStops[channel] != nil {
			s.operationMu.Unlock()
			return errors.New("voice channel is already stopping")
		}
		if s.radioOperation != nil && !s.radioOperation.stopping {
			s.radioOperation.cancel()
			retiring = append(retiring, s.radioOperation.done)
		}
		attempt := &radioOperation{stopping: true, cancel: func() {}, done: make(chan struct{})}
		if s.radioChannelStops == nil {
			s.radioChannelStops = make(map[string]*radioOperation)
		}
		s.radioChannelStops[channel] = attempt
		s.operationMu.Unlock()
		err := s.radio.StopChannel(ctx, channel)
		return errors.Join(err, s.awaitRadioRetirement(ctx, retiring, func() {
			s.operationMu.Lock()
			if s.radioChannelStops[channel] == attempt {
				delete(s.radioChannelStops, channel)
			}
			close(attempt.done)
			s.operationMu.Unlock()
		}))
	}
	previous := s.radioOperation
	if previous != nil {
		previous.cancel()
		retiring = append(retiring, previous.done)
	}
	for _, stopped := range s.radioChannelStops {
		retiring = append(retiring, stopped.done)
	}
	attempt := &radioOperation{stopping: true, cancel: func() {}, done: make(chan struct{})}
	s.radioOperation = attempt
	err := s.closeRFMediaLocked(ctx)
	s.radioRunActive, s.radioRunStartedAt = false, time.Time{}
	s.operationMu.Unlock()
	err = errors.Join(err, s.radio.Stop(ctx))
	return errors.Join(err, s.awaitRadioRetirement(ctx, retiring, func() { s.finishRadioStop(attempt) }))
}

// A deadline ends the caller's wait, never the retirement barrier. The final
// owner removes it only after every admitted worker and shared lease is gone.
func (s *Service) awaitRadioRetirement(ctx context.Context, retiring []<-chan struct{}, finish func()) error {
	for _, done := range retiring {
		select {
		case <-done:
		case <-ctx.Done():
			go func() {
				for _, pending := range retiring {
					<-pending
				}
				finish()
			}()
			return ctx.Err()
		}
	}
	finish()
	return nil
}

func (s *Service) finishRadioStop(attempt *radioOperation) {
	s.operationMu.Lock()
	if s.radioOperation == attempt {
		s.radioOperation = nil
	}
	close(attempt.done)
	s.operationMu.Unlock()
}
func (s *Service) allowedRadioGroup(rid, tg int) bool {
	v := s.view()
	if !groupAtSite(v.System, tg, s.Config().InstanceID) {
		return false
	}
	for _, r := range v.System.Radios {
		if r.RID == rid && r.Enabled {
			if r.Kind == "subscriber" && s.radioServiceInhibited(rid) {
				return false
			}
			// This private allocator receives only calls admitted by the broker.
			// It must preserve authorized patch relays whose destination need not
			// permit this subscriber to transmit there directly.
			return r.Kind == "subscriber" || r.Kind == "console" || r.Kind == "mobile" || r.Kind == "automation"
		}
	}
	return false
}
func (s *Service) principal(session Session) (dispatch.Principal, error) {
	if session.UserID == "" {
		return dispatch.Principal{}, errors.New("sign in with an application account to use radio audio")
	}
	r, ok := s.view().Users[session.UserID]
	if !ok || !r.User.Enabled {
		return dispatch.Principal{}, dispatch.ErrPermission
	}
	u, _, err := resolveUserPosition(r.User, s.view().System, session.PositionID)
	if err != nil {
		return dispatch.Principal{}, err
	}
	tx := []int{}
	if slices.Contains(u.Permissions, "ptt") {
		tx = slices.Clone(u.Talkgroups)
	}
	return dispatch.Principal{UserID: u.ID, RadioID: u.RadioID, ReceiveGroups: slices.Clone(u.Talkgroups), TransmitGroups: tx, CanTransmit: len(tx) > 0, CanSecure: slices.Contains(u.Permissions, "secure_ptt")}, nil
}
func (s *Service) operationsHTTP(w http.ResponseWriter, r *http.Request, session Session) bool {
	path := r.URL.Path
	if path == "/api/radio/c4fm-test" || strings.HasPrefix(path, "/api/radio/c4fm-test/") {
		s.c4fmTestHTTP(w, r, session)
		return true
	}
	if path == "/api/automations" || strings.HasPrefix(path, "/api/automations/") {
		s.automationsHTTP(w, r, session)
		return true
	}
	if strings.HasPrefix(path, "/api/positions") {
		if session.UserID != "" {
			fail(w, 403, dispatch.ErrPermission)
		} else {
			s.positionsHTTP(w, r)
			s.refreshDirectory()
		}
		return true
	}
	if strings.HasPrefix(path, "/api/crypto/") {
		s.cryptoHTTP(w, r, session)
		return true
	}
	if strings.HasPrefix(path, "/api/operator/") {
		s.operatorHTTP(w, r, session)
		return true
	}
	if path == "/api/profile" {
		s.profileHTTP(w, r, session)
		return true
	}
	if path == "/api/desktop-workspace" {
		s.desktopWorkspaceHTTP(w, r, session)
		return true
	}
	if strings.HasPrefix(path, "/api/users") {
		if session.UserID != "" {
			fail(w, 403, errors.New("administrator sign-in required"))
			return true
		}
		s.usersHTTP(w, r)
		s.refreshDirectory()
		return true
	}
	if strings.HasPrefix(path, "/api/history/") {
		s.historyHTTP(w, r, session)
		return true
	}
	if path == "/api/activity" {
		s.activityHTTP(w, r, session)
		return true
	}
	if path == "/api/radio" && r.Method == "GET" {
		if session.UserID != "" {
			fail(w, 403, dispatch.ErrPermission)
		} else {
			respond(w, 200, s.radioStatus())
		}
		return true
	}
	if path == "/api/radio/command" || path == "/api/radio/commands" {
		s.radioCommandHTTP(w, r, session)
		return true
	}
	if path == "/api/radio/start" || path == "/api/radio/stop" || path == "/api/radio/switch-control" {
		if r.Method != "POST" {
			w.WriteHeader(405)
			return true
		}
		if session.UserID != "" || s.Config().Role != RoleSite {
			fail(w, 403, errors.New("radio operation belongs to Site Administration"))
			return true
		}
		var input struct {
			ChannelID string `json:"channel_id"`
		}
		if err := readJSON(w, r, &input); err != nil {
			fail(w, 400, err)
			return true
		}
		if s.radio == nil {
			fail(w, 503, errors.New("radio executables are not configured"))
			return true
		}
		if path == "/api/radio/switch-control" {
			if err := s.switchControl(s.operationCtx, input.ChannelID); err != nil {
				fail(w, http.StatusServiceUnavailable, err)
			} else {
				respond(w, http.StatusAccepted, map[string]any{"accepted": true, "message": "Control switch queued; waiting for the Site to become idle."})
			}
			return true
		}
		var admission *radioStartAdmission
		if path == "/api/radio/start" {
			var err error
			// Accepted work belongs to the service, not the request connection.
			admission, err = s.admitRadioStart(s.operationCtx, input.ChannelID)
			if err != nil {
				fail(w, http.StatusServiceUnavailable, err)
				return true
			}
		}
		s.operationWG.Add(1)
		go func() {
			defer s.operationWG.Done()
			if admission != nil {
				defer s.finishRadioStart(admission)
			}
			operation := s.operationCtx
			if admission != nil {
				operation = admission.ctx
			}
			ctx, cancel := context.WithTimeout(operation, 60*time.Second)
			defer cancel()
			var err error
			if path == "/api/radio/start" {
				err = s.startRadioAdmitted(ctx, admission)
			} else {
				err = s.stopRadio(ctx, input.ChannelID)
			}
			message := "Radio operation completed."
			if err != nil {
				message = err.Error()
			}
			s.emit(activity.Event{Type: "radio_operation", Message: message, Audience: "operator"})
		}()
		respond(w, 202, map[string]bool{"accepted": true})
		return true
	}
	if strings.HasPrefix(path, "/api/dispatch/") {
		if s.Config().Role != RoleApplication || s.hub == nil {
			fail(w, 403, dispatch.ErrUnavailable)
			return true
		}
		p, err := s.principal(session)
		if err != nil {
			fail(w, 403, err)
			return true
		}
		switch path {
		case "/api/dispatch/session":
			s.hub.ServeSession(w, r, p, session.Key, s.loadedTrafficKeys())
		case "/api/dispatch/state":
			s.hub.ServeState(w, r, p)
		case "/api/dispatch/audio":
			s.hub.ServeSocket(w, r, p, session.Key)
		case "/api/dispatch/wav":
			s.hub.ServeWAV(w, r, p, session.Key)
		case "/api/dispatch/speech", "/api/dispatch/tts":
			s.speechHTTP(w, r, p, session.Key)
		default:
			http.NotFound(w, r)
		}
		return true
	}
	return false
}
func (s *Service) activityHTTP(w http.ResponseWriter, r *http.Request, session Session) {
	if r.Method != "GET" {
		w.WriteHeader(405)
		return
	}
	q := activity.Query{Limit: 200, SiteID: r.URL.Query().Get("site_id"), Audience: r.URL.Query().Get("audience")}
	if s.Config().Role == RoleSite {
		q.SiteID = s.Config().InstanceID
	}
	if session.UserID != "" {
		u, ok := s.view().Users[session.UserID]
		if !ok {
			fail(w, 403, dispatch.ErrPermission)
			return
		}
		q.Restricted = true
		effective, _, err := resolveUserPosition(u.User, s.view().System, session.PositionID)
		if err != nil {
			fail(w, 403, dispatch.ErrPermission)
			return
		}
		q.Groups = effective.Talkgroups
		q.Audience = ""
		if !slices.Contains(effective.Permissions, "location_view") {
			q.LocationRadioID = effective.RadioID
		}
	}
	events, err := s.activity.Events(q)
	if err != nil {
		fail(w, 500, err)
		return
	}
	radios, _ := s.activity.Presence()
	callPage, _ := s.activity.CallHistory(q)
	calls := callPage.Calls
	if q.SiteID != "" {
		radios = slices.DeleteFunc(radios, func(v activity.Radio) bool { return v.SiteID != q.SiteID })
	}
	if session.UserID != "" {
		groups := q.Groups
		radios = slices.DeleteFunc(radios, func(v activity.Radio) bool { return !slices.Contains(groups, v.AffiliatedTalkgroup) })
	}
	observed := map[int]bool{}
	for _, event := range events {
		observed[event.RadioID] = true
	}
	for _, call := range calls {
		observed[call.RadioID] = true
	}
	for _, radio := range radios {
		observed[radio.RID] = true
	}
	aliases := map[int]string{}
	directory := s.view().System.Radios
	for _, known := range directory {
		if observed[known.RID] {
			aliases[known.RID] = known.Alias
		}
	}
	for i := range radios {
		for _, known := range directory {
			if known.RID == radios[i].RID {
				radios[i].Alias = known.Alias
				radios[i].Kind = known.Kind
			}
		}
	}
	respond(w, 200, map[string]any{"events": events, "radios": radios, "calls": calls, "radio_aliases": aliases})
}
func (s *Service) runtimeError(err error) {
	if err != nil {
		s.emit(activity.Event{Type: "runtime_error", Message: fmt.Sprint(err), Audience: "operator", Severity: "warning"})
	}
}
