//go:build linux || darwin

package framework

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"reflect"
	"slices"
	"strings"
	"testing"
	"time"

	"open25.local/open25-fne/internal/radio"
)

func TestRFSourcePublicationFiltersAllFactKindsWithoutChangingSnapshots(t *testing.T) {
	now := time.Now().UTC()
	run := now.Add(-time.Minute)
	state := radio.SourceState{SiteID: "site-1", NativeStartedAt: run, ObservedAt: now, Revision: 7,
		Affiliations:     []radio.Affiliation{{RID: 10001, TGID: 1001, At: now}, {RID: 10003, TGID: 1002, At: now}},
		Registrations:    []radio.Registration{{RID: 10001, At: now}, {RID: 10003, At: now}},
		Terminals:        []radio.Registration{{RID: 10001, At: now}, {RID: 10003, At: now}},
		NullAffiliations: []radio.Registration{{RID: 10001, At: now}, {RID: 10003, At: now}},
		Withdrawals:      []radio.SourceWithdrawal{{RadioID: 10001, GroupID: 1001, AffiliatedAt: now, NativeStartedAt: run}, {RadioID: 10003, GroupID: 1002, AffiliatedAt: now, NativeStartedAt: run}},
		Retired:          []radio.RetiredSourceFact{{Kind: "registration", RID: 10001, At: now, NativeStartedAt: run}, {Kind: "deregistration", RID: 10003, At: now, NativeStartedAt: run}}}
	before, err := json.Marshal(state)
	if err != nil {
		t.Fatal(err)
	}
	scope := rfAuthorityScope{Radios: map[int]bool{10003: true}, Groups: map[int]bool{1002: true}}
	got := rfSourceUpdateInScope(state, scope)
	if len(got.Affiliations) != 1 || got.Affiliations[0] != state.Affiliations[1] || len(got.Registrations) != 1 || got.Registrations[0] != state.Registrations[1] || len(got.Terminals) != 1 || got.Terminals[0] != state.Terminals[1] || len(got.NullAffiliations) != 1 || got.NullAffiliations[0] != state.NullAffiliations[1] || len(got.Retired) != 1 || got.Retired[0] != state.Retired[1] || len(got.Withdrawals) != 1 || got.Withdrawals[0].RadioID != 10003 || !got.Withdrawals[0].AffiliatedAt.Equal(now) || !got.Withdrawals[0].NativeStartedAt.Equal(run) {
		t.Fatalf("configured original facts were lost or obsolete identities escaped: %#v", got)
	}
	// Removing only a group drops membership/withdrawal, not a still-configured
	// unit's registration, terminal, null or retired evidence.
	scope.Radios[10001] = true
	groupOnly := rfSourceUpdateInScope(state, scope)
	if len(groupOnly.Affiliations) != 1 || len(groupOnly.Withdrawals) != 1 || len(groupOnly.Registrations) != 2 || len(groupOnly.Terminals) != 2 || len(groupOnly.NullAffiliations) != 2 || len(groupOnly.Retired) != 2 {
		t.Fatal("group deletion incorrectly removed non-membership unit evidence", groupOnly)
	}
	got.Affiliations[0].RID, got.Registrations[0].RID, got.Terminals[0].RID = 999, 999, 999
	got.NullAffiliations[0].RID, got.Retired[0].RID, got.Withdrawals[0].RadioID = 999, 999, 999
	after, err := json.Marshal(state)
	if err != nil || !bytes.Equal(before, after) {
		t.Fatal("egress filtering or caller mutation changed the accepted source snapshot", err)
	}
}

