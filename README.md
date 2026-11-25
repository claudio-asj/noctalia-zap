# Noctalia Zap

A WhatsApp client that lives in the Noctalia bar. The plugin draws the whole interface (unread badge, chat list, conversation view, QR login), and a small Go helper, `noctalia-zapd`, talks to WhatsApp through [whatsmeow](https://github.com/tulir/whatsmeow).

> Status: project skeleton only. There is no working code yet.

## Layout

```
daemon/                 noctalia-zapd (Go + whatsmeow)
plugin/noctalia-zap/    Noctalia plugin (Luau)
docs/                   architecture, API notes, roadmap (Portuguese)
```

## Before you use it

This is an unofficial client. WhatsApp's terms forbid third-party clients, and WhatsApp can ban a number that uses one, so test with a spare number first.

The session and the message database stay on your machine and are never committed.
