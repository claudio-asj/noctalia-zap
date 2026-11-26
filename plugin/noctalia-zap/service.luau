--!nonstrict
-- Owns the noctalia-zapd process. Reads its JSON events (one per line) and
-- publishes the result to noctalia.state for the bar widget and the panel.
-- Message text is never logged.

local common = require("./lib/common.luau")

local MAX_CHATS = 300
local WATCHDOG_MS = 10000
local RESTART_GRACE_MS = 15000

local dataDir, dataError = noctalia.pluginDataDir()
local binary = nil
local lastStart = 0
local failures = 0
local restartPending = false

local function shellQuote(value: string): string
  return "'" .. string.gsub(value, "'", "'\\''") .. "'"
end

local function setStatus(state: string, detail: string?)
  noctalia.state.set(common.KEY_STATUS, { state = state, detail = detail or "" })
  if state ~= "qr" then
    noctalia.state.set(common.KEY_QR, nil)
  end
end

-- The binary comes from the setting, then PATH, then ~/.local/bin.
local function resolveBinary(): string?
  local configured = noctalia.getConfig("daemon_path")
  if type(configured) == "string" and configured ~= "" then
    local path = noctalia.expandPath(configured)
    if noctalia.fileExists(path) then
      return path
    end
    return nil
  end
  if noctalia.commandExists("noctalia-zapd") then
    return "noctalia-zapd"
  end
  local fallback = noctalia.expandPath("~/.local/bin/noctalia-zapd")
  if noctalia.fileExists(fallback) then
    return fallback
  end
  return nil
end

local function onChats(items)
  if type(items) ~= "table" then
    return
  end
  local chats = {}
  local unread = 0
  for i, c in items do
    if i > MAX_CHATS then
      break
    end
    if type(c) == "table" and type(c.jid) == "string" then
      local n = tonumber(c.unread) or 0
      unread += n
      table.insert(chats, {
        jid = c.jid,
        name = c.name or "",
        unread = n,
        last = c.last or "",
        lastKind = c.last_kind or "",
        ts = tonumber(c.ts) or 0,
      })
    end
  end
  noctalia.state.set(common.KEY_CHATS, chats)
  noctalia.state.set(common.KEY_UNREAD, unread)
end

local function onLine(line: string)
  local event = noctalia.json.decode(line)
  if type(event) ~= "table" then
    return
  end
  if event.type == "status" then
    setStatus(event.state or "error", event.detail)
    if event.state == "error" and event.detail == "socket" then
      -- A daemon from an earlier load still owns the socket: stop it and start again.
      noctalia.runAsync({ binary, "ctl", "quit" }, function(_r)
        restartPending = true
      end)
    end
  elseif event.type == "qr" then
    noctalia.state.set(common.KEY_QR, event.png)
  elseif event.type == "chats" then
    onChats(event.items)
  end
  -- "message" and "history" are handled by the conversation view (phase 3).
end

local function startDaemon()
  binary = resolveBinary()
  noctalia.state.set(common.KEY_DAEMON, binary)
  if binary == nil then
    setStatus("missing")
    return
  end
  if dataDir == nil then
    noctalia.log("noctalia-zap: no data dir: " .. tostring(dataError))
    setStatus("error", "data_dir")
    return
  end
  local limit = tonumber(noctalia.getConfig("history_limit")) or 50
  -- runStream only takes a shell string: only the binary path and the data dir
  -- go in, each quoted. Nothing from messages ever reaches a command line here.
  local cmd = "exec "
    .. shellQuote(binary)
    .. " run -data "
    .. shellQuote(dataDir)
    .. " -history-limit "
    .. tostring(math.floor(limit))
  lastStart = noctalia.nowMs()
  failures = 0
  setStatus("connecting")
  if not noctalia.runStream(cmd, onLine) then
    setStatus("error", "start_failed")
  end
end

-- runStream has no exit callback, so a slow tick checks that the daemon answers.
noctalia.setUpdateInterval(WATCHDOG_MS)

function update()
  if binary == nil then
    return
  end
  if restartPending then
    restartPending = false
    startDaemon()
    return
  end
  noctalia.runAsync({ binary, "ctl", "status" }, function(r)
    if r.exitCode == 0 then
      failures = 0
      return
    end
    failures += 1
    if failures >= 2 and noctalia.nowMs() - lastStart > RESTART_GRACE_MS then
      setStatus("error", "daemon_stopped")
      startDaemon()
    end
  end, 5000)
end

function onConfigChanged()
  -- The binary path or history limit may have changed: restart the daemon.
  local newBinary = resolveBinary()
  if newBinary ~= binary and binary ~= nil then
    noctalia.runAsync({ binary, "ctl", "quit" }, function(_r)
      restartPending = true
    end)
  elseif binary == nil then
    startDaemon()
  end
end

function onIpc(event, _payload)
  if event == "toggle" then
    noctalia.togglePanel(common.panelId())
  end
end

noctalia.state.set(common.KEY_CHATS, {})
noctalia.state.set(common.KEY_UNREAD, 0)
startDaemon()
