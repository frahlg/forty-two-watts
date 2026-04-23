// Settings → Devices tab: driver catalog picker + per-driver editor.
// Owns its own add/remove/connect button wiring; the Settings shell
// stays driver-agnostic.
(function () {
  var S = (window.FTWSettings = window.FTWSettings || { tabs: {} });
  S.tabs = S.tabs || {};

  S.tabs.devices = {
    render: function (ctx) {
      var help = ctx.help, escHtml = ctx.escHtml, config = ctx.config;
      if (!config.drivers) config.drivers = [];
      var html = '<fieldset><legend>Add from catalog</legend>' +
        '<div class="field-row"><div>' +
        '<label>Driver <span class="help" data-help="Pick a Lua driver from the drivers/ directory. Each driver declares its capabilities (MQTT/Modbus) + which manufacturer/model it supports.">?</span></label>' +
        '<select id="driver-catalog-picker"><option value="">Loading catalog…</option></select>' +
        '</div><div>' +
        '<label>Friendly name</label><input type="text" id="driver-catalog-name" placeholder="e.g. ferroamp-house">' +
        '</div></div>' +
        '<button class="btn-add" id="driver-catalog-add">+ Add selected</button>' +
        '<p style="color:var(--text-dim);font-size:0.75rem;margin:8px 0 0">' +
        '🟢 production — verified on real hardware at ≥1 site · ' +
        '🟡 beta — working on a single site, awaiting a second · ' +
        '🔴 experimental — ported from reference, not yet proven against live hardware. ' +
        'Hover a driver for site + date notes.' +
        '</p>' +
        '</fieldset>';
      html += '<div class="devices-list">';
      config.drivers.forEach(function (d, idx) {
        var cap = d.capabilities || {};
        var mqtt = cap.mqtt || d.mqtt;
        var modbus = cap.modbus || d.modbus;
        var protocol = mqtt ? "mqtt" : (modbus ? "modbus" : (cap.http ? "http" : "?"));
        var driverFile = d.lua || "(none)";
        var headerLabel = d.alias ? (escHtml(d.alias) + ' <span style="color:var(--text-dim);font-weight:400">(' + escHtml(d.name) + ')</span>') : escHtml(d.name);
        html += '<div class="device-item">' +
          '<div class="device-item-header">' +
          '<strong>' + headerLabel + '</strong>' +
          '<span style="color:var(--text-dim);font-size:0.75rem">lua · ' + protocol + ' · ' + escHtml(driverFile) + '</span>' +
          '<button class="btn-remove" data-remove-idx="' + idx + '">Remove</button>' +
          '</div>' +
          '<div class="field-row"><div>' +
          '<label>Name (stable ID) ' + help('Internal identifier — used by telemetry, schedule references (loadpoint.vehicle_driver etc.), and device registry. Must be unique across drivers. Change with care: renaming orphans any persisted state keyed on this name.') + '</label>' +
          '<input type="text" data-path="drivers.' + idx + '.name" value="' + escHtml(d.name || "") + '">' +
          '</div><div>' +
          '<label>Alias ' + help('Optional friendly label for the UI (e.g. "My Model Y"). When empty, UIs fall back to the stable name.') + '</label>' +
          '<input type="text" data-path="drivers.' + idx + '.alias" value="' + escHtml(d.alias || "") + '" placeholder="e.g. Garage Tesla">' +
          '</div></div>' +
          '<div class="field-row"><div>' +
          '<label>Driver file ' + help('Path to the .lua driver. Absolute or relative to the config file directory.') + '</label>' +
          '<input type="text" data-path="drivers.' + idx + '.lua" value="' + escHtml(driverFile) + '">' +
          '</div><div>' +
          '<label>Battery capacity (kWh) ' + help('Nameplate storage capacity in kilowatt-hours. Stored internally as Wh.') + '</label>' +
          '<input type="number" step="0.1" data-path="drivers.' + idx + '.battery_capacity_wh" data-unit-scale="1000" value="' + ((d.battery_capacity_wh || 0) / 1000) + '">' +
          '</div></div>';
        // Site-meter checkbox. Always rendered here so the DOM is
        // stable regardless of when the catalog arrives — the
        // post-fetch pass in `after` visits each `.drv-site-meter`
        // and hides the ones whose driver only has a "vehicle"
        // capability (no grid reading to offer). Hiding via a
        // post-render DOM edit instead of a re-render avoids the
        // select-wiping bug where a user clicking the catalog
        // dropdown had it disappear under them every time the
        // fetch resolved.
        html += '<label class="drv-site-meter" data-drv-lua="' + escHtml(d.lua || '') + '"><input type="checkbox" data-checkbox-path="drivers.' + idx + '.is_site_meter"' + (d.is_site_meter ? ' checked' : '') + '> Site meter ' + help('Exactly one driver should be the site meter — its grid reading defines the point-of-measurement the PI loop balances.') + '</label>';
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
        // Local-HTTP vs cloud-HTTP vs vehicle-over-proxy detection
        // by declared config shape + catalog capabilities.
        var dcfg = d.config || {};
        var hasHostField = Object.prototype.hasOwnProperty.call(dcfg, 'host');
        var hasAuthField = Object.prototype.hasOwnProperty.call(dcfg, 'email') ||
                           Object.prototype.hasOwnProperty.call(dcfg, 'password');
        var catalogEntry = (S.catalogByLua || {})[d.lua];
        var caps = (catalogEntry && catalogEntry.capabilities) || [];
        var isVehicleDriver = cap.http != null &&
          (caps.indexOf("vehicle") >= 0 ||
           Object.prototype.hasOwnProperty.call(dcfg, 'vin') ||
           Object.prototype.hasOwnProperty.call(dcfg, 'ip'));
        var isLocalHTTP = !isVehicleDriver && cap.http != null && hasHostField;
        var isCloudDriver = !isVehicleDriver && cap.http != null && !hasHostField &&
          (hasAuthField || Object.keys(dcfg).length === 0);
        if (isVehicleDriver) {
          // TeslaBLEProxy-style drivers only need the LAN IP of the
          // proxy and the VIN the proxy is paired to — no cloud
          // credentials, no charger serial. "Verify connection"
          // makes the backend issue a one-shot vehicle_data poll so
          // the operator can confirm pairing before saving.
          var vcfg = d.config || {};
          html += '<fieldset><legend>Vehicle</legend>' +
            '<div class="field-row"><div>' +
            '<label>Proxy IP ' + help('LAN address of the TeslaBLEProxy. Bare IP uses port 8080; append ":port" to override (e.g. 192.168.1.50:1234).') + '</label>' +
            '<input type="text" data-path="drivers.' + idx + '.config.ip" value="' + escHtml(vcfg.ip || '') + '" placeholder="192.168.1.50 (or 192.168.1.50:1234)">' +
            '</div><div>' +
            '<label>VIN ' + help('Vehicle Identification Number the proxy is paired to.') + '</label>' +
            '<input type="text" data-path="drivers.' + idx + '.config.vin" value="' + escHtml(vcfg.vin || '') + '" placeholder="5YJ3E1EA1KF000000">' +
            '</div></div>' +
            '<div style="margin-top:8px;display:flex;gap:10px;align-items:center">' +
            '<button class="btn-add tesla-verify-btn" type="button" data-driver-idx="' + idx + '">Verify connection</button>' +
            '<span class="tesla-verify-status" data-driver-idx="' + idx + '" style="font-size:0.82rem;color:var(--text-dim)"></span>' +
            '</div>' +
            '</fieldset>';
        }
        if (isLocalHTTP) {
          var lcfg = d.config || {};
          html += '<fieldset><legend>HTTP</legend>' +
            '<label>Host / IP ' + help('Hostname (e.g. zap.local) or IP address of the device. mDNS names work when your OS resolver supports them; otherwise use the LAN IP.') + '</label>' +
            '<input type="text" data-path="drivers.' + idx + '.config.host" value="' + escHtml(lcfg.host || '') + '" placeholder="zap.local">' +
            '</fieldset>';
        }
        if (isCloudDriver) {
          var cfg = d.config || {};
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
      return html;
    },
    after: function (ctx) {
      var config = ctx.config;
      var bodyEl = ctx.bodyEl;

      // Driver catalog picker — fetch async, populate the select +
      // post-process the per-driver Site-meter labels. Fetch is
      // module-cached on window.FTWSettings so opening the tab
      // again doesn't re-hit the endpoint; the in-DOM <select>
      // still gets repopulated on every open.
      var catalogPromise = S._catalogPromise;
      if (!catalogPromise) {
        catalogPromise = fetch("/api/drivers/catalog")
          .then(function (r) { return r.json(); })
          .catch(function () { return { entries: [] }; });
        S._catalogPromise = catalogPromise;
      }
      catalogPromise.then(function (data) {
        var byLua = {};
        (data && data.entries || []).forEach(function (e) {
          if (e && e.path) byLua[e.path] = e;
        });
        S.catalogByLua = byLua;
        // Hide Site-meter on drivers whose catalog advertises only
        // "vehicle" (or similar non-meter-capable capability sets).
        // Legacy drivers without a catalog entry keep the checkbox.
        bodyEl.querySelectorAll(".drv-site-meter").forEach(function (lbl) {
          var lua = lbl.getAttribute("data-drv-lua");
          var entry = lua && byLua[lua];
          if (!entry) return;
          var caps = entry.capabilities || [];
          if (caps.length === 0) return;
          var canBeSiteMeter =
            caps.indexOf("meter") >= 0 ||
            caps.indexOf("pv") >= 0 ||
            caps.indexOf("battery") >= 0;
          if (!canBeSiteMeter) lbl.style.display = "none";
        });
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
          var badge =
            e.verification_status === "production" ? "🟢 " :
            e.verification_status === "beta" ? "🟡 " : "🔴 ";
          opt.textContent = badge + (e.name || e.filename) + "  —  " + (e.manufacturer || "?") + "  [" + protoLabel + "]" + (e.version ? "  v" + e.version : "");
          opt.dataset.protocols = protoLabel;
          opt.dataset.id = e.id || "";
          opt.dataset.httpHosts = (e.http_hosts || []).join(",");
          opt.dataset.capabilities = (e.capabilities || []).join(",");
          opt.dataset.connectionHost = (e.connection_defaults && e.connection_defaults.host) || "";
          opt.dataset.verificationStatus = e.verification_status || "experimental";
          if (e.verification_notes) opt.title = e.verification_notes;
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
        var baseName = (nameEl.value || "").trim() || chosen.dataset.id || ("driver-" + config.drivers.length);
        // Auto-dedupe against existing driver names so adding a second
        // tesla_vehicle for a two-car household doesn't collide with
        // the first. "garage-tesla" → "garage-tesla-2" → "garage-tesla-3"…
        // The operator can still edit to whatever they want — this
        // only protects against the default picker producing the same
        // string twice in a row.
        var takenNames = {};
        (config.drivers || []).forEach(function (d) { if (d.name) takenNames[d.name] = true; });
        var name = baseName;
        if (takenNames[name]) {
          var i = 2;
          while (takenNames[baseName + "-" + i]) i++;
          name = baseName + "-" + i;
        }
        var driver = { name: name, lua: sel.value };
        driver.capabilities = {};
        if (protocols.indexOf("mqtt") >= 0) driver.capabilities.mqtt = { host: "", port: 1883 };
        if (protocols.indexOf("modbus") >= 0) driver.capabilities.modbus = { host: "", port: 502, unit_id: 1 };
        if (protocols.indexOf("http") >= 0) {
          var hosts = (chosen.dataset.httpHosts || "").split(",").filter(Boolean);
          driver.capabilities.http = { allowed_hosts: hosts };
          var connHost = chosen.dataset.connectionHost || "";
          var chosenCaps = (chosen.dataset.capabilities || "").split(",").filter(Boolean);
          if (chosenCaps.indexOf("vehicle") >= 0) {
            // TeslaBLEProxy-style: ip + vin only, no cloud creds.
            driver.config = { ip: "", vin: "" };
          } else if (connHost) {
            driver.config = { host: connHost };
          } else {
            driver.config = { email: "", password: "", serial: "" };
          }
        }
        config.drivers.push(driver);
        ctx.renderTab("devices");
      });

      // For vehicle drivers: keep capabilities.http.allowed_hosts
      // in sync with the configured IP. The capability layer gates
      // every outbound HTTP on this list — an empty list means the
      // driver can't talk to anything and gets stuck in watchdog
      // stale state (exactly what a freshly-added Tesla driver with
      // an empty list hit). We rewrite the list to [host] (port
      // stripped) whenever the operator edits the IP.
      bodyEl.querySelectorAll('[data-path$=".config.ip"]').forEach(function (ipInput) {
        var m = /^drivers\.(\d+)\.config\.ip$/.exec(ipInput.dataset.path);
        if (!m) return;
        var dIdx = parseInt(m[1], 10);
        function syncHost() {
          var raw = (ipInput.value || "").trim();
          if (!raw) return;
          var host = raw.split(":")[0];
          var drv = config.drivers[dIdx];
          if (!drv) return;
          drv.capabilities = drv.capabilities || {};
          drv.capabilities.http = drv.capabilities.http || {};
          drv.capabilities.http.allowed_hosts = [host];
        }
        ipInput.addEventListener("input", syncHost);
        ipInput.addEventListener("blur", syncHost);
        syncHost(); // catch pre-existing configs that arrived with an empty list
      });

      // Tesla Verify-connection buttons.
      bodyEl.querySelectorAll(".tesla-verify-btn").forEach(function (vbtn) {
        vbtn.addEventListener("click", function () {
          var dIdx = vbtn.dataset.driverIdx;
          var statusEl = bodyEl.querySelector('.tesla-verify-status[data-driver-idx="' + dIdx + '"]');
          var ipInput = bodyEl.querySelector('[data-path="drivers.' + dIdx + '.config.ip"]');
          var vinInput = bodyEl.querySelector('[data-path="drivers.' + dIdx + '.config.vin"]');
          var ip = ipInput ? ipInput.value.trim() : "";
          var vin = vinInput ? vinInput.value.trim() : "";
          if (!ip || !vin) {
            if (statusEl) { statusEl.style.color = "#e57373"; statusEl.textContent = "Fill in both IP and VIN first"; }
            return;
          }
          if (statusEl) { statusEl.style.color = "var(--text-dim)"; statusEl.textContent = "Verifying…"; }
          vbtn.disabled = true;
          fetch("/api/drivers/verify_tesla", {
            method: "POST",
            headers: { "Content-Type": "application/json" },
            body: JSON.stringify({ ip: ip, vin: vin }),
          })
            .then(function (r) { return r.json(); })
            .then(function (j) {
              if (j && j.ok) {
                if (statusEl) {
                  statusEl.style.color = "#81c995";
                  statusEl.textContent = "✓ SoC " + Math.round(j.soc_pct) +
                    " / " + Math.round(j.charge_limit_pct) + "% · " +
                    (j.charging_state || "idle");
                }
              } else {
                if (statusEl) {
                  statusEl.style.color = "#e57373";
                  statusEl.textContent = "✗ " + (j && j.error ? j.error : "unknown error");
                }
              }
            })
            .catch(function (e) {
              if (statusEl) { statusEl.style.color = "#e57373"; statusEl.textContent = "✗ " + e.message; }
            })
            .finally(function () { vbtn.disabled = false; });
        });
      });

      // Cloud-driver Connect buttons.
      bodyEl.querySelectorAll(".ev-connect-btn").forEach(function (connectBtn) {
        connectBtn.addEventListener("click", function () {
          var dIdx = connectBtn.dataset.driverIdx;
          var statusEl = document.getElementById("ev-connect-status-" + dIdx);
          var sel = document.getElementById("ev-charger-select-" + dIdx);
          var emailInput = bodyEl.querySelector('[data-path="drivers.' + dIdx + '.config.email"]');
          var pwInput = bodyEl.querySelector('[data-path="drivers.' + dIdx + '.config.password"]');
          var email = emailInput ? emailInput.value : "";
          var pw = pwInput ? pwInput.value : "";
          if (!email) { if (statusEl) statusEl.textContent = "Enter email first"; return; }
          if (statusEl) statusEl.textContent = "Connecting...";
          connectBtn.disabled = true;
          var dCfg = config && config.drivers ? config.drivers[dIdx] : null;
          var provider = "easee";
          if (dCfg && typeof dCfg.lua === "string" && dCfg.lua !== "") {
            provider = dCfg.lua
              .replace(/^.*[\\/]/, "")
              .replace(/\.lua$/i, "")
              .replace(/_cloud$/i, "");
            if (!provider) provider = "easee";
          }
          fetch("/api/ev/chargers", {
            method: "POST",
            headers: { "Content-Type": "application/json" },
            body: JSON.stringify({ provider: provider, email: email, password: pw }),
          }).then(function (r) {
            if (!r.ok) return r.json().then(function (j) { throw new Error(j.error || "HTTP " + r.status); });
            return r.json();
          }).then(function (chargers) {
            if (!sel || !Array.isArray(chargers) || chargers.length === 0) {
              if (statusEl) statusEl.textContent = "No chargers found";
              return;
            }
            var d = config.drivers[dIdx];
            var current = (d && d.config && d.config.serial) || "";
            sel.innerHTML = "";
            chargers.forEach(function (ch) {
              var opt = document.createElement("option");
              opt.value = ch.id;
              opt.textContent = ch.id + (ch.name ? "  —  " + ch.name : "");
              if (ch.id === current) opt.selected = true;
              sel.appendChild(opt);
            });
            var selected = sel.value;
            if (d && d.config) d.config.serial = selected;
            if (config.ev_charger) config.ev_charger.serial = selected;
            sel.onchange = function () {
              if (d && d.config) d.config.serial = sel.value;
              if (config.ev_charger) config.ev_charger.serial = sel.value;
            };
            if (statusEl) statusEl.textContent = chargers.length + " charger(s) found";
          }).catch(function (e) {
            if (statusEl) statusEl.textContent = "Error: " + e.message;
          }).finally(function () {
            connectBtn.disabled = false;
          });
        });
      });

      // Add/remove-device buttons.
      var addMqtt = document.getElementById("add-mqtt");
      var addModbus = document.getElementById("add-modbus");
      if (addMqtt) addMqtt.addEventListener("click", function () {
        ctx.captureCurrentTab();
        config.drivers.push({
          name: "new-device-" + (config.drivers.length + 1),
          lua: "drivers/new.lua",
          is_site_meter: false,
          battery_capacity_wh: 0,
          mqtt: { host: "", port: 1883, username: "", password: "" },
        });
        ctx.renderTab("devices");
      });
      if (addModbus) addModbus.addEventListener("click", function () {
        ctx.captureCurrentTab();
        config.drivers.push({
          name: "new-device-" + (config.drivers.length + 1),
          lua: "drivers/new.lua",
          is_site_meter: false,
          battery_capacity_wh: 0,
          modbus: { host: "", port: 502, unit_id: 1 },
        });
        ctx.renderTab("devices");
      });
      bodyEl.querySelectorAll("[data-remove-idx]").forEach(function (rmBtn) {
        rmBtn.addEventListener("click", function () {
          var idx = parseInt(rmBtn.dataset.removeIdx);
          ctx.captureCurrentTab();
          config.drivers.splice(idx, 1);
          ctx.renderTab("devices");
        });
      });
    },
  };
})();
