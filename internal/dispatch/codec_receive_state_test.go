package dispatch

import (
	"encoding/binary"
	"testing"
	"time"
)

func receiveVoice(secure bool) Frame {
	f := Frame{CallID: "native-token", SourceID: 10001, TalkgroupID: 1001, Sequence: 1, P25: testP25Metadata(0), AlgorithmID: 0x80, Encoded: make([]byte, 11)}
	if secure {
		f.Encrypted = true
		f.ServiceOptions = 0x44
		f.AlgorithmID = 0x84
		f.KeyID = 7
		f.MessageIndicator = []byte{1, 2, 3, 4, 5, 6, 7, 8, 9}
	}
	return f
}
func receiveLive(f Frame, sequence uint64, remaining time.Duration) opaqueLiveness {
	return opaqueLiveness{CodecReceiveActivity: CodecReceiveActivity{nativeID: f.CallID, SourceID: f.SourceID, TalkgroupID: f.TalkgroupID, ManufacturerID: f.ManufacturerID, Emergency: f.Emergency, ServiceOptions: f.ServiceOptions, Encrypted: f.Encrypted, AlgorithmID: f.AlgorithmID, KeyID: f.KeyID, LastVoiceSequence: f.Sequence, ControlSequence: sequence}, remaining: remaining}
}

func TestCodecOpaqueLivenessUsesSeparateSequencesAndBoundedRealVoice(t *testing.T) {
	for _, secure := range []bool{false, true} {
		t.Run(map[bool]string{false: "clear", true: "encrypted"}[secure], func(t *testing.T) {
			s := opaqueReceiveState{}
			start := time.Now()
			f := receiveVoice(secure)
			first, err := s.voice(f, start)
			if err != nil || len(first) != 1 {
				t.Fatal("voice did not create its application owner", err)
			}
			id := first[0].CallID
			for i := 1; i <= 14; i++ {
				now := start.Add(time.Duration(i) * 100 * time.Millisecond)
				a, ok := s.activity(receiveLive(f, uint64(i), OpaqueReceiveBudget), now)
				if !ok || a.CallID != id || a.LastVoiceSequence != f.Sequence || a.LastMediaAt != start || a.ValidUntil.After(start.Add(OpaqueReceiveBudget)) {
					t.Fatal("activity lost ownership, voice cadence or hard bound", i, a)
				}
			}
			if _, ok := s.expire(start.Add(OpaqueReceiveBudget - time.Nanosecond)); ok {
				t.Fatal("fresh bounded activity ended too soon")
			}
			ended, ok := s.expire(start.Add(OpaqueReceiveBudget))
			if !ok || !ended.End || ended.CallID != id {
				t.Fatal("activity held a call beyond its last-real-voice budget")
			}
			if _, ok := s.activity(receiveLive(f, 15, OpaqueReceiveBudget), start.Add(1600*time.Millisecond)); ok {
				t.Fatal("activity resumed an ended call")
			}
			// A genuinely new voice word may recover with a fresh application ID;
			// it must not inherit retirement of the expired stream.
			f.Sequence++
			resumed, err := s.voice(f, start.Add(1600*time.Millisecond))
			if err != nil || len(resumed) != 1 || resumed[0].CallID == id {
				t.Fatal("real voice recovery reused a retired application owner")
			}
			if resumed[0].Sequence != 2 {
				t.Fatal("control packets consumed voice sequence numbers")
			}
		})
	}
}

func TestCodecOpaqueActivityCannotAuthorizeOrReplayAStream(t *testing.T) {
	mutations := map[string]func(*opaqueLiveness){
		"token": func(a *opaqueLiveness) { a.nativeID = "different" }, "source": func(a *opaqueLiveness) { a.SourceID++ }, "group": func(a *opaqueLiveness) { a.TalkgroupID++ }, "manufacturer": func(a *opaqueLiveness) { a.ManufacturerID = 1 }, "security": func(a *opaqueLiveness) { a.Encrypted = false; a.AlgorithmID = 0x80; a.KeyID = 0 }, "key": func(a *opaqueLiveness) { a.KeyID++ }, "last voice": func(a *opaqueLiveness) { a.LastVoiceSequence++ },
	}
	for name, change := range mutations {
		t.Run(name, func(t *testing.T) {
			s := opaqueReceiveState{}
			now := time.Now()
			f := receiveVoice(true)
			s.voice(f, now)
			until := s.until
			a := receiveLive(f, 1, time.Second)
			change(&a)
			if _, ok := s.activity(a, now.Add(100*time.Millisecond)); ok || s.until != until {
				t.Fatal("unrelated activity changed receive ownership or deadline")
			}
		})
	}
	s := opaqueReceiveState{}
	now := time.Now()
	f := receiveVoice(false)
	a := receiveLive(f, 1, time.Second)
	if _, ok := s.activity(a, now); ok {
		t.Fatal("activity created a call")
	}
	s.voice(f, now)
	if _, ok := s.activity(a, now.Add(100*time.Millisecond)); !ok {
		t.Fatal("matching activity rejected")
	}
	until := s.until
	if _, ok := s.activity(a, now.Add(200*time.Millisecond)); ok || s.until != until {
		t.Fatal("duplicate activity extended the deadline")
	}
	a.ControlSequence = 2
	if _, ok := s.activity(a, until); ok {
		t.Fatal("late activity resurrected an expired receiver")
	}
}

