package rtpvoice

import (
	"reflect"
	"testing"
	"time"
	"open25.local/open25-fne/internal/p25rtp"
)

func TestEngineRequestWaitGrantTimers(t *testing.T) {
	at := time.Unix(100, 0)
	state, packets := newEngineState(p25rtp.GroupServing, p25rtp.GroupServing, at)
	if !reflect.DeepEqual(packets, []p25rtp.PacketType{p25rtp.Request}) || !state.requestUntil.Equal(at.Add(time.Second)) {
		t.Fatal("initial Request/default deadline")
	}
	if got := state.tick(at.Add(500 * time.Millisecond)); !reflect.DeepEqual(got, []p25rtp.PacketType{p25rtp.Request}) {
		t.Fatal("Request500ms retry", got)
	}
	state.control(p25rtp.Wait, at.Add(600*time.Millisecond))
	if state.phase != phaseWaiting || !state.requestUntil.IsZero() || !state.requestDue.IsZero() || !state.waitUntil.Equal(at.Add(5600*time.Millisecond)) {
		t.Fatal("B2 Wait transition")
	}
	state.control(p25rtp.Wait, at.Add(time.Second))
	if !state.waitUntil.Equal(at.Add(5600 * time.Millisecond)) {
		t.Fatal("duplicate Wait extended timer")
	}
	state.control(p25rtp.Grant, at.Add(1200*time.Millisecond))
	if !state.ready || state.phase != phaseActive || !state.waitUntil.IsZero() {
		t.Fatal("Grant did not cancel Wait")
	}
	state.control(p25rtp.Wait, at.Add(1300*time.Millisecond))
	if state.phase != phaseActive {
		t.Fatal("late Wait changed granted owner")
	}
	state.progress(at.Add(1400 * time.Millisecond))
	state.tick(at.Add(1599 * time.Millisecond))
	if state.phase != phaseActive {
		t.Fatal("early media loss")
	}
	state.tick(at.Add(1600 * time.Millisecond))
	if state.reason != ReasonMediaLoss {
		t.Fatal("200ms media loss", state.reason)
	}
}
func TestEngineWaitExpiryAndRequestTimeoutFailClosed(t *testing.T) {
	at := time.Unix(100, 0)
	state, _ := newEngineState(p25rtp.GroupServing, p25rtp.GroupServing, at)
	state.control(p25rtp.Wait, at.Add(10*time.Millisecond))
	got := state.tick(at.Add(5010 * time.Millisecond))
	if !reflect.DeepEqual(got, []p25rtp.PacketType{p25rtp.Request}) || state.phase != phaseRequesting || !state.requestUntil.Equal(at.Add(6010*time.Millisecond)) {
		t.Fatal("Wait5s did not re-request", got)
	}
	state.tick(at.Add(6010 * time.Millisecond))
	if state.phase != phaseEnding || state.ready || state.reason != ReasonRequestTimeout {
		t.Fatal("Request timeout self-granted or remained active")
	}
}
func TestEngineLostFirstGrantKeepsStableOneSecondHomeWait(t *testing.T) {
	at := time.Unix(100, 0)
	home, _ := newEngineState(p25rtp.GroupHome, p25rtp.GroupServing, at)
	first := home.control(p25rtp.Request, at)
	duplicate := home.control(p25rtp.Request, at.Add(500*time.Millisecond))
	if !reflect.DeepEqual(first, []p25rtp.PacketType{p25rtp.Grant}) || !reflect.DeepEqual(first, duplicate) || !home.mediaUntil.Equal(at.Add(time.Second)) {
		t.Fatal("lost-Grant retry changed decision or extended owner")
	}
	home.tick(at.Add(500 * time.Millisecond))
	if home.phase != phaseActive {
		t.Fatal("Home expired at Serving retry boundary")
	}
	home.progress(at.Add(520 * time.Millisecond))
	if !home.mediaUntil.Equal(at.Add(720 * time.Millisecond)) {
		t.Fatal("Progress not200ms")
	}
	denied, _ := newEngineState(p25rtp.GroupServing, p25rtp.GroupServing, at)
	denied.control(p25rtp.Deny, at)
	denied.control(p25rtp.Grant, at.Add(time.Millisecond))
	if denied.phase != phaseEnding || denied.ready {
		t.Fatal("late Grant revived denied owner")
	}
}
func TestEngineStartRepeatsAndEndCancelOnlySpurtTimers(t *testing.T) {
	at := time.Unix(100, 0)
	state, first := newEngineState(p25rtp.GroupHome, p25rtp.GroupHome, at)
	if !reflect.DeepEqual(first, []p25rtp.PacketType{p25rtp.HeartbeatQuery, p25rtp.Start}) {
		t.Fatal("Home Query/Start", first)
	}
	starts := 1
	for step := 1; step <= 54; step++ {
		now := at.Add(time.Duration(step) * 20 * time.Millisecond)
		state.progress(now)
		for _, kind := range state.tick(now) {
			if kind == p25rtp.Start {
				starts++
			}
		}
	}
	if starts != 4 || state.startsLeft != 0 {
		t.Fatal("four360ms Start copies", starts)
	}
	state.ending(ReasonLocalEnd)
	if !state.mediaUntil.IsZero() || !state.startDue.IsZero() {
		t.Fatal("End left spurt timers active")
	}
	if got := state.tick(at.Add(10 * time.Second)); !reflect.DeepEqual(got, []p25rtp.PacketType{p25rtp.Heartbeat}) {
		t.Fatal("session heartbeat was confused with spurt lifetime", got)
	}
}
func TestEngineFirstPacketAndHeartbeatLifetimes(t *testing.T) {
	at := time.Unix(100, 0)
	state, _ := newEngineState(p25rtp.GroupServing, p25rtp.GroupHome, at)
	state.control(p25rtp.Start, at)
	state.tick(at.Add(499 * time.Millisecond))
	if state.phase != phaseActive {
		t.Fatal("early first packet expiry")
	}
	state.tick(at.Add(500 * time.Millisecond))
	if state.reason != ReasonFirstMedia {
		t.Fatal("first packet500ms", state.reason)
	}
	session, _ := newEngineState(p25rtp.GroupServing, p25rtp.GroupHome, at)
	if got := session.heartbeat(p25rtp.HeartbeatQuery, 10, at.Add(time.Second)); !reflect.DeepEqual(got, []p25rtp.PacketType{p25rtp.Heartbeat}) {
		t.Fatal("Query did not get immediate Heartbeat")
	}
	session.heartbeat(p25rtp.Heartbeat, 0, at.Add(30*time.Second))
	if !session.heartbeatSeen.Equal(at.Add(time.Second)) {
		t.Fatal("invalid interval kept session alive")
	}
	session.control(p25rtp.Start, at)
	// Healthy actual voice never substitutes for the separate session heartbeat.
	for step := 0; step <= 2050; step++ {
		now := at.Add(time.Duration(step) * 20 * time.Millisecond)
		session.progress(now)
		session.tick(now)
	}
	session.tick(at.Add(41*time.Second + time.Nanosecond))
	if session.reason != ReasonHeartbeat {
		t.Fatal("heartbeat loss not separate")
	}
}

func TestEngineInitialControlGuards(t *testing.T) {
	at := time.Unix(100, 0)
	for _, test := range []struct {
		name          string
		local, origin p25rtp.Role
		limit         time.Duration
	}{{"HomeRequest", p25rtp.GroupHome, p25rtp.GroupServing, time.Second}, {"ServingStart", p25rtp.GroupServing, p25rtp.GroupHome, 500 * time.Millisecond}} {
		t.Run(test.name, func(t *testing.T) {
			state, _ := newEngineState(test.local, test.origin, at)
			state.tick(at.Add(test.limit - time.Nanosecond))
			if state.phase != phaseAwaiting {
				t.Fatal("early control timeout")
			}
			state.tick(at.Add(test.limit))
			if state.phase != phaseEnding || state.reason != ReasonFirstMedia || state.ready {
				t.Fatal("missing control retained owner or invented readiness")
			}
		})
	}
}
