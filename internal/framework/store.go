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
	mu     sync.Mutex
	db     *sql.DB
	readDB *sql.DB
	role   Role

	diagnosticMu     sync.RWMutex
	diagnostics      []Event
	diagnosticNextID int64

	// Current observations have no authority after a process restart. Keep them
	// separate from durable programming/authentication and never revive old SQL
	// copies. Only the four fixed keys below can occupy this map.
	observationMu sync.RWMutex
	observations  map[string]cacheObservation
}

type cacheObservation struct {
	raw json.RawMessage
	at  time.Time
}

func transientCache(name string) bool {
	switch name {
	case "connection", "site_reports", "zone_report", "hardware_reports":
		return true
	}
	return false
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
	readDSN := url.URL{Scheme: "file", Path: absolutePath}
	readDSN.RawQuery = url.Values{"mode": {"ro"}, "_pragma": {"busy_timeout(5000)"}}.Encode()
	s.readDB, err = sql.Open("sqlite", readDSN.String())
	if err != nil {
		db.Close()
		return nil, err
	}
	s.readDB.SetMaxOpenConns(1)
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
CREATE TABLE framework_administrator(singleton INTEGER PRIMARY KEY CHECK(singleton=1),username TEXT NOT NULL,password_hash BLOB NOT NULL);`)
		if err != nil {
			return err
		}
		if _, err = s.db.Exec("INSERT INTO framework_metadata(key,value) VALUES('role',?)", string(s.role)); err != nil {
			return err
		}
	} else {
		for _, name := range []string{"framework_metadata", "framework_documents", "framework_administrator"} {
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

func (s *Store) Close() error { return errors.Join(s.readDB.Close(), s.db.Close()) }

func (s *Store) Config() (json.RawMessage, error) {
	var raw []byte
	err := s.readDB.QueryRow("SELECT document FROM framework_documents WHERE name='config'").Scan(&raw)
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
	if _, err = s.db.Exec("INSERT INTO framework_documents(name,document,at) VALUES('config',?,?) ON CONFLICT(name) DO UPDATE SET document=excluded.document,at=excluded.at", []byte(canonical), nowStamp()); err != nil {
		return err
	}
	return s.AddEvent("configuration", "Configuration saved.")
}

func (s *Store) ReadCache(name string) (json.RawMessage, time.Time, error) {
	if transientCache(name) {
		s.observationMu.RLock()
		observation, ok := s.observations[name]
		s.observationMu.RUnlock()
		if !ok {
			return nil, time.Time{}, sql.ErrNoRows
		}
		return bytes.Clone(observation.raw), observation.at, nil
	}
	var raw []byte
	var at string
	err := s.readDB.QueryRow("SELECT document,at FROM framework_documents WHERE name=?", "cache:"+name).Scan(&raw, &at)
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
	if transientCache(name) {
		s.observationMu.Lock()
		if s.observations == nil {
			s.observations = make(map[string]cacheObservation, 4)
		}
		s.observations[name] = cacheObservation{raw: canonical, at: time.Now().UTC()}
		s.observationMu.Unlock()
		return nil
	}
	// A repeated accepted cache can linearize at this committed read, without
	// queuing behind a slow unrelated durable write. Changed data is rechecked
	// under the writer's ownership below before it is committed.
	var previous []byte
	if err := s.readDB.QueryRow("SELECT document FROM framework_documents WHERE name=?", "cache:"+name).Scan(&previous); err == nil && bytes.Equal(previous, canonical) {
		return nil
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	// Synchronization may repeatedly receive identical programming. Avoid even
	// entering a write statement for it; explicit administrator Save is separate.
	err := s.db.QueryRow("SELECT document FROM framework_documents WHERE name=?", "cache:"+name).Scan(&previous)
	if err == nil && bytes.Equal(previous, canonical) {
		return nil
	}
	if err != nil && !errors.Is(err, sql.ErrNoRows) {
		return err
	}
	_, err = s.db.Exec("INSERT INTO framework_documents(name,document,at) VALUES(?,?,?) ON CONFLICT(name) DO UPDATE SET document=excluded.document,at=excluded.at WHERE framework_documents.document <> excluded.document", "cache:"+name, canonical, nowStamp())
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
	documents := map[string]json.RawMessage{
		"system": canonicalSystem, "zone": canonicalZone, "radio-inhibition": inhibitionRaw,
		"users": usersRaw, "automations": automationsRaw,
	}
	// A repeated catalogue is a read, not a durable operation. Compare its
	// complete document/account bundle in one committed reader snapshot so it
	// cannot queue behind an unrelated fsync or mistake mixed commits for a
	// coherent accepted bundle. Actual changes still use the atomic writer below.
	unchanged, err := func() (bool, error) {
		tx, err := s.readDB.Begin()
		if err != nil {
			return false, err
		}
		defer tx.Rollback()
		for name, raw := range documents {
			if len(raw) == 0 {
				continue
			}
			var previous []byte
			if err = tx.QueryRow("SELECT document FROM framework_documents WHERE name=?", "cache:"+name).Scan(&previous); errors.Is(err, sql.ErrNoRows) {
				return false, nil
			} else if err != nil {
				return false, err
			}
			if !bytes.Equal(previous, raw) {
				return false, nil
			}
		}
		// An omitted Zone means retain it, including its home-System guard.
		// Supplied matching documents have already passed the checks above.
		if len(canonicalZone) == 0 {
			var previous []byte
			err = tx.QueryRow("SELECT document FROM framework_documents WHERE name='cache:zone'").Scan(&previous)
			if err != nil && !errors.Is(err, sql.ErrNoRows) {
				return false, err
			}
			if err == nil {
				var retained ZoneConfig
				if err = json.Unmarshal(previous, &retained); err != nil {
					return false, err
				}
				if retained.SystemID != system.ID {
					return false, errors.New("retained Zone belongs to a different home system")
				}
			}
		}
		var previousUsername string
		var previousHash []byte
		err = tx.QueryRow("SELECT username,password_hash FROM framework_administrator WHERE singleton=1").Scan(&previousUsername, &previousHash)
		if errors.Is(err, sql.ErrNoRows) {
			return false, nil
		}
		return err == nil && previousUsername == username && bytes.Equal(previousHash, hash), err
	}()
	if err != nil {
		return err
	}
	if unchanged {
		return nil
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
	for name, raw := range documents {
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
	return tx.Commit()
}

func (s *Store) Account() (string, []byte, error) {
	var username string
	var hash []byte
	err := s.readDB.QueryRow("SELECT username,password_hash FROM framework_administrator WHERE singleton=1").Scan(&username, &hash)
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

// Diagnostics describe this process only. They never enter the configuration
// database; durable historical observations belong to the activity owner.
const diagnosticLimit = 100

func (s *Store) AddEvent(kind, message string) error {
	kind = strings.TrimSpace(kind)
	message = strings.TrimSpace(message)
	if kind == "" || len(kind) > 80 || message == "" || len(message) > 2000 {
		return errors.New("event kind or message is invalid")
	}
	s.diagnosticMu.Lock()
	defer s.diagnosticMu.Unlock()
	s.diagnosticNextID++
	event := Event{ID: s.diagnosticNextID, At: time.Now().UTC(), Kind: kind, Message: message}
	if len(s.diagnostics) == diagnosticLimit {
		copy(s.diagnostics, s.diagnostics[1:])
		s.diagnostics[len(s.diagnostics)-1] = event
	} else {
		s.diagnostics = append(s.diagnostics, event)
	}
	return nil
}

func (s *Store) Events(limit int) ([]Event, error) {
	if limit < 1 || limit > diagnosticLimit {
		limit = diagnosticLimit
	}
	s.diagnosticMu.RLock()
	defer s.diagnosticMu.RUnlock()
	count := min(limit, len(s.diagnostics))
	events := make([]Event, count)
	for i := range events {
		events[i] = s.diagnostics[len(s.diagnostics)-1-i]
	}
	return events, nil
}

func nowStamp() string { return time.Now().UTC().Format(time.RFC3339Nano) }
