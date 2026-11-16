package framework

import (
	"database/sql"
	"errors"
	"fmt"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestAuthPutExpirationAndInsertCommitTogether(t *testing.T) {
	path := filepath.Join(t.TempDir(), "state.db")
	s, err := OpenStore(path, RoleSystem)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if s != nil {
			s.Close()
		}
	})
	if err = s.authPut("session", "expired-request", map[string]bool{"expired": true}, time.Now().Add(-time.Hour)); err != nil {
		t.Fatal(err)
	}
	if _, err = s.db.Exec(`CREATE TRIGGER reject_session BEFORE INSERT ON framework_documents
WHEN NEW.name LIKE 'auth:session:%' BEGIN SELECT RAISE(ABORT,'forced auth insert failure'); END`); err != nil {
		t.Fatal(err)
	}
	expires := time.Now().UTC().Add(time.Hour)
	value := Session{Username: "fixture", FamilyID: strings.Repeat("a", 64), Expires: expires}
	if err = s.authPut("session", "new-session", value, expires); err == nil {
		t.Fatal("rejected authentication insert reported success")
	}
	var count int
	if err = s.db.QueryRow("SELECT count(*) FROM framework_documents WHERE name=?", authName("session", "expired-request")).Scan(&count); err != nil || count != 1 {
		t.Fatalf("failed authentication write committed expiration: count=%d err=%v", count, err)
	}
	var stored Session
	if err = s.authRead("session", "new-session", &stored); !errors.Is(err, sql.ErrNoRows) {
		t.Fatalf("failed authentication write left a session: %v", err)
	}
	if _, err = s.db.Exec("DROP TRIGGER reject_session"); err != nil {
		t.Fatal(err)
	}
	if err = s.authPut("session", "new-session", value, expires); err != nil {
		t.Fatal(err)
	}
	if err = s.db.QueryRow("SELECT count(*) FROM framework_documents WHERE name=?", authName("session", "expired-request")).Scan(&count); err != nil || count != 0 {
		t.Fatalf("accepted authentication write retained expired row: count=%d err=%v", count, err)
	}
	if err = s.Close(); err != nil {
		t.Fatal(err)
	}
	s, err = OpenStore(path, RoleSystem)
	if err != nil {
		t.Fatal(err)
	}
	if err = s.authRead("session", "new-session", &stored); err != nil {
		t.Fatal(err)
	}
	if stored.Username != value.Username || stored.FamilyID != value.FamilyID || !stored.Expires.Equal(value.Expires) {
		t.Fatal("committed session changed across store restart")
	}
}

func TestAuthPutConcurrentLimitAndExistingReplacement(t *testing.T) {
	s := storeOpen(t, RoleSystem)
	expires := time.Now().UTC().Add(time.Hour)
	tx, err := s.db.Begin()
	if err != nil {
		t.Fatal(err)
	}
	defer tx.Rollback()
	for i := range 4095 {
		if _, err = tx.Exec("INSERT INTO framework_documents(name,document,at) VALUES(?,?,?)", authName("session", fmt.Sprintf("cap-%04d", i)), []byte(`{"before":true}`), expires.Format(authTimeFormat)); err != nil {
			t.Fatal(err)
		}
	}
	if err = tx.Commit(); err != nil {
		t.Fatal(err)
	}
	results := make(chan error, 2)
	for i := range 2 {
		go func() {
			results <- s.authPut("session", fmt.Sprintf("concurrent-%d", i), map[string]bool{"accepted": true}, expires)
		}()
	}
	accepted := 0
	for range 2 {
		if err := <-results; err == nil {
			accepted++
		} else if !strings.Contains(err.Error(), "too many active authentication records") {
			t.Fatal(err)
		}
	}
	if accepted != 1 {
		t.Fatalf("authentication record cap accepted %d concurrent writes", accepted)
	}
	if err = s.authPut("session", "cap-0000", map[string]bool{"replacement": true}, expires); err != nil {
		t.Fatalf("replacing an existing authentication record at capacity failed: %v", err)
	}
	var value map[string]bool
	if err = s.authRead("session", "cap-0000", &value); err != nil || !value["replacement"] {
		t.Fatalf("replacement did not commit: %v", err)
	}
	var count int
	if err = s.db.QueryRow("SELECT count(*) FROM framework_documents WHERE name LIKE 'auth:session:%'").Scan(&count); err != nil || count != 4096 {
		t.Fatalf("authentication record count=%d err=%v", count, err)
	}
}

func TestCreateSessionFailedDurableWriteSetsNoCookie(t *testing.T) {
	s := storeOpen(t, RoleSystem)
	if _, err := s.db.Exec(`CREATE TRIGGER reject_session BEFORE INSERT ON framework_documents
WHEN NEW.name LIKE 'auth:session:%' BEGIN SELECT RAISE(ABORT,'forced auth insert failure'); END`); err != nil {
		t.Fatal(err)
	}
	service := &Service{Store: s, config: FileConfig{Role: RoleSystem, StateDir: t.TempDir()}}
	v := Session{Username: "fixture", Expires: time.Now().Add(time.Hour)}
	w := httptest.NewRecorder()
	if err := service.createSession(w, httptest.NewRequest("GET", "https://system.example.test/", nil), &v); err == nil {
		t.Fatal("failed durable session write reported success")
	}
	if len(w.Header().Values("Set-Cookie")) != 0 || v.Key != "" {
		t.Fatal("failed durable session write issued a cookie or bearer")
	}
}
