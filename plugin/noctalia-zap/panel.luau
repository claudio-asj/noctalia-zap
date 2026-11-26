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
local MAX_LOADED = 400 -- messages kept for the open chat
-- Bubbles drawn at first; each "load older" adds a page. With day separators
-- and the top row, MAX_DRAWN_TOTAL keeps the scroll under 256 children.
local MAX_DRAWN = 120
local MAX_DRAWN_TOTAL = 200
local TYPING_TIMEOUT_MS = 8000 -- someone's "typing" fades without updates
local MY_TYPING_IDLE_S = "5" -- my "typing" turns to paused after this
-- Quick reactions. Emoji are not translatable text.
local QUICK_REACTIONS = { "👍", "❤️", "😂", "😮", "😢", "🙏" }
-- Rows drawn in the chat list. A scroll takes at most 256 children and every
-- row costs render time; search reaches the rest.
local MAX_ROWS = 80

local isOpen = false
local query = ""
local viewArchived = false -- showing the archived chats
local selected = nil
local confirmLogout = false
local busy = false
local actionError = nil

-- Open conversation.
local messages = {} -- oldest first
local messageIds = {} -- id -> true, to drop duplicates
local loadingHistory = false
local historyError = false
local initialMessages = noctalia.state.get(common.KEY_MESSAGES) or {}
local lastSeq = initialMessages.seq or 0
local lastEpoch = initialMessages.epoch
local lastOpenChat = 0
local draft = ""
local composerRev = 0
local sending = false
local sendError = false
local scrollRev = 0

-- Media: per message id.
local mediaBusy = {} -- id -> true while downloading
local mediaError = {} -- id -> translation key of the last error
local playback = noctalia.state.get(common.KEY_PLAYBACK) or {}

-- Phase 6: actions, replies, typing, presence, older messages.
local selectedMsg = nil -- id of the message showing its action bar
local replyTo = nil -- message being answered
local typing = nil -- { from, name, state, at } for the open chat
local typingToken = 0
local myTyping = false
local presenceChat = nil -- chat we are "online" in, or nil
local drawLimit = MAX_DRAWN
local olderLoading = false -- reading older messages from the store
local olderPending = false -- waiting for the phone to send older messages
local olderToken = 0
local hasMore = true -- false once the start of the chat was reached
local revealKey = nil

-- "Updating" indicator of the open chat: picture refresh and history resend.
local avatarPending = false
local backfillPending = false
local backfillToken = 0

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
    for _, raw in resp.data do
      local m = common.normalizeMessage(raw)
      if m then
        addMessage(m)
      end
    end
    for _, m in live do
      addMessage(m)
    end
    -- Older live messages may predate the fresh history: keep time order.
    table.sort(messages, function(a, b)
      return a.ts < b.ts
    end)
    scrollRev += 1
    render()
  end, 30000)
end

-- Online only while a chat is open in the panel (the phone keeps notifying
-- otherwise). nil goes back offline.
local function setPresence(jid)
  if daemon == nil or jid == presenceChat then
    return
  end
  presenceChat = jid
  local argv = { daemon, "ctl", "presence" }
  if jid ~= nil then
    table.insert(argv, jid)
  end
  noctalia.runAsync(argv, function(_r) end, 15000)
end

-- My "typing" state in the open chat; paused after a few idle seconds. One
-- timer at a time, whatever the typing speed.
local lastKeyMs = 0
local typingTimer = false

local function sendMyTyping(on: boolean)
  if on == myTyping or daemon == nil or selected == nil then
    return
  end
  myTyping = on
  noctalia.runAsync({ daemon, "ctl", "typing", selected, if on then "composing" else "paused" }, function(_r) end, 10000)
end

local function startTypingTimer()
  if typingTimer then
    return
  end
  typingTimer = true
  noctalia.runAsync({ "sleep", MY_TYPING_IDLE_S }, function(_r)
    typingTimer = false
    if not myTyping then
      return
    end
    if noctalia.nowMs() - lastKeyMs >= tonumber(MY_TYPING_IDLE_S) * 1000 - 100 then
      sendMyTyping(false)
    else
      startTypingTimer()
    end
  end, 10000)
end

local function setMyTyping(on: boolean)
  if daemon == nil or selected == nil or presenceChat ~= selected then
    myTyping = false
    return
  end
  if on then
    lastKeyMs = noctalia.nowMs()
    sendMyTyping(true)
    startTypingTimer()
  else
    sendMyTyping(false)
  end
end

local function findMessage(id)
  for i, m in messages do
    if m.id == id then
      return m, i
    end
  end
  return nil, nil
