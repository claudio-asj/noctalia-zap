package main

import (
	"context"
	"errors"
	"fmt"
	"log"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/skip2/go-qrcode"
	"go.mau.fi/whatsmeow"
	"go.mau.fi/whatsmeow/proto/waE2E"
	"go.mau.fi/whatsmeow/store"
	"go.mau.fi/whatsmeow/store/sqlstore"
	"go.mau.fi/whatsmeow/types"
	"go.mau.fi/whatsmeow/types/events"
	waLog "go.mau.fi/whatsmeow/util/log"
	"google.golang.org/protobuf/proto"
)

const (
	maxHistoryLimit = 500
	maxTextLength   = 65536
	chatsDebounce   = 300 * time.Millisecond
)

type Config struct {
	DataDir      string
	HistoryLimit int
	QRTerminal   bool
	WALog        waLog.Logger
}

type Daemon struct {
	cfg       Config
	container *sqlstore.Container
	store     *MsgStore
	emit      *Emitter
	quit      context.CancelFunc

	mu          sync.Mutex
	client      *whatsmeow.Client
	state       string
	detail      string
	pairing     bool
	chatsTimer  *time.Timer
	nameLookups map[string]bool
}

func NewDaemon(cfg Config, container *sqlstore.Container, store *MsgStore, emit *Emitter, quit context.CancelFunc) *Daemon {
	return &Daemon{
		cfg:         cfg,
		container:   container,
		store:       store,
		emit:        emit,
		quit:        quit,
		nameLookups: map[string]bool{},
	}
}

func (d *Daemon) qrPath() string { return filepath.Join(d.cfg.DataDir, "qr.png") }

func (d *Daemon) setStatus(state, detail string) {
	d.mu.Lock()
	d.state, d.detail = state, detail
	d.mu.Unlock()
	d.emit.Status(state, detail)
}

func (d *Daemon) getClient() *whatsmeow.Client {
	d.mu.Lock()
	defer d.mu.Unlock()
	return d.client
}

// Start loads the stored device and either reconnects or starts QR pairing.
func (d *Daemon) Start(ctx context.Context) error {
	device, err := d.container.GetFirstDevice(ctx)
	if err != nil {
		return fmt.Errorf("load device: %w", err)
	}
	d.newClient(device)
	d.emitChats(ctx)
	if device.ID == nil {
		return d.startPairing(ctx)
	}
	d.setStatus(StateConnecting, "")
	return d.getClient().Connect()
}

func (d *Daemon) Stop() {
	if c := d.getClient(); c != nil {
		c.Disconnect()
	}
	_ = os.Remove(d.qrPath())
}

func (d *Daemon) newClient(device *store.Device) {
	c := whatsmeow.NewClient(device, d.cfg.WALog)
	c.EnableAutoReconnect = true
	c.AddEventHandler(d.handleEvent)
	d.mu.Lock()
	d.client = c
	d.mu.Unlock()
}

// startPairing requests a QR channel and connects. QR codes are written as PNG
// into the data directory and announced with a "qr" event.
func (d *Daemon) startPairing(ctx context.Context) error {
	d.mu.Lock()
	if d.pairing {
		d.mu.Unlock()
		return nil
	}
	d.pairing = true
	c := d.client
	d.mu.Unlock()

	ch, err := c.GetQRChannel(ctx)
	if err != nil {
		d.mu.Lock()
		d.pairing = false
		d.mu.Unlock()
		return fmt.Errorf("qr channel: %w", err)
	}
	d.setStatus(StateConnecting, "")
	if err := c.Connect(); err != nil {
		d.mu.Lock()
		d.pairing = false
		d.mu.Unlock()
		return fmt.Errorf("connect: %w", err)
	}
	go d.pairLoop(c, ch)
	return nil
}

func (d *Daemon) pairLoop(c *whatsmeow.Client, ch <-chan whatsmeow.QRChannelItem) {
	defer func() {
		d.mu.Lock()
		d.pairing = false
		d.mu.Unlock()
		_ = os.Remove(d.qrPath())
	}()
	for item := range ch {
		switch item.Event {
		case whatsmeow.QRChannelEventCode:
			if err := d.writeQR(item.Code); err != nil {
				log.Printf("qr: %v", err)
				d.setStatus(StateError, "qr_write_failed")
				continue
			}
			d.setStatus(StateQR, "")
			d.emit.Emit(QREvent{Type: "qr", PNG: d.qrPath()})
		case whatsmeow.QRChannelSuccess.Event:
			d.setStatus(StateConnecting, "paired")
		case whatsmeow.QRChannelTimeout.Event:
			c.Disconnect()
			d.setStatus(StateLoggedOut, "qr_timeout")
		case whatsmeow.QRChannelEventError:
			c.Disconnect()
			d.setStatus(StateError, "pair_failed")
			log.Printf("pairing failed: %v", item.Error)
		default:
			// err-client-outdated, err-unexpected-state, err-scanned-without-multidevice, passkey...
			c.Disconnect()
			d.setStatus(StateError, item.Event)
		}
	}
}

