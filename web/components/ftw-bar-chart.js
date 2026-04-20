// <ftw-bar-chart> — vertical bar chart for small daily/weekly series.
//
// Pure presentational: takes a data array via the .data setter, draws
// one column per entry. Knows nothing about kWh, drivers, or the
// dashboard — accent color and bar layout are configurable. Use it
// anywhere a compact "N values, one per bucket" chart is wanted
// (history cards, MPC slots, twin diagnostics).
//
// Properties:
//   .data = [{ label, value, displayValue?, title? }, ...]
//     label        — short string under the bar (e.g. "Mon 15")
//     value        — numeric, drives bar height (relative to max in set)
//     displayValue — optional pre-formatted string above the bar; falls
//                    back to a 1-decimal short form of value
//     title        — optional tooltip on the column
//
// Attributes:
//   accent     — CSS color for the bars (default var(--cyan))
//   chart-height — px height of the bar area (default 96)
//   bar-max    — px max-width per bar (default 28)
//   loading    — "true" shows a placeholder; data setter clears it
//   empty-text — text shown when data is empty (default "no data")
//
// Dispatches: nothing (pure dumb).

import { FtwElement } from "./ftw-element.js";

class FtwBarChart extends FtwElement {
  static styles = `
    :host {
      display: block;
      --ftw-bar-color: var(--cyan, #38bdf8);
    }
    .bars {
      display: grid;
      grid-auto-flow: column;
      grid-auto-columns: minmax(0, 1fr);
      align-items: end;
      gap: 4px;
      height: var(--ftw-chart-height, 96px);
      padding-top: 4px;
    }
    .bars.placeholder {
      display: flex;
      align-items: center;
      justify-content: center;
      color: var(--fg-muted);
      font-family: var(--mono);
      font-size: 11px;
    }
    .col {
      display: flex;
      flex-direction: column;
      align-items: center;
      justify-content: end;
      height: 100%;
      min-width: 0;
    }
    .val {
      font-family: var(--mono);
      font-size: 9px;
      color: var(--fg-muted);
      margin-bottom: 2px;
      line-height: 1;
      white-space: nowrap;
    }
    .bar {
      width: 100%;
      max-width: var(--ftw-bar-max, 28px);
      background: var(--ftw-bar-color);
      border-radius: 2px 2px 0 0;
      min-height: 1px;
      transition: height 0.3s ease;
    }
    .lbl {
      font-family: var(--mono);
      font-size: 9px;
      color: var(--fg-muted);
      margin-top: 4px;
      white-space: nowrap;
    }
  `;

  static get observedAttributes() {
    return ["accent", "chart-height", "bar-max", "loading", "empty-text"];
  }

  constructor() {
    super();
    this._data = [];
  }

  attributeChangedCallback() { this.update(); }

  // Bulk setter — preferred update path. Replaces the whole series.
  set data(arr) {
    this._data = Array.isArray(arr) ? arr : [];
    this.removeAttribute("loading");
    this.update();
  }
  get data() { return this._data; }

  render() {
    const accent = this.getAttribute("accent");
    const height = this.getAttribute("chart-height");
    const barMax = this.getAttribute("bar-max");
    if (accent) this.style.setProperty("--ftw-bar-color", accent);
    if (height) this.style.setProperty("--ftw-chart-height", height + "px");
    if (barMax) this.style.setProperty("--ftw-bar-max", barMax + "px");

    const loading = this.getAttribute("loading") === "true";
    if (loading) {
      return `<div class="bars placeholder">loading…</div>`;
    }
    if (!this._data.length) {
      const empty = this.getAttribute("empty-text") || "no data";
      return `<div class="bars placeholder">${escapeHtml(empty)}</div>`;
    }

    let max = 0;
    for (const d of this._data) {
      const v = Number(d.value) || 0;
      if (v > max) max = v;
    }
    const cols = this._data.map((d) => {
      const v = Number(d.value) || 0;
      // 2% floor keeps tiny-but-nonzero values visible; gate on v>0 so
      // truly-zero buckets render as empty columns, not 2% slivers.
      const pct = max > 0 && v > 0 ? Math.max(2, (v / max) * 100) : 0;
      const display = d.displayValue != null ? String(d.displayValue) : shortNum(v);
      const title = d.title != null ? String(d.title) : `${d.label || ""}: ${display}`;
      return `<div class="col" title="${escapeHtml(title)}">` +
             `<span class="val">${escapeHtml(display)}</span>` +
             `<div class="bar" style="height:${pct}%"></div>` +
             `<span class="lbl">${escapeHtml(d.label || "")}</span>` +
             `</div>`;
    }).join("");
    return `<div class="bars">${cols}</div>`;
  }
}

function shortNum(v) {
  const a = Math.abs(v);
  if (a >= 100) return v.toFixed(0);
  if (a >= 10)  return v.toFixed(1);
  return v.toFixed(2);
}
function escapeHtml(s) {
  return String(s).replace(/[<>&"']/g, (c) =>
    ({ "<": "&lt;", ">": "&gt;", "&": "&amp;", '"': "&quot;", "'": "&#39;" }[c]));
}

customElements.define("ftw-bar-chart", FtwBarChart);
