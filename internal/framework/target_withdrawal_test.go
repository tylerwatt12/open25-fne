package framework

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"open25.local/open25-fne/internal/dispatch"
	"open25.local/open25-fne/internal/radio"
	"open25.local/open25-fne/internal/routing"
)

func TestTargetSelectionPreservesEligibilityAndCapturesExclusions(t *testing.T) {
	credentialPath := filepath.Join(t.TempDir(), "credentials.json")
	if err := WritePrivateJSON(credentialPath, ServiceCredentials{Peers: map[string]PeerCredential{
		"site-peer":       {Role: RoleSite, InstanceID: "site-1", Token: strings.Repeat("s", 32)},
		"other-site-peer": {Role: RoleSite, InstanceID: "site-2", Token: strings.Repeat("o", 32)},
	}}); err != nil {
		t.Fatal(err)
	}
	q := routing.Request{CallRequest: dispatch.CallRequest{ID: "selection-diagnostic", RadioID: 10002, Talkgroups: []int{1001}}, SourceKind: "application"}
	// This pass selects eligible demand from accepted authority. The receiving
	// Site's final native/SIP check owns health; web reports do not own membership.
	for _, tc := range []struct {
		name     string
		change   func(*Service, *directorySnapshot)
		want     string
		wantSite string
	}{
		{name: "eligible"},
		{name: "missing zone", change: func(_ *Service, v *directorySnapshot) { v.Zone = nil }, want: "zone"},
		{name: "missing report", change: func(_ *Service, v *directorySnapshot) { v.Reports = nil }},
		{name: "missing radio report", change: func(_ *Service, v *directorySnapshot) { v.Reports[0].Radio = nil }},
		{name: "stale report", change: func(_ *Service, v *directorySnapshot) { v.Reports[0].ReceivedAt = time.Now().Add(-time.Hour) }},
		{name: "fresh report", change: func(_ *Service, v *directorySnapshot) { v.Reports[0].ReceivedAt = time.Now().Add(-3 * time.Second) }},
		{name: "empty report affiliation table", change: func(_ *Service, v *directorySnapshot) { v.Reports[0].Radio.Affiliations = nil }},
		{name: "stopped radio report", change: func(_ *Service, v *directorySnapshot) { v.Reports[0].Radio.State = "stopped" }},
		{name: "failed radio report", change: func(_ *Service, v *directorySnapshot) { v.Reports[0].Error = "unreachable" }},
		{name: "missing authority", change: func(s *Service, _ *directorySnapshot) { s.rfAuthority = nil }, want: "authority"},
		{name: "closed authority", change: func(s *Service, _ *directorySnapshot) { s.rfAuthority.close() }, want: "authority"},
		{name: "corrupt authority on reopen", change: func(s *Service, v *directorySnapshot) {
			current := s.rfAuthority
			current.close()
			if err := os.WriteFile(current.path, []byte("{"), 0600); err != nil {
				t.Fatal(err)
			}
			broken, err := openRFSourceAuthority(current.path, v.Zone.ID)
			if err == nil || broken != nil {
				if broken != nil {
					broken.close()
				}
				t.Fatal("corrupt authority file reopened successfully")
			}
			s.rfAuthority = nil
		}, want: "authority"},
		{name: "fenced authority", change: func(s *Service, _ *directorySnapshot) {
			accepted := s.rfAuthority.value.Load()
			// Explicit immutable pending-commit fault fixture; reports cannot
			// authorize this RID while its durable authority is fenced.
			pending := *accepted
			pending.fenced = map[int]bool{10001: true}
			s.rfAuthority.value.Store(&pending)
		}, want: "owner mismatch 1"},
		{name: "accepted deregistration", change: func(s *Service, _ *directorySnapshot) {
			now := time.Now().UTC()
			u := rfAuthorityUpdate{SiteID: "site-1", NativeStartedAt: s.rfAuthority.value.Load().document.NativeRuns["site-1"], ObservedAt: now, Terminals: []radio.Registration{{RID: 10001, At: now}}}
			if err := s.rfAuthority.apply(context.Background(), u, s.rfAuthorityScope()); err != nil {
				t.Fatal(err)
			}
		}, want: "matching 0"},
		{name: "newer accepted other Site owner", change: func(s *Service, v *directorySnapshot) {
			v.Zone.Sites = append(v.Zone.Sites, SiteIdentity{ID: "site-2", Endpoint: "http://localhost:5103"})
			publishFixtureRFState(t, s, "site-2", radio.Status{Affiliations: []radio.Affiliation{{RID: 10001, TGID: 1001, At: time.Now().UTC()}}})
		}, wantSite: "site-2"},
		{name: "site group policy", change: func(_ *Service, v *directorySnapshot) {
			v.System.Talkgroups[0].SiteAccess, v.System.Talkgroups[0].SiteIDs = "selected", []string{"site-2"}
		}, want: "policy"},
		{name: "desired inhibition", change: func(s *Service, _ *directorySnapshot) {
			s.inhibition.Store(inhibitionSnapshot(radioInhibitionPolicy{Desired: []int{10001}}, nil))
		}, want: "inhibited 1"},
		{name: "reported inhibition cannot override accepted policy", change: func(_ *Service, v *directorySnapshot) {
			v.Reports[0].Radio.Inhibition = []radio.InhibitState{{RadioID: 10001, Blocked: true}}
		}},
		{name: "missing inhibition policy", change: func(s *Service, _ *directorySnapshot) { s.inhibition.Store(nil) }, want: "inhibited 1"},
		{name: "invalid inhibition policy", change: func(s *Service, _ *directorySnapshot) {
			s.inhibition.Store(inhibitionSnapshot(radioInhibitionPolicy{}, errors.New("injected policy load failure")))
		}, want: "inhibited 1"},
		{name: "disabled physical subscriber", change: func(_ *Service, v *directorySnapshot) { v.System.Radios[0].Enabled = false }, want: "subscriber denied 1"},
		{name: "subscriber changed to virtual radio", change: func(_ *Service, v *directorySnapshot) { v.System.Radios[0].Kind = "console" }, want: "subscriber denied 1"},
		{name: "subscriber policy", change: func(_ *Service, v *directorySnapshot) {
			v.System.Talkgroups[0].AffiliationAccess, v.System.Talkgroups[0].AffiliationRadioIDs = "selected", []int{10003}
		}, want: "subscriber denied 1"},
		{name: "peer lookup", change: func(s *Service, _ *directorySnapshot) {
			s.config.CredentialFile = filepath.Join(t.TempDir(), "missing.json")
		}, want: "peer"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			now := time.Now()
			zone := storeFixtureZone()
			view := &directorySnapshot{System: storeFixtureSystem(), Zone: &zone,
				Reports: []SiteReport{{ID: "site-1", ReceivedAt: now, ObservedAt: now,
					Radio: &radio.Status{SiteID: "site-1", State: "running", Affiliations: []radio.Affiliation{{RID: 10001, TGID: 1001, At: now.Add(-time.Minute)}}}}}}
			s := &Service{config: FileConfig{Role: RoleZone, InstanceID: "zone-1", CredentialFile: credentialPath}}
			s.inhibition.Store(inhibitionSnapshot(radioInhibitionPolicy{Desired: []int{}}, nil))
			s.directory.value.Store(view)
			publishFixtureRFState(t, s, "site-1", *view.Reports[0].Radio)
			if tc.change != nil {
				tc.change(s, view)
			}
			s.directory.value.Store(view)
			got := (&callBroker{s: s}).targets(q)
			if tc.want == "" {
				wantSite := tc.wantSite
				if wantSite == "" {
					wantSite = "site-1"
				}
				if len(got.Targets) != 1 || got.Targets[0].ID != wantSite || !slices.Equal(got.Targets[0].Groups, q.Talkgroups) || got.Excluded[wantSite] != "" {
					t.Fatalf("unchanged eligible Site/groups were lost: %+v", got)
				}
				if wantSite == "site-2" && !strings.Contains(got.Excluded["site-1"], "matching 0") {
					t.Fatalf("newer accepted owner left old Site demand eligible: %+v", got)
				}
				return
			}
			if len(got.Targets) != 0 {
				t.Fatalf("diagnostics admitted an ineligible Site: %+v", got)
			}
			reason := got.Excluded["site-1"]
			if reason == "" {
				reason = got.Reason
			}
			if !strings.Contains(strings.ToLower(reason), tc.want) {
				t.Fatalf("exclusion did not distinguish %q: %q", tc.want, reason)
			}
			if tc.name == "fenced authority" {
				// A new directory must not rewrite the decision already returned.
				s.directory.value.Store(&directorySnapshot{System: view.System, Zone: view.Zone})
				if got.Excluded["site-1"] != reason {
					t.Fatal("later directory publication rewrote the captured exclusion")
				}
			}
		})
	}
}
