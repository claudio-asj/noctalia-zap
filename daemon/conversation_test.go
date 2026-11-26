package main

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"go.mau.fi/whatsmeow/types"
)

const testChat = "123@s.whatsapp.net"

func addText(t *testing.T, s *MsgStore, id string, ts int64, me bool) {
	t.Helper()
	if _, err := s.AddMessage(context.Background(), Message{ID: id, Chat: testChat, From: testChat, Text: "t" + id,
		Kind: KindText, TS: ts, Me: me}, !me); err != nil {
		t.Fatal(err)
	}
}

func lookup(t *testing.T, s *MsgStore, id string) Message {
	t.Helper()
	m, found, err := s.Lookup(context.Background(), testChat, id)
	if err != nil || !found {
		t.Fatalf("lookup %s: found=%v err=%v", id, found, err)
	}
	return m
}

func TestReactions(t *testing.T) {
	ctx := context.Background()
	s := openTestStore(t)
	addText(t, s, "a", 10, false)
	react := func(sender, emoji string, ts int64) bool {
		ok, err := s.ApplyReaction(ctx, testChat, "a", sender, emoji, ts)
		if err != nil {
			t.Fatal(err)
		}
		return ok
	}
	if !react("p1@s.whatsapp.net", "👍", 20) || !react(selfSender, "👍", 21) || !react("p2@s.whatsapp.net", "❤️", 22) {
		t.Fatal("expected changes")
	}
	rs := lookup(t, s, "a").Reactions
	if len(rs) != 2 || rs[0] != (Reaction{"👍", 2, true}) || rs[1] != (Reaction{"❤️", 1, false}) {
		t.Fatalf("reactions = %+v", rs)
	}
	// Replacing: p1 switches to the heart, the thumbs-up keeps only me.
	if !react("p1@s.whatsapp.net", "❤️", 30) {
		t.Fatal("replace must change")
	}
	rs = lookup(t, s, "a").Reactions
	if len(rs) != 2 || rs[0] != (Reaction{"👍", 1, true}) || rs[1] != (Reaction{"❤️", 2, false}) {
		t.Fatalf("after replace = %+v", rs)
	}
	// An older replay must not undo the newer reaction; the same emoji changes nothing.
	if react("p1@s.whatsapp.net", "😂", 5) || react("p1@s.whatsapp.net", "❤️", 31) {
		t.Fatal("stale or identical reaction must not change")
	}
	// Removal.
	if !react(selfSender, "", 40) || react(selfSender, "", 41) {
		t.Fatal("remove once")
	}
	rs = lookup(t, s, "a").Reactions
	if len(rs) != 1 || rs[0] != (Reaction{"❤️", 2, false}) {
		t.Fatalf("after remove = %+v", rs)
	}
	// Unknown message and no extra rows or chat changes.
	if ok, err := s.ApplyReaction(ctx, testChat, "zzz", "p@s.whatsapp.net", "👍", 50); err != nil || ok {
		t.Fatalf("unknown message: %v %v", ok, err)
	}
	hist, _ := s.History(ctx, testChat, 10, 0)
	chats, _ := s.Chats(ctx)
	if len(hist) != 1 || chats[0].Unread != 1 || chats[0].Last != "ta" {
		t.Fatalf("hist=%d chat=%+v", len(hist), chats[0])
	}
	if err := s.Clear(ctx); err != nil {
		t.Fatal(err)
	}
	var n int
	if err := s.db.QueryRow(`SELECT COUNT(*) FROM reactions`).Scan(&n); err != nil || n != 0 {
		t.Fatalf("reactions after clear = %d, %v", n, err)
	}
}

