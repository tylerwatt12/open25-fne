package activity

import (
	"context"
	"fmt"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"
)

func TestHistoryIndexesPreserveExistingRowsOrderingAndRetention(t *testing.T) {
	path := filepath.Join(t.TempDir(), "activity.db")
	s, err := OpenArchive(path)
	if err != nil {
		t.Fatal(err)
	}
	waitActivityReady(t, s)
	at := time.Now().UTC().Add(-time.Hour).Truncate(time.Second)
	for i := 0; i < 36; i++ {
		e := event(fmt.Sprintf("indexed-%03d", i), "status", fmt.Sprintf("site-%d", i%2), at.Add(time.Duration(i/3)*time.Second))
		if i%3 == 0 {
			e.Type = "call_start"
			e.CallID = fmt.Sprintf("call-%d", i)
			e.Details = map[string]any{"allocation_id": fmt.Sprintf("allocation-%d", i)}
		}
		if err = s.Append(context.Background(), e); err != nil {
			_ = s.Close()
			t.Fatal(err)
		}
	}
	for _, name := range []string{"event_order", "event_site_order", "allocations_site", "positions_time"} {
		if _, err = s.db.Exec("DROP INDEX " + name); err != nil {
			t.Fatal(err)
		}
	}
	queries := []Query{{Limit: 200}, {SiteID: "site-0", Limit: 7}, {SiteID: "site-1", Since: at.Add(2 * time.Second), Until: at.Add(8 * time.Second), Limit: 5}}
	var events [][]Event
	var calls []CallPage
	for _, q := range queries {
		rows, e := s.Events(q)
		if e != nil {
			t.Fatal(e)
		}
		events = append(events, rows)
		page, e := s.CallHistory(q)
		if e != nil {
			t.Fatal(e)
		}
		calls = append(calls, page)
	}
	retention, err := s.Retention(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if err = s.Close(); err != nil {
		t.Fatal(err)
	}
	// Ordinary owner startup adds indexes to the existing current-format store.
	// Original observations, upload acknowledgments and policy stay authoritative.
	s, err = OpenArchive(path)
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	waitActivityReady(t, s)
	for i, q := range queries {
		rows, e := s.Events(q)
		if e != nil || !reflect.DeepEqual(rows, events[i]) {
			t.Fatalf("event scope/order changed for %+v: %v", q, e)
		}
		page, e := s.CallHistory(q)
		if e != nil || !reflect.DeepEqual(page, calls[i]) {
			t.Fatalf("call scope/order changed for %+v: %v", q, e)
		}
	}
	gotRetention, err := s.Retention(context.Background())
	if err != nil || gotRetention != retention {
		t.Fatal("retention policy changed", err)
	}
}

func TestHistoryPlansUseOrderedIndexesForReadsUploadsAndCleanup(t *testing.T) {
	s := activityOpen(t)
	for _, test := range []struct {
		name, query, index string
		args               []any
	}{
		{"all events", "SELECT id,at,label FROM radio_events ORDER BY at DESC,id DESC LIMIT 200", "event_order", nil},
		{"Site events", "SELECT id,at,label FROM radio_events WHERE site_id=? ORDER BY at DESC,id DESC LIMIT 200", "event_site_order", []any{"site-1"}},
		{"event cleanup", "SELECT id FROM radio_events ORDER BY at DESC,id DESC LIMIT -1 OFFSET 100000", "event_order", nil},
		{"location cleanup", "SELECT id FROM position_history ORDER BY fix_at DESC,id DESC LIMIT -1 OFFSET 50000", "positions_time", nil},
		{"Site allocations", "SELECT call_id,talkgroup_id,channel_id FROM call_allocations WHERE site_id=?", "allocations_site", []any{"site-1"}},
	} {
		t.Run(test.name, func(t *testing.T) {
			rows, err := s.db.Query("EXPLAIN QUERY PLAN "+test.query, test.args...)
			if err != nil {
				t.Fatal(err)
			}
			defer rows.Close()
			var details []string
			for rows.Next() {
				var id, parent, unused int
				var detail string
				if err = rows.Scan(&id, &parent, &unused, &detail); err != nil {
					t.Fatal(err)
				}
				details = append(details, detail)
			}
			if err = rows.Err(); err != nil {
				t.Fatal(err)
			}
			plan := strings.Join(details, "; ")
			if !strings.Contains(plan, test.index) || strings.Contains(plan, "USE TEMP B-TREE") {
				t.Fatalf("query still scans/sorts outside its index: %s", plan)
			}
		})
	}
}
