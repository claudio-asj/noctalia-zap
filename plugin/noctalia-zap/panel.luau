--!nonstrict
-- Panel: QR login, then the chat list. The conversation view arrives in phase 3.

local common = require("./lib/common.luau")
local style = require("./lib/style.luau")

local status = noctalia.state.get(common.KEY_STATUS) or { state = "connecting", detail = "" }
local qr = noctalia.state.get(common.KEY_QR)
local chats = noctalia.state.get(common.KEY_CHATS) or {}
local daemon = noctalia.state.get(common.KEY_DAEMON)

local isOpen = false
local query = ""
local selected = nil
local hovered = nil
local confirmLogout = false
local busy = false
local actionError = nil

local render -- forward declaration

-- Runs a daemon command. Only fixed words and a JID go into argv.
local function ctl(args: { string }, onDone: ((boolean) -> ())?)
  if daemon == nil or busy then
    return
  end
  busy = true
  actionError = nil
  local argv = { daemon, "ctl" }
  for _, a in args do
    table.insert(argv, a)
  end
  noctalia.runAsync(argv, function(r)
    busy = false
    local ok = r.exitCode == 0
    if not ok then
      actionError = noctalia.tr("panel.error.command")
    end
    if onDone then
      onDone(ok)
    end
    render()
  end, 30000)
end

-- ── Pieces ────────────────────────────────────────────────────────────────

local function centered(children)
  return ui.column({ flexGrow = 1, align = "center", justify = "center", gap = style.gap.lg }, children)
end

local function emptyState(glyph: string, title: string, body: string?, action)
  local children = {
    ui.glyph({ name = glyph, size = 40, color = "on_surface_variant" }),
    ui.label({ text = title, fontSize = style.font.title, fontWeight = "bold", color = "on_surface" }),
  }
  if body and body ~= "" then
    table.insert(children, ui.label({
      text = body,
      fontSize = style.font.body,
      color = "on_surface_variant",
      textAlign = "center",
      maxWidth = 420,
    }))
  end
  if action then
    table.insert(children, action)
  end
  if actionError then
    table.insert(children, ui.label({ text = actionError, fontSize = style.font.body, color = "error" }))
  end
  return centered(children)
end

local function statusText(): string
  local detail = status.detail or ""
  if detail ~= "" then
    local key = "detail." .. detail
    local text = noctalia.tr(key)
    if text ~= key then
      return text
    end
  end
  return noctalia.tr("status." .. (status.state or "error"))
end

local function loginButton(labelKey: string)
  return ui.button({
    text = noctalia.tr(labelKey),
    glyph = "qrcode",
    variant = "primary",
    enabled = not busy,
    onClick = function()
      ctl({ "login" })
    end,
  })
end

local function qrScreen()
  local steps = ui.column({ gap = style.gap.md, width = 320 }, {
    ui.label({ text = noctalia.tr("panel.qr.title"), fontSize = style.font.title, fontWeight = "bold", color = "on_surface" }),
    ui.label({ text = noctalia.tr("panel.qr.step1"), fontSize = style.font.body, color = "on_surface_variant" }),
    ui.label({ text = noctalia.tr("panel.qr.step2"), fontSize = style.font.body, color = "on_surface_variant" }),
    ui.label({ text = noctalia.tr("panel.qr.step3"), fontSize = style.font.body, color = "on_surface_variant" }),
    ui.label({ text = noctalia.tr("panel.qr.warning"), fontSize = style.font.caption, color = "on_surface_variant" }),
  })
  -- The QR needs a light quiet zone to scan in dark themes too.
  local code = ui.column({ padding = style.padding.lg, radius = style.radius.inner, fill = "on_surface" }, {
    ui.image({ key = qr, path = qr, width = style.qrSize, height = style.qrSize, fit = "contain" }),
  })
  return ui.row({ flexGrow = 1, align = "center", justify = "center", gap = 32 }, { code, steps })
end

