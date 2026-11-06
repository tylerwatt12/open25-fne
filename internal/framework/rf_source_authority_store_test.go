package framework

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"open25.local/open25-fne/internal/radio"
)

func rfStoreTestDocument() rfAuthorityDocument {
	now := time.Now().UTC().Truncate(time.Microsecond)
	run, affiliated := now.Add(-10*time.Minute), now.Add(-3*time.Minute)
	return rfAuthorityDocument{
		ZoneID:     "zone-a",
		NativeRuns: map[string]time.Time{"site-a": run, "site-b": run, "site-c": run},
		Units: map[int]rfAuthorityUnit{
			10001: {Affiliation: rfAuthorityFact{SiteID: "site-a", GroupID: 1001, At: affiliated, NativeStartedAt: run}},
			10002: {Affiliation: rfAuthorityFact{SiteID: "site-a", GroupID: 1002, At: affiliated, NativeStartedAt: run}},
		},
	}
}

func rfStoreTestScope() rfAuthorityScope {
	return rfAuthorityScope{Sites: map[string]bool{"site-a": true, "site-b": true, "site-c": true}, Radios: map[int]bool{10001: true, 10002: true}, Groups: map[int]bool{1001: true, 1002: true, 1003: true}}
}

func rfStoreTestUpdate(d rfAuthorityDocument, site string, rid, group int, at time.Time) rfAuthorityUpdate {
	return rfAuthorityUpdate{SiteID: site, NativeStartedAt: d.NativeRuns[site], ObservedAt: time.Now().UTC(), Affiliations: []radio.Affiliation{{RID: rid, TGID: group, At: at}}}
}

func rfStoreTestOpen(t *testing.T, d rfAuthorityDocument) (*rfSourceAuthority, string) {
	t.Helper()
	path := filepath.Join(t.TempDir(), "rf-source-authority.json")
	if err := writeRFAuthority(path, d); err != nil {
		t.Fatal(err)
	}
	a, err := openRFSourceAuthority(path, d.ZoneID)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(a.close)
	return a, path
}

type rfStoreTestWrite struct {
	document rfAuthorityDocument
	release  chan error
}

func rfStoreTestBlockWrites(t *testing.T, a *rfSourceAuthority) (<-chan rfStoreTestWrite, *atomic.Int32) {
	t.Helper()
	entered, abort := make(chan rfStoreTestWrite, 8), make(chan struct{})
	calls := &atomic.Int32{}
	a.write = func(path string, value any) error {
		calls.Add(1)
		request := rfStoreTestWrite{document: cloneRFAuthority(value.(rfAuthorityDocument)), release: make(chan error, 1)}
		select {
		case entered <- request:
		case <-abort:
			return errors.New("test cleanup interrupted writer")
		}
		select {
		case err := <-request.release:
			if err != nil {
				return err
			}
			return writeRFAuthority(path, value)
		case <-abort:
			return errors.New("test cleanup interrupted writer")
		}
	}
	t.Cleanup(func() { close(abort) })
	return entered, calls
}

func rfStoreTestAwaitWrite(t *testing.T, entered <-chan rfStoreTestWrite) rfStoreTestWrite {
	t.Helper()
	select {
	case request := <-entered:
		return request
	case <-time.After(3 * time.Second):
		t.Fatal("writer was not reached")
		return rfStoreTestWrite{}
	}
}

func rfStoreTestApplyAsync(a *rfSourceAuthority, ctx context.Context, u rfAuthorityUpdate) <-chan error {
	done := make(chan error, 1)
	go func() { done <- a.apply(ctx, u, rfStoreTestScope()) }()
	return done
}

func rfStoreTestAwaitApply(t *testing.T, done <-chan error) error {
	t.Helper()
	select {
	case err := <-done:
		return err
	case <-time.After(3 * time.Second):
		t.Fatal("authority apply did not return")
		return errors.New("apply timed out")
	}
}

func rfStoreTestStillPending(t *testing.T, done <-chan error) {
	t.Helper()
	select {
	case err := <-done:
		t.Fatalf("authority apply returned before commit: %v", err)
	default:
	}
}

