// <ftw-energy-flow> — hero diagram for /next.
//
// Planet/sun layout. The HOUSE sits at the center (the sun), and every
// other device — PV inverters, batteries, grid, EV chargers, whatever
// future category — is a "planet" that orbits it. A planet declares
// which CORNER it belongs to:
//
//       top-left  (225°)       top-right (315°)
//                    ╲       ╱
//                     ╲     ╱
//                   ┌──HOUSE──┐
//                     ╱     ╲
//                    ╱       ╲
//     bottom-left (135°)       bottom-right (45°)
//
// Corners are hard-wired at 45° diagonals so the overall X is uniform
// no matter how many planets each corner holds. Adding a second (or
// third) planet at the same corner makes the earlier ones scoot aside:
// they cluster along an arc centered on that corner's anchor angle,
// same orbit radius, so every power beam stays the same length.
// When no planets report at a corner, a "no data" placeholder fills
// it so the X reads as complete even on first paint.
//
// Flow edges carry two simultaneous animations:
//   1. A dashed, blurred stroke whose dash-offset animates via CSS —
//      gives a "current flowing" feel without redrawing any DOM.
//   2. Particle circles riding the edge via a single rAF loop, with
//      damped-oscillator perpendicular motion so the spray looks
//      turbulent rather than a rotating screw. Speed scales with |kW|.
// Both effects are skipped when |kW| < 50 W so idle edges read as still.
//
// Update pattern — next-app.js calls `setReadings(...)` each status poll
// with a fully-resolved planet list. The component never introspects
// /api/status itself and has no knowledge of driver roles — all
// role→corner/color/sub-text logic lives in the caller.
//
//   flow.setReadings({
//     load: 1.2,
//     planets: [
//       { id: "grid",     corner: "bottom-left",  title: "GRID",
//         kw:  0.5, toHub: true,  color: "var(--red-e)", sub: "importing" },
//       { id: "pv-east",  corner: "top-left",     title: "SOLAR", name: "east",
//         kw:  3.1, toHub: true,  color: "var(--amber)", sub: "generating" },
//       { id: "bat-main", corner: "top-right",    title: "BATTERY",
//         kw:  2.2, toHub: false, color: "var(--cyan)",  sub: "charging", soc: 78 },
//       { id: "ev",       corner: "bottom-right", title: "EV CHARGER",
//         kw:  3.7, toHub: false, color: "var(--green-e)", sub: "charging" },
//     ],
//   });

import { FtwElement } from "./ftw-element.js";

// World width is fixed at 1000; CX is always at the center of whatever
// crop we render. The viewBox HEIGHT (and CY = H/2) are computed per
// render — they grow with the largest cluster size so the arc never
// pushes a planet outside the box. Keeping H dynamic is what lets the
// hero card grow when the user adds a second/third PV/battery/etc.
const W = 1000;
const CX = W / 2;
// Baseline height used when no cluster has more than one planet — also
// the floor for the dynamic computation so the diagram never shrinks
// below the single-device size.
const H_BASE = 580;

// Corner → anchor angle in screen coordinates (0°=east, 90°=south).
// Fixed at exactly 45° so the whole diagram reads as a uniform X
// regardless of how many planets live at any corner.
const CORNER_ANGLE = {
  "top-left":     -3 * Math.PI / 4, // 225°
  "top-right":    -Math.PI / 4,     // 315°
  "bottom-right":  Math.PI / 4,     //  45°
  "bottom-left":   3 * Math.PI / 4, // 135°
};
// Default title shown when a corner has no planets reporting yet.
// Keeps the first-paint X intact; updated as soon as drivers push.
const CORNER_PLACEHOLDER_TITLE = {
  "top-left":     "SOLAR",
  "top-right":    "BATTERY",
  "bottom-right": "EV CHARGER",
  "bottom-left":  "GRID",
};

