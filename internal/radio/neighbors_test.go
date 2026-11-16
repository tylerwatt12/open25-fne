package radio

import (
	"context"
	"encoding/json"
	"net/http"
	"testing"
	"time"
)

func TestNeighborSnapshotAppliesRunningSelectionAndExactChannelReferences(t *testing.T) {
	type wireNeighbor struct {
		AdjacentSite
		RXChannel *uint16 `json:"rx_channel"`
		TTLMillis int64   `json:"ttl_ms"`
	}
	delivery := make(chan []wireNeighbor, 4)
	client := inhibitEndpoint(t, func(w http.ResponseWriter, r *http.Request) {
		if r.Method != "PUT" || r.URL.Path != "/p25/adjacent-sites" {
			t.Errorf("unexpected native request %s %s", r.Method, r.URL.Path)
		}
		var snapshot struct{ Sites []wireNeighbor }
		if err := json.NewDecoder(r.Body).Decode(&snapshot); err != nil {
			t.Error(err)
		}
		delivery <- snapshot.Sites
		_ = json.NewEncoder(w).Encode(map[string]int{"status": 200})
	})
	p := fixtureProfile()
	p.Zone.ChannelIdentifiers = []Identifier{
		{Identifier: 2, BaseHz: 434000000, SpacingHz: 12500, OffsetHz: 5000000},
		{Identifier: 3, BaseHz: 439000000, SpacingHz: 12500, OffsetHz: 5000000},
	}
	local, err := siteIdentity(p)
	if err != nil {
		t.Fatal(err)
	}
	control := AdjacentSite{WACN: p.System.WACN, SystemID: p.System.SystemID, RFSSID: p.Zone.RFSSID, SiteID: local.SiteID + 1,
		LRA: 0xA5, Identifier: 2, ChannelNo: 16, TXHz: 434200000, RXHz: 439200000, ServiceClass: 0x60, Networked: true}
	other := control
	other.SiteID++
	now := time.Now()
	sites := []NeighborSite{
		{ID: "first", Name: "First", Control: &control, ValidUntil: now.Add(9 * time.Second)},
		{ID: "second", Name: "Second", Control: &other, ValidUntil: now.Add(9 * time.Second)},
		{ID: "expired", Name: "Expired", Control: &control, ValidUntil: now.Add(-time.Second)},
		{ID: p.Site.ID, Name: "Self", Control: &control, ValidUntil: now.Add(9 * time.Second)},
	}
	c := &Controller{
		configuredProfile: p, runContext: context.Background(),
		children: map[string]*child{"control": inhibitOwner("control", "control", client)},
		status:   Status{State: "running", Mode: "hardware", ActiveControlChannelID: "control"},
	}
	if err := c.ApplyNeighborSites(context.Background(), sites); err != nil {
		t.Fatal(err)
	}
	all := <-delivery
	if len(all) != 2 || all[0].AdjacentSite != control || all[0].RXChannel == nil || *all[0].RXChannel != 0x21A0 ||
		all[0].TTLMillis <= 0 || all[0].TTLMillis > 9000 {
		t.Fatalf("invalid all-Sites snapshot: %+v", all)
	}

	p.Site.NeighborMode, p.Site.NeighborSiteIDs = "selected", []string{"second"}
	if RFConfigurationSignature(p) == RFConfigurationSignature(c.configuredProfile) || physicalDigest(p) != physicalDigest(c.configuredProfile) {
		t.Fatal("neighbor programming must stage a restart without changing the live RF or admission digest")
	}
	c.configuredProfile = p // explicit restart publishes the new programming
	if err := c.ApplyNeighborSites(context.Background(), sites); err != nil {
		t.Fatal(err)
	}
	selected := <-delivery
	if len(selected) != 1 || selected[0].SiteID != other.SiteID {
		t.Fatalf("invalid directional selection: %+v", selected)
	}

	other.RXHz++
	if err := c.ApplyNeighborSites(context.Background(), sites); err == nil {
		t.Fatal("missing diagnostic for an unrepresentable neighbor")
	}
	if unavailable := <-delivery; unavailable == nil || len(unavailable) != 0 {
		t.Fatalf("unrepresentable neighbor must be removed by an empty snapshot: %+v", unavailable)
	}
}

func TestNeighborExplicitReceiveReferenceCanUseAnotherIdentifier(t *testing.T) {
	plans := []Identifier{
		{Identifier: 2, BaseHz: 434000000, SpacingHz: 12500, OffsetHz: -5000000},
		{Identifier: 3, BaseHz: 429000000, SpacingHz: 12500, OffsetHz: -5000000},
	}
	control := AdjacentSite{Identifier: 2, ChannelNo: 16, TXHz: 434200000, RXHz: 429200000}
	ref, ok := neighborChannelReferences(plans, control)
	if !ok || ref == nil || *ref != 0x3010 {
		t.Fatalf("expected separate receive identifier, got %v (%v)", ref, ok)
	}
}
