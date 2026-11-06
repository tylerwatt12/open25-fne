package framework

import (
	"bytes"
	"context"

	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/cookiejar"

	"os"
	"slices"
	"strconv"
	"strings"
	"sync"

	"testing"
	"time"

	"github.com/coder/websocket"
	"github.com/pion/rtp"
	"github.com/pion/webrtc/v4"

	"open25.local/open25-fne/internal/dispatch"
	"open25.local/open25-fne/internal/radio"

	"open25.local/open25-fne/internal/siteaudio"
)

// These fixtures use actual authenticated SIP/TCP and UDP RTP service paths,
// with browser WebRTC when a test requests it. Only the final modem/IMBE codec boundary is a test double.
type mediaNative struct {
	mu      sync.Mutex
	paths   []string
	expires time.Time
}

func (n *mediaNative) Get(context.Context, string) (map[string]json.RawMessage, error) {
	return nil, errors.New("unexpected native GET")
}
func (n *mediaNative) Put(_ context.Context, path string, value any) (map[string]json.RawMessage, error) {
	n.mu.Lock()
	n.paths = append(n.paths, path)
	n.mu.Unlock()
	raw, _ := json.Marshal(value)
	var body map[string]any
	_ = json.Unmarshal(raw, &body)
	n.mu.Lock()
	if path == "/p25/dispatch/grant" || path == "/p25/dispatch/renew" {
		if path == "/p25/dispatch/renew" && !time.Now().Before(n.expires) {
			n.mu.Unlock()
			return map[string]json.RawMessage{"status": json.RawMessage("409")}, &radio.NativeError{HTTPStatus: 409, Status: 409, Message: "The one-second voice lease expired."}
		}
		ttl, _ := body["ttlMs"].(float64)
		n.expires = time.Now().Add(time.Duration(ttl) * time.Millisecond)
	}
	n.mu.Unlock()
	state := "granted"
	if path == "/p25/dispatch/release" {
		state = "released"
	}
	response := map[string]any{"status": 200, "issued": true, "accepted": true, "state": state, "srcId": body["srcId"], "dstId": body["dstId"], "leaseId": body["leaseId"], "epoch": "1", "channelId": 2, "channelNo": 81}
	result := map[string]json.RawMessage{}
	for k, v := range response {
		result[k], _ = json.Marshal(v)
	}
	return result, nil
}
func (n *mediaNative) count() int { n.mu.Lock(); defer n.mu.Unlock(); return len(n.paths) }

type mediaCodec struct {
	frames  chan dispatch.Frame
	mu      sync.Mutex
	written int
}

func (c *mediaCodec) Write(_ context.Context, f dispatch.Frame) error {
	c.mu.Lock()
	c.written++
	c.mu.Unlock()
	f.PCM = slices.Clone(f.PCM)
	select {
	case c.frames <- f:
	default:
	}
	return nil
}
func (*mediaCodec) Drain(context.Context, string, []int) error { return nil }

type mediaCluster struct {
	*integrationCluster
	natives map[string]*mediaNative
	codecs  map[string]*mediaCodec
	radios  map[string]*operationRadio
	users   []User
}

