// <ftw-energy-flow> — hero diagram for /next.
//
// Layout (site convention: +ve = INTO the site):
//
//   [pv₀] [pv₁] [pv₂]            TOP row  — solar cluster, horizontal
//              │
//              ▼
//   [grid] ───►[HOUSE]───►[bat₀]  MIDDLE  — grid left, battery column right
//                      │ [bat₁]
//                      │ [bat₂]
//                      ▼
//              [ev]                BOTTOM — EV single slot
//
// Flow edges carry two simultaneous animations:
//   1. A dashed, blurred stroke whose dash-offset animates via CSS —
//      gives a "current flowing" feel without redrawing any DOM.
//   2. Three particle circles riding the edge via <animateMotion>, spaced
//      by 1/3 of the cycle so the flow looks continuous. Speed scales
//      inversely with |kW| / maxKw — more power, faster particles.
// Both effects are skipped when |kW| < 50 W so idle edges read as still.
//
// Update pattern — next-app.js calls `setReadings(...)` each status poll
// with the full multi-driver payload. The component never introspects
// /api/status itself; keep the transformation in next-app.js so all
// driver-name logic stays in one place.
//
//   flow.setReadings({
//     grid: 0.5, load: 1.2, ev: 0,
//     pvs:       [{ name: "solaredge", kw: -5.3 }],
//     batteries: [{ name: "pixii",     kw: 2.2, soc: 78 }],
//   });

import { FtwElement } from "./ftw-element.js";

const W = 1000, H = 530;
const CX = W / 2, CY = H / 2;
const HUB_R = 64;
const BOX_W = 170, BOX_H = 78;
const GAP_H = 34;   // horizontal gap between stacked PV boxes
const GAP_V = 14;   // vertical gap between stacked battery boxes
// TOP_Y / BOT_Y picked so the vertical beam is ~107 px between box edge
// and hub edge — 30% longer than the earlier 82 px. Extra room lets
// the particle fountain spiral visibly into the hub instead of being
// crammed into a short run.
const TOP_Y = 55;
const BOT_Y = H - 55;
const LEFT_X = 150;
const RIGHT_X = W - 150;

class FtwEnergyFlow extends FtwElement {
  static styles = `
    :host {
      display: block;
      background: linear-gradient(180deg,
        var(--hero-bg-top) 0%,
        var(--hero-bg-bot) 100%);
      border: 1px solid var(--line);
      border-radius: var(--radius-lg);
      padding: 20px 28px 14px;
      position: relative;
      overflow: hidden;
    }
    :host::before {
      content: '';
      position: absolute;
      inset: 0;
      background: radial-gradient(circle at 50% 46%,
        var(--hero-glow-a), transparent 60%);
      pointer-events: none;
    }
    .head {
      display: flex;
      justify-content: space-between;
      align-items: flex-end;
      margin-bottom: 6px;
      position: relative;
    }
    .eyebrow {
      font-size: 10px;
      font-family: var(--mono);
      color: var(--fg-muted);
      text-transform: uppercase;
      letter-spacing: 0.12em;
    }
    .title {
      font-family: var(--sans);
      font-size: 22px;
      font-weight: 700;
      letter-spacing: -0.02em;
      margin-top: 2px;
      color: var(--fg);
    }
    .legend {
      display: flex;
      gap: 18px;
      font-size: 11px;
      color: var(--fg-dim);
      font-family: var(--mono);
      flex-wrap: wrap;
      justify-content: flex-end;
    }
    .legend i {
      display: inline-block;
      width: 10px;
      height: 10px;
      border-radius: 2px;
      margin-right: 6px;
      vertical-align: middle;
    }
    svg {
      width: 100%;
      height: 490px;
      display: block;
    }
    /* SVG text classes — font-size values are in viewBox units (the SVG
       scales with container width via preserveAspectRatio), so at narrow
       viewports the default sizes render too small. Media queries below
       bump them back into legible range on small screens. */
    .sv-node-title { font-family: var(--mono); font-size: 10px; font-weight: 500; letter-spacing: 0.08em; }
    .sv-node-value { font-family: var(--mono); font-size: 20px; font-weight: 700; font-variant-numeric: tabular-nums; letter-spacing: -0.01em; }
    .sv-node-sub   { font-family: var(--mono); font-size: 10px; letter-spacing: 0.04em; }
    .sv-hub-value  { font-family: var(--mono); font-size: 18px; font-weight: 700; font-variant-numeric: tabular-nums; }
    .sv-hub-label  { font-family: var(--mono); font-size: 9px; letter-spacing: 0.1em; }
    /* One dash cycle advances by exactly (dash + gap). The fwd/rev pair
       keeps direction declarative — we flip the animation-name, not the
       path, so swapping a source→sink edge (grid export, battery
       discharge) is a one-token change at render time. */
    @keyframes ef-dash-fwd { to { stroke-dashoffset: -48; } }
    @keyframes ef-spin { to { transform: rotate(360deg); } }
    .ring {
      transform-box: fill-box;
      transform-origin: center;
      animation: ef-spin 24s linear infinite;
    }
    @media (max-width: 900px) {
      :host { padding: 14px 12px 10px; }
      svg { height: 465px; }
      .sv-node-title { font-size: 13px; }
      .sv-node-value { font-size: 24px; }
      .sv-node-sub   { font-size: 13px; }
      .sv-hub-value  { font-size: 22px; }
      .sv-hub-label  { font-size: 11px; }
    }
    @media (max-width: 600px) {
      svg { height: 420px; }
      .sv-node-title { font-size: 18px; }
      .sv-node-value { font-size: 30px; }
      .sv-node-sub   { font-size: 16px; }
      .sv-hub-value  { font-size: 28px; }
      .sv-hub-label  { font-size: 14px; }
      .title         { font-size: 26px; }
      .legend        { font-size: 13px; }
    }
  `;

