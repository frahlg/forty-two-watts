// Ask why — explain the live site and draft a GitHub issue.
//
// Bound to #plan-ask-why and the header offline chip. The box runs a
// read-only tool loop. Model output is rendered as text, never HTML.
(function () {
  "use strict";

  var ISSUE_TEMPLATE_URL = "https://github.com/srcfl/ftw/issues/new?template=bug_report.yml";

  function apiFetch(path, opts) {
    return fetch(path, opts);
  }

  function escHtml(s) {
    var div = document.createElement("div");
    div.textContent = s == null ? "" : String(s);
    return div.innerHTML;
  }

  function ensureStyles() {
    if (document.getElementById("ftw-ask-styles")) return;
    var style = document.createElement("style");
    style.id = "ftw-ask-styles";
    style.textContent = [
      ".ftw-ask-backdrop{position:fixed;inset:0;background:rgba(0,0,0,0.55);z-index:9000;display:flex;align-items:center;justify-content:center;}",
      ".ftw-ask-shell{width:min(680px,94vw);max-height:90vh;display:flex;flex-direction:column;background:var(--ink-raised,#161616);border:1px solid var(--line,#2a2a2a);border-radius:10px;overflow:hidden;}",
      ".ftw-ask-head{display:flex;align-items:center;justify-content:space-between;gap:12px;padding:14px 18px;border-bottom:1px solid var(--line,#2a2a2a);background:var(--ink,#111);}",
      ".ftw-ask-title{font-family:var(--mono,monospace);font-size:0.7rem;letter-spacing:0.18em;text-transform:uppercase;color:var(--accent-e,#f5b942);font-weight:500;}",
      ".ftw-ask-close{font-size:1.4rem;line-height:1;background:transparent;border:none;color:var(--fg-muted,#858585);cursor:pointer;padding:4px 8px;}",
      ".ftw-ask-close:hover{color:var(--fg,#e8e8e8);}",
      ".ftw-ask-body{padding:16px 18px;overflow:auto;flex:1;}",
      ".ftw-ask-note{color:var(--fg-dim,#a0a0a0);font-size:0.84rem;line-height:1.45;margin:0 0 12px;}",
      ".ftw-ask-body textarea,.ftw-ask-body .ftw-ask-answer{box-sizing:border-box;width:100%;min-height:90px;resize:vertical;background:var(--ink-sunken,#0d0d0d);color:var(--fg,#e8e8e8);border:1px solid var(--line,#2a2a2a);border-radius:8px;padding:10px 12px;font:0.84rem/1.45 var(--sans,sans-serif);white-space:pre-wrap;overflow-wrap:anywhere;}",
      ".ftw-ask-answer{min-height:140px;font-family:var(--sans,sans-serif);}",
      ".ftw-ask-actions{display:flex;justify-content:flex-end;align-items:center;gap:8px;margin-top:12px;flex-wrap:wrap;}",
      ".ftw-ask-status{margin-right:auto;color:var(--fg-dim,#a0a0a0);font-size:0.78rem;}",
      ".ftw-ask-btn{font-family:var(--sans,sans-serif);font-size:0.78rem;font-weight:500;letter-spacing:0.02em;padding:8px 14px;border:1px solid var(--line,#2a2a2a);border-radius:8px;background:transparent;color:var(--fg,#e8e8e8);cursor:pointer;}",
      ".ftw-ask-btn:hover{border-color:var(--fg-dim,#a0a0a0);}",
      ".ftw-ask-btn[disabled]{opacity:0.55;cursor:default;}",
      ".ftw-ask-btn-primary{background:var(--accent-e,#f5b942);color:#0a0a0a;border-color:var(--accent-e,#f5b942);}",
      ".ftw-ask-error{color:var(--red-e,#ef4444);font-size:0.84rem;margin:8px 0 0;}",
      ".ftw-ask-setup a{color:var(--accent-e,#f5b942);}",
    ].join("");
    document.head.appendChild(style);
  }

  var state = {
    backdrop: null,
    last: null,
    keyHandler: null,
    trigger: null,
    offlineNames: [],
  };

  function offlineDrivers(data) {
    var names = [];
    var drivers = (data && data.drivers) || {};
    Object.keys(drivers).sort().forEach(function (name) {
      var d = drivers[name] || {};
      if (d.status === "offline") names.push(name);
    });
    return names;
  }

  function updateChip(data) {
    var chip = document.getElementById("ask-why-chip");
    if (!chip) return;
    var names = offlineDrivers(data);
    state.offlineNames = names;
    if (names.length === 0) {
      chip.hidden = true;
      chip.textContent = "";
      chip.removeAttribute("title");
      return;
    }
    chip.hidden = false;
    if (names.length === 1) {
      chip.textContent = names[0] + " is offline — Ask why";
    } else {
      chip.textContent = names.length + " drivers offline — Ask why";
    }
    chip.title = names.join(", ") + " offline";
  }

  function close() {
    if (state.keyHandler) {
      document.removeEventListener("keydown", state.keyHandler);
      state.keyHandler = null;
    }
    if (state.backdrop && state.backdrop.parentNode) {
      state.backdrop.parentNode.removeChild(state.backdrop);
    }
    state.backdrop = null;
  }

  function setStatus(el, text) {
    if (el) el.textContent = text || "";
  }

  function openIssueReview(result) {
    if (!result || !result.issue_title) return;
    var overlay = document.createElement("div");
    overlay.className = "ftw-ask-backdrop";
    overlay.innerHTML =
      '<div class="ftw-ask-shell" role="dialog" aria-modal="true" aria-label="Review GitHub issue">' +
      '  <div class="ftw-ask-head"><span class="ftw-ask-title">Review issue</span>' +
      '    <button class="ftw-ask-close" data-role="close-issue" aria-label="Close">×</button></div>' +
      '  <div class="ftw-ask-body">' +
      '    <p class="ftw-ask-note">Check the text before GitHub opens. Ask why leaves raw errors, logs, IP addresses, serial numbers and credentials out of the draft.</p>' +
      '    <label for="ftw-ask-issue-title">Title</label>' +
      '    <input id="ftw-ask-issue-title" value="' + escHtml(result.issue_title) + '" style="box-sizing:border-box;width:100%;margin:6px 0 10px;background:var(--ink-sunken,#0d0d0d);color:var(--fg,#e8e8e8);border:1px solid var(--line,#2a2a2a);border-radius:8px;padding:8px 10px;font:0.84rem var(--sans,sans-serif)">' +
      '    <textarea data-role="issue-body" aria-label="Issue body">' + escHtml(result.issue_body || "") + '</textarea>' +
      '    <div class="ftw-ask-actions"><span class="ftw-ask-status" data-role="copy-status" aria-live="polite"></span>' +
      '      <button class="ftw-ask-btn" data-role="copy-issue">Copy issue</button>' +
      '      <button class="ftw-ask-btn ftw-ask-btn-primary" data-role="open-issue">Open GitHub issue</button>' +
      '    </div>' +
      "  </div>" +
      "</div>";
    document.body.appendChild(overlay);
    overlay.addEventListener("click", function (e) {
      if (e.target === overlay) overlay.parentNode.removeChild(overlay);
    });
    overlay.querySelector('[data-role="close-issue"]').addEventListener("click", function () {
      overlay.parentNode.removeChild(overlay);
    });
    var bodyEl = overlay.querySelector('[data-role="issue-body"]');
    var titleEl = overlay.querySelector("#ftw-ask-issue-title");
    var copyStatus = overlay.querySelector('[data-role="copy-status"]');
    overlay.querySelector('[data-role="copy-issue"]').addEventListener("click", function () {
      var text = (titleEl.value || "") + "\n\n" + (bodyEl.value || "");
      function done() { setStatus(copyStatus, "Copied."); }
      if (navigator.clipboard && navigator.clipboard.writeText) {
        navigator.clipboard.writeText(text).then(done).catch(function () {
          bodyEl.select();
          document.execCommand("copy");
          done();
        });
        return;
      }
      bodyEl.select();
      document.execCommand("copy");
      done();
    });
    overlay.querySelector('[data-role="open-issue"]').addEventListener("click", function () {
      var url = result.issue_url || ISSUE_TEMPLATE_URL;
      var title = (titleEl.value || "").trim();
      if (title) url = ISSUE_TEMPLATE_URL + "&title=" + encodeURIComponent(title);
      window.open(url, "_blank", "noopener");
    });
  }

  function renderSetup(host, status) {
    host.innerHTML =
      '<div class="ftw-ask-setup">' +
      '  <p class="ftw-ask-note">' + escHtml(status.unavailable || "Ask why is off.") + "</p>" +
      '  <p class="ftw-ask-note">Create a free OpenRouter key (no card), paste it in Settings → System, turn Ask why on, and leave the model on <code>openrouter/free</code> unless you want to pay for another.</p>' +
      '  <div class="ftw-ask-actions">' +
      '    <a class="ftw-ask-btn" href="' + escHtml(status.setup_url || "https://openrouter.ai/keys") + '" target="_blank" rel="noopener">Get a free key</a>' +
      "  </div>" +
      "</div>";
  }

  function renderReady(host, status, opts) {
    opts = opts || {};
    var initial = opts.question || "What is going on right now?";
    host.innerHTML =
      '<p class="ftw-ask-note">Ask why reads this box with read-only tools and sends what it finds to <code>' +
      escHtml(status.model || "openrouter/free") +
      "</code>. It cannot change modes or send driver commands.</p>" +
      '<label for="ftw-ask-question">What looks wrong?</label>' +
      '<textarea id="ftw-ask-question" aria-label="Question">' + escHtml(initial) + "</textarea>" +
      '<div class="ftw-ask-error" data-role="error" hidden></div>' +
      '<div class="ftw-ask-answer" data-role="answer" hidden></div>' +
      '<div class="ftw-ask-actions"><span class="ftw-ask-status" data-role="status" aria-live="polite"></span>' +
      '  <button class="ftw-ask-btn" data-role="draft" hidden>Draft GitHub issue</button>' +
      '  <button class="ftw-ask-btn ftw-ask-btn-primary" data-role="ask">Ask why</button>' +
      "</div>";

    var askBtn = host.querySelector('[data-role="ask"]');
    var draftBtn = host.querySelector('[data-role="draft"]');
    var questionEl = host.querySelector("#ftw-ask-question");
    var answerEl = host.querySelector('[data-role="answer"]');
    var errorEl = host.querySelector('[data-role="error"]');
    var statusEl = host.querySelector('[data-role="status"]');

    function fail(msg) {
      errorEl.hidden = false;
      errorEl.textContent = msg;
      askBtn.disabled = false;
      setStatus(statusEl, "");
    }

    askBtn.addEventListener("click", function () {
      errorEl.hidden = true;
      errorEl.textContent = "";
      askBtn.disabled = true;
      draftBtn.hidden = true;
      setStatus(statusEl, "Reading the site…");
      var payload = { question: questionEl.value || "" };
      if (state.trigger) payload.trigger = state.trigger;
      apiFetch("/api/assistant/ask", {
        method: "POST",
        headers: { "Content-Type": "application/json" },
        body: JSON.stringify(payload),
      })
        .then(function (r) {
          return r.json().then(function (j) {
            if (!r.ok) throw new Error(j.error || ("HTTP " + r.status));
            return j;
          });
        })
        .then(function (j) {
          state.last = j;
          answerEl.hidden = false;
          answerEl.textContent = j.answer || "";
          askBtn.disabled = false;
          var used = j.resolved_model || j.model || "";
          setStatus(statusEl, used ? "Answered by " + used : "Done");
          if (j.issue_title) {
            draftBtn.hidden = false;
          }
        })
        .catch(function (err) {
          fail(err.message || String(err));
        });
    });

    draftBtn.addEventListener("click", function () {
      openIssueReview(state.last || {});
    });
    if (opts.autoAsk) askBtn.click();
  }

  function open(opts) {
    opts = opts || {};
    state.trigger = opts.trigger || null;
    ensureStyles();
    close();
    var backdrop = document.createElement("div");
    backdrop.className = "ftw-ask-backdrop";
    backdrop.innerHTML =
      '<div class="ftw-ask-shell" role="dialog" aria-modal="true" aria-labelledby="ftw-ask-title">' +
      '  <div class="ftw-ask-head"><span class="ftw-ask-title" id="ftw-ask-title">Ask why</span>' +
      '    <button class="ftw-ask-close" data-role="close" aria-label="Close">×</button></div>' +
      '  <div class="ftw-ask-body" data-role="body"><p class="ftw-ask-note">Loading…</p></div>' +
      "</div>";
    document.body.appendChild(backdrop);
    state.backdrop = backdrop;
    backdrop.addEventListener("click", function (e) {
      if (e.target === backdrop) close();
    });
    backdrop.querySelector('[data-role="close"]').addEventListener("click", close);
    state.keyHandler = function (e) {
      if (e.key === "Escape") close();
    };
    document.addEventListener("keydown", state.keyHandler);

    var body = backdrop.querySelector('[data-role="body"]');
    apiFetch("/api/assistant/status")
      .then(function (r) {
        if (!r.ok) throw new Error("HTTP " + r.status);
        return r.json();
      })
      .then(function (status) {
        if (status.ready) renderReady(body, status, opts);
        else renderSetup(body, status);
      })
      .catch(function (err) {
        body.innerHTML = '<p class="ftw-ask-error">Could not load Ask why: ' + escHtml(err.message) + "</p>";
      });
  }

  function bind() {
    var btn = document.getElementById("plan-ask-why");
    if (btn) btn.addEventListener("click", function () { open(); });
    var chip = document.getElementById("ask-why-chip");
    if (chip) {
      chip.addEventListener("click", function () {
        var names = state.offlineNames || [];
        var question = names.length === 1
          ? names[0] + " is offline. Why?"
          : names.length + " drivers are offline. Why?";
        open({
          question: question,
          trigger: { kind: "driver_offline", driver: names[0] || "" },
          autoAsk: true,
        });
      });
    }
    window.addEventListener("ftw-status", function (e) {
      updateChip(e.detail);
    });
  }

  if (document.readyState === "loading") {
    document.addEventListener("DOMContentLoaded", bind);
  } else {
    bind();
  }

  window.FTWAskWhy = { open: open, close: close, updateChip: updateChip };
})();
