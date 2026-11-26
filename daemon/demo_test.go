package main

import (
	"bytes"
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"sync"
	"testing"
	"time"

	"go.mau.fi/whatsmeow"
	"go.mau.fi/whatsmeow/store/sqlstore"
)

type lockedBuf struct {
	mu sync.Mutex
	b  bytes.Buffer
}

func (l *lockedBuf) Write(p []byte) (int, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.b.Write(p)
}

// events returns every emitted line decoded as a generic object.
func (l *lockedBuf) events() []map[string]any {
	l.mu.Lock()
	defer l.mu.Unlock()
	var out []map[string]any
	for _, line := range strings.Split(strings.TrimSpace(l.b.String()), "\n") {
		if line == "" {
			continue
		}
		var m map[string]any
		if err := json.Unmarshal([]byte(line), &m); err == nil {
			out = append(out, m)
		}
	}
	return out
}

func (l *lockedBuf) count(typ string) int {
	n := 0
	for _, e := range l.events() {
		if e["type"] == typ {
			n++
		}
	}
	return n
}

func newTestDemo(t *testing.T, tm demoTimings) (*DemoDaemon, *lockedBuf, string) {
	t.Helper()
	dir := t.TempDir()
	if err := resetDemoData(dir); err != nil {
		t.Fatal(err)
	}
	msgs, err := OpenMsgStore(filepath.Join(dir, "messages.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { msgs.Close() })
	buf := &lockedBuf{}
	d, err := NewDemoDaemon(dir, msgs, NewEmitter(buf), func() {}, tm, time.Now())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(d.Stop)
	return d, buf, dir
}

var fastTimings = demoTimings{
	Delivered: 10 * time.Millisecond, Read: 30 * time.Millisecond,
	Reply: 60 * time.Millisecond, Typing: 20 * time.Millisecond, PlayUnit: time.Millisecond,
}

func decodeData[T any](t *testing.T, r Response) T {
	t.Helper()
	if !r.OK {
		t.Fatalf("response error: %s", r.Error)
	}
	b, _ := json.Marshal(r.Data)
	var v T
	if err := json.Unmarshal(b, &v); err != nil {
		t.Fatal(err)
	}
	return v
}

func waitFor(t *testing.T, what string, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		if cond() {
			return
		}
		time.Sleep(5 * time.Millisecond)
	}
	t.Fatalf("timeout waiting for %s", what)
}

func TestDemoSeedsChats(t *testing.T) {
	d, buf, dir := newTestDemo(t, fastTimings)
	ctx := context.Background()
	d.Start()
	evs := buf.events()
	if len(evs) < 2 || evs[0]["type"] != "chats" || evs[1]["type"] != "status" || evs[1]["state"] != "connected" {
		t.Fatalf("unexpected start events: %v", evs)
	}
	chats := decodeData[[]Chat](t, d.Handle(ctx, Request{Cmd: "chats"}))
	if len(chats) != 8 {
		t.Fatalf("chats = %d, want 8", len(chats))
	}
	if !chats[0].Pinned {
		t.Error("pinned chat should be first")
	}
	var archived, group, withAvatar, unread int
	for _, c := range chats {
		if c.Archived {
			archived++
		}
		if c.JID == demoGroupJID {
			group++
		}
		if c.Avatar != "" {
			withAvatar++
			if !fileExists(c.Avatar) {
				t.Errorf("avatar missing: %s", c.Avatar)
			}
		}
		if c.Unread > 0 {
			unread++
		}
		if strings.Contains(c.JID, "@") && !strings.HasPrefix(c.JID, "1555010") && c.JID != demoGroupJID {
			t.Errorf("unexpected jid %s", c.JID)
		}
	}
	if archived != 1 || group != 1 || withAvatar < 3 || unread < 3 {
		t.Errorf("archived=%d group=%d avatars=%d unread=%d", archived, group, withAvatar, unread)
	}
	// Every chat has 5 to 15 messages in the last 3 days; media previews exist.
	kinds := map[string]bool{}
	var reacted, edited, deleted, quoted bool
	for _, c := range chats {
		msgs := decodeData[[]Message](t, d.Handle(ctx, Request{Cmd: "history", Chat: c.JID}))
		if len(msgs) < 5 || len(msgs) > 15 {
			t.Errorf("chat %s has %d messages", c.Name, len(msgs))
		}
		for _, m := range msgs {
			if age := time.Since(time.Unix(m.TS, 0)); age < 0 || age > 72*time.Hour {
				t.Errorf("message %s age %v", m.ID, age)
			}
			kinds[m.Kind] = true
			reacted = reacted || len(m.Reactions) > 0
			edited = edited || m.Edited
			deleted = deleted || m.Deleted
			if m.Quote != nil {
				quoted = true
				if !m.Quote.Me && m.Quote.Name == "" {
					t.Errorf("quote without name in %s", m.ID)
				}
			}
			if m.Me && m.Ack == "" {
				t.Errorf("my message %s without ack", m.ID)
			}
			if isMediaKind(m.Kind) {
				if m.Media == nil || (m.Kind != KindAudio && m.Kind != KindDocument && !fileExists(m.Media.Thumb)) {
					t.Errorf("media %s without thumb: %+v", m.ID, m.Media)
				}
				if m.Media.File == "" || !fileExists(m.Media.File) {
					t.Errorf("media %s without file", m.ID)
				}
				if !strings.HasPrefix(m.Media.File, dir) {
					t.Errorf("media file outside data dir: %s", m.Media.File)
				}
			}
		}
	}
	for _, k := range []string{KindImage, KindAudio, KindDocument, KindSticker} {
		if !kinds[k] {
			t.Errorf("missing kind %s", k)
		}
	}
	if !reacted || !edited || !deleted || !quoted {
		t.Errorf("reacted=%v edited=%v deleted=%v quoted=%v", reacted, edited, deleted, quoted)
	}
	// Every emitted line stays small.
	buf.mu.Lock()
	for _, line := range strings.Split(buf.b.String(), "\n") {
		if len(line) > maxPageBytes {
			t.Errorf("line of %d bytes", len(line))
		}
	}
	buf.mu.Unlock()
}

func TestDemoSendFlow(t *testing.T) {
	d, buf, _ := newTestDemo(t, fastTimings)
	ctx := context.Background()
	resp := d.Handle(ctx, Request{Cmd: "send", Chat: demoAlex.jid, Text: "hello there"})
	data := decodeData[map[string]any](t, resp)
	id, _ := data["id"].(string)
	if id == "" {
		t.Fatal("no id")
	}
	waitFor(t, "reply", func() bool { return buf.count("message") >= 2 })
	waitFor(t, "read receipt", func() bool { return buf.count("receipt") >= 2 })

	var acks []string
	var types []string
	for _, e := range buf.events() {
		types = append(types, e["type"].(string))
		if e["type"] == "receipt" {
			acks = append(acks, e["ack"].(string))
			ids := e["ids"].([]any)
			if len(ids) != 1 || ids[0] != id {
				t.Errorf("receipt ids %v", ids)
			}
		}
	}
	if !reflect.DeepEqual(acks, []string{"delivered", "read"}) {
		t.Errorf("acks = %v", acks)
	}
	if buf.count("typing") < 1 {
		t.Errorf("no typing event: %v", types)
	}
	var first, reply map[string]any
	for _, e := range buf.events() {
		if e["type"] != "message" {
			continue
		}
		if first == nil {
			first = e
		} else if reply == nil {
			reply = e
		}
	}
	if first["ack"] != "sent" || first["me"] != true || first["id"] != id {
		t.Errorf("sent message event: %v", first)
	}
	if reply["me"] == true || reply["chat"] != demoAlex.jid || reply["text"] == "" {
		t.Errorf("reply event: %v", reply)
	}
	msgs := decodeData[[]Message](t, d.Handle(ctx, Request{Cmd: "history", Chat: demoAlex.jid}))
	last := msgs[len(msgs)-1]
	if last.Me {
		t.Error("reply should be the newest message")
	}
	for _, m := range msgs {
		if m.ID == id && m.Ack != "read" {
			t.Errorf("stored ack = %q", m.Ack)
		}
	}
}

func TestDemoReactReadMediaPlay(t *testing.T) {
	d, buf, _ := newTestDemo(t, fastTimings)
	ctx := context.Background()
	if r := d.Handle(ctx, Request{Cmd: "react", Chat: demoAlex.jid, ID: "A01", Text: "🎉"}); !r.OK {
		t.Fatal(r.Error)
	}
	if buf.count("update") != 1 {
		t.Error("react should emit update")
	}
	if r := d.Handle(ctx, Request{Cmd: "react", Chat: demoAlex.jid, ID: "nope", Text: "x"}); r.Error != "not_found" {
		t.Errorf("react unknown: %+v", r)
	}
	if r := d.Handle(ctx, Request{Cmd: "read", Chat: demoAlex.jid}); !r.OK {
		t.Fatal(r.Error)
	}
	for _, c := range decodeData[[]Chat](t, d.Handle(ctx, Request{Cmd: "chats"})) {
		if c.JID == demoAlex.jid && c.Unread != 0 {
			t.Error("unread not cleared")
		}
	}
	old := decodeData[[]Message](t, d.Handle(ctx, Request{Cmd: "history", Chat: demoGroupJID, Limit: 3}))
	older := decodeData[[]Message](t, d.Handle(ctx, Request{Cmd: "history", Chat: demoGroupJID, Before: old[0].TS - 1}))
	if len(older) == 0 || older[len(older)-1].TS >= old[0].TS {
		t.Error("history --before did not paginate")
	}
	if got := decodeData[map[string]any](t, d.Handle(ctx, Request{Cmd: "backfill", Chat: demoAlex.jid})); got["requested"] != false {
		t.Errorf("backfill: %v", got)
	}
	path := decodeData[map[string]any](t, d.Handle(ctx, Request{Cmd: "media", Chat: demoMorgan.jid, ID: "M03"}))["path"].(string)
	if !fileExists(path) {
		t.Errorf("media file missing: %s", path)
	}
	if r := d.Handle(ctx, Request{Cmd: "media", Chat: demoMorgan.jid, ID: "M01"}); r.Error != "not_media" {
		t.Errorf("media on text: %+v", r)
	}
	if r := d.Handle(ctx, Request{Cmd: "play", Chat: demoMorgan.jid, ID: "M03"}); !r.OK {
		t.Fatal(r.Error)
	}
	waitFor(t, "playback stopped", func() bool { return buf.count("playback") >= 2 })
	var states []string
	for _, e := range buf.events() {
		if e["type"] == "playback" {
			states = append(states, e["state"].(string))
			if e["state"] == "playing" && (e["started_ms"] == nil || e["seconds"] != float64(14)) {
				t.Errorf("playing event: %v", e)
			}
		}
	}
	if !reflect.DeepEqual(states, []string{"playing", "stopped"}) {
		t.Errorf("states = %v", states)
	}
}

func TestDemoForbiddenCommands(t *testing.T) {
	d, _, _ := newTestDemo(t, fastTimings)
	ctx := context.Background()
	for _, req := range []Request{
		{Cmd: "login"}, {Cmd: "logout"}, {Cmd: "pick"}, {Cmd: "paste"},
		{Cmd: "record", Text: "start"},
		{Cmd: "send", Chat: demoAlex.jid, Media: "/tmp/x.png"},
	} {
		if r := d.Handle(ctx, req); r.OK || r.Error != "demo" {
			t.Errorf("%s: %+v", req.Cmd, r)
		}
	}
}

func TestResetDemoDataOnlyTouchesDemoFiles(t *testing.T) {
	dir := t.TempDir()
	keep := filepath.Join(dir, "session.db")
	if err := os.WriteFile(keep, []byte("x"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "messages.db"), []byte("x"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := resetDemoData(dir); err != nil {
		t.Fatal(err)
	}
	if !fileExists(keep) || fileExists(filepath.Join(dir, "messages.db")) {
		t.Error("reset touched the wrong files")
	}
}

// The demo path must never build a WhatsApp client or open the session store.
func TestDemoHasNoWhatsmeowDependency(t *testing.T) {
	forbidden := []reflect.Type{
		reflect.TypeOf((*sqlstore.Container)(nil)),
		reflect.TypeOf((*whatsmeow.Client)(nil)),
	}
	check := func(what string, typ reflect.Type) {
		for _, f := range forbidden {
			if typ == f {
				t.Errorf("%s uses %s", what, f)
			}
		}
	}
	fn := reflect.TypeOf(NewDemoDaemon)
	for i := 0; i < fn.NumIn(); i++ {
		check("NewDemoDaemon parameter", fn.In(i))
	}
	st := reflect.TypeOf(DemoDaemon{})
	for i := 0; i < st.NumField(); i++ {
		check("DemoDaemon."+st.Field(i).Name, st.Field(i).Type)
	}
	run := reflect.TypeOf(runDemo)
	for i := 0; i < run.NumIn(); i++ {
		check("runDemo parameter", run.In(i))
	}
}
