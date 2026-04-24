// Settings → Planner tab: MPC planner scalars + mode dropdown.
//
// Mode is a LIVE control — the dropdown writes directly to the same
// state.db key the dashboard mode-buttons use (POST /api/mode),
// NOT the YAML `planner.mode` field. That way the dashboard and
// Settings share one source of truth and reflect each other without
// a file round-trip. The shell's fetchStatus (every ~5 s) updates
// the dashboard buttons when Settings sets the mode; the tab's
// after() hook re-fetches /api/mode each open so Settings catches
// changes the dashboard made since last time it was viewed.
(function () {
  var S = (window.FTWSettings = window.FTWSettings || { tabs: {} });
  S.tabs = S.tabs || {};

  // Two-way mapping between the planner-facing strategy names and
  // the dashboard-facing control modes. Kept as a pair of tables to
  // avoid a "reverse-lookup-by-value" helper and because new modes
  // may land on either side asymmetrically in the future.
  var STRATEGY_FROM_DASH = {
    planner_self:      "self_consumption",
    planner_cheap:     "cheap_charge",
    planner_arbitrage: "arbitrage",
  };
  var DASH_FROM_STRATEGY = {
    self_consumption: "planner_self",
    cheap_charge:     "planner_cheap",
    arbitrage:        "planner_arbitrage",
  };

  S.tabs.planner = {
    render: function (ctx) {
      var field = ctx.field, help = ctx.help, config = ctx.config;
      if (!config.planner) config.planner = {};
      var opts = [
        { v: "self_consumption", label: "self_consumption — minimise grid import" },
        { v: "cheap_charge",     label: "cheap_charge — charge during cheapest hours" },
        { v: "arbitrage",        label: "arbitrage — buy low / sell high" },
      ];
      // Select is seeded with whatever config.planner.mode holds as a
      // transient placeholder; after() immediately re-fetches
      // /api/mode so the live value wins.
      var currentStrategy = config.planner.mode || "self_consumption";
      var optsHtml = opts.map(function (o) {
        return '<option value="' + o.v + '"' + (o.v === currentStrategy ? ' selected' : '') + '>' + o.label + '</option>';
      }).join("");
      return '<fieldset><legend>MPC Planner</legend>' +
        '<label><input type="checkbox" data-checkbox-path="planner.enabled"' + (config.planner.enabled ? ' checked' : '') + '> Enabled ' +
        help('Enable the MPC planner. When active it overrides manual mode with an optimised schedule.') + '</label>' +
        '<label style="margin-top:8px">Mode ' +
        help('Changing this instantly flips the dashboard\'s planner_* mode via /api/mode. The value lives in state.db, so the dashboard and this dropdown always agree.') +
        '</label>' +
        '<select id="planner-mode-select">' + optsHtml + '</select>' +
        '<span id="planner-mode-status" style="display:block;font-size:0.75rem;color:var(--text-dim);min-height:1em;margin-top:4px"></span>' +
        '<div class="field-row" style="margin-top:12px"><div>' +
        field("SoC min (%)", "planner.soc_min_pct", "number", 10,
          "Lowest SoC the planner will discharge to (percent). 10 = 10%.") +
        '</div><div>' +
        field("SoC max (%)", "planner.soc_max_pct", "number", 90,
          "Highest SoC the planner will charge to (percent). 90 = 90%.") +
        '</div></div>' +
        '<div class="field-row"><div>' +
        field("Base load (W)", "planner.base_load_w", "number", 0,
          "Constant household load estimate used when the load twin has no data yet.") +
        '</div><div>' +
        field("Horizon (hours)", "planner.horizon_hours", "number", 48,
          "Planning horizon in hours. 48 h covers two day-ahead price windows.") +
        '</div></div>' +
        '<div class="field-row"><div>' +
        field("Replan interval (min)", "planner.interval_min", "number", 15,
          "How often the planner re-solves. Lower = more responsive but more CPU.") +
        '</div><div>' +
        field("Export value (ore/kWh)", "planner.export_ore_per_kwh", "number", 0,
          "Override export value. 0 = use mean spot price.") +
        '</div></div>' +
        '<div class="field-row"><div>' +
        field("Charge efficiency", "planner.charge_efficiency", "number", 0.95,
          "Round-trip charge efficiency (0-1). 0.95 = 5% loss charging.") +
        '</div><div>' +
        field("Discharge efficiency", "planner.discharge_efficiency", "number", 0.95,
          "Round-trip discharge efficiency (0-1). 0.95 = 5% loss discharging.") +
        '</div></div>' +
        '</fieldset>' +
        '<p style="color:var(--text-dim);font-size:0.8rem;margin-top:8px">' +
        'The planner requires working price + weather forecasts. When disabled the system runs in the manual mode set on the Control page.' +
        '</p>';
    },
    after: function (ctx) {
      var sel = document.getElementById("planner-mode-select");
      var status = document.getElementById("planner-mode-status");
      if (!sel) return;

      // Fetch the live mode and reflect it in the select. This is
      // the "refresh on open" step: if the dashboard flipped the
      // mode while Settings was closed, the dropdown now catches up.
      fetch("/api/mode")
        .then(function (r) { return r.json(); })
        .then(function (d) {
          var dashMode = d && d.mode;
          var strat = STRATEGY_FROM_DASH[dashMode];
          if (strat) {
            sel.value = strat;
            // Mirror to the in-memory config so if the user hits
            // the footer Save for other reasons, planner.mode in
            // YAML tracks the live state too. Harmless no-op when
            // strat === the current form value.
            if (ctx.config && ctx.config.planner) ctx.config.planner.mode = strat;
          } else if (dashMode) {
            // Non-planner mode is active (e.g. "self_consumption",
            // "idle", "peak_shaving"). Surface a hint and keep the
            // dropdown selection as-is; changing the dropdown will
            // flip the dashboard to the matching planner_* mode.
            if (status) status.textContent =
              "Dashboard currently in \"" + dashMode + "\" (not a planner mode). Picking a strategy here will switch to planner_* mode.";
          }
        })
        .catch(function () { /* offline — keep placeholder */ });

      sel.addEventListener("change", function () {
        var strat = sel.value;
        var dashMode = DASH_FROM_STRATEGY[strat];
        if (!dashMode) return;
        if (status) { status.textContent = "Saving…"; status.style.color = "var(--text-dim)"; }
        // POST /api/mode — same path the dashboard buttons use. The
        // backend handler (api.go:handleSetMode) persists to state.db
        // and, when the target is a planner_* mode, also forwards to
        // mpcSvc.SetMode so the next tick runs with the new strategy.
        fetch("/api/mode", {
          method: "POST",
          headers: { "Content-Type": "application/json" },
          body: JSON.stringify({ mode: dashMode }),
        })
          .then(function (r) { return r.json().then(function (j) { return { ok: r.ok, j: j }; }); })
          .then(function (res) {
            if (!res.ok) {
              if (status) { status.textContent = "✗ " + (res.j.error || "unknown"); status.style.color = "#e57373"; }
              return;
            }
            if (ctx.config && ctx.config.planner) ctx.config.planner.mode = strat;
            if (status) { status.textContent = "✓ Applied (dashboard will update within ~5 s)"; status.style.color = "#81c995"; }
          })
          .catch(function (e) {
            if (status) { status.textContent = "✗ " + e.message; status.style.color = "#e57373"; }
          });
      });
    },
  };
})();
