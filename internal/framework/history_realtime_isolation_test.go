package framework

import (
	"context"
	"database/sql"
	"fmt"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"open25.local/open25-fne/internal/activity"
)

func blockHistoryWriter(t *testing.T, s *Service) {
	t.Helper()
	name := "event-outbox.db"
	if s.Config().Role == RoleSystem {
		name = "activity.db"
	}
	db, err := sql.Open("sqlite", filepath.Join(s.Config().StateDir, name))
	if err != nil {
		t.Fatal(err)
	}
	conn, err := db.Conn(context.Background())
	if err != nil {
		db.Close()
		t.Fatal(err)
	}
	if _, err = conn.ExecContext(context.Background(), "BEGIN IMMEDIATE"); err != nil {
		conn.Close()
		db.Close()
		t.Fatal(err)
	}
	t.Cleanup(func() { _, _ = conn.ExecContext(context.Background(), "ROLLBACK"); conn.Close(); db.Close() })
	if !s.activity.Enqueue(activity.Event{ID: "blocked-write", Origin: s.Config().InstanceID, SiteID: "site-1", Type: "status", SourceKind: "rf"}) {
		t.Fatal("could not submit blocked history write")
	}
	mediaWait(t, time.Second, func() bool { return s.activity.IngestionHealth().QueueDepth == 0 }, "history writer to take its blocked event")
}

func TestControllerSyncAndReportDoNotWaitForHistoryUploadOrWriter(t *testing.T) {
	c := integrationNewCluster(t)
	integrationSyncAll(t, c)
	s := c.site.service
	if !s.activity.Enqueue(activity.Event{ID: "pending-upload", Origin: "site-1", SiteID: "site-1", Type: "status", SourceKind: "rf"}) {
		t.Fatal("outbox capture rejected")
	}
	if err := s.activity.Flush(context.Background()); err != nil {
		t.Fatal(err)
	}
	started, release := make(chan struct{}), make(chan struct{})
	var once sync.Once
	delegate := c.zone.server.Config.Handler
	zone := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/internal/events" {
			once.Do(func() { close(started) })
			select {
			case <-release:
			case <-r.Context().Done():
				return
			}
		}
		delegate.ServeHTTP(w, r)
	}))
	t.Cleanup(zone.Close)
	settings := s.Config().Settings()
	settings.UpstreamURL = zone.URL
	if err := s.SaveSettings(settings); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() { defer close(done); s.runActivityHistory(ctx) }()
	defer func() {
		cancel()
		close(release)
		select {
		case <-done:
		case <-time.After(2 * time.Second):
			t.Error("history worker failed to stop")
		}
	}()
	select {
	case <-started:
	case <-time.After(time.Second):
		t.Fatal("history worker did not start upload")
	}
	blockHistoryWriter(t, s)
	startedAt := time.Now().UTC()
	syncCtx, stop := context.WithTimeout(context.Background(), 500*time.Millisecond)
	defer stop()
	if err := s.SyncOnce(syncCtx); err != nil {
		t.Fatal("control sync waited for blocked history", err)
	}
	reports := c.zone.service.siteReports()
	fresh := false
	for _, r := range reports {
		if r.ID == "site-1" && r.ReceivedAt.After(startedAt) {
			fresh = true
		}
	}
	if !fresh {
		t.Fatal("Site report did not advance while history was blocked")
	}
}

// HTTP, authentication and routing are real; native radio/codec boundaries are
// fixtures. This proves application isolation, not physical request-to-RF time.
func TestGrantRenewAndReleaseDoNotWaitForBlockedFullHistory(t *testing.T) {
	c := newMediaCluster(t)
	c.affiliations(t, true)
	source := c.site.service
	endpoint, err := source.mediaClient.endpointAt(func() uint64 { _, g := source.mediaClient.Link(); return g }())
	if err != nil {
		t.Fatal(err)
	}
	nodes := []*integrationService{c.site, c.zone, c.otherSite}
	for _, node := range nodes {
		blockHistoryWriter(t, node.service)
		for i := 0; i < 1100; i++ {
			node.service.emit(activity.Event{ID: fmt.Sprintf("queued-%d", i), Type: "status", SourceKind: "rf"})
		}
		if node.service.activity.IngestionHealth().QueueDropped == 0 {
			t.Fatal("history queue was not saturated")
		}
	}
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	start := time.Now()
	request, err := source.observeRFSource(phase0RFRequest(source, "history-isolated-grant"))
	if err != nil {
		t.Fatal("source admission waited for history", err)
	}
	call, err := source.voice.Load().begin(ctx, endpoint, request, false)
	if err != nil {
		t.Fatal("grant waited for history", err)
	}
	if c.natives[c.otherSite.service.Config().InstanceID].count() != 1 {
		t.Fatal("receiving Site was not granted")
	}
	if err = call.Write(admissionFrame(call.grant.ID, 1)); err != nil {
		t.Fatal("media waited for history", err)
	}
	if err = call.Renew(); err != nil {
		t.Fatal("renewal waited for history", err)
	}
	if err = call.EndSpurt(ctx); err != nil {
		t.Fatal("airtime release waited for history", err)
	}
	if err = call.End(ctx); err != nil {
		t.Fatal("call release waited for history", err)
	}

	t.Logf("grant, renewal and release with all three history writers blocked and queues full: %s", time.Since(start))
}

func TestHistoryTransferBudgetDoesNotWidenControlTimeout(t *testing.T) {
	c := integrationNewCluster(t)
	integrationSyncAll(t, c)
	s := c.site.service
	s.client.Timeout = 20 * time.Millisecond
	delegate := c.zone.server.Config.Handler
	zone := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/internal/events" {
			time.Sleep(60 * time.Millisecond)
		}
		delegate.ServeHTTP(w, r)
	}))
	t.Cleanup(zone.Close)
	settings := s.Config().Settings()
	settings.UpstreamURL = zone.URL
	if err := s.SaveSettings(settings); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	if err := s.upstreamRequest(ctx, http.MethodPost, "/internal/events", map[string]any{"events": []activity.Event{}}, nil); err != nil {
		t.Fatal("archive inherited the short control client timeout", err)
	}
	if s.client.Timeout != 20*time.Millisecond {
		t.Fatal("archive changed the control client timeout")
	}
}