func rfStoreTestWait(t *testing.T, ready func() bool) {
	t.Helper()
	deadline, tick := time.NewTimer(3*time.Second), time.NewTicker(time.Millisecond)
	defer deadline.Stop()
	defer tick.Stop()
	for !ready() {
		select {
		case <-tick.C:
		case <-deadline.C:
			t.Fatal("authority state did not reach the expected boundary")
		}
	}
}

func rfStoreTestOwner(t *testing.T, a *rfSourceAuthority, rid int, want affiliationOwner) {
	t.Helper()
	if got, ok := a.lookup(rid); !ok || got != want {
		t.Fatalf("lookup(%d) = %#v, %t; want %#v, true", rid, got, ok, want)
	}
}

func rfStoreTestFenced(t *testing.T, a *rfSourceAuthority, rid int) {
	t.Helper()
	if owner, ok := a.lookup(rid); ok {
		t.Fatalf("fenced RID %d still authorizes %#v", rid, owner)
	}
	if !a.value.Load().fenced[rid] {
		t.Fatalf("RID %d has no durable-commit fence", rid)
	}
}

func TestRFSourceAuthorityStorePublishesOnlyCommittedFactsAndReopensThem(t *testing.T) {
	d := rfStoreTestDocument()
	a, path := rfStoreTestOpen(t, d)
	entered, _ := rfStoreTestBlockWrites(t, a)
	b := rfStoreTestUpdate(d, "site-b", 10001, 1002, d.Units[10001].Affiliation.At.Add(time.Minute))
	done := rfStoreTestApplyAsync(a, context.Background(), b)
	request := rfStoreTestAwaitWrite(t, entered)
	rfStoreTestFenced(t, a, 10001)
	rfStoreTestStillPending(t, done)
	if snapshot := a.value.Load(); snapshot.version != 0 || !reflect.DeepEqual(snapshot.document, d) {
		t.Fatalf("uncommitted write published accepted memory: %#v", snapshot)
	}
	diskBefore, err := openRFSourceAuthority(path, d.ZoneID)
	if err != nil {
		t.Fatal(err)
	}
	rfStoreTestOwner(t, diskBefore, 10001, affiliationOwner{"site-a", 1001, d.Units[10001].Affiliation.At})
	diskBefore.close()
	request.release <- nil
	if err := rfStoreTestAwaitApply(t, done); err != nil {
		t.Fatal(err)
	}
	wantB := affiliationOwner{"site-b", 1002, b.Affiliations[0].At}
	rfStoreTestOwner(t, a, 10001, wantB)
	if snapshot := a.value.Load(); snapshot.version != 1 || snapshot.fenced[10001] || snapshot.err != nil {
		t.Fatalf("committed write did not clear its fence: %#v", snapshot)
	}
	a.close()
	reopened, err := openRFSourceAuthority(path, d.ZoneID)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(reopened.close)
	var writes atomic.Int32
	reopened.write = func(path string, value any) error { writes.Add(1); return writeRFAuthority(path, value) }
	oldA := rfStoreTestUpdate(d, "site-a", 10001, 1001, d.Units[10001].Affiliation.At)
	if err := reopened.apply(context.Background(), oldA, rfStoreTestScope()); err != nil {
		t.Fatal(err)
	}
	rfStoreTestOwner(t, reopened, 10001, wantB)
	if writes.Load() != 0 {
		t.Fatal("older A rewrote durable B ownership after reopen")
	}
}

