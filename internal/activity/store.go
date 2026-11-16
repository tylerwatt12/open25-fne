// Package activity stores structured observations independently of media timing.
package activity

import (
	"context"
	"crypto/rand"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"errors"
	"log"
	"net/url"
	"open25.local/open25-fne/internal/historypriority"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
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
type Store struct {
	db              *sql.DB
	readDB          *sql.DB
	uploadQueue     chan write
	queue           chan write
	wg              sync.WaitGroup
	stop            chan struct{}
	once            sync.Once
	mu              sync.RWMutex
	closed          bool
	commands        chan write
	cleanupWake     chan struct{}
	workerCtx       context.Context
	workerCancel    context.CancelFunc
	cleanupMu       sync.Mutex
	cleanup         CleanupStatus
	cleanupRequests uint64
	archive         bool
	ingestion       ingestionCounters
	closeErr        error // Written by the sole worker before wg completion.
	acquire         func(context.Context) (*historypriority.Turn, error)
	ready           chan struct{}
	initErr         error // Published once before ready closes, also guarded by mu.
	outboxPending   atomic.Int64
	outboxOldest    atomic.Int64
	retention       atomic.Pointer[Retention]
}
type write struct {
	event     Event
	events    []Event
	ctx       context.Context
	result    chan error
	barrier   chan struct{}
	ackIDs    []string
	retention *Retention
	pruneAt   *time.Time
	pruned    chan pruneResult
}

const stampFormat = "2006-01-02T15:04:05.000000000Z07:00"

// EventBatchLimit bounds live capture and upstream history transfer batches.
const EventBatchLimit = 32

// OutboxCapacity bounds durable source observations without expiring accepted
// records. Full outboxes reject new observations; only an archive ACK deletes.
const OutboxCapacity = 100000

var ErrArchiveOnly = errors.New("history is available at the System archive")

const outboxSchema = `CREATE TABLE IF NOT EXISTS outbox(seq INTEGER PRIMARY KEY,event_id TEXT NOT NULL UNIQUE,payload TEXT NOT NULL);`

const eventSchema = `
CREATE TABLE IF NOT EXISTS radio_events(id TEXT PRIMARY KEY,at TEXT NOT NULL,received_at TEXT NOT NULL,origin TEXT NOT NULL,site_id TEXT NOT NULL,channel_id TEXT NOT NULL,radio_id INTEGER NOT NULL,talkgroup_id INTEGER NOT NULL,call_id TEXT NOT NULL,type TEXT NOT NULL,label TEXT NOT NULL,message TEXT NOT NULL,audience TEXT NOT NULL,severity TEXT NOT NULL,outcome TEXT NOT NULL,source_kind TEXT NOT NULL,details TEXT NOT NULL);
CREATE INDEX IF NOT EXISTS event_radio ON radio_events(radio_id,at);
CREATE INDEX IF NOT EXISTS event_group ON radio_events(talkgroup_id,at);
CREATE INDEX IF NOT EXISTS event_order ON radio_events(at,id);
CREATE INDEX IF NOT EXISTS event_site_order ON radio_events(site_id,at,id);
`

// Open opens a producer's durable, bounded upload outbox.
func Open(path string) (*Store, error) { return open(path, false, historypriority.Acquire) }

func open(path string, archive bool, acquire func(context.Context) (*historypriority.Turn, error)) (*Store, error) {
	absolutePath, err := filepath.Abs(path)
	if err != nil {
		return nil, err
	}
	// Keep a bounded cleanup batch's dirty index pages in memory rather than
	// repeatedly spilling and syncing them mid-statement on Site storage.
	// DSN pragmas also apply if database/sql replaces a canceled connection.
	dsn := url.URL{Scheme: "file", Path: absolutePath}
	dsn.RawQuery = url.Values{"_pragma": {"busy_timeout(5000)", "cache_size(-65536)", "wal_autocheckpoint(128)"}}.Encode()
	db, err := sql.Open("sqlite", dsn.String())
	if err != nil {
		return nil, err
	}
	db.SetMaxOpenConns(1)
	// SQL initialization and final close/checkpoints belong to the same
	// dedicated low-priority worker as runtime writes. sql.Open is lazy; the
	// controller thread opens no SQLite connection and changes no priority.
	readDSN := url.URL{Scheme: "file", Path: absolutePath}
	readDSN.RawQuery = url.Values{"mode": {"ro"}, "_pragma": {"busy_timeout(5000)"}}.Encode()
	readDB, err := sql.Open("sqlite", readDSN.String())
	if err != nil {
		_ = db.Close()
		return nil, err
	}
	s := &Store{db: db, readDB: readDB, queue: make(chan write, 1024), stop: make(chan struct{}), ready: make(chan struct{}), archive: archive, acquire: acquire, ingestion: ingestionCounters{startedAt: time.Now().UTC()}}
	s.workerCtx, s.workerCancel = context.WithCancel(context.Background())
	s.cleanupWake = make(chan struct{}, 1)
	s.commands = make(chan write, 8)
	s.uploadQueue = make(chan write, 4)
	initialize := func() error {
		if err := os.MkdirAll(filepath.Dir(absolutePath), 0700); err != nil {
			return err
		}
		f, err := os.OpenFile(absolutePath, os.O_CREATE|os.O_RDWR, 0600)
		if err != nil {
			return err
		}
		err = errors.Join(f.Chmod(0600), f.Close())
		if err != nil {
			return err
		}
		_, err = db.Exec("PRAGMA journal_mode=WAL")
		if err != nil {
			return err
		}
		if archive {
			if _, err = db.Exec(eventSchema); err != nil {
				return err
			}
			if err = historySchema(db); err != nil {
				return err
			}
			policy, err := scanRetention(db.QueryRow(retentionQuery))
			if err != nil {
				return err
			}
			s.retention.Store(&policy)
			return nil
		}
		if _, err = db.Exec(outboxSchema); err != nil {
			return err
		}
		var count int64
		if err = db.QueryRow("SELECT COUNT(*) FROM outbox").Scan(&count); err != nil {
			return err
		}
		if count > OutboxCapacity {
			return errors.New("durable outbox exceeds its capacity")
		}
		oldest, err := outboxOldest(db.QueryRow("SELECT payload FROM outbox ORDER BY seq LIMIT 1"))
		if err == nil {
			s.outboxPending.Store(count)
			s.outboxOldest.Store(oldest)
		}
		return err
	}
	// WAL readers remain independent of the mutation scheduler and SQL writer.
	readDB.SetMaxOpenConns(1)
	s.wg.Add(1)
	go s.writer(initialize)
	return s, nil
}
func NewID() string { v := make([]byte, 16); _, _ = rand.Read(v); return hex.EncodeToString(v) }

// OpenArchive opens the final central destination; it has no upstream queue.
func OpenArchive(path string) (*Store, error) { return open(path, true, historypriority.Acquire) }
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
	if s.initErr != nil {
		s.ingestion.queueDropped.Add(1)
		return false
	}
	received := e.ReceivedAt
	e = normalize(e)
	if !s.archive && !received.IsZero() {
		e.ReceivedAt = received
	}
	select {
	case s.queue <- write{event: e}:
		return true
	default:
		s.ingestion.queueDropped.Add(1)
		return false
	}
}
func (s *Store) writer(initialize func() error) {
	defer s.wg.Done()
	if err := historypriority.Enter(); err != nil {
		log.Printf("history writer scheduling: %v", err)
	}
	// Store startup is asynchronous. History can wait for storage without
	// holding controller/RF startup; Close cancels a pending admission.
	initialTurn, err := s.acquire(s.workerCtx)
	if err != nil {
		// No SQLite connection exists before the first successful gate turn.
		s.closeErr = errors.Join(s.readDB.Close(), s.db.Close())
		s.finishInitialization(err)
		return
	}
	if err = initialize(); err != nil {
		s.closeErr = errors.Join(s.readDB.Close(), s.db.Close())
		s.finishInitialization(err)
		if releaseErr := initialTurn.Release(); releaseErr != nil {
			s.closeErr = errors.Join(s.closeErr, releaseErr)
		}
		return
	}
	s.finishInitialization(nil)
	if err = initialTurn.Release(); err != nil {
		s.observeMaintenance(err)
	}
	defer func() {
		turn, err := s.acquire(context.Background())
		if err != nil {
			// Closing an active WAL connection can checkpoint. Never bypass a
			// refused scheduler turn; committed WAL remains durable until process
			// exit, which closes descriptors without a SQLite checkpoint.
			s.closeErr = err
			return
		}
		s.closeErr = errors.Join(s.readDB.Close(), s.db.Close())
		s.closeErr = errors.Join(s.closeErr, turn.Release())
	}()
	var pending *write
	stopping := false
	timer := time.NewTimer(time.Minute)
	defer timer.Stop()
	if !s.archive {
		timer.Stop()
	}
	// A fixed host-wide quiet interval follows the durable result. A slow SQL
	// operation must not reserve another equal stretch of idle storage time.
	mutate := func(ctx context.Context, action func() error, result func(error)) {
		turn, err := s.acquire(ctx)
		if err != nil {
			if ctx.Err() == nil {
				s.observeMaintenance(err)
			}
			result(err)
			return
		}
		if err = ctx.Err(); err == nil {
			err = action()
		}
		result(err)
		if err = turn.Release(); err != nil {
			s.observeMaintenance(err)
		}
	}
	var nextCleanup time.Time
	reset := func(d time.Duration) {
		if !timer.Stop() {
			select {
			case <-timer.C:
			default:
			}
		}
		timer.Reset(d)
	}
	clean := func() {
		if wait := time.Until(nextCleanup); wait > 0 {
			reset(wait)
			return
		}
		more := false
		mutate(s.workerCtx, func() error {
			more, _ = s.cleanupBatch(time.Now().UTC())
			return nil
		}, func(err error) {
			if err != nil && s.workerCtx.Err() == nil {
				s.cleanupMu.Lock()
				at := time.Now().UTC()
				s.cleanup.Pending = true
				s.cleanup.LastError = "History cleanup could not obtain its background storage turn."
				s.cleanup.LastFailureAt = &at
				s.cleanupMu.Unlock()
				more = true
			}
		})
		delay := cleanupPause
		if s.CleanupStatus().LastError != "" {
			delay = max(delay, 5*time.Second)
		}
		nextCleanup = time.Now().Add(delay)
		if more {
			reset(delay)
		} else {
			reset(time.Minute)
		}
	}
	collectCaptures := func(w write) *write {
		w.events = []Event{w.event}
	collect:
		for len(w.events) < EventBatchLimit {
			select {
			case next := <-s.queue:
				if next.barrier != nil {
					pending = &next
					break collect
				}
				w.events = append(w.events, next.event)
			default:
				break collect
			}
		}
		return &w
	}
	for {
		var w write
		if pending != nil {
			w = *pending
			pending = nil
		} else if stopping {
			select {
			case w = <-s.queue:
			case w = <-s.uploadQueue:
			case w = <-s.commands:
			default:
				return
			}
		} else {
			select {
			case w = <-s.queue:
			case w = <-s.uploadQueue:
			case w = <-s.commands:
			case <-timer.C:
				s.requestCleanup()
				clean()
				continue
			case <-s.cleanupWake:
				clean()
				continue
			case <-s.stop:
				stopping = true
				continue
			}
		}
		if w.barrier != nil {
			close(w.barrier)
			continue
		}
		if w.pruned != nil {
			var removed Removed
			mutate(w.ctx, func() (err error) {
				removed, err = s.prune(w.ctx, *w.pruneAt)
				return err
			}, func(err error) { w.pruned <- pruneResult{removed, err} })
			continue
		}
		if w.retention != nil {
			mutate(w.ctx, func() error {
				err := s.saveRetention(w.ctx, *w.retention)
				if err == nil {
					_ = s.RequestCleanup()
				}
				return err
			}, func(err error) { w.result <- err })
			continue
		}
		admission := w.ctx
		if admission == nil {
			// Captures accepted into the bounded queue drain on Close, even
			// after cleanup's service-lifetime context has been cancelled.
			admission = context.Background()
		}
		actionRan := false
		mutate(admission, func() error {
			actionRan = true
			var observation, acknowledgment *write
			if w.ackIDs != nil {
				acknowledgment = &w
				// An ACK can share the next already queued observation batch;
				// never wait for more work or enlarge its 32-event contract.
				select {
				case next := <-s.queue:
					if next.barrier != nil {
						pending = &next
					} else {
						observation = collectCaptures(next)
					}
				default:
				}
			} else {
				if w.result == nil {
					observation = collectCaptures(w)
				} else {
					observation = &w
				}
				if !s.archive && pending == nil {
					select {
					case next := <-s.commands:
						if next.ackIDs != nil {
							acknowledgment = &next
						} else {
							pending = &next
						}
					default:
					}
				}
			}
			s.persist(observation, acknowledgment)
			return nil
		}, func(err error) {
			if actionRan || err == nil {
				return
			}
			if w.result != nil {
				w.result <- err
			} else {
				s.ingestion.writeFailures.Add(1)
				s.ingestion.lastWriteFailure.Store(time.Now().UTC().UnixNano())
			}
		})
	}
}

