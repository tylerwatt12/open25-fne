package activity

import (
	"context"
	"database/sql"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"time"
)

// A call is counted once. Its destinations record each talkgroup and Site
// delivery independently. Lifetime is not claimed to be measured speech time.
type Call struct {
	ID           string        `json:"id"`
	RadioID      int           `json:"radio_id"`
	StartedAt    time.Time     `json:"started_at"`
	EndedAt      *time.Time    `json:"ended_at,omitempty"`
	SourceKind   string        `json:"source_kind"`
	Destinations []Destination `json:"destinations"`
	LifetimeMS   *int64        `json:"lifetime_ms"`
	SpeechMS     *int64        `json:"speech_ms"`
}
type Destination struct {
	SiteID      string     `json:"site_id"`
	TalkgroupID int        `json:"talkgroup_id"`
	ChannelID   string     `json:"channel_id,omitempty"`
	StartedAt   time.Time  `json:"started_at"`
	EndedAt     *time.Time `json:"ended_at,omitempty"`
	Outcome     string     `json:"outcome"`
	AllocatedMS *int64     `json:"allocated_ms"`
	SpeechMS    *int64     `json:"speech_ms"`
}
type Location struct {
	ID          string    `json:"id"`
	RadioID     int       `json:"radio_id"`
	TalkgroupID int       `json:"talkgroup_id"`
	SiteID      string    `json:"site_id,omitempty"`
	ScopeID     string    `json:"scope_id"`
	Latitude    float64   `json:"latitude"`
	Longitude   float64   `json:"longitude"`
	Accuracy    float64   `json:"accuracy"`
	FixAt       time.Time `json:"fix_at"`
	ReceivedAt  time.Time `json:"received_at"`
	SourceKind  string    `json:"source_kind"`
	AgeSeconds  int64     `json:"age_seconds"`
	Stale       bool      `json:"stale"`
}
type EventPage struct {
	Events     []Event `json:"events"`
	NextCursor string  `json:"next_cursor,omitempty"`
}
type CallPage struct {
	Calls      []Call `json:"calls"`
	NextCursor string `json:"next_cursor,omitempty"`
}
type LocationPage struct {
	Locations  []Location `json:"locations"`
	NextCursor string     `json:"next_cursor,omitempty"`
}
type Retention struct {
	Days         int `json:"retention_days"`
	MaxEvents    int `json:"max_events"`
	MaxCalls     int `json:"max_calls"`
	MaxLocations int `json:"max_locations"`
	StaleSeconds int `json:"location_stale_seconds"`
}
type Removed struct {
	Events     int64 `json:"events"`
	Calls      int64 `json:"calls"`
	Locations  int64 `json:"locations"`
	Unuploaded int64 `json:"unuploaded"`
}
type HistoryStatus struct {
	Ingestion        IngestionHealth `json:"ingestion"`
	Settings         Retention       `json:"settings"`
	Events           int64           `json:"events"`
	Calls            int64           `json:"calls"`
	Locations        int64           `json:"locations"`
	PendingUpload    int64           `json:"pending_upload"`
	PrunedUnuploaded int64           `json:"pruned_unuploaded"`
}
type Statistics struct {
	Calls          int64            `json:"calls"`
	CompletedCalls int64            `json:"completed_calls"`
	SiteDeliveries int64            `json:"site_deliveries"`
	CallLifetimeMS int64            `json:"call_lifetime_ms"`
	SpeechMS       *int64           `json:"speech_ms"`
	Events         int64            `json:"events"`
	ByType         map[string]int64 `json:"events_by_type"`
}

var defaultRetention = Retention{Days: 30, MaxEvents: 100000, MaxCalls: 20000, MaxLocations: 50000, StaleSeconds: 300}

