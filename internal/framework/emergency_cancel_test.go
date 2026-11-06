package framework

import (
	"testing"

	"open25.local/open25-fne/internal/radio"
)

func TestAcceptedRFEmergencyCancelSurvivesGroupRemoval(t *testing.T) {
	s, _, _ := operatorFixture(t, RoleSite)
	if err := s.observeRFEmergency(10001, 1001, true); err != nil {
		t.Fatal(err)
	}
	// Provisioning can change between the alarm and its own-SU cancellation.
	s.view().System.Talkgroups = nil
	e := radio.ParseEvent("I: (RF) P25, TSDU (Trunking System Data Unit), Emergency Alarm Cancel Accepted, srcId = 10001, dstId = 1001")
	if e == nil {
		t.Fatal("native acceptance was lost")
	}
	s.radioEvent(*e)
	incidents := s.operatorSnapshot().Emergencies
	if len(incidents) != 1 || incidents[0].ClearedAt.IsZero() || incidents[0].ClearedBy != "radio" || s.activeEmergency(10001, 1001) {
		t.Fatalf("accepted own cancellation did not end its original incident: %+v", incidents)
	}
	clearedAt := incidents[0].ClearedAt
	s.radioEvent(*e)
	incidents = s.operatorSnapshot().Emergencies
	if len(incidents) != 1 || incidents[0].ClearedAt != clearedAt {
		t.Fatal("cancellation retry changed historical completion")
	}
}

func TestRFEmergencyCancelCannotCreateOrClearOtherIncidents(t *testing.T) {
	s, _, mobile := operatorFixture(t, RoleSite)
	operatorApply(t, s, "emergency", OperatorInput{Action: "declare", ID: "app-emergency", TalkgroupID: 1001}, mobile)
	if err := s.observeRFEmergency(10001, 1001, true); err != nil {
		t.Fatal(err)
	}
	for _, pair := range [][2]int{{10001, 1002}, {10003, 1001}, {10004, 1001}} {
		if err := s.observeRFEmergency(pair[0], pair[1], false); err != nil {
			t.Fatal(err)
		}
	}
	incidents := s.operatorSnapshot().Emergencies
	if len(incidents) != 2 || !s.activeEmergency(10001, 1001) || !s.activeEmergency(10003, 1001) {
		t.Fatalf("unrelated cancellation changed incident state: %+v", incidents)
	}
	for _, incident := range incidents {
		if !incident.ClearedAt.IsZero() {
			t.Fatal("RF cancellation cleared an unrelated/application incident")
		}
	}
	for _, pair := range [][2]int{{0, 1001}, {0xFFFFFC, 1001}, {10001, 0}, {10001, 65535}} {
		if err := s.observeRFEmergency(pair[0], pair[1], false); err == nil {
			t.Fatalf("invalid identity/group accepted: %v", pair)
		}
	}
}
