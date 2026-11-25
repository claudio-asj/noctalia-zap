# Arquitetura

```
┌───────────────────────── Noctalia Shell ─────────────────────────┐
│  plugin noctalia-zap (Luau)                                      │
│                                                                  │
│  [[service]] service.luau ──runStream──▶ noctalia-zapd (stdout)   │
│        │  ▲                                    ▲                 │
│        │  └──── comandos (runAsync/socket) ────┘                 │
│        ▼ noctalia.state (conversas, não lidas, status, qr)        │
│  [[widget]] bar.luau      → ícone + contador de não lidas        │
│  [[panel]]  panel.luau    → login QR · conversas · chat · envio  │
└──────────────────────────────────────────────────────────────────┘
                                  │
                       noctalia-zapd (Go + whatsmeow)
                                  │  WebSocket (protocolo multi-device)
                              servidores do WhatsApp
```

## noctalia-zapd (Go)

- Biblioteca: `go.mau.fi/whatsmeow`. A sessão fica num SQLite dentro do diretório de dados do plugin
  (`noctalia.pluginDataDir()`), passado por argumento.
- Faz login por QR. Gera o QR como **PNG**, que o painel mostra com `ui.image` (só aceita arquivo local).
- Saída: **um JSON por linha** no stdout, que o service lê com `noctalia.runStream(cmd, onLine)`.
- Entrada de comandos: a definir na fase 1. Opções:
  1. socket unix em `$XDG_RUNTIME_DIR/noctalia-zap.sock`, com o plugin chamando `noctalia-zapd ctl <cmd>` via
     `runAsync` com argv. Preferida, porque o `runStream` não oferece stdin.
  2. arquivo de fila no diretório de dados (fallback).

### Protocolo (rascunho)

Eventos (daemon → plugin):

```json
{"type":"status","state":"connecting|qr|connected|logged_out|error","detail":"..."}
{"type":"qr","png":"/abs/path/qr.png"}
{"type":"chats","items":[{"jid":"...","name":"...","unread":2,"last":"...","ts":1700000000}]}
{"type":"message","chat":"jid","from":"jid","name":"...","text":"...","ts":1700000000,"me":false}
```

Comandos (plugin → daemon):

```json
{"cmd":"send","chat":"jid","text":"..."}
{"cmd":"history","chat":"jid","limit":50}
{"cmd":"read","chat":"jid"}
{"cmd":"logout"}
```

## Plugin (Luau)

| Entrada | Arquivo | Papel |
|---|---|---|
| `[[service]]` | `service.luau` | Inicia o daemon, lê os eventos, mantém o `noctalia.state`, dispara `noctalia.notify` com botão "Responder" |
| `[[widget]]` | `bar.luau` | Ícone e contador (`barWidget.render` com `ui.box` e `ui.label`). Clique esquerdo abre o painel |
| `[[panel]]` | `panel.luau` | Telas: login (QR) → lista de conversas → conversa com campo de envio |

Persistência: só em `pluginDataDir()`. Configurações via `[[setting]]`: privacidade (esconder o texto nas
notificações), limite de histórico e caminho do binário.

## Segurança e privacidade

- Sessão e banco de mensagens nunca entram no git e nunca saem da máquina.
- Não logar o conteúdo das mensagens, nem no daemon nem no `noctalia.log`.
- Comandos sempre com argv, nunca com string de shell montada a partir do texto da mensagem.
- Risco de ban: cliente não oficial. Testar primeiro com um número secundário.
