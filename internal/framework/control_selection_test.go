package framework

import (
	"bytes"
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

type selectionTestRadio struct {
	*operationRadio
	targets []string
	err     error
}

func (r *selectionTestRadio) SwitchControl(_ context.Context, target string) error {
	r.targets = append(r.targets, target)
	return r.err
}

func TestSiteControlSwitchOperationOwnsOnlyExplicitRuntimeRequests(t *testing.T) {
	s, original := operationFixture(t)
	s.operationCancel()
	s.operationWG.Wait()
	s.operationCtx, s.operationCancel = context.WithCancel(context.Background())
	runtime := &selectionTestRadio{operationRadio: original}
	s.radio = runtime
	before, _ := s.Store.Config()
	for _, test := range []struct {
		role         Role
		session      Session
		method, body string
		status       int
	}{
		{RoleSite, Session{}, http.MethodGet, "", 405},
		{RoleZone, Session{}, http.MethodPost, `{"channel_id":"voice-1"}`, 403},
		{RoleSite, Session{UserID: "operator"}, http.MethodPost, `{"channel_id":"voice-1"}`, 403},
		{RoleSite, Session{}, http.MethodPost, `{"channel_id":`, 400},
		{RoleSite, Session{}, http.MethodPost, `{"channel_id":"voice-1"}`, 202},
	} {
		s.mu.Lock()
		s.config.Role = test.role
		s.mu.Unlock()
		request := httptest.NewRequest(test.method, "/api/radio/switch-control", strings.NewReader(test.body))
		request.Header.Set("Content-Type", "application/json")
		response := httptest.NewRecorder()
		if !s.operationsHTTP(response, request, test.session) || response.Code != test.status {
			t.Fatalf("role %s method %s: %d %s", test.role, test.method, response.Code, response.Body.String())
		}
	}
	if len(runtime.targets) != 1 || runtime.targets[0] != "voice-1" {
		t.Fatal("non-explicit or foreign-service request reached radio runtime", runtime.targets)
	}
	runtime.err = errors.New("selected resource is occupied")
	response := httptest.NewRecorder()
	request := httptest.NewRequest(http.MethodPost, "/api/radio/switch-control", strings.NewReader(`{"channel_id":"voice-2"}`))
	request.Header.Set("Content-Type", "application/json")
	s.operationsHTTP(response, request, Session{})
	if response.Code != http.StatusServiceUnavailable || !strings.Contains(response.Body.String(), runtime.err.Error()) {
		t.Fatal("runtime refusal was reported as switch success", response.Body.String())
	}
	after, _ := s.Store.Config()
	if !bytes.Equal(before, after) {
		t.Fatal("manual operation changed saved assignments or policy")
	}
}
