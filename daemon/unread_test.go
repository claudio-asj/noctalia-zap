package main

import (
	"context"
	"database/sql"
	"path/filepath"
	"strings"
	"testing"

	"go.mau.fi/whatsmeow/proto/waSyncAction"
	"go.mau.fi/whatsmeow/types"
	"go.mau.fi/whatsmeow/types/events"
	"google.golang.org/protobuf/proto"
)

func markedOf(t *testing.T, s *MsgStore, jid string) bool {
	t.Helper()
	chats, err := s.Chats(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	for _, c := range chats {
		if c.JID == jid {
			return c.MarkedUnread
		}
	}
	t.Fatalf("chat %s not found", jid)
	return false
}

func seedChat(t *testing.T, s *MsgStore, chat string) {
	t.Helper()
	m := Message{ID: "m1", Chat: chat, From: "5511@s.whatsapp.net", Text: "hi", Kind: KindText, TS: 10}
	if _, err := s.AddMessage(context.Background(), m, false); err != nil {
		t.Fatal(err)
	}
}

func TestMigrateAddsMarkedUnread(t *testing.T) {
	path := filepath.Join(t.TempDir(), "old.db")
	db, err := sql.Open("sqlite3", path)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec(`CREATE TABLE chats (jid TEXT PRIMARY KEY, name TEXT NOT NULL DEFAULT '',
		unread INTEGER NOT NULL DEFAULT 0, last TEXT NOT NULL DEFAULT '', last_kind TEXT NOT NULL DEFAULT '',
		ts INTEGER NOT NULL DEFAULT 0)`); err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec(`INSERT INTO chats (jid, ts) VALUES ('a@s.whatsapp.net', 5)`); err != nil {
		t.Fatal(err)
	}
	db.Close()
	s, err := OpenMsgStore(path)
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	if markedOf(t, s, "a@s.whatsapp.net") {
		t.Fatal("migrated chat must not be marked")
	}
}

func TestMarkedUnreadStore(t *testing.T) {
	ctx := context.Background()
	s := openTestStore(t)
	chat := "5511@s.whatsapp.net"
	seedChat(t, s, chat)
	if err := s.SetMarkedUnread(ctx, chat, true); err != nil {
		t.Fatal(err)
	}
	if !markedOf(t, s, chat) {
		t.Fatal("expected mark")
	}
	if _, err := s.TakeUnread(ctx, chat); err != nil {
		t.Fatal(err)
	}
	if markedOf(t, s, chat) {
		t.Fatal("TakeUnread must clear the mark")
	}
	ref, found, err := s.NewestRef(ctx, chat)
	if err != nil || !found || ref.ID != "m1" || ref.Sender != "5511@s.whatsapp.net" || ref.TS != 10 {
		t.Fatalf("ref = %+v %v %v", ref, found, err)
	}
}

func TestMarkChatAsReadEvent(t *testing.T) {
	s := openTestStore(t)
	chat := "5511@s.whatsapp.net"
	seedChat(t, s, chat)
	d := &Daemon{store: s, emit: NewEmitter(&strings.Builder{})}
	jid, _ := types.ParseJID(chat)
	send := func(read bool) {
		d.handleEvent(&events.MarkChatAsRead{JID: jid, Action: &waSyncAction.MarkChatAsReadAction{Read: proto.Bool(read)}})
	}
	send(false)
	if !markedOf(t, s, chat) {
		t.Fatal("read=false must mark")
	}
	send(true)
	if markedOf(t, s, chat) {
		t.Fatal("read=true must clear the mark")
	}
	d.mu.Lock()
	if d.chatsTimer != nil {
		d.chatsTimer.Stop()
	}
	d.mu.Unlock()
}

func TestUnreadCommandOffline(t *testing.T) {
	ctx := context.Background()
	s := openTestStore(t)
	chat := "5511@s.whatsapp.net"
	seedChat(t, s, chat)
	d := &Daemon{store: s, emit: NewEmitter(&strings.Builder{})}
	if r := d.Handle(ctx, Request{Cmd: "unread", Chat: chat}); r.Error != "not connected" {
		t.Fatalf("unread offline: %+v", r)
	}
	if markedOf(t, s, chat) {
		t.Fatal("failed unread must not mark")
	}
	// read of a marked chat while offline clears the mark locally.
	if err := s.SetMarkedUnread(ctx, chat, true); err != nil {
		t.Fatal(err)
	}
	if r := d.Handle(ctx, Request{Cmd: "read", Chat: chat}); !r.OK {
		t.Fatalf("read offline: %+v", r)
	}
	if markedOf(t, s, chat) {
		t.Fatal("read must clear the mark")
	}
	d.mu.Lock()
	if d.chatsTimer != nil {
		d.chatsTimer.Stop()
	}
	d.mu.Unlock()
}

func TestParseCtlUnread(t *testing.T) {
	r, err := parseCtlArgs([]string{"unread", "a@s.whatsapp.net"})
	if err != nil || r.Cmd != "unread" || r.Chat != "a@s.whatsapp.net" {
		t.Fatalf("%+v %v", r, err)
	}
	if _, err := parseCtlArgs([]string{"unread"}); err == nil {
		t.Fatal("expected usage error")
	}
}

func TestDemoUnread(t *testing.T) {
	d, _, _ := newTestDemo(t, fastTimings)
	ctx := context.Background()
	if r := d.Handle(ctx, Request{Cmd: "unread", Chat: demoAlex.jid}); !r.OK {
		t.Fatal(r.Error)
	}
	if !markedOf(t, d.store, demoAlex.jid) {
		t.Fatal("demo unread must mark")
	}
	if r := d.Handle(ctx, Request{Cmd: "read", Chat: demoAlex.jid}); !r.OK {
		t.Fatal(r.Error)
	}
	if markedOf(t, d.store, demoAlex.jid) {
		t.Fatal("demo read must clear")
	}
	if r := d.Handle(ctx, Request{Cmd: "unread", Chat: "x@s.whatsapp.net"}); r.Error != "not_found" {
		t.Fatalf("unknown: %+v", r)
	}
}
