package activity

import (
	"context"
	"database/sql"
	"fmt"
	"path/filepath"
	"testing"
	"time"
)

func TestHistoryCountsMultiselectAndSiteDestinationsWithoutLosingGroups(t *testing.T) {
	s := activityOpen(t)
	start := time.Now().UTC().Add(-time.Minute)
	for _, site := range []string{"site-1", "site-2"} {
		for _, tg := range []int{1001, 1002} {
			for _, kind := range []string{"call_start", "call_end"} {
				e := event(fmt.Sprintf("%s-%d-%s", site, tg, kind), kind, site, start)
				e.CallID = "one-call"
				e.TalkgroupID = tg
				e.ChannelID = fmt.Sprintf("%s-channel-%d", site, tg)
				if kind == "call_end" {
					e.At = start.Add(5 * time.Second)
					e.Outcome = "completed"
				}
				if err := s.Append(e); err != nil {
					t.Fatal(err)
				}
			}
		}
	}
	page, err := s.CallHistory(Query{})
	if err != nil || len(page.Calls) != 1 || len(page.Calls[0].Destinations) != 4 {
		t.Fatalf("logical call lost a destination: %+v %v", page, err)
	}
	c := page.Calls[0]
	if c.LifetimeMS == nil || *c.LifetimeMS != 5000 || c.SpeechMS != nil {
		t.Fatalf("call lifetime was presented as measured speech: %+v", c)
	}
	for _, d := range c.Destinations {
		if d.AllocatedMS != nil || d.SpeechMS != nil {
			t.Fatal("invented measurements")
		}
	}
	stats, err := s.Statistics(Query{})
	if err != nil || stats.Calls != 1 || stats.SiteDeliveries != 4 || stats.CompletedCalls != 1 || stats.CallLifetimeMS != 5000 || stats.SpeechMS != nil {
		t.Fatalf("inflated call counts: %+v %v", stats, err)
	}
	page, err = s.CallHistory(Query{Restricted: true, Groups: []int{1001}, SiteID: "site-2"})
	if err != nil || len(page.Calls) != 1 || len(page.Calls[0].Destinations) != 1 || page.Calls[0].Destinations[0].TalkgroupID != 1001 || page.Calls[0].Destinations[0].SiteID != "site-2" {
		t.Fatalf("scope leaked other destinations: %+v %v", page, err)
	}
	scoped, err := s.CallHistory(Query{CallID: "one-call", ChannelID: "site-2-channel-1002"})
	if err != nil || len(scoped.Calls) != 1 || len(scoped.Calls[0].Destinations) != 1 || scoped.Calls[0].Destinations[0].ChannelID != "site-2-channel-1002" {
		t.Fatalf("call/channel filter ignored: %+v %v", scoped, err)
	}
	scoped, err = s.CallHistory(Query{CallID: "another-call"})
	if err != nil || len(scoped.Calls) != 0 {
		t.Fatal("call ID filter ignored")
	}
}

func TestHistoryCursorPaginationUsesStableTieBreakerAndDateScope(t *testing.T) {
	s := activityOpen(t)
	at := time.Now().UTC()
	for i := 0; i < 7; i++ {
		e := event(fmt.Sprintf("event-%d", i), "status", "site-1", at)
		if err := s.Append(e); err != nil {
			t.Fatal(err)
		}
	}
	seen := map[string]bool{}
	q := Query{Limit: 2, Since: at.Add(-time.Second), Until: at.Add(time.Second)}
	for round := 0; round < 5; round++ {
		page, err := s.EventHistory(q)
		if err != nil {
			t.Fatal(err)
		}
		for _, e := range page.Events {
			if seen[e.ID] {
				t.Fatal("cursor repeated a row")
			}
			seen[e.ID] = true
		}
		if page.NextCursor == "" {
			break
		}
		q.Cursor = page.NextCursor
	}
	if len(seen) != 7 {
		t.Fatalf("cursor omitted rows: %v", seen)
	}
	if _, err := s.EventHistory(Query{Cursor: "not-a-cursor"}); err == nil {
		t.Fatal("malformed cursor accepted")
	}
	page, err := s.EventHistory(Query{Since: at.Add(time.Second)})
	if err != nil || len(page.Events) != 0 {
		t.Fatal("time filter ignored")
	}
}

