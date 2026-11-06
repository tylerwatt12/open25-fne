package framework

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"

	"open25.local/open25-fne/internal/activity"
	"open25.local/open25-fne/internal/dispatch"
)

func automationFixtureSystem() SystemConfig {
	system := userFixtureSystem()
	system.Radios = append(system.Radios, Radio{RID: 10020, Alias: "Announcements", Kind: "automation", Enabled: true})
	return system
}

func automationFixture() Automation {
	return Automation{ID: "announcements", Name: "Announcements", RadioID: 10020, Enabled: true, Talkgroups: []int{1001}, Actions: []string{"wav", "tts"}}
}

func TestAutomationCredentialsAreSeparateScopedAndShownOnlyOnce(t *testing.T) {
	s := storeOpen(t, RoleSystem)
	a := automationFixture()
	token, err := s.SaveAutomation(a.ID, a, true)
	if err != nil || len(token) != 64 {
		t.Fatalf("credential creation: %d %v", len(token), err)
	}
	records, _ := s.automationRecords()
	hash := sha256.Sum256([]byte(token))
	if len(records) != 1 || !bytes.Equal(records[0].TokenHash, hash[:]) {
		t.Fatal("credential was not stored as a SHA-256 verifier")
	}
	public, _ := s.Automations()
	raw, _ := json.Marshal(public)
	private, _ := json.Marshal(records)
	if bytes.Contains(raw, []byte("token")) || bytes.Contains(private, []byte(token)) {
		t.Fatal("public identity or private storage exposed the raw credential")
	}
	if users, _ := s.Users(); len(users) != 0 {
		t.Fatal("an automation created a human account")
	}
	a.Name = "Renamed source"
	if echoed, err := s.SaveAutomation(a.ID, a, false); err != nil || echoed != "" {
		t.Fatal("ordinary edits returned an API credential", err)
	}
	rotated, err := s.RotateAutomationToken(a.ID)
	if err != nil || rotated == token || len(rotated) != 64 {
		t.Fatal("credential rotation failed", err)
	}
	svc := &Service{Store: s, config: FileConfig{Role: RoleSystem}}
	if err = s.SaveConfig(storeDoc(t, automationFixtureSystem())); err != nil {
		t.Fatal(err)
	}
	svc.refreshDirectory()
	auth := func(secret string, cookie bool) bool {
		r := httptest.NewRequest("GET", "http://example.test/api/automation/jobs/test", nil)
		r.Header.Set("Authorization", "Bearer "+secret)
		if cookie {
			r.Header.Set("Cookie", "human-session=anything")
		}
		_, err := svc.automationBearer(r)
		return err == nil
	}
	if auth(token, false) || auth(hex.EncodeToString(hash[:]), false) || auth(rotated, true) || !auth(rotated, false) {
		t.Fatal("rotated token, verifier or cookie boundary was not enforced")
	}
	if _, _, err = s.LoginUser(a.ID); err == nil {
		t.Fatal("automation ID became a human sign-in")
	}
}

func TestAutomationConfigurationPreservesEnteredSettings(t *testing.T) {
	s := storeOpen(t, RoleSystem)
	a := automationFixture()
	a.RadioID = 10001
	a.Talkgroups = []int{999, 999}
	a.Actions = []string{"inhibit", "wav", "wav"}
	if _, err := s.SaveAutomation(a.ID, a, true); err != nil {
		t.Fatal(err)
	}
	saved, err := s.Automations()
	if err != nil || len(saved) != 1 || !reflect.DeepEqual(saved[0], a) {
		t.Fatal("entered automation changed", err)
	}
	a.ID = "second-source"
	if _, err = s.SaveAutomation(a.ID, a, true); err != nil {
		t.Fatal("duplicate configured radio ID was rejected", err)
	}
}

