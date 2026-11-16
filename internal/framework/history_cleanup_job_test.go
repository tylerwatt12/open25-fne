package framework

import (
	"context"
	"database/sql"
	"io"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"open25.local/open25-fne/internal/activity"
)

func TestHistoryCleanupAcceptanceDoesNotWaitForStorage(t *testing.T) {
	path := filepath.Join(t.TempDir(), "activity.db")
	store, err := activity.OpenArchive(path)
	if err != nil {
		t.Fatal(err)
	}
	waitHistoryInitialization(t, store)
	defer store.Close()
	before, err := store.Retention(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if err = store.Append(context.Background(), activity.Event{ID: "expired", Origin: "site-1", Type: "status", At: time.Now().Add(-40 * 24 * time.Hour)}); err != nil {
		t.Fatal(err)
	}
	blocker, err := sql.Open("sqlite", path)
	if err != nil {
		t.Fatal(err)
	}
	defer blocker.Close()
	conn, err := blocker.Conn(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()
	if _, err = conn.ExecContext(context.Background(), "BEGIN IMMEDIATE"); err != nil {
		t.Fatal(err)
	}
	defer conn.ExecContext(context.Background(), "ROLLBACK")
	s := &Service{config: FileConfig{Role: RoleSystem, InstanceID: "system-1"}, activity: store}
	done := make(chan *httptest.ResponseRecorder, 1)
	go func() {
		w := httptest.NewRecorder()
		s.historyHTTP(w, httptest.NewRequest(http.MethodPost, "/api/history/cleanup", nil), Session{})
		done <- w
	}()
	select {
	case w := <-done:
		if w.Code != http.StatusAccepted {
			t.Fatalf("HTTP %d: %s", w.Code, w.Body.String())
		}
	case <-time.After(250 * time.Millisecond):
		t.Fatal("cleanup acceptance waited for the database")
	}
	w := httptest.NewRecorder()
	s.historyHTTP(w, httptest.NewRequest(http.MethodGet, "/api/history/cleanup", nil), Session{})
	if w.Code != http.StatusOK {
		t.Fatal(w.Code, w.Body.String())
	}
	after, err := store.Retention(context.Background())
	if err != nil || after != before {
		t.Fatal("cleanup changed retention", after, err)
	}
	if _, err = conn.ExecContext(context.Background(), "ROLLBACK"); err != nil {
		t.Fatal(err)
	}
	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		v, err := store.HistoryStatus(context.Background())
		if err != nil {
			t.Fatal(err)
		}
		if v.Events == 0 && store.CleanupStatus().LastCompletedAt != nil {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatal("accepted cleanup did not finish after storage was released")
}

func TestHistoryCleanupRequiresAdministrator(t *testing.T) {
	s := &Service{config: FileConfig{Role: RoleSite}}
	for _, method := range []string{http.MethodGet, http.MethodPost} {
		w := httptest.NewRecorder()
		s.historyHTTP(w, httptest.NewRequest(method, "/api/history/cleanup", nil), Session{UserID: "operator"})
		if w.Code != http.StatusForbidden {
			t.Fatal(w.Code, w.Body.String())
		}
	}
}

func TestHistoryStorageFormsKeepCleanupSeparateFromPolicy(t *testing.T) {
	c := integrationNewCluster(t)
	pair := c.system
	client := earlyWebLogin(t, pair)
	address := pair.web.URL + "/history-settings"
	body := earlyWebGet(t, client, address, http.StatusOK)
	cleanup := earlyWebForm(t, body, "action")
	cleanup.Set("action", "cleanup")
	if cleanup.Has("retention_days") {
		t.Fatal("cleanup form submitted retention")
	}
	before, err := pair.service.activity.Retention(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	earlyWebPost(t, client, address, cleanup, pair.web.URL, http.StatusSeeOther)
	after, err := pair.service.activity.Retention(context.Background())
	if err != nil || after != before {
		t.Fatal("cleanup changed policy", after, err)
	}
	fields := earlyWebForm(t, body, "retention_days")
	fields.Set("retention_days", "17")
	blocker, err := sql.Open("sqlite", filepath.Join(pair.service.Config().StateDir, "activity.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer blocker.Close()
	if _, err = blocker.Exec("CREATE TRIGGER reject_settings BEFORE UPDATE ON history_settings BEGIN SELECT RAISE(ABORT,'test save rejection'); END"); err != nil {
		t.Fatal(err)
	}
	// Use the authenticated browser client to exercise the actual form/CSRF path.
	request, err := http.NewRequest(http.MethodPost, address+"?saved=1", strings.NewReader(fields.Encode()))
	if err != nil {
		t.Fatal(err)
	}
	request.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	request.Header.Set("Origin", pair.web.URL)
	response, err := client.Do(request)
	if err != nil {
		t.Fatal(err)
	}
	defer response.Body.Close()
	result, err := io.ReadAll(response.Body)
	if err != nil {
		t.Fatal(err)
	}
	if response.StatusCode != http.StatusOK || strings.Contains(string(result), "Settings saved.") {
		t.Fatalf("failed save showed success: %d %s", response.StatusCode, result)
	}
	posted := earlyWebForm(t, result, "retention_days")
	if posted.Get("retention_days") != "17" {
		t.Fatal("failed save lost entered value", posted)
	}
}