func TestRFSourceAuthorityStoreFailureFencesUntilRetryAndLeavesUnrelatedSourceAvailable(t *testing.T) {
	d := rfStoreTestDocument()
	a, _ := rfStoreTestOpen(t, d)
	entered, calls := rfStoreTestBlockWrites(t, a)
	b := rfStoreTestUpdate(d, "site-b", 10001, 1002, d.Units[10001].Affiliation.At.Add(time.Minute))
	first := rfStoreTestApplyAsync(a, context.Background(), b)
	request := rfStoreTestAwaitWrite(t, entered)
	request.release <- errors.New("injected durable-write failure")
	if err := rfStoreTestAwaitApply(t, first); err == nil || !strings.Contains(err.Error(), "injected durable-write failure") {
		t.Fatalf("failed commit returned %v", err)
	}
	rfStoreTestFenced(t, a, 10001)
	if snapshot := a.value.Load(); snapshot.err == nil || snapshot.version != 0 || !reflect.DeepEqual(snapshot.document, d) {
		t.Fatalf("failed write altered accepted facts: %#v", snapshot)
	}
	b.ObservedAt = time.Now().UTC()
	retry := rfStoreTestApplyAsync(a, context.Background(), b)
	request = rfStoreTestAwaitWrite(t, entered)
	rfStoreTestFenced(t, a, 10001)
	unchanged := rfStoreTestUpdate(d, "site-a", 10002, 1002, d.Units[10002].Affiliation.At)
	ctx, cancel := context.WithTimeout(context.Background(), 500*time.Millisecond)
	defer cancel()
	if err := a.apply(ctx, unchanged, rfStoreTestScope()); err != nil {
		t.Fatalf("unrelated accepted source waited on blocked writer: %v", err)
	}
	rfStoreTestOwner(t, a, 10002, affiliationOwner{"site-a", 1002, unchanged.Affiliations[0].At})
	if calls.Load() != 2 {
		t.Fatalf("unchanged unrelated source triggered a write; calls = %d", calls.Load())
	}
	rfStoreTestStillPending(t, retry)
	request.release <- nil
	if err := rfStoreTestAwaitApply(t, retry); err != nil {
		t.Fatal(err)
	}
	rfStoreTestOwner(t, a, 10001, affiliationOwner{"site-b", 1002, b.Affiliations[0].At})
	if snapshot := a.value.Load(); snapshot.err != nil || snapshot.fenced[10001] {
		t.Fatalf("successful retry retained failure/fence: %#v", snapshot)
	}
}

func TestRFSourceAuthorityStoreOlderCommitCannotClearNewerPendingFence(t *testing.T) {
	d := rfStoreTestDocument()
	a, _ := rfStoreTestOpen(t, d)
	entered, _ := rfStoreTestBlockWrites(t, a)
	b := rfStoreTestUpdate(d, "site-b", 10001, 1002, d.Units[10001].Affiliation.At.Add(time.Minute))
	c := rfStoreTestUpdate(d, "site-c", 10001, 1003, d.Units[10001].Affiliation.At.Add(2*time.Minute))
	bDone := rfStoreTestApplyAsync(a, context.Background(), b)
	bWrite := rfStoreTestAwaitWrite(t, entered)
	cDone := rfStoreTestApplyAsync(a, context.Background(), c)
	rfStoreTestWait(t, func() bool { a.mu.Lock(); defer a.mu.Unlock(); return a.version == 2 })
	bWrite.release <- nil
	if err := rfStoreTestAwaitApply(t, bDone); err != nil {
		t.Fatal(err)
	}
	cWrite := rfStoreTestAwaitWrite(t, entered)
	rfStoreTestFenced(t, a, 10001)
	rfStoreTestStillPending(t, cDone)
	if snapshot := a.value.Load(); snapshot.version != 1 || snapshot.document.Units[10001].Affiliation.SiteID != "site-b" {
		t.Fatalf("older B commit did not preserve its publication boundary: %#v", snapshot)
	}
	if cWrite.document.Units[10001].Affiliation.SiteID != "site-c" {
		t.Fatal("second commit did not contain newer C membership")
	}
	cWrite.release <- nil
	if err := rfStoreTestAwaitApply(t, cDone); err != nil {
		t.Fatal(err)
	}
	rfStoreTestOwner(t, a, 10001, affiliationOwner{"site-c", 1003, c.Affiliations[0].At})
}

