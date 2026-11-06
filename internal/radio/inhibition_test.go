package radio

import (
	"context"
	"encoding/json"
	"regexp"
	"strconv"
	"strings"
	"testing"
)

func renderedInhibitFixture(config string) InhibitPolicy {
	p := InhibitPolicy{}
	start := strings.Index(config, "        inhibitPolicy:\n")
	if start < 0 {
		return p
	}
	value := config[start:]
	if end := regexp.MustCompile(`\n        [a-zA-Z]`).FindStringIndex(value); end != nil {
		value = value[:end[0]]
	}
	parts := strings.SplitN(value, "            restore:", 2)
	for _, row := range regexp.MustCompile(`radio_id: ([0-9]+)`).FindAllStringSubmatch(parts[0], -1) {
		rid, _ := strconv.Atoi(row[1])
		p.Desired = append(p.Desired, rid)
	}
	if len(parts) > 1 {
		for _, row := range regexp.MustCompile(`radio_id: ([0-9]+)\n                  blocked: (true|false)\n                  pending_uninhibit: (true|false)`).FindAllStringSubmatch(parts[1], -1) {
			rid, _ := strconv.Atoi(row[1])
			p.Restore = append(p.Restore, InhibitRestore{rid, row[2] == "true", row[3] == "true"})
		}
	}
	return p
}

func TestInhibitionValidationAndStartupRender(t *testing.T) {
	p := fixtureProfile()
	p.System.Radios = append(p.System.Radios, Radio{RID: 10003, Kind: "subscriber"})
	p.Inhibition = InhibitPolicy{Desired: []int{10001}, Restore: []InhibitRestore{{10003, true, true}}}
	if err := Validate(p); err != nil {
		t.Fatal(err)
	}
	for name, change := range map[string]func(*InhibitPolicy){"duplicate": func(v *InhibitPolicy) { v.Desired = []int{10001, 10001} }, "app": func(v *InhibitPolicy) { v.Desired = []int{10002} }, "unknown": func(v *InhibitPolicy) { v.Restore = []InhibitRestore{{10004, true, true}} }, "false recovery": func(v *InhibitPolicy) { v.Restore = []InhibitRestore{{10003, false, true}} }} {
		t.Run(name, func(t *testing.T) {
			q := cloneProfile(p)
			change(&q.Inhibition)
			if Validate(q) == nil {
				t.Fatal("unsafe policy accepted")
			}
		})
	}
	c := testController(t, nil)
	if err := c.Start(context.Background(), p, "null"); err != nil {
		t.Fatal(err)
	}
	state := c.Status()
	if !state.InhibitionReady || len(state.Inhibition) != 2 || !inhibitionBlocked(state.Inhibition, 10003) {
		t.Fatalf("startup did not restore actual protection: %+v", state)
	}
	if err := c.ApplyInhibitionPolicy(context.Background(), []int{}); err != nil {
		t.Fatal(err)
	}
	state = c.Status()
	if len(state.Inhibition) != 2 || !state.Inhibition[0].PendingUninhibit || !state.Inhibition[0].Blocked {
		t.Fatalf("desired omission falsely cleared actual state: %+v", state.Inhibition)
	}
	if err := c.Stop(context.Background()); err != nil {
		t.Fatal(err)
	}
	if err := c.ApplyInhibitionPolicy(context.Background(), []int{10003}); err != nil {
		t.Fatal(err)
	}
	if c.Status().State != "stopped" {
		t.Fatal("passive policy started RF")
	}
}

func TestInhibitSnapshotRequiresCompleteCurrentPhysicalEvidence(t *testing.T) {
	p := fixtureProfile()
	p.Inhibition.Desired = []int{10001}
	row := InhibitState{10001, true, true, false, "idle", 0}
	encode := func(rows any) map[string]json.RawMessage {
		raw, _ := json.Marshal(rows)
		return map[string]json.RawMessage{"record_count": []byte("1"), "record_limit": []byte("32"), "radios": raw}
	}
	if _, err := decodeInhibition(encode([]InhibitState{row}), p); err != nil {
		t.Fatal(err)
	}
	for _, rows := range []any{[]map[string]any{{"radio_id": 10001, "desired_inhibited": true}}, []InhibitState{{10001, true, false, false, "idle", 0}}, []InhibitState{{10002, true, true, false, "idle", 0}}, []InhibitState{{10001, false, true, true, "idle", 0}}} {
		if _, err := decodeInhibition(encode(rows), p); err == nil {
			t.Fatalf("invalid current state accepted: %+v", rows)
		}
	}
}

