package framework

import (
	"context"
	"slices"
	"testing"

	"open25.local/open25-fne/internal/dispatch"
	"open25.local/open25-fne/internal/routing"
)

func pendingPatchFixture(t *testing.T, defaultKey int) (*Service, *routing.Router, *[]dispatch.Frame) {
	t.Helper()
	s := securityPolicyService(
		Talkgroup{TGID: 1001, Enabled: true, AdmissionMode: "both", TransmitMode: "selectable", AlgorithmID: 132, KeyID: defaultKey},
		Talkgroup{TGID: 1002, Enabled: true, AdmissionMode: "both", TransmitMode: "selectable", AlgorithmID: 132, KeyID: defaultKey},
		Talkgroup{TGID: 1003, Enabled: true, AdmissionMode: "both", TransmitMode: "selectable", AlgorithmID: 132, KeyID: defaultKey},
	)
	s.config = FileConfig{Role: RoleZone, InstanceID: "zone-1"}
	setPendingPatch(s, RuntimePatch{ID: "shared", ScopeID: "zone-1", Active: true, State: "active", Talkgroups: []int{1001, 1002}, AlgorithmID: 132, KeyID: 7})
	if len(s.activePatches()) != 1 {
		t.Fatal("console default key hid the active shared-key patch")
	}
	var published []dispatch.Frame
	r := routing.New(routing.Options{
		Authorize: func(q routing.Request) error { _, err := s.prepareCall(q.CallRequest, "rf", false); return err },
		Expand:    (&callBroker{s: s}).expandPatch,
		Publish: func(_ routing.Request, f dispatch.Frame) {
			if !f.End {
				published = append(published, f)
			}
		},
	})
	t.Cleanup(func() { _ = r.Close() })
	return s, r, &published
}

func setPendingPatch(s *Service, p RuntimePatch) {
	s.operatorPolicy.Store(&operatorPolicySnapshot{patches: []RuntimePatch{p}})
}

func pendingPatchRequest() routing.Request {
	return routing.Request{CallRequest: dispatch.CallRequest{ID: "pending-patch", RadioID: 10001, Talkgroups: []int{1001}, Secure: true, EncryptionPending: true}, SourceKind: "rf", OriginSite: "site1"}
}

func pendingPatchFrame(id string, key uint16) dispatch.Frame {
	f := sipTestFrame(id, 10001, 1001, 1, 0)
	f.Encrypted, f.AlgorithmID, f.KeyID, f.ServiceOptions = true, 132, key, 0x44
	f.P25.Header.AlgorithmID, f.P25.Header.KeyID = 132, key
	return f
}

func TestPendingSecurePatchReservationUsesActualKey(t *testing.T) {
	for _, key := range []uint16{7, 8} {
		t.Run(map[uint16]string{7: "matching", 8: "different"}[key], func(t *testing.T) {
			_, r, published := pendingPatchFixture(t, 7)
			g, err := r.Begin(context.Background(), "rf-owner", pendingPatchRequest())
			if err != nil {
				t.Fatalf("pending reservation rejected before actual key: %v", err)
			}
			if !slices.Equal(g.Talkgroups, []int{1001, 1002}) {
				t.Fatalf("patch grant: %+v", g)
			}
			f := pendingPatchFrame(g.ID, key)
			err = r.Write(context.Background(), "rf-owner", g, f)
			if key != 7 {
				if err == nil || len(*published) != 0 || len(r.Active()) != 0 {
					t.Fatalf("different key was not rejected and retired: err=%v frames=%d active=%d", err, len(*published), len(r.Active()))
				}
				return
			}
			if err != nil || len(*published) != 2 {
				t.Fatalf("matching media: %v frames=%d", err, len(*published))
			}
			for _, got := range *published {
				if got.AlgorithmID != f.AlgorithmID || got.KeyID != f.KeyID || !slices.Equal(got.Encoded, f.Encoded) {
					t.Fatal("patch changed actual ciphertext identity or bytes")
				}
			}
			active := r.Active()
			if len(active) != 1 || !active[0].EncryptionPending || active[0].AlgorithmID != 0 || active[0].KeyID != 0 {
				t.Fatalf("reservation invented fixed identity: %+v", active)
			}
		})
	}
}

func TestSecurePatchDoesNotUseConsoleDefaultKeyForRF(t *testing.T) {
	s, r, published := pendingPatchFixture(t, 8)
	g, err := r.Begin(context.Background(), "rf-owner", pendingPatchRequest())
	if err != nil {
		t.Fatal(err)
	}
	if err = r.Write(context.Background(), "rf-owner", g, pendingPatchFrame(g.ID, 7)); err != nil || len(*published) != 2 {
		t.Fatalf("actual patch key rejected: %v", err)
	}
	q, err := s.prepareTransmit(dispatch.CallRequest{Talkgroups: []int{1001}, Secure: true})
	if err != nil {
		t.Fatal(err)
	}
	if _, err = (&callBroker{s: s}).expandPatch(routing.Request{CallRequest: q}); err == nil {
		t.Fatal("console default key was rewritten to patch key")
	}
}

func TestPendingSecurePatchStillChecksMode(t *testing.T) {
	s, r, _ := pendingPatchFixture(t, 7)
	setPendingPatch(s, RuntimePatch{ID: "shared", ScopeID: "zone-1", Active: true, State: "active", Talkgroups: []int{1001, 1002}})
	if _, err := r.Begin(context.Background(), "rf-owner", pendingPatchRequest()); err == nil {
		t.Fatal("encrypted pending reservation entered a clear patch")
	}
}

func TestPendingPatchEndOrMembershipChangeRetiresReservation(t *testing.T) {
	for _, mode := range []string{"end", "membership", "key"} {
		t.Run(mode, func(t *testing.T) {
			s, r, published := pendingPatchFixture(t, 7)
			g, err := r.Begin(context.Background(), "rf-owner", pendingPatchRequest())
			if err != nil {
				t.Fatal(err)
			}
			patch := RuntimePatch{ID: "shared", ScopeID: "zone-1", Active: true, State: "active", Talkgroups: []int{1001, 1002}, AlgorithmID: 132, KeyID: 7}
			if mode == "end" {
				patch.Active = false
			} else if mode == "membership" {
				patch.Talkgroups = []int{1001, 1003}
			} else {
				patch.KeyID = 8
			}
			setPendingPatch(s, patch)
			err = r.Write(context.Background(), "rf-owner", g, pendingPatchFrame(g.ID, 7))
			if err == nil || len(*published) != 0 || len(r.Active()) != 0 {
				t.Fatalf("changed patch survived: err=%v frames=%d active=%d", err, len(*published), len(r.Active()))
			}
		})
	}
}
