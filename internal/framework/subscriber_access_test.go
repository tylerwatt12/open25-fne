package framework

import (
	"context"
	"errors"
	"reflect"
	"slices"
	"testing"
	"time"

	"open25.local/open25-fne/internal/dispatch"
	"open25.local/open25-fne/internal/radio"
	"open25.local/open25-fne/internal/routing"
	"open25.local/open25-fne/internal/siteaudio"
)

func TestSubscriberAccessSeparatesAffiliationTransmitAndApplicationPermissions(t *testing.T) {
	system := storeFixtureSystem()
	system.Radios = append(system.Radios, Radio{RID: 10002, Alias: "Desk", Kind: "console", Enabled: true})
	system.Talkgroups[0].AffiliationAccess = "selected"
	system.Talkgroups[0].AffiliationRadioIDs = []int{10001}
	system.Talkgroups[0].TransmitAccess = "selected"
	service, _ := operationFixture(t)
	set := func() {
		if err := service.Store.WriteCache("system", storeDoc(t, system)); err != nil {
			t.Fatal(err)
		}
		service.refreshDirectory()
	}
	set()
	if !subscriberGroupAllowed(system, 10001, 1001, false) || subscriberGroupAllowed(system, 10001, 1001, true) {
		t.Fatal("receive-only subscriber policy was not independent")
	}
	q := routing.Request{CallRequest: dispatch.CallRequest{ID: "rf-policy", RadioID: 10001, Talkgroups: []int{1001}}, SourceKind: "rf", OriginSite: "site-1"}
	q = fixtureLocalRFRequest(t, service, q)
	if err := service.broker.authorize(q); !errors.Is(err, dispatch.ErrPermission) {
		t.Fatalf("denied RF source admitted: %v", err)
	}
	if !service.allowedRadioGroup(10002, 1001) || service.allowedRadioGroup(999, 1001) {
		t.Fatal("Site confused physical rules with application identity or unknown source")
	}
	system.Talkgroups[0].TransmitRadioIDs = []int{10001}
	set()
	if err := service.broker.authorize(q); err != nil {
		t.Fatal("permitted physical source denied", err)
	}
	system.Talkgroups[0].AffiliationRadioIDs = nil
	system.Talkgroups[0].TransmitAccess = "all"
	set()
	if subscriberGroupAllowed(system, 10001, 1001, true) {
		t.Fatal("TX all bypassed affiliation denial")
	}
	if err := service.broker.authorize(q); !errors.Is(err, dispatch.ErrPermission) {
		t.Fatalf("affiliation-denied source admitted: %v", err)
	}
	system.Talkgroups[0].AffiliationAccess = "all"
	system.Radios[0].Enabled = false
	set()
	if service.allowedRadioGroup(10001, 1001) {
		t.Fatal("global radio disable was bypassed")
	}
}

func TestSubscriberAccessRefusesRawRFBeforeStartingStream(t *testing.T) {
	service, _ := operationFixture(t)
	system := storeFixtureSystem()
	system.Radios = append(system.Radios, Radio{RID: 10002, Kind: "console", Alias: "Desk", Enabled: true})
	system.Talkgroups[0].TransmitAccess = "selected"
	if err := service.Store.WriteCache("system", storeDoc(t, system)); err != nil {
		t.Fatal(err)
	}
	service.refreshDirectory()
	f := sipTestFrame("denied-raw-rf", 10001, 1001, 1, 0)
	if !f.MediaValid() {
		t.Fatal("invalid test frame")
	}
	for _, rid := range []int{10001, 10002, 999} {
		f.SourceID = rid
		service.codecFrame(f)
		q := routing.Request{CallRequest: dispatch.CallRequest{ID: "denied-rf-source", RadioID: rid, Talkgroups: []int{1001}}, SourceKind: "rf", OriginSite: "site-1"}
		if err := service.broker.authorize(q); !errors.Is(err, dispatch.ErrPermission) {
			t.Fatalf("denied, app or unknown RF identity admitted (%d): %v", rid, err)
		}
	}
	service.broker.mu.Lock()
	calls := len(service.broker.rfCalls)
	service.broker.mu.Unlock()
	if calls != 0 {
		t.Fatal("denied physical media started a stream")
	}
}

