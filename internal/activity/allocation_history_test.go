package activity

import (
	"context"
	"path/filepath"
	"testing"
	"time"
)

func TestAllocationHistoryKeepsRejoinedDestinationOpenAfterLateOldEnd(t *testing.T) {
	s := activityOpen(t)
	at := time.Now().UTC().Add(-time.Minute)
	appendAllocation := func(id, allocation, kind string, seconds int) {
		t.Helper()
		e := event(id, kind, "site-2", at.Add(time.Duration(seconds)*time.Second))
		e.CallID = "one-conversation"
		e.ChannelID = allocation + "-channel"
		e.Details = map[string]any{"allocation_id": allocation}
		if err := s.Append(context.Background(), e); err != nil {
			t.Fatal(err)
		}
	}
	appendAllocation("first-start", "first-grant", "call_start", 0)
	appendAllocation("first-end", "first-grant", "call_end", 3)
	appendAllocation("second-start", "second-grant", "call_start", 5)
	// Another end observation for the old allocation arrives after the new
	// allocation's start. Its occurred time can even be later than that start.
	appendAllocation("late-first-end", "first-grant", "call_end", 6)
	page, err := s.CallHistory(Query{})
	if err != nil || len(page.Calls) != 1 || len(page.Calls[0].Destinations) != 1 || page.Calls[0].EndedAt != nil || page.Calls[0].Destinations[0].EndedAt != nil {
		t.Fatalf("old grant ended the rejoined destination: %#v %v", page, err)
	}
	appendAllocation("second-end", "second-grant", "call_end", 9)
	// A delayed start for the first allocation cannot reopen either grant.
	appendAllocation("late-first-start", "first-grant", "call_start", 0)
	page, err = s.CallHistory(Query{})
	if err != nil || len(page.Calls) != 1 || page.Calls[0].LifetimeMS == nil || *page.Calls[0].LifetimeMS != 9000 || page.Calls[0].EndedAt == nil {
		t.Fatalf("ended allocation history was not monotonic: %#v %v", page, err)
	}
	stats, err := s.Statistics(Query{})
	if err != nil || stats.Calls != 1 || stats.SiteDeliveries != 1 || stats.CompletedCalls != 1 {
		t.Fatalf("rejoined allocation counted as another conversation: %#v %v", stats, err)
	}
	var allocations int
	if err := s.db.QueryRow("SELECT COUNT(*) FROM call_allocations").Scan(&allocations); err != nil || allocations != 2 {
		t.Fatal("one-use allocation facts were not retained", allocations, err)
	}
	filtered, err := s.CallHistory(Query{ChannelID: "first-grant-channel"})
	if err != nil || len(filtered.Calls) != 1 || len(filtered.Calls[0].Destinations) != 1 || filtered.Calls[0].Destinations[0].ChannelID != "first-grant-channel" || filtered.Calls[0].Destinations[0].EndedAt == nil || !filtered.Calls[0].Destinations[0].EndedAt.Equal(at.Add(6*time.Second)) {
		t.Fatalf("rejoin on a different channel hid the earlier allocation history: %#v %v", filtered, err)
	}
}

func TestOfflineAllocationProjectionRebuildPreservesOriginalEvents(t *testing.T) {
	path := filepath.Join(t.TempDir(), "activity.db")
	s, err := OpenArchive(path)
	if err != nil {
		t.Fatal(err)
	}
	waitActivityReady(t, s)
	at := time.Now().UTC().Add(-time.Minute)
	for i, row := range []struct{ allocation, kind string }{{"first", "call_start"}, {"first", "call_end"}, {"second", "call_start"}, {"second", "call_end"}} {
		id := row.allocation + "-" + row.kind
		e := event(id, row.kind, "site-1", at.Add(time.Duration(i)*time.Second))
		e.CallID = "original-conversation"
		e.Details = map[string]any{"allocation_id": row.allocation}
		if err = s.Append(context.Background(), e); err != nil {
			t.Fatal(err)
		}
	}
	if _, err = s.db.Exec("DELETE FROM call_allocations"); err != nil {
		t.Fatal(err)
	}
	if err = s.Close(); err != nil {
		t.Fatal(err)
	}
	damaged, err := OpenArchive(path)
	if err != nil {
		t.Fatal(err)
	}
	waitActivityReady(t, damaged)
	var beforeRebuild int
	if err = damaged.db.QueryRow("SELECT COUNT(*) FROM call_allocations").Scan(&beforeRebuild); err != nil || beforeRebuild != 0 {
		t.Fatal("ordinary opening replayed allocation history", beforeRebuild, err)
	}
	if err = damaged.Close(); err != nil {
		t.Fatal(err)
	}
	if err = RebuildHistory(path); err != nil {
		t.Fatal(err)
	}
	reopened, err := OpenArchive(path)
	if err != nil {
		t.Fatal(err)
	}
	defer reopened.Close()
	waitActivityReady(t, reopened)
	page, err := reopened.CallHistory(Query{})
	if err != nil || len(page.Calls) != 1 || page.Calls[0].ID != "original-conversation" || page.Calls[0].LifetimeMS == nil || *page.Calls[0].LifetimeMS != 3000 {
		t.Fatalf("offline projection lost original allocation facts: %#v %v", page, err)
	}
	var originalCount, allocations int
	if err = reopened.db.QueryRow("SELECT COUNT(*) FROM radio_events").Scan(&originalCount); err != nil || originalCount != 4 {
		t.Fatal("rebuild changed original observations", originalCount, err)
	}
	if err = reopened.db.QueryRow("SELECT COUNT(*) FROM call_allocations").Scan(&allocations); err != nil || allocations != 2 {
		t.Fatal("rebuild did not restore one-use allocations", allocations, err)
	}
}

func TestCurrentSchemaInitializationDoesNotBackfillDerivedFacts(t *testing.T) {
	s := activityOpen(t)
	e := event("original", "call_start", "site-1", time.Now())
	e.CallID = "kept-conversation"
	if err := s.Append(context.Background(), e); err != nil {
		t.Fatal(err)
	}
	if _, err := s.db.Exec("DELETE FROM call_allocations"); err != nil {
		t.Fatal(err)
	}
	if err := historySchema(s.db); err != nil {
		t.Fatal(err)
	}
	var originals int
	if err := s.db.QueryRow("SELECT COUNT(*) FROM radio_events WHERE id='original'").Scan(&originals); err != nil || originals != 1 {
		t.Fatal("initializing the current schema modified original observations")
	}
	var allocations int
	if err := s.db.QueryRow("SELECT COUNT(*) FROM call_allocations").Scan(&allocations); err != nil || allocations != 0 {
		t.Fatal("schema initialization backfilled derived allocation facts", allocations, err)
	}
}
