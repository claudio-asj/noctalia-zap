package main

import (
	"context"
	"errors"
	"fmt"
	"log"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"
)

// Demo mode: the daemon serves made-up conversations from a local message
// store. It never creates a whatsmeow client, never opens the session database
// and never touches the network, so nothing here takes a session container.

const (
	demoSelfJID   = "15550100000@s.whatsapp.net"
	demoGroupJID  = "120363000000000001@g.us"
	demoFilesDir  = "files"
	demoErrorCode = "demo"
)

// demoTimings are the delays of the simulated conversation. Tests shrink them.
type demoTimings struct {
	Delivered time.Duration // send -> delivered receipt
	Read      time.Duration // send -> read receipt
	Reply     time.Duration // send -> fictional reply
	Typing    time.Duration // how long "composing" shows before the reply
	PlayUnit  time.Duration // length of one second of audio playback
}

var defaultDemoTimings = demoTimings{
	Delivered: time.Second,
	Read:      3 * time.Second,
	Reply:     4 * time.Second,
	Typing:    2 * time.Second,
	PlayUnit:  time.Second,
}

type demoPlayback struct {
	chat, id string
	timer    *time.Timer
}

// DemoDaemon answers control commands over fictional data.
type DemoDaemon struct {
	dir   string
	store *MsgStore
	emit  *Emitter
	quit  context.CancelFunc
	tm    demoTimings

	mu       sync.Mutex
	pinned   map[string]bool
	archived map[string]bool
	people   map[string]string // jid -> display name
	groups   map[string]bool
	sendSeq  int
	recvSeq  int
	replySeq int
	timers   []*time.Timer
	play     *demoPlayback
	stopped  bool
}

// NewDemoDaemon wipes and fills the demo data under dir (messages.db is
// recreated by the caller; media/ is recreated here) and returns the handler.
func NewDemoDaemon(dir string, msgs *MsgStore, emit *Emitter, quit context.CancelFunc, tm demoTimings, now time.Time) (*DemoDaemon, error) {
	d := &DemoDaemon{
		dir: dir, store: msgs, emit: emit, quit: quit, tm: tm,
		pinned: map[string]bool{}, archived: map[string]bool{},
		people: map[string]string{}, groups: map[string]bool{},
	}
	if err := d.seed(context.Background(), now); err != nil {
		return nil, err
	}
	return d, nil
}

// resetDemoData removes the previous demo database and media. Only these
// paths inside dir are touched.
func resetDemoData(dir string) error {
	for _, suffix := range []string{"", "-wal", "-shm", "-journal"} {
		if err := os.Remove(filepath.Join(dir, "messages.db"+suffix)); err != nil && !errors.Is(err, os.ErrNotExist) {
			return err
		}
	}
	return os.RemoveAll(mediaDir(dir))
}

// Start announces the chat list and the connected state.
func (d *DemoDaemon) Start() {
	d.emitChats(context.Background())
	d.emit.Status(StateConnected, "")
}

// Stop cancels pending timers.
func (d *DemoDaemon) Stop() {
	d.mu.Lock()
	d.stopped = true
	for _, t := range d.timers {
		t.Stop()
	}
	d.timers = nil
	if d.play != nil {
		d.play.timer.Stop()
		d.play = nil
	}
	d.mu.Unlock()
}

func (d *DemoDaemon) after(dur time.Duration, fn func()) {
	d.mu.Lock()
	defer d.mu.Unlock()
	if d.stopped {
		return
	}
	d.timers = append(d.timers, time.AfterFunc(dur, fn))
}

func (d *DemoDaemon) chatList(ctx context.Context) ([]Chat, error) {
	chats, err := d.store.Chats(ctx)
	if err != nil {
		return nil, err
	}
	d.mu.Lock()
	for i := range chats {
		chats[i].Pinned = d.pinned[chats[i].JID]
		chats[i].Archived = d.archived[chats[i].JID]
	}
	d.mu.Unlock()
	for i := range chats {
		if p := avatarPath(d.dir, chats[i].JID); fileExists(p) {
			chats[i].Avatar = p
		}
	}
	sort.SliceStable(chats, func(a, b int) bool { return chats[a].Pinned && !chats[b].Pinned })
	return chats, nil
}

func (d *DemoDaemon) emitChats(ctx context.Context) {
	chats, err := d.chatList(ctx)
	if err != nil {
		log.Printf("demo: list chats: %v", err)
		return
	}
	d.emit.EmitAll(chatPages(chats))
}