func TestCodecOpaqueVoiceAndEndCannotChangeAnAcceptedGroup(t *testing.T) {
	for _, secure := range []bool{false, true} {
		s := opaqueReceiveState{}
		now := time.Now()
		f := receiveVoice(secure)
		out, err := s.voice(f, now)
		if err != nil || len(out) != 1 {
			t.Fatal("initial owner missing")
		}
		id, until := s.call, s.until
		f.Sequence++
		f.TalkgroupID++
		if out, _ = s.voice(f, now.Add(100*time.Millisecond)); len(out) != 0 || s.call != id || s.until != until {
			t.Fatal("same-token voice changed the accepted group or deadline")
		}
		f.Encoded, f.End = nil, true
		if out, _ = s.voice(f, now.Add(200*time.Millisecond)); len(out) != 0 || s.call != id {
			t.Fatal("foreign-group End retired the current owner")
		}
		if _, ok := s.expire(until); !ok {
			t.Fatal("expected bounded missing-End expiry")
		}
		if out, _ = s.voice(f, now.Add(400*time.Millisecond)); len(out) != 0 {
			t.Fatal("retired End was emitted")
		}
		f.End, f.Encoded = false, make([]byte, 11)
		if out, _ = s.voice(f, now.Add(450*time.Millisecond)); len(out) != 0 {
			t.Fatal("same token resumed in another group")
		}
		f.TalkgroupID--
		if out, _ = s.voice(f, now.Add(500*time.Millisecond)); len(out) != 1 || out[0].CallID == id {
			t.Fatal("unrelated terminal prevented legitimate same-owner voice recovery")
		}
	}
}

func TestCodecOpaqueEndAndTokenTransitionsKeepExactOwners(t *testing.T) {
	s := opaqueReceiveState{}
	now := time.Now()
	f := receiveVoice(true)
	frames, _ := s.voice(f, now)
	oldID := frames[0].CallID
	end := f
	end.End = true
	end.Encoded = nil
	end.P25 = nil
	end.MessageIndicator = nil
	if frames, _ := s.voice(end, now); len(frames) != 0 {
		t.Fatal("stale same-token End retired current voice")
	}
	next := f
	next.CallID = "replacement-token"
	frames, _ = s.voice(next, now.Add(10*time.Millisecond))
	if len(frames) != 2 || !frames[0].End || frames[0].CallID != oldID || frames[1].CallID == oldID {
		t.Fatal("token replacement did not retire its previous owner once")
	}
	id := frames[1].CallID
	end.Sequence++
	if frames, _ := s.voice(end, now.Add(20*time.Millisecond)); len(frames) != 0 {
		t.Fatal("old token End retired its replacement")
	}
	if frames, _ := s.voice(f, now.Add(30*time.Millisecond)); len(frames) != 0 {
		t.Fatal("delayed old token voice replaced a current stream")
	}
	next.Sequence++
	next.End = true
	next.Encoded = nil
	next.P25 = nil
	next.MessageIndicator = nil
	// LDU2 may already have announced the next encryption profile. End is
	// matched by native owner and forward voice sequence, never MI equality.
	next.AlgorithmID = 0x81
	next.KeyID = 8
	frames, _ = s.voice(next, now.Add(40*time.Millisecond))
	if len(frames) != 1 || !frames[0].End || frames[0].CallID != id {
		t.Fatal("forward matching End did not retire its owner")
	}
	if !frames[0].Encrypted || frames[0].AlgorithmID != 0x84 || frames[0].KeyID != 7 {
		t.Fatal("terminal lost the accepted public security profile required by its admission")
	}
	if frames, _ := s.voice(next, now.Add(50*time.Millisecond)); len(frames) != 0 {
		t.Fatal("duplicate terminal emitted another End")
	}
}

