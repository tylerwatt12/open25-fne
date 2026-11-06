package framework

import (
	"context"
	"database/sql"
	"open25.local/open25-fne/internal/activity"
	"open25.local/open25-fne/internal/dispatch"
	"open25.local/open25-fne/internal/radio"
	"open25.local/open25-fne/internal/routing"
	"os"
	"path/filepath"
	"syscall"
	"testing"
	"time"
)

func rfSeedFixture(t *testing.T) (string, time.Time) {
	t.Helper()
	dir := t.TempDir()
	now := time.Now().UTC().Add(-time.Minute)
	store, err := OpenStore(filepath.Join(dir, "configuration.db"), RoleZone)
	if err != nil {
		t.Fatal(err)
	}
	zone := storeFixtureZone()
	zone.Sites = append(zone.Sites, SiteIdentity{ID: "site-2", Name: "Second", SiteID: 2, NAC: 0x294})
	system := storeFixtureSystem()
	for _, write := range []func() error{
		func() error { return store.SaveConfig(storeDoc(t, zone)) },
		func() error { return store.WriteCache("system", storeDoc(t, system)) },
		func() error {
			return store.WriteCache("site_reports", storeDoc(t, []SiteReport{{ID: "site-1", ReceivedAt: now.Add(30 * time.Second), Radio: &radio.Status{Channels: []radio.ChannelStatus{{Role: "control", StartedAt: now}}, Affiliations: []radio.Affiliation{{RID: 10001, TGID: 1001, At: now.Add(10 * time.Second)}}}}}))
		},
	} {
		if err = write(); err != nil {
			t.Fatal(err)
		}
	}
	if err = store.Close(); err != nil {
		t.Fatal(err)
	}
	history, err := activity.Open(filepath.Join(dir, "activity.db"))
	if err != nil {
		t.Fatal(err)
	}
	if err = history.Append(activity.Event{ID: "accepted-B-registration", Origin: "site-2", SiteID: "site-2", At: now.Add(20 * time.Second), RadioID: 10001, Type: "registration", SourceKind: "rf"}); err != nil {
		t.Fatal(err)
	}
	if err = history.Close(); err != nil {
		t.Fatal(err)
	}
	return dir, now
}
func TestRFSourceAuthorityOfflineSeedRetainsNewerRegistration(t *testing.T) {
	dir, _ := rfSeedFixture(t)
	output := filepath.Join(t.TempDir(), "rf-source-authority.json")
	receipt, err := SeedRFSourceAuthority(context.Background(), dir, "zone-1", output)
	if err != nil {
		t.Fatal(err)
	}
	if receipt.RadioCount != 1 || receipt.SHA256 == "" {
		t.Fatal("missing offline receipt", receipt)
	}
	authority, err := openRFSourceAuthority(output, "zone-1")
	if err != nil {
		t.Fatal(err)
	}
	defer authority.close()
	owner, ok := authority.lookup(10001)
	if !ok || owner.SiteID != "site-2" || owner.TGID != 0 {
		t.Fatal("older A revived through offline seed", owner, ok)
	}
	if _, err = SeedRFSourceAuthority(context.Background(), dir, "zone-1", output); err == nil {
		t.Fatal("overwrote prepared authority")
	}
}
func TestRFSourceAuthorityOfflineSeedRefusesLiveOrUncheckpointedInput(t *testing.T) {
	dir, _ := rfSeedFixture(t)
	output := filepath.Join(t.TempDir(), "authority.json")
	lock, err := os.OpenFile(filepath.Join(dir, "service.lock"), os.O_CREATE|os.O_RDWR, 0600)
	if err != nil {
		t.Fatal(err)
	}
	if err = syscall.Flock(int(lock.Fd()), syscall.LOCK_EX|syscall.LOCK_NB); err != nil {
		t.Fatal(err)
	}
	if _, err = SeedRFSourceAuthority(context.Background(), dir, "zone-1", output); err == nil {
		t.Fatal("read a live Zone")
	}
	lock.Close()
	if _, err = SeedRFSourceAuthority(context.Background(), dir, "other-zone", output); err == nil {
		t.Fatal("accepted wrong Zone identity")
	}
	if err = os.WriteFile(filepath.Join(dir, "activity.db-wal"), []byte("uncheckpointed"), 0600); err != nil {
		t.Fatal(err)
	}
	if _, err = SeedRFSourceAuthority(context.Background(), dir, "zone-1", output); err == nil {
		t.Fatal("ignored uncheckpointed facts")
	}
	if _, err = os.Stat(output); !os.IsNotExist(err) {
		t.Fatal("failed preparation created output", err)
	}
}

