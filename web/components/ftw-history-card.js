// <ftw-history-card> — daily kWh card with Week / Month toggle.
//
// Smart wrapper around <ftw-bar-chart>. Owns the range state, fetches
// /api/energy/daily, picks the right field per metric, and renders the
// card chrome (label, total, toggle).
//
// Attributes:
//   metric   — "import" | "export" | "load" | "pv"
//              | "bat_charged" | "bat_discharged"
//              picks which *_wh field to plot from each day bucket
//   label    — heading text (e.g. "Imported")
//   accent   — bar + total color (default depends on metric; falls back
//              to var(--cyan))
//   default-range — "week" (default, last 7 days) | "month" (so far)
//   poll-ms  — refresh interval in ms (default 300000 = 5 min);
//              0 disables polling
//
// The card auto-fetches on connect and on range change. No external
// JS wiring needed beyond placing the element in HTML.

import { FtwElement } from "./ftw-element.js";
import "./ftw-bar-chart.js";

const FIELD_BY_METRIC = {
  import:        "import_wh",
  export:        "export_wh",
  load:          "load_wh",
  pv:            "pv_wh",
  bat_charged:   "bat_charged_wh",
  bat_discharged:"bat_discharged_wh",
};
const DEFAULT_ACCENT = {
  import:        "var(--red-e)",
  export:        "var(--green-e)",
  load:          "var(--fg)",
  pv:            "var(--amber)",
  bat_charged:   "var(--cyan)",
  bat_discharged:"var(--cyan-dim, var(--cyan))",
};

class FtwHistoryCard extends FtwElement {
  static styles = `
    :host { display: block; }
    /* All card chrome lives on .card-inner, NOT :host. The global
       reset "*, *::before, *::after { padding:0; margin:0 }" from
       style.css beats the shadow :host rule in Chromium for the
       host element — the universal selector reaches in from the
       document tree and wins. A class selector inside the shadow
       DOM has specificity (0,0,1,0), which always beats * (0,0,0,0).
       Use this wrapper pattern for any web component that needs
       visible padding/margin on its outermost surface. */
    .card-inner {
      display: flex;
      flex-direction: column;
      gap: 8px;
      background: var(--ink-raised);
      border: 1px solid var(--line);
      border-radius: var(--radius-md, 10px);
      padding: var(--card-pad, 14px 16px);
    }
    .head {
      display: flex;
      align-items: center;
      justify-content: space-between;
    }
    .label {
      font-family: var(--mono);
      font-size: 10px;
      color: var(--fg-muted);
      letter-spacing: 0.1em;
      text-transform: uppercase;
    }
    .toggle {
      display: inline-flex;
      border: 1px solid var(--line);
      border-radius: var(--radius-sm, 4px);
      overflow: hidden;
    }
    .toggle button {
      background: transparent;
      border: 0;
      color: var(--fg-muted);
      font-family: var(--mono);
      font-size: 10px;
      letter-spacing: 0.08em;
      text-transform: uppercase;
      padding: 4px 8px;
      cursor: pointer;
    }
    .toggle button.active {
      background: var(--accent, oklch(0.55 0.15 240));
      color: var(--fg);
    }
    .total {
      font-family: var(--mono);
      font-size: 1.05rem;
      font-weight: 700;
      font-variant-numeric: tabular-nums;
      letter-spacing: -0.01em;
      color: var(--ftw-history-accent, var(--fg));
    }
    @media (max-width: 900px) {
      .card-inner { padding: var(--card-pad-tight, 12px 14px); }
    }
  `;

  static get observedAttributes() {
    return ["metric", "label", "accent", "default-range", "poll-ms"];
  }

  constructor() {
    super();
    this._range = null;
    this._timer = null;
    this._chart = null;
    this._totalEl = null;
    this._toggleEl = null;
    this._reqSeq = 0;
    this._abort = null;
  }

  connectedCallback() {
    super.connectedCallback();
    this._refresh();
    this._restartPolling();
  }
  disconnectedCallback() {
    if (this._timer) { clearInterval(this._timer); this._timer = null; }
    if (this._abort) { this._abort.abort(); this._abort = null; }
  }

  attributeChangedCallback(name) {
    this.update();
    if (name === "metric" || name === "poll-ms") {
      this._refresh();
      this._restartPolling();
    }
  }

  _restartPolling() {
    if (this._timer) { clearInterval(this._timer); this._timer = null; }
    // `??` not `||`: poll-ms="0" must disable polling, but "0" is truthy
    // in the ||-fallback so that path silently reverts to 300000.
    const raw = this.getAttribute("poll-ms");
    const ms = Number(raw ?? 300000);
    if (ms > 0 && this.isConnected) {
      this._timer = setInterval(() => this._refresh(), ms);
    }
  }

  _accent() {
    const explicit = this.getAttribute("accent");
    if (explicit) return explicit;
    const metric = this.getAttribute("metric") || "import";
    return DEFAULT_ACCENT[metric] || "var(--cyan)";
  }

