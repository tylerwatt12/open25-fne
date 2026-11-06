package framework

import (
	"bytes"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"
	"unicode"

	"golang.org/x/crypto/bcrypt"
	_ "modernc.org/sqlite"
)

// Store owns one application's configuration. A different role must use a
// different file; upstream data is retained only in named read-only caches.
type Store struct {
	mu   sync.Mutex
	db   *sql.DB
	role Role
}

func OpenStore(path string, role Role) (*Store, error) {
	if !role.Valid() {
		return nil, errors.New("unknown application role")
	}
	if path == "" {
		return nil, errors.New("database path is required")
	}
	if err := os.MkdirAll(filepath.Dir(path), 0700); err != nil {
		return nil, err
	}
	if info, err := os.Lstat(path); err == nil && !info.Mode().IsRegular() {
		return nil, errors.New("database path must be a regular file")
	} else if err != nil && !errors.Is(err, os.ErrNotExist) {
		return nil, err
	}
	f, err := os.OpenFile(path, os.O_CREATE|os.O_RDWR, 0600)
	if err != nil {
		return nil, err
	}
	if err = f.Chmod(0600); err != nil {
		f.Close()
		return nil, err
	}
	if err = f.Close(); err != nil {
		return nil, err
	}
	absolutePath, err := filepath.Abs(path)
	if err != nil {
		return nil, err
	}
	db, err := sql.Open("sqlite", (&url.URL{Scheme: "file", Path: absolutePath}).String())
	if err != nil {
		return nil, err
	}
	db.SetMaxOpenConns(1)
	s := &Store{db: db, role: role}
	if err := s.initialize(); err != nil {
		db.Close()
		return nil, err
	}
	return s, nil
}

func (s *Store) initialize() error {
	rows, err := s.db.Query("SELECT name FROM sqlite_master WHERE type='table' AND name NOT LIKE 'sqlite_%'")
	if err != nil {
		return err
	}
	tables := map[string]bool{}
	for rows.Next() {
		var name string
		if err = rows.Scan(&name); err != nil {
			rows.Close()
			return err
		}
		tables[name] = true
	}
	if err = rows.Err(); err != nil {
		rows.Close()
		return err
	}
	rows.Close()
	if len(tables) == 0 {
		_, err = s.db.Exec(`CREATE TABLE framework_metadata(key TEXT PRIMARY KEY,value TEXT NOT NULL);
CREATE TABLE framework_documents(name TEXT PRIMARY KEY,document BLOB NOT NULL,at TEXT NOT NULL);
CREATE TABLE framework_administrator(singleton INTEGER PRIMARY KEY CHECK(singleton=1),username TEXT NOT NULL,password_hash BLOB NOT NULL);
CREATE TABLE framework_events(id INTEGER PRIMARY KEY AUTOINCREMENT,at TEXT NOT NULL,kind TEXT NOT NULL,message TEXT NOT NULL);`)
		if err != nil {
			return err
		}
		if _, err = s.db.Exec("INSERT INTO framework_metadata(key,value) VALUES('role',?)", string(s.role)); err != nil {
			return err
		}
	} else {
		for _, name := range []string{"framework_metadata", "framework_documents", "framework_administrator", "framework_events"} {
			if !tables[name] {
				return errors.New("not a current framework database; use a separate empty database file")
			}
			delete(tables, name)
		}
		if len(tables) != 0 {
			return errors.New("database contains unrelated tables; use a separate framework database")
		}
	}
	for table, expected := range map[string][]string{
		"framework_metadata":      {"key", "value"},
		"framework_documents":     {"name", "document", "at"},
		"framework_administrator": {"singleton", "username", "password_hash"},
		"framework_events":        {"id", "at", "kind", "message"},
	} {
		columns, err := s.db.Query("PRAGMA table_info(" + table + ")")
		if err != nil {
			return err
		}
		var names []string
		for columns.Next() {
			var cid, required, primary int
			var name, datatype string
			var defaultValue sql.NullString
			if err = columns.Scan(&cid, &name, &datatype, &required, &defaultValue, &primary); err != nil {
				columns.Close()
				return err
			}
			names = append(names, name)
		}
		if err = columns.Err(); err != nil {
			columns.Close()
			return err
		}
		columns.Close()
		if strings.Join(names, ",") != strings.Join(expected, ",") {
			return fmt.Errorf("%s is not in the current framework format", table)
		}
	}
	var saved string
	if err = s.db.QueryRow("SELECT value FROM framework_metadata WHERE key='role'").Scan(&saved); err != nil {
		return err
	}
	if saved != string(s.role) {
		return fmt.Errorf("database belongs to the %s application, not %s", saved, s.role)
	}
	_, err = s.db.Exec("PRAGMA journal_mode=WAL; PRAGMA busy_timeout=5000; PRAGMA foreign_keys=ON;")
	return err
}

