package framework

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"reflect"
	"testing"

	"open25.local/open25-fne/internal/activity"
)

func TestActivityAliasesIncludeOnlyReturnedPermittedRadioEntities(t *testing.T) {
	store, err := activity.Open(filepath.Join(t.TempDir(), "activity.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	system := userFixtureSystem()
	system.Radios = append(system.Radios, Radio{RID: 10005, Alias: "Unobserved directory entry", Kind: "subscriber", Enabled: true})
	u := fixtureUser()
	u.Talkgroups = []int{1001}
	s := &Service{config: FileConfig{Role: RoleApplication}, activity: store}
	s.directory.value.Store(&directorySnapshot{System: system, Users: map[string]userRecord{u.ID: {User: u}}})
	for _, event := range []activity.Event{
		{RadioID: 10001, TalkgroupID: 1001, Type: "affiliation", Audience: "operator"},
		{RadioID: 10002, TalkgroupID: 1001, Type: "call_start", CallID: "allowed-call", Audience: "operator"},
		{RadioID: 10003, TalkgroupID: 1001, Type: "status", Audience: "dispatcher"},
		{RadioID: 10004, TalkgroupID: 1002, Type: "affiliation", Audience: "dispatcher"},
		{RadioID: 10004, TalkgroupID: 1002, Type: "call_start", CallID: "denied-call", Audience: "dispatcher"},
	} {
		event.Origin, event.SiteID, event.SourceKind = "site-1", "site-1", "rf"
		if err = store.Append(event); err != nil {
			t.Fatal(err)
		}
	}
	request := httptest.NewRequest(http.MethodGet, "/api/activity", nil)
	reply := httptest.NewRecorder()
	s.activityHTTP(reply, request, Session{UserID: u.ID})
	if reply.Code != http.StatusOK {
		t.Fatal(reply.Code, reply.Body.String())
	}
	var result struct {
		Events  []activity.Event `json:"events"`
		Calls   []activity.Call  `json:"calls"`
		Radios  []activity.Radio `json:"radios"`
		Aliases map[int]string   `json:"radio_aliases"`
	}
	if err = json.Unmarshal(reply.Body.Bytes(), &result); err != nil {
		t.Fatal(err)
	}
	want := map[int]string{10001: "Portable", 10002: "Dispatch", 10003: "Mobile"}
	if !reflect.DeepEqual(result.Aliases, want) {
		t.Fatalf("returned aliases exposed unrelated or unauthorized identities: %+v", result.Aliases)
	}
	if len(result.Events) != 1 || len(result.Calls) != 1 || len(result.Radios) != 1 {
		t.Fatalf("activity scope changed: events=%d calls=%d radios=%d", len(result.Events), len(result.Calls), len(result.Radios))
	}
	for _, row := range result.Calls {
		if row.RadioID == 10004 {
			t.Fatal("blocked call returned")
		}
	}
}

func TestSiteActivityAliasesRespectLocalSiteScope(t *testing.T) {
	store, err := activity.Open(filepath.Join(t.TempDir(), "activity.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	s := &Service{config: FileConfig{Role: RoleSite, InstanceID: "site-1"}, activity: store}
	s.directory.value.Store(&directorySnapshot{System: userFixtureSystem()})
	for _, row := range []struct {
		site  string
		radio int
	}{{"site-1", 10001}, {"site-2", 10003}} {
		for _, kind := range []string{"affiliation", "call_start"} {
			if err = store.Append(activity.Event{Origin: row.site, SiteID: row.site, RadioID: row.radio, TalkgroupID: 1001, Type: kind, CallID: row.site + "-call", Audience: "dispatcher", SourceKind: "rf"}); err != nil {
				t.Fatal(err)
			}
		}
	}
	reply := httptest.NewRecorder()
	s.activityHTTP(reply, httptest.NewRequest(http.MethodGet, "/api/activity", nil), Session{})
	var result struct {
		Aliases map[int]string   `json:"radio_aliases"`
		Calls   []activity.Call  `json:"calls"`
		Radios  []activity.Radio `json:"radios"`
	}
	if err = json.Unmarshal(reply.Body.Bytes(), &result); err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(result.Aliases, map[int]string{10001: "Portable"}) || len(result.Calls) != 1 || len(result.Radios) != 1 {
		t.Fatalf("Site view included another Site's entities: %+v", result)
	}
}