  constructor() {
    super();
    // Start with empty clusters; render shows placeholder slots until the
    // first setReadings() push arrives from next-app.js.
    this._readings = {
      grid: 0, load: 0, ev: 0,
      pvs: [], batteries: [],
    };
    // JS-driven particle system — one rAF loop animates every "electron"
    // independently. Each particle has its own amp/phase/freq/speed plus
    // a low-frequency 2D noise term, so even at high particle counts
    // the stream looks like a turbulent spray, not a threaded screw.
    this._rafId = null;
    this._particles = [];
    this._bound = [];
    this._snapshot = null;
    // Anchored once at construction so `t = now - tickStart` is on the
    // same timeline for the entire component lifetime. Resetting it
    // each afterRender would make restored bornAt values (from the
    // snapshot Map) refer to the old timeline — particles would jump.
    this._tickStart = performance.now();
    // Compact layout kicks in on narrow viewports — shortens beams so
    // the node boxes cluster closer to the hub, leaving more room for
    // the enlarged text. Kept in sync with the (max-width: 600px) CSS
    // breakpoint via matchMedia so fonts and geometry flip together.
    this._mq = typeof window !== "undefined" && window.matchMedia
      ? window.matchMedia("(max-width: 600px)")
      : null;
    this._compact = !!(this._mq && this._mq.matches);
    this._onMqChange = (e) => {
      this._compact = e.matches;
      this.update();
    };
    if (this._mq) {
      this._mq.addEventListener("change", this._onMqChange);
    }
  }

  disconnectedCallback() {
    if (this._rafId) cancelAnimationFrame(this._rafId);
    this._rafId = null;
    this._particles = [];
    if (this._mq) {
      this._mq.removeEventListener("change", this._onMqChange);
    }
  }

  // Bulk setter — preferred update path. Scalars merge; arrays replace
  // when provided. Passing `undefined` for an array leaves the previous
  // cluster intact (useful during transient /api/status errors).
  setReadings(r) {
    if (r.grid != null) this._readings.grid = r.grid;
    if (r.load != null) this._readings.load = r.load;
    if (r.ev   != null) this._readings.ev   = r.ev;
    if (Array.isArray(r.pvs))       this._readings.pvs       = r.pvs;
    if (Array.isArray(r.batteries)) this._readings.batteries = r.batteries;
    this.update();
  }

  // Override FtwElement.update so we can snapshot particle motion
  // state BEFORE the base class wipes the shadow DOM. afterRender()
  // restores the state onto the freshly-bound particles keyed by
  // `_key`. Particles that survive across renders never stutter; new
  // particles (added because kW grew) warm up normally; dropped ones
  // just vanish. No per-2s reset.
  update() {
    if (this._bound && this._bound.length) {
      const snap = new Map();
      for (const b of this._bound) {
        if (b.p._key) {
          snap.set(b.p._key, {
            bornAt: b.p.bornAt,
            sx: b.p.sx, sy: b.p.sy,
            vx: b.p.vx, vy: b.p.vy,
            life: b.p.life,
            phase: b.p.phase,
            omega: b.p.omega,
            damp:  b.p.damp,
            amp:   b.p.amp,
          });
        }
      }
      this._snapshot = snap;
    }
    super.update();
  }

