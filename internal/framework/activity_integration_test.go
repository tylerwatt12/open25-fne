package framework

import (
	"context"
	"testing"
	"time"

	"open25.local/open25-fne/internal/activity"
)

func TestSiteForwardsDispatchActivityToCentralHistoryWithoutClaimingRF(t *testing.T) {
	c := integrationNewCluster(t)
	integrationSyncAll(t, c)
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	event := activity.Event{ID: "fallback-call", At: time.Now().UTC(), Origin: "application-1", RadioID: 10002, TalkgroupID: 1001, CallID: "call-1", Type: "call_start", Audience: "dispatcher", SourceKind: "application"}
	if err := c.dispatch.service.requestAt(ctx, c.site.server.URL, "POST", "/internal/events", map[string]any{"events": []activity.Event{event}}, nil, nil); err != nil {
		t.Fatal(err)
	}
	c.site.service.uploadActivity(ctx)
	c.zone.service.uploadActivity(ctx)
	rows, err := c.system.service.activity.Events(activity.Query{Limit: 10})
	if err != nil {
		t.Fatal(err)
	}
	if len(rows) != 1 || rows[0].ID != event.ID || rows[0].Origin != "application-1" || rows[0].SiteID != "site-1" || rows[0].SourceKind != "application" {
		t.Fatalf("forwarded history lost its actual origin: %+v", rows)
	}
	event.ID, event.Type, event.SourceKind = "forged-radio-presence", "affiliation", "rf"
	if err := c.dispatch.service.requestAt(ctx, c.site.server.URL, "POST", "/internal/events", map[string]any{"events": []activity.Event{event}}, nil, nil); err == nil {
		t.Fatal("Dispatch was allowed to claim physical RF presence")
	}
}