func TestAutomationCatalogScopesResourcesAndProofWithoutHumanAuthority(t *testing.T) {
	a := automationFixture()
	a.Talkgroups = []int{1001, 1002}
	hash := sha256.Sum256([]byte("credential"))
	record := automationRecord{Automation: a, TokenHash: hash[:]}
	system := automationFixtureSystem()
	system.Talkgroups = system.Talkgroups[:1]
	scoped := eligibleAutomations([]automationRecord{record}, system)
	if len(scoped) != 1 || !reflect.DeepEqual(scoped[0].Automation.Talkgroups, []int{1001}) || automationProof(scoped[0], "wav") != automationProof(record, "wav") {
		t.Fatal("scoped Site copies lost their identity proof or exposed another resource")
	}
	if automationProof(record, "wav") == automationProof(record, "tts") {
		t.Fatal("WAV identity proof could be reused as a TTS authorization")
	}
	s := &Service{}
	s.directory.value.Store(&directorySnapshot{System: system, Automations: map[string]automationRecord{a.ID: scoped[0]}, Users: map[string]userRecord{}})
	q := dispatch.CallRequest{AutomationID: a.ID, AutomationAction: "wav", RadioID: a.RadioID}
	if !s.automationCallAllowed(q, automationProof(record, "wav"), []int{1001}) || s.automationCallAllowed(q, automationProof(record, "wav"), []int{1002}) {
		t.Fatal("automation escaped scoped talkgroup authorization")
	}
	q.Secure = true
	if s.automationCallAllowed(q, automationProof(record, "wav"), []int{1001}) {
		t.Fatal("clear automation gained encryption permission")
	}
	if _, err := s.operatorActor(OperatorActor{AutomationID: a.ID, AutomationAction: "wav", RadioID: a.RadioID}); err == nil {
		t.Fatal("automation identity became a console operator")
	}
}

func seedMediaAutomation(t *testing.T, c *mediaCluster) (Automation, string) {
	t.Helper()
	var system SystemConfig
	if err := c.system.service.owned(&system); err != nil {
		t.Fatal(err)
	}
	system.Radios = append(system.Radios, Radio{RID: 10020, Alias: "Announcements", Kind: "automation", Enabled: true})
	if err := c.system.service.Store.SaveConfig(storeDoc(t, system)); err != nil {
		t.Fatal(err)
	}
	a := automationFixture()
	token, err := c.system.service.Store.SaveAutomation(a.ID, a, true)
	if err != nil {
		t.Fatal(err)
	}
	integrationSyncAll(t, c.integrationCluster)
	return a, token
}

func machineWAVRequest(t *testing.T, base, token, id string, duration time.Duration, expiry time.Time) (int, []byte) {
	t.Helper()
	wav := speechTestWAV(8000, int(8000*duration/time.Second), 675)
	r, err := http.NewRequest(http.MethodPost, base+"/api/automation/wav?talkgroup_id=1001", bytes.NewReader(wav))
	if err != nil {
		t.Fatal(err)
	}
	r.Header.Set("Authorization", "Bearer "+token)
	r.Header.Set("Content-Type", "audio/wav")
	r.Header.Set("X-Request-ID", id)
	r.Header.Set("X-Expires-At", expiry.Format(time.RFC3339Nano))
	response, err := http.DefaultClient.Do(r)
	if err != nil {
		t.Fatal(err)
	}
	defer response.Body.Close()
	raw, _ := io.ReadAll(response.Body)
	return response.StatusCode, raw
}

func machineJob(t *testing.T, base, token, id string) AutomationJob {
	t.Helper()
	status, raw := integrationHTTP(t, http.DefaultClient, http.MethodGet, base+"/api/automation/jobs/"+id, nil, map[string]string{"Authorization": "Bearer " + token})
	if status != 200 {
		t.Fatalf("job read: %d %s", status, raw)
	}
	var result struct{ Job AutomationJob }
	if err := json.Unmarshal(raw, &result); err != nil {
		t.Fatal(err)
	}
	return result.Job
}

func waitMachineJob(t *testing.T, base, token, id string) AutomationJob {
	t.Helper()
	var job AutomationJob
	mediaWait(t, 5*time.Second, func() bool {
		job = machineJob(t, base, token, id)
		return job.State != "accepted" && job.State != "running"
	}, "machine transmission completion")
	return job
}

