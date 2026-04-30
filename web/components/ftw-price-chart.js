// <ftw-price-chart> — full-width bar chart of known electricity spot
// prices (next 48 h or so), with a toggle to include VAT (default on)
// and hover tooltip per slot. Peaks + lows are marked. Self-fetching:
// hits /api/prices and /api/config on connect, polls /api/prices
// every 5 min after that.
//
// Inputs (none — autonomous). The component renders its own header
// (label + VAT toggle) and the SVG chart underneath.
//
// Data shape from /api/prices:
//   { zone: "SE4", enabled: true, items: [
//       { slot_ts_ms, slot_len_min, spot_ore_kwh, total_ore_kwh, ... }
//     ] }
//
// Sweden VAT rate (25 %) is read from /api/config price.vat_percent
// when available; falls back to 25 for the toggle math when the
// config endpoint is missing.

import { FtwElement } from "./ftw-element.js";

class FtwPriceChart extends FtwElement {
  static styles = `
    :host {
      display: block;
      font-family: var(--sans);
      color: var(--fg);
    }
    .head {
      display: flex;
      align-items: center;
      justify-content: space-between;
      margin-bottom: 10px;
      gap: 12px;
      flex-wrap: wrap;
    }
    .label {
      font-family: var(--mono);
      font-size: 0.7rem;
      font-weight: 500;
      letter-spacing: 0.18em;
      text-transform: uppercase;
      color: var(--fg-muted);
    }
    .meta {
      font-family: var(--mono);
      font-size: 11px;
      color: var(--fg-dim);
    }
    .toggle {
      position: relative;
      display: inline-grid;
      grid-auto-flow: column;
      grid-auto-columns: minmax(0, 1fr);
      border: 1px solid var(--line);
      border-radius: 999px;
      background: var(--ink-sunken);
      padding: 2px;
      isolation: isolate;
    }
    .toggle::before {
      content: '';
      position: absolute;
      top: 2px; bottom: 2px;
      left: 2px;
      width: calc(50% - 2px);
      background: var(--accent-e);
      border-radius: 999px;
      transform: translateX(0);
      transition: transform 240ms cubic-bezier(0.4, 0, 0.2, 1);
      z-index: 0;
    }
    .toggle[data-vat="off"]::before {
      transform: translateX(100%);
    }
    .toggle button {
      position: relative;
      z-index: 1;
      background: transparent;
      border: 0;
      color: var(--fg-dim);
      font-family: var(--mono);
      font-size: 10px;
      font-weight: 500;
      letter-spacing: 0.18em;
      text-transform: uppercase;
      padding: 4px 14px;
      cursor: pointer;
      transition: color 220ms ease;
    }
    .toggle button.active { color: #0a0a0a; }
    .toggle button:not(.active):hover { color: var(--fg); }

    .chart-wrap {
      position: relative;
    }
    svg.chart {
      width: 100%;
      display: block;
      user-select: none;
    }
    .empty {
      color: var(--fg-muted);
      font-size: 0.85rem;
      padding: 24px 8px;
      text-align: center;
    }
    /* Tooltip — absolutely positioned, follows the cursor's slot. */
    .tip {
      position: absolute;
      pointer-events: none;
      background: var(--ink-raised);
      border: 1px solid var(--line);
      border-radius: 6px;
      padding: 8px 10px;
      font-family: var(--mono);
      font-size: 12px;
      color: var(--fg);
      transform: translate(-50%, -110%);
      white-space: nowrap;
      opacity: 0;
      transition: opacity 80ms;
      z-index: 5;
    }
    .tip.visible { opacity: 1; }
    .tip-time {
      color: var(--fg-dim);
      margin-bottom: 2px;
    }
    .tip-price {
      font-size: 14px;
      font-weight: 600;
    }
    .tip-price.peak  { color: var(--red-e); }
    .tip-price.low   { color: var(--green-e); }
  `;

  constructor() {
    super();
    this._data = null;        // { zone, items: [{tsMs, ore}], min, max, vatPct }
    this._vatOn = true;       // toggle state — default ON per spec
    this._refreshTimer = null;
    this._hover = null;       // { idx, x, y } during hover
    this._vatPct = 25;        // fallback; overwritten from /api/config
  }

  connectedCallback() {
    super.connectedCallback();
    this._loadConfig();
    this._loadPrices();
    this._refreshTimer = setInterval(() => this._loadPrices(), 5 * 60 * 1000);
  }

