package dispatch

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/coder/websocket"
	"github.com/pion/rtp"
	"github.com/pion/webrtc/v4"
)

func TestWebRTCRoundTripAndIndependentReceiveTracks(t *testing.T) {
	router := &testRouter{}
	hub := New(Options{Router: router, LoopbackCandidates: true, AudioIdle: 2 * time.Second})
	defer hub.Close()
	owner := principal(10001, 1001, 1002)
	owner.CanSecure = true
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/api/dispatch/session":
			hub.ServeSession(w, r, owner, "authenticated-test-session", []KeyIdentity{{AlgorithmID: 129, KeyID: 0}})
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
		Ticket string        `json:"ticket"`
		Keys   []KeyIdentity `json:"keys"`
	}
	if err = json.NewDecoder(response.Body).Decode(&ticket); err != nil {
		t.Fatal(err)
	}
	response.Body.Close()
	if response.StatusCode != 201 || ticket.Ticket == "" {
		t.Fatal("no audio ticket")
	}
	if len(ticket.Keys) != 1 || ticket.Keys[0].AlgorithmID != 129 || ticket.Keys[0].KeyID != 0 {
		t.Fatal("loaded public key identity lost", ticket.Keys)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 12*time.Second)
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
	received := make(chan string, 16)
	peer.OnTrack(func(track *webrtc.TrackRemote, _ *webrtc.RTPReceiver) {
		go func() {
			for {
				packet, _, err := track.ReadRTP()
				if err != nil {
					return
				}
				if len(packet.Payload) == FrameSamples {
					select {
					case received <- track.StreamID():
					default:
					}
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
	granted := make(chan struct{})
	readFailure := make(chan error, 1)
	var connectedOnce, grantedOnce sync.Once
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
				if message.State.TX.State == "transmitting" {
					grantedOnce.Do(func() { close(granted) })
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
	if err = send(map[string]any{"type": "subscribe", "talkgroups": []int{1001, 1002}}); err != nil {
		t.Fatal(err)
	}
	if err = send(map[string]any{"type": "ptt_start", "request_id": "browser-test", "talkgroups": []int{1001}, "secure": true, "algorithm_id": 129, "key_id": 0}); err != nil {
		t.Fatal(err)
	}
	select {
	case <-granted:
	case err := <-readFailure:
		t.Fatal(err)
	case <-ctx.Done():
		t.Fatal("PTT not granted")
	}
	for n := 0; n < 8; n++ {
		payload := bytes.Repeat([]byte{0xce}, FrameSamples)
		if err = microphone.WriteRTP(&rtp.Packet{Header: rtp.Header{Version: 2, PayloadType: 0, SSRC: 123456, SequenceNumber: uint16(n), Timestamp: uint32(n * FrameSamples)}, Payload: payload}); err != nil {
			t.Fatal(err)
		}
		if n == 4 {
			_ = send(map[string]any{"type": "ptt_renew", "request_id": "browser-test"})
		}
		time.Sleep(FrameDuration)
	}
	waitFor(t, func() bool { _, n, _ := router.counts(); return n >= 4 })
	if err = send(map[string]any{"type": "ptt_stop", "request_id": "browser-test"}); err != nil {
		t.Fatal(err)
	}
	waitFor(t, func() bool { _, _, n := router.counts(); return n == 1 })
	for n := 0; n < 4; n++ {
		for _, tg := range []int{1001, 1002} {
			hub.Publish(Frame{CallID: "radio-test", TalkgroupID: tg, SourceID: 20001, PCM: MuLawToPCM(bytes.Repeat([]byte{0xce}, FrameSamples)), CapturedAt: time.Now()})
		}
		time.Sleep(FrameDuration)
	}
	seen := map[string]bool{}
	for len(seen) < 2 {
		select {
		case stream := <-received:
			seen[stream] = true
		case err := <-readFailure:
			t.Fatal("receive signaling:", err)
		case <-ctx.Done():
			t.Fatalf("independent receive tracks missing: %v", seen)
		}
	}
	if !seen["tg-1001"] || !seen["tg-1002"] {
		t.Fatalf("group stream identities lost: %v", seen)
	}
	router.mu.Lock()
	defer router.mu.Unlock()
	if len(router.begin) != 1 || router.begin[0].RadioID != 10001 || len(router.writes) < 4 {
		t.Fatal("WebRTC microphone did not reach the authenticated route")
	}
	if !router.begin[0].Secure || router.begin[0].AlgorithmID != 129 || router.begin[0].KeyID != 0 {
		t.Fatal("selected transmit key did not reach the route")
	}
	if !bytes.Equal(router.writes[0].PCM, MuLawToPCM(bytes.Repeat([]byte{0xce}, FrameSamples))) {
		t.Fatal("PCMU decode changed the PCM boundary")
	}
}
