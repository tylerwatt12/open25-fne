package rtpvoice

import (
	"context"
	"crypto/rand"
	"encoding/binary"
	"errors"
	"io"
	"sync"
	"sync/atomic"
	"time"
	"open25.local/open25-fne/internal/p25rtp"
)

type queuedFrame struct {
	Frame
	at time.Time
}
type packetArrival struct {
	packet p25rtp.Packet
	at     time.Time
}
type queuedDelivery struct {
	Delivery
	at time.Time
}
type EngineStats struct {
	SentFrames, ReceivedFrames, IPGaps, PacketHoles, LatePackets        uint64
	WrongSpurt, WrongSource, WrongOptions, InvalidFrames, DroppedFrames uint64
}
type engineCounts struct{ sent, received, gaps, holes, late, spurt, source, options, invalid, dropped atomic.Uint64 }

// Engine borrows the call's Session for one immutable admitted spurt and owns
// its media queues and joined UDP reader. The caller closes Session when the
// call ends; successive Engines may borrow it only after the prior Done.
type Engine struct {
	session             *Session
	spurt               Spurt
	ctx                 context.Context
	cancel              context.CancelFunc
	lifeMu              sync.Mutex
	started, closed     bool
	result              error
	done                chan struct{}
	ready               chan struct{}
	readyOnce           sync.Once
	txMu                sync.Mutex
	tx                  []queuedFrame
	txLast              uint64
	txHasLast, txEnding bool
	end                 chan struct{}
	endOnce             sync.Once
	wake                chan struct{}
	failure             chan error
	input               chan packetArrival
	inputError          chan error
	rxTokens            chan struct{}
	output              chan Delivery
	counts              engineCounts
	// The fields below are exclusively owned by Run's actor.
	state            engineState
	runAt            time.Time
	timestamp        uint32
	txBase           uint64
	txAnchored       bool
	txNext           time.Time
	txAnchor         time.Time
	pending          []packetArrival
	nextSequence     uint16
	sequenceKnown    bool
	holeAt           time.Time
	voices           []queuedDelivery
	voiceKnown       bool
	lastTimestamp    uint32
	lastPosition     uint64
	expectedPosition uint64
	rxTimestamp      uint32
	rxPlay           time.Time
	pendingHeader    *p25rtp.HeaderWord
	endingAt         time.Time
	endSent          int
	endDue           time.Time
}

func NewEngine(session *Session, spurt Spurt) (*Engine, error) {
	if session == nil {
		return nil, ErrSpurt
	}
	if err := spurt.validate(session.options.LocalRole); err != nil {
		return nil, err
	}
	select {
	case <-session.done:
		return nil, ErrClosed
	default:
	}
	var seed [4]byte
	if _, err := rand.Read(seed[:]); err != nil {
		return nil, err
	}
	ctx, cancel := context.WithCancel(context.Background())
	return &Engine{session: session, spurt: spurt.copy(), ctx: ctx, cancel: cancel, done: make(chan struct{}), ready: make(chan struct{}), end: make(chan struct{}), wake: make(chan struct{}, 1), failure: make(chan error, 1), input: make(chan packetArrival, rxCapacity), inputError: make(chan error, 1), rxTokens: make(chan struct{}, rxCapacity), output: make(chan Delivery, 1), timestamp: binary.BigEndian.Uint32(seed[:]), tx: make([]queuedFrame, 0, txCapacity), pending: make([]packetArrival, 0, rxCapacity), voices: make([]queuedDelivery, 0, rxCapacity)}, nil
}
func (e *Engine) Ready() <-chan struct{} { return e.ready }
func (e *Engine) Done() <-chan struct{}  { return e.done }
func (e *Engine) signal() {
	select {
	case e.wake <- struct{}{}:
	default:
	}
}
func (e *Engine) fail(err error) {
	select {
	case e.failure <- err:
	default:
	}
	e.signal()
}