const projectionSchema = `
CREATE TABLE IF NOT EXISTS calls(id TEXT PRIMARY KEY,radio_id INTEGER NOT NULL,started_at TEXT NOT NULL,source_kind TEXT NOT NULL);
CREATE INDEX IF NOT EXISTS calls_time ON calls(started_at,id);
CREATE TABLE IF NOT EXISTS call_destinations(call_id TEXT NOT NULL,site_id TEXT NOT NULL,talkgroup_id INTEGER NOT NULL,channel_id TEXT NOT NULL,started_at TEXT NOT NULL,ended_at TEXT,outcome TEXT NOT NULL,allocated_ms INTEGER,speech_ms INTEGER,PRIMARY KEY(call_id,site_id,talkgroup_id));
CREATE INDEX IF NOT EXISTS destinations_scope ON call_destinations(site_id,talkgroup_id,call_id);
CREATE TABLE IF NOT EXISTS call_allocations(call_id TEXT NOT NULL,site_id TEXT NOT NULL,talkgroup_id INTEGER NOT NULL,allocation_id TEXT NOT NULL,channel_id TEXT NOT NULL,started_at TEXT NOT NULL,ended_at TEXT,outcome TEXT NOT NULL,allocated_ms INTEGER,speech_ms INTEGER,PRIMARY KEY(call_id,site_id,talkgroup_id,allocation_id));
CREATE INDEX IF NOT EXISTS allocations_call ON call_allocations(call_id,site_id,talkgroup_id);
CREATE TABLE IF NOT EXISTS position_history(id TEXT PRIMARY KEY,radio_id INTEGER NOT NULL,talkgroup_id INTEGER NOT NULL,site_id TEXT NOT NULL,scope_id TEXT NOT NULL,latitude REAL NOT NULL,longitude REAL NOT NULL,accuracy REAL NOT NULL,fix_at TEXT NOT NULL,received_at TEXT NOT NULL,source_kind TEXT NOT NULL);
CREATE INDEX IF NOT EXISTS positions_radio_time ON position_history(radio_id,fix_at,id);
CREATE TABLE IF NOT EXISTS history_settings(id INTEGER PRIMARY KEY CHECK(id=1),retention_days INTEGER NOT NULL,max_events INTEGER NOT NULL,max_calls INTEGER NOT NULL,max_locations INTEGER NOT NULL,stale_seconds INTEGER NOT NULL,pruned_unuploaded INTEGER NOT NULL DEFAULT 0);
INSERT OR IGNORE INTO history_settings VALUES(1,30,100000,20000,50000,300,0);`

func historySchema(db *sql.DB) error {
	var old int
	if err := db.QueryRow("SELECT COUNT(*) FROM sqlite_master WHERE type='table' AND name='radio_calls'").Scan(&old); err != nil {
		return err
	}
	if old != 0 {
		return errors.New("activity projections need rebuilding; stop the service and run history-rebuild --database PATH")
	}
	var calls, allocations int
	if err := db.QueryRow("SELECT COUNT(*) FROM sqlite_master WHERE type='table' AND name='calls'").Scan(&calls); err != nil {
		return err
	}
	if err := db.QueryRow("SELECT COUNT(*) FROM sqlite_master WHERE type='table' AND name='call_allocations'").Scan(&allocations); err != nil {
		return err
	}
	if calls != 0 && allocations == 0 {
		return errors.New("activity projections need rebuilding; stop the service and run history-rebuild --database PATH")
	}
	_, err := db.Exec(projectionSchema)
	return err
}

// RebuildHistory is an explicit offline maintenance action. Original events and
// their upload acknowledgments survive; no compatibility schema stays in use.
func RebuildHistory(path string) error {
	if _, err := os.Stat(path); err != nil {
		return err
	}
	lock, err := os.OpenFile(filepath.Join(filepath.Dir(path), "service.lock"), os.O_CREATE|os.O_RDWR, 0600)
	if err != nil {
		return err
	}
	defer lock.Close()
	if err = syscall.Flock(int(lock.Fd()), syscall.LOCK_EX|syscall.LOCK_NB); err != nil {
		return errors.New("stop the service before rebuilding its history")
	}
	defer syscall.Flock(int(lock.Fd()), syscall.LOCK_UN)
	absolutePath, err := filepath.Abs(path)
	if err != nil {
		return err
	}
	db, err := sql.Open("sqlite", (&url.URL{Scheme: "file", Path: absolutePath}).String())
	if err != nil {
		return err
	}
	defer db.Close()
	db.SetMaxOpenConns(1)
	tx, err := db.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback()
	if _, err = tx.Exec("DROP TABLE IF EXISTS radio_calls; DROP TABLE IF EXISTS calls; DROP TABLE IF EXISTS call_destinations; DROP TABLE IF EXISTS call_allocations; DROP TABLE IF EXISTS position_history;" + projectionSchema); err != nil {
		return err
	}
	lastAt, lastID := "", ""
	for {
		rows, err := tx.Query("SELECT id,at,received_at,origin,site_id,channel_id,radio_id,talkgroup_id,call_id,type,label,message,audience,severity,outcome,source_kind,details FROM radio_events WHERE at>? OR (at=? AND id>?) ORDER BY at,id LIMIT 1000", lastAt, lastAt, lastID)
		if err != nil {
			return err
		}
		batch := []Event{}
		for rows.Next() {
			var e Event
			var at, received, details string
			if err = rows.Scan(&e.ID, &at, &received, &e.Origin, &e.SiteID, &e.ChannelID, &e.RadioID, &e.TalkgroupID, &e.CallID, &e.Type, &e.Label, &e.Message, &e.Audience, &e.Severity, &e.Outcome, &e.SourceKind, &details); err != nil {
				rows.Close()
				return err
			}
			e.At, _ = time.Parse(time.RFC3339Nano, at)
			e.ReceivedAt, _ = time.Parse(time.RFC3339Nano, received)
			_ = json.Unmarshal([]byte(details), &e.Details)
			batch = append(batch, e)
			lastAt, lastID = at, e.ID
		}
		err = rows.Err()
		rows.Close()
		if err != nil {
			return err
		}
		for _, e := range batch {
			if err = projectHistory(tx, e); err != nil {
				return err
			}
		}
		if len(batch) < 1000 {
			break
		}
	}

	return tx.Commit()
}