class FtwEnergyFlow extends FtwElement {
  static styles = `
    :host {
      display: block;
      background: linear-gradient(180deg,
        var(--hero-bg-top) 0%,
        var(--hero-bg-bot) 100%);
      border: 1px solid var(--line);
      border-radius: var(--radius-lg);
      padding: 20px 28px;
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
    .title {
      font-family: var(--mono);
      font-size: 16px;
      font-weight: 600;
      letter-spacing: 0.22em;
      text-transform: uppercase;
      color: var(--fg);
      text-align: center;
      padding: 2px 0;
      margin-top: 10px;
      margin-bottom: 48px;
      position: relative;
    }
    svg {
      width: 100%;
      height: calc(var(--efl-h-factor, 1) * 535px);
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
    .ef-clickable { cursor: pointer; outline: none; }
    .ef-clickable:focus-visible > circle { stroke-width: 3; filter: drop-shadow(0 0 4px var(--accent, #6cf)); }
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
      :host { padding: 20px 12px; }
      .title { margin-bottom: 8px; }
      svg { height: calc(var(--efl-h-factor, 1) * 510px); }
      .sv-node-title { font-size: 13px; }
      .sv-node-value { font-size: 24px; }
      .sv-node-sub   { font-size: 13px; }
      .sv-hub-value  { font-size: 22px; }
      .sv-hub-label  { font-size: 11px; }
    }
    @media (max-width: 600px) {
      svg { height: calc(var(--efl-h-factor, 1) * 460px); }
      .sv-node-title { font-size: 18px; }
      .sv-node-value { font-size: 30px; }
      .sv-node-sub   { font-size: 16px; }
      .sv-hub-value  { font-size: 28px; }
      .sv-hub-label  { font-size: 14px; }
    }
  `;

  constructor() {
    super();
    // Start with empty clusters; render shows placeholder slots until the
    // first setReadings() push arrives from next-app.js.
    this._readings = { load: 0, planets: [] };
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
    // Generic viewport listener — covers desktop window resizes and
    // device rotations that don't cross the 600px matchMedia threshold
    // (which `_mq` already handles). Throttled via rAF so a continuous
    // resize-drag triggers at most one re-render per frame.
    this._onResize = () => {
      if (this._resizeRaf) return;
      this._resizeRaf = requestAnimationFrame(() => {
        this._resizeRaf = 0;
        this.update();
      });
    };
    if (typeof window !== "undefined") {
      window.addEventListener("resize", this._onResize, { passive: true });
      window.addEventListener("orientationchange", this._onResize, { passive: true });
    }
  }

  disconnectedCallback() {
    if (this._rafId) cancelAnimationFrame(this._rafId);
    this._rafId = null;
    this._particles = [];
    if (this._resizeRaf) {
      cancelAnimationFrame(this._resizeRaf);
      this._resizeRaf = 0;
    }
    if (this._mq) {
      this._mq.removeEventListener("change", this._onMqChange);
    }
    if (typeof window !== "undefined" && this._onResize) {
      window.removeEventListener("resize", this._onResize);
      window.removeEventListener("orientationchange", this._onResize);
    }
  }

