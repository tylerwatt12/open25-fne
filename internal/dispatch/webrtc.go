package dispatch

import (
	"encoding/binary"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/pion/interceptor"
	"github.com/pion/rtp"
	"github.com/pion/webrtc/v4"
)

type rxTrack struct {
	track *webrtc.TrackLocalStaticRTP
	queue chan Frame
}

func mediaAPI(minPort, maxPort uint16, loopback bool) (*webrtc.API, error) {
	m := &webrtc.MediaEngine{}
	if err := m.RegisterCodec(webrtc.RTPCodecParameters{RTPCodecCapability: webrtc.RTPCodecCapability{MimeType: webrtc.MimeTypePCMU, ClockRate: SampleRate, Channels: 1}, PayloadType: 0}, webrtc.RTPCodecTypeAudio); err != nil {
		return nil, err
	}
	i := &interceptor.Registry{}
	if err := webrtc.RegisterDefaultInterceptors(m, i); err != nil {
		return nil, err
	}
	settings := webrtc.SettingEngine{}
	settings.SetIncludeLoopbackCandidate(loopback)
	if minPort != 0 || maxPort != 0 {
		if minPort == 0 || maxPort < minPort {
			return nil, errors.New("invalid WebRTC UDP port range")
		}
		if err := settings.SetEphemeralUDPPortRange(minPort, maxPort); err != nil {
			return nil, err
		}
	}
	return webrtc.NewAPI(webrtc.WithMediaEngine(m), webrtc.WithInterceptorRegistry(i), webrtc.WithSettingEngine(settings)), nil
}

func (c *client) offer(sdp string) error {
	if len(sdp) == 0 || len(sdp) > 65536 {
		return errors.New("audio offer is invalid")
	}
	c.peerMu.Lock()
	if c.peer != nil || c.peerClosed {
		c.peerMu.Unlock()
		return errors.New("reconnect audio before negotiating again")
	}
	api, err := mediaAPI(c.hub.options.ICEPortMin, c.hub.options.ICEPortMax, c.hub.options.LoopbackCandidates)
	if err != nil {
		c.peerMu.Unlock()
		return err
	}
	peer, err := api.NewPeerConnection(webrtc.Configuration{})
	if err != nil {
		c.peerMu.Unlock()
		return err
	}
	c.peer = peer
	c.peerMu.Unlock()
	failed := true
	defer func() {
		if failed {
			c.cancel()
			c.closePeer()
		}
	}()
	peer.OnICECandidate(func(candidate *webrtc.ICECandidate) {
		if candidate != nil {
			c.enqueue(map[string]any{"type": "ice", "candidate": candidate.ToJSON()})
		}
	})
	peer.OnConnectionStateChange(func(state webrtc.PeerConnectionState) {
		if state == webrtc.PeerConnectionStateConnected {
			c.connected.Store(true)
			c.hub.sendState(c)
		} else if state == webrtc.PeerConnectionStateDisconnected || state == webrtc.PeerConnectionStateFailed || state == webrtc.PeerConnectionStateClosed {
			c.connected.Store(false)
			c.cancel()
		}
	})
	peer.OnTrack(func(track *webrtc.TrackRemote, _ *webrtc.RTPReceiver) {
		c.peerMu.Lock()
		if c.peerClosed || c.ctx.Err() != nil {
			c.peerMu.Unlock()
			return
		}
		if c.txTrack.Swap(true) {
			c.peerMu.Unlock()
			c.cancel()
			return
		}
		c.mediaWG.Add(1)
		c.peerMu.Unlock()
		go func() { defer c.mediaWG.Done(); c.readMicrophone(track) }()
	})
	if _, err = peer.AddTransceiverFromKind(webrtc.RTPCodecTypeAudio, webrtc.RTPTransceiverInit{Direction: webrtc.RTPTransceiverDirectionRecvonly}); err != nil {
		return err
	}
	// Each resource has an independent stream identity so browser mixers can
	// listen to several groups and route/attenuate each one independently.
	for _, tg := range c.principal.ReceiveGroups {
		stream := fmt.Sprintf("tg-%d", tg)
		track, err := webrtc.NewTrackLocalStaticRTP(webrtc.RTPCodecCapability{MimeType: webrtc.MimeTypePCMU, ClockRate: SampleRate, Channels: 1}, stream, stream)
		if err != nil {
			return err
		}
		transceiver, err := peer.AddTransceiverFromTrack(track, webrtc.RTPTransceiverInit{Direction: webrtc.RTPTransceiverDirectionSendonly})
		if err != nil {
			return err
		}
		sender := transceiver.Sender()
		rt := &rxTrack{track: track, queue: make(chan Frame, 24)}
		c.peerMu.Lock()
		if c.peerClosed || c.ctx.Err() != nil {
			c.peerMu.Unlock()
			return errors.New("audio session disconnected during negotiation")
		}
		c.tracks[tg] = rt
		c.mediaWG.Add(2)
		c.peerMu.Unlock()
		go func() {
			defer c.mediaWG.Done()
			buffer := make([]byte, 1500)
			for {
				if _, _, err := sender.Read(buffer); err != nil {
					return
				}
			}
		}()
		go func() { defer c.mediaWG.Done(); c.writeResource(rt) }()
	}
	if err = peer.SetRemoteDescription(webrtc.SessionDescription{Type: webrtc.SDPTypeOffer, SDP: sdp}); err != nil {
		return errors.New("audio offer is incompatible with PCMU")
	}
	answer, err := peer.CreateAnswer(nil)
	if err != nil {
		return err
	}
	if err = peer.SetLocalDescription(answer); err != nil {
		return err
	}
	c.enqueue(map[string]any{"type": "answer", "sdp": answer.SDP})
	failed = false
	return nil
}

