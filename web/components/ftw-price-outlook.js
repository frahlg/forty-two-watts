// <ftw-price-outlook> — compact "is it cheap or expensive right now, and
// when does that change?" strip for the Overview destination. The full
// price chart lives on Energy; this one answers three questions in a
// glance and gets out of the way.
//
// Inputs (none — autonomous). Self-fetching: /api/prices and /api/config
// on connect, then re-polls prices every 5 min.
//
// Why a deviation chart and not a small bar chart of the price
// ------------------------------------------------------------
// The consumer price is (spot + grid tariff) × (1 + VAT/100). The tariff
// is the same every slot — on a 70 öre/kWh tariff, ~87 of a 109 öre slot
// never moves. Plotting the total makes every bar nearly the same height
// and hides the only part that varies. So the baseline is the mean over
// the window and each bar is that slot's distance from it: above the line
// is dearer than average, below is cheaper. The shape of the day is the
// signal, and the fixed floor drops out of the picture entirely.
//
// Why direction carries the meaning, not colour
// ---------------------------------------------
// The theme's green and red sit ΔE 2.4 apart under deuteranopia in light
// mode (ΔE 5.1 in dark) — far below the ΔE 8 needed to tell two hues
// apart. A red/green strip would be unreadable for roughly one man in
// twelve. Here up/down against the mean already says dear/cheap, the bar
// length says by how much, and the summary tiles name both windows in
// words. Colour only reinforces what the geometry has already said, so
// the strip survives greyscale, CVD and forced-colors.
//
// The cheapest/dearest tiles report the best and worst contiguous 2 h
// window — the unit household decisions actually come in (a dishwasher
// cycle, an EV top-up), not an isolated 15-minute slot you can't use.

import { FtwElement } from "./ftw-element.js";
import { apiFetch } from "./api-fetch.js";
import { bestBlock, consumerTotalOre } from "./price-math.js";

// Window shown, and the block length the cheap/dear tiles search for.
const HORIZON_H = 24;
const BLOCK_H = 2;

class FtwPriceOutlook extends FtwElement {
  static styles = `
    :host { display: block; font-family: var(--sans); color: var(--fg); }

    .head {
      display: flex;
      align-items: baseline;
      justify-content: space-between;
      gap: 4px 12px;
      /* Below ~270px the link drops to its own line rather than crushing
         the title into it. */
      flex-wrap: wrap;
      margin-bottom: 10px;
    }
    .label {
      font-family: var(--mono);
      font-size: 0.7rem;
      font-weight: 500;
      letter-spacing: 0.18em;
      text-transform: uppercase;
      color: var(--fg-muted);
    }
    /* Horizon on its own line, matching <ftw-price-chart>'s label+meta
       header. Keeping it inside .label wrapped mid-phrase on a phone. */
    .meta {
      font-family: var(--mono);
      font-size: 11px;
      color: var(--fg-dim);
      margin-top: 0.15rem;
    }
    .more {
      font-family: var(--mono);
      font-size: 10px;
      letter-spacing: 0.12em;
      text-transform: uppercase;
      color: var(--fg-dim);
      text-decoration: none;
      white-space: nowrap;
      border-bottom: 1px solid transparent;
    }
    .more:hover { color: var(--fg); border-bottom-color: var(--line); }

    /* Summary tiles — the answer in words, so the chart is confirmation
       rather than the only source of the finding.

       auto-fit rather than a fixed three columns: the tile count follows
       the card's own width, so a narrow column on a wide screen drops to
       two tiles per row instead of squeezing three and overflowing. A
       media query cannot do this — it measures the window, not the card. */
    .tiles {
      display: grid;
      grid-template-columns: repeat(auto-fit, minmax(150px, 1fr));
      gap: 8px;
      margin-bottom: 12px;
    }
    .tile {
      background: var(--ink-sunken);
      border: 1px solid var(--line);
      border-radius: 6px;
      padding: 7px 9px;
      min-width: 0;
    }
    .tile-label {
      font-family: var(--mono);
      font-size: 9px;
      letter-spacing: 0.12em;
      text-transform: uppercase;
      color: var(--fg-muted);
      display: flex;
      align-items: center;
      gap: 4px;
    }
    /* The arrow repeats the tile's meaning as a glyph, so cheap/dear is
       legible without reading the colour. */
    .tile-arrow { font-size: 11px; line-height: 1; }
    .tile-when {
      font-family: var(--mono);
      font-size: 13px;
      font-weight: 600;
      margin-top: 3px;
      white-space: nowrap;
      overflow: hidden;
      text-overflow: ellipsis;
    }
    /* No nowrap: if a tile still ends up narrower than its price line, the
       line wraps rather than running out past the tile's border. */
    .tile-price {
      font-family: var(--mono);
      font-size: 10px;
      color: var(--fg-dim);
      margin-top: 1px;
    }
    .tile.cheap .tile-arrow  { color: var(--green-e); }
    .tile.dear  .tile-arrow  { color: var(--red-e); }

    .chart-wrap { position: relative; }
    /* The viewBox is authored in real pixels, so the height attribute is
       honoured as-is and nothing inside gets scaled. */
    svg { width: 100%; display: block; touch-action: pan-y; }
    .empty {
      color: var(--fg-muted);
      font-size: 0.85rem;
      padding: 18px 8px;
      text-align: center;
    }

    .tip {
      position: absolute;
      pointer-events: none;
      background: var(--ink-raised);
      border: 1px solid var(--line);
      border-radius: 6px;
      padding: 6px 8px;
      font-family: var(--mono);
      font-size: 11px;
      color: var(--fg);
      transform: translate(-50%, -115%);
      white-space: nowrap;
      opacity: 0;
      transition: opacity 80ms;
      z-index: 5;
    }
    .tip.visible { opacity: 1; }
    .tip-when { color: var(--fg-dim); }
    .tip-dev { color: var(--fg-dim); margin-top: 1px; }

    /* One tile layout at every width. The stacked form (label, window,
       price) fits any column the auto-fit grid can produce, so there is no
       second layout to get wrong. */
  `;

