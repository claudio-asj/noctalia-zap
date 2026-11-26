--!nonstrict
-- Panel: QR login, the chat list and the open conversation.
-- Message text only travels through files in the private data dir, never argv.

local common = require("./lib/common.luau")
local style = require("./lib/style.luau")

local status = noctalia.state.get(common.KEY_STATUS) or { state = "connecting", detail = "" }
local qr = noctalia.state.get(common.KEY_QR)
local chats = noctalia.state.get(common.KEY_CHATS) or {}
local daemon = noctalia.state.get(common.KEY_DAEMON)

local dataDir = noctalia.pluginDataDir()

local HISTORY_LIMIT = 50
local MAX_LOADED = 200 -- messages kept for the open chat
-- Rows drawn in the chat list. A scroll takes at most 256 children and every
-- row costs render time; search reaches the rest.
local MAX_ROWS = 80

local isOpen = false
local query = ""
local selected = nil
local confirmLogout = false
local busy = false
local actionError = nil

-- Open conversation.
local messages = {} -- oldest first
local messageIds = {} -- id -> true, to drop duplicates
local loadingHistory = false
local historyError = false
local lastSeq = (noctalia.state.get(common.KEY_MESSAGES) or {}).seq or 0
local lastOpenChat = 0
local draft = ""
local composerRev = 0
local sending = false
local sendError = false
local scrollRev = 0

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

-- ── Conversation ──────────────────────────────────────────────────────────

local function publishActive()
  noctalia.state.set(common.KEY_ACTIVE_CHAT, if isOpen then selected else nil)
end

local function chatByJid(jid)
  for _, c in chats do
    if c.jid == jid then
      return c
    end
  end
  return nil
end

local function addMessage(m)
  if m.id ~= "" and messageIds[m.id] then
    return false
  end
  if m.id ~= "" then
    messageIds[m.id] = true
  end
  table.insert(messages, m)
  while #messages > MAX_LOADED do
    local old = table.remove(messages, 1)
    messageIds[old.id] = nil
  end
  return true
end

-- Sends read receipts for the open chat. Fire and forget: the chat list
-- updates through the service when the daemon clears the counter.
local function markRead(jid)
  if daemon == nil then
    return
  end
  noctalia.runAsync({ daemon, "ctl", "read", jid }, function(_r) end, 30000)
end

local function loadHistory(jid, limit)
  if daemon == nil then
    return
  end
  loadingHistory = true
  historyError = false
  noctalia.runAsync({ daemon, "ctl", "history", jid, tostring(limit) }, function(r)
    if selected ~= jid then
      return -- the user moved on
    end
    loadingHistory = false
    if r.stdoutTruncated and limit > 10 then
      -- Too much text for one read: ask for fewer messages.
      loadHistory(jid, math.floor(limit / 2))
      return
    end
    local resp = noctalia.json.decode(r.stdout or "")
    if r.exitCode ~= 0 or type(resp) ~= "table" or type(resp.data) ~= "table" then
      historyError = true
      render()
      return
    end
    -- Live messages that arrived meanwhile are kept; history goes before them.
    local live = messages
    messages, messageIds = {}, {}
    for _, m in resp.data do
      addMessage({
        id = m.id or "",
        from = m.from or "",
        name = m.name or "",
        text = m.text or "",
        kind = m.kind or "text",
        ts = tonumber(m.ts) or 0,
        me = m.me == true,
      })
    end
    for _, m in live do
      addMessage(m)
    end
    scrollRev += 1
    render()
  end, 30000)
end

local function selectChat(jid)
  if jid == selected then
    return
  end
  selected = jid
  messages, messageIds = {}, {}
  draft = ""
  composerRev += 1
  sendError = false
  publishActive()
  loadHistory(jid, HISTORY_LIMIT)
  local c = chatByJid(jid)
  if c and c.unread > 0 then
    markRead(jid)
  end
  render()
end

