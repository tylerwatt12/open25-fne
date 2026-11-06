package framework

import (
	"bytes"
	"crypto/sha256"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"slices"
	"strconv"
	"strings"

	"golang.org/x/crypto/bcrypt"
)

type ProfileZone struct {
	Name       string `json:"name"`
	Talkgroups []int  `json:"talkgroups"`
}
type ReceiveSetting struct {
	TalkgroupID int     `json:"talkgroup_id"`
	Volume      float64 `json:"volume"`
	Pan         int     `json:"pan"`
	Muted       bool    `json:"muted"`
}
type UserProfile struct {
	Name                string           `json:"name,omitempty"`
	PreferredPositionID string           `json:"preferred_position_id,omitempty"`
	Zones               []ProfileZone    `json:"zones"`
	Receive             []ReceiveSetting `json:"receive"`
	SelectedTalkgroup   int              `json:"selected_talkgroup"`
	TransmitTalkgroups  []int            `json:"transmit_talkgroups"`
	Desktop             *DesktopProfile  `json:"desktop,omitempty"`
}

// ConsolePosition is a system-owned desk identity. Human accounts may be
// authorized to use the same desk on different shifts; it is never a password.
type ConsolePosition struct {
	ID         string `json:"id"`
	Name       string `json:"name"`
	RadioID    int    `json:"radio_id"`
	Enabled    bool   `json:"enabled"`
	Talkgroups []int  `json:"talkgroups"`
}
type User struct {
	ID          string      `json:"id"`
	Username    string      `json:"username"`
	RadioID     int         `json:"radio_id"`
	PositionIDs []string    `json:"position_ids"`
	Role        string      `json:"role"`
	Enabled     bool        `json:"enabled"`
	Talkgroups  []int       `json:"talkgroups"`
	Permissions []string    `json:"permissions"`
	Profile     UserProfile `json:"profile"`
}
type userRecord struct {
	User         User   `json:"user"`
	PasswordHash []byte `json:"password_hash"`
}
type userWrite struct {
	User
	Password string `json:"password,omitempty"`
}

var userPermissions = []string{"ptt", "secure_ptt", "emergency", "patch", "radio_check", "page", "status", "message", "location_report", "location_view", "location_poll", "emergency_ack", "emergency_clear", "crypto_admin", "inhibit", "uninhibit", "regroup"}

