// Settings → Advanced (YAML) tab. Full-text config editor with
// CodeMirror 6 + YAML syntax highlighting + live lint + server-side
// validate + diff-confirm on save.
//
// Dependencies live in /vendor/codemirror/codemirror-yaml.bundle.js
// (CodeMirror 6 + @codemirror/lang-yaml + @codemirror/lint +
// js-yaml). The bundle is imported lazily on tab open so operators
// who never touch this tab pay nothing in startup bandwidth.
//
// This tab does NOT use the shell's generic Save button — the
// footer button POSTs /api/config as JSON, which would clobber the
// in-progress YAML edit. The tab hides that button on open and
// restores it on tab-switch, rendering its own "Validate & Save…"
// flow inside the body.
(function () {
  var S = (window.FTWSettings = window.FTWSettings || { tabs: {} });
  S.tabs = S.tabs || {};

  var cmModulePromise = null;
  function loadCM() {
    if (!cmModulePromise) {
      cmModulePromise = import("/vendor/codemirror/codemirror-yaml.bundle.js");
    }
    return cmModulePromise;
  }

  S.tabs.advanced = {
    render: function () {
      return (
        '<style>' +
        '  .adv-wrap { display: flex; flex-direction: column; gap: 10px; height: 65vh; min-height: 380px; }' +
        '  .adv-header { display: flex; gap: 8px; align-items: center; justify-content: flex-end; }' +
        '  .adv-header .adv-status { flex: 1 1 auto; font-family: var(--mono, monospace); font-size: 0.85rem; color: var(--text-dim); }' +
        '  .adv-header .adv-status.error { color: #e57373; }' +
        '  .adv-header .adv-status.success { color: #81c995; }' +
        '  .adv-editor { flex: 1 1 auto; min-height: 0; border: 1px solid var(--line, rgba(255,255,255,0.12)); border-radius: 6px; overflow: hidden; background: #f7f7f2; color: #1b1b1b; }' +
        '  .adv-editor .cm-editor { height: 100%; background: #f7f7f2; color: #1b1b1b; }' +
        '  .adv-editor .cm-gutters { background: #ececd9; color: #7a7a68; border-right: 1px solid #d5d5c2; }' +
        '  .adv-editor .cm-activeLine { background: rgba(255, 230, 130, 0.22); }' +
        '  .adv-editor .cm-activeLineGutter { background: rgba(255, 230, 130, 0.35); }' +
        '  .adv-editor .cm-selectionBackground, .adv-editor .cm-content ::selection { background: rgba(80, 140, 255, 0.28) !important; }' +
        '  .adv-editor .cm-cursor { border-left-color: #1b1b1b; }' +
        '  .adv-editor .cm-focused { outline: none; }' +
        /* Override the bundled (dark-theme) syntax colors so tokens
           read well against the light background. Targets the class
           names emitted by @codemirror/language\'s defaultHighlightStyle. */
        '  .adv-editor .tok-keyword, .adv-editor .ͼm { color: #a33; font-weight: 600; }' +
        '  .adv-editor .tok-string, .adv-editor .ͼp { color: #0a6a0a; }' +
        '  .adv-editor .tok-number, .adv-editor .tok-bool, .adv-editor .tok-null { color: #9a5f00; }' +
        '  .adv-editor .tok-comment, .adv-editor .ͼl { color: #7c7c7c; font-style: italic; }' +
        '  .adv-editor .tok-propertyName, .adv-editor .tok-typeName, .adv-editor .ͼk { color: #1a4bb5; }' +
        '  .adv-btn { padding: 6px 12px; border-radius: 4px; border: 1px solid var(--line, rgba(255,255,255,0.15)); background: rgba(255,255,255,0.06); color: inherit; cursor: pointer; font: inherit; }' +
        '  .adv-btn:hover { background: rgba(255,255,255,0.1); }' +
        '  .adv-btn.primary { background: var(--accent, #4aa3ff); border-color: var(--accent, #4aa3ff); color: #000; font-weight: 600; }' +
        '  .adv-btn.primary:hover { filter: brightness(1.1); }' +
        '  .adv-btn[disabled] { opacity: 0.5; cursor: not-allowed; }' +
        '  .adv-diff-backdrop { position: fixed; inset: 0; background: rgba(0,0,0,0.55); display: flex; align-items: center; justify-content: center; z-index: 100; }' +
        '  .adv-diff-backdrop.hidden { display: none; }' +
        '  .adv-diff-card { width: min(720px, 90vw); max-height: 80vh; background: var(--bg, #1a1d22); border: 1px solid var(--line, rgba(255,255,255,0.12)); border-radius: 8px; padding: 18px; display: flex; flex-direction: column; gap: 12px; }' +
        '  .adv-diff-card h3 { margin: 0; font-size: 1rem; }' +
        '  .adv-diff-card pre { flex: 1 1 auto; overflow: auto; margin: 0; padding: 10px; background: rgba(0,0,0,0.35); border-radius: 4px; font-family: var(--mono, monospace); font-size: 0.8rem; line-height: 1.4; white-space: pre; max-height: 50vh; }' +
        '  .adv-diff-card pre .add { color: #81c995; }' +
        '  .adv-diff-card pre .rm { color: #e57373; }' +
        '  .adv-diff-card pre .hdr { color: #8ab4f8; }' +
        '  .adv-diff-card .adv-diff-actions { display: flex; gap: 8px; justify-content: flex-end; }' +
        '  .adv-diff-card .adv-diff-empty { color: var(--text-dim); font-style: italic; text-align: center; padding: 24px; }' +
        '</style>' +
        '<div class="adv-wrap">' +
        '  <div class="adv-header">' +
        '    <span class="adv-status" id="adv-status">Loading editor…</span>' +
        '    <button class="adv-btn" id="adv-reload" type="button">Reload</button>' +
        '    <button class="adv-btn primary" id="adv-save" type="button" disabled>Validate &amp; Save…</button>' +
        '  </div>' +
        '  <div class="adv-editor" id="adv-editor"></div>' +
        '</div>' +
        '<div class="adv-diff-backdrop hidden" id="adv-diff-modal">' +
        '  <div class="adv-diff-card">' +
        '    <h3>Review changes</h3>' +
        '    <div id="adv-diff-content"></div>' +
        '    <div class="adv-diff-actions">' +
        '      <button class="adv-btn" id="adv-diff-cancel" type="button">Cancel</button>' +
        '      <button class="adv-btn primary" id="adv-diff-apply" type="button">Apply</button>' +
        '    </div>' +
        '  </div>' +
        '</div>'
      );
    },
    after: function (ctx) {
      var statusEl = document.getElementById("adv-status");
      var editorHost = document.getElementById("adv-editor");
      var saveBtn = document.getElementById("adv-save");
      var reloadBtn = document.getElementById("adv-reload");
      var diffModal = document.getElementById("adv-diff-modal");
      var diffContent = document.getElementById("adv-diff-content");
      var diffApply = document.getElementById("adv-diff-apply");
      var diffCancel = document.getElementById("adv-diff-cancel");

      // Hide the shell's footer save/status while Advanced is active.
      var footerSave = document.getElementById("settings-save");
      var footerStatus = document.getElementById("settings-status");
      var prevSave = footerSave ? footerSave.style.display : "";
      var prevStatus = footerStatus ? footerStatus.style.display : "";
      if (footerSave) footerSave.style.display = "none";
      if (footerStatus) footerStatus.style.display = "none";
      var tabsEl = document.getElementById("settings-tabs");
      function restoreFooter() {
        if (footerSave) footerSave.style.display = prevSave;
        if (footerStatus) footerStatus.style.display = prevStatus;
        if (tabsEl) tabsEl.removeEventListener("click", restoreFooter);
      }
      if (tabsEl) tabsEl.addEventListener("click", restoreFooter);

      function setStatus(msg, kind) {
        statusEl.textContent = msg || "";
        statusEl.className = "adv-status" + (kind ? " " + kind : "");
      }

      var view = null;

      function loadDoc() {
        setStatus("Loading…");
        return fetch("/api/config/raw")
          .then(function (r) {
            if (!r.ok) throw new Error("HTTP " + r.status);
            return r.text();
          })
          .then(function (doc) {
            if (view) {
              view.dispatch({ changes: { from: 0, to: view.state.doc.length, insert: doc } });
            } else {
              loadCM().then(function (cm) {
                editorHost.innerHTML = "";
                view = cm.createEditor(editorHost, { doc: doc });
                saveBtn.disabled = false;
                // Save on Cmd/Ctrl+S from inside the editor.
                editorHost.addEventListener("keydown", function (e) {
                  if ((e.metaKey || e.ctrlKey) && e.key === "s") {
                    e.preventDefault();
                    saveBtn.click();
                  }
                });
              });
            }
            setStatus("Ready");
          })
          .catch(function (e) {
            setStatus("Load failed: " + e.message, "error");
          });
      }

      function renderDiff(diffText) {
        if (!diffText || !diffText.trim()) {
          diffContent.innerHTML = '<div class="adv-diff-empty">No changes detected.</div>';
          diffApply.disabled = true;
          return;
        }
        diffApply.disabled = false;
        // Colorize unified-diff lines; trivial renderer, no library.
        var lines = diffText.split("\n").map(function (line) {
          var esc = line
            .replace(/&/g, "&amp;")
            .replace(/</g, "&lt;")
            .replace(/>/g, "&gt;");
          if (/^@@/.test(line)) return '<span class="hdr">' + esc + "</span>";
          if (/^\+\+\+|^---/.test(line)) return '<span class="hdr">' + esc + "</span>";
          if (/^\+/.test(line)) return '<span class="add">' + esc + "</span>";
          if (/^-/.test(line)) return '<span class="rm">' + esc + "</span>";
          return esc;
        });
        diffContent.innerHTML = "<pre>" + lines.join("\n") + "</pre>";
      }

      function openDiffModal() {
        diffModal.classList.remove("hidden");
      }
      function closeDiffModal() {
        diffModal.classList.add("hidden");
      }

      saveBtn.addEventListener("click", function () {
        if (!view) return;
        var body = view.state.doc.toString();
        setStatus("Validating…");
        fetch("/api/config/validate", {
          method: "POST",
          headers: { "Content-Type": "application/yaml" },
          body: body,
        })
          .then(function (r) { return r.json(); })
          .then(function (j) {
            if (!j.ok) {
              var msg = j.error || "Validation failed";
              if (j.line) msg = "Line " + j.line + ": " + msg;
              setStatus(msg, "error");
              return;
            }
            renderDiff(j.diff || "");
            openDiffModal();
            setStatus("Ready");
          })
          .catch(function (e) {
            setStatus("Validate failed: " + e.message, "error");
          });
      });

      diffCancel.addEventListener("click", closeDiffModal);
      diffApply.addEventListener("click", function () {
        if (!view) return;
        var body = view.state.doc.toString();
        diffApply.disabled = true;
        setStatus("Saving…");
        fetch("/api/config/raw", {
          method: "POST",
          headers: { "Content-Type": "application/yaml" },
          body: body,
        })
          .then(function (r) {
            if (!r.ok) {
              return r.json().then(function (j) {
                throw new Error(j.error || "HTTP " + r.status);
              });
            }
            return r.json();
          })
          .then(function () {
            closeDiffModal();
            setStatus("Saved ✓", "success");
            // Reload from server to pick up any normalization the
            // yaml.Marshal on the write path performed.
            return loadDoc();
          })
          .catch(function (e) {
            setStatus("Save failed: " + e.message, "error");
          })
          .finally(function () {
            diffApply.disabled = false;
          });
      });

      reloadBtn.addEventListener("click", loadDoc);

      loadDoc();
    },
  };
})();