  disconnectedCallback() {
    if (this._refreshTimer) {
      clearInterval(this._refreshTimer);
      this._refreshTimer = null;
    }
  }

  async _loadConfig() {
    try {
      const r = await fetch("/api/config");
      const j = await r.json();
      const v = j && j.price && j.price.vat_percent;
      if (typeof v === "number" && v > 0) {
        this._vatPct = v;
        this.update();
      }
    } catch (e) { /* ignore — fallback 25 % is fine */ }
  }

  async _loadPrices() {
    try {
      const r = await fetch("/api/prices?hours=48");
      const j = await r.json();
      if (!j || !Array.isArray(j.items)) {
        this._data = null;
      } else {
        // Items already carry both spot_ore_kwh and total_ore_kwh, but
        // the operator's mental model is "spot price ± VAT" — so we
        // base on spot and let the toggle add VAT. Keeps the toggle
        // semantics honest (it's NOT a tariff/grid-fee toggle, just
        // VAT) and matches the API spec the user asked for.
        const items = j.items.map((it) => ({
          tsMs:  Number(it.slot_ts_ms) || 0,
          lenMin: Number(it.slot_len_min) || 60,
          spot:  Number(it.spot_ore_kwh) || 0,
        })).sort((a, b) => a.tsMs - b.tsMs);
        this._data = { zone: j.zone || "", items };
      }
      this.update();
    } catch (e) {
      this._data = null;
      this.update();
    }
  }

  // Resolved öre/kWh per slot for the active toggle.
  _priceFor(item) {
    return this._vatOn
      ? item.spot * (1 + this._vatPct / 100)
      : item.spot;
  }

  render() {
    const data = this._data;
    const vatLabel = this._vatOn ? "incl. VAT" : "spot only";
    const head = `
      <div class="head">
        <div>
          <div class="label">Electricity prices</div>
          <div class="meta">${data ? `${escapeXml(data.zone)} · ${vatLabel}` : "—"}</div>
        </div>
        <div class="toggle" role="tablist" data-vat="${this._vatOn ? "on" : "off"}">
          <button type="button" data-vat="on"  class="${this._vatOn ? "active" : ""}" aria-selected="${this._vatOn}">Incl. VAT</button>
          <button type="button" data-vat="off" class="${!this._vatOn ? "active" : ""}" aria-selected="${!this._vatOn}">Spot</button>
        </div>
      </div>
    `;
    if (!data || !data.items.length) {
      return head + `<div class="empty">No price data available.</div>`;
    }
    return head + this._renderChart(data);
  }

