// Ask why — a short conversation about this box.
//
// The question is a chat line. The answer is a separate message. Progress
// streams while the model works. Opening a GitHub issue uses one filled
// markdown body, not the YAML bug form.
(function () {
  "use strict";

  var ISSUE_NEW = "https://github.com/srcfl/ftw/issues/new?template=ask_why.md";

  function apiFetch(path, opts) {
    return fetch(path, opts);
  }

  function escHtml(s) {
    var div = document.createElement("div");
    div.textContent = s == null ? "" : String(s);
    return div.innerHTML;
  }

  function formatAnswer(text) {
    var s = escHtml(text);
    s = s.replace(/\*\*([^*]+)\*\*/g, "<strong>$1</strong>");
    s = s.replace(/`([^`]+)`/g, "<code>$1</code>");
    s = s.replace(/^(\d+)\.\s/gm, "<br>$1. ");
    s = s.replace(/\n/g, "<br>");
    return s;
  }

  function toolLabel(name) {
    if (name === "get_driver_health") return "Checking drivers";
    if (name === "get_recent_logs") return "Reading logs";
    if (name === "get_plan_now") return "Reading the plan";
    if (name === "get_support_report") return "Reading the help report";
    if (name === "get_version") return "Reading the version";
    return "Looking further";
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
      ".ftw-ask-thread{padding:16px 18px;overflow:auto;flex:1;display:flex;flex-direction:column;gap:10px;}",
      ".ftw-ask-msg{max-width:92%;padding:10px 12px;border-radius:10px;font:0.9rem/1.5 var(--sans,sans-serif);overflow-wrap:anywhere;}",
      ".ftw-ask-msg.user{align-self:flex-end;background:var(--ink,#111);border:1px solid var(--line,#2a2a2a);color:var(--fg,#e8e8e8);}",
      ".ftw-ask-msg.assistant{align-self:flex-start;background:var(--ink-sunken,#0d0d0d);border:1px solid var(--line,#2a2a2a);color:var(--fg,#e8e8e8);}",
      ".ftw-ask-msg.assistant strong{color:var(--fg,#e8e8e8);}",
      ".ftw-ask-msg.assistant code{font-family:var(--mono,monospace);font-size:0.82em;}",
      ".ftw-ask-progress{align-self:flex-start;color:var(--fg-dim,#a0a0a0);font:0.8rem/1.4 var(--sans,sans-serif);}",
      ".ftw-ask-note{color:var(--fg-dim,#a0a0a0);font-size:0.84rem;line-height:1.45;margin:0;}",
      ".ftw-ask-error{color:var(--red-e,#ef4444);font-size:0.84rem;}",
      ".ftw-ask-foot{display:flex;justify-content:space-between;align-items:center;gap:8px;padding:0 18px 10px;flex-wrap:wrap;}",
      ".ftw-ask-status{color:var(--fg-dim,#a0a0a0);font-size:0.78rem;}",
      ".ftw-ask-composer{display:flex;gap:8px;padding:12px 18px 16px;border-top:1px solid var(--line,#2a2a2a);}",
      ".ftw-ask-composer input{flex:1;min-width:0;background:var(--ink-sunken,#0d0d0d);color:var(--fg,#e8e8e8);border:1px solid var(--line,#2a2a2a);border-radius:8px;padding:10px 12px;font:0.9rem var(--sans,sans-serif);}",
      ".ftw-ask-btn{font-family:var(--sans,sans-serif);font-size:0.78rem;font-weight:500;letter-spacing:0.02em;padding:8px 14px;border:1px solid var(--line,#2a2a2a);border-radius:8px;background:transparent;color:var(--fg,#e8e8e8);cursor:pointer;}",
      ".ftw-ask-btn:hover{border-color:var(--fg-dim,#a0a0a0);}",
      ".ftw-ask-btn[disabled]{opacity:0.55;cursor:default;}",
      ".ftw-ask-btn-primary{background:var(--accent-e,#f5b942);color:#0a0a0a;border-color:var(--accent-e,#f5b942);}",
      ".ftw-ask-setup{padding:16px 18px;}",
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
    busy: false,
    turns: [],
    generation: 0,
    abort: null,
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
    state.generation += 1;
    if (state.abort) {
      try { state.abort.abort(); } catch (e) { /* already closed */ }
      state.abort = null;
    }
    if (state.keyHandler) {
      document.removeEventListener("keydown", state.keyHandler);
      state.keyHandler = null;
    }
    if (state.backdrop && state.backdrop.parentNode) {
      state.backdrop.parentNode.removeChild(state.backdrop);
    }
    state.backdrop = null;
    state.busy = false;
    state.turns = [];
  }

  function stillOpen(gen) {
    return gen === state.generation && !!state.backdrop;
  }

  function threadEl() {
    return state.backdrop && state.backdrop.querySelector('[data-role="thread"]');
  }

  function addUser(text) {
    var thread = threadEl();
    if (!thread) return;
    var el = document.createElement("div");
    el.className = "ftw-ask-msg user";
    el.textContent = text;
    thread.appendChild(el);
    thread.scrollTop = thread.scrollHeight;
  }

  function addAssistant(text) {
    var thread = threadEl();
    if (!thread) return;
    var el = document.createElement("div");
    el.className = "ftw-ask-msg assistant";
    el.innerHTML = formatAnswer(text);
    thread.appendChild(el);
    thread.scrollTop = thread.scrollHeight;
  }

  function setProgress(text) {
    var thread = threadEl();
    if (!thread) return;
    var el = thread.querySelector('[data-role="progress"]');
    if (!el) {
      el = document.createElement("div");
      el.className = "ftw-ask-progress";
      el.setAttribute("data-role", "progress");
      thread.appendChild(el);
    }
    el.hidden = !text;
    el.textContent = text || "";
    thread.scrollTop = thread.scrollHeight;
  }

  function clearProgress() {
    setProgress("");
    var el = threadEl() && threadEl().querySelector('[data-role="progress"]');
    if (el && el.parentNode) el.parentNode.removeChild(el);
  }

  function setFoot(html) {
    var foot = state.backdrop && state.backdrop.querySelector('[data-role="foot"]');
    if (foot) foot.innerHTML = html || "";
  }

  function openFilledIssue(result) {
    if (!result || !result.issue_title) return;
    var url = result.issue_url || (ISSUE_NEW + "&title=" + encodeURIComponent(result.issue_title) + "&body=" + encodeURIComponent(result.issue_body || result.answer || ""));
    var body = result.issue_body || result.answer || "";
    if (navigator.clipboard && navigator.clipboard.writeText && body) {
      navigator.clipboard.writeText(body).catch(function () {});
    }
    window.open(url, "_blank", "noopener");
  }

  function parseSSEChunk(buf, onEvent) {
    var parts = buf.split("\n\n");
    var rest = parts.pop() || "";
    parts.forEach(function (block) {
      var line = block.split("\n").filter(function (l) { return l.indexOf("data:") === 0; }).map(function (l) {
        return l.replace(/^data:\s?/, "");
      }).join("");
      if (!line) return;
      var ev;
      try { ev = JSON.parse(line); } catch (e) { return; }
      onEvent(ev);
    });
    return rest;
  }

  function ask(question) {
    var q = String(question || "").trim();
    if (!q || state.busy) return;
    var gen = state.generation;
    state.busy = true;
    addUser(q);
    setProgress("This can take a minute. Reading the site…");
    setFoot("");
    var askBtn = state.backdrop && state.backdrop.querySelector('[data-role="ask"]');
    var input = state.backdrop && state.backdrop.querySelector('[data-role="input"]');
    if (askBtn) askBtn.disabled = true;
    if (input) input.value = "";
    var payload = { question: q, history: state.turns.slice() };
    if (state.trigger) payload.trigger = state.trigger;
    var fetchOpts = {
      method: "POST",
      headers: { "Content-Type": "application/json", Accept: "text/event-stream" },
      body: JSON.stringify(payload),
    };
    if (typeof AbortController === "function") {
      state.abort = new AbortController();
      fetchOpts.signal = state.abort.signal;
    }
    apiFetch("/api/assistant/ask", fetchOpts)
      .then(function (r) {
        if (!stillOpen(gen)) return null;
        var ctype = (r.headers.get("Content-Type") || "");
        if (ctype.indexOf("text/event-stream") >= 0 && r.body && r.body.getReader) {
          var reader = r.body.getReader();
          var decoder = new TextDecoder();
          var buf = "";
          var donePayload = null;
          function pump() {
            return reader.read().then(function (chunk) {
              if (!stillOpen(gen)) return null;
              if (chunk.value) buf += decoder.decode(chunk.value, { stream: true });
              buf = parseSSEChunk(buf, function (ev) {
                if (!stillOpen(gen)) return;
                if (ev.type === "status") setProgress(ev.text || "Working…");
                if (ev.type === "tool") setProgress(toolLabel(ev.text) + "…");
                if (ev.type === "error") throw new Error(ev.error || "Ask why failed");
                if (ev.type === "done") donePayload = ev;
              });
              if (chunk.done) return donePayload;
              return pump();
            });
          }
          return pump().then(function (ev) {
            if (!stillOpen(gen)) return null;
            if (!ev) throw new Error("No answer");
            return ev;
          });
        }
        return r.json().then(function (j) {
          if (!r.ok) throw new Error(j.error || ("HTTP " + r.status));
          return j;
        });
      })
      .then(function (j) {
        if (!stillOpen(gen) || !j) return;
        clearProgress();
        state.last = j;
        state.turns.push({ role: "user", text: q });
        state.turns.push({ role: "assistant", text: j.answer || "" });
        if (state.turns.length > 6) state.turns = state.turns.slice(-6);
        addAssistant(j.answer || "");
        var used = j.resolved_model || j.model || "";
        var foot = '<span class="ftw-ask-status">' + escHtml(used ? "Answered by " + used : "Done") + "</span>";
        if (j.issue_title) {
          foot += '<button class="ftw-ask-btn ftw-ask-btn-primary" data-role="open-issue" type="button">Open GitHub issue</button>';
        }
        setFoot(foot);
        var issueBtn = state.backdrop && state.backdrop.querySelector('[data-role="open-issue"]');
        if (issueBtn) issueBtn.addEventListener("click", function () { openFilledIssue(state.last); });
      })
      .catch(function (err) {
        if (!stillOpen(gen)) return;
        if (err && err.name === "AbortError") return;
        clearProgress();
        var thread = threadEl();
        if (thread) {
          var el = document.createElement("div");
          el.className = "ftw-ask-error";
          el.textContent = err.message || String(err);
          thread.appendChild(el);
        }
      })
      .then(function () {
        if (!stillOpen(gen)) return;
        state.busy = false;
        state.abort = null;
        if (askBtn) askBtn.disabled = false;
        if (input) input.focus();
      });
  }

  function renderSetup(host, status) {
    host.innerHTML =
      '<div class="ftw-ask-setup">' +
      '  <p class="ftw-ask-note">' + escHtml(status.unavailable || "Ask why needs an OpenRouter key.") + "</p>" +
      '  <p class="ftw-ask-note">Create a free key (no card) and paste it in Settings → System. Pasting the key turns Ask why on.</p>' +
      '  <p class="ftw-ask-note"><a href="' + escHtml(status.setup_url || "https://openrouter.ai/keys") + '" target="_blank" rel="noopener">Get a free key</a></p>' +
      "</div>";
  }

  function renderReady(host, status, opts) {
    opts = opts || {};
    var placeholder = (opts.trigger && opts.trigger.kind === "plan")
      ? "Ask about this plan…"
      : "Ask about this house…";
    host.innerHTML =
      '<div class="ftw-ask-thread" data-role="thread"></div>' +
      '<div class="ftw-ask-foot" data-role="foot"></div>' +
      '<form class="ftw-ask-composer" data-role="form">' +
      '  <input type="text" data-role="input" maxlength="2000" placeholder="' + escHtml(placeholder) + '" aria-label="Question" autocomplete="off">' +
      '  <button class="ftw-ask-btn ftw-ask-btn-primary" data-role="ask" type="submit">Ask why</button>' +
      "</form>";
    var form = host.querySelector('[data-role="form"]');
    var input = host.querySelector('[data-role="input"]');
    form.addEventListener("submit", function (e) {
      e.preventDefault();
      ask(input.value);
    });
    if (opts.question) {
      ask(opts.question);
    } else {
      input.focus();
    }
  }

  function open(opts) {
    opts = opts || {};
    ensureStyles();
    close();
    state.trigger = opts.trigger || null;
    state.last = null;
    state.turns = [];
    var gen = state.generation;
    var title = (opts.trigger && opts.trigger.kind === "plan") ? "Ask why this plan" : "Ask why";
    var backdrop = document.createElement("div");
    backdrop.className = "ftw-ask-backdrop";
    backdrop.innerHTML =
      '<div class="ftw-ask-shell" role="dialog" aria-modal="true" aria-labelledby="ftw-ask-title">' +
      '  <div class="ftw-ask-head"><span class="ftw-ask-title" id="ftw-ask-title">' + escHtml(title) + '</span>' +
      '    <button class="ftw-ask-close" data-role="close" aria-label="Close" type="button">×</button></div>' +
      '  <div data-role="body"><p class="ftw-ask-note" style="padding:16px 18px">Loading…</p></div>' +
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
        if (!stillOpen(gen)) return;
        if (status.ready) renderReady(body, status, opts);
        else renderSetup(body, status);
      })
      .catch(function (err) {
        if (!stillOpen(gen)) return;
        body.innerHTML = '<p class="ftw-ask-error" style="padding:16px 18px">Could not load Ask why: ' + escHtml(err.message) + "</p>";
      });
  }

  function askAboutPlan(question) {
    var q = String(question || "").trim();
    if (!q) q = "Why did FTW plan the next hours this way?";
    open({
      question: q,
      trigger: { kind: "plan" },
    });
  }

  function bind() {
    var btn = document.getElementById("plan-ask-why");
    if (btn) btn.addEventListener("click", function () { open(); });
    var form = document.getElementById("plan-ask-why-form");
    var input = document.getElementById("plan-ask-why-input");
    if (form) {
      form.addEventListener("submit", function (e) {
        e.preventDefault();
        var q = input ? input.value : "";
        if (input) input.value = "";
        askAboutPlan(q);
      });
    }
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

  window.FTWAskWhy = { open: open, close: close, updateChip: updateChip, _test: { parseSSEChunk: parseSSEChunk } };
})();
