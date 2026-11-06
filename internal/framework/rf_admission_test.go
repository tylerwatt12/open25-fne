package framework

import (
	"context"
	"encoding/json"
	"net/http"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"open25.local/open25-fne/internal/dispatch"
	"open25.local/open25-fne/internal/radio"
	"open25.local/open25-fne/internal/routing"
)

type admissionNative struct {
	mu             sync.Mutex
	record         nativeRFAdmission
	decisions      []bool
	approveEntered chan struct{}
	approveWait    <-chan struct{}
	enter          sync.Once
}

func (n *admissionNative) Get(context.Context, string) (map[string]json.RawMessage, error) {
	n.mu.Lock()
	defer n.mu.Unlock()
	raw, _ := json.Marshal([]nativeRFAdmission{n.record})
	return map[string]json.RawMessage{"requests": raw}, nil
}
func (n *admissionNative) Put(_ context.Context, _ string, value any) (map[string]json.RawMessage, error) {
	body := value.(map[string]any)
	approved := body["approved"].(bool)
	if approved && n.approveWait != nil {
		n.enter.Do(func() {
			if n.approveEntered != nil {
				close(n.approveEntered)
			}
		})
		<-n.approveWait // emulate a response/commit that outlives cancellation
	}
	n.mu.Lock()
	defer n.mu.Unlock()
	n.decisions = append(n.decisions, approved)
	if approved {
		n.record.State = "approved"
	} else {
		n.record.State = "rejected"
	}
	data, _ := json.Marshal(n.record)
	out := map[string]json.RawMessage{}
	_ = json.Unmarshal(data, &out)
	return out, nil
}
func (n *admissionNative) approved() bool {
	n.mu.Lock()
	defer n.mu.Unlock()
	return slices.Contains(n.decisions, true)
}
func admissionRecord() nativeRFAdmission {
	return nativeRFAdmission{ID: strings.Repeat("a", 32), Epoch: "1", Source: 10001, Group: 1001, State: "pending", ExpiresMS: 4900}
}
func boundManager() (*rfAdmissionManager, *admittedRF) {
	now := time.Now()
	record := admissionRecord()
	record.State = "committed"
	record.GrantEpoch = "17"
	record.ChannelNo = 81
	// This binding-only fixture owns no running route or burst worker.
	service := &Service{mediaClient: &mediaClient{activeURL: "http://zone"}, broker: &callBroker{closed: true}}
	service.inhibition.Store(inhibitionSnapshot(radioInhibitionPolicy{Desired: []int{}}, nil))
	service.directory.value.Store(&directorySnapshot{System: SystemConfig{Talkgroups: []Talkgroup{{TGID: 1001, AdmissionMode: "clear", TransmitMode: "clear", Enabled: true, StartPolicy: routing.AllStart}}}})
	entry := &admittedRF{request: record, created: now.Add(-time.Second), approvedAt: now.Add(-500 * time.Millisecond), lastObserved: now, stream: &rfStream{request: routing.Request{CallRequest: dispatch.CallRequest{ID: "approved-call", RadioID: 10001, Talkgroups: []int{1001}}}}}
	return &rfAdmissionManager{s: service, entries: map[string]*admittedRF{record.ID: entry}, aliases: map[string]string{}, seen: map[string]time.Time{}}, entry
}
func admissionFrame(id string, sequence uint64) dispatch.Frame {
	return sipTestFrame(id, 10001, 1001, 1, sequence-1)
}
func TestRFAdmissionBindingRetiresOldStreamsAndContinuesSequenceAcrossBursts(t *testing.T) {
	m, entry := boundManager()
	m.seen["old"] = entry.approvedAt.Add(-time.Millisecond)
	if _, ok := m.bind(admissionFrame("old", 1)); ok {
		t.Fatal("pre-approval stream attached to a new allocation")
	}
	a, ok := m.bind(admissionFrame("first-burst", 20))
	if !ok || a.CallID != "approved-call" || a.Sequence != 1 {
		t.Fatal("fresh approved stream not bound", a, ok)
	}
	if _, ok = m.bind(admissionFrame("first-burst", 19)); ok {
		t.Fatal("out of order source data admitted")
	}
	bad := admissionFrame("first-burst", 21)
	bad.SourceID = 10003
	if _, ok = m.bind(bad); ok {
		t.Fatal("alias substituted another source")
	}
	end := admissionFrame("first-burst", 21)
	end.End = true
	end.PCM, end.Encoded, end.P25 = nil, nil, nil
	if _, ok = m.bind(end); ok {
		t.Fatal("burst end was forwarded as allocation end")
	}
	if entry.ended {
		t.Fatal("burst end released AllStart allocation during native hang")
	}
	if _, ok = m.bind(admissionFrame("first-burst", 22)); ok {
		t.Fatal("old stream tail reopened a burst")
	}
	b, ok := m.bind(admissionFrame("second-burst", 1))
	if !ok || b.Sequence != 2 || b.CallID != a.CallID {
		t.Fatal("new burst replayed sequence or changed allocation", b, ok)
	}
	if _, ok = m.bind(admissionFrame("first-burst", 23)); ok {
		t.Fatal("old burst captured later grant")
	}
}
func TestRFAdmissionRejectsMalformedOrChangedNativeIdentity(t *testing.T) {
	m, entry := boundManager()
	entry.stream = nil
	entry.lastRenew = time.Now()
	native := &admissionNative{record: entry.request}
	entry.native = native
	for _, change := range []func(*nativeRFAdmission){func(r *nativeRFAdmission) { r.Epoch = "2" }, func(r *nativeRFAdmission) { r.Source = 10003 }, func(r *nativeRFAdmission) { r.Group = 1002 }, func(r *nativeRFAdmission) { r.Secure = true }, func(r *nativeRFAdmission) { r.GrantEpoch = "0" }, func(r *nativeRFAdmission) { r.ChannelNo = 0 }} {
		record := entry.request
		change(&record)
		m.observe(native, record)
		if !sameRFIdentity(entry.request, native.record) || entry.request.GrantEpoch != "17" || entry.request.ChannelNo != 81 {
			t.Fatal("native record replaced original call identity", entry.request)
		}
	}
	record := entry.request
	record.State = "ended"
	m.observe(native, record)
	if !entry.ended {
		t.Fatal("terminal allocation was not retired")
	}
	native.mu.Lock()
	defer native.mu.Unlock()
	if len(native.decisions) != 1 || native.decisions[0] {
		t.Fatal("stop did not cancel native allocation")
	}
}

