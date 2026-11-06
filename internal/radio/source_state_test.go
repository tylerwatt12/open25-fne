//go:build linux || darwin

package radio

import (
	"context"
	"encoding/json"
	"os"
	"reflect"
	"testing"
	"time"
)

func sourceStateTestController(t *testing.T, callback func(Event)) *Controller {
	t.Helper()
	c, err := New(Options{StateDir: t.TempDir(), OnEvent: callback})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = c.Close() })
	c.mu.Lock()
	c.profile = fixtureProfile()
	c.profile.System.Talkgroups = append(c.profile.System.Talkgroups, Talkgroup{TGID: 1002, AdmissionMode: "clear", TransmitMode: "clear", Enabled: true, SiteAccess: "all"})
	c.status.State, c.status.SiteID = "running", c.profile.Site.ID
	c.status.InhibitionReady = true
	c.status.StartedAt = time.Now().UTC().Add(-time.Hour)
	c.status.Channels = []ChannelStatus{{ID: "control-1", Role: "control", State: "ready"}, {ID: "voice-1", Role: "voice", State: "ready"}}
	c.mu.Unlock()
	return c
}

func sourceStateTestChild(t *testing.T, c *Controller, role string, started time.Time) *child {
	t.Helper()
	id := role + "-1"
	ready := make(chan struct{})
	close(ready)
	owner := &child{id: id, role: role, done: make(chan struct{}), ready: ready, started: started, groupRetired: true}
	t.Cleanup(func() { close(owner.done) })
	c.mu.Lock()
	c.children[id] = owner
	c.mu.Unlock()
	return owner
}

func sourceStateTestAffiliations(c *Controller, owner *child, raw string) {
	revision := c.SourceState().Revision
	c.applyAffiliations(owner, map[string]json.RawMessage{"affiliations": json.RawMessage(raw)}, revision)
}

func sourceStateTestSameFacts(t *testing.T, got, want SourceState) {
	t.Helper()
	got.ObservedAt, want.ObservedAt = time.Time{}, time.Time{}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("source facts changed:\n got %#v\nwant %#v", got, want)
	}
}

func sourceStateTestWithdrawal(t *testing.T, state SourceState, affiliation Affiliation, run time.Time) {
	t.Helper()
	want := SourceWithdrawal{RadioID: affiliation.RID, GroupID: affiliation.TGID, AffiliatedAt: affiliation.At, NativeStartedAt: run}
	if len(state.Withdrawals) != 1 || state.Withdrawals[0] != want {
		t.Fatalf("withdrawals = %#v, want exact tuple %#v", state.Withdrawals, want)
	}
}

func TestSourceStateIndependentOfBlockedEventCallbackAndCopiesActiveFacts(t *testing.T) {
	entered, release := make(chan struct{}, 1), make(chan struct{})
	c := sourceStateTestController(t, func(Event) {
		select {
		case entered <- struct{}{}:
		default:
		}
		<-release
	})
	t.Cleanup(func() { close(release) })
	control := sourceStateTestChild(t, c, "control", time.Now().UTC().Add(-time.Second))
	c.observe(control, "I: (HOST) P25 Affiliation, unit registration, srcId = 10001")
	select {
	case <-entered:
	case <-time.After(time.Second):
		t.Fatal("event callback was not reached")
	}
	c.observe(control, "I: (HOST) P25 Affiliation, group affiliation, srcId = 10001, dstId = 1001")
	state := c.SourceState()
	if !state.Active || state.SiteID != "site-1" || !state.NativeStartedAt.Equal(control.started) || state.NativeStartedAt.Equal(c.Status().StartedAt) || state.ObservedAt.IsZero() || state.Revision == 0 {
		t.Fatalf("source snapshot lacks the actual current control run: %#v", state)
	}
	if len(state.Affiliations) != 1 || state.Affiliations[0].RID != 10001 || state.Affiliations[0].TGID != 1001 || len(state.Registrations) != 1 || state.Registrations[0].RID != 10001 {
		t.Fatalf("blocked event publication hid accepted source facts: %#v", state)
	}
	if !c.SourceAllowed(10001, 1001, control.started) {
		t.Fatal("blocked event callback prevented current source renewal")
	}
	accepted := c.SourceState()
	state.Affiliations[0] = Affiliation{RID: 99999, TGID: 9999}
	state.Registrations[0] = Registration{RID: 99999}
	sourceStateTestSameFacts(t, c.SourceState(), accepted)
}