local function send()
  local text = noctalia.string.trim(draft)
  if text == "" or sending or selected == nil or daemon == nil or dataDir == nil then
    return
  end
  -- The text goes through a file in the private data dir, never argv.
  local path = `{dataDir}/outbox-{math.floor(noctalia.nowMs())}.txt`
  local ok = noctalia.writeFile(path, text)
  if not ok then
    sendError = true
    render()
    return
  end
  sending = true
  sendError = false
  local jid = selected
  noctalia.runAsync({ daemon, "ctl", "send", jid, "--file", path }, function(r)
    sending = false
    noctalia.removeFile(path) -- ctl removes it; this covers early failures
    if r.exitCode == 0 then
      if selected == jid then
        draft = ""
        composerRev += 1 -- a new key empties the uncontrolled input
      end
    else
      sendError = true
    end
    render()
  end, 60000)
  render()
end

-- Live messages from the service.
local function onMessages(value)
  if type(value) ~= "table" or type(value.items) ~= "table" then
    return
  end
  local changed = false
  local incoming = false
  for _, m in value.items do
    if m.seq > lastSeq then
      lastSeq = m.seq
      if m.chat == selected and addMessage(m) then
        changed = true
        incoming = incoming or not m.me
      end
    end
  end
  if changed then
    scrollRev += 1
    if incoming and isOpen then
      markRead(selected)
    end
    render()
  end
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
  local fill = if selected == c.jid then "primary/0.15" else nil
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
    onClick = function()
      selectChat(c.jid)
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
  local more = false
  for _, c in chats do
    if common.matches(c.name, query) then
      if #rows >= MAX_ROWS then
        more = true
        break
      end
      table.insert(rows, chatRow(c))
    end
  end
  if #rows == 0 then
    local text = if query ~= "" then noctalia.tr("panel.list.no_match") else noctalia.tr("panel.list.empty")
    return ui.column({ flexGrow = 1, align = "center", justify = "center" }, {
      ui.label({ text = text, fontSize = style.font.body, color = "on_surface_variant", textAlign = "center" }),
    })
  end
  if more then
    table.insert(rows, ui.label({
      key = "more",
      text = noctalia.tr("panel.list.more"),
      fontSize = style.font.caption,
      color = "on_surface_variant",
      textAlign = "center",
    }))
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

local function dayLabel(ts: number): string
  if noctalia.formatTime("%Y-%m-%d", ts) == noctalia.formatTime("%Y-%m-%d") then
    return noctalia.tr("panel.chat.today")
  end
  return noctalia.formatTime("%d/%m/%Y", ts)
end

local function bubble(m, showSender: boolean)
  local mine = m.me
  local children = {}
  if showSender and not mine and m.name ~= "" then
    table.insert(children, ui.label({
      text = m.name,
      fontSize = style.font.caption,
      fontWeight = "medium",
      color = "secondary",
      maxLines = 1,
    }))
  end
  local textColor = if mine then "on_primary_container" else "on_surface"
  if m.kind ~= "text" then
    table.insert(children, ui.row({ gap = style.gap.xs, align = "center" }, {
      ui.glyph({ name = common.kindGlyph(m.kind), size = 14, color = textColor }),
      ui.label({ text = noctalia.tr("kind." .. m.kind), fontSize = style.font.body, color = textColor }),
    }))
  end
  if m.text ~= "" then
    table.insert(children, ui.label({
      text = m.text,
      fontSize = style.font.body,
      color = textColor,
      maxWidth = style.bubbleMax,
    }))
  end
  local footer = noctalia.formatTime("%H:%M", m.ts)
  if m.truncated then
    footer = noctalia.tr("panel.chat.truncated") .. " · " .. footer
  end
  table.insert(children, ui.label({
    text = footer,
    fontSize = style.font.caption,
    color = if mine then "on_primary_container/0.7" else "on_surface_variant",
    textAlign = "end",
  }))
  return ui.row({ key = m.id, justify = if mine then "end" else "start" }, {
    ui.column({
      gap = style.gap.xs,
      padding = style.padding.sm,
      radius = style.radius.card,
      fill = if mine then "primary_container" else "surface_container_high",
    }, children),
  })
end

