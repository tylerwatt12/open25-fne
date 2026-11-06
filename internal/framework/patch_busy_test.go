package framework

import (
	"context"
	"open25.local/open25-fne/internal/dispatch"
	"open25.local/open25-fne/internal/routing"
	"testing"
)

func TestPatchChangesWaitForCurrentCallToFinish(t *testing.T) {
	s, desk, _ := operatorFixture(t, RoleZone)
	s.peers = newMediaPeers(s)
	s.broker = newCallBroker(s)
	t.Cleanup(func() { s.broker.Close(); s.peers.Close() })
	begin := func(id string) dispatch.Grant {
		t.Helper()
		g, err := s.broker.Begin(context.Background(), "application-1", routing.Request{CallRequest: dispatch.CallRequest{ID: id, UserID: desk.actor.UserID, RadioID: desk.actor.RadioID, Talkgroups: []int{1001}}, SourceKind: "application", Proof: desk.actor.Proof})
		if err != nil {
			t.Fatal(err)
		}
		return g
	}
	patch := OperatorInput{ID: "idle-only-patch", Action: "activate", Patch: &PatchInput{Name: "Test patch", Talkgroups: []int{1001, 1002}}}
	first := begin("before-patch")
	if _, err := s.applyOperator("patch", patch, desk); err == nil {
		t.Fatal("patch joined an ongoing call")
	}
	if err := s.broker.End(context.Background(), "application-1", first); err != nil {
		t.Fatal(err)
	}
	if _, err := s.applyOperator("patch", patch, desk); err != nil {
		t.Fatal(err)
	}
	next := begin("patched-call")
	if len(next.Talkgroups) != 2 {
		t.Fatal("patch was not applied to next call")
	}
	if _, err := s.applyOperator("patch", OperatorInput{ID: patch.ID, Action: "end"}, desk); err == nil {
		t.Fatal("patch ended and interrupted an ongoing call")
	}
	if err := s.broker.End(context.Background(), "application-1", next); err != nil {
		t.Fatal(err)
	}
	if _, err := s.applyOperator("patch", OperatorInput{ID: patch.ID, Action: "end"}, desk); err != nil {
		t.Fatal(err)
	}
}
