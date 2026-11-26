package main

import (
	"context"
	"errors"
	"log"
	"strings"
	"time"

	"go.mau.fi/whatsmeow/proto/waE2E"
	"go.mau.fi/whatsmeow/proto/waWeb"
	"go.mau.fi/whatsmeow/types"
	"go.mau.fi/whatsmeow/types/events"
)

const maxReactionLen = 64

// handleChange applies reactions, edits and deletions to stored messages.
// These never become rows of their own and never touch the chat preview or
// unread counter (except an edit of the newest message, which updates the
// preview). Returns true when the event was one of them. Changes arriving by
// history sync (live=false) are applied without an "update" event.
func (d *Daemon) handleChange(ctx context.Context, evt *events.Message, chat types.JID, live bool) bool {
	m := evt.Message
	key := chat.String()
	if r := m.GetReactionMessage(); r != nil {
		sender := selfSender
		if !evt.Info.IsFromMe {
			sender = d.chatKey(ctx, evt.Info.Sender).String()
		}
		ts := evt.Info.Timestamp.Unix()
		if ts <= 0 {
			ts = r.GetSenderTimestampMS() / 1000
		}
		d.reactTo(ctx, key, r.GetKey().GetID(), sender, r.GetText(), ts, live)
		return true
	}
	if pm := m.GetProtocolMessage(); pm != nil {
		switch pm.GetType() {
		case waE2E.ProtocolMessage_REVOKE:
			d.revokeMessage(ctx, key, pm.GetKey().GetID(), live)
			return true
		case waE2E.ProtocolMessage_MESSAGE_EDIT:
			d.editMessage(ctx, key, pm.GetKey().GetID(), pm.GetEditedMessage(), live)
			return true
		}
		return false
	}
	// whatsmeow already swapped the id and content of edits that come from
	// the history sync (ParseWebMessage), so they look like plain messages.
	if evt.IsEdit || evt.RawMessage.GetProtocolMessage().GetType() == waE2E.ProtocolMessage_MESSAGE_EDIT {
		d.editMessage(ctx, key, evt.Info.ID, m, live)
		return true
	}
	return false
}

// isChange reports whether a message event is a reaction, edit or deletion
// instead of a message of its own.
func isChange(evt *events.Message) bool {
	m := evt.Message
	if m.GetReactionMessage() != nil || evt.IsEdit {
		return true
	}
	switch m.GetProtocolMessage().GetType() {
	case waE2E.ProtocolMessage_REVOKE, waE2E.ProtocolMessage_MESSAGE_EDIT:
		return true
	}
	return evt.RawMessage.GetProtocolMessage().GetType() == waE2E.ProtocolMessage_MESSAGE_EDIT
}

func (d *Daemon) reactTo(ctx context.Context, chat, id, sender, emoji string, ts int64, live bool) {
	if id == "" || len(emoji) > maxReactionLen {
		return
	}
	changed, err := d.store.ApplyReaction(ctx, chat, id, sender, emoji, ts)
	if err != nil {
		log.Printf("apply reaction: %v", err)
		return
	}
	if changed && live {
		d.emitUpdate(ctx, chat, id)
	}
}

func (d *Daemon) editMessage(ctx context.Context, chat, id string, content *waE2E.Message, live bool) {
	if id == "" {
		return
	}
	text, kind := extractContent(content)
	if kind == "" {
		return
	}
	changed, err := d.store.ApplyEdit(ctx, chat, id, text)
	if err != nil {
		log.Printf("apply edit: %v", err)
		return
	}
	if !changed {
		return
	}
	d.scheduleChats()
	if live {
		d.emitUpdate(ctx, chat, id)
	}
}

func (d *Daemon) revokeMessage(ctx context.Context, chat, id string, live bool) {
	if id == "" {
		return
	}
	changed, err := d.store.ApplyRevoke(ctx, chat, id)
	if err != nil {
		log.Printf("apply revoke: %v", err)
		return
	}
	if !changed {
		return
	}
	d.scheduleChats()
	if live {
		d.emitUpdate(ctx, chat, id)
	}
}

// applyWebReactions applies the reactions a history message carries. They are
// attached to the reacted message, and each key identifies the reactor.
func (d *Daemon) applyWebReactions(ctx context.Context, chat types.JID, wm *waWeb.WebMessageInfo) {
	for _, r := range wm.GetReactions() {
		k := r.GetKey()
		sender := selfSender
		if !k.GetFromMe() {
			who := k.GetParticipant()
			if who == "" {
				who = k.GetRemoteJID()
			}
			jid, err := types.ParseJID(who)
			if err != nil || jid.IsEmpty() {
				continue
			}
			sender = d.chatKey(ctx, jid).String()
		}
		d.reactTo(ctx, chat.String(), wm.GetKey().GetID(), sender, r.GetText(), r.GetSenderTimestampMS()/1000, false)
	}
}