  // Called by FtwElement after each render() replaces the shadow DOM.
  // We cancel any in-flight rAF, bind the freshly-rendered <circle>
  // elements to the particle-param list `render()` just built, and
  // start a new animation loop. The loop is a single rAF that iterates
  // every particle — cheaper than SMIL when you have hundreds of them,
  // and gives us per-frame noise terms SMIL can't express.
  afterRender() {
    if (this._rafId) {
      cancelAnimationFrame(this._rafId);
      this._rafId = null;
    }
    const nodes = this.shadowRoot.querySelectorAll('.ef-p');
    if (!nodes.length || !this._particles.length) return;
    // Wire each DOM node to its param slot. `render()` assigned indices
    // via `data-i`; we trust those rather than node order in case the
    // browser reorders subtree attribute-only nodes in the future.
    const bound = [];
    nodes.forEach((n) => {
      const i = +n.dataset.i;
      const p = this._particles[i];
      if (p) bound.push({ el: n, p });
    });
    if (!bound.length) {
      this._bound = [];
      return;
    }
    // Carry per-particle motion state across re-renders so the fountain
    // doesn't visibly reset every 2 s. update() snapshots prior state
    // keyed on `_key`; here we copy it back onto the new param list
    // and skip warm-up for any particle that already existed.
    if (this._snapshot && this._snapshot.size) {
      for (const b of bound) {
        const prev = this._snapshot.get(b.p._key);
        if (prev) {
          b.p.bornAt = prev.bornAt;
          b.p.sx = prev.sx; b.p.sy = prev.sy;
          b.p.vx = prev.vx; b.p.vy = prev.vy;
          b.p.life = prev.life;
          b.p.phase = prev.phase;
          b.p.omega = prev.omega;
          b.p.damp  = prev.damp;
          b.p.amp   = prev.amp;
          b.p._warmUp = false;
        }
      }
      this._snapshot = null;
    }
    this._bound = bound;
    const tick = (now) => {
      const t = (now - this._tickStart) / 1000;
      for (let k = 0; k < bound.length; k++) {
        const b = bound[k];
        const p = b.p;
        let age = t - p.bornAt;
        if (age >= p.life || p.life === 0) {
          rollLife(p, t);
          // First-ever spawn: backdate bornAt uniformly across the
          // pool's lifetime so particles are spread evenly instead of
          // bursting together. p._warmUpIdx is in (0, 1), so this
          // seeds the fountain with a steady state.
          if (p._warmUp) {
            p.bornAt = t - p._warmUpIdx * p.life;
            p._warmUp = false;
          }
          age = t - p.bornAt;
        }
        // Along-path progress: linear travel from spawn toward target.
        // No easing — real electrons don't decelerate.
        const along = p.vx * age;              // along-vector component
        const alongY = p.vy * age;
        // Perpendicular offset: damped harmonic oscillator. This is
        // the "gravity circling the beam" effect — a spring pulls the
        // particle toward the beam centerline with angular frequency
        // omega, while γ damps amplitude over time so particles
        // spiral IN as they approach the target.
        //   perp(t) = A * e^(−γt) * cos(ωt + φ)
        const envelope = Math.exp(-p.damp * age);
        const wave = Math.cos(p.omega * age + p.phase);
        const perp = p.amp * envelope * wave;
        const x = p.sx + along + p.perpX * perp;
        const y = p.sy + alongY + p.perpY * perp;
        // Opacity is fixed — set at render time, never touched here.
        // Size variance (per-particle `radius`) replaces the old
        // opacity pulse as the "texture" cue.
        b.el.setAttribute('cx', x.toFixed(1));
        b.el.setAttribute('cy', y.toFixed(1));
      }
      this._rafId = requestAnimationFrame(tick);
    };
    this._rafId = requestAnimationFrame(tick);
  }