// Append accepts one immutable upload batch. Only the background writer touches
// SQLite; callers await its durable result, while RF capture uses Enqueue.
func (s *Store) Append(ctx context.Context, events ...Event) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	if !s.archive {
		return ErrArchiveOnly
	}
	if len(events) == 0 {
		return nil
	}
	if len(events) > EventBatchLimit {
		s.ingestion.rejected.Add(uint64(len(events)))
		return errors.New("activity batch exceeds the current observation limit")
	}
	for _, e := range events {
		if e.ID == "" {
			s.ingestion.rejected.Add(uint64(len(events)))
			return errors.New("an archived observation requires its producer event ID")
		}
	}
	result := make(chan error, 1)
	s.mu.RLock()
	if s.closed {
		s.mu.RUnlock()
		s.ingestion.writeFailures.Add(uint64(len(events)))
		s.ingestion.lastWriteFailure.Store(time.Now().UTC().UnixNano())
		return errors.New("activity store is closed")
	}
	if s.initErr != nil {
		s.mu.RUnlock()
		return ErrUnavailable
	}
	select {
	case s.uploadQueue <- write{events: events, ctx: ctx, result: result}:
		s.mu.RUnlock()
	default:
		s.mu.RUnlock()
		return errors.New("activity capture queue is busy; retry retained observations")
	}
	select {
	case err := <-result:
		return err
	case <-ctx.Done():
		return ctx.Err()
	}
}

