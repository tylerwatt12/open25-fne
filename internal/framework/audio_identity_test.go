package framework

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"open25.local/open25-fne/internal/dispatch"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestAudioIdentityExclusiveAcrossServersAndExpires(t *testing.T) {
	s := &Service{}
	now := time.Now()
	if e := s.updateAudioIdentity("dispatch-a", "one", "alice", 10002, "claim", now); e != nil {
		t.Fatal(e)
	}
	if e := s.updateAudioIdentity("dispatch-b", "two", "bob", 10002, "claim", now); e == nil {
		t.Fatal("two dispatch servers owned one RID")
	}
	if e := s.updateAudioIdentity("dispatch-b", "two", "bob", 10002, "release", now); e != nil {
		t.Fatal(e)
	}
	if e := s.updateAudioIdentity("dispatch-a", "one", "alice", 10002, "renew", now.Add(time.Second)); e != nil {
		t.Fatal(e)
	}
	if e := s.updateAudioIdentity("dispatch-b", "two", "bob", 10002, "claim", now.Add(audioIdentityLeaseDuration)); e == nil {
		t.Fatal("renewed lease was lost")
	}
	if e := s.updateAudioIdentity("dispatch-b", "two", "bob", 10002, "claim", now.Add(audioIdentityLeaseDuration+2*time.Second)); e != nil {
		t.Fatal(e)
	}
	if e := s.updateAudioIdentity("dispatch-a", "one", "alice", 10002, "renew", now.Add(audioIdentityLeaseDuration+2*time.Second)); e == nil {
		t.Fatal("old session renewed new owner")
	}
}

func identityRouter(t *testing.T, cluster *mediaCluster, id, address string) *networkRouter {
	t.Helper()
	token := strings.Repeat("identity-token-"+id, 5)
	for _, node := range []*integrationService{cluster.zone, cluster.site} {
		credentials, err := readCredentials(node.service.Config().CredentialFile)
		if err != nil {
			t.Fatal(err)
		}
		credentials.Peers[id] = PeerCredential{Role: RoleApplication, InstanceID: id, Token: token}
		if err = WritePrivateJSON(node.service.Config().CredentialFile, credentials); err != nil {
			t.Fatal(err)
		}
	}
	credentials := filepath.Join(t.TempDir(), "credentials.json")
	if err := WritePrivateJSON(credentials, ServiceCredentials{OutboundID: id, OutboundToken: token}); err != nil {
		t.Fatal(err)
	}
	service := &Service{Store: cluster.dispatch.service.Store, config: FileConfig{Role: RoleApplication, InstanceID: id, CredentialFile: credentials}, client: cluster.dispatch.service.client}
	service.directory.value.Store(cluster.dispatch.service.view())
	service.mediaClient = &mediaClient{s: service, activeURL: address, generation: 1}
	return &networkRouter{service: service}
}

func identityPrincipal() dispatch.Principal {
	return dispatch.Principal{UserID: "user-1", RadioID: 10002, ReceiveGroups: []int{1001, 1002}, TransmitGroups: []int{1001, 1002}, CanTransmit: true}
}

func TestAudioIdentityTwoDispatchServersCannotShareOneDeskAndRevocationReleasesIt(t *testing.T) {
	cluster := newMediaCluster(t)
	one := identityRouter(t, cluster, "identity-one", cluster.zone.server.URL)
	two := identityRouter(t, cluster, "identity-two", cluster.zone.server.URL)
	p := identityPrincipal()
	if err := one.ClaimAudio(context.Background(), p, "session-one"); err != nil {
		t.Fatal(err)
	}
	if err := two.ClaimAudio(context.Background(), p, "session-two"); err == nil {
		t.Fatal("two Application Servers acquired the same desk")
	}
	if err := one.ReleaseAudio(context.Background(), p, "session-one"); err != nil {
		t.Fatal(err)
	}
	if err := two.ClaimAudio(context.Background(), p, "session-two"); err != nil {
		t.Fatal(err)
	}
	previous := cluster.zone.service.view()
	changed := *previous
	changed.Users = map[string]userRecord{}
	for id, record := range previous.Users {
		changed.Users[id] = record
	}
	record := changed.Users[p.UserID]
	record.User.Enabled = false
	changed.Users[p.UserID] = record
	cluster.zone.service.directory.value.Store(&changed)
	if err := two.RenewAudio(context.Background(), p, "session-two"); !errors.Is(err, dispatch.ErrPermission) {
		t.Fatal("revoked identity renewed", err)
	}
	// Cleanup remains narrowly bound to the original owner/session even after
	// an account is disabled; it does not need the former actor proof to pass.
	if err := two.ReleaseAudio(context.Background(), p, "session-two"); err != nil {
		t.Fatal(err)
	}
	cluster.zone.service.directory.value.Store(previous)
	if err := one.ClaimAudio(context.Background(), p, "replacement"); err != nil {
		t.Fatal("revoked owner retained its lease", err)
	}
	_ = one.ReleaseAudio(context.Background(), p, "replacement")
}