func TestRFSourceAuthorityStoreCallerCancellationCannotReturnSuccessfulAdmission(t *testing.T) {
	d := rfStoreTestDocument()
	a, _ := rfStoreTestOpen(t, d)
	entered, _ := rfStoreTestBlockWrites(t, a)
	b := rfStoreTestUpdate(d, "site-b", 10001, 1002, d.Units[10001].Affiliation.At.Add(time.Minute))
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	done := rfStoreTestApplyAsync(a, ctx, b)
	request := rfStoreTestAwaitWrite(t, entered)
	cancel()
	if err := rfStoreTestAwaitApply(t, done); !errors.Is(err, context.Canceled) {
		t.Fatalf("canceled admission returned %v", err)
	}
	rfStoreTestFenced(t, a, 10001)
	request.release <- nil
	rfStoreTestWait(t, func() bool { return a.value.Load().version == 1 })
	rfStoreTestOwner(t, a, 10001, affiliationOwner{"site-b", 1002, b.Affiliations[0].At})
	// A later durable commit is reusable by new requests; the canceled caller
	// has already received an error and cannot treat that commit as admission.
	if err := a.apply(ctx, b, rfStoreTestScope()); !errors.Is(err, context.Canceled) {
		t.Fatalf("already canceled caller received committed admission: %v", err)
	}
}

func TestRFSourceAuthorityStoreCloseCannotPublishAfterWriterCancellation(t *testing.T) {
	d := rfStoreTestDocument()
	a, path := rfStoreTestOpen(t, d)
	entered, _ := rfStoreTestBlockWrites(t, a)
	b := rfStoreTestUpdate(d, "site-b", 10001, 1002, d.Units[10001].Affiliation.At.Add(time.Minute))
	done := rfStoreTestApplyAsync(a, context.Background(), b)
	request := rfStoreTestAwaitWrite(t, entered)
	a.cancel()
	if err := rfStoreTestAwaitApply(t, done); err == nil {
		t.Fatal("closed authority admitted a pending request")
	}
	closed := make(chan struct{})
	go func() { a.close(); close(closed) }()
	select {
	case <-closed:
		t.Fatal("close returned while its writer was still blocked")
	default:
	}
	request.release <- nil
	select {
	case <-closed:
	case <-time.After(3 * time.Second):
		t.Fatal("close did not finish after writer release")
	}
	if snapshot := a.value.Load(); snapshot.version != 0 || !reflect.DeepEqual(snapshot.document, d) {
		t.Fatalf("canceled writer published accepted memory: %#v", snapshot)
	}
	if owner, ok := a.lookup(10001); ok {
		t.Fatalf("closed authority authorized %#v", owner)
	}
	// The real atomic write may finish after cancellation; accepted memory is
	// still closed, while a new process may use that successfully durable fact.
	reopened, err := openRFSourceAuthority(path, d.ZoneID)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(reopened.close)
	rfStoreTestOwner(t, reopened, 10001, affiliationOwner{"site-b", 1002, b.Affiliations[0].At})
}

func TestRFSourceAuthorityStoreRejectsUnavailableInsecureAndMalformedState(t *testing.T) {
	for _, test := range []struct {
		name    string
		prepare func(string, rfAuthorityDocument) error
	}{
		{"missing", func(string, rfAuthorityDocument) error { return nil }},
		{"corrupt JSON", func(path string, _ rfAuthorityDocument) error { return os.WriteFile(path, []byte("{"), 0600) }},
		{"unknown JSON field", func(path string, _ rfAuthorityDocument) error {
			return os.WriteFile(path, []byte(`{"zone_id":"zone-a","native_runs":{},"units":{},"unknown":true}`), 0600)
		}},
		{"wrong Zone", func(path string, d rfAuthorityDocument) error {
			d.ZoneID = "zone-other"
			return writeRFAuthority(path, d)
		}},
		{"insecure permissions", func(path string, d rfAuthorityDocument) error {
			if err := writeRFAuthority(path, d); err != nil {
				return err
			}
			return os.Chmod(path, 0644)
		}},
		{"directory", func(path string, _ rfAuthorityDocument) error { return os.Mkdir(path, 0700) }},
		{"symlink", func(path string, d rfAuthorityDocument) error {
			target := path + ".target"
			if err := writeRFAuthority(target, d); err != nil {
				return err
			}
			return os.Symlink(target, path)
		}},
		{"missing native map", func(path string, d rfAuthorityDocument) error { d.NativeRuns = nil; return writeRFAuthority(path, d) }},
		{"missing unit map", func(path string, d rfAuthorityDocument) error { d.Units = nil; return writeRFAuthority(path, d) }},
		{"zero native run", func(path string, d rfAuthorityDocument) error {
			d.NativeRuns["site-a"] = time.Time{}
			return writeRFAuthority(path, d)
		}},
		{"invalid radio", func(path string, d rfAuthorityDocument) error {
			d.Units[0] = d.Units[10001]
			return writeRFAuthority(path, d)
		}},
		{"invalid talkgroup", func(path string, d rfAuthorityDocument) error {
			unit := d.Units[10001]
			unit.Affiliation.GroupID = 0
			d.Units[10001] = unit
			return writeRFAuthority(path, d)
		}},
		{"incomplete fact", func(path string, d rfAuthorityDocument) error {
			unit := d.Units[10001]
			unit.Affiliation.At = time.Time{}
			d.Units[10001] = unit
			return writeRFAuthority(path, d)
		}},
		{"future fact", func(path string, d rfAuthorityDocument) error {
			unit := d.Units[10001]
			unit.Affiliation.At = time.Now().Add(2 * time.Minute)
			d.Units[10001] = unit
			return writeRFAuthority(path, d)
		}},
	} {
		t.Run(test.name, func(t *testing.T) {
			path := filepath.Join(t.TempDir(), "rf-source-authority.json")
			if err := test.prepare(path, rfStoreTestDocument()); err != nil {
				t.Fatal(err)
			}
			a, err := openRFSourceAuthority(path, "zone-a")
			if a != nil {
				a.close()
			}
			if err == nil || a != nil {
				t.Fatalf("unsafe state opened: actor=%p error=%v", a, err)
			}
		})
	}
}