func projectHistory(tx *sql.Tx, e Event) error {
	if e.CallID != "" && (e.Type == "call_start" || e.Type == "call_end") {
		at := e.At.UTC().Format(stampFormat)
		if _, err := tx.Exec(`INSERT INTO calls VALUES(?,?,?,?) ON CONFLICT(id) DO UPDATE SET started_at=MIN(calls.started_at,excluded.started_at)`, e.CallID, e.RadioID, at, e.SourceKind); err != nil {
			return err
		}
		var end any
		if e.Type == "call_end" {
			end = at
		}
		allocation := e.CallID
		if id, ok := e.Details["allocation_id"].(string); ok {
			if id == "" || len(id) > 128 {
				return errors.New("invalid call allocation identity")
			}
			allocation = id
		}
		_, err := tx.Exec(`INSERT INTO call_allocations VALUES(?,?,?,?,?,?,?,?,?,?) ON CONFLICT(call_id,site_id,talkgroup_id,allocation_id) DO UPDATE SET started_at=MIN(call_allocations.started_at,excluded.started_at),ended_at=CASE WHEN excluded.ended_at IS NULL THEN call_allocations.ended_at ELSE MAX(COALESCE(call_allocations.ended_at,excluded.ended_at),excluded.ended_at) END,channel_id=CASE WHEN excluded.channel_id='' THEN call_allocations.channel_id ELSE excluded.channel_id END,outcome=CASE WHEN call_allocations.ended_at IS NOT NULL AND (excluded.ended_at IS NULL OR excluded.ended_at<call_allocations.ended_at) THEN call_allocations.outcome ELSE excluded.outcome END,allocated_ms=CASE WHEN call_allocations.ended_at IS NOT NULL AND (excluded.ended_at IS NULL OR excluded.ended_at<call_allocations.ended_at) THEN call_allocations.allocated_ms ELSE COALESCE(excluded.allocated_ms,call_allocations.allocated_ms) END,speech_ms=CASE WHEN call_allocations.ended_at IS NOT NULL AND (excluded.ended_at IS NULL OR excluded.ended_at<call_allocations.ended_at) THEN call_allocations.speech_ms ELSE COALESCE(excluded.speech_ms,call_allocations.speech_ms) END`, e.CallID, e.SiteID, e.TalkgroupID, allocation, e.ChannelID, at, end, e.Outcome, measuredMS(e.Details, "allocated_ms"), measuredMS(e.Details, "speech_ms"))
		if err != nil {
			return err
		}
		// A Site may leave and later rejoin a conversation with a different
		// one-use allocation. Retain each fact so a late end from an old grant
		// cannot end the current delivery or inflate the logical call count.
		_, err = tx.Exec(`INSERT INTO call_destinations
SELECT a.call_id,a.site_id,a.talkgroup_id,
 COALESCE((SELECT channel_id FROM call_allocations b WHERE b.call_id=a.call_id AND b.site_id=a.site_id AND b.talkgroup_id=a.talkgroup_id AND channel_id<>'' ORDER BY started_at DESC,allocation_id DESC LIMIT 1),''),
 MIN(a.started_at),CASE WHEN SUM(a.ended_at IS NULL)>0 THEN NULL ELSE MAX(a.ended_at) END,
 (SELECT outcome FROM call_allocations b WHERE b.call_id=a.call_id AND b.site_id=a.site_id AND b.talkgroup_id=a.talkgroup_id ORDER BY (ended_at IS NULL) DESC,COALESCE(ended_at,started_at) DESC,allocation_id DESC LIMIT 1),
 CASE WHEN COUNT(a.allocated_ms)=COUNT(*) THEN SUM(a.allocated_ms) ELSE NULL END,
 CASE WHEN COUNT(a.speech_ms)=COUNT(*) THEN SUM(a.speech_ms) ELSE NULL END
FROM call_allocations a WHERE a.call_id=? AND a.site_id=? AND a.talkgroup_id=? GROUP BY a.call_id,a.site_id,a.talkgroup_id
ON CONFLICT(call_id,site_id,talkgroup_id) DO UPDATE SET channel_id=excluded.channel_id,started_at=excluded.started_at,ended_at=excluded.ended_at,outcome=excluded.outcome,allocated_ms=excluded.allocated_ms,speech_ms=excluded.speech_ms`, e.CallID, e.SiteID, e.TalkgroupID)
		if err != nil {
			return err
		}
	}
	if e.Type == "location" {
		raw, err := json.Marshal(e.Details)
		if err != nil {
			return err
		}
		var fix struct {
			Latitude  *float64  `json:"latitude"`
			Longitude *float64  `json:"longitude"`
			Accuracy  *float64  `json:"accuracy"`
			FixAt     time.Time `json:"fix_at"`
			ScopeID   string    `json:"scope_id"`
		}
		if json.Unmarshal(raw, &fix) != nil || fix.Latitude == nil || fix.Longitude == nil || fix.Accuracy == nil || fix.FixAt.IsZero() {
			return nil
		}
		lat, lon, accuracy := *fix.Latitude, *fix.Longitude, *fix.Accuracy
		if e.RadioID < 1 || math.IsNaN(lat) || math.IsNaN(lon) || math.IsNaN(accuracy) || math.IsInf(lat, 0) || math.IsInf(lon, 0) || math.IsInf(accuracy, 0) || lat < -90 || lat > 90 || lon < -180 || lon > 180 || accuracy < 0 || accuracy > 100000 {
			return errors.New("invalid position observation")
		}
		if fix.ScopeID == "" {
			fix.ScopeID = e.Origin
		}
		_, err = tx.Exec("INSERT OR IGNORE INTO position_history VALUES(?,?,?,?,?,?,?,?,?,?,?)", e.ID, e.RadioID, e.TalkgroupID, e.SiteID, fix.ScopeID, lat, lon, accuracy, fix.FixAt.UTC().Format(stampFormat), e.ReceivedAt.UTC().Format(stampFormat), e.SourceKind)
		return err
	}
	return nil
}
func measuredMS(details map[string]any, key string) any {
	value, ok := details[key]
	if !ok {
		return nil
	}
	raw, _ := json.Marshal(value)
	var n int64
	if json.Unmarshal(raw, &n) != nil || n < 0 || n > 86400000 {
		return nil
	}
	return n
}

