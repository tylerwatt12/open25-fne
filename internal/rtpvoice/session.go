package rtpvoice

import (
	"context"
	"crypto/rand"
	"encoding/binary"
	"errors"
	"fmt"
	"net/netip"
	"sync"
	"sync/atomic"
	"time"

	"github.com/pion/transport/v5/replaydetector"
	"open25.local/open25-fne/internal/p25rtp"
)

const (
	maxDatagram = 12 + 1 + 4*4 + 4 + 24 + 18 + 18 // Console + HeaderWord + ES voice.
	writeBudget = 20 * time.Millisecond           // Private one-frame UDP write budget.
)

var ErrClosed = errors.New("rtpvoice: session closed")

// Options binds one immutable, negotiated peer. No endpoint learning or rekeying
// occurs here. A changed endpoint, role requires a new Pair and Session.
type Options struct {
	Remote                netip.AddrPort
	LocalRole, RemoteRole p25rtp.Role
	WriteTimeout          time.Duration // Zero defaults to 20ms; positive values may shorten it.
}

type Counters struct {
	Sent, Received, WrongEndpoint, InvalidHeader, Oversized   uint64
	StaleOrDuplicate, InvalidPayload, ReadErrors, WriteErrors uint64
}

type counts struct{ sent, received, endpoint, header, oversized, duplicate, payload, read, write atomic.Uint64 }

// Session owns its Pair. The caller owns reads and processing through Receive;
// there are no background readers, packet queues or user callback goroutines.
type Session struct {
	pair        *Pair
	options     Options
	rx          replaydetector.ReplayDetector // Owned by the serialized reader.
	next        uint16
	sendGate    chan struct{}
	receiveGate chan struct{}
	done        chan struct{}
	closeOnce   sync.Once
	counts      counts
}

// New takes ownership of pair only on success. Endpoints are copied.
func New(pair *Pair, options Options) (*Session, error) {
	if pair == nil || pair.rtp == nil || pair.rtcp == nil {
		return nil, fmt.Errorf("rtpvoice: missing port pair")
	}
	if !options.Remote.IsValid() || !options.Remote.Addr().Is4() || options.Remote.Addr().IsUnspecified() || options.Remote.Addr().IsMulticast() || options.Remote.Addr() == netip.AddrFrom4([4]byte{255, 255, 255, 255}) || options.Remote.Port() == 0 || options.Remote.Port() > 65532 || options.Remote.Port()%2 != 0 {
		return nil, fmt.Errorf("rtpvoice: remote must be a fixed IPv4 unicast even RTP endpoint")
	}
	if !((options.LocalRole == p25rtp.GroupHome && options.RemoteRole == p25rtp.GroupServing) || (options.LocalRole == p25rtp.GroupServing && options.RemoteRole == p25rtp.GroupHome)) {
		return nil, fmt.Errorf("rtpvoice: complementary group roles required")
	}
	if options.WriteTimeout == 0 {
		options.WriteTimeout = writeBudget
	}
	if options.WriteTimeout < 0 || options.WriteTimeout > writeBudget {
		return nil, fmt.Errorf("rtpvoice: write timeout must be positive and at most 20ms")
	}
	var seed [2]byte
	if _, err := rand.Read(seed[:]); err != nil {
		return nil, err
	}
	pair.mu.Lock()
	defer pair.mu.Unlock()
	if pair.closed || pair.used {
		return nil, fmt.Errorf("rtpvoice: port pair is closed or already owned")
	}
	pair.used = true
	s := &Session{pair: pair, options: options, rx: replaydetector.WithWrap(128, 65535), next: binary.BigEndian.Uint16(seed[:]), sendGate: make(chan struct{}, 1), receiveGate: make(chan struct{}, 1), done: make(chan struct{})}
	return s, nil
}

// Send supplies the session's own sequence number; p.Sequence is ignored.
// Timestamp and PTT/spurt policy remain caller-owned. A valid send attempt
// consumes a sequence number even if the subsequent UDP write fails.
func (s *Session) Send(ctx context.Context, p p25rtp.Packet) error {
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-s.done:
		return ErrClosed
	case s.sendGate <- struct{}{}:
	}
	defer func() { <-s.sendGate }()
	select {
	case <-s.done:
		return ErrClosed
	default:
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	if p.Role != s.options.LocalRole {
		return fmt.Errorf("rtpvoice: wrong local source role")
	}
	p.Sequence = s.next
	plain, err := p25rtp.Marshal(p)
	if err != nil {
		return err
	}
	s.next++
	deadline := time.Now().Add(s.options.WriteTimeout)
	if d, ok := ctx.Deadline(); ok && d.Before(deadline) {
		deadline = d
	}
	if err = s.pair.rtp.SetWriteDeadline(deadline); err != nil {
		return err
	}
	n, err := s.pair.rtp.WriteToUDPAddrPort(plain, s.options.Remote)
	if err != nil || n != len(plain) {
		s.counts.write.Add(1)
		if ctx.Err() != nil {
			return ctx.Err()
		}
		select {
		case <-s.done:
			return ErrClosed
		default:
		}
		if err == nil {
			err = fmt.Errorf("rtpvoice: incomplete UDP datagram")
		}
		return err
	}
	s.counts.sent.Add(1)
	return nil
}

