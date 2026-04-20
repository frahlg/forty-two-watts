// <ftw-pv-arrays-3d> — tiny 3D preview of a site's PV-array config.
//
// Purpose: the settings Weather tab lets the operator list each PV
// plane (name + kWp + tilt_deg + azimuth_deg). Those four numbers
// fully specify a panel's orientation but they are hard to cross-
// check by eye on a phone in the shed. This component turns the
// list into a simple 3D scene — ground plane, compass, and one
// scaled / tilted / rotated rectangle per array, pivoted around a
// shared centre (the metaphorical middle of the roof). The operator
// drags to rotate, and the camera auto-fits so the whole set stays
// in frame at any array count.
//
// The component is self-contained: Three.js loads via the importmap
// declared in index.html ("three" + "three/addons/"); this file is
// lazy-imported from settings.js the first time the Weather tab
// opens, so the dashboard's main thread never pays for three.js on
// pages that don't touch the settings modal.
//
// Public API
// ----------
//   el.setArrays([{ name, kwp, tilt_deg, azimuth_deg }, ...])
//     Replaces the scene contents. Call whenever the list changes
//     — pass an empty array to clear.
//
// Azimuth convention matches the rest of the stack + the config
// help text in settings.js: 0 = N, 90 = E, 180 = S, 270 = W.
// Tilt: 0 = flat roof (panel horizontal), 90 = wall (panel
// vertical), typical pitched roof ≈ 35.

import { FtwElement } from "./ftw-element.js";
import * as THREE from "three";
import { OrbitControls } from "three/addons/controls/OrbitControls.js";

// Panel sizing — kWp → square edge in world units (metres, approximately).
// Real mono-Si panels land around 2 m² per kWp (3 panels of ~1.7 m² per
// kWp of string), so a 5 kWp array is ~10 m² ≈ 3.16 m × 3.16 m. We keep
// the visual aspect-ratio square (roof-layout independent) and only
// scale the edge by sqrt(kWp * area-per-kWp) so a 10 kWp array reads
// as ~√2 × the 5 kWp one, not 2×.
const AREA_PER_KWP = 2;             // m²/kWp, conservative average
const PANEL_EDGE_MIN = 1.4;         // floor so a 0.5 kWp array is still visible
const PANEL_EDGE_MAX = 7;           // cap so a 30 kWp array doesn't swallow the scene
const PANEL_ELEV    = 0.4;          // height above ground plane for the panel base

// Gap between adjacent panels along the orbit (world units). Small —
// the squares are the dominant visual, gap just keeps them from
// kissing at the closest angular separation.
const PANEL_GAP = 0.3;
// Minimum orbit radius even when a single small array is present —
// keeps the panel visibly distinct from the centre pivot sphere.
const MIN_ORBIT_R = 1.8;

// Ground plane half-size relative to the placement radius, so the
// ground always extends past the outer edge of the arrays by a
// healthy margin regardless of kWp totals.
const GROUND_MARGIN = 4;

class FtwPvArrays3d extends FtwElement {
  static styles = `
    :host {
      display: block;
      position: relative;
      width: 100%;
      /* Responsive height — phone portrait gets ~220 px, tablet gets
         ~300 px, desktop caps around 360 px. Uses clamp over viewport
         height so rotating the phone doesn't blow the viz to full-
         screen. */
      height: clamp(220px, 38vh, 360px);
      background: var(--ink-sunken);
      border: 1px solid var(--line);
      border-radius: var(--radius-md);
      overflow: hidden;
      cursor: grab;
    }
    :host(:active) { cursor: grabbing; }
    canvas { display: block; width: 100% !important; height: 100% !important; }
    .hint {
      position: absolute;
      left: 10px;
      bottom: 8px;
      font-family: var(--mono);
      font-size: 10px;
      letter-spacing: 0.08em;
      color: var(--fg-muted);
      pointer-events: none;
      text-transform: uppercase;
      opacity: 0.75;
    }
    .empty {
      position: absolute;
      inset: 0;
      display: grid;
      place-items: center;
      font-family: var(--mono);
      font-size: 11px;
      color: var(--fg-muted);
      pointer-events: none;
    }
  `;

