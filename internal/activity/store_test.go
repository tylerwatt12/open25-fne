package activity

import (
	"context"
	"errors"
	"path/filepath"
	"testing"
	"time"
)

func activityOpen(t *testing.T) *Store {
	t.Helper()
	s, err := Open(filepath.Join(t.TempDir(), "activity.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = s.Close() })
	return s
}
func event(id, kind, site string, at time.Time) Event {
	return Event{ID: id, Origin: "site:" + site, SiteID: site, At: at, Type: kind, Label: kind, Message: "Observed radio activity", Audience: "dispatcher", Severity: "info", RadioID: 10001, TalkgroupID: 1001, SourceKind: "subscriber"}
}

func TestActivityDuplicateUploadDoesNotRewriteOriginalObservation(t *testing.T) {
	s := activityOpen(t)
	e := event("event-1", "affiliation", "site-1", time.Now().Add(-time.Minute))
	if err := s.Append(e); err != nil {
		t.Fatal(err)
	}
	rows, err := s.Events(Query{})
	if err != nil || len(rows) != 1 {
		t.Fatal(err)
	}
	first := rows[0]
	e.Message = "Retry must not replace original"
	e.TalkgroupID = 1002
	if err = s.Append(e); err != nil {
		t.Fatal(err)
	}
	rows, err = s.Events(Query{})
	if err != nil || len(rows) != 1 {
		t.Fatalf("duplicate created rows: %d %v", len(rows), err)
	}
	if rows[0].Message != first.Message || !rows[0].ReceivedAt.Equal(first.ReceivedAt) {
		t.Fatal("retry rewrote the accepted event")
	}
	presence, err := s.Presence()
	if err != nil || len(presence) != 1 || presence[0].AffiliatedTalkgroup != 1001 {
		t.Fatal("duplicate altered presence")
	}
}

func TestActivityPendingSurvivesRestartUntilAcknowledged(t *testing.T) {
	path := filepath.Join(t.TempDir(), "activity.db")
	s, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	for _, id := range []string{"event-1", "event-2", "event-3"} {
		if err = s.Append(event(id, "status", "site-1", time.Now())); err != nil {
			t.Fatal(err)
		}
	}
	if err = s.Acknowledge([]string{"event-2"}); err != nil {
		t.Fatal(err)
	}
	if err = s.Close(); err != nil {
		t.Fatal(err)
	}
	s, err = Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	pending, err := s.Pending(256)
	if err != nil {
		t.Fatal(err)
	}
	if len(pending) != 2 {
		t.Fatalf("wanted two retained uploads, got %d", len(pending))
	}
	seen := map[string]bool{}
	for _, v := range pending {
		seen[v.ID] = true
	}
	if !seen["event-1"] || !seen["event-3"] || seen["event-2"] {
		t.Fatal("wrong retry set")
	}
	if err = s.Acknowledge([]string{"event-1", "event-3", "event-1"}); err != nil {
		t.Fatal(err)
	}
	pending, err = s.Pending(256)
	if err != nil || len(pending) != 0 {
		t.Fatal("acknowledged events retried")
	}
	rows, err := s.Events(Query{})
	if err != nil || len(rows) != 3 {
		t.Fatal("acknowledgement removed central history")
	}
}

func TestActivityOneCallKeepsIndependentSiteLegs(t *testing.T) {
	s := activityOpen(t)
	start := time.Now().Add(-time.Minute)
	for _, site := range []string{"site-1", "site-2"} {
		e := event("start-"+site, "call_start", site, start)
		e.CallID = "call-1"
		if err := s.Append(e); err != nil {
			t.Fatal(err)
		}
	}
	end := event("end-site-1", "call_end", "site-1", start.Add(time.Second))
	end.CallID = "call-1"
	end.Outcome = "completed"
	if err := s.Append(end); err != nil {
		t.Fatal(err)
	}
	rows, err := s.Calls()
	if err != nil || len(rows) != 1 || len(rows[0].Destinations) != 2 {
		t.Fatalf("wanted one call with two Site deliveries: %#v %v", rows, err)
	}
	unique := map[string]bool{}
	active := 0
	for _, c := range rows {
		unique[c.ID] = true
		for _, d := range c.Destinations {
			if d.EndedAt == nil {
				active++
				if d.SiteID != "site-2" {
					t.Fatal("wrong remaining Site leg")
				}
			}
		}
	}
	if len(unique) != 1 || active != 1 {
		t.Fatal("one conversation became multiple calls or ended both Site legs")
	}
}

func TestActivityLateUploadCannotRegressPresence(t *testing.T) {
	s := activityOpen(t)
	now := time.Now()
	newer := event("new-affiliation", "affiliation", "site-1", now)
	newer.TalkgroupID = 1002
	if err := s.Append(newer); err != nil {
		t.Fatal(err)
	}
	older := event("old-affiliation", "affiliation", "site-1", now.Add(-time.Hour))
	if err := s.Append(older); err != nil {
		t.Fatal(err)
	}
	rows, err := s.Presence()
	if err != nil || len(rows) != 1 {
		t.Fatal(err)
	}
	if rows[0].AffiliatedTalkgroup != 1002 || !rows[0].LastSeen.Equal(now) {
		t.Fatal("a reconnect upload replaced current presence with an older affiliation")
	}
	oldOff := event("old-deregistration", "deregistration", "site-1", now.Add(-time.Minute))
	if err = s.Append(oldOff); err != nil {
		t.Fatal(err)
	}
	rows, _ = s.Presence()
	if !rows[0].Registered {
		t.Fatal("old deregistration cleared current registration")
	}
}

func TestActivityCallEndBeforeStartDoesNotLeavePhantomActiveCall(t *testing.T) {
	s := activityOpen(t)
	now := time.Now()
	end := event("call-end", "call_end", "site-1", now)
	end.CallID = "reordered-call"
	end.Outcome = "completed"
	if err := s.Append(end); err != nil {
		t.Fatal(err)
	}
	start := event("call-start", "call_start", "site-1", now.Add(-5*time.Second))
	start.CallID = end.CallID
	if err := s.Append(start); err != nil {
		t.Fatal(err)
	}
	rows, err := s.Calls()
	if err != nil || len(rows) != 1 {
		t.Fatal(err)
	}
	if rows[0].EndedAt == nil || !rows[0].EndedAt.Equal(end.At) || !rows[0].StartedAt.Equal(start.At) {
		t.Fatal("reordered upload created a permanently active or incorrectly timed call")
	}
}

func TestActivityRestrictedQueryContainsOnlyPermittedOperatorEvents(t *testing.T) {
	s := activityOpen(t)
	now := time.Now()
	records := []Event{event("allowed", "emergency", "site-1", now), event("other-group", "status", "site-1", now), event("diagnostic", "broadcast", "site-1", now), event("security", "login", "site-1", now)}
	records[1].TalkgroupID = 1002
	records[2].Audience = "diagnostic"
	records[3].Audience = "security"
	for _, e := range records {
		if err := s.Append(e); err != nil {
			t.Fatal(err)
		}
	}
	rows, err := s.Events(Query{Restricted: true, Groups: []int{1001}})
	if err != nil || len(rows) != 1 || rows[0].ID != "allowed" {
		t.Fatalf("restricted view leaked or hid events: %#v %v", rows, err)
	}
	rows, err = s.Events(Query{Restricted: true, Groups: []int{}})
	if err != nil || len(rows) != 0 {
		t.Fatal("empty grants exposed activity")
	}
}

func TestActivityQueueDrainsOnCloseAndRejectsNewEvents(t *testing.T) {
	path := filepath.Join(t.TempDir(), "activity.db")
	s, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	for i := 0; i < 128; i++ {
		if !s.Enqueue(event(NewID(), "status", "site-1", time.Now())) {
			t.Fatal("normal queue unexpectedly rejected observation")
		}
	}
	if err = s.Close(); err != nil {
		t.Fatal(err)
	}
	if s.Enqueue(event("after-close", "status", "site-1", time.Now())) {
		t.Fatal("closed queue accepted event")
	}
	s, err = Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	rows, err := s.Events(Query{Limit: 1000})
	if err != nil || len(rows) != 128 {
		t.Fatalf("accepted observations lost during close: %d %v", len(rows), err)
	}
}

func TestActivityFlushWaitsForInflightCommit(t *testing.T) {
	s := activityOpen(t)
	tx, err := s.db.Begin()
	if err != nil {
		t.Fatal(err)
	}
	defer tx.Rollback()
	if !s.Enqueue(event("blocked-write", "status", "site-1", time.Now())) {
		t.Fatal("queue rejected event")
	}
	deadline := time.Now().Add(time.Second)
	for len(s.queue) > 0 && time.Now().Before(deadline) {
		time.Sleep(time.Millisecond)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Millisecond)
	defer cancel()
	if err = s.Flush(ctx); !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("flush returned before blocked append committed: %v", err)
	}
	if err = tx.Rollback(); err != nil {
		t.Fatal(err)
	}
	ctx2, cancel2 := context.WithTimeout(context.Background(), time.Second)
	defer cancel2()
	if err = s.Flush(ctx2); err != nil {
		t.Fatal(err)
	}
	rows, err := s.Events(Query{})
	if err != nil || len(rows) != 1 {
		t.Fatal("flush completed without durable event")
	}
}