func TestRFSourceAuthorityStoreHistoricalFactsWithoutNativeRunReopenAsExclusionOnly(t *testing.T) {
	for _, test := range []struct {
		name         string
		affiliation  bool
		registration bool
		knownRun     bool
	}{
		{"zero fact run and missing NativeRuns entry", true, false, false},
		{"zero fact run with known Site run", true, false, true},
		{"historical registration without Site run", false, true, false},
		{"historical affiliation and registration without Site run", true, true, false},
	} {
		t.Run(test.name, func(t *testing.T) {
			d := rfStoreTestDocument()
			at, run := d.Units[10001].Affiliation.At, d.NativeRuns["site-a"]
			d.NativeRuns = map[string]time.Time{}
			if test.knownRun {
				d.NativeRuns["site-a"] = run
			}
			var unit rfAuthorityUnit
			if test.affiliation {
				unit.Affiliation = rfAuthorityFact{SiteID: "site-a", GroupID: 1001, At: at}
			}
			if test.registration {
				at = at.Add(time.Minute)
				unit.Registration = rfAuthorityFact{SiteID: "site-a", At: at}
			}
			d.Units = map[int]rfAuthorityUnit{10001: unit}
			a, path := rfStoreTestOpen(t, d)
			rfStoreTestOwner(t, a, 10001, affiliationOwner{SiteID: "site-a", At: at})
			if snapshot := a.value.Load(); snapshot.err != nil || len(snapshot.fenced) != 0 {
				t.Fatalf("valid historical exclusion was treated as an uncommitted error: %#v", snapshot)
			}
			a.close()
			reopened, err := openRFSourceAuthority(path, d.ZoneID)
			if err != nil {
				t.Fatal(err)
			}
			t.Cleanup(reopened.close)
			rfStoreTestOwner(t, reopened, 10001, affiliationOwner{SiteID: "site-a", At: at})
		})
	}
}