  render() {
    const { grid, load, ev } = this._readings;

    // Show one placeholder box per side when no drivers report — keeps
    // the layout stable during first paint + disables flow animation.
    const pvList = this._readings.pvs.length
      ? this._readings.pvs
      : [{ name: "", kw: 0, placeholder: true }];
    const batList = this._readings.batteries.length
      ? this._readings.batteries
      : [{ name: "", kw: 0, soc: null, placeholder: true }];

    // Compact mode pulls the four cardinal anchors toward the hub so the
    // beams are roughly half-length — leaves the box cluster tight and
    // gives the enlarged text on narrow viewports room to breathe
    // without overflowing the viewBox.
    const topY    = this._compact ? 110      : TOP_Y;
    const botY    = this._compact ? H - 110  : BOT_Y;
    const leftX   = this._compact ? 230      : LEFT_X;
    const rightX  = this._compact ? W - 230  : RIGHT_X;

    const pvPositions  = clusterH(pvList.length,  CX,      topY,  BOX_W, GAP_H);
    const batPositions = clusterV(batList.length, rightX,  CY,    BOX_H, GAP_V);
    const gridPos = { x: leftX, y: CY };
    const evPos   = { x: CX,    y: botY };

    // Build all edges: each entry carries geometry + magnitude + direction.
    const edges = [];
    pvList.forEach((pv, i) => {
      const magnitude = Math.max(0, -pv.kw);
      edges.push(edge(
        `pv-${i}`,
        pvPositions[i], gridPos /* unused for side=top */,
        "top", +1,
        magnitude,
        "var(--amber)",
        !pv.placeholder && magnitude > 0.05,
      ));
    });
    edges.push(edge(
      "grid",
      gridPos, gridPos,
      "left",
      grid >= 0 ? +1 : -1,
      Math.abs(grid),
      grid >= 0 ? "var(--red-e)" : "var(--green-e)",
      Math.abs(grid) > 0.05,
    ));
    batList.forEach((bat, i) => {
      edges.push(edge(
        `bat-${i}`,
        batPositions[i], batPositions[i],
        "right",
        bat.kw >= 0 ? +1 : -1,
        Math.abs(bat.kw),
        "var(--cyan)",
        !bat.placeholder && Math.abs(bat.kw) > 0.05,
      ));
    });
    edges.push(edge(
      "ev",
      evPos, evPos,
      "bottom", +1,
      Math.max(0, ev),
      "var(--white-s)",
      ev > 0.05,
    ));

    const maxKw = Math.max(0.5, ...edges.map(e => e.kw));
    // Stash the particle-param list on the instance so afterRender()
    // can pick it up once the shadow DOM is in place. Re-assigning
    // here (instead of pushing) ensures a re-render starts from a
    // clean slate — no stale particles from the previous frame.
    this._particles = [];
    const edgesSvg = edges.map(e => renderEdge(e, maxKw, this._particles)).join("");

    const pvNodes = pvList.map((pv, i) =>
      renderNode({
        pos: pvPositions[i],
        // Solar shows POSITIVE kW — sign flip is display-only, all
        // internal state (chartHistory, math) stays on site convention.
        value: pv.placeholder ? "—" : fmtKw(-pv.kw),
        title: labelWithName("SOLAR", pv.name, pvList.length),
        sub: pv.placeholder ? "no data" : (pv.kw < -0.05 ? "generating" : "idle"),
        color: !pv.placeholder && pv.kw < -0.05 ? "var(--amber)" : "var(--fg-muted)",
        side: "top", icon: "sun",
      })
    ).join("");

    const gridNode = renderNode({
      pos: gridPos,
      value: fmtKw(grid),
      title: "GRID",
      sub: Math.abs(grid) < 0.05 ? "balanced" : (grid >= 0 ? "importing" : "exporting"),
      color: Math.abs(grid) < 0.05 ? "var(--fg-muted)" :
             (grid >= 0 ? "var(--red-e)" : "var(--green-e)"),
      side: "left", icon: "grid",
    });

    const batNodes = batList.map((bat, i) =>
      renderNode({
        pos: batPositions[i],
        value: bat.placeholder ? "—" : fmtKw(bat.kw),
        title: labelWithName(
          "BATTERY", bat.name, batList.length,
          bat.soc != null && !bat.placeholder ? ` · ${Math.round(bat.soc)}%` : "",
        ),
        sub: bat.placeholder ? "no data" :
             (Math.abs(bat.kw) < 0.05 ? "idle" :
              (bat.kw >= 0 ? "charging" : "discharging")),
        color: bat.placeholder ? "var(--fg-muted)" : "var(--cyan)",
        side: "right", icon: "bat",
        soc: bat.placeholder ? null : bat.soc,
      })
    ).join("");

    const evNode = renderNode({
      pos: evPos,
      value: fmtKw(ev),
      title: "EV CHARGER",
      sub: ev > 0.05 ? "charging" : "idle",
      color: ev > 0.05 ? "var(--green-e)" : "var(--white-s)",
      side: "bottom", icon: "ev",
    });

    return `
      <div class="head">
        <div>
          <div class="eyebrow">Live flow</div>
          <div class="title">Energy balance</div>
        </div>
        <div class="legend">
          <span><i style="background:var(--amber)"></i>PV</span>
          <span><i style="background:var(--red-e)"></i>Import</span>
          <span><i style="background:var(--green-e)"></i>Export</span>
          <span><i style="background:var(--cyan)"></i>Battery</span>
        </div>
      </div>
      <svg viewBox="0 0 ${W} ${H}" preserveAspectRatio="xMidYMid meet" aria-hidden="true">
        <defs>
          <radialGradient id="ef-hub" cx="50%" cy="50%" r="50%">
            <stop offset="0%" stop-color="oklch(0.85 0.18 var(--accent-hue))" stop-opacity="0.55"/>
            <stop offset="70%" stop-color="oklch(0.5 0.12 var(--accent-hue))" stop-opacity="0.04"/>
            <stop offset="100%" stop-color="transparent"/>
          </radialGradient>
          <filter id="ef-soft">
            <feGaussianBlur stdDeviation="2.5" result="b"/>
            <feMerge><feMergeNode in="b"/><feMergeNode in="SourceGraphic"/></feMerge>
          </filter>
          <!-- Wide bloom for the outer railgun aura. stdDeviation=5 gives
               a 10 px halo which is enough to read as a glow without
               washing adjacent nodes. The filter region is 200% of the
               bbox so the bloom isn't clipped at edge endpoints. -->
          <filter id="ef-bloom" x="-50%" y="-50%" width="200%" height="200%">
            <feGaussianBlur stdDeviation="5" result="b"/>
            <feMerge><feMergeNode in="b"/><feMergeNode in="SourceGraphic"/></feMerge>
          </filter>
        </defs>

        <circle cx="${CX}" cy="${CY}" r="200" fill="url(#ef-hub)"/>

        ${edgesSvg}

        <!-- HOUSE / hub: load reading lives here -->
        <g>
          <circle cx="${CX}" cy="${CY}" r="${HUB_R}"
                  fill="var(--hero-house-fill)"
                  stroke="var(--hero-house-stroke)" stroke-width="1.5"/>
          <circle class="ring" cx="${CX}" cy="${CY}" r="${HUB_R - 8}"
                  fill="none"
                  stroke="var(--hero-house-ring)" stroke-width="1"
                  stroke-dasharray="2 4"/>
          <g transform="translate(${CX - 16}, ${CY - 30})"
             stroke="var(--hero-house-stroke)" stroke-width="1.6"
             fill="none" stroke-linecap="round" stroke-linejoin="round">
            <path d="M2 16 L16 3 L30 16 L30 26 L2 26 Z"/>
            <path d="M12 26 V18 H20 V26"/>
          </g>
          <text x="${CX}" y="${CY + 16}" text-anchor="middle"
                fill="var(--hero-load-text)" class="sv-hub-value">
            ${fmtKw(load)}
          </text>
          <text x="${CX}" y="${CY + 32}" text-anchor="middle"
                fill="var(--hero-label-text)" class="sv-hub-label">
            CONSUMING
          </text>
        </g>

        ${pvNodes}
        ${gridNode}
        ${batNodes}
        ${evNode}
      </svg>
    `;
  }
}

