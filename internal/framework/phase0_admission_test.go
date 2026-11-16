package framework

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"runtime"
	"slices"
	"strconv"
	"strings"
	"testing"
	"time"

	"open25.local/open25-fne/internal/dispatch"
	"open25.local/open25-fne/internal/radio"
	"open25.local/open25-fne/internal/routing"
)

func TestPhase0AdmissionTimingNeverBlocks(t *testing.T) {
	s := &Service{}
	if !s.admissionClock().IsZero() {
		t.Fatal("disabled diagnostics read the clock")
	}
	s.admissionElapsed(time.Time{}, "disabled", "", false)
	s.admissionTimings = make(chan admissionTiming, 1)
	for range 100 {
		s.admissionElapsed(s.admissionClock(), "stage", "public-call", true)
	}
	if len(s.admissionTimings) != 1 || s.admissionTimingDrops.Load() != 99 {
		t.Fatal("timing sink did not bound/drop observations")
	}
}

func phase0RFRequest(s *Service, id string) routing.Request {
	return routing.Request{CallRequest: dispatch.CallRequest{ID: id, RadioID: 10001, Talkgroups: []int{1001}}, SourceKind: "rf", OriginSite: s.Config().InstanceID}
}

func TestPhase0TwoIndependentZones(t *testing.T) {
	a := newMediaCluster(t)
	b := newMediaClusterFromIntegration(t, integrationNewZoneCluster(t, "zone-2", 2, nil))
	a.affiliations(t, true)
	b.affiliations(t, true)
	if a.zone.service.view().System.ID != b.zone.service.view().System.ID || a.zone.service.Config().StateDir == b.zone.service.Config().StateDir || a.zone.server.URL == b.zone.server.URL {
		t.Fatal("Zones do not have independent runtime state")
	}
	if a.zone.service.view().Zone.RFSSID != 1 || b.zone.service.view().Zone.RFSSID != 2 {
		t.Fatal("RFSS programming was shared")
	}
	calls := []*voiceCall{}
	for _, c := range []*mediaCluster{a, b} {
		s := c.site.service
		q, err := s.observeRFSource(phase0RFRequest(s, "phase0-"+c.zone.service.Config().InstanceID))
		if err != nil {
			t.Fatal(err)
		}
		endpoint, err := s.mediaClient.endpointAt(func() uint64 { _, g := s.mediaClient.Link(); return g }())
		if err != nil {
			t.Fatal(err)
		}
		ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
		call, err := s.voice.Load().begin(ctx, endpoint, q, false)
		cancel()
		if err != nil {
			t.Fatal(err)
		}
		if c.natives[c.otherSite.service.Config().InstanceID].count() != 1 {
			t.Fatal("own Zone did not grant its receiving Site")
		}
		calls = append(calls, call)
	}
	// Both Zones hold the same talkgroup at once: no hidden global floor.
	for _, c := range []*mediaCluster{a, b} {
		if len(c.zone.service.broker.router.Active()) != 1 {
			t.Fatal("independent Zone floor missing")
		}
	}
	for i, c := range []*mediaCluster{a, b} {
		if err := calls[i].Write(admissionFrame(calls[i].grant.ID, 1)); err != nil {
			t.Fatal(err)
		}
		frames := sipActualFrames(t, c, c.otherSite.service.Config().InstanceID, 1)
		if frames[0].SourceID != 10001 {
			t.Fatal("wrong Zone source")
		}
		ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
		err := calls[i].End(ctx)
		cancel()
		if err != nil {
			t.Fatal(err)
		}
		mediaWait(t, time.Second, func() bool { return len(c.zone.service.broker.router.Active()) == 0 }, "Zone floor retirement")
	}

	// Enroll the other Zone in this test only, proving rejection is a role
	// boundary as well as the absence of credentials. Production stays unchanged.
	ac, err := readCredentials(a.zone.service.Config().CredentialFile)
	if err != nil {
		t.Fatal(err)
	}
	bc, err := readCredentials(b.zone.service.Config().CredentialFile)
	if err != nil {
		t.Fatal(err)
	}
	if ac.OutboundToken == bc.OutboundToken {
		t.Fatal("Zone credentials are shared")
	}
	bc.Peers[ac.OutboundID] = PeerCredential{Role: RoleZone, InstanceID: a.zone.service.Config().InstanceID, Token: ac.OutboundToken}
	if err := WritePrivateJSON(b.zone.service.Config().CredentialFile, bc); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	if _, err := a.zone.service.voice.Load().discover(ctx, b.zone.server.URL, nil); err == nil || voiceRoles(RoleZone, RoleZone) {
		t.Fatal("production inter-Zone media was enabled")
	}
	before := b.zone.service.rfAuthority.value.Load().version
	update := rfUpdateFromSource(a.radios[a.site.service.Config().InstanceID].SourceState())
	if err := a.site.service.requestAt(ctx, b.zone.server.URL, "POST", "/internal/rf-authority", update, nil, nil); err == nil || b.zone.service.rfAuthority.value.Load().version != before {
		t.Fatal("foreign Site modified Zone ownership")
	}
	// Cached identical RID observations may independently exist in each Zone;
	// this fixture deliberately supplies no global mobility arbitration.
	for _, c := range []*mediaCluster{a, b} {
		owner, ok := c.zone.service.rfAuthority.lookup(10001)
		if !ok || owner.SiteID != c.site.service.Config().InstanceID {
			t.Fatal("one Zone overwrote another's ownership")
		}
	}
}

