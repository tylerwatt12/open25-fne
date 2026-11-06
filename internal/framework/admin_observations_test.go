package framework

import (
	"bytes"
	"context"
	"net/http"
	"net/url"
	"strings"
	"testing"
	"time"

	"open25.local/open25-fne/internal/activity"
	"open25.local/open25-fne/internal/radio"
)

func TestAdminReportedStatusExposesObservationsOnly(t *testing.T) {
	c := integrationNewCluster(t)
	s := c.system.service
	zone := storeFixtureZone()
	zone.Name = "Zone test"
	zone.Sites[0].AdminURL = c.site.web.URL
	if err := s.writeCache("zone", zone); err != nil {
		t.Fatal(err)
	}
	observed := time.Now().Add(-2 * time.Hour).UTC()
	received := observed.Add(time.Minute)
	relayed := received.Add(time.Minute)
	report := SiteReport{
		ID: zone.Sites[0].ID, Name: "Test Site", Endpoint: "http://private-native-endpoint.invalid", AdminURL: c.site.web.URL,
		ObservedAt: observed, ReceivedAt: received,
		Config: SiteConfig{ID: zone.Sites[0].ID, Modems: []Modem{{ID: "control", Device: "do-not-return-usb-path", Baud: 12345}}, Channels: []Channel{{ID: "control", Identifier: 99}}},
		Radio:  &radio.Status{State: "running", Mode: "hardware", Channels: []radio.ChannelStatus{{ID: "control", Name: "Control", State: "running", Ready: true, TXHz: 443459999, RXHz: 448459999, PowerPercent: 47, RPCPort: 15999, RESTPort: 16001}}, Registrations: []radio.Registration{{RID: 10001, At: observed}}, Affiliations: []radio.Affiliation{{RID: 10001, TGID: 1001, At: observed}}},
	}
	if err := s.writeCache("site_reports", []SiteReport{report}); err != nil {
		t.Fatal(err)
	}
	if err := s.writeCache("zone_report", zoneReport{ID: "zone-1", AdminURL: c.zone.web.URL, ObservedAt: received, ReceivedAt: relayed, Sites: []SiteReport{report}}); err != nil {
		t.Fatal(err)
	}
	before, _ := s.Store.Config()
	client := earlyWebLogin(t, c.system)
	body := string(earlyWebGet(t, client, c.system.web.URL+"/reports?site="+url.QueryEscape(report.ID), http.StatusOK))
	for _, want := range []string{"Reported", "Received by Zone test", "Received by Zone", "Relayed here", webReportTime(observed), webReportTime(received), webReportTime(relayed), "stale", "Channel observations", "Registration", "Affiliation", "10001 / 002711 hex"} {
		if !strings.Contains(body, want) {
			t.Errorf("status missing %q", want)
		}
	}
	for _, forbidden := range []string{"do-not-return-usb-path", "12345", "443459999", "448459999", "15999", "16001", "private-native-endpoint", "name=config", "name=record", "name=tx_frequency_hz", "name=material"} {
		if strings.Contains(body, forbidden) {
			t.Errorf("status exposes configuration/private field %q", forbidden)
		}
	}
	after, _ := s.Store.Config()
	if !bytes.Equal(before, after) {
		t.Fatal("viewing status changed configuration")
	}
	zoneBody := string(earlyWebGet(t, client, c.system.web.URL+"/zone-status", http.StatusOK))
	if !strings.Contains(zoneBody, "Stale") || strings.Contains(zoneBody, "do-not-return-usb-path") {
		t.Fatal("Zone metadata leaked Site configuration or hid stale age")
	}
}

func TestAdminMissingReportsAndReferencesStayReadOnly(t *testing.T) {
	c := integrationNewCluster(t)
	zone := storeFixtureZone()
	zone.Name = "Zone source"
	zone.Sites[0].Endpoint = "do-not-return-site-endpoint"
	if err := c.site.service.writeCache("zone", zone); err != nil {
		t.Fatal(err)
	}
	system := storeFixtureSystem()
	system.Talkgroups[0].Alias = "Reference talkgroup"
	system.Talkgroups[0].SiteIDs = []string{"do-not-return-policy-list"}
	if err := c.site.service.writeCache("system", system); err != nil {
		t.Fatal(err)
	}
	client := earlyWebLogin(t, c.site)
	for _, test := range []struct{ path, want, forbidden string }{
		{"/reference-system", "Reference talkgroup", "do-not-return-policy-list"},
		{"/reference-zone", "Zone source", "do-not-return-site-endpoint"},
	} {
		body := string(earlyWebGet(t, client, c.site.web.URL+test.path, http.StatusOK))
		if !strings.Contains(body, test.want) || !strings.Contains(body, "read-only reference") || strings.Contains(body, test.forbidden) || strings.Contains(body, "name=config") {
			t.Fatalf("reference %s is missing owner/programming or exposes unrelated fields", test.path)
		}
	}
	zoneClient := earlyWebLogin(t, c.zone)
	body := string(earlyWebGet(t, zoneClient, c.zone.web.URL+"/reports", http.StatusOK))
	if !strings.Contains(body, "No report") || strings.Contains(body, ">Offline<") || strings.Contains(body, ">Healthy<") {
		t.Fatal("missing report presented as equipment health")
	}
	for _, path := range []string{"/status", "/reports", "/reference-system", "/reference-zone", "/history", "/topology"} {
		request, err := http.NewRequest(http.MethodPost, c.site.web.URL+path, strings.NewReader("csrf=unused"))
		if err != nil {
			t.Fatal(err)
		}
		request.Header.Set("Content-Type", "application/x-www-form-urlencoded")
		// Exercise the page router directly after the normal session checks.
		_ = request.ParseForm()
		result := &webResult{header: make(http.Header), code: http.StatusOK}
		if !c.site.service.webAdmin(result, request, Session{Username: "administrator"}, strings.TrimPrefix(path, "/")) || result.code != http.StatusMethodNotAllowed {
			t.Fatalf("read-only page %s accepted a submission", path)
		}
	}
}