func TestEditAndRevoke(t *testing.T) {
	ctx := context.Background()
	s := openTestStore(t)
	addText(t, s, "a", 10, false)
	addText(t, s, "b", 20, false)
	if ok, err := s.ApplyEdit(ctx, testChat, "a", "new a"); err != nil || !ok {
		t.Fatal(ok, err)
	}
	if m := lookup(t, s, "a"); m.Text != "new a" || !m.Edited {
		t.Fatalf("a = %+v", m)
	}
	chats, _ := s.Chats(ctx)
	if chats[0].Last != "tb" {
		t.Fatalf("editing an old message changed the preview: %q", chats[0].Last)
	}
	if ok, _ := s.ApplyEdit(ctx, testChat, "a", "new a"); ok {
		t.Fatal("same edit twice must not change")
	}
	if ok, _ := s.ApplyEdit(ctx, testChat, "b", "new b"); !ok {
		t.Fatal("edit b")
	}
	chats, _ = s.Chats(ctx)
	if chats[0].Last != "new b" || chats[0].Unread != 2 {
		t.Fatalf("chat = %+v", chats[0])
	}
	if ok, err := s.ApplyEdit(ctx, testChat, "nope", "x"); err != nil || ok {
		t.Fatal("unknown edit", ok, err)
	}

	if ok, err := s.ApplyRevoke(ctx, testChat, "b"); err != nil || !ok {
		t.Fatal(ok, err)
	}
	m := lookup(t, s, "b")
	if !m.Deleted || m.Text != "" || m.Kind != KindText || m.Quote != nil {
		t.Fatalf("b = %+v", m)
	}
	chats, _ = s.Chats(ctx)
	if chats[0].Last != "new a" || chats[0].LastKind != KindText || chats[0].TS != 10 || chats[0].Unread != 1 {
		t.Fatalf("chat after revoke = %+v", chats[0])
	}
	if ok, _ := s.ApplyRevoke(ctx, testChat, "b"); ok {
		t.Fatal("second revoke must not change")
	}
	if ok, _ := s.ApplyEdit(ctx, testChat, "b", "again"); ok {
		t.Fatal("deleted message must not be edited")
	}
}

func TestAckNeverRegresses(t *testing.T) {
	ctx := context.Background()
	s := openTestStore(t)
	addText(t, s, "m", 10, true)
	addText(t, s, "in", 11, false)
	if m := lookup(t, s, "m"); m.Ack != "sent" {
		t.Fatalf("ack = %q", m.Ack)
	}
	if m := lookup(t, s, "in"); m.Ack != "" {
		t.Fatalf("incoming message has ack %q", m.Ack)
	}
	set := func(level int, ids ...string) []string {
		ch, err := s.SetAck(ctx, testChat, ids, level)
		if err != nil {
			t.Fatal(err)
		}
		return ch
	}
	if ch := set(ackRead, "m", "in", "none"); len(ch) != 1 || ch[0] != "m" {
		t.Fatalf("changed = %v", ch)
	}
	if ch := set(ackDelivered, "m"); len(ch) != 0 {
		t.Fatalf("read regressed: %v", ch)
	}
	if m := lookup(t, s, "m"); m.Ack != "read" {
		t.Fatalf("ack = %q", m.Ack)
	}
}

func TestHistoryBefore(t *testing.T) {
	ctx := context.Background()
	s := openTestStore(t)
	for i, id := range []string{"a", "b", "c", "d"} {
		addText(t, s, id, int64(10*(i+1)), false)
	}
	h, err := s.History(ctx, testChat, 2, 40)
	if err != nil || len(h) != 2 || h[0].ID != "c" || h[1].ID != "d" {
		t.Fatalf("before 40, limit 2 = %+v, %v", h, err)
	}
	h, _ = s.History(ctx, testChat, 10, 19)
	if len(h) != 1 || h[0].ID != "a" {
		t.Fatalf("before 19 = %+v", h)
	}
	// Equal timestamps: insertion order breaks the tie, in both directions.
	for _, id := range []string{"e1", "e2", "e3"} {
		addText(t, s, id, 50, false)
	}
	h, _ = s.History(ctx, testChat, 2, 50)
	if len(h) != 2 || h[0].ID != "e2" || h[1].ID != "e3" {
		t.Fatalf("ties = %+v", h)
	}
	if id, _, _, found, err := s.OldestMessage(ctx, testChat); err != nil || !found || id != "a" {
		t.Fatal(id, found, err)
	}
}

