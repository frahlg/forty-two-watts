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

  function field(label, path, type, dflt) {
    var val = getByPath(currentConfig, path, dflt);
    return '<label>' + label + '</label>' +
      '<input type="' + type + '" data-path="' + path + '" value="' + (val == null ? "" : val) + '">';
  }

  function selectField(label, path, options, dflt) {
    var val = getByPath(currentConfig, path, dflt);
    var opts = options.map(function (o) {
      return '<option value="' + o + '"' + (o === val ? ' selected' : '') + '>' + o + '</option>';
    }).join("");
    return '<label>' + label + '</label>' +
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
        html = '<div class="devices-list">';
        currentConfig.drivers.forEach(function (d, idx) {
          var protocol = d.mqtt ? "mqtt" : (d.modbus ? "modbus" : "?");
          html += '<div class="device-item">' +
            '<div class="device-item-header">' +
            '<strong>' + escHtml(d.name) + '</strong>' +
            '<span style="color:var(--text-dim);font-size:0.75rem">' + protocol + ' · ' + escHtml(d.lua) + '</span>' +
            '<button class="btn-remove" data-remove-idx="' + idx + '">Remove</button>' +
            '</div>' +
            '<div class="field-row"><div>' +
            '<label>Lua driver</label><input type="text" data-path="drivers.' + idx + '.lua" value="' + escHtml(d.lua) + '">' +
            '</div><div>' +
            '<label>Battery capacity (Wh)</label><input type="number" data-path="drivers.' + idx + '.battery_capacity_wh" value="' + (d.battery_capacity_wh || 0) + '">' +
            '</div></div>' +
            '<label><input type="checkbox" data-checkbox-path="drivers.' + idx + '.is_site_meter"' + (d.is_site_meter ? ' checked' : '') + '> Site meter (this drivers grid reading defines site grid)</label>';
          if (d.mqtt) {
            html += '<fieldset><legend>MQTT</legend>' +
              '<div class="field-row"><div>' +
              '<label>Host</label><input type="text" data-path="drivers.' + idx + '.mqtt.host" value="' + escHtml(d.mqtt.host) + '">' +
              '</div><div>' +
              '<label>Port</label><input type="number" data-path="drivers.' + idx + '.mqtt.port" value="' + (d.mqtt.port || 1883) + '">' +
              '</div></div>' +
              '<div class="field-row"><div>' +
              '<label>Username</label><input type="text" data-path="drivers.' + idx + '.mqtt.username" value="' + escHtml(d.mqtt.username || "") + '">' +
              '</div><div>' +
              '<label>Password</label><input type="password" data-path="drivers.' + idx + '.mqtt.password" value="' + escHtml(d.mqtt.password || "") + '">' +
              '</div></div></fieldset>';
          }
          if (d.modbus) {
            html += '<fieldset><legend>Modbus TCP</legend>' +
              '<div class="field-row"><div>' +
              '<label>Host</label><input type="text" data-path="drivers.' + idx + '.modbus.host" value="' + escHtml(d.modbus.host) + '">' +
              '</div><div>' +
              '<label>Port</label><input type="number" data-path="drivers.' + idx + '.modbus.port" value="' + (d.modbus.port || 502) + '">' +
              '</div></div>' +
              '<label>Unit ID</label><input type="number" data-path="drivers.' + idx + '.modbus.unit_id" value="' + (d.modbus.unit_id || 1) + '">' +
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
          field("PV rated (W)", "weather.pv_rated_w", "number", 10000) +
          field("API key (OpenWeather only)", "weather.api_key", "text", "") +
          '</fieldset>' +
          '<p style="color:var(--text-dim);font-size:0.8rem;margin-top:8px">' +
          'met.no is free and requires no key. PV rated is your array nameplate (sum of all panels) — seeds the digital-twin prior so day-one PV forecasts are accurate. The twin refines from live telemetry automatically.' +
          '</p>';
        break;

      case "batteries":
        if (!currentConfig.batteries) currentConfig.batteries = {};
        html = '<p style="color:var(--text-dim);font-size:0.8rem">Per-battery limits override the defaults. Leave blank to use BMS defaults.</p>';
        currentConfig.drivers.forEach(function (d) {
          if (d.battery_capacity_wh > 0) {
            if (!currentConfig.batteries[d.name]) currentConfig.batteries[d.name] = {};
            html += '<fieldset><legend>' + escHtml(d.name) + '</legend>' +
              '<div class="field-row"><div>' +
              field("Min SoC (0-1)", "batteries." + d.name + ".soc_min", "number", "") +
              '</div><div>' +
              field("Max SoC (0-1)", "batteries." + d.name + ".soc_max", "number", "") +
              '</div></div>' +
              '<div class="field-row"><div>' +
              field("Max charge (W)", "batteries." + d.name + ".max_charge_w", "number", "") +
              '</div><div>' +
              field("Max discharge (W)", "batteries." + d.name + ".max_discharge_w", "number", "") +
              '</div></div>' +
              field("Weight (for weighted mode)", "batteries." + d.name + ".weight", "number", 1) +
              '</fieldset>';
          }
        });
        break;

      case "ha":
        if (!currentConfig.homeassistant) currentConfig.homeassistant = {};
        html = '<fieldset><legend>Home Assistant MQTT</legend>' +
          '<label><input type="checkbox" data-checkbox-path="homeassistant.enabled"' + (currentConfig.homeassistant.enabled ? ' checked' : '') + '> Enabled</label>' +
          '<div class="field-row"><div>' +
          field("Broker host", "homeassistant.broker", "text", "192.168.1.1") +
          '</div><div>' +
          field("Port", "homeassistant.port", "number", 1883) +
          '</div></div>' +
          '<div class="field-row"><div>' +
          field("Username", "homeassistant.username", "text", "") +
          '</div><div>' +
          field("Password", "homeassistant.password", "password", "") +
          '</div></div>' +
          field("Publish interval (s)", "homeassistant.publish_interval_s", "number", 5) +
          '</fieldset>' +
          '<p style="color:var(--text-dim);font-size:0.8rem;margin-top:8px">Changes to HA config require a restart to take effect.</p>';
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
})();
