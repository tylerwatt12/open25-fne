package radio

import (
	"context"
	"encoding/csv"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"reflect"
	"slices"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

func TestUserAliasRepresentation(t *testing.T) {
	for _, name := range []string{"A", "12345678901234", "Engine, 12", "Desk \"A\"", " Leading", " "} {
		if !IsValidUserAlias(name) {
			t.Errorf("representable name rejected: %q", name)
		}
	}
	for _, name := range []string{"", "123456789012345", "Café", "line\nbreak", "null\x00", "tab\t", "delete\x7f"} {
		if IsValidUserAlias(name) {
			t.Errorf("unrepresentable name accepted: %q", name)
		}
	}
}

func TestUserAliasOmissionReportsOnlyNewUnusableNames(t *testing.T) {
	c := &Controller{events: make(chan Event, 4)}
	p := fixtureProfile()
	p.System.Radios[0].Alias = "Old name exceeding fourteen"
	c.reportUserAliasOmissions(p, nil)
	if len(c.events) != 1 || (<-c.events).RID != 10001 {
		t.Fatal("initial unusable name was not reported once")
	}
	next := cloneProfile(p)
	next.System.Radios[1].Alias = "New desk"
	c.reportUserAliasOmissions(next, radioNames(p.System))
	if len(c.events) != 0 {
		t.Fatal("unrelated rename repeated a legacy omission warning")
	}
}

func TestUserAliasCSVPreservesExactNamesAndStoredConfiguration(t *testing.T) {
	p := fixtureProfile()
	p.System.Radios = []Radio{{RID: 10001, Enabled: true, Alias: "Engine, 12"}, {RID: 10002, Alias: "Desk \"A\""}, {RID: 10003, Enabled: true, Alias: "Too long for air"}, {RID: 10004, Enabled: true, Alias: "Café\nnext"}}
	original, _ := json.Marshal(p)
	reader := csv.NewReader(strings.NewReader(RenderLookups(p)["rid_acl.dat"]))
	reader.Comment = '#'
	rows, err := reader.ReadAll()
	if err != nil {
		t.Fatal(err)
	}
	want := [][]string{{"10001", "1", "Engine, 12", "", ""}, {"10002", "0", "Desk \"A\"", "", ""}, {"10003", "1", "", "", ""}, {"10004", "1", "", "", ""}}
	after, _ := json.Marshal(p)
	if !reflect.DeepEqual(rows, want) || string(after) != string(original) {
		t.Fatalf("native serialization changed names/admission or saved values: rows=%q", rows)
	}
}

func appliedUserAliases(t *testing.T, c *Controller, channel string) []userAlias {
	t.Helper()
	c.mu.Lock()
	dir := c.rootDir
	c.mu.Unlock()
	raw, err := os.ReadFile(filepath.Join(dir, channel, "aliases-applied.json"))
	if err != nil {
		t.Fatal(err)
	}
	var request struct {
		Aliases []userAlias `json:"aliases"`
	}
	if err := json.Unmarshal(raw, &request); err != nil || request.Aliases == nil {
		t.Fatalf("native snapshot missing: %s %v", raw, err)
	}
	return request.Aliases
}

func TestUserAliasRefreshFailureRetainsOldProfileAndCanRetry(t *testing.T) {
	c := testController(t, nil)
	p := fixtureProfile()
	if err := c.Start(context.Background(), p, "null"); err != nil {
		t.Fatal(err)
	}
	c.mu.Lock()
	reject := filepath.Join(c.rootDir, "control-1", "reject-aliases")
	c.mu.Unlock()
	if err := os.WriteFile(reject, nil, 0600); err != nil {
		t.Fatal(err)
	}
	next := cloneProfile(p)
	next.System.Radios[0].Alias = "Replacement"
	if err := c.ApplyPolicy(context.Background(), next); !errors.Is(err, ErrUserAliasRefresh) {
		t.Fatalf("failed native update did not preserve its error classification: %v", err)
	}
	c.mu.Lock()
	oldName := c.profile.System.Radios[0].Alias
	c.mu.Unlock()
	if oldName != p.System.Radios[0].Alias || c.Status().State != "running" {
		t.Fatal("failed display update changed accepted policy or stopped RF")
	}
	if err := os.Remove(reject); err != nil {
		t.Fatal(err)
	}
	if err := c.ApplyPolicy(context.Background(), next); err != nil {
		t.Fatal(err)
	}
	for _, channel := range []string{"control-1", "voice-1"} {
		if got := appliedUserAliases(t, c, channel); !slices.Contains(got, userAlias{10001, "Replacement"}) {
			t.Fatalf("retry left %s with old aliases: %v", channel, got)
		}
	}
}

func TestUserAliasRefreshReachesEveryRunningHostWithoutRestart(t *testing.T) {
	events := make(chan Event, 100)
	c := testController(t, events)
	p := fixtureProfile()
	if err := c.Start(context.Background(), p, "null"); err != nil {
		t.Fatal(err)
	}
	before := c.Status()
	next := cloneProfile(p)
	next.System.Radios[0].Alias = "Engine, 12"
	next.System.Radios[1].Alias = "Desk \"A\""
	if err := c.ApplyPolicy(context.Background(), next); err != nil {
		t.Fatal(err)
	}
	want := []userAlias{{10001, "Engine, 12"}, {10002, "Desk \"A\""}}
	for _, ch := range before.Channels {
		if got := appliedUserAliases(t, c, ch.ID); !reflect.DeepEqual(got, want) {
			t.Fatalf("%s did not receive exact System names: %v", ch.ID, got)
		}
	}
	after := c.Status()
	if after.FNEPID != before.FNEPID {
		t.Fatal("metadata update restarted media")
	}
	for i := range before.Channels {
		if after.Channels[i].PID != before.Channels[i].PID {
			t.Fatal("metadata update restarted a radio host")
		}
	}
	// Previously saved unrepresentable text stays intact; voice resources stay running.
	next.System.Radios[0].Alias = "Old name exceeding fourteen"
	next.System.Radios[1].Alias = ""
	if err := c.ApplyPolicy(context.Background(), next); err != nil {
		t.Fatal(err)
	}
	for _, ch := range before.Channels {
		if got := appliedUserAliases(t, c, ch.ID); len(got) != 0 {
			t.Fatalf("old aliases survived removal: %v", got)
		}
	}
	c.mu.Lock()
	saved := c.profile.System.Radios[0].Alias
	c.mu.Unlock()
	if saved != next.System.Radios[0].Alias || c.Status().State != "running" {
		t.Fatal("unrepresentable name changed saved settings or stopped operation")
	}
	timer := time.NewTimer(time.Second)
	defer timer.Stop()
	for {
		select {
		case event := <-events:
			if event.Kind == "user_alias_omitted" && event.RID == 10001 {
				return
			}
		case <-timer.C:
			t.Fatal("unrepresentable legacy name was not reported")
		}
	}
}

func TestNativeAliasDirectoryBodyBudgetDoesNotWidenCommands(t *testing.T) {
	var requests atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/auth" {
			_, _ = w.Write([]byte(`{"status":200,"token":"test-token"}`))
			return
		}
		requests.Add(1)
		_, _ = w.Write([]byte(`{"status":200}`))
	}))
	defer server.Close()
	client, err := NewNativeClient(strings.TrimPrefix(server.URL, "http://"), "test-password")
	if err != nil {
		t.Fatal(err)
	}
	value := map[string]string{"payload": strings.Repeat("a", 65537)}
	if _, err := client.Put(context.Background(), "/p25/aliases", value); err != nil {
		t.Fatal(err)
	}
	if _, err := client.Put(context.Background(), "/p25/rid", value); err == nil {
		t.Fatal("alias directory exception widened ordinary command bodies")
	}
	if _, err := client.Put(context.Background(), "/p25/aliases", map[string]string{"payload": strings.Repeat("a", 4<<20)}); err == nil {
		t.Fatal("oversized alias directory accepted")
	}
	if requests.Load() != 1 {
		t.Fatalf("oversized requests reached native transport: %d", requests.Load())
	}
}
