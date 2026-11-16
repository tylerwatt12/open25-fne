package framework

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"strings"
	"testing"
	"time"
)

func TestAdministratorRevocationDeliveryHasSeparateDurableWriteBudget(t *testing.T) {
	c := integrationNewCluster(t)
	s := c.site.service
	budgets := map[string]time.Duration{}
	s.client = &http.Client{Timeout: 5 * time.Second, Transport: integrationRoundTripFunc(func(r *http.Request) (*http.Response, error) {
		deadline, ok := r.Context().Deadline()
		if !ok {
			return nil, fmt.Errorf("upstream request has no bounded deadline")
		}
		budgets[r.URL.Path] = time.Until(deadline)
		return &http.Response{StatusCode: http.StatusOK, Header: http.Header{}, Body: io.NopCloser(strings.NewReader(`{"revoked":true}`)), Request: r}, nil
	})}
	complete, err := s.signOutSession(Session{FamilyID: strings.Repeat("a", 64), Expires: time.Now().Add(time.Hour)})
	if err != nil || !complete {
		t.Fatal("durable administrator delivery did not complete", complete, err)
	}
	var status sessionFamilyStatus
	if err := s.upstreamRequest(context.Background(), http.MethodPost, "/internal/sso/status", map[string]string{"family_id": strings.Repeat("a", 64)}, &status); err != nil {
		t.Fatal(err)
	}
	if got := budgets["/internal/sso/revoke"]; got < 25*time.Second || got > revocationDeliveryBudget {
		t.Fatal("durable revocation still uses the short control delivery deadline", got)
	}
	if got := budgets["/internal/sso/status"]; got <= 0 || got > 5*time.Second || s.client.Timeout != 5*time.Second {
		t.Fatal("administrator delivery changed ordinary status/control deadlines", got, s.client.Timeout)
	}
}
