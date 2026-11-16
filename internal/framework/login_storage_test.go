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

func TestLoginWaitsForSessionPersistenceAndQueuesAudit(t *testing.T) {
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
	// A successful login must commit its persistent session before returning a
	// cookie. Security audit delivery remains independently queued.
	if _, err = connection.ExecContext(context.Background(), "BEGIN IMMEDIATE"); err != nil {
		t.Fatal(err)
	}
	defer connection.ExecContext(context.Background(), "ROLLBACK")
	raw, _ := json.Marshal(map[string]string{"username": "administrator", "password": integrationPassword})
	request, _ := http.NewRequest(http.MethodPost, node.web.URL+"/api/login", bytes.NewReader(raw))
	request.Header.Set("Content-Type", "application/json")
	client := &http.Client{Timeout: 3 * time.Second}
	type result struct {
		response *http.Response
		err      error
	}
	completed := make(chan result, 1)
	go func() { response, err := client.Do(request); completed <- result{response, err} }()
	select {
	case result := <-completed:
		if result.response != nil {
			result.response.Body.Close()
		}
		t.Fatal("login returned before its session could commit", result.err)
	case <-time.After(100 * time.Millisecond):
	}
	if _, err = connection.ExecContext(context.Background(), "ROLLBACK"); err != nil {
		t.Fatal(err)
	}
	resultValue := <-completed
	response, err := resultValue.response, resultValue.err
	if err != nil {
		t.Fatal("login did not recover after persistent storage was released", err)
	}
	defer response.Body.Close()
	if response.StatusCode != http.StatusOK || len(response.Cookies()) == 0 {
		t.Fatal("login did not establish its session", response.Status)
	}
	var persisted int
	if err = node.service.Store.db.QueryRow("SELECT count(*) FROM framework_documents WHERE name LIKE 'auth:session:%'").Scan(&persisted); err != nil || persisted != 1 {
		t.Fatal("successful login did not persist exactly one session", persisted, err)
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

func TestLoginStorageFailureDoesNotIssueCookie(t *testing.T) {
	system, zone := storeFixtureSystem(), storeFixtureZone()
	node := startIntegrationService(t, t.TempDir(), RoleSystem, "system-1", system, ServiceCredentials{}, system, zone)
	if _, err := node.service.Store.db.Exec(`CREATE TRIGGER reject_session BEFORE INSERT ON framework_documents WHEN NEW.name LIKE 'auth:session:%' BEGIN SELECT RAISE(ABORT,'injected session write failure'); END`); err != nil {
		t.Fatal(err)
	}
	raw, _ := json.Marshal(map[string]string{"username": "administrator", "password": integrationPassword})
	request, _ := http.NewRequest("POST", node.web.URL+"/api/login", bytes.NewReader(raw))
	request.Header.Set("Content-Type", "application/json")
	response, err := http.DefaultClient.Do(request)
	if err != nil {
		t.Fatal(err)
	}
	defer response.Body.Close()
	if response.StatusCode != 500 || len(response.Cookies()) != 0 {
		t.Fatal("failed persistence established a session", response.Status)
	}
}