func TestSourceStateTracksOnlyConfiguredPhysicalSubscribersAndCurrentControl(t *testing.T) {
	c := sourceStateTestController(t, nil)
	control := sourceStateTestChild(t, c, "control", time.Now().UTC().Add(-time.Second))
	voice := sourceStateTestChild(t, c, "voice", time.Now().UTC().Add(-time.Second))
	before := c.SourceState()
	for _, source := range []*child{voice, control} {
		for _, line := range []string{
			"I: (HOST) P25 Affiliation, unit registration, srcId = 10002",
			"I: (HOST) P25 Affiliation, group affiliation, srcId = 10002, dstId = 1001",
			"I: (HOST) P25 Affiliation, group affiliation, srcId = 10003, dstId = 1001",
			"I: (HOST) P25 Affiliation, group affiliation, srcId = 10001, dstId = 8888",
		} {
			c.observe(source, line)
		}
	}
	c.observe(voice, "I: (HOST) P25 Affiliation, unit registration, srcId = 10001")
	c.observe(voice, "I: (HOST) P25 Affiliation, group affiliation, srcId = 10001, dstId = 1001")
	sourceStateTestAffiliations(c, voice, `[{"srcId":10001,"grpId":1001}]`)
	sourceStateTestSameFacts(t, c.SourceState(), before)
	sourceStateTestAffiliations(c, control, `[{"srcId":10001,"grpId":1001},{"srcId":10002,"grpId":1001},{"srcId":10003,"grpId":1001},{"srcId":10004,"grpId":8888}]`)
	accepted := c.SourceState()
	if len(accepted.Affiliations) != 1 || accepted.Affiliations[0].RID != 10001 || accepted.Affiliations[0].TGID != 1001 {
		t.Fatalf("native table escaped configured physical source bounds: %#v", accepted)
	}
	c.observe(voice, "I: (HOST) P25 Affiliation, unit deregistration, srcId = 10001")
	sourceStateTestAffiliations(c, voice, `[]`)
	sourceStateTestSameFacts(t, c.SourceState(), accepted)
}

func TestSourceStateReplacementStartsEmptyAndRejectsLatePreviousRunSnapshot(t *testing.T) {
	c := sourceStateTestController(t, nil)
	old := sourceStateTestChild(t, c, "control", time.Now().UTC().Add(-2*time.Second))
	c.observe(old, "I: (HOST) P25 Affiliation, unit registration, srcId = 10001")
	c.observe(old, "I: (HOST) P25 Affiliation, group affiliation, srcId = 10001, dstId = 1001")
	previous := c.SourceState()
	if len(previous.Affiliations) != 1 {
		t.Fatalf("initial control membership missing: %#v", previous)
	}
	current := sourceStateTestChild(t, c, "control", time.Now().UTC())
	empty := c.SourceState()
	if !empty.Active || !empty.NativeStartedAt.Equal(current.started) || empty.Revision <= previous.Revision || len(empty.Affiliations) != 0 || len(empty.Registrations) != 0 {
		t.Fatalf("replacement inherited previous-run active membership: %#v", empty)
	}
	sourceStateTestWithdrawal(t, empty, previous.Affiliations[0], old.started)
	sourceStateTestAffiliations(c, current, `[{"srcId":10001,"grpId":1002}]`)
	accepted := c.SourceState()
	if len(accepted.Affiliations) != 1 || accepted.Affiliations[0].TGID != 1002 || accepted.Affiliations[0].At.Before(current.started) {
		t.Fatalf("replacement membership has no accepted current-run observation: %#v", accepted)
	}
	sourceStateTestAffiliations(c, old, `[]`)
	sourceStateTestAffiliations(c, old, `[{"srcId":10001,"grpId":1001}]`)
	c.observe(old, "I: (HOST) P25 Affiliation, unit deregistration, srcId = 10001")
	sourceStateTestSameFacts(t, c.SourceState(), accepted)
}