end

-- Sends (or removes, with an empty emoji) my reaction. The change comes back
-- as an "update" from the daemon.
local function react(m, emoji: string)
  if daemon == nil or selected == nil or m.id == "" then
    return
  end
  local argv = { daemon, "ctl", "react", selected, m.id }
  if emoji ~= "" then
    table.insert(argv, emoji)
  end
  noctalia.runAsync(argv, function(r)
    if r.exitCode ~= 0 then
      actionError = noctalia.tr("panel.error.command")
      render()
    end
  end, 30000)
end

-- Reads the page of messages before the oldest one shown. When the store has
-- no more, asks the phone; its answer arrives as a "backfill" event.
local loadOlder
local function askPhoneForOlder(jid)
  olderPending = true
  olderToken += 1
  local token = olderToken
  noctalia.runAsync({ daemon, "ctl", "backfill", jid, "--older" }, function(r)
    if selected ~= jid or olderToken ~= token then
      return
    end
    local resp = noctalia.json.decode(r.stdout or "")
    if r.exitCode ~= 0 or type(resp) ~= "table" or type(resp.data) ~= "table" or not resp.data.requested then
      olderPending = false
      hasMore = false
      render()
      return
    end
    -- The phone may be offline: stop waiting after a minute.
    noctalia.runAsync({ "sleep", "60" }, function(_r)
      if olderToken == token and olderPending then
        olderPending = false
        render()
      end
    end, 65000)
  end, 30000)
  render()
end

