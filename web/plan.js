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
    fuse: null,         // { max_amps, phases, voltage } — drives the power y-axis
    lastUpdate: null,
  };

  async function fetchAll() {
    const [p, f, m, c] = await Promise.all([
      fetch('/api/prices').then(r => r.json()).catch(() => ({})),
      fetch('/api/forecast').then(r => r.json()).catch(() => ({})),
      fetch('/api/mpc/plan').then(r => r.json()).catch(() => ({})),
      fetch('/api/config').then(r => r.json()).catch(() => ({})),
    ]);
    state.prices = (p && p.items) || [];
    state.forecast = (f && f.items) || [];
    state.plan = (m && m.plan) || null;
    state.planMeta = (m && m.meta) || null;
    state.fuse = (c && c.fuse) || null;
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
    const cssH = 320;
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
    const tMax = now + 48 * 60 * 60 * 1000;
    const xScale = t => pad.l + (t - tMin) / (tMax - tMin) * plotW;

    // Layout: price bars (top) | mode band (thin strip) | power bars (middle) | SoC (bottom)
    const modeBandH = 10;

    // Price range
    const prices = (state.prices || []).filter(p => p.slot_ts_ms >= tMin && p.slot_ts_ms <= tMax);
    const totals = prices.map(p => p.total_ore_kwh);
    const priceMin = totals.length ? Math.min(0, ...totals) : 0;
    const priceMax = totals.length ? Math.max(...totals, 1) : 200;
    const priceRange = priceMax - priceMin;

    // Price band on top
    const priceY0 = pad.t;
    const priceH = plotH * 0.29;
    const priceY = v => priceY0 + priceH - (v - priceMin) / priceRange * priceH;

    // Mode band — thin strip below price bars showing which EMS mode
    // is active per slot. Color-coded so operators see the schedule at a
    // glance without reading per-slot tooltips.
    const modeBandY0 = priceY0 + priceH + 2;

    // Power band in middle — covers battery + grid.
    // Several later sections ("Plan battery bars", "Load forecast",
    // predicted-zone shade, etc.) reference `plan` directly. Keep this
    // alias — removing it leaves those `plan` references undefined and
    // the whole render throws, wiping the chart.
    const plan = state.plan;
    const powerY0 = modeBandY0 + modeBandH + 4;
    const powerH = plotH * 0.42;
    // Scale off the fuse (what the site can *physically* deliver) plus a
    // 15% headroom so peak transients don't clip. e.g. 16 A × 3 φ × 230 V
    // ≈ 11 kW → y-axis spans ±12.7 kW. A fixed scale makes it easier to
    // eyeball plan magnitudes across runs instead of re-interpreting the
    // axis every time the max sample changes.
    const fuse = state.fuse || {};
    const fuseMaxW = (fuse.max_amps || 16) * (fuse.phases || 3) * (fuse.voltage || 230);
    let pMagMax = fuseMaxW * 1.15;
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
    for (let h = 0; h <= 48; h += 6) {
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

    // ---- Predicted-zone shade + boundary ----
    // Find the first ML-forecasted action. Everything at or past that
    // point gets a translucent band and a "predicted" label, so the
    // uncertain portion reads as visually different — not just dimmer
    // bars but a whole different region.
    if (plan && plan.actions && plan.actions.length) {
      const firstPred = plan.actions.find(a => a.confidence != null && a.confidence < 1.0);
      if (firstPred) {
        const xPred = Math.max(xScale(firstPred.slot_start_ms), pad.l);
        const xEnd = pad.l + plotW;
        if (xPred < xEnd) {
          // Shaded band behind everything in the plot area — strong
          // enough to read as "this zone is different".
          ctx.fillStyle = 'rgba(251,191,36,0.10)';
          ctx.fillRect(xPred, pad.t, xEnd - xPred, plotH);
          // Boundary line
          ctx.strokeStyle = 'rgba(251,191,36,0.65)';
          ctx.lineWidth = 1.2;
          ctx.setLineDash([4, 4]);
          ctx.beginPath();
          ctx.moveTo(xPred, pad.t);
          ctx.lineTo(xPred, pad.t + plotH);
          ctx.stroke();
          ctx.setLineDash([]);
          // Label "predicted →"
          ctx.fillStyle = 'rgba(251,191,36,0.9)';
          ctx.font = '10px system-ui, sans-serif';
          ctx.textAlign = 'left';
          ctx.fillText('predicted →', xPred + 4, pad.t + 10);
        }
      }
    }

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
      // Color by price tercile (cheap/neutral/expensive). Predicted bars
      // render as a hollow dashed outline so they read as "uncertain
      // ghost" vs the solid filled real bars.
      let baseRgb;
      if (priceVal <= p25) baseRgb = '34,197,94';       // green
      else if (priceVal >= p75) baseRgb = '239,68,68';  // red
      else baseRgb = '148,163,184';                     // slate
      const rectX = x0;
      const rectY = Math.min(y, zero);
      const rectW = Math.max(1, x1 - x0 - 1);
      const rectH = Math.abs(y - zero);
      if (isPredicted) {
        // Very faint fill + clear dashed outline
        ctx.fillStyle = `rgba(${baseRgb},0.10)`;
        ctx.fillRect(rectX, rectY, rectW, rectH);
        ctx.strokeStyle = `rgba(${baseRgb},0.75)`;
        ctx.lineWidth = 1;
        ctx.setLineDash([3, 3]);
        ctx.strokeRect(rectX + 0.5, rectY + 0.5, rectW - 1, rectH - 1);
        ctx.setLineDash([]);
      } else {
        ctx.fillStyle = `rgba(${baseRgb},0.60)`;
        ctx.fillRect(rectX, rectY, rectW, rectH);
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

    // ---- PV line (negative = generation, site sign) ----
    // Prefer the plan's own per-slot pv_w when the optimiser is running
    // — that's the number that drove the charge/idle/discharge
    // decisions, and it's what you want to compare against reality when
    // the battery behaves unexpectedly (e.g. plan says 0.8 kW PV,
    // reality is 4.6 kW, so the battery absorbs the unforecast surplus).
    // Fall back to the raw weather forecast when there's no plan.
    ctx.strokeStyle = 'rgba(34,197,94,0.9)';
    ctx.lineWidth = 2;
    ctx.beginPath();
    let first = true;
    if (plan && plan.actions && plan.actions.length) {
      for (const a of plan.actions) {
        if (a.slot_start_ms > tMax) break;
        if (a.pv_w == null) continue;
        const x = xScale(a.slot_start_ms);
        const y = powerY(a.pv_w); // plan.pv_w is already site-signed
        if (first) { ctx.moveTo(x, y); first = false; }
        else ctx.lineTo(x, y);
      }
    } else {
      for (const f of state.forecast || []) {
        if (f.slot_ts_ms > tMax || !f.pv_w_estimated) continue;
        const x = xScale(f.slot_ts_ms);
        const y = powerY(-f.pv_w_estimated); // flip forecast → site sign
        if (first) { ctx.moveTo(x, y); first = false; }
        else ctx.lineTo(x, y);
      }
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

    // ---- Battery action band — colored strip showing charge/discharge/idle per slot ----
    if (plan && plan.actions) {
      for (const a of plan.actions) {
        if (a.slot_start_ms > tMax) break;
        const x0 = xScale(a.slot_start_ms);
        const x1 = xScale(a.slot_start_ms + a.slot_len_min * 60 * 1000);
        let color;
        if (a.battery_w > 100)       color = 'rgba(245,158,11,0.6)';   // amber = charging
        else if (a.battery_w < -100) color = 'rgba(139,92,246,0.6)';   // purple = discharging
        else                         color = 'rgba(100,116,139,0.2)';  // slate = idle
        ctx.fillStyle = color;
        ctx.fillRect(x0, modeBandY0, Math.max(1, x1 - x0 - 1), modeBandH);
      }
      ctx.fillStyle = 'rgba(255,255,255,0.45)';
      ctx.font = '9px system-ui, sans-serif';
      ctx.textAlign = 'left';
      ctx.fillText('Battery', pad.l + 4, modeBandY0 + modeBandH - 2);
    }

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
      // SoC axis labels: right-align flush against the plot's right edge
      // so they read as part of the chart frame instead of floating off
      // in whitespace.
      ctx.fillStyle = 'rgba(255,255,255,0.55)';
      ctx.textAlign = 'right';
      ctx.fillText('100%', cssW - pad.r - 4, socY(100) + 4);
      ctx.fillText('0%',   cssW - pad.r - 4, socY(0)   + 4);
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
        let suffix = '';
        if (state.planMeta && state.planMeta.last_replan_ms) {
          const age = Math.round((Date.now() - state.planMeta.last_replan_ms) / 1000);
          const reason = state.planMeta.last_replan_reason || '';
          const ageTxt = age < 60 ? `${age}s` : `${Math.round(age/60)}m`;
          suffix = ` · replanned ${ageTxt} ago (${reason})`;
        }
        summary.textContent =
          `${plan.mode} · ${hh.toFixed(0)}h horizon · ${plan.horizon_slots} slots · ` +
          `SoC ${plan.initial_soc_pct.toFixed(0)}% → ${cost.toFixed(2)} SEK${suffix}`;
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
      if (a.battery_w != null) {
        let action;
        if (a.battery_w > 100) action = 'Charging';
        else if (a.battery_w < -100) action = 'Discharging';
        else action = 'Idle';
        lines.push(`<div class="tip-row"><span>Plan</span><b>${action}</b></div>`);
      }
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
    planner_self: 'Smart self-consumption (planner). Forecast-aware battery schedule that only covers local load or absorbs PV surplus. Never imports to charge, never exports via the battery.',
    planner_cheap: 'Cheap charging. Plans to import during the cheapest upcoming hours to top up the battery, still never exports via the battery. Good when export tariffs are low.',
    planner_arbitrage: 'Arbitrage. Full freedom: charges in the cheapest slots, discharges into the most expensive slots (including exporting). Biggest savings on volatile days; pays attention to battery efficiency + SoC bounds.',
    self_consumption: 'Self (manual). Simple PI tracks grid-target = 0 with no planner.',
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