func TestQuoteStored(t *testing.T) {
	ctx := context.Background()
	s := openTestStore(t)
	q := &Quote{ID: "orig", From: testChat, Text: "hi", Kind: KindImage}
	if _, err := s.AddMessage(ctx, Message{ID: "r", Chat: testChat, Kind: KindText, TS: 5, Quote: q}, false); err != nil {
		t.Fatal(err)
	}
	if got := lookup(t, s, "r").Quote; got == nil || *got != *q {
		t.Fatalf("quote = %+v", got)
	}
}

func TestParseCtlConversation(t *testing.T) {
	ok := []struct {
		args []string
		want Request
	}{
		{[]string{"send", "1@s.whatsapp.net", "--file", "/p", "--reply", "ID"}, Request{Cmd: "send", Chat: "1@s.whatsapp.net", File: "/p", Reply: "ID"}},
		{[]string{"send", "1@s.whatsapp.net", "oi", "--reply", "ID"}, Request{Cmd: "send", Chat: "1@s.whatsapp.net", Text: "oi", Reply: "ID"}},
		{[]string{"send", "1@s.whatsapp.net", "--reply"}, Request{Cmd: "send", Chat: "1@s.whatsapp.net", Text: "--reply"}},
		{[]string{"react", "1@s.whatsapp.net", "ID", "👍"}, Request{Cmd: "react", Chat: "1@s.whatsapp.net", ID: "ID", Text: "👍"}},
		{[]string{"react", "1@s.whatsapp.net", "ID"}, Request{Cmd: "react", Chat: "1@s.whatsapp.net", ID: "ID"}},
		{[]string{"history", "x@g.us", "20", "--before", "99"}, Request{Cmd: "history", Chat: "x@g.us", Limit: 20, Before: 99}},
		{[]string{"history", "x@g.us", "--before", "99"}, Request{Cmd: "history", Chat: "x@g.us", Before: 99}},
		{[]string{"backfill", "x@g.us", "--older"}, Request{Cmd: "backfill", Chat: "x@g.us", Older: true}},
		{[]string{"backfill", "x@g.us"}, Request{Cmd: "backfill", Chat: "x@g.us"}},
		{[]string{"presence"}, Request{Cmd: "presence"}},
		{[]string{"presence", "x@g.us"}, Request{Cmd: "presence", Chat: "x@g.us"}},
		{[]string{"typing", "x@g.us", "composing"}, Request{Cmd: "typing", Chat: "x@g.us", Text: "composing"}},
	}
	for _, c := range ok {
		got, err := parseCtlArgs(c.args)
		if err != nil || got != c.want {
			t.Errorf("%v = %+v, %v; want %+v", c.args, got, err, c.want)
		}
	}
	for _, bad := range [][]string{
		{"react", "x"}, {"react", "x", "id", "a", "b"}, {"history", "x", "--before", "z"},
		{"history", "x", "--before", "0"}, {"backfill", "x", "--old"}, {"presence", "a", "b"},
		{"typing", "x"}, {"typing", "x", "recording"}, {"send", "x", "t", "--reply", ""},
	} {
		if _, err := parseCtlArgs(bad); err == nil {
			t.Errorf("expected error for %v", bad)
		}
	}
}

func TestUpdateEventFitsLine(t *testing.T) {
	m := Message{ID: "1", Chat: testChat, Text: strings.Repeat("\"\n\x01", 3000), Kind: KindText, Edited: true,
		Quote: &Quote{ID: "q", From: testChat, Name: strings.Repeat("n", 500), Text: strings.Repeat("\"", 200), Kind: KindText}}
	for i := 0; i < 300; i++ {
		m.Reactions = append(m.Reactions, Reaction{Emoji: strings.Repeat("🧑‍🧑‍🧒", 3) + string(rune('a'+i%26)) + string(rune(0x4e00+i)), Count: 1000, Me: true})
	}
	ev := buildUpdate(m)
	var buf strings.Builder
	NewEmitter(&buf).Emit(ev)
	if len(buf.String()) > maxPageBytes+1 {
		t.Fatalf("update line too long: %d", buf.Len())
	}
	if !ev.Truncated || !strings.HasPrefix(buf.String(), `{"type":"update","message":{`) {
		t.Fatalf("unexpected update: %.80s truncated=%v", buf.String(), ev.Truncated)
	}
}

