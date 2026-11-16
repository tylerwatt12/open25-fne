package framework

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"strings"
	"testing"
	"time"

	"open25.local/open25-fne/internal/activity"
)

func TestRelayNeedsOnlySystemArchiveAndPreservesSourceObservation(t *testing.T) {
	c := integrationNewCluster(t)
	// The Site and Zone are relays. Their own delivery stores cannot become an
	// ingress dependency or retain another producer's copy.
	reopenFixtureWithoutHistory(t, c.site)
	reopenFixtureWithoutHistory(t, c.zone)
	settings := c.site.service.Config().Settings()
	settings.UpstreamURL = c.zone.server.URL
	if err := c.site.service.SaveSettings(settings); err != nil {
		t.Fatal(err)
	}
	at := time.Now().UTC().Add(-time.Hour)
	event := activity.Event{ID: "original-application-observation", At: at, ReceivedAt: at.Add(time.Second), Origin: "application-1", Type: "status", SourceKind: "application", Message: "original message"}
	delegate := c.system.server.Config.Handler
	var relayed []activity.Event
	c.system.server.Config.Handler = http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/internal/events" {
			raw, err := io.ReadAll(r.Body)
			if err != nil {
				t.Error(err)
				w.WriteHeader(500)
				return
			}
			var input struct {
				Events []activity.Event `json:"events"`
			}
			if err := json.Unmarshal(raw, &input); err != nil {
				t.Error(err)
			}
			relayed = input.Events
			r.Body = io.NopCloser(strings.NewReader(string(raw)))
		}
		delegate.ServeHTTP(w, r)
	})
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	var reply struct {
		IDs []string `json:"ids"`
	}
	if err := c.dispatch.service.requestAt(ctx, c.site.server.URL, "POST", "/internal/events", map[string]any{"events": []activity.Event{event}}, &reply, nil); err != nil {
		t.Fatal(err)
	}
	if len(relayed) != 1 || !relayed[0].At.Equal(at) || !relayed[0].ReceivedAt.Equal(event.ReceivedAt) || relayed[0].Origin != event.Origin || relayed[0].ID != event.ID || relayed[0].Message != event.Message || relayed[0].SiteID != "site-1" {
		t.Fatal("relay rewrote source observation", relayed)
	}
	if !completeActivityACK([]activity.Event{event}, reply.IDs) {
		t.Fatal("missing complete System receipt", reply.IDs)
	}
	rows, err := c.system.service.activity.Events(activity.Query{ID: event.ID})
	if err != nil || len(rows) != 1 || rows[0].Origin != event.Origin || !rows[0].At.Equal(event.At) {
		t.Fatal("ACK preceded System commit", rows, err)
	}
	for _, node := range []*integrationService{c.site, c.zone} {
		health := node.service.activity.IngestionHealth()
		if !health.StorageUnavailable || health.QueueDepth != 0 || health.OutboxPending != 0 || health.QueueDropped != 0 {
			t.Fatal("relay attempted a local producer copy", health)
		}
	}
}

func TestSourceRetainsWholeBatchUntilCompleteSystemACK(t *testing.T) {
	for _, kind := range []string{"unavailable", "partial", "duplicate", "foreign", "lost-response"} {
		t.Run(kind, func(t *testing.T) {
			c := integrationNewCluster(t)
			source := c.site.service.activity
			events := []activity.Event{{ID: "one", Origin: "site-1", SiteID: "site-1", Type: "status", SourceKind: "rf"}, {ID: "two", Origin: "site-1", SiteID: "site-1", Type: "status", SourceKind: "rf"}}
			for _, event := range events {
				if !source.Enqueue(event) {
					t.Fatal("capture rejected")
				}
			}
			if err := source.Flush(context.Background()); err != nil {
				t.Fatal(err)
			}
			delegate := c.system.server.Config.Handler
			c.system.server.Config.Handler = http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if r.URL.Path != "/internal/events" {
					delegate.ServeHTTP(w, r)
					return
				}
				switch kind {
				case "unavailable":
					w.WriteHeader(503)
				case "partial":
					respond(w, 200, map[string]any{"ids": []string{"one"}})
				case "duplicate":
					respond(w, 200, map[string]any{"ids": []string{"one", "one"}})
				case "foreign":
					respond(w, 200, map[string]any{"ids": []string{"one", "foreign"}})
				case "lost-response":
					// System commits, but its receipt is lost. Retrying must deduplicate.
					recorder := &discardActivityResponse{header: http.Header{}}
					delegate.ServeHTTP(recorder, r)
					w.WriteHeader(503)
				}
			})
			ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
			defer cancel()
			c.site.service.uploadActivity(ctx)
			if health := source.IngestionHealth(); health.OutboxPending != 2 {
				t.Fatal("incomplete receipt deleted source events", health.OutboxPending)
			}
			c.system.server.Config.Handler = delegate
			c.site.service.uploadActivity(ctx)
			if health := source.IngestionHealth(); health.OutboxPending != 0 {
				t.Fatal("valid durable receipt failed to drain", health.OutboxPending)
			}
			rows, err := c.system.service.activity.Events(activity.Query{Limit: 10})
			if err != nil || len(rows) != 2 {
				t.Fatal("retry duplicated/lost System events", len(rows), err)
			}
		})
	}
}

type discardActivityResponse struct{ header http.Header }

func (w *discardActivityResponse) Header() http.Header         { return w.header }
func (w *discardActivityResponse) WriteHeader(int)             {}
func (w *discardActivityResponse) Write(p []byte) (int, error) { return len(p), nil }

func TestConnectionOutboxRecordsTransitionsWithoutRetrySpam(t *testing.T) {
	c := integrationNewCluster(t)
	integrationSyncAll(t, c)
	site := c.site.service
	original := site.client.Transport
	site.client.Transport = integrationRoundTripFunc(func(*http.Request) (*http.Response, error) { return nil, errors.New("first unavailable attempt") })
	if site.SyncOnce(context.Background()) == nil {
		t.Fatal("failure fixture connected")
	}
	site.client.Transport = integrationRoundTripFunc(func(*http.Request) (*http.Response, error) { return nil, errors.New("different retry error") })
	if site.SyncOnce(context.Background()) == nil {
		t.Fatal("retry fixture connected")
	}
	site.client.Transport = original
	if err := site.SyncOnce(context.Background()); err != nil {
		t.Fatal(err)
	}
	if err := site.activity.Flush(context.Background()); err != nil {
		t.Fatal(err)
	}
	rows, err := site.activity.Pending(context.Background(), 32)
	if err != nil {
		t.Fatal(err)
	}
	transitions := []activity.Event{}
	for _, event := range rows {
		if event.Type == "connection" {
			transitions = append(transitions, event)
		}
	}
	if len(transitions) != 3 {
		t.Fatalf("expected connect, disconnect, reconnect; got %d", len(transitions))
	}
	for _, event := range transitions {
		if event.ID == "" || event.At.IsZero() || event.Origin != "site-1" || !strings.HasPrefix(event.Message, "Parent Zone ") {
			t.Fatal("transition lost source identity or parent boundary", event)
		}
		if strings.Contains(strings.ToLower(event.Message), "duration") {
			t.Fatal("transition fabricated restart outage duration")
		}
	}
	if transitions[1].Details["last_success"] == nil || transitions[1].Details["connected"] != false || transitions[2].Details["connected"] != true {
		t.Fatal("transition omitted known state", transitions)
	}
}