// historyAck maps the delivery status of a message in a history sync.
func historyAck(wm *waWeb.WebMessageInfo) string {
	switch wm.GetStatus() {
	case waWeb.WebMessageInfo_READ, waWeb.WebMessageInfo_PLAYED:
		return "read"
	case waWeb.WebMessageInfo_DELIVERY_ACK:
		return "delivered"
	}
	return "sent"
}

// contextInfo returns the reply context of whichever submessage carries it.
func contextInfo(m *waE2E.Message) *waE2E.ContextInfo {
	if m == nil {
		return nil
	}
	switch {
	case m.GetExtendedTextMessage() != nil:
		return m.GetExtendedTextMessage().GetContextInfo()
	case m.GetImageMessage() != nil:
		return m.GetImageMessage().GetContextInfo()
	case m.GetVideoMessage() != nil:
		return m.GetVideoMessage().GetContextInfo()
	case m.GetAudioMessage() != nil:
		return m.GetAudioMessage().GetContextInfo()
	case m.GetDocumentMessage() != nil:
		return m.GetDocumentMessage().GetContextInfo()
	case m.GetStickerMessage() != nil:
		return m.GetStickerMessage().GetContextInfo()
	case m.GetLocationMessage() != nil:
		return m.GetLocationMessage().GetContextInfo()
	case m.GetContactMessage() != nil:
		return m.GetContactMessage().GetContextInfo()
	case m.GetPollCreationMessage() != nil:
		return m.GetPollCreationMessage().GetContextInfo()
	}
	return nil
}

// buildQuote returns the quote of a reply, or nil. The name is left for
// fillQuoteNames.
func (d *Daemon) buildQuote(ctx context.Context, m *waE2E.Message) *Quote {
	ci := contextInfo(m)
	if ci == nil || ci.GetStanzaID() == "" {
		return nil
	}
	q := &Quote{ID: ci.GetStanzaID()}
	if jid, err := types.ParseJID(ci.GetParticipant()); err == nil && !jid.IsEmpty() {
		q.From = d.chatKey(ctx, jid).String()
	}
	text, kind := extractContent(ci.GetQuotedMessage())
	if kind == "" {
		kind = KindText
	}
	q.Text, _ = clip(text, maxPreviewText)
	q.Kind = kind
	return q
}

// isSelf reports whether a JID is the logged-in account (phone or LID).
func (d *Daemon) isSelf(jid types.JID) bool {
	c := d.getClient()
	if c == nil || c.Store == nil {
		return false
	}
	if me := ownJID(c); me.User != "" && jid.User == me.User {
		return true
	}
	lid := c.Store.GetLID()
	return !lid.IsEmpty() && jid.User == lid.User
}

// fillQuoteNames resolves Quote.Name: empty for my own messages, otherwise
// the contact name or the push name stored with the quoted message.
func (d *Daemon) fillQuoteNames(ctx context.Context, msgs []Message) {
	for i := range msgs {
		q := msgs[i].Quote
		if q == nil {
			continue
		}
		q.Name = ""
		jid, err := types.ParseJID(q.From)
		if err != nil || jid.IsEmpty() || d.isSelf(jid) {
			continue
		}
		if n := d.contactName(ctx, jid); n != "" {
			q.Name, _ = clip(n, maxPreviewText)
			continue
		}
		if n := d.store.MessageName(ctx, msgs[i].Chat, q.ID); n != "" {
			q.Name, _ = clip(n, maxPreviewText)
		}
	}
}

// handleReceipt raises the ack of my messages when someone delivers or reads them.
func (d *Daemon) handleReceipt(ctx context.Context, evt *events.Receipt) {
	var level int
	var name string
	switch evt.Type {
	case types.ReceiptTypeDelivered:
		level, name = ackDelivered, "delivered"
	case types.ReceiptTypeRead, types.ReceiptTypePlayed:
		level, name = ackRead, "read"
	default:
		return
	}
	if ignoredChat(evt.Chat) || len(evt.MessageIDs) == 0 {
		return
	}
	chat := d.chatKey(ctx, evt.Chat).String()
	ids := make([]string, len(evt.MessageIDs))
	copy(ids, evt.MessageIDs)
	changed, err := d.store.SetAck(ctx, chat, ids, level)
	if err != nil {
		log.Printf("receipt: %v", err)
		return
	}
	// Keep every line small: a read burst can list many ids.
	for len(changed) > 0 {
		n := min(len(changed), 100)
		d.emit.Emit(ReceiptEvent{Type: "receipt", Chat: chat, IDs: changed[:n], Ack: name})
		changed = changed[n:]
	}
}

