// Package messaging stores application conversations independently of radio and
// audio state. A successful write is committed before it is returned to callers.
package messaging

import (
	"context"
	"crypto/rand"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"net/url"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"time"

	_ "modernc.org/sqlite"
)

var (
	ErrForbidden = errors.New("this conversation is not available to this person")
	ErrNotFound  = errors.New("conversation not found")
	ErrConflict  = errors.New("message identifier was already used for different content")
	ErrInvalid   = errors.New("invalid messaging request")
)

type Conversation struct {
	ID             string    `json:"id"`
	Kind           string    `json:"kind"`
	Name           string    `json:"name"`
	Members        []string  `json:"members"`
	CreatedBy      string    `json:"created_by"`
	CreatedAt      time.Time `json:"created_at"`
	LastMessage    *Message  `json:"last_message,omitempty"`
	UnreadCount    int64     `json:"unread_count"`
	ReadThroughID  int64     `json:"read_through_id"`
	HistoryAfterID int64     `json:"history_after_id"`
}

type Message struct {
	ID             int64     `json:"id"`
	ConversationID string    `json:"conversation_id"`
	ClientID       string    `json:"client_id"`
	SenderID       string    `json:"sender_id"`
	SenderName     string    `json:"sender_name"`
	Body           string    `json:"body"`
	CreatedAt      time.Time `json:"created_at"`
}

type MessagePage struct {
	Conversation Conversation `json:"thread"`
	Messages     []Message    `json:"messages"`
	NextBeforeID int64        `json:"next_before_id,omitempty"`
}

type Store struct{ db *sql.DB }