func (d *DemoDaemon) knownChat(ctx context.Context, s string) (string, error) {
	if !d.store.HasChat(ctx, s) {
		return "", errors.New("not_found")
	}
	return s, nil
}

// Handle executes a control request over the demo data.
func (d *DemoDaemon) Handle(ctx context.Context, req Request) Response {
	switch req.Cmd {
	case "status":
		return Response{OK: true, Data: map[string]any{"state": StateConnected, "detail": "", "jid": demoSelfJID}}
	case "chats":
		chats, err := d.chatList(ctx)
		if err != nil {
			return fail(err)
		}
		return Response{OK: true, Data: chats}
	case "history":
		return d.cmdHistory(ctx, req)
	case "backfill":
		if _, err := d.knownChat(ctx, req.Chat); err != nil {
			return fail(err)
		}
		return Response{OK: true, Data: map[string]any{"requested": false}}
	case "avatar":
		chat, err := d.knownChat(ctx, req.Chat)
		if err != nil {
			return fail(err)
		}
		data := map[string]any{}
		if p := avatarPath(d.dir, chat); fileExists(p) {
			data["path"] = p
		}
		return Response{OK: true, Data: data}
	case "send":
		if req.Media != "" {
			return Response{Error: demoErrorCode}
		}
		return d.cmdSend(ctx, req)
	case "react":
		return d.cmdReact(ctx, req)
	case "read":
		return d.cmdRead(ctx, req)
	case "unread":
		chat, err := d.knownChat(ctx, req.Chat)
		if err != nil {
			return fail(err)
		}
		if err := d.store.SetMarkedUnread(ctx, chat, true); err != nil {
			return fail(err)
		}
		d.emitChats(ctx)
		return Response{OK: true}
	case "presence":
		if req.Chat != "" {
			if _, err := d.knownChat(ctx, req.Chat); err != nil {
				return fail(err)
			}
		}
		return Response{OK: true}
	case "typing":
		if _, err := d.knownChat(ctx, req.Chat); err != nil {
			return fail(err)
		}
		if req.Text != "composing" && req.Text != "paused" {
			return Response{Error: "invalid state"}
		}
		return Response{OK: true}
	case "media":
		return d.cmdMedia(ctx, req)
	case "play":
		return d.cmdPlay(ctx, req)
	case "stop":
		d.stopPlayback()
		return Response{OK: true}
	case "login", "logout", "pick", "paste", "record":
		return Response{Error: demoErrorCode}
	case "quit":
		time.AfterFunc(200*time.Millisecond, d.quit)
		return Response{OK: true}
	}
	return Response{Error: "unknown command"}
}

// fillQuotes resolves the names of quotes from the stored messages.
func (d *DemoDaemon) fillQuotes(ctx context.Context, msgs []Message) {
	for i := range msgs {
		q := msgs[i].Quote
		if q == nil {
			continue
		}
		q.Me, q.Name = false, ""
		if q.From == demoSelfJID {
			q.Me = true
			continue
		}
		d.mu.Lock()
		q.Name = d.people[q.From]
		d.mu.Unlock()
	}
}

func (d *DemoDaemon) cmdHistory(ctx context.Context, req Request) Response {
	chat, err := d.knownChat(ctx, req.Chat)
	if err != nil {
		return fail(err)
	}
	limit := req.Limit
	if limit <= 0 {
		limit = 50
	}
	msgs, err := d.store.History(ctx, chat, min(limit, maxHistoryLimit), req.Before)
	if err != nil {
		return fail(err)
	}
	for i := range msgs {
		fillMedia(d.dir, &msgs[i])
	}
	d.fillQuotes(ctx, msgs)
	return Response{OK: true, Data: msgs}
}

func (d *DemoDaemon) nextID(prefix string, seq *int) string {
	d.mu.Lock()
	defer d.mu.Unlock()
	*seq++
	return fmt.Sprintf("%s%04d", prefix, *seq)
}

func (d *DemoDaemon) emitStored(ctx context.Context, msg Message) {
	msg.Raw = nil
	fillMedia(d.dir, &msg)
	if msg.Quote != nil {
		q := *msg.Quote
		msg.Quote = &q
		d.fillQuotes(ctx, []Message{msg})
	}
	d.emit.Emit(buildMessageEvent(msg, false, false))
}