func TestSubscriberAccessPropagatesAndRemainsCachedWithoutZone(t *testing.T) {
	cluster := integrationNewCluster(t)
	var system SystemConfig
	if err := cluster.system.service.owned(&system); err != nil {
		t.Fatal(err)
	}
	for i := range system.Talkgroups {
		system.Talkgroups[i].AffiliationAccess = "selected"
		system.Talkgroups[i].AffiliationRadioIDs = []int{10001}
		system.Talkgroups[i].TransmitAccess = "selected"
		system.Talkgroups[i].TransmitRadioIDs = []int{}
	}
	if err := cluster.system.service.Store.SaveConfig(storeDoc(t, system)); err != nil {
		t.Fatal(err)
	}
	integrationSyncAll(t, cluster)
	for _, node := range []*integrationService{cluster.zone, cluster.site, cluster.otherSite, cluster.dispatch} {
		var cached SystemConfig
		if !node.service.readCache("system", &cached) {
			t.Fatal("System not distributed")
		}
		for _, g := range cached.Talkgroups {
			if g.AffiliationAccess != "selected" || !reflect.DeepEqual(g.AffiliationRadioIDs, []int{10001}) || g.TransmitAccess != "selected" || len(g.TransmitRadioIDs) != 0 {
				t.Fatalf("subscriber policy lost at %s: %#v", node.service.Config().Role, g)
			}
		}
	}
	// No Zone request participates in this Site decision: retained policy remains
	// authoritative locally after the upstream link is unavailable.
	cluster.zone.server.Close()
	if subscriberGroupAllowed(cluster.site.service.view().System, 10001, 1001, true) || !subscriberGroupAllowed(cluster.site.service.view().System, 10001, 1001, false) {
		t.Fatal("cached Site lost independent receive-only rule")
	}
}

func TestSubscriberAffiliationDeniedDoesNotCreateRemoteRFDemand(t *testing.T) {
	cluster := newMediaCluster(t)
	cluster.affiliations(t, true)
	zone := cluster.zone.service
	q := routing.Request{CallRequest: dispatch.CallRequest{RadioID: 10002, Talkgroups: []int{1001}}, SourceKind: "application", OriginSite: "site-1"}
	if len(zone.broker.targets(q).Targets) != 1 {
		t.Fatal("fixture has no remote RF demand")
	}
	system := zone.view().System
	system.Talkgroups = slices.Clone(system.Talkgroups)
	for i := range system.Talkgroups {
		if system.Talkgroups[i].TGID == 1001 {
			system.Talkgroups[i].AffiliationAccess = "selected"
			system.Talkgroups[i].AffiliationRadioIDs = []int{10001}
		}
	}
	if err := zone.Store.WriteCache("system", storeDoc(t, system)); err != nil {
		t.Fatal(err)
	}
	zone.refreshDirectory()
	if len(zone.broker.targets(q).Targets) != 0 {
		t.Fatal("newly denied retained affiliation allocated remote RF")
	}
	// The accepted registration is not inhibited or erased by talkgroup policy.
	if !slices.ContainsFunc(zone.view().Reports, func(r SiteReport) bool {
		return r.Radio != nil && slices.ContainsFunc(r.Radio.Affiliations, func(a radio.Affiliation) bool { return a.RID == 10003 })
	}) {
		t.Fatal("policy erased the underlying Site observation")
	}
}