func TestCodecOpaqueTimeoutRecoveryCannotDisplaceAReplacementToken(t *testing.T) {
	s := opaqueReceiveState{}
	now := time.Now()
	a := receiveVoice(false)
	s.voice(a, now)
	s.expire(now.Add(OpaqueReceiveFreshness))
	b := a
	b.CallID = "replacement-after-timeout"
	frames, _ := s.voice(b, now.Add(400*time.Millisecond))
	if len(frames) != 1 {
		t.Fatal("replacement native stream not admitted")
	}
	id := frames[0].CallID
	a.Sequence = 30
	if frames, _ := s.voice(a, now.Add(410*time.Millisecond)); len(frames) != 0 || s.call != id {
		t.Fatal("late higher-sequence voice from an expired native owner displaced its replacement")
	}
	a.End = true
	a.Encoded = nil
	a.P25 = nil
	if frames, _ := s.voice(a, now.Add(420*time.Millisecond)); len(frames) != 0 || s.call != id {
		t.Fatal("old terminal retired the replacement")
	}
	if previous := s.retired[opaqueIdentity{a.CallID, a.SourceID}]; !previous.terminal || previous.sequence != a.Sequence {
		t.Fatal("old terminal did not finalize only its own retained history")
	}
}

func TestCodecOpaqueControlWireRejectsMalformedAndCannotBecomeAudio(t *testing.T) {
	f := receiveVoice(true)
	packet, err := EncodeOpaqueCodecPacket(f)
	if err != nil {
		t.Fatal(err)
	}
	h := append([]byte{}, packet[:64]...)
	h[4], h[7] = 17, 0
	binary.BigEndian.PutUint16(h[58:60], 0)
	binary.BigEndian.PutUint32(h[20:24], 1400)
	binary.BigEndian.PutUint64(h[24:32], 1)
	binary.BigEndian.PutUint64(h[48:56], f.Sequence)
	h[56] = 0
	if _, err := decodeOpaqueLiveness(h); err != nil {
		t.Fatal("valid metadata-only packet rejected", err)
	}
	if _, err := DecodeOpaqueCodecPacket(h); err == nil {
		t.Fatal("receive activity was admitted as audio")
	}
	changes := map[string]func([]byte){"operation": func(b []byte) { b[4] = 16 }, "status": func(b []byte) { b[5] = 1 }, "flags": func(b []byte) { b[11] = 1 }, "voice index": func(b []byte) { b[7] = 1 }, "zero control": func(b []byte) { binary.BigEndian.PutUint64(b[24:32], 0) }, "zero voice": func(b []byte) { binary.BigEndian.PutUint64(b[48:56], 0) }, "zero budget": func(b []byte) { binary.BigEndian.PutUint32(b[20:24], 0) }, "excess budget": func(b []byte) { binary.BigEndian.PutUint32(b[20:24], 1501) }, "reserved": func(b []byte) { b[61] = 1 }, "emergency": func(b []byte) { b[60] = 2 }, "payload": func(b []byte) { b[59] = 11 }, "manufacturer": func(b []byte) { b[57] = 2 }, "algorithm": func(b []byte) { b[6] = 0x55 }, "source": func(b []byte) { binary.BigEndian.PutUint32(b[12:16], 0) }, "group": func(b []byte) { binary.BigEndian.PutUint32(b[16:20], 0) }}
	for name, change := range changes {
		t.Run(name, func(t *testing.T) {
			b := append([]byte{}, h...)
			change(b)
			if _, err := decodeOpaqueLiveness(b); err == nil {
				t.Fatal("malformed activity accepted")
			}
		})
	}
	if _, err := decodeOpaqueLiveness(h[:63]); err == nil {
		t.Fatal("short activity accepted")
	}
	if _, err := decodeOpaqueLiveness(append(h, 0)); err == nil {
		t.Fatal("activity payload accepted")
	}
}

func TestCodecOpaqueSecurityChangeDoesNotExtendCurrentCall(t *testing.T) {
	s := opaqueReceiveState{}
	now := time.Now()
	f := receiveVoice(true)
	s.voice(f, now)
	until := s.until
	f.Sequence++
	f.KeyID++
	if frames, _ := s.voice(f, now.Add(100*time.Millisecond)); len(frames) != 0 || s.until != until {
		t.Fatal("unannounced security change advanced current receive state")
	}
}