func (s *Store) userRecords() ([]userRecord, error) {
	var raw []byte
	err := s.db.QueryRow("SELECT document FROM framework_documents WHERE name=?", s.usersKey()).Scan(&raw)
	if errors.Is(err, sql.ErrNoRows) {
		return []userRecord{}, nil
	}
	if err != nil {
		return nil, err
	}
	var records []userRecord
	if err = json.Unmarshal(raw, &records); err != nil {
		return nil, err
	}
	if records == nil {
		records = []userRecord{}
	}
	return records, nil
}
func (s *Store) usersKey() string {
	if s.role == RoleSystem {
		return "users"
	}
	return "cache:users"
}
func (s *Store) Users() ([]User, error) {
	r, err := s.userRecords()
	if err != nil {
		return nil, err
	}
	out := []User{}
	for _, r := range r {
		out = append(out, r.User)
	}
	return out, nil
}
func (s *Store) User(id string) (User, []byte, error) {
	records, err := s.userRecords()
	if err != nil {
		return User{}, nil, err
	}
	for _, r := range records {
		if r.User.ID == id {
			return r.User, append([]byte(nil), r.PasswordHash...), nil
		}
	}
	return User{}, nil, sql.ErrNoRows
}
func (s *Store) LoginUser(identity string) (User, []byte, error) {
	records, err := s.userRecords()
	if err != nil {
		return User{}, nil, err
	}
	for _, r := range records {
		if r.User.Username == identity || (r.User.Role == "mobile" && r.User.RadioID > 0 && strconv.Itoa(r.User.RadioID) == identity) {
			return r.User, append([]byte(nil), r.PasswordHash...), nil
		}
	}
	return User{}, nil, sql.ErrNoRows
}
func (s *Store) SaveUser(id string, u User, password string) error {
	if s.role != RoleSystem {
		return errors.New("user accounts belong to System Administration")
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	records, err := s.userRecords()
	if err != nil {
		return err
	}
	var hash []byte
	index := -1
	for i, r := range records {
		if r.User.ID == id {
			index = i
			hash = r.PasswordHash
			break
		}
	}
	if password != "" {
		hash, err = bcrypt.GenerateFromPassword([]byte(password), bcrypt.DefaultCost)
		if err != nil {
			return err
		}
	}
	r := userRecord{u, hash}
	if index < 0 {
		records = append(records, r)
	} else {
		records[index] = r
	}
	raw, err := json.Marshal(records)
	if err != nil {
		return err
	}
	_, err = s.db.Exec("INSERT INTO framework_documents(name,document,at) VALUES(?,?,?) ON CONFLICT(name) DO UPDATE SET document=excluded.document,at=excluded.at", s.usersKey(), []byte(raw), nowStamp())
	return err
}
func (s *Store) DeleteUser(id string) error {
	if s.role != RoleSystem {
		return errors.New("user accounts belong to System Administration")
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	r, err := s.userRecords()
	if err != nil {
		return err
	}
	out := slices.DeleteFunc(r, func(v userRecord) bool { return v.User.ID == id })
	raw, err := json.Marshal(out)
	if err != nil {
		return err
	}
	_, err = s.db.Exec("UPDATE framework_documents SET document=?,at=? WHERE name=?", []byte(raw), nowStamp(), s.usersKey())
	return err
}
func (s *Store) SaveProfile(id string, p UserProfile) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	records, err := s.userRecords()
	if err != nil {
		return err
	}
	found := false
	for i, r := range records {
		if r.User.ID == id {
			// Radio-style programming and desktop layouts have separate writes.
			// A stale radio profile must not replace a newer desktop workspace.
			p.Desktop = r.User.Profile.Desktop
			records[i].User.Profile = p
			found = true
		}
	}
	if !found {
		return sql.ErrNoRows
	}
	raw, err := json.Marshal(records)
	if err != nil {
		return err
	}
	_, err = s.db.Exec("UPDATE framework_documents SET document=?,at=? WHERE name=?", []byte(raw), nowStamp(), s.usersKey())
	return err
}

func (s *Service) usersHTTP(w http.ResponseWriter, r *http.Request) {
	if s.Config().Role != RoleSystem {
		fail(w, 403, errors.New("users are managed in System Administration"))
		return
	}
	path := strings.TrimPrefix(r.URL.Path, "/api/users")
	id := strings.TrimPrefix(path, "/")
	if r.Method == "GET" && path == "" {
		users, err := s.Store.Users()
		if err != nil {
			fail(w, 500, err)
			return
		}
		respond(w, 200, map[string]any{"users": users})
		return
	}
	if r.Method == "DELETE" && path != "" {
		if err := s.Store.DeleteUser(id); err != nil {
			fail(w, 400, err)
			return
		}
		_ = s.Store.AddEvent("account", "Application user removed.")
		respond(w, 200, map[string]bool{"saved": true})
		return
	}
	if (r.Method != "POST" || path != "") && (r.Method != "PUT" || path == "") {
		w.WriteHeader(405)
		return
	}
	var input userWrite
	if err := readJSON(w, r, &input); err != nil {
		fail(w, 400, err)
		return
	}
	if path == "" {
		id = input.ID
	}
	if err := s.Store.SaveUser(id, input.User, input.Password); err != nil {
		fail(w, 400, err)
		return
	}
	_ = s.Store.AddEvent("account", "Application user saved.")
	respond(w, 200, map[string]bool{"saved": true})
}

func sameUserAuthentication(a, b userRecord) bool {
	return a.User.ID == b.User.ID && a.User.Username == b.User.Username && a.User.RadioID == b.User.RadioID && slices.Equal(a.User.PositionIDs, b.User.PositionIDs) && a.User.Enabled == b.User.Enabled && a.User.Role == b.User.Role && slices.Equal(a.User.Talkgroups, b.User.Talkgroups) && slices.Equal(a.User.Permissions, b.User.Permissions) && bytes.Equal(a.PasswordHash, b.PasswordHash)
}
func userDigest(u User, hash []byte) [32]byte {
	u.Profile = UserProfile{}
	raw, _ := json.Marshal(userRecord{u, hash})
	return sha256.Sum256(raw)
}
func userProof(u User, hash []byte) string {
	u.Talkgroups = nil
	u.Permissions = nil
	v := userDigest(u, hash)
	return fmt.Sprintf("%x", v[:])
}