  constructor() {
    super();
    this._arrays = [];
    this._three = null;
    this._raf = 0;
    this._ro = null;
    // Camera auto-fits on the FIRST successful rebuild and never
    // again in the same mount session — keeps the operator's
    // rotation/pan stable while they scrub kWp / tilt / azimuth
    // values from the form below.
    this._cameraFitted = false;
  }

  disconnectedCallback() {
    if (this._raf) cancelAnimationFrame(this._raf);
    this._raf = 0;
    if (this._ro) { this._ro.disconnect(); this._ro = null; }
    const t = this._three;
    if (t) {
      t.controls.dispose();
      t.renderer.dispose();
      t.scene.traverse((o) => {
        if (o.geometry) o.geometry.dispose();
        if (o.material) {
          const mats = Array.isArray(o.material) ? o.material : [o.material];
          mats.forEach((m) => { if (m.map) m.map.dispose(); m.dispose(); });
        }
      });
    }
    this._three = null;
  }

  // Public entry point. Arrays is the same shape as
  // config.weather.pv_arrays: [{ name, kwp, tilt_deg, azimuth_deg }, ...]
  setArrays(arrays) {
    this._arrays = Array.isArray(arrays) ? arrays.slice() : [];
    if (this._three) {
      this._rebuild();
    }
    this._updateEmptyHint();
  }

  render() {
    return `<div class="empty">configure at least one PV array to preview</div>
            <div class="hint">drag to rotate · scroll to zoom</div>`;
  }

  afterRender() {
    if (this._three) return;
    this._initThree();
    this._rebuild();
    this._startLoop();
    this._updateEmptyHint();
  }

  _updateEmptyHint() {
    const empty = this.shadowRoot.querySelector(".empty");
    if (empty) empty.style.display = this._arrays.length ? "none" : "grid";
  }