func TestSubscriberAccessChangeEndsActiveGoCall(t *testing.T) {
	service, _ := operationFixture(t)
	// Isolate broker revocation from the separately tested native reload worker.
	service.radioReconcile.Lock()
	defer service.radioReconcile.Unlock()
	q := routing.Request{CallRequest: dispatch.CallRequest{ID: "active-rf-acl", RadioID: 10001, Talkgroups: []int{1001}}, SourceKind: "rf", OriginSite: "site-1"}
	q = fixtureLocalRFRequest(t, service, q)
	grant, err := service.broker.Begin(context.Background(), "rf:site-1", q)
	if err != nil {
		t.Fatal(err)
	}
	system := storeFixtureSystem()
	system.Talkgroups[0].TransmitAccess = "selected"
	if err = service.Store.WriteCache("system", storeDoc(t, system)); err != nil {
		t.Fatal(err)
	}
	service.refreshDirectory()
	if err = service.broker.Renew(context.Background(), "rf:site-1", grant); !errors.Is(err, dispatch.ErrPermission) {
		t.Fatalf("changed transmit rule did not refuse renewal: %v", err)
	}
}

func TestSubscriberAuthorizedPatchRelaysToDeniedDirectDestinationInSiteTrunking(t *testing.T) {
	s, desk, _ := operatorFixture(t, RoleSite)
	view := *s.view()
	view.System.Talkgroups = slices.Clone(view.System.Talkgroups)
	for i := range view.System.Talkgroups {
		g := &view.System.Talkgroups[i]
		if g.TGID == 1001 {
			g.AffiliationAccess = "selected"
			g.AffiliationRadioIDs = []int{10001}
			g.TransmitAccess = "selected"
			g.TransmitRadioIDs = []int{10001}
		}
		if g.TGID == 1002 {
			g.AffiliationAccess = "selected"
			g.TransmitAccess = "selected"
		}
	}
	s.directory.value.Store(&view)
	s.peers = newMediaPeers(s)
	defer s.peers.Close()
	s.broker = newCallBroker(s)
	defer s.broker.Close()
	native := &mediaNative{}
	codec := &mediaCodec{frames: make(chan dispatch.Frame, 16)}
	s.rfRouter.Store(siteaudio.New(siteaudio.Options{AllowTX: s.allowedRadioGroup, HasRFInterest: func(int) bool { return true }, Control: func() (radio.Native, error) { return native, nil }, Codec: codec}))
	defer s.rfRouter.Load().Close(context.Background())
	if s.mediaClient != nil {
		t.Fatal("local patch fixture unexpectedly has a Zone connection")
	}
	operatorApply(t, s, "patch", OperatorInput{Action: "activate", ID: "explicit-bridge", Patch: &PatchInput{Name: "Explicit bridge", Talkgroups: []int{1001, 1002}}}, desk)
	s.radio = &operationRadio{status: radio.Status{SiteID: "site-1", State: "running", Affiliations: []radio.Affiliation{{RID: 10001, TGID: 1001, At: time.Now().UTC()}}}}
	q := routing.Request{CallRequest: dispatch.CallRequest{ID: "rf-patch", RadioID: 10001, Talkgroups: []int{1001}}, SourceKind: "rf", OriginSite: "site-1"}
	q = fixtureLocalRFRequest(t, s, q)
	grant, err := s.broker.Begin(context.Background(), "rf:site-1", q)
	if err != nil {
		t.Fatal("authorized original group could not use an explicit patch", err)
	}
	if !slices.Equal(grant.Talkgroups, []int{1001, 1002}) || s.rfRouter.Load().Status().RFGrants != 1 {
		t.Fatal("patch did not allocate its local destination", grant, s.rfRouter.Load().Status())
	}
	f := dispatch.Frame{CallID: grant.ID, SourceID: 10001, TalkgroupID: 1001, Sequence: 1, PCM: make([]byte, dispatch.FrameBytes), CapturedAt: time.Now()}
	if err = s.broker.Write(context.Background(), "rf:site-1", grant, f); err != nil {
		t.Fatal("authorized patch media refused", err)
	}
	select {
	case received := <-codec.frames:
		if received.TalkgroupID != 1002 || received.SourceID != 10001 {
			t.Fatal("patch changed source identity or failed to retarget", received)
		}
	default:
		t.Fatal("patch destination received no media")
	}
	if err = s.broker.End(context.Background(), "rf:site-1", grant); err != nil {
		t.Fatal(err)
	}
	for _, forged := range []routing.Request{
		{CallRequest: dispatch.CallRequest{ID: "direct-denied", RadioID: 10001, Talkgroups: []int{1002}}, SourceKind: "rf", OriginSite: "site-1"},
		{CallRequest: dispatch.CallRequest{ID: "forged-origin", RadioID: 10001, Talkgroups: []int{1002}, AuthorizationGroups: []int{1001}}, SourceKind: "rf", OriginSite: "site-1"},
		{CallRequest: dispatch.CallRequest{ID: "forged-expanded", RadioID: 10001, Talkgroups: []int{1001, 1002}, AuthorizationGroups: []int{1001}}, SourceKind: "rf", OriginSite: "site-1"},
	} {
		if _, err = s.broker.Begin(context.Background(), "rf:site-1", forged); !errors.Is(err, dispatch.ErrPermission) {
			t.Fatalf("forged or direct destination admitted (%s): %v", forged.ID, err)
		}
	}
	operatorApply(t, s, "patch", OperatorInput{Action: "end", ID: "explicit-bridge"}, desk)
	q.ID = "after-bridge-ended"
	grant, err = s.broker.Begin(context.Background(), "rf:site-1", q)
	if err != nil {
		t.Fatal(err)
	}
	if !slices.Equal(grant.Talkgroups, []int{1001}) || s.rfRouter.Load().Status().RFGrants != 0 {
		t.Fatal("ended patch continued to bridge denied destination")
	}
	if err = s.broker.End(context.Background(), "rf:site-1", grant); err != nil {
		t.Fatal(err)
	}
}

