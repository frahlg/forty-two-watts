// <ftw-tabs> + <ftw-tab> — tab strip with switchable panels.
//
// Usage:
//   <ftw-tabs value="control">
//     <ftw-tab value="control" label="Control">
//       <!-- panel content -->
//     </ftw-tab>
//     <ftw-tab value="devices" label="Devices">...</ftw-tab>
//   </ftw-tabs>
//
// <ftw-tabs> attributes:
//   value — active tab's `value`. Reflects to/from attribute; settable.
//
// Events:
//   change — { detail: { value } }
//
// <ftw-tab> attributes:
//   value — id used by the parent <ftw-tabs>
//   label — button text
//   active — set by the parent; controls panel visibility

import { FtwElement } from "./ftw-element.js";

class FtwTabs extends FtwElement {
  static styles = `
    :host {
      display: block;
    }
    .strip {
      display: flex;
      gap: 0.25rem;
      border-bottom: 1px solid var(--border);
      margin-bottom: 0.75rem;
      overflow-x: auto;
    }
    button {
      appearance: none;
      background: transparent;
      border: none;
      border-bottom: 2px solid transparent;
      color: var(--text-dim);
      padding: 0.5rem 0.9rem;
      font: inherit;
      font-size: 0.85rem;
      cursor: pointer;
      white-space: nowrap;
      border-radius: 4px 4px 0 0;
      transition: color 0.15s, border-color 0.15s, background 0.15s;
    }
    button:hover {
      color: var(--text);
      background: color-mix(in srgb, var(--accent) 8%, transparent);
    }
    button[data-active="true"] {
      color: var(--text);
      border-bottom-color: var(--accent);
    }
  `;

  static get observedAttributes() {
    return ["value"];
  }

  connectedCallback() {
    super.connectedCallback();
    // Re-sync when child <ftw-tab>s are added/removed at runtime.
    const slot = this.shadowRoot.querySelector("slot");
    if (slot) slot.addEventListener("slotchange", () => this._syncTabs());
    // Pick an initial value if none provided — first ftw-tab child.
    if (!this.hasAttribute("value")) {
      const first = this.querySelector("ftw-tab");
      if (first) this.setAttribute("value", first.getAttribute("value") || "");
    }
    this._syncTabs();
  }

  attributeChangedCallback() {
    this._syncTabs();
  }

  render() {
    return `
      <div class="strip" role="tablist"></div>
      <slot></slot>
    `;
  }

  afterRender() {
    this._syncTabs();
  }

  _syncTabs() {
    const strip = this.shadowRoot && this.shadowRoot.querySelector(".strip");
    if (!strip) return;
    const tabs = Array.from(this.querySelectorAll(":scope > ftw-tab"));
    const active = this.getAttribute("value");
    strip.innerHTML = "";
    tabs.forEach((tab) => {
      const v = tab.getAttribute("value") || "";
      const label = tab.getAttribute("label") || v;
      const btn = document.createElement("button");
      btn.type = "button";
      btn.textContent = label;
      btn.setAttribute("role", "tab");
      btn.dataset.value = v;
      btn.dataset.active = String(v === active);
      btn.addEventListener("click", () => this._select(v));
      strip.appendChild(btn);
      if (v === active) tab.setAttribute("active", "");
      else tab.removeAttribute("active");
    });
  }

  _select(value) {
    if (this.getAttribute("value") === value) return;
    this.setAttribute("value", value);
    this.dispatchEvent(new CustomEvent("change", { detail: { value }, bubbles: true }));
  }
}

class FtwTab extends FtwElement {
  static styles = `
    :host {
      display: none;
    }
    :host([active]) {
      display: block;
    }
  `;

  render() {
    return `<slot></slot>`;
  }
}

customElements.define("ftw-tabs", FtwTabs);
customElements.define("ftw-tab", FtwTab);