func TestAutomationWAVUsesRealServiceRoutingWithoutCreatingRFInterest(t *testing.T) {
	c := newMediaCluster(t)
	requireMediaPacketCodec(t, c)
	a, token := seedMediaAutomation(t, c)
	expires := time.Now().Add(30 * time.Second)
	status, raw := machineWAVRequest(t, c.dispatch.web.URL, token, "first", 200*time.Millisecond, expires)
	if status != 202 {
		t.Fatalf("machine admission: %d %s", status, raw)
	}
	job := waitMachineJob(t, c.dispatch.web.URL, token, "first")
	if job.State != "completed" || job.Frames != 10 || job.AutomationID != a.ID || job.RadioID != a.RadioID {
		t.Fatalf("machine did not complete through normal router: %#v", job)
	}
	for id, native := range c.natives {
		if native.count() != 0 {
			t.Fatalf("IP-only machine audio allocated RF at %s", id)
		}
	}
	if status, raw = machineWAVRequest(t, c.dispatch.web.URL, token, "first", 200*time.Millisecond, expires); status != 200 {
		t.Fatalf("idempotent retry: %d %s", status, raw)
	}
	if status, _ = machineWAVRequest(t, c.dispatch.web.URL, token, "first", 220*time.Millisecond, expires); status != 409 {
		t.Fatal("same request ID accepted different audio")
	}
	if err := c.zone.service.activity.Flush(context.Background()); err != nil {
		t.Fatal(err)
	}
	events, _ := c.zone.service.activity.Events(activity.Query{RadioID: a.RadioID})
	found := false
	for _, event := range events {
		if event.Type == "call_start" && event.SourceKind == "automation" {
			found = true
		}
		if event.Type == "registration" || event.Type == "affiliation" {
			t.Fatal("machine traffic manufactured subscriber presence")
		}
	}
	if !found {
		t.Fatal("machine source identity was lost at Zone")
	}
	// The identical path may use native legs only after actual subscriber
	// affiliations are supplied by the bench Site runtime.
	c.affiliations(t, true)
	if status, raw = machineWAVRequest(t, c.dispatch.web.URL, token, "with-subscribers", 200*time.Millisecond, expires); status != 202 {
		t.Fatalf("interested Site admission: %d %s", status, raw)
	}
	job = waitMachineJob(t, c.dispatch.web.URL, token, "with-subscribers")
	if job.State != "completed" {
		t.Fatalf("interested Site call: %#v", job)
	}
	// SIP acknowledges BYE before the receiver finishes its native tail drain.
	mediaWait(t, 2*time.Second, func() bool {
		return len(c.zone.service.broker.router.Active()) == 0 && len(c.site.service.broker.router.Active()) == 0 && len(c.otherSite.service.broker.router.Active()) == 0
	}, "machine receiving allocations released")
	for id, codec := range c.codecs {
		codec.mu.Lock()
		written := codec.written
		codec.mu.Unlock()
		// Ten PCM words plus eight actual native encoder tail words and
		// the explicit native End complete one18-word superframe.
		if written != 19 || c.natives[id].count() == 0 {
			t.Fatalf("Site %s received %d frames without its bounded allocation", id, written)
		}
	}
}