  constructor() {
    super();
    this._items = null;    // [{tsMs, lenMin, spot}]
    this._vatPct = 25;
    this._gridTariff = 0;
    this._timer = null;
    this._geom = null;     // { padL, plotW, n, W } for touch hit-testing
    this._width = 0;       // measured card width in CSS px
    this._ro = null;
  }

  connectedCallback() {
    super.connectedCallback();
    this._loadConfig();
    this._loadPrices();
    this._timer = setInterval(() => this._loadPrices(), 5 * 60 * 1000);
    // The chart's viewBox is set to the card's measured pixel width, so it
    // has to be re-rendered when that width changes. A media query is the
    // wrong instrument here: it reports the window, and this card can sit
    // in a narrow column on a wide screen.
    if (typeof ResizeObserver !== "undefined") {
      this._ro = new ResizeObserver((entries) => {
        const w = Math.round(entries[0].contentRect.width);
        // Re-render only on a real change — the observer fires again after
        // our own render, and an unguarded update() would loop.
        if (w > 0 && Math.abs(w - this._width) > 4) {
          this._width = w;
          this.update();
        }
      });
      this._ro.observe(this);
    }
  }

  disconnectedCallback() {
    if (this._timer) { clearInterval(this._timer); this._timer = null; }
    if (this._ro) { this._ro.disconnect(); this._ro = null; }
  }

  async _loadConfig() {
    try {
      const r = await apiFetch("/api/config");
      const j = await r.json();
      const p = (j && j.price) || {};
      if (typeof p.vat_percent === "number" && p.vat_percent > 0) this._vatPct = p.vat_percent;
      if (typeof p.grid_tariff_ore_kwh === "number" && p.grid_tariff_ore_kwh >= 0) {
        this._gridTariff = p.grid_tariff_ore_kwh;
      }
      this.update();
    } catch (e) { /* defaults are fine */ }
  }

