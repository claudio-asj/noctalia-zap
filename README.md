# Noctalia Zap

A WhatsApp client that lives in the Noctalia bar. The plugin draws the whole interface (unread badge, chat list, conversation view, QR login), and a small Go helper, `noctalia-zapd`, talks to WhatsApp through [whatsmeow](https://github.com/tulir/whatsmeow).

> Status: the helper daemon works from the terminal (QR login, send and receive text). The plugin UI is not written yet.

## Layout

```
daemon/                 noctalia-zapd (Go + whatsmeow)
plugin/zap/             Noctalia plugin (Luau), id claudio-asj/zap
docs/                   architecture, API notes, roadmap (Portuguese)
```

## Building the helper

Needs Go and a C compiler (SQLite uses cgo).

```
cd daemon
go build -o bin/noctalia-zapd .
./bin/noctalia-zapd run -data ~/.local/share/noctalia-zap-test -qr-terminal
```

Scan the QR code with WhatsApp (Linked devices). In another terminal:

```
./bin/noctalia-zapd ctl chats
./bin/noctalia-zapd ctl send 5511999999999@s.whatsapp.net "hello"
```

## Before you use it

This is an unofficial client. WhatsApp's terms forbid third-party clients, and WhatsApp can ban a number that uses one, so test with a spare number first.

The session and the message database stay on your machine and are never committed.
