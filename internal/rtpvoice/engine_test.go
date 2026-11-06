package rtpvoice

import (
	"bytes"
	"context"
	"encoding/hex"
	"errors"
	"io"
	"net/netip"
	"reflect"
	"sync"
	"testing"
	"time"
	"open25.local/open25-fne/internal/p25rtp"
)

var engineFrameTypes = [18]p25rtp.FrameType{p25rtp.FR1, p25rtp.FR2, p25rtp.FR3, p25rtp.FR4, p25rtp.FR5, p25rtp.FR6, p25rtp.FR7, p25rtp.FR8, p25rtp.FR9, p25rtp.FR10, p25rtp.FR11, p25rtp.FR12, p25rtp.FR13, p25rtp.FR14, p25rtp.FR15, p25rtp.FR16, p25rtp.FR17, p25rtp.FR18}

// Literal structural fixture independently derived from the original BACA
// diagrams in the sealed corpus review. It is not a valid encrypted/FEC vector.
const engineLiteralPacket = "80641234010203040000000204810000048b000008850000128000000e00c4040000001001002711040123456789abcdef018412340003e9293010c2123456789abcb4715990d5aea4"

func engineFixture(t *testing.T, origin p25rtp.Role) Spurt {
	t.Helper()
	raw, err := hex.DecodeString(engineLiteralPacket)
	if err != nil {
		t.Fatal(err)
	}
	p, err := p25rtp.Parse(raw)
	if err != nil {
		t.Fatal(err)
	}
	tsn := uint8(2)
	if origin == p25rtp.GroupHome {
		tsn = 1
	}
	return Spurt{Origin: origin, TSN: tsn, GroupID: p.Header.GroupID, ServiceOptions: p.Control.ServiceOptions, RF: p.RF}
}
func engineFrame(t *testing.T, position uint64) Frame {
	t.Helper()
	raw, _ := hex.DecodeString("c2123456789abcb4715990d5aea4")
	v, err := p25rtp.ParseVoice(raw)
	if err != nil {
		t.Fatal(err)
	}
	v.Type = engineFrameTypes[position%18]
	v.Quality.Superframe = uint8(position / 18 % 4)
	if v.Type == p25rtp.FR9 || v.Type == p25rtp.FR18 {
		v.LSD = &p25rtp.LowSpeedData{Words: [2]byte{0xa5, 0x5a}, Status: [2]uint8{1, 2}}
	}
	if position%18 >= 11 && position%18 <= 16 {
		v.ES = &p25rtp.EncryptionSync{Words: [4]uint8{1, 2, 3, 4}, Status: 59}
	}
	return Frame{Position: position, Voice: v}
}
func engineSessionPair(t *testing.T) (*Session, *Session) {
	t.Helper()
	local := netip.MustParseAddrPort("127.0.0.1:0")
	homePair, err := Listen(local)
	if err != nil {
		t.Fatal(err)
	}
	servingPair, err := Listen(local)
	if err != nil {
		homePair.Close()
		t.Fatal(err)
	}
	home, err := New(homePair, Options{Remote: servingPair.LocalRTP(), LocalRole: p25rtp.GroupHome, RemoteRole: p25rtp.GroupServing})
	if err != nil {
		t.Fatal(err)
	}
	serving, err := New(servingPair, Options{Remote: homePair.LocalRTP(), LocalRole: p25rtp.GroupServing, RemoteRole: p25rtp.GroupHome})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { home.Close(); serving.Close() })
	return home, serving
}
func engineStart(t *testing.T, session *Session, spurt Spurt) (*Engine, <-chan error) {
	t.Helper()
	e, err := NewEngine(session, spurt)
	if err != nil {
		t.Fatal(err)
	}
	done := make(chan error, 1)
	go func() { done <- e.Run(t.Context()) }()
	t.Cleanup(func() {
		e.Close()
		select {
		case <-e.Done():
		case <-time.After(time.Second):
			t.Error("engine worker survivedClose")
		}
	})
	return e, done
}
func engineReady(t *testing.T, e *Engine) {
	t.Helper()
	select {
	case <-e.Ready():
	case <-e.Done():
		t.Fatal("engine stopped before ready")
	case <-time.After(2 * time.Second):
		t.Fatal("engine did not become ready")
	}
}
func engineDrain(t *testing.T, e *Engine) []Delivery {
	t.Helper()
	ctx, cancel := context.WithTimeout(t.Context(), 2*time.Second)
	defer cancel()
	var out []Delivery
	for {
		d, err := e.Receive(ctx)
		if errors.Is(err, io.EOF) {
			return out
		}
		if err != nil {
			t.Fatal(err)
		}
		out = append(out, d)
	}
}
func engineRaw(t *testing.T, source *Session, p p25rtp.Packet) {
	t.Helper()
	raw, err := p25rtp.Marshal(p)
	if err != nil {
		t.Fatal(err)
	}
	if _, err = source.pair.rtp.WriteToUDPAddrPort(raw, source.options.Remote); err != nil {
		t.Fatal(err)
	}
}
func enginePacket(spurt Spurt, kind p25rtp.PacketType, sequence uint16) p25rtp.Packet {
	p := p25rtp.Packet{Role: spurt.Origin, Sequence: sequence, Control: p25rtp.PacketTypeControl{Type: kind, TSN: spurt.TSN, ServiceOptions: spurt.ServiceOptions}}
	if kind == p25rtp.Request || kind == p25rtp.Start || kind == p25rtp.Progress {
		p.RF = spurt.RF
		p.Console = spurt.Console
	}
	return p
}
func engineVoicePacket(spurt Spurt, frame Frame, sequence uint16, timestamp uint32) p25rtp.Packet {
	p := enginePacket(spurt, p25rtp.Progress, sequence)
	p.Timestamp = timestamp
	p.Header = frame.Header
	p.Voice = &frame.Voice
	return p
}
func TestEngineLiteralMetadataPacingAndOrderlyEnd(t *testing.T) {
	for _, origin := range []p25rtp.Role{p25rtp.GroupHome, p25rtp.GroupServing} {
		t.Run(map[p25rtp.Role]string{1: "home", 2: "serving"}[origin], func(t *testing.T) {
			home, serving := engineSessionPair(t)
			spurt := engineFixture(t, origin)
			senderSession, receiverSession := home, serving
			if origin == p25rtp.GroupServing {
				senderSession, receiverSession = serving, home
			}
			receiver, receiverDone := engineStart(t, receiverSession, spurt)
			sender, senderDone := engineStart(t, senderSession, spurt)
			engineReady(t, sender)
			engineReady(t, receiver)
			literal, _ := hex.DecodeString(engineLiteralPacket)
			parsed, _ := p25rtp.Parse(literal)
			originals := []Frame{engineFrame(t, 18), engineFrame(t, 19), engineFrame(t, 20)}
			originals[0].Header = parsed.Header
			for _, f := range originals {
				if err := sender.Push(f); err != nil {
					t.Fatal(err)
				}
			}
			// Caller-owned metadata may be reused/mutated immediately afterPush.
			parsed.Header.MFID ^= 0xff
			end := make(chan error, 1)
			go func() {
				ctx, cancel := context.WithTimeout(t.Context(), time.Second)
				defer cancel()
				end <- sender.End(ctx)
			}()
			deliveries := engineDrain(t, receiver)
			if len(deliveries) != 4 {
				t.Fatalf("ordered3frames+End got%#v", deliveries)
			}
			for i := 0; i < 3; i++ {
				if deliveries[i].Kind != FrameDelivery || deliveries[i].Position != uint64(18+i) || !reflect.DeepEqual(deliveries[i].Frame.Voice, originals[i].Voice) {
					t.Fatal("media/receiver reports changed")
				}
			}
			if deliveries[0].Frame.Header == nil || deliveries[0].Frame.Header.Superframe != 0 || deliveries[0].Frame.Voice.Quality.Superframe != 1 || deliveries[0].Frame.Header.MFID == parsed.Header.MFID {
				t.Fatal("header ownership/initial SF was invented or aliased")
			}
			if deliveries[3].Kind != EndDelivery || deliveries[3].Reason != ReasonRemoteEnd {
				t.Fatal("End overtook media")
			}
			if err := <-end; err != nil {
				t.Fatal(err)
			}
			if err := <-senderDone; err != nil {
				t.Fatal(err)
			}
			if err := <-receiverDone; err != nil {
				t.Fatal(err)
			}
			if sender.Stats().SentFrames != 3 || receiver.Stats().IPGaps != 0 || len(receiver.rxTokens) != 0 {
				t.Fatal("frame/token accounting")
			}
		})
	}
}
func TestEngineControlSequenceIsNotVoiceClockAndWraps(t *testing.T) {
	home, peer := engineSessionPair(t)
	spurt := engineFixture(t, p25rtp.GroupHome)
	e, err := NewEngine(home, spurt)
	if err != nil {
		t.Fatal(err)
	}
	e.timestamp = 0xffffffe0
	home.next = 65534
	done := make(chan error, 1)
	go func() { done <- e.Run(t.Context()) }()
	defer e.Close()
	engineReady(t, e)
	for position := uint64(18); position < 21; position++ {
		if err = e.Push(engineFrame(t, position)); err != nil {
			t.Fatal(err)
		}
	}
	ended := make(chan error, 1)
	go func() {
		ctx, cancel := context.WithTimeout(t.Context(), time.Second)
		defer cancel()
		ended <- e.End(ctx)
	}()
	ctx, cancel := context.WithTimeout(t.Context(), time.Second)
	defer cancel()
	var packets []p25rtp.Packet
	var times []time.Time
	ends := 0
	for ends < 4 {
		p, err := peer.Receive(ctx)
		if err != nil {
			t.Fatal(err)
		}
		packets = append(packets, p)
		if p.Voice != nil {
			times = append(times, time.Now())
		}
		if p.Control.Type == p25rtp.End {
			ends++
		}
	}
	var voice []p25rtp.Packet
	for i, p := range packets {
		if i > 0 && p.Sequence != packets[i-1].Sequence+1 {
			t.Fatal("packet sequence skipped a control or wrap")
		}
		if p.Voice != nil {
			voice = append(voice, p)
		}
	}
	if len(voice) != 3 || voice[0].Timestamp != 0xffffffe0 || voice[1].Timestamp != 128 || voice[2].Timestamp != 288 {
		t.Fatal("voice timestamp160/wrap was inferred from control sequence", voice)
	}
	for i := 1; i < len(times); i++ {
		if gap := times[i].Sub(times[i-1]); gap < 12*time.Millisecond || gap > 90*time.Millisecond {
			t.Fatal("unpaced/burst voice", gap)
		}
	}
	if err = <-ended; err != nil {
		t.Fatal(err)
	}
	if err = <-done; err != nil {
		t.Fatal(err)
	}
}
func TestEngineReorderLossAndLateFramesKeepQualityHonest(t *testing.T) {
	for _, mode := range []string{"reorder", "control_hole", "voice_hole"} {
		t.Run(mode, func(t *testing.T) {
			source, receiving := engineSessionPair(t)
			spurt := engineFixture(t, p25rtp.GroupHome)
			e, done := engineStart(t, receiving, spurt)
			engineRaw(t, source, enginePacket(spurt, p25rtp.Start, 100))
			engineReady(t, e)
			engineRaw(t, source, engineVoicePacket(spurt, engineFrame(t, 18), 101, 1000))
			if mode == "reorder" {
				engineRaw(t, source, engineVoicePacket(spurt, engineFrame(t, 20), 103, 1320))
				engineRaw(t, source, engineVoicePacket(spurt, engineFrame(t, 19), 102, 1160))
			} else if mode == "control_hole" {
				engineRaw(t, source, engineVoicePacket(spurt, engineFrame(t, 19), 103, 1160))
			} else {
				engineRaw(t, source, engineVoicePacket(spurt, engineFrame(t, 20), 103, 1320))
			}
			var deliveries []Delivery
			if mode == "voice_hole" {
				ctx, cancel := context.WithTimeout(t.Context(), time.Second)
				for range 3 {
					d, err := e.Receive(ctx)
					if err != nil {
						t.Fatal(err)
					}
					deliveries = append(deliveries, d)
				}
				cancel()
				engineRaw(t, source, engineVoicePacket(spurt, engineFrame(t, 19), 102, 1160))
				deadline := time.Now().Add(100 * time.Millisecond)
				for e.Stats().LatePackets == 0 && time.Now().Before(deadline) {
					time.Sleep(time.Millisecond)
				}
				if e.Stats().LatePackets == 0 {
					t.Fatal("late actual packet was not discarded")
				}
			}
			engineRaw(t, source, enginePacket(spurt, p25rtp.End, 104))
			deliveries = append(deliveries, engineDrain(t, e)...)
			if err := <-done; err != nil {
				t.Fatal(err)
			}
			if deliveries[len(deliveries)-1].Kind != EndDelivery {
				t.Fatal("missing terminal fence")
			}
			gaps := 0
			for _, d := range deliveries {
				if d.Kind == GapDelivery {
					gaps++
					if d.Position != 19 || !reflect.DeepEqual(d.Frame, Frame{}) {
						t.Fatal("IP gap fabricated voice or RFquality")
					}
				} else if d.Kind == FrameDelivery {
					if d.Frame.Voice.Quality != engineFrame(t, d.Position).Voice.Quality {
						t.Fatal("RFquality changed for IP loss")
					}
				}
			}
			if (mode == "voice_hole") != (gaps == 1) {
				t.Fatal("control sequence interpreted as audio count", mode, gaps)
			}
			if mode == "reorder" && len(deliveries) != 4 {
				t.Fatal("reordered actual frame lost")
			}
			if len(e.rxTokens) != 0 {
				t.Fatal("receive token leaked")
			}
		})
	}
}
func TestEngineWrongSpurtMetadataDoesNotGrantOrRefresh(t *testing.T) {
	source, receiving := engineSessionPair(t)
	spurt := engineFixture(t, p25rtp.GroupHome)
	e, done := engineStart(t, receiving, spurt)
	wrong := enginePacket(spurt, p25rtp.Start, 100)
	copy := *spurt.RF
	copy.Source.Unit++
	wrong.RF = &copy
	engineRaw(t, source, wrong)
	select {
	case <-e.Ready():
		t.Fatal("wrongsource allocated spurt")
	case <-time.After(30 * time.Millisecond):
	}
	engineRaw(t, source, enginePacket(spurt, p25rtp.Start, 101))
	engineReady(t, e)
	started := time.Now()
	for i := 0; i < 3; i++ {
		packet := engineVoicePacket(spurt, engineFrame(t, 18), uint16(102+i), uint32(1000+i*160))
		switch i {
		case 0:
			packet.Control.TSN += 2
		case 1:
			packet.Control.ServiceOptions ^= 0x80
		case 2:
			packet.RF = &copy
		}
		engineRaw(t, source, packet)
	}
	deliveries := engineDrain(t, e)
	if len(deliveries) != 1 || deliveries[0].Kind != EndDelivery || deliveries[0].Reason != ReasonFirstMedia {
		t.Fatal("wrongowner media refreshed/published", deliveries)
	}
	if elapsed := time.Since(started); elapsed < 450*time.Millisecond || elapsed > 900*time.Millisecond {
		t.Fatal("first-media timeout changed", elapsed)
	}
	if err := <-done; !errors.Is(err, ErrSpurtTimeout) {
		t.Fatal("firstmedia result", err)
	}
	counts := e.Stats()
	if counts.WrongSource < 2 || counts.WrongSpurt < 1 || counts.WrongOptions < 1 || counts.ReceivedFrames != 0 {
		t.Fatal("wrongmetadata counters", counts)
	}
}
func TestEngineRecipientEndNeverSendsOriginEnd(t *testing.T) {
	source, receiving := engineSessionPair(t)
	spurt := engineFixture(t, p25rtp.GroupHome)
	e, done := engineStart(t, receiving, spurt)
	engineRaw(t, source, enginePacket(spurt, p25rtp.Start, 100))
	engineReady(t, e)
	engineRaw(t, source, engineVoicePacket(spurt, engineFrame(t, 18), 101, 1000))
	ctx, cancel := context.WithTimeout(t.Context(), time.Second)
	defer cancel()
	first, err := e.Receive(ctx)
	if err != nil || first.Kind != FrameDelivery {
		t.Fatal("first media", first, err)
	}
	end := make(chan error, 1)
	go func() { end <- e.End(ctx) }()
	terminal, err := e.Receive(ctx)
	if err != nil || terminal.Kind != EndDelivery {
		t.Fatal("recipient drain", terminal, err)
	}
	if err = <-end; err != nil {
		t.Fatal(err)
	}
	if err = <-done; err != nil {
		t.Fatal(err)
	}
	probe, stop := context.WithTimeout(t.Context(), 80*time.Millisecond)
	defer stop()
	if p, err := source.Receive(probe); err == nil {
		t.Fatal("recipient sent unexpected reverse wire control", p.Control.Type)
	} else if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatal(err)
	}
}
func TestEngineQueuesPositionsAndWorkerCancellationAreBounded(t *testing.T) {
	t.Run("tx12", func(t *testing.T) {
		home, peer := engineSessionPair(t)
		_ = peer
		spurt := engineFixture(t, p25rtp.GroupHome)
		e, err := NewEngine(home, spurt)
		if err != nil {
			t.Fatal(err)
		}
		for pos := uint64(18); pos < 30; pos++ {
			if err = e.Push(engineFrame(t, pos)); err != nil {
				t.Fatal(err)
			}
		}
		if err = e.Push(engineFrame(t, 30)); !errors.Is(err, ErrQueueFull) {
			t.Fatal("unbounded TX", err)
		}
		if cap(e.tx) != txCapacity {
			t.Fatal("TXbacking capacity grew")
		}
		e.Close()
		if err = e.Run(t.Context()); !errors.Is(err, ErrEngineStarted) {
			t.Fatal("closed engine restarted")
		}
	})
	t.Run("ambiguous_position", func(t *testing.T) {
		home, _ := engineSessionPair(t)
		e, err := NewEngine(home, engineFixture(t, p25rtp.GroupHome))
		if err != nil {
			t.Fatal(err)
		}
		defer e.Close()
		if err = e.Push(engineFrame(t, 18)); err != nil {
			t.Fatal(err)
		}
		if err = e.Push(engineFrame(t, 90)); !errors.Is(err, ErrFramePosition) {
			t.Fatal("wholecycle guessed", err)
		}
	})
	t.Run("reader_cancel", func(t *testing.T) {
		home, _ := engineSessionPair(t)
		e, done := engineStart(t, home, engineFixture(t, p25rtp.GroupServing))
		time.Sleep(10 * time.Millisecond)
		var wait sync.WaitGroup
		for range 2 {
			wait.Add(1)
			go func() { defer wait.Done(); e.Close() }()
		}
		wait.Wait()
		select {
		case err := <-done:
			if !errors.Is(err, context.Canceled) {
				t.Fatal("Close result", err)
			}
		case <-time.After(time.Second):
			t.Fatal("blocked reader survivedClose")
		}
		if len(e.rxTokens) != 0 {
			t.Fatal("reader token leaked")
		}
	})
}
func TestEngineAdmissionAndOpaqueFrameCopies(t *testing.T) {
	home, _ := engineSessionPair(t)
	spurt := engineFixture(t, p25rtp.GroupHome)
	for _, test := range []struct {
		name   string
		change func(*Spurt)
	}{{"zero_tsn", func(s *Spurt) { s.TSN = 0 }}, {"wrong_parity", func(s *Spurt) { s.TSN = 2 }}, {"missing_group", func(s *Spurt) { s.GroupID = 0 }}, {"missing_source", func(s *Spurt) { s.RF = nil }}} {
		t.Run(test.name, func(t *testing.T) {
			copy := spurt.copy()
			test.change(&copy)
			if _, err := NewEngine(home, copy); err == nil {
				t.Fatal("invented missing identity/TSN")
			}
		})
	}
	frame := engineFrame(t, 18)
	raw, err := p25rtp.MarshalVoice(frame.Voice)
	if err != nil {
		t.Fatal(err)
	}
	literal, _ := hex.DecodeString("c2123456789abcb4715990d5aea4")
	if !bytes.Equal(raw, literal) {
		t.Fatal("fixture changed independently supplied quality/opaque bits")
	}
}

