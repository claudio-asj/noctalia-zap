package main

import (
	"encoding/json"
	"io"
	"sync"
	"unicode/utf8"
)

// Event types written to stdout, one JSON object per line.

type StatusEvent struct {
	Type   string `json:"type"` // "status"
	State  string `json:"state"`
	Detail string `json:"detail,omitempty"`
}

type QREvent struct {
	Type string `json:"type"` // "qr"
	PNG  string `json:"png"`
}

type Chat struct {
	JID      string `json:"jid"`
	Name     string `json:"name"`
	Unread   int    `json:"unread"`
	Last     string `json:"last"`
	LastKind string `json:"last_kind"`
	TS       int64  `json:"ts"`
	Archived bool   `json:"archived,omitempty"`
	Pinned   bool   `json:"pinned,omitempty"`
}

// ChatsEvent carries one page of the full chat list. The plugin's line reader
// splits long lines, so the list is sent in pages of a few KiB; the plugin
// replaces its list once page == pages-1 arrives.
type ChatsEvent struct {
	Type  string `json:"type"` // "chats"
	Page  int    `json:"page"`
	Pages int    `json:"pages"`
	Items []Chat `json:"items"`
}

type Message struct {
	ID   string `json:"id"`
	Chat string `json:"chat"`
	From string `json:"from"`
	Name string `json:"name"`
	Text string `json:"text"`
	Kind string `json:"kind"`
	TS   int64  `json:"ts"`
	Me   bool   `json:"me"`
	// Media is set for image, video, audio, document and sticker messages.
	Media *MediaInfo `json:"media,omitempty"`
	// Raw is the stored proto needed to download the media; never sent.
	Raw []byte `json:"-"`
}

type MessageEvent struct {
	Type string `json:"type"` // "message"
	Message
	// Backlog marks messages delivered late (offline sync, old timestamps);
	// the plugin should not notify for them.
	Backlog bool `json:"backlog,omitempty"`
	// Truncated means Text was cut to keep the line short; the full text
	// comes from the history command.
	Truncated bool `json:"truncated,omitempty"`
	// Muted is true when the chat is muted on the phone.
	Muted bool `json:"muted,omitempty"`
}

// BackfillEvent says that the phone answered a backfill request for a chat.
type BackfillEvent struct {
	Type string `json:"type"` // "backfill"
	Chat string `json:"chat"`
}

// Status states.
const (
	StateConnecting   = "connecting"
	StateQR           = "qr"
	StateConnected    = "connected"
	StateDisconnected = "disconnected"
	StateLoggedOut    = "logged_out"
	StateError        = "error"
)

// Emitter serializes events to a writer, one per line. Safe for concurrent use.
type Emitter struct {
	mu  sync.Mutex
	enc *json.Encoder
}

func NewEmitter(w io.Writer) *Emitter {
	enc := json.NewEncoder(w)
	enc.SetEscapeHTML(false)
	return &Emitter{enc: enc}
}

func (e *Emitter) Emit(v any) {
	e.mu.Lock()
	defer e.mu.Unlock()
	// Encode appends a newline. Write errors (closed stdout) are not recoverable here.
	_ = e.enc.Encode(v)
}

// EmitAll writes several events with no other event in between.
func (e *Emitter) EmitAll(vs []any) {
	e.mu.Lock()
	defer e.mu.Unlock()
	for _, v := range vs {
		_ = e.enc.Encode(v)
	}
}

// Line size limits: the plugin host splits lines that are too long.
const (
	maxEventText   = 2000
	maxPreviewText = 200
	maxPageBytes   = 8000
)

// clip cuts s to at most n bytes without splitting a UTF-8 character.
func clip(s string, n int) (string, bool) {
	if len(s) <= n {
		return s, false
	}
	for n > 0 && !utf8.RuneStart(s[n]) {
		n--
	}
	return s[:n], true
}

// chatPages splits the chat list into events that each stay under maxPageBytes.
func chatPages(chats []Chat) []any {
	var pages [][]Chat
	var cur []Chat
	size := 0
	for _, c := range chats {
		c.Last, _ = clip(c.Last, maxPreviewText)
		b, _ := json.Marshal(c)
		if len(cur) > 0 && size+len(b) > maxPageBytes {
			pages = append(pages, cur)
			cur, size = nil, 0
		}
		cur = append(cur, c)
		size += len(b) + 1
	}
	pages = append(pages, cur) // always at least one page, possibly empty
	out := make([]any, len(pages))
	for i, p := range pages {
		if p == nil {
			p = []Chat{}
		}
		out[i] = ChatsEvent{Type: "chats", Page: i, Pages: len(pages), Items: p}
	}
	return out
}

func (e *Emitter) Status(state, detail string) {
	e.Emit(StatusEvent{Type: "status", State: state, Detail: detail})
}