func TestSourceStateCompleteSnapshotRemovalWithdrawsWithoutInventingOwnershipTime(t *testing.T) {
	for _, raw := range []string{`[]`, `null`} {
		t.Run(raw, func(t *testing.T) {
			c := sourceStateTestController(t, nil)
			control := sourceStateTestChild(t, c, "control", time.Now().UTC().Add(-time.Second))
			c.observe(control, "I: (HOST) P25 Affiliation, unit registration, srcId = 10001")
			sourceStateTestAffiliations(c, control, `[{"srcId":10001,"grpId":1001}]`)
			previous := c.SourceState()
			sourceStateTestAffiliations(c, control, `[{"srcId":10001,"grpId":1001}]`)
			sourceStateTestSameFacts(t, c.SourceState(), previous)
			sourceStateTestAffiliations(c, control, raw)
			empty := c.SourceState()
			if len(empty.Affiliations) != 1 || empty.Affiliations[0] != previous.Affiliations[0] || len(empty.Registrations) != 1 || len(empty.NullAffiliations) != 0 || len(empty.Terminals) != 0 || empty.Revision <= previous.Revision {
				t.Fatalf("complete snapshot removal fabricated a membership event: %#v", empty)
			}
			sourceStateTestWithdrawal(t, empty, previous.Affiliations[0], control.started)
			sourceStateTestAffiliations(c, control, raw)
			sourceStateTestSameFacts(t, c.SourceState(), empty)
		})
	}
}

func TestSourceStateActualNullAndDeregistrationFactsCoalesceAndCopyDepartures(t *testing.T) {
	c := sourceStateTestController(t, nil)
	control := sourceStateTestChild(t, c, "control", time.Now().UTC().Add(-time.Second))
	c.observe(control, "I: (HOST) P25 Affiliation, unit registration, srcId = 10001")
	c.observe(control, "I: (HOST) P25 Affiliation, group affiliation, srcId = 10001, dstId = 1001")
	previous := c.SourceState()
	c.observe(control, "I: (HOST) P25 Affiliation, group unaffiliation, srcId = 10001, dstId = 1001")
	null := c.SourceState()
	if len(null.Affiliations) != 1 || null.Affiliations[0] != previous.Affiliations[0] || len(null.Registrations) != 1 || len(null.NullAffiliations) != 1 || null.NullAffiliations[0].RID != 10001 || !null.NullAffiliations[0].At.After(previous.Affiliations[0].At) || len(null.Terminals) != 0 {
		t.Fatalf("actual unaffiliation was not retained separately from deregistration: %#v", null)
	}
	sourceStateTestWithdrawal(t, null, previous.Affiliations[0], control.started)
	copy := c.SourceState()
	copy.NullAffiliations[0] = Registration{RID: 99999}
	copy.Withdrawals[0] = SourceWithdrawal{RadioID: 99999}
	sourceStateTestSameFacts(t, c.SourceState(), null)
	c.observe(control, "I: (HOST) P25 Affiliation, group unaffiliation, srcId = 10001, dstId = 1001")
	if repeated := c.SourceState(); len(repeated.NullAffiliations) != 1 || repeated.NullAffiliations[0].At.Before(null.NullAffiliations[0].At) || len(repeated.Withdrawals) != 1 {
		t.Fatalf("repeated departure did not remain bounded by configured RID: %#v", repeated)
	}
	c.observe(control, "I: (HOST) P25 Affiliation, group affiliation, srcId = 10001, dstId = 1002")
	newer := c.SourceState()
	if len(newer.Affiliations) != 1 || newer.Affiliations[0].TGID != 1002 || len(newer.NullAffiliations) != 0 || len(newer.Withdrawals) != 0 {
		t.Fatalf("newer same-run membership retained superseded null/departure facts: %#v", newer)
	}
	c.observe(control, "I: (HOST) P25 Affiliation, unit deregistration, srcId = 10001")
	terminal := c.SourceState()
	if len(terminal.Affiliations) != 1 || terminal.Affiliations[0] != newer.Affiliations[0] || len(terminal.Terminals) != 1 || terminal.Terminals[0].RID != 10001 || !terminal.Terminals[0].At.After(newer.Affiliations[0].At) {
		t.Fatalf("accepted deregistration did not retain its actual terminal fact: %#v", terminal)
	}
	sourceStateTestWithdrawal(t, terminal, newer.Affiliations[0], control.started)
	copy = c.SourceState()
	copy.Terminals[0] = Registration{RID: 99999}
	sourceStateTestSameFacts(t, c.SourceState(), terminal)
}

