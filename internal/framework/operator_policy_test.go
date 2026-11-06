package framework

import (
	"bytes"
	"context"
	"slices"
	"sync"
	"testing"
	"time"

	"open25.local/open25-fne/internal/dispatch"
	"open25.local/open25-fne/internal/routing"
)

func TestOperatorSnapshotsUnchangedWritesSkipUpdates(t *testing.T) {
	s := storeOpen(t, RoleSite)
	local, upstream := emptyOperatorSnapshot(), emptyOperatorSnapshot()
	if err := s.acceptOperatorSnapshots(&local, upstream); err != nil {
		t.Fatal(err)
	}
	beforeLocal, localAt, _ := s.ReadCache("operator-local")
	beforeUpstream, upstreamAt, _ := s.ReadCache("operator-upstream")
	if _, err := s.db.Exec(`CREATE TRIGGER reject_operator_update BEFORE UPDATE ON framework_documents
		WHEN NEW.name IN ('cache:operator-local','cache:operator-upstream')
		BEGIN SELECT RAISE(FAIL, 'unexpected operator update'); END`); err != nil {
		t.Fatal(err)
	}
	for _, retainedLocal := range []*OperatorSnapshot{&local, nil} {
		if err := s.acceptOperatorSnapshots(retainedLocal, upstream); err != nil {
			t.Fatal("unchanged operator state attempted an UPDATE:", err)
		}
	}
	afterLocal, afterLocalAt, localErr := s.ReadCache("operator-local")
	afterUpstream, afterUpstreamAt, upstreamErr := s.ReadCache("operator-upstream")
	if localErr != nil || upstreamErr != nil || !bytes.Equal(beforeLocal, afterLocal) || !localAt.Equal(afterLocalAt) ||
		!bytes.Equal(beforeUpstream, afterUpstream) || !upstreamAt.Equal(afterUpstreamAt) {
		t.Fatal("unchanged operator state changed its saved documents or timestamps")
	}
}

func TestMediaPolicyValidationDoesNotWaitForControlStorageOrFeatureLock(t *testing.T) {
	for _, blocked := range []string{"feature_lock", "configuration_storage"} {
		t.Run(blocked, func(t *testing.T) {
			s, desk, _ := operatorFixture(t, RoleZone)
			operatorApply(t, s, "patch", OperatorInput{ID: "accepted-patch", Action: "activate", Patch: &PatchInput{Name: "Accepted patch", Talkgroups: []int{1001, 1002}}}, desk)
			s.peers = newMediaPeers(s)
			s.broker = newCallBroker(s)
			defer s.broker.Close()
			defer s.peers.Close()
			grant, err := s.broker.Begin(context.Background(), "application-1", routing.Request{CallRequest: dispatch.CallRequest{ID: "memory-policy", SessionID: "memory-session", UserID: desk.actor.UserID, RadioID: desk.actor.RadioID, Talkgroups: []int{1001}}, SourceKind: "application", Proof: desk.actor.Proof})
			if err != nil {
				t.Fatal(err)
			}
			if len(grant.Talkgroups) != 2 {
				t.Fatal("accepted patch was not included in the call policy")
			}
			var release func()
			if blocked == "feature_lock" {
				s.featureMu.Lock()
				release = s.featureMu.Unlock
			} else {
				connection, err := s.Store.db.Conn(context.Background())
				if err != nil {
					t.Fatal(err)
				}
				release = func() { _ = connection.Close() }
			}
			var once sync.Once
			unlock := func() { once.Do(release) }
			defer unlock()
			for _, operation := range []string{"write", "renew"} {
				ctx, cancel := context.WithTimeout(context.Background(), 500*time.Millisecond)
				done := make(chan error, 1)
				go func() {
					if operation == "write" {
						done <- s.broker.Write(ctx, "application-1", grant, dispatch.Frame{CallID: grant.ID, SourceID: desk.actor.RadioID, Sequence: 1, PCM: make([]byte, dispatch.FrameBytes), CapturedAt: time.Now()})
					} else {
						done <- s.broker.Renew(ctx, "application-1", grant)
					}
				}()
				select {
				case err := <-done:
					cancel()
					if err != nil {
						t.Fatal(operation, err)
					}
				case <-time.After(100 * time.Millisecond):
					unlock()
					cancel()
					select {
					case <-done:
					case <-time.After(time.Second):
						t.Fatal("policy lookup remained blocked after releasing control storage")
					}
					t.Fatalf("media %s waited for %s despite an accepted unchanged policy", operation, blocked)
				}
			}
		})
	}
}

