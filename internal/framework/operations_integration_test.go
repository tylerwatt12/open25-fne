package framework

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/cookiejar"
	"net/url"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/coder/websocket"
	"open25.local/open25-fne/internal/activity"
	"open25.local/open25-fne/internal/dispatch"
	"open25.local/open25-fne/internal/radio"
	"open25.local/open25-fne/internal/routing"
	"open25.local/open25-fne/internal/siteaudio"
)

type policyApplication struct {
	profile radio.Profile
	context context.Context
}
type operationRadio struct {
	mu            sync.Mutex
	status        radio.Status
	applications  chan policyApplication
	applyError    error
	applyWait     <-chan struct{}
	commands      []radio.Command
	commandError  error
	starts, stops int
}

func (r *operationRadio) Start(context.Context, radio.Profile, string) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.starts++
	return nil
}
func (r *operationRadio) Stop(context.Context) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.stops++
	r.status.State = "stopped"
	return nil
}
func (r *operationRadio) StopRun(ctx context.Context, startedAt time.Time) error {
	if !r.Status().StartedAt.Equal(startedAt) {
		return nil
	}
	return r.Stop(ctx)
}
func (r *operationRadio) Close() error         { return nil }
func (r *operationRadio) Status() radio.Status { r.mu.Lock(); defer r.mu.Unlock(); return r.status }
func (r *operationRadio) SourceState() radio.SourceState {
	r.mu.Lock()
	defer r.mu.Unlock()
	run := r.status.StartedAt
	if run.IsZero() {
		run = time.Unix(1, 0).UTC()
	}
	return radio.SourceState{SiteID: r.status.SiteID, NativeStartedAt: run, ObservedAt: time.Now().UTC(), Active: r.status.State == "running", Affiliations: slices.Clone(r.status.Affiliations), Registrations: slices.Clone(r.status.Registrations)}
}
func (r *operationRadio) SourceAllowed(rid, tg int, run time.Time) bool {
	state := r.SourceState()
	_, found := findRFAffiliation(state.Affiliations, rid, tg)
	return state.Active && state.NativeStartedAt.Equal(run) && found
}
func (r *operationRadio) SetZoneConnected(connected bool) {
	r.mu.Lock()
	r.status.ZoneConnected = connected
	r.mu.Unlock()
}
func (r *operationRadio) ApplyPolicy(ctx context.Context, p radio.Profile) error {
	r.mu.Lock()
	err, wait := r.applyError, r.applyWait
	r.mu.Unlock()
	r.applications <- policyApplication{p, ctx}
	if wait != nil {
		select {
		case <-wait:
		case <-ctx.Done():
			return ctx.Err()
		}
	}
	return err
}
func (r *operationRadio) ApplyInhibitionPolicy(_ context.Context, desired []int) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	rows := []radio.InhibitState{}
	for _, rid := range desired {
		rows = append(rows, radio.InhibitState{RadioID: rid, DesiredInhibited: true, Blocked: true, Procedure: "idle"})
	}
	for _, old := range r.status.Inhibition {
		if !slices.Contains(desired, old.RadioID) && old.Blocked {
			old.DesiredInhibited = false
			old.PendingUninhibit = true
			rows = append(rows, old)
		}
	}
	r.status.Inhibition = rows
	r.status.InhibitionReady = true
	return nil
}
func (r *operationRadio) Network() radio.Network { return radio.Network{ConsolePeerIDs: map[int]int{}} }
func (r *operationRadio) ControlClient() (*radio.NativeClient, error) {
	return nil, dispatch.ErrUnavailable
}
func (r *operationRadio) Command(_ context.Context, command radio.Command) (map[string]json.RawMessage, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.commands = append(r.commands, command)
	return map[string]json.RawMessage{"status": json.RawMessage("200")}, r.commandError
}
func (r *operationRadio) HasRFInterest(int) bool                     { return false }
func (r *operationRadio) StartChannel(context.Context, string) error { return nil }
func (r *operationRadio) StopChannel(context.Context, string) error  { return nil }

