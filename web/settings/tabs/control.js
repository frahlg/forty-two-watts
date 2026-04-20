// Settings → Control tab: site + fuse scalars that feed the PI loop.
(function () {
  var S = (window.FTWSettings = window.FTWSettings || { tabs: {} });
  S.tabs = S.tabs || {};

  S.tabs.control = {
    render: function (ctx) {
      var field = ctx.field;
      return '<fieldset><legend>Site</legend>' +
        field("Name", "site.name", "text", "Home") +
        '<div class="field-row"><div>' +
        field("Grid target (W)", "site.grid_target_w", "number", 0) +
        '</div><div>' +
        field("Grid tolerance (W)", "site.grid_tolerance_w", "number", 42) +
        '</div></div>' +
        '<div class="field-row"><div>' +
        field("Slew rate (W/cycle)", "site.slew_rate_w", "number", 500) +
        '</div><div>' +
        field("Min dispatch interval (s)", "site.min_dispatch_interval_s", "number", 5) +
        '</div></div>' +
        '<div class="field-row"><div>' +
        field("Smoothing alpha", "site.smoothing_alpha", "number", 0.3,
          "EMA smoothing factor for the grid reading (0-1). Lower = smoother but slower response.") +
        '</div><div>' +
        field("PI gain", "site.gain", "number", 0.5,
          "Proportional gain of the PI controller. Higher = more aggressive correction.") +
        '</div></div>' +
        '<div class="field-row"><div>' +
        field("Control interval (s)", "site.control_interval_s", "number", 5) +
        '</div><div>' +
        field("Watchdog timeout (s)", "site.watchdog_timeout_s", "number", 60) +
        '</div></div>' +
        '</fieldset>' +
        '<fieldset><legend>Fuse</legend>' +
        '<div class="field-row"><div>' +
        field("Max amps (A)", "fuse.max_amps", "number", 16) +
        '</div><div>' +
        field("Phases", "fuse.phases", "number", 3) +
        '</div></div>' +
        field("Voltage (V)", "fuse.voltage", "number", 230) +
        '</fieldset>';
    },
  };
})();
