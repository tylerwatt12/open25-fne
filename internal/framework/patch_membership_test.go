package framework

import (
	"context"
	"encoding/json"
	"net/http"
	"path/filepath"
	"reflect"
	"slices"
	"sync"
	"testing"
	"time"

	"open25.local/open25-fne/internal/activity"
	"open25.local/open25-fne/internal/dispatch"
	"open25.local/open25-fne/internal/routing"
)

func patchEditFixture(t *testing.T, role Role) (*Service, operatorAuthority, RuntimePatch) {
	t.Helper()
	s, desk, _ := operatorFixture(t, role)
	view := *s.view()
	view.System.Talkgroups = slices.Clone(view.System.Talkgroups)
	for _, id := range []int{1003, 1004, 1005} {
		group := view.System.Talkgroups[0]
		group.TGID, group.Alias = id, "Additional group"
		view.System.Talkgroups = append(view.System.Talkgroups, group)
	}
	for i := range view.System.ConsolePositions {
		view.System.ConsolePositions[i].Talkgroups = append(slices.Clone(view.System.ConsolePositions[i].Talkgroups), 1003, 1004, 1005)
	}
	view.Users = make(map[string]userRecord, len(s.view().Users))
	for id, record := range s.view().Users {
		if id == desk.actor.UserID {
			record.User.Talkgroups = []int{1001, 1002, 1003, 1004, 1005}
			desk.actor.Proof = userProof(record.User, record.PasswordHash)
		}
		view.Users[id] = record
	}
	s.directory.value.Store(&view)
	var err error
	desk, err = s.operatorActor(desk.actor)
	if err != nil {
		t.Fatal(err)
	}
	state := operatorApply(t, s, "patch", OperatorInput{Action: "activate", ID: "editable-" + string(role) + "-patch", Patch: &PatchInput{Name: "Operations", Talkgroups: []int{1001, 1002}}}, desk)
	return s, desk, state.Patches[0]
}

func editMembership(patch RuntimePatch, groups ...int) OperatorInput {
	return OperatorInput{Action: "update", ID: patch.ID, Patch: &PatchInput{
		Name: patch.Name, Talkgroups: groups, AlgorithmID: patch.AlgorithmID, KeyID: patch.KeyID,
		PreviousTalkgroups: slices.Clone(patch.Talkgroups),
	}}
}

func TestPatchMembershipUpdatePreservesIdentityRoutingHistoryAndRestart(t *testing.T) {
	s, desk, before := patchEditFixture(t, RoleZone)
	store, err := activity.Open(filepath.Join(t.TempDir(), "activity.db"))
	if err != nil {
		t.Fatal(err)
	}
	s.activity = store
	t.Cleanup(func() { store.Close() })
	state := operatorApply(t, s, "patch", editMembership(before, 1003, 1001), desk)
	got := state.Patches[0]
	if !samePatchIdentity(before, got) || got.UpdatedAt.IsZero() || got.UpdatedBy != desk.actor.UserID || !got.CanEdit {
		t.Fatalf("lost patch identity or update facts: %+v", got)
	}
	if !slices.Equal(got.Talkgroups, []int{1001, 1003}) {
		t.Fatal(got.Talkgroups)
	}
	for source, want := range map[int][]int{1001: {1001, 1003}, 1003: {1001, 1003}, 1002: {1002}} {
		groups, err := s.expandPatchedGroups([]int{source})
		if err != nil || !slices.Equal(groups, want) {
			t.Fatalf("bidirectional routing %d: %v %v", source, groups, err)
		}
	}
	if err := store.Flush(context.Background()); err != nil {
		t.Fatal(err)
	}
	events, err := store.Events(activity.Query{Type: "patch_update", Limit: 10})
	if err != nil || len(events) != 1 {
		t.Fatalf("update history: %v %v", events, err)
	}
	var oldGroups, newGroups []int
	raw, _ := json.Marshal(events[0].Details["previous_talkgroups"])
	_ = json.Unmarshal(raw, &oldGroups)
	raw, _ = json.Marshal(events[0].Details["talkgroups"])
	_ = json.Unmarshal(raw, &newGroups)
	if events[0].Details["user_id"] != desk.actor.UserID || !slices.Equal(oldGroups, before.Talkgroups) || !slices.Equal(newGroups, got.Talkgroups) {
		t.Fatalf("update actor or before/after history lost: %+v", events[0])
	}
	restarted := &Service{Store: s.Store, config: s.Config()}
	restarted.directory.value.Store(s.view())
	restarted.refreshOperatorPolicy()
	if patches := restarted.activePatches(); len(patches) != 1 || !slices.Equal(patches[0].Talkgroups, got.Talkgroups) || !patches[0].UpdatedAt.Equal(got.UpdatedAt) {
		t.Fatal("updated policy did not survive controller restart", patches)
	}
	if _, err := s.applyOperator("patch", editMembership(before, 1001, 1004), desk); err == nil {
		t.Fatal("stale membership edit accepted")
	}
}