func TestLocationHistoryKeepsActualFixesAndReportsStaleness(t *testing.T) {
	s := activityOpen(t)
	at := time.Now().UTC().Add(-10 * time.Minute)
	old := event("old-location-log", "location", "site-1", at)
	old.Details = map[string]any{"fix_at": at}
	if err := s.Append(old); err != nil {
		t.Fatal(err)
	}
	e := event("actual-fix", "location", "site-1", at)
	e.SourceKind = "application"
	e.Details = map[string]any{"fix_at": at, "latitude": 41.5, "longitude": -81.7, "accuracy": 8.5, "scope_id": "zone-1"}
	if err := s.Append(e); err != nil {
		t.Fatal(err)
	}
	if err := s.Append(e); err != nil {
		t.Fatal(err)
	}
	page, err := s.LocationHistory(Query{}, time.Now().UTC())
	if err != nil || len(page.Locations) != 1 {
		t.Fatalf("invented or duplicated a position: %+v %v", page, err)
	}
	p := page.Locations[0]
	if !p.Stale || p.AgeSeconds < 599 || p.Latitude != 41.5 || p.Longitude != -81.7 || p.Accuracy != 8.5 || p.SourceKind != "application" || p.ScopeID != "zone-1" {
		t.Fatalf("position metadata lost: %+v", p)
	}
	page, err = s.LocationHistory(Query{Restricted: true, Groups: []int{1002}}, time.Now())
	if err != nil || len(page.Locations) != 0 {
		t.Fatal("unauthorized location exposed")
	}
	e.ID = "invalid-fix"
	e.Details["latitude"] = 91
	if err = s.Append(e); err == nil {
		t.Fatal("invalid coordinate accepted")
	}
}

func TestHistoryRetentionBoundsRowsAndReportsUndeliveredLoss(t *testing.T) {
	s := activityOpen(t)
	now := time.Now().UTC()
	for i := 0; i < 105; i++ {
		e := event(fmt.Sprintf("retained-%03d", i), "status", "site-1", now.Add(time.Duration(i)*time.Millisecond))
		if err := s.Append(e); err != nil {
			t.Fatal(err)
		}
	}
	old := event("expired", "status", "site-1", now.Add(-31*24*time.Hour))
	if err := s.Append(old); err != nil {
		t.Fatal(err)
	}
	r := defaultRetention
	r.MaxEvents = 100
	if err := s.SaveRetention(context.Background(), r); err != nil {
		t.Fatal(err)
	}
	status, err := s.HistoryStatus(context.Background())
	if err != nil || status.Events != 100 || status.PrunedUnuploaded != 6 || status.PendingUpload != 100 {
		t.Fatalf("retention not bounded or silent loss: %+v %v", status, err)
	}
	entered := Retention{Days: 0, MaxEvents: -1, MaxCalls: 0, MaxLocations: 7, StaleSeconds: 1}
	if err = s.SaveRetention(context.Background(), entered); err != nil {
		t.Fatal(err)
	}
	saved, err := s.Retention(context.Background())
	if err != nil || saved != entered {
		t.Fatal("entered retention settings changed", err)
	}
}

func TestArchiveHasNoUpstreamQueue(t *testing.T) {
	s, err := OpenArchive(filepath.Join(t.TempDir(), "activity.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	if err = s.Append(event("central", "status", "site-1", time.Now())); err != nil {
		t.Fatal(err)
	}
	pending, err := s.Pending(20)
	if err != nil || len(pending) != 0 {
		t.Fatal("central archive reported a nonexistent upstream backlog")
	}
}

func TestExplicitHistoryRebuildPreservesEventsAndUploadAcknowledgments(t *testing.T) {
	path := filepath.Join(t.TempDir(), "activity.db")
	s, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	for _, tg := range []int{1001, 1002} {
		e := event(fmt.Sprint(tg), "call_start", "site-1", time.Now())
		e.TalkgroupID = tg
		e.CallID = "original-call"
		if err = s.Append(e); err != nil {
			t.Fatal(err)
		}
	}
	if err = s.Acknowledge([]string{"1001"}); err != nil {
		t.Fatal(err)
	}
	s.Close()
	db, err := sql.Open("sqlite", path)
	if err != nil {
		t.Fatal(err)
	}
	_, err = db.Exec("CREATE TABLE radio_calls(id TEXT PRIMARY KEY)")
	db.Close()
	if err != nil {
		t.Fatal(err)
	}
	if stale, err := Open(path); err == nil {
		stale.Close()
		t.Fatal("obsolete schema silently used")
	}
	if err = RebuildHistory(path); err != nil {
		t.Fatal(err)
	}
	s, err = Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	page, err := s.CallHistory(Query{})
	if err != nil || len(page.Calls) != 1 || len(page.Calls[0].Destinations) != 2 {
		t.Fatalf("rebuild lost original projections: %+v %v", page, err)
	}
	events, err := s.Events(Query{})
	if err != nil || len(events) != 2 {
		t.Fatal("original evidence lost")
	}
	pending, err := s.Pending(20)
	if err != nil || len(pending) != 1 || pending[0].ID != "1002" {
		t.Fatal("upload acknowledgment lost")
	}
}
