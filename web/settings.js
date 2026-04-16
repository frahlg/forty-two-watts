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
      '<input type="' + type + '" data-path="' + path + '" value="' + (val == null ? "" : val) + '">';
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
            currentConfig.drivers.push(driver);
            renderTab("devices");
          });
        }, 0);
        html += '<div class="devices-list">';
        currentConfig.drivers.forEach(function (d, idx) {
          // Go-port config: d.wasm (or legacy d.lua), capabilities.mqtt/modbus
          var cap = d.capabilities || {};
          var mqtt = cap.mqtt || d.mqtt; // legacy fallback
          var modbus = cap.modbus || d.modbus;
          var protocol = mqtt ? "mqtt" : (modbus ? "modbus" : "?");
          var driverFile = d.wasm || d.lua || "(none)";
          var fmtKind = d.wasm ? "wasm" : (d.lua ? "lua" : "?");
          html += '<div class="device-item">' +
            '<div class="device-item-header">' +
            '<strong>' + escHtml(d.name) + '</strong>' +
            '<span style="color:var(--text-dim);font-size:0.75rem">' + fmtKind + ' · ' + protocol + ' · ' + escHtml(driverFile) + '</span>' +
            '<button class="btn-remove" data-remove-idx="' + idx + '">Remove</button>' +
            '</div>' +
            '<div class="field-row"><div>' +
            '<label>Driver file ' + help('Path to the .wasm (or legacy .lua) driver. Absolute or relative to the config file directory.') + '</label>' +
            '<input type="text" data-path="drivers.' + idx + '.' + fmtKind + '" value="' + escHtml(driverFile) + '">' +
            '</div><div>' +
            '<label>Battery capacity (Wh) ' + help('Nameplate storage capacity in watt-hours. Used for SoC scaling and MPC planning.') + '</label>' +
            '<input type="number" data-path="drivers.' + idx + '.battery_capacity_wh" value="' + (d.battery_capacity_wh || 0) + '">' +
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
          html += '</div>';
        });
        html += '</div>' +
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
          field("Grid tariff (öre/kWh)", "price.grid_tariff_ore_kwh", "number", 60) +
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
        html = '<fieldset><legend>Weather forecast &amp; PV</legend>' +
          selectField("Provider", "weather.provider", ["met_no", "openweather", "none"], "met_no") +
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
          '<p style="color:var(--text-dim);font-size:0.8rem;margin-top:8px">' +
          'met.no is free and requires no key. PV rated is your array nameplate (sum of all panels) — seeds the digital-twin prior so day-one PV forecasts are accurate. The twin refines from live telemetry automatically.' +
          '</p>';
        // Init Leaflet after innerHTML is set
        setTimeout(function() { initWeatherMap(); }, 0);
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
        var evHasPassword = !!getByPath(currentConfig, "ev_charger.password", "");
        html = '<div id="ev-status-indicator" class="ha-status-indicator">checking…</div>' +
          '<fieldset><legend>EV Charger</legend>' +
          selectField("Provider", "ev_charger.provider", ["easee"], "easee",
            "Cloud service provider for the EV charger. Currently only Easee is supported.") +
          field("Email", "ev_charger.email", "text", "",
            "Account email for the charger cloud service.") +
          '<label>Password ' + help("Account password for the charger cloud service.") + '</label>' +
          '<input type="password" data-path="ev_charger.password" value="" placeholder="' + (evHasPassword ? '••••••••' : '') + '">' +
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
              var drivers = d.drivers || [];
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
