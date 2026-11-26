--!nonstrict
-- Helpers shared by the service, the bar widget and the panel.

local M = {}

local PLUGIN_ID = "claudio-asj/noctalia-zap"
local PANELS = {
  compact = "panel_compact",
  normal = "panel",
  large = "panel_large",
  full = "panel_full",
}

-- Id of the panel matching the "panel_size" setting.
function M.panelId(): string
  local size = noctalia.getConfig("panel_size")
  return `{PLUGIN_ID}:{PANELS[size] or "panel"}`
end

-- Shared state keys written by service.luau.
M.KEY_STATUS = "status" -- { state = string, detail = string }
M.KEY_QR = "qr" -- absolute path of the current QR image, or nil
M.KEY_CHATS = "chats" -- list of chats, most recent first
M.KEY_UNREAD = "unread" -- total unread messages
M.KEY_DAEMON = "daemon" -- path of the noctalia-zapd binary, or nil when missing
M.KEY_MESSAGES = "messages" -- { seq = number, items = { message } }: last live messages
-- Written by the panel:
M.KEY_ACTIVE_CHAT = "activeChat" -- jid shown in the open panel, or nil
M.KEY_PANEL_OPEN = "panelOpen" -- true while a panel is open
-- Written by the service when a notification is clicked:
M.KEY_OPEN_CHAT = "openChat" -- { jid = string, at = number }

function M.isGroup(jid: string?): boolean
  return jid ~= nil and string.sub(jid, -5) == "@g.us"
end

-- Status states that mean "not usable right now".
function M.isProblem(state: string?): boolean
  return state == nil or state == "error" or state == "logged_out" or state == "missing"
end

-- Preview of the last message of a chat: text, or a label for media.
function M.preview(text: string?, kind: string?): string
  text = text or ""
  if kind == nil or kind == "" or kind == "text" then
    return text
  end
  local label = noctalia.tr("kind." .. kind)
  if text ~= "" then
    return `{label}: {text}`
  end
  return label
end

local KIND_GLYPHS = {
  image = "photo",
  video = "video",
  audio = "microphone",
  document = "file",
  sticker = "sticker",
  location = "map-pin",
  contact = "user",
  poll = "chart-bar",
}

function M.kindGlyph(kind: string?): string
  return KIND_GLYPHS[kind or ""] or "message"
end

-- Badge text, capped so it stays small.
function M.badgeText(count: number): string
  if count > 99 then
    return noctalia.tr("badge.many", { count = 99 })
  end
  return tostring(count)
end

-- First visible character of a name, for the avatar placeholder.
function M.initial(name: string?): string
  if name == nil or name == "" then
    return "?"
  end
  local ok, cp = pcall(utf8.codepoint, name, 1)
  if not ok or cp == nil then
    return "?"
  end
  if cp == 43 then -- "+": phone numbers get a generic mark
    return "#"
  end
  return string.upper(utf8.char(cp))
end

-- Short time for the chat list: hour today, date otherwise.
function M.shortTime(ts: number?): string
  if ts == nil or ts <= 0 then
    return ""
  end
  if noctalia.formatTime("%Y-%m-%d", ts) == noctalia.formatTime("%Y-%m-%d") then
    return noctalia.formatTime("%H:%M", ts)
  end
  return noctalia.formatTime("%d/%m", ts)
end

-- Case-insensitive plain-text match for the chat search.
function M.matches(haystack: string?, needle: string): boolean
  if needle == "" then
    return true
  end
  return string.find(string.lower(haystack or ""), string.lower(needle), 1, true) ~= nil
end

return M
