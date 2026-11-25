package main

import (
	"context"
	"database/sql"
	"fmt"
	"strings"
	"sync"
)

// MsgStore keeps chats and messages locally, since whatsmeow does not store them.
// The database lives in the data directory and never leaves the machine.
type MsgStore struct {
	db *sql.DB
	mu sync.Mutex
}

const schema = `
CREATE TABLE IF NOT EXISTS chats (
	jid       TEXT PRIMARY KEY,
	name      TEXT NOT NULL DEFAULT '',
	unread    INTEGER NOT NULL DEFAULT 0,
	last      TEXT NOT NULL DEFAULT '',
	last_kind TEXT NOT NULL DEFAULT '',
	ts        INTEGER NOT NULL DEFAULT 0
);
CREATE TABLE IF NOT EXISTS messages (
	chat   TEXT NOT NULL,
	id     TEXT NOT NULL,
	sender TEXT NOT NULL DEFAULT '',
	name   TEXT NOT NULL DEFAULT '',
	text   TEXT NOT NULL DEFAULT '',
	kind   TEXT NOT NULL DEFAULT '',
	ts     INTEGER NOT NULL DEFAULT 0,
	me     INTEGER NOT NULL DEFAULT 0,
	unread INTEGER NOT NULL DEFAULT 0,
	PRIMARY KEY (chat, id)
);
CREATE INDEX IF NOT EXISTS messages_chat_ts ON messages (chat, ts);
`

func OpenMsgStore(path string) (*MsgStore, error) {
	db, err := sql.Open("sqlite3", sqliteDSN(path, "_journal_mode=WAL&_busy_timeout=5000"))
	if err != nil {
		return nil, err
	}
	db.SetMaxOpenConns(1)
	if _, err := db.Exec(schema); err != nil {
		db.Close()
		return nil, fmt.Errorf("create schema: %w", err)
	}
	return &MsgStore{db: db}, nil
}

func (s *MsgStore) Close() error { return s.db.Close() }

// sqliteDSN builds a file: URI, escaping the characters that would end the path.
func sqliteDSN(path, params string) string {
	esc := strings.NewReplacer("%", "%25", "?", "%3F", "#", "%23").Replace(path)
	return "file:" + esc + "?" + params
}

// HasChat reports whether a chat exists.
func (s *MsgStore) HasChat(ctx context.Context, jid string) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	var n int
	_ = s.db.QueryRowContext(ctx, `SELECT COUNT(*) FROM chats WHERE jid = ?`, jid).Scan(&n)
	return n > 0
}

// RestoreUnread flags messages as unread again after read receipts failed.
func (s *MsgStore) RestoreUnread(ctx context.Context, jid string, ids []string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	for _, id := range ids {
		if _, err := tx.ExecContext(ctx, `UPDATE messages SET unread = 1 WHERE chat = ? AND id = ?`, jid, id); err != nil {
			return err
		}
	}
	if _, err := tx.ExecContext(ctx, `UPDATE chats SET unread = (
		SELECT COUNT(*) FROM messages WHERE chat = ? AND unread = 1) WHERE jid = ?`, jid, jid); err != nil {
		return err
	}
	return tx.Commit()
}