func TestPatchMembershipRequiresOldAndNewPermissionOwnerAndFixedKey(t *testing.T) {
	cases := []struct {
		name  string
		alter func(*Service, *operatorAuthority, *OperatorInput)
	}{
		{"no_patch_permission", func(_ *Service, a *operatorAuthority, _ *OperatorInput) { a.user.Permissions = []string{"ptt"} }},
		{"removed_old_member_access", func(_ *Service, a *operatorAuthority, _ *OperatorInput) { a.user.Talkgroups = []int{1001, 1003} }},
		{"new_member_access", func(_ *Service, a *operatorAuthority, _ *OperatorInput) { a.user.Talkgroups = []int{1001, 1002} }},
		{"rename", func(_ *Service, _ *operatorAuthority, in *OperatorInput) { in.Patch.Name = "Replacement" }},
		{"rekey", func(_ *Service, _ *operatorAuthority, in *OperatorInput) {
			in.Patch.AlgorithmID, in.Patch.KeyID = 132, 7
		}},
		{"duplicate", func(_ *Service, _ *operatorAuthority, in *OperatorInput) { in.Patch.Talkgroups = []int{1001, 1001} }},
		{"missing_previous_members", func(_ *Service, _ *operatorAuthority, in *OperatorInput) { in.Patch.PreviousTalkgroups = nil }},
		{"too_few", func(_ *Service, _ *operatorAuthority, in *OperatorInput) { in.Patch.Talkgroups = []int{1001} }},
		{"foreign_owner", func(s *Service, _ *operatorAuthority, _ *OperatorInput) { s.config.InstanceID = "other-controller" }},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			s, desk, before := patchEditFixture(t, RoleZone)
			input := editMembership(before, 1001, 1003)
			c.alter(s, &desk, &input)
			if _, err := s.applyOperator("patch", input, desk); err == nil {
				t.Fatal("invalid membership change accepted")
			}
			got := s.operatorLocalSnapshot().Patches[0]
			if !slices.Equal(got.Talkgroups, before.Talkgroups) || !got.UpdatedAt.IsZero() {
				t.Fatal("rejected change altered stored patch")
			}
		})
	}
	for _, state := range []string{"ended", "conflict"} {
		t.Run(state, func(t *testing.T) {
			s, desk, before := patchEditFixture(t, RoleZone)
			local := s.operatorLocalSnapshot()
			local.Patches[0].State = state
			if state == "ended" {
				local.Patches[0].EndedAt = time.Now().UTC()
				local.Patches[0].Active = false
			}
			if err := s.writeCache("operator-local", local); err != nil {
				t.Fatal(err)
			}
			s.refreshOperatorPolicy()
			if _, err := s.applyOperator("patch", editMembership(before, 1001, 1003), desk); err == nil {
				t.Fatal("inactive/conflicted patch edited")
			}
			if s.filterOperatorState(s.operatorSnapshot(), desk).Patches[0].CanEdit {
				t.Fatal("unsupported patch edit advertised")
			}
		})
	}
}

