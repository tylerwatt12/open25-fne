package framework

import (
	"context"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"slices"
	"strconv"
	"strings"
	"sync"
	"time"
	"unicode/utf8"

	"open25.local/open25-fne/internal/activity"
	"open25.local/open25-fne/internal/dispatch"
)

const automationJobLimit = 256

type AutomationJob struct {
	ID           string    `json:"id"`
	AutomationID string    `json:"automation_id"`
	Action       string    `json:"action"`
	RadioID      int       `json:"radio_id"`
	Talkgroups   []int     `json:"talkgroups"`
	Secure       bool      `json:"secure"`
	State        string    `json:"state"`
	RequestedAt  time.Time `json:"requested_at"`
	ExpiresAt    time.Time `json:"expires_at"`
	StartedAt    time.Time `json:"started_at,omitempty"`
	EndedAt      time.Time `json:"ended_at,omitempty"`
	Frames       int       `json:"frames"`
	Message      string    `json:"message,omitempty"`
}

type automationJobRecord struct {
	Job         AutomationJob `json:"job"`
	Fingerprint string        `json:"fingerprint"`
}

type automationJobInput struct {
	Text       string    `json:"text"`
	Talkgroups []int     `json:"talkgroups"`
	Secure     bool      `json:"secure"`
	RequestID  string    `json:"request_id"`
	ExpiresAt  time.Time `json:"expires_at"`
	pcm        []byte
	action     string
}

type automationRuntime struct {
	s       *Service
	mu      sync.Mutex
	jobs    map[string]automationJobRecord
	cancels map[string]context.CancelFunc
	wg      sync.WaitGroup
	closed  bool
}

func automationJobKey(identity, request string) string { return identity + "/" + request }

func (s *Service) automationRuntimeForRequest() (*automationRuntime, error) {
	s.automationMu.Lock()
	defer s.automationMu.Unlock()
	if s.automatedJobs != nil {
		return s.automatedJobs, nil
	}
	m := &automationRuntime{s: s, jobs: map[string]automationJobRecord{}, cancels: map[string]context.CancelFunc{}}
	var records []automationJobRecord
	if raw, _, err := s.Store.ReadCache("automation-jobs"); err == nil {
		if err = DecodeStrict(raw, &records); err != nil || len(records) > automationJobLimit {
			return nil, errors.New("stored automation job history is invalid")
		}
	}
	now := time.Now().UTC()
	interrupted := []AutomationJob{}
	for _, record := range records {
		if !recordID.MatchString(record.Job.ID) || !recordID.MatchString(record.Job.AutomationID) || len(record.Fingerprint) != 64 {
			return nil, errors.New("stored automation job history is invalid")
		}
		if record.Job.State == "accepted" || record.Job.State == "running" {
			record.Job.State, record.Job.Message, record.Job.EndedAt = "interrupted", "Application Server restarted; this transmission will not be replayed.", now
			interrupted = append(interrupted, record.Job)
		}
		m.jobs[automationJobKey(record.Job.AutomationID, record.Job.ID)] = record
	}
	if len(records) > 0 {
		if err := m.persistLocked(); err != nil {
			return nil, err
		}
	}
	s.automatedJobs = m
	for _, job := range interrupted {
		s.automationJobEvent(job)
	}
	return m, nil
}

func (s *Service) automationJobEvent(job AutomationJob) {
	at := job.RequestedAt
	if job.State == "running" {
		at = job.StartedAt
	} else if !job.EndedAt.IsZero() {
		at = job.EndedAt
	}
	message := "Automated audio request " + strings.ReplaceAll(job.State, "_", " ") + "."
	if job.State == "interrupted" {
		message = "Application Server restarted; automated audio was not replayed."
	}
	for _, tg := range job.Talkgroups {
		hash := sha256.Sum256([]byte(job.AutomationID + "\x00" + job.ID + "\x00" + job.RequestedAt.UTC().Format(time.RFC3339Nano) + "\x00" + job.State + "\x00" + strconv.Itoa(tg)))
		s.emit(activity.Event{ID: "automation-job-" + hex.EncodeToString(hash[:]), At: at, RadioID: job.RadioID, TalkgroupID: tg, Type: "automation_job", Label: "Automated audio", Message: message, Audience: "dispatcher", Outcome: job.State, SourceKind: "automation", Details: map[string]any{"automation_id": job.AutomationID, "job_id": job.ID, "action": job.Action, "frames": job.Frames, "secure": job.Secure}})
	}
}

