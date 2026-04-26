// savings.js — historical savings ledger.
//
// Shows three things:
//   1. KPI row (today / 7 d / 30 d total saved vs no-battery baseline)
//   2. Daily bar chart — one bar per day, height = savings_ore. Click a
//      bar to drill in.
//   3. Drill-down panel — per-slot table for the selected day, including
//      flow decomposition (where each kWh came from / went to) and the
//      predicted-vs-actual overlay from planner_diagnostics.
//
// Backed by:
//   GET /api/savings/summary    → KPI numbers
//   GET /api/savings/daily?days → bar chart
//   GET /api/savings/intraday?date → drill-down rows

(function () {
  'use strict';

  const REFRESH_MS = 60 * 1000;

  const state = {
    summary: null,
    daily: [],
    selectedDate: null,
    intraday: null,
    range: 30, // days shown on the bar chart
  };

  const fmtSEK = (ore) => (ore / 100).toFixed(2) + ' SEK';
  const fmtSEKShort = (ore) => {
    const sek = ore / 100;
    if (Math.abs(sek) >= 1000) return (sek / 1000).toFixed(1) + ' k';
    if (Math.abs(sek) >= 100) return sek.toFixed(0);
    return sek.toFixed(1);
  };
  const fmtSEKSigned = (ore) => (ore >= 0 ? '+' : '−') + fmtSEK(Math.abs(ore));
  const fmtKWh = (kwh) => kwh.toFixed(2) + ' kWh';
  const fmtPct = (p) => (p * 100).toFixed(0) + ' %';

  async function fetchAll() {
    try {
      const [summary, daily] = await Promise.all([
        fetch('/api/savings/summary').then(r => r.json()).catch(() => ({})),
        fetch('/api/savings/daily?days=' + state.range).then(r => r.json()).catch(() => ({})),
      ]);
      state.summary = summary || null;
      state.daily = (daily && daily.days) || [];
      // Keep the selected date if it still appears in the new range,
      // otherwise default to the most recent day.
      const seen = new Set(state.daily.map(d => d.day));
      if (!state.selectedDate || !seen.has(state.selectedDate)) {
        state.selectedDate = state.daily.length > 0 ? state.daily[state.daily.length - 1].day : null;
      }
      if (state.selectedDate) {
        await loadIntraday(state.selectedDate);
      } else {
        state.intraday = null;
      }
    } catch (e) {
      // Silent — UI shows empty state on its own.
    }
    render();
  }

  async function loadIntraday(date) {
    try {
      const r = await fetch('/api/savings/intraday?date=' + encodeURIComponent(date));
      state.intraday = await r.json();
    } catch (e) {
      state.intraday = null;
    }
  }

  function setRange(days) {
    state.range = days;
    fetchAll();
  }

  // ---- Renderers ----

  function renderKPIs() {
    const el = document.getElementById('savings-kpis');
    if (!el) return;
    const s = state.summary;
    if (!s || !s.enabled) {
      const reason = (s && s.reason) || 'savings not available — configure spot prices first';
      el.innerHTML = `<div class="savings-empty">${reason}</div>`;
      return;
    }
    const cls = (v) => v >= 0 ? 'saving-pos' : 'saving-neg';
    const card = (label, ore, hint) =>
      `<div class="card savings-kpi" title="${hint}">` +
      `  <div class="card-label">${label}</div>` +
      `  <div class="card-value ${cls(ore)}">${fmtSEKSigned(ore)}</div>` +
      `</div>`;
    el.innerHTML =
      card('Today', s.today_ore || 0,
        'Savings so far today vs running with no battery — recomputed on every page load.') +
      card('Last 7 days', s.last_7_days_ore || 0,
        'Sum of daily savings over the last 7 days (today included).') +
      card('Last 30 days', s.last_30_days_ore || 0,
        'Sum of daily savings over the last 30 days (today included).');
  }

  function renderRangeButtons() {
    const wrap = document.getElementById('savings-range');
    if (!wrap) return;
    wrap.querySelectorAll('button').forEach(b => {
      b.classList.toggle('active', Number(b.dataset.days) === state.range);
    });
  }

  // Daily bar chart. Two stacked rectangles per day:
  //   bottom (light) = no-battery cost
  //   on top (dark)  = actual cost
  // Saved area is the gap. Hovering / clicking selects the day.
  function renderDailyChart() {
    const canvas = document.getElementById('savings-chart');
    if (!canvas) return;
    const cssW = canvas.clientWidth || 800;
    const cssH = 220;
    const dpr = window.devicePixelRatio || 1;
    canvas.width = cssW * dpr;
    canvas.height = cssH * dpr;
    canvas.style.height = cssH + 'px';
    const ctx = canvas.getContext('2d');
    ctx.setTransform(dpr, 0, 0, dpr, 0, 0);
    ctx.clearRect(0, 0, cssW, cssH);

    const pad = { l: 50, r: 12, t: 12, b: 28 };
    const plotW = cssW - pad.l - pad.r;
    const plotH = cssH - pad.t - pad.b;

    if (state.daily.length === 0) {
      ctx.fillStyle = 'rgba(255,255,255,0.45)';
      ctx.font = '12px var(--mono, monospace)';
      ctx.textAlign = 'center';
      ctx.fillText('No priced days yet — savings appear once spot prices have been fetched.',
        cssW / 2, cssH / 2);
      state.barBounds = [];
      return;
    }

    // y-scale: max of (no_battery_ore, actual_ore) across days.
    let yMax = 0;
    for (const d of state.daily) {
      yMax = Math.max(yMax, d.no_battery_ore || 0, d.actual_ore || 0);
    }
    if (yMax <= 0) yMax = 1;

    // Headroom + nice round step for ticks
    const niceMax = Math.ceil(yMax / 100) * 100;
    const yScale = (v) => pad.t + plotH - (v / niceMax) * plotH;

    // Y-axis
    ctx.strokeStyle = 'rgba(255,255,255,0.08)';
    ctx.fillStyle = 'rgba(255,255,255,0.45)';
    ctx.font = '10px var(--mono, monospace)';
    ctx.textAlign = 'right';
    const ticks = 4;
    for (let i = 0; i <= ticks; i++) {
      const v = (niceMax / ticks) * i;
      const y = yScale(v);
      ctx.beginPath();
      ctx.moveTo(pad.l, y);
      ctx.lineTo(cssW - pad.r, y);
      ctx.stroke();
      ctx.fillText(fmtSEKShort(v), pad.l - 4, y + 3);
    }

    const n = state.daily.length;
    const barSlot = plotW / n;
    const barW = Math.max(2, Math.min(20, barSlot * 0.7));
    const bounds = [];
    for (let i = 0; i < n; i++) {
      const d = state.daily[i];
      const cx = pad.l + barSlot * i + barSlot / 2;
      const x0 = cx - barW / 2;
      const yNoBat = yScale(d.no_battery_ore || 0);
      const yActual = yScale(d.actual_ore || 0);
      const y0 = Math.min(yNoBat, yActual);
      const yBase = pad.t + plotH;

      // No-battery rectangle (full height)
      ctx.fillStyle = 'rgba(239,68,68,0.32)';
      ctx.fillRect(x0, yNoBat, barW, yBase - yNoBat);
      // Actual rectangle (smaller — overlays the bottom of the no-bat)
      ctx.fillStyle = 'rgba(34,197,94,0.78)';
      ctx.fillRect(x0, yActual, barW, yBase - yActual);

      // Selection highlight
      if (d.day === state.selectedDate) {
        ctx.strokeStyle = '#60a5fa';
        ctx.lineWidth = 2;
        ctx.strokeRect(x0 - 1, pad.t - 2, barW + 2, plotH + 4);
        ctx.lineWidth = 1;
      }

      bounds.push({ x0, x1: x0 + barW, day: d.day, data: d });

      // Date tick on every Nth bar so labels don't collide
      const everyN = Math.max(1, Math.ceil(n / 10));
      if (i % everyN === 0 || i === n - 1) {
        ctx.fillStyle = 'rgba(255,255,255,0.5)';
        ctx.textAlign = 'center';
        ctx.fillText(d.day.slice(5), cx, cssH - pad.b + 14);
      }
    }
    state.barBounds = bounds;
  }

  function renderDrillDown() {
    const head = document.getElementById('savings-drill-head');
    const body = document.getElementById('savings-drill-body');
    if (!head || !body) return;

    const i = state.intraday;
    if (!state.selectedDate) {
      head.textContent = 'Click a day to inspect it';
      body.innerHTML = '';
      return;
    }
    if (!i || !i.enabled) {
      head.textContent = state.selectedDate;
      body.innerHTML = '<div class="savings-empty">No data for this day yet.</div>';
      return;
    }

    const slots = i.slots || [];
    const sek = (ore) => (ore / 100).toFixed(2);

    const headParts = [
      `<span class="drill-day">${i.day}</span>`,
      `<span class="drill-stat">Saved <b>${fmtSEKSigned(i.savings_ore)}</b></span>`,
      `<span class="drill-stat">Actual <b>${sek(i.actual_ore)} SEK</b></span>`,
      `<span class="drill-stat">No battery <b>${sek(i.no_battery_ore)} SEK</b></span>`,
    ];
    if (typeof i.coverage_pct === 'number' && i.coverage_pct < 0.95) {
      headParts.push(`<span class="drill-stat drill-warn" title="Less than 95% of the day's slot-time had data — slots with gaps are excluded from totals">Coverage ${fmtPct(i.coverage_pct)}</span>`);
    }
    head.innerHTML = headParts.join(' · ');

    if (slots.length === 0) {
      body.innerHTML = '<div class="savings-empty">No priced slots for this day.</div>';
      return;
    }

    const rows = slots.map(s => {
      const t = new Date(s.start_ms);
      const hh = t.getHours().toString().padStart(2, '0') + ':' +
                 t.getMinutes().toString().padStart(2, '0');
      const cls = s.savings_ore >= 0 ? 'saving-pos' : 'saving-neg';
      const flows = s.flows || {};
      // Compact flow tags: only show non-zero contributions, ordered by magnitude.
      const tags = [];
      const push = (label, kwh, color) => {
        if (Math.abs(kwh) < 0.01) return;
        tags.push({ label, kwh, color });
      };
      push('self', flows.self_consumption_kwh, '#22c55e');
      push('PV→bat', flows.pv_to_bat_kwh, '#86efac');
      push('bat→home', flows.bat_to_home_kwh, '#8b5cf6');
      push('grid→bat', flows.grid_to_bat_kwh, '#f59e0b');
      push('bat→grid', flows.bat_to_grid_kwh, '#a78bfa');
      push('export', flows.direct_export_kwh, '#34d399');
      push('grid→home', flows.grid_to_home_kwh, '#ef4444');
      tags.sort((a, b) => b.kwh - a.kwh);
      const tagsHtml = tags.map(t =>
        `<span class="flow-tag" style="border-color:${t.color}33;color:${t.color}" title="${t.label} ${t.kwh.toFixed(2)} kWh">` +
        `${t.label} ${t.kwh.toFixed(2)}</span>`
      ).join('');

      let predictedCell = '';
      if (typeof s.predicted_ore === 'number') {
        const errCls = Math.abs(s.prediction_error_ore) < 1 ? '' :
          (s.prediction_error_ore > 0 ? 'saving-neg' : 'saving-pos');
        predictedCell = `<td class="num">${sek(s.predicted_ore)}</td>` +
          `<td class="num ${errCls}" title="actual − predicted (öre)">${sek(s.prediction_error_ore)}</td>`;
      } else {
        predictedCell = '<td class="num drill-dim">—</td><td class="num drill-dim">—</td>';
      }

      return `<tr>
        <td>${hh}</td>
        <td class="num">${(s.price_ore || 0).toFixed(0)}</td>
        <td class="num">${(s.load_kwh || 0).toFixed(2)}</td>
        <td class="num">${(s.pv_kwh || 0).toFixed(2)}</td>
        <td class="num">${(s.grid_import_kwh || 0).toFixed(2)}</td>
        <td class="num">${(s.grid_export_kwh || 0).toFixed(2)}</td>
        <td class="num">${(s.bat_charged_kwh || 0).toFixed(2)}</td>
        <td class="num">${(s.bat_discharged_kwh || 0).toFixed(2)}</td>
        <td class="num">${sek(s.actual_ore)}</td>
        <td class="num">${sek(s.no_battery_ore)}</td>
        <td class="num ${cls}"><b>${fmtSEKSigned(s.savings_ore)}</b></td>
        ${predictedCell}
        <td class="flow-cell">${tagsHtml}</td>
      </tr>`;
    });

    body.innerHTML = `
      <table class="savings-table">
        <thead>
          <tr>
            <th>Time</th>
            <th>öre/kWh</th>
            <th>Load</th>
            <th>PV</th>
            <th>Grid in</th>
            <th>Grid out</th>
            <th>Bat ch</th>
            <th>Bat dis</th>
            <th>Actual</th>
            <th>No bat</th>
            <th>Saved</th>
            <th title="Cost the MPC plan predicted for this slot at the time it ran">MPC said</th>
            <th title="Actual − predicted; positive = paid more than the plan expected">Δ</th>
            <th>Flows (kWh)</th>
          </tr>
        </thead>
        <tbody>${rows.join('')}</tbody>
      </table>`;
  }

  function render() {
    renderKPIs();
    renderRangeButtons();
    renderDailyChart();
    renderDrillDown();
  }

  // ---- Wiring ----

  function setupChartHover() {
    const canvas = document.getElementById('savings-chart');
    if (!canvas) return;
    let tip = document.getElementById('savings-tip');
    if (!tip) {
      tip = document.createElement('div');
      tip.id = 'savings-tip';
      tip.className = 'savings-tip';
      tip.style.display = 'none';
      document.body.appendChild(tip);
    }
    canvas.addEventListener('mousemove', (e) => {
      if (!state.barBounds || state.barBounds.length === 0) {
        tip.style.display = 'none';
        return;
      }
      const rect = canvas.getBoundingClientRect();
      const cx = e.clientX - rect.left;
      let found = null;
      for (const b of state.barBounds) {
        if (cx >= b.x0 && cx <= b.x1) { found = b; break; }
      }
      if (!found) { tip.style.display = 'none'; return; }
      const d = found.data;
      tip.innerHTML =
        `<div class="tip-day">${d.day}</div>` +
        `<div>Saved: <b>${fmtSEKSigned(d.savings_ore)}</b></div>` +
        `<div>Actual: ${fmtSEK(d.actual_ore)}</div>` +
        `<div>No battery: ${fmtSEK(d.no_battery_ore)}</div>` +
        (d.coverage_pct < 0.95 ? `<div class="tip-warn">Coverage ${fmtPct(d.coverage_pct)}</div>` : '');
      tip.style.display = 'block';
      tip.style.left = (e.clientX + 12) + 'px';
      tip.style.top = (e.clientY + 12) + 'px';
    });
    canvas.addEventListener('mouseleave', () => { tip.style.display = 'none'; });
    canvas.addEventListener('click', (e) => {
      if (!state.barBounds) return;
      const rect = canvas.getBoundingClientRect();
      const cx = e.clientX - rect.left;
      for (const b of state.barBounds) {
        if (cx >= b.x0 && cx <= b.x1) {
          state.selectedDate = b.day;
          loadIntraday(b.day).then(render);
          return;
        }
      }
    });
  }

  function setupRangeButtons() {
    const wrap = document.getElementById('savings-range');
    if (!wrap) return;
    wrap.addEventListener('click', (e) => {
      const b = e.target.closest('button[data-days]');
      if (!b) return;
      setRange(Number(b.dataset.days));
    });
  }

  // ---- Lifecycle ----

  let timer = null;
  let started = false;

  function start() {
    if (started) return;
    const view = document.getElementById('view-savings');
    if (!view) return;
    started = true;
    setupChartHover();
    setupRangeButtons();
    fetchAll();
    timer = setInterval(() => {
      // Only refresh if the view is actually visible — saves cycles +
      // avoids a request burst on slow links when the operator never
      // opens the tab.
      if (view.classList.contains('hidden')) return;
      fetchAll();
    }, REFRESH_MS);
  }

  // Boot when the savings view becomes visible (or on hash arrival).
  function maybeStart() {
    const view = document.getElementById('view-savings');
    if (view && !view.classList.contains('hidden')) start();
  }
  window.addEventListener('hashchange', maybeStart);
  document.addEventListener('DOMContentLoaded', maybeStart);
  // If the document is already parsed when this script runs (the
  // common case — we're loaded at the bottom of <body>), check once
  // synchronously so navigating directly to #savings works on first
  // load without a hash event.
  if (document.readyState !== 'loading') maybeStart();
})();