func TestSourceStateStoppedControllerRetainsExactWithdrawalWithoutActiveProof(t *testing.T) {
	c := sourceStateTestController(t, nil)
	control := sourceStateTestChild(t, c, "control", time.Now().UTC().Add(-time.Second))
	c.observe(control, "I: (HOST) P25 Affiliation, unit registration, srcId = 10001")
	c.observe(control, "I: (HOST) P25 Affiliation, group affiliation, srcId = 10001, dstId = 1001")
	previous := c.SourceState()
	if err := c.Stop(context.Background()); err != nil {
		t.Fatal(err)
	}
	stopped := c.SourceState()
	if stopped.Active || len(stopped.Affiliations) != 1 || stopped.Affiliations[0] != previous.Affiliations[0] {
		t.Fatalf("stopped controller lost exclusion facts or remained active: %#v", stopped)
	}
	sourceStateTestWithdrawal(t, stopped, previous.Affiliations[0], control.started)
}

func TestSourceStateCoalescedMembershipAndDepartureRetainAtomicExclusionFacts(t *testing.T) {
	for _, departure := range []struct {
		name     string
		line     string
		null     bool
		terminal bool
	}{
		{"table removal", "", false, false},
		{"actual null affiliation", "I: (HOST) P25 Affiliation, group unaffiliation, srcId = 10001, dstId = 1001", true, false},
		{"actual deregistration", "I: (HOST) P25 Affiliation, unit deregistration, srcId = 10001", false, true},
	} {
		t.Run(departure.name, func(t *testing.T) {
			c := sourceStateTestController(t, nil)
			control := sourceStateTestChild(t, c, "control", time.Now().UTC().Add(-time.Second))
			initial := c.SourceState()
			c.observe(control, "I: (HOST) P25 Affiliation, unit registration, srcId = 10001")
			c.observe(control, "I: (HOST) P25 Affiliation, group affiliation, srcId = 10001, dstId = 1001")
			if departure.line == "" {
				sourceStateTestAffiliations(c, control, `[]`)
			} else {
				c.observe(control, departure.line)
			}
			// The Site worker did not read the intervening membership. One
			// snapshot must still let the Zone merge that fact and its exact
			// withdrawal together, without creating active source proof.
			coalesced := c.SourceState()
			if !coalesced.Active || coalesced.Revision <= initial.Revision || len(coalesced.Affiliations) != 1 || coalesced.Affiliations[0].RID != 10001 || coalesced.Affiliations[0].TGID != 1001 {
				t.Fatalf("coalesced departure lost its accepted membership: %#v", coalesced)
			}
			sourceStateTestWithdrawal(t, coalesced, coalesced.Affiliations[0], control.started)
			if len(coalesced.NullAffiliations) > 1 || len(coalesced.Terminals) > 1 || (len(coalesced.NullAffiliations) == 1) != departure.null || (len(coalesced.Terminals) == 1) != departure.terminal {
				t.Fatalf("coalesced departure conflated table removal with an actual RF event: %#v", coalesced)
			}
		})
	}
}

func TestSourceStateWithoutUsableControlCannotProvideActiveProof(t *testing.T) {
	c := sourceStateTestController(t, nil)
	if state := c.SourceState(); state.Active || !state.NativeStartedAt.IsZero() || len(state.Affiliations) != 0 {
		t.Fatalf("absent control child provided active proof: %#v", state)
	}
	control := sourceStateTestChild(t, c, "control", time.Time{})
	c.observe(control, "I: (HOST) P25 Affiliation, group affiliation, srcId = 10001, dstId = 1001")
	if state := c.SourceState(); state.Active || len(state.Affiliations) != 0 {
		t.Fatalf("zero native-run identity provided active proof: %#v", state)
	}
}

func TestSourceAllowedRequiresExactCurrentRunAndMembership(t *testing.T) {
	c := sourceStateTestController(t, nil)
	control := sourceStateTestChild(t, c, "control", time.Now().UTC().Add(-time.Second))
	c.observe(control, "I: (HOST) P25 Affiliation, group affiliation, srcId = 10001, dstId = 1001")
	for _, test := range []struct {
		name string
		rid  int
		tgid int
		run  time.Time
		want bool
	}{
		{"accepted same-run membership", 10001, 1001, control.started, true},
		{"same run instant in another time zone", 10001, 1001, control.started.In(time.FixedZone("local", -4*60*60)), true},
		{"different run instant", 10001, 1001, control.started.Add(time.Nanosecond), false},
		{"zero run", 10001, 1001, time.Time{}, false},
		{"different configured group", 10001, 1002, control.started, false},
		{"unconfigured group", 10001, 8888, control.started, false},
		{"configured console identity", 10002, 1001, control.started, false},
		{"unconfigured identity", 10003, 1001, control.started, false},
	} {
		t.Run(test.name, func(t *testing.T) {
			if got := c.SourceAllowed(test.rid, test.tgid, test.run); got != test.want {
				t.Fatalf("SourceAllowed(%d, %d, %s) = %t, want %t", test.rid, test.tgid, test.run, got, test.want)
			}
		})
	}
	var absent *Controller
	if absent.SourceAllowed(10001, 1001, control.started) {
		t.Fatal("nil controller authorized a source")
	}
}