func TestAutomationMachineEndpointsRejectCookiesExpiredAndUnauthorizedRequests(t *testing.T) {
	c := newMediaCluster(t)
	_, token := seedMediaAutomation(t, c)
	base := c.dispatch.web.URL
	for _, headers := range []map[string]string{nil, {"Authorization": "Bearer wrong"}, {"Authorization": "Bearer " + token, "Cookie": "human-session=present"}} {
		if status, _ := integrationHTTP(t, http.DefaultClient, http.MethodGet, base+"/api/automation/jobs/unknown", nil, headers); status != 401 {
			t.Fatalf("credential boundary: %d", status)
		}
	}
	if status, _ := machineWAVRequest(t, base, token, "expired", 100*time.Millisecond, time.Now().Add(-time.Second)); status != 400 {
		t.Fatal("expired playback was admitted")
	}
	for _, body := range []map[string]any{
		{"text": "test", "talkgroups": []int{1002}, "request_id": "denied", "expires_at": time.Now().Add(time.Minute)},
		{"text": "test", "talkgroups": []int{1001}, "request_id": "injected", "expires_at": time.Now().Add(time.Minute), "radio_id": 10001},
	} {
		status, _ := integrationHTTP(t, http.DefaultClient, http.MethodPost, base+"/api/automation/tts", body, map[string]string{"Authorization": "Bearer " + token})
		if status != 400 && status != 403 {
			t.Fatalf("unauthorized machine input was accepted: %d", status)
		}
	}
	for _, native := range c.natives {
		if native.count() != 0 {
			t.Fatal("rejected machine request reached native grant")
		}
	}
}

func TestAutomationExpiryAndCancellationStopWithoutRetrying(t *testing.T) {
	c := newMediaCluster(t)
	requireMediaPacketCodec(t, c)
	_, token := seedMediaAutomation(t, c)
	base := c.dispatch.web.URL
	if status, raw := machineWAVRequest(t, base, token, "short-deadline", time.Second, time.Now().Add(250*time.Millisecond)); status != 202 {
		t.Fatalf("deadline admission: %d %s", status, raw)
	}
	if job := waitMachineJob(t, base, token, "short-deadline"); job.State != "expired" {
		t.Fatalf("deadline did not stop transmission: %#v", job)
	}
	mediaWait(t, time.Second, func() bool { return len(c.zone.service.broker.router.Active()) == 0 }, "expired automation floor released")
	if status, raw := machineWAVRequest(t, base, token, "cancel", time.Second, time.Now().Add(30*time.Second)); status != 202 {
		t.Fatalf("cancel admission: %d %s", status, raw)
	}
	mediaWait(t, time.Second, func() bool { return machineJob(t, base, token, "cancel").State == "running" }, "running cancellation target")
	if status, raw := integrationHTTP(t, http.DefaultClient, http.MethodDelete, base+"/api/automation/jobs/cancel", nil, map[string]string{"Authorization": "Bearer " + token}); status != 200 {
		t.Fatalf("cancel: %d %s", status, raw)
	}
	if job := waitMachineJob(t, base, token, "cancel"); job.State != "cancelled" {
		t.Fatalf("cancel did not stop transmission: %#v", job)
	}
	mediaWait(t, time.Second, func() bool { return len(c.zone.service.broker.router.Active()) == 0 }, "released automation floors")
}

func TestAutomationRestartKeepsIdempotenceWithoutReplayingAudio(t *testing.T) {
	s := storeOpen(t, RoleApplication)
	job := AutomationJob{ID: "pending", AutomationID: "announcements", State: "running", RequestedAt: time.Now(), ExpiresAt: time.Now().Add(time.Minute)}
	if err := s.WriteCache("automation-jobs", storeDoc(t, []automationJobRecord{{Job: job, Fingerprint: strings.Repeat("a", 64)}})); err != nil {
		t.Fatal(err)
	}
	service := &Service{Store: s}
	m, err := service.automationRuntimeForRequest()
	if err != nil {
		t.Fatal(err)
	}
	actual, ok := m.get(job.AutomationID, job.ID, false)
	if !ok || actual.State != "interrupted" || actual.EndedAt.IsZero() || len(m.cancels) != 0 {
		t.Fatalf("restart attempted to replay pending work: %#v", actual)
	}
}