func TestRFSourcePublicationStoppedControllerScopeChangesDoNotBlockOtherFacts(t *testing.T) {
	for _, removed := range []string{"radio", "group"} {
		t.Run(removed, func(t *testing.T) {
			site, runtime := radioLifetimeFixture(t, false)
			site.operationCancel()
			site.operationWG.Wait()
			// The real controller/process fixture observes physical facts; only
			// its executable is a test double and no modem or RF is opened.
			t.Setenv("FRAMEWORK_SOURCE_FACT_LINES", strings.Join([]string{
				"I: (HOST) P25 Affiliation, unit registration, srcId = 10001",
				"I: (HOST) P25 Affiliation, group affiliation, srcId = 10001, dstId = 1001",
				"I: (HOST) P25 Affiliation, unit registration, srcId = 10003",
				"I: (HOST) P25 Affiliation, group affiliation, srcId = 10003, dstId = 1002",
				"I: (HOST) P25 Affiliation, unit deregistration, srcId = 10003",
			}, "\n"))
			system := storeFixtureSystem()
			system.Radios = append(system.Radios, Radio{RID: 10003, Alias: "Still configured", Kind: "subscriber", Enabled: true})
			system.Talkgroups = append(system.Talkgroups, Talkgroup{TGID: 1002, AdmissionMode: "clear", TransmitMode: "clear", Alias: "Retained group", Enabled: true, SiteAccess: "all"})
			if err := site.Store.WriteCache("system", storeDoc(t, system)); err != nil {
				t.Fatal(err)
			}
			site.refreshDirectory()
			profile, err := site.profileRadio()
			if err != nil {
				t.Fatal(err)
			}
			ctx, cancel := context.WithTimeout(context.Background(), 8*time.Second)
			defer cancel()
			if err = runtime.Controller.Start(ctx, profile, "null"); err != nil {
				t.Fatal(err)
			}
			frameworkWait(t, "native fixture did not produce both units' original facts", func() bool {
				state := runtime.SourceState()
				return len(state.Affiliations) == 2 && len(state.Registrations) == 2 && len(state.Terminals) == 1
			})
			if err = runtime.Controller.Stop(ctx); err != nil {
				t.Fatal(err)
			}
			stopped := runtime.SourceState()
			if stopped.Active || len(stopped.Withdrawals) != 2 {
				t.Fatal("real stopped controller did not retain original exclusion tuples", stopped)
			}
			first, _ := findRFAffiliation(stopped.Affiliations, 10001, 1001)
			second, _ := findRFAffiliation(stopped.Affiliations, 10003, 1002)
			// Zone already accepted A; B's registration/terminal and the stopped
			// withdrawal tuples have not yet been sent by this publisher.
			document := rfAuthorityDocument{ZoneID: "zone-1", NativeRuns: map[string]time.Time{"site-1": stopped.NativeStartedAt}, Units: map[int]rfAuthorityUnit{10001: {Affiliation: rfAuthorityFact{SiteID: "site-1", GroupID: 1001, At: first.At, NativeStartedAt: stopped.NativeStartedAt}}}}
			authority, path := rfStoreTestOpen(t, document)
			credentialPath := filepath.Join(t.TempDir(), "zone-credentials.json")
			token := strings.Repeat("s", 64)
			if err = WritePrivateJSON(credentialPath, ServiceCredentials{Peers: map[string]PeerCredential{"site-1": {Role: RoleSite, InstanceID: "site-1", Token: token}}}); err != nil {
				t.Fatal(err)
			}
			zone := &Service{config: FileConfig{Role: RoleZone, InstanceID: "zone-1", CredentialFile: credentialPath}, rfAuthority: authority}
			zoneConfig := storeFixtureZone()
			setScope := func(next SystemConfig) {
				t.Helper()
				if err := site.Store.WriteCache("system", storeDoc(t, next)); err != nil {
					t.Fatal(err)
				}
				site.refreshDirectory()
				zone.directory.value.Store(&directorySnapshot{System: next, Zone: &zoneConfig})
			}
			reduced := system
			reduced.Radios, reduced.Talkgroups = slices.Clone(system.Radios), slices.Clone(system.Talkgroups)
			if removed == "radio" {
				reduced.Radios = slices.DeleteFunc(reduced.Radios, func(r Radio) bool { return r.RID == 10001 })
			} else {
				reduced.Talkgroups = slices.DeleteFunc(reduced.Talkgroups, func(g Talkgroup) bool { return g.TGID == 1001 })
			}
			setScope(reduced)
			changed, err := site.profileRadio()
			if err != nil {
				t.Fatal(err)
			}
			if err = runtime.ApplyPolicy(ctx, changed); err != nil {
				t.Fatal(err)
			}
			unchanged := runtime.SourceState()
			stopped.ObservedAt, unchanged.ObservedAt = time.Time{}, time.Time{}
			if !reflect.DeepEqual(stopped, unchanged) {
				t.Fatal("regression fixture did not retain the stopped controller's obsolete profile facts")
			}
			type publication struct {
				update rfAuthorityUpdate
				status int
			}
			published := make(chan publication, 32)
			handler := zone.InternalHandler()
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				var body bytes.Buffer
				r.Body = io.NopCloser(io.TeeReader(r.Body, &body))
				response := httptest.NewRecorder()
				handler.ServeHTTP(response, r)
				var update rfAuthorityUpdate
				_ = json.Unmarshal(body.Bytes(), &update)
				for name, values := range response.Header() {
					w.Header()[name] = values
				}
				w.WriteHeader(response.Code)
				_, _ = w.Write(response.Body.Bytes())
				select {
				case published <- publication{update, response.Code}:
				default:
				}
			}))
			defer server.Close()
			site.mu.Lock()
			site.config.UpstreamURL, site.config.CredentialFile = server.URL, filepath.Join(t.TempDir(), "site-credentials.json")
			site.mu.Unlock()
			if err = WritePrivateJSON(site.Config().CredentialFile, ServiceCredentials{OutboundID: "site-1", OutboundToken: token, Peers: map[string]PeerCredential{}}); err != nil {
				t.Fatal(err)
			}
			site.client = http.DefaultClient
			site.operationCtx, site.operationCancel = context.WithCancel(context.Background())
			done := make(chan struct{})
			go func() { defer close(done); site.publishRFSourceState() }()
			t.Cleanup(func() { site.operationCancel(); <-done })
			waitPublication := func(wantA bool) rfAuthorityUpdate {
				t.Helper()
				select {
				case sent := <-published:
					if sent.status != http.StatusOK {
						t.Fatalf("obsolete A blocked B's retained registration/terminal bulk: HTTP%d, update%#v", sent.status, sent.update)
					}
					if present := slices.ContainsFunc(sent.update.Affiliations, func(a radio.Affiliation) bool { return a.RID == 10001 }); present != wantA {
						t.Fatalf("scope change was not reflected in egress: wantA%t update%#v", wantA, sent.update)
					}
					if len(sent.update.Terminals) != 1 || sent.update.Terminals[0] != stopped.Terminals[0] || !slices.Contains(sent.update.Registrations, stopped.Registrations[1]) || !slices.ContainsFunc(sent.update.Withdrawals, func(w rfAuthorityWithdrawal) bool {
						return w.RadioID == second.RID && w.GroupID == second.TGID && w.AffiliatedAt.Equal(second.At) && w.NativeStartedAt.Equal(stopped.NativeStartedAt)
					}) {
						t.Fatal("configured B's original fact/departure clocks were changed or omitted", sent.update)
					}
					return sent.update
				case <-time.After(3 * time.Second):
					t.Fatal("unchanged revision/run hid a remove/re-add identity scope change")
					return rfAuthorityUpdate{}
				}
			}
			waitPublication(false)
			unit := authority.value.Load().document.Units[10003]
			if unit.Registration.At != stopped.Registrations[1].At || unit.Terminal.At != stopped.Terminals[0].At || !unit.Affiliation.Withdrawn || unit.Affiliation.At != second.At {
				t.Fatal("B's registration/terminal did not durably commit independently of obsolete A", unit)
			}
			retainedA := authority.value.Load().document.Units[10001]
			if retainedA.Affiliation != document.Units[10001].Affiliation || (removed == "radio" && retainedA != document.Units[10001]) {
				t.Fatal("filtering removed or changed an already committed durable A fence")
			}
			setScope(system)
			waitPublication(true)
			setScope(reduced)
			waitPublication(false)
			// Same sets in a different order and unrelated display edits must not
			// resend a stable source revision, even though observed_at advances.
			reordered := reduced
			reordered.Radios, reordered.Talkgroups = slices.Clone(reduced.Radios), slices.Clone(reduced.Talkgroups)
			slices.Reverse(reordered.Radios)
			slices.Reverse(reordered.Talkgroups)
			reordered.Radios[0].Alias += " renamed"
			setScope(reordered)
			select {
			case unexpected := <-published:
				t.Fatal("unchanged identity sets defeated successful-send coalescing", unexpected)
			case <-time.After(350 * time.Millisecond):
			}
			site.operationCancel()
			<-done
			authority.close()
			reopened, err := openRFSourceAuthority(path, "zone-1")
			if err != nil {
				t.Fatal(err)
			}
			defer reopened.close()
			for _, rid := range []int{10001, 10003} {
				if owner, ok := reopened.lookup(rid); !ok || owner.SiteID != "site-1" || owner.TGID != 0 {
					t.Fatal("committed original-time exclusion did not survive reopen", rid, owner, ok)
				}
			}
			if reopened.value.Load().document.Units[10003] != unit {
				t.Fatal("B's committed registration/terminal clocks changed across reopen")
			}
		})
	}
}
