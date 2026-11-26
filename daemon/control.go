package main

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"net"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"time"
)

// Request is one command sent over the control socket, one JSON object per line.
type Request struct {
	// File is only used by the ctl client: the text is read from it and the
	// file is removed. It is never sent to the daemon.
	File  string `json:"-"`
	Cmd   string `json:"cmd"`
	Chat  string `json:"chat,omitempty"`
	Text  string `json:"text,omitempty"`
	ID    string `json:"id,omitempty"`
	Limit int    `json:"limit,omitempty"`
	// Reply is the id of the message a send replies to.
	Reply string `json:"reply,omitempty"`
	// Before limits history to messages older than this unix time.
	Before int64 `json:"before,omitempty"`
	// Older makes backfill ask for messages older than the oldest stored.
	Older bool `json:"older,omitempty"`
}

// Response answers a Request.
type Response struct {
	OK    bool   `json:"ok"`
	Error string `json:"error,omitempty"`
	Data  any    `json:"data,omitempty"`
}

// Handler executes a request.
type Handler func(ctx context.Context, req Request) Response

const maxRequestSize = 256 * 1024

func defaultSocketPath() string {
	dir := os.Getenv("XDG_RUNTIME_DIR")
	if dir == "" {
		// A per-user private directory, checked in listenControl.
		dir = filepath.Join(os.TempDir(), fmt.Sprintf("noctalia-zap-%d", os.Getuid()))
	}
	return filepath.Join(dir, "noctalia-zap.sock")
}

// ensurePrivateDir makes sure the socket directory exists, belongs to us and
// is not writable by others, so nobody can plant a socket in it.
func ensurePrivateDir(dir string) error {
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return err
	}
	fi, err := os.Lstat(dir)
	if err != nil {
		return err
	}
	st, ok := fi.Sys().(*syscall.Stat_t)
	if !fi.IsDir() || !ok || int(st.Uid) != os.Getuid() {
		return fmt.Errorf("%s is not a directory owned by the current user", dir)
	}
	if fi.Mode().Perm()&0o022 != 0 {
		return fmt.Errorf("%s is writable by other users", dir)
	}
	return nil
}

// listenControl opens the unix socket, refusing to take over a socket that is still in use.
func listenControl(path string) (net.Listener, error) {
	if err := ensurePrivateDir(filepath.Dir(path)); err != nil {
		return nil, err
	}
	// On a plugin reload the previous daemon may still be shutting down: give it
	// a few seconds to release the socket before giving up.
	for attempt := 0; ; attempt++ {
		c, err := net.DialTimeout("unix", path, time.Second)
		if err != nil {
			break
		}
		c.Close()
		if attempt >= 10 {
			return nil, fmt.Errorf("another daemon is already listening on %s", path)
		}
		time.Sleep(500 * time.Millisecond)
	}
	if err := os.Remove(path); err != nil && !errors.Is(err, os.ErrNotExist) {
		return nil, err
	}
	old := umask(0o077)
	ln, err := net.Listen("unix", path)
	umask(old)
	if err != nil {
		return nil, err
	}
	if err := os.Chmod(path, 0o600); err != nil {
		ln.Close()
		return nil, err
	}
	return ln, nil
}

func serveControl(ctx context.Context, ln net.Listener, h Handler) {
	for {
		conn, err := ln.Accept()
		if err != nil {
			if ctx.Err() != nil || errors.Is(err, net.ErrClosed) {
				return
			}
			log.Printf("control: accept: %v", err)
			continue
		}
		go handleConn(ctx, conn, h)
	}
}

func handleConn(ctx context.Context, conn net.Conn, h Handler) {
	defer conn.Close()
	_ = conn.SetDeadline(time.Now().Add(60 * time.Second))
	r := bufio.NewReader(io.LimitReader(conn, maxRequestSize))
	line, err := r.ReadBytes('\n')
	if err != nil && !(errors.Is(err, io.EOF) && len(line) > 0) {
		return
	}
	var req Request
	var resp Response
	if err := json.Unmarshal(line, &req); err != nil {
		resp = Response{Error: "invalid request"}
	} else {
		resp = safeHandle(ctx, h, req)
	}
	enc := json.NewEncoder(conn)
	enc.SetEscapeHTML(false)
	_ = enc.Encode(resp)
}

// safeHandle keeps a panic in one command from taking the daemon down.
func safeHandle(ctx context.Context, h Handler, req Request) (resp Response) {
	defer func() {
		if r := recover(); r != nil {
			log.Printf("control: panic in %q: %v", req.Cmd, r)
			resp = Response{Error: "internal error"}
		}
	}()
	return h(ctx, req)
}