  _initThree() {
    const host = this;
    const rect = host.getBoundingClientRect();
    const w = Math.max(1, Math.round(rect.width));
    const h = Math.max(1, Math.round(rect.height));

    const scene = new THREE.Scene();
    scene.background = null; // stage's CSS bg shows through

    const camera = new THREE.PerspectiveCamera(45, w / h, 0.1, 500);
    camera.position.set(12, 10, 12);
    camera.lookAt(0, 0, 0);

    const renderer = new THREE.WebGLRenderer({ antialias: true, alpha: true });
    renderer.setPixelRatio(Math.min(window.devicePixelRatio, 2));
    renderer.setSize(w, h);
    host.shadowRoot.appendChild(renderer.domElement);

    // Orbit controls — left-drag rotates, middle-click (scroll-wheel
     // button) pans, right-drag dollies. Pan is enabled so a zoomed-
     // in operator can nudge the viewport when the auto-fit picked a
     // pose that hides a corner of their layout; clamps below stop
     // the camera from going below the ground plane.
    const controls = new OrbitControls(camera, renderer.domElement);
    controls.enableDamping = true;
    controls.dampingFactor = 0.08;
    controls.enablePan = true;
    controls.screenSpacePanning = true;
    controls.mouseButtons = {
      LEFT:   THREE.MOUSE.ROTATE,
      MIDDLE: THREE.MOUSE.PAN,
      RIGHT:  THREE.MOUSE.DOLLY,
    };
    // Middle-click on the canvas would otherwise trigger the
    // browser's auto-scroll cursor (the diamond pan icon that
    // scrolls the whole page) which fights OrbitControls' own pan.
    // preventDefault on mousedown blocks the auto-scroll without
    // interfering with the OrbitControls pan handler that runs
    // afterwards via its own capture listener.
    renderer.domElement.addEventListener("mousedown", (e) => {
      if (e.button === 1) e.preventDefault();
    });
    // Belt-and-braces: auxclick is the post-middle-click event
    // some browsers fire; blocking it kills any residual
    // click-to-toggle-autoscroll that slipped past mousedown.
    renderer.domElement.addEventListener("auxclick", (e) => {
      if (e.button === 1) e.preventDefault();
    });
    controls.minPolarAngle = 0.15;            // nearly top-down allowed
    controls.maxPolarAngle = Math.PI * 0.48;  // stop just above the ground
    controls.minDistance = 3;
    controls.maxDistance = 60;

    // Lighting — soft ambient + a warm sun from the south-east at ~45°
    // elevation. The sun direction is cosmetic; it just gives the
    // panels a side to catch that contrasts with the shaded side so
    // the tilt angle reads at a glance.
    scene.add(new THREE.AmbientLight(0xc7cfdd, 0.65));
    const sun = new THREE.DirectionalLight(0xffe8bf, 0.85);
    sun.position.set(12, 15, 6);
    scene.add(sun);

    // Ground plane. Size is refitted every _rebuild() based on the
    // outermost panel position, so a solo-array setup gets a tight
    // mat and a 6-array setup gets a correspondingly wider one.
    const groundGeo = new THREE.PlaneGeometry(20, 20);
    const groundMat = new THREE.MeshStandardMaterial({
      color: 0x24282f,
      roughness: 0.95,
      metalness: 0.0,
      transparent: true,
      opacity: 0.9,
    });
    const ground = new THREE.Mesh(groundGeo, groundMat);
    ground.rotation.x = -Math.PI / 2;
    ground.receiveShadow = false;
    scene.add(ground);

    // Light grid on the ground for scale cues — 2 m spacing.
    const grid = new THREE.GridHelper(20, 10, 0x4a5060, 0x353a44);
    grid.position.y = 0.01;
    scene.add(grid);

    // Compass rose — four labels pinned to N / E / S / W on the
    // ground plane edge. Labels are built as CanvasTextures so no
    // external font loading is needed. Each label is billboarded
    // (always faces the camera) via a per-frame update in the render
    // loop so rotating the scene doesn't bury a label in its own
    // backface.
    const compass = this._makeCompass();
    scene.add(compass);

    // Pivot marker — small ring at the roof centre. Kept tiny so it
    // doesn't fight the array rectangles for attention.
    const pivotGeo = new THREE.RingGeometry(0.35, 0.55, 32);
    const pivotMat = new THREE.MeshBasicMaterial({
      color: 0xffb85c, side: THREE.DoubleSide, transparent: true, opacity: 0.8,
    });
    const pivot = new THREE.Mesh(pivotGeo, pivotMat);
    pivot.rotation.x = -Math.PI / 2;
    pivot.position.y = 0.05;
    scene.add(pivot);

    const arrayRoot = new THREE.Group();
    scene.add(arrayRoot);

    const ro = new ResizeObserver(() => {
      const r = host.getBoundingClientRect();
      const w2 = Math.max(1, Math.round(r.width));
      const h2 = Math.max(1, Math.round(r.height));
      camera.aspect = w2 / h2;
      camera.updateProjectionMatrix();
      renderer.setSize(w2, h2);
    });
    ro.observe(host);
    this._ro = ro;

    this._three = { scene, camera, renderer, controls, ground, grid, compass, arrayRoot };
  }

  // Build a compass: four text sprites at the cardinal directions,
  // plus a thin colored stripe pointing North so the operator can
  // immediately see which way is up even before the labels resolve.
  _makeCompass() {
    const group = new THREE.Group();
    const dirs = [
      { label: "N", angle: 0,         color: "#ff7a7a" },
      { label: "E", angle: Math.PI/2, color: "#d7e1f0" },
      { label: "S", angle: Math.PI,   color: "#d7e1f0" },
      { label: "W", angle: Math.PI*1.5, color: "#d7e1f0" },
    ];
    // Radius is set relative to a default ground size; _rebuild
    // repositions these when the actual array spread is known.
    const r = 10;
    for (const d of dirs) {
      const sprite = makeLabelSprite(d.label, { color: d.color, canvasSize: 96 });
      // North = -Z, East = +X, South = +Z, West = -X.
      sprite.position.set(Math.sin(d.angle) * r, 0.2, -Math.cos(d.angle) * r);
      group.add(sprite);
      group.userData[d.label] = sprite;
    }
    // North stripe: a thin red line from the origin to the N edge,
    // sunk slightly so it hides under a PV panel's shadow if one
    // happens to sit at azimuth 0.
    const north = new THREE.Mesh(
      new THREE.PlaneGeometry(0.14, r),
      new THREE.MeshBasicMaterial({ color: 0xff7a7a, transparent: true, opacity: 0.7 }),
    );
    north.rotation.x = -Math.PI / 2;
    north.position.set(0, 0.015, -r / 2);
    group.add(north);
    group.userData.north = north;
    return group;
  }