// AddMessage stores a message and updates the chat preview. When unread is true
// and the message is new, the chat unread counter is incremented.
// Returns false if the message was already stored.
func (s *MsgStore) AddMessage(ctx context.Context, m Message, unread bool) (bool, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return false, err
	}
	defer tx.Rollback()

	res, err := tx.ExecContext(ctx, `INSERT OR IGNORE INTO messages
		(chat, id, sender, name, text, kind, ts, me, unread) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		m.Chat, m.ID, m.From, m.Name, m.Text, m.Kind, m.TS, m.Me, unread)
	if err != nil {
		return false, err
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return false, nil
	}
	if _, err := tx.ExecContext(ctx, `INSERT OR IGNORE INTO chats (jid) VALUES (?)`, m.Chat); err != nil {
		return false, err
	}
	if _, err := tx.ExecContext(ctx, `UPDATE chats SET last = ?, last_kind = ?, ts = ?
		WHERE jid = ? AND ts <= ?`, m.Text, m.Kind, m.TS, m.Chat, m.TS); err != nil {
		return false, err
	}
	if unread {
		if _, err := tx.ExecContext(ctx, `UPDATE chats SET unread = unread + 1 WHERE jid = ?`, m.Chat); err != nil {
			return false, err
		}
	}
	return true, tx.Commit()
}

// SetChatName sets the display name of a chat, creating it if needed.
// An empty name never overwrites an existing one.
func (s *MsgStore) SetChatName(ctx context.Context, jid, name string) error {
	if name == "" {
		return nil
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	_, err := s.db.ExecContext(ctx, `INSERT INTO chats (jid, name) VALUES (?, ?)
		ON CONFLICT (jid) DO UPDATE SET name = excluded.name`, jid, name)
	return err
}

// ChatName returns the stored name of a chat, or "" if unknown.
func (s *MsgStore) ChatName(ctx context.Context, jid string) string {
	s.mu.Lock()
	defer s.mu.Unlock()
	var name string
	_ = s.db.QueryRowContext(ctx, `SELECT name FROM chats WHERE jid = ?`, jid).Scan(&name)
	return name
}

// SetUnread overrides the unread counter of a chat (used by history sync).
// The newest `count` incoming messages are flagged as unread so they can be
// acknowledged later.
func (s *MsgStore) SetUnread(ctx context.Context, jid string, count int) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	if _, err := tx.ExecContext(ctx, `INSERT OR IGNORE INTO chats (jid) VALUES (?)`, jid); err != nil {
		return err
	}
	if _, err := tx.ExecContext(ctx, `UPDATE chats SET unread = ? WHERE jid = ?`, count, jid); err != nil {
		return err
	}
	if _, err := tx.ExecContext(ctx, `UPDATE messages SET unread = 0 WHERE chat = ?`, jid); err != nil {
		return err
	}
	if count > 0 {
		if _, err := tx.ExecContext(ctx, `UPDATE messages SET unread = 1 WHERE chat = ? AND id IN (
			SELECT id FROM messages WHERE chat = ? AND me = 0 ORDER BY ts DESC LIMIT ?)`,
			jid, jid, count); err != nil {
			return err
		}
	}
	return tx.Commit()
}

// UnreadRef identifies an unread incoming message, for read receipts.
type UnreadRef struct {
	ID     string
	Sender string
	TS     int64
}

// TakeUnread clears the unread state of a chat and returns the messages that were unread.
func (s *MsgStore) TakeUnread(ctx context.Context, jid string) ([]UnreadRef, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return nil, err
	}
	defer tx.Rollback()
	rows, err := tx.QueryContext(ctx, `SELECT id, sender, ts FROM messages
		WHERE chat = ? AND unread = 1 AND me = 0 ORDER BY ts`, jid)
	if err != nil {
		return nil, err
	}
	var refs []UnreadRef
	for rows.Next() {
		var r UnreadRef
		if err := rows.Scan(&r.ID, &r.Sender, &r.TS); err != nil {
			rows.Close()
			return nil, err
		}
		refs = append(refs, r)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return nil, err
	}
	if _, err := tx.ExecContext(ctx, `UPDATE messages SET unread = 0 WHERE chat = ?`, jid); err != nil {
		return nil, err
	}
	if _, err := tx.ExecContext(ctx, `UPDATE chats SET unread = 0 WHERE jid = ?`, jid); err != nil {
		return nil, err
	}
	return refs, tx.Commit()
}

// Chats returns all chats, most recent first.
func (s *MsgStore) Chats(ctx context.Context) ([]Chat, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	rows, err := s.db.QueryContext(ctx, `SELECT jid, name, unread, last, last_kind, ts
		FROM chats ORDER BY ts DESC`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	chats := []Chat{}
	for rows.Next() {
		var c Chat
		if err := rows.Scan(&c.JID, &c.Name, &c.Unread, &c.Last, &c.LastKind, &c.TS); err != nil {
			return nil, err
		}
		if c.Name == "" {
			c.Name = fallbackName(c.JID)
		}
		chats = append(chats, c)
	}
	return chats, rows.Err()
}

// History returns up to limit messages of a chat, oldest first.
func (s *MsgStore) History(ctx context.Context, jid string, limit int) ([]Message, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	rows, err := s.db.QueryContext(ctx, `SELECT * FROM (
		SELECT id, chat, sender, name, text, kind, ts, me FROM messages
		WHERE chat = ? ORDER BY ts DESC LIMIT ?) ORDER BY ts`, jid, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	msgs := []Message{}
	for rows.Next() {
		var m Message
		if err := rows.Scan(&m.ID, &m.Chat, &m.From, &m.Name, &m.Text, &m.Kind, &m.TS, &m.Me); err != nil {
			return nil, err
		}
		msgs = append(msgs, m)
	}
	return msgs, rows.Err()
}

// Clear removes every chat and message (used on logout).
func (s *MsgStore) Clear(ctx context.Context) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	_, err := s.db.ExecContext(ctx, `DELETE FROM messages; DELETE FROM chats;`)
	return err
}

// fallbackName shows the phone number of a user chat with no known name.
func fallbackName(jid string) string {
	if user, ok := strings.CutSuffix(jid, "@s.whatsapp.net"); ok && user != "" {
		return "+" + user
	}
	return ""
}