  // Bulk setter — preferred update path. `load` merges; `planets`
  // replaces the whole list when provided. Passing `undefined` for
  // `planets` leaves the previous cluster intact (useful during
  // transient /api/status errors so the diagram doesn't blank out).
  setReadings(r) {
    if (r.load != null)         this._readings.load    = r.load;
    if (Array.isArray(r.planets)) this._readings.planets = r.planets;
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
    // Delegated click on the SVG — one listener per render covers every
    // planet group that opted in via data-role. The handler dispatches
    // `ftw-planet-click` so callers (next-app.js) can route per-role
    // (e.g. ev → open EV modal scoped to this driver).
    const svg = this.shadowRoot.querySelector('svg');
    if (svg) {
      const fire = (g) => {
        const role = g.getAttribute('data-role') || '';
        const name = g.getAttribute('data-name') || '';
        const id   = g.getAttribute('data-id')   || '';
        this.dispatchEvent(new CustomEvent('ftw-planet-click', {
          detail: { role, name, id }, bubbles: true, composed: true,
        }));
      };
      svg.addEventListener('click', (e) => {
        const g = e.target.closest && e.target.closest('.ef-clickable');
        if (g) fire(g);
      });
      svg.addEventListener('keydown', (e) => {
        if (e.key !== 'Enter' && e.key !== ' ') return;
        const g = e.target.closest && e.target.closest('.ef-clickable');
        if (g) { e.preventDefault(); fire(g); }
      });
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
    const { load } = this._readings;

    // Two tiers of base parameters. Desktop keeps the full 0..1000 viewBox
    // with larger circles + hub; compact crops to 180..820 and shrinks the
    // base orbit so phone widths render legibly. Both tiers are FLOORS:
    // the dynamic-sizing block below grows orbitR + viewBox H/W when any
    // corner holds 2+ planets so the arc never pushes a node off-canvas.
    const tier = this._compact
      ? { vbX: 180, vbW: 640, orbitR: 268, baseR: 86, hubR: 95 }
      : { vbX: 0,   vbW: W,   orbitR: 288, baseR: 84, hubR: 99 };

    // Group planets by corner. Missing corners get a placeholder so
    // the X silhouette is complete on first paint even before any
    // drivers report.
    const groups = {
      "top-left": [], "top-right": [],
      "bottom-right": [], "bottom-left": [],
    };
    for (const p of this._readings.planets) {
      if (groups[p.corner]) groups[p.corner].push(p);
    }
    for (const c of Object.keys(groups)) {
      if (groups[c].length === 0) {
        groups[c].push({
          id: `_placeholder-${c}`, corner: c,
          title: CORNER_PLACEHOLDER_TITLE[c], name: null,
          kw: 0, toHub: true,
          color: "var(--fg-muted)", sub: "no data", soc: null,
          placeholder: true,
        });
      }
    }
    const maxN = Math.max(1, ...Object.values(groups).map(g => g.length));

    // -- Dynamic orbit + container sizing --------------------------------
    // For N>=3 the arc would either spill past 60° (clusterArc's maxSpan)
    // or shrink baseR — so we grow orbitR instead, keeping every node
    // full-sized. For N=1 or 2 the natural step already fits and orbitR
    // stays at the tier floor.
    const gap = 16;
    const maxSpan = Math.PI / 3;
    let orbitR = tier.orbitR;
    if (maxN >= 3) {
      const step = maxSpan / (maxN - 1);
      const required = (2 * tier.baseR + gap) / (2 * Math.sin(step / 2));
      orbitR = Math.max(orbitR, Math.ceil(required));
    }

    // Recompute the actual step + half-span at this orbitR (mirrors the
    // formula clusterArc uses), so we know how far the outermost arc
    // position lies from the corner anchor.
    const naturalStep = 2 * Math.asin(Math.min(1, (2 * tier.baseR + gap) / (2 * orbitR)));
    const stepActual = maxN <= 1 ? 0 : Math.min(naturalStep, maxSpan / (maxN - 1));
    const halfSpan = ((maxN - 1) * stepActual) / 2;

    // Worst-case x/y offsets from CX/CY across all four corners (each
    // anchor ± halfSpan). Whichever planet sits closest to the
    // top/bottom/left/right of the world drives the container size.
    const margin = 12;
    const corners = Object.values(CORNER_ANGLE);
    let maxYOff = 0, maxXOff = 0;
    for (const a0 of corners) {
      for (const da of [-halfSpan, +halfSpan]) {
        const ay = Math.abs(orbitR * Math.sin(a0 + da));
        const ax = Math.abs(orbitR * Math.cos(a0 + da));
        if (ay > maxYOff) maxYOff = ay;
        if (ax > maxXOff) maxXOff = ax;
      }
    }
    const Hdyn = Math.max(H_BASE, Math.ceil(2 * (maxYOff + tier.baseR + margin)));
    const Wneeded = Math.ceil(2 * (maxXOff + tier.baseR + margin));
    let vbW = tier.vbW, vbX = tier.vbX;
    if (Wneeded > vbW) {
      // Hub stays at world x=500; recenter the crop around it.
      vbW = Wneeded;
      vbX = Math.round(W / 2 - vbW / 2); // negative is fine — the viewBox can extend past world bounds
    }
    // Scale the rendered CSS height so the SVG keeps its on-screen
    // visual proportions as the viewBox grows. CSS picks the per-tier
    // base (535/510/460) via media queries; we just multiply.
    this.style.setProperty("--efl-h-factor", (Hdyn / H_BASE).toFixed(3));

    // Per-render layout struct. CY now varies — every helper that used
    // to read module-level CY now takes (cx, cy) explicitly.
    const cy = Hdyn / 2;
    const P = {
      vbX, vbW, H: Hdyn, cy,
      orbitR, baseR: tier.baseR, hubR: tier.hubR,
      hubIconY: cy - (this._compact ? 49 : 50),
      hubValueY: cy + 10,
      hubLabelY: cy + (this._compact ? 34 : 36),
    };
    // -- /Dynamic sizing -------------------------------------------------

    // Per-corner arc placement. Every corner uses the same orbitR so
    // beams are identical-length radial lines.
    const placed = [];
    for (const c of Object.keys(groups)) {
      const g = groups[c];
      const pl = clusterArc(g.length, CX, P.cy, P.orbitR, CORNER_ANGLE[c], P.baseR);
      g.forEach((planet, i) => {
        placed.push({ ...planet,
          _pos: pl.positions[i], _r: pl.r, _groupSize: g.length });
      });
    }

    // Build edges. Each planet owns one radial beam; `toHub` decides
    // particle direction (house-inward vs house-outward).
    const edges = placed.map(p => {
      const kwAbs = Math.abs(p.kw);
      return {
        id: p.id,
        ...radialEndpoints(p._pos, p._r, P.hubR, p.toHub, CX, P.cy),
        kw: kwAbs,
        color: p.color,
        active: !p.placeholder && kwAbs > 0.05,
      };
    });

    const maxKw = Math.max(0.5, ...edges.map(e => e.kw));
    // Stash the particle-param list on the instance so afterRender()
    // can pick it up once the shadow DOM is in place. Re-assigning
    // here (instead of pushing) ensures a re-render starts from a
    // clean slate — no stale particles from the previous frame.
    this._particles = [];
    const edgesSvg = edges.map(e => renderEdge(e, maxKw, this._particles)).join("");

    // Nodes. The driver name only appears (as a second title line) when
    // >1 planet shares the corner — keeps single-device rigs clean.
    // Clickable planets are tagged with data-* attrs picked up by the
    // delegated SVG click listener wired in afterRender().
    const nodesSvg = placed.map(p =>
      renderCircleNode({
        pos: p._pos,
        value: p.placeholder ? "—" : fmtKw(p.kw),
        title: p.title,
        nameLabel: p._groupSize > 1 && p.name ? p.name.toUpperCase() : null,
        sub: p.sub,
        color: p.color,
        soc: p.placeholder ? null : p.soc,
        radius: p._r,
        clickable: !p.placeholder && !!p.role,
        role: p.role || "",
        name: p.name || "",
        id: p.id,
      })
    ).join("");

    return `
      <div class="title">Energy balance</div>
      <svg viewBox="${P.vbX} 0 ${P.vbW} ${P.H}" preserveAspectRatio="xMidYMid meet" aria-hidden="true">
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

        <circle cx="${CX}" cy="${P.cy}" r="200" fill="url(#ef-hub)"/>

        ${edgesSvg}

        <!-- HOUSE / hub: load reading lives here -->
        <g>
          <circle cx="${CX}" cy="${P.cy}" r="${P.hubR}"
                  fill="var(--hero-house-fill)"
                  stroke="var(--hero-house-stroke)" stroke-width="1.5"/>
          <circle class="ring" cx="${CX}" cy="${P.cy}" r="${P.hubR - 8}"
                  fill="none"
                  stroke="var(--hero-house-ring)" stroke-width="1"
                  stroke-dasharray="2 4"/>
          <g transform="translate(${CX - 16}, ${P.hubIconY})"
             stroke="var(--hero-house-stroke)" stroke-width="1.6"
             fill="none" stroke-linecap="round" stroke-linejoin="round">
            <path d="M2 16 L16 3 L30 16 L30 26 L2 26 Z"/>
            <path d="M12 26 V18 H20 V26"/>
          </g>
          <text x="${CX}" y="${P.hubValueY}" text-anchor="middle"
                fill="var(--hero-load-text)" class="sv-hub-value">
            ${fmtKw(load)}
          </text>
          <text x="${CX}" y="${P.hubLabelY}" text-anchor="middle"
                fill="var(--hero-label-text)" class="sv-hub-label">
            CONSUMING
          </text>
        </g>

        ${nodesSvg}
      </svg>
    `;
  }
}

// ---------- geometry + edge helpers ----------

// Place N nodes on a hub-centered circle of radius `orbitR` along an
// arc around `anchorAngle` (the polar angle that points to the
// quadrant's corner). Every node sits on the same circle, so its
// radial beam to the hub is the same length as every other satellite's
// beam — the "circle around the house" layout.
//
// Sizing:
//   • n === 1 → node sits exactly on the anchor with full baseR.
//   • n  >= 2 → angular step is whatever keeps adjacent circles gap
//               apart on the orbit (chord = 2*baseR + gap). If that
//               would spread the cluster past `maxSpan`, we cap the
//               span and shrink nodeR so non-overlap still holds.
//
// `maxSpan` (≈60°) keeps the arc comfortably inside its quadrant — PV
// stops short of the grid anchor, battery stops short of EV — so the
// four regions stay visually distinct even with many devices.
function clusterArc(n, cx, cy, orbitR, anchorAngle, baseR) {
  if (n <= 1) {
    return { positions: [polar(cx, cy, orbitR, anchorAngle)], r: baseR };
  }
  const gap = 16;
  const maxSpan = Math.PI / 3; // 60°
  const idealChord = 2 * baseR + gap;
  let step = 2 * Math.asin(Math.min(1, idealChord / (2 * orbitR)));
  let r = baseR;
  if (step * (n - 1) > maxSpan) {
    step = maxSpan / (n - 1);
    const chord = 2 * orbitR * Math.sin(step / 2);
    r = Math.max(32, Math.floor((chord - gap) / 2));
  }
  const half = ((n - 1) * step) / 2;
  const positions = Array.from({ length: n }, (_, i) =>
    polar(cx, cy, orbitR, anchorAngle - half + i * step),
  );
  return { positions, r };
}

function polar(cx, cy, r, a) {
  return { x: cx + r * Math.cos(a), y: cy + r * Math.sin(a) };
}

// Radial beam endpoints for a planet sitting on the hub's orbit. Both
// endpoints lie on the line between the planet center and the hub
// center; the planet endpoint lands on its circle perimeter, the hub
// endpoint on the hub perimeter. `toHub` picks the particle direction
// (animateMotion always walks from → to, so swapping them flips the
// flow — cheaper than maintaining two edge variants).
function radialEndpoints(pos, nodeR, hubR, toHub, cx, cy) {
  const dx = cx - pos.x;
  const dy = cy - pos.y;
  const len = Math.hypot(dx, dy) || 1;
  const ux = dx / len;
  const uy = dy / len;
  const planetEdge = { x: pos.x + ux * nodeR, y: pos.y + uy * nodeR };
  const hubEdge    = { x: cx    - ux * hubR,   y: cy    - uy * hubR  };
  return toHub
    ? { from: planetEdge, to: hubEdge }
    : { from: hubEdge,    to: planetEdge };
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

// Circular node for the × layout. Text is centered and respread for a
// disk: title near the top, value at the middle, sub below the middle,
// and — for batteries — a SoC reading as a fourth line below sub.
// Text baselines scale with radius so a desktop-size 105 px circle and
// a multi-device 55 px circle both read proportionally. Stroke is the
// accent color so each node carries its identity on the edge of the
// circle — no separate stripe needed the way rectangular boxes have.
function renderCircleNode({ pos, title, nameLabel, value, sub, color, soc, radius = 86,
                            clickable = false, role = "", name = "", id = "" }) {
  const r = radius;
  const { x, y } = pos;
  // Clickable planets must advertise themselves to assistive tech:
  // role=button so screen readers announce "button", and aria-label
  // derived from the visible title/name so the announcement names
  // what activating this node will open.
  const nodeLabel = [title, nameLabel].filter(Boolean).join(" ");
  const ariaLabel = nodeLabel ? `Open ${nodeLabel}` : "Open node";
  const groupAttrs = clickable
    ? ` class="ef-node ef-clickable" data-role="${escapeXml(role)}" data-name="${escapeXml(name)}" data-id="${escapeXml(id)}" tabindex="0" role="button" aria-label="${escapeXml(ariaLabel)}"`
    : ` class="ef-node"`;
  // When a per-device name suffix is present, the title becomes two
  // stacked lines ("SOLAR" / "SUNGROW"). That preserves more horizontal
  // room inside the disk than a single "SOLAR · SUNGROW" line.
  const twoLine = !!nameLabel;
  const titleY = Math.round((twoLine ? -0.50 : -0.42) * r);
  const valueY = Math.round(0.09  * r);
  const subY   = Math.round(0.42  * r);
  const socY   = Math.round(0.70  * r);
  const titleSvg = twoLine
    ? `<text x="${x}" y="${y + titleY}" text-anchor="middle"
             fill="var(--hero-label-text)" class="sv-node-title">
         <tspan x="${x}" dy="0">${escapeXml(title)}</tspan>
         <tspan x="${x}" dy="1.2em">${escapeXml(nameLabel)}</tspan>
       </text>`
    : `<text x="${x}" y="${y + titleY}" text-anchor="middle"
             fill="var(--hero-label-text)" class="sv-node-title">
         ${escapeXml(title)}
       </text>`;
  const socText = soc != null
    ? `<text x="${x}" y="${y + socY}" text-anchor="middle"
             fill="var(--cyan)" class="sv-node-sub">SoC ${Math.round(soc)}%</text>`
    : "";
  return `
    <g${groupAttrs}>
      <circle cx="${x}" cy="${y}" r="${r}"
              fill="var(--hero-box-fill)" stroke="${color}" stroke-width="2"/>
      ${titleSvg}
      <text x="${x}" y="${y + valueY}" text-anchor="middle" fill="${color}" class="sv-node-value">
        ${value}
      </text>
      <text x="${x}" y="${y + subY}" text-anchor="middle"
            fill="var(--hero-sub-text)" class="sv-node-sub">
        ${escapeXml(sub)}
      </text>
      ${socText}
    </g>`;
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

customElements.define("ftw-energy-flow", FtwEnergyFlow);