func (d *Daemon) writeQR(code string) error {
	png, err := qrcode.Encode(code, qrcode.Medium, 512)
	if err != nil {
		return err
	}
	tmp := d.qrPath() + ".tmp"
	if err := os.WriteFile(tmp, png, 0o600); err != nil {
		return err
	}
	if err := os.Rename(tmp, d.qrPath()); err != nil {
		return err
	}
	if d.cfg.QRTerminal {
		if q, err := qrcode.New(code, qrcode.Low); err == nil {
			fmt.Fprintln(os.Stderr, q.ToSmallString(false))
		}
	}
	return nil
}

func (d *Daemon) handleEvent(raw any) {
	ctx := context.Background()
	switch evt := raw.(type) {
	case *events.Connected:
		d.setStatus(StateConnected, "")
		d.scheduleChats()
	case *events.Disconnected:
		// Auto reconnect is on for logged-in sessions; report it so the UI can show it.
		if c := d.getClient(); c != nil && c.Store.ID != nil {
			d.setStatus(StateConnecting, "reconnecting")
		}
	case *events.KeepAliveTimeout:
		d.setStatus(StateConnecting, "keepalive_timeout")
	case *events.KeepAliveRestored:
		d.setStatus(StateConnected, "")
	case *events.PairSuccess:
		d.setStatus(StateConnecting, "paired")
	case *events.LoggedOut:
		d.onLoggedOut(ctx, "logged_out_remotely")
	case *events.StreamReplaced:
		d.setStatus(StateError, "stream_replaced")
	case *events.TemporaryBan:
		d.setStatus(StateError, "temporary_ban")
	case *events.ClientOutdated:
		d.setStatus(StateError, "client_outdated")
	case *events.ConnectFailure:
		d.setStatus(StateError, "connect_failure")
		log.Printf("connect failure: %s", evt.Reason.String())
	case *events.Message:
		d.handleMessage(ctx, evt, true)
	case *events.HistorySync:
		d.handleHistory(ctx, evt)
	case *events.MarkChatAsRead:
		if evt.Action.GetRead() {
			if _, err := d.store.TakeUnread(ctx, evt.JID.ToNonAD().String()); err != nil {
				log.Printf("mark read: %v", err)
			}
			d.scheduleChats()
		}
	}
}

func (d *Daemon) onLoggedOut(ctx context.Context, detail string) {
	if err := d.store.Clear(ctx); err != nil {
		log.Printf("clear store: %v", err)
	}
	d.setStatus(StateLoggedOut, detail)
	d.emitChats(ctx)
}

func ignoredChat(jid types.JID) bool {
	switch jid.Server {
	case types.BroadcastServer, types.NewsletterServer:
		return true
	}
	return jid.IsEmpty()
}

func (d *Daemon) handleMessage(ctx context.Context, evt *events.Message, live bool) {
	chat := evt.Info.Chat.ToNonAD()
	if ignoredChat(chat) || evt.IsEdit {
		return
	}
	text, kind := extractContent(evt.Message)
	if kind == "" {
		return
	}
	me := evt.Info.IsFromMe
	msg := Message{
		ID:   evt.Info.ID,
		Chat: chat.String(),
		From: evt.Info.Sender.ToNonAD().String(),
		Text: text,
		Kind: kind,
		TS:   evt.Info.Timestamp.Unix(),
		Me:   me,
	}
	if !me {
		msg.Name = d.contactName(ctx, evt.Info.Sender.ToNonAD())
		if msg.Name == "" {
			msg.Name = evt.Info.PushName
		}
	}
	inserted, err := d.store.AddMessage(ctx, msg, live && !me)
	if err != nil {
		log.Printf("store message: %v", err)
		return
	}
	if !inserted {
		return
	}
	d.ensureChatName(ctx, chat, msg.Name)
	if !live {
		return
	}
	if me {
		// Sent from another device: the user has seen the chat.
		if _, err := d.store.TakeUnread(ctx, msg.Chat); err != nil {
			log.Printf("clear unread: %v", err)
		}
	}
	d.emit.Emit(MessageEvent{Type: "message", Message: msg})
	d.scheduleChats()
}

