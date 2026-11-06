// Package activity stores structured observations independently of media timing.
package activity

import (
	"context"
	"crypto/rand"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"errors"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	_ "modernc.org/sqlite"
)

type Event struct {
	ID          string         `json:"id"`
	At          time.Time      `json:"at"`
	ReceivedAt  time.Time      `json:"received_at"`
	Origin      string         `json:"origin"`
	SiteID      string         `json:"site_id,omitempty"`
	ChannelID   string         `json:"channel_id,omitempty"`
	RadioID     int            `json:"radio_id,omitempty"`
	TalkgroupID int            `json:"talkgroup_id,omitempty"`
	CallID      string         `json:"call_id,omitempty"`
	Type        string         `json:"type"`
	Label       string         `json:"label"`
	Message     string         `json:"message"`
	Audience    string         `json:"audience"`
	Severity    string         `json:"severity"`
	Outcome     string         `json:"outcome,omitempty"`
	SourceKind  string         `json:"source_kind,omitempty"`
	Details     map[string]any `json:"details,omitempty"`
}
type Query struct {
	Context                             context.Context
	Limit                               int
	ID                                  string
	Cursor                              string
	Since, Until                        time.Time
	Type, SourceKind, ChannelID, CallID string
	RadioID, TalkgroupID                int
	SiteID, Audience                    string
	Groups                              []int
	Restricted                          bool
	LocationRadioID                     int
}
type Radio struct {
	RID                 int       `json:"rid"`
	Alias               string    `json:"alias"`
	Registered          bool      `json:"registered"`
	AffiliatedTalkgroup int       `json:"affiliated_talkgroup"`
	SiteID              string    `json:"site_id"`
	LastSeen            time.Time `json:"last_seen"`
	Kind                string    `json:"kind"`
}
type Store struct {
	db        *sql.DB
	queue     chan write
	wg        sync.WaitGroup
	stop      chan struct{}
	once      sync.Once
	mu        sync.RWMutex
	closed    bool
	pruneMu   sync.Mutex
	appends   uint64
	archive   bool
	ingestion ingestionCounters
}
type write struct {
	event   Event
	barrier chan struct{}
}

const stampFormat = "2006-01-02T15:04:05.000000000Z07:00"

func Open(path string) (*Store, error) {
	if err := os.MkdirAll(filepath.Dir(path), 0700); err != nil {
		return nil, err
	}
	f, err := os.OpenFile(path, os.O_CREATE|os.O_RDWR, 0600)
	if err != nil {
		return nil, err
	}
	_ = f.Chmod(0600)
	_ = f.Close()
	absolutePath, err := filepath.Abs(path)
	if err != nil {
		return nil, err
	}
	db, err := sql.Open("sqlite", (&url.URL{Scheme: "file", Path: absolutePath}).String())
	if err != nil {
		return nil, err
	}
	db.SetMaxOpenConns(1)
	_, err = db.Exec(`PRAGMA journal_mode=WAL; PRAGMA busy_timeout=5000;
CREATE TABLE IF NOT EXISTS radio_events(id TEXT PRIMARY KEY,at TEXT NOT NULL,received_at TEXT NOT NULL,origin TEXT NOT NULL,site_id TEXT NOT NULL,channel_id TEXT NOT NULL,radio_id INTEGER NOT NULL,talkgroup_id INTEGER NOT NULL,call_id TEXT NOT NULL,type TEXT NOT NULL,label TEXT NOT NULL,message TEXT NOT NULL,audience TEXT NOT NULL,severity TEXT NOT NULL,outcome TEXT NOT NULL,source_kind TEXT NOT NULL,details TEXT NOT NULL,uploaded INTEGER NOT NULL DEFAULT 0);
CREATE INDEX IF NOT EXISTS event_time ON radio_events(received_at);
CREATE INDEX IF NOT EXISTS event_radio ON radio_events(radio_id,at);
CREATE INDEX IF NOT EXISTS event_group ON radio_events(talkgroup_id,at);
CREATE TABLE IF NOT EXISTS radio_presence(radio_id INTEGER NOT NULL,site_id TEXT NOT NULL,registered INTEGER NOT NULL,talkgroup_id INTEGER NOT NULL,last_seen TEXT NOT NULL,kind TEXT NOT NULL,PRIMARY KEY(radio_id,site_id));
`)
	if err != nil {
		db.Close()
		return nil, err
	}
	if err = historySchema(db); err != nil {
		db.Close()
		return nil, err
	}
	s := &Store{db: db, queue: make(chan write, 1024), stop: make(chan struct{}), ingestion: ingestionCounters{startedAt: time.Now().UTC()}}
	s.wg.Add(1)
	go s.writer()
	return s, nil
}
func NewID() string { v := make([]byte, 16); _, _ = rand.Read(v); return hex.EncodeToString(v) }

