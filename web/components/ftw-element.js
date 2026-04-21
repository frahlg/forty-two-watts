// FtwElement — base class for every <ftw-*> shadow-DOM component.
//
// Handles the two concerns we want solved once:
//   1. Shadow-root + adoptedStyleSheets boilerplate — subclasses just
//      provide a `static styles` CSS string and the sheet gets cached
//      once per class across all instances.
//   2. Theme tokens (--bg, --accent, …) inherit through shadow DOM
//      automatically because they're declared on :root in
//      /components/theme.css, so we don't have to re-adopt them.
//
// Usage:
//
//   import { FtwElement } from "./ftw-element.js";
//
//   class FtwBadge extends FtwElement {
//     static styles = `
//       :host { display: inline-flex; }
//       .pill { background: var(--ink-raised); color: var(--fg); }
//     `;
//     render() {
//       return `<span class="pill"><slot></slot></span>`;
//     }
//   }
//   customElements.define("ftw-badge", FtwBadge);
//
// Subclasses override render() returning the shadow-DOM HTML. For
// attribute-driven re-rendering, call this.update() from
// attributeChangedCallback.

// Dev-only hook — reads `?delay=N` from the page URL and returns it
// as a milliseconds int. Components that show a loading phase (e.g.
// ftw-energy-flow, ftw-history-card) use this to artificially hold in
// their skeleton/shimmer state for N ms so the loading→loaded
// transition can be inspected without hammering Refresh. Returns 0 in
// production and whenever the param is missing or non-numeric. Cached
// per load — the value is sticky for the whole session.
let _debugDelay = null;
export function ftwDebugDelay() {
  if (_debugDelay != null) return _debugDelay;
  try {
    const p = new URLSearchParams(location.search);
    const d = parseInt(p.get("delay"), 10);
    _debugDelay = Number.isFinite(d) && d > 0 ? d : 0;
  } catch (e) { _debugDelay = 0; }
  return _debugDelay;
}

export class FtwElement extends HTMLElement {
  // Subclasses override. Empty default means "no local styles" — tokens
  // from :root are still visible via var(--x).
  static styles = "";

  // Per-class CSSStyleSheet cache. Attached to the constructor so each
  // subclass gets its own sheet without recomputing per instance.
  static _sheet = null;

  constructor() {
    super();
    this.attachShadow({ mode: "open" });
    const sheet = this.constructor._ensureSheet();
    if (sheet) this.shadowRoot.adoptedStyleSheets = [sheet];
  }

  connectedCallback() {
    this.update();
  }

  // Re-renders the shadow content. Safe to call repeatedly; subclasses
  // should call this from attributeChangedCallback when a watched attr
  // changes. Default impl wipes + sets innerHTML — subclasses with
  // expensive renders can override for targeted updates.
  update() {
    const html = this.render();
    if (typeof html === "string") {
      this.shadowRoot.innerHTML = html;
      this.afterRender();
    }
  }

  // Subclasses return the component's HTML string. Default empty so the
  // element doesn't throw if render() wasn't overridden.
  render() {
    return "";
  }

  // Hook for wiring up event listeners after each render. Default no-op.
  afterRender() {}

  static _ensureSheet() {
    if (this._sheet) return this._sheet;
    if (!this.styles) return null;
    const s = new CSSStyleSheet();
    s.replaceSync(this.styles);
    this._sheet = s;
    return s;
  }
}