func (d *Daemon) handleHistory(ctx context.Context, evt *events.HistorySync) {
	c := d.getClient()
	for _, conv := range evt.Data.GetConversations() {
		jid, err := types.ParseJID(conv.GetID())
		if err != nil || ignoredChat(jid) {
			continue
		}
		jid = jid.ToNonAD()
		name := conv.GetName()
		if name == "" {
			name = conv.GetDisplayName()
		}
		if err := d.store.SetChatName(ctx, jid.String(), name); err != nil {
			log.Printf("history: chat name: %v", err)
		}
		msgs := conv.GetMessages()
		if len(msgs) > d.cfg.HistoryLimit {
			msgs = msgs[:d.cfg.HistoryLimit]
		}
		for _, hm := range msgs {
			parsed, err := c.ParseWebMessage(jid, hm.GetMessage())
			if err != nil {
				continue
			}
			d.handleMessage(ctx, parsed, false)
		}
		if err := d.store.SetUnread(ctx, jid.String(), int(conv.GetUnreadCount())); err != nil {
			log.Printf("history: unread: %v", err)
		}
		d.ensureChatName(ctx, jid, "")
	}
	d.scheduleChats()
}

// contactName looks up the saved name of a user, resolving LIDs to phone numbers.
func (d *Daemon) contactName(ctx context.Context, jid types.JID) string {
	c := d.getClient()
	if c == nil || c.Store == nil || c.Store.Contacts == nil {
		return ""
	}
	if jid.Server == types.HiddenUserServer && c.Store.LIDs != nil {
		if pn, err := c.Store.LIDs.GetPNForLID(ctx, jid); err == nil && !pn.IsEmpty() {
			jid = pn
		}
	}
	info, err := c.Store.Contacts.GetContact(ctx, jid)
	if err != nil || !info.Found {
		return ""
	}
	for _, n := range []string{info.FullName, info.FirstName, info.BusinessName, info.PushName} {
		if n != "" {
			return n
		}
	}
	return ""
}

// ensureChatName fills in a missing chat name. Group names need a network
// request, which runs in the background so the event loop is not blocked.
func (d *Daemon) ensureChatName(ctx context.Context, chat types.JID, fallback string) {
	key := chat.String()
	if d.store.ChatName(ctx, key) != "" {
		return
	}
	if chat.Server == types.GroupServer {
		d.mu.Lock()
		busy := d.nameLookups[key]
		d.nameLookups[key] = true
		c := d.client
		d.mu.Unlock()
		if busy || c == nil {
			return
		}
		go func() {
			defer func() {
				d.mu.Lock()
				delete(d.nameLookups, key)
				d.mu.Unlock()
			}()
			lctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
			defer cancel()
			info, err := c.GetGroupInfo(lctx, chat)
			if err != nil {
				return
			}
			if err := d.store.SetChatName(lctx, key, info.Name); err == nil {
				d.scheduleChats()
			}
		}()
		return
	}
	name := d.contactName(ctx, chat)
	if name == "" {
		name = fallback
	}
	if name == "" && chat.Server == types.DefaultUserServer {
		name = "+" + chat.User
	}
	if err := d.store.SetChatName(ctx, key, name); err != nil {
		log.Printf("chat name: %v", err)
	}
}

// scheduleChats emits the chat list once after a burst of changes.
func (d *Daemon) scheduleChats() {
	d.mu.Lock()
	defer d.mu.Unlock()
	if d.chatsTimer != nil {
		d.chatsTimer.Reset(chatsDebounce)
		return
	}
	d.chatsTimer = time.AfterFunc(chatsDebounce, func() {
		d.emitChats(context.Background())
	})
}

func (d *Daemon) emitChats(ctx context.Context) {
	chats, err := d.store.Chats(ctx)
	if err != nil {
		log.Printf("list chats: %v", err)
		return
	}
	d.emit.Emit(ChatsEvent{Type: "chats", Items: chats})
}

// Handle executes a control request.
func (d *Daemon) Handle(ctx context.Context, req Request) Response {
	switch req.Cmd {
	case "status":
		d.mu.Lock()
		data := map[string]any{"state": d.state, "detail": d.detail}
		c := d.client
		d.mu.Unlock()
		if c != nil && c.Store != nil && c.Store.ID != nil {
			data["jid"] = c.Store.ID.ToNonAD().String()
		}
		return Response{OK: true, Data: data}
	case "chats":
		chats, err := d.store.Chats(ctx)
		if err != nil {
			return fail(err)
		}
		return Response{OK: true, Data: chats}
	case "history":
		return d.cmdHistory(ctx, req)
	case "send":
		return d.cmdSend(ctx, req)
	case "read":
		return d.cmdRead(ctx, req)
	case "login":
		return d.cmdLogin(ctx)
	case "logout":
		return d.cmdLogout(ctx)
	case "quit":
		d.quit()
		return Response{OK: true}
	}
	return Response{Error: "unknown command"}
}

func fail(err error) Response { return Response{Error: err.Error()} }

func parseChat(s string) (types.JID, error) {
	jid, err := types.ParseJID(s)
	if err != nil || jid.IsEmpty() || jid.User == "" {
		return types.JID{}, errors.New("invalid chat jid")
	}
	return jid.ToNonAD(), nil
}

