package framework

import (
	"context"
	"errors"
	"fmt"
	"open25.local/open25-fne/internal/activity"
	"open25.local/open25-fne/internal/dispatch"
	"open25.local/open25-fne/internal/p25crypto"
	"open25.local/open25-fne/internal/p25rtp"
	"open25.local/open25-fne/internal/routing"
	"open25.local/open25-fne/internal/rtpvoice"
	"open25.local/open25-fne/internal/sipvoice"
	"slices"
	"sync"
	"time"
)

type voiceInput struct {
	frame             dispatch.Frame
	endSpurt, endCall bool
	result            chan error
}
type voiceBurst struct {
	source                      sipvoice.Source
	inputEpoch                  uint64
	engine                      *rtpvoice.Engine
	runDone, consumed, finished chan struct{}
	runError                    error
	reason                      rtpvoice.EngineReason // read after consumed closes
	started                     bool
	position                    uint64
	positionKnown               bool
	lastAt                      time.Time
}

// SIP owns the allocation. Each actual source epoch owns one borrowed-session
// RTP Engine; a normal RF burst end does not release a held channel allocation.
type voiceCall struct {
	runtime                    *voiceRuntime
	peerID                     string
	peer                       PeerCredential
	request                    routing.Request
	grant                      dispatch.Grant
	receive, source            bool
	origin                     p25rtp.Role
	pair                       *rtpvoice.Pair
	sip                        *sipvoice.Session
	ctx                        context.Context
	cancel                     context.CancelFunc
	mu                         sync.Mutex
	transport                  *rtpvoice.Session
	transportReady             chan struct{}
	startError, endError       error
	lastStats                  rtpvoice.EngineStats
	lastReason                 rtpvoice.EngineReason
	active                     *voiceBurst
	lastSource                 sipvoice.Source
	lastInputEpoch             uint64
	nextBurst                  chan *voiceBurst
	inputMu                    sync.Mutex
	ending                     bool
	inputs                     chan voiceInput // At most twelve actual pre-ready words, including Header.
	rxSequence                 uint64
	ready, cleaned, done, stop chan struct{}
	readyOnce, stopOnce        sync.Once
	failureOnce                sync.Once
}