func (d *DemoDaemon) cmdSend(ctx context.Context, req Request) Response {
	chat, err := d.knownChat(ctx, req.Chat)
	if err != nil {
		return fail(err)
	}
	if strings.TrimSpace(req.Text) == "" {
		return Response{Error: "empty text"}
	}
	if len(req.Text) > maxTextLength {
		return Response{Error: "text too long"}
	}
	msg := Message{
		ID: d.nextID("DEMOSEND", &d.sendSeq), Chat: chat, From: demoSelfJID, Text: req.Text,
		Kind: KindText, TS: time.Now().Unix(), Me: true, Ack: "sent",
	}
	if req.Reply != "" {
		orig, found, err := d.store.Lookup(ctx, chat, req.Reply)
		if err != nil {
			return fail(err)
		}
		if !found || orig.Deleted {
			return Response{Error: "not_found"}
		}
		from := orig.From
		if orig.Me {
			from = demoSelfJID
		}
		text, _ := clip(orig.Text, maxPreviewText)
		kind := orig.Kind
		if kind == "" {
			kind = KindText
		}
		msg.Quote = &Quote{ID: orig.ID, From: from, Text: text, Kind: kind, Me: orig.Me}
	}
	if _, err := d.store.AddMessage(ctx, msg, false); err != nil {
		return fail(err)
	}
	d.emitStored(ctx, msg)
	d.emitChats(ctx)
	d.scheduleConversation(chat, msg.ID)
	return Response{OK: true, Data: map[string]any{"id": msg.ID, "ts": msg.TS}}
}

var demoReplies = []string{
	"Sounds good to me!",
	"Ha, nice. Let me check and get back to you.",
	"Perfect, thanks for letting me know.",
	"Great idea, count me in.",
	"Give me a few minutes and I will reply properly.",
}

// scheduleConversation simulates receipts and a short answer to a sent message.
func (d *DemoDaemon) scheduleConversation(chat, id string) {
	receipt := func(level int, name string) func() {
		return func() {
			ctx := context.Background()
			changed, err := d.store.SetAck(ctx, chat, []string{id}, level)
			if err != nil || len(changed) == 0 {
				return
			}
			d.emit.Emit(ReceiptEvent{Type: "receipt", Chat: chat, IDs: changed, Ack: name})
		}
	}
	d.after(d.tm.Delivered, receipt(ackDelivered, "delivered"))
	d.after(d.tm.Read, receipt(ackRead, "read"))

	d.mu.Lock()
	from := chat
	name := d.people[chat]
	if d.groups[chat] {
		from, name = demoGroupMember(d.replySeq)
	}
	text := demoReplies[d.replySeq%len(demoReplies)]
	d.replySeq++
	d.mu.Unlock()

	typingAt := max(d.tm.Reply-d.tm.Typing, 0)
	d.after(typingAt, func() {
		d.emit.Emit(TypingEvent{Type: "typing", Chat: chat, From: from, Name: name, State: "composing"})
	})
	d.after(d.tm.Reply, func() {
		ctx := context.Background()
		d.emit.Emit(TypingEvent{Type: "typing", Chat: chat, From: from, Name: name, State: "paused"})
		reply := Message{
			ID: d.nextID("DEMOREPLY", &d.recvSeq), Chat: chat, From: from, Name: name,
			Text: text, Kind: KindText, TS: time.Now().Unix(),
		}
		if _, err := d.store.AddMessage(ctx, reply, true); err != nil {
			log.Printf("demo: store reply: %v", err)
			return
		}
		d.emitStored(ctx, reply)
		d.emitChats(ctx)
	})
}

func (d *DemoDaemon) cmdReact(ctx context.Context, req Request) Response {
	chat, err := d.knownChat(ctx, req.Chat)
	if err != nil {
		return fail(err)
	}
	if req.ID == "" {
		return Response{Error: "missing id"}
	}
	if len(req.Text) > maxReactionLen {
		return Response{Error: "invalid emoji"}
	}
	orig, found, err := d.store.Lookup(ctx, chat, req.ID)
	if err != nil {
		return fail(err)
	}
	if !found || orig.Deleted {
		return Response{Error: "not_found"}
	}
	changed, err := d.store.ApplyReaction(ctx, chat, req.ID, selfSender, req.Text, time.Now().Unix())
	if err != nil {
		return fail(err)
	}
	if changed {
		if msg, ok, err := d.store.Lookup(ctx, chat, req.ID); err == nil && ok {
			fillMedia(d.dir, &msg)
			d.fillQuotes(ctx, []Message{msg})
			d.emit.Emit(buildUpdate(msg))
		}
	}
	return Response{OK: true}
}