// ---------- geometry + edge helpers ----------

// Spread N items along a horizontal axis, centered on (anchorX, fixedY).
// Spacing is box-width + gap so boxes never overlap, even at N=3+.
function clusterH(n, anchorX, fixedY, boxW, gap) {
  const stride = boxW + gap;
  return Array.from({ length: n }, (_, i) => ({
    x: anchorX - ((n - 1) / 2) * stride + i * stride,
    y: fixedY,
  }));
}

function clusterV(n, fixedX, anchorY, boxH, gap) {
  const stride = boxH + gap;
  return Array.from({ length: n }, (_, i) => ({
    x: fixedX,
    y: anchorY - ((n - 1) / 2) * stride + i * stride,
  }));
}

// Compute the two endpoints of an edge given a box position and which
// side of the hub it lives on. `dir > 0` means energy flows INTO the hub
// (displayed as particles moving from box → hub). dir < 0 reverses the
// endpoints so animateMotion runs box-ward.
function edge(id, pos, _unused, side, dir, kw, color, active) {
  let from, to;
  if (side === "top") {
    from = { x: pos.x, y: pos.y + BOX_H / 2 };
    to   = { x: CX,    y: CY - HUB_R };
  } else if (side === "bottom") {
    from = { x: CX,    y: CY + HUB_R };
    to   = { x: pos.x, y: pos.y - BOX_H / 2 };
  } else if (side === "left") {
    from = { x: pos.x + BOX_W / 2, y: pos.y };
    to   = { x: CX - HUB_R,        y: CY };
  } else { // right
    from = { x: CX + HUB_R,        y: CY };
    to   = { x: pos.x - BOX_W / 2, y: pos.y };
  }
  // Swap when energy flows the "unusual" way for that side (grid export,
  // battery discharge). animateMotion always walks the path start→end,
  // so reversing from/to flips the particle direction with no extra CSS.
  if (dir < 0) [from, to] = [to, from];
  return { id, from, to, kw, color, active };
}

