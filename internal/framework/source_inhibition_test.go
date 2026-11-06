package framework

import (
	"context"
	"errors"
	"testing"
	"time"

	"open25.local/open25-fne/internal/dispatch"
	"open25.local/open25-fne/internal/radio"
	"open25.local/open25-fne/internal/routing"
)

func TestInhibitionRevokesPhysicalSourceRenewalMediaAndRetainsPending(t *testing.T) {
	s, _ := operationFixture(t)
	s.operationCancel()
	s.operationWG.Wait()
	q := routing.Request{CallRequest: dispatch.CallRequest{ID: "physical-before-inhibit", RadioID: 10001, Talkgroups: []int{1001}}, SourceKind: "rf", OriginSite: "site-1"}
	q = fixtureLocalRFRequest(t, s, q)
	g, err := s.broker.Begin(context.Background(), "rf:site-1", q)
	if err != nil {
		t.Fatal(err)
	}
	if err = s.cacheRadioInhibition(radioInhibitionPolicy{Desired: []int{10001}}); err != nil {
		t.Fatal(err)
	}
	if err = s.broker.Renew(context.Background(), "rf:site-1", g); !errors.Is(err, dispatch.ErrPermission) {
		t.Fatalf("inhibited source renewed: %v", err)
	}
	f := admissionFrame(g.ID, 1)
	if err = s.broker.Write(context.Background(), "rf:site-1", g, f); !errors.Is(err, dispatch.ErrPermission) {
		t.Fatalf("inhibited source carried media: %v", err)
	}
	s.codecFrame(admissionFrame("raw-inhibited-frame", 1))
	s.broker.mu.Lock()
	streams := len(s.broker.rfCalls)
	s.broker.mu.Unlock()
	if streams != 0 {
		t.Fatal("raw inhibited RF created a media stream")
	}
	if err = s.persistSiteInhibition([]radio.InhibitRestore{{RadioID: 10001, Blocked: true, PendingUninhibit: true}}); err != nil {
		t.Fatal(err)
	}
	if err = s.cacheRadioInhibition(radioInhibitionPolicy{Desired: []int{}}); err != nil {
		t.Fatal(err)
	}
	q.ID = "desired-false-still-pending"
	if _, err = s.broker.Begin(context.Background(), "rf:site-1", q); !errors.Is(err, dispatch.ErrPermission) {
		t.Fatalf("pending re-enable admitted a source: %v", err)
	}
	if err = s.persistSiteInhibition([]radio.InhibitRestore{}); err != nil {
		t.Fatal(err)
	}
	q.ID = "current-owner-cleared"
	g, err = s.broker.Begin(context.Background(), "rf:site-1", q)
	if err != nil {
		t.Fatal("validated actual clear did not reopen admission", err)
	}
	_ = s.broker.End(context.Background(), "rf:site-1", g)
}

type inhibitReservationRouter struct {
	speechTestRouter
	entered chan struct{}
	release <-chan struct{}
}

func (r *inhibitReservationRouter) Begin(ctx context.Context, q dispatch.CallRequest) (dispatch.Grant, error) {
	close(r.entered)
	select {
	case <-r.release:
	case <-ctx.Done():
		return dispatch.Grant{}, ctx.Err()
	}
	return r.speechTestRouter.Begin(ctx, q)
}