func TestAudioIdentitySiteConnectedConsoleUsesCanonicalZoneOwnerAcrossHandoff(t *testing.T) {
	cluster := newMediaCluster(t)
	local := identityRouter(t, cluster, "identity-local", cluster.site.server.URL)
	other := identityRouter(t, cluster, "identity-other", cluster.zone.server.URL)
	p := identityPrincipal()
	if err := local.ClaimAudio(context.Background(), p, "moving-browser"); err != nil {
		t.Fatal("normal Site-connected Application Server was rejected", err)
	}
	cluster.zone.service.audioLeaseMu.Lock()
	lease := cluster.zone.service.audioLeases[p.RadioID]
	cluster.zone.service.audioLeaseMu.Unlock()
	if lease.Owner != "identity-local" || lease.Session != "moving-browser" {
		t.Fatalf("Site proxy replaced the original owner: %#v", lease)
	}
	if err := other.ClaimAudio(context.Background(), p, "competing"); err == nil {
		t.Fatal("Zone ignored the Site-connected desk reservation")
	}
	local.service.mediaClient.mu.Lock()
	local.service.mediaClient.activeURL = cluster.zone.server.URL
	local.service.mediaClient.generation++
	local.service.mediaClient.mu.Unlock()
	if err := local.RenewAudio(context.Background(), p, "moving-browser"); err != nil {
		t.Fatal("Site-to-Zone handoff lost the same browser identity", err)
	}
	if err := local.ReleaseAudio(context.Background(), p, "moving-browser"); err != nil {
		t.Fatal(err)
	}
	if err := other.ClaimAudio(context.Background(), p, "replacement"); err != nil {
		t.Fatal("handoff cleanup left the desk reserved", err)
	}
	_ = other.ReleaseAudio(context.Background(), p, "replacement")
}

func TestAudioIdentityIsolatedSiteReconcilesWithoutInventingGlobalOwnership(t *testing.T) {
	cluster := newMediaCluster(t)
	cluster.site.service.mediaClient.Close()
	local := identityRouter(t, cluster, "isolated-dispatch", cluster.site.server.URL)
	other := identityRouter(t, cluster, "wide-dispatch", cluster.zone.server.URL)
	p := identityPrincipal()
	if err := local.ClaimAudio(context.Background(), p, "local-browser"); err != nil {
		t.Fatal("isolated Site did not admit local operation", err)
	}
	cluster.zone.service.audioLeaseMu.Lock()
	_, global := cluster.zone.service.audioLeases[p.RadioID]
	cluster.zone.service.audioLeaseMu.Unlock()
	if global {
		t.Fatal("an isolated Site claimed to hold a Zone-wide reservation")
	}
	if err := other.ClaimAudio(context.Background(), p, "wide-browser"); err != nil {
		t.Fatal(err)
	}
	cluster.site.service.mediaClient = &mediaClient{s: cluster.site.service, activeURL: cluster.zone.server.URL, generation: 1}
	if err := local.RenewAudio(context.Background(), p, "local-browser"); err == nil || errors.Is(err, dispatch.ErrUnavailable) {
		t.Fatal("reconnecting Site did not respect the Zone owner", err)
	}
	cluster.site.service.audioLeaseMu.Lock()
	_, held := cluster.site.service.audioLeases[p.RadioID]
	cluster.site.service.audioLeaseMu.Unlock()
	if held {
		t.Fatal("rejected Site session retained local authority")
	}
	_ = local.ReleaseAudio(context.Background(), p, "local-browser")
	if err := other.RenewAudio(context.Background(), p, "wide-browser"); err != nil {
		t.Fatal("stale Site cleanup released a different owner", err)
	}
	_ = other.ReleaseAudio(context.Background(), p, "wide-browser")
}

func TestAudioIdentityNewCallCannotUseAnUnregisteredAudioSession(t *testing.T) {
	cluster := newMediaCluster(t)
	router := identityRouter(t, cluster, "identity-one", cluster.zone.server.URL)
	if err := router.ensureAudioIdentity(context.Background(), identityPrincipal(), "invented-session"); !errors.Is(err, dispatch.ErrPermission) {
		t.Fatal("PTT admitted an unregistered browser identity", err)
	}
}

func TestAudioIdentityAmbiguousClaimCanBeRetiredAfterAResponseFailure(t *testing.T) {
	cluster := newMediaCluster(t)
	authority := &Service{}
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var input audioIdentityInput
		if err := json.NewDecoder(r.Body).Decode(&input); err != nil {
			t.Error(err)
			w.WriteHeader(400)
			return
		}
		action := strings.TrimPrefix(r.URL.Path, "/internal/audio-identity/")
		if err := authority.updateAudioIdentity("ambiguous-dispatch", input.Session, input.Actor.UserID, input.Actor.RadioID, action, time.Now()); err != nil {
			t.Error(err)
			w.WriteHeader(409)
			return
		}
		// The remote accepted the reservation, but its response failed. A client
		// must not infer that there is therefore no reservation to clean up.
		if action == "claim" {
			w.WriteHeader(503)
			return
		}
		w.WriteHeader(200)
	}))
	defer server.Close()
	router := identityRouter(t, cluster, "ambiguous-dispatch", server.URL)
	if err := router.ClaimAudio(context.Background(), identityPrincipal(), "ambiguous-session"); !errors.Is(err, dispatch.ErrUnavailable) {
		t.Fatal("response failure was not a temporary routing error", err)
	}
	if err := router.ReleaseAudio(context.Background(), identityPrincipal(), "ambiguous-session"); err != nil {
		t.Fatal(err)
	}
	authority.audioLeaseMu.Lock()
	_, held := authority.audioLeases[identityPrincipal().RadioID]
	authority.audioLeaseMu.Unlock()
	if held {
		t.Fatal("failed audio admission stranded a remote desk reservation")
	}
}