func TestPhase0SharedSystemCatalogBlocksSecondZone(t *testing.T) {
	first := integrationNewCluster(t)
	integrationSyncAll(t, first)
	second := integrationNewZoneCluster(t, "zone-2", 2, first.system)
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	// Characterize the incompatible singular Zone catalog. Do not repair or
	// filter that production response just to make a multi-Zone fixture pass.
	if err := second.zone.service.SyncOnce(ctx); err != nil {
		t.Fatal(err)
	}
	err := second.zone.service.SyncOnce(ctx)
	if err == nil || !strings.Contains(err.Error(), "different logical Zone") {
		t.Fatalf("singular catalog behavior changed: %v", err)
	}
}

func TestPhase0FASTStartGoRouteSurvivesUnavailableZone(t *testing.T) {
	c := newMediaCluster(t)
	c.affiliations(t, true)
	s := c.site.service
	s.mediaClient.Close() // no automatic discovery recovery during this test
	q := phase0RFRequest(s, "phase0-isolated-fast")
	stream := &rfStream{request: q, lastFrame: time.Now()}
	s.broker.mu.Lock()
	s.broker.rfCalls[q.ID] = stream
	s.broker.mu.Unlock()
	s.beginRFStream(stream)
	if !stream.ready || stream.local.ID == "" || stream.remoteCall != nil {
		t.Fatal("local Go allocation depended on Zone")
	}
	s.endRFStream(stream)
	if len(s.broker.router.Active()) != 0 {
		t.Fatal("local allocation leaked")
	}
}

type phase0PollRadio struct {
	*operationRadio
	client *radio.NativeClient
}

func (r *phase0PollRadio) ControlClient() (*radio.NativeClient, error) { return r.client, nil }

// Actual Go poll/HTTP/SIP/native-client paths; only the final native endpoint
// and modem are doubles. Approval is not an actual native/OTA grant.
func phase0StartPolling(t *testing.T, c *mediaCluster) (*rfAdmissionManager, *admissionNative) {
	t.Helper()
	s := c.site.service
	n := &admissionNative{}
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var reply map[string]json.RawMessage
		var err error
		switch r.URL.Path {
		case "/auth":
			reply = map[string]json.RawMessage{"status": json.RawMessage("200"), "token": json.RawMessage(`"phase0-native-token"`)}
		case "/p25/rf-admission/pending":
			reply, err = n.Get(r.Context(), r.URL.Path)
		case "/p25/rf-admission/decision":
			var body map[string]any
			if err = json.NewDecoder(r.Body).Decode(&body); err == nil {
				reply, err = n.Put(r.Context(), r.URL.Path, body)
			}
		default:
			t.Errorf("unexpected native request %s", r.URL.Path)
			http.Error(w, "unexpected", 404)
			return
		}
		if err != nil {
			http.Error(w, "fixture error", 500)
			return
		}
		reply["status"] = json.RawMessage("200")
		_ = json.NewEncoder(w).Encode(reply)
	}))
	t.Cleanup(server.Close)
	client, err := radio.NewNativeClient(server.Listener.Addr().String(), "phase0-native-password")
	if err != nil {
		t.Fatal(err)
	}
	s.radio = &phase0PollRadio{c.radios[s.Config().InstanceID], client}
	m := &rfAdmissionManager{s: s, entries: map[string]*admittedRF{}, aliases: map[string]string{}, seen: map[string]time.Time{}}
	s.rfAdmissions = m
	s.operationWG.Add(1)
	go func() { defer s.operationWG.Done(); m.run() }()
	// Cleanup before closing the native listener or the service state.
	t.Cleanup(func() { s.operationCancel(); s.operationWG.Wait() })
	return m, n
}