func (c *client) readMicrophone(track *webrtc.TrackRemote) {
	codec := track.Codec()
	if !strings.EqualFold(codec.MimeType, webrtc.MimeTypePCMU) || codec.ClockRate != SampleRate || codec.Channels > 1 {
		c.cancel()
		return
	}
	var lastSequence uint16
	var clock uint32
	var base time.Time
	var expected uint32
	var pending []byte
	var pendingClock uint32
	initialized := false
	for {
		packet, _, err := track.ReadRTP()
		if err != nil {
			return
		}
		if c.ctx.Err() != nil {
			return
		}
		if packet.PayloadType != 0 || len(packet.Payload) == 0 || len(packet.Payload) > 960 {
			continue
		}
		if initialized && int16(packet.SequenceNumber-lastSequence) <= 0 {
			continue
		}
		lastSequence = packet.SequenceNumber
		now := time.Now()
		if !initialized {
			clock = packet.Timestamp
			base = now
			initialized = true
		}
		at := base.Add(time.Duration(int32(packet.Timestamp-clock)) * time.Second / SampleRate)
		if now.Sub(at) > c.hub.options.MaxAudioAge || at.Sub(now) > time.Second {
			pending = nil
			continue
		}
		if expected != packet.Timestamp {
			pending = nil
		}
		expected = packet.Timestamp + uint32(len(packet.Payload))
		if len(pending) == 0 {
			pendingClock = packet.Timestamp
		}
		pending = append(pending, packet.Payload...)
		for len(pending) >= FrameSamples {
			at := base.Add(time.Duration(int32(pendingClock-clock)) * time.Second / SampleRate)
			_ = c.hub.acceptPCM(c, MuLawToPCM(pending[:FrameSamples]), at)
			pending = pending[FrameSamples:]
			pendingClock += FrameSamples
		}
	}
}

func (c *client) writeResource(resource *rxTrack) {
	var sequence uint16
	var timestamp uint32
	var next time.Time
	var base time.Time
	for {
		select {
		case <-c.ctx.Done():
			return
		case frame := <-resource.queue:
			if time.Since(frame.CapturedAt) > c.hub.options.MaxAudioAge {
				continue
			}
			c.hub.mu.Lock()
			subscribed := c.subscriptions[frame.TalkgroupID]
			c.hub.mu.Unlock()
			if !subscribed || !c.connected.Load() {
				continue
			}
			if delay := time.Until(next); delay > 0 {
				timer := time.NewTimer(delay)
				select {
				case <-c.ctx.Done():
					timer.Stop()
					return
				case <-timer.C:
				}
			}
			now := time.Now()
			if time.Since(frame.CapturedAt) > c.hub.options.MaxAudioAge {
				continue
			}
			if base.IsZero() {
				base = now
			} else {
				elapsed := uint32(now.Sub(base)/FrameDuration) * FrameSamples
				if int32(elapsed-timestamp) > 0 {
					timestamp = elapsed
				}
			}
			packet := &rtp.Packet{Header: rtp.Header{Version: 2, PayloadType: 0, SequenceNumber: sequence, Timestamp: timestamp}, Payload: PCMToMuLaw(frame.PCM)}
			if err := resource.track.WriteRTP(packet); err != nil {
				c.cancel()
				return
			}
			sequence++
			timestamp += FrameSamples
			if next.IsZero() || now.Sub(next) > c.hub.options.MaxAudioAge {
				next = now
			}
			next = next.Add(FrameDuration)
		}
	}
}

// G.711 companding is only the browser transport. P25 IMBE encoding/decoding
// remains in the licensed/qualified native vocoder endpoint.
func MuLawToPCM(input []byte) []byte {
	out := make([]byte, len(input)*2)
	for i, b := range input {
		u := ^b
		v := (int(u&15) << 3) + 132
		v <<= uint((u & 112) >> 4)
		v -= 132
		if u&128 != 0 {
			v = -v
		}
		binary.LittleEndian.PutUint16(out[i*2:], uint16(int16(v)))
	}
	return out
}
func PCMToMuLaw(input []byte) []byte {
	out := make([]byte, len(input)/2)
	for i := range out {
		v := int(int16(binary.LittleEndian.Uint16(input[i*2:])))
		mask := byte(255)
		if v < 0 {
			v = -v
			mask = 127
		}
		if v > 32635 {
			v = 32635
		}
		v += 132
		exp := 7
		for bit := 16384; exp > 0 && v&bit == 0; bit >>= 1 {
			exp--
		}
		mantissa := (v >> uint(exp+3)) & 15
		out[i] = byte(exp<<4|mantissa) ^ mask
	}
	return out
}
