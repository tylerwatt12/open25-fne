package framework

import (
	"bytes"
	"context"
	"net/http"
	"os"
	"reflect"
	"slices"
	"testing"
	"time"

	"open25.local/open25-fne/internal/dispatch"
	"open25.local/open25-fne/internal/radio"
	"open25.local/open25-fne/internal/routing"
)

func reportPublicationFixture(t *testing.T) (*integrationCluster, SiteReport, routing.Request) {
	t.Helper()
	c := integrationNewCluster(t)
	integrationSyncAll(t, c)
	report, err := c.site.service.ownSiteReport()
	if err != nil {
		t.Fatal(err)
	}
	report.Radio = &radio.Status{SiteID: "site-1", State: "running", Affiliations: []radio.Affiliation{{RID: 10001, TGID: 1001, At: time.Now().UTC().Add(-time.Minute)}}}
	report.ObservedAt = time.Now().UTC()
	q := routing.Request{CallRequest: dispatch.CallRequest{ID: "report-publication", RadioID: 10002, Talkgroups: []int{1001}}, SourceKind: "application"}
	return c, report, q
}

func sendPublicationReport(t *testing.T, c *integrationCluster, report SiteReport) error {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	return c.site.service.upstreamRequest(ctx, "POST", "/internal/report", report, nil)
}

func publicationControlUpdate(report SiteReport) rfAuthorityUpdate {
	return rfAuthorityUpdate{SiteID: report.ID, NativeStartedAt: time.Unix(1, 0).UTC(), ObservedAt: time.Now().UTC(), Affiliations: report.Radio.Affiliations, Registrations: report.Radio.Registrations}
}

func sendPublicationAuthority(t *testing.T, c *integrationCluster, sender *Service, update rfAuthorityUpdate) (int, []byte) {
	t.Helper()
	credentials, err := readCredentials(sender.Config().CredentialFile)
	if err != nil {
		t.Fatal(err)
	}
	return integrationHTTP(t, http.DefaultClient, "POST", c.zone.server.URL+"/internal/rf-authority", update, map[string]string{"X-Service-ID": credentials.OutboundID, "Authorization": "Bearer " + credentials.OutboundToken})
}

func publicationTarget(t *testing.T, zone *Service, q routing.Request, want bool) {
	t.Helper()
	got := zone.broker.targets(q).Targets
	if want && (len(got) != 1 || got[0].ID != "site-1") {
		t.Fatalf("accepted control fact did not immediately select site-1: %+v", got)
	}
	if !want && len(got) != 0 {
		t.Fatalf("unconfirmed or withdrawn membership selected RF demand: %+v", got)
	}
}

func TestAcceptedRFAuthorityImmediatelyUpdatesRoutingDemandIndependentlyOfWebReport(t *testing.T) {
	c, report, q := reportPublicationFixture(t)
	zone := c.zone.service
	report.ReceivedAt = time.Now().Add(-5 * time.Second)
	if err := zone.writeCache("site_reports", []SiteReport{report}); err != nil {
		t.Fatal(err)
	}
	zone.refreshDirectory()
	publicationTarget(t, zone, q, false)
	if err := sendPublicationReport(t, c, report); err != nil {
		t.Fatal(err)
	}
	cached := zone.siteReports()
	if len(cached) != 1 || time.Since(cached[0].ReceivedAt) > time.Second {
		t.Fatal("valid web report did not reach its durable cache")
	}
	publicationTarget(t, zone, q, false)
	update := publicationControlUpdate(report)
	if status, raw := sendPublicationAuthority(t, c, c.site.service, update); status != 200 {
		t.Fatalf("authenticated control update rejected: %d %s", status, raw)
	}
	publicationTarget(t, zone, q, true)
	owner, ok := zone.rfAuthority.lookup(10001)
	if !ok || owner != (affiliationOwner{"site-1", 1001, update.Affiliations[0].At}) {
		t.Fatalf("authenticated control update was not durably accepted: %#v %t", owner, ok)
	}
}