func TestAutomationTTSJobUsesSameRoutingAndDoesNotRetainText(t *testing.T) {
	c := newMediaCluster(t)
	requireMediaPacketCodec(t, c)
	_, token := seedMediaAutomation(t, c)
	root := t.TempDir()
	wav, capture := filepath.Join(root, "speech.wav"), filepath.Join(root, "input.txt")
	if err := os.WriteFile(wav, speechTestWAV(8000, 1600, 675), 0600); err != nil {
		t.Fatal(err)
	}
	engine, _ := speechEngineFixture(t, fmt.Sprintf("out=''\nwhile [ $# -gt 0 ]; do\n if [ \"$1\" = '-w' ]; then shift; out=$1; fi\n shift\ndone\ncat > %q\ncp %q \"$out\"\n", capture, wav))
	c.dispatch.service.config.Media.SpeechBinary = engine.config.Media.SpeechBinary
	text := "Literal $(not-a-command) --help & test"
	status, raw := integrationHTTP(t, http.DefaultClient, http.MethodPost, c.dispatch.web.URL+"/api/automation/tts", map[string]any{"text": text, "talkgroups": []int{1001}, "request_id": "speech", "expires_at": time.Now().Add(30 * time.Second)}, map[string]string{"Authorization": "Bearer " + token})
	if status != 202 {
		t.Fatalf("TTS admission: %d %s", status, raw)
	}
	job := waitMachineJob(t, c.dispatch.web.URL, token, "speech")
	if job.State != "completed" || job.Frames != 10 || job.Action != "tts" {
		t.Fatalf("TTS did not use ordinary paced audio: %#v", job)
	}
	got, _ := os.ReadFile(capture)
	if string(got) != text {
		t.Fatal("machine text became shell arguments")
	}
	private, _, _ := c.dispatch.service.Store.ReadCache("automation-jobs")
	if bytes.Contains(private, []byte(text)) || bytes.Contains(private, []byte(token)) || bytes.Contains(private, []byte("RIFF")) {
		t.Fatal("durable job history retained speech, audio or credentials")
	}
	if err := c.dispatch.service.activity.Flush(context.Background()); err != nil {
		t.Fatal(err)
	}
	c.dispatch.service.uploadActivity(context.Background())
	events, err := c.zone.service.activity.Events(activity.Query{RadioID: job.RadioID})
	if err != nil {
		t.Fatal(err)
	}
	states := map[string]bool{}
	for _, event := range events {
		if event.Type != "automation_job" {
			continue
		}
		states[event.Outcome] = true
		raw, _ := json.Marshal(event)
		if event.SourceKind != "automation" || event.Details["job_id"] != "speech" || event.Details["automation_id"] != job.AutomationID || bytes.Contains(raw, []byte(text)) || bytes.Contains(raw, []byte(token)) || bytes.Contains(raw, []byte("RIFF")) {
			t.Fatalf("central job event lost its identity or exposed content: %s", raw)
		}
	}
	if !states["accepted"] || !states["running"] || !states["completed"] {
		t.Fatalf("central job history omitted stages: %#v", states)
	}
}

func TestAutomationRevocationStopsActivePlayback(t *testing.T) {
	c := newMediaCluster(t)
	requireMediaPacketCodec(t, c)
	a, token := seedMediaAutomation(t, c)
	if status, raw := machineWAVRequest(t, c.dispatch.web.URL, token, "revoke", 2*time.Second, time.Now().Add(30*time.Second)); status != 202 {
		t.Fatalf("admission: %d %s", status, raw)
	}
	mediaWait(t, time.Second, func() bool { return len(c.zone.service.broker.router.Active()) != 0 }, "active machine call")
	a.Enabled = false
	var system SystemConfig
	if err := c.system.service.owned(&system); err != nil {
		t.Fatal(err)
	}
	if _, err := c.system.service.Store.SaveAutomation(a.ID, a, false); err != nil {
		t.Fatal(err)
	}
	integrationSyncAll(t, c.integrationCluster)
	manager, err := c.dispatch.service.automationRuntimeForRequest()
	if err != nil {
		t.Fatal(err)
	}
	mediaWait(t, 2*time.Second, func() bool {
		job, ok := manager.get(a.ID, "revoke", false)
		return ok && job.State != "running" && job.State != "accepted"
	}, "revoked machine playback ended")
	job, _ := manager.get(a.ID, "revoke", false)
	if job.State == "completed" {
		t.Fatal("revoked account completed its transmission")
	}
	if status, _ := integrationHTTP(t, http.DefaultClient, http.MethodGet, c.dispatch.web.URL+"/api/automation/jobs/revoke", nil, map[string]string{"Authorization": "Bearer " + token}); status != 401 {
		t.Fatal("revoked credential remained usable")
	}
	mediaWait(t, time.Second, func() bool { return len(c.zone.service.broker.router.Active()) == 0 }, "revocation released floor")
}

