package dispatch

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/coder/websocket"
	"github.com/pion/webrtc/v4"
)

type client struct {
	id            string
	principal     Principal
	session       [32]byte
	hub           *Hub
	ctx           context.Context
	cancel        context.CancelFunc
	send          chan any
	created       time.Time
	connected     atomic.Bool
	file          atomic.Bool
	subscriptions map[int]bool    // hub.mu
	usedRequests  map[string]bool // hub.mu
	endedRequest  string          // hub.mu
	peerMu        sync.Mutex
	peer          *webrtc.PeerConnection
	tracks        map[int]*rxTrack
	peerClosed    bool
	closeOnce     sync.Once
	txTrack       atomic.Bool
	mediaWG       sync.WaitGroup
}

func (c *client) enqueue(message any) {
	select {
	case <-c.ctx.Done():
		return
	default:
	}
	select {
	case c.send <- message:
	default:
		c.cancel()
	}
}
func (c *client) enqueueAudio(frame Frame) {
	c.peerMu.Lock()
	track := c.tracks[frame.TalkgroupID]
	c.peerMu.Unlock()
	if track == nil {
		return
	}
	select {
	case <-c.ctx.Done():
		return
	case track.queue <- frame:
	default:
		c.cancel()
	}
}
func (c *client) closePeer() {
	c.closeOnce.Do(func() {
		c.connected.Store(false)
		c.peerMu.Lock()
		c.peerClosed = true
		pc := c.peer
		c.peer = nil
		c.peerMu.Unlock()
		if pc != nil {
			_ = pc.Close()
		}
	})
}

func SameOrigin(r *http.Request) bool {
	origin := r.Header.Get("Origin")
	if origin == "" || r.Header.Get("Sec-Fetch-Site") == "cross-site" {
		return false
	}
	u, err := url.Parse(origin)
	scheme := "http"
	if r.TLS != nil {
		scheme = "https"
	}
	return err == nil && u.Scheme == scheme && u.Host == r.Host && u.User == nil && u.Path == ""
}
func writeJSON(w http.ResponseWriter, status int, value any) {
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("Cache-Control", "no-store")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(value)
}
func writeError(w http.ResponseWriter, status int, err error) {
	writeJSON(w, status, map[string]string{"error": err.Error()})
}
func decodeJSON(raw []byte, target any) error {
	d := json.NewDecoder(bytes.NewReader(raw))
	d.DisallowUnknownFields()
	if err := d.Decode(target); err != nil {
		return err
	}
	if err := d.Decode(new(any)); err != io.EOF {
		return errors.New("provide one JSON object")
	}
	return nil
}

// ServeSession/ServeSocket require authentication and CSRF enforcement by the
// application router. The one-use ticket additionally binds the media socket to
// the authenticated account, virtual RID and login session.
func (h *Hub) ServeSession(w http.ResponseWriter, r *http.Request, p Principal, sessionKey string, keys []KeyIdentity) {
	if r.Method != "POST" {
		w.WriteHeader(405)
		return
	}
	if !SameOrigin(r) {
		writeError(w, 403, ErrPermission)
		return
	}
	var empty struct{}
	raw, err := io.ReadAll(http.MaxBytesReader(w, r.Body, 1024))
	if err != nil || decodeJSON(raw, &empty) != nil {
		writeError(w, 400, errors.New("send an empty JSON object"))
		return
	}
	p, err = validPrincipal(p)
	if err != nil {
		writeError(w, 403, err)
		return
	}
	token, err := h.IssueTicket(p, sessionKey)
	if err != nil {
		writeError(w, 409, err)
		return
	}
	if keys == nil {
		keys = []KeyIdentity{}
	}
	writeJSON(w, 201, map[string]any{"ticket": token, "socket_path": "/api/dispatch/audio", "expires_in": 30, "codec": "PCMU", "sample_rate": SampleRate, "frame_samples": FrameSamples, "radio_id": p.RadioID, "receive_groups": p.ReceiveGroups, "transmit_groups": p.TransmitGroups, "keys": keys})
}

func (h *Hub) ServeState(w http.ResponseWriter, r *http.Request, p Principal) {
	if r.Method != "GET" {
		w.WriteHeader(405)
		return
	}
	if _, err := validPrincipal(p); err != nil {
		writeError(w, 403, err)
		return
	}
	writeJSON(w, 200, h.Overview(p))
}