func Open(path string) (*Store, error) {
	if path == "" {
		return nil, fmt.Errorf("%w: a messaging database path is required", ErrInvalid)
	}
	if err := os.MkdirAll(filepath.Dir(path), 0700); err != nil {
		return nil, err
	}
	if info, err := os.Lstat(path); err == nil && !info.Mode().IsRegular() {
		return nil, errors.New("messaging database path must be a regular file")
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
	absolute, err := filepath.Abs(path)
	if err != nil {
		return nil, err
	}
	db, err := sql.Open("sqlite", (&url.URL{Scheme: "file", Path: absolute}).String())
	if err != nil {
		return nil, err
	}
	db.SetMaxOpenConns(1)
	_, err = db.Exec(`PRAGMA journal_mode=WAL; PRAGMA busy_timeout=5000; PRAGMA synchronous=FULL; PRAGMA foreign_keys=ON;
CREATE TABLE IF NOT EXISTS conversations(id TEXT PRIMARY KEY,kind TEXT NOT NULL,name TEXT NOT NULL,direct_key TEXT UNIQUE,created_by TEXT NOT NULL,created_at TEXT NOT NULL);
CREATE TABLE IF NOT EXISTS conversation_members(conversation_id TEXT NOT NULL REFERENCES conversations(id),user_id TEXT NOT NULL,join_after_id INTEGER NOT NULL DEFAULT 0,last_read_id INTEGER NOT NULL DEFAULT 0,PRIMARY KEY(conversation_id,user_id));
CREATE INDEX IF NOT EXISTS member_user ON conversation_members(user_id,conversation_id);
CREATE TABLE IF NOT EXISTS messages(id INTEGER PRIMARY KEY AUTOINCREMENT,conversation_id TEXT NOT NULL REFERENCES conversations(id),client_id TEXT NOT NULL,sender_id TEXT NOT NULL,sender_name TEXT NOT NULL,body TEXT NOT NULL,created_at TEXT NOT NULL,UNIQUE(sender_id,client_id));
CREATE INDEX IF NOT EXISTS message_conversation ON messages(conversation_id,id);`)
	if err != nil {
		db.Close()
		return nil, err
	}
	return &Store{db: db}, nil
}

func (s *Store) Close() error { return s.db.Close() }

func newID() (string, error) {
	value := make([]byte, 16)
	if _, err := rand.Read(value); err != nil {
		return "", err
	}
	return hex.EncodeToString(value), nil
}

func members(actorID string, selected []string) ([]string, error) {
	if actorID == "" {
		return nil, ErrInvalid
	}
	value := append([]string{actorID}, selected...)
	for _, user := range value {
		if user == "" {
			return nil, ErrInvalid
		}
	}
	slices.Sort(value)
	return slices.Compact(value), nil
}

func (s *Store) CreateDirect(ctx context.Context, actorID, peerID string) (Conversation, error) {
	participants, err := members(actorID, []string{peerID})
	if err != nil {
		return Conversation{}, err
	}
	key, err := json.Marshal(participants)
	if err != nil {
		return Conversation{}, err
	}
	return s.create(ctx, actorID, "direct", "", participants, string(key))
}

// CreateGroup records its initial membership. The caller resolves these person
// IDs against the authenticated application directory before creation.
func (s *Store) CreateGroup(ctx context.Context, actorID, name string, selected []string) (Conversation, error) {
	participants, err := members(actorID, selected)
	if err != nil {
		return Conversation{}, err
	}
	return s.create(ctx, actorID, "group", name, participants, nil)
}

func (s *Store) create(ctx context.Context, actorID, kind, name string, participants []string, directKey any) (Conversation, error) {
	id, err := newID()
	if err != nil {
		return Conversation{}, err
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return Conversation{}, err
	}
	defer tx.Rollback()
	createdAt := time.Now().UTC().Format(time.RFC3339Nano)
	result, err := tx.ExecContext(ctx, "INSERT INTO conversations(id,kind,name,direct_key,created_by,created_at) VALUES(?,?,?,?,?,?) ON CONFLICT(direct_key) DO NOTHING", id, kind, name, directKey, actorID, createdAt)
	if err != nil {
		return Conversation{}, err
	}
	changed, err := result.RowsAffected()
	if err != nil {
		return Conversation{}, err
	}
	if changed == 0 {
		if err = tx.QueryRowContext(ctx, "SELECT id FROM conversations WHERE direct_key=?", directKey).Scan(&id); err != nil {
			return Conversation{}, err
		}
	} else {
		for _, user := range participants {
			if _, err = tx.ExecContext(ctx, "INSERT INTO conversation_members(conversation_id,user_id) VALUES(?,?)", id, user); err != nil {
				return Conversation{}, err
			}
		}
	}
	conversation, err := loadConversation(ctx, tx, actorID, id)
	if err != nil {
		return Conversation{}, err
	}
	if err = tx.Commit(); err != nil {
		return Conversation{}, err
	}
	return conversation, nil
}

func membership(ctx context.Context, tx *sql.Tx, userID, conversationID string) (int64, error) {
	var found int
	err := tx.QueryRowContext(ctx, "SELECT 1 FROM conversations WHERE id=?", conversationID).Scan(&found)
	if errors.Is(err, sql.ErrNoRows) {
		return 0, ErrNotFound
	}
	if err != nil {
		return 0, err
	}
	var joinedAfter int64
	err = tx.QueryRowContext(ctx, "SELECT join_after_id FROM conversation_members WHERE conversation_id=? AND user_id=?", conversationID, userID).Scan(&joinedAfter)
	if errors.Is(err, sql.ErrNoRows) {
		return 0, ErrForbidden
	}
	return joinedAfter, err
}

const conversationQuery = `SELECT c.id,c.kind,c.name,c.created_by,c.created_at,member.last_read_id,member.join_after_id,
(SELECT COUNT(*) FROM messages unseen WHERE unseen.conversation_id=c.id AND unseen.id>member.join_after_id AND unseen.id>member.last_read_id AND unseen.sender_id<>member.user_id),
msg.id,msg.client_id,msg.sender_id,msg.sender_name,msg.body,msg.created_at
FROM conversation_members member JOIN conversations c ON c.id=member.conversation_id
LEFT JOIN messages msg ON msg.id=(SELECT MAX(latest.id) FROM messages latest WHERE latest.conversation_id=c.id AND latest.id>member.join_after_id)
WHERE member.user_id=?`

type scanner interface{ Scan(...any) error }

func scanConversation(row scanner) (Conversation, error) {
	var value Conversation
	var created string
	var messageID sql.NullInt64
	var clientID, senderID, senderName, body, sentAt sql.NullString
	err := row.Scan(&value.ID, &value.Kind, &value.Name, &value.CreatedBy, &created, &value.ReadThroughID, &value.HistoryAfterID, &value.UnreadCount, &messageID, &clientID, &senderID, &senderName, &body, &sentAt)
	if err != nil {
		return Conversation{}, err
	}
	value.CreatedAt, err = time.Parse(time.RFC3339Nano, created)
	if err != nil {
		return Conversation{}, err
	}
	if messageID.Valid {
		at, err := time.Parse(time.RFC3339Nano, sentAt.String)
		if err != nil {
			return Conversation{}, err
		}
		value.LastMessage = &Message{ID: messageID.Int64, ConversationID: value.ID, ClientID: clientID.String, SenderID: senderID.String, SenderName: senderName.String, Body: body.String, CreatedAt: at}
	}
	return value, nil
}

func loadMembers(ctx context.Context, tx *sql.Tx, conversationID string) ([]string, error) {
	rows, err := tx.QueryContext(ctx, "SELECT user_id FROM conversation_members WHERE conversation_id=? ORDER BY user_id", conversationID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	value := []string{}
	for rows.Next() {
		var id string
		if err = rows.Scan(&id); err != nil {
			return nil, err
		}
		value = append(value, id)
	}
	return value, rows.Err()
}

func loadConversation(ctx context.Context, tx *sql.Tx, userID, conversationID string) (Conversation, error) {
	if _, err := membership(ctx, tx, userID, conversationID); err != nil {
		return Conversation{}, err
	}
	value, err := scanConversation(tx.QueryRowContext(ctx, conversationQuery+" AND c.id=?", userID, conversationID))
	if err != nil {
		return Conversation{}, err
	}
	value.Members, err = loadMembers(ctx, tx, conversationID)
	return value, err
}

func (s *Store) Conversation(ctx context.Context, userID, conversationID string) (Conversation, error) {
	tx, err := s.db.BeginTx(ctx, &sql.TxOptions{ReadOnly: true})
	if err != nil {
		return Conversation{}, err
	}
	defer tx.Rollback()
	value, err := loadConversation(ctx, tx, userID, conversationID)
	if err != nil {
		return Conversation{}, err
	}
	if err = tx.Commit(); err != nil {
		return Conversation{}, err
	}
	return value, nil
}

func (s *Store) Conversations(ctx context.Context, userID string) ([]Conversation, error) {
	tx, err := s.db.BeginTx(ctx, &sql.TxOptions{ReadOnly: true})
	if err != nil {
		return nil, err
	}
	defer tx.Rollback()
	rows, err := tx.QueryContext(ctx, conversationQuery+" ORDER BY COALESCE(msg.id,0) DESC,c.created_at DESC,c.id", userID)
	if err != nil {
		return nil, err
	}
	values := []Conversation{}
	for rows.Next() {
		value, e := scanConversation(rows)
		if e != nil {
			rows.Close()
			return nil, e
		}
		values = append(values, value)
	}
	err = rows.Err()
	rows.Close()
	if err != nil {
		return nil, err
	}
	for i := range values {
		values[i].Members, err = loadMembers(ctx, tx, values[i].ID)
		if err != nil {
			return nil, err
		}
	}
	if err = tx.Commit(); err != nil {
		return nil, err
	}
	return values, nil
}

const messageFields = "id,conversation_id,client_id,sender_id,sender_name,body,created_at"

func scanMessage(row scanner) (Message, error) {
	var value Message
	var created string
	if err := row.Scan(&value.ID, &value.ConversationID, &value.ClientID, &value.SenderID, &value.SenderName, &value.Body, &created); err != nil {
		return Message{}, err
	}
	var err error
	value.CreatedAt, err = time.Parse(time.RFC3339Nano, created)
	return value, err
}

func (s *Store) Send(ctx context.Context, userID, username, conversationID, clientID, body string) (Message, []string, error) {
	if userID == "" || conversationID == "" || clientID == "" || strings.TrimSpace(body) == "" {
		return Message{}, nil, ErrInvalid
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return Message{}, nil, err
	}
	defer tx.Rollback()
	// Membership gates the insert inside the first write. This also acquires the
	// writer before a read snapshot, including across separate Store connections.
	_, err = tx.ExecContext(ctx, "INSERT INTO messages(conversation_id,client_id,sender_id,sender_name,body,created_at) SELECT ?,?,?,?,?,? WHERE EXISTS(SELECT 1 FROM conversation_members WHERE conversation_id=? AND user_id=?) ON CONFLICT(sender_id,client_id) DO NOTHING", conversationID, clientID, userID, username, body, time.Now().UTC().Format(time.RFC3339Nano), conversationID, userID)
	if err != nil {
		return Message{}, nil, err
	}
	joinedAfter, err := membership(ctx, tx, userID, conversationID)
	if err != nil {
		return Message{}, nil, err
	}
	value, err := scanMessage(tx.QueryRowContext(ctx, "SELECT "+messageFields+" FROM messages WHERE sender_id=? AND client_id=?", userID, clientID))
	if err != nil {
		return Message{}, nil, err
	}
	if value.ConversationID != conversationID || value.Body != body || value.ID <= joinedAfter {
		return Message{}, nil, ErrConflict
	}
	recipients, err := loadMembers(ctx, tx, conversationID)
	if err != nil {
		return Message{}, nil, err
	}
	if err = tx.Commit(); err != nil {
		return Message{}, nil, err
	}
	return value, recipients, nil
}

// Messages returns the membership boundary, preview and page from one snapshot.
// A concurrent remove/rejoin cannot combine an earlier preview with a later page.
func (s *Store) Messages(ctx context.Context, userID, conversationID string, beforeID int64, limit int) (MessagePage, error) {
	if beforeID < 0 || limit < 1 {
		return MessagePage{}, ErrInvalid
	}
	tx, err := s.db.BeginTx(ctx, &sql.TxOptions{ReadOnly: true})
	if err != nil {
		return MessagePage{}, err
	}
	defer tx.Rollback()
	conversation, err := loadConversation(ctx, tx, userID, conversationID)
	if err != nil {
		return MessagePage{}, err
	}
	joinedAfter := conversation.HistoryAfterID
	query := "SELECT " + messageFields + " FROM messages WHERE conversation_id=? AND id>?"
	args := []any{conversationID, joinedAfter}
	if beforeID > 0 {
		if beforeID <= joinedAfter {
			return MessagePage{}, ErrInvalid
		}
		var found int
		if err = tx.QueryRowContext(ctx, "SELECT 1 FROM messages WHERE conversation_id=? AND id=?", conversationID, beforeID).Scan(&found); errors.Is(err, sql.ErrNoRows) {
			return MessagePage{}, ErrInvalid
		} else if err != nil {
			return MessagePage{}, err
		}
		query += " AND id<?"
		args = append(args, beforeID)
	}
	query += " ORDER BY id DESC LIMIT ?"
	args = append(args, int64(limit)+1)
	rows, err := tx.QueryContext(ctx, query, args...)
	if err != nil {
		return MessagePage{}, err
	}
	page := MessagePage{Conversation: conversation, Messages: []Message{}}
	for rows.Next() {
		value, e := scanMessage(rows)
		if e != nil {
			rows.Close()
			return MessagePage{}, e
		}
		page.Messages = append(page.Messages, value)
	}
	err = rows.Err()
	rows.Close()
	if err != nil {
		return MessagePage{}, err
	}
	if len(page.Messages) > limit {
		page.Messages = page.Messages[:limit]
		page.NextBeforeID = page.Messages[len(page.Messages)-1].ID
	}
	slices.Reverse(page.Messages)
	if err = tx.Commit(); err != nil {
		return MessagePage{}, err
	}
	return page, nil
}

func (s *Store) MarkRead(ctx context.Context, userID, conversationID string, messageID int64) error {
	if messageID < 1 {
		return ErrInvalid
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	_, err = tx.ExecContext(ctx, "UPDATE conversation_members SET last_read_id=MAX(last_read_id,?) WHERE conversation_id=? AND user_id=? AND ?>join_after_id AND EXISTS(SELECT 1 FROM messages WHERE conversation_id=? AND id=?)", messageID, conversationID, userID, messageID, conversationID, messageID)
	if err != nil {
		return err
	}
	joinedAfter, err := membership(ctx, tx, userID, conversationID)
	if err != nil {
		return err
	}
	if messageID <= joinedAfter {
		return ErrInvalid
	}
	var found int
	err = tx.QueryRowContext(ctx, "SELECT 1 FROM messages WHERE conversation_id=? AND id=?", conversationID, messageID).Scan(&found)
	if errors.Is(err, sql.ErrNoRows) {
		return ErrInvalid
	}
	if err != nil {
		return err
	}
	return tx.Commit()
}

// SetMembers is available only to a private group's creator, who remains a
// member. Each addition starts a new history window; retained members keep their
// existing window and read position. The returned IDs include removed people so
// callers can notify every affected inbox after the transaction commits.
func (s *Store) SetMembers(ctx context.Context, actorID, conversationID string, selected []string) (Conversation, []string, error) {
	participants, err := members(actorID, selected)
	if err != nil {
		return Conversation{}, nil, err
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return Conversation{}, nil, err
	}
	defer tx.Rollback()
	// Acquire the writer before reading membership or the message boundary. A
	// concurrent send therefore commits completely before or after this change.
	if _, err = tx.ExecContext(ctx, "UPDATE conversations SET name=name WHERE id=?", conversationID); err != nil {
		return Conversation{}, nil, err
	}
	current, err := loadConversation(ctx, tx, actorID, conversationID)
	if err != nil {
		return Conversation{}, nil, err
	}
	if current.Kind != "group" {
		return Conversation{}, nil, ErrInvalid
	}
	if current.CreatedBy != actorID {
		return Conversation{}, nil, ErrForbidden
	}
	var boundary int64
	if err = tx.QueryRowContext(ctx, "SELECT COALESCE(MAX(id),0) FROM messages").Scan(&boundary); err != nil {
		return Conversation{}, nil, err
	}
	for _, id := range current.Members {
		if !slices.Contains(participants, id) {
			if _, err = tx.ExecContext(ctx, "DELETE FROM conversation_members WHERE conversation_id=? AND user_id=?", conversationID, id); err != nil {
				return Conversation{}, nil, err
			}
		}
	}
	for _, id := range participants {
		if !slices.Contains(current.Members, id) {
			if _, err = tx.ExecContext(ctx, "INSERT INTO conversation_members(conversation_id,user_id,join_after_id,last_read_id) VALUES(?,?,?,?)", conversationID, id, boundary, boundary); err != nil {
				return Conversation{}, nil, err
			}
		}
	}
	value, err := loadConversation(ctx, tx, actorID, conversationID)
	if err != nil {
		return Conversation{}, nil, err
	}
	if err = tx.Commit(); err != nil {
		return Conversation{}, nil, err
	}
	affected := append(slices.Clone(current.Members), participants...)
	slices.Sort(affected)
	return value, slices.Compact(affected), nil
}