func (s *Store) Close() error { return s.db.Close() }

func (s *Store) Config() (json.RawMessage, error) {
	var raw []byte
	err := s.db.QueryRow("SELECT document FROM framework_documents WHERE name='config'").Scan(&raw)
	return json.RawMessage(raw), err
}

func (s *Store) upstreamLocked() (*SystemConfig, *ZoneConfig, error) {
	var system *SystemConfig
	var zone *ZoneConfig
	for _, name := range []string{"system", "zone"} {
		var raw []byte
		err := s.db.QueryRow("SELECT document FROM framework_documents WHERE name=?", "cache:"+name).Scan(&raw)
		if errors.Is(err, sql.ErrNoRows) {
			continue
		}
		if err != nil {
			return nil, nil, err
		}
		if name == "system" {
			var c SystemConfig
			if err = json.Unmarshal(raw, &c); err != nil {
				return nil, nil, err
			}
			system = &c
		} else {
			var c ZoneConfig
			if err = json.Unmarshal(raw, &c); err != nil {
				return nil, nil, err
			}
			zone = &c
		}
	}
	return system, zone, nil
}

func (s *Store) SaveConfig(raw json.RawMessage) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	canonical, err := configurationJSON(raw)
	if err != nil {
		return err
	}
	tx, err := s.db.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback()
	if _, err = tx.Exec("INSERT INTO framework_documents(name,document,at) VALUES('config',?,?) ON CONFLICT(name) DO UPDATE SET document=excluded.document,at=excluded.at", []byte(canonical), nowStamp()); err != nil {
		return err
	}
	if _, err = tx.Exec("INSERT INTO framework_events(at,kind,message) VALUES(?,?,?)", nowStamp(), "configuration", "Configuration saved."); err != nil {
		return err
	}
	return tx.Commit()
}

func (s *Store) ReadCache(name string) (json.RawMessage, time.Time, error) {
	var raw []byte
	var at string
	err := s.db.QueryRow("SELECT document,at FROM framework_documents WHERE name=?", "cache:"+name).Scan(&raw, &at)
	if err != nil {
		return nil, time.Time{}, err
	}
	when, err := time.Parse(time.RFC3339Nano, at)
	return json.RawMessage(raw), when, err
}

// WriteCache retains upstream data without checking configuration compatibility.
func (s *Store) WriteCache(name string, raw json.RawMessage) error {
	if !recordID.MatchString(name) {
		return errors.New("invalid cache name")
	}
	if len(raw) == 0 || len(raw) > maxConfigurationBytes || !json.Valid(raw) {
		return errors.New("cache must contain valid JSON no larger than 64 MiB")
	}
	var out bytes.Buffer
	if err := json.Compact(&out, raw); err != nil {
		return err
	}
	canonical := out.Bytes()
	s.mu.Lock()
	defer s.mu.Unlock()
	_, err := s.db.Exec("INSERT INTO framework_documents(name,document,at) VALUES(?,?,?) ON CONFLICT(name) DO UPDATE SET document=excluded.document,at=excluded.at WHERE framework_documents.document <> excluded.document", "cache:"+name, canonical, nowStamp())
	return err
}

