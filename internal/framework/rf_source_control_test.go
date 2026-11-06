package framework

import (
	"context"
	"open25.local/open25-fne/internal/dispatch"
	"open25.local/open25-fne/internal/radio"
	"open25.local/open25-fne/internal/routing"
	"os"
	"path/filepath"
	"reflect"
	"sync"
	"syscall"
	"testing"
	"time"
)

func rfProtocolFixture(t *testing.T) (*Service, routing.Request, PeerCredential) {
	d := rfStoreTestDocument()
	a, _ := rfStoreTestOpen(t, d)
	system := storeFixtureSystem()
	zone := storeFixtureZone()
	zone.ID = "zone-a"
	zone.Sites = []SiteIdentity{{ID: "site-a"}, {ID: "site-b"}, {ID: "site-c"}}
	s := &Service{config: FileConfig{Role: RoleZone, InstanceID: "zone-a"}, rfAuthority: a}
	s.directory.value.Store(&directorySnapshot{System: system, Zone: &zone})
	s.inhibition.Store(inhibitionSnapshot(radioInhibitionPolicy{Desired: []int{}}, nil))
	s.broker = &callBroker{s: s}
	q := routing.Request{CallRequest: dispatch.CallRequest{ID: "observed-native-source", RadioID: 10001, Talkgroups: []int{1001}}, SourceKind: "rf", OriginSite: "site-a", RFSource: routing.RFSourceObservation{NativeStartedAt: d.NativeRuns["site-a"], ObservedAt: time.Now().UTC(), AffiliatedAt: d.Units[10001].Affiliation.At}}
	return s, q, PeerCredential{Role: RoleSite, InstanceID: "site-a"}
}
func TestRFSourceProtocolRejectsInvalidClaimsWithoutChangingAuthority(t *testing.T) {
	for _, test := range []struct {
		name   string
		mutate func(*routing.Request, *PeerCredential)
	}{
		{"missing", func(q *routing.Request, _ *PeerCredential) { q.RFSource = routing.RFSourceObservation{} }},
		{"stale", func(q *routing.Request, _ *PeerCredential) { q.RFSource.ObservedAt = time.Now().Add(-5 * time.Second) }},
		{"future", func(q *routing.Request, _ *PeerCredential) { q.RFSource.ObservedAt = time.Now().Add(2 * time.Minute) }},
		{"before run", func(q *routing.Request, _ *PeerCredential) { q.RFSource.NativeStartedAt = time.Now() }},
		{"unknown RID", func(q *routing.Request, _ *PeerCredential) { q.RadioID = 20001 }},
		{"other Site", func(q *routing.Request, _ *PeerCredential) { q.OriginSite = "site-b" }},
		{"wrong peer role", func(_ *routing.Request, p *PeerCredential) { p.Role = RoleApplication }},
	} {
		t.Run(test.name, func(t *testing.T) {
			s, q, peer := rfProtocolFixture(t)
			before := s.rfAuthority.value.Load()
			desired := cloneRFAuthority(s.rfAuthority.desired)
			test.mutate(&q, &peer)
			if err := s.validateVoiceSource(peer, q); err == nil {
				t.Fatal("invalid source accepted")
			}
			if s.rfAuthority.value.Load() != before || !reflect.DeepEqual(s.rfAuthority.desired, desired) {
				t.Fatal("invalid claim poisoned valid authority")
			}
		})
	}
}
func TestRFSourceProtocolCommitsMembershipAndRetainsTerminalFences(t *testing.T) {
	s, q, peer := rfProtocolFixture(t)
	if err := s.validateVoiceSource(peer, q); err != nil {
		t.Fatal(err)
	}
	if err := s.acceptRFObservation(context.Background(), q); err != nil {
		t.Fatal(err)
	}
	for _, kind := range []string{"null affiliation", "deregistration"} {
		t.Run(kind, func(t *testing.T) {
			s, q, _ := rfProtocolFixture(t)
			u := rfAuthorityUpdate{SiteID: "site-a", NativeStartedAt: q.RFSource.NativeStartedAt, ObservedAt: time.Now().UTC()}
			fact := []radio.Registration{{RID: q.RadioID, At: time.Now().UTC().Add(-time.Second)}}
			if kind == "null affiliation" {
				u.NullAffiliations = fact
			} else {
				u.Terminals = fact
			}
			if err := s.rfAuthority.apply(context.Background(), u, s.rfAuthorityScope()); err != nil {
				t.Fatal(err)
			}
			owner, ok := s.rfAuthority.lookup(q.RadioID)
			if !ok || owner.SiteID != "site-a" || owner.TGID != 0 {
				t.Fatal("terminal did not retain owner fence", owner, ok)
			}
			if err := s.acceptRFObservation(context.Background(), q); err == nil {
				t.Fatal("old affiliation revived after terminal")
			}
		})
	}
}

