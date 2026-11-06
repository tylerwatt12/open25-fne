package framework

import (
	"context"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"slices"
	"strconv"
	"sync"
	"time"

	"open25.local/open25-fne/internal/dispatch"
	"open25.local/open25-fne/internal/radio"
	"open25.local/open25-fne/internal/routing"
)

type nativeRFAdmission struct {
	ID         string `json:"id"`
	Epoch      string `json:"epoch"`
	Source     int    `json:"srcId"`
	Group      int    `json:"dstId"`
	Emergency  bool   `json:"emergency"`
	Secure     bool   `json:"secure"`
	State      string `json:"state"`
	ExpiresMS  int    `json:"expiresMs"`
	GrantEpoch string `json:"grantEpoch"`
	ChannelNo  int    `json:"channelNo"`
}
type admittedRF struct {
	localOnly                                               bool
	request                                                 nativeRFAdmission
	native                                                  radio.Native
	created, approvedAt, lastObserved, lastRenew, stoppedAt time.Time
	cancel                                                  context.CancelFunc
	stream                                                  *rfStream
	reserving, ended                                        bool
	boundStream                                             string
	boundAlgorithm                                          uint8
	boundKey                                                uint16
	inputSequence, sequence                                 uint64
}
type rfAdmissionManager struct {
	s       *Service
	mu      sync.Mutex
	entries map[string]*admittedRF
	aliases map[string]string
	seen    map[string]time.Time
	wg      sync.WaitGroup
	epoch   uint64
}

// Reservations and stream aliases belong to one native Site lifetime. End the
// old entries before clearing their IDs; late replies cannot enter the new run.
func (m *rfAdmissionManager) retireRun() {
	m.mu.Lock()
	m.epoch++
	entries := []*admittedRF{}
	for _, entry := range m.entries {
		if !entry.ended {
			entry.ended, entry.stoppedAt = true, time.Now()
			if entry.cancel != nil {
				entry.cancel()
			}
			entries = append(entries, entry)
		}
	}
	m.entries = map[string]*admittedRF{}
	m.aliases = map[string]string{}
	m.seen = map[string]time.Time{}
	m.mu.Unlock()
	var retiring sync.WaitGroup
	for _, entry := range entries {
		retiring.Add(1)
		go func(entry *admittedRF) {
			defer retiring.Done()
			m.reject(entry)
			if entry.stream != nil {
				m.s.endRFStream(entry.stream)
			}
		}(entry)
	}
	retiring.Wait()
}

