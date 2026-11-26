# Notas da API de plugins do Noctalia (v5, Luau)

Fonte: docs.noctalia.dev/noctalia/plugins/development/ (manifest, entries, runtime-api, declarative-ui,
workflow), lidas em 2026-11-25, e plugins da comunidade. Noctalia instalado: 5.6.0 (`plugin_api` de 3 a 59).

## Limitações já confirmadas

- **Não há API para ler as notificações recebidas.** O histórico do shell é criptografado
  (`notification_history.enc`). Se precisar, o contorno testado é `dbus-monitor --session
  "interface='org.freedesktop.Notifications',member='Notify'"` via `runStream`.
- `runStream` não tem stdin nem handle para parar o processo (ele é encerrado no reload ou no stop do plugin).
  Também não avisa quando o processo sai e **só aceita string de shell** (`runAsync` aceita argv). Use `exec` e
  aspas simples escapadas em cada caminho (padrão do `androidconnect`), e um watchdog no `update()` para saber se
  o processo morreu.
- **`runStream` quebra linhas longas** (confirmado: uma linha de 98 KB chegou em pedaços de ~28 KB, JSON
  inválido). Mantenha cada evento abaixo de ~8 KB: o daemon pagina a lista de conversas e corta textos longos.
- Não há API para focar janelas. Use `niri msg action ...` via `runAsync`.
- Painéis não renderizam web. `ui.image` só aceita arquivo local.

## Manifesto (`plugin.toml`)

Campos obrigatórios: `id = "<autor>/<plugin>"` (minúsculo, `[a-z0-9][a-z0-9._-]*`), `name`, `version` (estrito,
MAJOR.MINOR.PATCH), `plugin_api` (inteiro). Opcionais: `license`, `icon`, `description`, `tags`,
`dependencies`.

```toml
[[widget]]  id = "bar"     entry = "bar.luau"
[[panel]]   id = "panel"   entry = "panel.luau"   placement = "floating"  position = "center"  width = 900  height = 640
[[service]] id = "service" entry = "service.luau"
[[setting]] key = "..." type = "bool|int|string|select|file|..." label_key = "settings.x.label" default = ...
```

Opções do painel: `width` (px ou "fill"), `height` (px, "fill" ou "content" + `max_height`), `placement`,
`position`, `keyboard_focus` (on_demand|exclusive|none), `dismiss_on_outside_click`, `persistent`,
`capture_keys`, `layer`.

Gates de versão: argv no `runAsync` ≥ 24, `require` ≥ 22, `getColor` ≥ 31, layer ≥ 36, height "content" ≥ 42,
onSuspend/onResume ≥ 48, `compositor.subscribe` ≥ 54, botões em notificação ≥ 56.

## Runtime

| Função | Uso |
|---|---|
| `noctalia.runAsync(argv [, cb])` | `cb(r)` com `r.exitCode`, `r.stdout`, `r.stderr`, `r.timedOut` |
| `noctalia.runStream(cmd, onLine)` | processo longo; `onLine(line)` para cada linha do stdout |
| `noctalia.commandExists(name)` / `processMatches(cb, ...)` | checagens |
| `noctalia.state.set/get/watch(key[, fn])` | dados compartilhados entre as entradas (VMs isoladas, valores copiados) |
| `noctalia.pluginDataDir()` | persistência (sobrevive a updates). **Não** gravar em `pluginDir()` |
| `noctalia.readFile/writeFile/fileExists/mkdirAll/listDir` | arquivos |
| `noctalia.json.encode/decode` | JSON |
| `noctalia.setUpdateInterval(ms)` | dispara o `update()` global (mínimo de 16 ms) |
| `noctalia.notify(title, body, {actions={{id,label}}, onAction="fn", context=...})` | notificação com até 6 botões |
| `noctalia.togglePanel("autor/plugin:panel")` | abrir o painel |
| `noctalia.getConfig(key)` | lê uma configuração declarada |
| `noctalia.tr(key)` | tradução (`translations/en.json`, obrigatório se houver settings) |
| `noctalia.getColor(role)` | hex de um papel de cor do tema |
| `noctalia.compositor.subscribe(opts, cb)` | eventos do niri (`cb(event, data)`; ex.: `WindowsChanged`) |
| `require("./lib/x.luau")` | módulos |