// runCtl implements `noctalia-zapd ctl <cmd> [args...]`: it builds a request from
// argv, sends it to the running daemon and prints the JSON response to stdout.
func runCtl(socket string, args []string) int {
	req, err := parseCtlArgs(args)
	if err == nil && req.Cmd == "send" && req.File != "" {
		// The plugin writes the text to a private file so it never shows up in
		// the process list; the file is consumed here.
		var b []byte
		b, err = os.ReadFile(req.File)
		_ = os.Remove(req.File)
		req.Text = string(b)
	}
	if err == nil && req.Cmd == "send" && req.Text == "-" {
		// Read the text from stdin so it does not show up in the process list.
		var b []byte
		b, err = io.ReadAll(io.LimitReader(os.Stdin, maxTextLength+1))
		req.Text = strings.TrimRight(string(b), "\n")
	}
	if err != nil {
		fmt.Fprintln(os.Stderr, "ctl:", err)
		fmt.Fprintln(os.Stderr, ctlUsage)
		return 2
	}
	conn, err := net.DialTimeout("unix", socket, 3*time.Second)
	if err != nil {
		fmt.Fprintln(os.Stderr, "ctl: daemon not running:", err)
		return 1
	}
	defer conn.Close()
	_ = conn.SetDeadline(time.Now().Add(70 * time.Second))
	if err := json.NewEncoder(conn).Encode(req); err != nil {
		fmt.Fprintln(os.Stderr, "ctl:", err)
		return 1
	}
	line, err := bufio.NewReader(conn).ReadBytes('\n')
	if err != nil && len(line) == 0 {
		fmt.Fprintln(os.Stderr, "ctl: no response:", err)
		return 1
	}
	os.Stdout.Write(line)
	var resp Response
	if json.Unmarshal(line, &resp) != nil || !resp.OK {
		return 1
	}
	return 0
}

const ctlUsage = `commands:
  status
  chats
  history <chat-jid> [limit] [--before <unix-ts>]
  send <chat-jid> <text>     (use "-" as text to read it from stdin)
  send <chat-jid> --file <path>  (text read from the file, which is then removed)
  send ... [--reply <msg-id>]  (reply to a message; also valid with - and --file)
  react <chat-jid> <msg-id> [emoji]  (no emoji removes my reaction)
  read <chat-jid>
  backfill <chat-jid> [--older]  (ask the phone to resend recent or older history)
  presence [chat-jid]        (online + typing of the chat; no chat: offline)
  typing <chat-jid> composing|paused
  avatar <chat-jid>          (refresh the chat's profile picture if due)
  media <chat-jid> <msg-id>  (download if needed, print the file path)
  play <chat-jid> <msg-id>
  stop
  login
  logout
  quit`

func parseCtlArgs(args []string) (Request, error) {
	if len(args) == 0 {
		return Request{}, errors.New("missing command")
	}
	req := Request{Cmd: args[0]}
	rest := args[1:]
	switch req.Cmd {
	case "status", "chats", "login", "logout", "quit", "stop":
		if len(rest) != 0 {
			return req, fmt.Errorf("%s takes no arguments", req.Cmd)
		}
	case "avatar":
		if len(rest) != 1 {
			return req, fmt.Errorf("usage: %s <chat-jid>", req.Cmd)
		}
		req.Chat = rest[0]
	case "backfill":
		switch {
		case len(rest) == 1:
			req.Chat = rest[0]
		case len(rest) == 2 && rest[1] == "--older":
			req.Chat, req.Older = rest[0], true
		default:
			return req, errors.New("usage: backfill <chat-jid> [--older]")
		}
	case "presence":
		switch len(rest) {
		case 0:
		case 1:
			req.Chat = rest[0]
		default:
			return req, errors.New("usage: presence [chat-jid]")
		}
	case "typing":
		if len(rest) != 2 || (rest[1] != "composing" && rest[1] != "paused") {
			return req, errors.New("usage: typing <chat-jid> composing|paused")
		}
		req.Chat, req.Text = rest[0], rest[1]
	case "react":
		if len(rest) < 2 || len(rest) > 3 || rest[1] == "" {
			return req, errors.New("usage: react <chat-jid> <msg-id> [emoji]")
		}
		req.Chat, req.ID = rest[0], rest[1]
		if len(rest) == 3 {
			req.Text = rest[2]
		}
	case "read":
		if len(rest) != 1 {
			return req, errors.New("usage: read <chat-jid>")
		}
		req.Chat = rest[0]
	case "media", "play":
		if len(rest) != 2 || rest[1] == "" {
			return req, fmt.Errorf("usage: %s <chat-jid> <msg-id>", req.Cmd)
		}
		req.Chat, req.ID = rest[0], rest[1]
	case "history":
		const usage = "usage: history <chat-jid> [limit] [--before <unix-ts>]"
		if n := len(rest); n >= 3 && rest[n-2] == "--before" {
			ts, err := strconv.ParseInt(rest[n-1], 10, 64)
			if err != nil || ts <= 0 {
				return req, errors.New("before must be a positive unix time")
			}
			req.Before = ts
			rest = rest[:n-2]
		}
		if len(rest) < 1 || len(rest) > 2 {
			return req, errors.New(usage)
		}
		req.Chat = rest[0]
		if len(rest) == 2 {
			n, err := strconv.Atoi(rest[1])
			if err != nil || n <= 0 {
				return req, errors.New("limit must be a positive integer")
			}
			req.Limit = n
		}
	case "send":
		if n := len(rest); n >= 4 && rest[n-2] == "--reply" {
			if rest[n-1] == "" {
				return req, errors.New("reply id is empty")
			}
			req.Reply = rest[n-1]
			rest = rest[:n-2]
		}
		switch {
		case len(rest) == 2:
			req.Chat, req.Text = rest[0], rest[1]
		case len(rest) == 3 && rest[1] == "--file":
			req.Chat, req.File = rest[0], rest[2]
		default:
			return req, errors.New("usage: send <chat-jid> <text> | send <chat-jid> - | send <chat-jid> --file <path> [--reply <msg-id>]")
		}
	default:
		return req, fmt.Errorf("unknown command %q", req.Cmd)
	}
	return req, nil
}