func (m *rfAdmissionManager) connected() bool {
	return m.s.mediaClient != nil && m.s.mediaClient.URL() != ""
}
func (m *rfAdmissionManager) allStart(tg int) bool {
	if applied := m.s.radioPolicy.Load(); applied != nil {
		return slices.ContainsFunc(applied.Talkgroups, func(g radio.Talkgroup) bool { return g.TGID == tg && g.Enabled && g.StartPolicy == routing.AllStart })
	}
	return slices.ContainsFunc(m.s.view().System.Talkgroups, func(g Talkgroup) bool { return g.TGID == tg && g.Enabled && g.StartPolicy == routing.AllStart })
}
func (m *rfAdmissionManager) hasAllStart() bool {
	if applied := m.s.radioPolicy.Load(); applied != nil {
		return slices.ContainsFunc(applied.Talkgroups, func(g radio.Talkgroup) bool { return g.Enabled && g.StartPolicy == routing.AllStart })
	}
	return slices.ContainsFunc(m.s.view().System.Talkgroups, func(g Talkgroup) bool { return g.Enabled && g.StartPolicy == routing.AllStart })
}
func (m *rfAdmissionManager) active() bool {
	m.mu.Lock()
	defer m.mu.Unlock()
	for _, v := range m.entries {
		if !v.ended {
			return true
		}
	}
	return false
}
func (m *rfAdmissionManager) run() {
	tick := time.NewTicker(50 * time.Millisecond)
	defer tick.Stop()
	defer m.wg.Wait()
	defer func() {
		for _, entry := range m.snapshot() {
			m.stop(entry)
		}
	}()
	for {
		select {
		case <-m.s.operationCtx.Done():
			return
		case <-tick.C:
		}
		now := time.Now()
		m.expire(now)
		if !m.connected() {
			for _, entry := range m.snapshot() {
				m.isolate(entry)
			}
		}
		if !m.active() && !m.hasAllStart() {
			continue
		}
		native, err := m.s.radio.ControlClient()
		if err != nil {
			continue
		}
		status := m.s.radio.Status()
		m.mu.Lock()
		epoch := m.epoch
		m.mu.Unlock()
		ctx, done := context.WithTimeout(m.s.operationCtx, 300*time.Millisecond)
		raw, err := native.Get(ctx, "/p25/rf-admission/pending")
		done()
		if err != nil {
			continue
		}
		current, ownerErr := m.s.radio.ControlClient()
		after := m.s.radio.Status()
		if ownerErr != nil || current != native || after.State != "running" || !after.StartedAt.Equal(status.StartedAt) {
			continue
		}
		var records []nativeRFAdmission
		if json.Unmarshal(raw["requests"], &records) != nil {
			continue
		}
		for _, record := range records {
			m.observeAtEpoch(native, record, epoch)
		}
	}
}
func (m *rfAdmissionManager) snapshot() []*admittedRF {
	m.mu.Lock()
	defer m.mu.Unlock()
	out := make([]*admittedRF, 0, len(m.entries))
	for _, entry := range m.entries {
		if !entry.ended {
			out = append(out, entry)
		}
	}
	return out
}
func (m *rfAdmissionManager) expire(now time.Time) {
	m.mu.Lock()
	stale := []*admittedRF{}
	for id, entry := range m.entries {
		if !entry.ended && now.Sub(entry.lastObserved) > time.Second {
			stale = append(stale, entry)
		}
		if entry.ended && !entry.stoppedAt.IsZero() && now.Sub(entry.stoppedAt) > time.Minute {
			delete(m.entries, id)
		}
	}
	for id, at := range m.seen {
		if now.Sub(at) > time.Minute {
			delete(m.seen, id)
			delete(m.aliases, id)
		}
	}
	m.mu.Unlock()
	for _, entry := range stale {
		m.stop(entry)
	}
}
func validRFAdmission(r nativeRFAdmission) bool {
	id, e := hex.DecodeString(r.ID)
	if e != nil || len(id) != 16 || len(r.ID) != 32 {
		return false
	}
	epoch, e := strconv.ParseUint(r.Epoch, 10, 64)
	if e != nil || epoch == 0 || r.Source < 1 || r.Source > 0xfffffb || r.Group < 1 || r.Group > 65534 {
		return false
	}
	switch r.State {
	case "pending", "approved", "committing":
		return r.ExpiresMS >= 0 && r.ExpiresMS <= 5000
	case "committed":
		grant, e := strconv.ParseUint(r.GrantEpoch, 10, 64)
		return e == nil && grant > 0 && r.ChannelNo > 0 && r.ChannelNo <= 4095
	case "ended", "expired", "rejected":
		return true
	}
	return false
}
func sameRFIdentity(a, b nativeRFAdmission) bool {
	return a.ID == b.ID && a.Epoch == b.Epoch && a.Source == b.Source && a.Group == b.Group && a.Emergency == b.Emergency && a.Secure == b.Secure
}
func (m *rfAdmissionManager) observe(native radio.Native, record nativeRFAdmission) {
	m.mu.Lock()
	epoch := m.epoch
	m.mu.Unlock()
	m.observeAtEpoch(native, record, epoch)
}