local function avatar(name: string)
  local size = style.avatar
  return ui.column({
    width = size,
    height = size,
    radius = size / 2,
    fill = "primary_container",
    align = "center",
    justify = "center",
  }, {
    ui.label({ text = common.initial(name), color = "on_primary_container", fontWeight = "bold", fontSize = style.font.name }),
  })
end

local function badge(count: number)
  return ui.row({ fill = "primary", radius = style.radius.card, paddingH = style.gap.sm, align = "center" }, {
    ui.label({
      text = if count > 99 then "99+" else tostring(count),
      color = "on_primary",
      fontWeight = "bold",
      fontSize = style.font.caption,
    }),
  })
end

local function chatRow(c)
  local isSelected = selected == c.jid
  local fill = nil
  if isSelected then
    fill = "primary/0.15"
  elseif hovered == c.jid then
    fill = "hover"
  end
  local bottom = {
    ui.label({
      text = common.preview(c.last, c.lastKind),
      fontSize = style.font.body,
      color = "on_surface_variant",
      maxLines = 1,
      flexGrow = 1,
    }),
  }
  if c.unread > 0 then
    table.insert(bottom, badge(c.unread))
  end
  return ui.row({
    key = c.jid,
    gap = style.gap.md,
    padding = style.padding.sm,
    radius = style.radius.card,
    align = "center",
    fill = fill,
    onHover = function(state, _key)
      hovered = if state == "true" then c.jid else nil
      render()
    end,
    onClick = function()
      selected = c.jid
      render()
    end,
  }, {
    avatar(c.name),
    ui.column({ flexGrow = 1, gap = style.gap.xs }, {
      ui.row({ gap = style.gap.sm, align = "center" }, {
        ui.label({
          text = c.name,
          fontSize = style.font.name,
          fontWeight = "medium",
          color = "on_surface",
          maxLines = 1,
          flexGrow = 1,
        }),
        ui.label({ text = common.shortTime(c.ts), fontSize = style.font.caption, color = "on_surface_variant" }),
      }),
      ui.row({ gap = style.gap.sm, align = "center" }, bottom),
    }),
  })
end

local function chatList()
  local rows = {}
  for _, c in chats do
    if common.matches(c.name, query) then
      table.insert(rows, chatRow(c))
    end
  end
  if #rows == 0 then
    local text = if query ~= "" then noctalia.tr("panel.list.no_match") else noctalia.tr("panel.list.empty")
    return ui.column({ flexGrow = 1, align = "center", justify = "center" }, {
      ui.label({ text = text, fontSize = style.font.body, color = "on_surface_variant", textAlign = "center" }),
    })
  end
  return ui.scroll({ flexGrow = 1, gap = style.gap.xs }, rows)
end

local function listHeader()
  local children = {
    ui.column({ flexGrow = 1, gap = 2 }, {
      ui.label({ text = noctalia.tr("panel.list.title"), fontSize = style.font.title, fontWeight = "bold", color = "on_surface" }),
    }),
    ui.button({
      glyph = "logout",
      variant = "ghost",
      controlSize = "sm",
      tooltip = noctalia.tr("panel.logout.button"),
      onClick = function()
        confirmLogout = true
        render()
      end,
    }),
  }
  if status.state ~= "connected" then
    table.insert(children[1].children, ui.label({
      text = statusText(),
      fontSize = style.font.caption,
      color = if common.isProblem(status.state) then "error" else "on_surface_variant",
    }))
  end
  return ui.row({ gap = style.gap.md, align = "center" }, children)
end