Ciclo de vida do service: o chunk roda ao carregar, `update()` roda no intervalo, e há `onExit(signal, reason)`,
`onSuspend()`, `onResume()` e `onConfigChanged()`.

## Widget de barra

`barWidget.setText/setGlyph/setImage/setTooltip/setColor/setGlyphColor/setVisible/isVertical/render(tree)`.
Cliques: `onClick()`, `onRightClick()`, `onMiddleClick()` (reservado para as configurações, salvo
`middle = "none"`) e `onScroll(axis, steps)`. Depois de chamar `render()`, `setText` e `setGlyph` deixam de valer.

## UI declarativa

`ui.column/row({gap, align, justify}, {...})`, `ui.scroll`, `ui.label({text, fontSize, fontWeight, color,
flexGrow})`, `ui.glyph`, `ui.image`, `ui.button({glyph|text, onClick})`, `ui.box({fill})`. Itens de lista
precisam de `key`. Painel: `panel.render(tree)`, `panel.close()`, `onOpen(ctx)`, `onClose()`, `onKey(chord,
pressed, repeated)`.

Cores: sempre por papel (`primary`, `on_surface`, `on_surface_variant`, `outline_variant`, `error`,
`surface_container`...).

## Confirmado nos tipos (`noctalia.d.luau` do repositório oficial)

- `noctalia.formatTime(padrão, ts?)` usa strftime (`"%H:%M"`); sem `ts`, usa a hora atual.
- `noctalia.trp(chave, n)` com plurais `{"one": "...", "other": "..."}` e `{count}` no texto.
- Traduções em português: `translations/pt-BR.json` (não `pt.json`).
- `ui.column/row` não têm `maxWidth` (só `ui.label`). Têm `fill`, `radius`, `padding*`, `onClick`, `onHover`,
  `tooltip`.
- Callbacks de UI podem ser closures (API ≥ 9); os argumentos chegam como string (`onHover` recebe `"true"`/`"false"`).
- `noctalia.openSettings()` abre as configurações do plugin (API 15).
- Clique do meio no widget é `settings-open-widget` por padrão; `onMiddleClick` só roda com `middle = "none"`.
- Settings `int` aceitam `min`/`max`; `advanced = true` esconde a opção na seção avançada.
- `ui.image` pode manter cache pelo caminho: o daemon grava cada QR com um nome novo (`qr-<n>.png`).

- Confirmado na prática: um `ui.column` com `width` fixo, sem `flexGrow`, dentro de um `ui.row` pode tomar o espaço
  livre do row e empurrar os irmãos com `flexGrow = 1` (avatar de inicial no cabeçalho). Declare `flexGrow = 0` em
  colunas/rows de tamanho fixo.

## Desenvolvimento local

- Pasta: `~/.local/share/noctalia/plugins/<plugin>/`, a fonte de maior precedência. Neste projeto, link
  simbólico para `plugin/zap`.
- Habilitar uma vez em Configurações → Plugins. Os `.luau` recarregam sozinhos.
- Testes: `noctalia msg panel-toggle <id>:panel`, `noctalia msg plugin <id>:<entrada> all <cmd> ...` e
  `noctalia plugins lint <pasta>`.
- Log: `~/.cache/noctalia/noctalia.log`.

## Paleta e UI (complemento)

- Paleta completa (28 papéis) e o mapeamento de uso no plugin: `docs/design.md`. Fonte:
  docs.noctalia.dev/noctalia/theming/palette/.
- `ui.input`: `value` (só semente; o campo é *uncontrolled*), `placeholder`, `multiline`, `submitOnEnter` (≥ 21),
  `frameVisible` (≥ 27), `focus` (só na criação), `onChange(text)` e `onSubmit(text)`. Só em painéis.
- `ui.scroll`: `stickToBottom`, `revealKey` (≥ 33), `onScroll(offset, max)`.
- `ui.image`: `fit` (contain/cover/stretch), `radius`, `onClick`. Para imagens remotas, baixe antes com
  `noctalia.download(url, dest, cb)`.
- `ui.button`: `variant` (default/primary/secondary/destructive/outline/ghost), `controlSize` (sm/md/lg),
  `tooltip`, `selected`.
- `onHover(on, key)` em row, column, box, image e button. `onClick` torna o container clicável e focável.
- Prop de estilo omitida num novo render volta ao padrão.
- Versão estável mais recente usada aqui: 5.6.0, com `plugin_api` até 56. O 57, 58 e 59 ainda não saíram.
