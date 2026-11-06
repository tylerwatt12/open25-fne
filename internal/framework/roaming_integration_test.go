package framework

import (
	"context"
	"net/http"
	"strings"
	"testing"
	"time"

	"open25.local/open25-fne/internal/activity"
	"open25.local/open25-fne/internal/dispatch"
	"open25.local/open25-fne/internal/radio"
	"open25.local/open25-fne/internal/routing"
)

// These are routing fixtures; no native table or RF device is opened.
func TestZoneRoutesAndConfirmsSourcesOnlyAtNewestAcceptedRoamingOwner(t *testing.T) {
	cluster := integrationNewCluster(t)
	integrationSyncAll(t, cluster)
	zone := cluster.zone.service
	now := time.Now()
	reports := []SiteReport{
		{ID: "site-1", ReceivedAt: now, Radio: &radio.Status{Affiliations: []radio.Affiliation{{RID: 10001, TGID: 1001, At: now.Add(-time.Minute)}}}},
		{ID: "site-2", ReceivedAt: now, Radio: &radio.Status{Affiliations: []radio.Affiliation{{RID: 10001, TGID: 1001, At: now.Add(-30 * time.Second)}}}},
	}
	publish := func() {
		t.Helper()
		if err := zone.writeCache("site_reports", reports); err != nil {
			t.Fatal(err)
		}
		zone.refreshDirectory()
	}
	target := func(want string) {
		t.Helper()
		routes := zone.broker.targets(routing.Request{CallRequest: dispatch.CallRequest{ID: "application-call", RadioID: 10002, Talkgroups: []int{1001}}, SourceKind: "application"}).Targets
		if len(routes) != 1 || routes[0].ID != want {
			t.Fatalf("RF demand was duplicated or assigned to wrong Site: %+v, want %s", routes, want)
		}
	}
	send := func(siteID, id string) int {
		t.Helper()
		q := routing.Request{CallRequest: dispatch.CallRequest{ID: id, RadioID: 10001, Talkgroups: []int{1001}}, SourceKind: "rf", OriginSite: siteID}
		for _, report := range reports {
			if report.ID == siteID {
				q.RFSource = routing.RFSourceObservation{NativeStartedAt: time.Unix(1, 0).UTC(), ObservedAt: time.Now().UTC(), AffiliatedAt: report.Radio.Affiliations[0].At}
			}
		}
		if err := zone.validateVoiceSource(PeerCredential{Role: RoleSite, InstanceID: siteID}, q); err != nil {
			return 403
		}
		if err := zone.acceptRFObservation(context.Background(), q); err != nil {
			return 403
		}
		return 200
	}
	publish()
	for _, report := range reports {
		publishFixtureRFState(t, zone, report.ID, *report.Radio)
	}
	target("site-2")
	if status := send("site-1", "old-site-source"); status != 403 {
		t.Fatalf("old Site remained a current RF source: %d", status)
	}
	if status := send("site-2", "current-site-source"); status != 200 {
		t.Fatalf("new Site source rejected: %d", status)
	}
	zone.broker.CancelAll()
	// Polling an old table again does not make its affiliation newer.
	reports[0].ReceivedAt = time.Now()
	publish()
	target("site-2")
	// A genuine accepted affiliation back at A moves ownership back to A.
	reports[0].Radio.Affiliations[0].At = now.Add(-10 * time.Second)
	reports[0].ReceivedAt = time.Now()
	publish()
	target("site-2")
	publishFixtureRFState(t, zone, "site-1", *reports[0].Radio)
	target("site-1")
	if status := send("site-2", "stale-after-return"); status != 403 {
		t.Fatalf("previous Site remained a source after return: %d", status)
	}
	if status := send("site-1", "returned-source"); status != 200 {
		t.Fatalf("returning Site source rejected: %d", status)
	}
	zone.broker.CancelAll()
}