func newMediaCluster(t *testing.T) *mediaCluster {
	t.Helper()
	c := &mediaCluster{integrationCluster: integrationNewCluster(t), natives: map[string]*mediaNative{}, codecs: map[string]*mediaCodec{}, radios: map[string]*operationRadio{}}
	c.dispatch.service.hub.Close()
	// Drive catalog synchronization explicitly so the test can isolate media
	// connection lifetimes and controller loss from background polling.
	for _, node := range []*integrationService{c.system, c.zone, c.site, c.otherSite, c.dispatch} {
		node.service.operationCancel()
		node.service.operationWG.Wait()
		if node.service.mediaClient != nil {
			node.service.mediaClient.Close()
			node.service.mediaClient = newMediaClient(node.service)
		}
		node.service.operationCtx, node.service.operationCancel = context.WithCancel(context.Background())
		node.service.broker.cancel()
		node.service.broker.ctx, node.service.broker.cancel = context.WithCancel(node.service.operationCtx)
	}
	var system SystemConfig
	if err := c.system.service.owned(&system); err != nil {
		t.Fatal(err)
	}
	system.Radios = append(system.Radios, Radio{RID: 10002, Alias: "Console one", Kind: "console", Enabled: true}, Radio{RID: 10003, Alias: "Remote portable", Kind: "subscriber", Enabled: true}, Radio{RID: 10004, Alias: "Console two", Kind: "console", Enabled: true})
	system.ConsolePositions = []ConsolePosition{{ID: "desk-1", Name: "Dispatch 1", RadioID: 10002, Enabled: true, Talkgroups: []int{1001, 1002}}, {ID: "desk-2", Name: "Dispatch 2", RadioID: 10004, Enabled: true, Talkgroups: []int{1001, 1002}}}
	if err := c.system.service.Store.SaveConfig(storeDoc(t, system)); err != nil {
		t.Fatal(err)
	}
	first := fixtureUser()
	second := cloneUser(t, first)
	second.ID, second.Username, second.PositionIDs, second.Profile.PreferredPositionID = "user-2", "dispatcher-two", []string{"desk-2"}, "desk-2"
	c.users = []User{first, second}
	for _, user := range c.users {
		if err := c.system.service.Store.SaveUser(user.ID, user, "media-integration-password"); err != nil {
			t.Fatal(err)
		}
	}
	var zone ZoneConfig
	if err := c.zone.service.owned(&zone); err != nil {
		t.Fatal(err)
	}
	zone.Sites[0].Endpoint, zone.Sites[1].Endpoint = c.site.server.URL, c.otherSite.server.URL
	if err := c.zone.service.Store.SaveConfig(storeDoc(t, zone)); err != nil {
		t.Fatal(err)
	}
	if err := c.dispatch.service.Store.SaveConfig(storeDoc(t, ApplicationConfig{Name: "Dispatch", ConnectionURL: c.zone.server.URL, LocalSiteURL: c.site.server.URL})); err != nil {
		t.Fatal(err)
	}
	integrationSyncAll(t, c.integrationCluster)
	zoneCredentials, err := readCredentials(c.zone.service.Config().CredentialFile)
	if err != nil {
		t.Fatal(err)
	}
	dispatchCredentials, err := readCredentials(c.dispatch.service.Config().CredentialFile)
	if err != nil {
		t.Fatal(err)
	}
	dispatchCredentials.Peers[zoneCredentials.OutboundID] = PeerCredential{Role: RoleZone, InstanceID: "zone-1", Token: zoneCredentials.OutboundToken}
	for _, node := range []*integrationService{c.site, c.otherSite} {
		sc, e := readCredentials(node.service.Config().CredentialFile)
		if e != nil {
			t.Fatal(e)
		}
		dispatchCredentials.Peers[sc.OutboundID] = PeerCredential{Role: RoleSite, InstanceID: node.service.Config().InstanceID, Token: sc.OutboundToken}
	}
	if err = WritePrivateJSON(c.dispatch.service.Config().CredentialFile, dispatchCredentials); err != nil {
		t.Fatal(err)
	}
	for _, node := range []*integrationService{c.site, c.otherSite} {
		s := node.service
		credentials, err := readCredentials(s.Config().CredentialFile)
		if err != nil {
			t.Fatal(err)
		}
		credentials.Peers[zoneCredentials.OutboundID] = PeerCredential{Role: RoleZone, InstanceID: "zone-1", Token: zoneCredentials.OutboundToken}
		if err = WritePrivateJSON(s.Config().CredentialFile, credentials); err != nil {
			t.Fatal(err)
		}
		runtime := &operationRadio{status: radio.Status{SiteID: s.Config().InstanceID, State: "running", Mode: "null"}}
		s.radio = runtime
		c.radios[s.Config().InstanceID] = runtime
		native := &mediaNative{}
		codec := &mediaCodec{frames: make(chan dispatch.Frame, 128)}
		c.natives[s.Config().InstanceID], c.codecs[s.Config().InstanceID] = native, codec
		s.rfRouter.Store(siteaudio.New(siteaudio.Options{Control: func() (radio.Native, error) { return native, nil }, Codec: codec, HasRFInterest: func(tg int) bool {
			runtime.mu.Lock()
			defer runtime.mu.Unlock()
			return slices.ContainsFunc(runtime.status.Affiliations, func(a radio.Affiliation) bool { return a.TGID == tg })
		}, AllowTX: s.allowedRadioGroup}))
		s.mediaClient = newMediaClient(s)
		s.mediaClient.Ensure()
	}
	router := &networkRouter{service: c.dispatch.service}
	c.dispatch.service.hub = dispatch.New(dispatch.Options{Router: router, Available: router.Available, Event: c.dispatch.service.dispatchEvent, LoopbackCandidates: true, AudioIdle: 2 * time.Second})
	c.dispatch.service.mediaClient.Ensure()
	mediaWait(t, 4*time.Second, func() bool {
		return c.site.service.mediaClient.URL() == c.zone.server.URL && c.otherSite.service.mediaClient.URL() == c.zone.server.URL && c.dispatch.service.mediaClient.URL() == c.zone.server.URL
	}, "service media connections")
	return c
}

