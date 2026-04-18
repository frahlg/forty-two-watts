// <ftw-update-badge> — self-contained Web Component that checks for a
// newer forty-two-watts image, renders a notification dot in the header,
// and drives the update/restart flow end-to-end (pull → recreate →
// auto-reload). Everything lives in shadow DOM so dashboard styles are
// untouched.
//
// Placement: one <ftw-update-badge></ftw-update-badge> inside the header.
// The element exposes a public open() method so the #version span (which
// lives outside shadow DOM) can also trigger the modal.

(function () {
  "use strict";

  const CHECK_INTERVAL_MS = 10 * 60 * 1000; // /api/version/check cadence
  const STATUS_INTERVAL_MS = 2000;           // during updates
  const UPDATE_SOFT_TIMEOUT_MS = 180 * 1000; // after this we stop auto-reloading

  class FtwUpdateBadge extends HTMLElement {
    constructor() {
      super();
      this._shadow = this.attachShadow({ mode: "open" });
      this._info = null;              // last /api/version/check payload
      this._phase = "idle";           // idle | dialog | updating
      this._sidecarState = null;      // last /api/version/update/status
      this._updateStartedAt = 0;
      this._updateOriginalVersion = null;
      this._checkTimer = null;
      this._statusTimer = null;
      this._disabled = false;         // set true on 503 (feature gated off)
      this._render();
    }

    connectedCallback() {
      this._refresh(false);
      this._checkTimer = setInterval(() => this._refresh(false), CHECK_INTERVAL_MS);
    }

    disconnectedCallback() {
      clearInterval(this._checkTimer);
      clearInterval(this._statusTimer);
    }

    // Public: called by the header #version click handler in index.html so
    // the operator can open the modal without aiming at the tiny dot. No-op
    // when the backend has told us the feature is gated off.
    open() {
      if (this._disabled) return;
      this._phase = "dialog";
      this._render();
      this._refresh(false); // surface the freshest info when opened
    }

    // Permanently shut the element down: stop polling, clear shadow DOM, hide
    // from layout, and fire an event so the #version bridge can drop its
    // cursor/pointer affordance. Called when the backend returns 503, which
    // means the feature is gated off (FTW_SELFUPDATE_ENABLED unset) — not a
    // transient error, so we don't ever retry.
    _disable() {
      if (this._disabled) return;
      this._disabled = true;
      clearInterval(this._checkTimer);
      clearInterval(this._statusTimer);
      this._shadow.innerHTML = "";
      this.hidden = true;
      this.dispatchEvent(new CustomEvent("ftw-selfupdate-disabled", { bubbles: true }));
    }

    // ---- data ----
    _refresh(force) {
      if (this._disabled) return;
      const url = force ? "/api/version/check?force=1" : "/api/version/check";
      fetch(url)
        .then((r) => {
          // 503 = feature disabled by the backend. Stop polling and get out
          // of the way entirely — this is deployment config, not a bug.
          if (r.status === 503) {
            this._disable();
            return null;
          }
          return r.json()
            .then((body) => ({ ok: r.ok, body }))
            .catch(() => ({ ok: r.ok, body: null }));
        })
        .then((result) => {
          if (!result) return; // disabled, nothing to render
          // The handler returns the full Info schema on both success and the
          // force=1 error path, so we render either way. When ok=false,
          // body.err carries the reason and the UI shows "Last check failed".
          if (result.body && typeof result.body === "object") {
            this._info = result.body;
            this._render();
          }
        })
        .catch(() => { /* silent — periodic noise is not useful */ });
    }

    _postJSON(url, body) {
      return fetch(url, {
        method: "POST",
        headers: { "Content-Type": "application/json" },
        body: body ? JSON.stringify(body) : undefined,
      }).then((r) => r.json().then((j) => ({ ok: r.ok, body: j })));
    }

    // ---- actions ----
    _skip() {
      if (!this._info || !this._info.latest) return;
      this._postJSON("/api/version/skip", { version: this._info.latest }).then(() => {
        this._phase = "idle";
        this._refresh(false);
      });
    }

    _unskipAndCheck() {
      // "Check for updates" also clears skip so a hidden version resurfaces
      // without waiting for something newer. Matches user intent: if you're
      // asking, you want to see it.
      this._postJSON("/api/version/unskip", null).finally(() => this._refresh(true));
    }

    _beginUpdate(action) {
      this._phase = "updating";
      this._updateStartedAt = Date.now();
      this._updateOriginalVersion = this._info ? this._info.current : null;
      this._sidecarState = { state: "starting", action };
      this._render();

      const url = action === "restart" ? "/api/version/restart" : "/api/version/update";
      this._postJSON(url, null)
        .then((resp) => {
          if (!resp.ok) {
            this._sidecarState = { state: "failed", action, message: (resp.body && resp.body.error) || "failed to start" };
            this._render();
            return;
          }
          this._startStatusPolling();
        })
        .catch((e) => {
          this._sidecarState = { state: "failed", action, message: String(e) };
          this._render();
        });
    }

    _startStatusPolling() {
      clearInterval(this._statusTimer);
      this._statusTimer = setInterval(() => this._tickStatus(), STATUS_INTERVAL_MS);
      this._tickStatus();
    }

    _tickStatus() {
      // 1) Poll sidecar state.json.
      fetch("/api/version/update/status")
        .then((r) => (r.ok ? r.json() : null))
        .then((st) => {
          if (st) {
            this._sidecarState = st;
            this._render();
            if (st.state === "done") {
              this._attemptReload();
            }
          }
        })
        .catch(() => {
          // Main container is likely mid-restart; expected — keep polling.
        });

      // 2) If we've been updating too long with no progress, give the user
      // a manual reload escape hatch instead of spinning forever.
      if (Date.now() - this._updateStartedAt > UPDATE_SOFT_TIMEOUT_MS) {
        if (this._sidecarState && this._sidecarState.state !== "done") {
          this._sidecarState = Object.assign({}, this._sidecarState, { timedOut: true });
          this._render();
        }
      }
    }

    _attemptReload() {
      // Give the new container a moment to open its listener, then
      // hard-reload. Bypass cache so a new app.js version is picked up.
      clearInterval(this._statusTimer);
      setTimeout(() => {
        // location.reload(true) is deprecated; a cache-busting query is a
        // reliable cross-browser alternative that forces a fresh index.html.
        const u = new URL(window.location.href);
        u.searchParams.set("_u", Date.now().toString());
        window.location.replace(u.toString());
      }, 800);
    }

    // ---- render ----
    _render() {
      const info = this._info || {};
      const showDot = info.update_available && !info.skipped && this._phase !== "updating";

      this._shadow.innerHTML = `
        <style>${this._styles()}</style>
        <button part="badge" class="badge${showDot ? "" : " hidden"}" title="Update available: ${escapeHTML(info.latest || "")}" aria-label="Update available">●</button>
        ${this._phase !== "idle" ? this._modalHTML() : ""}
      `;

      const btn = this._shadow.querySelector(".badge");
      if (btn) btn.addEventListener("click", () => this.open());

      const modal = this._shadow.querySelector(".modal");
      if (modal) this._wireModal(modal);
    }

    _modalHTML() {
      const info = this._info || {};
      if (this._phase === "updating") return this._updatingModalHTML();

      const hasUpdate = !!info.update_available;
      const subtitle = hasUpdate
        ? `A newer release is available.`
        : `You're running the latest release.`;

      const actions = hasUpdate
        ? `
            <button class="btn btn-primary" data-action="update">Update to ${escapeHTML(info.latest || "")}</button>
            <button class="btn" data-action="restart">Restart (test)</button>
            <button class="btn btn-ghost" data-action="skip">Skip this version</button>
          `
        : `
            <button class="btn" data-action="check">Check for updates</button>
            <button class="btn" data-action="restart">Restart (test)</button>
          `;

      const notesHref = safeHref(info.release_notes_url);
      const notes = hasUpdate && notesHref
        ? `<a class="notes" href="${escapeHTML(notesHref)}" target="_blank" rel="noopener">Release notes ↗</a>`
        : "";

      return `
        <div class="backdrop" data-action="close"></div>
        <div class="modal" role="dialog" aria-modal="true" aria-labelledby="ftw-upd-title">
          <header>
            <h3 id="ftw-upd-title">forty-two-watts</h3>
            <button class="x" data-action="close" aria-label="Close">×</button>
          </header>
          <div class="body">
            <p class="subtitle">${escapeHTML(subtitle)}</p>
            <dl>
              <div><dt>Current</dt><dd>${escapeHTML(info.current || "?")}</dd></div>
              ${info.latest ? `<div><dt>Latest</dt><dd>${escapeHTML(info.latest)}</dd></div>` : ""}
              ${info.skipped_version ? `<div><dt>Skipped</dt><dd>${escapeHTML(info.skipped_version)}</dd></div>` : ""}
            </dl>
            ${notes}
            ${info.err ? `<p class="err">Last check failed: ${escapeHTML(info.err)}</p>` : ""}
          </div>
          <footer>${actions}</footer>
        </div>
      `;
    }

    _updatingModalHTML() {
      const st = this._sidecarState || { state: "starting" };
      const action = st.action || "update";
      const elapsed = Math.round((Date.now() - this._updateStartedAt) / 1000);
      const label = actionLabel(st.state, action);
      const spinner = st.state === "failed" ? "" : `<span class="spinner"></span>`;
      const timedOut = !!st.timedOut;
      const failed = st.state === "failed";

      const body = failed
        ? `<p class="err">${escapeHTML(st.message || "Update failed")}</p>
           <p>The main service may still be running — reload the page to check.</p>`
        : timedOut
        ? `<p>Still working after ${elapsed}s. The main container may have been slow to restart.</p>
           <p>You can reload manually if the UI keeps the overlay stuck.</p>`
        : `<p>${escapeHTML(label)}…</p>
           <p class="dim">Elapsed: ${elapsed}s. The page will reload automatically.</p>`;

      const footer = failed || timedOut
        ? `<button class="btn btn-primary" data-action="reload">Reload page</button>
           <button class="btn btn-ghost" data-action="close">Dismiss</button>`
        : `<span class="dim">Don't close this tab.</span>`;

      return `
        <div class="backdrop"></div>
        <div class="modal" role="dialog" aria-modal="true" aria-live="polite">
          <header>
            <h3>${action === "restart" ? "Restarting service" : "Updating service"}</h3>
          </header>
          <div class="body center">
            ${spinner}
            ${body}
          </div>
          <footer>${footer}</footer>
        </div>
      `;
    }

    _wireModal(modal) {
      // Delegate: one listener on the shadow root, dispatch by data-action.
      this._shadow.querySelectorAll("[data-action]").forEach((el) => {
        el.addEventListener("click", (e) => {
          const action = e.currentTarget.dataset.action;
          switch (action) {
            case "close":
              this._phase = "idle";
              this._render();
              break;
            case "update":
              this._beginUpdate("update");
              break;
            case "restart":
              this._beginUpdate("restart");
              break;
            case "skip":
              this._skip();
              break;
            case "check":
              this._unskipAndCheck();
              break;
            case "reload":
              this._attemptReload();
              break;
          }
        });
      });
    }

    _styles() {
      return `
        :host { all: initial; font-family: inherit; }
        .hidden { display: none !important; }
        .badge {
          appearance: none;
          background: transparent;
          color: var(--accent, #f59e0b);
          border: none;
          cursor: pointer;
          font-size: 1.1rem;
          line-height: 1;
          padding: 0 0.3rem;
          animation: pulse 2s ease-in-out infinite;
        }
        @keyframes pulse {
          0%, 100% { opacity: 1; }
          50%      { opacity: 0.45; }
        }
        .backdrop {
          position: fixed; inset: 0;
          background: rgba(0,0,0,0.65);
          z-index: 1000;
        }
        .modal {
          position: fixed;
          top: 50%; left: 50%;
          transform: translate(-50%, -50%);
          width: min(92vw, 460px);
          background: var(--surface, #1e293b);
          color: var(--text, #e2e8f0);
          border: 1px solid var(--border, #334155);
          border-radius: 8px;
          z-index: 1001;
          display: flex; flex-direction: column;
          font-family: system-ui, -apple-system, sans-serif;
          font-size: 0.9rem;
        }
        .modal header {
          display: flex; align-items: center; justify-content: space-between;
          padding: 0.9rem 1rem;
          border-bottom: 1px solid var(--border, #334155);
        }
        .modal h3 { margin: 0; font-size: 1rem; }
        .modal .x {
          appearance: none; background: transparent;
          color: var(--text-dim, #94a3b8);
          border: none; cursor: pointer;
          font-size: 1.25rem; line-height: 1;
        }
        .modal .body { padding: 1rem; }
        .modal .body.center { text-align: center; padding: 1.4rem 1rem; }
        .subtitle { margin: 0 0 0.75rem; color: var(--text-dim, #94a3b8); }
        dl { margin: 0; display: grid; gap: 0.35rem; grid-template-columns: auto 1fr; }
        dl > div { display: contents; }
        dt { color: var(--text-dim, #94a3b8); font-size: 0.8rem; }
        dd { margin: 0; font-variant-numeric: tabular-nums; }
        .notes {
          display: inline-block; margin-top: 0.75rem;
          color: var(--accent, #f59e0b);
          text-decoration: none; font-size: 0.85rem;
        }
        .notes:hover { text-decoration: underline; }
        .err {
          margin-top: 0.75rem;
          color: #f87171; font-size: 0.85rem;
        }
        .dim { color: var(--text-dim, #94a3b8); font-size: 0.8rem; }
        .modal footer {
          display: flex; gap: 0.5rem; justify-content: flex-end;
          padding: 0.75rem 1rem;
          border-top: 1px solid var(--border, #334155);
          flex-wrap: wrap;
        }
        .btn {
          appearance: none;
          padding: 0.4rem 0.9rem;
          border: 1px solid var(--border, #334155);
          background: transparent;
          color: var(--text, #e2e8f0);
          border-radius: 4px;
          cursor: pointer;
          font-size: 0.85rem;
          font-family: inherit;
        }
        .btn:hover { background: rgba(255,255,255,0.04); }
        .btn-primary {
          background: var(--accent, #f59e0b);
          border-color: var(--accent, #f59e0b);
          color: #0f172a;
        }
        .btn-primary:hover { opacity: 0.9; background: var(--accent, #f59e0b); }
        .btn-ghost { color: var(--text-dim, #94a3b8); border-color: transparent; }
        .spinner {
          display: inline-block;
          width: 20px; height: 20px;
          border: 2px solid var(--border, #334155);
          border-top-color: var(--accent, #f59e0b);
          border-radius: 50%;
          animation: spin 0.9s linear infinite;
          margin-bottom: 0.6rem;
        }
        @keyframes spin { to { transform: rotate(360deg); } }
      `;
    }
  }

  function actionLabel(state, action) {
    switch (state) {
      case "pulling":    return "Pulling new image";
      case "restarting": return action === "restart" ? "Restarting service" : "Applying update";
      case "done":       return "Reloading";
      case "failed":     return "Failed";
      default:           return action === "restart" ? "Restarting" : "Starting update";
    }
  }

  // safeHref rejects anything that isn't http:/https:. The release-notes URL
  // comes from the GitHub Releases API, but we belt-and-brace here: an
  // attacker who somehow lands a javascript:/data: URL into the payload
  // shouldn't get code execution via the anchor href.
  function safeHref(u) {
    if (!u) return "";
    try {
      const p = new URL(String(u), window.location.href);
      if (p.protocol === "http:" || p.protocol === "https:") return p.toString();
    } catch (_) { /* fall through */ }
    return "";
  }

  function escapeHTML(s) {
    return String(s == null ? "" : s)
      .replace(/&/g, "&amp;")
      .replace(/</g, "&lt;")
      .replace(/>/g, "&gt;")
      .replace(/"/g, "&quot;")
      .replace(/'/g, "&#39;");
  }

  customElements.define("ftw-update-badge", FtwUpdateBadge);
})();
