// forty-two-watts dashboard — plain JS, no framework

(function () {
  "use strict";

  const POLL_INTERVAL = 2000;        // status poll cadence — snappier cards
  const CHART_POINTS = 360;          // up to 30 min of points (server pushes every ~5s)
  const CHART_RANGE_MS = {           // visible time window per range option
    "5m": 5 * 60 * 1000,
    "15m": 15 * 60 * 1000,
    "1h": 60 * 60 * 1000,
    "6h": 6 * 60 * 60 * 1000,
    "24h": 24 * 60 * 60 * 1000,
    "3d": 3 * 24 * 60 * 60 * 1000,
  };
  let chartRange = "5m";             // current selected range
  let currentMode = null;
  let animating = true;              // 30fps redraw loop flag
  let lastDataTs = 0;                // browser-clock timestamp of newest pushed point
  let lastPushAt = 0;                // browser-clock timestamp of last push attempt — for dedupe (NEVER mix with server ts)
  let lastFlashAt = 0;               // browser-clock timestamp of last "new data" flash

  // ---- Chart data ----
  var chartHistory = {
    grid: [],
    pv: [],
    load: [],
    ferroamp_bat: [],
    sungrow_bat: [],
    ferroamp_target: [],
    sungrow_target: [],
    timestamps: [],
    // Energy counters (cumulative Wh, today-scoped)
    e_import: [],
    e_export: [],
    e_pv: [],
    e_charged: [],
    e_discharged: [],
    e_load: [],
  };
  var chartLayout = null;
  var hoverIndex = -1;
  var chartView = "power"; // "power" or "energy"

  // ---- DOM refs ----
  const $ = (id) => document.getElementById(id);
  const gridW = $("grid-w");
  const gridDir = $("grid-dir");
  const loadW = $("load-w");
  const cardGrid = $("card-grid");
  const pvW = $("pv-w");
  const batW = $("bat-w");
  const batDir = $("bat-dir");
  const batSoc = $("bat-soc");
  const socFill = $("soc-fill");
  const connStatus = $("conn-status");
  const driversGrid = $("drivers-grid");
  const dispatchList = $("dispatch-list");
  const modeButtons = $("mode-buttons");
  const gridTargetSlider = $("grid-target-slider");
  const gridTargetValue = $("grid-target-value");
  const gridTargetSend = $("grid-target-send");
  const peakLimitSlider = $("peak-limit-slider");
  const peakLimitValue = $("peak-limit-value");
  const peakLimitSend = $("peak-limit-send");
  const evSlider = $("ev-slider");
  const evValue = $("ev-value");
  const evSend = $("ev-send");
  const fuseUse = $("fuse-use");
  const fuseFill = $("fuse-fill");
  const eImport = $("e-import");
  const eExport = $("e-export");
  const ePv = $("e-pv");
  const eCharged = $("e-charged");
  const eDischarged = $("e-discharged");
  const eLoad = $("e-load");
  const lastUpdate = $("last-update");
  const versionEl = $("version");
  const FUSE_MAX_W = 11040; // 16A * 230V * 3ph

  // ---- Formatting ----
  function formatW(w) {
    const abs = Math.abs(w);
    if (abs >= 1000) {
      return (w / 1000).toFixed(1) + " kW";
    }
    return Math.round(w) + " W";
  }

  function formatSoc(soc) {
    return Math.round(soc * 100) + "%";
  }

  function formatKwh(wh) {
    var kwh = (wh || 0) / 1000;
    if (kwh >= 100) return kwh.toFixed(0) + " kWh";
    if (kwh >= 10) return kwh.toFixed(1) + " kWh";
    return kwh.toFixed(2) + " kWh";
  }

  function statusClass(status) {
    if (!status) return "status-offline";
    const s = status.toLowerCase();
    if (s === "ok") return "status-ok";
    if (s === "degraded") return "status-degraded";
    return "status-offline";
  }

  // ---- Render ----
  function render(data) {
    // PUSH CHART DATA FIRST — never let a DOM render error somewhere below
    // silently kill the chart-update path. (Prior bug: missing #dispatch-list
    // threw inside renderDispatch, which is between renderDrivers and
    // pushChartData, so the chart starved while ticks kept incrementing.)
    try {
      var fd0 = (data.drivers || {}).ferroamp || {};
      var sd0 = (data.drivers || {}).sungrow || {};
      var ft0 = 0, st0 = 0;
      (data.dispatch || []).forEach(function(d) {
        if (d.driver === "ferroamp") ft0 = d.target_w;
        if (d.driver === "sungrow") st0 = d.target_w;
      });
      pushChartData(data, fd0.bat_w||0, sd0.bat_w||0, ft0, st0);
    } catch (e) { console.error("pushChartData error:", e); }

    // Version (live from API — survives stale browser cache of index.html)
    if (versionEl && data.version) {
      versionEl.textContent = "v" + data.version;
    }
    // Grid
    gridW.textContent = formatW(data.grid_w);
    if (data.grid_w > 10) {
      gridDir.textContent = "importing";
      gridW.className = "card-value val-import";
    } else if (data.grid_w < -10) {
      gridDir.textContent = "exporting";
      gridW.className = "card-value val-export";
    } else {
      gridDir.textContent = "balanced";
      gridW.className = "card-value val-neutral";
    }

    // PV — negative = generating
    pvW.textContent = formatW(data.pv_w);
    pvW.className = "card-value val-generation";

    // Load
    loadW.textContent = formatW(data.load_w || 0);

    // Battery — positive=charge, negative=discharge
    batW.textContent = formatW(data.bat_w);
    if (data.bat_w > 10) {
      batDir.textContent = "charging";
      batW.className = "card-value val-charging";
    } else if (data.bat_w < -10) {
      batDir.textContent = "discharging";
      batW.className = "card-value val-discharging";
    } else {
      batDir.textContent = "idle";
      batW.className = "card-value val-neutral";
    }

    // SoC
    var socPct = Math.round(data.bat_soc * 100);
    batSoc.textContent = socPct + "%";
    socFill.style.width = socPct + "%";

    // Mode buttons
    currentMode = data.mode;
    var buttons = modeButtons.querySelectorAll("button");
    buttons.forEach(function (btn) {
      if (btn.dataset.mode === data.mode) {
        btn.classList.add("active");
      } else {
        btn.classList.remove("active");
      }
    });

    // Grid target — only update slider if user is not actively dragging
    if (gridTargetSlider && document.activeElement !== gridTargetSlider) {
      gridTargetSlider.value = data.grid_target_w;
      gridTargetValue.textContent = formatW(data.grid_target_w);
    }
    if (peakLimitSlider && document.activeElement !== peakLimitSlider && data.peak_limit_w != null) {
      peakLimitSlider.value = data.peak_limit_w;
      peakLimitValue.textContent = formatW(data.peak_limit_w);
    }
    if (evSlider && document.activeElement !== evSlider && data.ev_charging_w != null) {
      evSlider.value = data.ev_charging_w;
      evValue.textContent = formatW(data.ev_charging_w);
    }

    // Energy today
    if (data.energy && data.energy.today) {
      var t = data.energy.today;
      if (eImport) eImport.textContent = formatKwh(t.import_wh);
      if (eExport) eExport.textContent = formatKwh(t.export_wh);
      if (ePv) ePv.textContent = formatKwh(t.pv_wh);
      if (eCharged) eCharged.textContent = formatKwh(t.bat_charged_wh);
      if (eDischarged) eDischarged.textContent = formatKwh(t.bat_discharged_wh);
      if (eLoad) eLoad.textContent = formatKwh(t.load_wh);
    }

    // Fuse gauge (if present)
    if (fuseUse && fuseFill) {
      var totalDischarge = 0;
      if (data.bat_w < 0) totalDischarge = Math.abs(data.bat_w);
      var pvGen = Math.abs(data.pv_w);
      var throughput = Math.max(Math.abs(data.grid_w), pvGen + totalDischarge);
      var fusePct = Math.min(100, (throughput / FUSE_MAX_W) * 100);
      var amps = throughput / 230 / 3;
      fuseUse.textContent = amps.toFixed(1) + " A";
      fuseFill.style.width = fusePct + "%";
      fuseFill.className = "fuse-fill" + (fusePct > 85 ? " crit" : fusePct > 65 ? " warn" : "");
    }

    // Drivers
    renderDrivers(data.drivers || {});

    // Dispatch
    renderDispatch(data.dispatch || []);

    // Chart push happens at the top of render() for resilience to DOM errors.
    // The rAF loop redraws ~30fps for the smooth flowing feel.
    // Timestamp is updated in fetchStatus (before render, so it's robust to render errors)
  }

  function pushChartData(data, ferroBat, sunBat, ferroTarget, sunTarget) {
    var t = (data.energy && data.energy.today) || {};
    var now = Date.now();
    // Dedupe via JS-side push timer ONLY — never compare against server timestamps
    // from chartHistory.timestamps because clock skew between RPi and browser
    // would silently block all pushes if server is even slightly ahead.
    if (lastPushAt > 0 && now - lastPushAt < 800) return;
    lastPushAt = now;

    chartHistory.grid.push(data.grid_w);
    chartHistory.pv.push(data.pv_w);
    chartHistory.load.push(data.load_w || 0);
    chartHistory.ferroamp_bat.push(ferroBat);
    chartHistory.sungrow_bat.push(sunBat);
    chartHistory.ferroamp_target.push(ferroTarget);
    chartHistory.sungrow_target.push(sunTarget);
    chartHistory.timestamps.push(now);
    chartHistory.e_import.push(t.import_wh || 0);
    chartHistory.e_export.push(t.export_wh || 0);
    chartHistory.e_pv.push(t.pv_wh || 0);
    chartHistory.e_charged.push(t.bat_charged_wh || 0);
    chartHistory.e_discharged.push(t.bat_discharged_wh || 0);
    chartHistory.e_load.push(t.load_wh || 0);
    if (chartHistory.grid.length > CHART_POINTS) {
      Object.keys(chartHistory).forEach(function(k) { chartHistory[k].shift(); });
    }
    lastDataTs = now;
    // Fire a discrete pulse for this new data point — heartbeat feel
    lastFlashAt = now;
  }

  function renderChart() {
    var canvas = document.getElementById("power-chart");
    if (!canvas) return;
    var ctx = canvas.getContext("2d");
    var dpr = window.devicePixelRatio || 1;
    var w = canvas.parentElement.clientWidth - 32;
    var h = 250;
    if (canvas.width !== w * dpr || canvas.height !== h * dpr) {
      canvas.width = w * dpr;
      canvas.height = h * dpr;
      canvas.style.width = w + "px";
      canvas.style.height = h + "px";
      ctx.setTransform(dpr, 0, 0, dpr, 0, 0);
    }

    var pad = { top: 20, right: 10, bottom: 25, left: 55 };
    var plotW = w - pad.left - pad.right;
    var plotH = h - pad.top - pad.bottom;

    var windowMs = CHART_RANGE_MS[chartRange] || CHART_RANGE_MS["5m"];
    var now = Date.now();
    var windowStart = now - windowMs;

    // Build series based on view
    var series;
    if (chartView === "energy") {
      function toKwh(arr) { return arr.map(function(x){ return x / 1000; }); }
      series = [
        { data: toKwh(chartHistory.e_import),     color: "#ef4444", width: 2, dash: [], name: "Import",     fill: true },
        { data: toKwh(chartHistory.e_export),     color: "#22c55e", width: 2, dash: [], name: "Export",     fill: true },
        { data: toKwh(chartHistory.e_pv),         color: "#10b981", width: 2, dash: [], name: "PV",         fill: true },
        { data: toKwh(chartHistory.e_charged),    color: "#3b82f6", width: 2, dash: [], name: "Charged",    fill: false },
        { data: toKwh(chartHistory.e_discharged), color: "#f59e0b", width: 2, dash: [], name: "Discharged", fill: false },
        { data: toKwh(chartHistory.e_load),       color: "#e2e8f0", width: 2, dash: [], name: "Load",       fill: false },
      ];
    } else {
      series = [
        { data: chartHistory.grid,            color: "#ef4444", width: 2,   dash: [],     name: "Grid",         fill: true },
        { data: chartHistory.pv,              color: "#22c55e", width: 2,   dash: [],     name: "PV",           fill: true },
        { data: chartHistory.load,            color: "#e2e8f0", width: 1.5, dash: [],     name: "Load",         fill: false },
        { data: chartHistory.ferroamp_bat,    color: "#f59e0b", width: 2,   dash: [],     name: "Ferroamp",     fill: false },
        { data: chartHistory.ferroamp_target, color: "#f59e0b", width: 1.5, dash: [6, 4], name: "Ferroamp tgt", fill: false },
        { data: chartHistory.sungrow_bat,     color: "#8b5cf6", width: 2,   dash: [],     name: "Sungrow",      fill: false },
        { data: chartHistory.sungrow_target,  color: "#8b5cf6", width: 1.5, dash: [6, 4], name: "Sungrow tgt",  fill: false },
      ];
    }

    // Y range only across points within the visible time window
    var visibleVals = [];
    for (var k = 0; k < chartHistory.timestamps.length; k++) {
      if (chartHistory.timestamps[k] >= windowStart) {
        for (var s = 0; s < series.length; s++) {
          if (series[s].data[k] != null) visibleVals.push(series[s].data[k]);
        }
      }
    }
    if (visibleVals.length === 0) {
      // Empty state — draw axes + "waiting for data" hint
      ctx.clearRect(0, 0, w, h);
      ctx.fillStyle = "#666";
      ctx.font = "12px monospace";
      ctx.textAlign = "center";
      ctx.fillText("waiting for data...", w / 2, h / 2);
      ctx.textAlign = "left";
      return;
    }
    var yMin = Math.min(0, Math.min.apply(null, visibleVals));
    var yMax = Math.max(100, Math.max.apply(null, visibleVals));
    var yRange = yMax - yMin || 1;
    yMin -= yRange * 0.1;
    yMax += yRange * 0.1;
    yRange = yMax - yMin;

    // Smooth y-range transitions to avoid jarring re-scales
    if (chartLayout && chartLayout.yMin != null) {
      var lerp = 0.18; // tighter = snappier; looser = smoother
      yMin = chartLayout.yMin + (yMin - chartLayout.yMin) * lerp;
      yMax = chartLayout.yMax + (yMax - chartLayout.yMax) * lerp;
      yRange = yMax - yMin;
    }

    ctx.clearRect(0, 0, w, h);

    // Clip to plot area so flowing lines don't draw over the y-axis labels
    ctx.save();
    ctx.beginPath();
    ctx.rect(pad.left, pad.top, plotW, plotH);
    ctx.clip();

    // Grid lines (drawn inside clip so they only appear in the plot area)
    ctx.strokeStyle = "#2a2a2a";
    ctx.lineWidth = 0.5;
    ctx.font = "11px monospace";
    var steps = 5;
    for (var i = 0; i <= steps; i++) {
      var y = pad.top + plotH - (plotH * i / steps);
      ctx.beginPath();
      ctx.moveTo(pad.left, y);
      ctx.lineTo(w - pad.right, y);
      ctx.stroke();
    }

    // Zero line
    if (yMin < 0 && yMax > 0) {
      var zeroY = pad.top + plotH * (1 - (0 - yMin) / yRange);
      ctx.strokeStyle = "#444";
      ctx.lineWidth = 1;
      ctx.setLineDash([4, 4]);
      ctx.beginPath();
      ctx.moveTo(pad.left, zeroY);
      ctx.lineTo(w - pad.right, zeroY);
      ctx.stroke();
      ctx.setLineDash([]);
    }

    // Map ts → x. Points outside the window get clipped naturally.
    function tsToX(ts) {
      return pad.left + plotW * (ts - windowStart) / windowMs;
    }
    function valToY(v) {
      return pad.top + plotH * (1 - (v - yMin) / yRange);
    }

    // Collect latest-point coordinates per series so we can draw the live
    // pulses OUTSIDE the clip rect (otherwise they get cut off at the right edge)
    var liveTips = [];

    // Draw each series. Fill area under prominent ones with subtle gradient.
    series.forEach(function (sr) {
      if (sr.data.length < 2 || chartHistory.timestamps.length < 2) return;

      var pts = [];
      for (var j = 0; j < sr.data.length; j++) {
        var ts = chartHistory.timestamps[j];
        if (sr.data[j] == null) continue;
        pts.push({ x: tsToX(ts), y: valToY(sr.data[j]) });
      }
      if (pts.length < 2) return;

      if (sr.fill) {
        var grad = ctx.createLinearGradient(0, pad.top, 0, pad.top + plotH);
        grad.addColorStop(0, hexAlpha(sr.color, 0.22));
        grad.addColorStop(1, hexAlpha(sr.color, 0.0));
        ctx.fillStyle = grad;
        ctx.beginPath();
        ctx.moveTo(pts[0].x, pad.top + plotH);
        for (var p = 0; p < pts.length; p++) ctx.lineTo(pts[p].x, pts[p].y);
        ctx.lineTo(pts[pts.length - 1].x, pad.top + plotH);
        ctx.closePath();
        ctx.fill();
      }

      ctx.strokeStyle = sr.color;
      ctx.lineWidth = sr.width;
      ctx.lineJoin = "round";
      ctx.lineCap = "round";
      ctx.setLineDash(sr.dash);
      ctx.beginPath();
      ctx.moveTo(pts[0].x, pts[0].y);
      for (var p2 = 1; p2 < pts.length; p2++) ctx.lineTo(pts[p2].x, pts[p2].y);
      ctx.stroke();
      ctx.setLineDash([]);

      // Cache the right-most point for un-clipped pulse drawing
      if (sr.width >= 2) {
        liveTips.push({ x: pts[pts.length - 1].x, y: pts[pts.length - 1].y, color: sr.color });
      }
    });

    ctx.restore();

    // ---- Live pulses (drawn outside clip so they're never cut off) ----
    // ONE discrete ripple per new data point — no continuous breathing.
    // Each new push (lastFlashAt) triggers an 800ms expanding ring + flash.
    var FLASH_MS = 800;
    var sinceFlash = lastFlashAt > 0 ? (now - lastFlashAt) : Infinity;
    var flashActive = sinceFlash < FLASH_MS;
    var flashProgress = flashActive ? (sinceFlash / FLASH_MS) : 1; // 0..1
    var rippleR = 5 + flashProgress * 32;
    var rippleAlpha = flashActive ? (1 - flashProgress) * 0.85 : 0;
    // Brief brightness boost on the dot itself for the first 200ms
    var dotBoost = sinceFlash < 200 ? (1 - sinceFlash / 200) : 0;

    liveTips.forEach(function (tip) {
      // Static halo — soft, always visible (NOT breathing)
      ctx.fillStyle = tip.color;
      ctx.globalAlpha = 0.18;
      ctx.beginPath();
      ctx.arc(tip.x, tip.y, 7, 0, Math.PI * 2);
      ctx.fill();

      // Solid core dot
      ctx.globalAlpha = 1;
      ctx.beginPath();
      ctx.arc(tip.x, tip.y, 3 + dotBoost * 1.5, 0, Math.PI * 2);
      ctx.fill();

      // White center for crispness
      ctx.fillStyle = "#fff";
      ctx.globalAlpha = 0.7 + dotBoost * 0.3;
      ctx.beginPath();
      ctx.arc(tip.x, tip.y, 1.2 + dotBoost, 0, Math.PI * 2);
      ctx.fill();

      // Ripple — fires once per new data point, expands and fades
      if (flashActive) {
        ctx.strokeStyle = tip.color;
        ctx.globalAlpha = rippleAlpha;
        ctx.lineWidth = 2;
        ctx.beginPath();
        ctx.arc(tip.x, tip.y, rippleR, 0, Math.PI * 2);
        ctx.stroke();
      }
    });
    ctx.globalAlpha = 1;

    // Subtle vertical "now" line — anchor point so the eye knows where present is
    var nowX = pad.left + plotW;
    ctx.strokeStyle = "rgba(255,255,255,0.12)";
    ctx.lineWidth = 1;
    ctx.beginPath();
    ctx.moveTo(nowX, pad.top);
    ctx.lineTo(nowX, pad.top + plotH);
    ctx.stroke();

    // Y-axis labels (outside clip so they're fully visible)
    ctx.fillStyle = "#888";
    ctx.font = "11px monospace";
    for (var i2 = 0; i2 <= steps; i2++) {
      var yVal = yMin + (yRange * i2 / steps);
      var ly = pad.top + plotH - (plotH * i2 / steps);
      ctx.fillText(chartView === "energy" ? yVal.toFixed(1) + " kWh" : formatW(yVal), 2, ly + 4);
    }

    // Time labels
    ctx.fillStyle = "#666";
    ctx.fillText(chartRange + " ago", pad.left, h - 5);
    ctx.textAlign = "right";
    ctx.fillText("now", w - pad.right, h - 5);
    ctx.textAlign = "left";

    // Live freshness indicator — top-right corner of plot area
    if (lastDataTs > 0) {
      var ageMs = now - lastDataTs;
      var ageStr;
      if (ageMs < 1500) ageStr = "live";
      else if (ageMs < 60_000) ageStr = Math.round(ageMs / 1000) + "s ago";
      else ageStr = Math.round(ageMs / 60_000) + "m ago";
      var fresh = ageMs < 5000;
      // Dot flashes briefly when fresh data lands (discrete, not breathing)
      var dotFlash = sinceFlash < 400 ? (1 - sinceFlash / 400) : 0;
      ctx.fillStyle = fresh ? "#22c55e" : "#f59e0b";
      ctx.globalAlpha = 0.35 + dotFlash * 0.5;
      ctx.beginPath();
      ctx.arc(w - pad.right - 78, pad.top + 4, 3.5 + dotFlash * 3, 0, Math.PI * 2);
      ctx.fill();
      ctx.globalAlpha = 1;
      ctx.beginPath();
      ctx.arc(w - pad.right - 78, pad.top + 4, 2.5, 0, Math.PI * 2);
      ctx.fill();
      ctx.font = "10px monospace";
      ctx.fillStyle = fresh ? "#aaa" : "#f59e0b";
      ctx.fillText(ageStr, w - pad.right - 70, pad.top + 8);
    }

    // Store layout for hover tooltip + animation loop
    chartLayout = {
      pad: pad, plotW: plotW, plotH: plotH, w: w, h: h,
      yMin: yMin, yMax: yMax, yRange: yRange,
      windowStart: windowStart, windowMs: windowMs,
      series: series,
      pointCount: chartHistory.timestamps.length
    };

    if (hoverIndex >= 0 && hoverIndex < chartLayout.pointCount) {
      drawHoverOverlay(ctx);
    }
  }

  // hex like "#ef4444" → "rgba(239,68,68,a)"
  function hexAlpha(hex, alpha) {
    var h = hex.replace("#", "");
    if (h.length === 3) h = h[0]+h[0]+h[1]+h[1]+h[2]+h[2];
    var r = parseInt(h.substr(0,2), 16);
    var g = parseInt(h.substr(2,2), 16);
    var b = parseInt(h.substr(4,2), 16);
    return "rgba(" + r + "," + g + "," + b + "," + alpha + ")";
  }

  function drawHoverOverlay(ctx) {
    if (!chartLayout) return;
    var l = chartLayout;
    var i = hoverIndex;
    // Map by timestamp (matches the time-anchored line drawing)
    var ts = chartHistory.timestamps[i];
    if (ts == null) return;
    var x = l.pad.left + l.plotW * (ts - l.windowStart) / l.windowMs;

    // Vertical line
    ctx.strokeStyle = "rgba(255,255,255,0.3)";
    ctx.lineWidth = 1;
    ctx.setLineDash([2, 2]);
    ctx.beginPath();
    ctx.moveTo(x, l.pad.top);
    ctx.lineTo(x, l.pad.top + l.plotH);
    ctx.stroke();
    ctx.setLineDash([]);

    // Dots on each series at this x
    l.series.forEach(function (s) {
      if (i >= s.data.length) return;
      var y = l.pad.top + l.plotH * (1 - (s.data[i] - l.yMin) / l.yRange);
      ctx.fillStyle = s.color;
      ctx.beginPath();
      ctx.arc(x, y, 3, 0, Math.PI * 2);
      ctx.fill();
    });

    // Tooltip box — labels match current view
    var labels = chartView === "energy" ? [
      { name: "Import",     data: chartHistory.e_import,     color: "#ef4444" },
      { name: "Export",     data: chartHistory.e_export,     color: "#22c55e" },
      { name: "PV",         data: chartHistory.e_pv,         color: "#10b981" },
      { name: "Charged",    data: chartHistory.e_charged,    color: "#3b82f6" },
      { name: "Discharged", data: chartHistory.e_discharged, color: "#f59e0b" },
      { name: "Load",       data: chartHistory.e_load,       color: "#e2e8f0" },
    ] : [
      { name: "Grid",     data: chartHistory.grid,     color: "#ef4444" },
      { name: "PV",       data: chartHistory.pv,       color: "#22c55e" },
      { name: "Load",     data: chartHistory.load,     color: "#e2e8f0" },
      { name: "Ferroamp", data: chartHistory.ferroamp_bat, color: "#f59e0b" },
      { name: "  target", data: chartHistory.ferroamp_target, color: "#f59e0b", dim: true },
      { name: "Sungrow",  data: chartHistory.sungrow_bat, color: "#8b5cf6" },
      { name: "  target", data: chartHistory.sungrow_target, color: "#8b5cf6", dim: true },
    ];

    var ts = chartHistory.timestamps[i] || 0;
    var timeStr = ts > 0 ? new Date(ts).toLocaleTimeString() : "";
    var lineHeight = 16;
    var boxW = 170;
    var boxH = (labels.length + 1) * lineHeight + 10;

    // Position tooltip (avoid going off-screen)
    var boxX = x + 10;
    if (boxX + boxW > l.w - 5) boxX = x - boxW - 10;
    var boxY = l.pad.top + 5;

    ctx.fillStyle = "rgba(20,20,35,0.95)";
    ctx.strokeStyle = "#444";
    ctx.lineWidth = 1;
    ctx.fillRect(boxX, boxY, boxW, boxH);
    ctx.strokeRect(boxX, boxY, boxW, boxH);

    ctx.font = "10px monospace";
    ctx.fillStyle = "#888";
    ctx.fillText(timeStr, boxX + 6, boxY + lineHeight - 2);

    labels.forEach(function (lab, idx) {
      if (i >= lab.data.length) return;
      var y = boxY + (idx + 2) * lineHeight - 4;
      ctx.fillStyle = lab.color;
      ctx.fillRect(boxX + 6, y - 8, 8, 8);
      ctx.fillStyle = lab.dim ? "#888" : "#ddd";
      ctx.fillText(lab.name, boxX + 18, y);
      ctx.fillStyle = "#fff";
      ctx.textAlign = "right";
      var val = chartView === "energy"
        ? lab.data[i].toFixed(2) + " kWh"
        : formatW(lab.data[i]);
      ctx.fillText(val, boxX + boxW - 6, y);
      ctx.textAlign = "left";
    });
  }

  function renderDrivers(drivers) {
    driversGrid.innerHTML = "";
    var names = Object.keys(drivers).sort();
    names.forEach(function (name) {
      var d = drivers[name];
      var card = document.createElement("div");
      card.className = "driver-card";

      var meterW = d.meter_w != null ? d.meter_w : 0;
      var pvWVal = d.pv_w != null ? d.pv_w : 0;
      var batWVal = d.bat_w != null ? d.bat_w : 0;
      var batSocVal = d.bat_soc != null ? d.bat_soc : 0;
      var ticks = d.tick_count != null ? d.tick_count : 0;
      var errors = d.consecutive_errors != null ? d.consecutive_errors : 0;

      card.innerHTML =
        '<div class="driver-header">' +
        '  <span class="driver-name">' + escHtml(name) + "</span>" +
        '  <span class="status-dot ' + statusClass(d.status) + '" title="' + escHtml(d.status || "unknown") + '"></span>' +
        "</div>" +
        '<div class="driver-stats">' +
        '  <span class="stat-label">Meter</span><span class="stat-value">' + formatW(meterW) + "</span>" +
        '  <span class="stat-label">PV</span><span class="stat-value">' + formatW(pvWVal) + "</span>" +
        '  <span class="stat-label">Battery</span><span class="stat-value">' + formatW(batWVal) + "</span>" +
        '  <span class="stat-label">SoC</span><span class="stat-value">' + formatSoc(batSocVal) + "</span>" +
        '  <span class="stat-label">Ticks</span><span class="stat-value">' + ticks + "</span>" +
        '  <span class="stat-label">Errors</span><span class="stat-value">' + errors + "</span>" +
        "</div>" +
        '<div class="driver-soc-bar"><div class="driver-soc-fill" style="width:' + Math.round(batSocVal * 100) + '%"></div></div>';

      driversGrid.appendChild(card);
    });
  }

  function renderDispatch(dispatch) {
    // index.html no longer has #dispatch-list — graceful no-op if missing
    if (!dispatchList) return;
    dispatchList.innerHTML = "";
    dispatch.forEach(function (d) {
      var item = document.createElement("div");
      item.className = "dispatch-item";
      item.innerHTML =
        '<span class="dispatch-driver">' + escHtml(d.driver) + "</span>" +
        "<span>" +
        '<span class="dispatch-target">' + formatW(d.target_w) + "</span>" +
        (d.clamped ? '<span class="dispatch-clamped">CLAMPED</span>' : "") +
        "</span>";
      dispatchList.appendChild(item);
    });
    if (dispatch.length === 0) {
      dispatchList.innerHTML = '<div class="dispatch-item" style="color:var(--text-dim)">No dispatch targets</div>';
    }
  }

  function escHtml(str) {
    var div = document.createElement("div");
    div.textContent = str;
    return div.innerHTML;
  }

  // ---- API ----
  function fetchStatus() {
    fetch("/api/status")
      .then(function (res) {
        if (!res.ok) throw new Error("HTTP " + res.status);
        return res.json();
      })
      .then(function (data) {
        setConnected(true);
        // Always refresh timestamp on successful fetch
        lastUpdate.textContent = "Last update: " + new Date().toLocaleTimeString();
        // Isolate render errors from connection state / timestamp
        try { render(data); }
        catch (e) { console.error("render error:", e); }
      })
      .catch(function (e) {
        console.warn("status fetch failed:", e);
        setConnected(false);
      });
  }

  function setMode(mode) {
    fetch("/api/mode", {
      method: "POST",
      headers: { "Content-Type": "application/json" },
      body: JSON.stringify({ mode: mode }),
    })
      .then(function (res) {
        if (!res.ok) throw new Error("HTTP " + res.status);
        // Immediately poll to reflect change
        fetchStatus();
      })
      .catch(function () {
        setConnected(false);
      });
  }

  function setTarget(w) {
    postJson("/api/target", { grid_target_w: w });
  }

  function setPeakLimit(w) {
    postJson("/api/peak_limit", { peak_limit_w: w });
  }

  function setEvCharging(w) {
    postJson("/api/ev_charging", { power_w: w, active: w > 0 });
  }

  function postJson(url, body) {
    fetch(url, {
      method: "POST",
      headers: { "Content-Type": "application/json" },
      body: JSON.stringify(body),
    })
      .then(function (res) {
        if (!res.ok) throw new Error("HTTP " + res.status);
        fetchStatus();
      })
      .catch(function (e) {
        console.warn("POST failed:", url, e);
        // Don't flip connection state on POST failures —
        // connection state reflects read polling, not write commands
      });
  }

  function setConnected(ok) {
    if (ok) {
      connStatus.className = "conn-status connected";
      connStatus.title = "Connected";
      // render() will update lastUpdate with timestamp
    } else {
      connStatus.className = "conn-status disconnected";
      connStatus.title = "Disconnected";
      lastUpdate.textContent = "Connection lost";
    }
  }

  // ---- Events ----
  modeButtons.addEventListener("click", function (e) {
    if (e.target.tagName === "BUTTON" && e.target.dataset.mode) {
      setMode(e.target.dataset.mode);
    }
  });

  gridTargetSlider.addEventListener("input", function () {
    gridTargetValue.textContent = formatW(Number(gridTargetSlider.value));
  });

  gridTargetSend.addEventListener("click", function () {
    setTarget(Number(gridTargetSlider.value));
  });

  peakLimitSlider.addEventListener("input", function () {
    peakLimitValue.textContent = formatW(Number(peakLimitSlider.value));
  });
  peakLimitSend.addEventListener("click", function () {
    setPeakLimit(Number(peakLimitSlider.value));
  });

  evSlider.addEventListener("input", function () {
    evValue.textContent = formatW(Number(evSlider.value));
  });
  evSend.addEventListener("click", function () {
    setEvCharging(Number(evSlider.value));
  });

  // Range selector
  var rangeButtons = document.getElementById("range-buttons");
  if (rangeButtons) {
    rangeButtons.addEventListener("click", function (e) {
      if (e.target.tagName === "BUTTON" && e.target.dataset.range) {
        rangeButtons.querySelectorAll("button").forEach(function (b) {
          b.classList.toggle("active", b === e.target);
        });
        chartRange = e.target.dataset.range;
        loadHistory(chartRange);
      }
    });
  }

  // Power / Energy view toggle
  var viewButtons = document.getElementById("view-buttons");
  var chartTitle = document.getElementById("chart-title");
  if (viewButtons) {
    viewButtons.addEventListener("click", function (e) {
      if (e.target.tagName === "BUTTON" && e.target.dataset.view) {
        viewButtons.querySelectorAll("button").forEach(function (b) {
          b.classList.toggle("active", b === e.target);
        });
        chartView = e.target.dataset.view;
        if (chartTitle) chartTitle.textContent = chartView === "energy" ? "Energy (cumulative today)" : "Power";
        updateLegend();
        renderChart();
      }
    });
  }

  function updateLegend() {
    var legend = document.getElementById("chart-legend");
    if (!legend) return;
    var items = chartView === "energy" ? [
      ["#ef4444", "Import"], ["#22c55e", "Export"], ["#10b981", "PV"],
      ["#3b82f6", "Charged"], ["#f59e0b", "Discharged"], ["#e2e8f0", "Load"],
    ] : [
      ["#ef4444", "Grid"], ["#22c55e", "PV"], ["#e2e8f0", "Load"],
      ["#f59e0b", "Ferroamp"], ["#8b5cf6", "Sungrow"],
    ];
    legend.innerHTML = items.map(function(it) {
      return '<span class="legend-item"><span class="legend-color" style="background:'+it[0]+'"></span> '+it[1]+'</span>';
    }).join('');
  }

  // ---- Chart hover ----
  var canvas = document.getElementById("power-chart");
  if (canvas) {
    canvas.addEventListener("mousemove", function (e) {
      if (!chartLayout) return;
      var rect = canvas.getBoundingClientRect();
      // canvas is sized in CSS px (we use devicePixelRatio inside),
      // so no scaling needed here
      var x = e.clientX - rect.left;
      var l = chartLayout;
      if (x < l.pad.left || x > l.pad.left + l.plotW) {
        if (hoverIndex !== -1) hoverIndex = -1;
        return;
      }
      // Map x → timestamp → nearest point index
      var hoverTs = l.windowStart + (x - l.pad.left) / l.plotW * l.windowMs;
      var bestIdx = -1, bestDelta = Infinity;
      for (var i = 0; i < chartHistory.timestamps.length; i++) {
        var d = Math.abs(chartHistory.timestamps[i] - hoverTs);
        if (d < bestDelta) { bestDelta = d; bestIdx = i; }
      }
      hoverIndex = bestIdx;
      // animation loop redraws — no manual call needed
    });
    canvas.addEventListener("mouseleave", function () {
      hoverIndex = -1;
    });
  }

  // ---- History loader ----
  function loadHistory(range) {
    var points = CHART_POINTS;
    return fetch("/api/history?range=" + (range || "5m") + "&points=" + points)
      .then(function (res) { return res.ok ? res.json() : null; })
      .then(function (data) {
        if (!data || !data.items) return;
        // Populate chart history from persisted data
        Object.keys(chartHistory).forEach(function(k) { chartHistory[k] = []; });
        data.items.forEach(function (it) {
          var fd = (it.drivers || {}).ferroamp || {};
          var sd = (it.drivers || {}).sungrow || {};
          var ft = (it.targets || {}).ferroamp || 0;
          var st = (it.targets || {}).sungrow || 0;
          var et = it.energy_today || {};
          chartHistory.grid.push(it.grid_w || 0);
          chartHistory.pv.push(it.pv_w || 0);
          chartHistory.load.push(it.load_w || 0);
          chartHistory.ferroamp_bat.push(fd.bat_w || 0);
          chartHistory.sungrow_bat.push(sd.bat_w || 0);
          chartHistory.ferroamp_target.push(ft);
          chartHistory.sungrow_target.push(st);
          chartHistory.timestamps.push(it.ts || 0);
          chartHistory.e_import.push(et.import_wh || 0);
          chartHistory.e_export.push(et.export_wh || 0);
          chartHistory.e_pv.push(et.pv_wh || 0);
          chartHistory.e_charged.push(et.bat_charged_wh || 0);
          chartHistory.e_discharged.push(et.bat_discharged_wh || 0);
          chartHistory.e_load.push(et.load_wh || 0);
        });
        renderChart();
      })
      .catch(function () { /* silent */ });
  }

  // ---- Animation loop ----
  // Drives the chart at ~30fps — points scroll left smoothly as time advances,
  // pulse rings shimmer at the latest data point. Cards still update on poll.
  var lastFrame = 0;
  function animationFrame(ts) {
    if (animating) {
      // Throttle to ~30fps to keep CPU low — the visual feel is the same
      if (ts - lastFrame > 33) {
        renderChart();
        lastFrame = ts;
      }
    }
    requestAnimationFrame(animationFrame);
  }

  // Pause animation when tab is hidden (saves battery on background tabs)
  document.addEventListener("visibilitychange", function () {
    animating = !document.hidden;
  });

  // ---- Init ----
  loadHistory(chartRange);
  fetchStatus();
  setInterval(fetchStatus, POLL_INTERVAL);
  requestAnimationFrame(animationFrame);
})();
