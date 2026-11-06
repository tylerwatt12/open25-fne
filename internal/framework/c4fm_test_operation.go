package framework

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"time"

	"open25.local/open25-fne/internal/radio"
)

type c4fmTestRuntime interface {
	StartC4FMTest(context.Context, radio.Profile, radio.C4FMTestRequest) error
	StopC4FMTest(context.Context) error
	C4FMTestStatus() radio.C4FMTestStatus
}

func (s *Service) c4fmTestHTTP(w http.ResponseWriter, r *http.Request, session Session) {
	if session.UserID != "" || s.Config().Role != RoleSite {
		fail(w, http.StatusForbidden, errors.New("transmitter testing belongs to Site Administration"))
		return
	}
	path := r.URL.Path
	if (path == "/api/radio/c4fm-test" && r.Method != http.MethodGet) ||
		(path != "/api/radio/c4fm-test" && r.Method != http.MethodPost) {
		w.WriteHeader(http.StatusMethodNotAllowed)
		return
	}
	if path != "/api/radio/c4fm-test" && path != "/api/radio/c4fm-test/start" && path != "/api/radio/c4fm-test/stop" {
		w.WriteHeader(http.StatusNotFound)
		return
	}
	runtime, ok := s.radio.(c4fmTestRuntime)
	if !ok {
		fail(w, http.StatusServiceUnavailable, errors.New("C4FM transmitter testing is unavailable at this Site"))
		return
	}
	if path == "/api/radio/c4fm-test" {
		respond(w, http.StatusOK, runtime.C4FMTestStatus())
		return
	}
	if path == "/api/radio/c4fm-test/stop" {
		var input struct{}
		if err := readJSON(w, r, &input); err != nil {
			fail(w, http.StatusBadRequest, err)
			return
		}
		ctx, cancel := context.WithTimeout(r.Context(), 15*time.Second)
		defer cancel()
		if err := runtime.StopC4FMTest(ctx); err != nil {
			fail(w, http.StatusServiceUnavailable, err)
			return
		}
		respond(w, http.StatusOK, runtime.C4FMTestStatus())
		return
	}
	var input radio.C4FMTestRequest
	if err := readJSON(w, r, &input); err != nil {
		fail(w, http.StatusBadRequest, err)
		return
	}
	// Only Site-owned channel/modem programming is needed for this signal.
	raw, err := s.Store.Config()
	if err != nil {
		fail(w, http.StatusInternalServerError, err)
		return
	}
	var site radio.Site
	if err = json.Unmarshal(raw, &site); err != nil {
		fail(w, http.StatusInternalServerError, err)
		return
	}
	if err = runtime.StartC4FMTest(s.operationCtx, radio.Profile{Site: site}, input); err != nil {
		fail(w, http.StatusServiceUnavailable, err)
		return
	}
	respond(w, http.StatusAccepted, runtime.C4FMTestStatus())
}
