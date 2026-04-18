// <ftw-badge> — labeled status pill.
//
// Attributes:
//   status — "ok" (default) | "warn" | "error" | "info" | "neutral"
//   size   — "sm" | "md" (default "md")
//
// Default slot renders the label text.
//
// Example:
//   <ftw-badge status="ok">Connected</ftw-badge>
//   <ftw-badge status="warn" size="sm">Degraded</ftw-badge>
//   <ftw-badge status="error">Offline</ftw-badge>

import { FtwElement } from "./ftw-element.js";

class FtwBadge extends FtwElement {
  static styles = `
    :host {
      display: inline-flex;
      align-items: center;
      padding: 0.15rem 0.55rem;
      border-radius: 999px;
      font-size: 0.75rem;
      font-weight: 600;
      line-height: 1.4;
      letter-spacing: 0.02em;
      border: 1px solid transparent;
      background: var(--surface2);
      color: var(--text-dim);
      white-space: nowrap;
    }
    :host([size="sm"]) {
      padding: 0.05rem 0.4rem;
      font-size: 0.7rem;
    }
    :host([status="ok"]) {
      color: var(--green);
      border-color: color-mix(in srgb, var(--green) 45%, transparent);
      background: color-mix(in srgb, var(--green) 14%, var(--surface2));
    }
    :host([status="warn"]) {
      color: var(--yellow);
      border-color: color-mix(in srgb, var(--yellow) 45%, transparent);
      background: color-mix(in srgb, var(--yellow) 14%, var(--surface2));
    }
    :host([status="error"]) {
      color: var(--red);
      border-color: color-mix(in srgb, var(--red) 45%, transparent);
      background: color-mix(in srgb, var(--red) 14%, var(--surface2));
    }
    :host([status="info"]) {
      color: var(--blue);
      border-color: color-mix(in srgb, var(--blue) 45%, transparent);
      background: color-mix(in srgb, var(--blue) 14%, var(--surface2));
    }
  `;

  render() {
    return `<slot></slot>`;
  }
}

customElements.define("ftw-badge", FtwBadge);