// persist replaces separate capture, inbound and ACK transactions. One admitted
// turn contains at most one observation batch and one acknowledgment batch.
// Once admitted, the Store owns the transaction; an abandoned request can retry
// its retained IDs, but cannot cancel another participant's durable work.
func (s *Store) persist(observation, acknowledgment *write) (err error) {
	skipCanceled := func(w **write) {
		if *w != nil && (*w).ctx != nil {
			if err := (*w).ctx.Err(); err != nil {
				(*w).result <- err
				*w = nil
			}
		}
	}
	skipCanceled(&observation)
	skipCanceled(&acknowledgment)
	complete := func(err error) {
		for _, w := range []*write{observation, acknowledgment} {
			if w != nil && w.result != nil {
				w.result <- err
			}
		}
	}
	var events []Event
	if observation != nil {
		events = observation.events
	}
	normalized := make([]Event, 0, len(events))
	detailJSON := make([]string, 0, len(events))
	payloadJSON := make([]string, 0, len(events))
	for _, e := range events {
		received := e.ReceivedAt
		e = normalize(e)
		if !s.archive && !received.IsZero() {
			e.ReceivedAt = received
		}
		if len(e.ID) > 128 || e.Origin == "" || len(e.Origin) > 128 || e.Type == "" || len(e.Type) > 128 || len(e.SiteID) > 128 || len(e.ChannelID) > 128 || len(e.CallID) > 128 || len(e.Label) > 256 || len(e.SourceKind) > 32 || len(e.Outcome) > 256 || len(e.Message) > 2000 || e.RadioID < 0 || e.RadioID > 0xFFFFFE || e.TalkgroupID < 0 || e.TalkgroupID > 0xFFFF {
			s.ingestion.rejected.Add(uint64(len(events)))
			err = errors.New("invalid activity observation")
			complete(err)
			return err
		}
		if e.Audience != "dispatcher" && e.Audience != "operator" && e.Audience != "diagnostic" && e.Audience != "security" {
			s.ingestion.rejected.Add(uint64(len(events)))
			err = errors.New("invalid activity audience")
			complete(err)
			return err
		}
		details, err := json.Marshal(e.Details)
		if err != nil || len(details) > 16384 {
			s.ingestion.rejected.Add(uint64(len(events)))
			err = errors.New("invalid activity details")
			complete(err)
			return err
		}
		normalized = append(normalized, e)
		detailJSON = append(detailJSON, string(details))
		if !s.archive {
			payload, encodeErr := json.Marshal(e)
			if encodeErr != nil {
				s.ingestion.rejected.Add(uint64(len(events)))
				err = errors.New("invalid activity observation")
				complete(err)
				return err
			}
			payloadJSON = append(payloadJSON, string(payload))
		}
	}
	// Validation and scheduling can outlive a queued request. Check each
	// participant again immediately before opening the shared transaction.
	skipCanceled(&observation)
	skipCanceled(&acknowledgment)
	if observation == nil {
		events, normalized, detailJSON = nil, nil, nil
		payloadJSON = nil
	}
	if observation == nil && acknowledgment == nil {
		return nil
	}
	committed := false
	defer func() {
		if !committed && err != nil {
			if observation != nil {
				s.ingestion.writeFailures.Add(uint64(len(events)))
				s.ingestion.lastWriteFailure.Store(time.Now().UTC().UnixNano())
			}
			if acknowledgment != nil {
				s.observeMaintenance(err)
			}
		}
		complete(err)
	}()
	connection, err := s.db.Conn(context.Background())
	if err != nil {
		return err
	}
	defer connection.Close()
	// Acquiring the sole writer connection can also queue behind an explicit
	// offline/test holder. Cancellation still wins until SQL actually starts.
	skipCanceled(&observation)
	skipCanceled(&acknowledgment)
	if observation == nil {
		events, normalized, detailJSON = nil, nil, nil
		payloadJSON = nil
	}
	if observation == nil && acknowledgment == nil {
		return nil
	}
	tx, err := connection.BeginTx(context.Background(), nil)
	if err != nil {
		return err
	}
	var removed Removed
	var cleanupAt time.Time
	if s.archive {
		cleanupAt = time.Now().UTC()
		s.requestCleanup()
		generation := s.beginCleanup(cleanupAt)
		defer func() {
			// Rollback runs first. Deletions never become progress unless the
			// same transaction also durably committed the incoming observations.
			_, _ = s.finishCleanup(generation, removed, err)
		}()
		// Prune existing facts before accepting this batch. Incoming old IDs
		// must exist durably at least once before a later retention turn can
		// remove them; an ACK never describes insert-and-delete in one commit.
	}
	defer tx.Rollback()
	if s.archive {
		removed, err = s.pruneTransaction(context.Background(), tx, cleanupAt)
	}
	if err != nil {
		return err
	}
	pending := s.outboxPending.Load()
	if acknowledgment != nil {
		ids := acknowledgment.ackIDs
		args := make([]any, len(ids))
		for i, id := range ids {
			args[i] = id
		}
		result, err := tx.Exec("DELETE FROM outbox WHERE event_id IN ("+strings.TrimSuffix(strings.Repeat("?,", len(ids)), ",")+")", args...)
		if err != nil {
			return err
		}
		deleted, err := result.RowsAffected()
		if err != nil {
			return err
		}
		pending -= deleted
	}
	inserted := uint64(0)
	dropped := uint64(0)
	for i, e := range normalized {
		if !s.archive {
			if pending >= OutboxCapacity {
				var exists int
				err := tx.QueryRow("SELECT 1 FROM outbox WHERE event_id=?", e.ID).Scan(&exists)
				if errors.Is(err, sql.ErrNoRows) {
					// Durable accepted rows survive capacity pressure. Only this
					// new observation is dropped, without delaying its RF producer.
					dropped++
					continue
				}
				if err != nil {
					return err
				}
				continue
			}
			result, err := tx.Exec("INSERT OR IGNORE INTO outbox(event_id,payload) VALUES(?,?)", e.ID, payloadJSON[i])
			if err != nil {
				return err
			}
			n, err := result.RowsAffected()
			if err != nil {
				return err
			}
			pending += n
			inserted += uint64(n)
			continue
		}
		r, err := tx.Exec("INSERT OR IGNORE INTO radio_events VALUES(?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?)", e.ID, e.At.UTC().Format(stampFormat), e.ReceivedAt.UTC().Format(stampFormat), e.Origin, e.SiteID, e.ChannelID, e.RadioID, e.TalkgroupID, e.CallID, e.Type, e.Label, e.Message, e.Audience, e.Severity, e.Outcome, e.SourceKind, detailJSON[i])
		if err != nil {
			return err
		}
		n, _ := r.RowsAffected()
		if n == 0 {
			continue
		}
		inserted++
		if err = projectHistory(tx, e); err != nil {
			return err
		}

	}
	var oldest int64
	if !s.archive {
		oldest, err = outboxOldest(tx.QueryRow("SELECT payload FROM outbox ORDER BY seq LIMIT 1"))
		if err != nil {
			return err
		}
	}
	if err = tx.Commit(); err != nil {
		return err
	}
	committed = true
	if !s.archive {
		s.outboxPending.Store(pending)
		s.outboxOldest.Store(oldest)
		s.ingestion.outboxDropped.Add(dropped)
	}
	if observation != nil && (s.archive || inserted > 0) {
		s.ingestion.lastWriteSuccess.Store(time.Now().UTC().UnixNano())
	}
	if s.archive && inserted > 0 {
		// These newly durable facts were deliberately outside this turn's
		// pre-insert cleanup. Keep verification pending for a later ordinary
		// archive turn or the existing periodic/explicit cleanup job.
		s.requestCleanup()
	}
	return nil
}