// Push is nonblocking. A failed source position or full queue fails this owner;
// the actor discards queued voice and sends its originating End sequence.
func (e *Engine) Push(frame Frame) error {
	if e.spurt.Origin != e.session.options.LocalRole {
		return ErrDirection
	}
	if err := frame.validate(e.spurt.GroupID); err != nil {
		e.fail(err)
		return err
	}
	e.txMu.Lock()
	defer e.txMu.Unlock()
	if e.txEnding || e.ctx.Err() != nil {
		return ErrClosed
	}
	if e.txHasLast && (frame.Position <= e.txLast || frame.Position-e.txLast >= 72) {
		e.fail(ErrFramePosition)
		return ErrFramePosition
	}
	if len(e.tx) >= txCapacity {
		e.fail(ErrQueueFull)
		return ErrQueueFull
	}
	e.tx = append(e.tx, queuedFrame{Frame: frame.copy(), at: time.Now()})
	e.txLast = frame.Position
	e.txHasLast = true
	e.signal()
	return nil
}
func (e *Engine) Receive(ctx context.Context) (Delivery, error) {
	select {
	case delivery, ok := <-e.output:
		if !ok {
			return Delivery{}, io.EOF
		}
		if delivery.token {
			e.releaseRX()
		}
		e.signal()
		return delivery, nil
	case <-ctx.Done():
		return Delivery{}, ctx.Err()
	}
}
func (e *Engine) End(ctx context.Context) error {
	e.lifeMu.Lock()
	started := e.started
	e.lifeMu.Unlock()
	if !started {
		return e.Close()
	}
	e.txMu.Lock()
	e.txEnding = true
	e.txMu.Unlock()
	e.endOnce.Do(func() { close(e.end) })
	e.signal()
	select {
	case <-e.done:
		e.lifeMu.Lock()
		err := e.result
		e.lifeMu.Unlock()
		return err
	case <-ctx.Done():
		return ctx.Err()
	}
}
func (e *Engine) Close() error {
	e.lifeMu.Lock()
	if !e.started && !e.closed {
		e.closed = true
		e.cancel()
		close(e.output)
		close(e.done)
	}
	started := e.started
	e.lifeMu.Unlock()
	if started {
		e.cancel()
		<-e.done
	}
	return nil
}
func (e *Engine) Stats() EngineStats {
	c := &e.counts
	return EngineStats{SentFrames: c.sent.Load(), ReceivedFrames: c.received.Load(), IPGaps: c.gaps.Load(), PacketHoles: c.holes.Load(), LatePackets: c.late.Load(), WrongSpurt: c.spurt.Load(), WrongSource: c.source.Load(), WrongOptions: c.options.Load(), InvalidFrames: c.invalid.Load(), DroppedFrames: c.dropped.Load()}
}
func (e *Engine) releaseRX() { <-e.rxTokens }
func (e *Engine) read(ctx context.Context) {
	for {
		// One shared token follows a packet through all receive stages until its
		// actual frame is consumed. Queues do not multiply the16-packet bound.
		select {
		case e.rxTokens <- struct{}{}:
		case <-ctx.Done():
			return
		}
		packet, err := e.session.Receive(ctx)
		if err != nil {
			e.releaseRX()
			select {
			case e.inputError <- err:
			default:
			}
			return
		}
		arrival := packetArrival{packet: packet, at: time.Now()}
		select {
		case e.input <- arrival:
		case <-ctx.Done():
			e.releaseRX()
			return
		}
	}
}

