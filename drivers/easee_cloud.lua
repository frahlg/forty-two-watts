-- Easee Cloud EV Charger Driver
-- Emits: EV
-- Protocol: HTTPS (Easee Cloud REST API)
--
-- Authenticates with the user's Easee account (email + password from
-- config), polls charger state every 5 seconds, and emits DerEV
-- readings so the dispatch clamp keeps home batteries from feeding
-- the car.
--
-- Config example:
--   drivers:
--     - name: easee
--       lua: drivers/easee_cloud.lua
--       capabilities:
--         http:
--           allowed_hosts: ["api.easee.com"]
--       config:
--         email: "user@example.com"
--         password: "secret"
--         serial: "EHHZBKPF"    # optional — auto-detected if omitted

DRIVER = {
  id           = "easee-cloud",
  name         = "Easee Cloud",
  manufacturer = "Easee",
  version      = "1.0.0",
  protocols    = { "http" },
  capabilities = { "ev" },
  description  = "Easee Home/Charge via Cloud REST API. No local protocol needed.",
  homepage     = "https://easee.com",
  authors      = { "forty-two-watts contributors" },
  tested_models = { "Home", "Charge" },
}

PROTOCOL = "http"

local BASE_URL = "https://api.easee.com/api"
local access_token = nil
local refresh_token = nil
local token_expires_at = 0   -- millis
local charger_serial = nil

-- ---- Auth helpers ----

local function login(email, password)
    local body = host.json_encode({userName = email, password = password})
    local resp, err = host.http_post(BASE_URL .. "/accounts/login", body)
    if err then
        host.log("error", "Easee login failed: " .. tostring(err))
        return false
    end
    local data = host.json_decode(resp)
    if not data or not data.accessToken then
        host.log("error", "Easee login: no accessToken in response")
        return false
    end
    access_token = data.accessToken
    refresh_token = data.refreshToken
    -- expiresIn is seconds; convert to absolute millis
    local expires_in = data.expiresIn or 3600
    token_expires_at = host.millis() + (expires_in * 1000) - 60000 -- refresh 1 min early
    host.log("info", "Easee: logged in, token expires in " .. expires_in .. "s")
    return true
end

local function do_refresh()
    if not access_token or not refresh_token then return false end
    local body = host.json_encode({
        accessToken = access_token,
        refreshToken = refresh_token
    })
    local resp, err = host.http_post(BASE_URL .. "/accounts/refresh_token", body)
    if err then
        host.log("warn", "Easee token refresh failed: " .. tostring(err))
        return false
    end
    local data = host.json_decode(resp)
    if not data or not data.accessToken then
        host.log("warn", "Easee refresh: no accessToken, will re-login")
        return false
    end
    access_token = data.accessToken
    refresh_token = data.refreshToken
    local expires_in = data.expiresIn or 3600
    token_expires_at = host.millis() + (expires_in * 1000) - 60000
    host.log("debug", "Easee: token refreshed")
    return true
end

local function ensure_auth(email, password)
    if access_token and host.millis() < token_expires_at then
        return true
    end
    -- Try refresh first, fall back to full login
    if access_token and do_refresh() then
        return true
    end
    return login(email, password)
end

local function auth_headers()
    return {Authorization = "Bearer " .. (access_token or "")}
end

-- ---- API helpers ----

local function get_chargers()
    local resp, err = host.http_get(BASE_URL .. "/chargers", auth_headers())
    if err then return nil, err end
    return host.json_decode(resp), nil
end

local function get_state(serial)
    local resp, err = host.http_get(
        BASE_URL .. "/chargers/" .. serial .. "/state",
        auth_headers()
    )
    if err then return nil, err end
    return host.json_decode(resp), nil
end

-- ---- State mapping ----
-- Easee chargerOpMode:
--   1 = disconnected (standby)
--   2 = awaiting start (connected, not charging)
--   3 = charging
--   4 = completed
--   5 = error
--   6 = ready to charge

local OP_MODE_LABELS = {
    [0] = "offline",
    [1] = "disconnected",
    [2] = "awaiting start",
    [3] = "charging",
    [4] = "completed",
    [5] = "error",
    [6] = "ready",
}