func (m *rfAdmissionManager) observeAtEpoch(native radio.Native, record nativeRFAdmission, epoch uint64) {
	if !validRFAdmission(record) {
		return
	}
	now := time.Now()
	blocked := m.s.radioServiceInhibited(record.Source)
	m.mu.Lock()
	if m.epoch != epoch {
		m.mu.Unlock()
		return
	}
	entry := m.entries[record.ID]
	if blocked && entry == nil {
		m.mu.Unlock()
		m.reject(&admittedRF{request: record, native: native})
		return
	}
	if entry == nil && record.State == "pending" && record.ExpiresMS > 0 && len(m.entries) < 256 && m.connected() {
		ctx, cancel := context.WithTimeout(m.s.operationCtx, time.Duration(record.ExpiresMS)*time.Millisecond)
		entry = &admittedRF{request: record, native: native, created: now, lastObserved: now, reserving: true, cancel: cancel}
		m.entries[record.ID] = entry
		m.wg.Add(1)
		go func() { defer m.wg.Done(); defer cancel(); m.reserve(ctx, entry) }()
	}
	if entry == nil || entry.ended || !sameRFIdentity(entry.request, record) {
		m.mu.Unlock()
		return
	}
	if blocked {
		m.mu.Unlock()
		m.stop(entry)
		return
	}
	if entry.request.GrantEpoch != "" && entry.request.GrantEpoch != "0" &&
		(entry.request.GrantEpoch != record.GrantEpoch || entry.request.ChannelNo != record.ChannelNo) {
		m.mu.Unlock()
		return
	}
	entry.request = record
	entry.lastObserved = now
	terminal := record.State == "rejected" || record.State == "expired" || record.State == "ended"
	stream := entry.stream
	localOnly := entry.localOnly
	ready := record.State == "committed" && stream != nil
	renew := stream != nil && now.Sub(entry.lastRenew) > 350*time.Millisecond
	if renew {
		entry.lastRenew = now
	}
	m.mu.Unlock()
	if terminal {
		m.stop(entry)
		return
	}
	if ready {
		m.s.broker.mu.Lock()
		present := m.s.broker.rfCalls[stream.request.ID] == stream
		if present {
			stream.ready = true
		}
		m.s.broker.mu.Unlock()
		if !present {
			m.stop(entry)
			return
		}
	}
	if renew {
		ctx, done := context.WithTimeout(m.s.operationCtx, 500*time.Millisecond)
		defer done()
		m.s.broker.mu.Lock()
		local, call, address, generation := stream.local, stream.remoteCall, stream.remoteURL, stream.remoteGeneration
		m.s.broker.mu.Unlock()
		current, gen := m.s.mediaClient.Link()
		if !localOnly && (current != address || gen != generation) {
			m.isolate(entry)
			localOnly = true
		}
		if m.s.broker.Renew(ctx, "rf:"+m.s.Config().InstanceID, local) != nil {
			m.stop(entry)
		} else if !localOnly && (call == nil || call.Renew() != nil) {
			m.isolate(entry)
		}
	}
}
func (m *rfAdmissionManager) reject(entry *admittedRF) {
	m.mu.Lock()
	native, id, epoch := entry.native, entry.request.ID, entry.request.Epoch
	m.mu.Unlock()
	if native == nil {
		return
	}
	ctx, done := context.WithTimeout(context.Background(), 300*time.Millisecond)
	defer done()
	_, _ = native.Put(ctx, "/p25/rf-admission/decision", map[string]any{"id": id, "epoch": epoch, "approved": false})
}
func (m *rfAdmissionManager) reserve(ctx context.Context, entry *admittedRF) {
	m.mu.Lock()
	record := entry.request
	native := entry.native
	m.mu.Unlock()
	id := fmt.Sprintf("rf-admission:%s:%s", m.s.Config().InstanceID, record.ID)
	q := routing.Request{CallRequest: dispatch.CallRequest{ID: id, SessionID: "rf:" + m.s.Config().InstanceID, RadioID: record.Source, Talkgroups: []int{record.Group}, Secure: record.Secure, Emergency: record.Emergency}, SourceKind: "rf", OriginSite: m.s.Config().InstanceID}
	q.EncryptionPending = q.Secure
	if m.s.radioServiceInhibited(record.Source) || !subscriberGroupAllowed(m.s.view().System, record.Source, record.Group, true) {
		m.reject(entry)
		m.stop(entry)
		return
	}
	prepared, err := m.s.prepareCall(q.CallRequest, "rf", false)
	q.CallRequest = prepared
	address, generation := m.s.mediaClient.Link()
	var remote, local dispatch.Grant
	var remoteCall *voiceCall
	installed := false
	defer func() {
		if installed {
			return
		}
		// A cancelled approval HTTP request may have committed natively. Send
		// another conditional rejection after that request has returned.
		m.reject(entry)
		m.stop(entry)
		cleanup, done := context.WithTimeout(context.Background(), 2*time.Second)
		defer done()
		if local.ID != "" {
			_ = m.s.broker.End(cleanup, "rf:"+m.s.Config().InstanceID, local)
		}
		if remote.ID != "" {
			if remoteCall != nil {
				_ = remoteCall.End(cleanup)
			}
		}
	}()
	if err != nil || address == "" {
		return
	}
	q, err = m.s.observeRFSource(q)
	if err != nil {
		return
	}
	endpoint, e := m.s.mediaClient.endpointAt(generation)
	if e != nil {
		return
	}
	if remoteCall, err = m.s.voice.Load().begin(ctx, endpoint, q, false); err != nil {
		return
	}
	remote = remoteCall.grant
	if local, err = m.s.broker.Begin(ctx, "rf:"+m.s.Config().InstanceID, q); err != nil {
		return
	}
	m.mu.Lock()
	current, currentGeneration := m.s.mediaClient.Link()
	valid := ctx.Err() == nil && !entry.ended && entry.request.State == "pending" && current == address && currentGeneration == generation && !m.s.radioServiceInhibited(record.Source)
	if valid {
		stream := &rfStream{request: q, local: local, remote: remote, remoteCall: remoteCall, remoteURL: address, remoteGeneration: generation, lastFrame: time.Now()}
		entry.stream = stream
		entry.reserving = false
		entry.approvedAt = time.Now()
		m.s.broker.mu.Lock()
		m.s.broker.rfCalls[id] = stream
		m.s.broker.mu.Unlock()
	}
	m.mu.Unlock()
	if !valid {
		return
	}
	if m.s.radioServiceInhibited(record.Source) {
		return
	}
	raw, err := native.Put(ctx, "/p25/rf-admission/decision", map[string]any{"id": record.ID, "epoch": record.Epoch, "approved": true})
	if err != nil {
		return
	}
	data, _ := json.Marshal(raw)
	var decision nativeRFAdmission
	_ = json.Unmarshal(data, &decision)
	if !sameRFIdentity(record, decision) || (decision.State != "approved" && decision.State != "committing" && decision.State != "committed") {
		return
	}
	m.mu.Lock()
	alive := !entry.ended && !m.s.radioServiceInhibited(record.Source)
	m.mu.Unlock()
	if !alive {
		return
	}
	installed = true
}
func (m *rfAdmissionManager) stop(entry *admittedRF) {
	m.mu.Lock()
	if entry.ended {
		m.mu.Unlock()
		return
	}
	entry.ended = true
	entry.stoppedAt = time.Now()
	cancel, stream := entry.cancel, entry.stream
	m.mu.Unlock()
	if cancel != nil {
		cancel()
	}
	m.reject(entry)
	if stream != nil {
		m.s.endRFStream(stream)
	}
}

