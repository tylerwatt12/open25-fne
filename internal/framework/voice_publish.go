package framework

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"open25.local/open25-fne/internal/activity"
	"open25.local/open25-fne/internal/dispatch"
	"open25.local/open25-fne/internal/routing"
	"slices"
	"strconv"
	"sync"
	"time"
)

type publicationKey struct {
	peer, call string
	group      int
}
type mediaPublication struct {
	ctx     context.Context
	cancel  context.CancelFunc
	call    *voiceCall
	pending []voiceInput
}

// Failed legs stay failed until allocation End. Reconnection never replays an
// active call. After SIP admission, media enters the call's sole queue.
type mediaPeers struct {
	service *Service
	mu      sync.Mutex
	calls   map[publicationKey]*mediaPublication
	closed  bool
	wg      sync.WaitGroup
}

func newMediaPeers(s *Service) *mediaPeers {
	return &mediaPeers{service: s, calls: map[publicationKey]*mediaPublication{}}
}
func (p *mediaPeers) Publish(request routing.Request, f dispatch.Frame) {
	if p.service.voice.Load() == nil {
		return
	}
	p.service.voice.Load().mu.Lock()
	peers := []voicePeer{}
	for _, peer := range p.service.voice.Load().peers {
		if peer.credential.Role == RoleApplication {
			peers = append(peers, peer)
		}
	}
	p.service.voice.Load().mu.Unlock()
	for _, peer := range peers {
		key := publicationKey{peer.endpoint.PeerID, f.CallID, f.TalkgroupID}
		p.mu.Lock()
		if p.closed {
			p.mu.Unlock()
			return
		}
		item := p.calls[key]
		if item == nil && !f.End && p.service.receiveOwnerAllowed(peer.credential.InstanceID, f.TalkgroupID) && len(p.calls) < 64 {
			ctx, cancel := context.WithCancel(p.service.operationCtx)
			item = &mediaPublication{ctx: ctx, cancel: cancel}
			p.calls[key] = item
			p.wg.Add(1)
			q := request
			digest := sha256.Sum256([]byte(key.peer + "\x00" + key.call + "\x00" + strconv.Itoa(key.group)))
			q.ID = hex.EncodeToString(digest[:])
			q.Talkgroups = []int{f.TalkgroupID}
			go p.publish(peer, q, item)
		}
		if item != nil {
			if f.End {
				delete(p.calls, key)
				item.cancel()
			} else if item.ctx.Err() == nil {
				if item.call != nil {
					if err := item.call.Write(f); err != nil {
						item.cancel()
					}
				} else if len(item.pending) < 11 {
					f.Encoded = slices.Clone(f.Encoded)
					f.P25 = f.P25.Clone()
					f.PCM = nil
					f.MessageIndicator = nil
					item.pending = append(item.pending, voiceInput{frame: f})
				} else {
					item.cancel()
					item.pending = nil
				}
			}
		}
		p.mu.Unlock()
	}
}
func (p *mediaPeers) publish(peer voicePeer, q routing.Request, item *mediaPublication) {
	defer p.wg.Done()
	defer item.cancel()
	setup, cancel := context.WithTimeout(item.ctx, 2*time.Second)
	call, err := p.service.voice.Load().begin(setup, peer.endpoint, q, true)
	cancel()
	if err != nil {
		if item.ctx.Err() == nil {
			p.service.emit(activity.Event{CallID: q.LogicalCallID, RadioID: q.RadioID, TalkgroupID: q.Talkgroups[0], Type: "media_link", Audience: "diagnostic", Severity: "warning", Outcome: "failed", Message: "Application Server receive could not start: " + err.Error()})
		}
		return
	}
	defer func() {
		ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
		defer cancel()
		_ = call.End(ctx)
	}()
	p.mu.Lock()
	if item.ctx.Err() != nil {
		p.mu.Unlock()
		return
	}
	for _, in := range item.pending {
		if in.endSpurt {
			err = call.queueSpurtEnd()
		} else if time.Since(in.frame.CapturedAt) > 240*time.Millisecond {
			err = dispatch.ErrUnavailable
		} else {
			err = call.Write(in.frame)
		}
		if err != nil {
			item.cancel()
			break
		}
	}
	item.pending = nil
	item.call = call
	p.mu.Unlock()
	select {
	case <-item.ctx.Done():
	case <-call.done:
	}
}
func (p *mediaPeers) EndSpurt(q routing.Request) {
	p.mu.Lock()
	defer p.mu.Unlock()
	for key, item := range p.calls {
		if key.call != q.ID || item.ctx.Err() != nil {
			continue
		}
		if item.call != nil {
			if err := item.call.queueSpurtEnd(); err != nil {
				item.cancel()
			}
		} else if len(item.pending) < 11 {
			item.pending = append(item.pending, voiceInput{endSpurt: true})
		} else {
			item.cancel()
			item.pending = nil
		}
	}
}
func (p *mediaPeers) Close() {
	p.mu.Lock()
	p.closed = true
	for _, item := range p.calls {
		item.cancel()
	}
	p.mu.Unlock()
	p.wg.Wait()
}
