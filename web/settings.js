// Settings modal shell — owns the lifecycle (open/close, fetch, save,
// tab switching) and exposes a registry so each tab can live in its own
// file under /web/settings/tabs/*.js. Tab files register themselves
// into window.FTWSettings.tabs at load time; the shell looks them up
// whenever renderTab is called.
//
// Contract for a tab file:
//
//   (function () {
//     var S = (window.FTWSettings = window.FTWSettings || { tabs: {} });
//     S.tabs = S.tabs || {};
//     S.tabs.<name> = {
//       render: function (ctx) { return htmlString; },
//       after:  function (ctx) { /* optional post-render hook */ },
//     };
//   })();
//
// ctx is built fresh on each render and exposes the shell's helpers
// (field, selectField, help, escHtml, getByPath, setByPath,
// captureCurrentTab, renderTab, bodyEl, config).
(function () {
  "use strict";

  var modal = document.getElementById("settings-modal");
  var openBtn = document.getElementById("settings-btn");
  var closeBtn = document.getElementById("settings-close");
  var saveBtn = document.getElementById("settings-save");
  var statusEl = document.getElementById("settings-status");
  var tabsEl = document.getElementById("settings-tabs");
  var bodyEl = document.getElementById("settings-body");

  if (!modal || !openBtn) return;

  // Expose the registry namespace immediately so tab files that load
  // before or after this shell can register idempotently.
  var S = (window.FTWSettings = window.FTWSettings || { tabs: {} });
  S.tabs = S.tabs || {};

  var currentConfig = null;
  var currentTab = "control";

  openBtn.addEventListener("click", function () {
    fetch("/api/config")
      .then(function (r) { return r.json(); })
      .then(function (cfg) {
        currentConfig = cfg;
        modal.classList.remove("hidden");
        renderTab(currentTab);
        setStatus("");
      })
      .catch(function (e) {
        setStatus("Failed to load config: " + e, "error");
      });
  });

  closeBtn.addEventListener("click", function () {
    modal.classList.add("hidden");
  });
  modal.addEventListener("click", function (e) {
    if (e.target === modal) modal.classList.add("hidden");
  });

  tabsEl.addEventListener("click", function (e) {
    if (e.target.tagName === "BUTTON" && e.target.dataset.tab) {
      tabsEl.querySelectorAll("button").forEach(function (b) {
        b.classList.toggle("active", b === e.target);
      });
      captureCurrentTab();
      currentTab = e.target.dataset.tab;
      renderTab(currentTab);
    }
  });

  saveBtn.addEventListener("click", function () {
    captureCurrentTab();
    setStatus("Saving...");
    fetch("/api/config", {
      method: "POST",
      headers: { "Content-Type": "application/json" },
      body: JSON.stringify(currentConfig),
    })
      .then(function (r) {
        if (!r.ok) return r.json().then(function (j) { throw new Error(j.error || ("HTTP " + r.status)); });
        return r.json();
      })
      .then(function () {
        setStatus("Saved ✓", "success");
        setTimeout(function () { setStatus(""); }, 2000);
      })
      .catch(function (e) {
        setStatus("Save failed: " + e.message, "error");
      });
  });

  function setStatus(msg, kind) {
    statusEl.textContent = msg || "";
    statusEl.className = "settings-status" + (kind ? " " + kind : "");
  }

  function captureCurrentTab() {
    var inputs = bodyEl.querySelectorAll("[data-path]");
    inputs.forEach(function (input) {
      var path = input.dataset.path;
      var val = input.type === "number" ? parseFloat(input.value) : input.value;
      if (input.type === "number" && isNaN(val)) val = 0;
      if (input.type === "number" && input.dataset.unitScale) {
        val = val * parseFloat(input.dataset.unitScale);
      }
      // Preserve a stored password when the user hasn't typed over it.
      if (input.type === "password" && val === "" && getByPath(currentConfig, path, "")) return;
      setByPath(currentConfig, path, val);
    });
  }

  function setByPath(obj, path, val) {
    var parts = path.split(".");
    var node = obj;
    for (var i = 0; i < parts.length - 1; i++) {
      if (!node[parts[i]]) node[parts[i]] = {};
      node = node[parts[i]];
    }
    node[parts[parts.length - 1]] = val;
  }

  function getByPath(obj, path, dflt) {
    var parts = path.split(".");
    var node = obj;
    for (var i = 0; i < parts.length; i++) {
      if (node == null) return dflt;
      node = node[parts[i]];
    }
    return node == null ? dflt : node;
  }

  function help(text) {
    return '<span class="help" data-help="' + escHtml(text) + '" title="' + escHtml(text) + '">?</span>';
  }

  function field(label, path, type, dflt, helpText) {
    var val = getByPath(currentConfig, path, dflt);
    return '<label>' + label + (helpText ? ' ' + help(helpText) : '') + '</label>' +
      '<input type="' + type + '" data-path="' + path + '" value="' + escHtml(val == null ? "" : String(val)) + '">';
  }

  function selectField(label, path, options, dflt, helpText) {
    var val = getByPath(currentConfig, path, dflt);
    var opts = options.map(function (o) {
      return '<option value="' + o + '"' + (o === val ? ' selected' : '') + '>' + o + '</option>';
    }).join("");
    return '<label>' + label + (helpText ? ' ' + help(helpText) : '') + '</label>' +
      '<select data-path="' + path + '">' + opts + '</select>';
  }

  function escHtml(s) {
    var div = document.createElement("div");
    div.textContent = s == null ? "" : String(s);
    return div.innerHTML;
  }

  function renderTab(tab) {
    var def = S.tabs[tab];
    if (!def) {
      bodyEl.innerHTML = '<p style="color:var(--text-dim)">Unknown tab: ' + escHtml(tab) + '</p>';
      return;
    }
    var ctx = {
      config: currentConfig,
      bodyEl: bodyEl,
      field: field,
      selectField: selectField,
      help: help,
      escHtml: escHtml,
      getByPath: getByPath,
      setByPath: setByPath,
      captureCurrentTab: captureCurrentTab,
      renderTab: renderTab,
    };
    var html = "";
    try {
      html = (def.render ? def.render(ctx) : "") || "";
    } catch (e) {
      html = '<p style="color:#e57373">Render error: ' + escHtml(e.message) + '</p>';
      console.error("tab render:", tab, e);
    }
    bodyEl.innerHTML = html;

    // Generic handler for data-checkbox-path — shared across every tab.
    bodyEl.querySelectorAll("[data-checkbox-path]").forEach(function (cb) {
      cb.addEventListener("change", function () {
        setByPath(currentConfig, cb.dataset.checkboxPath, cb.checked);
      });
    });

    if (def.after) {
      try { def.after(ctx); } catch (e) { console.error("tab after:", tab, e); }
    }
  }
})();