  async _loadPrices() {
    try {
      const now = Date.now();
      // The API filters on the slot's START time, so asking from `now`
      // drops the slot we are currently inside — and that is the one the
      // Now tile reports. Ask an hour back, then keep only slots that
      // haven't finished yet.
      const r = await apiFetch(
        `/api/prices?since_ms=${now - 3600_000}&until_ms=${now + HORIZON_H * 3600_000}`);
      const j = await r.json();
      this._items = (j && Array.isArray(j.items))
        ? j.items.map((it) => ({
            tsMs: Number(it.slot_ts_ms) || 0,
            lenMin: Number(it.slot_len_min) || 60,
            spot: Number(it.spot_ore_kwh) || 0,
          }))
          .filter((it) => it.tsMs + it.lenMin * 60_000 > now)
          .sort((a, b) => a.tsMs - b.tsMs)
        : null;
    } catch (e) {
      this._items = null;
    }
    this.update();
  }

  _totalFor(item) {
    return consumerTotalOre(item.spot, this._gridTariff, this._vatPct);
  }

  render() {
    const head = (meta) => `
      <div class="head">
        <div>
          <div class="label">Prices vs average</div>
          <div class="meta">${meta}</div>
        </div>
        <a class="more" href="#energy">All prices →</a>
      </div>
    `;
    const items = this._items || [];
    // Two slots is the floor for a mean to say anything about spread.
    if (items.length < 2) {
      return head(`next ${HORIZON_H} h`) + `<div class="empty">No price data available.</div>`;
    }
    const totals = items.map((it) => this._totalFor(it));
    const mean = totals.reduce((a, b) => a + b, 0) / totals.length;
    const best = bestBlock(items, totals, BLOCK_H, "min");
    const worst = bestBlock(items, totals, BLOCK_H, "max");
    const nowIdx = indexAtNow(items);
    // Slots are filtered to "not finished yet", so index 0 is the current
    // slot whenever the clock has already entered it — and the next one
    // otherwise, which is the honest answer to "what does it cost now".
    const nowTotal = totals[nowIdx >= 0 ? nowIdx : 0];

    return head(`next ${HORIZON_H} h · avg ${roundOre(mean)} öre`) +
      this._renderTiles({ mean, best, worst, nowTotal }) +
      this._renderChart(items, totals, mean);
  }

  _renderTiles({ mean, best, worst, nowTotal }) {
    // Bare "+73%" rather than "+73% vs avg": the header already says
    // "Prices vs average" and the baseline is labelled with the number, so
    // repeating it on every tile only cost width.
    const rel = (v) => {
      const pct = mean !== 0 ? ((v - mean) / Math.abs(mean)) * 100 : 0;
      return `${pct >= 0 ? "+" : "−"}${Math.abs(pct).toFixed(0)}%`;
    };
    const nowCls = nowTotal == null ? "" : nowTotal < mean ? "cheap" : "dear";
    const nowArrow = nowTotal == null ? "" : nowTotal < mean ? "↓" : "↑";
    const nowTile = `
      <div class="tile ${nowCls}">
        <div class="tile-label"><span class="tile-arrow" aria-hidden="true">${nowArrow}</span>Now</div>
        <div class="tile-when">${nowTotal == null ? "—" : roundOre(nowTotal) + " öre"}</div>
        <div class="tile-price">${nowTotal == null ? "no slot" : escapeHtml(rel(nowTotal))}</div>
      </div>`;
    const blockTile = (b, cls, arrow, label) => {
      if (!b) return `<div class="tile"><div class="tile-label">${label}</div>
        <div class="tile-when">—</div><div class="tile-price">not enough data</div></div>`;
      return `
      <div class="tile ${cls}">
        <div class="tile-label"><span class="tile-arrow" aria-hidden="true">${arrow}</span>${label}</div>
        <div class="tile-when">${fmtClock(b.startMs)}–${fmtClock(b.endMs)}</div>
        <div class="tile-price">${roundOre(b.mean)} öre · ${escapeHtml(rel(b.mean))}</div>
      </div>`;
    };
    return `
      <div class="tiles">
        ${nowTile}
        ${blockTile(best,  "cheap", "↓", `Cheapest ${BLOCK_H} h`)}
        ${blockTile(worst, "dear",  "↑", `Priciest ${BLOCK_H} h`)}
      </div>
    `;
  }