  // Rebuild the array meshes + refit the ground plane + camera. Called
  // every setArrays() and once on first attach. Cheap enough to run
  // full-rebuild rather than diff — a typical site has < 6 arrays.
  _rebuild() {
    const t = this._three;
    if (!t) return;
    // Dispose old meshes.
    while (t.arrayRoot.children.length) {
      const c = t.arrayRoot.children.pop();
      c.traverse((o) => {
        if (o.geometry) o.geometry.dispose();
        if (o.material) {
          const mats = Array.isArray(o.material) ? o.material : [o.material];
          mats.forEach((m) => m.dispose());
        }
      });
    }
    if (!this._arrays.length) {
      this._resizeGroundAndCompass(MIN_ORBIT_R + 2);
      if (!this._cameraFitted) {
        this._fitCamera(MIN_ORBIT_R + 2);
      }
      return;
    }

    // Compute each array's edge length (metres) from its kWp, with
    // soft min/max so pathologically small or large ratings don't
    // blow the scene out of scale.
    const panels = this._arrays.map((a) => {
      const kwp = Math.max(0.1, Number(a.kwp) || 0.1);
      const edge = Math.max(PANEL_EDGE_MIN,
        Math.min(PANEL_EDGE_MAX, Math.sqrt(kwp * AREA_PER_KWP)));
      const azimuth = Number.isFinite(a.azimuth_deg) ? a.azimuth_deg : 180;
      const tilt = Number.isFinite(a.tilt_deg) ? a.tilt_deg : 35;
      return { edge, azimuth, tilt, name: a.name || "" };
    });

    // Orbit radius must be big enough that two adjacent panels
    // don't collide. The "edge/2 tangent" bound underestimated
    // overlap because each square can be tilted or oriented so its
    // diagonal points at its neighbour — the safe bound is the
    // bounding-circle radius, which for a square of side e is
    // e·√2/2 (the half-diagonal). So the chord between two centres
    // must exceed the sum of their half-diagonals + gap:
    //   2·r·sin(Δθ/2) >= eᵢ/√2 + eⱼ/√2 + gap
    const sorted = panels.slice().sort((a, b) => a.azimuth - b.azimuth);
    let needR = MIN_ORBIT_R;
    for (let i = 0; i < sorted.length; i++) {
      const a = sorted[i];
      const b = sorted[(i + 1) % sorted.length];
      let dth = b.azimuth - a.azimuth;
      if (i === sorted.length - 1) dth += 360;
      const dRad = THREE.MathUtils.degToRad(Math.max(1, dth));
      const span = a.edge / Math.SQRT2 + b.edge / Math.SQRT2 + PANEL_GAP;
      const req = span / (2 * Math.sin(dRad / 2));
      if (req > needR) needR = req;
    }
    // Hard floor so a single small array doesn't sit on top of the pivot.
    const r = Math.max(needR, MIN_ORBIT_R);

    for (const p of panels) {
      const mesh = this._buildPanel(p, r);
      t.arrayRoot.add(mesh);
    }

    this._resizeGroundAndCompass(r);
    // Only auto-fit on the first successful rebuild — subsequent
    // setArrays() calls (from the operator scrubbing tilt / kWp /
    // azimuth) should keep the current camera pose so their
    // rotation / pan isn't snapped back to the default overview.
    if (!this._cameraFitted) {
      this._fitCamera(r);
      this._cameraFitted = true;
    }
  }