// Render a single edge as beam paths + plain particle circles. Each
// particle is POSITIONED by the rAF loop in afterRender, not by SMIL —
// so every electron has its own independent amp/phase/freq plus a 2D
// noise term, and at high kW the stream looks genuinely chaotic
// instead of resolving into visible screw threads.
function renderEdge(e, _maxKw, collect) {
  const width = clamp(1.5 + e.kw * 1.8, 1.5, 16);
  const dx = e.to.x - e.from.x;
  const dy = e.to.y - e.from.y;
  const len = Math.hypot(dx, dy);
  const straightD = `M ${e.from.x} ${e.from.y} L ${e.to.x} ${e.to.y}`;
  if (!e.active || len < 1) {
    return `<path d="${straightD}" stroke="var(--hero-line-base)" stroke-width="${width.toFixed(1)}" fill="none" stroke-linecap="round"/>`;
  }
  // Railgun beam — bloom + body + white core. Opacities nudged up so
  // the beam reads as a hot wire behind the particle spray, not a
  // ghost. Still balanced so particles stay the primary signal.
  const beam =
    `<path d="${straightD}" stroke="${e.color}" stroke-width="${(width * 2.6).toFixed(1)}" ` +
      `fill="none" stroke-linecap="round" opacity="0.22" filter="url(#ef-bloom)"/>` +
    `<path d="${straightD}" stroke="${e.color}" stroke-width="${width.toFixed(1)}" ` +
      `fill="none" stroke-linecap="round" opacity="0.45"/>` +
    `<path d="${straightD}" stroke="var(--white-s)" stroke-width="${(width * 0.35).toFixed(1)}" ` +
      `fill="none" stroke-linecap="round" opacity="0.55"/>`;

  // Fountain emitter: each particle has its own spawn jitter, velocity,
  // lifetime, lateral wobble, and opacity envelope — reset on respawn.
  // No shared path; no modulo-of-time-loop. When a particle's life
  // expires the rAF loop re-rolls its parameters and sends it again
  // from the source box, so the visual is a continuous spray with no
  // pattern that the eye can latch onto.
  const dirX = dx / len;
  const dirY = dy / len;
  const perpX = -dirY;
  const perpY =  dirX;
  // Base speed in px/s. A 250 px edge at 80 px/s takes ~3 s end-to-end —
  // calm enough to be readable but not sluggish.
  const baseSpeed = 80;
  // Per-kW particle count. Pool is continuously alive — particles
  // respawn the instant life ends. 75 is the ceiling per beam at
  // ~5 kW and above; min 10 so trickle flows still read as a stream.
  const count = clamp(Math.round(e.kw * 15), 10, 75);

  let particleSvg = "";
  for (let i = 0; i < count; i++) {
    // Static per-particle geometry. The dynamic bits (jitter, speed,
    // life, wobble, born-at) are rolled on every respawn in the rAF
    // loop — see rollLife() there.
    const params = {
      // Static geometry (frozen at edge-render time).
      fx: e.from.x, fy: e.from.y,
      dirX, dirY, perpX, perpY,
      len, baseSpeed,
      // Emission area half-width along the source box face — spawn
      // points are randomised within this interval each respawn.
      spread: 10,
      // Cone half-angle (radians) — particles deviate this much from
      // a straight line to the target. Narrow enough that the overall
      // flow direction is still obvious but wide enough that no two
      // particles trace identical arcs.
      cone: 0.18,
      // Dynamic fields — initialised below with random starting phases
      // so the fountain is already mid-flight on first paint instead
      // of bursting from zero.
      bornAt: 0,
      sx: 0, sy: 0,           // spawn point (after jitter)
      vx: 0, vy: 0,            // velocity vector (after cone + speed)
      life: 0,                 // seconds until respawn
      wobbleAmp: 0,
      wobbleFreq: 0,
      wobblePhase: 0,
      // Per-particle constants (not reset on respawn). Opacity is
      // baked into the circle's initial attribute and never touched
      // again — no per-frame fade. Size variance stands in for the
      // old pulse, giving the spray visual texture without animation.
      radius: 0.8 + Math.random() * 1.3,
      fixedOpacity: (0.75 + Math.random() * 0.25).toFixed(2),
    };
    // First tick triggers rollLife (life===0). `_warmUp` + uniform
    // warm-up index backdates the first bornAt so particles are
    // spread evenly across the pool's lifetime on initial paint —
    // not bunched into a burst. After the first spawn, each particle
    // respawns independently whenever its own life expires.
    params._warmUp = true;
    params._warmUpIdx = (i + 0.5) / count;
    params.bornAt = 0;
    const idx = collect.length;
    collect.push(params);
    // Stable key across re-renders so update() can carry particle
    // motion state forward — otherwise every /api/status poll (every
    // ~2s) wipes innerHTML and every electron resets to its spawn
    // point, which reads as a visible "jam + restart" tick.
    params._key = `${e.id}|${i}`;
    particleSvg +=
      `<circle class="ef-p" data-i="${idx}" cx="${e.from.x.toFixed(1)}" cy="${e.from.y.toFixed(1)}" ` +
      `r="${params.radius.toFixed(2)}" fill="${e.color}" opacity="${params.fixedOpacity}"/>`;
  }
  return beam + particleSvg;
}

