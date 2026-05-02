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

  // Upstream version checks don't change often; 3 h is plenty of
  // headroom to surface a new release on a normal workday without
  // hammering /api/version/check (which can hit GitHub each tick if
  // the local cache is stale).
  const CHECK_INTERVAL_MS = 3 * 60 * 60 * 1000; // /api/version/check cadence
  const STATUS_INTERVAL_MS = 2000;               // during updates
  const UPDATE_SOFT_TIMEOUT_MS = 180 * 1000;     // after this we stop auto-reloading

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
      this._skipSnapshot = false;     // per-session opt-out toggle (#149)
      this._snapshots = null;         // last /api/version/snapshots payload (#150)
      this._deletingSnapshot = null;  // id being deleted right now (#150)
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
      this._refreshSnapshots(); // pull the list for the Snapshots accordion
    }

    // Fetch the snapshot list so the operator sees the retained set and
    // can delete entries without SSH. Tolerates 503 (feature off) and
    // 404s silently — the UI simply hides the section.
    _refreshSnapshots() {
      if (this._disabled) return;
      fetch("/api/version/snapshots")
        .then((r) => (r.ok ? r.json() : null))
        .then((body) => {
          if (!body) return;
          this._snapshots = body;
          this._render();
        })
        .catch(() => { /* silent */ });
    }

    _deleteSnapshot(id) {
      if (!id) return;
      // Guard against rapid double-clicks while the request is pending.
      if (this._deletingSnapshot) return;
      this._deletingSnapshot = id;
      fetch("/api/version/snapshots/" + encodeURIComponent(id), { method: "DELETE" })
        .finally(() => {
          this._deletingSnapshot = null;
          this._refreshSnapshots();
        });
    }

    // _beginRollback kicks off a rollback-to-snapshot. Reuses the same
    // "updating" modal skin as _beginUpdate — the sidecar emits state
    // transitions (restoring → restarting → done) that feed straight
    // into the existing _tickStatus → _render path. See #152.
    _beginRollback(snapshotID) {
      this._phase = "updating";
      this._updateStartedAt = Date.now();
      this._updateOriginalVersion = this._info ? this._info.current : null;
      this._sidecarState = { state: "starting", action: "rollback", snapshot: snapshotID };
      this._render();

      this._postJSON("/api/version/rollback", { snapshot_id: snapshotID })
        .then((resp) => {
          if (!resp.ok) {
            this._sidecarState = { state: "failed", action: "rollback", message: (resp.body && resp.body.error) || "failed to start" };
            this._render();
            return;
          }
          this._startStatusPolling();
        })
        .catch((e) => {
          this._sidecarState = { state: "failed", action: "rollback", message: String(e) };
          this._render();
        });
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
      // For /update we ship a body so the operator can opt out of the
      // pre-update snapshot (retained set already covers them / tight
      // on disk). Restart doesn't snapshot anyway, so keep its body nil.
      let body = null;
      if (action === "update" && this._skipSnapshot) {
        body = { skip_snapshot: true };
      }
      this._postJSON(url, body)
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

      // Surface to the rest of the page via body class: the header's
      // green #conn-status dot sits right next to this badge, and
      // having both visible at once clutters the corner. CSS in
      // next.css hides #conn-status when .has-update is on, so the
      // two dots swap instead of stacking.
      if (typeof document !== "undefined" && document.body) {
        document.body.classList.toggle("has-update", !!showDot);
      }

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
            <button class="btn" data-action="restart">Restart</button>
            <button class="btn btn-ghost" data-action="skip">Skip this version</button>
          `
        : `
            <button class="btn" data-action="check">Check for updates</button>
            <button class="btn" data-action="restart">Restart</button>
          `;

      const notesHref = safeHref(info.release_notes_url);
      const notesLink = hasUpdate && notesHref
        ? `<a class="notes-link" href="${escapeHTML(notesHref)}" target="_blank" rel="noopener">Open on GitHub ↗</a>`
        : "";
      // Render the release body inline so the operator can read what's
      // about to be applied without opening a tab. Markdown is a small
      // subset (headings, lists, code, strong, safe links) — anything
      // else stays as plain escaped text. See renderReleaseBody.
      const bodyHTML = hasUpdate && info.release_body
        ? `<details class="changelog" open>
             <summary>What's in ${escapeHTML(info.latest || "this release")}</summary>
             <div class="changelog-body">${renderReleaseBody(info.release_body)}</div>
             ${notesLink ? `<p class="changelog-link">${notesLink}</p>` : ""}
           </details>`
        : (hasUpdate && notesLink ? `<p class="changelog-link">${notesLink}</p>` : "");

      // Reassure the operator that a rollback point will be captured
      // before the update runs — and let them opt out for this update
      // via a checkbox (the retained 5 older snapshots usually cover
      // them; power-users on small SD cards may not want another
      // ~200 MB). Default unchecked (safety first).
      const snapshotHint = hasUpdate
        ? `<div class="snapshot-hint">
             <p>🛟 A snapshot of your data and config is saved before each update so you can roll back if needed.</p>
             <label class="snapshot-skip">
               <input type="checkbox" data-action="toggle-skip-snapshot" ${this._skipSnapshot ? "checked" : ""}>
               Skip backup for this update
             </label>
           </div>`
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
            ${bodyHTML}
            ${snapshotHint}
            ${this._snapshotsSectionHTML()}
            ${info.err ? `<p class="err">Last check failed: ${escapeHTML(info.err)}</p>` : ""}
          </div>
          <footer>${actions}</footer>
        </div>
      `;
    }

    _snapshotsSectionHTML() {
      const payload = this._snapshots;
      if (!payload || !payload.enabled) return "";
      const snaps = Array.isArray(payload.snapshots) ? payload.snapshots : [];
      if (!snaps.length) {
        return `<details class="snapshots">
                  <summary>Backup snapshots (0)</summary>
                  <p class="dim snapshots-empty">No backups on disk yet. One is created before every update unless you opt out.</p>
                </details>`;
      }
      const rows = snaps.map((s) => this._snapshotRowHTML(s)).join("");
      return `<details class="snapshots">
                <summary>Backup snapshots (${snaps.length})</summary>
                <table class="snapshots-table">
                  <thead>
                    <tr><th>Created</th><th>From → To</th><th>Size</th><th></th></tr>
                  </thead>
                  <tbody>${rows}</tbody>
                </table>
              </details>`;
    }

    _snapshotRowHTML(s) {
      const when = s.created_at ? new Date(s.created_at).toLocaleString() : "?";
      const range = (s.from_version || "?") + " → " + (s.to_version || "?");
      const sizeMB = s.size_bytes ? (s.size_bytes / (1024 * 1024)).toFixed(1) + " MB" : "?";
      const deleting = this._deletingSnapshot === s.id;
      // Rollback target for a *pre-rollback* safety snapshot takes the
      // operator forward again — the 'from' version is what was running
      // when we captured it. For a routine pre-update snapshot the 'from'
      // version is what was running before that update — rolling back to
      // it reverts that update. Either way the operation is the same:
      // restore the files from this snapshot.
      const deleteBtn = deleting
        ? `<span class="dim">deleting…</span>`
        : `<button class="btn btn-ghost btn-small" data-action="delete-snapshot" data-id="${escapeHTML(s.id)}" title="Delete this backup">Delete</button>`;
      const rollbackBtn = deleting
        ? ""
        : `<button class="btn btn-small" data-action="rollback-snapshot" data-id="${escapeHTML(s.id)}" data-from="${escapeHTML(s.from_version || "")}" title="Restore this backup (service will restart)">Roll back</button>`;
      return `<tr>
                <td class="nowrap">${escapeHTML(when)}</td>
                <td class="mono">${escapeHTML(range)}</td>
                <td class="nowrap">${escapeHTML(sizeMB)}</td>
                <td class="snapshot-actions">${rollbackBtn}${deleteBtn}</td>
              </tr>`;
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

      let title;
      switch (action) {
        case "restart":  title = "Restarting service"; break;
        case "rollback": title = "Rolling back"; break;
        default:         title = "Updating service";
      }

      return `
        <div class="backdrop"></div>
        <div class="modal" role="dialog" aria-modal="true" aria-live="polite">
          <header>
            <h3>${title}</h3>
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
            case "toggle-skip-snapshot":
              // Don't re-render — the <input> element already reflects
              // its own state and a full render would reset focus.
              this._skipSnapshot = !!e.currentTarget.checked;
              break;
            case "delete-snapshot": {
              const id = e.currentTarget.dataset.id;
              // Simple confirm — this is a destructive operation but a
              // recoverable one (the retention/prune logic will regenerate
              // on future updates). Don't over-engineer the dialog.
              if (id && window.confirm(`Delete snapshot ${id}? This can't be undone.`)) {
                this._deleteSnapshot(id);
                this._render(); // reflect the "deleting…" state immediately
              }
              break;
            }
            case "rollback-snapshot": {
              const id = e.currentTarget.dataset.id;
              const from = e.currentTarget.dataset.from || "that point";
              // Sharper warning for rollback — it stops the service,
              // swaps live state, and restarts. Much more visible
              // consequence than a Delete.
              const msg =
                `Roll back to ${id}?\n\n` +
                `This will stop the service, restore state.db + config.yaml ` +
                `from the snapshot (state as of "${from}"), and restart. ` +
                `Any data written since the snapshot will be lost.\n\n` +
                `A pre-rollback backup of the current state is saved ` +
                `automatically so you can roll forward again.`;
              if (id && window.confirm(msg)) {
                this._beginRollback(id);
              }
              break;
            }
          }
        });
      });
    }

    _styles() {
      return `
        /* Inherit the dashboard tokens (--accent-e, --ink-raised, --fg, …)
           and font stacks (--sans, --mono) from theme.css instead of
           defaulting to "all: initial", which used to wall this shadow
           DOM off from the design system and force the legacy hex
           fallbacks (--surface, --border, --accent) to render. */
        :host {
          font-family: var(--sans, system-ui, -apple-system, sans-serif);
          color: var(--fg);
        }
        .hidden { display: none !important; }

        .badge {
          /* Pulsing amber accent dot — same visual language as the rest
             of the dashboard's actionable signals. The connection dot
             next door is green (state) and hides when this is on (see
             body.has-update rule in next.css), so we don't get two
             accent dots stacked. */
          appearance: none;
          background: transparent;
          color: var(--accent-e);
          border: none;
          cursor: pointer;
          font-size: 1.1rem;
          line-height: 1;
          padding: 0 0.3rem;
          animation: pulse 1.4s ease-in-out infinite;
        }
        @keyframes pulse {
          0%, 100% { opacity: 1; }
          50%      { opacity: 0.45; }
        }

        .backdrop {
          position: fixed; inset: 0;
          background: rgba(0, 0, 0, 0.65);
          z-index: 1000;
        }
        .modal {
          position: fixed;
          top: 50%; left: 50%;
          transform: translate(-50%, -50%);
          width: min(92vw, 460px);
          /* Cap height + scroll so shorter viewports can't push the
             header or footer off-screen on small laptops. */
          max-height: 88vh;
          overflow-y: auto;
          background: var(--ink-raised);
          color: var(--fg);
          border: 1px solid var(--line);
          border-radius: var(--radius-md, 14px);
          z-index: 1001;
          display: flex; flex-direction: column;
          font-family: var(--sans, system-ui, sans-serif);
          font-size: 0.9rem;
        }
        .modal header {
          display: flex; align-items: center; justify-content: space-between;
          gap: 0.75rem;
          padding: 14px 18px;
          border-bottom: 1px solid var(--line);
        }
        .modal h3 {
          margin: 0;
          font-size: 1rem;
          font-weight: 600;
        }
        .modal .x {
          appearance: none; background: transparent;
          color: var(--fg-dim);
          border: none; cursor: pointer;
          font-size: 1.5rem; line-height: 1;
          padding: 2px 6px;
          border-radius: 6px;
          transition: color 0.15s, background 0.15s;
        }
        .modal .x:hover { color: var(--fg); background: rgba(255, 255, 255, 0.05); }

        .modal .body { padding: 14px 18px; }
        .modal .body.center { text-align: center; padding: 22px 18px; }
        .subtitle { margin: 0 0 12px; color: var(--fg-dim); }

        /* Definition list — eyebrow labels are mono UPPERCASE 0.18em
           letter-spacing per DESIGN.md, values mono with tabular nums. */
        dl {
          margin: 0;
          display: grid;
          gap: 8px 14px;
          grid-template-columns: max-content 1fr;
          align-items: center;
        }
        dl > div { display: contents; }
        dt {
          color: var(--fg-muted);
          font-family: var(--mono);
          font-size: 0.68rem;
          font-weight: 500;
          letter-spacing: 0.18em;
          text-transform: uppercase;
        }
        dd {
          margin: 0;
          font-family: var(--mono);
          font-variant-numeric: tabular-nums;
          color: var(--fg);
        }

        .changelog,
        .snapshots {
          margin-top: 12px;
          border: 1px solid var(--line);
          border-radius: var(--radius-sm, 10px);
          background: var(--ink-sunken);
        }
        .changelog > summary,
        .snapshots > summary {
          padding: 8px 12px;
          cursor: pointer;
          font-weight: 600;
          font-size: 0.85rem;
          color: var(--fg-dim);
          list-style: none;
        }
        .changelog > summary::-webkit-details-marker,
        .snapshots > summary::-webkit-details-marker { display: none; }
        .changelog > summary::before,
        .snapshots > summary::before {
          content: "▸";
          display: inline-block;
          margin-right: 0.4rem;
          color: var(--fg-muted);
          transition: transform 0.15s;
        }
        .changelog[open] > summary::before,
        .snapshots[open] > summary::before { transform: rotate(90deg); }
        .changelog-body {
          padding: 4px 14px 8px;
          max-height: 40vh;
          overflow-y: auto;
          font-size: 0.85rem;
          line-height: 1.5;
        }
        .changelog-body h4 {
          margin: 12px 0 5px;
          font-size: 0.9rem;
          color: var(--fg);
        }
        .changelog-body h5 {
          margin: 10px 0 4px;
          font-size: 0.7rem;
          font-family: var(--mono);
          color: var(--fg-muted);
          text-transform: uppercase;
          letter-spacing: 0.18em;
          font-weight: 500;
        }
        .changelog-body ul { margin: 4px 0; padding-left: 1.1rem; }
        .changelog-body li { margin-bottom: 3px; }
        .changelog-body p  { margin: 6px 0; }
        .changelog-body code {
          background: var(--ink-raised);
          border: 1px solid var(--line);
          padding: 0.05rem 0.3rem;
          border-radius: 4px;
          font-family: var(--mono);
          font-size: 0.82rem;
        }
        .changelog-body a,
        .notes-link {
          color: var(--accent-e);
          text-decoration: none;
        }
        .changelog-body a:hover,
        .notes-link:hover { text-decoration: underline; }
        .changelog-link {
          margin: 6px 14px 8px;
          font-size: 0.8rem;
        }

        .snapshot-hint {
          margin-top: 12px;
          padding: 10px 12px;
          border: 1px solid var(--line);
          border-radius: var(--radius-sm, 10px);
          background: var(--ink-sunken);
          color: var(--fg-dim);
          font-size: 0.78rem;
          line-height: 1.45;
        }
        .snapshot-hint p { margin: 0; }
        .snapshot-skip {
          display: flex;
          align-items: center;
          gap: 0.4rem;
          margin-top: 8px;
          font-size: 0.76rem;
          color: var(--fg-dim);
          cursor: pointer;
          user-select: none;
        }
        .snapshot-skip input[type="checkbox"] {
          margin: 0;
          cursor: pointer;
          accent-color: var(--accent-e);
        }

        .snapshots-empty {
          margin: 4px 14px 8px;
          font-size: 0.78rem;
          color: var(--fg-dim);
        }
        .snapshots-table {
          width: 100%;
          border-collapse: collapse;
          font-size: 0.78rem;
          color: var(--fg-dim);
        }
        .snapshots-table th,
        .snapshots-table td {
          padding: 6px 12px;
          text-align: left;
          border-top: 1px solid var(--line);
        }
        .snapshots-table th {
          font-family: var(--mono);
          font-size: 0.65rem;
          font-weight: 500;
          letter-spacing: 0.18em;
          text-transform: uppercase;
          color: var(--fg-muted);
          border-top: none;
        }
        .snapshots-table .nowrap { white-space: nowrap; }
        .snapshots-table .mono   { font-family: var(--mono); }
        .snapshot-actions {
          display: flex;
          gap: 4px;
          justify-content: flex-end;
          flex-wrap: wrap;
        }
        .btn-small {
          padding: 4px 10px;
          font-size: 0.75rem;
        }

        .err {
          margin-top: 12px;
          color: var(--red-e);
          font-size: 0.85rem;
        }
        .dim { color: var(--fg-dim); font-size: 0.8rem; }

        .modal footer {
          display: flex;
          gap: 8px;
          justify-content: flex-end;
          padding: 12px 18px;
          border-top: 1px solid var(--line);
          flex-wrap: wrap;
          /* Stick to the modal bottom while the body scrolls so the
             primary action stays visible however long the release
             notes get. */
          position: sticky;
          bottom: 0;
          background: var(--ink-raised);
        }

        /* Buttons — DESIGN.md component vocabulary, exact values:
           Primary CTA: amber on near-black (never white), 11x18 px,
                        radius 8 px, 14 px / 500 weight, hover only
                        translateY(-1px) — no colour shift.
           Secondary / ghost: transparent + 1 px hairline, hover bumps
                        border-color to --fg-dim. Never changes
                        background — that's a hard rule. */
        .btn {
          appearance: none;
          padding: 11px 18px;
          border: 1px solid var(--line);
          background: transparent;
          color: var(--fg);
          border-radius: 8px;
          cursor: pointer;
          font-size: 14px;
          font-weight: 500;
          font-family: var(--sans);
          transition: border-color 0.15s, color 0.15s, transform 0.1s;
        }
        .btn:hover  { border-color: var(--fg-dim); }
        .btn:active { transform: translateY(1px); }
        .btn-primary {
          background: var(--accent-e);
          border-color: var(--accent-e);
          color: #0a0a0a;
        }
        .btn-primary:hover {
          background: var(--accent-e);
          border-color: var(--accent-e);
          transform: translateY(-1px);
        }
        .btn-ghost {
          color: var(--fg-dim);
          border-color: transparent;
        }
        .btn-ghost:hover {
          color: var(--fg);
          border-color: transparent;
        }

        .spinner {
          display: inline-block;
          width: 22px; height: 22px;
          border: 2px solid var(--line);
          border-top-color: var(--accent-e);
          border-radius: 50%;
          animation: spin 0.9s linear infinite;
          margin-bottom: 0.6rem;
        }
        @keyframes spin { to { transform: rotate(360deg); } }

        /* Phone — keep the modal centered and slim (sized to content,
           capped under the viewport), just wider than the desktop pill
           and with tighter paddings. Footer flips to a vertical stack
           with the primary action on top — easier thumb target than a
           wrapped row of pills crammed against the right edge. */
        @media (max-width: 600px) {
          .modal {
            width: calc(100vw - 16px);
            max-width: calc(100vw - 16px);
            max-height: 88dvh;
          }
          .modal header { padding: 12px 14px; }
          .modal .body  { padding: 14px; }
          .modal .body.center { padding: 22px 14px; }
          .modal footer {
            padding: 12px 14px;
            flex-direction: column-reverse;
            align-items: stretch;
          }
          .modal footer .btn,
          .modal footer .dim {
            width: 100%;
            text-align: center;
          }
          .changelog-body { max-height: 44vh; }
          .snapshots-table th,
          .snapshots-table td { padding: 6px 8px; }
          .snapshot-actions { justify-content: flex-start; }
        }
      `;
    }
  }

  function actionLabel(state, action) {
    switch (state) {
      case "pulling":    return "Pulling new image";
      case "restoring":  return "Restoring snapshot";
      case "restarting":
        if (action === "restart")  return "Restarting service";
        if (action === "rollback") return "Restarting on restored state";
        return "Applying update";
      case "done":       return "Reloading";
      case "failed":     return "Failed";
      default:
        if (action === "restart")  return "Restarting";
        if (action === "rollback") return "Starting rollback";
        return "Starting update";
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

  // renderReleaseBody turns GitHub-flavored markdown (as emitted by
  // semantic-release: headings, bullet lists, links, `code`, **bold**)
  // into a safe HTML subset. Strategy: escape everything first, then
  // rewrite a short whitelist of markdown tokens. Untrusted content —
  // link URLs — is routed through safeHref so a `javascript:` href
  // can't sneak in.
  //
  // What we handle (enough for conventional-commits changelogs):
  //   ##, ###           → h4, h5
  //   - x / * x         → unordered list (adjacent bullets grouped)
  //   **bold**          → <strong>
  //   `code`            → <code>
  //   [text](url)       → <a href=...>   (url filtered)
  //   blank line        → paragraph break
  //
  // What we deliberately drop: images, tables, raw HTML, setext
  // headings, nested lists, numbered lists. They're rare in release
  // notes and the operator still has the "Open on GitHub ↗" link for
  // the full formatted version.
  function renderReleaseBody(md) {
    const escaped = escapeHTML(String(md || "").trim());
    const lines = escaped.split(/\r?\n/);
    const out = [];
    let inList = false;
    const flushList = () => {
      if (inList) {
        out.push("</ul>");
        inList = false;
      }
    };
    for (let raw of lines) {
      const line = raw.replace(/\s+$/, "");
      if (!line) {
        flushList();
        continue;
      }
      // Bullet: "- text" or "* text" (leading spaces tolerated for
      // semantic-release which indents scope details).
      const bullet = line.match(/^\s*[*-]\s+(.*)$/);
      if (bullet) {
        if (!inList) {
          out.push("<ul>");
          inList = true;
        }
        out.push("<li>" + renderInline(bullet[1]) + "</li>");
        continue;
      }
      flushList();
      // Headings
      const h3 = line.match(/^###\s+(.*)$/);
      if (h3) { out.push("<h5>" + renderInline(h3[1]) + "</h5>"); continue; }
      const h2 = line.match(/^##\s+(.*)$/);
      if (h2) { out.push("<h4>" + renderInline(h2[1]) + "</h4>"); continue; }
      // Paragraph fallback.
      out.push("<p>" + renderInline(line) + "</p>");
    }
    flushList();
    return out.join("");
  }

  // renderInline handles **bold**, `code`, and [text](url) on an
  // already-HTML-escaped line. Order matters: code first so backticks
  // can't eat a `**bold**` marker that happened to be inside code.
  function renderInline(s) {
    // Inline code: backticks are already literal in the escaped text.
    s = s.replace(/`([^`]+)`/g, (_m, code) => "<code>" + code + "</code>");
    // Bold: **text**
    s = s.replace(/\*\*([^*]+)\*\*/g, "<strong>$1</strong>");
    // Links: [text](url). The URL has been HTML-escaped already (amp →
    // &amp;), so decode just the &amp; inside the href before running
    // safeHref — otherwise a legitimate query-string URL gets rejected.
    s = s.replace(/\[([^\]]+)\]\(([^)]+)\)/g, (_m, text, url) => {
      const clean = String(url).replace(/&amp;/g, "&");
      const safe = safeHref(clean);
      if (!safe) return text; // drop the link, keep the visible text
      return '<a href="' + escapeHTML(safe) + '" target="_blank" rel="noopener">' + text + "</a>";
    });
    return s;
  }

  customElements.define("ftw-update-badge", FtwUpdateBadge);
})();