type cursor struct {
	At string `json:"at"`
	ID string `json:"id"`
}

func nextCursor(at time.Time, id string) string {
	raw, _ := json.Marshal(cursor{at.UTC().Format(stampFormat), id})
	return base64.RawURLEncoding.EncodeToString(raw)
}
func readCursor(value string) (cursor, error) {
	var c cursor
	raw, err := base64.RawURLEncoding.DecodeString(value)
	if err != nil || len(raw) > 1024 {
		return c, errors.New("invalid history cursor")
	}
	if json.Unmarshal(raw, &c) != nil || c.ID == "" {
		return c, errors.New("invalid history cursor")
	}
	at, err := time.Parse(time.RFC3339Nano, c.At)
	if err != nil {
		return c, errors.New("invalid history cursor")
	}
	c.At = at.UTC().Format(stampFormat)
	return c, nil
}
func pageLimit(q Query) int {
	if q.Limit < 1 || q.Limit > 1000 {
		return 200
	}
	return q.Limit
}
func scopeWhere(q Query, timeColumn, idColumn string) ([]string, []any, error) {
	where, args := []string{"1=1"}, []any{}
	for _, f := range []struct {
		column string
		value  any
		use    bool
	}{{"radio_id", q.RadioID, q.RadioID > 0}, {"talkgroup_id", q.TalkgroupID, q.TalkgroupID > 0}, {"site_id", q.SiteID, q.SiteID != ""}, {"source_kind", q.SourceKind, q.SourceKind != ""}} {
		if f.use {
			where = append(where, f.column+"=?")
			args = append(args, f.value)
		}
	}
	if !q.Since.IsZero() {
		where = append(where, timeColumn+">=?")
		args = append(args, q.Since.UTC().Format(stampFormat))
	}
	if !q.Until.IsZero() {
		where = append(where, timeColumn+"<=?")
		args = append(args, q.Until.UTC().Format(stampFormat))
	}
	if q.Cursor != "" {
		c, err := readCursor(q.Cursor)
		if err != nil {
			return nil, nil, err
		}
		where = append(where, "("+timeColumn+"<? OR ("+timeColumn+"=? AND "+idColumn+"<?))")
		args = append(args, c.At, c.At, c.ID)
	}
	if q.Restricted {
		if len(q.Groups) == 0 {
			where = append(where, "0=1")
		} else {
			marks := []string{}
			for _, g := range q.Groups {
				marks = append(marks, "?")
				args = append(args, g)
			}
			where = append(where, "talkgroup_id IN ("+strings.Join(marks, ",")+")")
		}
	}
	return where, args, nil
}
func eventWhere(q Query) ([]string, []any, error) {
	where, args, err := scopeWhere(q, "at", "id")
	if err != nil {
		return nil, nil, err
	}
	for _, f := range []struct{ column, value string }{{"id", q.ID}, {"type", q.Type}, {"audience", q.Audience}, {"channel_id", q.ChannelID}, {"call_id", q.CallID}} {
		if f.value != "" {
			where = append(where, f.column+"=?")
			args = append(args, f.value)
		}
	}
	if q.Restricted {
		where = append(where, "audience='dispatcher'")
	}
	if q.LocationRadioID > 0 {
		where = append(where, "(type<>'location' OR radio_id=?)")
		args = append(args, q.LocationRadioID)
	}
	return where, args, nil
}
func (s *Store) EventHistory(q Query) (EventPage, error) {
	limit := pageLimit(q)
	q.Limit = limit
	events, err := s.Events(q)
	page := EventPage{Events: events}
	if len(events) == limit {
		last := events[limit-1]
		page.NextCursor = nextCursor(last.At, last.ID)
	}
	return page, err
}