func TestOperatorPolicyRetainedRestartAndOwnedPatchSlices(t *testing.T) {
	s, desk, mobile := operatorFixture(t, RoleZone)
	operatorApply(t, s, "patch", OperatorInput{ID: "retained-patch", Action: "activate", Patch: &PatchInput{Name: "Retained", Talkgroups: []int{1001, 1002}}}, desk)
	operatorApply(t, s, "emergency", OperatorInput{ID: "retained-emergency", Action: "declare", TalkgroupID: 1001}, mobile)
	restarted := &Service{Store: s.Store, config: s.Config()}
	restarted.directory.value.Store(s.view())
	restarted.refreshOperatorPolicy()
	if !restarted.activeEmergency(mobile.actor.RadioID, 1001) || len(restarted.activePatches()) != 1 {
		t.Fatal("retained patch/emergency policy was not restored")
	}
	patches := restarted.activePatches()
	patches[0].Talkgroups[0] = 1099
	if !slices.Equal(restarted.activePatches()[0].Talkgroups, []int{1001, 1002}) {
		t.Fatal("caller changed the accepted immutable patch policy")
	}
	operatorApply(t, restarted, "emergency", OperatorInput{ID: "retained-emergency", Action: "clear"}, desk)
	if restarted.activeEmergency(mobile.actor.RadioID, 1001) {
		t.Fatal("accepted emergency clear did not replace the live policy")
	}
	if err := restarted.writeCache("operator-upstream", map[string]any{"emergencies": "invalid"}); err != nil {
		t.Fatal(err)
	}
	previous := restarted.operatorPolicy.Load()
	restarted.featureMu.Lock()
	err := restarted.publishOperatorPolicyLocked()
	restarted.featureMu.Unlock()
	if err == nil || previous != restarted.operatorPolicy.Load() {
		t.Fatal("invalid retained state replaced the previously accepted policy")
	}
}

func TestOperatorSnapshotPublishesOnlyAfterBothReconciliationWrites(t *testing.T) {
	s, desk, _ := operatorFixture(t, RoleSite)
	if err := s.acceptOperatorSnapshot(emptyOperatorSnapshot()); err != nil {
		t.Fatal(err)
	}
	operatorApply(t, s, "patch", OperatorInput{ID: "local-patch", Action: "activate", Patch: &PatchInput{Name: "Local", Talkgroups: []int{1001, 1002}}}, desk)
	previous := s.operatorPolicy.Load()
	localBefore, localAt, err := s.Store.ReadCache("operator-local")
	if err != nil {
		t.Fatal(err)
	}
	upstreamBefore, upstreamAt, err := s.Store.ReadCache("operator-upstream")
	if err != nil {
		t.Fatal(err)
	}
	_, err = s.Store.db.Exec(`CREATE TRIGGER reject_operator_upstream BEFORE INSERT ON framework_documents
		WHEN NEW.name='cache:operator-upstream' BEGIN SELECT RAISE(FAIL, 'rejected test write'); END`)
	if err != nil {
		t.Fatal(err)
	}
	upstream := emptyOperatorSnapshot()
	upstream.Patches = []RuntimePatch{{ID: "zone-patch", Name: "Zone", Talkgroups: []int{1001, 1002}, Active: true, ScopeID: "zone-1", State: "active", At: time.Now().UTC()}}
	if err := s.acceptOperatorSnapshot(upstream); err == nil {
		t.Fatal("failed upstream write was accepted")
	}
	if previous != s.operatorPolicy.Load() || len(s.activePatches()) != 1 || s.activePatches()[0].ID != "local-patch" {
		t.Fatal("half-reconciled policy became visible before the upstream write succeeded")
	}
	localAfter, localAfterAt, err := s.Store.ReadCache("operator-local")
	if err != nil {
		t.Fatal(err)
	}
	upstreamAfter, upstreamAfterAt, err := s.Store.ReadCache("operator-upstream")
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(localBefore, localAfter) || !bytes.Equal(upstreamBefore, upstreamAfter) || !localAt.Equal(localAfterAt) || !upstreamAt.Equal(upstreamAfterAt) {
		t.Fatal("failed reconciliation changed the retained local/upstream documents")
	}
	var number int
	var name, filename string
	if err := s.Store.db.QueryRow("PRAGMA database_list").Scan(&number, &name, &filename); err != nil {
		t.Fatal(err)
	}
	retained, err := OpenStore(filename, RoleSite)
	if err != nil {
		t.Fatal(err)
	}
	defer retained.Close()
	restarted := &Service{Store: retained, config: s.Config()}
	restarted.directory.value.Store(s.view())
	restarted.refreshOperatorPolicy()
	if got := restarted.activePatches(); len(got) != 1 || got[0].ID != "local-patch" {
		t.Fatalf("failed reconciliation changed the policy after restart: %#v", got)
	}
	if _, err := s.Store.db.Exec("DROP TRIGGER reject_operator_upstream"); err != nil {
		t.Fatal(err)
	}
	if err := s.acceptOperatorSnapshot(upstream); err != nil {
		t.Fatal(err)
	}
	if got := s.activePatches(); len(got) != 1 || got[0].ID != "zone-patch" {
		t.Fatalf("accepted reconciliation did not replace the complete policy: %#v", got)
	}
}
