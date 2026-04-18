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
  http_hosts   = { "api.easee.com" },
  authors      = { "forty-two-watts contributors" },
  tested_models = { "Home", "Charge" },
  verification_status = "production",
  verified_by = { "frahlg@homelab-rpi:2d", "erikarenhill@fortytwo:1d" },
  verified_at = "2026-04-18",
  verification_notes = "Observations API + lifecycle commands exercised against an Easee Home charger. Session state, op_mode labels, charge/pause/resume all verified.",
}

PROTOCOL = "http"

local BASE_URL = "https://api.easee.com/api"
local access_token = nil
local refresh_token = nil
local token_expires_at = 0   -- millis
local charger_serial = nil
local phases = 3   -- populated from config.phases (if present) in driver_init

-- Easee error bodies have historically echoed submitted form data
-- (credentials, tokens). Strip the body and keep only the status prefix
-- so nothing sensitive ever lands in the driver log.
local function redact_http_err(err)
    if err == nil then return "request failed" end
    return tostring(err):match("^(HTTP %d+)") or "request failed"
end

-- ---- Auth helpers ----

local function login(email, password)
    local body = host.json_encode({userName = email, password = password})
    local resp, err = host.http_post(BASE_URL .. "/accounts/login", body)
    if err then
        host.log("error", "Easee login failed: " .. redact_http_err(err))
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
        host.log("warn", "Easee token refresh failed: " .. redact_http_err(err))
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

-- Observation IDs (from developer.easee.com/docs/charger-observation-ids)
local OBS_DYN_CURRENT     = 48
local OBS_REASON_NO_CUR   = 96
local OBS_CABLE_LOCKED    = 103
local OBS_OP_MODE         = 109
local OBS_TOTAL_POWER     = 120
local OBS_SESSION_ENERGY  = 121
local OBS_LIFETIME_ENERGY = 124
local OBS_CURRENT         = 183
local OBS_VOLTAGE         = 194

local OBS_IDS = "48,96,103,109,120,121,124,183,194"

local function get_observations(serial)
    local url = "https://api.easee.com/state/" .. serial .. "/observations?ids=" .. OBS_IDS
    local resp, err = host.http_get(url, auth_headers())
    if err then return nil, err end
    local decoded = host.json_decode(resp)
    if not decoded then return nil, "decode failed" end
    local list = decoded.observations or decoded
    local obs = {}
    for _, item in ipairs(list) do
        if item.id then
            obs[item.id] = tonumber(item.value) or item.value
        end
    end
    return obs, nil
end

-- ---- State mapping ----
-- Easee chargerOpMode (observation 109):
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
    -- UI writes "" for an unselected <select>. In Lua "" is truthy, so the
    -- auto-detect branch below would never fire without this normalization.
    if charger_serial == "" then charger_serial = nil end
    if email == "" then email = nil end
    if password == "" then password = nil end

    -- Phases: default to 3 since that's the common European install.
    -- Users on a single-phase service (Easee Home <11 kW) must set
    -- `phases: 1` in config, otherwise amperage math for ev_set_current
    -- is 3x under-requested and can fall below the 6 A Easee minimum
    -- (silently halting the session).
    if config and tonumber(config.phases) then
        local p = math.floor(tonumber(config.phases))
        if p == 1 or p == 2 or p == 3 then phases = p end
    end

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
        local chargers, cerr = get_chargers()
        if cerr or not chargers or #chargers == 0 then
            host.log("error", "Easee: could not list chargers: " .. redact_http_err(cerr))
            return
        end
        charger_serial = chargers[1].id
        host.log("info", "Easee: auto-detected charger " .. tostring(charger_serial))
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

    local obs, err = get_observations(charger_serial)
    if err or not obs then
        host.log("warn", "Easee: observations poll failed: " .. redact_http_err(err))
        return 10000
    end

    local op_mode = obs[OBS_OP_MODE] or 1
    local power_w = (obs[OBS_TOTAL_POWER] or 0) * 1000  -- kW → W
    local session_wh = (obs[OBS_SESSION_ENERGY] or 0) * 1000  -- kWh → Wh
    local connected = (op_mode >= 2 and op_mode <= 6)
    local charging = (op_mode == 3)
    -- op_mode 0 is the sentinel Easee emits when the cloud hasn't heard
    -- from the unit recently. Anything else means the charger itself is
    -- responsive even when no car is plugged in.
    local is_online = (op_mode ~= 0)

    local reason_code = obs[OBS_REASON_NO_CUR]
    local cable_locked = obs[OBS_CABLE_LOCKED]
    if cable_locked ~= nil then cable_locked = (cable_locked == 1 or cable_locked == true) end
    local dyn_current = obs[OBS_DYN_CURRENT]

    host.emit("ev", {
        w                       = power_w,
        connected               = connected,
        charging                = charging,
        session_wh              = session_wh,
        op_mode                 = op_mode,                     -- 1=disc,2=awaiting,3=charging,4=completed,5=error,6=ready
        state_label             = OP_MODE_LABELS[op_mode] or "unknown",
        reason_no_current       = reason_code,                 -- int: 0=ok; why NOT drawing current
        reason_no_current_label = reason_code and REASON_LABELS[reason_code], -- nil if 0/ok, string otherwise
        is_online               = is_online,
        cable_locked            = cable_locked,
        max_a                   = dyn_current,                 -- current dynamic limit (A)
        phases                  = phases,                      -- from config.phases; default 3
    })

    if obs[OBS_VOLTAGE] then
        host.emit_metric("ev_voltage_v", obs[OBS_VOLTAGE])
    end
    if obs[OBS_CURRENT] then
        host.emit_metric("ev_current_a", obs[OBS_CURRENT])
    end
    if obs[OBS_LIFETIME_ENERGY] then
        host.emit_metric("ev_lifetime_kwh", obs[OBS_LIFETIME_ENERGY])
    end
    if dyn_current then
        host.emit_metric("ev_dynamic_current_a", dyn_current)
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
        -- Easee dynamicChargerCurrent is per-phase amps, so divide by the
        -- configured phase count. Round (not floor) so a request like
        -- 4000 W on 3φ produces 6 A (4000/3/230≈5.8) rather than 5 A
        -- which the charger would reject as below its 6 A minimum.
        local amps = math.floor(((power_w or 0) / 230 / phases) + 0.5)
        -- Clamp to the Easee 0..32 A permitted band. 0 pauses the session
        -- cleanly; anything <6 A the charger rejects as below minimum, so
        -- normalise those to 0 to avoid a silent "no current" state.
        if amps > 0 and amps < 6 then amps = 0 end
        if amps > 32 then amps = 32 end
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