func TestRejectedRFAuthorityClaimsCannotPoisonAcceptedOwnership(t *testing.T) {
	c, report, q := reportPublicationFixture(t)
	zone := c.zone.service
	update := publicationControlUpdate(report)
	if status, raw := sendPublicationAuthority(t, c, c.site.service, update); status != 200 {
		t.Fatalf("valid control update rejected: %d %s", status, raw)
	}
	before := zone.rfAuthority.value.Load()
	zone.rfAuthority.mu.Lock()
	desired := cloneRFAuthority(zone.rfAuthority.desired)
	zone.rfAuthority.mu.Unlock()
	durable, err := os.ReadFile(zone.rfAuthority.path)
	if err != nil {
		t.Fatal(err)
	}
	for _, test := range []struct {
		name   string
		sender *Service
		mutate func(*rfAuthorityUpdate)
		status int
	}{
		{"different Site origin", c.site.service, func(u *rfAuthorityUpdate) { u.SiteID = "site-2" }, 403},
		{"Dispatch cannot publish physical facts", c.dispatch.service, func(*rfAuthorityUpdate) {}, 403},
		{"unknown subscriber", c.site.service, func(u *rfAuthorityUpdate) { u.Affiliations[0].RID = 12345 }, 409},
		{"native run after observation", c.site.service, func(u *rfAuthorityUpdate) { u.NativeStartedAt = u.ObservedAt.Add(time.Second) }, 409},
	} {
		t.Run(test.name, func(t *testing.T) {
			u := publicationControlUpdate(report)
			u.Affiliations = append([]radio.Affiliation(nil), u.Affiliations...)
			test.mutate(&u)
			if status, raw := sendPublicationAuthority(t, c, test.sender, u); status != test.status {
				t.Fatalf("invalid physical claim returned %d, want %d: %s", status, test.status, raw)
			}
			zone.rfAuthority.mu.Lock()
			desiredChanged := !reflect.DeepEqual(zone.rfAuthority.desired, desired)
			zone.rfAuthority.mu.Unlock()
			after, err := os.ReadFile(zone.rfAuthority.path)
			if err != nil || zone.rfAuthority.value.Load() != before || desiredChanged || !bytes.Equal(after, durable) {
				t.Fatalf("rejected claim changed accepted, pending, or durable authority: desiredChanged=%t error=%v", desiredChanged, err)
			}
			publicationTarget(t, zone, q, true)
		})
	}
}

func TestEmptyWebReportCannotRevokeAuthorityButExplicitWithdrawalDoes(t *testing.T) {
	c, report, q := reportPublicationFixture(t)
	zone := c.zone.service
	update := publicationControlUpdate(report)
	if status, raw := sendPublicationAuthority(t, c, c.site.service, update); status != 200 {
		t.Fatalf("accepted control membership rejected: %d %s", status, raw)
	}
	publicationTarget(t, zone, q, true)
	empty := report
	empty.Radio = &radio.Status{SiteID: "site-1", State: "running"}
	empty.ObservedAt = time.Now().UTC()
	if err := sendPublicationReport(t, c, empty); err != nil {
		t.Fatal(err)
	}
	publicationTarget(t, zone, q, true)
	if cached := zone.siteReports(); !slices.ContainsFunc(cached, func(r SiteReport) bool { return r.ID == "site-1" && r.Radio != nil && len(r.Radio.Affiliations) == 0 }) {
		t.Fatalf("empty web report was not independently cached: %#v", cached)
	}
	affiliation := update.Affiliations[0]
	update.ObservedAt = time.Now().UTC()
	update.Withdrawals = []rfAuthorityWithdrawal{{RadioID: affiliation.RID, GroupID: affiliation.TGID, AffiliatedAt: affiliation.At, NativeStartedAt: update.NativeStartedAt}}
	if status, raw := sendPublicationAuthority(t, c, c.site.service, update); status != 200 {
		t.Fatalf("explicit accepted tuple withdrawal rejected: %d %s", status, raw)
	}
	publicationTarget(t, zone, q, false)
	if owner, ok := zone.rfAuthority.lookup(10001); !ok || owner != (affiliationOwner{"site-1", 0, affiliation.At}) {
		t.Fatalf("withdrawal did not preserve original-time exclusion: %#v %t", owner, ok)
	}
	report.ObservedAt = time.Now().UTC()
	if err := sendPublicationReport(t, c, report); err != nil {
		t.Fatal(err)
	}
	publicationTarget(t, zone, q, false)
}

func TestRejectedSiteReportCannotReplaceCachedReportOrAcceptedAuthority(t *testing.T) {
	c, report, q := reportPublicationFixture(t)
	zone := c.zone.service
	if status, raw := sendPublicationAuthority(t, c, c.site.service, publicationControlUpdate(report)); status != 200 {
		t.Fatalf("valid control update rejected: %d %s", status, raw)
	}
	if err := sendPublicationReport(t, c, report); err != nil {
		t.Fatal(err)
	}
	admitted, authority := zone.view(), zone.rfAuthority.value.Load()
	before, _, err := zone.Store.ReadCache("site_reports")
	if err != nil {
		t.Fatal(err)
	}
	report.Radio.SiteID = "wrong-site"
	if err := sendPublicationReport(t, c, report); err == nil {
		t.Fatal("mismatched Site web report was accepted")
	}
	after, _, err := zone.Store.ReadCache("site_reports")
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(after, before) || zone.view() != admitted || zone.rfAuthority.value.Load() != authority {
		t.Fatal("rejected web report replaced its cache, directory, or current authority")
	}
	publicationTarget(t, zone, q, true)
}
