package framework

import (
	"bytes"
	"context"
	"database/sql"
	"encoding/json"
	"net/http"
	"path/filepath"
	"testing"
	"time"

	"open25.local/open25-fne/internal/activity"
)

func TestLoginResponseDoesNotWaitForConfigurationAuditWrites(t *testing.T) {
	system, zone := storeFixtureSystem(), storeFixtureZone()
	node := startIntegrationService(t, t.TempDir(), RoleSystem, "system-1", system, ServiceCredentials{}, system, zone)
	writer, err := sql.Open("sqlite", filepath.Join(node.service.Config().StateDir, "configuration.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer writer.Close()
	connection, err := writer.Conn(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	defer connection.Close()
	// WAL readers can still authenticate, but a second writer cannot append a
	// configuration event until this explicit write reservation is released.
	if _, err = connection.ExecContext(context.Background(), "BEGIN IMMEDIATE"); err != nil {
		t.Fatal(err)
	}
	defer connection.ExecContext(context.Background(), "ROLLBACK")
	raw, _ := json.Marshal(map[string]string{"username": "administrator", "password": integrationPassword})
	request, _ := http.NewRequest(http.MethodPost, node.web.URL+"/api/login", bytes.NewReader(raw))
	request.Header.Set("Content-Type", "application/json")
	client := &http.Client{Timeout: time.Second}
	response, err := client.Do(request)
	if err != nil {
		t.Fatal("successful authentication waited for a configuration write:", err)
	}
	defer response.Body.Close()
	if response.StatusCode != http.StatusOK || len(response.Cookies()) == 0 {
		t.Fatal("login did not establish its session", response.Status)
	}
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	if err = node.service.activity.Flush(ctx); err != nil {
		t.Fatal(err)
	}
	events, err := node.service.activity.Events(activity.Query{Type: "login", Audience: "security", Limit: 10})
	if err != nil || len(events) != 1 || events[0].Details["username"] != "administrator" {
		t.Fatal("asynchronous security audit was lost", events, err)
	}
}
