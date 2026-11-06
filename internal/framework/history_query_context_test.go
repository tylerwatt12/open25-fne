package framework

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestHistoryPageAndCSVReadsHonorCancelledHTTPRequest(t *testing.T) {
	for _, name := range []string{"events", "calls", "locations"} {
		for _, format := range []string{"", "?format=csv"} {
			t.Run(name+format, func(t *testing.T) {
				s, _ := historyFixture(t)
				ctx, cancel := context.WithCancel(context.Background())
				cancel()
				r := httptest.NewRequest(http.MethodGet, "/api/history/"+name+format, nil).WithContext(ctx)
				w := httptest.NewRecorder()
				s.historyHTTP(w, r, Session{})
				if w.Code < 400 || !strings.Contains(w.Body.String(), "context canceled") || strings.Contains(w.Header().Get("Content-Type"), "text/csv") {
					t.Fatalf("cancelled request returned a successful history page/export: HTTP%d %s", w.Code, w.Body.String())
				}
				// Cancellation belongs to this read, not the store's lifetime.
				w = historyGet(s, "/api/history/"+name+format, Session{})
				if w.Code != http.StatusOK {
					t.Fatalf("cancelled request prevented subsequent history: HTTP%d %s", w.Code, w.Body.String())
				}
			})
		}
	}
}