func (s *Store) CallHistory(q Query) (_ CallPage, err error) {
	ctx := q.Context
	if ctx == nil {
		ctx = context.Background()
	}
	defer func() { err = contextSQLError(ctx, err) }()
	limit := pageLimit(q)
	where, args, err := scopeWhere(q, "started_at", "id")
	if err != nil {
		return CallPage{}, err
	}
	// A joined row exposes only the permitted destination before grouping.
	if q.CallID != "" {
		where = append(where, "id=?")
		args = append(args, q.CallID)
	}
	if q.ChannelID != "" {
		where = append(where, "channel_id=?")
		args = append(args, q.ChannelID)
	}
	whereSQL := strings.Join(where, " AND ")
	args = append(args, limit+1)
	rows, err := s.db.QueryContext(ctx, `SELECT id,radio_id,MIN(started_at),source_kind FROM (SELECT c.id,c.radio_id,c.started_at,c.source_kind,d.site_id,d.talkgroup_id,d.channel_id FROM calls c JOIN call_allocations d ON d.call_id=c.id) WHERE `+whereSQL+` GROUP BY id,radio_id,source_kind ORDER BY MIN(started_at) DESC,id DESC LIMIT ?`, args...)
	if err != nil {
		return CallPage{}, err
	}
	page := CallPage{Calls: []Call{}}
	for rows.Next() {
		var c Call
		var at string
		if err = rows.Scan(&c.ID, &c.RadioID, &at, &c.SourceKind); err != nil {
			rows.Close()
			return page, err
		}
		c.StartedAt, _ = time.Parse(time.RFC3339Nano, at)
		page.Calls = append(page.Calls, c)
	}
	err = rows.Err()
	rows.Close()
	if err != nil {
		return page, err
	}
	if len(page.Calls) > limit {
		page.Calls = page.Calls[:limit]
		last := page.Calls[limit-1]
		page.NextCursor = nextCursor(last.StartedAt, last.ID)
	}
	for i := range page.Calls {
		c := &page.Calls[i]
		c.Destinations, err = s.destinationHistory(q, c.ID)
		if err != nil {
			return page, err
		}
		allEnded, allMeasured := true, true
		var latest time.Time
		var speech int64
		for _, d := range c.Destinations {
			if d.EndedAt == nil {
				allEnded = false
			} else if d.EndedAt.After(latest) {
				latest = *d.EndedAt
			}
			if d.SpeechMS == nil {
				allMeasured = false
			} else if *d.SpeechMS > speech {
				speech = *d.SpeechMS
			}
		}
		if allEnded && !latest.IsZero() {
			c.EndedAt = &latest
			duration := latest.Sub(c.StartedAt).Milliseconds()
			if duration < 0 {
				duration = 0
			}
			c.LifetimeMS = &duration
		}
		if allMeasured && len(c.Destinations) > 0 {
			c.SpeechMS = &speech
		}
	}
	return page, nil
}

