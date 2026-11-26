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
	// Messages older than this when they arrive are backlog, not news.
	backlogAge = 5 * time.Minute
	// After pairing, the library reconnects on its own; if that fails we retry once.
	pairWatchdog = 30 * time.Second
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

	// lifecycle serializes Start, login and logout so clients are never swapped concurrently.
	lifecycle sync.Mutex
	// statusMu keeps the stored status and the emitted status in the same order.
	statusMu sync.Mutex
	state    string
	detail   string

	mu          sync.Mutex
	client      *whatsmeow.Client
	pairing     bool
	offline     bool // between OfflineSyncPreview and OfflineSyncCompleted
	chatsTimer  *time.Timer
	nameLookups map[string]bool
	// qrFile is the current QR image. Each code gets a new file name so image
	// caches keyed by path (the plugin's ui.image) always show the new code.
	qrFile string
}

func NewDaemon(cfg Config, container *sqlstore.Container, msgs *MsgStore, emit *Emitter, quit context.CancelFunc) *Daemon {
	return &Daemon{
		cfg:         cfg,
		container:   container,
		store:       msgs,
		emit:        emit,
		quit:        quit,
		nameLookups: map[string]bool{},
	}
}

// removeQR deletes the current QR image, if any.
func (d *Daemon) removeQR() {
	d.mu.Lock()
	path := d.qrFile
	d.qrFile = ""
	d.mu.Unlock()
	if path != "" {
		_ = os.Remove(path)
	}
}

func (d *Daemon) setStatus(state, detail string) {
	d.statusMu.Lock()
	defer d.statusMu.Unlock()
	d.state, d.detail = state, detail
	d.emit.Status(state, detail)
}

func (d *Daemon) getStatus() (string, string) {
	d.statusMu.Lock()
	defer d.statusMu.Unlock()
	return d.state, d.detail
}

func (d *Daemon) getClient() *whatsmeow.Client {
	d.mu.Lock()
	defer d.mu.Unlock()
	return d.client
}

// ownJID returns the logged-in JID, or an empty JID. Store.ID can be cleared by
// another goroutine on logout, so it is read once.
func ownJID(c *whatsmeow.Client) types.JID {
	if c == nil || c.Store == nil {
		return types.JID{}
	}
	id := c.Store.ID
	if id == nil {
		return types.JID{}
	}
	return id.ToNonAD()
}

// Start loads the stored device and either reconnects or starts QR pairing.
func (d *Daemon) Start(ctx context.Context) error {
	d.lifecycle.Lock()
	defer d.lifecycle.Unlock()
	device, err := d.container.GetFirstDevice(ctx)
	if err != nil {
		return fmt.Errorf("load device: %w", err)
	}
	d.newClient(device)
	d.emitChats(ctx)
	if device.ID == nil {
		return d.startPairing()
	}
	d.setStatus(StateConnecting, "")
	// With InitialAutoReconnect the client keeps retrying in the background
	// even when this first attempt fails (e.g. no network yet at login).
	if err := d.getClient().Connect(); err != nil {
		log.Printf("connect: %v (retrying)", err)
		d.setStatus(StateConnecting, "reconnecting")
	}
	return nil
}

func (d *Daemon) Stop() {
	if c := d.getClient(); c != nil {
		c.Disconnect()
	}
	d.removeQR()
}

func (d *Daemon) newClient(device *store.Device) {
	c := whatsmeow.NewClient(device, d.cfg.WALog)
	c.EnableAutoReconnect = true
	c.InitialAutoReconnect = true
	c.AddEventHandler(d.handleEvent)
	d.mu.Lock()
	old := d.client
	d.client = c
	d.mu.Unlock()
	if old != nil {
		old.RemoveEventHandlers()
		old.Disconnect()
	}
}

