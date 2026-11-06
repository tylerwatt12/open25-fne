package framework

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"testing"

	"open25.local/open25-fne/internal/activity"
)

func TestHistoryStorageRequestCancellationReturnsUnavailableWithoutSaving(t *testing.T) {
	for _, method := range []string{http.MethodGet, http.MethodPut} {
		t.Run(method, func(t *testing.T) {
			store, err := activity.Open(filepath.Join(t.TempDir(), "activity.db"))
			if err != nil {
				t.Fatal(err)
			}
			defer store.Close()
			s := &Service{config: FileConfig{Role: RoleSystem, InstanceID: "system-1"}, activity: store}
			before, err := store.Retention(context.Background())
			if err != nil {
				t.Fatal(err)
			}
			changed := before
			changed.Days = 1
			raw, _ := json.Marshal(changed)
			ctx, cancel := context.WithCancel(context.Background())
			cancel()
			r := httptest.NewRequest(method, "/api/history/settings", bytes.NewReader(raw)).WithContext(ctx)
			r.Header.Set("Content-Type", "application/json")
			w := httptest.NewRecorder()
			s.historyHTTP(w, r, Session{})
			if w.Code != http.StatusServiceUnavailable {
				t.Fatalf("canceled storage request returned HTTP%d: %s", w.Code, w.Body.String())
			}
			after, err := store.Retention(context.Background())
			if err != nil || after != before {
				t.Fatal("canceled request changed saved retention", after, err)
			}
		})
	}
}