func TestRFAdmissionRetiresNativeRunAndRejectsLatePollWithReusedID(t *testing.T) {
	m, old := boundManager()
	old.stream = nil
	old.native = &admissionNative{record: old.request}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	old.cancel = cancel
	m.aliases["old-stream"] = old.request.ID
	m.seen["old-stream"] = time.Now()
	previous := m.epoch
	m.retireRun()
	if ctx.Err() == nil || !old.ended || len(m.entries) != 0 || len(m.aliases) != 0 || len(m.seen) != 0 {
		t.Fatal("native lifetime retirement retained an admission or stream alias")
	}
	current := &admittedRF{request: old.request, native: &admissionNative{record: old.request}, lastObserved: time.Now(), lastRenew: time.Now()}
	m.entries[current.request.ID] = current
	late := old.request
	late.State = "ended"
	m.observeAtEpoch(old.native, late, previous)
	if current.ended || current.request.State != "committed" {
		t.Fatal("a late old-native poll terminated the same ID in the new run")
	}
	m.observeAtEpoch(current.native, current.request, m.epoch)
	if current.ended || !sameRFIdentity(current.request, old.request) {
		t.Fatal("current-run admission could not use a retired native ID")
	}
}
func TestRFAdmissionBindingChecksSecureMetadataForEveryFrame(t *testing.T) {
	m, entry := boundManager()
	entry.request.Secure = true
	entry.stream.request.Secure = true
	entry.stream.request.EncryptionPending = true
	f := sipTestFrame("cipher-stream", 10001, 1001, 1, 0)
	f.AlgorithmID, f.KeyID, f.Encrypted, f.ServiceOptions = 0x84, 7, true, 0x44
	f.P25.Header.AlgorithmID, f.P25.Header.KeyID = 0x84, 7
	if _, ok := m.bind(f); !ok {
		t.Fatal("approved secure stream rejected")
	}
	if entry.stream.request.KeyID != 0 || entry.stream.request.AlgorithmID != 0 {
		t.Fatal("reservation invented an assigned receive identity")
	}
	f.Sequence = 2
	f.AlgorithmID = 129
	if _, ok := m.bind(f); ok {
		t.Fatal("secure RF stream switched algorithm with same KID")
	}
	f.KeyID = 8
	f.AlgorithmID = 132
	if _, ok := m.bind(f); ok {
		t.Fatal("stream switched keys")
	}
	clear := admissionFrame("cipher-stream", 2)
	if _, ok := m.bind(clear); ok {
		t.Fatal("secure stream downgraded to clear")
	}
	f.KeyID = 7
	f.AlgorithmID = 132
	entry.lastObserved = time.Now().Add(-2 * time.Second)
	if _, ok := m.bind(f); ok {
		t.Fatal("stale native authorization kept carrying media")
	}
}
func TestRFAdmissionReservesActualRoutesBeforeApprovingNativeGrant(t *testing.T) {
	c := newMediaCluster(t)
	var system SystemConfig
	if err := c.system.service.owned(&system); err != nil {
		t.Fatal(err)
	}
	for i := range system.Talkgroups {
		if system.Talkgroups[i].TGID == 1001 {
			system.Talkgroups[i].StartPolicy = routing.AllStart
		}
	}
	if err := c.system.service.Store.SaveConfig(storeDoc(t, system)); err != nil {
		t.Fatal(err)
	}
	integrationSyncAll(t, c.integrationCluster)
	c.affiliations(t, true)
	source := c.site.service
	record := admissionRecord()
	native := &admissionNative{record: record}
	m := &rfAdmissionManager{s: source, entries: map[string]*admittedRF{}, aliases: map[string]string{}, seen: map[string]time.Time{}}
	source.rfAdmissions = m
	t.Cleanup(func() {
		for _, entry := range m.snapshot() {
			m.stop(entry)
		}
		m.wg.Wait()
	})
	m.observe(native, record)
	mediaWait(t, 3*time.Second, native.approved, "native approval after remote reservation")
	if len(c.zone.service.broker.router.Active()) != 1 || len(source.broker.router.Active()) != 1 || c.natives["site-2"].count() == 0 {
		t.Fatal("native was approved without the remote and local call routes")
	}
	record.State = "committed"
	record.GrantEpoch = "17"
	record.ChannelNo = 81
	m.observe(native, record)
	source.codecFrame(admissionFrame("first-fne-stream", 1))
	first := sipActualFrames(t, c, "site-2", 1)[0]
	if first.Sequence != 1 || first.SourceEpoch != 1 {
		t.Fatal("wrong first source epoch/sequence", first)
	}
	end := admissionFrame("first-fne-stream", 2)
	end.End = true
	end.PCM, end.Encoded, end.P25 = nil, nil, nil
	source.codecFrame(end)
	if len(c.zone.service.broker.router.Active()) != 1 {
		t.Fatal("burst end retired native hang allocation")
	}
	source.codecFrame(admissionFrame("second-fne-stream", 1))
	second := sipActualFrames(t, c, "site-2", 1)[0]
	if second.SourceEpoch != 2 || second.CallID == first.CallID || second.Sequence != first.Sequence+2 {
		t.Fatal("new burst reused native media owner or lost its discontinuity", first, second)
	}
	record.State = "ended"
	m.observe(native, record)
	mediaWait(t, time.Second, func() bool {
		return len(c.zone.service.broker.router.Active()) == 0 && len(source.broker.router.Active()) == 0
	}, "native allocation route cleanup")
}