func operationFixture(t *testing.T) (*Service, *operationRadio) {
	return operationConfiguredFixture(t, nil)
}

func operationConfiguredFixture(t *testing.T, setup func(*Service)) (*Service, *operationRadio) {
	t.Helper()
	store := storeOpen(t, RoleSite)
	if err := store.SaveConfig(storeDoc(t, storeFixtureSite())); err != nil {
		t.Fatal(err)
	}
	if err := store.WriteCache("system", storeDoc(t, storeFixtureSystem())); err != nil {
		t.Fatal(err)
	}
	if err := store.WriteCache("zone", storeDoc(t, storeFixtureZone())); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	runtime := &operationRadio{applications: make(chan policyApplication, 8), status: radio.Status{State: "running", Mode: "null", SiteID: "site-1", Affiliations: []radio.Affiliation{{RID: 10001, TGID: 1001, At: time.Now().UTC()}}}}
	events, err := activity.Open(filepath.Join(t.TempDir(), "activity.db"))
	if err != nil {
		t.Fatal(err)
	}
	service := &Service{Store: store, config: FileConfig{Role: RoleSite, InstanceID: "site-1", PublicURL: "http://127.0.0.1:5002", StateDir: t.TempDir()}, operationCtx: ctx, operationCancel: cancel, operationWake: make(chan struct{}, 1), radio: runtime, activity: events}
	service.peers = newMediaPeers(service)
	service.broker = newCallBroker(service)
	service.refreshDirectory()
	profile, err := service.profileRadio()
	if err != nil {
		t.Fatal(err)
	}
	service.radioAppliedProfile = profile
	service.radioCandidateSignature = profileSignature(profile)
	service.radioRunActive = true
	service.radioRunStartedAt = runtime.status.StartedAt
	if setup != nil {
		setup(service)
	}
	service.operationWG.Add(1)
	go service.operationLoop()
	t.Cleanup(service.closeOperations)
	return service, runtime
}

func TestSiteTrunkingUsesLiveLinkRatherThanSlowConfigurationPolling(t *testing.T) {
	site, runtime := operationConfiguredFixture(t, func(s *Service) {
		s.config.UpstreamURL = "http://127.0.0.1:15999"
		s.mediaClient = newMediaClient(s)
		s.mediaClient.activeURL = s.config.UpstreamURL
		s.mediaClient.running = true // This fixture supplies the live-link state.
		if err := s.writeCache("connection", Connection{State: "Connected", LastSuccess: time.Now().Add(-time.Minute)}); err != nil {
			t.Fatal(err)
		}
	})
	wait := func(want bool) {
		t.Helper()
		deadline := time.Now().Add(2 * time.Second)
		for time.Now().Before(deadline) {
			if runtime.Status().ZoneConnected == want {
				return
			}
			time.Sleep(10 * time.Millisecond)
		}
		t.Fatalf("live Zone connection did not become %v", want)
	}
	wait(true)
	site.mediaClient.mu.Lock()
	site.mediaClient.activeURL = ""
	site.mediaClient.mu.Unlock()
	wait(false)
}

func TestSiteWorkerAppliesCachedAccessOnItsOwnLifetime(t *testing.T) {
	site, runtime := operationFixture(t)
	wait := make(chan struct{})
	runtime.mu.Lock()
	runtime.applyWait = wait
	runtime.mu.Unlock()
	defer close(wait)
	system := storeFixtureSystem()
	system.Radios[0].Enabled = false
	if err := site.Store.WriteCache("system", storeDoc(t, system)); err != nil {
		t.Fatal(err)
	}
	site.refreshDirectory()
	select {
	case applied := <-runtime.applications:
		if applied.profile.System.Radios[0].Enabled || applied.profile.Site.ID != "site-1" {
			t.Fatal("worker did not apply the received Site access settings")
		}
		if applied.context.Err() != nil {
			t.Fatal("the Site worker's operation lifetime was already cancelled")
		}
	case <-time.After(2 * time.Second):
		t.Fatal("cached access never reached the running radio adapter")
	}
	runtime.mu.Lock()
	starts, stops := runtime.starts, runtime.stops
	runtime.mu.Unlock()
	if starts != 0 || stops != 0 {
		t.Fatal("framework bypassed the adapter's policy transaction")
	}
}