func TestRFSourceAuthorityStoreInvalidUpdatesCannotAlterOrFenceAcceptedState(t *testing.T) {
	d := rfStoreTestDocument()
	a, _ := rfStoreTestOpen(t, d)
	var writes atomic.Int32
	a.write = func(path string, value any) error { writes.Add(1); return writeRFAuthority(path, value) }
	for _, test := range []struct {
		name   string
		mutate func(*rfAuthorityUpdate)
	}{
		{"unknown Site", func(u *rfAuthorityUpdate) { u.SiteID = "site-unknown" }},
		{"malformed Site", func(u *rfAuthorityUpdate) { u.SiteID = "bad site" }},
		{"unknown radio", func(u *rfAuthorityUpdate) { u.Affiliations[0].RID = 10003 }},
		{"zero radio", func(u *rfAuthorityUpdate) { u.Affiliations[0].RID = 0 }},
		{"radio outside P25 range", func(u *rfAuthorityUpdate) { u.Affiliations[0].RID = 0xfffffc }},
		{"unknown group", func(u *rfAuthorityUpdate) { u.Affiliations[0].TGID = 1004 }},
		{"zero group", func(u *rfAuthorityUpdate) { u.Affiliations[0].TGID = 0 }},
		{"group outside P25 range", func(u *rfAuthorityUpdate) { u.Affiliations[0].TGID = 65535 }},
		{"stale observation", func(u *rfAuthorityUpdate) { u.ObservedAt = time.Now().Add(-5 * time.Second) }},
		{"future observation", func(u *rfAuthorityUpdate) { u.ObservedAt = time.Now().Add(2 * time.Minute) }},
		{"missing observation", func(u *rfAuthorityUpdate) { u.ObservedAt = time.Time{} }},
		{"missing native run", func(u *rfAuthorityUpdate) { u.NativeStartedAt = time.Time{} }},
		{"superseded native run", func(u *rfAuthorityUpdate) { u.NativeStartedAt = u.NativeStartedAt.Add(-time.Second) }},
		{"native run after observation", func(u *rfAuthorityUpdate) { u.NativeStartedAt = u.ObservedAt.Add(time.Second) }},
		{"missing membership time", func(u *rfAuthorityUpdate) { u.Affiliations[0].At = time.Time{} }},
		{"membership before native run", func(u *rfAuthorityUpdate) { u.Affiliations[0].At = u.NativeStartedAt.Add(-time.Nanosecond) }},
		{"membership after observation", func(u *rfAuthorityUpdate) { u.Affiliations[0].At = u.ObservedAt.Add(time.Nanosecond) }},
		{"duplicate membership", func(u *rfAuthorityUpdate) { u.Affiliations = append(u.Affiliations, u.Affiliations[0]) }},
		{"unknown registration", func(u *rfAuthorityUpdate) {
			u.Registrations = []radio.Registration{{RID: 10003, At: u.Affiliations[0].At}}
		}},
		{"duplicate registration", func(u *rfAuthorityUpdate) {
			r := radio.Registration{RID: 10001, At: u.Affiliations[0].At}
			u.Registrations = []radio.Registration{r, r}
		}},
		{"retired unknown kind", func(u *rfAuthorityUpdate) {
			u.Retired = []radio.RetiredSourceFact{{Kind: "made_up", RID: 10001, At: u.Affiliations[0].At, NativeStartedAt: u.NativeStartedAt}}
		}},
		{"retired unknown radio", func(u *rfAuthorityUpdate) {
			u.Retired = []radio.RetiredSourceFact{{Kind: "registration", RID: 10003, At: u.Affiliations[0].At, NativeStartedAt: u.NativeStartedAt}}
		}},
		{"retired zero native run", func(u *rfAuthorityUpdate) {
			u.Retired = []radio.RetiredSourceFact{{Kind: "registration", RID: 10001, At: u.Affiliations[0].At}}
		}},
		{"retired later native run", func(u *rfAuthorityUpdate) {
			u.Retired = []radio.RetiredSourceFact{{Kind: "registration", RID: 10001, At: u.Affiliations[0].At, NativeStartedAt: u.NativeStartedAt.Add(time.Second)}}
		}},
		{"retired before native run", func(u *rfAuthorityUpdate) {
			u.Retired = []radio.RetiredSourceFact{{Kind: "null_affiliation", RID: 10001, At: u.NativeStartedAt.Add(-time.Second), NativeStartedAt: u.NativeStartedAt}}
		}},
		{"retired duplicate kind and radio", func(u *rfAuthorityUpdate) {
			r := radio.RetiredSourceFact{Kind: "deregistration", RID: 10001, At: u.Affiliations[0].At, NativeStartedAt: u.NativeStartedAt}
			u.Retired = []radio.RetiredSourceFact{r, r}
		}},
		{"unknown terminal", func(u *rfAuthorityUpdate) { u.Terminals = []radio.Registration{{RID: 10003, At: u.Affiliations[0].At}} }},
		{"invalid withdrawal", func(u *rfAuthorityUpdate) {
			u.Withdrawals = []rfAuthorityWithdrawal{{RadioID: 10001, GroupID: 1004, AffiliatedAt: u.Affiliations[0].At, NativeStartedAt: u.NativeStartedAt}}
		}},
	} {
		t.Run(test.name, func(t *testing.T) {
			u := rfStoreTestUpdate(d, "site-b", 10001, 1002, d.Units[10001].Affiliation.At.Add(time.Minute))
			test.mutate(&u)
			if err := a.apply(context.Background(), u, rfStoreTestScope()); err == nil {
				t.Fatal("invalid update was accepted")
			}
			if snapshot := a.value.Load(); snapshot.version != 0 || snapshot.err != nil || len(snapshot.fenced) != 0 || !reflect.DeepEqual(snapshot.document, d) {
				t.Fatalf("invalid update altered or fenced accepted state: %#v", snapshot)
			}
			a.mu.Lock()
			desiredChanged := a.version != 0 || !reflect.DeepEqual(a.desired, d)
			a.mu.Unlock()
			if desiredChanged || writes.Load() != 0 {
				t.Fatalf("invalid update staged a durable change: desiredChanged=%t writes=%d", desiredChanged, writes.Load())
			}
			rfStoreTestOwner(t, a, 10001, affiliationOwner{"site-a", 1001, d.Units[10001].Affiliation.At})
			rfStoreTestOwner(t, a, 10002, affiliationOwner{"site-a", 1002, d.Units[10002].Affiliation.At})
		})
	}
}