func TestRFAdmissionCancelledApprovalCannotCommitAfterItsRoutesWereReleased(t *testing.T) {
	c := newMediaCluster(t)
	c.affiliations(t, true)
	source := c.site.service
	release := make(chan struct{})
	entered := make(chan struct{})
	native := &admissionNative{record: admissionRecord(), approveEntered: entered, approveWait: release}
	m := &rfAdmissionManager{s: source, entries: map[string]*admittedRF{}, aliases: map[string]string{}, seen: map[string]time.Time{}}
	source.rfAdmissions = m
	t.Cleanup(func() {
		select {
		case <-release:
		default:
			close(release)
		}
		for _, entry := range m.snapshot() {
			m.stop(entry)
		}
		m.wg.Wait()
	})
	m.observe(native, native.record)
	select {
	case <-entered:
	case <-time.After(3 * time.Second):
		t.Fatal("native approval did not start")
	}
	entries := m.snapshot()
	if len(entries) != 1 {
		t.Fatal("missing pending reservation")
	}
	m.stop(entries[0])
	close(release)
	m.wg.Wait()
	native.mu.Lock()
	state := native.record.State
	native.mu.Unlock()
	if state != "rejected" {
		t.Fatal("late native approval was not conditionally cancelled", state)
	}
	mediaWait(t, time.Second, func() bool {
		return len(source.broker.router.Active()) == 0 && len(c.zone.service.broker.router.Active()) == 0
	}, "cancelled approval reservations released")
	source.broker.mu.Lock()
	calls := len(source.broker.rfCalls)
	source.broker.mu.Unlock()
	if calls != 0 {
		t.Fatal("cancelled approval revived RF stream")
	}
}