func TestSubscriberRestrictionsDoNotGrantOrRemoveApplicationUserAccess(t *testing.T) {
	s, desk, _ := operatorFixture(t, RoleZone)
	view := *s.view()
	view.System.Talkgroups = slices.Clone(view.System.Talkgroups)
	for i := range view.System.Talkgroups {
		view.System.Talkgroups[i].AffiliationAccess = "selected"
		view.System.Talkgroups[i].TransmitAccess = "selected"
	}
	s.directory.value.Store(&view)
	b := newCallBroker(s)
	defer b.Close()
	record := view.Users[desk.user.ID]
	q := routing.Request{CallRequest: dispatch.CallRequest{ID: "application-policy", RadioID: 10002, UserID: record.User.ID, Talkgroups: []int{1001}}, SourceKind: "application", Proof: userProof(record.User, record.PasswordHash)}
	if err := b.authorize(q); err != nil {
		t.Fatal("physical deny-all removed an authorized app user", err)
	}
	limited := record
	limited.User = cloneUser(t, record.User)
	limited.User.Talkgroups = []int{1002}
	view.Users = map[string]userRecord{limited.User.ID: limited}
	s.directory.value.Store(&view)
	q.Proof = userProof(limited.User, limited.PasswordHash)
	if err := b.authorize(q); !errors.Is(err, dispatch.ErrPermission) {
		t.Fatalf("physical rules bypassed the app user's own TG permission: %v", err)
	}
}

