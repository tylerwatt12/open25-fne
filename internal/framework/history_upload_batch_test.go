package framework

import (
	"context"
	"fmt"
	"strings"
	"testing"
	"time"

	"open25.local/open25-fne/internal/activity"
)

func TestHistoryUploadBoundsCommittedBatchesAndRetainsRemainder(t *testing.T) {
	c := integrationNewCluster(t)
	source, destination := c.site.service.activity, c.system.service.activity
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	for _, store := range []*activity.Store{source, destination} {
		if err := store.Flush(ctx); err != nil {
			t.Fatal(err)
		}
	}
	policy, err := destination.Retention(ctx)
	if err != nil {
		t.Fatal(err)
	}
	events := make([]activity.Event, 65)
	for i := range events {
		events[i] = activity.Event{ID: fmt.Sprintf("bounded-upload-%03d", i), Origin: "site-1", SiteID: "site-1", Type: "bounded_upload", SourceKind: "rf"}
	}
	for _, event := range events {
		if !source.Enqueue(event) {
			t.Fatal("outbox capture rejected")
		}
	}
	if err := source.Flush(ctx); err != nil {
		t.Fatal(err)
	}
	for _, wantRemaining := range []int{33, 1, 0} {
		c.site.service.uploadActivity(ctx)
		pending, err := source.Pending(ctx, 200)
		if err != nil || len(pending) != min(wantRemaining, activity.EventBatchLimit) {
			t.Fatalf("upload pending page is not bounded: count=%d error=%v", len(pending), err)
		}
		if status := source.IngestionHealth(); status.OutboxPending != int64(wantRemaining) {
			t.Fatalf("upload left %d pending observations; want %d", status.OutboxPending, wantRemaining)
		}
		committed, err := destination.Events(activity.Query{Context: ctx, Type: "bounded_upload", Limit: 100})
		if err != nil || len(committed) != len(events)-wantRemaining {
			t.Fatalf("source ACK did not match destination's committed batch: count=%d err=%v", len(committed), err)
		}
		seen := make(map[string]bool, len(committed))
		for _, event := range committed {
			if seen[event.ID] || event.Origin != "site-1" || event.SiteID != "site-1" {
				t.Fatal("upload duplicated or changed the owning observation")
			}
			seen[event.ID] = true
		}
	}
	if after, err := destination.Retention(ctx); err != nil || after != policy {
		t.Fatalf("smaller upload batches changed retention: %+v %v", after, err)
	}
}

func TestHistoryTransportRejectsOversizedBatchAndRetainsWholeSource(t *testing.T) {
	c := integrationNewCluster(t)
	source, destination := c.site.service.activity, c.system.service.activity
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	for _, store := range []*activity.Store{source, destination} {
		if err := store.Flush(ctx); err != nil {
			t.Fatal(err)
		}
	}
	events := make([]activity.Event, activity.EventBatchLimit+1)
	for i := range events {
		events[i] = activity.Event{ID: fmt.Sprintf("oversized-upload-%03d", i), Origin: "site-1", SiteID: "site-1", Type: "oversized_upload", SourceKind: "rf"}
	}
	for _, event := range events {
		if !source.Enqueue(event) {
			t.Fatal("outbox capture rejected")
		}
	}
	if err := source.Flush(ctx); err != nil {
		t.Fatal(err)
	}
	var reply struct {
		IDs []string `json:"ids"`
	}
	if err := c.site.service.upstreamRequest(ctx, "POST", "/internal/events", map[string]any{"events": events}, &reply); err == nil || !strings.Contains(err.Error(), "HTTP 400") {
		t.Fatalf("oversized transport batch did not fail: %v", err)
	}
	if len(reply.IDs) != 0 {
		t.Fatal("oversized transport batch received an acknowledgment")
	}
	if rows, err := destination.Events(activity.Query{Context: ctx, Type: "oversized_upload", Limit: 100}); err != nil || len(rows) != 0 {
		t.Fatalf("rejected transport batch partially committed: count=%d err=%v", len(rows), err)
	}
	if status := source.IngestionHealth(); status.OutboxPending != int64(len(events)) {
		t.Fatalf("rejected transport batch lost source observations: %d", status.OutboxPending)
	}
	// The current publisher retries retained observations in ordinary batches.
	for range 2 {
		c.site.service.uploadActivity(ctx)
	}
	if pending, err := source.Pending(ctx, 100); err != nil || len(pending) != 0 {
		t.Fatalf("ordinary retry did not durably acknowledge every observation: count=%d err=%v", len(pending), err)
	}
	if rows, err := destination.Events(activity.Query{Context: ctx, Type: "oversized_upload", Limit: 100}); err != nil || len(rows) != len(events) {
		t.Fatalf("ordinary retry changed retained observations: count=%d err=%v", len(rows), err)
	}
}