  _renderChart(items, totals, mean) {
    // viewBox is set to the card's measured width so one user unit is one
    // CSS pixel. With a fixed 1000-unit box the whole drawing — text
    // included — scaled with the card, and axis labels shrank to a few
    // pixels in a narrow column. Now font sizes mean what they say at
    // every width. 640 is the pre-measurement fallback for the first paint.
    const W = this._width > 0 ? this._width : 640;
    const small = W < 480;
    const H = small ? 118 : 104;
    const fsAxis = 10;
    const pad = small
      ? { t: 10, r: 4, b: 18, l: 4 }
      : { t: 12, r: 6, b: 20, l: 6 };
    const plotW = W - pad.l - pad.r;
    const plotH = H - pad.t - pad.b;
    const n = items.length;
    const barW = plotW / n;
    this._geom = { padL: pad.l, plotW, n, W };

    // One öre is the same number of pixels above and below the line, so a
    // 20 öre saving and a 20 öre surcharge read as equal. The line itself
    // is placed by the ratio of the two extremes rather than at the
    // geometric middle: a day that runs 100 öre over average but only 40
    // under would otherwise leave the bottom half empty and squash every
    // cheap slot into a stub — and "when is it cheap" is half the question.
    const devs = totals.map((t) => t - mean);
    const up = Math.max(...devs, 0);
    const down = Math.max(...devs.map((d) => -d), 0);
    const span = Math.max(up + down, 1);
    const midY = pad.t + (up / span) * plotH;
    const devToPx = (d) => (d / span) * plotH;

    const bars = items.map((it, i) => {
      const d = devs[i];
      const h = Math.max(1.5, Math.abs(devToPx(d)));
      const y = d >= 0 ? midY - h : midY;
      const x = pad.l + i * barW;
      // 1px gap between fills; radius only on the end away from the
      // baseline, and only when the bar is wide enough to show it.
      const w = Math.max(0.6, barW - 1);
      const r = Math.min(2, w / 2, h / 2);
      // Slots within 5 % of the span of the mean are the "neither" case
      // and stay neutral rather than being forced onto one side.
      const near = Math.abs(d) < span * 0.05;
      const fill = near ? "var(--fg-muted)" : d >= 0 ? "var(--red-e)" : "var(--green-e)";
      return `<rect x="${(x + 0.5).toFixed(2)}" y="${y.toFixed(2)}"
                    width="${w.toFixed(2)}" height="${h.toFixed(2)}" rx="${r.toFixed(2)}"
                    fill="${fill}" opacity="0.85" data-idx="${i}" />`;
    }).join("");

    // Baseline — a bare rule. The average it represents is named in the
    // header instead of floating on a plate inside the plot: bars reach the
    // line from both sides at unpredictable places, so any in-plot label
    // eventually lands on one.
    const baseline = `
      <line x1="${pad.l}" x2="${pad.l + plotW}" y1="${midY}" y2="${midY}"
            stroke="var(--fg-muted)" stroke-width="1.5" opacity="0.9" />`;

    // Hour ticks — enough to place a window, few enough to stay out of the
    // way. The step is chosen from the width that is actually available per
    // label, so ticks thin out in a narrow card instead of crowding.
    const minTickPx = 62;
    const tickStepH = [3, 6, 12].find((h) => (h / HORIZON_H) * plotW >= minTickPx) || 12;
    const tickStepMs = tickStepH * 3600_000;
    const startT = items[0].tsMs;
    const endT = items[n - 1].tsMs + items[n - 1].lenMin * 60_000;
    const ticks = [];
    for (let t = ceilTo(startT, tickStepMs); t < endT; t += tickStepMs) {
      const x = pad.l + ((t - startT) / (endT - startT)) * plotW;
      // Skip a tick whose label would be clipped by either edge.
      if (x < pad.l + 16 || x > pad.l + plotW - 16) continue;
      ticks.push(`<text x="${x.toFixed(1)}" y="${H - 6}" text-anchor="middle"
                        fill="var(--fg-label)" font-family="var(--mono)" font-size="${fsAxis}">${fmtClock(t)}</text>`);
    }

    // No "now" marker: the window starts at the current slot, so it would
    // sit on the left edge every time and say nothing. The Now tile above
    // carries the current price instead.

    const hits = items.map((_, i) => {
      const x = pad.l + i * barW;
      return `<rect x="${x.toFixed(2)}" y="${pad.t}" width="${barW.toFixed(2)}" height="${plotH}"
                    fill="transparent" data-idx="${i}" />`;
    }).join("");

    // aria-label carries the finding for anyone who can't see the strip;
    // the tiles above carry it visually.
    const label = `Price relative to the ${HORIZON_H} hour average of ` +
      `${roundOre(mean)} öre per kWh. Bars above the line are dearer than average, below are cheaper.`;

    return `
      <div class="chart-wrap">
        <svg viewBox="0 0 ${W} ${H}" height="${H}"
             role="img" aria-label="${escapeHtml(label)}">
          ${bars}
          ${baseline}
          ${ticks.join("")}
          ${hits}
        </svg>
        <div class="tip" data-tip>
          <div class="tip-when" data-tip-when>—</div>
          <div data-tip-price>—</div>
          <div class="tip-dev" data-tip-dev>—</div>
        </div>
      </div>`;
  }