func (e *Engine) Run(parent context.Context) (result error) {
	e.lifeMu.Lock()
	if e.started || e.closed {
		e.lifeMu.Unlock()
		return ErrEngineStarted
	}
	e.started = true
	e.lifeMu.Unlock()
	ctx, cancel := context.WithCancel(parent)
	cancelDone := make(chan struct{})
	stop := context.AfterFunc(e.ctx, func() { cancel(); close(cancelDone) })
	var readers sync.WaitGroup
	orderedEnd := false
	defer func() {
		if !stop() {
			<-cancelDone
		}
		cancel()
		e.cancel()
		readers.Wait()
		e.dropRX(!orderedEnd)
		e.txMu.Lock()
		e.counts.dropped.Add(uint64(len(e.tx)))
		clear(e.tx)
		e.tx = e.tx[:0]
		e.txEnding = true
		e.txMu.Unlock()
		if !orderedEnd {
			select {
			case e.output <- Delivery{Kind: EndDelivery, Reason: e.state.reason}:
			default:
			}
		}
		close(e.output)
		e.lifeMu.Lock()
		e.result = result
		e.closed = true
		close(e.done)
		e.lifeMu.Unlock()
	}()
	e.runAt = time.Now()
	var initial []p25rtp.PacketType
	e.state, initial = newEngineState(e.session.options.LocalRole, e.spurt.Origin, e.runAt)
	readers.Add(1)
	go func() { defer readers.Done(); e.read(ctx) }()
	if err := e.sendControls(ctx, initial, e.runAt); err != nil {
		if ctx.Err() != nil || e.ctx.Err() != nil {
			e.state.reason = ReasonCanceled
			return context.Canceled
		}
		e.state.reason = ReasonTransport
		return err
	}
	if e.state.ready {
		e.readyOnce.Do(func() { close(e.ready) })
	}
	ticker := time.NewTicker(framePeriod)
	defer ticker.Stop()
	mediaTimer := time.NewTimer(framePeriod)
	mediaTimer.Stop()
	defer mediaTimer.Stop()
	for {
		// Admit already-arrived packets before evaluating a coincident loss timer.
		for {
			select {
			case packet := <-e.input:
				e.insert(packet)
			default:
				goto drained
			}
		}
	drained:
		now := time.Now()
		if err := e.drainPending(ctx, now); err != nil {
			if !errors.Is(err, ErrFramePosition) && !errors.Is(err, ErrSpurt) {
				e.state.reason = ReasonTransport
				return err
			}
			e.beginEnding(ReasonSource, now, true)
		}
		select {
		case <-e.end:
			e.beginEnding(ReasonLocalEnd, now, false)
		default:
		}
		select {
		case failure := <-e.failure:
			reason := ReasonSource
			if errors.Is(failure, ErrQueueFull) {
				reason = ReasonQueue
			}
			e.beginEnding(reason, now, true)
		default:
		}
		if err := e.sendControls(ctx, e.state.tick(now), now); err != nil {
			e.state.reason = ReasonTransport
			return err
		}
		if e.state.ready {
			e.readyOnce.Do(func() { close(e.ready) })
		}
		if e.state.phase == phaseEnding && e.endingAt.IsZero() {
			e.beginEnding(e.state.reason, now, false)
		}
		complete, err := e.drive(ctx, now)
		if err != nil {
			if ctx.Err() != nil || e.ctx.Err() != nil {
				e.state.reason = ReasonCanceled
				return context.Canceled
			}
			e.state.reason = ReasonTransport
			if errors.Is(err, ErrQueueFull) {
				e.state.reason = ReasonQueue
			}
			return err
		}
		if complete {
			orderedEnd = true
			return reasonError(e.state.reason)
		}
		// The actor and media clocks need not share a phase. Wake at the next
		// actual media deadline; a full output waits for the consumer wake.
		mediaTimer.Stop()
		var media <-chan time.Time
		if due, ok := e.mediaDeadline(); ok {
			mediaTimer.Reset(time.Until(due))
			media = mediaTimer.C
		}
		select {
		case <-ctx.Done():
			e.state.reason = ReasonCanceled
			return ctx.Err()
		case <-e.ctx.Done():
			e.state.reason = ReasonCanceled
			return context.Canceled
		case err := <-e.inputError:
			if ctx.Err() != nil || e.ctx.Err() != nil {
				e.state.reason = ReasonCanceled
				return context.Canceled
			}
			e.state.reason = ReasonTransport
			return err
		case packet := <-e.input:
			e.insert(packet)
		case <-e.wake:
		case <-ticker.C:
		case <-media:
		}
	}
}

func (e *Engine) mediaDeadline() (time.Time, bool) {
	if e.state.phase != phaseActive && e.state.phase != phaseEnding {
		return time.Time{}, false
	}
	if !e.state.localSource {
		if e.voiceKnown && len(e.output) == 0 && (len(e.voices) > 0 || e.state.phase != phaseEnding) {
			return e.rxPlay, true
		}
		return time.Time{}, false
	}
	e.txMu.Lock()
	defer e.txMu.Unlock()
	if len(e.tx) == 0 {
		return time.Time{}, false
	}
	return e.frameDeadline(e.tx[0].Position), true
}

