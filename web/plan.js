// plan.js — MPC plan + prices + forecast visualization.
// Renders a stacked canvas chart: price bars on top, battery+grid bars in
// the middle, SoC + PV line on bottom. Refreshes every 30s.

(function () {
  'use strict';

  const PLAN_REFRESH_MS = 30000;

  const state = {
    prices: null,
    forecast: null,
    plan: null,
    lastUpdate: null,
  };

  async function fetchAll() {
    const [p, f, m] = await Promise.all([
      fetch('/api/prices').then(r => r.json()).catch(() => ({})),
      fetch('/api/forecast').then(r => r.json()).catch(() => ({})),
      fetch('/api/mpc/plan').then(r => r.json()).catch(() => ({})),
    ]);
    state.prices = (p && p.items) || [];
    state.forecast = (f && f.items) || [];
    state.plan = (m && m.plan) || null;
    state.enabled = {
      prices: p && p.enabled,
      forecast: f && f.enabled,
      mpc: m && m.enabled,
    };
    state.lastUpdate = new Date();
    render();
  }

  async function replan() {
    try {
      const r = await fetch('/api/mpc/replan', { method: 'POST' });
      const j = await r.json();
      if (j && j.plan) state.plan = j.plan;
      render();
    } catch (e) { /* ignore */ }
  }

  function fmtHHMM(ts) {
    const d = new Date(ts);
    return d.getHours().toString().padStart(2, '0') + ':' +
           d.getMinutes().toString().padStart(2, '0');
  }

  function render() {
    const canvas = document.getElementById('plan-chart');
    if (!canvas) return;
    const ctx = canvas.getContext('2d');
    const cssW = canvas.clientWidth || 800;
    const cssH = 260;
    const dpr = window.devicePixelRatio || 1;
    canvas.width = cssW * dpr;
    canvas.height = cssH * dpr;
    canvas.style.height = cssH + 'px';
    ctx.setTransform(dpr, 0, 0, dpr, 0, 0);
    ctx.clearRect(0, 0, cssW, cssH);

    const pad = { l: 44, r: 44, t: 16, b: 28 };
    const plotW = cssW - pad.l - pad.r;
    const plotH = cssH - pad.t - pad.b;

    // X range = now → +24h
    const now = Date.now();
    const tMin = now - 30 * 60 * 1000; // 30 min look-back so in-progress slot is visible
    const tMax = now + 24 * 60 * 60 * 1000;
    const xScale = t => pad.l + (t - tMin) / (tMax - tMin) * plotW;

    // Price range
    const prices = (state.prices || []).filter(p => p.slot_ts_ms >= tMin && p.slot_ts_ms <= tMax);
    const totals = prices.map(p => p.total_ore_kwh);
    const priceMin = totals.length ? Math.min(0, ...totals) : 0;
    const priceMax = totals.length ? Math.max(...totals, 1) : 200;
    const priceRange = priceMax - priceMin;

    // Price band on top third
    const priceY0 = pad.t;
    const priceH = plotH * 0.32;
    const priceY = v => priceY0 + priceH - (v - priceMin) / priceRange * priceH;

    // Power band in middle — covers battery + grid
    const powerY0 = priceY0 + priceH + 10;
    const powerH = plotH * 0.42;
    // Scale based on plan battery + PV magnitudes
    const plan = state.plan;
    let pMagMax = 1000;
    if (plan && plan.actions) {
      for (const a of plan.actions) {
        pMagMax = Math.max(pMagMax, Math.abs(a.battery_w), Math.abs(a.grid_w), Math.abs(a.pv_w));
      }
    } else {
      for (const f of state.forecast || []) {
        if (f.pv_w_estimated) pMagMax = Math.max(pMagMax, f.pv_w_estimated);
      }
    }
    const powerYCenter = powerY0 + powerH / 2;
    const powerY = w => powerYCenter - (w / pMagMax) * (powerH / 2);

    // SoC line on bottom band
    const socY0 = powerY0 + powerH + 4;
    const socH = plotH * 0.18;
    const socY = p => socY0 + socH - (p / 100) * socH;

    // ---- Grid ticks (hours) ----
    ctx.strokeStyle = 'rgba(255,255,255,0.08)';
    ctx.lineWidth = 1;
    ctx.fillStyle = 'rgba(255,255,255,0.45)';
    ctx.font = '11px system-ui, sans-serif';
    ctx.textAlign = 'center';
    for (let h = 0; h <= 24; h += 3) {
      const t = now + h * 3600 * 1000;
      const x = xScale(t);
      ctx.beginPath();
      ctx.moveTo(x, pad.t);
      ctx.lineTo(x, pad.t + plotH);
      ctx.stroke();
      ctx.fillText(fmtHHMM(t), x, cssH - 10);
    }
    // Now-line
    const xNow = xScale(now);
    ctx.strokeStyle = '#ef4444';
    ctx.lineWidth = 1.2;
    ctx.setLineDash([3, 3]);
    ctx.beginPath();
    ctx.moveTo(xNow, pad.t);
    ctx.lineTo(xNow, pad.t + plotH);
    ctx.stroke();
    ctx.setLineDash([]);

    // ---- Price bars ----
    // Color cheap (low) → green, expensive → red.
    const sortedTotals = [...totals].sort((a, b) => a - b);
    // Price bars: use plan actions when available (covers full horizon
    // including ML-forecasted slots). Confidence < 1 → reduced alpha +
    // dashed top outline so it's obvious which slots are predicted.
    const p25 = sortedTotals[Math.floor(sortedTotals.length * 0.25)] || priceMin;
    const p75 = sortedTotals[Math.floor(sortedTotals.length * 0.75)] || priceMax;
    state.priceBarBounds = []; // {x0,x1,yMinPx,yMaxPx, action} for hover hit-test
    const barSource = (plan && plan.actions && plan.actions.length) ? plan.actions : prices;
    for (const bar of barSource) {
      const ts = bar.slot_ts_ms ?? bar.slot_start_ms;
      const len = bar.slot_len_min;
      const priceVal = bar.total_ore_kwh ?? bar.price_ore;
      if (ts == null || priceVal == null) continue;
      if (ts + len * 60 * 1000 < tMin || ts > tMax) continue;
      const x0 = xScale(ts);
      const x1 = xScale(ts + len * 60 * 1000);
      const y = priceY(priceVal);
      const zero = priceY(Math.max(0, priceMin));
      const isPredicted = bar.confidence != null && bar.confidence < 1.0;
      const alpha = isPredicted ? 0.28 : 0.55;
      let color;
      if (priceVal <= p25) color = `rgba(34,197,94,${alpha})`;
      else if (priceVal >= p75) color = `rgba(239,68,68,${alpha})`;
      else color = `rgba(148,163,184,${alpha * 0.85})`;
      ctx.fillStyle = color;
      ctx.fillRect(x0, Math.min(y, zero), Math.max(1, x1 - x0 - 1), Math.abs(y - zero));
      // Dashed top outline on predicted bars so the style reads as "ML"
      if (isPredicted) {
        ctx.strokeStyle = 'rgba(255,255,255,0.25)';
        ctx.lineWidth = 1;
        ctx.setLineDash([2, 2]);
        ctx.beginPath();
        ctx.moveTo(x0, y);
        ctx.lineTo(x1 - 1, y);
        ctx.stroke();
        ctx.setLineDash([]);
      }
      // Track for hover hit-test
      state.priceBarBounds.push({
        x0: x0, x1: x1,
        ts: ts, len: len,
        action: bar, // either PricePoint or Action
      });
    }
    // Price axis labels
    ctx.fillStyle = 'rgba(255,255,255,0.55)';
    ctx.textAlign = 'right';
    ctx.fillText(priceMax.toFixed(0) + ' öre', pad.l - 6, priceY0 + 10);
    ctx.fillText(priceMin.toFixed(0), pad.l - 6, priceY0 + priceH);
    ctx.textAlign = 'left';
    ctx.fillText('Price', pad.l + 4, priceY0 + 12);

    // ---- Forecast PV line (negative = generation, site sign) ----
    ctx.strokeStyle = 'rgba(34,197,94,0.9)';
    ctx.lineWidth = 2;
    ctx.beginPath();
    let first = true;
    for (const f of state.forecast || []) {
      if (f.slot_ts_ms > tMax || !f.pv_w_estimated) continue;
      const x = xScale(f.slot_ts_ms);
      const y = powerY(-f.pv_w_estimated); // site sign
      if (first) { ctx.moveTo(x, y); first = false; }
      else ctx.lineTo(x, y);
    }
    ctx.stroke();

    // Load forecast from the plan's per-slot predictions (twin-driven).
    // Rendered above the PV curve as a pale-yellow dashed line so we can
    // see what the optimizer expects the house to consume each slot.
    if (plan && plan.actions && plan.actions.length) {
      ctx.strokeStyle = '#fde68a';
      ctx.lineWidth = 1.8;
      ctx.setLineDash([4, 5]);
      ctx.beginPath();
      let f2 = true;
      for (const a of plan.actions) {
        if (a.slot_start_ms > tMax) break;
        if (a.load_w == null) continue;
        const x = xScale(a.slot_start_ms);
        const y = powerY(a.load_w);
        if (f2) { ctx.moveTo(x, y); f2 = false; }
        else ctx.lineTo(x, y);
      }
      ctx.stroke();
      ctx.setLineDash([]);
    }

    // Power zero-line
    ctx.strokeStyle = 'rgba(255,255,255,0.25)';
    ctx.lineWidth = 1;
    ctx.beginPath();
    ctx.moveTo(pad.l, powerYCenter);
    ctx.lineTo(pad.l + plotW, powerYCenter);
    ctx.stroke();
    ctx.fillStyle = 'rgba(255,255,255,0.55)';
    ctx.textAlign = 'right';
    ctx.fillText('+' + (pMagMax / 1000).toFixed(1) + 'kW', pad.l - 6, powerY(pMagMax) + 4);
    ctx.fillText((-pMagMax / 1000).toFixed(1) + 'kW', pad.l - 6, powerY(-pMagMax) + 4);
    ctx.textAlign = 'left';
    ctx.fillText('Power', pad.l + 4, powerY0 + 12);

    // ---- Plan battery bars ----
    if (plan && plan.actions) {
      for (const a of plan.actions) {
        if (a.slot_start_ms > tMax) break;
        const x0 = xScale(a.slot_start_ms);
        const x1 = xScale(a.slot_start_ms + a.slot_len_min * 60 * 1000);
        const y = powerY(a.battery_w);
        const color = a.battery_w >= 0 ? 'rgba(245,158,11,0.65)' : 'rgba(139,92,246,0.65)';
        ctx.fillStyle = color;
        ctx.fillRect(x0, Math.min(y, powerYCenter), Math.max(1, x1 - x0 - 1), Math.abs(y - powerYCenter));
      }
      // SoC line
      ctx.strokeStyle = 'rgba(96,165,250,0.95)';
      ctx.lineWidth = 2;
      ctx.beginPath();
      first = true;
      // Anchor at start SoC at now
      if (plan.initial_soc_pct != null) {
        ctx.moveTo(xScale(now), socY(plan.initial_soc_pct));
        first = false;
      }
      for (const a of plan.actions) {
        if (a.slot_start_ms > tMax) break;
        const x = xScale(a.slot_start_ms + a.slot_len_min * 60 * 1000);
        const y = socY(a.soc_pct);
        if (first) { ctx.moveTo(x, y); first = false; }
        else ctx.lineTo(x, y);
      }
      ctx.stroke();
      ctx.fillStyle = 'rgba(255,255,255,0.55)';
      ctx.textAlign = 'right';
      ctx.fillText('100%', cssW - pad.r + 40, socY(100) + 4);
      ctx.fillText('0%', cssW - pad.r + 40, socY(0) + 4);
      ctx.textAlign = 'left';
      ctx.fillText('SoC', pad.l + 4, socY0 + 12);
    }

    // ---- Summary ----
    const summary = document.getElementById('plan-summary');
    if (summary) {
      if (!state.enabled || !state.enabled.mpc) {
        summary.textContent = 'MPC planner disabled';
      } else if (!plan) {
        summary.textContent = state.prices && state.prices.length
          ? 'Waiting for first plan…'
          : 'Waiting for price data…';
      } else {
        const hh = plan.horizon_slots * (plan.actions[0] ? plan.actions[0].slot_len_min : 15) / 60;
        const cost = plan.total_cost_ore / 100;
        summary.textContent =
          `${plan.mode} · ${hh.toFixed(0)}h horizon · ${plan.horizon_slots} slots · ` +
          `SoC ${plan.initial_soc_pct.toFixed(0)}% → plan cost ${cost.toFixed(2)} SEK`;
      }
    }
  }

  // Hover tooltip: hit-tests the x-coordinate against the cached
  // priceBarBounds, pops a floating panel with slot details.
  function setupHover() {
    const canvas = document.getElementById('plan-chart');
    let tip = document.getElementById('plan-tip');
    if (!tip) {
      tip = document.createElement('div');
      tip.id = 'plan-tip';
      tip.className = 'plan-tip';
      tip.style.display = 'none';
      document.body.appendChild(tip);
    }
    if (!canvas) return;
    canvas.addEventListener('mousemove', function (e) {
      if (!state.priceBarBounds || state.priceBarBounds.length === 0) {
        tip.style.display = 'none';
        return;
      }
      const rect = canvas.getBoundingClientRect();
      const cx = e.clientX - rect.left;
      let found = null;
      for (const b of state.priceBarBounds) {
        if (cx >= b.x0 && cx <= b.x1) { found = b; break; }
      }
      if (!found) { tip.style.display = 'none'; return; }
      const a = found.action;
      const d = new Date(found.ts);
      const hh = d.getHours().toString().padStart(2, '0') + ':' + d.getMinutes().toString().padStart(2, '0');
      const dayStr = d.toLocaleDateString(undefined, { weekday: 'short' });
      const predicted = a.confidence != null && a.confidence < 1.0;
      const price = a.total_ore_kwh ?? a.price_ore;
      const lines = [
        `<div class="tip-head">${dayStr} ${hh}${predicted ? ' <span class="tip-pred">predicted</span>' : ''}</div>`,
        `<div class="tip-row"><span>Price</span><b>${price.toFixed(1)} öre/kWh</b></div>`,
      ];
      if (a.pv_w != null) lines.push(`<div class="tip-row"><span>PV</span><b>${(a.pv_w / 1000).toFixed(1)} kW</b></div>`);
      if (a.load_w != null) lines.push(`<div class="tip-row"><span>Load</span><b>${(a.load_w / 1000).toFixed(1)} kW</b></div>`);
      if (a.battery_w != null) {
        const dir = a.battery_w > 100 ? 'charge' : a.battery_w < -100 ? 'discharge' : 'idle';
        lines.push(`<div class="tip-row"><span>Battery</span><b>${(a.battery_w / 1000).toFixed(1)} kW (${dir})</b></div>`);
      }
      if (a.grid_w != null) {
        const gdir = a.grid_w > 0 ? 'import' : 'export';
        lines.push(`<div class="tip-row"><span>Grid</span><b>${(Math.abs(a.grid_w) / 1000).toFixed(1)} kW ${gdir}</b></div>`);
      }
      if (a.soc_pct != null) lines.push(`<div class="tip-row"><span>SoC (end)</span><b>${a.soc_pct.toFixed(0)}%</b></div>`);
      if (a.reason) lines.push(`<div class="tip-reason">${a.reason}</div>`);
      tip.innerHTML = lines.join('');
      tip.style.left = (e.clientX + 14) + 'px';
      tip.style.top = (e.clientY + 14) + 'px';
      tip.style.display = 'block';
    });
    canvas.addEventListener('mouseleave', function () { tip.style.display = 'none'; });
  }

  // Strategy explanation — surfaces one-sentence logic for the current mode.
  const STRATEGY_DESC = {
    planner_self: 'Self-consumption. Battery only covers local load or absorbs PV surplus. Never imports to charge, never exports via the battery. Safest mode.',
    planner_cheap: 'Cheap charging. Plans to import during the cheapest upcoming hours to top up the battery, still never exports via the battery. Good when export tariffs are low.',
    planner_arbitrage: 'Arbitrage. Full freedom: charges in the cheapest slots, discharges into the most expensive slots (including exporting). Biggest savings on volatile days; pays attention to battery efficiency + SoC bounds.',
    self_consumption: 'Manual self-consumption. Simple PI tracks grid-target = 0; no planning.',
    peak_shaving: 'Manual peak shaving. Limits grid import to the peak-limit setting.',
    charge: 'Manual full charge — forces the battery to charge regardless of price.',
    idle: 'Battery idle — no dispatch.',
  };
  function renderStrategyHint() {
    fetch('/api/status')
      .then(function (r) { return r.json(); })
      .then(function (d) {
        const el = document.getElementById('strategy-hint');
        if (!el) return;
        el.textContent = STRATEGY_DESC[d.mode] || '';
      })
      .catch(function () {});
  }

  function init() {
    fetchAll();
    setupHover();
    renderStrategyHint();
    setInterval(fetchAll, PLAN_REFRESH_MS);
    setInterval(renderStrategyHint, 5000);
    window.addEventListener('resize', render);
    const btn = document.getElementById('plan-replan');
    if (btn) btn.addEventListener('click', replan);
  }

  if (document.readyState === 'loading') {
    document.addEventListener('DOMContentLoaded', init);
  } else {
    init();
  }
})();
