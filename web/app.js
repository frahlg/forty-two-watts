// forty-two-watts dashboard — plain JS, no framework

(function () {
  "use strict";

  const POLL_INTERVAL = 5000;
  const CHART_POINTS = 60; // 5 min at 5s intervals
  let currentMode = null;

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
  };
  // Last render layout, needed for hover tooltip
  var chartLayout = null;
  var hoverIndex = -1;

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
  const lastUpdate = $("last-update");
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

  function statusClass(status) {
    if (!status) return "status-offline";
    const s = status.toLowerCase();
    if (s === "ok") return "status-ok";
    if (s === "degraded") return "status-degraded";
    return "status-offline";
  }

  // ---- Render ----
  function render(data) {
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

    // Chart — per-driver battery actual + dispatch targets
    var fd = data.drivers.ferroamp || {};
    var sd = data.drivers.sungrow || {};
    var ft = 0, st = 0;
    (data.dispatch || []).forEach(function(d) {
      if (d.driver === "ferroamp") ft = d.target_w;
      if (d.driver === "sungrow") st = d.target_w;
    });
    pushChartData(data.grid_w, data.pv_w, data.load_w||0, fd.bat_w||0, sd.bat_w||0, ft, st);
    renderChart();

    // Timestamp
    lastUpdate.textContent =
      "Last update: " + new Date().toLocaleTimeString();
  }

  function pushChartData(grid, pv, load, ferroBat, sunBat, ferroTarget, sunTarget) {
    chartHistory.grid.push(grid);
    chartHistory.pv.push(pv);
    chartHistory.load.push(load);
    chartHistory.ferroamp_bat.push(ferroBat);
    chartHistory.sungrow_bat.push(sunBat);
    chartHistory.ferroamp_target.push(ferroTarget);
    chartHistory.sungrow_target.push(sunTarget);
    chartHistory.timestamps.push(Date.now());
    if (chartHistory.grid.length > CHART_POINTS) {
      chartHistory.grid.shift();
      chartHistory.pv.shift();
      chartHistory.load.shift();
      chartHistory.ferroamp_bat.shift();
      chartHistory.sungrow_bat.shift();
      chartHistory.ferroamp_target.shift();
      chartHistory.sungrow_target.shift();
      chartHistory.timestamps.shift();
    }
  }

  function renderChart() {
    var canvas = document.getElementById("power-chart");
    if (!canvas) return;
    var ctx = canvas.getContext("2d");
    var w = canvas.parentElement.clientWidth - 32;
    var h = 250;
    canvas.width = w;
    canvas.height = h;

    var pad = { top: 20, right: 10, bottom: 25, left: 55 };
    var plotW = w - pad.left - pad.right;
    var plotH = h - pad.top - pad.bottom;

    // Find y range across all series
    var all = chartHistory.grid.concat(chartHistory.pv).concat(chartHistory.load)
      .concat(chartHistory.ferroamp_bat).concat(chartHistory.sungrow_bat)
      .concat(chartHistory.ferroamp_target).concat(chartHistory.sungrow_target);
    if (all.length === 0) return;
    var yMin = Math.min(0, Math.min.apply(null, all));
    var yMax = Math.max(100, Math.max.apply(null, all));
    var yRange = yMax - yMin || 1;
    // Add 10% padding
    yMin -= yRange * 0.1;
    yMax += yRange * 0.1;
    yRange = yMax - yMin;

    ctx.clearRect(0, 0, w, h);

    // Grid lines
    ctx.strokeStyle = "#333";
    ctx.lineWidth = 0.5;
    ctx.font = "11px monospace";
    ctx.fillStyle = "#888";
    var steps = 5;
    for (var i = 0; i <= steps; i++) {
      var yVal = yMin + (yRange * i / steps);
      var y = pad.top + plotH - (plotH * i / steps);
      ctx.beginPath();
      ctx.moveTo(pad.left, y);
      ctx.lineTo(w - pad.right, y);
      ctx.stroke();
      ctx.fillText(formatW(yVal), 2, y + 4);
    }

    // Zero line
    if (yMin < 0 && yMax > 0) {
      var zeroY = pad.top + plotH * (1 - (0 - yMin) / yRange);
      ctx.strokeStyle = "#555";
      ctx.lineWidth = 1;
      ctx.setLineDash([4, 4]);
      ctx.beginPath();
      ctx.moveTo(pad.left, zeroY);
      ctx.lineTo(w - pad.right, zeroY);
      ctx.stroke();
      ctx.setLineDash([]);
    }

    // Draw series: solid = actual, dashed = target
    var series = [
      { data: chartHistory.grid,            color: "#ef4444", width: 2, dash: [] },
      { data: chartHistory.pv,              color: "#22c55e", width: 2, dash: [] },
      { data: chartHistory.load,            color: "#e2e8f0", width: 1.5, dash: [] },
      { data: chartHistory.ferroamp_bat,    color: "#f59e0b", width: 2, dash: [] },       // amber solid
      { data: chartHistory.ferroamp_target, color: "#f59e0b", width: 1.5, dash: [6, 4] }, // amber dashed
      { data: chartHistory.sungrow_bat,     color: "#8b5cf6", width: 2, dash: [] },       // purple solid
      { data: chartHistory.sungrow_target,  color: "#8b5cf6", width: 1.5, dash: [6, 4] }, // purple dashed
    ];

    series.forEach(function (s) {
      if (s.data.length < 2) return;
      ctx.strokeStyle = s.color;
      ctx.lineWidth = s.width;
      ctx.setLineDash(s.dash);
      ctx.beginPath();
      for (var j = 0; j < s.data.length; j++) {
        var x = pad.left + (plotW * j / (CHART_POINTS - 1));
        var y = pad.top + plotH * (1 - (s.data[j] - yMin) / yRange);
        if (j === 0) ctx.moveTo(x, y);
        else ctx.lineTo(x, y);
      }
      ctx.stroke();
      ctx.setLineDash([]);
    });

    // Time labels
    ctx.fillStyle = "#666";
    ctx.fillText("5m ago", pad.left, h - 5);
    ctx.fillText("now", w - pad.right - 20, h - 5);

    // Store layout for hover tooltip
    chartLayout = {
      pad: pad, plotW: plotW, plotH: plotH, w: w, h: h,
      yMin: yMin, yMax: yMax, yRange: yRange,
      series: series,
      pointCount: Math.max.apply(null, series.map(function(s){ return s.data.length; }))
    };

    // Draw hover overlay if active
    if (hoverIndex >= 0 && hoverIndex < chartLayout.pointCount) {
      drawHoverOverlay(ctx);
    }
  }

  function drawHoverOverlay(ctx) {
    if (!chartLayout) return;
    var l = chartLayout;
    var i = hoverIndex;
    var x = l.pad.left + (l.plotW * i / Math.max(1, l.pointCount - 1));

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

    // Tooltip box
    var labels = [
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
      ctx.fillText(formatW(lab.data[i]), boxX + boxW - 6, y);
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
        // Isolate render errors from connection state
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
        loadHistory(e.target.dataset.range);
      }
    });
  }

  // ---- Chart hover ----
  var canvas = document.getElementById("power-chart");
  if (canvas) {
    canvas.addEventListener("mousemove", function (e) {
      if (!chartLayout) return;
      var rect = canvas.getBoundingClientRect();
      var scaleX = canvas.width / rect.width;
      var x = (e.clientX - rect.left) * scaleX;
      var l = chartLayout;
      if (x < l.pad.left || x > l.pad.left + l.plotW) {
        if (hoverIndex !== -1) { hoverIndex = -1; renderChart(); }
        return;
      }
      var relX = x - l.pad.left;
      var idx = Math.round(relX / l.plotW * (l.pointCount - 1));
      idx = Math.max(0, Math.min(l.pointCount - 1, idx));
      if (idx !== hoverIndex) {
        hoverIndex = idx;
        renderChart();
      }
    });
    canvas.addEventListener("mouseleave", function () {
      if (hoverIndex !== -1) { hoverIndex = -1; renderChart(); }
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
        chartHistory.grid = [];
        chartHistory.pv = [];
        chartHistory.load = [];
        chartHistory.ferroamp_bat = [];
        chartHistory.sungrow_bat = [];
        chartHistory.ferroamp_target = [];
        chartHistory.sungrow_target = [];
        chartHistory.timestamps = [];
        data.items.forEach(function (it) {
          var fd = (it.drivers || {}).ferroamp || {};
          var sd = (it.drivers || {}).sungrow || {};
          var ft = (it.targets || {}).ferroamp || 0;
          var st = (it.targets || {}).sungrow || 0;
          chartHistory.grid.push(it.grid_w || 0);
          chartHistory.pv.push(it.pv_w || 0);
          chartHistory.load.push(it.load_w || 0);
          chartHistory.ferroamp_bat.push(fd.bat_w || 0);
          chartHistory.sungrow_bat.push(sd.bat_w || 0);
          chartHistory.ferroamp_target.push(ft);
          chartHistory.sungrow_target.push(st);
          chartHistory.timestamps.push(it.ts || 0);
        });
        renderChart();
      })
      .catch(function () { /* silent */ });
  }

  // ---- Init ----
  loadHistory("5m");
  fetchStatus();
  setInterval(fetchStatus, POLL_INTERVAL);
})();