// Respawn a particle — called when age >= life or on first tick.
// Re-rolls every dynamic parameter with Math.random() so each flight
// is unique. Gravity model: spawn with an angular-velocity spring
// around the beam line; damping γ is tuned so amplitude decays to
// ~10% by end-of-life, giving the "spirals into the target" feel.
function rollLife(p, now) {
  p.bornAt = now;
  // Spawn jitter along the source box face — spread perpendicular to
  // the beam direction so the fountain emits from a line segment, not
  // a point.
  const jitter = (Math.random() - 0.5) * 2 * p.spread;
  p.sx = p.fx + p.perpX * jitter;
  p.sy = p.fy + p.perpY * jitter;
  // Cone emission: small angular deviation from the straight-line
  // direction (±cone radians). Keeps the flow heading target-ward
  // while giving each particle its own trajectory.
  const coneOff = (Math.random() - 0.5) * 2 * p.cone;
  const c = Math.cos(coneOff), s = Math.sin(coneOff);
  // Rotate (dirX,dirY) by coneOff into this life's velocity vector.
  const speed = p.baseSpeed * (0.75 + Math.random() * 0.5);
  p.vx = (p.dirX * c - p.dirY * s) * speed;
  p.vy = (p.dirX * s + p.dirY * c) * speed;
  // Lifetime sized to roughly reach target (len / speed). Slight extra
  // randomness so particles don't all respawn in lockstep.
  p.life = (p.len / speed) * (0.9 + Math.random() * 0.25);
  // Spring/damping parameters. Heavy damping — particles stay glued
  // to the beam for most of the flight, with a short initial spiral
  // around the source and a long tight glide into the target.
  // damp ≈ 5.5/life drops amplitude to e^-5.5 (~0.4%) by end-of-life
  // and already to e^-2.75 (~6%) at the midpoint, so the second half
  // of the trip reads as "on the beam".
  p.omega = 3.5 + Math.random() * 4;
  p.damp  = 5.5 / p.life + Math.random() * 0.6;
  p.phase = Math.random() * Math.PI * 2;
  // Smaller initial radius to match — otherwise the early orbit flies
  // too far off the wire before gravity grabs it.
  p.amp = 2.5 + Math.random() * 3.5;
}

// Tiny xorshift — deterministic per-particle jitter so re-renders don't
// shuffle the stream. Returns a value in [0, 1).
function seedRand(seed) {
  let x = (seed + 0x9E3779B9) | 0;
  x ^= x << 13; x ^= x >>> 17; x ^= x << 5;
  return ((x >>> 0) / 4294967295);
}
function hashStr(s) {
  let h = 2166136261;
  for (let i = 0; i < s.length; i++) {
    h ^= s.charCodeAt(i); h = Math.imul(h, 16777619);
  }
  return h;
}

