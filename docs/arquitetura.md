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
- Entrada de comandos: socket unix em `$XDG_RUNTIME_DIR/noctalia-zap.sock` (permissão 0600). O plugin chama
  `noctalia-zapd ctl <cmd> [args]` via `runAsync` com argv; o `ctl` monta o JSON, envia uma linha e imprime a
  resposta (uma linha JSON) no stdout. O `runStream` não oferece stdin, por isso o socket.
- Arquivos no diretório de dados: `session.db` (whatsmeow), `messages.db` (conversas e mensagens locais, porque o
  whatsmeow não guarda mensagens) e `qr.png` (só durante o pareamento).
- Reconexão: auto-reconnect do whatsmeow. Logout remoto apaga a sessão e as mensagens locais.

### Uso

```
noctalia-zapd run -data DIR [-socket PATH] [-history-limit 50] [-qr-terminal] [-log-level warn]
noctalia-zapd ctl [-socket PATH] status | chats | history <jid> [limit] | send <jid> <texto>
                                 | read <jid> | login | logout | quit
```

### Protocolo

Eventos (daemon → plugin, stdout):

```json
{"type":"status","state":"connecting|qr|connected|logged_out|error","detail":"..."}
{"type":"qr","png":"/abs/path/qr.png"}
{"type":"chats","page":0,"pages":3,"items":[{"jid":"...","name":"...","unread":2,"last":"...","last_kind":"text","ts":1700000000,"archived":false,"pinned":false,"avatar":"/abs/media/avatars/<h>.jpg"}]}
{"type":"message","id":"...","chat":"jid","from":"jid","name":"...","text":"...","kind":"text","ts":1700000000,"me":false,"backlog":false,"truncated":false,"muted":false}
```

- Cada linha fica abaixo de ~8 KB, porque o `runStream` do Noctalia quebra linhas longas.
- `chats` sai na partida, ao conectar e (com debounce de 300 ms) depois de qualquer mudança. É a lista completa,
  em páginas: a página 0 recomeça a lista e a última (`page == pages-1`) fecha. `last` vem cortado em 200 bytes.
  Fixadas primeiro; `archived`/`pinned` vêm do estado que o whatsmeow guarda (muda no celular, a lista é reenviada).
  Arquivadas não contam no contador da barra nem notificam (como "Manter conversas arquivadas" do WhatsApp).
- Conversas sem nenhuma mensagem (o history sync manda muitas) ficam fora da lista, como no WhatsApp.
- `name` de conversa individual sem nome gravado é resolvido na hora: contato salvo → nome de perfil do contato →
  nome de perfil da última mensagem recebida → `+telefone`. `@lid` sem nada conhecido vem vazio (o plugin mostra
  "Desconhecido").
- `avatar`: miniatura da foto de perfil (96 px), só se já foi baixada. `{"cmd":"avatar","chat":"jid"}` busca a foto
  se ela não foi conferida nas últimas 24 h (o painel chama ao abrir a conversa; nada roda em massa em segundo
  plano). Foto oculta ou ausente fica registrada como "sem foto" por 24 h. O evento `Picture` do whatsmeow zera o
  prazo daquela conversa. Arquivos em `media/avatars/`, apagados no logout.
- `message.text` vem cortado em 2000 bytes (`truncated: true`); o texto inteiro sai pelo `history`.
  `muted`: conversa silenciada no celular, não notificar.
- `kind`: `text`, `image`, `video`, `audio`, `document`, `sticker`, `location`, `contact`, `poll`. Para tipos que
  não são texto, `text` traz a legenda/nome do arquivo (pode vir vazio); o plugin traduz o rótulo.
- `detail` do status: `reconnecting`, `keepalive_timeout`, `paired`, `qr_timeout`, `pair_failed`,
  `logged_out_remotely`, `stream_replaced`, `temporary_ban`, `client_outdated`, `connect_failure`...
  `connect_failed`. Depois de `qr_timeout`, `connect_failed` ou `logged_out`, o plugin chama `ctl login` para gerar
  um QR novo. Com sessão já pareada e desconectada (`stream_replaced`, `connect_failure`...), `login` reconecta.
- `backlog: true` marca mensagem que chegou atrasada (sync offline ou mais de 5 min de idade). Não notificar.
- Chat sem nome conhecido vem com o telefone (`+55...`) em `name`; o nome real chega depois, com a sincronização de
  contatos. Chats `@lid` são convertidos para o JID de telefone quando o mapeamento é conhecido.
- Mensagens de status (stories), canais, reações e edições são ignoradas por enquanto.

Comandos (plugin → daemon, socket; uma linha JSON de ida e uma de volta):

```json
{"cmd":"send","chat":"jid","text":"..."}      → {"ok":true,"data":{"id":"...","ts":1700000000}}
{"cmd":"history","chat":"jid","limit":50}      → {"ok":true,"data":[mensagens, da mais antiga à mais nova]}
{"cmd":"read","chat":"jid"}                    → {"ok":true}  (erro "not connected" sem conexão; nada é zerado)
{"cmd":"login"}                                → {"ok":true,"data":{"action":"pairing|reconnect"}}
{"cmd":"logout"}                               → {"ok":true,"data":{"remote":false}}  (false: remover no celular)
{"cmd":"chats"} {"cmd":"status"} {"cmd":"quit"}
```

Erro: `{"ok":false,"error":"..."}` e o `ctl` sai com código 1. `send` também emite o evento `message` e o `read`
atualiza `chats` no stdout, então o service não precisa tratar a resposta além do erro.

