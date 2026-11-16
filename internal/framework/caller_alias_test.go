package framework

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"reflect"
	"testing"
	"time"

	"open25.local/open25-fne/internal/radio"
)

func TestActivityAliasesIncludeOnlyFreshPermittedPhysicalRadios(t *testing.T) {
	system := userFixtureSystem()
	system.Radios = append(system.Radios, Radio{RID: 10005, Alias: "Unobserved directory entry", Kind: "subscriber", Enabled: true})
	user := fixtureUser()
	user.Talkgroups = []int{1001}
	s := &Service{config: FileConfig{Role: RoleZone}}
	now := time.Now().UTC()
	s.directory.value.Store(&directorySnapshot{System: system, Zone: &ZoneConfig{Sites: []SiteIdentity{{ID: "site-1"}, {ID: "site-2"}}}, Users: map[string]userRecord{user.ID: {User: user}}, Reports: []SiteReport{
		{ID: "site-1", ReceivedAt: now, Radio: &radio.Status{SiteID: "site-1", State: "running", Affiliations: []radio.Affiliation{{RID: 10001, TGID: 1001, At: now}, {RID: 10002, TGID: 1001, At: now}, {RID: 10003, TGID: 1001, At: now}, {RID: 10004, TGID: 1002, At: now}}}},
		{ID: "site-2", ReceivedAt: now.Add(-5 * time.Second), Radio: &radio.Status{SiteID: "site-2", State: "running", Affiliations: []radio.Affiliation{{RID: 10005, TGID: 1001, At: now}}}},
	}})
	reply := httptest.NewRecorder()
	s.activityHTTP(reply, httptest.NewRequest(http.MethodGet, "/api/activity", nil), Session{UserID: user.ID})
	var result struct {
		Radios  []currentRadio  `json:"radios"`
		Aliases map[int]string  `json:"radio_aliases"`
		Events  json.RawMessage `json:"events"`
		Calls   json.RawMessage `json:"calls"`
	}
	if reply.Code != 200 || json.Unmarshal(reply.Body.Bytes(), &result) != nil {
		t.Fatal(reply.Code, reply.Body.String())
	}
	if !reflect.DeepEqual(result.Aliases, map[int]string{10001: "Portable"}) || len(result.Radios) != 1 {
		t.Fatalf("unobserved, stale, nonphysical or forbidden identity exposed: %+v", result)
	}
	if result.Events != nil || result.Calls != nil {
		t.Fatal("current presence loaded historical data")
	}
}

func TestSiteActivityUsesCurrentNativePresenceWithoutHistory(t *testing.T) {
	s, native := operationFixture(t)
	native.mu.Lock()
	native.status.Affiliations = []radio.Affiliation{{RID: 10001, TGID: 1001, At: time.Now().UTC()}}
	native.mu.Unlock()
	// History is optional for this current operational view.
	if err := s.activity.Close(); err != nil {
		t.Fatal(err)
	}
	reply := httptest.NewRecorder()
	s.activityHTTP(reply, httptest.NewRequest("GET", "/api/activity", nil), Session{})
	var result struct {
		Radios []currentRadio `json:"radios"`
	}
	if reply.Code != 200 || json.Unmarshal(reply.Body.Bytes(), &result) != nil || len(result.Radios) != 1 || result.Radios[0].SiteID != "site-1" {
		t.Fatal(reply.Code, reply.Body.String())
	}
	native.mu.Lock()
	native.status.State = "stopped"
	native.mu.Unlock()
	if rows := s.currentRadioPresence(); len(rows) != 0 {
		t.Fatal("stopped native presence remained current", rows)
	}
}
