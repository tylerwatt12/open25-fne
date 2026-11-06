package framework

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"open25.local/open25-fne/internal/activity"
)

func TestHistoryHealthIsAdministratorOnlyAndIndependentOfStorageQueries(t *testing.T) {
	s, user := historyFixture(t)
	// Closing the SQL connection simulates activity storage being unavailable.
	// The in-memory health endpoint must still return its diagnostics.
	if err := s.activity.Close(); err != nil {
		t.Fatal(err)
	}
	if err := s.activity.Append(activity.Event{ID: "failed-write", Origin: "site-1", Type: "status"}); err == nil {
		t.Fatal("closed activity storage accepted a write")
	}
	result := make(chan *httptest.ResponseRecorder, 1)
	go func() { result <- historyGet(s, "/api/history/health", Session{}) }()
	select {
	case reply := <-result:
		var health activity.IngestionHealth
		if reply.Code != http.StatusOK || json.Unmarshal(reply.Body.Bytes(), &health) != nil || health.WriteFailures != 1 || !health.Degraded || health.LastWriteFailureAt == nil {
			t.Fatalf("unavailable storage hid its diagnostics: HTTP%d %s", reply.Code, reply.Body.String())
		}
	case <-time.After(200 * time.Millisecond):
		t.Fatal("health endpoint waited for activity SQL")
	}
	if reply := historyGet(s, "/api/history/health", Session{UserID: user.ID}); reply.Code != http.StatusForbidden {
		t.Fatal("an application user received administrator storage diagnostics")
	}
	w := httptest.NewRecorder()
	s.historyHTTP(w, httptest.NewRequest(http.MethodPost, "/api/history/health", nil), Session{})
	if w.Code != http.StatusMethodNotAllowed {
		t.Fatal("health accepted a mutating request")
	}
	if err := s.activity.Flush(context.Background()); err == nil {
		t.Fatal("closed store appeared writable")
	}
}