func TestRFSourceAuthorityStoreControllerCoalescedWithdrawalFencesAcrossFailureAndRestart(t *testing.T) {
	now := time.Now().UTC()
	w := radio.SourceWithdrawal{RadioID: 10001, GroupID: 1002, AffiliatedAt: now.Add(-2 * time.Second), NativeStartedAt: now.Add(-3 * time.Second)}
	state := radio.SourceState{SiteID: "site-b", NativeStartedAt: now.Add(-time.Second), ObservedAt: now, Revision: 7, Active: true, Withdrawals: []radio.SourceWithdrawal{w}}
	aRun := w.NativeStartedAt.Add(-time.Second)
	aAt := w.AffiliatedAt.Add(-100 * time.Millisecond)
	d := rfAuthorityDocument{ZoneID: "zone-a", NativeRuns: map[string]time.Time{"site-a": aRun}, Units: map[int]rfAuthorityUnit{10001: {Affiliation: rfAuthorityFact{SiteID: "site-a", GroupID: 1001, At: aAt, NativeStartedAt: aRun}}}}
	a, path := rfStoreTestOpen(t, d)
	entered, _ := rfStoreTestBlockWrites(t, a)
	state.ObservedAt = time.Now().UTC()
	u := rfUpdateFromSource(state)
	done := rfStoreTestApplyAsync(a, context.Background(), u)
	request := rfStoreTestAwaitWrite(t, entered)
	rfStoreTestFenced(t, a, 10001)
	request.release <- errors.New("simulated storage failure")
	if err := rfStoreTestAwaitApply(t, done); err == nil {
		t.Fatal("failed negative fact was acknowledged")
	}
	rfStoreTestFenced(t, a, 10001)
	u.ObservedAt = time.Now().UTC()
	done = rfStoreTestApplyAsync(a, context.Background(), u)
	request = rfStoreTestAwaitWrite(t, entered)
	request.release <- nil
	if err := rfStoreTestAwaitApply(t, done); err != nil {
		t.Fatal(err)
	}
	want := affiliationOwner{SiteID: "site-b", At: w.AffiliatedAt}
	rfStoreTestOwner(t, a, 10001, want)
	fact := a.value.Load().document.Units[10001].Affiliation
	if !fact.Withdrawn || fact.GroupID != w.GroupID || !fact.NativeStartedAt.Equal(w.NativeStartedAt) {
		t.Fatal("original departed identity was lost", fact, w)
	}
	a.close()
	reopened, err := openRFSourceAuthority(path, "zone-a")
	if err != nil {
		t.Fatal(err)
	}
	defer reopened.close()
	rfStoreTestOwner(t, reopened, 10001, want)
	oldSource := rfAuthorityUpdate{SiteID: "site-a", NativeStartedAt: aRun, ObservedAt: time.Now().UTC(), Affiliations: []radio.Affiliation{{RID: 10001, TGID: 1001, At: aAt}}}
	if err = reopened.apply(context.Background(), oldSource, rfStoreTestScope()); err != nil {
		t.Fatal(err)
	}
	rfStoreTestOwner(t, reopened, 10001, want)
}

