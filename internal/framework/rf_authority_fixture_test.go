package framework

import (
	"context"
	"path/filepath"
	"testing"
	"time"

	"open25.local/open25-fne/internal/radio"
)

// publishFixtureRFState explicitly admits accepted control facts for routing
// fixtures. Web reports and activity-history uploads remain independent.
func publishFixtureRFState(t *testing.T, zone *Service, siteID string, status radio.Status) {
	t.Helper()
	if zone.rfAuthority == nil {
		zoneID := zone.Config().InstanceID
		if current := zone.view().Zone; current != nil {
			zoneID = current.ID
		}
		// Static Service fixtures have no startup/offline initialization path.
		// Prepare their empty current-model file explicitly before opening it.
		path := filepath.Join(t.TempDir(), "rf-source-authority.json")
		d := rfAuthorityDocument{ZoneID: zoneID, NativeRuns: map[string]time.Time{}, Units: map[int]rfAuthorityUnit{}}
		if err := writeRFAuthority(path, d); err != nil {
			t.Fatal(err)
		}
		a, err := openRFSourceAuthority(path, zoneID)
		if err != nil {
			t.Fatal(err)
		}
		zone.rfAuthority = a
		t.Cleanup(a.close)
	}
	run := time.Unix(1, 0).UTC()
	for _, channel := range status.Channels {
		if channel.Role == "control" && !channel.StartedAt.IsZero() {
			run = channel.StartedAt.UTC()
			break
		}
	}
	u := rfAuthorityUpdate{SiteID: siteID, NativeStartedAt: run, ObservedAt: time.Now().UTC(), Affiliations: status.Affiliations, Registrations: status.Registrations}
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	if err := zone.rfAuthority.apply(ctx, u, zone.rfAuthorityScope()); err != nil {
		t.Fatalf("publish fixture control facts for %s: %v", siteID, err)
	}
}
