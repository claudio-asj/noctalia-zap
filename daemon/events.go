package main

import (
	"encoding/json"
	"io"
	"sync"
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
}

type ChatsEvent struct {
	Type  string `json:"type"` // "chats"
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
}

type MessageEvent struct {
	Type string `json:"type"` // "message"
	Message
	// Backlog marks messages delivered late (offline sync, old timestamps);
	// the plugin should not notify for them.
	Backlog bool `json:"backlog,omitempty"`
}

type HistoryEvent struct {
	Type  string    `json:"type"` // "history"
	Chat  string    `json:"chat"`
	Items []Message `json:"items"`
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

func (e *Emitter) Status(state, detail string) {
	e.Emit(StatusEvent{Type: "status", State: state, Detail: detail})
}
