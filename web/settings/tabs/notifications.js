// Settings → Notifications tab: operator-facing notifications settings.
// Transport runtime lives in internal/notifications; this file renders
// the editor and embeds the <ftw-notif-*> Web Components for live
// status + the test-button.
(function () {
  var S = (window.FTWSettings = window.FTWSettings || { tabs: {} });
  S.tabs = S.tabs || {};

  function defaultEvents() {
    return [
      { type: "driver_offline", enabled: false, threshold_s: 600, priority: 4, cooldown_s: 3600, tags: "", title_template: "", body_template: "" },
      { type: "driver_recovered", enabled: false, priority: 3, cooldown_s: 0, tags: "", title_template: "", body_template: "" },
    ];
  }

  S.tabs.notifications = {
    render: function (ctx) {
      var field = ctx.field, help = ctx.help, config = ctx.config;

      // Fetch backend-defined template defaults once so the operator
      // sees exactly what will be sent and can tweak in place. Failure
      // is non-fatal — fields just render empty (blank falls back to
      // default at render time on the server anyway).
      if (!window._notifDefaults) {
        fetch("/api/notifications/defaults")
          .then(function (r) { return r.ok ? r.json() : null; })
          .then(function (d) {
            if (!d) return;
            window._notifDefaults = d;
            var active = document.querySelector("#settings-tabs .active");
            if (active && active.dataset.tab === "notifications") {
              ctx.renderTab("notifications");
            }
          }).catch(function () {});
      }
      var defaults = window._notifDefaults || {};

      if (!config.notifications) {
        config.notifications = {
          enabled: false,
          provider: "ntfy",
          default_priority: 3,
          ntfy: { server: "https://ntfy.sh", topic: "" },
          events: defaultEvents(),
        };
      }
      var nc = config.notifications;
      if (!nc.provider) nc.provider = "ntfy";
      if (!nc.ntfy) nc.ntfy = { server: "https://ntfy.sh", topic: "" };
      if (!nc.events || !nc.events.length) nc.events = defaultEvents();

      // Pre-fill blank template fields with the backend defaults.
      for (var i = 0; i < nc.events.length; i++) {
        var ev = nc.events[i];
        var def = defaults[ev.type];
        if (def) {
          if (!ev.title_template) ev.title_template = def.title;
          if (!ev.body_template) ev.body_template = def.body;
        }
      }

      var html = '<ftw-notif-status interval-ms="5000" style="margin-bottom:10px"></ftw-notif-status>' +
        '<fieldset><legend>Transport</legend>' +
        '<label><input type="checkbox" data-checkbox-path="notifications.enabled"' + (nc.enabled ? ' checked' : '') + '> Enabled</label>' +
        '<div class="field-row"><div>' +
        '<label>Provider' +
        '<select data-path="notifications.provider">' +
        '<option value="ntfy"' + (nc.provider === "ntfy" ? ' selected' : '') + '>ntfy</option>' +
        '</select></label>' +
        '</div><div>' +
        field("Default priority (1–5)", "notifications.default_priority", "number", 3,
          "Used when a rule leaves priority=0. 5 = urgent, 1 = low.") +
        '</div></div>' +
        '</fieldset>' +
        '<fieldset><legend>ntfy</legend>' +
        '<div class="field-row"><div>' +
        field("Server", "notifications.ntfy.server", "text", "https://ntfy.sh",
          "Public ntfy.sh or your self-hosted server URL.") +
        '</div><div>' +
        field("Topic", "notifications.ntfy.topic", "text", "",
          "The ntfy topic (subscribe to it in the ntfy app).") +
        '</div></div>' +
        // Access-token uses the masked-placeholder pattern: typing replaces
        // the saved value, leaving blank preserves it via
        // PreserveMaskedSecrets on the backend.
        '<label>Access token ' + help("Bearer token issued by your ntfy server (or ntfy.sh reserved topic).") + '</label>' +
        '<input type="password" data-path="notifications.ntfy.access_token" value="" ' +
        'placeholder="' + (nc.ntfy && nc.ntfy.has_access_token
          ? "configured — hidden, type to replace"
          : "paste token") + '">' +
        '</fieldset>';

      for (var ei = 0; ei < nc.events.length; ei++) {
        var rule = nc.events[ei];
        html += '<fieldset><legend>' + (rule.type || "event #" + ei) + '</legend>' +
          '<label><input type="checkbox" data-checkbox-path="notifications.events.' + ei + '.enabled"' + (rule.enabled ? ' checked' : '') + '> Enabled</label>' +
          '<div class="field-row"><div>' +
          field("Threshold (s)", "notifications.events." + ei + ".threshold_s", "number", 600,
            "How long the condition must hold before firing. Default 600 s (10 min). Independent of the control-loop watchdog.") +
          '</div><div>' +
          field("Priority (0–5)", "notifications.events." + ei + ".priority", "number", 3,
            "0 uses the default priority above.") +
          '</div></div>' +
          '<div class="field-row"><div>' +
          field("Cooldown (s)", "notifications.events." + ei + ".cooldown_s", "number", 3600,
            "Minimum seconds between fires per driver.") +
          '</div><div>' +
          field("Tags (comma-separated)", "notifications.events." + ei + ".tags", "text", "") +
          '</div></div>' +
          field("Title template", "notifications.events." + ei + ".title_template", "text", "",
            "Go text/template. Leave blank for default.") +
          field("Body template", "notifications.events." + ei + ".body_template", "text", "",
            "Go text/template. Leave blank for default.") +
          '</fieldset>';
      }

      html += '<fieldset><legend>Test</legend>' +
        '<ftw-notif-test-button label="Send test notification"></ftw-notif-test-button>' +
        '</fieldset>' +
        '<p style="color:var(--text-dim);font-size:0.8rem;margin-top:8px">' +
        'Template fields: {{.Device}}, {{.Make}}, {{.Serial}}, {{.Duration}}, {{.DurationS}}, {{.EventType}}, {{.Timestamp}}.' +
        '</p>';
      return html;
    },
  };
})();
