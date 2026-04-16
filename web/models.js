// Battery Models panel + self-tune modal — fetches /api/battery_models, renders
// per-battery cards with τ, gain, deadband, confidence, health bar. Self-tune
// modal manages the calibration flow.

(function () {
  "use strict";

  const POLL_INTERVAL = 3000;
  const TUNE_POLL = 1000;

  const grid = document.getElementById("models-grid");
  const openBtn = document.getElementById("self-tune-btn");
  const modal = document.getElementById("self-tune-modal");
  const closeBtn = document.getElementById("self-tune-close");
  const startBtn = document.getElementById("self-tune-start");
  const cancelBtn = document.getElementById("self-tune-cancel");
  const body = document.getElementById("self-tune-body");
  const statusEl = document.getElementById("self-tune-status");

  if (!grid) return;

  let lastModels = {};
  let tunePollHandle = null;

  // Expose the latest models payload globally so app.js renderDrivers can
  // inline the model panel in the same pass it renders the driver card.
  // This prevents the model from "blinking" between the two polls (which
  // each owned a different DOM element on different intervals).
  window._lastBatteryModels = lastModels;
  window.renderInlineBatteryModel = function () { return ""; };

  // ---- Models grid: rendered once per /api/battery_models poll ----

  function fetchModels() {
    fetch("/api/battery_models")
      .then(function (r) { return r.ok ? r.json() : null; })
      .then(function (data) {
        if (!data) return;
        lastModels = data;
        window._lastBatteryModels = data;
        renderModels(data);
      })
      .catch(function () { /* silent */ });
  }

  function renderModels(models) {
    // Models are now drawn by app.js inside each driver card (via
    // renderInlineBatteryModel exposed below). We just keep #models-grid
    // empty — it stays in the DOM only as the home for the Self-tune
    // button + as a fallback for orphan models without a matching driver.
    if (grid) grid.innerHTML = '';
  }

  // Exposed to app.js: returns the model HTML for a driver name (or "" if
  // we don't have a model for it yet). Called inside renderDrivers so the
  // driver card and model are always rendered together — no race.
  window.renderInlineBatteryModel = function (name) {
    var m = lastModels[name];
    if (!m) return "";
    return renderInlineModel(name, m);
  };

  // Minimal CSS.escape polyfill — only need it to escape driver names that
  // happen to contain characters CSS doesn't allow in attribute selectors.
  function cssEscape(s) {
    if (window.CSS && window.CSS.escape) return window.CSS.escape(s);
    return s.replace(/[^a-zA-Z0-9_-]/g, function (c) { return "\\" + c; });
  }

  // Inline model panel rendered inside a driver-card. Compact summary
  // always shown; details unfold on click.
  function renderInlineModel(name, m) {
    var conf = (m.confidence * 100).toFixed(0) + "%";
    var cascadeBadge = m.confidence >= 0.5
      ? '<span style="color:#22c55e;font-size:0.7rem">● cascade</span>'
      : '<span style="color:#f59e0b;font-size:0.7rem">○ direct</span>';
    var health = m.health_score;
    var healthPct = (health * 100).toFixed(0);
    var healthClass = health >= 0.8 ? "health-good" : health >= 0.5 ? "health-warn" : "health-bad";
    var driftPerDay = m.health_drift_per_day || 0;
    var driftStr = driftPerDay === 0
      ? "stable"
      : (driftPerDay > 0 ? "+" : "") + (driftPerDay * 100).toFixed(2) + "%/day";
    var driftClass = Math.abs(driftPerDay) < 0.001 ? "tune-delta-neutral"
      : driftPerDay < 0 ? "tune-delta-negative" : "tune-delta-positive";

    var expandKey = "model-expanded:" + name;
    var expanded = localStorage.getItem(expandKey) === "1";
    var caret = expanded ? "▾" : "▸";

    var details = !expanded ? "" :
      '<div class="model-stats" style="margin-top:6px">' +
      '<span class="model-stat-label">τ (response)</span>' +
      '<span class="model-stat-value">' + m.tau_s.toFixed(2) + ' s</span>' +
      '<span class="model-stat-label">gain</span>' +
      '<span class="model-stat-value">' + m.gain.toFixed(3) + '</span>' +
      '<span class="model-stat-label">deadband</span>' +
      '<span class="model-stat-value">' + (m.deadband_w || 0).toFixed(0) + ' W</span>' +
      '<span class="model-stat-label">calibrated</span>' +
      '<span class="model-stat-value">' + (m.last_calibrated_ts_ms ? humanAge((Date.now() - m.last_calibrated_ts_ms) / 1000) : "never") + '</span>' +
      '</div>' +
      '<button class="btn-reset-model" data-reset-battery="' + esc(name) + '" ' +
      'style="margin-top:6px;padding:4px 10px;font-size:0.7rem;background:var(--surface2);' +
      'border:1px solid var(--border);color:var(--text-dim);border-radius:3px;cursor:pointer;width:100%">' +
      '↻ Reset model' +
      '</button>';

    return '<div class="driver-model-panel">' +
      '<div class="driver-model-header" data-expand="' + esc(name) + '" style="cursor:pointer">' +
      '<span class="driver-model-title">' + caret + ' Model · ' + cascadeBadge + '</span>' +
      '<span class="driver-model-meta">' + m.n_samples + ' samples · ' + conf + '</span>' +
      '</div>' +
      '<div class="model-bar" style="margin-top:4px"><div class="model-bar-fill" style="width:' + healthPct + '%"></div></div>' +
      '<div class="model-health-text">' +
      '<span class="' + healthClass + '">health ' + healthPct + '%</span>' +
      '<span class="' + driftClass + '">' + driftStr + '</span>' +
      '</div>' +
      details +
      '</div>';
  }

  function renderCard(name, m) {
    var conf = (m.confidence * 100).toFixed(0) + "%";
    var cascadeActive = m.confidence >= 0.5;
    var cascadeBadge = cascadeActive
      ? '<span style="color:#22c55e;font-size:0.7rem">● cascade</span>'
      : '<span style="color:#f59e0b;font-size:0.7rem">○ direct</span>';
    var health = m.health_score;
    var healthPct = (health * 100).toFixed(0);
    var healthClass = health >= 0.8 ? "health-good" : health >= 0.5 ? "health-warn" : "health-bad";
    var tau = m.tau_s.toFixed(2);
    var gain = m.gain.toFixed(3);
    var calibratedAgo = m.last_calibrated_ts_ms
      ? humanAge((Date.now() - m.last_calibrated_ts_ms) / 1000)
      : "never";
    var driftPerDay = m.health_drift_per_day || 0;
    var driftStr = driftPerDay === 0
      ? "stable"
      : (driftPerDay > 0 ? "+" : "") + (driftPerDay * 100).toFixed(2) + "%/day";
    var driftClass = Math.abs(driftPerDay) < 0.001 ? "tune-delta-neutral"
      : driftPerDay < 0 ? "tune-delta-negative" : "tune-delta-positive";

    // Persist expanded state per battery name across re-renders.
    var expandKey = "model-expanded:" + name;
    var expanded = localStorage.getItem(expandKey) === "1";
    var caret = expanded ? "▾" : "▸";

    var headerLine =
      '<div class="model-card-header" data-expand="' + esc(name) + '" style="cursor:pointer">' +
      '<span class="model-name">' + caret + ' ' + esc(name) + ' ' + cascadeBadge + '</span>' +
      '<span class="model-confidence">' + m.n_samples + ' samples · ' + conf + '</span>' +
      '</div>';

    // Compact summary always visible: health bar + drift.
    var summary =
      '<div class="model-bar"><div class="model-bar-fill" style="width:' + healthPct + '%"></div></div>' +
      '<div class="model-health-text">' +
      '<span class="' + healthClass + '">health ' + healthPct + '%</span>' +
      '<span class="' + driftClass + '">' + driftStr + '</span>' +
      '</div>';

    // Details only rendered when expanded — keeps the section quiet.
    var details = !expanded ? "" :
      '<div class="model-stats" style="margin-top:8px">' +
      '<span class="model-stat-label">τ (response)</span>' +
      '<span class="model-stat-value">' + tau + ' s</span>' +
      '<span class="model-stat-label">gain</span>' +
      '<span class="model-stat-value">' + gain + '</span>' +
      '<span class="model-stat-label">deadband</span>' +
      '<span class="model-stat-value">' + (m.deadband_w || 0).toFixed(0) + ' W</span>' +
      '<span class="model-stat-label">calibrated</span>' +
      '<span class="model-stat-value">' + calibratedAgo + '</span>' +
      '</div>' +
      '<button class="btn-reset-model" data-reset-battery="' + esc(name) + '" ' +
      'style="margin-top:8px;padding:4px 10px;font-size:0.7rem;background:var(--surface2);' +
      'border:1px solid var(--border);color:var(--text-dim);border-radius:3px;cursor:pointer;width:100%">' +
      '↻ Reset model' +
      '</button>';

    return '<div class="model-card">' + headerLine + summary + details + '</div>';
  }

  // Expand/collapse on header click. Listen on document because the model
  // panel now lives inside each driver card (rendered by app.js), not
  // inside #models-grid. Use closest() so clicks on inner spans still
  // toggle the card. State persisted to localStorage so it survives the
  // 3s re-render cycle.
  document.addEventListener("click", function (e) {
    var hdr = e.target.closest && e.target.closest("[data-expand]");
    if (hdr && !e.target.dataset.resetBattery) {
      var name = hdr.dataset.expand;
      var key = "model-expanded:" + name;
      if (localStorage.getItem(key) === "1") {
        localStorage.removeItem(key);
      } else {
        localStorage.setItem(key, "1");
      }
      // Re-render this panel in place using cached data so the click is
      // instant. Previously we waited for fetchModels → /api/battery_models
      // → next /api/status poll (app.js renderDrivers), which added up to
      // a 3s lag before the section visibly unfolded.
      var m = lastModels[name];
      if (m) {
        var panel = hdr.closest(".driver-model-panel");
        if (panel) {
          // renderInlineModel returns the full panel wrapper; strip it so
          // we replace panel's inner HTML, preserving the outer element
          // and any click-state the browser has on it.
          var html = renderInlineModel(name, m);
          var tmp = document.createElement("div");
          tmp.innerHTML = html;
          var newPanel = tmp.firstChild;
          if (newPanel) panel.innerHTML = newPanel.innerHTML;
        }
      }
      // Still kick off a background refresh so we see the latest numbers.
      fetchModels();
      return;
    }
    if (!e.target.dataset || !e.target.dataset.resetBattery) return;
    var name = e.target.dataset.resetBattery;
    if (!confirm("Reset " + name + " model to fresh defaults?\\n\\nRLS will re-learn from scratch. Baseline (if set by self-tune) will be cleared.")) return;
    fetch("/api/battery_models/reset", {
      method: "POST",
      headers: { "Content-Type": "application/json" },
      body: JSON.stringify({ battery: name }),
    })
      .then(function (r) { return r.ok ? r.json() : r.json().then(function (e) { throw new Error(e.error); }); })
      .then(function () { fetchModels(); })
      .catch(function (e) { alert("Reset failed: " + e.message); });
  });

  function humanAge(s) {
    if (s < 60) return Math.round(s) + "s ago";
    if (s < 3600) return Math.round(s / 60) + "m ago";
    if (s < 86400) return Math.round(s / 3600) + "h ago";
    return Math.round(s / 86400) + "d ago";
  }

  // ---- Self-tune modal ----

  function openModal() {
    modal.classList.remove("hidden");
    setStatus("");
    // Decide what to render: idle (checklist), active (progress), or done (diff)
    fetch("/api/self_tune/status")
      .then(function (r) { return r.json(); })
      .then(function (s) {
        if (s.active) {
          startBtn.style.display = "none";
          cancelBtn.style.display = "inline-block";
          renderProgress(s);
          startTunePolling();
        } else if (Object.keys(s.after || {}).length > 0) {
          startBtn.textContent = "Run again";
          startBtn.style.display = "inline-block";
          cancelBtn.style.display = "none";
          renderDiff(s);
        } else {
          startBtn.textContent = "Start calibration";
          startBtn.style.display = "inline-block";
          cancelBtn.style.display = "none";
          renderChecklist();
        }
      });
  }

  function renderChecklist() {
    var names = Object.keys(lastModels).sort();
    body.innerHTML =
      '<p style="color:var(--text-dim);font-size:0.9rem;margin:0 0 8px 0">' +
      'Pause grid balancing for ~3 minutes per battery. Drives each through a known step pattern, ' +
      'fits an ARX(1) model from the response, and writes the result as the baseline for hardware-health drift detection.' +
      '</p>' +
      '<div class="self-tune-warning">' +
      '⚠ Recommended only when:<br>' +
      '&nbsp;&nbsp;• Low PV generation (cloudy or evening)<br>' +
      '&nbsp;&nbsp;• House load stable (no major appliances cycling)<br>' +
      '&nbsp;&nbsp;• Battery SoC between 30–70%' +
      '</div>' +
      '<div class="self-tune-checklist">' +
      names.map(function (n) {
        return '<label><input type="checkbox" data-tune-battery="' + esc(n) + '" checked> ' + esc(n) + '</label>';
      }).join("") +
      '</div>';
  }

  function renderProgress(s) {
    var stepNames = {
      stabilize: "Stabilize at 0W",
      step_up_small: "Small step UP (+1000W)",
      settle_up: "Settle",
      step_down_small: "Small step DOWN (-1000W)",
      settle_down: "Settle",
      step_up_large: "Large step UP (+3000W)",
      settle_high_up: "Settle",
      step_down_large: "Large step DOWN (-3000W)",
      settle_high_down: "Settle",
      fit: "Fitting model parameters...",
      done: "Done",
    };
    var stepDur = {
      stabilize: 15, step_up_small: 15, settle_up: 15,
      step_down_small: 15, settle_down: 15,
      step_up_large: 20, settle_high_up: 10,
      step_down_large: 20, settle_high_down: 10,
      fit: 1, done: 0,
    };
    var stepLabel = stepNames[s.current_step] || s.current_step;
    var stepProg = stepDur[s.current_step]
      ? Math.min(100, (s.step_elapsed_s / stepDur[s.current_step]) * 100)
      : 0;
    var totalSteps = 9; // active steps before fit
    var perBatterySec = 135; // approx total per battery
    var totalSec = perBatterySec * s.battery_total;
    var overallPct = Math.min(100, (s.total_elapsed_s / totalSec) * 100);

    body.innerHTML =
      '<div class="self-tune-progress">' +
      '<div class="self-tune-step">' +
      'Battery <strong>' + esc(s.current_battery) + '</strong> ' +
      '(' + (s.battery_index + 1) + '/' + s.battery_total + ')' +
      '</div>' +
      '<div class="self-tune-step" style="font-size:0.85rem;color:var(--text-dim)">' + esc(stepLabel) + '</div>' +
      '<div class="self-tune-bar"><div class="self-tune-bar-fill" style="width:' + stepProg + '%"></div></div>' +
      '<div class="self-tune-meta">' +
      '<span>Step: ' + s.step_elapsed_s.toFixed(0) + 's</span>' +
      '<span>Total: ' + s.total_elapsed_s.toFixed(0) + 's / ~' + totalSec + 's</span>' +
      '</div>' +
      '<div class="self-tune-bar-overall"><div class="self-tune-bar-overall-fill" style="width:' + overallPct + '%"></div></div>' +
      '</div>';
  }

  function renderDiff(s) {
    var rows = '';
    var names = Object.keys(s.after);
    names.forEach(function (n) {
      var b = s.before[n] || {};
      var a = s.after[n];
      rows += diffRow(n, "τ (s)", b.tau_s, a.tau_s, 2);
      rows += diffRow(n, "gain", b.gain, a.gain, 3);
    });
    body.innerHTML =
      '<p style="color:var(--text-dim);font-size:0.85rem;margin:0 0 8px 0">Calibration complete.</p>' +
      '<table class="tune-diff-table">' +
      '<tr><th>Battery</th><th>Param</th><th>Before</th><th>After</th><th>Δ</th></tr>' +
      rows +
      '</table>';
  }

  function diffRow(name, label, before, after, decimals) {
    if (after == null) return '';
    var b = before == null ? '–' : before.toFixed(decimals);
    var a = after.toFixed(decimals);
    var delta = (before != null) ? (after - before) : 0;
    var deltaStr = (delta > 0 ? '+' : '') + delta.toFixed(decimals);
    var cls = Math.abs(delta) < 1e-6 ? "tune-delta-neutral"
      : delta > 0 ? "tune-delta-positive" : "tune-delta-negative";
    return '<tr>' +
      '<td>' + esc(name) + '</td>' +
      '<td>' + esc(label) + '</td>' +
      '<td>' + b + '</td>' +
      '<td>' + a + '</td>' +
      '<td class="' + cls + '">' + deltaStr + '</td>' +
      '</tr>';
  }

  function startTunePolling() {
    if (tunePollHandle) return;
    tunePollHandle = setInterval(function () {
      fetch("/api/self_tune/status")
        .then(function (r) { return r.json(); })
        .then(function (s) {
          if (s.active) {
            renderProgress(s);
          } else {
            stopTunePolling();
            startBtn.style.display = "inline-block";
            cancelBtn.style.display = "none";
            startBtn.textContent = "Run again";
            renderDiff(s);
            // Refresh model cards once tune is done
            fetchModels();
            if (s.last_error) setStatus(s.last_error, "error");
          }
        });
    }, TUNE_POLL);
  }

  function stopTunePolling() {
    if (tunePollHandle) {
      clearInterval(tunePollHandle);
      tunePollHandle = null;
    }
  }

  function setStatus(msg, kind) {
    statusEl.textContent = msg || "";
    statusEl.className = "settings-status" + (kind ? " " + kind : "");
  }

  function closeModal() {
    modal.classList.add("hidden");
    stopTunePolling();
  }

  // Wire events
  if (openBtn) openBtn.addEventListener("click", openModal);
  if (closeBtn) closeBtn.addEventListener("click", closeModal);
  if (modal) modal.addEventListener("click", function (e) { if (e.target === modal) closeModal(); });

  if (startBtn) startBtn.addEventListener("click", function () {
    var checks = body.querySelectorAll("[data-tune-battery]");
    var batteries = [];
    checks.forEach(function (c) { if (c.checked) batteries.push(c.dataset.tuneBattery); });
    if (batteries.length === 0) {
      setStatus("Select at least one battery", "error");
      return;
    }
    setStatus("Starting...");
    fetch("/api/self_tune/start", {
      method: "POST",
      headers: { "Content-Type": "application/json" },
      body: JSON.stringify({ batteries: batteries }),
    })
      .then(function (r) { return r.ok ? r.json() : r.json().then(function (e) { throw new Error(e.error || ("HTTP " + r.status)); }); })
      .then(function () {
        setStatus("");
        startBtn.style.display = "none";
        cancelBtn.style.display = "inline-block";
        startTunePolling();
      })
      .catch(function (e) { setStatus("Failed: " + e.message, "error"); });
  });

  if (cancelBtn) cancelBtn.addEventListener("click", function () {
    setStatus("Cancelling...");
    fetch("/api/self_tune/cancel", { method: "POST" })
      .then(function () {
        stopTunePolling();
        setStatus("Cancelled");
        startBtn.style.display = "inline-block";
        cancelBtn.style.display = "none";
        startBtn.textContent = "Start calibration";
        renderChecklist();
      });
  });

  function esc(s) {
    var d = document.createElement("div");
    d.textContent = s == null ? "" : String(s);
    return d.innerHTML;
  }

  // ---- Init ----
  fetchModels();
  setInterval(fetchModels, POLL_INTERVAL);
})();