func TestSourceAllowedRejectsOnlyMatchingWithdrawalAndNonOlderDepartureFacts(t *testing.T) {
	c := sourceStateTestController(t, nil)
	control := sourceStateTestChild(t, c, "control", time.Now().UTC().Add(-time.Second))
	c.observe(control, "I: (HOST) P25 Affiliation, group affiliation, srcId = 10001, dstId = 1001")
	a := c.SourceState().Affiliations[0]
	withdrawal := SourceWithdrawal{RadioID: a.RID, GroupID: a.TGID, AffiliatedAt: a.At, NativeStartedAt: control.started}
	for _, test := range []struct {
		name       string
		withdrawal *SourceWithdrawal
		nullAt     time.Time
		terminalAt time.Time
		want       bool
	}{
		{"exact withdrawal", &withdrawal, time.Time{}, time.Time{}, false},
		{"withdrawal for another group", &SourceWithdrawal{RadioID: a.RID, GroupID: 1002, AffiliatedAt: a.At, NativeStartedAt: control.started}, time.Time{}, time.Time{}, true},
		{"withdrawal for another acceptance", &SourceWithdrawal{RadioID: a.RID, GroupID: a.TGID, AffiliatedAt: a.At.Add(-time.Nanosecond), NativeStartedAt: control.started}, time.Time{}, time.Time{}, true},
		{"withdrawal for another native run", &SourceWithdrawal{RadioID: a.RID, GroupID: a.TGID, AffiliatedAt: a.At, NativeStartedAt: control.started.Add(-time.Nanosecond)}, time.Time{}, time.Time{}, true},
		{"older null", nil, a.At.Add(-time.Nanosecond), time.Time{}, true},
		{"equal null", nil, a.At, time.Time{}, false},
		{"newer null", nil, a.At.Add(time.Nanosecond), time.Time{}, false},
		{"older terminal", nil, time.Time{}, a.At.Add(-time.Nanosecond), true},
		{"equal terminal", nil, time.Time{}, a.At, false},
		{"newer terminal", nil, time.Time{}, a.At.Add(time.Nanosecond), false},
	} {
		t.Run(test.name, func(t *testing.T) {
			c.mu.Lock()
			delete(c.source.withdrawals, a.RID)
			delete(c.source.nullAffiliations, a.RID)
			delete(c.source.terminals, a.RID)
			if test.withdrawal != nil {
				c.source.withdrawals[a.RID] = *test.withdrawal
			}
			if !test.nullAt.IsZero() {
				c.source.nullAffiliations[a.RID] = Registration{RID: a.RID, At: test.nullAt}
			}
			if !test.terminalAt.IsZero() {
				c.source.terminals[a.RID] = Registration{RID: a.RID, At: test.terminalAt}
			}
			c.mu.Unlock()
			if got := c.SourceAllowed(a.RID, a.TGID, control.started); got != test.want {
				t.Fatalf("SourceAllowed with %s = %t, want %t", test.name, got, test.want)
			}
		})
	}
}

func TestSourceAllowedRejectsReplacedAndStoppedControlImmediately(t *testing.T) {
	c := sourceStateTestController(t, nil)
	old := sourceStateTestChild(t, c, "control", time.Now().UTC().Add(-time.Second))
	c.observe(old, "I: (HOST) P25 Affiliation, group affiliation, srcId = 10001, dstId = 1001")
	if !c.SourceAllowed(10001, 1001, old.started) {
		t.Fatal("initial current source was not admitted")
	}
	current := sourceStateTestChild(t, c, "control", time.Now().UTC())
	if c.SourceAllowed(10001, 1001, old.started) || c.SourceAllowed(10001, 1001, current.started) {
		t.Fatal("replacement admitted inherited proof before a new accepted observation")
	}
	c.observe(current, "I: (HOST) P25 Affiliation, group affiliation, srcId = 10001, dstId = 1001")
	if c.SourceAllowed(10001, 1001, old.started) || !c.SourceAllowed(10001, 1001, current.started) {
		t.Fatal("accepted replacement membership did not require its exact new native run")
	}
	c.mu.Lock()
	c.status.State = "stopping"
	c.mu.Unlock()
	if c.SourceAllowed(10001, 1001, current.started) {
		t.Fatal("stopping control admitted renewal before a snapshot reader refreshed state")
	}
}

