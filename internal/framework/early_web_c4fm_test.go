package framework

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
	"time"

	"open25.local/open25-fne/internal/radio"
)

type webC4FMRuntime struct {
	*operationRadio
	testStatus radio.C4FMTestStatus
	testStarts []radio.C4FMTestRequest
	testStops  int
	testError  error
}

func (r *webC4FMRuntime) StartC4FMTest(_ context.Context, _ radio.Profile, request radio.C4FMTestRequest) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.testStarts = append(r.testStarts, request)
	return r.testError
}

func (r *webC4FMRuntime) StopC4FMTest(context.Context) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.testStops++
	return r.testError
}

func (r *webC4FMRuntime) C4FMTestStatus() radio.C4FMTestStatus {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.testStatus
}

func webC4FMService(t *testing.T) (*Service, *webC4FMRuntime) {
	t.Helper()
	var runtime *webC4FMRuntime
	s, _ := operationConfiguredFixture(t, func(s *Service) {
		original := s.radio.(*operationRadio)
		original.status.State = "stopped"
		runtime = &webC4FMRuntime{operationRadio: original, testStatus: radio.C4FMTestStatus{State: "idle"}}
		s.radio = runtime
		s.radioRunActive = false
	})
	return s, runtime
}

func webC4FMAPI(t *testing.T, s *Service, method, path, body string, session Session) *httptest.ResponseRecorder {
	t.Helper()
	request := httptest.NewRequest(method, path, strings.NewReader(body))
	request.Header.Set("Content-Type", "application/json")
	response := httptest.NewRecorder()
	if !s.operationsHTTP(response, request, session) {
		t.Fatalf("C4FM API route not handled: %s %s", method, path)
	}
	return response
}

func TestC4FMTestAPIBelongsToSiteAdministrator(t *testing.T) {
	for _, role := range []Role{RoleSystem, RoleZone, RoleSite, RoleApplication} {
		for _, user := range []string{"", "operator-one"} {
			t.Run(string(role)+"/"+user, func(t *testing.T) {
				s, runtime := webC4FMService(t)
				s.mu.Lock()
				s.config.Role = role
				s.mu.Unlock()
				for _, action := range []struct{ method, path, body string }{
					{http.MethodGet, "/api/radio/c4fm-test", ""},
					{http.MethodPost, "/api/radio/c4fm-test/start", `{"channel_id":"channel-voice","duration_seconds":12}`},
					{http.MethodPost, "/api/radio/c4fm-test/stop", `{}`},
				} {
					response := webC4FMAPI(t, s, action.method, action.path, action.body, Session{UserID: user})
					if role != RoleSite || user != "" {
						if response.Code != http.StatusForbidden {
							t.Fatalf("operation escaped Site/admin boundary: HTTP %d %s", response.Code, response.Body.String())
						}
					} else if response.Code < 200 || response.Code >= 300 {
						t.Fatalf("Site administrator operation rejected: HTTP %d %s", response.Code, response.Body.String())
					}
				}
				runtime.mu.Lock()
				defer runtime.mu.Unlock()
				if (role != RoleSite || user != "") && (len(runtime.testStarts) != 0 || runtime.testStops != 0) {
					t.Fatal("forbidden API request reached the transmitter adapter")
				}
			})
		}
	}
}

func TestC4FMTestAPIReportsAdapterStatusAndActualFailures(t *testing.T) {
	s, runtime := webC4FMService(t)
	runtime.mu.Lock()
	runtime.testStatus = radio.C4FMTestStatus{State: "stopped", ChannelID: "channel-voice", TXHz: 443450001, TXObserved: true, StopConfirmed: true, FinishedAt: time.Date(2026, 11, 4, 20, 0, 0, 0, time.UTC)}
	runtime.testError = errors.New("selected modem is unavailable")
	runtime.mu.Unlock()
	response := webC4FMAPI(t, s, http.MethodGet, "/api/radio/c4fm-test", "", Session{})
	var status radio.C4FMTestStatus
	if response.Code != http.StatusOK || json.Unmarshal(response.Body.Bytes(), &status) != nil || status.State != "stopped" || !status.TXObserved || !status.StopConfirmed || status.TXHz != 443450001 {
		t.Fatalf("actual adapter observation lost: HTTP %d %s", response.Code, response.Body.String())
	}
	for _, action := range []struct{ path, body string }{
		{"/api/radio/c4fm-test/start", `{"channel_id":"channel-voice","duration_seconds":12}`},
		{"/api/radio/c4fm-test/stop", `{}`},
	} {
		response = webC4FMAPI(t, s, http.MethodPost, action.path, action.body, Session{})
		if response.Code != http.StatusServiceUnavailable || !strings.Contains(response.Body.String(), "selected modem is unavailable") {
			t.Fatalf("actual operation failure was hidden: HTTP %d %s", response.Code, response.Body.String())
		}
	}
}

