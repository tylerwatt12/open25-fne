package framework

import (
	"bytes"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"open25.local/open25-fne/internal/radio"
)

func TestTransientObservationsIgnoreOccupiedConfigurationWriter(t *testing.T) {
	s := storeOpen(t, RoleZone)
	tx, err := s.db.Begin()
	if err != nil {
		t.Fatal(err)
	}
	defer tx.Rollback()
	if _, err = tx.Exec("UPDATE framework_metadata SET value=value WHERE key='role'"); err != nil {
		t.Fatal(err)
	}
	s.mu.Lock()
	done := make(chan error, 1)
	go func() {
		for _, name := range []string{"connection", "site_reports", "zone_report", "hardware_reports"} {
			if err := s.WriteCache(name, []byte(`{"fresh":true}`)); err != nil {
				done <- err
				return
			}
			value, at, err := s.ReadCache(name)
			if err != nil || at.IsZero() || !bytes.Equal(value, []byte(`{"fresh":true}`)) {
				done <- fmt.Errorf("%s observation unavailable: %s %v", name, value, err)
				return
			}
		}
		done <- nil
	}()
	select {
	case err = <-done:
		s.mu.Unlock()
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(time.Second):
		s.mu.Unlock()
		_ = tx.Rollback()
		<-done
		t.Fatal("current observations waited for the configuration writer")
	}
	if err = tx.Rollback(); err != nil {
		t.Fatal(err)
	}
	var persisted int
	if err = s.readDB.QueryRow("SELECT count(*) FROM framework_documents").Scan(&persisted); err != nil || persisted != 0 {
		t.Fatalf("transient observations reached durable storage: count=%d error=%v", persisted, err)
	}
}

func TestTransientObservationsAreOwnedCopiesAndNotRevivedOnRestart(t *testing.T) {
	path := filepath.Join(t.TempDir(), "configuration.db")
	s, err := OpenStore(path, RoleSite)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = s.Close() }()
	for _, name := range []string{"connection", "site_reports", "zone_report", "hardware_reports"} {
		if _, err = s.db.Exec("INSERT INTO framework_documents(name,document,at) VALUES(?,?,?)", "cache:"+name, []byte(`{"old_process":true}`), nowStamp()); err != nil {
			t.Fatal(err)
		}
		if _, _, err = s.ReadCache(name); !errors.Is(err, sql.ErrNoRows) {
			t.Fatalf("%s revived a legacy observation: %v", name, err)
		}
		raw := []byte(` { "fresh": true } `)
		if err = s.WriteCache(name, raw); err != nil {
			t.Fatal(err)
		}
		clear(raw)
		first, at, err := s.ReadCache(name)
		if err != nil || at.IsZero() || string(first) != `{"fresh":true}` {
			t.Fatalf("%s did not own its input: %s %v", name, first, err)
		}
		clear(first)
		second, _, err := s.ReadCache(name)
		if err != nil || string(second) != `{"fresh":true}` {
			t.Fatalf("%s exposed mutable published bytes: %s %v", name, second, err)
		}
		if err = s.WriteCache(name, []byte(`not JSON`)); err == nil {
			t.Fatalf("%s accepted a malformed observation", name)
		}
	}
	tree := topology{SystemURL: "https://system.example/", SiteURLs: map[string]string{"site-1": "https://site.example/"}}
	if err = s.WriteCache("topology", storeDoc(t, tree)); err != nil {
		t.Fatal(err)
	}
	if err = s.WriteCache("zone", storeDoc(t, storeFixtureZone())); err != nil {
		t.Fatal(err)
	}
	if err = s.Close(); err != nil {
		t.Fatal(err)
	}
	s, err = OpenStore(path, RoleSite)
	if err != nil {
		t.Fatal(err)
	}
	for _, name := range []string{"connection", "site_reports", "zone_report", "hardware_reports"} {
		if _, _, err = s.ReadCache(name); !errors.Is(err, sql.ErrNoRows) {
			t.Fatalf("%s survived a process restart: %v", name, err)
		}
	}
	for _, name := range []string{"topology", "zone"} {
		if _, _, err = s.ReadCache(name); err != nil {
			t.Fatalf("durable %s was lost: %v", name, err)
		}
	}
}