func TestPatchMembershipSharedAlgorithmKeyAndSecurePermission(t *testing.T) {
	for _, bad := range []string{"none", "permission", "algorithm", "key", "clear"} {
		t.Run(bad, func(t *testing.T) {
			s, desk, before := patchEditFixture(t, RoleZone)
			for i := range s.view().System.Talkgroups {
				s.view().System.Talkgroups[i].AdmissionMode = "secure"
				s.view().System.Talkgroups[i].TransmitMode = "secure"
				s.view().System.Talkgroups[i].AlgorithmID = 129
				s.view().System.Talkgroups[i].KeyID = 0
			}
			local := s.operatorLocalSnapshot()
			local.Patches[0].AlgorithmID = 129
			local.Patches[0].KeyID = 0
			if err := s.writeCache("operator-local", local); err != nil {
				t.Fatal(err)
			}
			s.refreshOperatorPolicy()
			before = local.Patches[0]
			for i := range s.view().System.Talkgroups {
				g := &s.view().System.Talkgroups[i]
				if g.TGID != 1003 {
					continue
				}
				switch bad {
				case "algorithm":
					g.AlgorithmID = 132
				case "key":
					g.KeyID = 7
				case "clear":
					g.AdmissionMode = "clear"
					g.TransmitMode = "clear"
				}
			}
			if bad == "permission" {
				desk.user.Permissions = []string{"patch"}
			}
			result, err := s.applyOperator("patch", editMembership(before, 1001, 1003), desk)
			if bad == "none" || bad == "algorithm" || bad == "key" {
				if err != nil || result.Patches[0].AlgorithmID != 129 || result.Patches[0].KeyID != 0 {
					t.Fatal("console defaults changed fixed patch identity", result, err)
				}
				q := routing.Request{CallRequest: dispatch.CallRequest{Talkgroups: []int{1001}, Secure: true, AlgorithmID: 129, KeyID: 0}}
				if _, err := (&callBroker{s: s}).expandPatch(q); err != nil {
					t.Fatal("actual patch identity rejected", err)
				}
				q.AlgorithmID, q.KeyID = 132, 7
				if _, err := (&callBroker{s: s}).expandPatch(q); err == nil {
					t.Fatal("patch translated actual different key")
				}
			} else if err == nil {
				t.Fatal("encrypted patch restriction bypassed")
			}
		})
	}
}

func TestPatchMembershipRefusesBusyOldNewAndOverlappingResources(t *testing.T) {
	for _, busy := range []int{1001, 1002, 1003} {
		t.Run(string(rune('a'+busy-1001)), func(t *testing.T) {
			s, desk, before := patchEditFixture(t, RoleZone)
			s.peers = newMediaPeers(s)
			s.broker = newCallBroker(s)
			t.Cleanup(func() { s.broker.Close(); s.peers.Close() })
			grant, err := s.broker.Begin(context.Background(), "application-1", routing.Request{CallRequest: dispatch.CallRequest{ID: "busy-edit", UserID: desk.actor.UserID, RadioID: desk.actor.RadioID, Talkgroups: []int{busy}}, SourceKind: "application", Proof: desk.actor.Proof})
			if err != nil {
				t.Fatal(err)
			}
			if _, err = s.applyOperator("patch", editMembership(before, 1001, 1003), desk); err == nil {
				t.Fatal("busy old/new member edited")
			}
			if !slices.Equal(s.operatorLocalSnapshot().Patches[0].Talkgroups, before.Talkgroups) {
				t.Fatal("busy edit changed routing")
			}
			if err = s.broker.End(context.Background(), "application-1", grant); err != nil {
				t.Fatal(err)
			}
			operatorApply(t, s, "patch", editMembership(before, 1001, 1003), desk)
		})
	}
	s, desk, before := patchEditFixture(t, RoleZone)
	operatorApply(t, s, "patch", OperatorInput{Action: "activate", ID: "other-patch", Patch: &PatchInput{Name: "Other", Talkgroups: []int{1003, 1004}}}, desk)
	if _, err := s.applyOperator("patch", editMembership(before, 1001, 1003), desk); err == nil {
		t.Fatal("edit joined another active patch")
	}
}

func TestPatchMembershipConcurrentEditsAndPersistenceFailure(t *testing.T) {
	s, desk, before := patchEditFixture(t, RoleZone)
	start := make(chan struct{})
	results := make(chan error, 2)
	var wg sync.WaitGroup
	for _, groups := range [][]int{{1001, 1003}, {1001, 1004}} {
		wg.Add(1)
		go func(groups []int) {
			defer wg.Done()
			<-start
			_, err := s.applyOperator("patch", editMembership(before, groups...), desk)
			results <- err
		}(groups)
	}
	close(start)
	wg.Wait()
	close(results)
	success := 0
	for err := range results {
		if err == nil {
			success++
		}
	}
	if success != 1 {
		t.Fatalf("stale concurrent edit accepted: %d successes", success)
	}
	current := s.operatorLocalSnapshot().Patches[0]
	policy := s.operatorPolicy.Load()
	if _, err := s.Store.db.Exec("CREATE TRIGGER reject_patch_update BEFORE UPDATE ON framework_documents WHEN NEW.name='cache:operator-local' BEGIN SELECT RAISE(ABORT,'fixture write failure'); END"); err != nil {
		t.Fatal(err)
	}
	if _, err := s.applyOperator("patch", editMembership(current, 1001, 1005), desk); err == nil {
		t.Fatal("storage failure accepted")
	}
	if got := s.operatorLocalSnapshot().Patches[0]; !reflect.DeepEqual(got, current) || policy != s.operatorPolicy.Load() {
		t.Fatal("failed write published partial/new routing")
	}
}