func (d *Daemon) handleChatPresence(ctx context.Context, evt *events.ChatPresence) {
	if ignoredChat(evt.Chat) || evt.IsFromMe {
		return
	}
	state := string(evt.State)
	if evt.State == types.ChatPresenceComposing && evt.Media == types.ChatPresenceMediaAudio {
		state = "recording"
	}
	name := ""
	if evt.Sender.User != "" {
		name = d.contactName(ctx, evt.Sender.ToNonAD())
	}
	name, _ = clip(name, maxPreviewText)
	d.emit.Emit(TypingEvent{
		Type:  "typing",
		Chat:  d.chatKey(ctx, evt.Chat).String(),
		From:  d.chatKey(ctx, evt.Sender).String(),
		Name:  name,
		State: state,
	})
}

func (d *Daemon) setPresence(ctx context.Context, state types.Presence) {
	c := d.getClient()
	if c == nil {
		return
	}
	pctx, cancel := context.WithTimeout(ctx, 15*time.Second)
	defer cancel()
	if err := c.SendPresence(pctx, state); err != nil {
		log.Printf("send presence %s: %v", state, err)
	}
}

func (d *Daemon) cmdReact(ctx context.Context, req Request) Response {
	jid, err := d.parseChat(ctx, req.Chat)
	if err != nil {
		return fail(err)
	}
	if req.ID == "" {
		return Response{Error: "missing id"}
	}
	if len(req.Text) > maxReactionLen {
		return Response{Error: "invalid emoji"}
	}
	orig, found, err := d.store.Lookup(ctx, jid.String(), req.ID)
	if err != nil {
		return fail(err)
	}
	if !found {
		return Response{Error: "not_found"}
	}
	c, _, err := d.connectedClient()
	if err != nil {
		return fail(err)
	}
	author := types.EmptyJID // my own message: BuildMessageKey marks FromMe
	if !orig.Me {
		author, err = types.ParseJID(orig.From)
		if err != nil || author.IsEmpty() {
			return Response{Error: "invalid sender"}
		}
	}
	sctx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()
	if _, err := c.SendMessage(sctx, jid, c.BuildReaction(jid, author, req.ID, req.Text)); err != nil {
		log.Printf("react failed: %v", err)
		return Response{Error: "react failed"}
	}
	d.reactTo(ctx, jid.String(), req.ID, selfSender, req.Text, time.Now().Unix(), true)
	return Response{OK: true}
}

// cmdPresence marks the account online and subscribes to the typing state of
// one chat, or (without a chat) goes back to unavailable.
func (d *Daemon) cmdPresence(ctx context.Context, req Request) Response {
	c, _, err := d.connectedClient()
	if err != nil {
		return fail(err)
	}
	pctx, cancel := context.WithTimeout(ctx, 20*time.Second)
	defer cancel()
	if req.Chat == "" {
		if err := c.SendPresence(pctx, types.PresenceUnavailable); err != nil {
			log.Printf("presence: %v", err)
			return Response{Error: "presence failed"}
		}
		return Response{OK: true}
	}
	jid, err := d.parseChat(ctx, req.Chat)
	if err != nil {
		return fail(err)
	}
	if err := c.SendPresence(pctx, types.PresenceAvailable); err != nil {
		log.Printf("presence: %v", err)
		return Response{Error: "presence failed"}
	}
	// Groups send their typing state without a subscription.
	if jid.Server != types.GroupServer {
		if err := c.SubscribePresence(pctx, jid); err != nil {
			log.Printf("subscribe presence: %v", err)
			return Response{Error: "presence failed"}
		}
	}
	return Response{OK: true}
}

// cmdTyping sends my typing state in a chat.
func (d *Daemon) cmdTyping(ctx context.Context, req Request) Response {
	jid, err := d.parseChat(ctx, req.Chat)
	if err != nil {
		return fail(err)
	}
	var state types.ChatPresence
	switch strings.TrimSpace(req.Text) {
	case "composing":
		state = types.ChatPresenceComposing
	case "paused":
		state = types.ChatPresencePaused
	default:
		return Response{Error: "invalid state"}
	}
	c, _, err := d.connectedClient()
	if err != nil {
		return fail(err)
	}
	sctx, cancel := context.WithTimeout(ctx, 15*time.Second)
	defer cancel()
	if err := c.SendChatPresence(sctx, jid, state, types.ChatPresenceMediaText); err != nil {
		log.Printf("typing: %v", err)
		if errors.Is(err, context.DeadlineExceeded) {
			return Response{Error: "timeout"}
		}
		return Response{Error: "typing failed"}
	}
	return Response{OK: true}
}