-- Easee ReasonForNoCurrent. 0 = charger OK (no reason to surface).
-- Source: developer.easee.com/docs/enumerations
local REASON_LABELS = {
    [0]   = nil, -- no reason
    [1]   = "max circuit current too low",
    [2]   = "dynamic circuit current too low",
    [3]   = "offline fallback circuit current too low",
    [4]   = "circuit fuse too low",
    [5]   = "waiting in queue",
    [6]   = "waiting (other cars fully charged)",
    [7]   = "illegal grid type",
    [8]   = "no current request from primary",
    [9]   = "max dynamic charger current too low",
    [10]  = "phase imbalance",
    [11]  = "equalizer communication lost",
    [25]  = "equalizer dynamic limit too low",
    [26]  = "equalizer static limit too low",
    [27]  = "offline fallback equalizer too low",
    [28]  = "fuse limit reached",
    [29]  = "current limited by equalizer",
    [30]  = "current limited by offline equalizer",
    [50]  = "secondary unit not requesting current",
    [51]  = "max charger current too low",
    [52]  = "max dynamic charger current too low",
    [53]  = "charger disabled",
    [54]  = "pending scheduled charging",
    [55]  = "pending authorization",
    [56]  = "charger in error state",
    [57]  = "erratic EV",
    [75]  = "limited by cable rating",
    [76]  = "limited by schedule",
    [77]  = "limited by charger current",
    [78]  = "limited by dynamic charger current",
    [79]  = "car not drawing current",
    [80]  = "current ramping",
    [81]  = "limited by car",
    [100] = "undefined error",
}

local email, password

function driver_init(config)
    host.set_make("Easee")

    email = config and config.email
    password = config and config.password
    charger_serial = config and config.serial

    if not email or not password then
        host.log("error", "Easee: email and password required in driver config")
        return
    end

    if not ensure_auth(email, password) then
        host.log("error", "Easee: initial login failed")
        return
    end

    -- Auto-detect serial if not provided
    if not charger_serial then
        local chargers, err = get_chargers()
        if err or not chargers or #chargers == 0 then
            host.log("error", "Easee: could not list chargers: " .. tostring(err))
            return
        end
        charger_serial = chargers[1].id
        host.log("info", "Easee: auto-detected charger " .. charger_serial)
    end

    host.set_sn(charger_serial)
    host.log("info", "Easee: driver initialized for " .. charger_serial)
end

function driver_poll()
    if not charger_serial or not email then
        return 10000
    end

    if not ensure_auth(email, password) then
        host.log("warn", "Easee: auth failed, skipping poll")
        return 10000
    end

    local state, err = get_state(charger_serial)
    if err or not state then
        host.log("warn", "Easee: state poll failed: " .. tostring(err))
        return 10000
    end

    local op_mode = state.chargerOpMode or 1
    local power_w = (state.totalPower or 0) * 1000  -- kW → W
    local session_wh = (state.sessionEnergy or 0) * 1000  -- kWh → Wh
    local connected = (op_mode >= 2 and op_mode <= 6)
    local charging = (op_mode == 3)

    local reason_code = state.reasonForNoCurrent
    host.emit("ev", {
        w                       = power_w,
        connected               = connected,
        charging                = charging,
        session_wh              = session_wh,
        op_mode                 = op_mode,                     -- 1=disc,2=awaiting,3=charging,4=completed,5=error,6=ready
        state_label             = OP_MODE_LABELS[op_mode] or "unknown",
        reason_no_current       = reason_code,                 -- int: 0=ok; why NOT drawing current
        reason_no_current_label = reason_code and REASON_LABELS[reason_code], -- nil if 0/ok, string otherwise
        is_online               = state.isOnline,              -- Easee cloud considers charger online
        cable_locked            = state.cableLocked,
        max_a                   = state.dynamicChargerCurrent, -- current dynamic limit (A)
        phases                  = 3,                           -- Easee defaults 3-phase
    })

    -- Diagnostic metrics
    if state.voltage then
        host.emit_metric("ev_voltage_v", state.voltage)
    end
    if state.outputCurrent then
        host.emit_metric("ev_current_a", state.outputCurrent)
    end
    if state.lifetimeEnergy then
        host.emit_metric("ev_lifetime_kwh", state.lifetimeEnergy)
    end
    if state.dynamicChargerCurrent then
        host.emit_metric("ev_dynamic_current_a", state.dynamicChargerCurrent)
    end
    if state.temperature then
        host.emit_metric("ev_temp_c", state.temperature)
    end

    return 5000
end

local function post_command(path)
    local _, err = host.http_post(
        BASE_URL .. "/chargers/" .. charger_serial .. path,
        "null", auth_headers())
    return err == nil
end

function driver_command(action, power_w, cmd)
    if not charger_serial or not ensure_auth(email, password) then return false end

    if action == "ev_start" then
        return post_command("/commands/start_charging")
    elseif action == "ev_pause" then
        return post_command("/commands/pause_charging")
    elseif action == "ev_resume" then
        return post_command("/commands/resume_charging")
    elseif action == "ev_set_current" then
        -- Easee dynamicChargerCurrent is per-phase amps; assume 3-phase.
        local amps = math.floor((power_w or 0) / 230 / 3)
        local body = host.json_encode({dynamicChargerCurrent = amps})
        local _, err = host.http_post(
            BASE_URL .. "/chargers/" .. charger_serial .. "/settings",
            body, auth_headers())
        return err == nil
    end

    return false
end

function driver_default_mode()
    -- No-op — cloud charger manages itself.
end

function driver_cleanup()
    access_token = nil
    refresh_token = nil
end