type phase0Distribution struct {
	Samples  int     `json:"samples"`
	MedianMS float64 `json:"median_ms"`
	P95MS    float64 `json:"p95_ms"`
	WorstMS  float64 `json:"worst_ms"`
}

func phase0DistributionOf(v []time.Duration) phase0Distribution {
	v = slices.Clone(v)
	slices.Sort(v)
	median := float64(v[(len(v)-1)/2])
	if len(v)%2 == 0 {
		median = (median + float64(v[len(v)/2])) / 2
	}
	return phase0Distribution{len(v), median / float64(time.Millisecond), float64(v[(95*len(v)+99)/100-1]) / float64(time.Millisecond), float64(v[len(v)-1]) / float64(time.Millisecond)}
}

func TestPhase0AdmissionBaseline(t *testing.T) {
	samples := 3
	if raw := os.Getenv("OPEN25_PHASE0_SAMPLES"); raw != "" {
		n, err := strconv.Atoi(raw)
		if err != nil || n < 1 || n > 200 {
			t.Fatal("OPEN25_PHASE0_SAMPLES must be 1..200")
		}
		samples = n
	}
	for _, scenario := range []phase0Scenario{
		{routing.FASTStart, routing.FASTStart, false},
		{routing.AllStart, routing.AllStart, false},
		{"all_start_changed_fact", routing.AllStart, true},
	} {
		t.Run(scenario.name, func(t *testing.T) {
			measurements := map[string][]time.Duration{}
			// Ended SIP dialogs occupy the existing 64-session table for 64*T1.
			// A Zone uses two dialogs per attempt. Fresh warmed batches keep this
			// setup baseline below that limit without tuning production capacity.
			batches := (samples + 19) / 20
			for offset := 0; offset < samples; offset += 20 {
				count := min(20, samples-offset)
				if !t.Run(fmt.Sprintf("batch_%d", offset/20+1), func(t *testing.T) {
					for stage, values := range phase0BaselineBatch(t, scenario, count) {
						measurements[stage] = append(measurements[stage], values...)
					}
				}) {
					return
				}
			}
			mode := scenario.mode
			results := map[string]phase0Distribution{}
			required := []string{"zone/zone-1/rf_source_authority", "zone/zone-1/site_discovery", "zone/zone-1/site_sip_admission", "zone/zone-1/voice_broker_admission", "site/site-2/voice_broker_admission"}
			if mode == routing.AllStart {
				required = append(required, "pending_to_go_approval", "source/rf_reserve", "source/rf_zone_admission", "source/rf_local_admission", "source/rf_native_approval_http")
			} else {
				required = append(required, "post_native_go_forwarding", "site/site-1/rf_fast_local_admission", "site/site-1/rf_fast_zone_admission")
			}
			for _, stage := range required {
				if len(measurements[stage]) != samples {
					t.Fatalf("missing complete stage %s", stage)
				}
			}
			for stage, values := range measurements {
				if len(values) != samples {
					t.Fatalf("stage %s: %d samples, want %d", stage, len(values), samples)
				}
				results[stage] = phase0DistributionOf(values)
			}
			report := map[string]any{"scenario": scenario.name, "mode": mode, "warmup_samples_excluded": batches, "batch_size_max": 20, "scope": "warm loopback Go admission; simulated native boundary; not native request-to-grant or radio permit latency", "go": runtime.Version(), "os": runtime.GOOS, "arch": runtime.GOARCH, "stages": results}
			raw, _ := json.Marshal(report)
			t.Logf("PHASE0_BASELINE %s", raw)
		})
	}
}

type phase0Scenario struct {
	name, mode  string
	changedFact bool
}

