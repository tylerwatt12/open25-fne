package framework

import (
	"context"
	"encoding/json"
	"testing"

	"open25.local/open25-fne/internal/radio"
)

func TestSiteActualInhibitionSurvivesOmittedDesiredAndProcessRecovery(t *testing.T) {
	s, runtime := operationFixture(t)
	s.operationCancel()
	s.operationWG.Wait()
	if err := s.persistSiteInhibition([]radio.InhibitRestore{{RadioID: 10001, Blocked: true}}); err != nil {
		t.Fatal(err)
	}
	if err := s.cacheRadioInhibition(radioInhibitionPolicy{Desired: []int{}}); err != nil {
		t.Fatal(err)
	}
	if !s.radioServiceInhibited(10001) {
		t.Fatal("omitted desired erased actual Site inhibition")
	}
	p, err := s.profileRadio()
	if err != nil {
		t.Fatal(err)
	}
	if len(p.Inhibition.Desired) != 0 || len(p.Inhibition.Restore) != 1 || !p.Inhibition.Restore[0].Blocked {
		t.Fatalf("restart profile lost pending protection: %+v", p.Inhibition)
	}
	if !p.System.Radios[0].Enabled {
		t.Fatal("operational inhibition changed provisioning Enabled")
	}
	// A new process reads current actual protection, with no operation history.
	restarted := &Service{Store: s.Store, config: FileConfig{Role: RoleSite, InstanceID: "site-1"}}
	restarted.directory.value.Store(s.view())
	_ = restarted.currentRadioInhibition()
	_ = restarted.siteActualInhibition()
	if !restarted.radioServiceInhibited(10001) {
		t.Fatal("new Site process did not recover actual protection")
	}
	if err = s.applySiteInhibitionPolicy(context.Background(), []int{}); err != nil {
		t.Fatal(err)
	}
	if !s.radioServiceInhibited(10001) {
		t.Fatal("passive false choice invented a native acknowledgment")
	}
	runtime.mu.Lock()
	commands := len(runtime.commands)
	runtime.mu.Unlock()
	if commands != 0 {
		t.Fatal("recovering policy replayed operator commands")
	}
	// This callback represents a validated current native owner clear; receipt
	// acceptance and policy omission never call it with invented empty state.
	if err = s.persistSiteInhibition([]radio.InhibitRestore{}); err != nil {
		t.Fatal(err)
	}
	if s.radioServiceInhibited(10001) {
		t.Fatal("current owner actual clear was not published")
	}
	restarted.actualInhibition.Store(nil)
	_ = restarted.siteActualInhibition()
	if restarted.radioServiceInhibited(10001) {
		t.Fatal("durably observed clear was lost on process recovery")
	}
}

func TestSiteActualInhibitionRejectsUnknownAndCorruptRecoveryState(t *testing.T) {
	s, runtime := operationFixture(t)
	s.operationCancel()
	s.operationWG.Wait()
	for _, rows := range [][]radio.InhibitRestore{{{RadioID: 10002, Blocked: true}}, {{RadioID: 424242, Blocked: true}}, {{RadioID: 10001, Blocked: false, PendingUninhibit: true}}, {{RadioID: 10001, Blocked: true}, {RadioID: 10001, Blocked: true}}} {
		if err := s.persistSiteInhibition(rows); err == nil {
			t.Fatalf("unsafe recovery cache accepted: %+v", rows)
		}
	}
	if err := s.Store.WriteCache("radio-inhibition-actual", json.RawMessage(`[{"radio_id":10001,"blocked":false,"pending_uninhibit":true}]`)); err != nil {
		t.Fatal(err)
	}
	s.actualInhibition.Store(nil)
	if !s.radioServiceInhibited(10001) {
		t.Fatal("corrupt actual state opened admission")
	}
	if _, err := s.profileRadio(); err == nil {
		t.Fatal("corrupt actual state became a startup profile")
	}
	if err := s.applySiteInhibitionPolicy(context.Background(), []int{}); err == nil {
		t.Fatal("corrupt recovery state reported enforced")
	}
	if runtime.Status().State != "stopped" {
		t.Fatal("failed recovery protection left RF running")
	}
}

func TestFailedActualClearSaveRetainsAdmissionProtection(t *testing.T) {
	s, _ := operationFixture(t)
	s.operationCancel()
	s.operationWG.Wait()
	if err := s.persistSiteInhibition([]radio.InhibitRestore{{RadioID: 10001, Blocked: true, PendingUninhibit: true}}); err != nil {
		t.Fatal(err)
	}
	if err := s.Store.Close(); err != nil {
		t.Fatal(err)
	}
	if err := s.persistSiteInhibition([]radio.InhibitRestore{}); err == nil {
		t.Fatal("failed durable clear was accepted")
	}
	if !s.radioServiceInhibited(10001) {
		t.Fatal("failed clear save erased immutable admission protection")
	}
}

func TestCancelledCurrentPolicyCallerWithdrawsRFButStaleCallerCannotStopNewerChoice(t *testing.T) {
	s, runtime := operationFixture(t)
	s.operationCancel()
	s.operationWG.Wait()
	if err := s.cacheRadioInhibition(radioInhibitionPolicy{Desired: []int{10001}}); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if err := s.applySiteInhibitionPolicy(ctx, []int{10001}); err == nil {
		t.Fatal("expired accepted policy caller reported success")
	}
	if runtime.Status().State != "stopped" {
		t.Fatal("accepted policy cancellation retained old RF policy")
	}
	runtime.mu.Lock()
	runtime.status.State = "running"
	runtime.mu.Unlock()
	if err := s.cacheRadioInhibition(radioInhibitionPolicy{Desired: []int{}}); err != nil {
		t.Fatal(err)
	}
	if err := s.applySiteInhibitionPolicy(ctx, []int{10001}); err == nil {
		t.Fatal("stale accepted-policy caller reported success")
	}
	if runtime.Status().State != "running" {
		t.Fatal("cancelled stale caller withdrew a newer policy owner")
	}
}
