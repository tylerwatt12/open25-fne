package sipvoice

import (
	"context"
	"errors"
	"strings"
	"time"

	"github.com/emiago/sipgo"
	"github.com/emiago/sipgo/sip"
)

const renewPackage = "trunksystem-p25-renew"
const allocationLease = 1500 * time.Millisecond // Private allocation policy, not a TIA media timer.

func (n *Node) infoPackages() string {
	var packages []string
	if n.opt.OnSource != nil {
		packages = append(packages, sourcePackage)
	}
	if n.opt.OnRenew != nil {
		packages = append(packages, renewPackage)
	}
	return strings.Join(packages, ", ")
}
func advertisesInfo(response *sip.Response, name string) bool {
	for _, value := range strings.Split(header(response, "Recv-Info"), ",") {
		if strings.TrimSpace(value) == name {
			return true
		}
	}
	return false
}
func (n *Node) onInfo(r *sip.Request, tx sip.ServerTransaction) {
	if header(r, "Info-Package") == renewPackage {
		n.onRenew(r, tx)
	} else {
		n.onSource(r, tx)
	}
}

// INFO exchanges share one gate so a source announcement and renewal cannot
// reach the peer in the opposite order from their dialog CSeq numbers.
func (s *Session) beginInfo(ctx context.Context, name string) (context.Context, func(), error) {
	if err := ctx.Err(); err != nil {
		return nil, nil, err
	}
	if !s.localFrom {
		return nil, nil, ErrRejected
	}
	if !s.node.begin() {
		return nil, nil, ErrClosed
	}
	c, cancel := context.WithTimeout(ctx, 2*time.Second)
	canceled := make(chan struct{})
	stop := context.AfterFunc(s.life, func() { cancel(); close(canceled) })
	acquired := false
	done := func() {
		if acquired {
			<-s.infoGate
		}
		if !stop() {
			<-canceled
		}
		cancel()
		s.node.wg.Done()
	}
	s.mu.Lock()
	closed := s.closed
	s.mu.Unlock()
	if closed {
		done()
		return nil, nil, ErrClosed
	}
	select {
	case <-c.Done():
		done()
		return nil, nil, c.Err()
	case <-s.ready:
	}
	select {
	case s.infoGate <- struct{}{}:
		acquired = true
	case <-c.Done():
		done()
		return nil, nil, c.Err()
	}
	s.mu.Lock()
	closed, supported := s.closed, advertisesInfo(s.response, name)
	s.mu.Unlock()
	if closed {
		done()
		return nil, nil, ErrClosed
	}
	if !supported {
		done()
		return nil, nil, ErrProfile
	}
	return c, done, nil
}
func (s *Session) sendInfo(ctx context.Context, r *sip.Request) error {
	tx, err := s.node.client.TransactionRequest(ctx, r, sipgo.ClientRequestAddVia)
	if err != nil {
		if ctx.Err() != nil {
			return ctx.Err()
		}
		return errors.New("SIP INFO failed")
	}
	defer tx.Terminate()
	for {
		select {
		case res := <-tx.Responses():
			if res == nil {
				return ErrRejected
			}
			if res.IsProvisional() {
				continue
			}
			if res.StatusCode != 200 || !sourceResponseMatches(r, res) || header(res, "X-TrunkSystem-Peer") != s.peer.ID {
				return ErrRejected
			}
			return nil
		case <-tx.Done():
			if ctx.Err() != nil {
				return ctx.Err()
			}
			return ErrRejected
		case <-ctx.Done():
			return ctx.Err()
		}
	}
}
func (s *Session) beginInfoCallback(ctx context.Context) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.closed || ctx.Err() != nil {
		return false
	}
	s.infoPending = true
	return true
}
func (s *Session) endInfoCallback() {
	s.mu.Lock()
	s.infoPending = false
	closed := s.closed && !s.allocationPending
	s.mu.Unlock()
	if closed {
		s.cleanup()
	}
}