func TestAdminHistoryConfigurationHasItsOwnPage(t *testing.T) {
	c := integrationNewCluster(t)
	client := earlyWebLogin(t, c.system)
	body := string(earlyWebGet(t, client, c.system.web.URL+"/history", http.StatusOK))
	if strings.Contains(body, "name=retention_days") || strings.Contains(body, "name=\"retention_days\"") || strings.Contains(body, ">Save<") {
		t.Fatal("history observations contain configuration controls")
	}
	settings := earlyWebGet(t, client, c.system.web.URL+"/history-settings", http.StatusOK)
	values := earlyWebForm(t, settings, "retention_days")
	values.Set("retention_days", "13")
	values.Set("max_events", "9007199254740993")
	earlyWebPost(t, client, c.system.web.URL+"/history-settings", values, c.system.web.URL, http.StatusSeeOther)
	settings = earlyWebGet(t, client, c.system.web.URL+"/history-settings", http.StatusOK)
	saved := earlyWebForm(t, settings, "retention_days")
	if saved.Get("retention_days") != "13" || saved.Get("max_events") != "9007199254740993" {
		t.Fatal("separate history configuration did not save")
	}
}

func TestAdminDirectoryParseErrorPreservesHexAndOtherEdits(t *testing.T) {
	c := integrationNewCluster(t)
	client := earlyWebLogin(t, c.system)
	page := earlyWebGet(t, client, c.system.web.URL+"/users/edit", http.StatusOK)
	values := earlyWebForm(t, page, "username")
	values.Set("username", "unsaved-user-text")
	values.Set("radio_id", "10001")
	values.Set("radio_id__base", "hex")
	values.Set("radio_id__hex", "G")
	values.Set("password", "do-not-echo-password")
	request, err := http.NewRequest(http.MethodPost, c.system.web.URL+"/users/edit", strings.NewReader(values.Encode()))
	if err != nil {
		t.Fatal(err)
	}
	request.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	request.Header.Set("Origin", c.system.web.URL)
	response, err := client.Do(request)
	if err != nil {
		t.Fatal(err)
	}
	defer response.Body.Close()
	var result bytes.Buffer
	_, _ = result.ReadFrom(response.Body)
	if response.StatusCode != http.StatusOK {
		t.Fatalf("parse error response: %d", response.StatusCode)
	}
	resubmitted := earlyWebForm(t, result.Bytes(), "username")
	if resubmitted.Get("username") != "unsaved-user-text" || resubmitted.Get("radio_id__hex") != "G" || resubmitted.Get("radio_id__base") != "hex" || strings.Contains(result.String(), "do-not-echo-password") {
		t.Fatal("failed form replaced edits or echoed a password")
	}
}

func TestAdminActivityStatusDoesNotExposeArbitraryEventDetails(t *testing.T) {
	c := integrationNewCluster(t)
	if err := c.system.service.activity.Append(activity.Event{ID: "status-projection-test", Origin: "site-1", Audience: "operator", Type: "radio_status", Message: "Observed status message", RadioID: 10001, TalkgroupID: 1001, SiteID: "site-1", SourceKind: "rf", Details: map[string]any{"future_private_field": "never-expose-arbitrary-details"}}); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if err := c.system.service.activity.Flush(ctx); err != nil {
		t.Fatal(err)
	}
	client := earlyWebLogin(t, c.system)
	body := string(earlyWebGet(t, client, c.system.web.URL+"/activity-status", http.StatusOK))
	if !strings.Contains(body, "Observed status message") || strings.Contains(body, "never-expose-arbitrary-details") || strings.Contains(body, "future_private_field") {
		t.Fatal("activity status omitted the observation or exposed arbitrary details")
	}
	detail := string(earlyWebGet(t, client, c.system.web.URL+"/activity-status?kind=events&id=status-projection-test", http.StatusOK))
	for _, want := range []string{"Selected record", "status-projection-test", "Observed status message"} {
		if !strings.Contains(detail, want) {
			t.Errorf("selected activity detail missing %q", want)
		}
	}
	for _, forbidden := range []string{"View records", "never-expose-arbitrary-details", "future_private_field"} {
		if strings.Contains(detail, forbidden) {
			t.Errorf("selected activity detail exposes generic/private content %q", forbidden)
		}
	}
}

func TestAdminObservationNotationKeepsStringRecordIDs(t *testing.T) {
	var text, number bytes.Buffer
	webProperties(&text, map[string]any{"system_id": "1001"}, []webColumn{{"system_id", "System ID"}})
	webProperties(&number, map[string]any{"system_id": 1001}, []webColumn{{"system_id", "System ID"}})
	if strings.Contains(text.String(), "hex") || !strings.Contains(number.String(), "1001 / 3E9 hex") {
		t.Fatal("string record identity was treated as an on-air number or numeric notation was omitted")
	}
}