func TestRevokeFilesAndUnread(t *testing.T) {
	ctx := context.Background()
	s := openTestStore(t)
	dir := t.TempDir()
	if err := os.MkdirAll(thumbsDir(dir), 0o700); err != nil {
		t.Fatal(err)
	}
	add := func(id string, ts int64, kind string, unread bool) {
		m := Message{ID: id, Chat: testChat, From: testChat, Text: "t" + id, Kind: kind, TS: ts}
		if kind == KindAudio {
			m.Media, m.Raw = &MediaInfo{Mime: "audio/ogg"}, []byte{1}
		}
		if _, err := s.AddMessage(ctx, m, unread); err != nil {
			t.Fatal(err)
		}
	}
	add("a", 10, KindText, true)
	add("v", 20, KindAudio, false) // read
	add("u", 30, KindAudio, true)  // unread, newest
	cache := cacheFilePath(dir, testChat, "u", "audio/ogg")
	sound := cacheFilePath(dir, testChat, "u#sound", "audio/ogg")
	thumb := thumbFilePath(dir, testChat, "u", KindAudio)
	downloads := filepath.Join(t.TempDir(), "mine.ogg")
	for _, p := range []string{cache, sound, thumb, downloads} {
		if err := os.WriteFile(p, []byte("x"), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	if err := s.SetMediaFile(ctx, testChat, "u", downloads); err != nil {
		t.Fatal(err)
	}
	d := &Daemon{cfg: Config{DataDir: dir}, store: s, emit: NewEmitter(&strings.Builder{})}
	d.revokeMessage(ctx, testChat, "u", false)
	for _, p := range []string{cache, sound, thumb} {
		if fileExists(p) {
			t.Errorf("%s must be removed", p)
		}
	}
	if !fileExists(downloads) {
		t.Fatal("file outside the cache must stay")
	}
	m := lookup(t, s, "u")
	if !m.Deleted || m.Media != nil {
		t.Fatalf("u = %+v", m)
	}
	var file string
	if err := s.db.QueryRow(`SELECT file FROM messages WHERE id = 'u'`).Scan(&file); err != nil || file != "" {
		t.Fatalf("file column = %q, %v", file, err)
	}
	chats, _ := s.Chats(ctx)
	if c := chats[0]; c.Last != "tv" || c.LastKind != KindAudio || c.TS != 20 || c.Unread != 1 {
		t.Fatalf("chat = %+v", c)
	}
	// Deleting a message that was already read leaves the counter alone.
	if ok, _ := s.ApplyRevoke(ctx, testChat, "v"); !ok {
		t.Fatal("revoke v")
	}
	chats, _ = s.Chats(ctx)
	if c := chats[0]; c.Unread != 1 || c.Last != "ta" || c.TS != 10 {
		t.Fatalf("chat = %+v", c)
	}
	// Deleted media is no longer pending backfill nor refilled.
	if need, _ := s.NeedsBackfill(ctx, testChat); need {
		t.Fatal("deleted media must not need backfill")
	}
	filled, err := s.FillMedia(ctx, Message{ID: "u", Chat: testChat, Raw: []byte{9}, Media: &MediaInfo{}})
	if err != nil || filled {
		t.Fatalf("FillMedia on deleted = %v, %v", filled, err)
	}
	// Last message gone: empty preview, chat stays listed.
	if ok, _ := s.ApplyRevoke(ctx, testChat, "a"); !ok {
		t.Fatal("revoke a")
	}
	chats, _ = s.Chats(ctx)
	if len(chats) != 1 || chats[0].Last != "" || chats[0].Unread != 0 {
		t.Fatalf("chats = %+v", chats)
	}
}

type fakePresence struct {
	mu    sync.Mutex
	calls []string
	delay map[string]time.Duration
}

func (f *fakePresence) SendPresence(_ context.Context, st types.Presence) error {
	f.mu.Lock()
	d := f.delay[string(st)]
	f.mu.Unlock()
	time.Sleep(d)
	f.mu.Lock()
	f.calls = append(f.calls, string(st))
	f.mu.Unlock()
	return nil
}
func (f *fakePresence) SubscribePresence(context.Context, types.JID) error { return nil }
func (f *fakePresence) get() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]string(nil), f.calls...)
}