func (m *automationRuntime) persistLocked() error {
	records := make([]automationJobRecord, 0, len(m.jobs))
	for _, record := range m.jobs {
		records = append(records, record)
	}
	slices.SortFunc(records, func(a, b automationJobRecord) int {
		return strings.Compare(automationJobKey(a.Job.AutomationID, a.Job.ID), automationJobKey(b.Job.AutomationID, b.Job.ID))
	})
	return m.s.writeCache("automation-jobs", records)
}

func (s *Service) closeAutomationJobs() {
	s.automationMu.Lock()
	m := s.automatedJobs
	s.automationMu.Unlock()
	if m == nil {
		return
	}
	m.mu.Lock()
	m.closed = true
	for _, cancel := range m.cancels {
		cancel()
	}
	m.mu.Unlock()
	m.wg.Wait()
}

func cloneAutomationJob(job AutomationJob) AutomationJob {
	job.Talkgroups = slices.Clone(job.Talkgroups)
	return job
}

func (m *automationRuntime) get(identity, request string, cancel bool) (AutomationJob, bool) {
	m.mu.Lock()
	defer m.mu.Unlock()
	key := automationJobKey(identity, request)
	record, ok := m.jobs[key]
	if !ok {
		return AutomationJob{}, false
	}
	if cancel {
		if stop := m.cancels[key]; stop != nil {
			stop()
		}
	}
	return cloneAutomationJob(record.Job), true
}

func (m *automationRuntime) start(record automationRecord, input automationJobInput) (AutomationJob, int, error) {
	a := record.Automation
	if !recordID.MatchString(input.RequestID) || input.ExpiresAt.IsZero() {
		return AutomationJob{}, 400, errors.New("provide request_id and expires_at for this transmission")
	}
	groups := slices.Clone(input.Talkgroups)
	slices.Sort(groups)
	if len(groups) < 1 || len(groups) > 16 || !slices.Contains(a.Actions, input.action) {
		return AutomationJob{}, 403, dispatch.ErrPermission
	}
	for i, tg := range groups {
		if (i > 0 && groups[i-1] == tg) || !slices.Contains(a.Talkgroups, tg) {
			return AutomationJob{}, 403, dispatch.ErrPermission
		}
	}
	input.Talkgroups = groups
	payload := []byte(input.Text)
	if input.action == "wav" {
		payload = input.pcm
	}
	payloadHash := sha256.Sum256(payload)
	raw, _ := json.Marshal(struct {
		Action string
		Groups []int
		Secure bool
		Hash   [32]byte
	}{input.action, groups, input.Secure, payloadHash})
	fingerprint := sha256.Sum256(raw)
	key := automationJobKey(a.ID, input.RequestID)
	now := time.Now().UTC()
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.closed {
		return AutomationJob{}, 503, errors.New("Application Server is stopping")
	}
	if old, ok := m.jobs[key]; ok {
		if old.Fingerprint != hex.EncodeToString(fingerprint[:]) {
			return AutomationJob{}, 409, errors.New("this request ID was already used for different audio or destinations")
		}
		return cloneAutomationJob(old.Job), 200, nil
	}
	if !input.ExpiresAt.After(now) || input.ExpiresAt.After(now.Add(90*time.Second)) {
		return AutomationJob{}, 400, errors.New("expires_at must be in the next ninety seconds")
	}
	for id, old := range m.jobs {
		if !old.Job.EndedAt.IsZero() && old.Job.EndedAt.Before(now.Add(-24*time.Hour)) {
			delete(m.jobs, id)
		}
	}
	if len(m.jobs) >= automationJobLimit {
		return AutomationJob{}, 429, errors.New("automation history is full; completed request IDs are retained for twenty-four hours")
	}
	job := AutomationJob{ID: input.RequestID, AutomationID: a.ID, Action: input.action, RadioID: a.RadioID, Talkgroups: groups, Secure: input.Secure, State: "accepted", RequestedAt: now, ExpiresAt: input.ExpiresAt}
	busy := len(m.cancels) >= 4
	for _, current := range m.jobs {
		if current.Job.AutomationID == a.ID && (current.Job.State == "accepted" || current.Job.State == "running") {
			busy = true
		}
	}
	if busy {
		job.State, job.Message, job.EndedAt = "failed", "An automation transmission is already active; this request was not queued.", now
	}
	m.jobs[key] = automationJobRecord{Job: job, Fingerprint: hex.EncodeToString(fingerprint[:])}
	if err := m.persistLocked(); err != nil {
		delete(m.jobs, key)
		return AutomationJob{}, 503, errors.New("could not preserve this request ID; no transmission was started")
	}
	m.s.automationJobEvent(job)
	if busy {
		return cloneAutomationJob(job), 409, nil
	}
	parent := m.s.operationCtx
	if parent == nil {
		parent = context.Background()
	}
	ctx, cancel := context.WithDeadline(parent, input.ExpiresAt)
	m.cancels[key] = cancel
	m.wg.Add(1)
	go m.run(ctx, key, record, input)
	return cloneAutomationJob(job), 202, nil
}