func TestTransientObservationsConcurrentSnapshots(t *testing.T) {
	s := storeOpen(t, RoleSite)
	var workers sync.WaitGroup
	for i := range 12 {
		workers.Add(1)
		go func(i int) {
			defer workers.Done()
			for n := range 40 {
				raw := []byte(fmt.Sprintf(`{"writer":%d,"sequence":%d}`, i, n))
				if err := s.WriteCache("site_reports", raw); err != nil {
					t.Error(err)
					return
				}
				clear(raw)
				read, _, err := s.ReadCache("site_reports")
				if err != nil || !json.Valid(read) {
					t.Errorf("corrupted concurrent snapshot: %q %v", read, err)
					return
				}
				clear(read)
			}
		}(i)
	}
	workers.Wait()
}

func withOccupiedConfigurationWriter(t *testing.T, store *Store, action func() error) {
	t.Helper()
	tx, err := store.db.Begin()
	if err != nil {
		t.Fatal(err)
	}
	defer tx.Rollback()
	if _, err = tx.Exec("UPDATE framework_metadata SET value=value WHERE key='role'"); err != nil {
		t.Fatal(err)
	}
	store.mu.Lock()
	var released sync.Once
	release := func() {
		released.Do(func() {
			_ = tx.Rollback()
			store.mu.Unlock()
		})
	}
	defer release()
	done := make(chan error, 1)
	go func() { done <- action() }()
	select {
	case err = <-done:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(time.Second):
		release()
		<-done
		t.Fatal("unchanged programming plus fresh observations queued behind the configuration writer")
	}
}

func observationReportService(t *testing.T, role Role) (*Service, SiteReport, ZoneConfig) {
	t.Helper()
	zone := storeFixtureZone()
	s := &Service{Store: storeOpen(t, role), config: FileConfig{Role: role, PollSeconds: 1, InstanceID: "system-1", PublicURL: "https://system.example"}}
	system := storeFixtureSystem()
	if role == RoleZone {
		s.config.InstanceID, s.config.PublicURL = zone.ID, "https://zone.example"
		if err := s.Store.SaveConfig(storeDoc(t, zone)); err != nil {
			t.Fatal(err)
		}
		if err := s.Store.WriteCache("system", storeDoc(t, system)); err != nil {
			t.Fatal(err)
		}
		s.refreshDirectory()
	} else if err := s.Store.SaveConfig(storeDoc(t, system)); err != nil {
		t.Fatal(err)
	}
	report := SiteReport{ID: "site-1", AdminURL: "https://site.example", ObservedAt: time.Now().UTC(), Config: storeFixtureSite(), Radio: &radio.Status{SiteID: "site-1", State: "stopped", FNEPID: 100}}
	return s, report, zone
}

func TestAcceptedReportsRefreshWithoutConfigurationWrites(t *testing.T) {
	t.Run("Site report on Zone", func(t *testing.T) {
		s, report, _ := observationReportService(t, RoleZone)
		peer := PeerCredential{Role: RoleSite, InstanceID: report.ID}
		if err := s.acceptSiteReport(peer, report); err != nil {
			t.Fatal(err)
		}
		report.ObservedAt = time.Now().UTC()
		report.Radio.FNEPID = 200
		withOccupiedConfigurationWriter(t, s.Store, func() error { return s.acceptSiteReport(peer, report) })
		got := s.siteReports()
		if len(got) != 1 || got[0].Radio.FNEPID != 200 || !got[0].ObservedAt.Equal(report.ObservedAt) {
			t.Fatalf("fresh Site telemetry was not published: %+v", got)
		}
	})
	t.Run("Zone report on System", func(t *testing.T) {
		s, site, zone := observationReportService(t, RoleSystem)
		site.ReceivedAt = time.Now().UTC()
		report := zoneReport{ID: zone.ID, AdminURL: "https://zone.example", ObservedAt: time.Now().UTC(), Config: zone, Sites: []SiteReport{site}}
		peer := PeerCredential{Role: RoleZone, InstanceID: zone.ID}
		if err := s.acceptZoneReport(peer, report); err != nil {
			t.Fatal(err)
		}
		report.ObservedAt = time.Now().UTC()
		report.Sites[0].Radio.FNEPID = 200
		withOccupiedConfigurationWriter(t, s.Store, func() error { return s.acceptZoneReport(peer, report) })
		var got zoneReport
		if !s.readCache("zone_report", &got) || !got.ObservedAt.Equal(report.ObservedAt) || got.Sites[0].Radio.FNEPID != 200 {
			t.Fatalf("fresh Zone telemetry was not published: %+v", got)
		}
	})
}

