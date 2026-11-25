# Padrão visual

Objetivo: o plugin tem que parecer parte do Noctalia. Ao trocar o tema (Theme Switcher, claro/escuro), tudo
acompanha sem nenhum ajuste. Valores tirados da documentação oficial (`/noctalia/theming/palette/`,
`declarative-ui`) e dos plugins oficiais.

## Cores: só papéis do tema

Nunca use hex fixo. Os papéis vêm da paleta Material 3 do Noctalia (28 papéis). Aceitam alfa: `"primary/0.6"`.

| Uso no Noctalia Zap | Papel |
|---|---|
| Texto principal (nome do contato, mensagem) | `on_surface` |
| Texto secundário (prévia, hora, status) | `on_surface_variant` |
| Destaque: badge de não lidas, ícone ativo, seleção | `primary` (texto sobre ele: `on_primary`) |
| Balão de mensagem **minha** | `primary_container` / texto `on_primary_container` |
| Balão de mensagem **recebida** | `surface_container_high` / texto `on_surface` |
| Fundo de item/cartão | `surface_container` (hover: `hover`, texto `on_hover`) |
| Área de digitação | `surface_container_highest` |
| Divisórias e bordas | `outline_variant` |
| Erro / desconectado | `error` (fundo `error_container`, texto `on_error_container`) |
| Acento alternativo (ex.: grupos) | `secondary` / `tertiary` |

Papéis aposentados, **não usar**: `surface_variant` e `outline`. Precisa de `plugin_api >= 43`.

## Medidas (padrão dos plugins oficiais)

Não existem tokens de raio ou espaçamento na API. Use estes números fixos em todo o plugin, sempre os mesmos:

| Propriedade | Valores |
|---|---|
| `gap` | 4 · 6 · **8** (padrão) · 12 |
| `padding` | 8 · **10** (padrão) · 16 |
| `radius` | 6 (botões, entradas) · **8** (cartões, balões) · 12 (painel interno) · metade da largura (avatar redondo) |
| `fontSize` | 11 (hora, legenda) · **12** (corpo) · 13 (nome) · 16 (título da tela) |
| `fontWeight` | `"medium"` em nomes · `"bold"` em títulos e contadores |
| `controlSize` | `"md"` (padrão) · `"sm"` em barras de ferramentas |

Centralize esses números num módulo (`lib/style.luau`) e importe com `require`. Não espalhe números soltos.

## Componentes

- **Botões:** `variant = "ghost"` para ações de ícone (é o mais usado nos oficiais), `"primary"` só para a ação
  principal da tela (ex.: Enviar) e `"destructive"` para sair/desconectar.
- **Ícones:** `ui.glyph` / `glyph =` com nomes **Tabler** em kebab-case (`message-circle`, `send`, `search`,
  `arrow-left`, `logout`, `qrcode`, `users`). Ícone do plugin: `brand-whatsapp` se existir, senão
  `message-circle`.
- **Entrada de texto:** `ui.input` com `submitOnEnter = true` (Enter envia, Shift+Enter quebra linha),
  `multiline = true`, `frameVisible = false` dentro de um container próprio. É *uncontrolled*: use `key` estável e
  troque a `key` para limpar o campo depois de enviar.
- **Listas:** `ui.scroll` com um `ui.row` por item, sempre com `key = jid`. Na conversa, `stickToBottom = true`.
- **Avatar:** `ui.image` com `fit = "cover"` e `radius` = metade do tamanho. Sem foto: `ui.box` redondo com a
  inicial (`primary_container` / `on_primary_container`).
- **Badge:** não existe componente. Use `ui.box` (`fill = "primary"`, `radius` alto) com um `ui.label`
  (`on_primary`, `bold`, 11).
- **Separadores:** `ui.separator` com `color = "outline_variant"`.
- **Tooltips** em todo botão só de ícone.

## Barra (widget)

- Uma linha de altura. Herda a fonte da barra, então não defina `fontFamily`.
- Respeite `barWidget.isVertical()`: na vertical, só o ícone com o badge por cima.
- Ícone `on_surface` normal. Com não lidas, o badge fica em `primary`. Desconectado, o ícone fica em `error`.
- Sem mensagens: só o ícone, ou oculto se a setting `hide_when_empty` estiver ligada.
- Cliques: esquerdo abre o painel; direito abre a última conversa ou um menu; meio fica com o padrão do Noctalia
  (configurações do plugin).

## Painel

- `placement = "floating"`, `position = "center"`, cerca de 900 × 640 (cabe no notebook de 1366×768),
  `keyboard_focus = "on_demand"`.
- Telas: **Login** (QR centralizado + status) → **Conversas** (busca + lista) → **Conversa** (cabeçalho com
  voltar, mensagens, entrada).
- Layout largo (≥ 800 px): lista à esquerda (cerca de 300 px) e conversa à direita, lado a lado.
- Estados obrigatórios em toda tela: carregando, vazio, erro/desconectado, cada um com ícone, texto curto e
  ação.
- Atalhos (`onKey`): `Escape` volta ou fecha, `Ctrl+F` busca.

## Notificações

`noctalia.notify(nome, prévia, { actions = {{id="reply",label=tr(...)},{id="open",...}} })`. Com a setting de
privacidade ligada, o corpo vira apenas "Nova mensagem".