// Allocation facts are filtered before grouping. A destination can change
// channels when it rejoins; filtering its previous channel must not disappear
// merely because its latest allocation used another voice resource.
func (s *Store) destinationHistory(q Query, callID string) (_ []Destination, err error) {
	ctx := q.Context
	if ctx == nil {
		ctx = context.Background()
	}
	defer func() { err = contextSQLError(ctx, err) }()
	q.Cursor, q.Since, q.Until = "", time.Time{}, time.Time{}
	where, args, err := scopeWhere(q, "started_at", "call_id")
	if err != nil {
		return nil, err
	}
	where = append(where, "call_id=?")
	args = append(args, callID)
	if q.ChannelID != "" {
		where = append(where, "channel_id=?")
		args = append(args, q.ChannelID)
	}
	rows, err := s.db.QueryContext(ctx, `SELECT site_id,talkgroup_id,channel_id,started_at,ended_at,outcome,allocated_ms,speech_ms FROM (SELECT d.*,c.radio_id,c.source_kind FROM call_allocations d JOIN calls c ON c.id=d.call_id) WHERE `+strings.Join(where, " AND ")+` ORDER BY site_id,talkgroup_id,started_at,allocation_id`, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	type key struct {
		site string
		tg   int
	}
	out := []Destination{}
	indexes := map[key]int{}
	open := map[key]bool{}
	for rows.Next() {
		var d Destination
		var at string
		var end sql.NullString
		var allocated, speech sql.NullInt64
		if err = rows.Scan(&d.SiteID, &d.TalkgroupID, &d.ChannelID, &at, &end, &d.Outcome, &allocated, &speech); err != nil {
			return nil, err
		}
		d.StartedAt, _ = time.Parse(time.RFC3339Nano, at)
		if end.Valid {
			v, _ := time.Parse(time.RFC3339Nano, end.String)
			d.EndedAt = &v
		}
		if allocated.Valid {
			v := allocated.Int64
			d.AllocatedMS = &v
		}
		if speech.Valid {
			v := speech.Int64
			d.SpeechMS = &v
		}
		id := key{d.SiteID, d.TalkgroupID}
		index, exists := indexes[id]
		if !exists {
			indexes[id] = len(out)
			out = append(out, d)
			open[id] = d.EndedAt == nil
			continue
		}
		current := &out[index]
		if d.ChannelID != "" {
			current.ChannelID = d.ChannelID
		}
		if d.EndedAt == nil {
			open[id] = true
			current.Outcome = d.Outcome
		} else if current.EndedAt == nil || d.EndedAt.After(*current.EndedAt) {
			current.EndedAt = d.EndedAt
			if !open[id] {
				current.Outcome = d.Outcome
			}
		}
		if current.AllocatedMS != nil && d.AllocatedMS != nil {
			*current.AllocatedMS += *d.AllocatedMS
		} else {
			current.AllocatedMS = nil
		}
		if current.SpeechMS != nil && d.SpeechMS != nil {
			*current.SpeechMS += *d.SpeechMS
		} else {
			current.SpeechMS = nil
		}
	}
	for id, index := range indexes {
		if open[id] {
			out[index].EndedAt = nil
		}
	}
	return out, rows.Err()
}

func (s *Store) LocationHistory(q Query, now time.Time) (_ LocationPage, err error) {
	ctx := q.Context
	if ctx == nil {
		ctx = context.Background()
	}
	defer func() { err = contextSQLError(ctx, err) }()
	settings, err := s.Retention(ctx)
	if err != nil {
		return LocationPage{}, err
	}
	where, args, err := scopeWhere(q, "fix_at", "id")
	if err != nil {
		return LocationPage{}, err
	}
	limit := pageLimit(q)
	args = append(args, limit+1)
	rows, err := s.db.QueryContext(ctx, "SELECT id,radio_id,talkgroup_id,site_id,scope_id,latitude,longitude,accuracy,fix_at,received_at,source_kind FROM position_history WHERE "+strings.Join(where, " AND ")+" ORDER BY fix_at DESC,id DESC LIMIT ?", args...)
	if err != nil {
		return LocationPage{}, err
	}
	defer rows.Close()
	page := LocationPage{Locations: []Location{}}
	for rows.Next() {
		var p Location
		var fix, received string
		if err = rows.Scan(&p.ID, &p.RadioID, &p.TalkgroupID, &p.SiteID, &p.ScopeID, &p.Latitude, &p.Longitude, &p.Accuracy, &fix, &received, &p.SourceKind); err != nil {
			return page, err
		}
		p.FixAt, _ = time.Parse(time.RFC3339Nano, fix)
		p.ReceivedAt, _ = time.Parse(time.RFC3339Nano, received)
		p.AgeSeconds = int64(now.Sub(p.FixAt).Seconds())
		if p.AgeSeconds < 0 {
			p.AgeSeconds = 0
		}
		p.Stale = p.AgeSeconds >= int64(settings.StaleSeconds)
		page.Locations = append(page.Locations, p)
	}
	if len(page.Locations) > limit {
		page.Locations = page.Locations[:limit]
		last := page.Locations[limit-1]
		page.NextCursor = nextCursor(last.FixAt, last.ID)
	}
	return page, rows.Err()
}

const maintenanceBudget = 5 * time.Second

const retentionQuery = "SELECT retention_days,max_events,max_calls,max_locations,stale_seconds FROM history_settings WHERE id=1"

func scanRetention(row *sql.Row) (Retention, error) {
	var r Retention
	err := row.Scan(&r.Days, &r.MaxEvents, &r.MaxCalls, &r.MaxLocations, &r.StaleSeconds)
	return r, err
}

func contextSQLError(ctx context.Context, err error) error {
	if err != nil && ctx.Err() != nil {
		return ctx.Err()
	}
	return err
}

func (s *Store) Retention(ctx context.Context) (Retention, error) {
	r, err := scanRetention(s.db.QueryRowContext(ctx, retentionQuery))
	return r, contextSQLError(ctx, err)
}

// Settings and their immediate cleanup are one accepted operation. A timeout
// or cleanup failure must not leave new limits saved with only partial pruning.
func (s *Store) SaveRetention(ctx context.Context, r Retention) (err error) {
	defer func() { err = contextSQLError(ctx, err); s.observeMaintenance(err) }()
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	if _, err = tx.ExecContext(ctx, "UPDATE history_settings SET retention_days=?,max_events=?,max_calls=?,max_locations=?,stale_seconds=? WHERE id=1", r.Days, r.MaxEvents, r.MaxCalls, r.MaxLocations, r.StaleSeconds); err != nil {
		return err
	}
	if _, err = pruneInTransaction(ctx, tx, r, time.Now().UTC()); err != nil {
		return err
	}
	return tx.Commit()
}
func (s *Store) HistoryStatus(ctx context.Context) (HistoryStatus, error) {
	v := HistoryStatus{Ingestion: s.IngestionHealth()}
	err := s.db.QueryRowContext(ctx, `SELECT retention_days,max_events,max_calls,max_locations,stale_seconds,
(SELECT COUNT(*) FROM radio_events),(SELECT COUNT(*) FROM calls),(SELECT COUNT(*) FROM position_history),
(SELECT COUNT(*) FROM radio_events WHERE uploaded=0),pruned_unuploaded FROM history_settings WHERE id=1`).Scan(&v.Settings.Days, &v.Settings.MaxEvents, &v.Settings.MaxCalls, &v.Settings.MaxLocations, &v.Settings.StaleSeconds, &v.Events, &v.Calls, &v.Locations, &v.PendingUpload, &v.PrunedUnuploaded)
	return v, contextSQLError(ctx, err)
}
func (s *Store) Prune(ctx context.Context, now time.Time) (removed Removed, err error) {
	defer func() { err = contextSQLError(ctx, err); s.observeMaintenance(err) }()
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return Removed{}, err
	}
	defer tx.Rollback()
	r, err := scanRetention(tx.QueryRowContext(ctx, retentionQuery))
	if err != nil {
		return Removed{}, err
	}
	if removed, err = pruneInTransaction(ctx, tx, r, now); err != nil {
		return Removed{}, err
	}
	if err = tx.Commit(); err != nil {
		return Removed{}, err
	}
	return removed, nil
}