func TestReportTopologyPreservesEndpointChangesAcrossRestart(t *testing.T) {
	for _, role := range []Role{RoleZone, RoleSystem} {
		t.Run(string(role), func(t *testing.T) {
			s, site, zone := observationReportService(t, role)
			publish := func() error {
				if role == RoleZone {
					return s.acceptSiteReport(PeerCredential{Role: RoleSite, InstanceID: site.ID}, site)
				}
				site.ReceivedAt = time.Now().UTC()
				return s.acceptZoneReport(PeerCredential{Role: RoleZone, InstanceID: zone.ID}, zoneReport{ID: zone.ID, AdminURL: "https://new-zone.example", ObservedAt: time.Now().UTC(), Config: zone, Sites: []SiteReport{site}})
			}
			if err := publish(); err != nil {
				t.Fatal(err)
			}
			site.AdminURL = "https://new-site.example"
			if err := publish(); err != nil {
				t.Fatal(err)
			}
			var filename string
			if err := s.Store.readDB.QueryRow("SELECT file FROM pragma_database_list WHERE name='main'").Scan(&filename); err != nil {
				t.Fatal(err)
			}
			if err := s.Store.Close(); err != nil {
				t.Fatal(err)
			}
			reopened, err := OpenStore(filename, role)
			if err != nil {
				t.Fatal(err)
			}
			defer reopened.Close()
			s.Store = reopened
			for _, name := range []string{"site_reports", "zone_report"} {
				if _, _, err = reopened.ReadCache(name); !errors.Is(err, sql.ErrNoRows) {
					t.Fatalf("cold navigation revived %s", name)
				}
			}
			if role == RoleZone {
				if err = s.acceptCatalog(observationCatalog()); err != nil {
					t.Fatal(err)
				}
			}
			tree := s.adminTopology(&zone)
			if tree.SiteURLs[site.ID] != site.AdminURL || (role == RoleSystem && tree.ZoneURL != "https://new-zone.example") {
				t.Fatalf("cold navigation lost admitted addresses: %+v", tree)
			}
		})
	}
}

func observationCatalog() catalog {
	return catalog{
		System:        storeFixtureSystem(),
		Administrator: administratorRecord{Username: "administrator", Hash: []byte("$2a$10$N9qo8uLOickgx2ZMRZoMyeIjZAgcfl7p92ldGxad68LJZdL17lhWy")},
		Topology: topology{
			SystemURL: "https://parent-system.example", ZoneURL: "https://old-zone.example",
			ApplicationURL: "https://application.example",
			SiteURLs:       map[string]string{"site-1": "https://old-site.example", "site-2": "https://old-site-2.example", "removed-site": "https://removed.example"},
		},
	}
}