func TestSourceAllowedCurrentRenewalDoesNotAllocate(t *testing.T) {
	c := sourceStateTestController(t, nil)
	control := sourceStateTestChild(t, c, "control", time.Now().UTC().Add(-time.Second))
	c.observe(control, "I: (HOST) P25 Affiliation, group affiliation, srcId = 10001, dstId = 1001")
	allowed := true
	allocations := testing.AllocsPerRun(100, func() {
		if !c.SourceAllowed(10001, 1001, control.started) {
			allowed = false
		}
	})
	if !allowed || allocations != 0 {
		t.Fatalf("current renewal allowed=%t allocations=%g, want true and zero", allowed, allocations)
	}
}

func TestSourceStateLateSameRunSnapshotCannotReplaceNewerAcceptedFacts(t *testing.T) {
	c := sourceStateTestController(t, nil)
	control := sourceStateTestChild(t, c, "control", time.Now().UTC().Add(-time.Second))
	c.observe(control, "I: (HOST) P25 Affiliation, group affiliation, srcId = 10001, dstId = 1001")
	requested := c.SourceState()
	c.observe(control, "I: (HOST) P25 Affiliation, group unaffiliation, srcId = 10001, dstId = 1001")
	afterNull := c.SourceState()
	c.applyAffiliations(control, map[string]json.RawMessage{"affiliations": json.RawMessage(`[{"srcId":10001,"grpId":1001}]`)}, requested.Revision)
	sourceStateTestSameFacts(t, c.SourceState(), afterNull)
	if c.SourceAllowed(10001, 1001, control.started) {
		t.Fatal("late same-run snapshot restored source proof after accepted null affiliation")
	}
	requested = c.SourceState()
	c.observe(control, "I: (HOST) P25 Affiliation, group affiliation, srcId = 10001, dstId = 1002")
	afterNewMembership := c.SourceState()
	c.applyAffiliations(control, map[string]json.RawMessage{"affiliations": json.RawMessage(`[]`)}, requested.Revision)
	sourceStateTestSameFacts(t, c.SourceState(), afterNewMembership)
	if !c.SourceAllowed(10001, 1002, control.started) || c.SourceAllowed(10001, 1001, control.started) {
		t.Fatal("late same-run empty snapshot withdrew a newer accepted membership")
	}
}

func TestSourceStateUnpublishedMembershipSurvivesEmptyReplacementAsExactWithdrawal(t *testing.T) {
	c := sourceStateTestController(t, nil)
	c.mu.Lock()
	c.profile.Site.ID, c.status.SiteID = "site-b", "site-b"
	c.mu.Unlock()
	b1 := sourceStateTestChild(t, c, "control", time.Now().UTC().Add(-time.Second))
	c.observe(b1, "I: (HOST) P25 Affiliation, group affiliation, srcId = 10001, dstId = 1002")
	// Read the accepted semantic tuple under the controller lock. No source
	// snapshot or source publisher has observed this membership yet.
	c.mu.Lock()
	accepted, exists := c.source.affiliations[10001]
	acceptedRun := c.source.nativeStartedAt
	c.mu.Unlock()
	if !exists || accepted.TGID != 1002 || !acceptedRun.Equal(b1.started) {
		t.Fatalf("B1 did not retain its accepted unpublished tuple: %#v, run %s", accepted, acceptedRun)
	}
	b2 := sourceStateTestChild(t, c, "control", time.Now().UTC())
	first := c.SourceState()
	if !first.Active || first.SiteID != "site-b" || !first.NativeStartedAt.Equal(b2.started) || len(first.Affiliations) != 0 || len(first.Registrations) != 0 || len(first.NullAffiliations) != 0 || len(first.Terminals) != 0 {
		t.Fatalf("first B2 snapshot inherited or fabricated membership: %#v", first)
	}
	if !accepted.At.Before(first.NativeStartedAt) {
		t.Fatalf("test did not cross the B1/B2 native-run boundary: affiliation=%s B2=%s", accepted.At, first.NativeStartedAt)
	}
	sourceStateTestWithdrawal(t, first, accepted, b1.started)
	if c.SourceAllowed(10001, 1002, b1.started) || c.SourceAllowed(10001, 1002, b2.started) {
		t.Fatal("unpublished departed membership authorized either native lifetime")
	}
	if path := os.Getenv("P25_RF_SOURCE_VECTOR"); path != "" {
		raw, err := json.MarshalIndent(first, "", "  ")
		if err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(path, append(raw, '\n'), 0600); err != nil {
			t.Fatalf("write requested cross-package source vector: %v", err)
		}
	}
}