func TestPatchMembershipReplicationRejectsStaleEditsAndRetainsEnd(t *testing.T) {
	zone, desk, before := patchEditFixture(t, RoleZone)
	edited := operatorApply(t, zone, "patch", editMembership(before, 1001, 1003), desk)
	stale := emptyOperatorSnapshot()
	stale.Patches = []RuntimePatch{before}
	for _, pair := range [][2]OperatorSnapshot{{stale, edited}, {edited, stale}} {
		merged := mergeOperatorSnapshots(pair[0], pair[1])
		if !slices.Equal(merged.Patches[0].Talkgroups, []int{1001, 1003}) {
			t.Fatal("stale edit won merge")
		}
	}
	site, siteDesk, local := patchEditFixture(t, RoleSite)
	if err := site.acceptOperatorSnapshot(edited); err != nil {
		t.Fatal(err)
	}
	if err := site.acceptOperatorSnapshot(stale); err != nil {
		t.Fatal(err)
	}
	var upstream OperatorSnapshot
	site.readCache("operator-upstream", &upstream)
	if !slices.Equal(upstream.Patches[0].Talkgroups, []int{1001, 1003}) {
		t.Fatal("stale upstream replacement reverted edit")
	}
	if got := site.operatorLocalSnapshot().Patches[0]; got.State != "conflict" || got.ScopeID != "site-1" {
		t.Fatal("Zone edit did not retain local conflict/ownership", got)
	}
	if _, err := site.applyOperator("patch", editMembership(local, 1001, 1004), siteDesk); err == nil {
		t.Fatal("conflicted Site-local patch edited")
	}
	ended := operatorApply(t, zone, "patch", OperatorInput{Action: "end", ID: before.ID}, desk)
	for _, pair := range [][2]OperatorSnapshot{{ended, edited}, {edited, ended}} {
		merged := mergeOperatorSnapshots(pair[0], pair[1])
		if merged.Patches[0].Active || merged.Patches[0].EndedAt.IsZero() {
			t.Fatal("stale edit resurrected ended patch")
		}
	}
	for _, change := range []func(*RuntimePatch){func(p *RuntimePatch) { p.ScopeID = "site-2" }, func(p *RuntimePatch) { p.CreatedBy = "foreign" }, func(p *RuntimePatch) { p.KeyID = 99 }} {
		bad := edited
		bad.Patches = slices.Clone(edited.Patches)
		change(&bad.Patches[0])
		if merged := mergeOperatorSnapshots(edited, bad); !samePatchIdentity(merged.Patches[0], edited.Patches[0]) {
			t.Fatal("foreign identity replaced patch")
		}
	}
}