// Receive returns endpoint-bound, sequence-checked, profile-valid packets.
// Plain RTP has no cryptographic authentication; the deployment uses a VPN.
// Session/lease/spurt checks, buffering and pacing belong to the caller. Calls
// serialize reads, but cancellation also interrupts waiting for the read owner.
func (s *Session) Receive(ctx context.Context) (p25rtp.Packet, error) {
	if err := ctx.Err(); err != nil {
		return p25rtp.Packet{}, err
	}
	select {
	case <-ctx.Done():
		return p25rtp.Packet{}, ctx.Err()
	case <-s.done:
		return p25rtp.Packet{}, ErrClosed
	case s.receiveGate <- struct{}{}:
	}
	defer func() { <-s.receiveGate }()
	select {
	case <-s.done:
		return p25rtp.Packet{}, ErrClosed
	default:
	}
	if err := ctx.Err(); err != nil {
		return p25rtp.Packet{}, err
	}
	deadline, _ := ctx.Deadline()
	if err := s.pair.rtp.SetReadDeadline(deadline); err != nil {
		return p25rtp.Packet{}, ErrClosed
	}
	// Wait for any running cancellation callback before another reader can reset
	// the deadline. The callback owns no packet/session processing.
	cancelDone := make(chan struct{})
	stop := context.AfterFunc(ctx, func() { s.pair.rtp.SetReadDeadline(time.Now()); close(cancelDone) })
	defer func() {
		if !stop() {
			<-cancelDone
		}
		s.pair.rtp.SetReadDeadline(time.Time{})
	}()
	var buffer [maxDatagram + 1]byte
	for {
		if err := ctx.Err(); err != nil {
			return p25rtp.Packet{}, err
		}
		n, remote, err := s.pair.rtp.ReadFromUDPAddrPort(buffer[:])
		if err != nil {
			if ctx.Err() != nil {
				return p25rtp.Packet{}, ctx.Err()
			}
			select {
			case <-s.done:
				return p25rtp.Packet{}, ErrClosed
			default:
			}
			if !deadline.IsZero() && !time.Now().Before(deadline) {
				return p25rtp.Packet{}, context.DeadlineExceeded
			}
			s.counts.read.Add(1)
			s.shutdown()
			return p25rtp.Packet{}, err
		}
		if remote != s.options.Remote {
			s.counts.endpoint.Add(1)
			continue
		}
		if n > maxDatagram {
			s.counts.oversized.Add(1)
			continue
		}
		// Reject unexpected header/role before payload parsing. The SIP owner
		// admits this endpoint; packets cannot create or replace that owner.
		if n < 13 || buffer[0] != 0x80 || buffer[1] != p25rtp.PayloadType || binary.BigEndian.Uint32(buffer[8:12]) != uint32(s.options.RemoteRole) {
			s.counts.header.Add(1)
			continue
		}
		p, err := p25rtp.Parse(buffer[:n])
		if err != nil {
			s.counts.payload.Add(1)
			continue
		}
		accept, ok := s.rx.Check(uint64(p.Sequence))
		if !ok {
			s.counts.duplicate.Add(1)
			continue
		}
		accept()
		select {
		case <-s.done:
			return p25rtp.Packet{}, ErrClosed
		default:
		}
		if ctx.Err() != nil {
			return p25rtp.Packet{}, ctx.Err()
		}
		s.counts.received.Add(1)
		return p, nil
	}
}

func (s *Session) Close() error {
	s.shutdown()
	// Socket closure interrupts an in-flight write; exclude any packet writer
	// before returning. Waiting Send callers observe done without using it.
	s.sendGate <- struct{}{}
	<-s.sendGate
	s.receiveGate <- struct{}{}
	<-s.receiveGate
	return nil
}

func (s *Session) shutdown() {
	s.closeOnce.Do(func() {
		close(s.done)
		s.pair.mu.Lock()
		s.pair.closeLocked()
		s.pair.mu.Unlock()
	})
}

func (s *Session) Counters() Counters {
	c := &s.counts
	return Counters{Sent: c.sent.Load(), Received: c.received.Load(), WrongEndpoint: c.endpoint.Load(), InvalidHeader: c.header.Load(), Oversized: c.oversized.Load(), StaleOrDuplicate: c.duplicate.Load(), InvalidPayload: c.payload.Load(), ReadErrors: c.read.Load(), WriteErrors: c.write.Load()}
}