func TestRFSourceAuthorityStoreRetiredControlFactsFenceAcrossFailureAndReopen(t *testing.T) {
	for _, kind := range []string{"registration", "null_affiliation", "deregistration"} {
		t.Run(kind, func(t *testing.T) {
			now := time.Now().UTC()
			fact := radio.RetiredSourceFact{Kind: kind, RID: 10001, At: now.Add(-2 * time.Second), NativeStartedAt: now.Add(-3 * time.Second)}
			state := radio.SourceState{SiteID: "site-b", NativeStartedAt: now.Add(-time.Second), ObservedAt: now, Revision: 7, Active: true, Retired: []radio.RetiredSourceFact{fact}}
			aRun := fact.NativeStartedAt.Add(-time.Second)
			aAt := fact.At.Add(-100 * time.Millisecond)
			d := rfAuthorityDocument{ZoneID: "zone-a", NativeRuns: map[string]time.Time{"site-a": aRun}, Units: map[int]rfAuthorityUnit{10001: {Affiliation: rfAuthorityFact{SiteID: "site-a", GroupID: 1001, At: aAt, NativeStartedAt: aRun}}}}
			a, path := rfStoreTestOpen(t, d)
			entered, _ := rfStoreTestBlockWrites(t, a)
			state.ObservedAt = time.Now().UTC()
			update := rfUpdateFromSource(state)
			done := rfStoreTestApplyAsync(a, context.Background(), update)
			request := rfStoreTestAwaitWrite(t, entered)
			rfStoreTestFenced(t, a, 10001)
			request.release <- errors.New("simulated unavailable storage")
			if err := rfStoreTestAwaitApply(t, done); err == nil {
				t.Fatal("failed fact was acknowledged")
			}
			rfStoreTestFenced(t, a, 10001)
			update.ObservedAt = time.Now().UTC()
			done = rfStoreTestApplyAsync(a, context.Background(), update)
			request = rfStoreTestAwaitWrite(t, entered)
			request.release <- nil
			if err := rfStoreTestAwaitApply(t, done); err != nil {
				t.Fatal(err)
			}
			want := affiliationOwner{SiteID: "site-b", At: fact.At}
			rfStoreTestOwner(t, a, 10001, want)
			unit := a.value.Load().document.Units[10001]
			var stored rfAuthorityFact
			switch fact.Kind {
			case "registration":
				stored = unit.Registration
			case "null_affiliation":
				stored = unit.NullAffiliation
			case "deregistration":
				stored = unit.Terminal
			}
			if stored.SiteID != "site-b" || !stored.At.Equal(fact.At) || !stored.NativeStartedAt.Equal(fact.NativeStartedAt) || !unit.MembershipUnknown.At.IsZero() {
				t.Fatal("retirement fabricated a procedure/time/native run", stored, fact)
			}
			a.close()
			reopened, err := openRFSourceAuthority(path, "zone-a")
			if err != nil {
				t.Fatal(err)
			}
			defer reopened.close()
			rfStoreTestOwner(t, reopened, 10001, want)
			oldSource := rfAuthorityUpdate{SiteID: "site-a", NativeStartedAt: aRun, ObservedAt: time.Now().UTC(), Affiliations: []radio.Affiliation{{RID: 10001, TGID: 1001, At: aAt}}}
			if err := reopened.apply(context.Background(), oldSource, rfStoreTestScope()); err != nil {
				t.Fatal(err)
			}
			rfStoreTestOwner(t, reopened, 10001, want)
		})
	}
}
