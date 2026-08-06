// <ftw-demand-card> — C&I billing demand: the running demand-integration
// window, the billing-cycle peak, and utilization of the Notified
// Maximum Demand. The C&I analog of the fuse card: the fuse is the
// physical boundary, NMD is the contractual one.
//
// Self-fetching: hits /api/demand on connect and every poll-ms
// (default 30 s — the window integrates over 30 min, so faster polling
// buys nothing). A 404 means the site has no tariff configured
// (residential): the card removes itself so the dashboard is untouched.
//
// Attributes:
//   poll-ms — refresh interval (default 30000; 0 disables)

import { FtwElement } from "./ftw-element.js";
import { apiFetch } from "./api-fetch.js";
import { buildDemandView, formatKVA, estimateDemandCost } from "./demand-math.js";

class FtwDemandCard extends FtwElement {
  static styles = `
    :host { display: block; }
    :host([hidden]) { display: none; }
    .card-inner {
      display: flex;
      flex-direction: column;
      gap: 10px;
      background: var(--ink-raised);
      border: 1px solid var(--line);
      border-radius: var(--radius-md, 10px);
      padding: var(--card-pad, 14px 16px);
    }
    .label {
      font-family: var(--mono);
      font-size: 10px;
      color: var(--fg-label);
      letter-spacing: 0.1em;
      text-transform: uppercase;
      display: flex;
      justify-content: space-between;
      align-items: baseline;
      gap: 8px;
    }
    .band {
      font-family: var(--mono);
      font-size: 9px;
      letter-spacing: 0.12em;
      padding: 2px 8px;
      border-radius: 999px;
      border: 1px solid var(--line);
      color: var(--fg-dim);
    }
    .band.counted { border-color: var(--accent-e); color: var(--accent-e); }
    .figures {
      display: flex;
      gap: 24px;
      flex-wrap: wrap;
    }
    .figure { min-width: 90px; }
    .figure .v {
      font-family: var(--mono);
      font-size: 1.4rem;
      font-weight: 700;
      font-variant-numeric: tabular-nums;
      color: var(--fg);
    }
    .figure .u { font-size: 0.75rem; color: var(--fg-muted); margin-left: 3px; }
    .figure .k {
      font-family: var(--mono);
      font-size: 9px;
      letter-spacing: 0.12em;
      text-transform: uppercase;
      color: var(--fg-label);
    }
    .figure.sev-near .v { color: var(--warn, #e0a020); }
    .figure.sev-over .v { color: var(--bad, #e05555); }
    .meter {
      position: relative;
      height: 6px;
      border-radius: 999px;
      background: var(--ink-sunken);
      overflow: hidden;
    }
    .meter .fill {
      position: absolute; inset: 0 auto 0 0;
      border-radius: 999px;
      background: var(--accent-e);
      transition: width 400ms ease;
    }
    .meter .fill.sev-near { background: var(--warn, #e0a020); }
    .meter .fill.sev-over { background: var(--bad, #e05555); }
    .sub {
      font-family: var(--sans);
      font-size: 0.75rem;
      color: var(--fg-muted);
    }
    .sub b { font-family: var(--mono); font-weight: 500; color: var(--fg-dim); }
  `;

  static get observedAttributes() { return ["poll-ms"]; }

  connectedCallback() {
    super.connectedCallback();
    this._load();
    this._startPolling();
  }

  disconnectedCallback() {
    if (this._timer) clearInterval(this._timer);
  }

  attributeChangedCallback() { this._startPolling(); }

  _startPolling() {
    if (this._timer) clearInterval(this._timer);
    const ms = Number(this.getAttribute("poll-ms") ?? 30000);
    if (ms > 0) this._timer = setInterval(() => this._load(), ms);
  }

  async _load() {
    try {
      const res = await apiFetch("/api/demand?limit=0");
      if (res.status === 404) {
        // Residential site — no demand tracking. Remove entirely.
        if (this._timer) clearInterval(this._timer);
        this.remove();
        return;
      }
      if (!res.ok) return; // transient — keep last render
      this._view = buildDemandView(await res.json());
      this.update();
    } catch {
      // network hiccup — keep last render
    }
  }

  render() {
    const v = this._view;
    if (!v) return `<div class="card-inner"><div class="label">Demand</div><div class="sub">Loading…</div></div>`;
    const sev = v.severity === "ok" ? "" : `sev-${v.severity}`;
    const windowPct = Math.round(v.windowProgress * 100);
    const meterPct = v.hasNMD ? Math.min(100, Math.round(v.peakUtil * 100)) : 0;
    const cost = estimateDemandCost(v.peakKVA, v.demandChargeCt);
    const parts = [];
    if (v.hasNMD) parts.push(`NMD <b>${formatKVA(v.nmdKVA)} kVA</b>`);
    if (cost !== null) parts.push(`demand cost so far <b>${cost.toFixed(0)} ${v.currency || ""}</b>`.trim());
    if (v.threatens) parts.push(`<b>current window sets a new peak</b>`);
    return `
      <div class="card-inner">
        <div class="label">
          <span>Demand</span>
          ${v.windowBand ? `<span class="band ${v.windowCounted ? "counted" : ""}">${v.windowBand}${v.windowCounted ? " · billed" : ""}</span>` : ""}
        </div>
        <div class="figures">
          <div class="figure ${v.threatens ? sev : ""}">
            <div class="k">Window (${windowPct}%)</div>
            <div><span class="v">${formatKVA(v.windowKVA)}</span><span class="u">kVA</span></div>
          </div>
          <div class="figure ${sev}">
            <div class="k">Billing peak</div>
            <div><span class="v">${formatKVA(v.peakKVA)}</span><span class="u">kVA</span></div>
          </div>
          ${v.hasNMD ? `
          <div class="figure ${sev}">
            <div class="k">Of NMD</div>
            <div><span class="v">${Math.round(v.peakUtil * 100)}</span><span class="u">%</span></div>
          </div>` : ""}
        </div>
        ${v.hasNMD ? `<div class="meter"><div class="fill ${sev}" style="width:${meterPct}%"></div></div>` : ""}
        ${parts.length ? `<div class="sub">${parts.join(" · ")}</div>` : ""}
      </div>`;
  }
}

customElements.define("ftw-demand-card", FtwDemandCard);
