package framework

import (
	"context"
	"encoding/json"
	"testing"
	"time"
)

func publicationAlgorithm(t *testing.T, system SystemConfig) int {
	t.Helper()
	for _, group := range system.Talkgroups {
		if group.TGID == 1001 {
			return group.AlgorithmID
		}
	}
	t.Fatal("fixture talkgroup missing")
	return -1
}

func TestForwardedCatalogAndBrowserPolicyMatchTheAdmittedDirectory(t *testing.T) {
	c := newMediaCluster(t)
	var next SystemConfig
	raw, err := json.Marshal(c.zone.service.view().System)
	if err != nil {
		t.Fatal(err)
	}
	if err = json.Unmarshal(raw, &next); err != nil {
		t.Fatal(err)
	}
	for index := range next.Talkgroups {
		if next.Talkgroups[index].TGID == 1001 {
			next.Talkgroups[index].AdmissionMode = "both"
			next.Talkgroups[index].TransmitMode = "selectable"
			next.Talkgroups[index].AlgorithmID = 129
			next.Talkgroups[index].KeyID = 0
		}
	}
	raw, _ = json.Marshal(next)
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	readCatalog := func() SystemConfig {
		var incoming catalog
		if err := c.dispatch.service.requestAt(ctx, c.zone.server.URL, "GET", "/internal/catalog", nil, &incoming, nil); err != nil {
			t.Fatal(err)
		}
		return incoming.System
	}
	readBrowser := func() SystemConfig {
		state := c.dispatch.service.operatorState(Session{UserID: c.users[0].ID, PositionID: "desk-1"})
		raw, _ := json.Marshal(state["system"])
		var system SystemConfig
		if err := json.Unmarshal(raw, &system); err != nil {
			t.Fatal(err)
		}
		return system
	}
	for _, node := range []*integrationService{c.zone, c.dispatch} {
		if err = node.service.Store.WriteCache("system", raw); err != nil {
			t.Fatal(err)
		}
	}
	if publicationAlgorithm(t, readCatalog()) != 0 || publicationAlgorithm(t, readBrowser()) != 0 {
		t.Fatal("an unfinished cache write escaped ahead of the admitting controller's policy")
	}
	c.zone.service.refreshDirectory()
	if publicationAlgorithm(t, readCatalog()) != 129 {
		t.Fatal("published Zone policy was not forwarded")
	}
	if publicationAlgorithm(t, readBrowser()) != 0 {
		t.Fatal("Dispatch reported policy before accepting it")
	}
	c.dispatch.service.refreshDirectory()
	if publicationAlgorithm(t, readBrowser()) != 129 {
		t.Fatal("accepted browser policy did not become visible")
	}
	// Per-Site catalogue filtering must not mutate the shared Zone snapshot.
	var incoming catalog
	if err = c.site.service.requestAt(ctx, c.zone.server.URL, "GET", "/internal/catalog", nil, &incoming, nil); err != nil {
		t.Fatal(err)
	}
	if incoming.Zone == nil || len(incoming.Zone.Sites) != 1 || len(c.zone.service.view().Zone.Sites) != 2 {
		t.Fatal("Site projection changed the admitting Zone directory")
	}
}
