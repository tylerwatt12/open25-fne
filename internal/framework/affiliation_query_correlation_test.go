package framework

import (
	"context"
	"testing"
	"time"

	"open25.local/open25-fne/internal/radio"
)

func pendingAffiliationQuery(t *testing.T) (*Service, radioCommandReceipt) {
	t.Helper()
	s, _ := commandLifecycleFixture(t)
	receipt, err := s.sendSiteRadioCommand(context.Background(), radioCommandInput{ID: "affiliation-query", Action: "affiliation_query", RadioID: 10001}, OperatorActor{Administrator: true}, 1001)
	if err != nil || receipt.State != "sent" || !receipt.AwaitingAcknowledgment || receipt.AttemptedAt.IsZero() {
		t.Fatalf("the query was not pending after send: %+v %v", receipt, err)
	}
	return s, receipt
}

func parsedAffiliationObservation(t *testing.T, receipt radioCommandReceipt, line string) radio.Event {
	t.Helper()
	event := radio.ParseEvent(line)
	if event == nil || event.Kind != "affiliated" || event.Direction != "state" || event.RID != receipt.RadioID {
		t.Fatal("the accepted native observation was not parsed as affiliation state")
	}
	event.ID = "native-observation"
	event.SiteID = receipt.SiteID
	event.At = receipt.AttemptedAt.Add(time.Millisecond)
	return *event
}

func TestAffiliationQueryOrdinaryAffiliationDoesNotCompleteReceipt(t *testing.T) {
	for _, test := range []struct{ name, line string }{
		{"normal group affiliation", "I: (RF) P25, IOSP_GRP_AFF (Group Affiliation Response), anncId = 65534, srcId = 10001, dstId = 1001"},
		{"location registration", "I: (RF) P25, OSP_LOC_REG_RSP (Location Registration Response), srcId = 10001, dstId = 1001"},
		{"native table change", "I: (HOST) P25 Affiliation, group affiliation, srcId = 10001, dstId = 1001"},
	} {
		t.Run(test.name, func(t *testing.T) {
			s, receipt := pendingAffiliationQuery(t)
			s.observeRadioCommand(parsedAffiliationObservation(t, receipt, test.line))
			rows, err := s.Store.radioCommands()
			if err != nil || len(rows) != 1 || rows[0].State != "sent" || !rows[0].AwaitingAcknowledgment || rows[0].ResponseKind != "" || rows[0].ResponseEventID != "" {
				t.Fatalf("ordinary affiliation completed a query: %+v %v", rows, err)
			}
		})
	}
}

func TestAffiliationQueryAcceptedQueryResponseCompletesOnlyStateObservation(t *testing.T) {
	s, receipt := pendingAffiliationQuery(t)
	event := parsedAffiliationObservation(t, receipt, "I: (RF) P25, ISP_GRP_AFF_Q_RSP (Group Affiliation Query Response), srcId = 10001, dstId = 1001, anncId = 65534")
	event.Direction = "received"
	s.observeRadioCommand(event)
	rows, err := s.Store.radioCommands()
	if err != nil || len(rows) != 1 || rows[0].State != "sent" || !rows[0].AwaitingAcknowledgment {
		t.Fatal("an observation outside the accepted-state path completed the query", err)
	}
	event.Direction = "state"
	s.observeRadioCommand(event)
	rows, err = s.Store.radioCommands()
	if err != nil || len(rows) != 1 || rows[0].State != "response_observed" || rows[0].AwaitingAcknowledgment || rows[0].ResponseKind != "affiliated" || rows[0].ResponseEventID != event.ID || !rows[0].ResponseAt.Equal(event.At) {
		t.Fatalf("the accepted query response was not retained: %+v %v", rows, err)
	}
}
