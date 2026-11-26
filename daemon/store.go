package main

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"math"
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
CREATE TABLE IF NOT EXISTS reactions (
	chat   TEXT NOT NULL,
	msg_id TEXT NOT NULL,
	sender TEXT NOT NULL,
	emoji  TEXT NOT NULL,
	ts     INTEGER NOT NULL DEFAULT 0,
	PRIMARY KEY (chat, msg_id, sender)
);
CREATE TABLE IF NOT EXISTS avatars (
	jid     TEXT PRIMARY KEY,
	pic_id  TEXT NOT NULL DEFAULT '',
	checked INTEGER NOT NULL DEFAULT 0
);
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
	if err := migrate(db); err != nil {
		db.Close()
		return nil, fmt.Errorf("migrate schema: %w", err)
	}
	return &MsgStore{db: db}, nil
}

// migrate adds the media and conversation columns to databases created by earlier versions.
func migrate(db *sql.DB) error {
	rows, err := db.Query(`PRAGMA table_info(messages)`)
	if err != nil {
		return err
	}
	have := map[string]bool{}
	for rows.Next() {
		var cid, notnull, pk int
		var name, typ string
		var dflt sql.NullString
		if err := rows.Scan(&cid, &name, &typ, &notnull, &dflt, &pk); err != nil {
			rows.Close()
			return err
		}
		have[name] = true
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return err
	}
	for _, c := range []struct{ name, def string }{
		{"raw", "BLOB"},
		{"media", "TEXT"},
		{"file", "TEXT NOT NULL DEFAULT ''"},
		{"quote_id", "TEXT NOT NULL DEFAULT ''"},
		{"quote_from", "TEXT NOT NULL DEFAULT ''"},
		{"quote_text", "TEXT NOT NULL DEFAULT ''"},
		{"quote_kind", "TEXT NOT NULL DEFAULT ''"},
		{"edited", "INTEGER NOT NULL DEFAULT 0"},
		{"deleted", "INTEGER NOT NULL DEFAULT 0"},
		{"ack", "INTEGER NOT NULL DEFAULT 0"},
	} {
		if have[c.name] {
			continue
		}
		if _, err := db.Exec(`ALTER TABLE messages ADD COLUMN ` + c.name + ` ` + c.def); err != nil {
			return err
		}
	}
	crows, err := db.Query(`PRAGMA table_info(chats)`)
	if err != nil {
		return err
	}
	haveMark := false
	for crows.Next() {
		var cid, notnull, pk int
		var name, typ string
		var dflt sql.NullString
		if err := crows.Scan(&cid, &name, &typ, &notnull, &dflt, &pk); err != nil {
			crows.Close()
			return err
		}
		if name == "marked_unread" {
			haveMark = true
		}
	}
	crows.Close()
	if err := crows.Err(); err != nil {
		return err
	}
	if !haveMark {
		if _, err := db.Exec(`ALTER TABLE chats ADD COLUMN marked_unread INTEGER NOT NULL DEFAULT 0`); err != nil {
			return err
		}
	}
	// Early versions stored the "+phone" fallback as the name, which then
	// blocked real names from replacing it. The fallback is computed on read now.
	if _, err := db.Exec(`UPDATE chats SET name = '' WHERE name LIKE '+%' AND jid LIKE '%@s.whatsapp.net'`); err != nil {
		return err
	}
	return nil
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
// mediaColumns returns the raw and media column values of a message (nil when absent).
func mediaColumns(m Message) (raw, mediaJSON any) {
	if m.Media != nil {
		meta := *m.Media
		meta.Thumb, meta.File = "", ""
		if b, err := json.Marshal(meta); err == nil {
			mediaJSON = string(b)
		}
	}
	if len(m.Raw) > 0 {
		raw = m.Raw
	}
	return raw, mediaJSON
}

// FillMedia adds the download data to a message stored without it (saved
// before media support). Reports whether a row was updated.
func (s *MsgStore) FillMedia(ctx context.Context, m Message) (bool, error) {
	raw, mediaJSON := mediaColumns(m)
	if raw == nil {
		return false, nil
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	res, err := s.db.ExecContext(ctx, `UPDATE messages SET raw = ?, media = ?
		WHERE chat = ? AND id = ? AND deleted = 0 AND (raw IS NULL OR length(raw) = 0)`, raw, mediaJSON, m.Chat, m.ID)
	if err != nil {
		return false, err
	}
	n, _ := res.RowsAffected()
	return n > 0, nil
}

// NeedsBackfill reports whether a chat has media messages stored without download data.
func (s *MsgStore) NeedsBackfill(ctx context.Context, chat string) (bool, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	var n int
	err := s.db.QueryRowContext(ctx, `SELECT COUNT(*) FROM messages WHERE chat = ? AND deleted = 0
		AND kind IN ('image', 'video', 'audio', 'document', 'sticker')
		AND (raw IS NULL OR length(raw) = 0)`, chat).Scan(&n)
	return n > 0, err
}

// NewestMessage returns the newest stored message of a chat, used as the
// anchor of an on-demand history request.
func (s *MsgStore) NewestMessage(ctx context.Context, chat string) (id string, ts int64, me bool, found bool, err error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	err = s.db.QueryRowContext(ctx, `SELECT id, ts, me FROM messages WHERE chat = ?
		ORDER BY ts DESC, rowid DESC LIMIT 1`, chat).Scan(&id, &ts, &me)
	if errors.Is(err, sql.ErrNoRows) {
		return "", 0, false, false, nil
	}
	return id, ts, me, err == nil, err
}

// MessageRef identifies a stored message the way a WhatsApp message key does.
type MessageRef struct {
	ID     string
	Sender string
	TS     int64
	Me     bool
}

// NewestRef returns the newest stored message of a chat with its sender, used
// as the key of mark-as-unread patches.
func (s *MsgStore) NewestRef(ctx context.Context, chat string) (MessageRef, bool, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	var r MessageRef
	err := s.db.QueryRowContext(ctx, `SELECT id, sender, ts, me FROM messages WHERE chat = ?
		ORDER BY ts DESC, rowid DESC LIMIT 1`, chat).Scan(&r.ID, &r.Sender, &r.TS, &r.Me)
	if errors.Is(err, sql.ErrNoRows) {
		return MessageRef{}, false, nil
	}
	return r, err == nil, err
}

// SetMarkedUnread turns the "marked as unread" flag of a chat on or off.
func (s *MsgStore) SetMarkedUnread(ctx context.Context, jid string, marked bool) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	_, err := s.db.ExecContext(ctx, `UPDATE chats SET marked_unread = ? WHERE jid = ?`, marked, jid)
	return err
}

// MarkedUnread reports whether a chat carries the "marked as unread" flag.
func (s *MsgStore) MarkedUnread(ctx context.Context, jid string) (bool, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	var m bool
	err := s.db.QueryRowContext(ctx, `SELECT marked_unread FROM chats WHERE jid = ?`, jid).Scan(&m)
	if errors.Is(err, sql.ErrNoRows) {
		return false, nil
	}
	return m, err
}

// OldestMessage returns the oldest stored message of a chat, used as the
// anchor of an on-demand request for older history.
func (s *MsgStore) OldestMessage(ctx context.Context, chat string) (id string, ts int64, me bool, found bool, err error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	err = s.db.QueryRowContext(ctx, `SELECT id, ts, me FROM messages WHERE chat = ?
		ORDER BY ts ASC, rowid ASC LIMIT 1`, chat).Scan(&id, &ts, &me)
	if errors.Is(err, sql.ErrNoRows) {
		return "", 0, false, false, nil
	}
	return id, ts, me, err == nil, err
}

func (s *MsgStore) AddMessage(ctx context.Context, m Message, unread bool) (bool, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return false, err
	}
	defer tx.Rollback()

	raw, mediaJSON := mediaColumns(m)
	var q Quote
	if m.Quote != nil {
		q = *m.Quote
	}
	ack := 0
	if m.Me {
		ack = max(ackLevel(m.Ack), ackSent)
	}
	res, err := tx.ExecContext(ctx, `INSERT OR IGNORE INTO messages
		(chat, id, sender, name, text, kind, ts, me, unread, raw, media,
		 quote_id, quote_from, quote_text, quote_kind, ack)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		m.Chat, m.ID, m.From, m.Name, m.Text, m.Kind, m.TS, m.Me, unread, raw, mediaJSON,
		q.ID, q.From, q.Text, q.Kind, ack)
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
			SELECT id FROM messages WHERE chat = ? AND me = 0 ORDER BY ts DESC, rowid DESC LIMIT ?)`,
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
		WHERE chat = ? AND unread = 1 AND me = 0 ORDER BY ts, rowid`, jid)
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
	if _, err := tx.ExecContext(ctx, `UPDATE chats SET unread = 0, marked_unread = 0 WHERE jid = ?`, jid); err != nil {
		return nil, err
	}
	return refs, tx.Commit()
}

// Chats returns all chats, most recent first.
func (s *MsgStore) Chats(ctx context.Context) ([]Chat, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	// Chats without any message (history sync sends many) stay hidden, as in WhatsApp.
	rows, err := s.db.QueryContext(ctx, `SELECT jid, name, unread, last, last_kind, ts, marked_unread
		FROM chats WHERE ts > 0 ORDER BY ts DESC`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	chats := []Chat{}
	for rows.Next() {
		var c Chat
		if err := rows.Scan(&c.JID, &c.Name, &c.Unread, &c.Last, &c.LastKind, &c.TS, &c.MarkedUnread); err != nil {
			return nil, err
		}
		chats = append(chats, c)
	}
	return chats, rows.Err()
}

// messageColumns is the column list read by History and Lookup; scanMessages
// reads rows in the same order.
const messageColumns = `id, chat, sender, name, text, kind, ts, me, media, file,
	quote_id, quote_from, quote_text, quote_kind, edited, deleted, ack`

// History returns up to limit messages of a chat, oldest first. When before is
// positive, only messages with ts <= before are considered (pagination).
func (s *MsgStore) History(ctx context.Context, jid string, limit int, before int64) ([]Message, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if before <= 0 {
		before = math.MaxInt64
	}
	// ts <= before: messages sharing the boundary second are not lost; the
	// caller deduplicates by id.
	rows, err := s.db.QueryContext(ctx, `SELECT `+messageColumns+` FROM (
		SELECT `+messageColumns+`, rowid AS rid FROM messages
		WHERE chat = ? AND ts <= ? ORDER BY ts DESC, rowid DESC LIMIT ?) ORDER BY ts, rid`, jid, before, limit)
	if err != nil {
		return nil, err
	}
	return s.scanMessages(ctx, rows, jid)
}

// Lookup returns one stored message, with its reactions.
func (s *MsgStore) Lookup(ctx context.Context, chat, id string) (Message, bool, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	rows, err := s.db.QueryContext(ctx, `SELECT `+messageColumns+` FROM messages
		WHERE chat = ? AND id = ?`, chat, id)
	if err != nil {
		return Message{}, false, err
	}
	msgs, err := s.scanMessages(ctx, rows, chat)
	if err != nil || len(msgs) == 0 {
		return Message{}, false, err
	}
	return msgs[0], true, nil
}

// scanMessages reads and closes rows, then attaches the aggregated reactions.
// The caller holds s.mu.
func (s *MsgStore) scanMessages(ctx context.Context, rows *sql.Rows, chat string) ([]Message, error) {
	msgs := []Message{}
	for rows.Next() {
		var m Message
		var media sql.NullString
		var file string
		var q Quote
		var edited, deleted bool
		var ack int
		if err := rows.Scan(&m.ID, &m.Chat, &m.From, &m.Name, &m.Text, &m.Kind, &m.TS, &m.Me, &media, &file,
			&q.ID, &q.From, &q.Text, &q.Kind, &edited, &deleted, &ack); err != nil {
			rows.Close()
			return nil, err
		}
		if media.Valid {
			var mi MediaInfo
			if json.Unmarshal([]byte(media.String), &mi) == nil {
				mi.File = file
				m.Media = &mi
			}
		}
		if q.ID != "" {
			m.Quote = &q
		}
		m.Edited, m.Deleted = edited, deleted
		if m.Me {
			m.Ack = ackName(max(ack, ackSent))
		}
		msgs = append(msgs, m)
	}
	err := rows.Err()
	rows.Close()
	if err != nil || len(msgs) == 0 {
		return msgs, err
	}
	// One query for the whole chat page: reactions of the messages read.
	byID := make(map[string]*Message, len(msgs))
	lo, hi := msgs[0].TS, msgs[0].TS
	for i := range msgs {
		byID[msgs[i].ID] = &msgs[i]
		lo, hi = min(lo, msgs[i].TS), max(hi, msgs[i].TS)
	}
	rr, err := s.db.QueryContext(ctx, `SELECT r.msg_id, r.sender, r.emoji FROM reactions r
		JOIN messages m ON m.chat = r.chat AND m.id = r.msg_id
		WHERE r.chat = ? AND m.ts BETWEEN ? AND ? ORDER BY r.rowid`, chat, lo, hi)
	if err != nil {
		return nil, err
	}
	defer rr.Close()
	for rr.Next() {
		var id, sender, emoji string
		if err := rr.Scan(&id, &sender, &emoji); err != nil {
			return nil, err
		}
		m := byID[id]
		if m == nil || m.Deleted {
			continue
		}
		addReaction(m, sender, emoji)
	}
	return msgs, rr.Err()
}

// addReaction counts one reaction into the aggregate list of a message.
func addReaction(m *Message, sender, emoji string) {
	for i := range m.Reactions {
		if m.Reactions[i].Emoji == emoji {
			m.Reactions[i].Count++
			m.Reactions[i].Me = m.Reactions[i].Me || sender == selfSender
			return
		}
	}
	m.Reactions = append(m.Reactions, Reaction{Emoji: emoji, Count: 1, Me: sender == selfSender})
}

// ApplyReaction sets (or, with an empty emoji, removes) the reaction of sender
// on a stored message. Reactions older than the one already stored are
// ignored, so history replays cannot undo newer changes. Reports whether the
// aggregate changed; reactions on unknown messages are ignored.
func (s *MsgStore) ApplyReaction(ctx context.Context, chat, id, sender, emoji string, ts int64) (bool, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return false, err
	}
	defer tx.Rollback()
	var n int
	if err := tx.QueryRowContext(ctx, `SELECT COUNT(*) FROM messages WHERE chat = ? AND id = ?`, chat, id).Scan(&n); err != nil {
		return false, err
	}
	if n == 0 {
		return false, nil
	}
	var oldEmoji string
	var oldTS int64
	err = tx.QueryRowContext(ctx, `SELECT emoji, ts FROM reactions WHERE chat = ? AND msg_id = ? AND sender = ?`,
		chat, id, sender).Scan(&oldEmoji, &oldTS)
	exists := err == nil
	if err != nil && !errors.Is(err, sql.ErrNoRows) {
		return false, err
	}
	if exists && oldTS > ts {
		return false, nil
	}
	changed := false
	switch {
	case emoji == "":
		if exists {
			if _, err := tx.ExecContext(ctx, `DELETE FROM reactions WHERE chat = ? AND msg_id = ? AND sender = ?`,
				chat, id, sender); err != nil {
				return false, err
			}
			changed = true
		}
	case exists && oldEmoji == emoji:
		if _, err := tx.ExecContext(ctx, `UPDATE reactions SET ts = ? WHERE chat = ? AND msg_id = ? AND sender = ?`,
			ts, chat, id, sender); err != nil {
			return false, err
		}
	default:
		// Delete and insert (new rowid) so a changed reaction counts as the latest.
		if _, err := tx.ExecContext(ctx, `DELETE FROM reactions WHERE chat = ? AND msg_id = ? AND sender = ?`,
			chat, id, sender); err != nil {
			return false, err
		}
		if _, err := tx.ExecContext(ctx, `INSERT INTO reactions (chat, msg_id, sender, emoji, ts) VALUES (?, ?, ?, ?, ?)`,
			chat, id, sender, emoji, ts); err != nil {
			return false, err
		}
		changed = true
	}
	return changed, tx.Commit()
}

// ApplyEdit replaces the text of a stored, not deleted message. When it is the
// newest message of the chat, the chat preview follows. Reports whether
// anything changed.
func (s *MsgStore) ApplyEdit(ctx context.Context, chat, id, text string) (bool, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return false, err
	}
	defer tx.Rollback()
	res, err := tx.ExecContext(ctx, `UPDATE messages SET text = ?, edited = 1
		WHERE chat = ? AND id = ? AND deleted = 0 AND (edited = 0 OR text != ?)`, text, chat, id, text)
	if err != nil {
		return false, err
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return false, nil
	}
	if _, err := tx.ExecContext(ctx, `UPDATE chats SET last = ? WHERE jid = ? AND ? = (
		SELECT id FROM messages WHERE chat = ? ORDER BY ts DESC, rowid DESC LIMIT 1)`, text, chat, id, chat); err != nil {
		return false, err
	}
	return true, tx.Commit()
}

// ApplyRevoke marks a stored message as deleted for everyone and drops its
// content (including the recorded file path). If it was the newest message,
// the chat preview falls back to the previous message that is not deleted.
// An unread message stops counting as unread. Reports whether anything changed.
func (s *MsgStore) ApplyRevoke(ctx context.Context, chat, id string) (bool, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return false, err
	}
	defer tx.Rollback()
	var wasUnread bool
	err = tx.QueryRowContext(ctx, `SELECT unread FROM messages WHERE chat = ? AND id = ? AND deleted = 0`,
		chat, id).Scan(&wasUnread)
	if errors.Is(err, sql.ErrNoRows) {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	var newest string
	if err := tx.QueryRowContext(ctx, `SELECT id FROM messages WHERE chat = ? AND deleted = 0
		ORDER BY ts DESC, rowid DESC LIMIT 1`, chat).Scan(&newest); err != nil {
		return false, err
	}
	if _, err := tx.ExecContext(ctx, `UPDATE messages SET deleted = 1, text = '', raw = NULL, media = NULL,
		file = '', quote_id = '', quote_from = '', quote_text = '', quote_kind = '', unread = 0
		WHERE chat = ? AND id = ?`, chat, id); err != nil {
		return false, err
	}
	if newest == id {
		var text, kind string
		var ts int64
		err := tx.QueryRowContext(ctx, `SELECT text, kind, ts FROM messages WHERE chat = ? AND deleted = 0
			ORDER BY ts DESC, rowid DESC LIMIT 1`, chat).Scan(&text, &kind, &ts)
		switch {
		case err == nil:
			_, err = tx.ExecContext(ctx, `UPDATE chats SET last = ?, last_kind = ?, ts = ? WHERE jid = ?`,
				text, kind, ts, chat)
		case errors.Is(err, sql.ErrNoRows):
			// Nothing left to show; ts stays so the chat remains listed.
			_, err = tx.ExecContext(ctx, `UPDATE chats SET last = '', last_kind = '' WHERE jid = ?`, chat)
		}
		if err != nil {
			return false, err
		}
	}
	if wasUnread {
		if _, err := tx.ExecContext(ctx, `UPDATE chats SET unread = MAX(unread - 1, 0) WHERE jid = ?`, chat); err != nil {
			return false, err
		}
	}
	return true, tx.Commit()
}

// SetAck raises the delivery state of my own messages. It never lowers it.
// Returns the ids that changed.
func (s *MsgStore) SetAck(ctx context.Context, chat string, ids []string, level int) ([]string, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return nil, err
	}
	defer tx.Rollback()
	var changed []string
	for _, id := range ids {
		res, err := tx.ExecContext(ctx, `UPDATE messages SET ack = ?
			WHERE chat = ? AND id = ? AND me = 1 AND MAX(ack, ?) < ?`, level, chat, id, ackSent, level)
		if err != nil {
			return nil, err
		}
		if n, _ := res.RowsAffected(); n > 0 {
			changed = append(changed, id)
		}
	}
	return changed, tx.Commit()
}

// MediaRef is what is needed to download a media message again.
// MediaIDs lists the media messages of a chat that have download data, newest first.
func (s *MsgStore) MediaIDs(ctx context.Context, chat string, limit int) ([]string, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	rows, err := s.db.QueryContext(ctx, `SELECT id FROM messages WHERE chat = ?
		AND kind IN ('image', 'video', 'sticker') AND length(raw) > 0
		ORDER BY ts DESC, rowid DESC LIMIT ?`, chat, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var ids []string
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			return nil, err
		}
		ids = append(ids, id)
	}
	return ids, rows.Err()
}

type MediaRef struct {
	Kind string
	TS   int64
	Raw  []byte
	Meta MediaInfo
	File string // last saved path, may no longer exist
}

// MediaRef returns the stored media data of a message. found is false when the
// message does not exist. Messages stored before media support have no Raw.
func (s *MsgStore) MediaRef(ctx context.Context, chat, id string) (ref MediaRef, found bool, err error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	var media sql.NullString
	err = s.db.QueryRowContext(ctx, `SELECT kind, ts, raw, media, file FROM messages
		WHERE chat = ? AND id = ?`, chat, id).Scan(&ref.Kind, &ref.TS, &ref.Raw, &media, &ref.File)
	if errors.Is(err, sql.ErrNoRows) {
		return ref, false, nil
	}
	if err != nil {
		return ref, false, err
	}
	if media.Valid {
		_ = json.Unmarshal([]byte(media.String), &ref.Meta)
	}
	return ref, true, nil
}

// SetMediaFile records where a downloaded media file was saved.
func (s *MsgStore) SetMediaFile(ctx context.Context, chat, id, path string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	_, err := s.db.ExecContext(ctx, `UPDATE messages SET file = ? WHERE chat = ? AND id = ?`, path, chat, id)
	return err
}

// Clear removes every chat and message (used on logout).
func (s *MsgStore) Clear(ctx context.Context) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	_, err := s.db.ExecContext(ctx, `DELETE FROM messages; DELETE FROM reactions; DELETE FROM chats; DELETE FROM avatars;`)
	return err
}

// MessageName returns the push name stored with a message, or "".
func (s *MsgStore) MessageName(ctx context.Context, chat, id string) string {
	s.mu.Lock()
	defer s.mu.Unlock()
	var name string
	_ = s.db.QueryRowContext(ctx, `SELECT name FROM messages WHERE chat = ? AND id = ?`, chat, id).Scan(&name)
	return name
}

// LastSenderName returns the push name of the newest incoming message of a chat.
func (s *MsgStore) LastSenderName(ctx context.Context, chat string) string {
	s.mu.Lock()
	defer s.mu.Unlock()
	var name string
	_ = s.db.QueryRowContext(ctx, `SELECT name FROM messages WHERE chat = ? AND me = 0 AND name != ''
		ORDER BY ts DESC LIMIT 1`, chat).Scan(&name)
	return name
}

// AvatarState returns the stored picture id and last check time (unix seconds).
func (s *MsgStore) AvatarState(ctx context.Context, jid string) (picID string, checked int64) {
	s.mu.Lock()
	defer s.mu.Unlock()
	_ = s.db.QueryRowContext(ctx, `SELECT pic_id, checked FROM avatars WHERE jid = ?`, jid).Scan(&picID, &checked)
	return picID, checked
}

// SetAvatar records the picture id of a chat (empty: none) and when it was checked.
func (s *MsgStore) SetAvatar(ctx context.Context, jid, picID string, checked int64) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	_, err := s.db.ExecContext(ctx, `INSERT INTO avatars (jid, pic_id, checked) VALUES (?, ?, ?)
		ON CONFLICT (jid) DO UPDATE SET pic_id = excluded.pic_id, checked = excluded.checked`, jid, picID, checked)
	return err
}

// fallbackName shows the phone number of a user chat with no known name.
func fallbackName(jid string) string {
	if user, ok := strings.CutSuffix(jid, "@s.whatsapp.net"); ok && user != "" {
		return "+" + user
	}
	return ""
}
