package framework

import (
	"net/http"
	"testing"

	"golang.org/x/crypto/bcrypt"
)

func TestZoneRadioCommandAuthoritySurvivesIndependentSitePasswordChange(t *testing.T) {
	site, native, _, _, zoneClient, _, headers := inhibitionCluster(t)
	hash, err := bcrypt.GenerateFromPassword([]byte("separate-site-password"), bcrypt.MinCost)
	if err != nil {
		t.Fatal(err)
	}
	if err := site.Store.SetAccount("local-site-admin", hash); err != nil {
		t.Fatal(err)
	}
	for _, action := range []string{"page", "status_query", "inhibit"} {
		status, body := zoneClient.request(t, "POST", "/api/radio/command", radioCommandInput{ID: "independent-" + action, Action: action, RadioID: 10001, SiteID: "site-1"})
		if status != http.StatusAccepted {
			t.Fatalf("independent local credential broke %s: HTTP%d %s", action, status, body)
		}
	}
	for _, actor := range []OperatorActor{
		{Administrator: true},
		{Administrator: true, Username: "zone-admin", UserID: "person"},
		{Administrator: true, Username: "zone-admin", RadioID: 10001},
		{Administrator: true, Username: "zone-admin", AutomationID: "job"},
		{Administrator: true, Username: "bad\nname"},
	} {
		status, _ := integrationHTTP(t, http.DefaultClient, "POST", site.Config().InternalURL+"/internal/radio/command", radioCommandEnvelope{Input: radioCommandInput{ID: "invalid-actor", Action: "page", RadioID: 10001}, Actor: actor}, headers)
		if status != http.StatusForbidden {
			t.Fatalf("invalid administrator assertion accepted: HTTP%d actor=%+v", status, actor)
		}
	}
	// A browser cannot supply the enrolled Zone's administrator assertion.
	status, _ := integrationHTTP(t, http.DefaultClient, "POST", site.Config().InternalURL+"/internal/radio/command", radioCommandEnvelope{Input: radioCommandInput{ID: "untrusted-admin", Action: "page", RadioID: 10001}, Actor: OperatorActor{Administrator: true, Username: "zone-admin"}}, nil)
	if status != http.StatusUnauthorized {
		t.Fatalf("unauthenticated delegation accepted: HTTP%d", status)
	}
	native.mu.Lock()
	defer native.mu.Unlock()
	if len(native.commands) != 3 {
		t.Fatalf("rejected assertions reached native: %+v", native.commands)
	}
}