func (e *Engine) frameDeadline(position uint64) time.Time {
	due := e.txNext
	if e.txAnchored {
		positionDue := e.txAnchor.Add(time.Duration(position-e.txBase) * framePeriod)
		if positionDue.After(due) {
			due = positionDue
		}
	}
	return due
}
func reasonError(reason EngineReason) error {
	switch reason {
	case ReasonDenied:
		return ErrSpurtDenied
	case ReasonFirstMedia, ReasonMediaLoss, ReasonRequestTimeout, ReasonHeartbeat:
		return ErrSpurtTimeout
	case ReasonQueue:
		return ErrQueueFull
	case ReasonSource:
		return ErrFramePosition
	default:
		return nil
	}
}
func (e *Engine) beginEnding(reason EngineReason, now time.Time, drop bool) {
	e.state.ending(reason)
	if e.endingAt.IsZero() {
		e.endingAt = now
	}
	e.txMu.Lock()
	e.txEnding = true
	if drop {
		e.counts.dropped.Add(uint64(len(e.tx)))
		clear(e.tx)
		e.tx = e.tx[:0]
	}
	e.txMu.Unlock()
}
func (e *Engine) sendControls(ctx context.Context, kinds []p25rtp.PacketType, now time.Time) error {
	for _, kind := range kinds {
		packet := p25rtp.Packet{Role: e.session.options.LocalRole, Control: p25rtp.PacketTypeControl{Type: kind, TSN: e.spurt.TSN, ServiceOptions: e.spurt.ServiceOptions}}
		if kind == p25rtp.Heartbeat || kind == p25rtp.HeartbeatQuery {
			packet.Control.TSN = 0
			packet.Control.Interval = uint8(e.state.heartbeatInterval / time.Second)
		}
		if kind == p25rtp.Request || kind == p25rtp.Start {
			packet.RF = e.spurt.RF
			packet.Console = e.spurt.Console
		}
		if kind == p25rtp.Request {
			packet.Timestamp = e.timestamp + uint32(now.Sub(e.runAt)/(125*time.Microsecond))
		}
		if err := e.session.Send(ctx, packet); err != nil {
			return err
		}
	}
	return nil
}
func (e *Engine) insert(arrival packetArrival) {
	sequence := arrival.packet.Sequence
	if !e.sequenceKnown {
		e.sequenceKnown = true
		e.nextSequence = sequence
	}
	if int16(sequence-e.nextSequence) < 0 {
		e.counts.late.Add(1)
		e.releaseRX()
		return
	}
	for _, old := range e.pending {
		if old.packet.Sequence == sequence {
			e.counts.late.Add(1)
			e.releaseRX()
			return
		}
	}
	e.pending = append(e.pending, arrival)
	// At most16 entries; compare wrapped offsets from the current stream cursor.
	for i := len(e.pending) - 1; i > 0 && uint16(e.pending[i].packet.Sequence-e.nextSequence) < uint16(e.pending[i-1].packet.Sequence-e.nextSequence); i-- {
		e.pending[i], e.pending[i-1] = e.pending[i-1], e.pending[i]
	}
}
func (e *Engine) drainPending(ctx context.Context, now time.Time) error {
	for len(e.pending) > 0 {
		head := e.pending[0]
		if head.packet.Sequence != e.nextSequence {
			if e.holeAt.IsZero() {
				e.holeAt = head.at
			}
			// Never let waiting for a missing control packet manufacture media loss.
			lossDue := !e.state.mediaUntil.IsZero() && !now.Before(e.state.mediaUntil)
			if now.Before(e.holeAt.Add(reorderDelay)) && !lossDue {
				return nil
			}
			e.counts.holes.Add(uint64(uint16(head.packet.Sequence - e.nextSequence)))
		}
		copy(e.pending, e.pending[1:])
		e.pending[len(e.pending)-1] = packetArrival{}
		e.pending = e.pending[:len(e.pending)-1]
		e.nextSequence = head.packet.Sequence + 1
		e.holeAt = time.Time{}
		held, err := e.packet(ctx, head)
		if !held {
			e.releaseRX()
		}
		if err != nil {
			return err
		}
	}
	return nil
}
func (e *Engine) packet(ctx context.Context, arrival packetArrival) (bool, error) {
	p, now := arrival.packet, arrival.at
	if p.Role != e.session.options.RemoteRole {
		e.counts.spurt.Add(1)
		return false, nil
	}
	if p.Control.Type == p25rtp.Heartbeat || p.Control.Type == p25rtp.HeartbeatQuery {
		if p.Control.TSN != 0 || p.Control.Mute || p.Control.Losing {
			e.counts.spurt.Add(1)
			return false, nil
		}
		return false, e.sendControls(ctx, e.state.heartbeat(p.Control.Type, p.Control.Interval, now), now)
	}
	if p.Control.TSN != e.spurt.TSN {
		e.counts.spurt.Add(1)
		return false, nil
	}
	if p.Control.ServiceOptions != e.spurt.ServiceOptions || p.Control.Mute || p.Control.Losing {
		e.counts.options.Add(1)
		return false, nil
	}
	sourceType := p.Control.Type == p25rtp.Request || p.Control.Type == p25rtp.Start || p.Control.Type == p25rtp.Progress
	if sourceType {
		if e.state.localSource || (p.RF == nil) != (e.spurt.RF == nil) || (p.Console == nil) != (e.spurt.Console == nil) || (p.RF != nil && *p.RF != *e.spurt.RF) || (p.Console != nil && *p.Console != *e.spurt.Console) {
			e.counts.source.Add(1)
			return false, nil
		}
	}
	switch p.Control.Type {
	case p25rtp.Request:
		if e.spurt.Origin != p25rtp.GroupServing || p.Voice != nil || p.Header != nil {
			e.counts.spurt.Add(1)
			return false, nil
		}
	case p25rtp.Start:
		if e.spurt.Origin != p25rtp.GroupHome {
			e.counts.spurt.Add(1)
			return false, nil
		}
	case p25rtp.Grant, p25rtp.Deny, p25rtp.Wait:
		if !e.state.localSource || e.spurt.Origin != p25rtp.GroupServing {
			e.counts.spurt.Add(1)
			return false, nil
		}
	case p25rtp.End:
		if e.state.localSource {
			e.counts.spurt.Add(1)
			return false, nil
		}
	case p25rtp.Progress:
		if e.state.localSource || e.state.phase != phaseActive {
			e.counts.spurt.Add(1)
			return false, nil
		}
		if p.Header != nil {
			if p.Header.GroupID != e.spurt.GroupID {
				e.counts.source.Add(1)
				return false, nil
			}
			copy := *p.Header
			e.pendingHeader = &copy
		}
		if p.Voice == nil {
			// Private actual-voice deadline: preserve the supplied Header but
			// header-only traffic cannot hold an admitted RF/media owner alive.
			return false, nil
		}
		slot, err := voiceSlot(*p.Voice)
		if err != nil {
			e.counts.invalid.Add(1)
			return false, err
		}
		position := slot
		if e.voiceKnown {
			delta := int32(p.Timestamp - e.lastTimestamp)
			if delta <= 0 {
				e.counts.late.Add(1)
				return false, nil
			}
			if delta%p25rtp.FrameTicks != 0 || delta >= 72*p25rtp.FrameTicks {
				e.counts.invalid.Add(1)
				return false, ErrFramePosition
			}
			position = e.lastPosition + uint64(delta/p25rtp.FrameTicks)
			if position%72 != slot {
				e.counts.invalid.Add(1)
				return false, ErrFramePosition
			}
		} else {
			e.expectedPosition = position
			e.rxTimestamp = p.Timestamp
			e.rxPlay = now.Add(reorderDelay)
			e.voiceKnown = true
		}
		if position < e.expectedPosition {
			e.counts.late.Add(1)
			return false, nil
		}
		frame := Frame{Position: position, Voice: *p.Voice, Header: e.pendingHeader}.copy()
		e.pendingHeader = nil
		if err = frame.validate(e.spurt.GroupID); err != nil {
			e.counts.invalid.Add(1)
			return false, err
		}
		e.lastPosition, e.lastTimestamp = position, p.Timestamp
		e.voices = append(e.voices, queuedDelivery{Delivery: Delivery{Kind: FrameDelivery, Position: position, Timestamp: p.Timestamp, Frame: frame, token: true}, at: now})
		e.state.progress(now)
		e.counts.received.Add(1)
		return true, nil
	default:
		e.counts.spurt.Add(1)
		return false, nil
	}
	return false, e.sendControls(ctx, e.state.control(p.Control.Type, now), now)
}
func (e *Engine) drive(ctx context.Context, now time.Time) (bool, error) {
	active := e.state.phase == phaseActive || e.state.phase == phaseEnding
	if active && e.state.localSource && !now.Before(e.txNext) {
		e.txMu.Lock()
		var frame queuedFrame
		has := len(e.tx) > 0
		if has {
			frame = e.tx[0]
		}
		e.txMu.Unlock()
		var due time.Time
		if has {
			due = e.frameDeadline(frame.Position)
			if now.Before(due) {
				has = false
			}
		}
		if has {
			if now.Sub(frame.at) > maximumQueueAge {
				e.beginEnding(ReasonQueue, now, true)
			} else {
				if !e.txAnchored {
					e.txAnchored = true
					e.txBase = frame.Position
					e.txAnchor = now
				}
				packet := p25rtp.Packet{Role: e.session.options.LocalRole, Timestamp: e.timestamp + uint32(frame.Position-e.txBase)*p25rtp.FrameTicks, Control: p25rtp.PacketTypeControl{Type: p25rtp.Progress, TSN: e.spurt.TSN, ServiceOptions: e.spurt.ServiceOptions}, RF: e.spurt.RF, Console: e.spurt.Console, Header: frame.Header, Voice: &frame.Voice}
				if err := e.session.Send(ctx, packet); err != nil {
					return false, err
				}
				e.txMu.Lock()
				copy(e.tx, e.tx[1:])
				e.tx[len(e.tx)-1] = queuedFrame{}
				e.tx = e.tx[:len(e.tx)-1]
				e.txMu.Unlock()
				e.txNext = due.Add(framePeriod)
				if e.state.phase == phaseActive {
					e.state.progress(now)
				}
				e.counts.sent.Add(1)
			}
		}
	}
	if active && !e.state.localSource && e.voiceKnown && !now.Before(e.rxPlay) {
		ending := e.state.phase == phaseEnding
		if len(e.voices) > 0 || !ending {
			delivery := Delivery{Kind: GapDelivery, Position: e.expectedPosition, Timestamp: e.rxTimestamp}
			actual := len(e.voices) > 0 && e.voices[0].Position == e.expectedPosition
			if actual {
				if now.Sub(e.voices[0].at) > maximumQueueAge {
					return false, ErrQueueFull
				}
				delivery = e.voices[0].Delivery
			}
			select {
			case e.output <- delivery:
				if actual {
					copy(e.voices, e.voices[1:])
					e.voices[len(e.voices)-1] = queuedDelivery{}
					e.voices = e.voices[:len(e.voices)-1]
				} else {
					e.counts.gaps.Add(1)
				}
				e.expectedPosition++
				e.rxTimestamp += p25rtp.FrameTicks
				e.rxPlay = e.rxPlay.Add(framePeriod)
			default:
			}
		}
	}
	if e.state.phase != phaseEnding {
		return false, nil
	}
	// End-trigger queue draining is bounded even if the consumer stops reading.
	if now.Sub(e.endingAt) > maximumQueueAge+endCopies*endRetry {
		return false, ErrQueueFull
	}
	e.txMu.Lock()
	emptyTX := len(e.tx) == 0
	e.txMu.Unlock()
	if e.state.localSource {
		if !emptyTX {
			return false, nil
		}
		if e.endSent < endCopies && !now.Before(e.endDue) {
			if err := e.sendControls(ctx, []p25rtp.PacketType{p25rtp.End}, now); err != nil {
				return false, err
			}
			e.endSent++
			e.endDue = now.Add(endRetry)
		}
		if e.endSent < endCopies {
			return false, nil
		}
	} else if len(e.voices) > 0 {
		return false, nil
	}
	select {
	case e.output <- Delivery{Kind: EndDelivery, Reason: e.state.reason}:
		return true, nil
	default:
		return false, nil
	}
}
func (e *Engine) dropRX(abort bool) {
	for range e.pending {
		e.releaseRX()
	}
	clear(e.pending)
	e.pending = nil
	for range e.voices {
		e.releaseRX()
		e.counts.dropped.Add(1)
	}
	clear(e.voices)
	e.voices = nil
	for {
		select {
		case <-e.input:
			e.releaseRX()
		default:
			goto drained
		}
	}
drained:
	if abort {
		for {
			select {
			case item := <-e.output:
				if item.token {
					e.releaseRX()
					e.counts.dropped.Add(1)
				}
			default:
				return
			}
		}
	}
}
