package dispatch

import (
	"bytes"
	"context"
	_ "embed"
	"encoding/binary"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"sort"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/coder/websocket"
	"github.com/pion/webrtc/v4"
)

//go:embed testdata/audio_livenessCadence_oracle.json
var livenessCadenceOracleJSON []byte

//go:embed testdata/audioop-pcmu-u16le.bin
var livenessPCMUOracle []byte

//go:embed testdata/audioop-pcmu-transition-neighborhoods.bitmap
var livenessPCMUBoundaries []byte

//go:embed testdata/audioop-ulaw-decoder-s16le.bin
var livenessPCMUDecoded []byte

type livenessCadenceOracle struct {
	Schema                int    `json:"schema"`
	Oracle                string `json:"oracle"`
	Samples               int    `json:"samples_per_frame"`
	FrameMS               int    `json:"frame_ms"`
	FirstLivenessCadence  int    `json:"first_onset"`
	SecondLivenessCadence int    `json:"second_onset"`
	SilenceFrames         int    `json:"silence_frames"`
	Frames                []struct {
		Ordinal int    `json:"ordinal"`
		Phase   string `json:"phase"`
		PCM     []byte `json:"pcm"`
		PCMU    []byte `json:"pcmu"`
	} `json:"frames"`
}
type livenessCadenceReceived struct {
	Stream    string
	Sequence  uint16
	Timestamp uint32
	SSRC      uint32
	Payload   []byte
	At        time.Time
}
type livenessCadenceControl struct {
	CallID        string
	Source, Group int
	Active        bool
}

func TestCodecOpaqueLivenessPreservesActualNativeAndPionCadence(t *testing.T) {
	if os.Getenv("TRUNKSYSTEM_TEST_PACKET_CODEC") == "" {
		t.Skip("set TRUNKSYSTEM_TEST_PACKET_CODEC to the pinned native packet-codec executable")
	}
	var fixture livenessCadenceOracle
	if err := json.Unmarshal(livenessCadenceOracleJSON, &fixture); err != nil {
		t.Fatal(err)
	}
	if fixture.Schema != 1 || fixture.Samples != 160 || fixture.FrameMS != 20 || len(fixture.Frames) != 216 || fixture.SecondLivenessCadence != 158 || fixture.SilenceFrames != 113 {
		t.Fatal("Independent oracle shape differs")
	}
	for i, frame := range fixture.Frames {
		if frame.Ordinal != i || len(frame.PCM) != FrameBytes || len(frame.PCMU) != FrameSamples {
			t.Fatalf("Oracle frame %d malformed", i)
		}
	}
	for _, cadence := range []string{"native_nine_frame_180ms_bursts_with_liveness"} {
		t.Run(cadence, func(t *testing.T) { runReceiveLivenessCadenceLoopback(t, cadence, fixture) })
	}
}