func (d *DemoDaemon) cmdRead(ctx context.Context, req Request) Response {
	chat, err := d.knownChat(ctx, req.Chat)
	if err != nil {
		return fail(err)
	}
	if _, err := d.store.TakeUnread(ctx, chat); err != nil {
		return fail(err)
	}
	d.emitChats(ctx)
	return Response{OK: true}
}

// mediaOf returns the stored media message and its file.
func (d *DemoDaemon) mediaOf(ctx context.Context, req Request) (Message, string, error) {
	chat, err := d.knownChat(ctx, req.Chat)
	if err != nil {
		return Message{}, "", err
	}
	if req.ID == "" {
		return Message{}, "", errors.New("missing id")
	}
	msg, found, err := d.store.Lookup(ctx, chat, req.ID)
	if err != nil {
		return Message{}, "", err
	}
	if !found || msg.Deleted {
		return Message{}, "", errors.New("not_found")
	}
	if !isMediaKind(msg.Kind) || msg.Media == nil {
		return Message{}, "", errors.New("not_media")
	}
	if msg.Media.File == "" || !fileExists(msg.Media.File) {
		return Message{}, "", errors.New("media_unavailable")
	}
	return msg, msg.Media.File, nil
}

func (d *DemoDaemon) cmdMedia(ctx context.Context, req Request) Response {
	_, path, err := d.mediaOf(ctx, req)
	if err != nil {
		return fail(err)
	}
	return Response{OK: true, Data: map[string]any{"path": path}}
}

// cmdPlay simulates playback: events only, no sound.
func (d *DemoDaemon) cmdPlay(ctx context.Context, req Request) Response {
	msg, _, err := d.mediaOf(ctx, req)
	if err != nil {
		return fail(err)
	}
	if msg.Kind != KindAudio {
		return Response{Error: "not_media"}
	}
	seconds := max(msg.Media.Seconds, 1)
	d.stopPlayback()
	pb := &demoPlayback{chat: msg.Chat, id: msg.ID}
	d.mu.Lock()
	if d.stopped {
		d.mu.Unlock()
		return Response{Error: "play failed"}
	}
	d.play = pb
	d.emit.Emit(PlaybackEvent{Type: "playback", Chat: msg.Chat, ID: msg.ID, State: "playing",
		StartedMS: time.Now().UnixMilli(), Seconds: seconds})
	pb.timer = time.AfterFunc(time.Duration(seconds)*d.tm.PlayUnit, func() { d.endPlayback(pb) })
	d.mu.Unlock()
	return Response{OK: true}
}

func (d *DemoDaemon) endPlayback(pb *demoPlayback) {
	d.mu.Lock()
	defer d.mu.Unlock()
	if d.play != pb {
		return
	}
	d.play = nil
	d.emit.Emit(PlaybackEvent{Type: "playback", Chat: pb.chat, ID: pb.id, State: "stopped"})
}

func (d *DemoDaemon) stopPlayback() {
	d.mu.Lock()
	pb := d.play
	d.mu.Unlock()
	if pb == nil {
		return
	}
	pb.timer.Stop()
	d.endPlayback(pb)
}

// runDemo runs the daemon in demo mode: fictional data in dir, no whatsmeow
// client, no session database, no network.
func runDemo(ctx context.Context, cancel context.CancelFunc, dir, socket string, emit *Emitter) int {
	if err := resetDemoData(dir); err != nil {
		log.Printf("demo: reset data: %v", err)
		emit.Status(StateError, "message_store")
		return 1
	}
	msgs, err := OpenMsgStore(filepath.Join(dir, "messages.db"))
	if err != nil {
		log.Printf("open message store: %v", err)
		emit.Status(StateError, "message_store")
		return 1
	}
	defer msgs.Close()
	d, err := NewDemoDaemon(dir, msgs, emit, cancel, defaultDemoTimings, time.Now())
	if err != nil {
		log.Printf("demo: seed: %v", err)
		emit.Status(StateError, "message_store")
		return 1
	}
	ln, err := listenControl(socket)
	if err != nil {
		log.Printf("control socket: %v", err)
		emit.Status(StateError, "socket")
		return 1
	}
	defer func() {
		ln.Close()
		_ = os.Remove(socket)
	}()
	d.Start()
	go serveControl(ctx, ln, d.Handle)
	<-ctx.Done()
	d.Stop()
	return 0
}
