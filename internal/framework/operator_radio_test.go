package framework

import (
	"open25.local/open25-fne/internal/radio"
	"testing"
	"time"
)

func TestOperatorRadioCheckUsesCurrentAffiliationAndCannotEscalate(t *testing.T) {
	s, native := operationFixture(t)
	native.mu.Lock()
	native.status.State = "running"
	native.status.Affiliations = []radio.Affiliation{{RID: 10001, TGID: 1001, At: time.Now().UTC()}}
	native.mu.Unlock()
	actor := operatorAuthority{actor: OperatorActor{UserID: "person"}, user: User{ID: "person", RadioID: 10002, Talkgroups: []int{1001}, Permissions: []string{"radio_check"}}}
	if _, err := s.operatorRadioCommand(OperatorInput{Action: "radio_check", RadioID: 10001}, actor); err != nil {
		t.Fatal(err)
	}
	for _, action := range []string{"page", "inhibit", "uninhibit", "regroup"} {
		if _, err := s.operatorRadioCommand(OperatorInput{Action: action, RadioID: 10001}, actor); err == nil {
			t.Fatal("command escalation", action)
		}
	}
	actor.user.Talkgroups = []int{1002}
	if _, err := s.operatorRadioCommand(OperatorInput{Action: "radio_check", RadioID: 10001}, actor); err == nil {
		t.Fatal("queried an unauthorized talkgroup's subscriber")
	}
	native.mu.Lock()
	defer native.mu.Unlock()
	if len(native.commands) != 1 || native.commands[0].RID != 10001 {
		t.Fatal("unauthorized command reached radio")
	}
}