func TestSourceAllowedRequiresCurrentNativeInhibitionProtection(t *testing.T) {
	c := sourceStateTestController(t, nil)
	control := sourceStateTestChild(t, c, "control", time.Now().UTC().Add(-time.Second))
	c.observe(control, "I: (HOST) P25 Affiliation, group affiliation, srcId = 10001, dstId = 1001")
	if !c.SourceAllowed(10001, 1001, control.started) {
		t.Fatal("current source missing")
	}
	c.mu.Lock()
	c.status.InhibitionReady = false
	c.mu.Unlock()
	if c.SourceAllowed(10001, 1001, control.started) || c.SourceState().Active {
		t.Fatal("unconfirmed native protection admitted proof")
	}
	c.mu.Lock()
	c.status.InhibitionReady = true
	c.status.Inhibition = []InhibitState{{RadioID: 10001, Blocked: true, PendingUninhibit: true}}
	c.mu.Unlock()
	if c.SourceAllowed(10001, 1001, control.started) {
		t.Fatal("pending native unblock admitted renewal")
	}
	c.mu.Lock()
	c.status.Inhibition = nil
	c.profile.Inhibition.Desired = []int{10001}
	c.mu.Unlock()
	if c.SourceAllowed(10001, 1001, control.started) {
		t.Fatal("current native desired inhibit admitted renewal")
	}
}

func TestSourceStateUnpublishedControlFactsSurviveReplacement(t *testing.T) {
	vectors := []SourceState{}
	for _, test := range []struct{ kind, line string }{
		{"registration", "I: (HOST) P25 Affiliation, unit registration, srcId = 10001"},
		{"null_affiliation", "I: (HOST) P25 Affiliation, group unaffiliation, srcId = 10001, dstId = 1001"},
		{"deregistration", "I: (HOST) P25 Affiliation, unit deregistration, srcId = 10001"},
	} {
		t.Run(test.kind, func(t *testing.T) {
			c := sourceStateTestController(t, nil)
			c.mu.Lock()
			c.profile.Site.ID = "site-b"
			c.status.SiteID = "site-b"
			c.mu.Unlock()
			old := sourceStateTestChild(t, c, "control", time.Now().UTC().Add(-time.Second))
			c.observe(old, test.line)
			c.mu.Lock()
			var at time.Time
			switch test.kind {
			case "registration":
				at = c.source.registrations[10001].At
			case "null_affiliation":
				at = c.source.nullAffiliations[10001].At
			case "deregistration":
				at = c.source.terminals[10001].At
			}
			c.mu.Unlock()
			current := sourceStateTestChild(t, c, "control", time.Now().UTC())
			state := c.SourceState()
			want := RetiredSourceFact{Kind: test.kind, RID: 10001, At: at, NativeStartedAt: old.started}
			if len(state.Retired) != 1 || state.Retired[0] != want || !state.NativeStartedAt.Equal(current.started) || len(state.Affiliations) != 0 || len(state.Registrations) != 0 || len(state.NullAffiliations) != 0 || len(state.Terminals) != 0 {
				t.Fatal("unpublished original control fact lost or rebound to new run", state, want)
			}
			vectors = append(vectors, state)
		})
	}
	if path := os.Getenv("P25_RF_RETIRED_VECTOR"); path != "" {
		raw, err := json.MarshalIndent(vectors, "", "  ")
		if err != nil {
			t.Fatal(err)
		}
		if err = os.WriteFile(path, append(raw, '\n'), 0600); err != nil {
			t.Fatal(err)
		}
	}
}