func (m *automationRuntime) run(ctx context.Context, key string, authority automationRecord, input automationJobInput) {
	defer m.wg.Done()
	m.mu.Lock()
	record := m.jobs[key]
	record.Job.State, record.Job.StartedAt = "running", time.Now().UTC()
	m.jobs[key] = record
	err := m.persistLocked()
	m.mu.Unlock()
	if err == nil {
		m.s.automationJobEvent(record.Job)
	}
	pcm := input.pcm
	if err == nil && input.action == "tts" {
		pcm, err = m.s.synthesizeSpeech(ctx, input.Text)
	}
	frames := 0
	if err == nil {
		err = ctx.Err()
	}
	if err == nil {
		current, ok := m.s.view().Automations[authority.Automation.ID]
		if !ok || profileSignature(current) != profileSignature(authority) {
			err = dispatch.ErrPermission
		} else if m.s.hub == nil {
			err = dispatch.ErrUnavailable
		} else {
			p := dispatch.Principal{AutomationID: authority.Automation.ID, AutomationAction: input.action, RadioID: authority.Automation.RadioID, ReceiveGroups: []int{}, TransmitGroups: slices.Clone(authority.Automation.Talkgroups), CanTransmit: true, CanSecure: slices.Contains(authority.Automation.Actions, "secure_ptt")}
			frames, err = m.s.hub.PlayPCM(ctx, p, "automation:"+key, input.Talkgroups, pcm, input.Secure, 0, 0)
		}
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	if cancel := m.cancels[key]; cancel != nil {
		cancel()
	}
	delete(m.cancels, key)
	record = m.jobs[key]
	record.Job.Frames, record.Job.EndedAt = frames, time.Now().UTC()
	record.Job.State = "completed"
	if err != nil {
		record.Job.State, record.Job.Message = "failed", err.Error()
		if errors.Is(err, context.DeadlineExceeded) {
			record.Job.State = "expired"
		} else if errors.Is(err, context.Canceled) {
			record.Job.State = "cancelled"
		}
	}
	m.jobs[key] = record
	if err := m.persistLocked(); err != nil {
		m.s.runtimeError(errors.New("could not preserve the final automation job result"))
	}
	m.s.automationJobEvent(record.Job)
}

func (s *Service) automationBearer(r *http.Request) (automationRecord, error) {
	if r.Header.Get("Cookie") != "" || !strings.HasPrefix(r.Header.Get("Authorization"), "Bearer ") {
		return automationRecord{}, dispatch.ErrPermission
	}
	token := strings.TrimPrefix(r.Header.Get("Authorization"), "Bearer ")
	if len(token) != 64 {
		return automationRecord{}, dispatch.ErrPermission
	}
	if _, err := hex.DecodeString(token); err != nil {
		return automationRecord{}, dispatch.ErrPermission
	}
	hash := sha256.Sum256([]byte(token))
	for _, record := range s.view().Automations {
		if record.Automation.Enabled && subtle.ConstantTimeCompare(hash[:], record.TokenHash) == 1 {
			return record, nil
		}
	}
	return automationRecord{}, dispatch.ErrPermission
}

func (s *Service) automationHTTP(w http.ResponseWriter, r *http.Request) {
	if s.Config().Role != RoleApplication {
		fail(w, 403, errors.New("automated audio is supplied by Application Server"))
		return
	}
	record, err := s.automationBearer(r)
	if err != nil {
		fail(w, 401, errors.New("automation credential was not accepted"))
		return
	}
	path := strings.TrimPrefix(r.URL.Path, "/api/automation/")
	if strings.HasPrefix(path, "jobs/") {
		id := strings.TrimPrefix(path, "jobs/")
		if !recordID.MatchString(id) || (r.Method != http.MethodGet && r.Method != http.MethodDelete) {
			http.NotFound(w, r)
			return
		}
		manager, err := s.automationRuntimeForRequest()
		if err != nil {
			fail(w, 503, err)
			return
		}
		job, ok := manager.get(record.Automation.ID, id, r.Method == http.MethodDelete)
		if !ok {
			http.NotFound(w, r)
			return
		}
		respond(w, 200, map[string]any{"job": job})
		return
	}
	if (path != "wav" && path != "tts") || r.Method != http.MethodPost {
		http.NotFound(w, r)
		return
	}
	if !slices.Contains(record.Automation.Actions, path) {
		fail(w, 403, dispatch.ErrPermission)
		return
	}
	input := automationJobInput{action: path}
	if path == "tts" {
		if err = readJSON(w, r, &input); err != nil {
			fail(w, 400, err)
			return
		}
		if strings.TrimSpace(input.Text) == "" || !utf8.ValidString(input.Text) || utf8.RuneCountInString(input.Text) > 500 {
			fail(w, 400, errors.New("supply one to five hundred characters of text"))
			return
		}
	} else {
		input.RequestID = r.Header.Get("X-Request-ID")
		input.ExpiresAt, err = time.Parse(time.RFC3339Nano, r.Header.Get("X-Expires-At"))
		if err != nil {
			fail(w, 400, errors.New("X-Expires-At must contain a UTC or offset timestamp"))
			return
		}
		secure := r.URL.Query().Get("secure")
		if secure != "" && secure != "false" && secure != "true" {
			fail(w, 400, errors.New("secure must be true or false"))
			return
		}
		input.Secure = secure == "true"
		for _, group := range r.URL.Query()["talkgroup_id"] {
			tg, parseErr := strconv.Atoi(group)
			if parseErr != nil {
				fail(w, 400, errors.New("talkgroup_id must be a decimal number"))
				return
			}
			input.Talkgroups = append(input.Talkgroups, tg)
		}
		mediaType := strings.Split(r.Header.Get("Content-Type"), ";")[0]
		if mediaType != "audio/wav" && mediaType != "audio/x-wav" && mediaType != "application/octet-stream" {
			fail(w, 415, errors.New("send an 8 kHz mono PCM WAV file"))
			return
		}
		raw, readErr := io.ReadAll(http.MaxBytesReader(w, r.Body, 512<<10))
		if readErr != nil {
			fail(w, 413, errors.New("WAV file is too large"))
			return
		}
		input.pcm, err = dispatch.ParseWAV(raw)
		if err != nil {
			fail(w, 400, err)
			return
		}
	}
	manager, err := s.automationRuntimeForRequest()
	if err != nil {
		fail(w, 503, err)
		return
	}
	job, status, err := manager.start(record, input)
	if err != nil {
		fail(w, status, err)
		return
	}
	respond(w, status, map[string]any{"job": job})
}