local function logoutConfirm()
  return ui.column({ gap = style.gap.md, padding = style.padding.md, radius = style.radius.card, fill = "error_container" }, {
    ui.label({ text = noctalia.tr("panel.logout.confirm"), fontSize = style.font.body, color = "on_error_container" }),
    ui.row({ gap = style.gap.md, justify = "end" }, {
      ui.button({
        text = noctalia.tr("panel.logout.cancel"),
        variant = "ghost",
        controlSize = "sm",
        onClick = function()
          confirmLogout = false
          render()
        end,
      }),
      ui.button({
        text = noctalia.tr("panel.logout.button"),
        variant = "destructive",
        controlSize = "sm",
        enabled = not busy,
        onClick = function()
          ctl({ "logout" }, function(_ok)
            confirmLogout = false
            selected = nil
          end)
        end,
      }),
    }),
  })
end

local function conversationPane()
  if selected == nil then
    return ui.column({ flexGrow = 1, align = "center", justify = "center", gap = style.gap.md }, {
      ui.glyph({ name = "message-circle", size = 40, color = "on_surface_variant" }),
      ui.label({ text = noctalia.tr("panel.chat.pick"), fontSize = style.font.body, color = "on_surface_variant" }),
    })
  end
  local name = ""
  for _, c in chats do
    if c.jid == selected then
      name = c.name
      break
    end
  end
  return ui.column({ flexGrow = 1, gap = style.gap.lg }, {
    ui.row({ gap = style.gap.md, align = "center" }, {
      avatar(name),
      ui.label({ text = name, fontSize = style.font.title, fontWeight = "bold", color = "on_surface", maxLines = 1, flexGrow = 1 }),
    }),
    ui.separator({ color = "outline_variant" }),
    ui.column({ flexGrow = 1, align = "center", justify = "center" }, {
      ui.label({ text = noctalia.tr("panel.chat.soon"), fontSize = style.font.body, color = "on_surface_variant" }),
    }),
  })
end

local function mainScreen()
  local left = {
    listHeader(),
    ui.input({
      key = "search",
      placeholder = noctalia.tr("panel.list.search"),
      onChange = function(text)
        query = text
        render()
      end,
    }),
    chatList(),
  }
  if confirmLogout then
    table.insert(left, logoutConfirm())
  end
  return ui.row({ flexGrow = 1, gap = style.gap.lg }, {
    ui.column({ width = style.listWidth, gap = style.gap.md }, left),
    ui.separator({ color = "outline_variant", orientation = "vertical" }),
    conversationPane(),
  })
end

-- ── Screen choice ─────────────────────────────────────────────────────────

function render()
  if not isOpen then
    return
  end
  local state = status.state
  local tree
  if state == "missing" or daemon == nil then
    tree = emptyState(
      "alert-triangle",
      noctalia.tr("panel.missing.title"),
      noctalia.tr("panel.missing.body"),
      ui.button({
        text = noctalia.tr("panel.missing.settings"),
        glyph = "settings",
        onClick = function()
          noctalia.openSettings()
        end,
      })
    )
  elseif state == "qr" and qr ~= nil then
    tree = qrScreen()
  elseif state == "logged_out" then
    tree = emptyState("brand-whatsapp", noctalia.tr("panel.login.title"), statusText(), loginButton("panel.login.button"))
  elseif state == "error" and #chats == 0 then
    tree = emptyState("plug-connected-x", noctalia.tr("status.error"), statusText(), loginButton("panel.retry"))
  elseif #chats == 0 and state ~= "connected" then
    tree = emptyState("loader-2", statusText(), nil, nil)
  else
    tree = mainScreen()
  end
  panel.render(ui.column({ flexGrow = 1 }, { tree }))
end

noctalia.state.watch(common.KEY_STATUS, function(value)
  status = value or { state = "error", detail = "" }
  render()
end)
noctalia.state.watch(common.KEY_QR, function(value)
  qr = value
  render()
end)
noctalia.state.watch(common.KEY_CHATS, function(value)
  chats = value or {}
  render()
end)
noctalia.state.watch(common.KEY_DAEMON, function(value)
  daemon = value
  render()
end)

function onOpen(_context)
  isOpen = true
  actionError = nil
  render()
end

function onClose()
  isOpen = false
  hovered = nil
  confirmLogout = false
end