func TestAssignedWACNRangeIncludesEveryBEEPrefix(t *testing.T) {
	p := fixtureProfile()
	for wacn := 0xBEDFF; wacn <= 0xBEF00; wacn++ {
		p.System.WACN = wacn
		if err := Validate(p); err != nil {
			t.Fatalf("assigned WACN %05X rejected: %v", wacn, err)
		}
	}
	for _, wacn := range []int{1, 0xFFFFE} {
		p.System.WACN = wacn
		if err := Validate(p); err != nil {
			t.Fatalf("assigned boundary %05X rejected: %v", wacn, err)
		}
	}
	for _, wacn := range []int{-1, 0, 0xFFFFF, 0x100000} {
		p.System.WACN = wacn
		if Validate(p) == nil {
			t.Fatalf("reserved/out-of-range WACN %X accepted", wacn)
		}
	}
}

func TestInhibitCommandsRequireCurrentZoneIntentAndKeepPendingAfterQueue(t *testing.T) {
	c := testController(t, nil)
	if err := c.Start(context.Background(), fixtureProfile(), "null"); err != nil {
		t.Fatal(err)
	}
	if _, err := c.Command(context.Background(), Command{Action: "uninhibit", RID: 10001}); err == nil {
		t.Fatal("disconnected Site originated a new re-enable command")
	}
	c.SetZoneConnected(true)
	if _, err := c.Command(context.Background(), Command{Action: "inhibit", RID: 10001}); err == nil {
		t.Fatal("inhibit command ignored false Zone intent")
	}
	if _, err := c.Command(context.Background(), Command{Action: "uninhibit", RID: 10001}); err != nil {
		t.Fatal(err)
	}
	got := c.Status()
	if len(got.Inhibition) != 1 || !got.Inhibition[0].Blocked || !got.Inhibition[0].PendingUninhibit {
		t.Fatalf("queued re-enable falsely cleared local protection: %+v", got.Inhibition)
	}
	if err := c.ApplyInhibitionPolicy(context.Background(), []int{10001}); err != nil {
		t.Fatal(err)
	}
	if _, err := c.Command(context.Background(), Command{Action: "uninhibit", RID: 10001}); err == nil {
		t.Fatal("opposite old command ignored current desired inhibit")
	}
	if _, err := c.Command(context.Background(), Command{Action: "inhibit", RID: 10001}); err != nil {
		t.Fatal(err)
	}
	got = c.Status()
	if !got.Inhibition[0].Blocked || got.Inhibition[0].PendingUninhibit {
		t.Fatal("new inhibit choice retained opposite pending state")
	}
}

func TestInhibitionTerminalEventsRequireFNEAndCompleteAttemptCycle(t *testing.T) {
	for _, v := range []struct{ name, kind string }{{"Inhibit", "inhibit_no_response"}, {"Uninhibit", "uninhibit_no_response"}} {
		line := "I: (HOST) P25 Radio " + v.name + ", no response, arg = 16777212, tgt = 10001, attempts = 5"
		e := ParseEvent(line)
		if e == nil || e.Kind != v.kind || e.RID != 10001 || e.RequestorID != NativeFNEID || e.Attempts != 5 || e.Direction != "state" {
			t.Fatalf("terminal protocol observation lost: %+v", e)
		}
		if ParseEvent(strings.Replace(line, "attempts = 5", "attempts = 4", 1)) != nil || ParseEvent(strings.Replace(line, "16777212", "10002", 1)) != nil {
			t.Fatal("partial/foreign terminal line became an operation outcome")
		}
	}
}

func TestInhibitCurrentUnionAndTransportBoundsAreExplicit(t *testing.T) {
	radios := []Radio{}
	desired := []int{}
	restore := []InhibitRestore{}
	rows := []InhibitState{}
	for i := 0; i < InhibitRecordLimit; i++ {
		rid := 0xFFFFFB - i
		radios = append(radios, Radio{RID: rid, Kind: "subscriber"})
		desired = append(desired, rid)
		restore = append(restore, InhibitRestore{rid, true, false})
		rows = append(rows, InhibitState{rid, true, true, false, "acknowledged", 5})
	}
	if err := ValidateInhibition(radios, InhibitPolicy{Desired: desired, Restore: restore}); err != nil {
		t.Fatal(err)
	}
	request := inhibitionRequest(desired)
	request.ControlState = &restore
	body, err := json.Marshal(request)
	if err != nil || len(body) > 4096 {
		t.Fatalf("bounded native request exceeds parser limit: %d %v", len(body), err)
	}
	reply, err := json.Marshal(inhibitReply(rows))
	if err != nil || len(reply) > 16384 {
		t.Fatalf("bounded native reply exceeds parser limit: %d %v", len(reply), err)
	}
	radios = append(radios, Radio{RID: 1, Kind: "subscriber"})
	if ValidateInhibition(radios, InhibitPolicy{Desired: desired, Restore: []InhibitRestore{{1, true, true}}}) == nil {
		t.Fatal("33-current-record union was silently truncated or accepted")
	}
	if ValidateInhibition(radios, InhibitPolicy{Desired: append(desired, 1)}) == nil {
		t.Fatal("33 desired rows were silently truncated or accepted")
	}
}
