package framework

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"open25.local/open25-fne/internal/activity"
)

// Fail the actual activity.Open call on disposable fixture storage. Configuration,
// accounts and independently required RF state remain readable in their owners.
func stopFixtureForUnavailableHistory(t *testing.T, node *integrationService) {
	t.Helper()
	node.server.Close()
	node.web.Close()
	state := node.service.Config().StateDir
	if err := node.service.Close(); err != nil {
		t.Fatal(err)
	}
	name := "event-outbox.db"
	if node.service.Config().Role == RoleSystem {
		name = "activity.db"
	}
	path := filepath.Join(state, name)
	if err := os.Rename(path, path+".retained"); err != nil {
		t.Fatal(err)
	}
	if err := os.Mkdir(path, 0700); err != nil {
		t.Fatal(err)
	}
}

func reopenFixtureWithoutHistory(t *testing.T, node *integrationService) {
	t.Helper()
	stopFixtureForUnavailableHistory(t, node)
	s, err := OpenService(node.configPath, integrationAssets())
	if err != nil {
		t.Fatal("history failure prevented controller initialization", err)
	}
	node.service = s
	node.server = httptest.NewServer(s.InternalHandler())
	node.web = httptest.NewServer(s.Handler())
	settings := s.Config().Settings()
	settings.InternalURL, settings.PublicURL = node.server.URL, node.web.URL
	if err = s.SaveSettings(settings); err != nil {
		t.Fatal(err)
	}
	mediaWait(t, 2*time.Second, func() bool {
		return errors.Is(s.activity.InitializationError(), activity.ErrUnavailable)
	}, "unavailable history initialization")
	if health := s.activity.IngestionHealth(); !health.StorageUnavailable || health.OpenFailures != 1 || health.LastOpenFailureAt == nil {
		t.Fatal("history owner did not retain its unavailable observation")
	}
}

