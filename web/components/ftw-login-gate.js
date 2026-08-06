// <ftw-login-gate> — full-screen login overlay for api.auth.mode.
//
// On connect it asks /api/auth/session. In open mode (or when already
// logged in) it removes itself and the dashboard renders untouched. In
// local_trust/required without a session it covers the app with a login
// form; a successful login reloads the page so every component fetches
// with the session cookie from the start.
//
// The logout affordance renders into the header's settings area when a
// session is active (small button, username + role tooltip).

import { FtwElement } from "./ftw-element.js";
import { apiFetch } from "./api-fetch.js";
import { shouldShowLogin, loginErrorText } from "./login-math.js";

class FtwLoginGate extends FtwElement {
  static styles = `
    :host { display: contents; }
    .overlay {
      position: fixed; inset: 0; z-index: 10000;
      display: flex; align-items: center; justify-content: center;
      background: var(--ink, #0a0a0a);
    }
    form {
      display: flex; flex-direction: column; gap: 12px;
      width: min(320px, 86vw);
      background: var(--ink-raised);
      border: 1px solid var(--line);
      border-radius: var(--radius-md, 10px);
      padding: 28px 24px;
    }
    h1 {
      margin: 0 0 4px;
      font-family: var(--mono);
      font-size: 0.85rem;
      letter-spacing: 0.18em;
      text-transform: uppercase;
      color: var(--fg-label);
    }
    label {
      font-family: var(--mono);
      font-size: 10px;
      letter-spacing: 0.1em;
      text-transform: uppercase;
      color: var(--fg-label);
      display: flex; flex-direction: column; gap: 5px;
    }
    input {
      font: inherit;
      color: var(--fg);
      background: var(--ink-sunken);
      border: 1px solid var(--line);
      border-radius: 6px;
      padding: 9px 10px;
    }
    input:focus-visible { outline: 1px solid var(--accent-e); }
    button {
      font-family: var(--mono);
      font-size: 0.8rem;
      letter-spacing: 0.08em;
      text-transform: uppercase;
      color: var(--on-accent, #0a0a0a);
      background: var(--accent-e);
      border: 0; border-radius: 6px;
      padding: 10px;
      cursor: pointer;
      margin-top: 4px;
    }
    button[disabled] { opacity: 0.6; cursor: wait; }
    .err {
      min-height: 1.2em;
      font-size: 0.8rem;
      color: var(--bad, #e05555);
    }
  `;

  connectedCallback() {
    super.connectedCallback();
    this._check();
  }

  async _check() {
    try {
      const res = await apiFetch("/api/auth/session");
      if (!res.ok) {
        this.remove();
        return;
      }
      const session = await res.json();
      if (!shouldShowLogin(session)) {
        this.remove();
        return;
      }
      this._show = true;
      this.update();
      this.shadowRoot.querySelector("#user")?.focus();
    } catch {
      this.remove(); // unreachable backend: the app's own error UI owns this
    }
  }

  render() {
    if (!this._show) return "";
    return `
      <div class="overlay" role="dialog" aria-labelledby="login-title">
        <form>
          <h1 id="login-title">FTW · Sign in</h1>
          <label>Username <input id="user" name="username" autocomplete="username" required></label>
          <label>Password <input id="pass" name="password" type="password" autocomplete="current-password" required></label>
          <div class="err" id="err" role="alert"></div>
          <button type="submit" id="submit">Sign in</button>
        </form>
      </div>`;
  }

  afterRender() {
    const form = this.shadowRoot.querySelector("form");
    if (!form) return;
    form.addEventListener("submit", async (e) => {
      e.preventDefault();
      const btn = this.shadowRoot.querySelector("#submit");
      const err = this.shadowRoot.querySelector("#err");
      btn.disabled = true;
      err.textContent = "";
      try {
        const res = await apiFetch("/api/auth/login", {
          method: "POST",
          headers: { "Content-Type": "application/json" },
          body: JSON.stringify({
            username: this.shadowRoot.querySelector("#user").value.trim(),
            password: this.shadowRoot.querySelector("#pass").value,
          }),
        });
        if (res.ok) {
          location.reload();
          return;
        }
        err.textContent = loginErrorText(res.status);
      } catch {
        err.textContent = "Network error — try again.";
      } finally {
        btn.disabled = false;
      }
    });
  }
}

customElements.define("ftw-login-gate", FtwLoginGate);