func phase0BaselineBatch(t *testing.T, scenario phase0Scenario, samples int) map[string][]time.Duration {
	t.Helper()
	mode := scenario.mode
	c := newMediaCluster(t)
	var system SystemConfig
	if err := c.system.service.owned(&system); err != nil {
		t.Fatal(err)
	}
	for i := range system.Talkgroups {
		if system.Talkgroups[i].TGID == 1001 {
			system.Talkgroups[i].StartPolicy = mode
		}
	}
	if err := c.system.service.Store.SaveConfig(storeDoc(t, system)); err != nil {
		t.Fatal(err)
	}
	integrationSyncAll(t, c.integrationCluster)
	c.affiliations(t, true)
	s := c.site.service
	// No traffic begins before these immutable sink assignments.
	for _, node := range []*integrationService{c.site, c.zone, c.otherSite} {
		node.service.admissionTimings = make(chan admissionTiming, 128)
	}
	var m *rfAdmissionManager
	var native *admissionNative
	if mode == routing.AllStart {
		m, native = phase0StartPolling(t, c)
	}
	measurements := map[string][]time.Duration{}
	for i := -1; i < samples; i++ {
		previousVersion := c.zone.service.rfAuthority.value.Load().version
		if scenario.changedFact {
			r := c.radios[s.Config().InstanceID]
			r.mu.Lock()
			r.status.Affiliations[0].At = time.Now().UTC()
			r.mu.Unlock()
		}
		started := time.Now()
		if mode == routing.AllStart {
			record := admissionRecord()
			record.ID = fmt.Sprintf("%032x", i+1)
			native.mu.Lock()
			native.record = record
			native.mu.Unlock()
			// Let the actual 50ms manager poll discover the request. Never
			// manufacture approval or bypass reserve in this measurement.
			deadline := time.NewTimer(4 * time.Second)
			complete := false
			for !complete {
				select {
				case observation := <-s.admissionTimings:
					if !observation.Succeeded {
						t.Fatalf("failed stage %s", observation.Stage)
					}
					measurements["source/"+observation.Stage] = append(measurements["source/"+observation.Stage], observation.Elapsed)
					complete = observation.Stage == "rf_reserve"
				case <-deadline.C:
					t.Fatal("admission did not finish")
				}
			}
			deadline.Stop()
			measurements["pending_to_go_approval"] = append(measurements["pending_to_go_approval"], time.Since(started))
			if len(c.zone.service.broker.router.Active()) != 1 || c.natives[c.otherSite.service.Config().InstanceID].count() == 0 {
				t.Fatal("approval preceded reservations")
			}
			record.State = "ended"
			m.observe(native, record)
			m.mu.Lock()
			delete(m.entries, record.ID)
			m.mu.Unlock()
		} else {
			q := phase0RFRequest(s, fmt.Sprintf("phase0-fast-%d", i))
			stream := &rfStream{request: q, lastFrame: time.Now()}
			s.broker.mu.Lock()
			s.broker.rfCalls[q.ID] = stream
			s.broker.mu.Unlock()
			s.beginRFStream(stream)
			measurements["post_native_go_forwarding"] = append(measurements["post_native_go_forwarding"], time.Since(started))
			if !stream.ready || stream.remoteCall == nil {
				t.Fatal("FASTStart forwarding failed")
			}
			s.endRFStream(stream)
		}
		for _, node := range []*integrationService{c.site, c.zone, c.otherSite} {
			for len(node.service.admissionTimings) > 0 {
				o := <-node.service.admissionTimings
				if !o.Succeeded {
					t.Fatalf("failed stage %s", o.Stage)
				}
				key := string(node.service.Config().Role) + "/" + node.service.Config().InstanceID + "/" + o.Stage
				measurements[key] = append(measurements[key], o.Elapsed)
			}
			if node.service.admissionTimingDrops.Load() != 0 {
				t.Fatal("baseline dropped samples")
			}
		}
		mediaWait(t, time.Second, func() bool {
			return len(c.zone.service.broker.router.Active()) == 0 && len(s.broker.router.Active()) == 0 && len(c.otherSite.service.broker.router.Active()) == 0
		}, "baseline allocation retirement")
		version := c.zone.service.rfAuthority.value.Load().version
		if (scenario.changedFact && version != previousVersion+1) || (!scenario.changedFact && version != previousVersion) {
			t.Fatal("baseline did not exercise the declared durable/unchanged ownership case")
		}
		if i == -1 {
			measurements = map[string][]time.Duration{}
		} // unreported warmup
	}
	return measurements
}
