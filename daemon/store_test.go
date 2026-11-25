package main

import (
	"context"
	"path/filepath"
	"testing"

	_ "github.com/mattn/go-sqlite3"
	"go.mau.fi/whatsmeow/proto/waE2E"
	"google.golang.org/protobuf/proto"
)

func openTestStore(t *testing.T) *MsgStore {
	t.Helper()
	s, err := OpenMsgStore(filepath.Join(t.TempDir(), "messages.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { s.Close() })
	return s
}

func TestStoreUnreadFlow(t *testing.T) {
	ctx := context.Background()
	s := openTestStore(t)
	chat := "123@s.whatsapp.net"

	add := func(id string, ts int64, me, unread bool) bool {
		ok, err := s.AddMessage(ctx, Message{ID: id, Chat: chat, From: chat, Text: "t" + id, Kind: KindText, TS: ts, Me: me}, unread)
		if err != nil {
			t.Fatal(err)
		}
		return ok
	}
	if !add("a", 10, false, true) || !add("b", 20, false, true) {
		t.Fatal("expected inserts")
	}
	if add("a", 10, false, true) {
		t.Fatal("duplicate must be ignored")
	}
	add("c", 5, true, false) // older message must not replace the preview

	chats, err := s.Chats(ctx)
	if err != nil || len(chats) != 1 {
		t.Fatalf("chats = %v, %v", chats, err)
	}
	if c := chats[0]; c.Unread != 2 || c.Last != "tb" || c.TS != 20 {
		t.Fatalf("unexpected chat %+v", c)
	}

	refs, err := s.TakeUnread(ctx, chat)
	if err != nil || len(refs) != 2 || refs[0].ID != "a" {
		t.Fatalf("refs = %v, %v", refs, err)
	}
	if refs, _ := s.TakeUnread(ctx, chat); len(refs) != 0 {
		t.Fatal("unread must be cleared")
	}

	hist, err := s.History(ctx, chat, 2)
	if err != nil || len(hist) != 2 || hist[0].ID != "a" || hist[1].ID != "b" {
		t.Fatalf("history = %v, %v", hist, err)
	}
}

func TestStoreSetUnreadAndNames(t *testing.T) {
	ctx := context.Background()
	s := openTestStore(t)
	chat := "g@g.us"
	for i, id := range []string{"1", "2", "3"} {
		if _, err := s.AddMessage(ctx, Message{ID: id, Chat: chat, Kind: KindText, TS: int64(i)}, false); err != nil {
			t.Fatal(err)
		}
	}
	if err := s.SetUnread(ctx, chat, 2); err != nil {
		t.Fatal(err)
	}
	refs, _ := s.TakeUnread(ctx, chat)
	if len(refs) != 2 || refs[0].ID != "2" || refs[1].ID != "3" {
		t.Fatalf("refs = %v", refs)
	}
	_ = s.SetChatName(ctx, chat, "Group")
	_ = s.SetChatName(ctx, chat, "")
	if n := s.ChatName(ctx, chat); n != "Group" {
		t.Fatalf("name = %q", n)
	}
	if err := s.Clear(ctx); err != nil {
		t.Fatal(err)
	}
	if chats, _ := s.Chats(ctx); len(chats) != 0 {
		t.Fatal("clear failed")
	}
}

func TestExtractContent(t *testing.T) {
	cases := []struct {
		msg        *waE2E.Message
		text, kind string
	}{
		{nil, "", ""},
		{&waE2E.Message{Conversation: proto.String("hi")}, "hi", KindText},
		{&waE2E.Message{ExtendedTextMessage: &waE2E.ExtendedTextMessage{Text: proto.String("link")}}, "link", KindText},
		{&waE2E.Message{ImageMessage: &waE2E.ImageMessage{Caption: proto.String("pic")}}, "pic", KindImage},
		{&waE2E.Message{AudioMessage: &waE2E.AudioMessage{}}, "", KindAudio},
		{&waE2E.Message{ReactionMessage: &waE2E.ReactionMessage{}}, "", ""},
	}
	for _, c := range cases {
		text, kind := extractContent(c.msg)
		if text != c.text || kind != c.kind {
			t.Errorf("extractContent(%v) = %q, %q; want %q, %q", c.msg, text, kind, c.text, c.kind)
		}
	}
}

func TestParseCtlArgs(t *testing.T) {
	req, err := parseCtlArgs([]string{"send", "123@s.whatsapp.net", "a; rm -rf ~"})
	if err != nil || req.Cmd != "send" || req.Text != "a; rm -rf ~" {
		t.Fatalf("send = %+v, %v", req, err)
	}
	req, err = parseCtlArgs([]string{"history", "x@g.us", "20"})
	if err != nil || req.Limit != 20 {
		t.Fatalf("history = %+v, %v", req, err)
	}
	for _, bad := range [][]string{{}, {"nope"}, {"send", "x"}, {"history", "x", "-1"}, {"status", "x"}} {
		if _, err := parseCtlArgs(bad); err == nil {
			t.Errorf("expected error for %v", bad)
		}
	}
}