// ---------- nodes ----------

function renderNode({ pos, title, value, sub, color, side, icon, soc }) {
  const x = pos.x - BOX_W / 2;
  const y = pos.y - BOX_H / 2;
  // Accent stripe sits on the OUTER edge of each box (away from hub) so
  // the eye follows the colored line back toward the house.
  const stripe =
    side === "left"   ? { x,                 y,                 w: 3,     h: BOX_H } :
    side === "right"  ? { x: x + BOX_W - 3,  y,                 w: 3,     h: BOX_H } :
    side === "top"    ? { x,                 y,                 w: BOX_W, h: 3 }     :
                        { x,                 y: y + BOX_H - 3,  w: BOX_W, h: 3 };
  const socBar = soc != null ? `
    <rect x="${x + 14}" y="${y + 70}" width="${BOX_W - 28}" height="2.5" rx="1.25"
          fill="var(--hero-soc-track)"/>
    <rect x="${x + 14}" y="${y + 70}" width="${((BOX_W - 28) * (soc / 100)).toFixed(1)}" height="2.5" rx="1.25"
          fill="var(--cyan)"/>` : "";
  return `
    <g>
      <rect x="${x}" y="${y}" width="${BOX_W}" height="${BOX_H}" rx="12"
            fill="var(--hero-box-fill)" stroke="var(--hero-box-border)" stroke-width="1"/>
      <rect x="${stripe.x}" y="${stripe.y}" width="${stripe.w}" height="${stripe.h}" rx="1.5"
            fill="${color}"/>
      <text x="${x + 14}" y="${y + 20}"
            fill="var(--hero-label-text)" class="sv-node-title">
        ${escapeXml(title)}
      </text>
      <text x="${x + 14}" y="${y + 46}" fill="${color}" class="sv-node-value">
        ${value}
      </text>
      <text x="${x + 14}" y="${y + 64}"
            fill="var(--hero-sub-text)" class="sv-node-sub">
        ${escapeXml(sub)}
      </text>
      <g transform="translate(${x + BOX_W - 30}, ${y + 12})" opacity="0.55">
        ${iconGlyph(icon, color)}
      </g>
      ${socBar}
    </g>`;
}

// Title includes the driver name only when more than one of its kind
// exists — avoids visual clutter ("SOLAR · solaredge") in the common
// single-driver case while still disambiguating multi-driver rigs.
function labelWithName(base, name, count, suffix = "") {
  if (count > 1 && name) return `${base} · ${name.toUpperCase()}${suffix}`;
  return `${base}${suffix}`;
}

// ---------- primitives ----------

function fmtKw(kw) {
  const abs = Math.abs(kw);
  if (abs < 0.1) return "0 W";
  if (abs < 1)   return `${Math.round(kw * 1000)} W`;
  return `${kw.toFixed(2)} kW`;
}
function clamp(v, a, b) { return Math.max(a, Math.min(b, v)); }
function escapeXml(s) {
  return String(s).replace(/[<>&"']/g, c =>
    ({ "<": "&lt;", ">": "&gt;", "&": "&amp;", '"': "&quot;", "'": "&apos;" }[c]));
}

function iconGlyph(kind, color) {
  const a = `stroke="${color}" stroke-width="1.5" fill="none" stroke-linecap="round" stroke-linejoin="round"`;
  if (kind === "sun")  return `<g ${a}><circle cx="10" cy="10" r="4"/><path d="M10 1v2M10 17v2M1 10h2M17 10h2M4 4l1.4 1.4M14.6 14.6L16 16M4 16l1.4-1.4M14.6 5.4L16 4"/></g>`;
  if (kind === "grid") return `<g ${a}><path d="M4 3v14M16 3v14M3 6h14M3 14h14"/></g>`;
  if (kind === "bat")  return `<g ${a}><rect x="3" y="5" width="13" height="10" rx="1.5"/><path d="M16 8v4M8 8v4M12 8v4"/></g>`;
  if (kind === "ev")   return `<g ${a}><rect x="3" y="7" width="12" height="7" rx="1.5"/><path d="M5 7V5h8v2M6 16v1M12 16v1M15 9l2 1v3l-2 1"/></g>`;
  return "";
}

customElements.define("ftw-energy-flow", FtwEnergyFlow);
