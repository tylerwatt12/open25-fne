package framework

import (
	"context"
	"testing"
	"time"

	"open25.local/open25-fne/internal/activity"
)

func TestExplicitParentIsolationActionsAreDurablyRecordedOnce(t *testing.T) {
	c := integrationNewCluster(t)
	integrationSyncAll(t, c)
	site := c.site.service
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	before := site.activity.IngestionHealth()
	var originalID string
	for _, isolated := range []bool{true, false} {
		// Repeating an already accepted action must create neither another
		// operational transition nor another historical action record.
		for range 2 {
			if err := site.setParentIsolation(ctx, isolated, "administrator"); err != nil {
				t.Fatal(err)
			}
		}
		if err := site.activity.Flush(ctx); err != nil {
			t.Fatal(err)
		}
		pending, err := site.activity.Pending(ctx, activity.EventBatchLimit)
		if err != nil {
			t.Fatal(err)
		}
		actions := []activity.Event{}
		for _, event := range pending {
			if event.Type == "parent_connection" {
				actions = append(actions, event)
			}
		}
		want := 2
		if isolated {
			want = 1
		}
		if len(actions) != want {
			t.Fatalf("accepted action was rejected, duplicated, or not durable: %+v", actions)
		}
		event := actions[len(actions)-1]
		if event.ID == "" || event.At.IsZero() || event.Origin != site.Config().InstanceID || event.SiteID != site.Config().InstanceID || event.Audience != "operator" || event.Severity != "notice" || event.Outcome != "accepted" || event.Details["isolated"] != isolated || event.Details["administrator"] != "administrator" {
			t.Fatalf("parent action lost its accepted identity or fields: %+v", event)
		}
		if isolated {
			originalID = event.ID
			// The unavailable parent cannot acknowledge this accepted observation.
			site.uploadActivity(ctx)
			retained, err := site.activity.Pending(ctx, activity.EventBatchLimit)
			found := false
			for _, row := range retained {
				found = found || row.ID == originalID
			}
			if err != nil || !found {
				t.Fatal("isolation discarded its unacknowledged record", retained, err)
			}
		}
	}
	site.uploadActivity(ctx)
	archived, err := c.system.service.activity.Events(activity.Query{Context: ctx, Type: "parent_connection"})
	if err != nil || len(archived) != 2 {
		t.Fatal("accepted parent actions did not reach System history", archived, err)
	}
	seen := map[bool]bool{}
	for _, event := range archived {
		isolated, ok := event.Details["isolated"].(bool)
		if !ok || seen[isolated] || event.Audience != "operator" || event.Details["administrator"] != "administrator" {
			t.Fatal("archive changed or duplicated the parent actions", archived)
		}
		seen[isolated] = true
		if isolated && event.ID != originalID {
			t.Fatal("retry changed the accepted disconnect event ID")
		}
	}
	after := site.activity.IngestionHealth()
	if after.RejectedObservations != before.RejectedObservations || after.WriteFailures != before.WriteFailures {
		t.Fatal("explicit connection action generated invalid or failed observations", after)
	}
}