Texto do `send` fora do argv: o `runAsync` não tem stdin, então o plugin grava o texto num arquivo dentro de
`pluginDataDir()` (diretório 0700) e chama `ctl send <jid> --file <caminho>`. O `ctl` lê e apaga o arquivo. No
terminal, `ctl send <jid> -` lê do stdin.

### Mídia (fase 4)

Mensagens de mídia (`image`, `video`, `audio`, `document`, `sticker`) ganham o campo opcional `media`, tanto no
evento `message` quanto no `history`:

```json
"media": {"thumb":"/abs/media/thumbs/<h>.jpg","w":1080,"h":1920,"seconds":12,"size":204800,
          "mime":"image/jpeg","ptt":false,"file":"/abs/media/<h>.jpg"}
```

- Figurinha com som: chega embrulhada em `AudioStickerMessage` (o whatsmeow não desembrulha; o daemon sim) e o
  `StickerMessage` traz um `AudioMessage`. `media.sound: true` e `media.seconds`. `play` numa figurinha toca o
  som (baixado para o cache como `<h de id+"#sound">`), não a imagem.
- Todos os campos são opcionais. `thumb` só existe se a mensagem trouxe prévia (`JPEGThumbnail`; figurinha:
  `PngThumbnail` em `.png`) e o arquivo foi gravado. `file` só existe se a mídia já foi baixada para o cache.
- `<h>` = os primeiros 32 caracteres hex de `sha256(chat + "|" + id)`. Diretório `media/` 0700 dentro do
  diretório de dados; arquivos de cache 0600. Extensão a partir do `mime` (`audio/ogg` → `.ogg`; desconhecido →
  `.bin`).
- **Onde o arquivo baixado fica:**
  - `audio` → cache: `media/<h>.<ext>`. Entra no limite `-media-cache-mb` e na limpeza automática.
  - `image`, `video`, `document`, `sticker` → pasta de downloads do usuário: flag `-download-dir DIR`. Se vazia,
    o padrão é `XDG_DOWNLOAD_DIR` de `~/.config/user-dirs.dirs` (ou `~/Downloads` se não existir) + `/WhatsApp`.
    Criada com `MkdirAll` 0755 se não existir. Nunca é limpa pelo daemon.
  - Nome na pasta de downloads: documento usa o nome original (`FileName`), sem `/`, sem `..`, sem caracteres de
    controle, até 120 bytes; o resto usa `WhatsApp <tipo> AAAA-MM-DD HH.MM.SS.<ext>` com o horário da mensagem
    (`tipo`: Image, Video, Sticker). Se já existir um arquivo com o nome, acrescenta ` (1)`, ` (2)`... antes da
    extensão. Arquivo criado com `O_EXCL` (nunca sobrescreve nada do usuário).
  - O caminho final fica gravado no banco (coluna `file`), e é ele que volta em `media.file` e no comando `media`.
    Se o usuário apagar ou mover o arquivo, `media.file` deixa de vir e o próximo `media` baixa de novo.
- O banco guarda, para mensagens de mídia, o proto da mensagem (`raw BLOB`, necessário para baixar depois) e os
  metadados (`media TEXT`, JSON). Migração com `ALTER TABLE ... ADD COLUMN` quando a coluna não existir.

Comandos novos:

```json
{"cmd":"media","chat":"jid","id":"msgid"}  → {"ok":true,"data":{"path":"/abs/media/<h>.jpg"}}
{"cmd":"play","chat":"jid","id":"msgid"}   → {"ok":true}   (baixa se preciso e toca)
{"cmd":"stop"}                             → {"ok":true}
```

- Erros de `media`/`play`: `not_media`, `not_found`, `not_stored` (mensagem guardada antes da fase 4, sem as
  chaves de download), `media_unavailable` (404/410: mídia expirou no servidor),
  `not connected` (precisa baixar e está offline), `no_player`.
- `ctl media <jid> <id>`, `ctl play <jid> <id>`, `ctl stop`.
- Player: o daemon mantém no máximo um processo, com argv fixo: `mpv --no-video --really-quiet --no-terminal
  <arquivo>`; sem mpv, `ffplay -nodisp -autoexit -loglevel quiet <arquivo>`; sem os dois, erro `no_player`.
  `play` mata o anterior. Evento ao começar e ao terminar (fim, `stop` ou novo `play`):
  `{"type":"playback","chat":"jid","id":"msgid","state":"playing|stopped"}`.
- Mídia antiga sem dados de download (`not_stored`): `{"cmd":"backfill","chat":"jid"}` pede ao celular o
  histórico recente da conversa (pedido sob demanda do whatsmeow, ancorado na mensagem mais nova; até
  `-history-limit` mensagens). Responde `{"requested":true|false}` (false: nada a recuperar ou pedido feito há
  menos de 10 min). A resposta chega como history sync: mensagens já guardadas ganham `raw`/`media`, as que
  faltavam entram, o contador de não lidas não muda. O daemon baixa em seguida as miniaturas que faltam
  (`DownloadThumbnail` em foto/vídeo; figurinha até 512 KB baixada inteira e convertida de WebP para PNG) e
  então emite `{"type":"backfill","chat":"jid"}`; o painel recarrega a conversa aberta. O painel pede
  `backfill` ao abrir cada conversa. Só funciona com o celular online.
- Cache: flag `-media-cache-mb N` (padrão 500). Depois de cada download para o cache, apaga os arquivos mais
  antigos (mtime) de `media/` (não `media/thumbs/`) até caber, nunca o que acabou de baixar. Logout apaga `media/`
  inteiro, mas nunca a pasta de downloads do usuário.

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