func mediaWait(t *testing.T, duration time.Duration, predicate func() bool, name string) {
	t.Helper()
	deadline := time.Now().Add(duration)
	for time.Now().Before(deadline) {
		if predicate() {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatal("timed out waiting for", name)
}

func (c *mediaCluster) affiliations(t *testing.T, enabled bool) {
	t.Helper()
	states := []SiteReport{}
	for index, node := range []*integrationService{c.site, c.otherSite} {
		id := node.service.Config().InstanceID
		runtime := c.radios[id]
		runtime.mu.Lock()
		runtime.status.Affiliations = nil
		if enabled {
			rid := 10001
			if index == 1 {
				rid = 10003
			}
			runtime.status.Affiliations = []radio.Affiliation{{RID: rid, TGID: 1001, At: time.Now()}}
		}
		state := runtime.status
		runtime.mu.Unlock()
		ctx, cancel := context.WithTimeout(context.Background(), time.Second)
		err := c.zone.service.rfAuthority.apply(ctx, rfUpdateFromSource(runtime.SourceState()), c.zone.service.rfAuthorityScope())
		cancel()
		if err != nil {
			t.Fatal(err)
		}
		states = append(states, SiteReport{ID: id, ReceivedAt: time.Now(), Radio: &state})
	}
	if err := c.zone.service.writeCache("site_reports", states); err != nil {
		t.Fatal(err)
	}
	c.zone.service.refreshDirectory()
}

type browserMediaMessage struct {
	Encrypted   bool                     `json:"encrypted"`
	KeyID       int                      `json:"key_id"`
	AlgorithmID int                      `json:"algorithm_id"`
	TalkgroupID int                      `json:"talkgroup_id"`
	Active      bool                     `json:"active"`
	Type        string                   `json:"type"`
	SDP         string                   `json:"sdp"`
	Candidate   *webrtc.ICECandidateInit `json:"candidate"`
	State       dispatch.State           `json:"state"`
	Message     string                   `json:"message"`
	Action      string                   `json:"action"`
	RequestID   string                   `json:"request_id"`
}
type browserMediaFailure struct{ Action, RequestID, Message string }

func (e *browserMediaFailure) Error() string {
	return fmt.Sprintf("request %q (%s): %s", e.RequestID, e.Action, e.Message)
}

type browserMediaAudio struct {
	stream    string
	payload   []byte
	sequence  uint16
	timestamp uint32
	at        time.Time
}
type mediaBrowser struct {
	ctx      context.Context
	cancel   context.CancelFunc
	socket   *websocket.Conn
	http     *http.Client
	base     string
	peer     *webrtc.PeerConnection
	mic      *webrtc.TrackLocalStaticRTP
	mu       sync.Mutex
	state    chan dispatch.State
	audio    chan browserMediaAudio
	errors   chan error
	security chan browserMediaMessage
	seq      uint16
	stamp    uint32
	rtpEpoch time.Time
	keys     []dispatch.KeyIdentity
}

func newMediaBrowser(t *testing.T, node *integrationService, user User) *mediaBrowser {
	return newMediaBrowserCredentials(t, node, user, "media-integration-password", nil)
}
func nativeTestFrameCount(t *testing.T) int {
	t.Helper()
	value := os.Getenv("OPEN25_TEST_AUDIO_FRAMES")
	if value == "" {
		return 80
	}
	frames, err := strconv.Atoi(value)
	if err != nil || frames < 80 || frames > 500 {
		t.Fatal("live qualification frame count must be between 80 and 500")
	}
	return frames
}

func newMediaBrowserCredentials(t *testing.T, node *integrationService, user User, password string, transport http.RoundTripper) *mediaBrowser {
	t.Helper()
	jar, _ := cookiejar.New(nil)
	client := &http.Client{Jar: jar, Timeout: 3 * time.Second, Transport: transport}
	base := node.web.URL
	status, raw := integrationHTTP(t, client, "POST", base+"/api/login", map[string]string{"username": user.Username, "password": password}, map[string]string{"Origin": base})
	if status != 200 {
		t.Fatalf("browser login: %d %s", status, raw)
	}
	var login struct {
		CSRF string `json:"csrf"`
	}
	_ = json.Unmarshal(raw, &login)
	status, raw = integrationHTTP(t, client, "POST", base+"/api/dispatch/session", map[string]any{}, map[string]string{"Origin": base, "X-CSRF-Token": login.CSRF})
	if status != 201 {
		t.Fatalf("browser session: %d %s", status, raw)
	}
	var ticket struct {
		Ticket string                 `json:"ticket"`
		Keys   []dispatch.KeyIdentity `json:"keys"`
	}
	_ = json.Unmarshal(raw, &ticket)
	// The browser spans configuration syncs and multiple calls until test cleanup.
	ctx, cancel := context.WithCancel(t.Context())
	b := &mediaBrowser{ctx: ctx, cancel: cancel, http: client, base: base, keys: ticket.Keys, state: make(chan dispatch.State, 128), audio: make(chan browserMediaAudio, 128), errors: make(chan error, 4), security: make(chan browserMediaMessage, 256)}
	var err error
	b.socket, _, err = websocket.Dial(ctx, "ws"+strings.TrimPrefix(base, "http")+"/api/dispatch/audio?ticket="+ticket.Ticket, &websocket.DialOptions{HTTPClient: client, HTTPHeader: http.Header{"Origin": []string{base}}})
	if err != nil {
		cancel()
		t.Fatal(err)
	}
	engine := &webrtc.MediaEngine{}
	if err = engine.RegisterCodec(webrtc.RTPCodecParameters{RTPCodecCapability: webrtc.RTPCodecCapability{MimeType: webrtc.MimeTypePCMU, ClockRate: dispatch.SampleRate, Channels: 1}, PayloadType: 0}, webrtc.RTPCodecTypeAudio); err != nil {
		t.Fatal(err)
	}
	settings := webrtc.SettingEngine{}
	settings.SetIncludeLoopbackCandidate(true)
	b.peer, err = webrtc.NewAPI(webrtc.WithMediaEngine(engine), webrtc.WithSettingEngine(settings)).NewPeerConnection(webrtc.Configuration{})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { b.cancel(); _ = b.socket.CloseNow(); _ = b.peer.Close() })
	b.mic, err = webrtc.NewTrackLocalStaticRTP(webrtc.RTPCodecCapability{MimeType: webrtc.MimeTypePCMU, ClockRate: dispatch.SampleRate, Channels: 1}, "microphone", "microphone")
	if err != nil {
		t.Fatal(err)
	}
	tx, err := b.peer.AddTransceiverFromTrack(b.mic, webrtc.RTPTransceiverInit{Direction: webrtc.RTPTransceiverDirectionSendonly})
	if err != nil {
		t.Fatal(err)
	}
	go func() {
		buffer := make([]byte, 1500)
		for {
			if _, _, err := tx.Sender().Read(buffer); err != nil {
				return
			}
		}
	}()
	for range user.Talkgroups {
		if _, err = b.peer.AddTransceiverFromKind(webrtc.RTPCodecTypeAudio, webrtc.RTPTransceiverInit{Direction: webrtc.RTPTransceiverDirectionRecvonly}); err != nil {
			t.Fatal(err)
		}
	}
	b.peer.OnTrack(func(track *webrtc.TrackRemote, _ *webrtc.RTPReceiver) {
		go func() {
			for {
				packet, _, err := track.ReadRTP()
				if err != nil {
					return
				}
				select {
				case b.audio <- browserMediaAudio{stream: track.StreamID(), payload: slices.Clone(packet.Payload), sequence: packet.SequenceNumber, timestamp: packet.Timestamp, at: time.Now()}:
				default:
				}
			}
		}()
	})
	go b.read()
	b.peer.OnICECandidate(func(candidate *webrtc.ICECandidate) {
		if candidate != nil {
			_ = b.send(map[string]any{"type": "ice", "candidate": candidate.ToJSON()})
		}
	})
	offer, err := b.peer.CreateOffer(nil)
	if err != nil {
		t.Fatal(err)
	}
	if err = b.peer.SetLocalDescription(offer); err != nil {
		t.Fatal(err)
	}
	if err = b.send(map[string]any{"type": "offer", "sdp": offer.SDP}); err != nil {
		t.Fatal(err)
	}
	b.waitState(t, func(s dispatch.State) bool { return s.MediaConnected }, "WebRTC connection")
	if err = b.send(map[string]any{"type": "subscribe", "talkgroups": user.Talkgroups}); err != nil {
		t.Fatal(err)
	}
	b.waitState(t, func(s dispatch.State) bool { return slices.Equal(s.Subscriptions, user.Talkgroups) }, "talkgroup subscriptions")
	return b
}

func (b *mediaBrowser) send(value any) error {
	b.mu.Lock()
	defer b.mu.Unlock()
	raw, _ := json.Marshal(value)
	ctx, cancel := context.WithTimeout(b.ctx, time.Second)
	defer cancel()
	return b.socket.Write(ctx, websocket.MessageText, raw)
}

func (b *mediaBrowser) talkgroupPolicyReady(tg, algorithmID, keyID int) bool {
	ctx, cancel := context.WithTimeout(b.ctx, time.Second)
	defer cancel()
	request, err := http.NewRequestWithContext(ctx, http.MethodGet, b.base+"/api/state", nil)
	if err != nil {
		return false
	}
	response, err := b.http.Do(request)
	if err != nil {
		return false
	}
	defer response.Body.Close()
	if response.StatusCode != http.StatusOK {
		return false
	}
	var state struct {
		System struct {
			Talkgroups []Talkgroup `json:"talkgroups"`
		} `json:"system"`
	}
	if json.NewDecoder(io.LimitReader(response.Body, 4<<20)).Decode(&state) != nil {
		return false
	}
	return slices.ContainsFunc(state.System.Talkgroups, func(g Talkgroup) bool {
		return g.TGID == tg && g.Enabled && g.AdmissionMode == "both" && g.AlgorithmID == algorithmID && g.KeyID == keyID
	})
}

func (b *mediaBrowser) read() {
	pending := []webrtc.ICECandidateInit{}
	fail := func(err error) {
		select {
		case b.errors <- err:
		default:
		}
	}
	for {
		_, raw, err := b.socket.Read(b.ctx)
		if err != nil {
			if b.ctx.Err() == nil {
				fail(err)
			}
			return
		}
		var message browserMediaMessage
		if err = json.Unmarshal(raw, &message); err != nil {
			fail(err)
			return
		}
		switch message.Type {
		case "answer":
			if err = b.peer.SetRemoteDescription(webrtc.SessionDescription{Type: webrtc.SDPTypeAnswer, SDP: message.SDP}); err != nil {
				fail(err)
				return
			}
			for _, candidate := range pending {
				if err = b.peer.AddICECandidate(candidate); err != nil {
					fail(err)
					return
				}
			}
			pending = nil
		case "ice":
			if message.Candidate != nil {
				if b.peer.RemoteDescription() == nil {
					pending = append(pending, *message.Candidate)
				} else if err = b.peer.AddICECandidate(*message.Candidate); err != nil {
					fail(err)
					return
				}
			}
		case "state":
			select {
			case b.state <- message.State:
			default:
			}
		case "rx":
			select {
			case b.security <- message:
			default:
			}
		case "error":
			fail(&browserMediaFailure{message.Action, message.RequestID, message.Message})
		}
	}
}
func (b *mediaBrowser) waitState(t *testing.T, predicate func(dispatch.State) bool, name string) dispatch.State {
	t.Helper()
	ctx, cancel := context.WithTimeout(b.ctx, 5*time.Second)
	defer cancel()
	state, err := b.waitStateResult(ctx, predicate)
	if err != nil {
		t.Fatal(name, err)
	}
	return state
}
func (b *mediaBrowser) waitStateResult(ctx context.Context, predicate func(dispatch.State) bool) (dispatch.State, error) {
	for {
		select {
		case state := <-b.state:
			if predicate(state) {
				// The WebSocket reader enqueues an error before the following
				// idle state. Separate buffered channels must not reorder them
				// and leave the failed call's error for a later transmission.
				select {
				case err := <-b.errors:
					return state, err
				default:
					return state, nil
				}
			}
		case err := <-b.errors:
			return dispatch.State{}, err
		case <-ctx.Done():
			return dispatch.State{}, ctx.Err()
		}
	}
}
func (b *mediaBrowser) microphone(t *testing.T, count int) {
	t.Helper()
	for range count {
		packet := &rtp.Packet{Header: rtp.Header{Version: 2, PayloadType: 0, SSRC: 123456, SequenceNumber: b.seq, Timestamp: b.stamp}, Payload: bytes.Repeat([]byte{0xce}, dispatch.FrameSamples)}
		b.seq++
		b.stamp += dispatch.FrameSamples
		if err := b.mic.WriteRTP(packet); err != nil {
			t.Fatal(err)
		}
		time.Sleep(dispatch.FrameDuration)
	}
}
func (b *mediaBrowser) receive(t *testing.T, stream string) {
	b.receivePayload(t, stream, 0xce)
}
func (b *mediaBrowser) receivePayload(t *testing.T, stream string, value byte) {
	t.Helper()
	timer := time.NewTimer(4 * time.Second)
	defer timer.Stop()
	for {
		select {
		case audio := <-b.audio:
			if audio.stream == stream && bytes.Equal(audio.payload, bytes.Repeat([]byte{value}, dispatch.FrameSamples)) {
				return
			}
		case err := <-b.errors:
			t.Fatal("browser receive", err)
		case <-timer.C:
			t.Fatal("browser did not receive the expected live PCMU audio", stream)
		}
	}
}

// The service HTTP client has a five-second request timeout. A media link
// must remain open beyond that timeout without reconnecting or dropping RX.

// The connection stays open but never processes frames or responds to
// Ping. This models a partition that does not immediately reset TCP.

// Background synchronization is disabled in this fixture. Keep the
// receiving Site reports fresh during the five-second call, as production
// Site heartbeats do; stale Sites must leave the live audience.