// startPairing requests a QR channel and connects. QR codes are written as PNG
// into the data directory and announced with a "qr" event. The caller holds
// d.lifecycle and has just created a fresh client.
func (d *Daemon) startPairing() error {
	d.mu.Lock()
	if d.pairing {
		d.mu.Unlock()
		return nil
	}
	d.pairing = true
	c := d.client
	d.mu.Unlock()

	stopPairing := func() {
		d.mu.Lock()
		d.pairing = false
		d.mu.Unlock()
	}
	ch, err := c.GetQRChannel(context.Background())
	if err != nil {
		stopPairing()
		return fmt.Errorf("qr channel: %w", err)
	}
	d.setStatus(StateConnecting, "")
	if err := c.Connect(); err != nil {
		stopPairing()
		c.Disconnect()
		log.Printf("connect: %v", err)
		// The plugin can call `login` again; it creates a new client.
		d.setStatus(StateLoggedOut, "connect_failed")
		return nil
	}
	go d.pairLoop(c, ch)
	return nil
}

func (d *Daemon) pairLoop(c *whatsmeow.Client, ch <-chan whatsmeow.QRChannelItem) {
	defer func() {
		d.mu.Lock()
		if d.client == c {
			d.pairing = false
		}
		d.mu.Unlock()
		d.removeQR()
	}()
	for item := range ch {
		if d.getClient() != c {
			continue // replaced by a newer login attempt; drain only
		}
		switch item.Event {
		case whatsmeow.QRChannelEventCode:
			path, err := d.writeQR(item.Code)
			if err != nil {
				log.Printf("qr: %v", err)
				d.setStatus(StateError, "qr_write_failed")
				continue
			}
			d.setStatus(StateQR, "")
			d.emit.Emit(QREvent{Type: "qr", PNG: path})
		case whatsmeow.QRChannelSuccess.Event:
			d.setStatus(StateConnecting, "paired")
			time.AfterFunc(pairWatchdog, func() {
				if d.getClient() == c && ownJID(c).User != "" && !c.IsConnected() {
					if err := c.Connect(); err != nil {
						log.Printf("connect after pairing: %v", err)
					}
				}
			})
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

func (d *Daemon) writeQR(code string) (string, error) {
	png, err := qrcode.Encode(code, qrcode.Medium, 512)
	if err != nil {
		return "", err
	}
	path := filepath.Join(d.cfg.DataDir, fmt.Sprintf("qr-%d.png", time.Now().UnixNano()))
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, png, 0o600); err != nil {
		return "", err
	}
	if err := os.Rename(tmp, path); err != nil {
		_ = os.Remove(tmp)
		return "", err
	}
	d.removeQR()
	d.mu.Lock()
	d.qrFile = path
	d.mu.Unlock()
	if d.cfg.QRTerminal {
		if q, err := qrcode.New(code, qrcode.Low); err == nil {
			fmt.Fprintln(os.Stderr, q.ToSmallString(false))
		}
	}
	return path, nil
}

func (d *Daemon) setOffline(v bool) {
	d.mu.Lock()
	d.offline = v
	d.mu.Unlock()
}

func (d *Daemon) handleEvent(raw any) {
	ctx := context.Background()
	switch evt := raw.(type) {
	case *events.Connected:
		d.setStatus(StateConnected, "")
		d.scheduleChats()
	case *events.Disconnected:
		// Auto reconnect is on for logged-in sessions; report it so the UI can show it.
		// Events are dispatched in goroutines, so a late Disconnected may arrive after
		// the reconnection already happened.
		d.setOffline(false)
		if c := d.getClient(); ownJID(c).User != "" && !c.IsConnected() {
			d.setStatus(StateConnecting, "reconnecting")
		}
	case *events.KeepAliveTimeout:
		d.setStatus(StateConnecting, "keepalive_timeout")
	case *events.KeepAliveRestored:
		d.setStatus(StateConnected, "")
	case *events.PairSuccess:
		d.setStatus(StateConnecting, "paired")
	case *events.OfflineSyncPreview:
		d.setOffline(evt.Messages > 0)
	case *events.OfflineSyncCompleted:
		d.setOffline(false)
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
	case *events.Contact:
		if evt.Action != nil {
			name := evt.Action.GetFullName()
			if name == "" {
				name = evt.Action.GetFirstName()
			}
			d.renameUserChat(ctx, evt.JID, name, true)
		}
	case *events.PushName:
		d.renameUserChat(ctx, evt.JID, evt.NewPushName, false)
	case *events.MarkChatAsRead:
		if evt.Action.GetRead() {
			if _, err := d.store.TakeUnread(ctx, d.chatKey(ctx, evt.JID).String()); err != nil {
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

// chatKey normalizes a chat JID: device part removed and LIDs mapped to phone
// numbers when the mapping is known, so the same person is a single chat.
func (d *Daemon) chatKey(ctx context.Context, jid types.JID) types.JID {
	jid = jid.ToNonAD()
	if jid.Server != types.HiddenUserServer {
		return jid
	}
	c := d.getClient()
	if c == nil || c.Store == nil || c.Store.LIDs == nil {
		return jid
	}
	if pn, err := c.Store.LIDs.GetPNForLID(ctx, jid); err == nil && !pn.IsEmpty() {
		return pn.ToNonAD()
	}
	return jid
}

func (d *Daemon) handleMessage(ctx context.Context, evt *events.Message, live bool) {
	if ignoredChat(evt.Info.Chat) || evt.IsEdit {
		return
	}
	chat := d.chatKey(ctx, evt.Info.Chat)
	text, kind := extractContent(evt.Message)
	if kind == "" {
		return
	}
	me := evt.Info.IsFromMe
	msg := Message{
		ID:   evt.Info.ID,
		Chat: chat.String(),
		From: d.chatKey(ctx, evt.Info.Sender).String(),
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
	pushName := ""
	if !me && chat.Server != types.GroupServer {
		pushName = evt.Info.PushName
	}
	d.ensureChatName(ctx, chat, pushName)
	if !live {
		return
	}
	if me {
		// Sent from another device: the user has seen the chat.
		if _, err := d.store.TakeUnread(ctx, msg.Chat); err != nil {
			log.Printf("clear unread: %v", err)
		}
	}
	d.mu.Lock()
	backlog := d.offline
	d.mu.Unlock()
	if time.Since(evt.Info.Timestamp) > backlogAge {
		backlog = true
	}
	d.emit.Emit(MessageEvent{Type: "message", Message: msg, Backlog: backlog})
	d.scheduleChats()
}

func (d *Daemon) handleHistory(ctx context.Context, evt *events.HistorySync) {
	c := d.getClient()
	if c == nil {
		return
	}
	for _, conv := range evt.Data.GetConversations() {
		raw, err := types.ParseJID(conv.GetID())
		if err != nil || ignoredChat(raw) {
			continue
		}
		raw = raw.ToNonAD()
		jid := d.chatKey(ctx, raw)
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
			parsed, err := c.ParseWebMessage(raw, hm.GetMessage())
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
	jid = d.chatKey(ctx, jid)
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

// renameUserChat updates the name of a one-to-one chat when contact info changes.
// A push name only fills in a chat that has no name yet; a saved contact name wins.
func (d *Daemon) renameUserChat(ctx context.Context, jid types.JID, name string, contact bool) {
	if name == "" || jid.Server == types.GroupServer {
		return
	}
	key := d.chatKey(ctx, jid).String()
	if !contact {
		if d.store.ChatName(ctx, key) != "" {
			return
		}
	}
	if !d.store.HasChat(ctx, key) {
		return
	}
	if err := d.store.SetChatName(ctx, key, name); err != nil {
		log.Printf("chat name: %v", err)
		return
	}
	d.scheduleChats()
}

// ensureChatName fills in a missing chat name. Group names need a network
// request, which runs in the background so the event loop is not blocked.
// Chats with no known name are left empty; the phone number is shown instead
// when the list is read, so a later contact sync can still name them.
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
		state, detail := d.getStatus()
		data := map[string]any{"state": state, "detail": detail}
		if jid := ownJID(d.getClient()); jid.User != "" {
			data["jid"] = jid.String()
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
		// Let the response reach the caller before shutting down.
		time.AfterFunc(200*time.Millisecond, d.quit)
		return Response{OK: true}
	}
	return Response{Error: "unknown command"}
}

func fail(err error) Response { return Response{Error: err.Error()} }

func (d *Daemon) parseChat(ctx context.Context, s string) (types.JID, error) {
	jid, err := types.ParseJID(s)
	if err != nil || jid.IsEmpty() || jid.User == "" {
		return types.JID{}, errors.New("invalid chat jid")
	}
	return d.chatKey(ctx, jid), nil
}

func (d *Daemon) cmdHistory(ctx context.Context, req Request) Response {
	jid, err := d.parseChat(ctx, req.Chat)
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

func (d *Daemon) connectedClient() (*whatsmeow.Client, types.JID, error) {
	c := d.getClient()
	me := ownJID(c)
	if me.User == "" || !c.IsConnected() || !c.IsLoggedIn() {
		return nil, types.JID{}, errors.New("not connected")
	}
	return c, me, nil
}

func (d *Daemon) cmdSend(ctx context.Context, req Request) Response {
	jid, err := d.parseChat(ctx, req.Chat)
	if err != nil {
		return fail(err)
	}
	if strings.TrimSpace(req.Text) == "" {
		return Response{Error: "empty text"}
	}
	if len(req.Text) > maxTextLength {
		return Response{Error: "text too long"}
	}
	c, me, err := d.connectedClient()
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
		From: me.String(),
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

// cmdRead sends read receipts and clears the unread state. Without a
// connection nothing is cleared, so the receipts can be sent later.
func (d *Daemon) cmdRead(ctx context.Context, req Request) Response {
	jid, err := d.parseChat(ctx, req.Chat)
	if err != nil {
		return fail(err)
	}
	c, _, err := d.connectedClient()
	if err != nil {
		return fail(err)
	}
	refs, err := d.store.TakeUnread(ctx, jid.String())
	if err != nil {
		return fail(err)
	}
	d.scheduleChats()
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
			if err := d.store.RestoreUnread(ctx, jid.String(), ids); err != nil {
				log.Printf("restore unread: %v", err)
			}
			d.scheduleChats()
			return Response{Error: "read receipts failed"}
		}
	}
	return Response{OK: true}
}

// cmdLogin starts QR pairing when there is no session, or reconnects a session
// that stopped (stream replaced, connect failure...).
func (d *Daemon) cmdLogin(ctx context.Context) Response {
	d.lifecycle.Lock()
	defer d.lifecycle.Unlock()
	c := d.getClient()
	if ownJID(c).User != "" && !c.Store.Deleted {
		if c.IsConnected() {
			return Response{Error: "already logged in"}
		}
		c.Disconnect()
		d.setStatus(StateConnecting, "")
		if err := c.Connect(); err != nil {
			log.Printf("reconnect: %v", err)
			d.setStatus(StateConnecting, "reconnecting")
		}
		return Response{OK: true, Data: map[string]any{"action": "reconnect"}}
	}
	d.mu.Lock()
	pairing := d.pairing
	d.mu.Unlock()
	if pairing {
		return Response{OK: true, Data: map[string]any{"action": "pairing"}}
	}
	// A fresh device for every pairing attempt: an unpaired device is never
	// stored, and a new client avoids stacking QR handlers.
	d.newClient(d.container.NewDevice())
	if err := d.startPairing(); err != nil {
		return fail(err)
	}
	return Response{OK: true, Data: map[string]any{"action": "pairing"}}
}

func (d *Daemon) cmdLogout(ctx context.Context) Response {
	d.lifecycle.Lock()
	defer d.lifecycle.Unlock()
	c := d.getClient()
	if ownJID(c).User == "" {
		return Response{Error: "not logged in"}
	}
	lctx, cancel := context.WithTimeout(ctx, 20*time.Second)
	defer cancel()
	remote := true
	if err := c.Logout(lctx); err != nil {
		// Server unreachable: drop the local session anyway. The phone still
		// lists this device and the user has to remove it there.
		log.Printf("logout: %v", err)
		remote = false
		c.Disconnect()
		if err := c.Store.Delete(lctx); err != nil {
			return fail(err)
		}
	}
	d.onLoggedOut(ctx, "")
	return Response{OK: true, Data: map[string]any{"remote": remote}}
}
