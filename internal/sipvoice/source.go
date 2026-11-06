package sipvoice

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"time"

	"github.com/emiago/sipgo/sip"
)

const sourcePackage = "trunksystem-p25-source"
const sourceContentType = "application/vnd.trunksystem.p25-source+json"

// Source binds actual first-voice metadata after channel reservation. This is
// one private INFO package, not an ISSI procedure or an RTP layout extension.
type Source struct {
	Epoch       uint64 `json:"epoch"`
	TSN         uint8  `json:"tsn"`
	MFID        uint8  `json:"mfid"`
	Service     uint8  `json:"service"`
	AlgorithmID uint8  `json:"algorithm_id"`
	KeyID       uint16 `json:"key_id"`
}

func (s Source) valid() bool { return s.Epoch != 0 && s.TSN > 0 && s.TSN <= 127 && s.MFID <= 1 }

func (s Source) follows(old *Source) bool {
	if old == nil {
		return s.Epoch == 1
	}
	if s == *old {
		return true
	}
	next := old.TSN + 2
	if next > 127 {
		next = 2 - old.TSN%2
	}
	return old.Epoch != ^uint64(0) && s.Epoch == old.Epoch+1 && s.TSN == next
}

// SourceReady binds successive actual spurts on the caller's admitted dialog.
// An exact repeat is harmless; a new spurt increments epoch and TSN. No media
// timer starts here: the owner starts RTP only after this exchange succeeds.
func (s *Session) SourceReady(ctx context.Context, source Source) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	if !source.valid() {
		return ErrRejected
	}
	c, done, err := s.beginInfo(ctx, sourcePackage)
	if err != nil {
		return err
	}
	defer done()
	s.mu.Lock()
	old := s.source
	s.mu.Unlock()
	if !source.follows(old) {
		return ErrRejected
	}
	if old != nil && *old == source {
		return nil
	}
	r, err := s.dialogRequest(sip.INFO)
	if err != nil {
		return err
	}
	r.AppendHeader(sip.NewHeader("Info-Package", sourcePackage))
	r.AppendHeader(sip.NewHeader("Content-Disposition", "Info-Package"))
	r.AppendHeader(sip.NewHeader("Content-Type", sourceContentType))
	body, _ := json.Marshal(source)
	r.SetBody(body)
	if err = s.sendInfo(c, r); err != nil {
		return err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.closed || c.Err() != nil {
		return ErrClosed
	}
	s.source = &source
	return nil
}

func sourceResponseMatches(r *sip.Request, res *sip.Response) bool {
	return res.CallID() != nil && res.CallID().Value() == r.CallID().Value() && res.From() != nil && res.From().Params.GetOr("tag", "") == r.From().Params.GetOr("tag", "") && res.To() != nil && res.To().Params.GetOr("tag", "") == r.To().Params.GetOr("tag", "") && res.CSeq() != nil && res.CSeq().SeqNo == r.CSeq().SeqNo && res.CSeq().MethodName == sip.INFO
}

func (n *Node) onSource(r *sip.Request, tx sip.ServerTransaction) {
	if !n.begin() {
		return
	}
	defer n.wg.Done()
	reply := func(code int) {
		res := sip.NewResponseFromRequest(r, code, "Source Binding", nil)
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
	if header(r, "Info-Package") != sourcePackage || n.opt.OnSource == nil {
		reply(469)
		return
	}
	if header(r, "Content-Type") != sourceContentType || header(r, "Content-Disposition") != "Info-Package" || len(r.Body()) > 128 {
		reply(415)
		return
	}
	var value struct {
		Epoch       *uint64 `json:"epoch"`
		TSN         *uint8  `json:"tsn"`
		MFID        *uint8  `json:"mfid"`
		Service     *uint8  `json:"service"`
		AlgorithmID *uint8  `json:"algorithm_id"`
		KeyID       *uint16 `json:"key_id"`
	}
	decoder := json.NewDecoder(bytes.NewReader(r.Body()))
	decoder.DisallowUnknownFields()
	if decoder.Decode(&value) != nil || value.Epoch == nil || value.TSN == nil || value.MFID == nil || value.Service == nil || value.AlgorithmID == nil || value.KeyID == nil || decoder.Decode(new(any)) != io.EOF {
		reply(400)
		return
	}
	source := Source{Epoch: *value.Epoch, TSN: *value.TSN, MFID: *value.MFID, Service: *value.Service, AlgorithmID: *value.AlgorithmID, KeyID: *value.KeyID}
	if !source.valid() {
		reply(488)
		return
	}
	select {
	case s.infoGate <- struct{}{}:
	case <-ctx.Done():
		reply(480)
		return
	}
	defer func() { <-s.infoGate }()
	s.mu.Lock()
	valid := !s.closed && (r.CSeq().SeqNo > s.remoteCSeq || (r.CSeq().SeqNo == s.sourceCSeq && s.source != nil && *s.source == source))
	old := s.source
	s.mu.Unlock()
	select {
	case <-s.ready:
	case <-s.done:
		valid = false
	case <-ctx.Done():
		valid = false
	}
	if !valid || !source.follows(old) {
		reply(481)
		return
	}
	if old != nil && *old == source {
		// Exact source repeats do not rewind a later renewal's remote CSeq.
		s.mu.Lock()
		closed := s.closed || ctx.Err() != nil
		if !closed && r.CSeq().SeqNo > s.remoteCSeq {
			s.remoteCSeq, s.sourceCSeq = r.CSeq().SeqNo, r.CSeq().SeqNo
		}
		s.mu.Unlock()
		if closed {
			reply(481)
		} else {
			reply(200)
		}
		return
	}
	if !s.beginInfoCallback(ctx) {
		reply(481)
		return
	}
	err := n.opt.OnSource(ctx, s, source)
	s.mu.Lock()
	code := 200
	if s.closed || ctx.Err() != nil {
		code = 481
	} else if err != nil {
		code = 488
	} else {
		s.source = &source
		s.remoteCSeq, s.sourceCSeq = r.CSeq().SeqNo, r.CSeq().SeqNo
	}
	s.mu.Unlock()
	s.endInfoCallback()
	reply(code)
}