func TestSubscriberAuthorizedPatchAcrossZoneAndReceivingSite(t *testing.T) {
	c := newMediaCluster(t)
	var system SystemConfig
	if err := c.system.service.owned(&system); err != nil {
		t.Fatal(err)
	}
	for i := range system.Talkgroups {
		g := &system.Talkgroups[i]
		if g.TGID == 1001 {
			g.AffiliationAccess = "selected"
			g.AffiliationRadioIDs = []int{10001}
			g.TransmitAccess = "selected"
			g.TransmitRadioIDs = []int{10001}
		}
		if g.TGID == 1002 {
			g.SiteAccess = "all"
			g.SiteIDs = []string{}
			g.AffiliationAccess = "selected"
			g.AffiliationRadioIDs = []int{10003}
			g.TransmitAccess = "selected"
			g.TransmitRadioIDs = []int{10003}
		}
	}
	if err := c.system.service.Store.SaveConfig(storeDoc(t, system)); err != nil {
		t.Fatal(err)
	}
	// Patch execution rechecks the accepted directory after waiting for its
	// admission lock. Give the fixture user real policy, not a transient grant
	// added only to a previously resolved authority value.
	c.users[0].Permissions = append(c.users[0].Permissions, "patch")
	if err := c.system.service.Store.SaveUser(c.users[0].ID, c.users[0], ""); err != nil {
		t.Fatal(err)
	}
	integrationSyncAll(t, c.integrationCluster)
	states := []SiteReport{}
	for index, node := range []*integrationService{c.site, c.otherSite} {
		id := node.service.Config().InstanceID
		runtime := c.radios[id]
		rid, tg := 10001, 1001
		if index == 1 {
			rid, tg = 10003, 1002
		}
		runtime.mu.Lock()
		runtime.status.Affiliations = []radio.Affiliation{{RID: rid, TGID: tg, At: time.Now()}}
		status := runtime.status
		runtime.mu.Unlock()
		publishFixtureRFState(t, c.zone.service, id, status)
		states = append(states, SiteReport{ID: id, ReceivedAt: time.Now(), Radio: &status})
	}
	zone := c.zone.service
	if err := zone.writeCache("site_reports", states); err != nil {
		t.Fatal(err)
	}
	zone.refreshDirectory()
	record := zone.view().Users[c.users[0].ID]
	actor, err := zone.operatorActor(OperatorActor{UserID: record.User.ID, Proof: userProof(record.User, record.PasswordHash), RadioID: 10002})
	if err != nil {
		t.Fatal(err)
	}
	operatorApply(t, zone, "patch", OperatorInput{Action: "activate", ID: "remote-bridge", Patch: &PatchInput{Name: "Remote bridge", Talkgroups: []int{1001, 1002}}}, actor)
	request := routing.Request{CallRequest: dispatch.CallRequest{ID: "permitted-rf-patched", RadioID: 10001, Talkgroups: []int{1001}}, SourceKind: "rf", OriginSite: "site-1"}
	endpoint, err := c.site.service.voice.Load().discover(context.Background(), c.zone.server.URL, nil)
	if err != nil {
		t.Fatal(err)
	}
	call, err := c.site.service.voice.Load().begin(context.Background(), endpoint, request, false)
	if err != nil {
		t.Fatal("permitted source could not reach remote patch destination", err)
	}
	f := sipTestFrame(call.grant.ID, 10001, 1001, 1, 0)
	if err = call.Write(f); err != nil {
		t.Fatal(err)
	}
	select {
	case frame := <-c.codecs["site-2"].frames:
		if frame.SourceID != 10001 || frame.TalkgroupID != 1002 {
			t.Fatal("remote patch lost source/destination identity", frame)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("authorized patch did not reach the remote Site")
	}
	if c.natives["site-1"].count() != 0 || c.natives["site-2"].count() == 0 {
		t.Fatal("remote patch reflected into source RF or omitted destination RF")
	}
	if err = call.End(context.Background()); err != nil {
		t.Fatal(err)
	}
	forged := request
	forged.ID = "forged-source-group"
	forged.Talkgroups = []int{1002}
	forged.AuthorizationGroups = []int{1001}
	if _, err = c.site.service.voice.Load().begin(context.Background(), endpoint, forged, false); err == nil {
		t.Fatal("untrusted Site origin-group claim bypassed direct destination denial")
	}
	forged = request
	forged.ID = "forged-origin-site"
	forged.OriginSite = "site-2"
	if _, err = c.site.service.voice.Load().begin(context.Background(), endpoint, forged, false); err == nil {
		t.Fatal("source Site claimed another Site's RF origin")
	}
}