function loadOlder(fromPhoneAnswer: boolean?)
  if daemon == nil or selected == nil or olderLoading or #messages == 0 then
    return
  end
  if drawLimit >= MAX_DRAWN_TOTAL and not fromPhoneAnswer then
    return
  end
  local jid = selected
  local oldest = messages[1]
  olderLoading = true
  noctalia.runAsync({ daemon, "ctl", "history", jid, tostring(HISTORY_LIMIT), "--before", tostring(oldest.ts) }, function(r)
    if selected ~= jid then
      return
    end
    olderLoading = false
    local resp = noctalia.json.decode(r.stdout or "")
    if r.exitCode ~= 0 or type(resp) ~= "table" or type(resp.data) ~= "table" then
      render()
      return
    end
    local older = {}
    for _, raw in resp.data do
      local m = common.normalizeMessage(raw)
      if m and (m.id == "" or not messageIds[m.id]) then
        table.insert(older, m)
      end
    end
    if #older == 0 then
      if fromPhoneAnswer then
        -- The phone answered and nothing older came: this is the start.
        hasMore = false
      else
        askPhoneForOlder(jid)
      end
      render()
      return
    end
    for _, m in older do
      if m.id ~= "" then
        messageIds[m.id] = true
      end
    end
    table.move(messages, 1, #messages, #older + 1, older)
    messages = older
    drawLimit = math.min(MAX_DRAWN_TOTAL, drawLimit + #resp.data)
    -- Keep the message that was on top in view.
    revealKey = oldest.id
    render()
  end, 30000)
  render()
end

local function selectChat(jid)
  if jid == selected then
    return
  end
  setMyTyping(false)
  selected = jid
  messages, messageIds = {}, {}
  draft = ""
  composerRev += 1
  sendError = false
  selectedMsg, replyTo, typing = nil, nil, nil
  drawLimit, hasMore, revealKey = MAX_DRAWN, true, nil
  olderLoading, olderPending = false, false
  if isOpen then
    setPresence(jid)
  end
  publishActive()
  loadHistory(jid, HISTORY_LIMIT)
  -- Older media may lack download data: ask the phone to resend this chat's
  -- history. The daemon skips chats that need nothing; the answer comes back
  -- as a "backfill" event and the chat reloads.
  avatarPending, backfillPending = false, false
  if daemon ~= nil then
    backfillToken += 1
    local token = backfillToken
    noctalia.runAsync({ daemon, "ctl", "backfill", jid }, function(r)
      local resp = noctalia.json.decode(r.stdout or "")
      if selected == jid and type(resp) == "table" and type(resp.data) == "table" and resp.data.requested then
        backfillPending = true
        render()
        -- The phone may never answer (offline): give up on the indicator after a minute.
        noctalia.runAsync({ "sleep", "60" }, function(_r)
          if backfillToken == token and backfillPending then
            backfillPending = false
            render()
          end
        end, 65000)
      end
    end, 30000)
    -- Profile picture, refreshed at most once a day per chat.
    avatarPending = true
    noctalia.runAsync({ daemon, "ctl", "avatar", jid }, function(_r)
      if selected == jid then
        avatarPending = false
        render()
      end
    end, 30000)
  end
  -- Receipts only once the user can see the chat; onOpen covers the rest.
  local c = chatByJid(jid)
  if isOpen and c and c.unread > 0 then
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
  local argv = { daemon, "ctl", "send", jid, "--file", path }
  if replyTo ~= nil and replyTo.id ~= "" then
    table.insert(argv, "--reply")
    table.insert(argv, replyTo.id)
  end
  setMyTyping(false)
  noctalia.runAsync(argv, function(r)
    sending = false
    noctalia.removeFile(path) -- ctl removes it; this covers early failures
    if r.exitCode == 0 then
      if selected == jid then
        draft = ""
        replyTo = nil
        composerRev += 1 -- a new key empties the uncontrolled input
      end
    else
      sendError = true
    end
    render()
  end, 60000)
  render()
end

-- Error keys the daemon may answer for media commands.
local MEDIA_ERRORS = {
  media_unavailable = "panel.media.unavailable",
  not_stored = "panel.media.not_stored",
  ["not connected"] = "panel.media.offline",
  no_player = "panel.media.no_player",
}

local function mediaFailed(id, stdout)
  local resp = noctalia.json.decode(stdout or "")
  local err = if type(resp) == "table" then resp.error else nil
  mediaError[id] = MEDIA_ERRORS[err or ""] or "panel.media.failed"
end

-- Downloads (if needed) and opens a photo, video, document or sticker.
local function openMedia(m)
  if daemon == nil or mediaBusy[m.id] then
    return
  end
  mediaBusy[m.id] = true
  mediaError[m.id] = nil
  local jid = selected
  noctalia.runAsync({ daemon, "ctl", "media", jid, m.id }, function(r)
    mediaBusy[m.id] = nil
    local resp = noctalia.json.decode(r.stdout or "")
    if r.exitCode == 0 and type(resp) == "table" and type(resp.data) == "table" and type(resp.data.path) == "string" then
      if m.media then
        m.media.file = resp.data.path
      end
      noctalia.openPath(resp.data.path)
    else
      mediaFailed(m.id, r.stdout)
    end
    render()
  end, 60000)
  render()
end

local function isPlaying(m): boolean
  return playback.playing == true and playback.id == m.id and playback.chat == selected
end

local function toggleAudio(m)
  if daemon == nil then
    return
  end
  if isPlaying(m) then
    noctalia.runAsync({ daemon, "ctl", "stop" }, function(_r) end, 10000)
    return
  end
  if mediaBusy[m.id] then
    return
  end
  mediaBusy[m.id] = true
  mediaError[m.id] = nil
  noctalia.runAsync({ daemon, "ctl", "play", selected, m.id }, function(r)
    mediaBusy[m.id] = nil
    if r.exitCode ~= 0 then
      mediaFailed(m.id, r.stdout)
    end
    render()
  end, 60000)
  render()
end

-- Removes outbox files left behind if a send never finished (panel closed,
-- shell restarted). They hold message text, so they must not linger.
local function cleanOutbox()
  if dataDir == nil or sending then
    return
  end
  local files = noctalia.listDir(dataDir)
  if files == nil then
    return
  end
  for _, name in files do
    if string.sub(name, 1, 7) == "outbox-" then
      noctalia.removeFile(`{dataDir}/{name}`)
    end
  end
end

-- Live messages from the service.
local function onMessages(value)
  if type(value) ~= "table" or type(value.items) ~= "table" then
    return
  end
  if value.epoch ~= lastEpoch then
    -- The service restarted and seq starts over.
    lastEpoch = value.epoch
    lastSeq = 0
  end
  local first = value.items[1]
  if selected ~= nil and first ~= nil and lastSeq > 0 and first.seq > lastSeq + 1 then
    -- More messages arrived than the service keeps: reload instead of guessing.
    lastSeq = value.seq or lastSeq
    loadHistory(selected, HISTORY_LIMIT)
    return
  end
  local changed = false
  local added = false
  local incoming = false
  for _, m in value.items do
    if m.seq > lastSeq then
      lastSeq = m.seq
      if m.chat ~= selected then
        continue
      end
      if m.receipt then
        local wanted = {}
        for _, id in m.ids do
          wanted[id] = true
        end
        for _, mine in messages do
          if wanted[mine.id] then
            mine.ack = m.ack
            changed = true
          end
        end
      elseif m.update then
        local _, i = findMessage(m.id)
        if i then
          messages[i] = m
          changed = true
        end
      elseif addMessage(m) then
        changed = true
        added = true
        incoming = incoming or not m.me
        if typing and typing.from == m.from then
          typing = nil -- they sent it
        end
      end
    end
  end
  if changed then
    if added then
      scrollRev += 1
    end
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

local function avatar(name: string, picture: string?)
  local size = style.avatar
  if picture ~= nil then
    return ui.image({ key = picture, path = picture, width = size, height = size, radius = size / 2, fit = "cover" })
  end
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
      text = common.badgeText(count),
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
    avatar(c.name, c.avatar),
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
        ui.glyph({ name = "pin", size = 12, color = "on_surface_variant", visible = c.pinned }),
        ui.label({ text = common.shortTime(c.ts), fontSize = style.font.caption, color = "on_surface_variant" }),
      }),
      ui.row({ gap = style.gap.sm, align = "center" }, bottom),
    }),
  })