func TestAutomationJobBoundsDoNotQueueOrDiscardFreshIdempotence(t *testing.T) {
	s := &Service{Store: storeOpen(t, RoleApplication)}
	m, err := s.automationRuntimeForRequest()
	if err != nil {
		t.Fatal(err)
	}
	a := automationRecord{Automation: automationFixture(), TokenHash: make([]byte, 32)}
	input := automationJobInput{action: "wav", pcm: make([]byte, dispatch.FrameBytes), Talkgroups: []int{1001}, RequestID: "busy", ExpiresAt: time.Now().Add(time.Minute)}
	for i := 0; i < 4; i++ {
		m.cancels[fmt.Sprint(i)] = func() {}
	}
	job, status, err := m.start(a, input)
	if err != nil || status != 409 || job.State != "failed" || len(m.cancels) != 4 {
		t.Fatalf("busy request was queued: %#v %d %v", job, status, err)
	}
	m.cancels = map[string]context.CancelFunc{}
	job, status, err = m.start(a, input)
	if err != nil || status != 200 || job.State != "failed" || len(m.cancels) != 0 {
		t.Fatal("retry replayed a previously refused request")
	}
	for len(m.jobs) < automationJobLimit {
		id := fmt.Sprintf("kept-%d", len(m.jobs))
		m.jobs[id] = automationJobRecord{Job: AutomationJob{ID: id, EndedAt: time.Now()}}
	}
	input.RequestID = "full"
	if _, status, err = m.start(a, input); status != 429 || err == nil || len(m.jobs) != automationJobLimit {
		t.Fatal("full history evicted a fresh request or admitted more work")
	}
}

func TestDesktopWorkspaceCatalogScopeKeepsPTTAndDesktopIndependent(t *testing.T) {
	user := fixtureUser()
	desktop := desktopFixture()
	desktop.Workspaces[0].Placements = append(desktop.Workspaces[0].Placements, DesktopPlacement{ID: "other-site", TalkgroupID: 1002})
	user.Profile.Desktop = &desktop
	records := scopeUsers([]userRecord{{User: user}}, []Talkgroup{{TGID: 1001, AdmissionMode: "clear", TransmitMode: "clear", Enabled: true}})
	if len(records) != 1 || records[0].User.Profile.Desktop == nil || len(records[0].User.Profile.Desktop.Workspaces[0].Placements) != 2 {
		t.Fatal("Site catalog retained another Site's desktop resource")
	}
	if len(user.Profile.Desktop.Workspaces[0].Placements) != 3 {
		t.Fatal("catalog filtering mutated System's saved workspace")
	}
}

func TestAutomationIdentityChangePreservesCredential(t *testing.T) {
	s := storeOpen(t, RoleSystem)
	a := automationFixture()
	token, err := s.SaveAutomation(a.ID, a, true)
	if err != nil {
		t.Fatal(err)
	}
	oldID := a.ID
	a.ID = "renamed"
	a.Actions = []string{"future"}
	if replacement, err := s.SaveAutomation(oldID, a, false); err != nil || replacement != "" {
		t.Fatal("rename changed credential", err)
	}
	records, err := s.automationRecords()
	hash := sha256.Sum256([]byte(token))
	if err != nil || len(records) != 1 || records[0].Automation.ID != a.ID || !bytes.Equal(records[0].TokenHash, hash[:]) {
		t.Fatal("renamed account or token lost", err)
	}
}