// The caller owns the transaction and its one maintenance outcome. This helper
// never commits or records another failure count.
func pruneInTransaction(ctx context.Context, tx *sql.Tx, r Retention, now time.Time) (Removed, error) {
	cutoff := now.Add(-time.Duration(r.Days) * 24 * time.Hour).UTC().Format(stampFormat)
	v := Removed{}
	condition := "at<? OR id IN (SELECT id FROM radio_events ORDER BY at DESC,id DESC LIMIT -1 OFFSET ?)"
	if err := tx.QueryRowContext(ctx, "SELECT COUNT(*) FROM radio_events WHERE uploaded=0 AND ("+condition+")", cutoff, r.MaxEvents).Scan(&v.Unuploaded); err != nil {
		return Removed{}, err
	}
	result, err := tx.ExecContext(ctx, "DELETE FROM radio_events WHERE "+condition, cutoff, r.MaxEvents)
	if err != nil {
		return Removed{}, err
	}
	v.Events, _ = result.RowsAffected()
	for _, p := range []struct {
		table, column string
		max           int
		count         *int64
	}{{"calls", "started_at", r.MaxCalls, &v.Calls}, {"position_history", "fix_at", r.MaxLocations, &v.Locations}} {
		result, err = tx.ExecContext(ctx, fmt.Sprintf("DELETE FROM %s WHERE %s<? OR id IN (SELECT id FROM %s ORDER BY %s DESC,id DESC LIMIT -1 OFFSET ?)", p.table, p.column, p.table, p.column), cutoff, p.max)
		if err != nil {
			return Removed{}, err
		}
		*p.count, _ = result.RowsAffected()
	}
	if _, err = tx.ExecContext(ctx, "DELETE FROM call_destinations WHERE call_id NOT IN (SELECT id FROM calls)"); err != nil {
		return Removed{}, err
	}
	if _, err = tx.ExecContext(ctx, "DELETE FROM call_allocations WHERE call_id NOT IN (SELECT id FROM calls)"); err != nil {
		return Removed{}, err
	}
	if _, err = tx.ExecContext(ctx, "UPDATE history_settings SET pruned_unuploaded=pruned_unuploaded+? WHERE id=1", v.Unuploaded); err != nil {
		return Removed{}, err
	}
	return v, nil
}