func TestRFAdmissionKeepsCommittedLocalCallDuringZoneLoss(t *testing.T) {
	c := newMediaCluster(t)
	c.affiliations(t, true)
	source := c.site.service
	native := &admissionNative{record: admissionRecord()}
	m := &rfAdmissionManager{s: source, entries: map[string]*admittedRF{}, aliases: map[string]string{}, seen: map[string]time.Time{}}
	source.rfAdmissions = m
	t.Cleanup(func() {
		for _, entry := range m.snapshot() {
			m.stop(entry)
		}
		m.wg.Wait()
	})
	m.observe(native, native.record)
	mediaWait(t, 3*time.Second, native.approved, "approved RF route")
	record := admissionRecord()
	record.State = "committed"
	record.GrantEpoch = "17"
	record.ChannelNo = 81
	m.observe(native, record)
	source.codecFrame(admissionFrame("local-during-outage", 1))
	select {
	case <-c.codecs["site-2"].frames:
	case <-time.After(time.Second):
		t.Fatal("pre-outage RF media missing")
	}
	entries := m.snapshot()
	if len(entries) != 1 {
		t.Fatal("missing native allocation")
	}
	entry := entries[0]
	c.zone.service.peers.Close()
	c.zone.server.Close()
	m.isolate(entry)
	m.mu.Lock()
	survived := !entry.ended && entry.localOnly
	entry.lastRenew = time.Time{}
	m.mu.Unlock()
	if !survived {
		t.Fatal("Zone loss terminated the native allocation")
	}
	m.observe(native, record)
	if len(source.broker.router.Active()) != 1 {
		t.Fatal("Zone loss released local floor")
	}
	native.mu.Lock()
	cancelled := slices.Contains(native.decisions, false)
	native.mu.Unlock()
	if cancelled {
		t.Fatal("Zone loss cancelled a committed native call")
	}
	f, accepted := m.bind(admissionFrame("local-during-outage", 2))
	if !accepted || f.Sequence != 2 {
		t.Fatal("local RF stopped after Zone loss")
	}
	source.broker.mu.Lock()
	remote := entry.stream.remoteURL
	source.broker.mu.Unlock()
	if remote != "" {
		t.Fatal("stale Zone route retained")
	}
	record.State = "ended"
	m.observe(native, record)
	if len(source.broker.router.Active()) != 0 {
		t.Fatal("local allocation leaked after native end")
	}
}
func TestRFAdmissionUsesAppliedNativePolicyDuringConfigurationChange(t *testing.T) {
	m, _ := boundManager()
	m.s.radioPolicy.Store(&radio.System{Talkgroups: []radio.Talkgroup{{TGID: 1001, AdmissionMode: "clear", TransmitMode: "clear", Enabled: true, StartPolicy: routing.FASTStart}}})
	if m.allStart(1001) || m.hasAllStart() {
		t.Fatal("desired System change overrode running native policy")
	}
	m.s.radioPolicy.Store(&radio.System{Talkgroups: []radio.Talkgroup{{TGID: 1001, AdmissionMode: "clear", TransmitMode: "clear", Enabled: true, StartPolicy: routing.AllStart}}})
	if !m.allStart(1001) || !m.hasAllStart() {
		t.Fatal("applied native AllStart policy was ignored")
	}
}

