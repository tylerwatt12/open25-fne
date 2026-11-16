package activity

import (
	"context"
	"fmt"
	"reflect"
	"testing"
	"time"
)

func TestPruneBatchesConvergeToExactRetentionSurvivors(t *testing.T) {
	s := activityOpen(t)
	now := time.Now().UTC()
	seed := event("seed", "status", "site-1", now.Add(-time.Hour))
	if err := s.Append(context.Background(), seed); err != nil {
		t.Fatal(err)
	}
	tx, err := s.db.Begin()
	if err != nil {
		t.Fatal(err)
	}
	defer tx.Rollback()
	for i := 0; i < 1200; i++ {
		id := fmt.Sprintf("row-%04d", i)
		at := now.Add(-time.Duration(1200-i/3) * time.Second)
		if i < 100 {
			at = at.Add(-40 * 24 * time.Hour)
		}
		stamp := at.Format(stampFormat)
		statements := []struct {
			sql  string
			args []any
		}{
			{`INSERT INTO radio_events SELECT ?,?,received_at,origin,site_id,channel_id,radio_id,talkgroup_id,call_id,type,label,message,audience,severity,outcome,source_kind,details FROM radio_events WHERE id='seed'`, []any{id, stamp}},
			{`INSERT INTO calls VALUES(?,10001,?,'rf')`, []any{id, stamp}},
			{`INSERT INTO call_destinations VALUES(?,'site-1',1001,'voice',?,NULL,'active',NULL,NULL)`, []any{id, stamp}},
			{`INSERT INTO call_allocations VALUES(?,'site-1',1001,'allocation','voice',?,NULL,'active',NULL,NULL)`, []any{id, stamp}},
			{`INSERT INTO position_history VALUES(?,10001,1001,'site-1','site-1',41,-81,1,?,?,'rf')`, []any{id, stamp, stamp}},
		}
		for _, st := range statements {
			if _, err = tx.Exec(st.sql, st.args...); err != nil {
				t.Fatal(err)
			}
		}
	}
	if _, err = tx.Exec(`DELETE FROM radio_events WHERE id='seed'; UPDATE history_settings SET max_events=500,max_calls=450,max_locations=550`); err != nil {
		t.Fatal(err)
	}
	if err = tx.Commit(); err != nil {
		t.Fatal(err)
	}
	limits, err := s.Retention(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	cutoff := now.Add(-30 * 24 * time.Hour).Format(stampFormat)
	tables := []struct {
		table, column string
		max           int
	}{{"radio_events", "at", 500}, {"calls", "started_at", 450}, {"position_history", "fix_at", 550}}
	expected := map[string][]string{}
	ids := func(query string, args ...any) []string {
		rows, err := s.db.Query(query, args...)
		if err != nil {
			t.Fatal(err)
		}
		defer rows.Close()
		out := []string{}
		for rows.Next() {
			var id string
			if err := rows.Scan(&id); err != nil {
				t.Fatal(err)
			}
			out = append(out, id)
		}
		if err := rows.Err(); err != nil {
			t.Fatal(err)
		}
		return out
	}
	for _, p := range tables {
		// This is the original policy, independent of the batched implementation.
		expected[p.table] = ids(fmt.Sprintf(`SELECT id FROM %s WHERE NOT (%s<? OR id IN (SELECT id FROM %s ORDER BY %s DESC,id DESC LIMIT -1 OFFSET ?)) ORDER BY id`, p.table, p.column, p.table, p.column), cutoff, p.max)
	}
	total := Removed{}
	for i := 0; ; i++ {
		if i > 30 {
			t.Fatal("cleanup did not converge")
		}
		removed, err := s.Prune(context.Background(), now)
		if err != nil {
			t.Fatal(err)
		}
		if removed.Events > 32 || removed.Calls > 32 || removed.Locations > 32 {
			t.Fatal("unbounded cleanup", removed)
		}
		total.Events += removed.Events
		total.Calls += removed.Calls
		total.Locations += removed.Locations
		if removed.Events+removed.Calls+removed.Locations == 0 {
			break
		}
	}
	for _, p := range tables {
		if got := ids("SELECT id FROM " + p.table + " ORDER BY id"); !reflect.DeepEqual(got, expected[p.table]) {
			t.Fatalf("%s survivors changed", p.table)
		}
	}
	for _, table := range []string{"call_destinations", "call_allocations"} {
		if got := ids("SELECT call_id FROM " + table + " ORDER BY call_id"); !reflect.DeepEqual(got, expected["calls"]) {
			t.Fatalf("%s lost surviving children or retained expired children", table)
		}
	}
	status, err := s.HistoryStatus(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if status.Settings != limits {
		t.Fatalf("System retention settings changed: %+v %+v", status, total)
	}
	if err = s.SaveRetention(context.Background(), Retention{}); err != nil {
		t.Fatal(err)
	}
	waitCleanupState(t, s, func(v CleanupStatus) bool { return !v.Pending && !v.Running && v.LastCompletedAt != nil })
	status, err = s.HistoryStatus(context.Background())
	if err != nil || status.Events+status.Calls+status.Locations != 0 {
		t.Fatal("explicit retention save did not complete all batches", status, err)
	}
}

// A writer may spend longer than the SQL deadline in an OS disk sync. WAL
// readers still need the last committed events while that connection is held.
func TestRecentEventsReadCommittedHistoryWhileMaintenanceOwnsWriter(t *testing.T) {
	s := activityOpen(t)
	if err := s.Append(context.Background(), event("committed", "status", "site-1", time.Now().UTC())); err != nil {
		t.Fatal(err)
	}
	tx, err := s.db.Begin()
	if err != nil {
		t.Fatal(err)
	}
	defer tx.Rollback()
	if _, err = tx.Exec(`DELETE FROM radio_events WHERE id='committed'`); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 200*time.Millisecond)
	defer cancel()
	rows, err := s.Events(Query{Context: ctx, Limit: 6})
	if err != nil || len(rows) != 1 || rows[0].ID != "committed" {
		t.Fatal("event read waited for writer or exposed uncommitted deletion", rows, err)
	}
	if err = tx.Commit(); err != nil {
		t.Fatal(err)
	}
	rows, err = s.Events(Query{Context: ctx, Limit: 6})
	if err != nil || len(rows) != 0 {
		t.Fatal("event reader retained a stale snapshot after commit", rows, err)
	}
	if _, err = s.readDB.Exec(`DELETE FROM radio_events`); err == nil {
		t.Fatal("event connection permits writes")
	}
}

func TestUploadBatchCommitsOnceOrRollsBackEveryObservation(t *testing.T) {
	s := activityOpen(t)
	ctx := context.Background()
	a := event("batch-first", "status", "site-1", time.Now().UTC())
	b := event("batch-reject", "status", "site-1", time.Now().UTC())
	if _, err := s.db.Exec(`CREATE TRIGGER reject_batch BEFORE INSERT ON radio_events WHEN NEW.id='batch-reject' BEGIN SELECT RAISE(ABORT,'injected batch failure'); END`); err != nil {
		t.Fatal(err)
	}
	if err := s.Append(ctx, a, b); err == nil {
		t.Fatal("rejected batch succeeded")
	}
	if rows, err := s.Events(Query{Limit: 10}); err != nil || len(rows) != 0 {
		t.Fatal("partially committed upload batch", rows, err)
	}
	if _, err := s.db.Exec(`DROP TRIGGER reject_batch`); err != nil {
		t.Fatal(err)
	}
	if err := s.Append(ctx, a, b); err != nil {
		t.Fatal(err)
	}
	a.Message = "duplicate must not replace committed history"
	if err := s.Append(ctx, a, b); err != nil {
		t.Fatal(err)
	}
	rows, err := s.Events(Query{Limit: 10})
	if err != nil || len(rows) != 2 {
		t.Fatal(rows, err)
	}
	for _, e := range rows {
		if e.Message == a.Message {
			t.Fatal("batch retry rewrote original history")
		}
	}
}