func TestInhibitionDuringBrokerReservationCannotCommitReturnedSourceCall(t *testing.T) {
	s, _ := operationFixture(t)
	s.operationCancel()
	s.operationWG.Wait()
	release := make(chan struct{})
	target := &inhibitReservationRouter{entered: make(chan struct{}), release: release}
	_ = s.broker.router.Close()
	s.broker.router = routing.New(routing.Options{Authorize: s.broker.authorize, Policy: func(routing.Request) routing.Policy {
		return routing.Policy{StartMode: routing.AllStart, Wait: time.Second}
	}, Targets: func(routing.Request) routing.Selection {
		return routing.Selection{Targets: []routing.Target{{ID: "remote-site", Router: target, Groups: []int{1001}}}}
	}})
	result := make(chan error, 1)
	go func() {
		_, err := s.broker.Begin(context.Background(), "rf:site-1", fixtureLocalRFRequest(t, s, routing.Request{CallRequest: dispatch.CallRequest{ID: "late-source-inhibit", RadioID: 10001, Talkgroups: []int{1001}}, SourceKind: "rf", OriginSite: "site-1"}))
		result <- err
	}()
	select {
	case <-target.entered:
	case <-time.After(time.Second):
		t.Fatal("source did not enter reservation")
	}
	if err := s.cacheRadioInhibition(radioInhibitionPolicy{Desired: []int{10001}}); err != nil {
		t.Fatal(err)
	}
	close(release)
	select {
	case err := <-result:
		if err == nil {
			t.Fatal("late inhibition returned a committed source grant")
		}
	case <-time.After(2 * time.Second):
		t.Fatal("blocked reservation did not retire")
	}
	if len(s.broker.router.Active()) != 0 {
		t.Fatal("late-inhibited broker reservation retained its floor")
	}
}

func TestPhysicalInhibitionDoesNotRemoveTrustedDeskIdentity(t *testing.T) {
	s, desk, _ := operatorFixture(t, RoleZone)
	if err := s.cacheRadioInhibition(radioInhibitionPolicy{Desired: []int{10001}}); err != nil {
		t.Fatal(err)
	}
	b := newCallBroker(s)
	defer b.Close()
	record := s.view().Users[desk.user.ID]
	q := routing.Request{CallRequest: dispatch.CallRequest{ID: "desk-with-inhibited-subscriber", RadioID: 10002, UserID: record.User.ID, Talkgroups: []int{1001}}, SourceKind: "application", Proof: userProof(record.User, record.PasswordHash)}
	if err := b.authorize(q); err != nil {
		t.Fatal("physical inhibition removed authorized desk access", err)
	}
	q.SourceKind = "rf"
	q.RadioID = 10001
	q.UserID = ""
	q.Proof = ""
	q.OriginSite = "site-1"
	if err := b.authorize(q); !errors.Is(err, dispatch.ErrPermission) {
		t.Fatalf("inhibited physical identity admitted: %v", err)
	}
}

func TestInhibitionDuringNativeApprovalRejectsLateCommitAndMedia(t *testing.T) {
	c := newMediaCluster(t)
	c.affiliations(t, true)
	s := c.site.service
	release, entered := make(chan struct{}), make(chan struct{})
	native := &admissionNative{record: admissionRecord(), approveEntered: entered, approveWait: release}
	m := &rfAdmissionManager{s: s, entries: map[string]*admittedRF{}, aliases: map[string]string{}, seen: map[string]time.Time{}}
	s.rfAdmissions = m
	t.Cleanup(func() {
		select {
		case <-release:
		default:
			close(release)
		}
		for _, e := range m.snapshot() {
			m.stop(e)
		}
		m.wg.Wait()
	})
	m.observe(native, native.record)
	select {
	case <-entered:
	case <-time.After(3 * time.Second):
		t.Fatal("approval did not reach native owner")
	}
	if err := s.cacheRadioInhibition(radioInhibitionPolicy{Desired: []int{10001}}); err != nil {
		t.Fatal(err)
	}
	close(release)
	m.wg.Wait()
	native.mu.Lock()
	state := native.record.State
	native.mu.Unlock()
	if state != "rejected" {
		t.Fatal("late native approval survived physical inhibition", state)
	}
	if _, ok := m.bind(admissionFrame("late-inhibited-stream", 1)); ok {
		t.Fatal("inhibited source bound late media")
	}
	mediaWait(t, time.Second, func() bool {
		return len(s.broker.router.Active()) == 0 && len(c.zone.service.broker.router.Active()) == 0
	}, "inhibited late approval routes released")
}