func (v *voiceRuntime) newCall(id string, p PeerCredential, q routing.Request, g dispatch.Grant, receive, source bool, origin p25rtp.Role, pair *rtpvoice.Pair) *voiceCall {
	ctx, cancel := context.WithCancel(v.s.operationCtx)
	return &voiceCall{runtime: v, peerID: id, peer: p, request: q, grant: g, receive: receive, source: source, origin: origin, pair: pair, ctx: ctx, cancel: cancel, transportReady: make(chan struct{}), nextBurst: make(chan *voiceBurst, 1), inputs: make(chan voiceInput, 11), ready: make(chan struct{}), cleaned: make(chan struct{}), done: make(chan struct{}), stop: make(chan struct{})}
}
func (c *voiceCall) release() {
	c.stopOnce.Do(func() { close(c.stop) })
	select {
	case <-c.cleaned:
	case <-time.After(2500 * time.Millisecond):
		c.cancel()
	}
}
func (c *voiceCall) spurt(source sipvoice.Source) rtpvoice.Spurt {
	q := c.request
	system := c.runtime.s.view().System
	id := p25rtp.SUID{WACN: uint32(system.WACN), System: uint16(system.SystemID), Unit: uint32(q.RadioID)}
	s := rtpvoice.Spurt{Origin: c.origin, TSN: source.TSN, GroupID: uint16(q.Talkgroups[0]), ServiceOptions: source.Service}
	priority := source.Service & 7
	if q.SourceKind == "rf" {
		s.RF = &p25rtp.RFControlWord{Source: id, TransmitPriority: priority}
	} else {
		s.Console = &p25rtp.ConsoleControlWord{Console: id, PhysicalConsole: id, VoiceSource: id, TransmitPriority: priority, ConsoleTXPriority: 0x10}
	}
	return s
}
func (c *voiceCall) sourceValid(source sipvoice.Source) bool {
	q := c.request
	secure := source.Service&0x40 != 0
	identity := source.AlgorithmID == p25crypto.Clear && source.KeyID == 0
	if secure {
		identity = p25crypto.ValidSelection(true, int(source.AlgorithmID), int(source.KeyID))
	}
	if secure && !q.EncryptionPending {
		identity = identity && int(source.AlgorithmID) == q.AlgorithmID && int(source.KeyID) == q.KeyID
	}
	return identity && source.MFID <= 1 && (source.Service&0x80 != 0) == q.Emergency && secure == q.Secure && (source.TSN%2 == 1) == (c.origin == p25rtp.GroupHome)
}
func (c *voiceCall) bind(ctx context.Context, source sipvoice.Source) error {
	if c.source || !c.sourceValid(source) {
		return dispatch.ErrPermission
	}
	select {
	case <-c.transportReady:
	case <-ctx.Done():
		return ctx.Err()
	case <-c.ctx.Done():
		return dispatch.ErrUnavailable
	}
	c.mu.Lock()
	old := c.active
	c.mu.Unlock()
	if old != nil {
		select {
		case <-old.finished:
		case <-ctx.Done():
			return ctx.Err()
		case <-c.ctx.Done():
			return dispatch.ErrUnavailable
		}
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.startError != nil || c.transport == nil || c.ctx.Err() != nil {
		return dispatch.ErrUnavailable
	}
	engine, err := rtpvoice.NewEngine(c.transport, c.spurt(source))
	if err != nil {
		return err
	}
	b := &voiceBurst{source: source, engine: engine, runDone: make(chan struct{}), consumed: make(chan struct{}), finished: make(chan struct{})}
	c.active = b
	c.lastSource = source
	select {
	case c.nextBurst <- b:
		return nil
	default:
		c.active = nil
		_ = engine.Close()
		return dispatch.ErrBusy
	}
}
func (c *voiceCall) run() {
	defer c.runtime.wg.Done()
	defer close(c.done)
	defer c.cancel()
	defer func() { c.runtime.mu.Lock(); delete(c.runtime.calls, c); c.runtime.mu.Unlock() }()
	local := c.origin
	if !c.source {
		if local == p25rtp.GroupHome {
			local = p25rtp.GroupServing
		} else {
			local = p25rtp.GroupHome
		}
	}
	remoteRole := p25rtp.GroupHome
	if local == p25rtp.GroupHome {
		remoteRole = p25rtp.GroupServing
	}
	_, remote := c.sip.Media()
	transport, err := rtpvoice.New(c.pair, rtpvoice.Options{Remote: remote.RTP, LocalRole: local, RemoteRole: remoteRole})
	c.mu.Lock()
	c.transport, c.startError = transport, err
	close(c.transportReady)
	c.mu.Unlock()
	defer func() {
		c.mu.Lock()
		b := c.active
		c.mu.Unlock()
		if b != nil {
			c.finishBurst(b, true)
		}
		if transport != nil {
			_ = transport.Close()
		} else {
			_ = c.pair.Close()
		}
		if !c.source && !c.receive {
			ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
			e := c.runtime.s.broker.End(ctx, c.peerID, c.grant)
			cancel()
			if e != nil {
				c.mu.Lock()
				c.endError = errors.Join(c.endError, e)
				c.mu.Unlock()
			}
		}
		if !c.source && c.receive {
			q := c.request
			c.runtime.s.publishDispatchFrame(dispatch.Frame{CallID: c.grant.ID, LogicalCallID: q.LogicalCallID, TalkgroupID: q.Talkgroups[0], SourceID: q.RadioID, End: true, CapturedAt: time.Now().UTC()})
		}
		close(c.cleaned)
		ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
		e := c.sip.End(ctx)
		cancel()
		c.mu.Lock()
		c.endError = errors.Join(c.endError, e)
		c.mu.Unlock()
	}()
	if err != nil {
		return
	}
	maintained := make(chan struct{})
	go func() { defer close(maintained); c.maintain() }()
	defer func() {
		if c.sip.Reason() == sipvoice.EndRemote {
			c.mu.Lock()
			b := c.active
			c.mu.Unlock()
			if b != nil {
				_ = c.finishBurst(b, false)
			}
		}
		c.cancel()
		<-maintained
	}()
	select {
	case <-c.sip.Ready():
	case <-c.sip.Done():
		return
	case <-c.ctx.Done():
		return
	case <-c.stop:
		return
	}
	var b *voiceBurst
	var done <-chan struct{}
	for {
		select {
		case next := <-c.nextBurst:
			b = next
			c.startBurst(b)
			done = b.engine.Done()
		case in := <-c.inputs:
			if in.endSpurt || in.endCall {
				var result error
				if b != nil {
					result = c.finishBurst(b, false)
					b = nil
					done = nil
				}
				if in.result != nil {
					in.result <- result
				}
				if in.endCall {
					return
				}
				continue
			}
			f := in.frame
			if time.Since(f.CapturedAt) > 240*time.Millisecond {
				c.fail("source_age", fmt.Errorf("voice is %s old", time.Since(f.CapturedAt)))
				continue
			}
			if b == nil || b.inputEpoch != f.SourceEpoch {
				if b != nil {
					if err = c.finishBurst(b, false); err != nil {
						c.cancel()
						continue
					}
					b = nil
					done = nil
				}
				b, err = c.beginBurst(f)
				if err != nil {
					c.fail("source_ready", err)
					continue
				}
				c.startBurst(b)
				done = b.engine.Done()
			}
			if err = c.push(b, f); err != nil {
				c.fail("voice_send", err)
			}
		case <-done:
			err = c.finishBurst(b, false)
			b = nil
			done = nil
			if err != nil {
				c.cancel()
			}
		case <-c.stop:
			return
		case <-c.sip.Done():
			return
		case <-c.ctx.Done():
			return
		}
	}
}
func (c *voiceCall) maintain() {
	ticker := time.NewTicker(400 * time.Millisecond)
	defer ticker.Stop()
	for {
		select {
		case <-c.ctx.Done():
			return
		case <-c.sip.Done():
			c.cancel()
			return
		case <-ticker.C:
			if c.source {
				ctx, cancel := context.WithTimeout(c.ctx, 500*time.Millisecond)
				err := c.sip.Renew(ctx)
				cancel()
				if err != nil {
					c.fail("lease_renew", err)
				}
			}
			if c.receive {
				allowed := c.runtime.s.dispatchReceiveAllowed(c.request.Talkgroups[0])
				if c.source {
					allowed = c.runtime.s.receiveOwnerAllowed(c.peer.InstanceID, c.request.Talkgroups[0])
				}
				if !allowed {
					c.cancel()
				}
			}
		}
	}
}
func (c *voiceCall) beginBurst(f dispatch.Frame) (*voiceBurst, error) {
	if f.SourceEpoch == 0 || f.SourceEpoch <= c.lastInputEpoch {
		return nil, dispatch.ErrPermission
	}
	source := sipvoice.Source{Epoch: c.lastSource.Epoch + 1, TSN: c.lastSource.TSN + 2, MFID: f.ManufacturerID, Service: f.ServiceOptions, AlgorithmID: f.AlgorithmID, KeyID: f.KeyID}
	if c.lastSource.Epoch == 0 {
		source.TSN = 2
		if c.origin == p25rtp.GroupHome {
			source.TSN = 1
		}
	} else if source.TSN > 127 {
		source.TSN = 2 - c.lastSource.TSN%2
	}
	if !c.sourceValid(source) {
		return nil, dispatch.ErrPermission
	}
	setup, cancel := context.WithTimeout(c.ctx, 2*time.Second)
	err := c.sip.SourceReady(setup, source)
	cancel()
	if err != nil {
		return nil, err
	}
	engine, err := rtpvoice.NewEngine(c.transport, c.spurt(source))
	if err != nil {
		return nil, err
	}
	b := &voiceBurst{source: source, inputEpoch: f.SourceEpoch, engine: engine, runDone: make(chan struct{}), consumed: make(chan struct{}), finished: make(chan struct{})}
	c.mu.Lock()
	c.active = b
	c.lastSource = source
	c.lastInputEpoch = f.SourceEpoch
	c.mu.Unlock()
	return b, nil
}
func (c *voiceCall) startBurst(b *voiceBurst) {
	b.started = true
	go func() { defer close(b.runDone); b.runError = b.engine.Run(c.ctx) }()
	go func() { defer close(b.consumed); c.consume(b) }()
	if c.source {
		select {
		case <-b.engine.Ready():
		case <-b.engine.Done():
		case <-c.ctx.Done():
		}
	}
	c.readyOnce.Do(func() { close(c.ready) })
}
func (c *voiceCall) finishBurst(b *voiceBurst, abort bool) error {
	if b == nil {
		return nil
	}
	if !b.started {
		_ = b.engine.Close()
		close(b.runDone)
		close(b.consumed)
	} else if abort {
		_ = b.engine.Close()
	} else {
		ctx, cancel := context.WithTimeout(context.Background(), 350*time.Millisecond)
		_ = b.engine.End(ctx)
		cancel()
		_ = b.engine.Close()
	}
	<-b.runDone
	<-b.consumed
	var err error
	if b.reason != rtpvoice.ReasonLocalEnd && b.reason != rtpvoice.ReasonRemoteEnd && c.ctx.Err() == nil {
		err = dispatch.ErrUnavailable
	}
	if !c.source && !c.receive {
		ctx, cancel := context.WithTimeout(c.ctx, 500*time.Millisecond)
		err = errors.Join(err, c.runtime.s.broker.EndSpurt(ctx, c.peerID, c.grant))
		cancel()
	}
	c.mu.Lock()
	c.lastStats, c.lastReason = b.engine.Stats(), b.reason
	if c.active == b {
		c.active = nil
	}
	c.mu.Unlock()
	close(b.finished)
	if err != nil && c.ctx.Err() == nil {
		q := c.request
		c.runtime.s.emit(activity.Event{CallID: q.LogicalCallID, RadioID: q.RadioID, TalkgroupID: q.Talkgroups[0], Type: "media_link", Audience: "diagnostic", Severity: "warning", Outcome: "failed", Message: fmt.Sprintf("Voice burst ended: %s (%v).", b.reason, b.runError), Details: map[string]any{"allocation_id": c.grant.ID, "peer_id": c.peerID, "source_epoch": b.source.Epoch, "rtp": b.engine.Stats()}})
	}
	return err
}
func (c *voiceCall) consume(b *voiceBurst) {
	var lastPosition uint64
	known := false
	for {
		d, err := b.engine.Receive(c.ctx)
		if err != nil {
			return
		}
		if c.source {
			if d.Kind == rtpvoice.EndDelivery {
				b.reason = d.Reason
				return
			}
			continue
		}
		if d.Kind == rtpvoice.GapDelivery {
			if !c.receive {
				ctx, cancel := context.WithTimeout(c.ctx, 200*time.Millisecond)
				err = c.runtime.s.broker.Gap(ctx, c.peerID, c.grant)
				cancel()
				if err != nil {
					c.cancel()
					return
				}
			}
			continue
		}
		if d.Kind == rtpvoice.EndDelivery {
			b.reason = d.Reason
			return
		}
		if !known {
			if c.rxSequence == 0 {
				c.rxSequence = d.Position + 1
			} else {
				c.rxSequence += 2
			}
			known = true
		} else {
			c.rxSequence += d.Position - lastPosition
		}
		lastPosition = d.Position
		q := c.request
		v := d.Frame.Voice
		index, _ := v.Type.Index()
		f := dispatch.Frame{CallID: c.grant.ID, LogicalCallID: q.LogicalCallID, TalkgroupID: q.Talkgroups[0], SourceID: q.RadioID, SourceKind: q.SourceKind, SourceEpoch: b.source.Epoch, Sequence: c.rxSequence, Encoded: slices.Clone(v.IMBE[:]), P25: &dispatch.P25VoiceMetadata{Type: v.Type, Quality: v.Quality, LSD: v.LSD, ES: v.ES, Header: d.Frame.Header}, ManufacturerID: b.source.MFID, AlgorithmID: b.source.AlgorithmID, KeyID: b.source.KeyID, VoiceIndex: index, CapturedAt: time.Now().UTC(), Emergency: b.source.Service&0x80 != 0, ServiceOptions: b.source.Service, Encrypted: q.Secure}
		if !q.Secure {
			f.AlgorithmID = 0x80
			f.KeyID = 0
		}
		if !f.MediaValid() {
			c.fail("media_identity", dispatch.ErrPermission)
			return
		}
		for _, r := range c.runtime.s.view().System.Radios {
			if r.RID == f.SourceID {
				f.SourceAlias = r.Alias
				break
			}
		}
		if !c.receive && c.runtime.s.Config().Role == RoleSite && q.SourceKind != "rf" && f.P25.Header != nil {
			if err := c.runtime.s.stampGeneratedVoiceHeader(&f); err != nil {
				c.fail("generated_header", err)
				return
			}
		}
		if c.receive {
			c.runtime.s.publishDispatchFrame(f)
		} else {
			ctx, cancel := context.WithTimeout(c.ctx, 200*time.Millisecond)
			err = c.runtime.s.broker.Write(ctx, c.peerID, c.grant, f)
			cancel()
			if err != nil {
				c.fail("route_write", err)
				return
			}
		}
	}
}
func (c *voiceCall) Write(f dispatch.Frame) error {
	if !c.source {
		return dispatch.ErrPermission
	}
	if !f.MediaValid() || f.End || len(f.Encoded) != 11 || f.P25 == nil || f.SourceEpoch == 0 {
		return dispatch.ErrP25MetadataNotSupported
	}
	if f.SourceID != c.request.RadioID || f.Encrypted != c.request.Secure || (f.ServiceOptions&0x80 != 0) != c.request.Emergency || (f.Encrypted && !c.request.EncryptionPending && (int(f.AlgorithmID) != c.request.AlgorithmID || int(f.KeyID) != c.request.KeyID)) {
		return dispatch.ErrPermission
	}
	c.inputMu.Lock()
	defer c.inputMu.Unlock()
	if c.ending || c.ctx.Err() != nil {
		return dispatch.ErrUnavailable
	}
	f.Encoded = slices.Clone(f.Encoded)
	f.P25 = f.P25.Clone()
	f.MessageIndicator = nil
	select {
	case c.inputs <- voiceInput{frame: f}:
		return nil
	default:
		c.cancel()
		return rtpvoice.ErrQueueFull
	}
}
func (c *voiceCall) push(b *voiceBurst, f dispatch.Frame) error {
	if age := time.Since(f.CapturedAt); age > 240*time.Millisecond {
		return fmt.Errorf("source voice is %s old (limit240ms)", age.Round(time.Millisecond))
	}
	if f.ManufacturerID != b.source.MFID || f.ServiceOptions != b.source.Service || f.AlgorithmID != b.source.AlgorithmID || f.KeyID != b.source.KeyID {
		return dispatch.ErrPermission
	}
	index, ok := f.P25.Type.Index()
	if !ok {
		return rtpvoice.ErrFramePosition
	}
	slot := uint64(f.P25.Quality.Superframe)*18 + uint64(index)
	// Engine checks increasing positions. Its timestamp advances by actual voice
	// ordinal, never by native/control/RTP packet sequence numbers.
	position := slot
	if b.positionKnown {
		delta := (slot + 72 - b.position%72) % 72
		if delta == 0 || time.Since(b.lastAt) >= 72*20*time.Millisecond {
			return rtpvoice.ErrFramePosition
		}
		position = b.position + delta
	}
	v := p25rtp.FullRateVoice{Type: f.P25.Type, Quality: f.P25.Quality, LSD: f.P25.LSD, ES: f.P25.ES}
	copy(v.IMBE[:], f.Encoded)
	header := f.P25.Header
	if header != nil {
		h := *header
		h.GroupID = uint16(c.request.Talkgroups[0])
		header = &h
	}
	if err := b.engine.Push(rtpvoice.Frame{Position: position, Voice: v, Header: header}); err != nil {
		return err
	}
	b.position, b.positionKnown, b.lastAt = position, true, time.Now()
	return nil
}
func (c *voiceCall) Renew() error {
	select {
	case <-c.done:
		return dispatch.ErrUnavailable
	case <-c.ctx.Done():
		return dispatch.ErrUnavailable
	default:
		return nil
	}
}
func (c *voiceCall) control(ctx context.Context, endCall bool) error {
	c.inputMu.Lock()
	if c.ending {
		c.inputMu.Unlock()
		if endCall {
			select {
			case <-c.done:
				c.mu.Lock()
				err := c.endError
				c.mu.Unlock()
				return err
			case <-ctx.Done():
				return ctx.Err()
			}
		}
		return dispatch.ErrUnavailable
	}
	if endCall {
		c.ending = true
	}
	result := make(chan error, 1)
	select {
	case c.inputs <- voiceInput{endSpurt: !endCall, endCall: endCall, result: result}:
	case <-ctx.Done():
		c.cancel()
		c.inputMu.Unlock()
		return ctx.Err()
	case <-c.ctx.Done():
		c.inputMu.Unlock()
		return dispatch.ErrUnavailable
	}
	c.inputMu.Unlock()
	if endCall {
		select {
		case <-c.done:
			c.mu.Lock()
			err := c.endError
			c.mu.Unlock()
			return err
		case <-ctx.Done():
			c.cancel()
			return ctx.Err()
		}
	}
	select {
	case err := <-result:
		return err
	case <-ctx.Done():
		c.cancel()
		return ctx.Err()
	case <-c.ctx.Done():
		return dispatch.ErrUnavailable
	}
}
func (c *voiceCall) EndSpurt(ctx context.Context) error {
	if !c.source {
		return dispatch.ErrPermission
	}
	return c.control(ctx, false)
}
func (c *voiceCall) End(ctx context.Context) error { return c.control(ctx, true) }

func (c *voiceCall) fail(stage string, err error) {
	c.failureOnce.Do(func() {
		q := c.request
		c.runtime.s.emit(activity.Event{CallID: q.LogicalCallID, RadioID: q.RadioID, TalkgroupID: q.Talkgroups[0], Type: "media_link", Audience: "diagnostic", Severity: "warning", Outcome: "failed", Message: fmt.Sprintf("Voice allocation failed during %s: %v", stage, err), Details: map[string]any{"allocation_id": c.grant.ID, "peer_id": c.peerID, "stage": stage}})
	})
	c.cancel()
}

func (c *voiceCall) queueSpurtEnd() error {
	c.inputMu.Lock()
	defer c.inputMu.Unlock()
	if c.ending || c.ctx.Err() != nil {
		return dispatch.ErrUnavailable
	}
	select {
	case c.inputs <- voiceInput{endSpurt: true}:
		return nil
	default:
		c.cancel()
		return rtpvoice.ErrQueueFull
	}
}