  _buildPanel(p, radius) {
    const aRad = THREE.MathUtils.degToRad(p.azimuth);
    const tRad = THREE.MathUtils.degToRad(p.tilt);

    // Centre of the array (world). Azimuth convention:
    //   N = 0 → -Z, E = 90 → +X, S = 180 → +Z, W = 270 → -X.
    const cx = Math.sin(aRad) * radius;
    const cz = -Math.cos(aRad) * radius;

    // The panel is a Group with two transforms stacked: the outer
    // group rotates around the pivot's Y axis to face the azimuth,
    // the inner mesh sits at local (radius, 0, 0) and is tilted
    // around local Z. Using the group keeps the local axes meaningful
    // so "tilt" and "azimuth" stay independent of their numeric
    // values instead of crossing into Euler-angle interaction.
    const group = new THREE.Group();
    // Face the azimuth: rotate the group so its local +X axis points
    // in the azimuth direction. Three.js Y-rotation is CCW when
    // viewed from +Y looking down, which aligns with our
    // N→E→S→W = CW-from-above convention when we negate.
    group.rotation.y = -aRad + Math.PI / 2;
    // Inner group positions + tilts the panel. The Y offset grows
    // with tilt so the panel's lower edge never clips through the
    // ground. At tilt=0 the panel sits at PANEL_ELEV; at tilt=90
    // (wall), its bottom corner would otherwise be at
    //   PANEL_ELEV − edge/2 ≈ 0.4 − 1.2 = −0.8
    // which buries it under the ground mat. Bumping Y by
    // edge/2 · sin(tilt) keeps the bottom at PANEL_ELEV regardless
    // of how steep the tilt gets, and the panel appears to "lift
    // off" for a wall-mount — visually correct + never clips.
    const tiltLift = (p.edge / 2) * Math.sin(tRad);
    const tiltGroup = new THREE.Group();
    tiltGroup.position.set(radius, PANEL_ELEV + tiltLift, 0);
    // Tilt: rotate around local Z so the panel's TOP surface (its
    // original +Y normal) tips to face local +X — which, after the
    // outer Y-rotation, is the azimuth direction. This matches the
    // physics: a south-facing panel at 35° tilt has its normal
    // leaning south, its *far* edge (south end at the eave) LOW
    // and its *near* edge (north end at the ridge) HIGH.
    //
    // Positive rotation around +Z takes +X→+Y, which would lift the
    // far edge UP and tilt the normal toward -X (away from the
    // azimuth) — inverted from what we want. Negating flips both
    // signs at once: far edge drops, near edge rises, normal tilts
    // toward +X = azimuth. Reported as "tilt appears inverted" in
    // the earlier draft.
    tiltGroup.rotation.z = -tRad;

    // Panel: a rectangle in the XZ plane (lay flat first, then the
    // tilt above rotates it). The panel's "top edge" (the one
    // leaning away from the pivot) is its +X-local edge.
    const color = panelColor(p.azimuth, p.tilt);
    const geo = new THREE.PlaneGeometry(p.edge, p.edge);
    const mat = new THREE.MeshStandardMaterial({
      color,
      roughness: 0.45,
      metalness: 0.2,
      side: THREE.DoubleSide,
      emissive: color,
      emissiveIntensity: 0.05,
    });
    const panel = new THREE.Mesh(geo, mat);
    panel.rotation.x = -Math.PI / 2; // lay flat on the tiltGroup's local plane
    tiltGroup.add(panel);

    // Outline — a slightly larger wireframe square around the panel
    // so tilt transitions read cleanly even when the material's
    // shading is subtle (e.g. panel facing the camera edge-on).
    const edges = new THREE.EdgesGeometry(geo);
    const line = new THREE.LineSegments(edges,
      new THREE.LineBasicMaterial({ color: 0xf2e5cf, transparent: true, opacity: 0.55 }));
    line.rotation.x = -Math.PI / 2;
    line.position.y = 0.002; // anti-z-fight
    tiltGroup.add(line);

    // Name label if present. Amber pill (--accent-e style) with
    // near-black text — matches the 42W eyebrow palette in
    // DESIGN.md and reads from any rotation against either the
    // ground or the sky. Offset is kept tight to the panel's top
    // so the label lands right above the square instead of
    // floating far above where it loses its association.
    if (p.name) {
      const sprite = makeLabelSprite(p.name, {
        color: "#0a0a0a",
        bgColor: "#f5c45a",
        canvasSize: 72,
      });
      sprite.position.set(0, 0.15 + p.edge * 0.2, 0);
      tiltGroup.add(sprite);
    }

    group.add(tiltGroup);
    group.position.x = 0; // pivot around origin
    group.userData = { azimuth: p.azimuth, tilt: p.tilt, edge: p.edge,
                       name: p.name, center: new THREE.Vector3(cx, PANEL_ELEV, cz) };
    return group;
  }