local function messageList(group: boolean)
  if #messages == 0 then
    local text
    if loadingHistory then
      text = noctalia.tr("panel.chat.loading")
    elseif historyError then
      text = noctalia.tr("panel.chat.history_error")
    else
      text = noctalia.tr("panel.chat.empty")
    end
    return ui.column({ flexGrow = 1, align = "center", justify = "center" }, {
      ui.label({ text = text, fontSize = style.font.body, color = "on_surface_variant" }),
    })
  end
  local rows = {}
  local lastDay = nil
  for _, m in messages do
    local day = noctalia.formatTime("%Y-%m-%d", m.ts)
    if day ~= lastDay then
      lastDay = day
      table.insert(rows, ui.row({ key = "day-" .. day, justify = "center" }, {
        ui.label({ text = dayLabel(m.ts), fontSize = style.font.caption, color = "on_surface_variant" }),
      }))
    end
    table.insert(rows, bubble(m, group))
  end
  return ui.scroll({ flexGrow = 1, gap = style.gap.sm, stickToBottom = true, scrollToBottomRev = scrollRev }, rows)
end

local function composer()
  local connected = status.state == "connected"
  local children = {
    ui.input({
      key = "composer-" .. composerRev,
      placeholder = noctalia.tr(if connected then "panel.chat.placeholder" else "panel.chat.offline"),
      multiline = true,
      submitOnEnter = true,
      frameVisible = false,
      flexGrow = 1,
      focus = true,
      enabled = connected and not sending,
      onChange = function(text)
        draft = text
      end,
      onSubmit = function(text)
        draft = text
        send()
      end,
    }),
    ui.button({
      glyph = "send",
      variant = "primary",
      tooltip = noctalia.tr("panel.chat.send"),
      enabled = connected and not sending,
      onClick = function()
        send()
      end,
    }),
  }
  local box = ui.row({
    gap = style.gap.md,
    padding = style.padding.sm,
    radius = style.radius.card,
    fill = "surface_container_highest",
    align = "center",
  }, children)
  if sendError then
    return ui.column({ gap = style.gap.xs }, {
      ui.label({ text = noctalia.tr("panel.chat.send_error"), fontSize = style.font.caption, color = "error" }),
      box,
    })
  end
  return box
end

local function conversationPane()
  if selected == nil then
    return ui.column({ flexGrow = 1, align = "center", justify = "center", gap = style.gap.md }, {
      ui.glyph({ name = "message-circle", size = 40, color = "on_surface_variant" }),
      ui.label({ text = noctalia.tr("panel.chat.pick"), fontSize = style.font.body, color = "on_surface_variant" }),
    })
  end
  local c = chatByJid(selected)
  local name = if c then c.name else selected
  return ui.column({ flexGrow = 1, gap = style.gap.md }, {
    ui.row({ gap = style.gap.md, align = "center" }, {
      avatar(name),
      ui.label({ text = name, fontSize = style.font.title, fontWeight = "bold", color = "on_surface", maxLines = 1, flexGrow = 1 }),
      ui.button({
        glyph = "x",
        variant = "ghost",
        controlSize = "sm",
        tooltip = noctalia.tr("panel.chat.close"),
        onClick = function()
          selected = nil
          messages, messageIds = {}, {}
          publishActive()
          render()
        end,
      }),
    }),
    ui.separator({ color = "outline_variant" }),
    messageList(common.isGroup(selected)),
    composer(),
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
noctalia.state.watch(common.KEY_MESSAGES, onMessages)

-- A notification asked to open a chat.
local function checkOpenChat(value)
  if type(value) == "table" and type(value.jid) == "string" and (value.at or 0) > lastOpenChat then
    lastOpenChat = value.at or 0
    selectChat(value.jid)
  end
end
noctalia.state.watch(common.KEY_OPEN_CHAT, checkOpenChat)

function onOpen(_context)
  isOpen = true
  actionError = nil
  noctalia.state.set(common.KEY_PANEL_OPEN, true)
  publishActive()
  checkOpenChat(noctalia.state.get(common.KEY_OPEN_CHAT))
  if selected ~= nil then
    -- Messages may have arrived while closed: reload and acknowledge them.
    loadHistory(selected, HISTORY_LIMIT)
    local c = chatByJid(selected)
    if c and c.unread > 0 then
      markRead(selected)
    end
  end
  render()
end

function onClose()
  isOpen = false
  confirmLogout = false
  noctalia.state.set(common.KEY_PANEL_OPEN, false)
  publishActive()
end