func TestRFSourceAuthorityServiceShutdownRetainsLockUntilWriterExits(t *testing.T) {
	system, zone := storeFixtureSystem(), storeFixtureZone()
	node := startIntegrationService(t, t.TempDir(), RoleZone, "zone-1", zone, ServiceCredentials{Peers: map[string]PeerCredential{}}, system, zone)
	s := node.service
	authority := s.rfAuthority
	entered := make(chan struct{})
	release := make(chan struct{})
	var unblock sync.Once
	defer unblock.Do(func() { close(release) })
	authority.write = func(path string, value any) error { close(entered); <-release; return writeRFAuthority(path, value) }
	now := time.Now().UTC()
	update := rfAuthorityUpdate{SiteID: "site-1", NativeStartedAt: now.Add(-time.Minute), ObservedAt: now, Affiliations: []radio.Affiliation{{RID: 10001, TGID: 1001, At: now.Add(-time.Second)}}}
	done := make(chan error, 1)
	go func() { done <- authority.apply(context.Background(), update, s.rfAuthorityScope()) }()
	select {
	case <-entered:
	case <-time.After(time.Second):
		t.Fatal("writer not entered")
	}
	s.shutdownDeadline.Store(time.Now().Add(100 * time.Millisecond).UnixNano())
	started := time.Now()
	err := s.Close()
	if err == nil || time.Since(started) > 500*time.Millisecond {
		t.Fatal("shutdown did not honor its shared deadline", time.Since(started), err)
	}
	select {
	case err = <-done:
		if err == nil {
			t.Fatal("shutdown returned a successful admission")
		}
	case <-time.After(time.Second):
		t.Fatal("shutdown did not cancel admission")
	}
	lock, err := os.OpenFile(filepath.Join(s.Config().StateDir, "service.lock"), os.O_RDWR, 0600)
	if err != nil {
		t.Fatal(err)
	}
	defer lock.Close()
	if err = syscall.Flock(int(lock.Fd()), syscall.LOCK_EX|syscall.LOCK_NB); err == nil {
		t.Fatal("replacement could start before late writer finished")
	}
	unblock.Do(func() { close(release) })
	select {
	case <-authority.done:
	case <-time.After(time.Second):
		t.Fatal("released writer did not exit")
	}
	rfStoreTestWait(t, func() bool { return syscall.Flock(int(lock.Fd()), syscall.LOCK_EX|syscall.LOCK_NB) == nil })
	if state := authority.value.Load(); len(state.document.Units) != 0 {
		t.Fatal("late writer published after service cancellation")
	}
}

func fixtureLocalRFRequest(t *testing.T, s *Service, q routing.Request) routing.Request {
	t.Helper()
	current, err := s.observeRFSource(q)
	if err != nil {
		t.Fatal("current native source fixture", err)
	}
	return current
}

func TestRFSourceProtocolPolicyDeniedCannotStageNewerEvidence(t *testing.T) {
	s, q, peer := rfProtocolFixture(t)
	view := *s.view()
	view.System.Talkgroups = append([]Talkgroup(nil), view.System.Talkgroups...)
	view.System.Talkgroups[0].TransmitAccess = "selected"
	view.System.Talkgroups[0].TransmitRadioIDs = []int{}
	s.directory.value.Store(&view)
	q.RFSource.AffiliatedAt = time.Now().UTC().Add(-time.Millisecond)
	q.RFSource.ObservedAt = time.Now().UTC()
	before := s.rfAuthority.value.Load()
	desired := cloneRFAuthority(s.rfAuthority.desired)
	if err := s.validateVoiceSource(peer, q); err != nil {
		t.Fatal("well-formed peer evidence failed syntax validation", err)
	}
	if err := s.acceptRFObservation(context.Background(), q); err == nil {
		t.Fatal("policy-denied source admitted")
	}
	if s.rfAuthority.value.Load() != before || !reflect.DeepEqual(desired, s.rfAuthority.desired) {
		t.Fatal("policy-denied newer claim poisoned accepted authority")
	}
}