func TestPresenceLease(t *testing.T) {
	f := &fakePresence{}
	d := &Daemon{presenceTest: f, presenceLease: 80 * time.Millisecond}
	ctx := context.Background()
	if r := d.cmdPresence(ctx, Request{Cmd: "presence", Chat: "g@g.us"}); !r.OK {
		t.Fatal(r.Error)
	}
	time.Sleep(50 * time.Millisecond)
	d.cmdPresence(ctx, Request{Cmd: "presence", Chat: "g@g.us"}) // renews
	time.Sleep(50 * time.Millisecond)
	if got := f.get(); len(got) != 2 {
		t.Fatalf("lease expired too early: %v", got)
	}
	time.Sleep(100 * time.Millisecond)
	if got := f.get(); len(got) != 3 || got[2] != "unavailable" {
		t.Fatalf("calls = %v", got)
	}
	// Explicit offline cancels a pending lease: no second unavailable later.
	d.cmdPresence(ctx, Request{Cmd: "presence", Chat: "g@g.us"})
	d.cmdPresence(ctx, Request{Cmd: "presence"})
	time.Sleep(150 * time.Millisecond)
	if got := f.get(); len(got) != 5 || got[4] != "unavailable" {
		t.Fatalf("calls = %v", got)
	}
}

func TestPresenceLastRequestWins(t *testing.T) {
	f := &fakePresence{delay: map[string]time.Duration{"available": 100 * time.Millisecond}}
	d := &Daemon{presenceTest: f}
	ctx := context.Background()
	done := make(chan struct{})
	go func() {
		d.cmdPresence(ctx, Request{Cmd: "presence", Chat: "g@g.us"})
		close(done)
	}()
	time.Sleep(20 * time.Millisecond)
	d.cmdPresence(ctx, Request{Cmd: "presence"}) // arrives later, must end last
	<-done
	got := f.get()
	if len(got) != 2 || got[0] != "available" || got[1] != "unavailable" {
		t.Fatalf("calls = %v", got)
	}
}

func TestQuoteMeAndNames(t *testing.T) {
	ctx := context.Background()
	s := openTestStore(t)
	d := &Daemon{store: s}
	msgs := []Message{{Chat: testChat, Quote: &Quote{ID: "x", From: "5511999@s.whatsapp.net"}}}
	d.fillQuoteNames(ctx, msgs)
	if q := msgs[0].Quote; q.Name != "+5511999" || q.Me {
		t.Fatalf("quote = %+v", q)
	}
	var buf strings.Builder
	d.emit = NewEmitter(&buf)
	d.emitMessage(ctx, Message{ID: "r", Chat: testChat, Kind: KindText,
		Quote: &Quote{ID: "x", From: "5511999@s.whatsapp.net", Text: "hi", Kind: KindText}}, false)
	if !strings.Contains(buf.String(), `"name":"+5511999"`) {
		t.Fatalf("live message without quote name: %s", buf.String())
	}
	b, _ := jsonLine(Quote{ID: "q", From: "f", Me: true})
	if !strings.Contains(b, `"me":true`) {
		t.Fatal(b)
	}
}

func TestBackfillEventShape(t *testing.T) {
	older := true
	b1, _ := jsonLine(BackfillEvent{Type: "backfill", Chat: "c", Older: &older})
	b2, _ := jsonLine(BackfillEvent{Type: "backfill", Chat: "c", Thumbs: true})
	if b1 != `{"type":"backfill","chat":"c","older":true}` || b2 != `{"type":"backfill","chat":"c","thumbs":true}` {
		t.Fatalf("%s %s", b1, b2)
	}
}

func jsonLine(v any) (string, error) {
	var buf strings.Builder
	NewEmitter(&buf).Emit(v)
	return strings.TrimSpace(buf.String()), nil
}
