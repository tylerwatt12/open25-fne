package framework

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"

	"open25.local/open25-fne/internal/activity"
)

func TestHistoryAliasesAreLimitedToReturnedPermittedEntities(t *testing.T) {
	s, _ := historyFixture(t)
	view := s.view()
	view.System.Radios = append(view.System.Radios, Radio{RID: 10005, Alias: "Unobserved directory name", Kind: "subscriber", Enabled: true})
	now := time.Now().UTC()
	for _, event := range []activity.Event{
		{RadioID: 10001, TalkgroupID: 1001, CallID: "named-call", Type: "call_start"},
		{RadioID: 10003, TalkgroupID: 1001, Type: "status"},
		{RadioID: 10004, TalkgroupID: 1002, CallID: "blocked-call", Type: "call_start"},
		{RadioID: 10002, TalkgroupID: 1001, Type: "location", Details: map[string]any{"fix_at": now, "latitude": 41.5, "longitude": -81.7, "accuracy": 5}},
		{RadioID: 10003, TalkgroupID: 1001, Type: "location", Details: map[string]any{"fix_at": now, "latitude": 41.5, "longitude": -81.7, "accuracy": 5}},
	} {
		event.ID = activity.NewID()
		event.Origin, event.SiteID, event.Audience, event.SourceKind = "site-1", "site-1", "dispatcher", "application"
		if err := s.activity.Append(context.Background(), event); err != nil {
			t.Fatal(err)
		}
	}
	for _, check := range []struct {
		path    string
		aliases map[int]string
	}{
		{"calls", map[int]string{10001: "Portable", 10004: "Second console"}},
		{"events", map[int]string{10001: "Portable", 10002: "Dispatch", 10003: "Mobile", 10004: "Second console"}},
		{"locations", map[int]string{10002: "Dispatch", 10003: "Mobile"}},
	} {
		w := historyGet(s, "/api/history/"+check.path, Session{})
		var reply struct {
			Aliases map[int]string `json:"radio_aliases"`
		}
		if w.Code != 200 || json.Unmarshal(w.Body.Bytes(), &reply) != nil || !reflect.DeepEqual(reply.Aliases, check.aliases) {
			t.Fatalf("%s alias scope differs from returned records: HTTP%d %+v", check.path, w.Code, reply.Aliases)
		}
		if strings.Contains(w.Body.String(), "Unobserved directory name") {
			t.Fatal("page exposed blocked or unobserved radio names")
		}
	}
}

func historyFixture(t *testing.T) (*Service, User) {
	t.Helper()
	store, err := activity.OpenArchive(filepath.Join(t.TempDir(), "activity.db"))
	if err != nil {
		t.Fatal(err)
	}
	waitHistoryInitialization(t, store)
	t.Cleanup(func() { store.Close() })
	system := userFixtureSystem()
	u := fixtureUser()
	u.Talkgroups = []int{1001}
	s := &Service{activity: store, config: FileConfig{Role: RoleSystem, InstanceID: "system-1"}}
	s.directory.value.Store(&directorySnapshot{System: system, Users: map[string]userRecord{u.ID: {User: u}}})
	return s, u
}
func historyGet(s *Service, path string, session Session) *httptest.ResponseRecorder {
	w := httptest.NewRecorder()
	s.historyHTTP(w, httptest.NewRequest(http.MethodGet, path, nil), session)
	return w
}

func TestSystemHistoryHTTPFiltersDestinationsCSVAndStatistics(t *testing.T) {
	s, _ := historyFixture(t)
	at := time.Now().UTC().Add(-time.Minute)
	for _, tg := range []int{1001, 1002} {
		e := activity.Event{ID: "event-" + string(rune(tg)), Origin: "site-1", SiteID: "site-1", RadioID: 10001, TalkgroupID: tg, CallID: "multiselect", Type: "call_start", At: at, Message: "=untrusted spreadsheet formula", Audience: "dispatcher", SourceKind: "rf"}
		if err := s.activity.Append(context.Background(), e); err != nil {
			t.Fatal(err)
		}
	}
	w := historyGet(s, "/api/history/calls", Session{})
	var page activity.CallPage
	if w.Code != 200 || json.Unmarshal(w.Body.Bytes(), &page) != nil || len(page.Calls) != 1 || len(page.Calls[0].Destinations) != 2 || page.Calls[0].Destinations[0].TalkgroupID != 1001 {
		t.Fatalf("call history leaked scope: %s", w.Body.String())
	}
	w = historyGet(s, "/api/history/events?format=csv", Session{})
	if w.Code != 200 || !strings.HasPrefix(w.Header().Get("Content-Type"), "text/csv") || !strings.Contains(w.Body.String(), "'=untrusted spreadsheet formula") || !strings.Contains(w.Body.String(), ",1002,") {
		t.Fatalf("CSV scope or spreadsheet protection failed: %s", w.Body.String())
	}
	w = historyGet(s, "/api/history/statistics", Session{})
	var stats activity.Statistics
	if w.Code != 200 || json.Unmarshal(w.Body.Bytes(), &stats) != nil || stats.Calls != 1 || stats.SiteDeliveries != 2 || stats.Events != 2 || stats.SpeechMS != nil {
		t.Fatalf("scoped totals incorrect: %s", w.Body.String())
	}
	s.config.Role, s.config.InstanceID = RoleSite, "site-2"
	w = historyGet(s, "/api/history/calls?site_id=site-1", Session{})
	if w.Code != http.StatusNotFound {
		t.Fatal("source exposed a historical endpoint")
	}
}