  _resizeGroundAndCompass(radius) {
    const t = this._three;
    const outer = radius + GROUND_MARGIN;
    // Ground + grid scale as a ratio of their original size (both
    // built at size 20). Rescaling keeps the grid's 2 m cell size
    // constant per world unit rather than per screen unit, which
    // preserves the "scale cue" role of the grid lines.
    const s = (outer * 2) / 20;
    t.ground.scale.set(s, s, s);
    t.grid.scale.set(s, 1, s);
    // Reposition compass labels to the edge + orient their north stripe.
    ["N", "E", "S", "W"].forEach((k, i) => {
      const sprite = t.compass.userData[k];
      const angle = i * Math.PI / 2;
      sprite.position.set(Math.sin(angle) * outer, 0.2, -Math.cos(angle) * outer);
    });
    const north = t.compass.userData.north;
    if (north) {
      north.geometry.dispose();
      north.geometry = new THREE.PlaneGeometry(0.14, outer);
      north.position.set(0, 0.015, -outer / 2);
    }
  }

  // Fit the camera so the outer orbit radius (+ a margin) is fully
  // visible at the current aspect. Uses perspective FOV math rather
  // than controls.fitSphere to keep compatibility with OrbitControls
  // (which doesn't ship a native fit method in the lean addons).
  _fitCamera(radius) {
    const t = this._three;
    const target = radius + GROUND_MARGIN * 0.4;
    const fov = THREE.MathUtils.degToRad(t.camera.fov);
    // Account for the aspect — at narrow portrait we need more room
    // vertically, so use the smaller FOV across the two dims.
    const vFov = fov;
    const hFov = 2 * Math.atan(Math.tan(fov / 2) * t.camera.aspect);
    const effectiveFov = Math.min(vFov, hFov);
    const dist = target / Math.sin(effectiveFov / 2);
    // Look down at ~35° above the horizon — a mid-elevation viewing
    // angle that reads both tilt and azimuth at a glance. The
    // operator can drag to their preferred pose afterwards.
    const elev = 0.45;
    t.camera.position.set(
      dist * Math.sin(Math.PI * 0.7) * Math.cos(elev),
      dist * Math.sin(elev),
      dist * Math.cos(Math.PI * 0.7) * Math.cos(elev),
    );
    t.camera.lookAt(0, 0, 0);
    t.controls.target.set(0, 0, 0);
    t.controls.update();
  }

  _startLoop() {
    const t = this._three;
    const step = () => {
      this._raf = requestAnimationFrame(step);
      // Billboard the compass sprites so they always face the camera;
      // otherwise a back-side rotation would show them mirrored.
      ["N", "E", "S", "W"].forEach((k) => {
        const s = t.compass.userData[k];
        if (s) s.lookAt(t.camera.position);
      });
      t.controls.update();
      t.renderer.render(t.scene, t.camera);
    };
    this._raf = requestAnimationFrame(step);
  }
}

