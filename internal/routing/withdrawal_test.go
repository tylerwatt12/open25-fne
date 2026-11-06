package routing

import (
	"context"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"open25.local/open25-fne/internal/dispatch"
)

func TestWithdrawalKeepsItsDecisionAndEmitsOnceOutsideLocks(t *testing.T) {
	entered, release := make(chan struct{}, 1), make(chan struct{})
	var unblock sync.Once
	defer unblock.Do(func() { close(release) })
	target := &testTarget{id: "site", endStarted: entered, endBlock: release}
	var interested atomic.Bool
	interested.Store(true)
	var reason atomic.Value
	reason.Store("Site report is stale (age 4.1s; received 2026-11-01T13:03:58Z; limit 4s).")
	var evaluations atomic.Int32
	events := make(chan string, 8)
	var r *Router
	r = newRouter(t, Options{Policy: func(Request) Policy { return Policy{Retry: 10 * time.Millisecond} }, Targets: func(q Request) Selection {
		evaluations.Add(1)
		if interested.Load() {
			return Selection{Targets: []Target{{ID: "site", Router: target, Groups: q.Talkgroups}}}
		}
		return Selection{Excluded: map[string]string{"site": reason.Load().(string)}}
	}, Event: func(q Request, kind, message string) {
		if kind == "site_left" {
			// This would deadlock if the callback still held the call's media lock.
			_ = r.Delivery("owner", dispatch.Grant{ID: q.ID, Talkgroups: q.Talkgroups})
			events <- message
		}
	}})
	g, err := r.Begin(context.Background(), "owner", request("withdrawal", 1001))
	if err != nil {
		t.Fatal(err)
	}
	original := reason.Load().(string)
	interested.Store(false)
	select {
	case <-entered:
	case <-time.After(time.Second):
		t.Fatal("withdrawal did not release its Site")
	}
	got := r.Delivery("owner", g)
	if len(got) != 1 || got[0].State != "left" || got[0].Message != original {
		t.Fatalf("wrong captured withdrawal: %+v", got)
	}
	reason.Store("Radio ownership data is unavailable.")
	unblock.Do(func() { close(release) })
	select {
	case message := <-events:
		if message != "site: "+original {
			t.Fatal("later state rewrote withdrawal event:", message)
		}
	case <-time.After(time.Second):
		t.Fatal("withdrawal event was blocked")
	}
	after := evaluations.Load()
	waitFor(t, func() bool { return evaluations.Load() >= after+3 })
	if _, _, _, ends := target.counts(); ends != 1 {
		t.Fatal("withdrawal repeated its release:", ends)
	}
	select {
	case message := <-events:
		t.Fatal("withdrawal was logged again:", message)
	default:
	}
	if got := r.Delivery("owner", g); len(got) != 1 || got[0].Message != original {
		t.Fatal("later evaluation rewrote stored decision:", got)
	}
}

func TestWithdrawalReportsChangedGroupSetFromSameSelection(t *testing.T) {
	target := &testTarget{id: "site"}
	var changed atomic.Bool
	events := make(chan string, 8)
	r := newRouter(t, Options{Policy: func(Request) Policy { return Policy{Retry: 10 * time.Millisecond} }, Targets: func(q Request) Selection {
		if changed.Load() {
			return Selection{Targets: []Target{{ID: "site", Router: target, Groups: []int{1001}}}, Excluded: map[string]string{"site": "TG1002 policy does not enable this Site."}}
		}
		return Selection{Targets: []Target{{ID: "site", Router: target, Groups: q.Talkgroups}}}
	}, Event: func(_ Request, kind, message string) {
		if kind == "site_left" {
			events <- message
		}
	}})
	g, err := r.Begin(context.Background(), "owner", request("group-change", 1001, 1002))
	if err != nil {
		t.Fatal(err)
	}
	changed.Store(true)
	select {
	case message := <-events:
		if !strings.Contains(message, "[1001 1002] to [1001]") || !strings.Contains(message, "TG1002 policy") {
			t.Fatal("changed groups lost same-decision reason:", message)
		}
	case <-time.After(time.Second):
		t.Fatal("group change was not recorded")
	}
	waitFor(t, func() bool { d := r.Delivery("owner", g); return len(d) == 1 && d[0].State == "active" })
	if len(r.Active()) != 1 {
		t.Fatal("diagnostic ended the logical call")
	}
	if begins, _, _, ends := target.counts(); begins != 2 || ends != 1 {
		t.Fatalf("group replacement changed allocation behavior: begins=%d ends=%d", begins, ends)
	}
}

func TestSelectionPreservesOriginNormalizationReason(t *testing.T) {
	target := &testTarget{id: "origin"}
	r := newRouter(t, Options{Targets: func(q Request) Selection {
		return Selection{Targets: []Target{{ID: "origin", Router: target, Groups: q.Talkgroups}}}
	}})
	q := request("origin-normalization", 1001)
	q.OriginSite = "origin"
	selected, err := r.targetList(q)
	if err != nil || len(selected.Targets) != 0 || !strings.Contains(selected.Excluded["origin"], "Source Site") {
		t.Fatalf("origin filtering lost its reason: %+v %v", selected, err)
	}
}
