package framework

import (
	"encoding/json"
	"golang.org/x/crypto/bcrypt"
	"path/filepath"
	"slices"
	"sync"
	"testing"
)

func inhibitionFixture(t *testing.T, role Role) *Service {
	t.Helper()
	return &Service{Store: storeOpen(t, role), config: FileConfig{Role: role}}
}
func TestRadioInhibitionCurrentSetIsCanonicalAndImmutable(t *testing.T) {
	s := inhibitionFixture(t, RoleZone)
	if got, err := s.DesiredInhibitedRadios(); err != nil || len(got) != 0 {
		t.Fatalf("initial policy %v %v", got, err)
	}
	value := radioInhibitionPolicy{Desired: []int{10003, 10001}}
	if err := s.cacheRadioInhibition(value); err != nil {
		t.Fatal(err)
	}
	value.Desired[0] = 7
	got, err := s.DesiredInhibitedRadios()
	if err != nil || !slices.Equal(got, []int{10001, 10003}) {
		t.Fatal(got, err)
	}
	got[0] = 9
	if blocked, err := s.desiredRadioInhibited(10001); err != nil || !blocked {
		t.Fatal(blocked, err)
	}
	raw, _, err := s.Store.ReadCache("radio-inhibition")
	if err != nil {
		t.Fatal(err)
	}
	var fields map[string]json.RawMessage
	if err = json.Unmarshal(raw, &fields); err != nil || len(fields) != 1 || fields["desired"] == nil {
		t.Fatalf("policy retained extra history: %s %v", raw, err)
	}
	if err = s.Store.Close(); err != nil {
		t.Fatal(err)
	} // hot lookup must not query closed DB
	if got, err = s.DesiredInhibitedRadios(); err != nil || !slices.Equal(got, []int{10001, 10003}) {
		t.Fatal(got, err)
	}
}
func TestRadioInhibitionRejectsMalformedReplacementWithoutLosingCurrentSet(t *testing.T) {
	s := inhibitionFixture(t, RoleSite)
	if err := s.cacheRadioInhibition(radioInhibitionPolicy{Desired: []int{10001}}); err != nil {
		t.Fatal(err)
	}
	tooMany := make([]int, 33)
	for i := range tooMany {
		tooMany[i] = i + 1
	}
	for _, bad := range [][]int{{0}, {-1}, {0xFFFFFC}, {10001, 10001}, tooMany} {
		if err := s.cacheRadioInhibition(radioInhibitionPolicy{Desired: bad}); err == nil {
			t.Fatalf("accepted %v", bad)
		}
		if got, err := s.DesiredInhibitedRadios(); err != nil || !slices.Equal(got, []int{10001}) {
			t.Fatal(got, err)
		}
	}
	if err := s.cacheRadioInhibition(radioInhibitionPolicy{}); err != nil {
		t.Fatal(err)
	}
	raw, _, err := s.Store.ReadCache("radio-inhibition")
	if err != nil || string(raw) != "{\"desired\":[]}" {
		t.Fatal(string(raw), err)
	}
	if err := inhibitionFixture(t, RoleSystem).cacheRadioInhibition(radioInhibitionPolicy{Desired: []int{10001}}); err == nil {
		t.Fatal("System took Zone policy authority")
	}
}
func TestRadioInhibitionRetainsCacheAcrossRestartAndFailsClosedOnCorruption(t *testing.T) {
	path := filepath.Join(t.TempDir(), "policy.db")
	store, err := OpenStore(path, RoleSite)
	if err != nil {
		t.Fatal(err)
	}
	s := &Service{Store: store, config: FileConfig{Role: RoleSite}}
	if err = s.cacheRadioInhibition(radioInhibitionPolicy{Desired: []int{10001}}); err != nil {
		t.Fatal(err)
	}
	store.Close()
	store, err = OpenStore(path, RoleSite)
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	restarted := &Service{Store: store, config: FileConfig{Role: RoleSite}}
	if got, err := restarted.DesiredInhibitedRadios(); err != nil || !slices.Equal(got, []int{10001}) {
		t.Fatal(got, err)
	}
	if err = store.WriteCache("radio-inhibition", json.RawMessage(`{"desired":[0]}`)); err != nil {
		t.Fatal(err)
	}
	corrupt := &Service{Store: store, config: FileConfig{Role: RoleSite}}
	if _, err = corrupt.DesiredInhibitedRadios(); err == nil {
		t.Fatal("corrupt cache became empty policy")
	}
	if _, err = corrupt.desiredRadioInhibited(10001); err == nil {
		t.Fatal("corrupt cache became permission")
	}
	if err = corrupt.cacheRadioInhibition(radioInhibitionPolicy{Desired: []int{10001}}); err != nil {
		t.Fatal(err)
	}
	if got, err := corrupt.DesiredInhibitedRadios(); err != nil || !slices.Equal(got, []int{10001}) {
		t.Fatal(got, err)
	}
}
func TestRadioInhibitionSnapshotReadersAndWriter(t *testing.T) {
	s := inhibitionFixture(t, RoleZone)
	var wg sync.WaitGroup
	for i := 0; i < 4; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for n := 0; n < 100; n++ {
				v, err := s.DesiredInhibitedRadios()
				if err != nil {
					t.Error(err)
					return
				}
				if len(v) > 1 {
					t.Error(v)
					return
				}
				_, err = s.desiredRadioInhibited(10001)
				if err != nil {
					t.Error(err)
					return
				}
			}
		}()
	}
	for n := 0; n < 20; n++ {
		v := []int{}
		if n%2 == 0 {
			v = []int{10001}
		}
		if err := s.cacheRadioInhibition(radioInhibitionPolicy{Desired: v}); err != nil {
			t.Fatal(err)
		}
	}
	wg.Wait()
}

