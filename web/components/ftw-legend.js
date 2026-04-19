// <ftw-legend> — clickable chart legend with localStorage persistence.
//
// Usage:
//   const legend = document.querySelector("ftw-legend");
//   legend.items = [
//     { key: "grid", label: "Grid",   color: "#ef4444" },
//     { key: "pv",   label: "PV",     color: "#22c55e" },
//     { key: "pv_fc",label: "PV fc",  color: "#86efac", dash: true },
//   ];
//   legend.addEventListener("toggle", (e) => {
//     // e.detail = { key, visible, hidden: [...keysHidden], visible: [...keysShown] }
//     chart.setHiddenSeries(e.detail.hidden);
//   });
//
// Attributes:
//   storage-key — localStorage key for the persisted hidden-set. Omit to
//                 run without persistence.
//
// Each legend item is a struct:
//   { key: string, label: string, color: string, dash?: boolean }
//
// Clicking toggles the visible/hidden state (also reflected with class
// `.hidden` on the pill) and fires a `toggle` event carrying the full
// current list of hidden + visible keys — consumers don't have to
// accumulate state themselves.

import { FtwElement } from "./ftw-element.js";

class FtwLegend extends FtwElement {
  static styles = `
    :host {
      display: flex;
      flex-wrap: wrap;
      gap: 0.25rem 0.9rem;
      align-items: center;
      font-size: 0.8rem;
      color: var(--fg-dim);
    }
    .item {
      display: inline-flex;
      align-items: center;
      gap: 0.35rem;
      cursor: pointer;
      user-select: none;
      padding: 0.1rem 0.2rem;
      border-radius: 4px;
      transition: opacity 0.15s, color 0.15s;
    }
    .item:hover {
      color: var(--fg);
    }
    .item.hidden {
      opacity: 0.35;
    }
    .swatch {
      display: inline-block;
      width: 14px;
      height: 3px;
      border-radius: 2px;
      background: currentColor;
    }
    .swatch.dash {
      height: 0;
      background: transparent;
      border-top: 2px dashed currentColor;
    }
  `;

  constructor() {
    super();
    this._items = [];
    this._hidden = new Set();
  }

  connectedCallback() {
    super.connectedCallback();
    this._restore();
  }

  set items(next) {
    this._items = Array.isArray(next) ? next : [];
    this.update();
  }
  get items() {
    return this._items.slice();
  }

  /** Force a series visible/hidden from outside (e.g. default overrides). */
  setHidden(key, hidden) {
    if (hidden) this._hidden.add(key);
    else this._hidden.delete(key);
    this._persist();
    this.update();
  }

  /** @returns {string[]} keys currently hidden */
  hiddenKeys() {
    return Array.from(this._hidden);
  }

  render() {
    return this._items
      .map((it) => {
        const hidden = this._hidden.has(it.key) ? " hidden" : "";
        const dash = it.dash ? " dash" : "";
        const color = escapeAttr(it.color || "var(--fg)");
        return `
          <span class="item${hidden}" data-key="${escapeAttr(it.key)}" style="color:${color}">
            <span class="swatch${dash}"></span>
            <span>${escapeHTML(it.label)}</span>
          </span>
        `;
      })
      .join("");
  }

  afterRender() {
    this.shadowRoot.querySelectorAll(".item").forEach((el) => {
      el.addEventListener("click", () => this._toggle(el.dataset.key));
    });
  }

  _toggle(key) {
    if (this._hidden.has(key)) this._hidden.delete(key);
    else this._hidden.add(key);
    this._persist();
    this.update();
    this.dispatchEvent(new CustomEvent("toggle", {
      detail: {
        key,
        visible: !this._hidden.has(key),
        hidden: this.hiddenKeys(),
        visibleKeys: this._items.map((x) => x.key).filter((k) => !this._hidden.has(k)),
      },
      bubbles: true,
    }));
  }

  _restore() {
    const key = this.getAttribute("storage-key");
    if (!key) return;
    try {
      const raw = localStorage.getItem(key);
      if (!raw) return;
      const arr = JSON.parse(raw);
      if (Array.isArray(arr)) this._hidden = new Set(arr);
    } catch (_) { /* ignore malformed */ }
  }

  _persist() {
    const key = this.getAttribute("storage-key");
    if (!key) return;
    try {
      localStorage.setItem(key, JSON.stringify(this.hiddenKeys()));
    } catch (_) { /* quota / private mode */ }
  }
}

function escapeHTML(s) {
  return String(s == null ? "" : s)
    .replace(/&/g, "&amp;")
    .replace(/</g, "&lt;")
    .replace(/>/g, "&gt;");
}
function escapeAttr(s) {
  return escapeHTML(s).replace(/"/g, "&quot;");
}

customElements.define("ftw-legend", FtwLegend);