func TestRFSourceAuthorityOfflineSeedPreservesUnknownMembershipProjection(t *testing.T) {
	for _, kind := range []string{"presence TG0 without retained event", "report TG0"} {
		t.Run(kind, func(t *testing.T) {
			dir, base := rfSeedFixture(t)
			db, err := sql.Open("sqlite", filepath.Join(dir, "activity.db"))
			if err != nil {
				t.Fatal(err)
			}
			if _, err = db.Exec("DELETE FROM radio_events; DELETE FROM radio_presence"); err != nil {
				t.Fatal(err)
			}
			if kind == "presence TG0 without retained event" {
				_, err = db.Exec("INSERT INTO radio_presence VALUES(?,?,?,?,?,?)", 10001, "site-1", true, 0, base.Add(20*time.Second).Format(time.RFC3339Nano), "rf")
				if err != nil {
					t.Fatal(err)
				}
			}
			if err = db.Close(); err != nil {
				t.Fatal(err)
			}
			if kind == "report TG0" {
				config, err := OpenStore(filepath.Join(dir, "configuration.db"), RoleZone)
				if err != nil {
					t.Fatal(err)
				}
				reports := []SiteReport{{ID: "site-1", ReceivedAt: base.Add(30 * time.Second), Radio: &radio.Status{Channels: []radio.ChannelStatus{{Role: "control", StartedAt: base}}, Affiliations: []radio.Affiliation{{RID: 10001, TGID: 0, At: base.Add(20 * time.Second)}}}}}
				if err = config.WriteCache("site_reports", storeDoc(t, reports)); err != nil {
					t.Fatal(err)
				}
				config.Close()
				history, err := activity.Open(filepath.Join(dir, "activity.db"))
				if err != nil {
					t.Fatal(err)
				}
				if err = history.Append(activity.Event{ID: "older-accepted-group", Origin: "site-1", SiteID: "site-1", At: base.Add(10 * time.Second), RadioID: 10001, TalkgroupID: 1001, Type: "affiliation", SourceKind: "rf"}); err != nil {
					t.Fatal(err)
				}
				history.Close()
			}
			output := filepath.Join(t.TempDir(), "authority.json")
			receipt, err := SeedRFSourceAuthority(context.Background(), dir, "zone-1", output)
			if err != nil {
				t.Fatal(err)
			}
			authority, err := openRFSourceAuthority(output, "zone-1")
			if err != nil {
				t.Fatal(err)
			}
			defer authority.close()
			owner, ok := authority.lookup(10001)
			if !ok || owner.SiteID != "site-1" || owner.TGID != 0 {
				t.Fatal("projection resurrected old group", owner, ok)
			}
			if receipt.UnknownMemberships != 1 {
				t.Fatal("unknown membership was not labelled", receipt)
			}
			fact := authority.value.Load().document.Units[10001]
			if !fact.NullAffiliation.At.IsZero() || !fact.MembershipUnknown.At.Equal(base.Add(20*time.Second)) {
				t.Fatal("lossy projection was converted to a radio procedure", fact)
			}
			s := &Service{config: FileConfig{Role: RoleZone, InstanceID: "zone-1"}, rfAuthority: authority}
			zone := storeFixtureZone()
			s.directory.value.Store(&directorySnapshot{System: storeFixtureSystem(), Zone: &zone})
			s.inhibition.Store(inhibitionSnapshot(radioInhibitionPolicy{Desired: []int{}}, nil))
			s.broker = &callBroker{s: s}
			q := routing.Request{CallRequest: dispatch.CallRequest{RadioID: 10001, Talkgroups: []int{1001}}, SourceKind: "rf", OriginSite: "site-1", RFSource: routing.RFSourceObservation{NativeStartedAt: base, ObservedAt: time.Now().UTC(), AffiliatedAt: base.Add(15 * time.Second)}}
			if err = s.acceptRFObservation(context.Background(), q); err == nil {
				t.Fatal("previously unseen pre-exclusion affiliation revived")
			}
		})
	}
}