func TestRFAdmissionDoesNotWaitForManagement(t *testing.T) {
	for _, allStart := range []bool{false, true} {
		mode := "FASTStart"
		if allStart {
			mode = "AllStart"
		}
		for _, blocked := range []string{"source database", "Zone database", "report"} {
			t.Run(mode+"/"+blocked, func(t *testing.T) {
				c := newMediaCluster(t)
				if allStart {
					var system SystemConfig
					if err := c.system.service.owned(&system); err != nil {
						t.Fatal(err)
					}
					for i := range system.Talkgroups {
						if system.Talkgroups[i].TGID == 1001 {
							system.Talkgroups[i].StartPolicy = routing.AllStart
						}
					}
					if err := c.system.service.Store.SaveConfig(storeDoc(t, system)); err != nil {
						t.Fatal(err)
					}
					integrationSyncAll(t, c.integrationCluster)
				}
				c.affiliations(t, true)
				source := c.site.service
				view := *c.zone.service.view()
				view.Reports = slices.Clone(view.Reports)
				for i := range view.Reports {
					if view.Reports[i].ID == "site-1" {
						view.Reports[i].ReceivedAt = time.Now().Add(-time.Hour)
					}
				}
				c.zone.service.directory.value.Store(&view)
				// Discovery is already established. Stop its periodic polling so
				// this fixture changes no shared HTTP transport while in use.
				source.mediaClient.mu.Lock()
				stopDiscovery := source.mediaClient.cancel
				source.mediaClient.mu.Unlock()
				stopDiscovery()
				source.mediaClient.wg.Wait()
				if blocked == "source database" || blocked == "Zone database" {
					store := source.Store
					if blocked == "Zone database" {
						store = c.zone.service.Store
					}
					conn, err := store.db.Conn(context.Background())
					if err != nil {
						t.Fatal(err)
					}
					defer conn.Close()
				} else {
					transport := source.client.Transport
					source.client.Transport = integrationRoundTripFunc(func(r *http.Request) (*http.Response, error) {
						if r.URL.Path == "/internal/report" {
							<-r.Context().Done()
							return nil, r.Context().Err()
						}
						return transport.RoundTrip(r)
					})
				}
				runtime := c.radios["site-1"]
				runtime.mu.Lock()
				runtime.status.Affiliations[0].At = time.Now().UTC()
				runtime.mu.Unlock()
				if allStart {
					record := admissionRecord()
					native := &admissionNative{record: record}
					m := &rfAdmissionManager{s: source, entries: map[string]*admittedRF{}, aliases: map[string]string{}, seen: map[string]time.Time{}}
					source.rfAdmissions = m
					t.Cleanup(func() {
						for _, entry := range m.snapshot() {
							m.stop(entry)
						}
						m.wg.Wait()
					})
					m.observe(native, record)
					mediaWait(t, 3*time.Second, native.approved, "RF approval independent of management")
					record.State, record.GrantEpoch, record.ChannelNo = "committed", "17", 81
					m.observe(native, record)
				}
				for i := uint64(1); i <= 9; i++ {
					source.codecFrame(admissionFrame("management-independent", i))
					if i < 9 {
						time.Sleep(20 * time.Millisecond)
					}
				}
				sipActualFrames(t, c, "site-2", 9)
				end := admissionFrame("management-independent", 10)
				end.End, end.Encoded, end.P25 = true, nil, nil
				source.codecFrame(end)
			})
		}
	}
}