// Renew refreshes this caller's already admitted allocation. It does not
// authorize a source, start media, or change RTP endpoints or radio metadata.
func (s *Session) Renew(ctx context.Context) error {
	c, done, err := s.beginInfo(ctx, renewPackage)
	if err != nil {
		return err
	}
	defer done()
	r, err := s.dialogRequest(sip.INFO)
	if err != nil {
		return err
	}
	r.AppendHeader(sip.NewHeader("Info-Package", renewPackage))
	r.AppendHeader(sip.NewHeader("Content-Disposition", "Info-Package"))
	return s.sendInfo(c, r)
}
func (n *Node) onRenew(r *sip.Request, tx sip.ServerTransaction) {
	if !n.begin() {
		return
	}
	defer n.wg.Done()
	reply := func(code int) {
		res := sip.NewResponseFromRequest(r, code, "Allocation Renewal", nil)
		res.AppendHeader(sip.NewHeader("X-TrunkSystem-Peer", n.opt.PeerID))
		if code == 469 {
			res.AppendHeader(sip.NewHeader("Recv-Info", n.infoPackages()))
		}
		_ = tx.Respond(res)
	}
	s := n.find(r)
	if s == nil || !s.matches(r) || s.localFrom {
		reply(481)
		return
	}
	ctx, cancel := context.WithTimeout(s.life, 2*time.Second)
	defer cancel()
	if ctx.Err() != nil || n.auth(ctx, r, s.call, s.remoteEndpoints()) != nil {
		reply(481)
		return
	}
	if n.opt.OnRenew == nil {
		reply(469)
		return
	}
	if header(r, "Content-Disposition") != "Info-Package" || len(r.Body()) != 0 || len(r.GetHeaders("Content-Type")) != 0 {
		reply(415)
		return
	}
	select {
	case s.infoGate <- struct{}{}:
	case <-ctx.Done():
		reply(481)
		return
	}
	defer func() { <-s.infoGate }()
	select {
	case <-s.ready:
	case <-ctx.Done():
		reply(481)
		return
	}
	s.mu.Lock()
	seq := r.CSeq().SeqNo
	duplicate := seq == s.renewCSeq && s.renewCSeq != 0
	valid := !s.closed && (seq > s.remoteCSeq || duplicate)
	s.mu.Unlock()
	if !valid {
		reply(481)
		return
	}
	if duplicate {
		reply(200)
		return
	} // A replay is never a lease refresh.
	if !s.beginInfoCallback(ctx) {
		reply(481)
		return
	}
	err := n.opt.OnRenew(ctx, s)
	s.mu.Lock()
	code := 200
	if s.closed || ctx.Err() != nil {
		code = 481
	} else if err != nil {
		code = 488
	} else {
		s.remoteCSeq, s.renewCSeq = seq, seq
		s.resetLeaseLocked()
	}
	s.mu.Unlock()
	s.endInfoCallback()
	reply(code)
}
func (s *Session) resetLeaseLocked() {
	s.leaseDeadline = time.Now().Add(allocationLease)
	if s.leaseTimer == nil {
		s.leaseTimer = time.AfterFunc(allocationLease, s.expireLease)
	} else {
		s.leaseTimer.Reset(allocationLease)
	}
}
func (s *Session) expireLease() {
	if !s.node.begin() {
		return
	}
	defer s.node.wg.Done()
	s.mu.Lock()
	if s.closed || s.leaseTimer == nil {
		s.mu.Unlock()
		return
	}
	if remaining := time.Until(s.leaseDeadline); remaining > 0 {
		s.leaseTimer.Reset(remaining)
		s.mu.Unlock()
		return
	}
	complete := s.finishLocked(EndTimeout)
	s.mu.Unlock()
	if complete != nil {
		complete()
		ctx, cancel := context.WithTimeout(s.node.ctx, 2*time.Second)
		defer cancel()
		_ = s.sendBye(ctx)
	}
}