func TestCodecPCMIdleStillEndsWithUnrelatedActivityTraffic(t *testing.T) {
	c := shutdownCodec(t)
	r := shutdownResource(t)
	c.frames = make(chan codecReceiveUpdate, 32)
	c.wg.Add(1)
	go c.receive(r)
	t.Cleanup(func() { c.cancel(); r.read.Close(); c.wg.Wait() })
	packet, err := EncodeCodecPacket(Frame{SourceID: 10001, TalkgroupID: 1001, PCM: make([]byte, FrameBytes)})
	if err != nil {
		t.Fatal(err)
	}
	if _, err = r.write.Write(packet); err != nil {
		t.Fatal(err)
	}
	var first Frame
	select {
	case update := <-c.frames:
		first = update.frame
	case <-time.After(time.Second):
		t.Fatal("PCM frame missing")
	}
	if len(first.PCM) != FrameBytes || first.End {
		t.Fatal("PCM receive changed")
	}
	for _, sample := range first.PCM {
		if sample != 0 {
			t.Fatal("zero PCM input changed")
		}
	}
	f := receiveVoice(false)
	encoded, err := EncodeOpaqueCodecPacket(f)
	if err != nil {
		t.Fatal(err)
	}
	h := append([]byte{}, encoded[:64]...)
	h[4], h[7] = 17, 0
	binary.BigEndian.PutUint16(h[58:60], 0)
	binary.BigEndian.PutUint32(h[20:24], 1500)
	binary.BigEndian.PutUint64(h[48:56], 1)
	h[56] = 0
	start := time.Now()
	ticker := time.NewTicker(10 * time.Millisecond)
	defer ticker.Stop()
	deadline := time.NewTimer(time.Second)
	defer deadline.Stop()
	var sequence uint64
	for {
		select {
		case <-ticker.C:
			sequence++
			binary.BigEndian.PutUint64(h[24:32], sequence)
			if _, err = r.write.Write(h); err != nil {
				t.Fatal(err)
			}
		case update := <-c.frames:
			if update.activity != nil || !update.frame.End || update.frame.CallID != first.CallID {
				t.Fatal("unrelated opaque metadata changed PCM ownership")
			}
			if elapsed := time.Since(start); elapsed < 300*time.Millisecond || elapsed > 600*time.Millisecond {
				t.Fatalf("PCM350ms idle behavior changed: %v", elapsed)
			}
			return
		case <-deadline.C:
			t.Fatal("metadata kept PCM alive without voice")
		}
	}
}

func TestOpaqueEmergencyReceiveActivityAndEndKeepVoiceOwner(t *testing.T) {
	for _, secure := range []bool{false, true} {
		var state opaqueReceiveState
		now := time.Now()
		f := receiveVoice(secure)
		f = f.WithEmergency(true)
		voice, err := state.voice(f, now)
		if err != nil || len(voice) != 1 || !voice[0].Emergency {
			t.Fatal("voice emergency lost", voice, err)
		}
		a := receiveLive(f, 1, time.Second)
		a.Emergency = false
		until := state.until
		if _, ok := state.activity(a, now.Add(100*time.Millisecond)); ok || state.until != until {
			t.Fatal("activity changed observed emergency")
		}
		a.Emergency = true
		if got, ok := state.activity(a, now.Add(100*time.Millisecond)); !ok || !got.Emergency {
			t.Fatal("matching activity lost emergency")
		}
		f.Sequence++
		f.End = true
		f.Encoded = nil
		f.P25 = nil
		f = f.WithEmergency(false)
		ended, err := state.voice(f, now.Add(110*time.Millisecond))
		if err != nil || len(ended) != 1 || !ended[0].End || !ended[0].Emergency {
			t.Fatal("terminal changed accepted voice metadata", ended, err)
		}
		if _, ok := state.activity(a, now.Add(120*time.Millisecond)); ok {
			t.Fatal("ended activity resurrected receiver")
		}
		f = receiveVoice(secure)
		f = f.WithEmergency(true)
		f.CallID = "replacement-emergency"
		if _, err = state.voice(f, now.Add(200*time.Millisecond)); err != nil {
			t.Fatal(err)
		}
		timed, ok := state.expire(now.Add(200*time.Millisecond + OpaqueReceiveFreshness))
		if !ok || !timed.End || !timed.Emergency {
			t.Fatal("timeout lost owned emergency", timed)
		}
	}
}