func TestRadioInhibitionCatalogueCommitIsAtomicAndNeverAcceptsOmission(t *testing.T) {
	s := inhibitionFixture(t, RoleSite)
	s.config.InstanceID = "site-1"
	hash, err := bcrypt.GenerateFromPassword([]byte("test-only-policy"), bcrypt.MinCost)
	if err != nil {
		t.Fatal(err)
	}
	baseSystem := storeFixtureSystem()
	baseZone := storeFixtureZone()
	original := radioInhibitionPolicy{Desired: []int{10001}}
	if err = s.Store.AcceptCatalog(storeDoc(t, baseSystem), storeDoc(t, baseZone), "administrator", hash, []userRecord{}, []automationRecord{}, &original); err != nil {
		t.Fatal(err)
	}
	if got, err := s.DesiredInhibitedRadios(); err != nil || !slices.Equal(got, original.Desired) {
		t.Fatal(got, err)
	}
	changed := baseSystem
	changed.Name = "Different accepted name"
	incoming := catalog{System: changed, Zone: &baseZone, Administrator: administratorRecord{Username: "administrator", Hash: hash}, Inhibition: &radioInhibitionPolicy{Desired: []int{}}}
	if _, err = s.Store.db.Exec(`CREATE TRIGGER reject_inhibit_change BEFORE UPDATE ON framework_documents WHEN NEW.name='cache:radio-inhibition' BEGIN SELECT RAISE(ABORT,'test policy transaction failure'); END`); err != nil {
		t.Fatal(err)
	}
	if err = s.acceptCatalog(incoming); err == nil {
		t.Fatal("expected policy write failure")
	}
	raw, _, err := s.Store.ReadCache("system")
	if err != nil {
		t.Fatal(err)
	}
	var system SystemConfig
	if err = json.Unmarshal(raw, &system); err != nil || system.Name != baseSystem.Name {
		t.Fatal("catalogue partly committed", system.Name, err)
	}
	if got, err := s.DesiredInhibitedRadios(); err != nil || !slices.Equal(got, original.Desired) {
		t.Fatal("failed transaction published policy", got, err)
	}
	if _, err = s.Store.db.Exec(`DROP TRIGGER reject_inhibit_change`); err != nil {
		t.Fatal(err)
	}
	missing := incoming
	missing.Inhibition = nil
	if err = s.acceptCatalog(missing); err == nil {
		t.Fatal("omitted Zone policy cleared inhibition")
	}
	if err = s.acceptCatalog(incoming); err != nil {
		t.Fatal(err)
	}
	if got, err := s.DesiredInhibitedRadios(); err != nil || len(got) != 0 {
		t.Fatal(got, err)
	}
}

func TestRadioInhibitionCacheRequiresCompleteArray(t *testing.T) {
	store := storeOpen(t, RoleSite)
	for _, raw := range []string{`{}`, `{"desired":null}`, `{"desired":[],"timestamp":"not-policy"}`} {
		if err := store.WriteCache("radio-inhibition", json.RawMessage(raw)); err != nil {
			t.Fatal(err)
		}
		s := &Service{Store: store, config: FileConfig{Role: RoleSite}}
		if _, err := s.DesiredInhibitedRadios(); err == nil {
			t.Fatalf("incomplete cache became clear policy: %s", raw)
		}
	}
}

func TestRadioInhibitionZoneReceiptAndIntentRollbackTogether(t *testing.T) {
	s := inhibitionFixture(t, RoleZone)
	if err := s.cacheRadioInhibition(radioInhibitionPolicy{Desired: []int{10001}}); err != nil {
		t.Fatal(err)
	}
	if _, err := s.Store.db.Exec(`CREATE TRIGGER reject_zone_policy BEFORE UPDATE ON framework_documents WHEN NEW.name='cache:radio-inhibition' BEGIN SELECT RAISE(ABORT,'test Zone intent failure'); END`); err != nil {
		t.Fatal(err)
	}
	receipt, duplicate, err := s.admitZoneRadioInhibition(radioCommandInput{ID: "must-rollback", Action: "uninhibit", RadioID: 10001, SiteID: "site-1"}, OperatorActor{Administrator: true, Username: "administrator"})
	if err == nil || duplicate || receipt.ID != "" {
		t.Fatalf("failed transaction reported saved: %+v %v %v", receipt, duplicate, err)
	}
	got, err := s.DesiredInhibitedRadios()
	if err != nil || !slices.Equal(got, []int{10001}) {
		t.Fatal(got, err)
	}
	rows, err := s.Store.radioCommands()
	if err != nil || len(rows) != 0 {
		t.Fatalf("partial request receipt: %+v %v", rows, err)
	}
}