  _renderChart(data) {
    // Compute prices, min/max, and the indices of the lowest +
    // highest slots for the marker overlays.
    const items = data.items;
    const n = items.length;
    const prices = items.map((it) => this._priceFor(it));
    let lo = 0, hi = 0;
    for (let i = 1; i < n; i++) {
      if (prices[i] < prices[lo]) lo = i;
      if (prices[i] > prices[hi]) hi = i;
    }
    const minP = prices[lo];
    const maxP = prices[hi];
    const meanP = prices.reduce((a, p) => a + p, 0) / n;

    // SVG geometry. Width = 100 % via viewBox; height fixed-ish so
    // the section holds twice the fuse-card height (DESIGN spec).
    const W = 1000;
    const H = 240;
    // Wider left padding so the y-axis öre labels have breathing
    // room between the SVG edge and the plot's first bar (was 36 →
    // labels rendered too close to the card's left border).
    const pad = { t: 16, r: 16, b: 28, l: 56 };
    const plotW = W - pad.l - pad.r;
    const plotH = H - pad.t - pad.b;
    const barW = plotW / n;
    // Y scale: include 0 so a negative-spot day still renders, and
    // pad the top so the peak's marker doesn't kiss the edge.
    const yMin = Math.min(0, minP);
    const yMax = Math.max(maxP * 1.08, 1);
    const yToPx = (v) => pad.t + plotH - ((v - yMin) / (yMax - yMin)) * plotH;
    const zeroY = yToPx(0);
    const meanY = yToPx(meanP);

    // "Now" vertical line — falls inside one of the slots if any.
    const now = Date.now();
    let nowIdx = -1;
    for (let i = 0; i < n; i++) {
      const start = items[i].tsMs;
      const end   = start + items[i].lenMin * 60_000;
      if (now >= start && now < end) { nowIdx = i; break; }
    }

    // Bars — colour by relative price (cheaper = green, expensive =
    // red, mid = neutral). Using the per-slot deviation from the mean
    // keeps the colour discipline meaningful even on flat-price days.
    const bars = items.map((it, i) => {
      const x = pad.l + i * barW;
      const p = prices[i];
      const y = yToPx(p);
      const h = Math.max(1, zeroY - y);
      const dev = (p - meanP) / Math.max(1, maxP - minP);
      const fill = i === lo ? "var(--green-e)"
                  : i === hi ? "var(--red-e)"
                  : (p < meanP ? `color-mix(in srgb, var(--green-e) ${Math.round(40 - dev * 40)}%, transparent)`
                              : `color-mix(in srgb, var(--red-e) ${Math.round(40 + dev * 40)}%, transparent)`);
      const stroke = (i === lo || i === hi) ? "currentColor" : "none";
      return `<rect x="${x + 0.5}" y="${y}" width="${Math.max(0.1, barW - 1)}" height="${h}"
                    fill="${fill}" data-idx="${i}"
                    style="${i === lo ? 'color: var(--green-e)' : i === hi ? 'color: var(--red-e)' : ''}"
                    stroke="${stroke}" stroke-width="${stroke === 'none' ? 0 : 1}" />`;
    }).join("");

    // Mean reference line — true dotted (round caps + zero-length
    // dashes spaced by 6 px) so it reads as "average over the known
    // price period" rather than a regular dashed grid line. Sits
    // above the bars but below the markers and tooltip.
    const meanLine = `<line x1="${pad.l}" x2="${pad.l + plotW}" y1="${meanY}" y2="${meanY}"
                          stroke="var(--fg-muted)" stroke-width="1.5"
                          stroke-linecap="round" stroke-dasharray="0.01 6" />`;

    // X-axis time ticks — every 3 hours so 48 h reads as ~16 evenly
    // spaced labels, not the 8-label sparse grid we had before.
    // Operators kept asking "what hour is this?" mid-chart.
    const xTicks = [];
    if (n > 0) {
      const startT = items[0].tsMs;
      const endT = items[n - 1].tsMs + items[n - 1].lenMin * 60_000;
      const step = 3 * 3600_000;
      for (let t = ceilTo(startT, step); t < endT; t += step) {
        const frac = (t - startT) / (endT - startT);
        const x = pad.l + frac * plotW;
        xTicks.push(`<line x1="${x}" x2="${x}" y1="${pad.t + plotH}" y2="${pad.t + plotH + 4}"
                          stroke="var(--line)" />
                     <text x="${x}" y="${pad.t + plotH + 16}" text-anchor="middle"
                           fill="var(--fg-muted)" font-family="var(--mono)" font-size="10">
                       ${fmtClock(t)}
                     </text>`);
      }
    }
    // Y-axis labels — min / mean / max.
    const yLabels = [
      { y: yToPx(yMax), text: roundOre(yMax) + " ö" },
      { y: meanY,       text: roundOre(meanP) + " ö" },
      { y: yToPx(yMin), text: roundOre(yMin) + " ö" },
    ].map((l) => `<text x="${pad.l - 4}" y="${l.y + 3}" text-anchor="end"
                       fill="var(--fg-muted)" font-family="var(--mono)" font-size="10">${l.text}</text>`).join("");

    // "Now" marker — vertical line plus a "now" pill.
    let nowMarker = "";
    if (nowIdx >= 0) {
      const x = pad.l + (nowIdx + 0.5) * barW;
      nowMarker = `
        <line x1="${x}" x2="${x}" y1="${pad.t}" y2="${pad.t + plotH}"
              stroke="var(--accent-e)" stroke-width="1.5" stroke-dasharray="2 3"
              opacity="0.7" />
        <text x="${x}" y="${pad.t - 4}" text-anchor="middle"
              fill="var(--accent-e)" font-family="var(--mono)" font-size="10"
              font-weight="600">NOW</text>
      `;
    }

    // Peak / low markers — small triangles above their bars.
    const markBar = (idx, color, glyph) => {
      const x = pad.l + (idx + 0.5) * barW;
      const y = yToPx(prices[idx]) - 6;
      return `<text x="${x}" y="${y}" text-anchor="middle"
                    fill="${color}" font-family="var(--mono)" font-size="11"
                    font-weight="700">${glyph}</text>`;
    };

    // Hit-target overlay — invisible rects sized to bar width that
    // cover the FULL plot height so hover is forgiving even when a
    // bar is short (cheap slots).
    const hits = items.map((_, i) => {
      const x = pad.l + i * barW;
      return `<rect x="${x}" y="${pad.t}" width="${barW}" height="${plotH}"
                    fill="transparent" data-idx="${i}" class="hit" />`;
    }).join("");

    return `
      <div class="chart-wrap">
        <svg class="chart" viewBox="0 0 ${W} ${H}" preserveAspectRatio="none"
             role="img" aria-label="Electricity price chart">
          ${meanLine}
          ${bars}
          ${nowMarker}
          ${markBar(lo, "var(--green-e)", "▼")}
          ${markBar(hi, "var(--red-e)",   "▲")}
          ${xTicks.join("")}
          ${yLabels}
          ${hits}
        </svg>
        <div class="tip" data-tip>
          <div class="tip-time" data-tip-time>—</div>
          <div class="tip-price" data-tip-price>—</div>
        </div>
      </div>
    `;
  }