func TestHistoryHTTPRejectsMalformedFiltersAndNonAdminRetention(t *testing.T) {
	s, u := historyFixture(t)
	for _, query := range []string{"limit=0", "limit=1001", "radio_id=no", "since=yesterday", "since=2026-10-30T00:00:00Z&until=2026-10-29T00:00:00Z", "cursor=garbage", "audience=everything", "format=xml"} {
		w := historyGet(s, "/api/history/events?"+query, Session{})
		if w.Code != 400 {
			t.Fatalf("accepted malformed filter %s: %d", query, w.Code)
		}
	}
	w := historyGet(s, "/api/history/settings", Session{UserID: u.ID})
	if w.Code != 403 {
		t.Fatal("operator received retention controls")
	}
	w = historyGet(s, "/api/history/settings", Session{})
	if w.Code != 200 || !strings.Contains(w.Body.String(), `"max_events":100000`) {
		t.Fatal("default retention missing")
	}
}

func TestHistoricalEndpointsBelongOnlyToSystemAdministrators(t *testing.T) {
	for _, role := range []Role{RoleSystem, RoleZone, RoleSite, RoleApplication} {
		s := &Service{config: FileConfig{Role: role}}
		for _, path := range []string{"events", "calls", "locations", "statistics", "settings", "cleanup", "health"} {
			route := "/api/history/" + path
			if w := historyGet(s, route, Session{UserID: "operator"}); w.Code != http.StatusForbidden {
				t.Fatalf("%s operator %s: HTTP%d", role, route, w.Code)
			}
			if role != RoleSystem && path != "health" {
				if w := historyGet(s, route, Session{}); w.Code != http.StatusNotFound {
					t.Fatalf("%s source %s: HTTP%d", role, route, w.Code)
				}
			}
		}
	}
}

func TestAcceptedApplicationLocationIsRetainedInCentralHistory(t *testing.T) {
	c := integrationNewCluster(t)
	integrationSyncAll(t, c)
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	fix := time.Now().UTC()
	e := activity.Event{ID: "actual-mobile-fix", At: fix, Origin: "application-1", RadioID: 10002, TalkgroupID: 1001, Type: "location", Audience: "dispatcher", SourceKind: "application", Details: map[string]any{"latitude": 41.5, "longitude": -81.7, "accuracy": 6.0, "fix_at": fix, "scope_id": "site-1"}}
	e.SiteID = "site-1"
	c.dispatch.service.emit(e)
	if err := c.dispatch.service.activity.Flush(ctx); err != nil {
		t.Fatal(err)
	}
	c.dispatch.service.uploadActivity(ctx)
	page, err := c.system.service.activity.LocationHistory(activity.Query{}, time.Now())
	if err != nil || len(page.Locations) != 1 || page.Locations[0].SourceKind != "application" || page.Locations[0].Latitude != 41.5 || page.Locations[0].SiteID != "site-1" || !page.Locations[0].FixAt.Equal(fix) {
		t.Fatalf("central history changed actual fix: %+v %v", page, err)
	}
}

func TestLocationMutationEmitsCoordinatesForHistory(t *testing.T) {
	s, _, mobile := operatorFixture(t, RoleZone)
	s.activity, _ = activity.Open(filepath.Join(t.TempDir(), "activity.db"))
	waitHistoryInitialization(t, s.activity)
	defer s.activity.Close()
	now := time.Now().UTC()
	operatorApply(t, s, "location", OperatorInput{Action: "report", TalkgroupID: 1001, Location: &LocationInput{Latitude: 41.5, Longitude: -81.7, Accuracy: 5, FixAt: now}}, mobile)
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	if err := s.activity.Flush(ctx); err != nil {
		t.Fatal(err)
	}
	events, err := s.activity.Pending(ctx, activity.EventBatchLimit)
	if err != nil || len(events) != 1 || events[0].Type != "location" || events[0].RadioID != mobile.actor.RadioID || events[0].Details["accuracy"] != float64(5) {
		t.Fatalf("accepted fix did not enter its durable source outbox: %+v %v", events, err)
	}
}