func TestZoneRetainsDeregistrationAcrossEmptyReportsAndRejectsVirtualPresence(t *testing.T) {
	cluster := integrationNewCluster(t)
	integrationSyncAll(t, cluster)
	zone := cluster.zone.service
	now := time.Now().UTC()
	reports := []SiteReport{
		{ID: "site-1", ReceivedAt: now, Radio: &radio.Status{Affiliations: []radio.Affiliation{{RID: 10001, TGID: 1001, At: now.Add(-time.Minute)}}}},
		{ID: "site-2", ReceivedAt: now, Radio: &radio.Status{}},
	}
	if err := zone.writeCache("site_reports", reports); err != nil {
		t.Fatal(err)
	}
	zone.refreshDirectory()
	publishFixtureRFState(t, zone, "site-1", *reports[0].Radio)
	upload := func(peer, token string, event activity.Event) int {
		t.Helper()
		status, _ := integrationHTTP(t, http.DefaultClient, "POST", cluster.zone.server.URL+"/internal/events", map[string]any{"events": []activity.Event{event}}, map[string]string{"X-Service-ID": peer, "Authorization": "Bearer " + strings.Repeat(token, 64)})
		return status
	}
	event := activity.Event{ID: "accepted-deregistration", At: now.Add(-10 * time.Second), Origin: "site-2", SiteID: "site-2", RadioID: 10001, Type: "deregistration", SourceKind: "rf", Audience: "operator"}
	if status := upload("site-2", "o", event); status != 200 {
		t.Fatalf("Site deregistration report rejected: %d", status)
	}
	if rows := zone.view().Presence; len(rows) != 1 || rows[0].Registered || rows[0].SiteID != "site-2" {
		t.Fatalf("accepted terminal state was not cached from the activity table: %+v", rows)
	}
	request := routing.Request{CallRequest: dispatch.CallRequest{ID: "demand", RadioID: 10002, Talkgroups: []int{1001}}, SourceKind: "application"}
	if targets := zone.broker.targets(request).Targets; len(targets) != 1 || targets[0].ID != "site-1" {
		t.Fatalf("history upload changed independently accepted RF authority: %+v", targets)
	}
	terminal := rfAuthorityUpdate{SiteID: "site-2", NativeStartedAt: time.Unix(1, 0).UTC(), ObservedAt: time.Now().UTC(), Terminals: []radio.Registration{{RID: event.RadioID, At: event.At}}}
	if err := zone.rfAuthority.apply(context.Background(), terminal, zone.rfAuthorityScope()); err != nil {
		t.Fatal(err)
	}
	if targets := zone.broker.targets(request).Targets; len(targets) != 0 {
		t.Fatalf("empty report revived an older Site affiliation: %+v", targets)
	}
	fake := event
	fake.ID, fake.At, fake.Origin, fake.SiteID, fake.Type, fake.TalkgroupID = "virtual-affiliation", now, "application-1", "site-1", "affiliation", 1001
	if status := upload("application-1", "d", fake); status != 403 {
		t.Fatalf("Dispatch fabricated physical ownership: %d", status)
	}
	fake.ID, fake.SourceKind, fake.Type = "virtual-registration", "application", "registration"
	if status := upload("application-1", "d", fake); status != 403 {
		t.Fatalf("Dispatch fabricated presence under application source: %d", status)
	}
	if rows := zone.view().Presence; len(rows) != 1 || rows[0].Registered {
		t.Fatalf("rejected virtual report changed radio presence: %+v", rows)
	}
	// Re-reading the persisted table cannot turn the old report into current demand.
	zone.refreshDirectory()
	if targets := zone.broker.targets(request).Targets; len(targets) != 0 {
		t.Fatalf("ownership changed while refreshing the retained table: %+v", targets)
	}
	// Only a fresh, genuinely accepted affiliation permits the radio to return.
	reports[0].Radio.Affiliations[0].At = now
	if err := zone.writeCache("site_reports", reports); err != nil {
		t.Fatal(err)
	}
	zone.refreshDirectory()
	if targets := zone.broker.targets(request).Targets; len(targets) != 0 {
		t.Fatalf("web report alone replaced accepted terminal ownership: %+v", targets)
	}
	publishFixtureRFState(t, zone, "site-1", *reports[0].Radio)
	if targets := zone.broker.targets(request).Targets; len(targets) != 1 || targets[0].ID != "site-1" {
		t.Fatalf("fresh return was not admitted: %+v", targets)
	}
}