func runReceiveLivenessCadenceLoopback(t *testing.T, cadence string, fixture livenessCadenceOracle) {
	router := &testRouter{}
	hub := New(Options{Router: router, LoopbackCandidates: true, AudioIdle: 2 * time.Second})
	defer hub.Close()
	owner := principal(10002, 1001)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/api/dispatch/session":
			hub.ServeSession(w, r, owner, "authenticated-test-session", nil)
		case "/api/dispatch/audio":
			hub.ServeSocket(w, r, owner, "authenticated-test-session")
		default:
			http.NotFound(w, r)
		}
	}))
	defer server.Close()
	request, _ := http.NewRequest("POST", server.URL+"/api/dispatch/session", bytes.NewBufferString("{}"))
	request.Header.Set("Origin", server.URL)
	response, err := http.DefaultClient.Do(request)
	if err != nil {
		t.Fatal(err)
	}
	var ticket struct {
		Ticket string `json:"ticket"`
	}
	if err = json.NewDecoder(response.Body).Decode(&ticket); err != nil {
		t.Fatal(err)
	}
	response.Body.Close()
	if response.StatusCode != 201 || ticket.Ticket == "" {
		t.Fatal("no audio ticket")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	headers := http.Header{}
	headers.Set("Origin", server.URL)
	socket, _, err := websocket.Dial(ctx, "ws"+strings.TrimPrefix(server.URL, "http")+"/api/dispatch/audio?ticket="+ticket.Ticket, &websocket.DialOptions{HTTPHeader: headers})
	if err != nil {
		t.Fatal(err)
	}
	defer socket.CloseNow()
	api, err := mediaAPI(0, 0, true)
	if err != nil {
		t.Fatal(err)
	}
	peer, err := api.NewPeerConnection(webrtc.Configuration{})
	if err != nil {
		t.Fatal(err)
	}
	defer peer.Close()
	microphone, err := webrtc.NewTrackLocalStaticRTP(webrtc.RTPCodecCapability{MimeType: webrtc.MimeTypePCMU, ClockRate: SampleRate, Channels: 1}, "microphone", "microphone")
	if err != nil {
		t.Fatal(err)
	}
	transceiver, err := peer.AddTransceiverFromTrack(microphone, webrtc.RTPTransceiverInit{Direction: webrtc.RTPTransceiverDirectionSendonly})
	if err != nil {
		t.Fatal(err)
	}
	go func() {
		buffer := make([]byte, 1500)
		for {
			if _, _, err := transceiver.Sender().Read(buffer); err != nil {
				return
			}
		}
	}()
	for range owner.ReceiveGroups {
		if _, err = peer.AddTransceiverFromKind(webrtc.RTPCodecTypeAudio, webrtc.RTPTransceiverInit{Direction: webrtc.RTPTransceiverDirectionRecvonly}); err != nil {
			t.Fatal(err)
		}
	}
	received := make(chan livenessCadenceReceived, len(fixture.Frames)+16)
	peer.OnTrack(func(track *webrtc.TrackRemote, _ *webrtc.RTPReceiver) {
		go func() {
			for {
				packet, _, readErr := track.ReadRTP()
				if readErr != nil {
					return
				}
				copy := livenessCadenceReceived{Stream: track.StreamID(), Sequence: packet.SequenceNumber, Timestamp: packet.Timestamp, SSRC: packet.SSRC, Payload: append([]byte(nil), packet.Payload...), At: time.Now()}
				select {
				case received <- copy:
				case <-ctx.Done():
					return
				}
			}
		}()
	})
	var writeMu sync.Mutex
	send := func(value any) error {
		writeMu.Lock()
		defer writeMu.Unlock()
		raw, _ := json.Marshal(value)
		return socket.Write(ctx, websocket.MessageText, raw)
	}
	connected := make(chan struct{})
	subscribed := make(chan struct{})
	control := make(chan livenessCadenceControl, len(fixture.Frames)+16)
	readFailure := make(chan error, 1)
	var connectedOnce, subscribedOnce sync.Once
	go func() {
		var pending []webrtc.ICECandidateInit
		for {
			_, raw, err := socket.Read(ctx)
			if err != nil {
				select {
				case readFailure <- err:
				default:
				}
				return
			}
			var message struct {
				Type      string                   `json:"type"`
				SDP       string                   `json:"sdp"`
				Candidate *webrtc.ICECandidateInit `json:"candidate"`
				State     State                    `json:"state"`
				Message   string                   `json:"message"`
				CallID    string                   `json:"call_id"`
				Source    int                      `json:"source_id"`
				Group     int                      `json:"talkgroup_id"`
				Active    bool                     `json:"active"`
			}
			if err = json.Unmarshal(raw, &message); err != nil {
				continue
			}
			switch message.Type {
			case "answer":
				if err = peer.SetRemoteDescription(webrtc.SessionDescription{Type: webrtc.SDPTypeAnswer, SDP: message.SDP}); err != nil {
					readFailure <- err
					return
				}
				for _, candidate := range pending {
					if err = peer.AddICECandidate(candidate); err != nil {
						readFailure <- err
						return
					}
				}
				pending = nil
			case "ice":
				if message.Candidate != nil {
					if peer.RemoteDescription() == nil {
						pending = append(pending, *message.Candidate)
					} else if err = peer.AddICECandidate(*message.Candidate); err != nil {
						readFailure <- err
						return
					}
				}
			case "state":
				if message.State.MediaConnected {
					connectedOnce.Do(func() { close(connected) })
				}
				if len(message.State.Subscriptions) == 1 && message.State.Subscriptions[0] == 1001 {
					subscribedOnce.Do(func() { close(subscribed) })
				}
			case "rx":
				select {
				case control <- livenessCadenceControl{message.CallID, message.Source, message.Group, message.Active}:
				case <-ctx.Done():
					return
				}
			case "error":
				select {
				case readFailure <- io.ErrUnexpectedEOF:
				default:
				}
				return
			}
		}
	}()
	peer.OnICECandidate(func(candidate *webrtc.ICECandidate) {
		if candidate != nil {
			_ = send(map[string]any{"type": "ice", "candidate": candidate.ToJSON()})
		}
	})
	offer, err := peer.CreateOffer(nil)
	if err != nil {
		t.Fatal(err)
	}
	if err = peer.SetLocalDescription(offer); err != nil {
		t.Fatal(err)
	}
	if err = send(map[string]any{"type": "offer", "sdp": offer.SDP}); err != nil {
		t.Fatal(err)
	}
	select {
	case <-connected:
	case err := <-readFailure:
		t.Fatal("signaling failed:", err)
	case <-ctx.Done():
		t.Fatal("WebRTC did not connect")
	}
	if err = send(map[string]any{"type": "subscribe", "talkgroups": []int{1001}}); err != nil {
		t.Fatal(err)
	}
	select {
	case <-subscribed:
	case err := <-readFailure:
		t.Fatal(err)
	case <-ctx.Done():
		t.Fatal("Subscription not acknowledged")
	}
	// The actual browser transport and native vocoder are connected before
	// voice input. Neither native process has an FNE, modem or radio path.
	nativePath := os.Getenv("TRUNKSYSTEM_TEST_PACKET_CODEC")
	encoder, err := NewPacketCodec(PacketCodecOptions{Binary: nativePath, KeyFile: "-"})
	if err != nil {
		t.Fatal(err)
	}
	if err = encoder.Start(ctx); err != nil {
		t.Fatal(err)
	}
	defer encoder.Close()
	decoder, err := NewPacketCodec(PacketCodecOptions{Binary: nativePath, KeyFile: "-"})
	if err != nil {
		t.Fatal(err)
	}
	if err = decoder.Start(ctx); err != nil {
		t.Fatal(err)
	}
	defer decoder.Close()
	encoded := make([][]byte, len(fixture.Frames))
	for i, input := range fixture.Frames {
		f, err := encoder.Encode(ctx, Frame{CallID: "one-native-opaque-cadence-call", SourceID: 10001, TalkgroupID: 1001, Sequence: uint64(i + 1), PCM: input.PCM, AlgorithmID: 0x80, CapturedAt: time.Now()})
		if err != nil {
			t.Fatal(err)
		}
		encoded[i], err = EncodeOpaqueCodecPacket(f)
		if err != nil {
			t.Fatal(err)
		}
	}
	executable, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	codec, err := NewCodec(CodecOptions{Binary: executable, StateDir: t.TempDir(), Identity: "offline-pion-cadence", MasterAddress: "127.0.0.1", MasterPort: 60001, MasterPassword: strings.Repeat("a", 64), PeerIDs: map[int]int{1001: 123}, RadioID: 10002, WACN: 1, SystemID: 1, Opaque: true})
	if err != nil {
		t.Fatal(err)
	}
	resource := shutdownResource(t)
	var receiveMu sync.Mutex
	expectedPCM := make([][]byte, len(fixture.Frames))
	var applicationID string
	var receivedWords int
	var activityCount int
	codec.options.OnFrame = func(f Frame) {
		receiveMu.Lock()
		if applicationID == "" {
			applicationID = f.CallID
		}
		if f.CallID != applicationID || (!f.End && f.Sequence != uint64(receivedWords+1)) {
			receiveMu.Unlock()
			select {
			case readFailure <- fmt.Errorf("opaque control changed call or voice cadence"):
			default:
			}
			return
		}
		receiveMu.Unlock()
		if !f.End {
			plain, err := decoder.Decode(ctx, f)
			if err != nil {
				select {
				case readFailure <- err:
				default:
				}
				return
			}
			receiveMu.Lock()
			expectedPCM[receivedWords] = append([]byte{}, plain.PCM...)
			receivedWords++
			receiveMu.Unlock()
			hub.Publish(plain)
		} else {
			hub.Publish(f)
		}
	}
	codec.options.OnReceiveActivity = func(a CodecReceiveActivity) { receiveMu.Lock(); activityCount++; receiveMu.Unlock() }
	codec.wg.Add(2)
	go codec.receive(resource)
	go codec.publishFrames()
	defer func() { codec.cancel(); resource.read.Close(); codec.wg.Wait() }()
	published := make([]time.Time, len(fixture.Frames))
	begin := time.Now()
	pause := time.Duration(0)
	lastVoiceTime := time.Time{}
	var lastPacket []byte
	var controlSeq uint64
	heartbeat := func() {
		if len(lastPacket) == 0 {
			return
		}
		remaining := OpaqueReceiveBudget - time.Since(lastVoiceTime)
		if remaining <= 0 {
			t.Fatal("test native liveness exceeded real voice budget")
		}
		h := append([]byte{}, lastPacket[:64]...)
		h[4], h[7] = 17, 0
		binary.BigEndian.PutUint16(h[58:60], 0)
		binary.BigEndian.PutUint32(h[20:24], uint32(remaining/time.Millisecond))
		controlSeq++
		binary.BigEndian.PutUint64(h[24:32], controlSeq)
		binary.BigEndian.PutUint64(h[48:56], binary.BigEndian.Uint64(lastPacket[24:32]))
		h[56] = 0
		if _, err := resource.write.Write(h); err != nil {
			t.Fatal(err)
		}
	}
	for start := 0; start < len(encoded); start += 9 {
		target := begin.Add(time.Duration(start)*FrameDuration + pause)
		for delay := time.Until(target); delay > 0; delay = time.Until(target) {
			time.Sleep(min(delay, 100*time.Millisecond))
			if time.Since(lastVoiceTime) >= 100*time.Millisecond {
				heartbeat()
			}
		}
		for i := start; i < min(start+9, len(encoded)); i++ {
			published[i] = time.Now()
			if _, err := resource.write.Write(encoded[i]); err != nil {
				t.Fatal(err)
			}
			lastPacket = encoded[i]
			lastVoiceTime = published[i]
		}
		if start == 18 {
			for range 10 {
				time.Sleep(100 * time.Millisecond)
				heartbeat()
			}
			pause += time.Second
		}
	}
	packets := make([]livenessCadenceReceived, 0, len(fixture.Frames))
	deadline := time.NewTimer(2 * time.Second)
	defer deadline.Stop()
	for len(packets) < len(fixture.Frames) {
		select {
		case packet := <-received:
			packets = append(packets, packet)
		case err := <-readFailure:
			t.Fatalf("Receive signaling failed: %v", err)
		case <-deadline.C:
			t.Fatalf("Missing frames: received %d of %d", len(packets), len(fixture.Frames))
		case <-ctx.Done():
			t.Fatal("Receive deadline")
		}
	}
	// Exact native decoder output must cross the real Hub/Pion transport once.
	receiveMu.Lock()
	if activityCount == 0 || receivedWords != len(encoded) {
		receiveMu.Unlock()
		t.Fatal("activity missing or voice cadence changed")
	}
	receiveMu.Unlock()
	latencies := make([]time.Duration, len(packets))
	silent := 0
	boundarySamples := 0
	zeroNativeFrames := 0
	timestampGaps := 0
	maxTimestampStep := uint32(0)
	for i, p := range packets {
		expected := fixture.Frames[i]
		receiveMu.Lock()
		expectedPayload := append([]byte{}, expectedPCM[i]...)
		receiveMu.Unlock()
		if p.Stream != "tg-1001" || p.SSRC != packets[0].SSRC || p.Sequence != packets[0].Sequence+uint16(i) {
			t.Fatalf("Frame %d lost identity/order: stream=%s seq=%d", i, p.Stream, p.Sequence)
		}

		if len(p.Payload) != FrameSamples || len(expectedPayload) != FrameBytes {
			t.Fatal("native PCM or RTP sample count changed")
		}
		allZero := bytes.Equal(expectedPayload, make([]byte, FrameBytes))
		if allZero {
			zeroNativeFrames++
		}
		for sample := 0; sample < FrameSamples; sample++ {
			raw := binary.LittleEndian.Uint16(expectedPayload[sample*2:])
			want := livenessPCMUOracle[int(raw)]
			allowed := p.Payload[sample] == want
			if !allZero && livenessPCMUBoundaries[int(raw)/8]&(1<<(uint(raw)%8)) != 0 {
				boundarySamples++
				value := int(int16(raw))
				for neighbor := max(-32768, value-3); neighbor <= min(32767, value+3); neighbor++ {
					candidate := livenessPCMUOracle[int(uint16(int16(neighbor)))]
					actual := p.Payload[sample]
					if actual == candidate || bytes.Equal(livenessPCMUDecoded[int(actual)*2:int(actual)*2+2], livenessPCMUDecoded[int(candidate)*2:int(candidate)*2+2]) {
						allowed = true
					}
				}
			}
			if !allowed {
				t.Fatalf("Frame%d sample%d differs from independent audioop quantizer neighborhood: actual=%02x exact=%02x", i, sample, p.Payload[sample], want)
			}
		}
		if expected.Phase == "silence" {
			silent++
		}
		latencies[i] = p.At.Sub(published[i])
		if latencies[i] < 0 || latencies[i] > 350*time.Millisecond {
			t.Fatalf("Frame %d latency %s exceeds bounded loopback envelope", i, latencies[i])
		}
		if i > 0 {
			step := p.Timestamp - packets[i-1].Timestamp
			if step < FrameSamples {
				t.Fatalf("Frame %d RTP sample clock regressed", i)
			}
			if step != FrameSamples {
				timestampGaps++
			}
			if step > maxTimestampStep {
				maxTimestampStep = step
			}
		}
	}
	if silent != fixture.SilenceFrames {
		t.Fatal("Continuous silence/new livenessCadence boundaries missing")
	}
	for i := 0; i < len(fixture.Frames); i++ {
		select {
		case c := <-control:
			if c.CallID != applicationID || c.Source != 10001 || c.Group != 1001 || !c.Active {
				t.Fatalf("Frame %d identity reset across silence: %#v", i, c)
			}
		case <-ctx.Done():
			t.Fatal("Missing RX control evidence")
		}
	}
	endPacket := append([]byte{}, lastPacket[:64]...)
	binary.BigEndian.PutUint16(endPacket[10:12], 1)
	binary.BigEndian.PutUint16(endPacket[58:60], 0)
	binary.BigEndian.PutUint64(endPacket[24:32], uint64(len(encoded)+1))
	if _, err := resource.write.Write(endPacket); err != nil {
		t.Fatal(err)
	}
	select {
	case c := <-control:
		if c.Active || c.CallID != applicationID {
			t.Fatal("Missing exact final end")
		}
	case <-ctx.Done():
		t.Fatal("Missing end notification")
	}
	select {
	case p := <-received:
		t.Fatalf("Extra RTP frame after end: seq=%d", p.Sequence)
	case <-time.After(80 * time.Millisecond):
	}
	// A separately declared finite zero-PCM reference checks the independent
	// zero code exactly. It does not replace or modify any native decoder PCM.
	const zeroCall = "declared-zero-pcm-reference"
	hub.Publish(Frame{CallID: zeroCall, SourceID: 10001, TalkgroupID: 1001, Sequence: 1, PCM: make([]byte, FrameBytes), CapturedAt: time.Now()})
	select {
	case p := <-received:
		if len(p.Payload) != FrameSamples {
			t.Fatal("zero reference changed sample count")
		}
		for _, sample := range p.Payload {
			if sample != livenessPCMUOracle[0] {
				t.Fatal("declared zero PCM did not use exact independent zero code")
			}
		}
	case <-ctx.Done():
		t.Fatal("zero reference RTP missing")
	}
	select {
	case c := <-control:
		if !c.Active || c.CallID != zeroCall {
			t.Fatal("zero reference owner missing")
		}
	case <-ctx.Done():
		t.Fatal("zero reference control missing")
	}
	hub.Publish(Frame{CallID: zeroCall, SourceID: 10001, TalkgroupID: 1001, End: true, CapturedAt: time.Now()})
	select {
	case c := <-control:
		if c.Active || c.CallID != zeroCall {
			t.Fatal("zero reference End missing")
		}
	case <-ctx.Done():
		t.Fatal("zero reference End control missing")
	}
	select {
	case <-received:
		t.Fatal("extra RTP after declared zero reference")
	case <-time.After(60 * time.Millisecond):
	}
	sorted := append([]time.Duration(nil), latencies...)
	sort.Slice(sorted, func(i, j int) bool { return sorted[i] < sorted[j] })
	t.Logf("ONSET_RESULT cadence=%s input_frames=%d received_frames=%d input_silence_frames=%d first_livenessCadence_frame=%d second_livenessCadence_frame=%d first_latency_ms=%.3f resumed_latency_ms=%.3f p95_latency_ms=%.3f max_latency_ms=%.3f timestamp_gap_count=%d maximum_timestamp_step=%d source=10001 group=1001 call_identity_unchanged=true heartbeat_packets_have_no_audio=true oracle=CPython_audioop_transition_neighborhood_radius3_engineering_tolerance boundary_sample_positions=%d exact_zero_native_frames=%d declared_zero_pcm_reference_frames=1 scope=actual_Codec_opaque_native_vocoder_Hub_PCMU_RTP_Pion_loopback_not_browser_playout", cadence, len(fixture.Frames), len(packets), silent, fixture.FirstLivenessCadence, fixture.SecondLivenessCadence, float64(latencies[fixture.FirstLivenessCadence])/float64(time.Millisecond), float64(latencies[fixture.SecondLivenessCadence])/float64(time.Millisecond), float64(sorted[len(sorted)*95/100])/float64(time.Millisecond), float64(sorted[len(sorted)-1])/float64(time.Millisecond), timestampGaps, maxTimestampStep, boundarySamples, zeroNativeFrames)
}