func TestPatchMembershipAdmissionWaitsForWholeStoredPolicy(t *testing.T) {
	s, desk, before := patchEditFixture(t, RoleZone)
	s.peers = newMediaPeers(s)
	s.broker = newCallBroker(s)
	t.Cleanup(func() { s.broker.Close(); s.peers.Close() })
	connection, err := s.Store.db.Conn(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	var once sync.Once
	release := func() { once.Do(func() { connection.Close() }) }
	defer release()
	edited := make(chan error, 1)
	go func() { _, err := s.applyOperator("patch", editMembership(before, 1001, 1003), desk); edited <- err }()
	deadline := time.Now().Add(time.Second)
	for s.patchMu.TryRLock() {
		s.patchMu.RUnlock()
		if time.Now().After(deadline) {
			t.Fatal("edit did not acquire admission guard")
		}
		time.Sleep(time.Millisecond)
	}
	type result struct {
		grant dispatch.Grant
		err   error
	}
	admitted := make(chan result, 1)
	go func() {
		g, err := s.broker.Begin(context.Background(), "application-1", routing.Request{CallRequest: dispatch.CallRequest{ID: "atomic-edit", UserID: desk.actor.UserID, RadioID: desk.actor.RadioID, Talkgroups: []int{1001}}, SourceKind: "application", Proof: desk.actor.Proof})
		admitted <- result{g, err}
	}()
	select {
	case <-admitted:
		t.Fatal("call admission bypassed a pending stored patch edit")
	case <-time.After(20 * time.Millisecond):
	}
	release()
	if err := <-edited; err != nil {
		t.Fatal(err)
	}
	select {
	case got := <-admitted:
		if got.err != nil || !slices.Equal(got.grant.Talkgroups, []int{1001, 1003}) {
			t.Fatal("call saw partial or old membership", got.grant, got.err)
		}
		if err := s.broker.End(context.Background(), "application-1", got.grant); err != nil {
			t.Fatal(err)
		}
	case <-time.After(time.Second):
		t.Fatal("call admission did not resume")
	}
}

func TestPatchMembershipThroughAuthenticatedDispatchAndCurrentController(t *testing.T) {
	c, _ := featureCluster(t)
	var system SystemConfig
	if err := c.system.service.owned(&system); err != nil {
		t.Fatal(err)
	}
	third := system.Talkgroups[0]
	third.TGID, third.Alias = 1003, "Third"
	if index := slices.IndexFunc(system.Talkgroups, func(group Talkgroup) bool { return group.TGID == 1003 }); index >= 0 {
		system.Talkgroups[index] = third
	} else {
		system.Talkgroups = append(system.Talkgroups, third)
	}
	for i := range system.ConsolePositions {
		if !slices.Contains(system.ConsolePositions[i].Talkgroups, 1003) {
			system.ConsolePositions[i].Talkgroups = append(system.ConsolePositions[i].Talkgroups, 1003)
		}
	}
	if err := c.system.service.Store.SaveConfig(storeDoc(t, system)); err != nil {
		t.Fatal(err)
	}
	for i := range c.users {
		if !slices.Contains(c.users[i].Talkgroups, 1003) {
			c.users[i].Talkgroups = append(c.users[i].Talkgroups, 1003)
		}
		if err := c.system.service.Store.SaveUser(c.users[i].ID, c.users[i], ""); err != nil {
			t.Fatal(err)
		}
	}
	integrationSyncAll(t, c.integrationCluster)
	one, two := featureLogin(t, c.dispatch, c.users[0]), featureLogin(t, c.dispatch, c.users[1])
	created := featureAction(t, one, "patch", OperatorInput{Action: "activate", ID: "http-membership", Patch: &PatchInput{Name: "Live patch", Talkgroups: []int{1001, 1002}}})
	before := created.Patches[0]
	if !before.CanEdit {
		t.Fatal("owner did not expose an allowed edit")
	}
	updated := featureAction(t, two, "patch", editMembership(before, 1001, 1003))
	if got := updated.Patches[0]; !samePatchIdentity(before, got) || got.CreatedBy != c.users[0].ID || got.UpdatedBy != c.users[1].ID || !slices.Equal(got.Talkgroups, []int{1001, 1003}) {
		t.Fatal("authenticated edit lost actor or ownership", got)
	}
	if status, _ := one.request(t, http.MethodPost, "/api/operator/patch", editMembership(before, 1002, 1003)); status == 200 {
		t.Fatal("stale desk overwrote another desk's edit")
	}
	if got := featureState(t, one).Patches[0]; !slices.Equal(got.Talkgroups, []int{1001, 1003}) {
		t.Fatal("desk refresh did not see accepted membership")
	}
}

func TestPatchMembershipSiteLocalEditIsReportedWithoutPromotionOrStaleReversion(t *testing.T) {
	zone, desk, created := patchEditFixture(t, RoleZone)
	operatorApply(t, zone, "patch", OperatorInput{Action: "end", ID: created.ID}, desk)
	site, localDesk, before := patchEditFixture(t, RoleSite)
	stale := site.operatorLocalSnapshot()
	updated := operatorApply(t, site, "patch", editMembership(before, 1001, 1003), localDesk)
	if updated.Patches[0].ScopeID != "site-1" || updated.Patches[0].CreatedBy != before.CreatedBy {
		t.Fatal("Site edit changed owner/creator")
	}
	for _, snapshot := range []OperatorSnapshot{updated, stale} {
		if err := zone.mergeSiteOperatorSnapshot("site-1", snapshot); err != nil {
			t.Fatal(err)
		}
	}
	if len(zone.activePatches()) != 0 {
		t.Fatal("Site-local membership edit became a wide-area patch")
	}
	for _, p := range zone.operatorSnapshot().Patches {
		if p.ID == before.ID && (!slices.Equal(p.Talkgroups, []int{1001, 1003}) || p.ScopeID != "site-1") {
			t.Fatal("Site report lost updated membership or ownership")
		}
	}
}

func TestPatchMembershipEditFactsRemainOrderedAfterClockCorrection(t *testing.T) {
	s, desk, before := patchEditFixture(t, RoleZone)
	local := s.operatorLocalSnapshot()
	if err := s.updatePatchMembership(&local, local, editMembership(before, 1001, 1003), desk, before.At.Add(-time.Hour)); err != nil {
		t.Fatal(err)
	}
	first := local.Patches[0]
	if !first.UpdatedAt.After(before.At) {
		t.Fatal("edit fact predates creation")
	}
	if err := s.updatePatchMembership(&local, local, editMembership(first, 1001, 1004), desk, before.At.Add(-time.Hour)); err != nil {
		t.Fatal(err)
	}
	if !local.Patches[0].UpdatedAt.After(first.UpdatedAt) {
		t.Fatal("later owner edit did not remain ordered")
	}
}

func TestPatchMembershipQueuedUpdateRechecksCurrentDirectoryAuthority(t *testing.T) {
	for _, change := range []string{"patch_permission", "old_member", "new_member", "position_member", "password", "disabled", "secure_permission"} {
		t.Run(change, func(t *testing.T) {
			s, desk, before := patchEditFixture(t, RoleZone)
			if change == "secure_permission" {
				for i := range s.view().System.Talkgroups {
					g := &s.view().System.Talkgroups[i]
					g.AdmissionMode = "secure"
					g.TransmitMode = "secure"
					g.AlgorithmID = 129
					g.KeyID = 0
				}
				local := s.operatorLocalSnapshot()
				local.Patches[0].AlgorithmID = 129
				if err := s.writeCache("operator-local", local); err != nil {
					t.Fatal(err)
				}
				s.refreshOperatorPolicy()
				before = local.Patches[0]
			}
			s.patchMu.Lock()
			var once sync.Once
			release := func() { once.Do(s.patchMu.Unlock) }
			defer release()
			entered := make(chan struct{})
			result := make(chan error, 1)
			go func() {
				close(entered)
				_, err := s.applyOperator("patch", editMembership(before, 1001, 1003), desk)
				result <- err
			}()
			<-entered
			view := *s.view()
			view.Users = make(map[string]userRecord, len(s.view().Users))
			for id, record := range s.view().Users {
				if id == desk.actor.UserID {
					switch change {
					case "patch_permission", "secure_permission":
						permission := "patch"
						if change == "secure_permission" {
							permission = "secure_ptt"
						}
						record.User.Permissions = slices.DeleteFunc(slices.Clone(record.User.Permissions), func(value string) bool { return value == permission })
					case "old_member", "new_member":
						group := 1002
						if change == "new_member" {
							group = 1003
						}
						record.User.Talkgroups = slices.DeleteFunc(slices.Clone(record.User.Talkgroups), func(value int) bool { return value == group })
					case "password":
						record.PasswordHash = []byte("replacement proof")
					case "disabled":
						record.User.Enabled = false
					}
				}
				view.Users[id] = record
			}
			if change == "position_member" {
				view.System.ConsolePositions = slices.Clone(view.System.ConsolePositions)
				for i := range view.System.ConsolePositions {
					p := &view.System.ConsolePositions[i]
					p.Talkgroups = slices.DeleteFunc(slices.Clone(p.Talkgroups), func(value int) bool { return value == 1002 })
				}
			}
			s.directory.value.Store(&view)
			release()
			select {
			case err := <-result:
				if err == nil {
					t.Fatal("queued mutation used revoked authority")
				}
			case <-time.After(time.Second):
				t.Fatal("queued update did not finish")
			}
			got := s.operatorLocalSnapshot().Patches[0]
			if !slices.Equal(got.Talkgroups, before.Talkgroups) || !got.UpdatedAt.IsZero() {
				t.Fatal("revoked update changed retained routing")
			}
		})
	}
}
