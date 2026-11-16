package framework

import (
	"context"
	"crypto/sha256"
	"fmt"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"golang.org/x/crypto/bcrypt"
)

func memorySSOService(t *testing.T) (*Service, ssoGrant, ssoExchange) {
	t.Helper()
	store := storeOpen(t, RoleSystem)
	hash, err := bcrypt.GenerateFromPassword([]byte("fixture-password"), bcrypt.MinCost)
	if err != nil {
		t.Fatal(err)
	}
	if err := store.SetAccount("administrator", hash); err != nil {
		t.Fatal(err)
	}
	s := &Service{Store: store, config: FileConfig{Role: RoleSystem, StateDir: t.TempDir()}}
	input := ssoExchange{Code: strings.Repeat("c", 64), Verifier: strings.Repeat("v", 64), Callback: "https://site.example.test/sso/callback"}
	grant := ssoGrant{FamilyID: strings.Repeat("f", 64), Callback: input.Callback, Challenge: ssoChallenge(input.Verifier), Username: "administrator", AccountDigest: sha256.Sum256(hash), SessionExpires: time.Now().Add(time.Hour), Expires: time.Now().Add(time.Minute)}
	return s, grant, input
}

func TestSSOTemporaryStateDoesNotWaitForDurableWriter(t *testing.T) {
	s, grant, input := memorySSOService(t)
	var before, after int
	if err := s.Store.readDB.QueryRow("PRAGMA data_version").Scan(&before); err != nil {
		t.Fatal(err)
	}
	connection, err := s.Store.db.Conn(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	defer connection.Close()
	s.Store.mu.Lock()
	defer s.Store.mu.Unlock()
	result := make(chan error, 1)
	go func() {
		state := strings.Repeat("s", 64)
		pending := ssoPending{Verifier: input.Verifier, Callback: input.Callback, Authority: "https://system.example.test", Return: "/channels", Expires: time.Now().Add(5 * time.Minute)}
		if err := s.rememberSSOPending(state, pending); err != nil {
			result <- err
			return
		}
		if _, ok := s.consumeSSOPending(state, pending.Authority, pending.Callback); !ok {
			result <- fmt.Errorf("pending request could not be consumed")
			return
		}
		if err := s.rememberSSOCode(input.Code, grant); err != nil {
			result <- err
			return
		}
		identity, err := s.redeemSSO(input)
		if err == nil && (identity.FamilyID != grant.FamilyID || !identity.Expires.Equal(grant.SessionExpires)) {
			err = fmt.Errorf("redemption changed identity/absolute session expiry")
		}
		result <- err
	}()
	select {
	case err := <-result:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(250 * time.Millisecond):
		t.Fatal("ephemeral SSO waited for configuration writer or disk commit")
	}
	if err := s.Store.readDB.QueryRow("PRAGMA data_version").Scan(&after); err != nil || before != after {
		t.Fatal("temporary sign-in state changed durable storage", before, after, err)
	}
	var count int
	if err := s.Store.readDB.QueryRow("SELECT COUNT(*) FROM framework_documents WHERE name LIKE 'auth:pending:%' OR name LIKE 'auth:code:%'").Scan(&count); err != nil || count != 0 {
		t.Fatal("temporary sign-in state was persisted", count, err)
	}
}

func TestSSORestartAbandonsUnfinishedStateButPreservesFinalSession(t *testing.T) {
	s, grant, input := memorySSOService(t)
	state := strings.Repeat("s", 64)
	pending := ssoPending{Verifier: input.Verifier, Callback: input.Callback, Authority: "https://system.example.test", Expires: time.Now().Add(5 * time.Minute)}
	if err := s.rememberSSOPending(state, pending); err != nil {
		t.Fatal(err)
	}
	if err := s.rememberSSOCode(input.Code, grant); err != nil {
		t.Fatal(err)
	}
	request := httptest.NewRequest("GET", "https://system.example.test/", nil)
	response := httptest.NewRecorder()
	final := Session{FamilyID: grant.FamilyID, Username: grant.Username, AccountDigest: grant.AccountDigest, Expires: grant.SessionExpires}
	if err := s.createSession(response, request, &final); err != nil {
		t.Fatal(err)
	}
	var path string
	if err := s.Store.db.QueryRow("SELECT file FROM pragma_database_list WHERE name='main'").Scan(&path); err != nil {
		t.Fatal(err)
	}
	if err := s.Store.Close(); err != nil {
		t.Fatal(err)
	}
	reopened, err := OpenStore(filepath.Clean(path), RoleSystem)
	if err != nil {
		t.Fatal(err)
	}
	defer reopened.Close()
	restarted := &Service{Store: reopened, config: s.config}
	if _, ok := restarted.consumeSSOPending(state, pending.Authority, pending.Callback); ok {
		t.Fatal("restart resurrected an unfinished browser request")
	}
	if _, err := restarted.redeemSSO(input); err == nil {
		t.Fatal("restart replayed an unfinished authorization code")
	}
	request.AddCookie(&http.Cookie{Name: restarted.cookieName(), Value: final.Key})
	if session, ok := restarted.session(request); !ok || !session.Expires.Equal(grant.SessionExpires) {
		t.Fatal("restart lost or extended the durable final session")
	}
}

func TestSSOPendingBrowserIssuerAudienceAndAtomicSingleUse(t *testing.T) {
	s := &Service{config: FileConfig{Role: RoleSite, PublicURL: "https://site.example.test", StateDir: t.TempDir()}}
	state := strings.Repeat("s", 64)
	pending := ssoPending{Verifier: strings.Repeat("v", 64), Callback: "https://site.example.test/sso/callback", Authority: "https://system.example.test", Expires: time.Now().Add(5 * time.Minute)}
	for _, test := range []struct{ name, cookie, issuer string }{{"missing browser cookie", "", pending.Authority}, {"other browser", strings.Repeat("x", 64), pending.Authority}, {"wrong issuer", state, "https://other.example.test"}} {
		t.Run(test.name, func(t *testing.T) {
			if err := s.rememberSSOPending(state, pending); err != nil {
				t.Fatal(err)
			}
			request := httptest.NewRequest("GET", pending.Callback+"?state="+state+"&iss="+test.issuer, nil)
			if test.cookie != "" {
				request.AddCookie(&http.Cookie{Name: s.cookieName() + "_sso", Value: test.cookie})
			}
			response := httptest.NewRecorder()
			s.ssoWeb(response, request)
			if response.Code != http.StatusForbidden {
				t.Fatal("unbound callback was accepted", response.Code)
			}
			if _, ok := s.consumeSSOPending(state, pending.Authority, pending.Callback); !ok {
				t.Fatal("unproven callback consumed the valid browser request")
			}
		})
	}
	if err := s.rememberSSOPending(state, pending); err != nil {
		t.Fatal(err)
	}
	if _, ok := s.consumeSSOPending(state, pending.Authority, "https://other.example.test/sso/callback"); ok {
		t.Fatal("pending request accepted another callback audience")
	}
	results := make(chan bool, 2)
	for range 2 {
		go func() { _, ok := s.consumeSSOPending(state, pending.Authority, pending.Callback); results <- ok }()
	}
	accepted := 0
	for range 2 {
		if <-results {
			accepted++
		}
	}
	if accepted != 1 {
		t.Fatal("browser state was not single use", accepted)
	}
}

func TestSSOTemporaryCapacityAndExpirationAreAtomic(t *testing.T) {
	for _, kind := range []string{"pending", "code"} {
		t.Run(kind, func(t *testing.T) {
			s := &Service{ssoPending: map[[32]byte]ssoPending{}, ssoCodes: map[[32]byte]ssoGrant{}}
			future := time.Now().Add(time.Minute)
			// Prepare the bound directly; this tests concurrent admission rather
			// than repeatedly exercising expiration scans for fixture creation.
			for i := range ssoLimit - 1 {
				key := sha256.Sum256([]byte(fmt.Sprintf("%064x", i)))
				if kind == "pending" {
					s.ssoPending[key] = ssoPending{Expires: future}
				} else {
					s.ssoCodes[key] = ssoGrant{Expires: future}
				}
			}
			remember := func(token string) error {
				if kind == "pending" {
					return s.rememberSSOPending(token, ssoPending{Expires: future})
				}
				return s.rememberSSOCode(token, ssoGrant{Expires: future})
			}
			results := make(chan error, 2)
			for i := range 2 {
				go func() { results <- remember(fmt.Sprintf("%064x", ssoLimit+i)) }()
			}
			accepted := 0
			for range 2 {
				if <-results == nil {
					accepted++
				}
			}
			if accepted != 1 {
				t.Fatal("ephemeral capacity was exceeded concurrently", accepted)
			}
			if err := remember(fmt.Sprintf("%064x", 0)); err != nil {
				t.Fatal("replacement at capacity failed", err)
			}
			key := sha256.Sum256([]byte(fmt.Sprintf("%064x", 0)))
			s.ssoMu.Lock()
			if kind == "pending" {
				value := s.ssoPending[key]
				value.Expires = time.Now().Add(-time.Second)
				s.ssoPending[key] = value
			} else {
				value := s.ssoCodes[key]
				value.Expires = time.Now().Add(-time.Second)
				s.ssoCodes[key] = value
			}
			s.ssoMu.Unlock()
			if err := remember(strings.Repeat("n", 64)); err != nil {
				t.Fatal("expired state did not free capacity", err)
			}
			s.ssoMu.Lock()
			_, pending := s.ssoPending[key]
			_, code := s.ssoCodes[key]
			s.ssoMu.Unlock()
			if pending || code {
				t.Fatal("expired state survived create-time expiration")
			}
		})
	}
}

func TestSSORedemptionRejectsCurrentAccountRevocationAndReadFailures(t *testing.T) {
	for _, failure := range []string{"account", "revocation", "malformed revocation", "SQL revocation read"} {
		t.Run(failure, func(t *testing.T) {
			s, grant, input := memorySSOService(t)
			switch failure {
			case "account":
				_, hash, _ := s.Store.Account()
				if err := s.Store.SetAccount("changed", hash); err != nil {
					t.Fatal(err)
				}
			case "revocation":
				if err := s.recordRevocation(sessionRevocation{FamilyID: grant.FamilyID, Expires: grant.SessionExpires}); err != nil {
					t.Fatal(err)
				}
			case "malformed revocation":
				if _, err := s.Store.db.Exec("INSERT INTO framework_documents(name,document,at) VALUES(?,?,?)", authName("revoked", grant.FamilyID), []byte("invalid json"), grant.SessionExpires.UTC().Format(authTimeFormat)); err != nil {
					t.Fatal(err)
				}
			case "SQL revocation read":
				if _, err := s.Store.db.Exec("DROP TABLE framework_documents"); err != nil {
					t.Fatal(err)
				}
				if _, _, err := s.Store.Account(); err != nil {
					t.Fatal("fixture account must remain readable", err)
				}
			}
			if err := s.rememberSSOCode(input.Code, grant); err != nil {
				t.Fatal(err)
			}
			if _, err := s.redeemSSO(input); err == nil {
				t.Fatal("current durable authority failure granted an identity")
			}
		})
	}
}