func TestEngineActualRequestRetryAfterLostGrant(t *testing.T) {
	home, serving := engineSessionPair(t)
	spurt := engineFixture(t, p25rtp.GroupServing)
	e, done := engineStart(t, serving, spurt)
	ctx, cancel := context.WithTimeout(t.Context(), 2*time.Second)
	defer cancel()
	first, err := home.Receive(ctx)
	if err != nil || first.Control.Type != p25rtp.Request {
		t.Fatal("initial Request", first, err)
	}
	at := time.Now()
	retry, err := home.Receive(ctx)
	if err != nil || retry.Control.Type != p25rtp.Request {
		t.Fatal("Request retry", retry, err)
	}
	elapsed := time.Since(at)
	if elapsed < 400*time.Millisecond || elapsed > 800*time.Millisecond || retry.Control.TSN != first.Control.TSN || *retry.RF != *first.RF || retry.Sequence != first.Sequence+1 {
		t.Fatal("retry owner/timing changed", elapsed)
	}
	if delta := int32(retry.Timestamp - first.Timestamp); delta < 3200 || delta > 6400 {
		t.Fatal("Request timestamp was not8kHz clock", delta)
	}
	if err = home.Send(ctx, p25rtp.Packet{Role: p25rtp.GroupHome, Control: p25rtp.PacketTypeControl{Type: p25rtp.Grant, TSN: spurt.TSN, ServiceOptions: spurt.ServiceOptions}}); err != nil {
		t.Fatal(err)
	}
	engineReady(t, e)
	if err = e.Push(engineFrame(t, 18)); err != nil {
		t.Fatal(err)
	}
	ending := make(chan error, 1)
	go func() { ending <- e.End(ctx) }()
	voice, ends := 0, 0
	for ends < 4 {
		packet, err := home.Receive(ctx)
		if err != nil {
			t.Fatal(err)
		}
		if packet.Voice != nil {
			voice++
		}
		if packet.Control.Type == p25rtp.End {
			ends++
		}
	}
	if voice != 1 {
		t.Fatal("Grant did not start exact queued voice", voice)
	}
	if err = <-ending; err != nil {
		t.Fatal(err)
	}
	if err = <-done; err != nil {
		t.Fatal(err)
	}
}
func TestEngineReceiveTimestampWrapAndCycleAmbiguity(t *testing.T) {
	source, receiving := engineSessionPair(t)
	spurt := engineFixture(t, p25rtp.GroupHome)
	e, done := engineStart(t, receiving, spurt)
	engineRaw(t, source, enginePacket(spurt, p25rtp.Start, 100))
	engineReady(t, e)
	engineRaw(t, source, engineVoicePacket(spurt, engineFrame(t, 18), 101, 0xffffffa0))
	engineRaw(t, source, engineVoicePacket(spurt, engineFrame(t, 19), 102, 64))
	engineRaw(t, source, engineVoicePacket(spurt, engineFrame(t, 20), 103, 64+72*p25rtp.FrameTicks))
	deliveries := engineDrain(t, e)
	if len(deliveries) != 3 || deliveries[0].Kind != FrameDelivery || deliveries[0].Position != 18 || deliveries[1].Position != 19 || deliveries[2].Kind != EndDelivery || deliveries[2].Reason != ReasonSource {
		t.Fatal("ambiguouscycle was guessed or accepted wrap lost", deliveries)
	}
	if err := <-done; !errors.Is(err, ErrFramePosition) {
		t.Fatal("wrong cycle failure", err)
	}
	if len(e.rxTokens) != 0 {
		t.Fatal("cycle failure leaked packet tokens")
	}
}
func TestEngineReceiveAggregateBoundAndAbortedDrain(t *testing.T) {
	source, receiving := engineSessionPair(t)
	spurt := engineFixture(t, p25rtp.GroupHome)
	e, done := engineStart(t, receiving, spurt)
	engineRaw(t, source, enginePacket(spurt, p25rtp.Start, 100))
	engineReady(t, e)
	for i := 0; i < 20; i++ {
		engineRaw(t, source, engineVoicePacket(spurt, engineFrame(t, uint64(18+i)), uint16(101+i), uint32(1000+i*p25rtp.FrameTicks)))
	}
	// No application consumer: storage is shared across stages, not16perqueue.
	select {
	case <-e.Done():
	case <-time.After(time.Second):
		t.Fatal("unread bounded media kept owner alive")
	}
	if err := <-done; !errors.Is(err, ErrQueueFull) {
		t.Fatal("backpressure outcome", err)
	}
	if len(e.rxTokens) != 0 || e.Stats().ReceivedFrames > rxCapacity || e.Stats().DroppedFrames == 0 {
		t.Fatal("aggregate accepted-packet bound/cleanup", e.Stats())
	}
	deliveries := engineDrain(t, e)
	if len(deliveries) != 1 || deliveries[0].Kind != EndDelivery || deliveries[0].Reason != ReasonQueue {
		t.Fatal("abort claimed an orderly media tail", deliveries)
	}
}
func TestEngineConsoleSourceRemainsExplicitAndImmutable(t *testing.T) {
	home, serving := engineSessionPair(t)
	spurt := engineFixture(t, p25rtp.GroupHome)
	suid := spurt.RF.Source
	spurt.RF = nil
	spurt.Console = &p25rtp.ConsoleControlWord{Console: suid, PhysicalConsole: p25rtp.SUID{WACN: suid.WACN, System: suid.System, Unit: suid.Unit + 1}, VoiceSource: suid, TransmitPriority: 0x20, ConsoleTXPriority: 5}
	receiver, receiverDone := engineStart(t, serving, spurt)
	sender, senderDone := engineStart(t, home, spurt)
	original := *spurt.Console
	spurt.Console.VoiceSource.Unit++
	engineReady(t, sender)
	engineReady(t, receiver)
	if err := sender.Push(engineFrame(t, 18)); err != nil {
		t.Fatal(err)
	}
	end := make(chan error, 1)
	go func() {
		ctx, cancel := context.WithTimeout(t.Context(), time.Second)
		defer cancel()
		end <- sender.End(ctx)
	}()
	deliveries := engineDrain(t, receiver)
	if len(deliveries) != 2 || deliveries[0].Kind != FrameDelivery || sender.spurt.RF != nil || receiver.spurt.RF != nil || *sender.spurt.Console != original || *receiver.spurt.Console != original {
		t.Fatal("Console source fell back to RF or aliased caller")
	}
	if err := <-end; err != nil {
		t.Fatal(err)
	}
	if err := <-senderDone; err != nil {
		t.Fatal(err)
	}
	if err := <-receiverDone; err != nil {
		t.Fatal(err)
	}
}