  afterRender() {
    const svg = this.shadowRoot.querySelector("svg");
    const tip = this.shadowRoot.querySelector("[data-tip]");
    if (!svg || !tip || !this._items) return;
    const show = (i, localX) => {
      const items = this._items;
      const item = items[i];
      if (!item) return;
      const totals = items.map((it) => this._totalFor(it));
      const mean = totals.reduce((a, b) => a + b, 0) / totals.length;
      const d = totals[i] - mean;
      tip.querySelector("[data-tip-when]").textContent =
        `${fmtClock(item.tsMs)}–${fmtClock(item.tsMs + item.lenMin * 60_000)}`;
      tip.querySelector("[data-tip-price]").textContent = `${roundOre(totals[i])} öre/kWh`;
      tip.querySelector("[data-tip-dev]").textContent =
        `${d >= 0 ? "+" : "−"}${roundOre(Math.abs(d))} vs avg`;
      tip.style.left = localX + "px";
      tip.style.top = "0px";
      tip.classList.add("visible");
    };
    const hide = () => tip.classList.remove("visible");

    svg.addEventListener("mousemove", (e) => {
      const t = e.target.closest("[data-idx]");
      if (!t) { hide(); return; }
      const i = Number(t.dataset.idx);
      if (!Number.isFinite(i)) { hide(); return; }
      show(i, e.clientX - svg.getBoundingClientRect().left);
    });
    svg.addEventListener("mouseleave", hide);
    svg.addEventListener("touchstart", (e) => {
      if (e.touches.length !== 1) return;
      const i = this._idxFromClientX(e.touches[0].clientX);
      if (i >= 0) show(i, e.touches[0].clientX - svg.getBoundingClientRect().left);
    }, { passive: true });
    svg.addEventListener("touchend", hide);
  }

  _idxFromClientX(clientX) {
    const svg = this.shadowRoot.querySelector("svg");
    if (!svg || !this._geom) return -1;
    const rect = svg.getBoundingClientRect();
    if (!rect.width) return -1;
    const vbX = ((clientX - rect.left) / rect.width) * this._geom.W;
    const i = Math.floor((vbX - this._geom.padL) / (this._geom.plotW / this._geom.n));
    return i >= 0 && i < this._geom.n ? i : -1;
  }
}

function indexAtNow(items) {
  const now = Date.now();
  for (let i = 0; i < items.length; i++) {
    const start = items[i].tsMs;
    if (now >= start && now < start + items[i].lenMin * 60_000) return i;
  }
  return -1;
}

function fmtClock(tsMs) {
  const d = new Date(tsMs);
  return d.getHours().toString().padStart(2, "0") + ":" +
         d.getMinutes().toString().padStart(2, "0");
}

function roundOre(v) {
  if (Math.abs(v) >= 100) return v.toFixed(0);
  if (Math.abs(v) >= 10) return v.toFixed(1);
  return v.toFixed(2);
}

function ceilTo(t, step) {
  return Math.ceil(t / step) * step;
}

function escapeHtml(s) {
  return String(s).replace(/[&<>"']/g, (c) => ({
    "&": "&amp;", "<": "&lt;", ">": "&gt;", "\"": "&quot;", "'": "&#39;",
  }[c]));
}

customElements.define("ftw-price-outlook", FtwPriceOutlook);