func TestPhysicalChangesRemainPendingWithoutAutomaticRadioRestart(t *testing.T) {
	site, runtime := operationFixture(t)
	runtime.mu.Lock()
	runtime.applyError = errors.New("RF settings changed; stop and start the Site")
	runtime.mu.Unlock()
	config := storeFixtureSite()
	config.Channels[0].PowerPercent = 65
	if err := site.Store.SaveConfig(storeDoc(t, config)); err != nil {
		t.Fatal(err)
	}
	site.refreshDirectory()
	select {
	case <-runtime.applications:
	case <-time.After(2 * time.Second):
		t.Fatal("saved RF settings were not reconciled")
	}
	time.Sleep(50 * time.Millisecond)
	runtime.mu.Lock()
	defer runtime.mu.Unlock()
	if runtime.starts != 0 || runtime.stops != 0 || runtime.status.State != "running" {
		t.Fatal("pending physical edit restarted or stopped the radio")
	}
}

func TestNativeEventsPopulatePresenceAndPreserveDispatchSignaling(t *testing.T) {
	site, _ := operationFixture(t)
	now := time.Now().UTC()
	code := 1
	site.radioEvent(radio.Event{ID: "register-1", At: now, RID: 10001, Kind: "registered", Audience: "system_operator"})
	site.radioEvent(radio.Event{ID: "affiliate-1", At: now.Add(time.Millisecond), RID: 10001, TGID: 1001, Kind: "affiliated", Audience: "system_operator"})
	site.radioEvent(radio.Event{ID: "message-1", At: now.Add(2 * time.Millisecond), RID: 10001, TGID: 16777212, Kind: "message", Audience: "operator", Code: &code, Direction: "received", Encrypted: true})
	if err := site.activity.Flush(context.Background()); err != nil {
		t.Fatal(err)
	}
	presence, err := site.activity.Presence()
	if err != nil || len(presence) != 1 || !presence[0].Registered || presence[0].AffiliatedTalkgroup != 1001 {
		t.Fatalf("accepted state was not materialized: %+v %v", presence, err)
	}
	events, err := site.activity.Events(activity.Query{Restricted: true, Groups: []int{1001}})
	if err != nil || len(events) != 1 {
		t.Fatalf("dispatcher did not receive its permitted signaling: %+v %v", events, err)
	}
	event := events[0]
	if event.Type != "message" || event.TalkgroupID != 1001 || event.Details["code"] != float64(1) || event.Details["direction"] != "received" || event.Details["encrypted"] != true || event.Details["destination_id"] != float64(16777212) {
		t.Fatalf("signaling fields were discarded: %+v", event)
	}
	site.radioEvent(radio.Event{ID: "unaffiliate-1", At: now.Add(3 * time.Millisecond), RID: 10001, Kind: "unaffiliated", Audience: "system_operator"})
	_ = site.activity.Flush(context.Background())
	presence, _ = site.activity.Presence()
	if !presence[0].Registered || presence[0].AffiliatedTalkgroup != 0 {
		t.Fatal("unaffiliation left stale talkgroup membership")
	}
	site.radioEvent(radio.Event{ID: "deregister-1", At: now.Add(4 * time.Millisecond), RID: 10001, Kind: "deregistered", Audience: "system_operator"})
	_ = site.activity.Flush(context.Background())
	presence, _ = site.activity.Presence()
	if presence[0].Registered {
		t.Fatal("deregistration left the radio registered")
	}
}