end

local function chatList()
  local rows = {}
  local more = false
  if not viewArchived and query == "" then
    local count, unreadArchived = 0, 0
    for _, c in chats do
      if c.archived then
        count += 1
        unreadArchived += c.unread
      end
    end
    if count > 0 then
      local right = {}
      if unreadArchived > 0 then
        table.insert(right, badge(unreadArchived))
      end
      table.insert(rows, ui.row({
        key = "archived-entry",
        gap = style.gap.md,
        padding = style.padding.sm,
        radius = style.radius.card,
        align = "center",
        onClick = function()
          viewArchived = true
          render()
        end,
      }, {
        ui.glyph({ name = "archive", size = 18, color = "on_surface_variant" }),
        ui.label({
          text = noctalia.tr("panel.list.archived_count", { count = count }),
          fontSize = style.font.name,
          fontWeight = "medium",
          color = "on_surface",
          flexGrow = 1,
        }),
        ui.row({ gap = style.gap.sm, align = "center" }, right),
      }))
    end
  end
  for _, c in chats do
    if c.archived == viewArchived and common.matches(c.name, query) then
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
      ui.label({
        text = noctalia.tr(if viewArchived then "panel.list.archived_title" else "panel.list.title"),
        fontSize = style.font.title,
        fontWeight = "bold",
        color = "on_surface",
      }),
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
  if viewArchived then
    table.insert(children, 1, ui.button({
      glyph = "arrow-left",
      variant = "ghost",
      controlSize = "sm",
      tooltip = noctalia.tr("panel.list.back"),
      onClick = function()
        viewArchived = false
        render()
      end,
    }))
  end
  local titleColumn = if viewArchived then children[2] else children[1]
  if status.state ~= "connected" then
    table.insert(titleColumn.children, ui.label({
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
            messages, messageIds = {}, {}
            publishActive()
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
  return noctalia.formatTime("%x", ts) -- the locale's date format
end

-- Thumbnail box size: keeps the aspect ratio inside thumbMax x thumbMax.
local function thumbSize(media): (number, number)
  local w, h = media.w, media.h
  local max = style.thumbMax
  if w <= 0 or h <= 0 then
    return max, math.floor(max * 3 / 4)
  end
  local scale = math.min(max / w, max / h)
  return math.max(48, math.floor(w * scale)), math.max(48, math.floor(h * scale))
end

-- Body of a media message: thumbnail, audio player or file row.
local function mediaBody(m, textColor: string)
  local media = m.media or common.copyMedia({})
  local busy = mediaBusy[m.id] == true
  local subColor = if m.me then "on_primary_container/0.7" else "on_surface_variant"
  local parts = {}

  if m.kind == "audio" then
    local playing = isPlaying(m)
    table.insert(parts, ui.row({ gap = style.gap.md, align = "center" }, {
      ui.button({
        glyph = if playing then "player-stop" else "player-play",
        variant = "ghost",
        controlSize = "sm",
        enabled = not busy,
        tooltip = noctalia.tr(if playing then "panel.media.stop" else "panel.media.play"),
        onClick = function()
          toggleAudio(m)
        end,
      }),
      ui.glyph({ name = if media.ptt then "microphone" else "music", size = 14, color = textColor }),
      ui.label({
        text = if busy then noctalia.tr("panel.media.loading") else common.duration(media.seconds),
        fontSize = style.font.body,
        color = textColor,
      }),
    }))
  elseif m.kind == "document" then
    local details = {}
    if media.size > 0 then
      table.insert(details, common.fileSize(media.size))
    end
    table.insert(parts, ui.row({ gap = style.gap.md, align = "center", onClick = function()
      openMedia(m)
    end }, {
      ui.glyph({ name = "file", size = 24, color = textColor }),
      ui.column({ gap = 2, flexGrow = 1 }, {
        ui.label({
          text = if m.text ~= "" then m.text else noctalia.tr("kind.document"),
          fontSize = style.font.body,
          fontWeight = "medium",
          color = textColor,
          maxLines = 2,
          maxWidth = style.bubbleMax - 80,
        }),
        ui.label({
          text = if busy then noctalia.tr("panel.media.loading") else table.concat(details, " "),
          fontSize = style.font.caption,
          color = subColor,
        }),
      }),
      ui.glyph({ name = if media.file then "external-link" else "download", size = 16, color = textColor }),
    }))
  elseif media.thumb then
    -- image, video, sticker with a preview
    local w, h = thumbSize(media)
    table.insert(parts, ui.image({
      key = "thumb-" .. m.id,
      path = media.thumb,
      width = w,
      height = h,
      fit = "cover",
      radius = style.radius.control,
      tooltip = noctalia.tr("panel.media.open"),
      onClick = function()
        openMedia(m)
      end,
    }))
    if m.kind == "video" or busy then
      local info = {}
      if m.kind == "video" then
        table.insert(info, ui.glyph({ name = "player-play", size = 12, color = subColor }))
        table.insert(info, ui.label({ text = common.duration(media.seconds), fontSize = style.font.caption, color = subColor }))
      end
      if busy then
        table.insert(info, ui.label({ text = noctalia.tr("panel.media.loading"), fontSize = style.font.caption, color = subColor }))
      end
      table.insert(parts, ui.row({ gap = style.gap.xs, align = "center" }, info))
    end
  else
    -- No preview: a button that downloads and opens it.
    table.insert(parts, ui.button({
      text = noctalia.tr("kind." .. m.kind),
      glyph = common.kindGlyph(m.kind),
      variant = "outline",
      controlSize = "sm",
      enabled = not busy,
      tooltip = noctalia.tr("panel.media.open"),
      onClick = function()
        openMedia(m)
      end,
    }))
  end

  if mediaError[m.id] then
    table.insert(parts, ui.label({ text = noctalia.tr(mediaError[m.id]), fontSize = style.font.caption, color = "error" }))
  end
  return parts
end

local MEDIA_KINDS = { image = true, video = true, audio = true, document = true, sticker = true }

-- Stickers are drawn like WhatsApp does: no bubble, fixed size, not cropped,
-- and clicking does nothing (they are not files the user wants to keep).
local function stickerBubble(m, key: string, showSender: boolean)
  local mine = m.me
  local children = {}
  if showSender and not mine and m.name ~= "" then
    table.insert(children, ui.label({
      text = m.name,
      fontSize = style.font.caption,
      fontWeight = "medium",
      color = common.senderColor(m.from),
      maxLines = 1,
    }))
  end
  local image = ui.image({
    key = "sticker-" .. m.id,
    path = m.media.thumb,
    width = style.sticker,
    height = style.sticker,
    fit = "contain",
  })
  if m.media.sound then
    -- Sticker with sound: a play button beside it.
    local playing = isPlaying(m)
    local controls = {
      ui.button({
        glyph = if playing then "player-stop" else "volume",
        variant = "ghost",
        controlSize = "sm",
        enabled = not mediaBusy[m.id],
        tooltip = noctalia.tr(if playing then "panel.media.stop" else "panel.media.play_sound"),
        onClick = function()
          toggleAudio(m)
        end,
      }),
    }
    if mine then
      image = ui.row({ gap = style.gap.xs, align = "center" }, { controls[1], image })
    else
      image = ui.row({ gap = style.gap.xs, align = "center" }, { image, controls[1] })
    end
  end
  table.insert(children, image)
  if mediaError[m.id] then
    table.insert(children, ui.label({ text = noctalia.tr(mediaError[m.id]), fontSize = style.font.caption, color = "error" }))
  end
  table.insert(children, ui.label({
    text = noctalia.formatTime("%H:%M", m.ts),
    fontSize = style.font.caption,
    color = "on_surface_variant",
    textAlign = if mine then "end" else "start",
  }))
  return ui.row({ key = key, justify = if mine then "end" else "start" }, {
    ui.column({ gap = style.gap.xs, align = if mine then "end" else "start" }, children),
  })
end

-- The answered message, shown on top of a reply. Clicking scrolls to it.
local function quoteBlock(q, mine: boolean)
  local name = if q.name ~= "" then q.name else noctalia.tr("panel.chat.you")
  local preview = common.preview(q.text, q.kind)
  return ui.column({
    gap = 2,
    padding = style.gap.sm,
    radius = style.radius.control,
    fill = if mine then "on_primary_container/0.12" else "on_surface/0.08",
    onClick = function()
      if messageIds[q.id] then
        revealKey = q.id
        render()
      end
    end,
  }, {
    ui.label({
      text = name,
      fontSize = style.font.caption,
      fontWeight = "medium",
      color = if q.name == "" then "primary" else common.senderColor(q.from),
      maxLines = 1,
    }),
    ui.label({
      text = preview,
      fontSize = style.font.caption,
      color = if mine then "on_primary_container/0.8" else "on_surface_variant",
      maxLines = 2,
      maxWidth = style.bubbleMax - 20,
    }),
  })
end

-- Reaction chips under a bubble. Clicking one toggles my reaction with it.
local function reactionRow(m)
  local chips = {}
  for _, r in m.reactions do
    local label = if r.count > 1 then `{r.emoji} {r.count}` else r.emoji
    table.insert(chips, ui.row({
      key = "r-" .. r.emoji,
      paddingH = style.gap.sm,
      paddingV = 2,
      radius = style.radius.card,
      fill = if r.me then "primary/0.2" else "surface_container_highest",
      onClick = function()
        react(m, if r.me then "" else r.emoji)
      end,
    }, {
      ui.label({ text = label, fontSize = style.font.caption, color = "on_surface" }),
    }))
  end
  return ui.row({ gap = style.gap.xs, justify = if m.me then "end" else "start" }, chips)
end

-- Reply and quick reactions for the selected message.
local function actionBar(m)
  local mineEmoji = ""
  for _, r in m.reactions do
    if r.me then
      mineEmoji = r.emoji
    end
  end
  local items = {
    ui.button({
      glyph = "arrow-back-up",
      variant = "ghost",
      controlSize = "sm",
      tooltip = noctalia.tr("panel.chat.reply"),
      onClick = function()
        replyTo = m
        selectedMsg = nil
        composerRev += 1 -- focus the input again
        render()
      end,
    }),
  }
  for _, emoji in QUICK_REACTIONS do
    table.insert(items, ui.button({
      text = emoji,
      variant = "ghost",
      controlSize = "sm",
      selected = emoji == mineEmoji,
      onClick = function()
        selectedMsg = nil
        react(m, if emoji == mineEmoji then "" else emoji)
        render()
      end,
    }))
  end
  return ui.row({
    gap = style.gap.xs,
    padding = style.gap.xs,
    radius = style.radius.card,
    fill = "surface_container_highest",
    align = "center",
  }, items)
end

-- Delivery ticks of my messages.
local function ackGlyph(ack: string): (string?, string)
  if ack == "read" then
    return "checks", "primary"
  elseif ack == "delivered" then
    return "checks", "on_primary_container/0.7"
  elseif ack == "sent" then
    return "check", "on_primary_container/0.7"
  end
  return nil, ""
end

local function bubble(m, index: number, showSender: boolean)
  local mine = m.me
  if m.kind == "sticker" and m.media ~= nil and m.media.thumb ~= nil and not m.deleted then
    return stickerBubble(m, if m.id ~= "" then m.id else `msg-{index}-{m.ts}`, showSender)
  end
  local children = {}
  if showSender and not mine and m.name ~= "" then
    table.insert(children, ui.label({
      text = m.name,
      fontSize = style.font.caption,
      fontWeight = "medium",
      color = common.senderColor(m.from),
      maxLines = 1,
    }))
  end
  local textColor = if mine then "on_primary_container" else "on_surface"
  if m.quote and not m.deleted then
    table.insert(children, quoteBlock(m.quote, mine))
  end
  -- Media stored before download support has no "media" field: show the label only.
  local isMedia = MEDIA_KINDS[m.kind] == true and m.media ~= nil
  if m.deleted then
    table.insert(children, ui.row({ gap = style.gap.xs, align = "center" }, {
      ui.glyph({ name = "ban", size = 14, color = if mine then "on_primary_container/0.7" else "on_surface_variant" }),
      ui.label({
        text = noctalia.tr("panel.chat.deleted"),
        fontSize = style.font.body,
        color = if mine then "on_primary_container/0.7" else "on_surface_variant",
      }),
    }))
  elseif isMedia then
    for _, part in mediaBody(m, textColor) do
      table.insert(children, part)
    end
  elseif m.kind ~= "text" then
    table.insert(children, ui.row({ gap = style.gap.xs, align = "center" }, {
      ui.glyph({ name = common.kindGlyph(m.kind), size = 14, color = textColor }),
      ui.label({ text = noctalia.tr("kind." .. m.kind), fontSize = style.font.body, color = textColor }),
    }))
  end
  -- Documents show their name in the file row; everything else shows the caption.
  if m.text ~= "" and m.kind ~= "document" and not m.deleted then
    table.insert(children, ui.label({
      text = m.text,
      fontSize = style.font.body,
      color = textColor,
      maxWidth = style.bubbleMax,
    }))
  end
  local footer = noctalia.formatTime("%H:%M", m.ts)
  if m.truncated then
    footer = noctalia.tr("panel.chat.truncated", { time = footer })
  end
  if m.edited and not m.deleted then
    footer = noctalia.tr("panel.chat.edited", { time = footer })
  end
  local footerParts = {
    ui.label({
      text = footer,
      fontSize = style.font.caption,
      color = if mine then "on_primary_container/0.7" else "on_surface_variant",
    }),
  }
  local tick, tickColor = ackGlyph(if mine then m.ack else "")
  if tick then
    table.insert(footerParts, ui.glyph({ name = tick, size = 12, color = tickColor }))
  end
  table.insert(children, ui.row({ gap = style.gap.xs, align = "center", justify = "end" }, footerParts))

  local key = if m.id ~= "" then m.id else `msg-{index}-{m.ts}`
  local column = {
    ui.column({
      gap = style.gap.xs,
      padding = style.padding.sm,
      radius = style.radius.card,
      fill = if mine then "primary_container" else "surface_container_high",
      border = if selectedMsg == m.id then "primary" else nil,
      borderWidth = if selectedMsg == m.id then 1 else nil,
      -- Click selects the message: shows reply and reactions.
      onClick = if m.id ~= "" and not m.deleted then function()
        selectedMsg = if selectedMsg == m.id then nil else m.id
        render()
      end else nil,
    }, children),
  }
  if #m.reactions > 0 then
    table.insert(column, reactionRow(m))
  end
  if selectedMsg == m.id then
    table.insert(column, actionBar(m))
  end
  return ui.row({ key = key, justify = if mine then "end" else "start" }, {
    ui.column({ gap = style.gap.xs, align = if mine then "end" else "start" }, column),
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
  local firstDrawn = math.max(1, #messages - drawLimit + 1)
  -- Top of the list: load older messages, or say why not.
  local top
  if olderLoading or olderPending then
    top = ui.label({ text = noctalia.tr("panel.chat.loading_older"), fontSize = style.font.caption, color = "on_surface_variant" })
  elseif firstDrawn > 1 or (hasMore and drawLimit < MAX_DRAWN_TOTAL) then
    top = ui.button({
      text = noctalia.tr("panel.chat.load_older"),
      glyph = "arrow-up",
      variant = "ghost",
      controlSize = "sm",
      onClick = function()
        if firstDrawn > 1 then
          -- Already loaded, just not drawn.
          drawLimit = math.min(MAX_DRAWN_TOTAL, drawLimit + HISTORY_LIMIT)
          revealKey = messages[firstDrawn].id
          render()
        else
          loadOlder()
        end
      end,
    })
  elseif drawLimit >= MAX_DRAWN_TOTAL then
    top = ui.label({ text = noctalia.tr("panel.chat.limit_older"), fontSize = style.font.caption, color = "on_surface_variant" })
  else
    top = ui.label({ text = noctalia.tr("panel.chat.start"), fontSize = style.font.caption, color = "on_surface_variant" })
  end
  table.insert(rows, ui.row({ key = "top", justify = "center" }, { top }))
  for i = firstDrawn, #messages do
    local m = messages[i]
    local day = noctalia.formatTime("%Y-%m-%d", m.ts)
    if day ~= lastDay then
      lastDay = day
      table.insert(rows, ui.row({ key = "day-" .. day, justify = "center" }, {
        ui.label({ text = dayLabel(m.ts), fontSize = style.font.caption, color = "on_surface_variant" }),
      }))
    end
    table.insert(rows, bubble(m, i, group))
  end
  -- Many different days add many separators: drop the oldest rows (keeping
  -- the top row) so the scroll stays under its 256 children.
  while #rows > 250 do
    table.remove(rows, 2)
  end
  return ui.scroll({
    flexGrow = 1,
    gap = style.gap.sm,
    stickToBottom = true,
    scrollToBottomRev = scrollRev,
    revealKey = revealKey,
  }, rows)
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
        setMyTyping(text ~= "")
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
  local parts = {}
  if sendError then
    table.insert(parts, ui.label({ text = noctalia.tr("panel.chat.send_error"), fontSize = style.font.caption, color = "error" }))
  end
  if replyTo ~= nil then
    table.insert(parts, ui.row({ gap = style.gap.sm, align = "center" }, {
      ui.glyph({ name = "arrow-back-up", size = 14, color = "primary" }),
      ui.column({ flexGrow = 1 }, { quoteBlock({
        id = replyTo.id,
        from = replyTo.from,
        name = if replyTo.me then "" else replyTo.name,
        text = replyTo.text,
        kind = replyTo.kind,
      }, false) }),
      ui.button({
        glyph = "x",
        variant = "ghost",
        controlSize = "sm",
        tooltip = noctalia.tr("panel.chat.cancel_reply"),
        onClick = function()
          replyTo = nil
          render()
        end,
      }),
    }))
  end
  if #parts == 0 then
    return box
  end
  table.insert(parts, box)
  return ui.column({ gap = style.gap.xs }, parts)
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
  local titleParts = {
    ui.label({ text = name, fontSize = style.font.title, fontWeight = "bold", color = "on_surface", maxLines = 1 }),
  }
  if typing ~= nil and typing.state ~= "paused" and noctalia.nowMs() - typing.at < TYPING_TIMEOUT_MS then
    local key = if typing.state == "recording" then "panel.chat.recording" else "panel.chat.typing"
    local who = if common.isGroup(selected) and typing.name ~= "" then typing.name else ""
    table.insert(titleParts, ui.label({
      text = noctalia.tr(key .. (if who ~= "" then "_named" else ""), { name = who }),
      fontSize = style.font.caption,
      color = "primary",
    }))
  elseif avatarPending or backfillPending then
    table.insert(titleParts, ui.row({ gap = style.gap.xs, align = "center" }, {
      ui.glyph({ name = "refresh", size = 12, color = "on_surface_variant" }),
      ui.label({
        text = noctalia.tr(if backfillPending then "panel.chat.updating_media" else "panel.chat.updating"),
        fontSize = style.font.caption,
        color = "on_surface_variant",
      }),
    }))
  end
  return ui.column({ flexGrow = 1, gap = style.gap.md }, {
    ui.row({ gap = style.gap.md, align = "center" }, {
      avatar(name, if c then c.avatar else nil),
      ui.column({ gap = 2, flexGrow = 1 }, titleParts),
      ui.button({
        glyph = "x",
        variant = "ghost",
        controlSize = "sm",
        tooltip = noctalia.tr("panel.chat.close"),
        onClick = function()
          setMyTyping(false)
          setPresence(nil)
          selected = nil
          messages, messageIds = {}, {}
          selectedMsg, replyTo, typing = nil, nil, nil
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
  -- Messages from offline sync are not sent live: reload the open chat when
  -- the list says it has something newer than what is shown.
  if isOpen and selected ~= nil and not loadingHistory then
    local c = chatByJid(selected)
    local newest = if #messages > 0 then messages[#messages].ts else 0
    if c and c.ts > newest then
      loadHistory(selected, HISTORY_LIMIT)
    end
  end
  render()
end)
noctalia.state.watch(common.KEY_DAEMON, function(value)
  daemon = value
  render()
end)
noctalia.state.watch(common.KEY_MESSAGES, onMessages)
noctalia.state.watch(common.KEY_BACKFILL, function(value)
  if type(value) == "table" and value.chat == selected and olderPending then
    -- The phone sent older messages we asked for.
    olderPending = false
    olderToken += 1
    loadOlder(true)
    return
  end
  if type(value) == "table" and value.chat == selected then
    backfillPending = false
    if isOpen and not loadingHistory then
      loadHistory(selected, HISTORY_LIMIT)
    end
    render()
  end
end)
noctalia.state.watch(common.KEY_TYPING, function(value)
  if type(value) ~= "table" or value.chat ~= selected then
    return
  end
  typing = value
  typingToken += 1
  local token = typingToken
  render()
  -- Without new updates, "typing" fades out.
  noctalia.runAsync({ "sleep", "9" }, function(_r)
    if typingToken == token then
      typing = nil
      render()
    end
  end, 15000)
end)
noctalia.state.watch(common.KEY_PLAYBACK, function(value)
  playback = value or {}
  render()
end)

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
  -- The input is recreated empty on open, so the draft must go with it.
  draft = ""
  composerRev += 1
  cleanOutbox()
  noctalia.state.set(common.KEY_PANEL_OPEN, true)
  publishActive()
  if selected ~= nil then
    setPresence(selected)
  end
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
  setMyTyping(false)
  setPresence(nil)
  selectedMsg = nil
  publishActive()
end