func TestUnavailableHistoryStartupKeepsAuthenticationReportsAndGrantsWorking(t *testing.T) {
	cluster := integrationNewCluster(t)
	reopenFixtureWithoutHistory(t, cluster.site)
	c := newMediaClusterFromIntegration(t, cluster)
	c.affiliations(t, true)
	s := c.site.service
	client := integrationLogin(t, c.site, integrationPassword)
	status, session := client.request(t, http.MethodGet, "/api/session", nil)
	if status != http.StatusOK || !strings.Contains(string(session), `"authenticated":true`) {
		t.Fatal("history startup failure prevented durable authentication", status)
	}

	// The unavailable path is an atomic counter, independent even of the short
	// service/configuration lock. No event or failed storage error is retained.
	beforeDrops := s.activity.IngestionHealth().QueueDropped
	done := make(chan struct{})
	s.mu.Lock()
	go func() {
		defer close(done)
		for range 10000 {
			s.emit(activity.Event{Type: "unavailable-history-test"})
		}
	}()
	select {
	case <-done:
		s.mu.Unlock()
	case <-time.After(250 * time.Millisecond):
		s.mu.Unlock()
		<-done
		t.Fatal("unavailable history capture waited for a controller lock")
	}
	if s.activity.IngestionHealth().QueueDropped < beforeDrops+10000 {
		t.Fatal("unavailable history discarded observations without accounting")
	}
	status, raw := client.request(t, http.MethodGet, "/api/history/health", nil)
	var health activity.IngestionHealth
	if status != http.StatusServiceUnavailable || json.Unmarshal(raw, &health) != nil ||
		!health.StorageUnavailable || !health.Degraded || health.OpenFailures != 1 || health.QueueDropped < 10000 || health.QueueDepth != 0 {
		t.Fatal("history availability or drop accounting was hidden", status)
	}
	if strings.Contains(string(raw), s.Config().StateDir) || strings.Contains(string(raw), "unavailable-history-test") {
		t.Fatal("unavailable diagnostics exposed event or private storage details")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	observed := time.Now()
	if err := s.SyncOnce(ctx); err != nil {
		t.Fatal("history startup failure prevented controller synchronization", err)
	}
	fresh := false
	for _, report := range c.zone.service.siteReports() {
		fresh = fresh || report.ID == s.Config().InstanceID && report.ReceivedAt.After(observed)
	}
	if !fresh {
		t.Fatal("history startup failure prevented a fresh Site report")
	}
	endpoint, err := s.mediaClient.endpointAt(func() uint64 { _, generation := s.mediaClient.Link(); return generation }())
	if err != nil {
		t.Fatal(err)
	}
	request, err := s.observeRFSource(phase0RFRequest(s, "unavailable-history-grant"))
	if err != nil {
		t.Fatal("source admission depended on unavailable history", err)
	}
	started := time.Now()
	call, err := s.voice.Load().begin(ctx, endpoint, request, false)
	if err != nil {
		t.Fatal("grant depended on unavailable history", err)
	}
	if c.natives[c.otherSite.service.Config().InstanceID].count() == 0 {
		t.Fatal("the receiving native endpoint was not granted")
	}
	if err = call.Write(admissionFrame(call.grant.ID, 1)); err != nil {
		t.Fatal("media depended on unavailable history", err)
	}
	if err = call.Renew(); err != nil {
		t.Fatal("renewal depended on unavailable history", err)
	}
	if err = call.EndSpurt(ctx); err != nil {
		t.Fatal("airtime release depended on unavailable history", err)
	}
	if err = call.End(ctx); err != nil {
		t.Fatal("call release depended on unavailable history", err)
	}
	t.Logf("grant, media, renewal and release after actual history.Open failure: %s", time.Since(started))
}

func TestPendingHistoryInitializationDoesNotBlockControllerStartup(t *testing.T) {
	c := integrationNewCluster(t)
	node := c.site
	node.server.Close()
	node.web.Close()
	state := node.service.Config().StateDir
	if err := node.service.Close(); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(state, "event-outbox.db")
	db, err := sql.Open("sqlite", path)
	if err != nil {
		t.Fatal(err)
	}
	connection, err := db.Conn(context.Background())
	if err != nil {
		_ = db.Close()
		t.Fatal(err)
	}
	// Force actual schema initialization to need the held writer while the
	// existing WAL mode remains usable. This is disposable fixture storage.
	if _, err = connection.ExecContext(context.Background(), "DROP TABLE outbox"); err != nil {
		t.Fatal(err)
	}
	if _, err = connection.ExecContext(context.Background(), "BEGIN IMMEDIATE"); err != nil {
		_ = connection.Close()
		_ = db.Close()
		t.Fatal(err)
	}
	var service *Service
	defer func() {
		_, _ = connection.ExecContext(context.Background(), "ROLLBACK")
		_ = connection.Close()
		_ = db.Close()
		if service != nil {
			_ = service.Close()
		}
	}()
	started := time.Now()
	service, err = OpenService(node.configPath, integrationAssets())
	if err != nil || time.Since(started) > 500*time.Millisecond {
		t.Fatal("controller startup waited for history initialization", err)
	}
	if !errors.Is(service.activity.InitializationError(), activity.ErrInitializing) {
		t.Fatal("blocked history initializer did not remain visibly pending")
	}
	for range 8 {
		service.emit(activity.Event{Type: "pending-history-capture"})
	}
	health := service.activity.IngestionHealth()
	if !health.StorageInitializing || health.StorageUnavailable || health.OpenFailures != 0 || health.QueueDepth != 8 {
		t.Fatal("pending history capture or initialization was misreported")
	}
	w := httptest.NewRecorder()
	service.historyHTTP(w, httptest.NewRequest(http.MethodGet, "/api/history/health", nil), Session{})
	var pending activity.IngestionHealth
	if w.Code != http.StatusServiceUnavailable || json.Unmarshal(w.Body.Bytes(), &pending) != nil || !pending.StorageInitializing {
		t.Fatal("pending history query did not return an honest unavailable result", w.Code, w.Body.String())
	}
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	if err = service.SyncOnce(ctx); err != nil {
		t.Fatal("pending history initialization blocked controller reporting", err)
	}
	if _, err = connection.ExecContext(context.Background(), "ROLLBACK"); err != nil {
		t.Fatal(err)
	}
	if err = service.activity.Flush(ctx); err != nil {
		t.Fatal("accepted captures did not persist after initialization could proceed", err)
	}
	events, err := service.activity.Pending(context.Background(), activity.EventBatchLimit)
	count := 0
	for _, event := range events {
		if event.Type == "pending-history-capture" {
			count++
		}
	}
	if err != nil || count != 8 || service.activity.InitializationError() != nil {
		t.Fatal("pending initialization lost captured observations", err)
	}
}

func TestUnavailableSystemHistoryCannotAcknowledgeAndSourcesRemainRelays(t *testing.T) {
	c := integrationNewCluster(t)
	reopenFixtureWithoutHistory(t, c.system)
	for _, route := range []string{"/api/history/events", "/api/history/calls", "/api/history/settings", "/api/history/health"} {
		w := httptest.NewRecorder()
		c.system.service.historyHTTP(w, httptest.NewRequest("GET", route, nil), Session{})
		if w.Code != 503 {
			t.Fatalf("unavailable System route %s returned %d", route, w.Code)
		}
	}
	// The parent address changed when this fixture reopened; sources still
	// relay normally and retain their own outbox on the unavailable response.
	settings := c.zone.service.Config().Settings()
	settings.UpstreamURL = c.system.server.URL
	if err := c.zone.service.SaveSettings(settings); err != nil {
		t.Fatal(err)
	}
	for _, spoof := range []bool{false, true} {
		event := activity.Event{ID: "retained-at-source", Origin: "application-1", Type: "status"}
		if spoof {
			event.Origin = "foreign-owner"
		}
		raw, _ := json.Marshal(map[string]any{"events": []activity.Event{event}})
		w := httptest.NewRecorder()
		request := httptest.NewRequest("POST", "/internal/events", strings.NewReader(string(raw)))
		request.Header.Set("Content-Type", "application/json")
		c.site.service.activityInternal(w, request, PeerCredential{Role: RoleApplication, InstanceID: "application-1"})
		want := 503
		if spoof {
			want = 403
		}
		if w.Code != want || strings.Contains(w.Body.String(), `"ids"`) {
			t.Fatal("unavailable System acknowledged or ownership bypassed", w.Code)
		}
	}
	for _, node := range []*integrationService{c.site, c.zone, c.dispatch} {
		w := httptest.NewRecorder()
		node.service.historyHTTP(w, httptest.NewRequest("GET", "/api/history/events", nil), Session{})
		if w.Code != 404 {
			t.Fatal("source exposed local history", w.Code)
		}
	}
}

func TestUnavailableHistoryDoesNotHideMandatorySiteProtectionFailure(t *testing.T) {
	c := integrationNewCluster(t)
	stopFixtureForUnavailableHistory(t, c.site)
	config, err := ReadFileConfig(c.site.configPath)
	if err != nil {
		t.Fatal(err)
	}
	store, err := OpenStore(filepath.Join(config.StateDir, "configuration.db"), RoleSite)
	if err != nil {
		t.Fatal(err)
	}
	if err = store.WriteCache("radio-inhibition-actual", json.RawMessage("null")); err != nil {
		t.Fatal(err)
	}
	if err = store.Close(); err != nil {
		t.Fatal(err)
	}
	reopened, err := OpenService(c.site.configPath, integrationAssets())
	if reopened != nil {
		_ = reopened.Close()
	}
	if err == nil || !strings.Contains(err.Error(), "inhibit state") {
		t.Fatal("history degradation hid a required Site protection failure", err)
	}
}