// Panel colour — hue from azimuth so south-facing arrays read warm
// and north-facing (rare, typically verification cases) read cool.
// Lightness slightly higher for near-flat panels (receive more light)
// and a touch darker for steeper tilts, so the visual "temperature"
// of the array pile quickly separates low-yield roofs from high-yield.
function panelColor(azimuth, tilt) {
  // Map azimuth → hue: 180° (south) → warm amber (40°), 0° (north)
  // → cool blue (220°). Use a cosine lerp so E and W (90° / 270°)
  // land mid-way.
  const a = ((azimuth % 360) + 360) % 360;
  const southness = 0.5 * (1 - Math.cos(THREE.MathUtils.degToRad(a)));
  // southness: 0 at N, 1 at S, 0.5 at E/W.
  const hue = 220 - southness * 180; // 220° (blue) → 40° (amber)
  const light = 0.55 - Math.min(0.15, tilt / 600); // 0.55 → 0.40 as tilt rises
  return new THREE.Color().setHSL(hue / 360, 0.55, light);
}

// Build a Sprite with a text label. When `bgColor` is provided the
// label renders as a filled rounded pill (amber 42W eyebrow look
// per DESIGN.md, near-black on-accent text) — used for the per-
// panel name chip so it reads from any rotation without blending
// into the panel colour. When `bgColor` is null, falls back to a
// transparent-background sprite (used by the compass cardinals).
function makeLabelSprite(text, opts = {}) {
  const color = opts.color || "#e8ecf4";
  const bgColor = opts.bgColor || null;
  const canvasSize = opts.canvasSize || 72;
  const fontPx = Math.round(canvasSize * 0.6);

  // Measure text first so the canvas (and therefore the sprite's
  // aspect ratio) matches the label content instead of hard-wrapping
  // at a fixed 3× width.
  const measureCtx = document.createElement("canvas").getContext("2d");
  measureCtx.font = `600 ${fontPx}px 'JetBrains Mono', ui-monospace, Menlo, monospace`;
  const textW = Math.max(canvasSize, Math.ceil(measureCtx.measureText(text).width) + canvasSize * 0.8);

  const canvas = document.createElement("canvas");
  canvas.width = Math.max(128, textW);
  canvas.height = canvasSize;
  const ctx = canvas.getContext("2d");
  ctx.clearRect(0, 0, canvas.width, canvas.height);

  if (bgColor) {
    // Rounded pill background — roundRect is Chromium/Safari/Firefox
    // all modern. If an older browser lands here it throws; fall
    // back to a plain rect in that case.
    ctx.fillStyle = bgColor;
    const pad = canvasSize * 0.12;
    const r = (canvas.height - pad * 2) / 2;
    if (ctx.roundRect) {
      ctx.beginPath();
      ctx.roundRect(pad, pad, canvas.width - pad * 2, canvas.height - pad * 2, r);
      ctx.fill();
    } else {
      ctx.fillRect(pad, pad, canvas.width - pad * 2, canvas.height - pad * 2);
    }
  }

  ctx.font = `600 ${fontPx}px 'JetBrains Mono', ui-monospace, Menlo, monospace`;
  ctx.textAlign = "center";
  ctx.textBaseline = "middle";
  ctx.fillStyle = color;
  ctx.fillText(text, canvas.width / 2, canvas.height / 2);

  const tex = new THREE.CanvasTexture(canvas);
  tex.colorSpace = THREE.SRGBColorSpace;
  // depthTest:false + high renderOrder keeps the label on top of
  // every panel, compass line, and ground element regardless of
  // camera angle. Without this, sprites were z-occluded by their
  // own panel when the camera looked up from ground level, and by
  // the opposite panel when looking across the scene from the
  // west. depthWrite stays false so the label doesn't write a
  // depth value other objects could depth-test against.
  const mat = new THREE.SpriteMaterial({
    map: tex,
    transparent: true,
    depthWrite: false,
    depthTest: false,
  });
  const s = new THREE.Sprite(mat);
  s.renderOrder = 999;
  // Sprite aspect = canvas aspect so text doesn't squash when names
  // differ in length. World scale picked so a 72px-tall canvas
  // renders at ~0.55 world-units tall at default camera distance.
  const worldH = 0.55;
  const worldW = worldH * (canvas.width / canvas.height);
  s.scale.set(worldW, worldH, 1);
  return s;
}

customElements.define("ftw-pv-arrays-3d", FtwPvArrays3d);
