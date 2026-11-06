// Package rtpvoice transports the private p25rtp profile using plain UDP over the deployment VPN.
package rtpvoice

import (
	"fmt"
	"net"
	"net/netip"
	"sync"
)

// Pair reserves an even IPv4 RTP port and its adjacent RTCP port. A successful
// New transfers it to exactly one Session. Closing an unused Pair releases it.
type Pair struct {
	rtp, rtcp    *net.UDPConn
	mu           sync.Mutex
	used, closed bool
}

// Listen reserves a pair. Port zero chooses an available even pair, with at most
// 32 attempts. No socket sharing or remote endpoint learning is enabled.
func Listen(local netip.AddrPort) (*Pair, error) {
	if !local.IsValid() || !local.Addr().Is4() || local.Addr().IsMulticast() || local.Addr() == netip.AddrFrom4([4]byte{255, 255, 255, 255}) || local.Port() > 65532 || local.Port()%2 != 0 {
		return nil, fmt.Errorf("rtpvoice: local address must be IPv4 with an even RTP port")
	}
	for range 32 {
		r, err := net.ListenUDP("udp4", net.UDPAddrFromAddrPort(local))
		if err != nil {
			return nil, err
		}
		port := r.LocalAddr().(*net.UDPAddr).AddrPort().Port()
		if port%2 != 0 || port > 65532 {
			r.Close()
			continue
		}
		c, err := net.ListenUDP("udp4", net.UDPAddrFromAddrPort(netip.AddrPortFrom(local.Addr(), port+1)))
		if err == nil {
			return &Pair{rtp: r, rtcp: c}, nil
		}
		r.Close()
		if local.Port() != 0 {
			return nil, err
		}
	}
	return nil, fmt.Errorf("rtpvoice: could not reserve an even RTP/RTCP pair")
}

func (p *Pair) LocalRTP() netip.AddrPort {
	if p == nil || p.rtp == nil {
		return netip.AddrPort{}
	}
	return p.rtp.LocalAddr().(*net.UDPAddr).AddrPort()
}
func (p *Pair) LocalRTCP() netip.AddrPort {
	if p == nil || p.rtcp == nil {
		return netip.AddrPort{}
	}
	return p.rtcp.LocalAddr().(*net.UDPAddr).AddrPort()
}

// Close is idempotent. An owned Pair is closed through its Session instead.
func (p *Pair) Close() error {
	if p == nil || p.rtp == nil || p.rtcp == nil {
		return fmt.Errorf("rtpvoice: invalid port pair")
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.used {
		return fmt.Errorf("rtpvoice: pair belongs to a session")
	}
	return p.closeLocked()
}

func (p *Pair) closeLocked() error {
	if p.closed {
		return nil
	}
	p.closed = true
	e := p.rtp.Close()
	if err := p.rtcp.Close(); e == nil {
		e = err
	}
	return e
}