func (d *Daemon) cmdHistory(ctx context.Context, req Request) Response {
	jid, err := parseChat(req.Chat)
	if err != nil {
		return fail(err)
	}
	limit := req.Limit
	if limit <= 0 {
		limit = d.cfg.HistoryLimit
	}
	limit = min(limit, maxHistoryLimit)
	msgs, err := d.store.History(ctx, jid.String(), limit)
	if err != nil {
		return fail(err)
	}
	d.emit.Emit(HistoryEvent{Type: "history", Chat: jid.String(), Items: msgs})
	return Response{OK: true, Data: msgs}
}

func (d *Daemon) connectedClient() (*whatsmeow.Client, error) {
	c := d.getClient()
	if c == nil || !c.IsConnected() || !c.IsLoggedIn() {
		return nil, errors.New("not connected")
	}
	return c, nil
}

func (d *Daemon) cmdSend(ctx context.Context, req Request) Response {
	jid, err := parseChat(req.Chat)
	if err != nil {
		return fail(err)
	}
	if strings.TrimSpace(req.Text) == "" {
		return Response{Error: "empty text"}
	}
	if len(req.Text) > maxTextLength {
		return Response{Error: "text too long"}
	}
	c, err := d.connectedClient()
	if err != nil {
		return fail(err)
	}
	sctx, cancel := context.WithTimeout(ctx, 45*time.Second)
	defer cancel()
	resp, err := c.SendMessage(sctx, jid, &waE2E.Message{Conversation: proto.String(req.Text)})
	if err != nil {
		log.Printf("send failed: %v", err)
		return Response{Error: "send failed"}
	}
	msg := Message{
		ID:   resp.ID,
		Chat: jid.String(),
		From: c.Store.ID.ToNonAD().String(),
		Text: req.Text,
		Kind: KindText,
		TS:   resp.Timestamp.Unix(),
		Me:   true,
	}
	if _, err := d.store.AddMessage(ctx, msg, false); err != nil {
		log.Printf("store sent message: %v", err)
	}
	d.ensureChatName(ctx, jid, "")
	d.emit.Emit(MessageEvent{Type: "message", Message: msg})
	d.scheduleChats()
	return Response{OK: true, Data: map[string]any{"id": resp.ID, "ts": msg.TS}}
}

func (d *Daemon) cmdRead(ctx context.Context, req Request) Response {
	jid, err := parseChat(req.Chat)
	if err != nil {
		return fail(err)
	}
	refs, err := d.store.TakeUnread(ctx, jid.String())
	if err != nil {
		return fail(err)
	}
	d.scheduleChats()
	if len(refs) == 0 {
		return Response{OK: true}
	}
	c, err := d.connectedClient()
	if err != nil {
		// Local state is cleared; receipts are best effort.
		return Response{OK: true, Data: map[string]any{"receipts": false}}
	}
	bySender := map[string][]types.MessageID{}
	for _, r := range refs {
		bySender[r.Sender] = append(bySender[r.Sender], r.ID)
	}
	for sender, ids := range bySender {
		var senderJID types.JID
		if jid.Server == types.GroupServer {
			senderJID, _ = types.ParseJID(sender)
		}
		if err := c.MarkRead(ctx, ids, time.Now(), jid, senderJID); err != nil {
			log.Printf("mark read: %v", err)
			return Response{OK: true, Data: map[string]any{"receipts": false}}
		}
	}
	return Response{OK: true, Data: map[string]any{"receipts": true}}
}

func (d *Daemon) cmdLogin(ctx context.Context) Response {
	c := d.getClient()
	if c != nil && c.Store.ID != nil && !c.Store.Deleted {
		return Response{Error: "already logged in"}
	}
	d.mu.Lock()
	pairing := d.pairing
	d.mu.Unlock()
	if pairing {
		return Response{OK: true}
	}
	if c != nil {
		c.Disconnect()
	}
	if c == nil || c.Store.Deleted {
		d.newClient(d.container.NewDevice())
	}
	if err := d.startPairing(context.Background()); err != nil {
		return fail(err)
	}
	return Response{OK: true}
}

func (d *Daemon) cmdLogout(ctx context.Context) Response {
	c := d.getClient()
	if c == nil || c.Store.ID == nil {
		return Response{Error: "not logged in"}
	}
	lctx, cancel := context.WithTimeout(ctx, 20*time.Second)
	defer cancel()
	if err := c.Logout(lctx); err != nil {
		// Server unreachable: drop the local session anyway.
		log.Printf("logout: %v", err)
		c.Disconnect()
		if err := c.Store.Delete(lctx); err != nil {
			return fail(err)
		}
	}
	d.onLoggedOut(ctx, "")
	return Response{OK: true}
}