func TestDirectedNativeSignalingUsesAffiliationInsteadOfDestinationAsTalkgroup(t *testing.T) {
	site, _ := operationFixture(t)
	for _, kind := range []string{"status", "message", "page_request", "page", "page_ack", "radio_check", "radio_check_ack", "registration_demand", "affiliation_query"} {
		code := 1
		site.radioEvent(radio.Event{ID: "directed-" + kind, At: time.Now(), RID: 10001, TGID: 10002, Kind: kind, Audience: "operator", Code: &code})
	}
	if err := site.activity.Flush(context.Background()); err != nil {
		t.Fatal(err)
	}
	events, err := site.activity.Events(activity.Query{Restricted: true, Groups: []int{1001}})
	if err != nil || len(events) != 9 {
		t.Fatalf("directed signaling disappeared from its affiliated group: %+v %v", events, err)
	}
	for _, event := range events {
		if event.TalkgroupID != 1001 || event.Details["destination_id"] != float64(10002) {
			t.Fatalf("individual destination was interpreted as a talkgroup: %+v", event)
		}
	}
}

func TestDispatchTicketOpensItsAdvertisedAudioSocket(t *testing.T) {
	cluster := newMediaCluster(t).integrationCluster
	var system SystemConfig
	if err := cluster.system.service.owned(&system); err != nil {
		t.Fatal(err)
	}
	if err := cluster.system.service.Store.SaveUser(fixtureUser().ID, fixtureUser(), "dispatch-test-password"); err != nil {
		t.Fatal(err)
	}
	integrationSyncAll(t, cluster)
	jar, _ := cookiejar.New(nil)
	client := &http.Client{Jar: jar, Timeout: 3 * time.Second}
	base := cluster.dispatch.web.URL
	status, body := integrationHTTP(t, client, "POST", base+"/api/login", map[string]string{"username": fixtureUser().Username, "password": "dispatch-test-password"}, map[string]string{"Origin": base})
	if status != 200 {
		t.Fatalf("dispatch login: %d %s", status, body)
	}
	var login struct {
		CSRF string `json:"csrf"`
	}
	_ = json.Unmarshal(body, &login)
	status, body = integrationHTTP(t, client, "POST", base+"/api/dispatch/session", map[string]string{}, map[string]string{"Origin": base, "X-CSRF-Token": login.CSRF})
	if status != 201 {
		t.Fatalf("audio session: %d %s", status, body)
	}
	var ticket struct {
		Ticket string `json:"ticket"`
		Path   string `json:"socket_path"`
	}
	_ = json.Unmarshal(body, &ticket)
	if ticket.Path != "/api/dispatch/audio" {
		t.Fatal("wrong advertised audio path")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	address := strings.Replace(base, "http://", "ws://", 1) + ticket.Path + "?ticket=" + url.QueryEscape(ticket.Ticket)
	connection, _, err := websocket.Dial(ctx, address, &websocket.DialOptions{HTTPClient: client, HTTPHeader: http.Header{"Origin": []string{base}}})
	if err != nil {
		t.Fatal(err)
	}
	defer connection.CloseNow()
	if _, _, err = connection.Read(ctx); err != nil {
		t.Fatal("advertised audio socket did not deliver its initial state", err)
	}
}

func TestZoneForwardedRFRequiresZoneTrustAndOneGroup(t *testing.T) {
	cluster := integrationNewCluster(t)
	integrationSyncAll(t, cluster)
	site := cluster.site.service
	creds, err := readCredentials(site.Config().CredentialFile)
	if err != nil {
		t.Fatal(err)
	}
	creds.Peers["zone-1"] = PeerCredential{Role: RoleZone, InstanceID: "zone-1", Token: strings.Repeat("z", 64)}
	if err = WritePrivateJSON(site.Config().CredentialFile, creds); err != nil {
		t.Fatal(err)
	}
	request := routing.Request{CallRequest: dispatch.CallRequest{ID: "forwarded-rf", LogicalCallID: "original-rf-conversation", RadioID: 10001, Talkgroups: []int{1001}, AuthorizationGroups: []int{1001}}, SourceKind: "rf", OriginSite: "site-2"}
	_, peer, err := cluster.zone.service.peerIdentityForSite("site-1")
	if err != nil {
		t.Fatal(err)
	}
	endpoint, err := cluster.zone.service.voice.Load().discover(context.Background(), cluster.site.server.URL, &peer)
	if err != nil {
		t.Fatal(err)
	}
	call, err := cluster.zone.service.voice.Load().begin(context.Background(), endpoint, request, false)
	if err != nil {
		t.Fatal("enrolled Zone forwarding", err)
	}
	if len(site.broker.router.Active()) != 1 {
		t.Fatal("forwarded allocation absent")
	}
	if err = call.End(context.Background()); err != nil {
		t.Fatal(err)
	}
	missing := request
	missing.ID, missing.LogicalCallID = "missing-conversation", ""
	if _, err = cluster.zone.service.voice.Load().begin(context.Background(), endpoint, missing, false); err == nil {
		t.Fatal("missing conversation accepted")
	}
	for name, change := range map[string]func(*routing.Request){"RFmultiselect": func(q *routing.Request) { q.Talkgroups = []int{1001, 1002}; q.AuthorizationGroups = []int{1001, 1002} }, "reflectedorigin": func(q *routing.Request) { q.OriginSite = "site-1" }} {
		bad := request
		bad.ID = name
		change(&bad)
		if _, err = cluster.zone.service.voice.Load().begin(context.Background(), endpoint, bad, false); err == nil {
			t.Fatal("invalid forwarded RF admitted", name)
		}
	}
	if err = site.validateVoiceSource(PeerCredential{Role: RoleApplication, InstanceID: "application-1"}, request); err == nil {
		t.Fatal("Dispatch claimed RF source")
	}

}

func TestBrokerCancelAllUsesActualOwnersAndLeavesNoFloors(t *testing.T) {
	site, runtime := operationFixture(t)
	site.operationCancel()
	site.operationWG.Wait()
	system := storeFixtureSystem()
	for _, id := range []int{10004, 10005} {
		system.Radios = append(system.Radios, Radio{RID: id, Alias: "Additional subscriber", Kind: "subscriber", Enabled: true})
	}
	for _, id := range []int{1002, 1003} {
		system.Talkgroups = append(system.Talkgroups, Talkgroup{TGID: id, AdmissionMode: "clear", TransmitMode: "clear", Alias: "Additional group", Enabled: true, SiteAccess: "all"})
	}
	if err := site.Store.WriteCache("system", storeDoc(t, system)); err != nil {
		t.Fatal(err)
	}
	site.refreshDirectory()
	runtime.mu.Lock()
	runtime.status.Affiliations = []radio.Affiliation{{RID: 10001, TGID: 1001, At: time.Now().UTC()}, {RID: 10004, TGID: 1002, At: time.Now().UTC()}, {RID: 10005, TGID: 1003, At: time.Now().UTC()}}
	runtime.mu.Unlock()

	for i, owner := range []string{"zone:actual-zone", "dispatch:operator-a", "rf:site-1"} {
		rid := 10001
		if i > 0 {
			rid = 10003 + i
		}
		_, err := site.broker.Begin(context.Background(), owner, fixtureLocalRFRequest(t, site, routing.Request{CallRequest: dispatch.CallRequest{ID: owner, RadioID: rid, Talkgroups: []int{1001 + i}}, SourceKind: "rf", OriginSite: "site-1"}))
		if err != nil {
			t.Fatal(err)
		}
	}
	site.broker.CancelAll()
	if len(site.broker.router.Active()) != 0 {
		t.Fatal("stop left an owned call floor active")
	}
	_, err := site.broker.Begin(context.Background(), "dispatch:new", fixtureLocalRFRequest(t, site, routing.Request{CallRequest: dispatch.CallRequest{ID: "new-call", RadioID: 10001, Talkgroups: []int{1001}}, SourceKind: "rf", OriginSite: "site-1"}))
	if err != nil {
		t.Fatal("cancelled floor was not reusable", err)
	}
}

type blockedSiteLeg struct {
	entered chan struct{}
	release chan struct{}
	ended   atomic.Int32
}

func (r *blockedSiteLeg) Begin(_ context.Context, q dispatch.CallRequest) (dispatch.Grant, error) {
	close(r.entered)
	<-r.release
	return dispatch.Grant{ID: q.ID, Talkgroups: q.Talkgroups}, nil
}
func (r *blockedSiteLeg) Write(context.Context, dispatch.Grant, dispatch.Frame) error { return nil }
func (r *blockedSiteLeg) Renew(context.Context, dispatch.Grant) error                 { return nil }
func (r *blockedSiteLeg) End(context.Context, dispatch.Grant) error                   { r.ended.Add(1); return nil }

func TestCancellationRetiresAnAllocationThatCompletesAfterSiteStop(t *testing.T) {
	site, _ := operationFixture(t)
	site.operationCancel()
	site.operationWG.Wait()
	_ = site.broker.router.Close()
	leg := &blockedSiteLeg{entered: make(chan struct{}), release: make(chan struct{})}
	site.broker.router = routing.New(routing.Options{Authorize: site.broker.authorize, Targets: func(q routing.Request) routing.Selection {
		return routing.Selection{Targets: []routing.Target{{ID: "destination", Router: leg, Groups: q.Talkgroups}}}
	}})
	done := make(chan error, 1)
	go func() {
		_, err := site.broker.Begin(context.Background(), "zone:actual", fixtureLocalRFRequest(t, site, routing.Request{CallRequest: dispatch.CallRequest{ID: "pending-call", RadioID: 10001, Talkgroups: []int{1001}}, SourceKind: "rf", OriginSite: "site-1"}))
		done <- err
	}()
	select {
	case <-leg.entered:
	case <-time.After(time.Second):
		t.Fatal("call did not begin allocation")
	}
	site.broker.CancelAll()
	close(leg.release)
	select {
	case err := <-done:
		if err == nil {
			t.Fatal("a cancelled allocation was admitted after stop")
		}
	case <-time.After(time.Second):
		t.Fatal("pending allocation was not retired")
	}
	if leg.ended.Load() != 1 || len(site.broker.router.Active()) != 0 {
		t.Fatal("late resource or call floor survived cancellation")
	}
}

func TestSiteStopClosesItsAudioRouterBeforeDiscardingIt(t *testing.T) {
	site, runtime := operationFixture(t)
	router := siteaudio.New(siteaudio.Options{AllowTX: site.allowedRadioGroup, HasRFInterest: func(int) bool { return false }})
	if _, err := router.Begin(context.Background(), dispatch.CallRequest{ID: "local-media", RadioID: 10001, Talkgroups: []int{1001}}); err != nil {
		t.Fatal(err)
	}
	site.operationMu.Lock()
	site.rfRouter.Store(router)
	site.operationMu.Unlock()
	if err := site.stopRadio(context.Background(), ""); err != nil {
		t.Fatal(err)
	}
	if router.Status().ActiveCalls != 0 {
		t.Fatal("stop abandoned an active Site audio floor")
	}
	if _, err := router.Begin(context.Background(), dispatch.CallRequest{ID: "after-stop", RadioID: 10001, Talkgroups: []int{1001}}); err == nil {
		t.Fatal("discarded router still admitted calls")
	}
	runtime.mu.Lock()
	stops := runtime.stops
	runtime.mu.Unlock()
	if stops != 1 {
		t.Fatal("native stop was not invoked after media retirement")
	}
}
