package framework

import (
	"fmt"
	"open25.local/open25-fne/internal/dispatch"
	"open25.local/open25-fne/internal/p25crypto"
	"sync"
	"time"
)

// keyRuntime belongs only to Application Server. System owns custody and authorized
// distribution; neither the Zone nor any Site receives plaintext traffic keys.
type keyRuntime struct {
	// Rebuilding a process may perform I/O. Media only takes mu for a snapshot,
	// never reconcileMu, and no process operation runs with mu held.
	reconcileMu sync.Mutex
	mu          sync.Mutex
	codec       *dispatch.PacketCodec
	digest      string
	keys        map[p25crypto.Identity]bool
	receiveMu   sync.Mutex
	receive     map[receiveCodecIdentity]receiveCodecBinding
}

type receiveCodecIdentity struct {
	call          string
	source, group int
}
type receiveCodecBinding struct {
	codec   *dispatch.PacketCodec
	last    time.Time
	blocked bool
}

const receiveCodecIdle = 30 * time.Second
const maxReceiveCodecBindings = 256

var errAudioProcessChanged = fmt.Errorf("%w: Audio processing changed; start a new transmission.", dispatch.ErrUnavailable)

func receiveIdentity(frame dispatch.Frame) receiveCodecIdentity {
	return receiveCodecIdentity{frame.CallID, frame.SourceID, frame.TalkgroupID}
}

func (r *keyRuntime) snapshot() (*dispatch.PacketCodec, map[p25crypto.Identity]bool, string) {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.codec, r.keys, r.digest
}
func (r *keyRuntime) detach() *dispatch.PacketCodec {
	r.mu.Lock()
	defer r.mu.Unlock()
	old := r.codec
	r.codec = nil
	r.keys = map[p25crypto.Identity]bool{}
	r.digest = ""
	return old
}
func (s *Service) currentPacketCodec(owner *dispatch.PacketCodec) bool {
	current, _ := s.packetCodec()
	return owner == current && (owner == nil || owner.Alive())
}

// A receiving stream never moves to another codec process, even if the same
// numeric ALGID/KID is present there with new material. End removes ownership;
// idle expiry and a hard cap bound missing-End state without evicting live calls.
func (r *keyRuntime) receiveOwner(frame dispatch.Frame, codec *dispatch.PacketCodec, now time.Time) (*dispatch.PacketCodec, error) {
	r.receiveMu.Lock()
	defer r.receiveMu.Unlock()
	if r.receive == nil {
		r.receive = map[receiveCodecIdentity]receiveCodecBinding{}
	}
	for key, value := range r.receive {
		if now.Sub(value.last) > receiveCodecIdle {
			delete(r.receive, key)
		}
	}
	identity := receiveIdentity(frame)
	if binding, exists := r.receive[identity]; exists {
		binding.last = now
		if binding.blocked || binding.codec != codec || codec == nil {
			binding.blocked = true
			r.receive[identity] = binding
			return nil, errAudioProcessChanged
		}
		r.receive[identity] = binding
		return binding.codec, nil
	}
	if codec == nil || len(r.receive) >= maxReceiveCodecBindings {
		return nil, dispatch.ErrUnavailable
	}
	r.receive[identity] = receiveCodecBinding{codec: codec, last: now}
	return codec, nil
}
func (r *keyRuntime) blockReceive(frame dispatch.Frame, codec *dispatch.PacketCodec) {
	r.receiveMu.Lock()
	defer r.receiveMu.Unlock()
	key := receiveIdentity(frame)
	if binding, ok := r.receive[key]; ok && binding.codec == codec {
		binding.blocked = true
		r.receive[key] = binding
	}
}
func (r *keyRuntime) receiveOwned(frame dispatch.Frame, codec *dispatch.PacketCodec) bool {
	r.receiveMu.Lock()
	defer r.receiveMu.Unlock()
	binding, ok := r.receive[receiveIdentity(frame)]
	return ok && !binding.blocked && binding.codec == codec
}
func (r *keyRuntime) endReceive(frame dispatch.Frame) *dispatch.PacketCodec {
	r.receiveMu.Lock()
	defer r.receiveMu.Unlock()
	key := receiveIdentity(frame)
	binding := r.receive[key]
	delete(r.receive, key)
	return binding.codec
}

func (r *keyRuntime) activeReceiveOwner(frame dispatch.Frame) *dispatch.PacketCodec {
	r.receiveMu.Lock()
	defer r.receiveMu.Unlock()
	binding, ok := r.receive[receiveIdentity(frame)]
	if !ok || binding.blocked {
		return nil
	}
	return binding.codec
}