func TestEngineHeaderOnlyPreservationAndActualVoiceDeadline(t *testing.T) {
	for _, mode := range []string{"preserve", "cannot_keep_alive"} {
		t.Run(mode, func(t *testing.T) {
			source, receiving := engineSessionPair(t)
			spurt := engineFixture(t, p25rtp.GroupHome)
			e, done := engineStart(t, receiving, spurt)
			literal, _ := hex.DecodeString(engineLiteralPacket)
			fixture, _ := p25rtp.Parse(literal)
			engineRaw(t, source, enginePacket(spurt, p25rtp.Start, 100))
			engineReady(t, e)
			header := enginePacket(spurt, p25rtp.Progress, 101)
			header.Header = fixture.Header
			engineRaw(t, source, header)
			if mode == "preserve" {
				engineRaw(t, source, engineVoicePacket(spurt, engineFrame(t, 18), 102, 1000))
				engineRaw(t, source, enginePacket(spurt, p25rtp.End, 103))
				deliveries := engineDrain(t, e)
				if len(deliveries) != 2 || deliveries[0].Frame.Header == nil || *deliveries[0].Frame.Header != *fixture.Header || deliveries[0].Kind != FrameDelivery || deliveries[1].Kind != EndDelivery {
					t.Fatal("actual Header not preserved", deliveries)
				}
				if err := <-done; err != nil {
					t.Fatal(err)
				}
			} else {
				for i := 0; i < 4; i++ {
					time.Sleep(100 * time.Millisecond)
					header.Sequence++
					engineRaw(t, source, header)
				}
				deliveries := engineDrain(t, e)
				if len(deliveries) != 1 || deliveries[0].Kind != EndDelivery || deliveries[0].Reason != ReasonFirstMedia || e.Stats().ReceivedFrames != 0 {
					t.Fatal("header-only stream invented media/liveness", deliveries)
				}
				if err := <-done; !errors.Is(err, ErrSpurtTimeout) {
					t.Fatal("header-only timeout", err)
				}
			}
		})
	}
}