const eventColumns = "id,at,received_at,origin,site_id,channel_id,radio_id,talkgroup_id,call_id,type,label,message,audience,severity,outcome,source_kind,details"

func (s *Store) Events(q Query) (_ []Event, err error) {
	if err := s.InitializationError(); err != nil {
		return nil, err
	}
	if !s.archive {
		return nil, ErrArchiveOnly
	}
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
	rows, err := s.readDB.QueryContext(ctx, "SELECT "+eventColumns+" FROM radio_events WHERE "+strings.Join(where, " AND ")+" ORDER BY at DESC,id DESC LIMIT ?", args...)
	if err != nil {
		return nil, err
	}
	return scanEvents(rows)
}

func scanEvents(rows *sql.Rows) ([]Event, error) {
	defer rows.Close()
	out := []Event{}
	for rows.Next() {
		var e Event
		var at, received, details string
		if err := rows.Scan(&e.ID, &at, &received, &e.Origin, &e.SiteID, &e.ChannelID, &e.RadioID, &e.TalkgroupID, &e.CallID, &e.Type, &e.Label, &e.Message, &e.Audience, &e.Severity, &e.Outcome, &e.SourceKind, &details); err != nil {
			return nil, err
		}
		e.At, _ = time.Parse(time.RFC3339Nano, at)
		e.ReceivedAt, _ = time.Parse(time.RFC3339Nano, received)
		_ = json.Unmarshal([]byte(details), &e.Details)
		out = append(out, e)
	}
	return out, rows.Err()
}
func (s *Store) Calls() ([]Call, error) {
	page, err := s.CallHistory(Query{Limit: 200})
	return page.Calls, err
}