func (h *Hub) ServeSocket(w http.ResponseWriter, r *http.Request, p Principal, sessionKey string) {
	if r.Method != "GET" {
		w.WriteHeader(405)
		return
	}
	if !SameOrigin(r) {
		writeError(w, 403, ErrPermission)
		return
	}
	var err error
	p, err = validPrincipal(p)
	if err != nil {
		writeError(w, 403, err)
		return
	}
	if err = h.consumeTicket(r.URL.Query().Get("ticket"), p, sessionKey); err != nil {
		writeError(w, 403, err)
		return
	}
	c, err := h.openClient(p, sessionKey)
	if err != nil {
		writeError(w, 409, err)
		return
	}
	defer h.removeClient(c)
	conn, err := websocket.Accept(w, r, nil)
	if err != nil {
		return
	}
	defer conn.CloseNow()
	conn.SetReadLimit(65536)
	stopRequest := context.AfterFunc(r.Context(), c.cancel)
	defer stopRequest()
	h.sendState(c)
	done := make(chan struct{})
	go func() {
		defer close(done)
		for {
			select {
			case <-c.ctx.Done():
				_ = conn.CloseNow()
				return
			case message := <-c.send:
				raw, err := json.Marshal(message)
				if err != nil {
					c.cancel()
					return
				}
				ctx, cancel := context.WithTimeout(c.ctx, time.Second)
				err = conn.Write(ctx, websocket.MessageText, raw)
				cancel()
				if err != nil {
					c.cancel()
					return
				}
			}
		}
	}()
	defer func() { c.cancel(); c.closePeer(); <-done; c.mediaWG.Wait() }()
	for {
		kind, raw, err := conn.Read(c.ctx)
		if err != nil {
			return
		}
		if kind != websocket.MessageText {
			c.enqueue(map[string]any{"type": "error", "message": "Audio uses WebRTC; send JSON control messages on this connection."})
			continue
		}
		var request struct {
			Type        string                   `json:"type"`
			SDP         string                   `json:"sdp,omitempty"`
			Candidate   *webrtc.ICECandidateInit `json:"candidate,omitempty"`
			Talkgroups  []int                    `json:"talkgroups,omitempty"`
			Secure      bool                     `json:"secure"`
			AlgorithmID int                      `json:"algorithm_id"`
			KeyID       int                      `json:"key_id"`
			RequestID   string                   `json:"request_id,omitempty"`
		}
		if err = decodeJSON(raw, &request); err != nil {
			c.enqueue(map[string]any{"type": "error", "message": "Invalid audio control message."})
			continue
		}
		switch request.Type {
		case "offer":
			err = c.offer(request.SDP)
		case "ice":
			c.peerMu.Lock()
			peer := c.peer
			c.peerMu.Unlock()
			if request.Candidate != nil {
				if peer == nil {
					err = errors.New("send an offer before ICE candidates")
				} else {
					err = peer.AddICECandidate(*request.Candidate)
				}
			}
		case "subscribe":
			err = h.subscribe(c, request.Talkgroups)
		case "ptt_start":
			_, err = h.start(c, request.Talkgroups, request.RequestID, false, request.Secure, request.AlgorithmID, request.KeyID)
		case "ptt_renew":
			err = h.renew(c, request.RequestID)
		case "ptt_stop":
			err = h.stopRequest(c, request.RequestID)
		case "ping":
			c.enqueue(map[string]any{"type": "pong"})
		default:
			err = errors.New("unknown audio control message")
		}
		if err != nil {
			c.enqueue(map[string]any{"type": "error", "action": request.Type, "request_id": request.RequestID, "message": err.Error()})
		}
	}
}

// AccountDigest is useful to an outer router which wants to tie active audio
// to its password/permission state without exposing any password material.
func AccountDigest(value []byte) [32]byte { return sha256.Sum256(value) }

func (h *Hub) ServeWAV(w http.ResponseWriter, r *http.Request, p Principal, sessionKey string) {
	if r.Method != "POST" {
		w.WriteHeader(405)
		return
	}
	if !SameOrigin(r) {
		writeError(w, 403, ErrPermission)
		return
	}
	groups, err := parseGroups(r.URL.Query()["talkgroup_id"])
	if err != nil {
		writeError(w, 400, err)
		return
	}
	if mediaType := strings.Split(r.Header.Get("Content-Type"), ";")[0]; mediaType != "audio/wav" && mediaType != "audio/x-wav" && mediaType != "application/octet-stream" {
		writeError(w, 415, errors.New("send an 8 kHz mono PCM WAV file"))
		return
	}
	raw, err := io.ReadAll(http.MaxBytesReader(w, r.Body, 512<<10))
	if err != nil {
		writeError(w, 413, errors.New("WAV file is too large"))
		return
	}
	pcm, err := ParseWAV(raw)
	if err != nil {
		writeError(w, 400, err)
		return
	}
	secure := r.URL.Query().Get("secure") == "true"
	algorithm, key := r.URL.Query().Get("algorithm_id"), r.URL.Query().Get("key_id")
	algorithmID, algorithmErr := strconv.Atoi(algorithm)
	keyID, keyErr := strconv.Atoi(key)
	if (algorithm != "" && algorithmErr != nil) || (key != "" && keyErr != nil) {
		writeError(w, 400, errors.New("algorithm_id and key_id must be decimal numbers"))
		return
	}
	frames, err := h.PlayPCM(r.Context(), p, sessionKey, groups, pcm, secure, algorithmID, keyID)
	if err != nil {
		writeError(w, 409, err)
		return
	}
	writeJSON(w, 200, map[string]any{"completed": true, "frames": frames, "duration_ms": frames * 20})
}
