// <ftw-card> — base card shell.
//
// Slots:
//   title    — header text (optional; header row renders only if present)
//   actions  — buttons / badges aligned to the header's right
//   default  — body content
//
// Attributes:
//   variant — "default" (roomy) | "compact" (tight padding) | "summary"
//             (center-aligned compact, for dashboard tiles)
//   interactive — presence adds a hover lift + cursor; emit ftw-card-click
//             when clicked / keyboard-activated (Enter/Space)
//
// Example:
//   <ftw-card>
//     <span slot="title">Battery</span>
//     <ftw-badge slot="actions" status="ok">Online</ftw-badge>
//     ...
//   </ftw-card>
//
//   <ftw-card variant="summary">
//     <div class="card-label">Grid</div>
//     <div class="card-value">1240 W</div>
//   </ftw-card>

import { FtwElement } from "./ftw-element.js";

class FtwCard extends FtwElement {
  static styles = `
    :host {
      display: block;
      background: var(--ink-raised);
      border: 1px solid var(--line);
      border-radius: var(--radius-sm);
      padding: 1rem 1.25rem;
    }
    :host([variant="compact"]) {
      padding: 0.75rem 0.9rem;
    }
    :host([variant="summary"]) {
      padding: 0.9rem 1rem;
      text-align: center;
    }
    :host([interactive]) {
      cursor: pointer;
      transition: transform 0.1s ease, border-color 0.15s ease;
    }
    :host([interactive]:hover) {
      border-color: color-mix(in srgb, var(--accent-e) 60%, var(--line));
      transform: translateY(-1px);
    }
    :host([interactive]:focus-visible) {
      outline: 2px solid var(--accent-e);
      outline-offset: 2px;
    }
    header {
      display: flex;
      align-items: center;
      justify-content: space-between;
      gap: 0.5rem;
      margin-bottom: 0.6rem;
    }
    /* Collapse header entirely when neither title nor actions is slotted. */
    header:not([data-has-header]) {
      display: none;
    }
    header ::slotted([slot="title"]) {
      font-size: 0.9rem;
      font-weight: 600;
      color: var(--fg);
    }
  `;

  connectedCallback() {
    super.connectedCallback();
    if (this.hasAttribute("interactive") && !this.hasAttribute("tabindex")) {
      this.setAttribute("tabindex", "0");
    }
    this._onClick = () => this._emitClick();
    this._onKey = (e) => {
      if (e.key === "Enter" || e.key === " ") {
        e.preventDefault();
        this._emitClick();
      }
    };
    this.addEventListener("click", this._onClick);
    this.addEventListener("keydown", this._onKey);
  }

  disconnectedCallback() {
    this.removeEventListener("click", this._onClick);
    this.removeEventListener("keydown", this._onKey);
  }

  _emitClick() {
    if (!this.hasAttribute("interactive")) return;
    this.dispatchEvent(new CustomEvent("ftw-card-click", { bubbles: true }));
  }

  render() {
    return `
      <header>
        <slot name="title"></slot>
        <slot name="actions"></slot>
      </header>
      <slot></slot>
    `;
  }

  afterRender() {
    const header = this.shadowRoot.querySelector("header");
    const titleSlot = header.querySelector('slot[name="title"]');
    const actionsSlot = header.querySelector('slot[name="actions"]');
    const sync = () => {
      const has =
        titleSlot.assignedNodes({ flatten: true }).length > 0 ||
        actionsSlot.assignedNodes({ flatten: true }).length > 0;
      if (has) header.setAttribute("data-has-header", "");
      else header.removeAttribute("data-has-header");
    };
    titleSlot.addEventListener("slotchange", sync);
    actionsSlot.addEventListener("slotchange", sync);
    sync();
  }
}

customElements.define("ftw-card", FtwCard);