// Statistics groups allocation facts in two bounded queries. It does not load
// every call page or take one SQL connection round trip per conversation.
func (s *Store) Statistics(q Query) (v Statistics, err error) {
	q.Cursor = ""
	ctx := q.Context
	if ctx == nil {
		ctx = context.Background()
	}
	defer func() { err = contextSQLError(ctx, err) }()
	where, args, err := eventWhere(q)
	if err != nil {
		return Statistics{}, err
	}
	v = Statistics{ByType: map[string]int64{}}
	rows, err := s.db.QueryContext(ctx, "SELECT type,COUNT(*) FROM radio_events WHERE "+strings.Join(where, " AND ")+" GROUP BY type", args...)
	if err != nil {
		return v, err
	}
	for rows.Next() {
		var kind string
		var n int64
		if err = rows.Scan(&kind, &n); err != nil {
			rows.Close()
			return v, err
		}
		v.ByType[kind] = n
		v.Events += n
	}
	err = rows.Err()
	rows.Close()
	if err != nil {
		return v, err
	}
	where, args, err = scopeWhere(q, "started_at", "id")
	if err != nil {
		return v, err
	}
	if q.CallID != "" {
		where = append(where, "id=?")
		args = append(args, q.CallID)
	}
	if q.ChannelID != "" {
		where = append(where, "channel_id=?")
		args = append(args, q.ChannelID)
	}
	rows, err = s.db.QueryContext(ctx, `WITH selected AS (
 SELECT c.id,c.radio_id,c.started_at,c.source_kind,d.site_id,d.talkgroup_id,d.channel_id,d.ended_at,d.speech_ms
 FROM calls c JOIN call_allocations d ON d.call_id=c.id
), destinations AS (
 SELECT id,site_id,talkgroup_id,MIN(started_at) AS call_start,
 COUNT(ended_at)=COUNT(*) AS ended,MAX(ended_at) AS last_end,
 CASE WHEN COUNT(speech_ms)=COUNT(*) THEN SUM(speech_ms) ELSE NULL END AS speech_ms
 FROM selected WHERE `+strings.Join(where, " AND ")+` GROUP BY id,site_id,talkgroup_id
)
SELECT id,MIN(call_start),SUM(ended)=COUNT(*),MAX(last_end),SUM(site_id<>''),
 CASE WHEN COUNT(speech_ms)=COUNT(*) THEN MAX(speech_ms) ELSE NULL END
FROM destinations GROUP BY id ORDER BY id`, args...)
	if err != nil {
		return v, err
	}
	defer rows.Close()
	var speech int64
	allSpeech := true
	for rows.Next() {
		var id, start string
		var ended bool
		var end sql.NullString
		var routes int64
		var measured sql.NullInt64
		if err = rows.Scan(&id, &start, &ended, &end, &routes, &measured); err != nil {
			return v, err
		}
		v.Calls++
		v.SiteDeliveries += routes
		if ended && end.Valid {
			first, err := time.Parse(time.RFC3339Nano, start)
			if err != nil {
				return v, errors.New("invalid recorded call start")
			}
			last, err := time.Parse(time.RFC3339Nano, end.String)
			if err != nil {
				return v, errors.New("invalid recorded call ending")
			}
			v.CompletedCalls++
			if duration := last.Sub(first).Milliseconds(); duration > 0 {
				v.CallLifetimeMS += duration
			}
		}
		if measured.Valid {
			speech += measured.Int64
		} else {
			allSpeech = false
		}
	}
	if err = rows.Err(); err != nil {
		return v, err
	}
	if allSpeech && v.Calls > 0 {
		v.SpeechMS = &speech
	}
	return v, nil
}