func TestCatalogTopologyCannotRegressDirectSiteReports(t *testing.T) {
	s, report, zone := observationReportService(t, RoleZone)
	peer := PeerCredential{Role: RoleSite, InstanceID: report.ID}
	if err := s.acceptSiteReport(peer, report); err != nil {
		t.Fatal(err)
	}
	if err := s.acceptCatalog(observationCatalog()); err != nil {
		t.Fatal(err)
	}
	tree := s.adminTopology(&zone)
	if tree.SiteURLs[report.ID] != report.AdminURL || tree.ZoneURL != s.Config().PublicURL || tree.SystemURL != "https://parent-system.example" || tree.ApplicationURL != "https://application.example" {
		t.Fatalf("stale parent topology regressed accepted direct addresses: %+v", tree)
	}
	if _, exists := tree.SiteURLs["removed-site"]; exists {
		t.Fatal("parent topology restored an unregistered Site")
	}
	// An explicitly missing address is still a direct observation, not an
	// instruction to resurrect an older parent or registry address.
	report.AdminURL = ""
	if err := s.acceptSiteReport(peer, report); err != nil {
		t.Fatal(err)
	}
	if err := s.acceptCatalog(observationCatalog()); err != nil {
		t.Fatal(err)
	}
	if tree = s.adminTopology(&zone); tree.SiteURLs[report.ID] != "" {
		t.Fatalf("unknown direct address was replaced: %+v", tree)
	}
}

func TestConcurrentCatalogAndDirectReportsKeepBothSiteAddresses(t *testing.T) {
	s, first, zone := observationReportService(t, RoleZone)
	zone.Sites = append(zone.Sites, SiteIdentity{ID: "site-2", Name: "Second Site", SiteID: 2})
	if err := s.Store.SaveConfig(storeDoc(t, zone)); err != nil {
		t.Fatal(err)
	}
	s.refreshDirectory()
	second := first
	second.ID, second.Config.ID, second.AdminURL = "site-2", "site-2", "https://second.example"
	second.Radio = &radio.Status{SiteID: "site-2", State: "stopped"}
	for _, report := range []SiteReport{first, second} {
		if err := s.acceptSiteReport(PeerCredential{Role: RoleSite, InstanceID: report.ID}, report); err != nil {
			t.Fatal(err)
		}
	}
	first.AdminURL, second.AdminURL = "https://new-first.example", "https://new-second.example"
	errors := make(chan error, 3)
	var workers sync.WaitGroup
	for _, report := range []SiteReport{first, second} {
		workers.Add(1)
		go func(report SiteReport) {
			defer workers.Done()
			for range 12 {
				report.ObservedAt = time.Now().UTC()
				if err := s.acceptSiteReport(PeerCredential{Role: RoleSite, InstanceID: report.ID}, report); err != nil {
					errors <- err
					return
				}
			}
		}(report)
	}
	workers.Add(1)
	go func() {
		defer workers.Done()
		for range 12 {
			if err := s.acceptCatalog(observationCatalog()); err != nil {
				errors <- err
				return
			}
		}
	}()
	workers.Wait()
	close(errors)
	for err := range errors {
		t.Error(err)
	}
	tree := s.adminTopology(&zone)
	if len(tree.SiteURLs) != 2 || tree.SiteURLs[first.ID] != first.AdminURL || tree.SiteURLs[second.ID] != second.AdminURL {
		t.Fatalf("concurrent parent/direct publication dropped or regressed an address: %+v", tree)
	}
}

func TestCatalogTopologyReadFailureCannotReplaceDirectProjection(t *testing.T) {
	s, report, _ := observationReportService(t, RoleZone)
	if err := s.acceptSiteReport(PeerCredential{Role: RoleSite, InstanceID: report.ID}, report); err != nil {
		t.Fatal(err)
	}
	// Represent a storage/decode failure only at the projection read. Ordinary
	// programming and authentication rows remain available to the catalogue.
	broken := []byte(`{"site_urls":`)
	if _, err := s.Store.db.Exec("UPDATE framework_documents SET document=? WHERE name='cache:topology'", broken); err != nil {
		t.Fatal(err)
	}
	if err := s.acceptCatalog(observationCatalog()); err == nil {
		t.Fatal("catalogue silently replaced an unreadable direct-address projection")
	}
	raw, _, err := s.Store.ReadCache("topology")
	if err != nil || !bytes.Equal(raw, broken) {
		t.Fatalf("failed projection was replaced with the stale parent: %s %v", raw, err)
	}
}