  render() {
    if (this._range == null) {
      this._range = this.getAttribute("default-range") || "week";
    }
    const label = this.getAttribute("label") || "";
    const accent = this._accent();
    this.style.setProperty("--ftw-history-accent", accent);
    // Plain buttons with aria-pressed rather than role=tablist/tab: a
    // proper tabs pattern requires aria-selected + arrow-key navigation
    // we don't implement, so we'd only be lying to assistive tech.
    // accent is applied to the bar-chart in afterRender() via
    // setAttribute, not interpolated here, so a future caller passing a
    // CSS value containing quotes can't escape the attribute context.
    const wk = this._range === "week";
    return `
      <div class="card-inner">
        <div class="head">
          <div class="label">${escapeHtml(label)}</div>
          <div class="toggle">
            <button type="button" data-range="week"  aria-pressed="${wk ? "true" : "false"}"${wk ? ' class="active"' : ""}>Week</button>
            <button type="button" data-range="month" aria-pressed="${!wk ? "true" : "false"}"${!wk ? ' class="active"' : ""}>Month</button>
          </div>
        </div>
        <div class="total" data-role="total">— kWh</div>
        <ftw-bar-chart data-role="chart" loading="true"></ftw-bar-chart>
      </div>
    `;
  }

  afterRender() {
    this._chart   = this.shadowRoot.querySelector('[data-role="chart"]');
    this._totalEl = this.shadowRoot.querySelector('[data-role="total"]');
    this._toggleEl = this.shadowRoot.querySelector('.toggle');
    if (this._chart) this._chart.setAttribute("accent", this._accent());
    if (this._toggleEl) {
      this._toggleEl.addEventListener('click', (e) => {
        const btn = e.target.closest('button[data-range]');
        if (!btn) return;
        const next = btn.getAttribute('data-range');
        if (!next || next === this._range) return;
        this._range = next;
        this.update();
        this._refresh();
        this.dispatchEvent(new CustomEvent('ftw-history-range', {
          detail: { range: next, metric: this.getAttribute('metric') || '' },
          bubbles: true, composed: true,
        }));
      });
    }
  }

  _daysFor(range) {
    if (range === "month") {
      const now = new Date();
      return now.getDate();
    }
    return 7;
  }

  _refresh() {
    if (!this._chart || !this._totalEl) return;
    const metric = this.getAttribute("metric") || "import";
    const field  = FIELD_BY_METRIC[metric] || "import_wh";
    const days   = this._daysFor(this._range);

    // Cancel any in-flight request and bump the sequence so stale
    // responses arriving after a Week/Month toggle don't overwrite the
    // newer chart. Both guards matter: AbortController stops the older
    // network request, and the seq check stops a response that resolved
    // just before we aborted.
    if (this._abort) this._abort.abort();
    this._abort = new AbortController();
    const seq = ++this._reqSeq;
    const signal = this._abort.signal;

    this._chart.setAttribute("loading", "true");
    fetch("/api/energy/daily?days=" + days, { signal })
      .then((r) => r.json())
      .then((resp) => {
        if (seq !== this._reqSeq) return;
        const buckets = (resp && resp.days) || [];
        let sum = 0;
        const data = buckets.map((b) => {
          const wh = Number(b[field]) || 0;
          sum += wh;
          const kwh = wh / 1000;
          return {
            label: fmtDayShort(b.day),
            value: wh,
            displayValue: kwh >= 100 ? kwh.toFixed(0) : kwh.toFixed(1),
            title: fmtDayShort(b.day) + ": " + fmtKwh(wh),
          };
        });
        this._totalEl.textContent = data.length
          ? fmtKwh(sum) + " total"
          : "— kWh";
        this._chart.data = data;
      })
      .catch((err) => {
        if (err && err.name === "AbortError") return;
        if (seq !== this._reqSeq) return;
        this._chart.removeAttribute("loading");
        this._chart.data = [];
        this._totalEl.textContent = "failed to load";
      });
  }
}

function fmtKwh(wh) {
  const kwh = (wh || 0) / 1000;
  if (kwh >= 100) return kwh.toFixed(0) + " kWh";
  if (kwh >= 10)  return kwh.toFixed(1) + " kWh";
  return kwh.toFixed(2) + " kWh";
}
function fmtDayShort(iso) {
  const parts = String(iso || "").split("-");
  if (parts.length !== 3) return iso || "";
  const d = new Date(+parts[0], +parts[1] - 1, +parts[2]);
  return d.toLocaleDateString(undefined, { weekday: "short", day: "numeric" });
}
function escapeHtml(s) {
  return String(s).replace(/[<>&"']/g, (c) =>
    ({ "<": "&lt;", ">": "&gt;", "&": "&amp;", '"': "&quot;", "'": "&#39;" }[c]));
}

customElements.define("ftw-history-card", FtwHistoryCard);
