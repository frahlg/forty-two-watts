// Settings modal — fetch, edit, save config
(function () {
  "use strict";

  var modal = document.getElementById("settings-modal");
  var openBtn = document.getElementById("settings-btn");
  var closeBtn = document.getElementById("settings-close");
  var saveBtn = document.getElementById("settings-save");
  var statusEl = document.getElementById("settings-status");
  var tabsEl = document.getElementById("settings-tabs");
  var bodyEl = document.getElementById("settings-body");

  if (!modal || !openBtn) return;

  var currentConfig = null;
  var currentTab = "control";

  openBtn.addEventListener("click", function () {
    fetch("/api/config")
      .then(function (r) { return r.json(); })
      .then(function (cfg) {
        currentConfig = cfg;
        modal.classList.remove("hidden");
        renderTab(currentTab);
        setStatus("");
      })
      .catch(function (e) {
        setStatus("Failed to load config: " + e, "error");
      });
  });

  closeBtn.addEventListener("click", function () {
    modal.classList.add("hidden");
  });
  modal.addEventListener("click", function (e) {
    if (e.target === modal) modal.classList.add("hidden");
  });

  tabsEl.addEventListener("click", function (e) {
    if (e.target.tagName === "BUTTON" && e.target.dataset.tab) {
      tabsEl.querySelectorAll("button").forEach(function (b) {
        b.classList.toggle("active", b === e.target);
      });
      // Capture form values from current tab before switching
      captureCurrentTab();
      currentTab = e.target.dataset.tab;
      renderTab(currentTab);
    }
  });

  saveBtn.addEventListener("click", function () {
    captureCurrentTab();
    setStatus("Saving...");
    fetch("/api/config", {
      method: "POST",
      headers: { "Content-Type": "application/json" },
      body: JSON.stringify(currentConfig),
    })
      .then(function (r) {
        if (!r.ok) return r.json().then(function (j) { throw new Error(j.error || ("HTTP " + r.status)); });
        return r.json();
      })
      .then(function () {
        setStatus("Saved ✓", "success");
        setTimeout(function () { setStatus(""); }, 2000);
      })
      .catch(function (e) {
        setStatus("Save failed: " + e.message, "error");
      });
  });

  function setStatus(msg, kind) {
    statusEl.textContent = msg || "";
    statusEl.className = "settings-status" + (kind ? " " + kind : "");
  }

  function captureCurrentTab() {
    var inputs = bodyEl.querySelectorAll("[data-path]");
    inputs.forEach(function (input) {
      var path = input.dataset.path;
      var val = input.type === "number" ? parseFloat(input.value) : input.value;
      if (input.type === "number" && isNaN(val)) val = 0;
      // Scale display units back to internal units (e.g. kWh → Wh)
      if (input.type === "number" && input.dataset.unitScale) {
        val = val * parseFloat(input.dataset.unitScale);
      }
      // Don't overwrite a saved password with empty string when user hasn't typed anything
      if (input.type === "password" && val === "" && getByPath(currentConfig, path, "")) return;
      setByPath(currentConfig, path, val);
    });
  }

  function setByPath(obj, path, val) {
    var parts = path.split(".");
    var node = obj;
    for (var i = 0; i < parts.length - 1; i++) {
      if (!node[parts[i]]) node[parts[i]] = {};
      node = node[parts[i]];
    }
    node[parts[parts.length - 1]] = val;
  }

  function getByPath(obj, path, dflt) {
    var parts = path.split(".");
    var node = obj;
    for (var i = 0; i < parts.length; i++) {
      if (node == null) return dflt;
      node = node[parts[i]];
    }
    return node == null ? dflt : node;
  }

  // help() builds a "?" badge next to a field label that shows an
  // explanation on hover. Kept as HTML attrs so no JS wiring needed
  // — the browser's native tooltip is enough for a first pass, and
  // we also style a nicer custom one via :hover::after in CSS.
  function help(text) {
    return '<span class="help" data-help="' + escHtml(text) + '" title="' + escHtml(text) + '">?</span>';
  }

  function field(label, path, type, dflt, helpText) {
    var val = getByPath(currentConfig, path, dflt);
    return '<label>' + label + (helpText ? ' ' + help(helpText) : '') + '</label>' +
      '<input type="' + type + '" data-path="' + path + '" value="' + escHtml(val == null ? "" : String(val)) + '">';
  }

  function selectField(label, path, options, dflt, helpText) {
    var val = getByPath(currentConfig, path, dflt);
    var opts = options.map(function (o) {
      return '<option value="' + o + '"' + (o === val ? ' selected' : '') + '>' + o + '</option>';
    }).join("");
    return '<label>' + label + (helpText ? ' ' + help(helpText) : '') + '</label>' +
      '<select data-path="' + path + '">' + opts + '</select>';
  }

  function renderTab(tab) {
    var html = "";
    switch (tab) {
      case "control":
        html = '<fieldset><legend>Site</legend>' +
          field("Name", "site.name", "text", "Home") +
          '<div class="field-row"><div>' +
          field("Grid target (W)", "site.grid_target_w", "number", 0) +
          '</div><div>' +
          field("Grid tolerance (W)", "site.grid_tolerance_w", "number", 42) +
          '</div></div>' +
          '<div class="field-row"><div>' +
          field("Slew rate (W/cycle)", "site.slew_rate_w", "number", 500) +
          '</div><div>' +
          field("Min dispatch interval (s)", "site.min_dispatch_interval_s", "number", 5) +
          '</div></div>' +
          '<div class="field-row"><div>' +
          field("Smoothing alpha", "site.smoothing_alpha", "number", 0.3,
            "EMA smoothing factor for the grid reading (0-1). Lower = smoother but slower response.") +
          '</div><div>' +
          field("PI gain", "site.gain", "number", 0.5,
            "Proportional gain of the PI controller. Higher = more aggressive correction.") +
          '</div></div>' +
          '<div class="field-row"><div>' +
          field("Control interval (s)", "site.control_interval_s", "number", 5) +
          '</div><div>' +
          field("Watchdog timeout (s)", "site.watchdog_timeout_s", "number", 60) +
          '</div></div>' +
          '</fieldset>' +
          '<fieldset><legend>Fuse</legend>' +
          '<div class="field-row"><div>' +
          field("Max amps (A)", "fuse.max_amps", "number", 16) +
          '</div><div>' +
          field("Phases", "fuse.phases", "number", 3) +
          '</div></div>' +
          field("Voltage (V)", "fuse.voltage", "number", 230) +
          '</fieldset>';
        break;

      case "devices":
        if (!currentConfig.drivers) currentConfig.drivers = [];
        // Catalog picker lives at the top: fetch on tab open, render
        // the dropdown asynchronously (doesn't block the tab).
        html = '<fieldset><legend>Add from catalog</legend>' +
          '<div class="field-row"><div>' +
          '<label>Driver <span class="help" data-help="Pick a Lua driver from the drivers/ directory. Each driver declares its capabilities (MQTT/Modbus) + which manufacturer/model it supports.">?</span></label>' +
          '<select id="driver-catalog-picker"><option value="">Loading catalog…</option></select>' +
          '</div><div>' +
          '<label>Friendly name</label><input type="text" id="driver-catalog-name" placeholder="e.g. ferroamp-house">' +
          '</div></div>' +
          '<button class="btn-add" id="driver-catalog-add">+ Add selected</button>' +
          '</fieldset>';
        setTimeout(function () {
          fetch("/api/drivers/catalog").then(function (r) { return r.json(); }).then(function (data) {
            var sel = document.getElementById("driver-catalog-picker");
            if (!sel) return;
            sel.innerHTML = "";
            var entries = (data && data.entries) || [];
            if (entries.length === 0) {
              sel.innerHTML = "<option value=''>(no drivers found in drivers/)</option>";
              return;
            }
            entries.forEach(function (e) {
              var opt = document.createElement("option");
              opt.value = e.path;
              var protoLabel = (e.protocols || []).join("+");
              opt.textContent = (e.name || e.filename) + "  —  " + (e.manufacturer || "?") + "  [" + protoLabel + "]" + (e.version ? "  v" + e.version : "");
              opt.dataset.protocols = protoLabel;
              opt.dataset.id = e.id || "";
              opt.dataset.httpHosts = (e.http_hosts || []).join(",");
              // connection_defaults.host is the local-HTTP discriminator —
              // http_hosts is declared by cloud drivers too (Easee uses it
              // for allowed-hosts), so it can't be used on its own.
              opt.dataset.connectionHost = (e.connection_defaults && e.connection_defaults.host) || "";
              sel.appendChild(opt);
            });
          });
          var btn = document.getElementById("driver-catalog-add");
          if (btn) btn.addEventListener("click", function () {
            var sel = document.getElementById("driver-catalog-picker");
            var nameEl = document.getElementById("driver-catalog-name");
            if (!sel || !sel.value) return;
            var chosen = sel.options[sel.selectedIndex];
            var protocols = (chosen.dataset.protocols || "").split("+");
            var name = (nameEl.value || "").trim() || chosen.dataset.id || ("driver-" + currentConfig.drivers.length);
            var driver = { name: name, lua: sel.value };
            driver.capabilities = {};
            if (protocols.indexOf("mqtt") >= 0) driver.capabilities.mqtt = { host: "", port: 1883 };
            if (protocols.indexOf("modbus") >= 0) driver.capabilities.modbus = { host: "", port: 502, unit_id: 1 };
            if (protocols.indexOf("http") >= 0) {
              var hosts = (chosen.dataset.httpHosts || "").split(",").filter(Boolean);
              driver.capabilities.http = { allowed_hosts: hosts };
              // connection_defaults.host is declared only by drivers that
              // take a user-configurable local endpoint (e.g. Sourceful
              // Zap → "zap.local"). Cloud drivers like Easee declare
              // http_hosts for allowed-hosts but have no connection_defaults
              // — their endpoint is hardcoded and they need email/password
              // in config instead.
              var connHost = chosen.dataset.connectionHost || "";
              if (connHost) {
                driver.config = { host: connHost };
              } else {
                driver.config = { email: "", password: "", serial: "" };
              }
            }
            currentConfig.drivers.push(driver);
            renderTab("devices");
          });

          // Wire up Connect buttons for cloud drivers
          bodyEl.querySelectorAll(".ev-connect-btn").forEach(function (btn) {
            btn.addEventListener("click", function () {
              var dIdx = btn.dataset.driverIdx;
              var statusEl = document.getElementById("ev-connect-status-" + dIdx);
              var sel = document.getElementById("ev-charger-select-" + dIdx);
              var emailInput = bodyEl.querySelector('[data-path="drivers.' + dIdx + '.config.email"]');
              var pwInput = bodyEl.querySelector('[data-path="drivers.' + dIdx + '.config.password"]');
              var email = emailInput ? emailInput.value : "";
              var pw = pwInput ? pwInput.value : "";
              if (!email) { if (statusEl) statusEl.textContent = "Enter email first"; return; }
              if (statusEl) statusEl.textContent = "Connecting...";
              btn.disabled = true;
              // Derive the provider name from the driver's lua path so a new
              // cloud driver can slot in without touching this button — e.g.
              // "drivers/easee_cloud.lua" → "easee". Strip any directory
              // prefix, the trailing "_cloud" tag, and the ".lua" extension;
              // fall back to "easee" when nothing matches (preserves current
              // behavior for oddly-named files and for the case where the
              // driver config itself is missing a lua path).
              var dCfg = currentConfig && currentConfig.drivers
                ? currentConfig.drivers[dIdx] : null;
              var provider = "easee";
              if (dCfg && typeof dCfg.lua === "string" && dCfg.lua !== "") {
                provider = dCfg.lua
                  .replace(/^.*[\\/]/, "")   // strip dirs
                  .replace(/\.lua$/i, "")
                  .replace(/_cloud$/i, "");
                if (!provider) provider = "easee";
              }
              fetch("/api/ev/chargers", {
                method: "POST",
                headers: { "Content-Type": "application/json" },
                body: JSON.stringify({ provider: provider, email: email, password: pw })
              }).then(function (r) {
                if (!r.ok) return r.json().then(function (j) { throw new Error(j.error || "HTTP " + r.status); });
                return r.json();
              }).then(function (chargers) {
                if (!sel || !Array.isArray(chargers) || chargers.length === 0) {
                  if (statusEl) statusEl.textContent = "No chargers found";
                  return;
                }
                var d = currentConfig.drivers[dIdx];
                var current = (d && d.config && d.config.serial) || "";
                sel.innerHTML = "";
                chargers.forEach(function (ch) {
                  var opt = document.createElement("option");
                  opt.value = ch.id;
                  opt.textContent = ch.id + (ch.name ? "  —  " + ch.name : "");
                  if (ch.id === current) opt.selected = true;
                  sel.appendChild(opt);
                });
                // Update config immediately so save picks up the selected charger.
                // Assign via onchange (not addEventListener) so repeated Connect
                // clicks replace the handler instead of stacking duplicates that
                // each write to config.
                var selected = sel.value;
                if (d && d.config) d.config.serial = selected;
                if (currentConfig.ev_charger) currentConfig.ev_charger.serial = selected;
                sel.onchange = function () {
                  if (d && d.config) d.config.serial = sel.value;
                  if (currentConfig.ev_charger) currentConfig.ev_charger.serial = sel.value;
                };
                if (statusEl) statusEl.textContent = chargers.length + " charger(s) found";
              }).catch(function (e) {
                if (statusEl) statusEl.textContent = "Error: " + e.message;
              }).finally(function () {
                btn.disabled = false;
              });
            });
          });
        }, 0);
        html += '<div class="devices-list">';
        currentConfig.drivers.forEach(function (d, idx) {
          var cap = d.capabilities || {};
          var mqtt = cap.mqtt || d.mqtt; // legacy fallback
          var modbus = cap.modbus || d.modbus;
          var protocol = mqtt ? "mqtt" : (modbus ? "modbus" : (cap.http ? "http" : "?"));
          var driverFile = d.lua || "(none)";
          html += '<div class="device-item">' +
            '<div class="device-item-header">' +
            '<strong>' + escHtml(d.name) + '</strong>' +
            '<span style="color:var(--text-dim);font-size:0.75rem">lua · ' + protocol + ' · ' + escHtml(driverFile) + '</span>' +
            '<button class="btn-remove" data-remove-idx="' + idx + '">Remove</button>' +
            '</div>' +
            '<div class="field-row"><div>' +
            '<label>Driver file ' + help('Path to the .lua driver. Absolute or relative to the config file directory.') + '</label>' +
            '<input type="text" data-path="drivers.' + idx + '.lua" value="' + escHtml(driverFile) + '">' +
            '</div><div>' +
            '<label>Battery capacity (kWh) ' + help('Nameplate storage capacity in kilowatt-hours. Stored internally as Wh.') + '</label>' +
            '<input type="number" step="0.1" data-path="drivers.' + idx + '.battery_capacity_wh" data-unit-scale="1000" value="' + ((d.battery_capacity_wh || 0) / 1000) + '">' +
            '</div></div>' +
            '<label><input type="checkbox" data-checkbox-path="drivers.' + idx + '.is_site_meter"' + (d.is_site_meter ? ' checked' : '') + '> Site meter ' + help('Exactly one driver should be the site meter — its grid reading defines the point-of-measurement the PI loop balances.') + '</label>';
          if (mqtt) {
            html += '<fieldset><legend>MQTT</legend>' +
              '<div class="field-row"><div>' +
              '<label>Host ' + help('IP or hostname of the MQTT broker exposing the device data (e.g. the Ferroamp EnergyHub).') + '</label>' +
              '<input type="text" data-path="drivers.' + idx + '.capabilities.mqtt.host" value="' + escHtml(mqtt.host) + '">' +
              '</div><div>' +
              '<label>Port</label><input type="number" data-path="drivers.' + idx + '.capabilities.mqtt.port" value="' + (mqtt.port || 1883) + '">' +
              '</div></div>' +
              '<div class="field-row"><div>' +
              '<label>Username</label><input type="text" data-path="drivers.' + idx + '.capabilities.mqtt.username" value="' + escHtml(mqtt.username || "") + '">' +
              '</div><div>' +
              '<label>Password</label><input type="password" data-path="drivers.' + idx + '.capabilities.mqtt.password" value="' + escHtml(mqtt.password || "") + '">' +
              '</div></div></fieldset>';
          }
          if (modbus) {
            html += '<fieldset><legend>Modbus TCP</legend>' +
              '<div class="field-row"><div>' +
              '<label>Host ' + help('IP of the Modbus-TCP device (e.g. Sungrow inverter LAN port).') + '</label>' +
              '<input type="text" data-path="drivers.' + idx + '.capabilities.modbus.host" value="' + escHtml(modbus.host) + '">' +
              '</div><div>' +
              '<label>Port</label><input type="number" data-path="drivers.' + idx + '.capabilities.modbus.port" value="' + (modbus.port || 502) + '">' +
              '</div></div>' +
              '<label>Unit ID ' + help('Slave address. Usually 1 for a single-device setup.') + '</label>' +
              '<input type="number" data-path="drivers.' + idx + '.capabilities.modbus.unit_id" value="' + (modbus.unit_id || 1) + '">' +
              '</fieldset>';
          }
          // Distinguish "local HTTP driver that needs an IP" (e.g. Sourceful
          // Zap) from "cloud API driver that needs creds" (e.g. Easee) by
          // the config shape the driver declared: config.host → local,
          // config.email/password → cloud. Name-based matching rots the
          // moment a new driver lands. If only an http capability is set
          // and the config is empty, fall back to cloud so a hand-edited
          // yaml still surfaces the auth form.
          var dcfg = d.config || {};
          var hasHostField = Object.prototype.hasOwnProperty.call(dcfg, 'host');
          var hasAuthField = Object.prototype.hasOwnProperty.call(dcfg, 'email') ||
                             Object.prototype.hasOwnProperty.call(dcfg, 'password');
          var isLocalHTTP = cap.http != null && hasHostField;
          var isCloudDriver = cap.http != null && !hasHostField && (hasAuthField || Object.keys(dcfg).length === 0);
          if (isLocalHTTP) {
            var lcfg = d.config || {};
            html += '<fieldset><legend>HTTP</legend>' +
              '<label>Host / IP ' + help('Hostname (e.g. zap.local) or IP address of the device. mDNS names work when your OS resolver supports them; otherwise use the LAN IP.') + '</label>' +
              '<input type="text" data-path="drivers.' + idx + '.config.host" value="' + escHtml(lcfg.host || '') + '" placeholder="zap.local">' +
              '</fieldset>';
          }
          if (isCloudDriver) {
            var cfg = d.config || {};
            // Server sets has_password=true via MaskSecrets when a password
            // is on disk — lets us show a "saved" badge even though the
            // value itself is blanked out of the response.
            var hasPw = d.has_password === true;
            var pwBadge = hasPw
              ? '<span class="creds-badge creds-saved">✓ Saved</span>'
              : '<span class="creds-badge creds-missing">⚠ Not saved</span>';
            html += '<fieldset><legend>Cloud credentials</legend>' +
              '<div class="field-row"><div>' +
              '<label>Email ' + help('Account email for the cloud service.') + '</label>' +
              '<input type="text" data-path="drivers.' + idx + '.config.email" value="' + escHtml(cfg.email || '') + '">' +
              '</div><div>' +
              '<label>Password ' + pwBadge + '</label>' +
              '<input type="password" data-path="drivers.' + idx + '.config.password" value="" ' +
                'placeholder="' + (hasPw ? '•••••••• (leave empty to keep)' : 'enter password') + '">' +
              '</div></div>' +
              '<div class="field-row" style="align-items:flex-end"><div style="flex:1">' +
              '<label>Charger ' + help('Click Connect to load chargers from your account.') + '</label>' +
              '<select id="ev-charger-select-' + idx + '" data-path="drivers.' + idx + '.config.serial">' +
              (cfg.serial
                ? '<option value="' + escHtml(cfg.serial) + '" selected>' + escHtml(cfg.serial) + '</option>'
                : '<option value="">(not connected)</option>') +
              '</select>' +
              '</div><div>' +
              '<button class="btn-add ev-connect-btn" type="button" data-driver-idx="' + idx + '">Connect</button>' +
              '</div></div>' +
              '<span id="ev-connect-status-' + idx + '" style="font-size:0.8rem;color:var(--text-dim)"></span>' +
              '</fieldset>';
          }
          html += '</div>';
        });
        html += '</div>' +
          '<a href="/setup?step=3" class="btn-add" style="display:block;text-align:center;text-decoration:none">Add new device&hellip;</a>' +
          '<button class="btn-add" id="add-mqtt">+ Add MQTT device</button>' +
          '<button class="btn-add" id="add-modbus">+ Add Modbus device</button>';
        break;

      case "price":
        if (!currentConfig.price) currentConfig.price = {};
        html = '<fieldset><legend>Spot price</legend>' +
          selectField("Provider", "price.provider", ["elprisetjustnu", "entsoe", "none"], "elprisetjustnu") +
          selectField("Zone", "price.zone", ["SE1","SE2","SE3","SE4","NO1","NO2","NO3","NO4","DK1","DK2","FI","DE"], "SE3") +
          selectField("Currency", "price.currency", ["SEK", "NOK", "DKK", "EUR"], "SEK") +
          '<div class="field-row"><div>' +
          field("Grid tariff excl. VAT (öre/kWh)", "price.grid_tariff_ore_kwh", "number", 60,
            "Per-kWh network/distribution fee from your DSO (elnätsavgift), excluding VAT. This is the cost of moving electricity over the wire, independent of the spot price.") +
          '</div><div>' +
          field("VAT (%)", "price.vat_percent", "number", 25) +
          '</div></div>' +
          '<div class="field-row"><div>' +
          field("Export bonus (öre/kWh)", "price.export_bonus_ore_kwh", "number", 0) +
          '</div><div>' +
          field("Export fee (öre/kWh)", "price.export_fee_ore_kwh", "number", 0) +
          '</div></div>' +
          '<p id="tariff-warning" class="tariff-warning" style="display:none">' +
          '⚠ Grid tariff below ~60 öre/kWh (0.06 €/kWh) is unusually low. ' +
          'Underestimating it will make the MPC planner over-charge from the grid — you may lose money. ' +
          'Include DSO transmission fee + any fixed taxes.</p>' +
          field("API key (ENTSO-E only)", "price.api_key", "text", "") +
          '</fieldset>' +
          '<p style="color:var(--text-dim);font-size:0.8rem;margin-top:8px">' +
          'elprisetjustnu.se is free and requires no key (Sweden only). ENTSO-E covers all EU but needs an API key. ' +
          'Currency applies to ENTSO-E only; FX rates come from ECB daily.' +
          '</p>';
        setTimeout(function () {
          var input = document.querySelector('[data-path="price.grid_tariff_ore_kwh"]');
          var warn = document.getElementById('tariff-warning');
          if (!input || !warn) return;
          function check() {
            var v = parseFloat(input.value);
            if (!isNaN(v) && v < 60) {
              warn.style.display = 'block';
              input.classList.add('field-warn');
            } else {
              warn.style.display = 'none';
              input.classList.remove('field-warn');
            }
          }
          input.addEventListener('input', check);
          check();
        }, 0);
        break;

      case "weather":
        if (!currentConfig.weather) currentConfig.weather = { latitude: 59.3293, longitude: 18.0686 };
        if (!Array.isArray(currentConfig.weather.pv_arrays)) currentConfig.weather.pv_arrays = [];
        html = '<fieldset><legend>Weather forecast &amp; PV</legend>' +
          selectField("Provider", "weather.provider", ["met_no", "openweather", "open_meteo", "forecast_solar", "none"], "met_no",
            "met_no + openweather: cloud-cover only. open_meteo: direct shortwave radiation (better day-one forecast). forecast_solar: site-calibrated watts using the panel geometry below (best with multi-array setups).") +
          '<div class="field-row"><div>' +
          field("Latitude", "weather.latitude", "number", 59.3293) +
          '</div><div>' +
          field("Longitude", "weather.longitude", "number", 18.0686) +
          '</div></div>' +
          '<div id="weather-map" style="height:260px;border-radius:6px;margin:6px 0;background:#1e293b"></div>' +
          '<p style="color:var(--text-dim);font-size:0.75rem;margin:-2px 0 8px">Click or drag the marker to set your location.</p>' +
          field("PV rated (W)", "weather.pv_rated_w", "number", 10000) +
          field("API key (OpenWeather only)", "weather.api_key", "text", "") +
          '</fieldset>' +
          // Multi-array editor — only actually used by forecast_solar, but
          // visible under all providers so advanced operators can enter
          // their site geometry up front. Each row: optional label + kWp +
          // tilt + azimuth. Lets forecast_solar sum per-plane output
          // instead of averaging into a single tilt/azimuth.
          '<fieldset><legend>PV arrays ' + help(
            'Optional. If set, forecast_solar uses these per-plane values to produce a site-calibrated forecast. ' +
            'Leave empty to let the model learn your orientation from telemetry — predictions are fine after a few varied days.') + '</legend>' +
          '<div id="pv-arrays-list"></div>' +
          '<button class="btn-add" id="pv-array-add" type="button">+ Add array</button>' +
          '<p style="color:var(--text-dim);font-size:0.75rem;margin:8px 0 0">' +
          'Tilt: 0° = flat roof, 35° = typical pitched roof, 90° = wall. Azimuth: 0 = N, 90 = E, 180 = S, 270 = W.' +
          '</p>' +
          '</fieldset>';
        // Init Leaflet + arrays editor after innerHTML is set
        setTimeout(function() {
          initWeatherMap();
          renderPVArrays();
          var addBtn = document.getElementById("pv-array-add");
          if (addBtn) addBtn.addEventListener("click", function () {
            currentConfig.weather.pv_arrays.push({ name: "", kwp: 0, tilt_deg: 35, azimuth_deg: 180 });
            renderPVArrays();
          });
        }, 0);
        break;

      case "batteries":
        if (!currentConfig.batteries) currentConfig.batteries = {};
        html = '<p style="color:var(--text-dim);font-size:0.8rem">Per-battery limits override the defaults. Leave blank to use BMS defaults.</p>';
        currentConfig.drivers.forEach(function (d) {
          if (d.battery_capacity_wh > 0) {
            if (!currentConfig.batteries[d.name]) currentConfig.batteries[d.name] = {};
            html += '<fieldset><legend>' + escHtml(d.name) + ' &mdash; ' + (d.battery_capacity_wh/1000).toFixed(1) + ' kWh</legend>' +
              '<div class="field-row"><div>' +
              field("Min SoC (fraction 0–1)", "batteries." + d.name + ".soc_min", "number", "",
                "Lower SoC bound the planner is allowed to discharge to. 0.10 = 10%. Leave blank to use the battery BMS default.") +
              '</div><div>' +
              field("Max SoC (fraction 0–1)", "batteries." + d.name + ".soc_max", "number", "",
                "Upper SoC bound the planner is allowed to charge to. 0.95 = 95%. Avoid 1.0 to extend battery life.") +
              '</div></div>' +
              '<div class="field-row"><div>' +
              field("Max charge (W)", "batteries." + d.name + ".max_charge_w", "number", "",
                "Peak charge rate the driver will command. Defaults to 0.5C (half capacity).") +
              '</div><div>' +
              field("Max discharge (W)", "batteries." + d.name + ".max_discharge_w", "number", "",
                "Peak discharge rate the driver will command. Defaults to 0.5C.") +
              '</div></div>' +
              field("Weight (for weighted mode)", "batteries." + d.name + ".weight", "number", 1,
                "Share of correction this battery takes when control mode is 'weighted'. 1.0 = equal with other batteries.") +
              '</fieldset>';
          }
        });
        break;

      case "planner":
        if (!currentConfig.planner) currentConfig.planner = {};
        html = '<fieldset><legend>MPC Planner</legend>' +
          '<label><input type="checkbox" data-checkbox-path="planner.enabled"' + (currentConfig.planner.enabled ? ' checked' : '') + '> Enabled ' +
          help('Enable the MPC planner. When active it overrides manual mode with an optimised schedule.') + '</label>' +
          selectField("Mode", "planner.mode", ["self_consumption", "cheap_charge", "arbitrage"], "self_consumption",
            "self_consumption = minimise grid import. cheap_charge = charge batteries during cheapest hours. arbitrage = buy low / sell high.") +
          '<div class="field-row"><div>' +
          field("SoC min (%)", "planner.soc_min_pct", "number", 10,
            "Lowest SoC the planner will discharge to (percent). 10 = 10%.") +
          '</div><div>' +
          field("SoC max (%)", "planner.soc_max_pct", "number", 90,
            "Highest SoC the planner will charge to (percent). 90 = 90%.") +
          '</div></div>' +
          '<div class="field-row"><div>' +
          field("Base load (W)", "planner.base_load_w", "number", 0,
            "Constant household load estimate used when the load twin has no data yet.") +
          '</div><div>' +
          field("Horizon (hours)", "planner.horizon_hours", "number", 48,
            "Planning horizon in hours. 48 h covers two day-ahead price windows.") +
          '</div></div>' +
          '<div class="field-row"><div>' +
          field("Replan interval (min)", "planner.interval_min", "number", 15,
            "How often the planner re-solves. Lower = more responsive but more CPU.") +
          '</div><div>' +
          field("Export value (ore/kWh)", "planner.export_ore_per_kwh", "number", 0,
            "Override export value. 0 = use mean spot price.") +
          '</div></div>' +
          '<div class="field-row"><div>' +
          field("Charge efficiency", "planner.charge_efficiency", "number", 0.95,
            "Round-trip charge efficiency (0-1). 0.95 = 5% loss charging.") +
          '</div><div>' +
          field("Discharge efficiency", "planner.discharge_efficiency", "number", 0.95,
            "Round-trip discharge efficiency (0-1). 0.95 = 5% loss discharging.") +
          '</div></div>' +
          '</fieldset>' +
          '<p style="color:var(--text-dim);font-size:0.8rem;margin-top:8px">' +
          'The planner requires working price + weather forecasts. When disabled the system runs in the manual mode set on the Control page.' +
          '</p>';
        break;

      case "ha":
        if (!currentConfig.homeassistant) currentConfig.homeassistant = {};
        html = '<div id="ha-status-indicator" class="ha-status-indicator">checking…</div>' +
          '<fieldset><legend>Home Assistant MQTT</legend>' +
          '<label><input type="checkbox" data-checkbox-path="homeassistant.enabled"' + (currentConfig.homeassistant.enabled ? ' checked' : '') + '> Enabled</label>' +
          '<div class="field-row"><div>' +
          field("Broker host", "homeassistant.broker", "text", "192.168.1.1",
            "IP or hostname of the MQTT broker Home Assistant uses. Typically the HA server itself (Mosquitto addon).") +
          '</div><div>' +
          field("Port", "homeassistant.port", "number", 1883) +
          '</div></div>' +
          '<div class="field-row"><div>' +
          field("Username", "homeassistant.username", "text", "") +
          '</div><div>' +
          field("Password", "homeassistant.password", "password", "") +
          '</div></div>' +
          field("Publish interval (s)", "homeassistant.publish_interval_s", "number", 5,
            "How often state topics are pushed to HA. 5 s is a good default.") +
          '</fieldset>' +
          '<p style="color:var(--text-dim);font-size:0.8rem;margin-top:8px">Changes to HA config require a restart to take effect.</p>';
        setTimeout(function () {
          var el = document.getElementById("ha-status-indicator");
          if (!el) return;
          function refresh() {
            fetch("/api/ha/status").then(function(r){return r.json();}).then(function(d) {
              if (!d.enabled) {
                el.className = "ha-status-indicator ha-off";
                el.textContent = "○  disabled in config";
                return;
              }
              if (d.connected) {
                var age = d.last_publish_ms > 0 ? Math.round((Date.now() - d.last_publish_ms)/1000) + "s ago" : "no publish yet";
                el.className = "ha-status-indicator ha-ok";
                el.textContent = "● connected to " + d.broker + "  ·  " + (d.sensors_announced || 0) + " sensors  ·  last publish " + age;
              } else {
                el.className = "ha-status-indicator ha-warn";
                el.textContent = "⚠  not connected to " + (d.broker || "?") + "  —  check broker + credentials";
              }
            }).catch(function(){
              el.className = "ha-status-indicator ha-warn";
              el.textContent = "? status endpoint unreachable";
            });
          }
          refresh();
          window._haStatusTimer && clearInterval(window._haStatusTimer);
          window._haStatusTimer = setInterval(refresh, 5000);
        }, 0);
        break;

      case "ev":
        if (!currentConfig.ev_charger) currentConfig.ev_charger = {};
        // If ev_charger is empty but an easee driver exists with config,
        // populate the EV tab from the driver's config block so the UI
        // reflects what's actually running.
        if (!currentConfig.ev_charger.email && currentConfig.drivers) {
          for (var di = 0; di < currentConfig.drivers.length; di++) {
            var drv = currentConfig.drivers[di];
            if (drv.name === "easee" && drv.config) {
              currentConfig.ev_charger.provider = "easee";
              currentConfig.ev_charger.email = drv.config.email || "";
              currentConfig.ev_charger.password = drv.config.password || "";
              currentConfig.ev_charger.serial = drv.config.serial || "";
              break;
            }
          }
        }
        var evHasPassword = !!getByPath(currentConfig, "ev_charger.password", "");
        // Authoritative source for credentials-saved is /api/status; the
        // cfg may show the placeholder but not the actual state.db entry.
        // We fill this in once the indicator-refresh fetch completes.
        var credsBadge = evHasPassword
          ? '<span id="ev-creds-badge" class="creds-badge creds-saved">✓ Credentials saved</span>'
          : '<span id="ev-creds-badge" class="creds-badge creds-missing">⚠ No credentials saved</span>';
        html = '<div id="ev-status-indicator" class="ha-status-indicator">checking…</div>' +
          '<fieldset><legend>EV Charger</legend>' +
          selectField("Provider", "ev_charger.provider", ["easee"], "easee",
            "Cloud service provider for the EV charger. Currently only Easee is supported.") +
          field("Email", "ev_charger.email", "text", "",
            "Account email for the charger cloud service.") +
          '<label>Password ' + help("Account password for the charger cloud service.") + '</label>' +
          '<input type="password" data-path="ev_charger.password" value="" placeholder="' + (evHasPassword ? '••••••••' : '') + '">' +
          '<div style="margin-top:4px">' + credsBadge + '</div>' +
          field("Charger serial", "ev_charger.serial", "text", "",
            "Serial number of the charger. Leave empty to auto-detect the first charger on the account.") +
          '</fieldset>' +
          '<p style="color:var(--text-dim);font-size:0.8rem;margin-top:8px">' +
          'Credentials are used to authenticate with the Easee Cloud API. ' +
          'The charger serial is optional — if left empty the driver will use the first charger found on your account.' +
          '</p>';
        setTimeout(function () {
          var pwInput = bodyEl.querySelector('[data-path="ev_charger.password"]');
          if (pwInput) {
            pwInput.addEventListener("focus", function () {
              pwInput.placeholder = "";
            });
            pwInput.addEventListener("blur", function () {
              if (!pwInput.value && evHasPassword) {
                pwInput.placeholder = "••••••••";
              }
            });
          }

          var el = document.getElementById("ev-status-indicator");
          if (!el) return;
          function refresh() {
            fetch("/api/status").then(function(r){return r.json();}).then(function(d) {
              // Update the credentials-saved badge from the authoritative
              // server flag (based on state.db, not the masked cfg value).
              var badge = document.getElementById("ev-creds-badge");
              if (badge) {
                if (d.ev_credentials_saved) {
                  badge.textContent = "✓ Credentials saved";
                  badge.className = "creds-badge creds-saved";
                } else {
                  badge.textContent = "⚠ No credentials saved";
                  badge.className = "creds-badge creds-missing";
                }
              }
              // drivers may be an object keyed by name or an array — normalize.
              var rawDrivers = d.drivers || {};
              var drivers = [];
              if (Array.isArray(rawDrivers)) {
                drivers = rawDrivers;
              } else {
                Object.keys(rawDrivers).forEach(function(k) {
                  var entry = rawDrivers[k];
                  if (typeof entry === "object" && entry !== null) {
                    if (!entry.name) entry.name = k;
                    drivers.push(entry);
                  }
                });
              }
              var easee = null;
              for (var i = 0; i < drivers.length; i++) {
                if ((drivers[i].name || "").toLowerCase().indexOf("easee") >= 0) {
                  easee = drivers[i];
                  break;
                }
              }
              if (!easee) {
                el.className = "ha-status-indicator ha-off";
                el.textContent = "○  no Easee driver configured";
                return;
              }
              if (easee.status === "ok" || easee.status === "online") {
                el.className = "ha-status-indicator ha-ok";
                el.textContent = "● charger connected  ·  " + (easee.device_id || easee.name);
              } else {
                el.className = "ha-status-indicator ha-warn";
                el.textContent = "⚠  charger " + (easee.status || "unknown") + "  —  check credentials";
              }
            }).catch(function(){
              el.className = "ha-status-indicator ha-warn";
              el.textContent = "? status endpoint unreachable";
            });
          }
          refresh();
          window._evStatusTimer && clearInterval(window._evStatusTimer);
          window._evStatusTimer = setInterval(refresh, 5000);
        }, 0);
        break;
    }
    bodyEl.innerHTML = html;

    // Wire checkbox handlers (different from text/number which use blur/save)
    bodyEl.querySelectorAll("[data-checkbox-path]").forEach(function (cb) {
      cb.addEventListener("change", function () {
        setByPath(currentConfig, cb.dataset.checkboxPath, cb.checked);
      });
    });

    // Wire device add/remove buttons
    var addMqtt = document.getElementById("add-mqtt");
    var addModbus = document.getElementById("add-modbus");
    if (addMqtt) addMqtt.addEventListener("click", function () {
      captureCurrentTab();
      currentConfig.drivers.push({
        name: "new-device-" + (currentConfig.drivers.length + 1),
        lua: "drivers/new.lua",
        is_site_meter: false,
        battery_capacity_wh: 0,
        mqtt: { host: "", port: 1883, username: "", password: "" },
      });
      renderTab("devices");
    });
    if (addModbus) addModbus.addEventListener("click", function () {
      captureCurrentTab();
      currentConfig.drivers.push({
        name: "new-device-" + (currentConfig.drivers.length + 1),
        lua: "drivers/new.lua",
        is_site_meter: false,
        battery_capacity_wh: 0,
        modbus: { host: "", port: 502, unit_id: 1 },
      });
      renderTab("devices");
    });
    bodyEl.querySelectorAll("[data-remove-idx]").forEach(function (btn) {
      btn.addEventListener("click", function () {
        var idx = parseInt(btn.dataset.removeIdx);
        captureCurrentTab();
        currentConfig.drivers.splice(idx, 1);
        renderTab("devices");
      });
    });
  }

  function escHtml(s) {
    var div = document.createElement("div");
    div.textContent = s == null ? "" : String(s);
    return div.innerHTML;
  }

  // Render the PV-arrays list inside #pv-arrays-list. Each row is an
  // independent panel plane (tilt + azimuth + kWp). Entries write back
  // to currentConfig.weather.pv_arrays by index so the global save
  // handler picks them up without per-input data-path plumbing.
  function renderPVArrays() {
    var host = document.getElementById("pv-arrays-list");
    if (!host) return;
    var arrays = (currentConfig.weather && currentConfig.weather.pv_arrays) || [];
    if (arrays.length === 0) {
      host.innerHTML = '<p style="color:var(--text-dim);font-size:0.75rem;margin:4px 0 8px">No arrays defined — model will learn orientation from telemetry.</p>';
      return;
    }
    var rows = arrays.map(function (a, i) {
      return '<fieldset style="margin:6px 0;padding:8px 10px">' +
        '<div class="field-row" style="gap:8px;align-items:flex-end">' +
          '<div style="flex:1.4"><label>Name</label>' +
            '<input type="text" data-pv-arr="' + i + '" data-field="name" value="' + escHtml(a.name || "") + '" placeholder="e.g. south roof">' +
          '</div>' +
          '<div style="flex:1"><label>kWp</label>' +
            '<input type="number" step="0.1" data-pv-arr="' + i + '" data-field="kwp" value="' + (a.kwp || 0) + '">' +
          '</div>' +
          '<div style="flex:1"><label>Tilt °</label>' +
            '<input type="number" step="1" min="0" max="90" data-pv-arr="' + i + '" data-field="tilt_deg" value="' + (a.tilt_deg || 0) + '">' +
          '</div>' +
          '<div style="flex:1"><label>Azimuth °</label>' +
            '<input type="number" step="1" min="0" max="360" data-pv-arr="' + i + '" data-field="azimuth_deg" value="' + (a.azimuth_deg || 0) + '">' +
          '</div>' +
          '<button class="btn-remove" data-pv-arr-remove="' + i + '" type="button" title="Remove">✕</button>' +
        '</div></fieldset>';
    });
    host.innerHTML = rows.join("");
    // Wire handlers — delegation off host so re-renders don't leak listeners.
    host.onchange = function (e) {
      var idx = e.target && e.target.dataset && e.target.dataset.pvArr;
      if (idx == null || idx === "") return;
      var field = e.target.dataset.field;
      var arr = currentConfig.weather.pv_arrays;
      if (!arr[idx]) return;
      if (field === "name") {
        arr[idx][field] = e.target.value;
      } else {
        var v = parseFloat(e.target.value);
        if (!isNaN(v)) arr[idx][field] = v;
      }
    };
    host.onclick = function (e) {
      var idx = e.target && e.target.dataset && e.target.dataset.pvArrRemove;
      if (idx == null || idx === "") return;
      currentConfig.weather.pv_arrays.splice(parseInt(idx, 10), 1);
      renderPVArrays();
    };
  }

  function initWeatherMap() {
    var container = document.getElementById("weather-map");
    if (!container || !window.L) return;
    var latInput = bodyEl.querySelector('[data-path="weather.latitude"]');
    var lonInput = bodyEl.querySelector('[data-path="weather.longitude"]');
    if (!latInput || !lonInput) return;
    var lat = parseFloat(latInput.value);
    var lon = parseFloat(lonInput.value);
    if (isNaN(lat)) lat = 59.3293;
    if (isNaN(lon)) lon = 18.0686;
    // Tear down any previous instance (tab re-render)
    if (window._weatherMap) { try { window._weatherMap.remove(); } catch (e) {} window._weatherMap = null; }
    var map = L.map(container, { zoomControl: true }).setView([lat, lon], 11);
    window._weatherMap = map;
    L.tileLayer("https://tile.openstreetmap.org/{z}/{x}/{y}.png", {
      maxZoom: 18,
      attribution: "© OpenStreetMap"
    }).addTo(map);
    var marker = L.marker([lat, lon], { draggable: true }).addTo(map);
    function setCoord(la, lo) {
      latInput.value = la.toFixed(4);
      lonInput.value = lo.toFixed(4);
      setByPath(currentConfig, "weather.latitude", la);
      setByPath(currentConfig, "weather.longitude", lo);
    }
    marker.on("dragend", function () {
      var ll = marker.getLatLng();
      setCoord(ll.lat, ll.lng);
    });
    map.on("click", function (e) {
      marker.setLatLng(e.latlng);
      setCoord(e.latlng.lat, e.latlng.lng);
    });
    function syncFromInputs() {
      var la = parseFloat(latInput.value), lo = parseFloat(lonInput.value);
      if (!isNaN(la) && !isNaN(lo)) {
        marker.setLatLng([la, lo]);
        map.panTo([la, lo]);
      }
    }
    latInput.addEventListener("change", syncFromInputs);
    lonInput.addEventListener("change", syncFromInputs);
    // Leaflet sometimes mis-sizes when rendered in a hidden modal; retrigger
    setTimeout(function () { map.invalidateSize(); }, 150);
  }
})();