// Pending reads one current transfer batch without per-event database queries.
func (s *Store) Pending(ctx context.Context, limit int) (_ []Event, err error) {
	if err := s.InitializationError(); err != nil {
		return nil, err
	}
	if s.archive {
		return nil, nil
	}
	defer func() { err = contextSQLError(ctx, err) }()
	if limit < 1 || limit > EventBatchLimit {
		limit = EventBatchLimit
	}
	rows, err := s.readDB.QueryContext(ctx, "SELECT payload FROM outbox ORDER BY seq LIMIT ?", limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	events := []Event{}
	for rows.Next() {
		var payload string
		if err := rows.Scan(&payload); err != nil {
			return nil, err
		}
		var e Event
		if err := json.Unmarshal([]byte(payload), &e); err != nil {
			return nil, err
		}
		events = append(events, e)
	}
	return events, rows.Err()
}
func (s *Store) Acknowledge(ctx context.Context, ids []string) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	if s.archive {
		return errors.New("the System archive has no upload outbox")
	}
	if len(ids) == 0 {
		return ctx.Err()
	}
	if len(ids) > EventBatchLimit {
		return errors.New("activity acknowledgment exceeds the current observation limit")
	}
	return s.command(ctx, write{ackIDs: append([]string{}, ids...)})
}

func outboxOldest(row *sql.Row) (int64, error) {
	var payload string
	if err := row.Scan(&payload); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return 0, nil
		}
		return 0, err
	}
	var e Event
	if err := json.Unmarshal([]byte(payload), &e); err != nil {
		return 0, err
	}
	if e.ReceivedAt.IsZero() {
		return 0, errors.New("an outbox observation has no producer receipt time")
	}
	return e.ReceivedAt.UnixNano(), nil
}
func (s *Store) Flush(ctx context.Context) error {
	s.mu.RLock()
	if s.closed {
		s.mu.RUnlock()
		return errors.New("activity store is closed")
	}
	if s.initErr != nil {
		s.mu.RUnlock()
		return ErrUnavailable
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
		return s.InitializationError()
	case <-ctx.Done():
		return ctx.Err()
	}
}
func (s *Store) Close() error {
	s.once.Do(func() {
		s.mu.Lock()
		s.closed = true
		close(s.stop)
		s.workerCancel()
		s.mu.Unlock()
		s.wg.Wait()
	})
	return s.closeErr
}