// OpenArchive opens the final central destination; it has no upstream queue.
func OpenArchive(path string) (*Store, error) {
	s, err := Open(path)
	if err != nil {
		return nil, err
	}
	s.archive = true
	if _, err = s.db.Exec("UPDATE radio_events SET uploaded=1 WHERE uploaded=0"); err != nil {
		_ = s.Close()
		return nil, err
	}
	return s, nil
}
func normalize(e Event) Event {
	if e.ID == "" {
		e.ID = NewID()
	}
	if e.At.IsZero() {
		e.At = time.Now().UTC()
	}
	e.ReceivedAt = time.Now().UTC()
	if e.Audience == "" {
		e.Audience = "operator"
	}
	if e.Severity == "" {
		e.Severity = "info"
	}
	if e.Label == "" {
		e.Label = strings.ReplaceAll(e.Type, "_", " ")
	}
	return e
}
func (s *Store) Enqueue(e Event) bool {
	s.mu.RLock()
	defer s.mu.RUnlock()
	if s.closed {
		return false
	}
	e = normalize(e)
	select {
	case s.queue <- write{event: e}:
		return true
	default:
		s.ingestion.queueDropped.Add(1)
		return false
	}
}
func (s *Store) writer() {
	defer s.wg.Done()
	cleanup := time.NewTicker(time.Minute)
	defer cleanup.Stop()
	for {
		select {
		case e := <-s.queue:
			s.write(e)
		case now := <-cleanup.C:
			ctx, cancel := context.WithTimeout(context.Background(), maintenanceBudget)
			_, _ = s.Prune(ctx, now.UTC())
			cancel()
		case <-s.stop:
			for {
				select {
				case e := <-s.queue:
					s.write(e)
				default:
					return
				}
			}
		}
	}
}
func (s *Store) write(w write) {
	if w.barrier != nil {
		close(w.barrier)
		return
	}
	_ = s.Append(w.event)
}
func (s *Store) Append(e Event) (err error) {
	e = normalize(e)
	if len(e.ID) > 128 || e.Origin == "" || len(e.Origin) > 128 || e.Type == "" || len(e.Type) > 128 || len(e.SiteID) > 128 || len(e.ChannelID) > 128 || len(e.CallID) > 128 || len(e.Label) > 256 || len(e.SourceKind) > 32 || len(e.Outcome) > 256 || len(e.Message) > 2000 || e.RadioID < 0 || e.RadioID > 0xFFFFFE || e.TalkgroupID < 0 || e.TalkgroupID > 0xFFFF {
		s.ingestion.rejected.Add(1)
		return errors.New("invalid activity observation")
	}
	if e.Audience != "dispatcher" && e.Audience != "operator" && e.Audience != "diagnostic" && e.Audience != "security" {
		s.ingestion.rejected.Add(1)
		return errors.New("invalid activity audience")
	}
	details, err := json.Marshal(e.Details)
	if err != nil || len(details) > 16384 {
		s.ingestion.rejected.Add(1)
		return errors.New("invalid activity details")
	}
	committed := false
	defer func() {
		if !committed && err != nil {
			s.ingestion.writeFailures.Add(1)
			s.ingestion.lastWriteFailure.Store(time.Now().UTC().UnixNano())
		}
	}()
	tx, err := s.db.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback()
	r, err := tx.Exec("INSERT OR IGNORE INTO radio_events VALUES(?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?)", e.ID, e.At.UTC().Format(stampFormat), e.ReceivedAt.UTC().Format(stampFormat), e.Origin, e.SiteID, e.ChannelID, e.RadioID, e.TalkgroupID, e.CallID, e.Type, e.Label, e.Message, e.Audience, e.Severity, e.Outcome, e.SourceKind, string(details), s.archive)
	if err != nil {
		return err
	}
	n, _ := r.RowsAffected()
	if n == 0 {
		err = tx.Commit()
		committed = err == nil
		if committed {
			s.ingestion.lastWriteSuccess.Store(time.Now().UTC().UnixNano())
		}
		return err
	}
	if e.RadioID > 0 && (e.Type == "registration" || e.Type == "affiliation" || e.Type == "deregistration") {
		registered := e.Type != "deregistration"
		tg := 0
		if e.Type == "affiliation" {
			tg = e.TalkgroupID
		}
		_, err = tx.Exec(`INSERT INTO radio_presence VALUES(?,?,?,?,?,?) ON CONFLICT(radio_id,site_id) DO UPDATE SET registered=excluded.registered,talkgroup_id=CASE WHEN ?='registration' THEN radio_presence.talkgroup_id ELSE excluded.talkgroup_id END,last_seen=excluded.last_seen,kind=excluded.kind WHERE excluded.last_seen>=radio_presence.last_seen`, e.RadioID, e.SiteID, registered, tg, e.At.UTC().Format(stampFormat), e.SourceKind, e.Type)
		if err != nil {
			return err
		}
	}
	if err = projectHistory(tx, e); err != nil {
		return err
	}

	if err != nil {
		return err
	}
	if err = tx.Commit(); err != nil {
		return err
	}
	committed = true
	s.ingestion.lastWriteSuccess.Store(time.Now().UTC().UnixNano())
	s.pruneMu.Lock()
	s.appends++
	prune := s.appends%256 == 0
	s.pruneMu.Unlock()
	if prune {
		ctx, cancel := context.WithTimeout(context.Background(), maintenanceBudget)
		_, err = s.Prune(ctx, time.Now().UTC())
		cancel()
	}
	return err
}
func (s *Store) Events(q Query) (_ []Event, err error) {
	ctx := q.Context
	if ctx == nil {
		ctx = context.Background()
	}
	defer func() { err = contextSQLError(ctx, err) }()
	if q.Limit < 1 || q.Limit > 1000 {
		q.Limit = 200
	}
	where, args, err := eventWhere(q)
	if err != nil {
		return nil, err
	}
	args = append(args, q.Limit)
	rows, err := s.db.QueryContext(ctx, "SELECT id,at,received_at,origin,site_id,channel_id,radio_id,talkgroup_id,call_id,type,label,message,audience,severity,outcome,source_kind,details FROM radio_events WHERE "+strings.Join(where, " AND ")+" ORDER BY at DESC,id DESC LIMIT ?", args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []Event{}
	for rows.Next() {
		var e Event
		var at, received, details string
		if err = rows.Scan(&e.ID, &at, &received, &e.Origin, &e.SiteID, &e.ChannelID, &e.RadioID, &e.TalkgroupID, &e.CallID, &e.Type, &e.Label, &e.Message, &e.Audience, &e.Severity, &e.Outcome, &e.SourceKind, &details); err != nil {
			return nil, err
		}
		e.At, _ = time.Parse(time.RFC3339Nano, at)
		e.ReceivedAt, _ = time.Parse(time.RFC3339Nano, received)
		_ = json.Unmarshal([]byte(details), &e.Details)
		out = append(out, e)
	}
	return out, rows.Err()
}
func (s *Store) Presence() ([]Radio, error) {
	rows, err := s.db.Query("SELECT radio_id,site_id,registered,talkgroup_id,last_seen,kind FROM radio_presence")
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []Radio{}
	for rows.Next() {
		var r Radio
		var at string
		if err = rows.Scan(&r.RID, &r.SiteID, &r.Registered, &r.AffiliatedTalkgroup, &at, &r.Kind); err != nil {
			return nil, err
		}
		r.LastSeen, _ = time.Parse(time.RFC3339Nano, at)
		out = append(out, r)
	}
	return out, rows.Err()
}
func (s *Store) Calls() ([]Call, error) {
	page, err := s.CallHistory(Query{Limit: 200})
	return page.Calls, err
}
func (s *Store) Pending(limit int) ([]Event, error) {
	if limit < 1 || limit > 256 {
		limit = 256
	}
	rows, err := s.db.Query("SELECT id FROM radio_events WHERE uploaded=0 ORDER BY received_at LIMIT ?", limit)
	if err != nil {
		return nil, err
	}
	ids := []string{}
	for rows.Next() {
		var id string
		if err = rows.Scan(&id); err != nil {
			rows.Close()
			return nil, err
		}
		ids = append(ids, id)
	}
	rows.Close()
	out := []Event{}
	for _, id := range ids {
		e, err := s.Events(Query{ID: id, Limit: 1})
		if err != nil {
			return nil, err
		}
		if len(e) > 0 {
			out = append(out, e[0])
		}
	}
	return out, nil
}
func (s *Store) Acknowledge(ids []string) error {
	tx, err := s.db.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback()
	for _, id := range ids {
		if _, err = tx.Exec("UPDATE radio_events SET uploaded=1 WHERE id=?", id); err != nil {
			return err
		}
	}
	return tx.Commit()
}
func (s *Store) Flush(ctx context.Context) error {
	s.mu.RLock()
	if s.closed {
		s.mu.RUnlock()
		return errors.New("activity store is closed")
	}
	done := make(chan struct{})
	select {
	case s.queue <- write{barrier: done}:
		s.mu.RUnlock()
	case <-ctx.Done():
		s.mu.RUnlock()
		return ctx.Err()
	}
	select {
	case <-done:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}
func (s *Store) Close() error {
	s.once.Do(func() { s.mu.Lock(); s.closed = true; close(s.stop); s.mu.Unlock(); s.wg.Wait() })
	return s.db.Close()
}