func TestC4FMTestAPIRejectsWrongMethodsAndUnreadableRequestsWithoutCommands(t *testing.T) {
	s, runtime := webC4FMService(t)
	for _, action := range []struct {
		method, path, body string
		code               int
	}{
		{http.MethodGet, "/api/radio/c4fm-test/start", "", http.StatusMethodNotAllowed},
		{http.MethodGet, "/api/radio/c4fm-test/stop", "", http.StatusMethodNotAllowed},
		{http.MethodPost, "/api/radio/c4fm-test", `{}`, http.StatusMethodNotAllowed},
		{http.MethodPost, "/api/radio/c4fm-test/unknown", `{}`, http.StatusNotFound},
		{http.MethodPost, "/api/radio/c4fm-test/start", `{"channel_id":"voice","duration_seconds":12.5}`, http.StatusBadRequest},
		{http.MethodPost, "/api/radio/c4fm-test/start", `{"channel_id":`, http.StatusBadRequest},
		{http.MethodPost, "/api/radio/c4fm-test/stop", `{"unexpected":"value"}`, http.StatusBadRequest},
	} {
		response := webC4FMAPI(t, s, action.method, action.path, action.body, Session{})
		if response.Code != action.code {
			t.Fatalf("%s %s: HTTP %d; want %d; %s", action.method, action.path, response.Code, action.code, response.Body.String())
		}
	}
	runtime.mu.Lock()
	defer runtime.mu.Unlock()
	if len(runtime.testStarts) != 0 || runtime.testStops != 0 {
		t.Fatal("wrong-method or unreadable request reached the transmitter adapter")
	}
}

func TestC4FMTestFormRedirectPreventsReplayOnReloadAndRefresh(t *testing.T) {
	s, runtime := webC4FMService(t)
	session := Session{Username: "administrator", CSRF: "test-c4fm-csrf"}
	for _, action := range []string{"start", "stop"} {
		form := url.Values{"action": {action}}
		if action == "start" {
			form.Set("channel_id", "channel-voice")
			form.Set("duration_seconds", "12")
		}
		request := httptest.NewRequest(http.MethodPost, "/c4fm-test", strings.NewReader(form.Encode()))
		request.Header.Set("Content-Type", "application/x-www-form-urlencoded")
		response := httptest.NewRecorder()
		s.webC4FMTest(response, request, session)
		if response.Code != http.StatusSeeOther || !strings.HasPrefix(response.Header().Get("Location"), "/c4fm-test") {
			t.Fatalf("accepted %s did not redirect: HTTP %d %s", action, response.Code, response.Body.String())
		}
		for i := 0; i < 3; i++ {
			for _, path := range []string{response.Header().Get("Location"), "/c4fm-test"} {
				reload := httptest.NewRecorder()
				s.webC4FMTest(reload, httptest.NewRequest(http.MethodGet, path, nil), session)
				if reload.Code != http.StatusOK {
					t.Fatalf("test reload: HTTP %d", reload.Code)
				}
			}
		}
	}
	runtime.mu.Lock()
	defer runtime.mu.Unlock()
	if len(runtime.testStarts) != 1 || runtime.testStarts[0].ChannelID != "channel-voice" || runtime.testStarts[0].DurationSeconds != 12 || runtime.testStops != 1 {
		t.Fatalf("GET/reload replayed accepted form work: starts %+v stops %d", runtime.testStarts, runtime.testStops)
	}
}