// Losing the Zone does not terminate an RF call already committed locally.
// This call finishes at its Site; the next call uses the current Zone link.
func (m *rfAdmissionManager) isolate(entry *admittedRF) {
	m.mu.Lock()
	if entry.ended || entry.localOnly {
		m.mu.Unlock()
		return
	}
	if entry.request.State != "committed" || entry.stream == nil {
		m.mu.Unlock()
		m.stop(entry)
		return
	}
	entry.localOnly = true
	stream := entry.stream
	m.mu.Unlock()
	stream.media.Lock()
	m.s.broker.mu.Lock()
	call := stream.remoteCall
	stream.remote, stream.remoteURL, stream.remoteGeneration, stream.remoteCall = dispatch.Grant{}, "", 0, nil
	m.s.broker.mu.Unlock()
	stream.media.Unlock()
	if call != nil {
		m.s.retireRFRemote(call)
	}
}

// Each native stream is accepted once. A burst end retires that stream, while
// the AllStart allocations remain owned until the native channel allocation
// ends. A subsequent burst uses a fresh stream ID and a continuing sequence.
func (m *rfAdmissionManager) bind(f dispatch.Frame) (dispatch.Frame, bool) {
	if m.s.radioServiceInhibited(f.SourceID) {
		return f, false
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	first, known := m.seen[f.CallID]
	if !known {
		if len(m.seen) >= 4096 {
			return f, false
		}
		first = time.Now()
		m.seen[f.CallID] = first
	}
	var entry *admittedRF
	if id, ok := m.aliases[f.CallID]; ok {
		entry = m.entries[id]
		if entry == nil || entry.ended || entry.stream == nil || entry.boundStream != f.CallID {
			return f, false
		}
	} else {
		for id, candidate := range m.entries {
			r := candidate.request
			if candidate.ended || candidate.boundStream != "" || candidate.stream == nil || candidate.approvedAt.IsZero() || r.State != "committed" || r.Source != f.SourceID || r.Group != f.TalkgroupID || first.Before(candidate.approvedAt) {
				continue
			}
			if f.End || !f.MediaValid() || f.Encrypted != r.Secure {
				return f, false
			}
			entry = candidate
			entry.boundStream = f.CallID
			entry.boundAlgorithm, entry.boundKey = f.AlgorithmID, f.KeyID
			entry.inputSequence = 0
			m.aliases[f.CallID] = id
			break
		}
	}
	if entry == nil {
		if m.connected() && m.allStart(f.TalkgroupID) {
			return f, false
		}
		return f, true
	}
	r := entry.request
	if r.Source != f.SourceID || r.Group != f.TalkgroupID || f.Encrypted != r.Secure || (f.Encrypted && (f.KeyID != entry.boundKey || f.AlgorithmID != entry.boundAlgorithm)) || time.Since(entry.lastObserved) > time.Second {
		return f, false
	}
	if f.End {
		entry.boundStream = ""
		entry.inputSequence = 0
		stream := entry.stream
		m.s.broker.mu.Lock()
		if !m.s.broker.closed {
			m.s.broker.wg.Add(1)
			go func() { defer m.s.broker.wg.Done(); m.s.endRFSpurt(stream) }()
		}
		m.s.broker.mu.Unlock()
		return f, false
	}
	if f.Sequence == 0 || f.Sequence <= entry.inputSequence {
		return f, false
	}
	entry.inputSequence = f.Sequence
	entry.sequence++
	f.Sequence = entry.sequence
	f.CallID = entry.stream.request.ID
	return f, true
}
