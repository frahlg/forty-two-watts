// Settings → EV tab: cloud EV charger credentials + charge plans (loadpoints) + live status.
(function () {
  var S = (window.FTWSettings = window.FTWSettings || { tabs: {} });
  S.tabs = S.tabs || {};

  function driverOptionsHTML(ctx, selected) {
    var drivers = (ctx.config.drivers || []).map(function (d) { return d.name; }).filter(Boolean);
    // Keep the current value even if it no longer matches a driver, so
    // stale config still round-trips cleanly.
    if (selected && drivers.indexOf(selected) === -1) drivers.unshift(selected);
    var opts = '<option value=""' + (selected ? '' : ' selected') + '>— select driver —</option>';
    drivers.forEach(function (n) {
      opts += '<option value="' + ctx.escHtml(n) + '"' + (n === selected ? ' selected' : '') + '>' + ctx.escHtml(n) + '</option>';
    });
    return opts;
  }

  function renderLoadpoints(ctx) {
    var config = ctx.config, field = ctx.field, escHtml = ctx.escHtml, help = ctx.help, getByPath = ctx.getByPath;
    if (!Array.isArray(config.loadpoints)) config.loadpoints = [];
    var html = '<fieldset><legend>Charge Plans</legend>' +
      '<div id="lp-live" class="lp-live" style="margin-bottom:12px">' +
        '<div class="lp-live-placeholder" style="color:var(--text-dim);font-size:0.85rem">Loading live state…</div>' +
      '</div>';
    if (config.loadpoints.length === 0) {
      html += '<p style="color:var(--text-dim);font-size:0.85rem;margin:0 0 8px 0">No charge plans configured. Click "Add charge plan" to create one.</p>';
    }
    config.loadpoints.forEach(function (lp, i) {
      var base = "loadpoints." + i + ".";
      var currentDriver = getByPath(config, base + "driver_name", "") || "";
      html +=
        '<div class="lp-entry" style="border:1px solid var(--border);padding:10px;margin-bottom:10px;border-radius:4px">' +
          '<div class="field-row"><div>' +
            field("ID", base + "id", "text", "",
              "Stable identifier for this charge plan (e.g. 'garage', 'street').") +
          '</div><div>' +
            '<label>Driver ' + help("Which configured driver controls the charger for this loadpoint.") + '</label>' +
            '<select data-path="' + base + 'driver_name">' + driverOptionsHTML(ctx, currentDriver) + '</select>' +
          '</div></div>' +
          '<div class="field-row"><div>' +
            field("Min charge (W)", base + "min_charge_w", "number", "",
              "Lower bound the planner is allowed to command. 0 = no minimum.") +
          '</div><div>' +
            field("Max charge (W)", base + "max_charge_w", "number", "",
              "Upper bound the planner is allowed to command.") +
          '</div></div>' +
          '<div class="field-row"><div>' +
            field("Vehicle capacity (Wh)", base + "vehicle_capacity_wh", "number", "",
              "Battery capacity in Wh. 0 defaults to 60 kWh.") +
          '</div><div>' +
            field("Plug-in SoC (%)", base + "plugin_soc_pct", "number", "",
              "Assumed vehicle SoC at plug-in. 0 defaults to 20%.") +
          '</div></div>' +
          '<label>Allowed steps (W, comma-separated) ' + help("Discrete power levels the charger supports. Leave blank for continuous. E.g. '0,1380,2760,4140'.") + '</label>' +
          '<input type="text" class="lp-steps-input" data-lp-index="' + i + '" value="' +
            escHtml((lp.allowed_steps_w || []).join(",")) + '">' +
          '<div style="text-align:right;margin-top:6px">' +
            '<button type="button" class="btn-remove-lp" data-lp-index="' + i + '" style="background:transparent;border:1px solid var(--border);color:#e57373;padding:4px 10px;border-radius:4px;cursor:pointer">Remove</button>' +
          '</div>' +
        '</div>';
    });
    html += '<button type="button" id="btn-add-lp" style="background:transparent;border:1px solid var(--border);color:var(--text);padding:6px 12px;border-radius:4px;cursor:pointer">+ Add charge plan</button>';
    html += '</fieldset>';
    return html;
  }

  function msToLocalInput(ms) {
    if (!ms || ms <= 0) return "";
    var d = new Date(ms);
    if (isNaN(d.getTime())) return "";
    var pad = function (n) { return n < 10 ? "0" + n : "" + n; };
    return d.getFullYear() + "-" + pad(d.getMonth() + 1) + "-" + pad(d.getDate()) +
      "T" + pad(d.getHours()) + ":" + pad(d.getMinutes());
  }

  function parseTargetTimeMs(state) {
    if (!state || !state.target_time) return 0;
    if (state.target_time.indexOf("0001-") === 0) return 0;
    var d = new Date(state.target_time);
    var ms = d.getTime();
    return (isNaN(ms) || ms <= 0) ? 0 : ms;
  }

  function buildLiveCard(ctx, id) {
    var card = document.createElement("div");
    card.className = "lp-live-card";
    card.dataset.lpStateId = id;
    card.style.cssText = "border:1px solid var(--border);padding:10px;margin-bottom:8px;border-radius:4px";
    card.innerHTML =
      '<div class="lp-live-summary" style="margin-bottom:4px"></div>' +
      '<div class="lp-live-target" style="color:var(--text-dim);font-size:0.85rem;margin-bottom:8px"></div>' +
      '<div class="field-row">' +
        '<div><label>Target SoC (%)</label><input type="number" class="lp-target-soc" min="0" max="100" step="1"></div>' +
        '<div><label>By (local time, optional)</label><input type="datetime-local" class="lp-target-time"></div>' +
      '</div>' +
      '<div style="text-align:right;margin-top:6px">' +
        '<button type="button" class="lp-target-apply" style="background:transparent;border:1px solid var(--border);color:var(--text);padding:4px 12px;border-radius:4px;cursor:pointer">Apply target</button>' +
        '<span class="lp-target-status" style="margin-left:8px;font-size:0.85rem"></span>' +
      '</div>';

    card.querySelector(".lp-target-apply").addEventListener("click", function () {
      var socInp = card.querySelector(".lp-target-soc");
      var timeInp = card.querySelector(".lp-target-time");
      var status = card.querySelector(".lp-target-status");
      var soc = parseFloat(socInp.value);
      if (isNaN(soc) || soc < 0 || soc > 100) {
        status.textContent = "SoC must be 0–100";
        status.style.color = "#e57373";
        return;
      }
      var tMs = 0;
      if (timeInp.value) {
        var d = new Date(timeInp.value);
        if (!isNaN(d.getTime())) tMs = d.getTime();
      }
      status.textContent = "Applying…";
      status.style.color = "";
      fetch("/api/loadpoints/" + encodeURIComponent(id) + "/target", {
        method: "POST",
        headers: { "Content-Type": "application/json" },
        body: JSON.stringify({ soc_pct: soc, target_time_ms: tMs }),
      }).then(function (r) {
        if (!r.ok) return r.json().then(function (j) { throw new Error(j.error || ("HTTP " + r.status)); });
        return r.json();
      }).then(function () {
        status.textContent = "✓ applied";
        status.style.color = "#6ec26e";
        setTimeout(function () { status.textContent = ""; }, 2000);
      }).catch(function (e) {
        status.textContent = "Failed: " + e.message;
        status.style.color = "#e57373";
      });
    });

    return card;
  }

  function updateLiveCard(ctx, card, s) {
    var summary = card.querySelector(".lp-live-summary");
    var target = card.querySelector(".lp-live-target");
    var socInp = card.querySelector(".lp-target-soc");
    var timeInp = card.querySelector(".lp-target-time");

    var soc = (s.current_soc_pct || 0).toFixed(0);
    var pw = (s.current_power_w || 0).toFixed(0);
    var tag = s.plugged_in ? "● plugged in" : "○ unplugged";
    summary.innerHTML = '<strong>' + ctx.escHtml(s.id || "(no id)") + '</strong> &mdash; ' +
      tag + ' · SoC ' + soc + '% · ' + pw + ' W';

    var tMs = parseTargetTimeMs(s);
    if (s.target_soc_pct && s.target_soc_pct > 0) {
      var when = tMs > 0 ? " by " + new Date(tMs).toLocaleString() : " (no deadline)";
      target.textContent = "Current target: " + s.target_soc_pct.toFixed(0) + "%" + when;
    } else {
      target.textContent = "No target set.";
    }

    // Seed the inputs once so user sees the current target and can tweak.
    // Don't overwrite once the user has typed something (focus-tracked).
    if (!card.dataset.lpSeeded) {
      socInp.value = (s.target_soc_pct && s.target_soc_pct > 0) ? s.target_soc_pct.toFixed(0) : "";
      timeInp.value = msToLocalInput(tMs);
      card.dataset.lpSeeded = "1";
    }
  }

  function wireLoadpoints(ctx) {
    var bodyEl = ctx.bodyEl, config = ctx.config;

    // Comma-separated allowed_steps_w → float[] on input.
    bodyEl.querySelectorAll(".lp-steps-input").forEach(function (inp) {
      inp.addEventListener("input", function () {
        var idx = parseInt(inp.dataset.lpIndex, 10);
        if (!config.loadpoints || !config.loadpoints[idx]) return;
        var parts = inp.value.split(",").map(function (s) { return parseFloat(s.trim()); }).filter(function (n) { return !isNaN(n); });
        config.loadpoints[idx].allowed_steps_w = parts.length ? parts : undefined;
      });
    });

    bodyEl.querySelectorAll(".btn-remove-lp").forEach(function (btn) {
      btn.addEventListener("click", function () {
        var idx = parseInt(btn.dataset.lpIndex, 10);
        ctx.captureCurrentTab();
        config.loadpoints.splice(idx, 1);
        ctx.renderTab("ev");
      });
    });

    var addBtn = bodyEl.querySelector("#btn-add-lp");
    if (addBtn) {
      addBtn.addEventListener("click", function () {
        ctx.captureCurrentTab();
        if (!Array.isArray(config.loadpoints)) config.loadpoints = [];
        config.loadpoints.push({ id: "", driver_name: "" });
        ctx.renderTab("ev");
      });
    }

    var liveEl = bodyEl.querySelector("#lp-live");
    if (!liveEl) return;
    function refreshLive() {
      fetch("/api/loadpoints").then(function (r) { return r.json(); }).then(function (d) {
        var placeholder = liveEl.querySelector(".lp-live-placeholder");
        if (placeholder) placeholder.remove();
        if (!d.enabled) {
          liveEl.innerHTML = '<div style="color:var(--text-dim);font-size:0.85rem">Loadpoints not active (none configured).</div>';
          return;
        }
        var states = d.loadpoints || [];
        if (states.length === 0) {
          liveEl.innerHTML = '<div style="color:var(--text-dim);font-size:0.85rem">No active loadpoints.</div>';
          return;
        }
        // Strip any stale non-card message.
        Array.from(liveEl.children).forEach(function (child) {
          if (!child.dataset.lpStateId) child.remove();
        });
        var seen = {};
        states.forEach(function (s) {
          if (!s.id) return;
          seen[s.id] = true;
          var sel = '[data-lp-state-id="' + s.id.replace(/"/g, '\\"') + '"]';
          var card = liveEl.querySelector(sel);
          if (!card) {
            card = buildLiveCard(ctx, s.id);
            liveEl.appendChild(card);
          }
          updateLiveCard(ctx, card, s);
        });
        Array.from(liveEl.querySelectorAll("[data-lp-state-id]")).forEach(function (card) {
          if (!seen[card.dataset.lpStateId]) card.remove();
        });
      }).catch(function () {
        liveEl.innerHTML = '<div style="color:var(--text-dim);font-size:0.85rem">Loadpoint status endpoint unreachable.</div>';
      });
    }
    refreshLive();
    if (window._lpStatusTimer) clearInterval(window._lpStatusTimer);
    window._lpStatusTimer = setInterval(refreshLive, 5000);
  }

  S.tabs.ev = {
    render: function (ctx) {
      var field = ctx.field, selectField = ctx.selectField, help = ctx.help;
      var getByPath = ctx.getByPath, config = ctx.config;
      if (!config.ev_charger) config.ev_charger = {};
      // If ev_charger is empty but an easee driver exists with config,
      // populate the EV tab from the driver's config block so the UI
      // reflects what's actually running.
      if (!config.ev_charger.email && config.drivers) {
        for (var di = 0; di < config.drivers.length; di++) {
          var drv = config.drivers[di];
          if (drv.name === "easee" && drv.config) {
            config.ev_charger.provider = "easee";
            config.ev_charger.email = drv.config.email || "";
            config.ev_charger.password = drv.config.password || "";
            config.ev_charger.serial = drv.config.serial || "";
            break;
          }
        }
      }
      var evHasPassword = !!getByPath(config, "ev_charger.password", "");
      var credsBadge = evHasPassword
        ? '<span id="ev-creds-badge" class="creds-badge creds-saved">✓ Credentials saved</span>'
        : '<span id="ev-creds-badge" class="creds-badge creds-missing">⚠ No credentials saved</span>';
      return '<div id="ev-status-indicator" class="ha-status-indicator">checking…</div>' +
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
        '</p>' +
        renderLoadpoints(ctx);
    },
    after: function (ctx) {
      var bodyEl = ctx.bodyEl;
      var evHasPassword = !!ctx.getByPath(ctx.config, "ev_charger.password", "");
      var pwInput = bodyEl.querySelector('[data-path="ev_charger.password"]');
      if (pwInput) {
        pwInput.addEventListener("focus", function () { pwInput.placeholder = ""; });
        pwInput.addEventListener("blur", function () {
          if (!pwInput.value && evHasPassword) pwInput.placeholder = "••••••••";
        });
      }
      wireLoadpoints(ctx);
      var el = document.getElementById("ev-status-indicator");
      if (!el) return;
      function refresh() {
        fetch("/api/status").then(function (r) { return r.json(); }).then(function (d) {
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
          var rawDrivers = d.drivers || {};
          var drivers = [];
          if (Array.isArray(rawDrivers)) {
            drivers = rawDrivers;
          } else {
            Object.keys(rawDrivers).forEach(function (k) {
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
        }).catch(function () {
          el.className = "ha-status-indicator ha-warn";
          el.textContent = "? status endpoint unreachable";
        });
      }
      refresh();
      if (window._evStatusTimer) clearInterval(window._evStatusTimer);
      window._evStatusTimer = setInterval(refresh, 5000);
    },
  };
})();
