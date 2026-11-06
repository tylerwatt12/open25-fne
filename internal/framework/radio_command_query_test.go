package framework

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestCommandHistoryQueriesHealthySiteDespiteEarlierHungSites(t *testing.T) {
	slow := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		<-r.Context().Done()
	}))
	defer slow.Close()
	healthy := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/internal/radio/commands" || r.Header.Get("X-Service-ID") != "zone-main" || r.Header.Get("Authorization") != "Bearer "+strings.Repeat("z", 64) {
			http.Error(w, "invalid enrolled request", http.StatusForbidden)
			return
		}
		respond(w, http.StatusOK, []radioCommandReceipt{
			{ID: "healthy-result", SiteID: "healthy", State: "acknowledged", RadioID: 10001, TalkgroupID: 1001, Action: "radio_check", CreatedAt: time.Now()},
			{ID: "foreign-result", SiteID: "different-site", State: "acknowledged", CreatedAt: time.Now()},
		})
	}))
	defer healthy.Close()
	zone := ZoneConfig{ID: "zone-1"}
	credentials := ServiceCredentials{OutboundID: "zone-main", OutboundToken: strings.Repeat("z", 64), Peers: map[string]PeerCredential{}}
	// More than eight early hangs catches a worker pool that lets an unhealthy
	// first batch consume the shared deadline before this healthy Site starts.
	for i := 0; i < 12; i++ {
		id := fmt.Sprintf("hung-%d", i)
		zone.Sites = append(zone.Sites, SiteIdentity{ID: id, SiteID: i + 1, Endpoint: slow.URL})
		credentials.Peers[id] = PeerCredential{Role: RoleSite, InstanceID: id, Token: strings.Repeat("s", 64)}
	}
	zone.Sites = append(zone.Sites, SiteIdentity{ID: "healthy", SiteID: 13, Endpoint: healthy.URL})
	credentials.Peers["healthy"] = PeerCredential{Role: RoleSite, InstanceID: "healthy", Token: strings.Repeat("h", 64)}
	path := filepath.Join(t.TempDir(), "credentials.json")
	if err := WritePrivateJSON(path, credentials); err != nil {
		t.Fatal(err)
	}
	s := &Service{Store: storeOpen(t, RoleZone), config: FileConfig{Role: RoleZone, CredentialFile: path}, client: http.DefaultClient}
	s.directory.value.Store(&directorySnapshot{Zone: &zone})
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	type outcome struct {
		listing radioCommandListing
		err     error
	}
	done := make(chan outcome, 1)
	start := time.Now()
	go func() { listing, err := s.commandRows(ctx); done <- outcome{listing, err} }()
	var got outcome
	select {
	case got = <-done:
	case <-time.After(3200 * time.Millisecond):
		cancel()
		t.Fatal("administrator command history had no bounded completion")
	}
	if got.err != nil || len(got.listing.Commands) != 1 || got.listing.Commands[0].ID != "healthy-result" {
		t.Fatalf("unhealthy Sites hid the healthy outcome or accepted a foreign receipt: %+v %v", got.listing, got.err)
	}
	if elapsed := time.Since(start); elapsed > 2500*time.Millisecond {
		t.Fatalf("independent Site queries took %v", elapsed)
	}
	if len(got.listing.Sites) != 13 {
		t.Fatal("one or more Site availability results were omitted")
	}
	for i, status := range got.listing.Sites {
		if status.SiteID != zone.Sites[i].ID {
			t.Fatal("parallel completion reordered the configured Sites")
		}
		if (i == 12 && status.State != "available") || (i < 12 && (status.State != "unavailable" || status.Message == "")) {
			t.Fatalf("dishonest availability: %+v", status)
		}
	}
}

func TestLocalCommandHistoryRemainsAvailableWithoutZoneConnection(t *testing.T) {
	s := &Service{Store: storeOpen(t, RoleSite), config: FileConfig{Role: RoleSite, InstanceID: "site-1", UpstreamURL: "http://127.0.0.1:1"}}
	if err := s.Store.mutateRadioCommands(func(rows *[]radioCommandReceipt) error {
		*rows = append(*rows, radioCommandReceipt{ID: "local-receipt", SiteID: "site-1", State: "acknowledged", CreatedAt: time.Now()})
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
	defer cancel()
	listing, err := s.commandRows(ctx)
	if err != nil || len(listing.Commands) != 1 || listing.Commands[0].ID != "local-receipt" || len(listing.Sites) != 1 || listing.Sites[0].State != "available" {
		t.Fatalf("Zone outage hid Site-owned outcomes: %+v %v", listing, err)
	}
}