func TestC4FMTestPageReadsWithoutSavingOrStartingRadio(t *testing.T) {
	c := integrationNewCluster(t)
	s := c.site.service
	var site SiteConfig
	if err := s.owned(&site); err != nil {
		t.Fatal(err)
	}
	site.Channels = []Channel{
		{ID: "control-one", Name: "Control <&>", Role: "control", ModemID: "control-modem", Enabled: true},
		{ID: "disabled-voice", Name: "Disabled voice", Role: "voice", ModemID: "voice-modem", Enabled: false},
	}
	if err := s.Store.SaveConfig(storeDoc(t, site)); err != nil {
		t.Fatal(err)
	}
	before, err := s.Store.Config()
	if err != nil {
		t.Fatal(err)
	}
	client := earlyWebLogin(t, c.site)
	page := earlyWebGet(t, client, c.site.web.URL+"/c4fm-test", http.StatusOK)
	form := earlyWebForm(t, page, "duration_seconds")
	if form.Get("duration_seconds") != "" || form.Get("channel_id") != "" || form.Get("csrf") == "" {
		t.Fatal("test form acquired starting values or omitted CSRF")
	}
	for _, want := range []string{"C4FM transmitter test", `data-chooser-kind="channels"`, "name=action value=start", "name=action value=stop", "Refresh status", "class=ts-editor", "type=number", "external receiving or test equipment"} {
		if !strings.Contains(string(page), want) {
			t.Fatalf("test page missing %q", want)
		}
	}
	for _, forbidden := range []string{"http-equiv=refresh", "class=admin-record-layout", " disabled", "name=config", "name=power_percent", "name=tx_frequency_hz"} {
		if strings.Contains(string(page), forbidden) {
			t.Fatalf("test page includes a settings editor, filtered choice or background refresh: %q", forbidden)
		}
	}
	// Reopening with form preferences remains read-only.
	earlyWebGet(t, client, c.site.web.URL+"/c4fm-test?channel_id=disabled-voice&duration_seconds=12&accepted=start", http.StatusOK)
	after, _ := s.Store.Config()
	if !bytes.Equal(before, after) || s.radioStatus().State != "stopped" {
		t.Fatal("opening or refreshing the test page changed configuration or started radio")
	}
	for _, pair := range []*integrationService{c.system, c.zone, c.dispatch} {
		other := earlyWebLogin(t, pair)
		earlyWebGet(t, other, pair.web.URL+"/c4fm-test", http.StatusNotFound)
	}
}

func TestC4FMTestFormRetainsSubmittedSelectionAndParsingErrors(t *testing.T) {
	c := integrationNewCluster(t)
	s := c.site.service
	request := httptest.NewRequest(http.MethodPost, "/c4fm-test", strings.NewReader(url.Values{
		"action": {"start"}, "channel_id": {"missing<&>"}, "duration_seconds": {"12.5"},
	}.Encode()))
	request.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	response := httptest.NewRecorder()
	s.webC4FMTest(response, request, Session{Username: "administrator", CSRF: "c4fm-form-csrf"})
	page := response.Body.String()
	for _, want := range []string{"could not read duration as a whole number of seconds", `value="12.5"`, `value="missing&lt;&amp;&gt;"`, "c4fm-form-csrf"} {
		if !strings.Contains(page, want) {
			t.Fatalf("failed operation input was lost: %q", want)
		}
	}
	if response.Header().Get("Location") != "" || s.radioStatus().State != "stopped" {
		t.Fatal("a parsing error redirected or started radio")
	}
}

func TestC4FMTestStatusUsesExactMHzAndOnlySelectedObservationFields(t *testing.T) {
	var output bytes.Buffer
	webC4FMTestStatus(&output, map[string]any{
		"state": "fault", "channel_name": "Test <channel>", "channel_id": "voice-one", "modem_id": "modem-one",
		"tx_frequency_hz": json.Number("9223372036854775807"), "power_percent": json.Number("47"),
		"duration_seconds": json.Number("12"), "pattern": "P25 C4FM test", "tx_observed": true, "stop_confirmed": false,
		"error": "Stop <unconfirmed>", "private_secret": "never-display-this", "key_material": "also-not-page-data",
	})
	for _, want := range []string{"Configured transmit frequency (MHz)", "9223372036854.775807", "Configured power (%)", "Test &lt;channel&gt;", "Modem TX observed", "Modem stop confirmed", "Stop &lt;unconfirmed&gt;"} {
		if !strings.Contains(output.String(), want) {
			t.Fatalf("test observation missing or imprecise: %q", want)
		}
	}
	for _, forbidden := range []string{"never-display-this", "also-not-page-data", "name=", "Clean signal", "Measured power"} {
		if strings.Contains(output.String(), forbidden) {
			t.Fatalf("status includes unrelated data or an invented measurement: %q", forbidden)
		}
	}
}

func TestC4FMTransmitTestNavigationBelongsToSiteOperations(t *testing.T) {
	if adminPageGroup("c4fm-test") != "operations" {
		t.Fatal("test page is not in Operations")
	}
	for _, role := range []Role{RoleSite, RoleSystem, RoleZone, RoleApplication} {
		found := false
		for _, category := range adminCategories(role, "operations") {
			for _, link := range category.Links {
				found = found || link.Path == "c4fm-test"
			}
		}
		if found != (role == RoleSite) {
			t.Fatalf("C4FM test navigation at wrong service: %s", role)
		}
	}
}