// AcceptCatalog commits one accepted upstream bundle. A rejected administrator
// verifier or document cannot leave a mixture of old and new cached settings.
// A nil Zone document leaves the previously accepted Zone document untouched.
func (s *Store) AcceptCatalog(systemRaw, zoneRaw json.RawMessage, username string, hash []byte, users []userRecord, automations []automationRecord, inhibition *radioInhibitionPolicy) error {
	if s.role == RoleSite && inhibition == nil {
		return errors.New("Site catalogue requires authoritative Zone inhibit policy")
	}
	if err := validAdministrator(username, hash); err != nil {
		return err
	}
	canonicalSystem, err := configurationJSON(systemRaw)
	if err != nil {
		return err
	}
	var system SystemConfig
	if err = json.Unmarshal(canonicalSystem, &system); err != nil {
		return err
	}
	if users == nil {
		users = []userRecord{}
	}
	for _, r := range users {
		if _, err = bcrypt.Cost(r.PasswordHash); err != nil {
			return err
		}
	}
	usersRaw, err := json.Marshal(users)
	if err != nil {
		return err
	}
	if automations == nil {
		automations = []automationRecord{}
	}
	automationsRaw, err := json.Marshal(automations)
	if err != nil {
		return err
	}
	var canonicalZone json.RawMessage
	if len(zoneRaw) > 0 {
		canonicalZone, err = configurationJSON(zoneRaw)
		if err != nil {
			return err
		}
		var zone ZoneConfig
		if err = json.Unmarshal(canonicalZone, &zone); err != nil {
			return err
		}
		if zone.SystemID != system.ID {
			return errors.New("upstream Zone belongs to a different home system")
		}
	}
	var inhibitionRaw json.RawMessage
	if inhibition != nil {
		if s.role != RoleSite {
			return errors.New("only an enrolled Site accepts upstream inhibit policy")
		}
		value, policyErr := normalizedRadioInhibition(*inhibition)
		if policyErr != nil {
			return policyErr
		}
		inhibitionRaw, err = json.Marshal(value)
		if err != nil {
			return err
		}
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	oldSystem, oldZone, err := s.upstreamLocked()
	if err != nil {
		return err
	}
	if oldSystem != nil && oldSystem.ID != system.ID {
		return errors.New("upstream belongs to a different home system")
	}
	if len(canonicalZone) == 0 && oldZone != nil && oldZone.SystemID != system.ID {
		return errors.New("retained Zone belongs to a different home system")
	}
	tx, err := s.db.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback()
	at := nowStamp()
	for name, raw := range map[string]json.RawMessage{"system": canonicalSystem, "zone": canonicalZone, "radio-inhibition": inhibitionRaw} {
		if len(raw) == 0 {
			continue
		}
		if _, err = tx.Exec("INSERT INTO framework_documents(name,document,at) VALUES(?,?,?) ON CONFLICT(name) DO UPDATE SET document=excluded.document,at=excluded.at WHERE framework_documents.document <> excluded.document", "cache:"+name, []byte(raw), at); err != nil {
			return err
		}
	}
	if _, err = tx.Exec("INSERT INTO framework_administrator(singleton,username,password_hash) VALUES(1,?,?) ON CONFLICT(singleton) DO UPDATE SET username=excluded.username,password_hash=excluded.password_hash WHERE framework_administrator.username <> excluded.username OR framework_administrator.password_hash <> excluded.password_hash", username, hash); err != nil {
		return err
	}
	if _, err = tx.Exec("INSERT INTO framework_documents(name,document,at) VALUES('cache:users',?,?) ON CONFLICT(name) DO UPDATE SET document=excluded.document,at=excluded.at WHERE framework_documents.document <> excluded.document", []byte(usersRaw), at); err != nil {
		return err
	}
	if _, err = tx.Exec("INSERT INTO framework_documents(name,document,at) VALUES('cache:automations',?,?) ON CONFLICT(name) DO UPDATE SET document=excluded.document,at=excluded.at WHERE framework_documents.document <> excluded.document", automationsRaw, at); err != nil {
		return err
	}
	return tx.Commit()
}

func (s *Store) Account() (string, []byte, error) {
	var username string
	var hash []byte
	err := s.db.QueryRow("SELECT username,password_hash FROM framework_administrator WHERE singleton=1").Scan(&username, &hash)
	return username, hash, err
}

func validAdministrator(username string, hash []byte) error {
	if username != strings.TrimSpace(username) || len(username) < 1 || len(username) > 64 || strings.ContainsRune(username, ' ') || strings.IndexFunc(username, unicode.IsControl) >= 0 {
		return errors.New("administrator username must be 1-64 characters without spaces")
	}
	if _, err := bcrypt.Cost(hash); err != nil {
		return errors.New("administrator password must be a bcrypt verifier")
	}
	return nil
}

func (s *Store) SetAccount(username string, hash []byte) error {
	if err := validAdministrator(username, hash); err != nil {
		return err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	_, err := s.db.Exec("INSERT INTO framework_administrator(singleton,username,password_hash) VALUES(1,?,?) ON CONFLICT(singleton) DO UPDATE SET username=excluded.username,password_hash=excluded.password_hash", username, hash)
	return err
}

func (s *Store) AddEvent(kind, message string) error {
	kind = strings.TrimSpace(kind)
	message = strings.TrimSpace(message)
	if kind == "" || len(kind) > 80 || message == "" || len(message) > 2000 {
		return errors.New("event kind or message is invalid")
	}
	_, err := s.db.Exec("INSERT INTO framework_events(at,kind,message) VALUES(?,?,?)", nowStamp(), kind, message)
	return err
}

func (s *Store) Events(limit int) ([]Event, error) {
	if limit < 1 || limit > 1000 {
		limit = 100
	}
	rows, err := s.db.Query("SELECT id,at,kind,message FROM framework_events ORDER BY id DESC LIMIT ?", limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	events := []Event{}
	for rows.Next() {
		var e Event
		var at string
		if err = rows.Scan(&e.ID, &at, &e.Kind, &e.Message); err != nil {
			return nil, err
		}
		e.At, err = time.Parse(time.RFC3339Nano, at)
		if err != nil {
			return nil, err
		}
		events = append(events, e)
	}
	return events, rows.Err()
}

func nowStamp() string { return time.Now().UTC().Format(time.RFC3339Nano) }