  afterRender() {
    const root = this.shadowRoot;
    const toggle = root.querySelector(".toggle");
    if (toggle) {
      toggle.querySelectorAll("button[data-vat]").forEach((b) => {
        b.addEventListener("click", () => {
          const next = b.dataset.vat === "on";
          if (next === this._vatOn) return;
          this._vatOn = next;
          this.update();
        });
      });
    }
    // Tooltip wiring — listen on the SVG and route by data-idx.
    const svg = root.querySelector("svg.chart");
    const tip = root.querySelector("[data-tip]");
    if (!svg || !tip || !this._data) return;
    const onMove = (e) => {
      const target = e.target.closest("[data-idx]");
      if (!target) { this._hideTip(); return; }
      const i = Number(target.dataset.idx);
      if (!Number.isFinite(i)) { this._hideTip(); return; }
      this._showTip(i, e);
    };
    svg.addEventListener("mousemove", onMove);
    svg.addEventListener("mouseleave", () => this._hideTip());
  }

  _showTip(idx, e) {
    const tip = this.shadowRoot.querySelector("[data-tip]");
    const item = this._data.items[idx];
    if (!tip || !item) return;
    const price = this._priceFor(item);
    const tEnd = item.tsMs + item.lenMin * 60_000;
    tip.querySelector("[data-tip-time]").textContent =
      `${fmtClock(item.tsMs)}–${fmtClock(tEnd)}`;
    const priceEl = tip.querySelector("[data-tip-price]");
    priceEl.textContent = `${roundOre(price)} öre / kWh`;
    // Annotate peak/low per the same indices used in render.
    const items = this._data.items;
    const prices = items.map((it) => this._priceFor(it));
    let lo = 0, hi = 0;
    for (let i = 1; i < items.length; i++) {
      if (prices[i] < prices[lo]) lo = i;
      if (prices[i] > prices[hi]) hi = i;
    }
    priceEl.classList.toggle("peak", idx === hi);
    priceEl.classList.toggle("low",  idx === lo);
    // Position the tooltip at the cursor's X, anchored above.
    const rect = e.currentTarget.getBoundingClientRect();
    const x = e.clientX - rect.left;
    const y = e.clientY - rect.top;
    tip.style.left = x + "px";
    tip.style.top  = y + "px";
    tip.classList.add("visible");
  }

  _hideTip() {
    const tip = this.shadowRoot.querySelector("[data-tip]");
    if (tip) tip.classList.remove("visible");
  }
}

function fmtClock(tsMs) {
  const d = new Date(tsMs);
  return d.getHours().toString().padStart(2, "0") + ":" +
         d.getMinutes().toString().padStart(2, "0");
}

function roundOre(v) {
  if (Math.abs(v) >= 100) return v.toFixed(0);
  if (Math.abs(v) >= 10)  return v.toFixed(1);
  return v.toFixed(2);
}

function ceilTo(t, step) {
  return Math.ceil(t / step) * step;
}

function escapeXml(s) {
  return String(s).replace(/[&<>"']/g, (c) => ({
    "&": "&amp;", "<": "&lt;", ">": "&gt;", "\"": "&quot;", "'": "&#39;"
  }[c]));
}

customElements.define("ftw-price-chart", FtwPriceChart);
