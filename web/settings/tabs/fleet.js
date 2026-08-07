// Settings → Fleet ping: the message this box sends Sourceful once a day.
//
// This tab is not a privacy policy. A policy is a promise; this screen shows
// the actual payload, built by the same call the sender uses, so somebody
// deciding whether to leave it on can read what leaves rather than trust a
// paragraph about it. The whole design in go/internal/fleetping rests on the
// claim being checkable here.
//
// Three consequences for this file:
//
//   - the rows are built from the payload's own keys, never from a list kept
//     here. If the box ever started sending a field it should not, this screen
//     shows it instead of quietly dropping it;
//   - the exact JSON body is printed underneath the readable version, because
//     a friendly rendering is a rendering, and this is the one place where the
//     literal bytes are the point;
//   - the paragraph names every field and concedes what the message cannot
//     hide. It is the one part of this screen that can go stale without
//     anything breaking, so a test holds the count in it to the number of
//     fields the box sends.
(function () {
  var S = (window.FTWSettings = window.FTWSettings || { tabs: {} });
  S.tabs = S.tabs || {};

  // Labels for the fields the payload carries today. A field with no label
  // here is shown under its own name — see the note above.
  var LABELS = {
    ftw_version: "FTW version",
    channel: "Release channel",
    drivers: "Driver types",
    battery_kwh: "Battery size",
    price_zone: "Price zone",
    install_age: "Time since install",
  };

  var BATTERY = {
    none: "no battery",
    "0-5": "under 5 kWh",
    "5-15": "5 to 15 kWh",
    "15-30": "15 to 30 kWh",
    "30+": "over 30 kWh",
  };

  var AGE = {
    unknown: "not known",
    "0-1m": "under a month",
    "1-6m": "one to six months",
    "6-12m": "six to twelve months",
    "1y+": "over a year",
  };

  function describe(key, value) {
    if (Array.isArray(value)) return value.length ? value.join(", ") : "none";
    if (key === "battery_kwh" && BATTERY[value]) return BATTERY[value];
    if (key === "install_age" && AGE[value]) return AGE[value];
    if (value === "unknown") return "not known";
    return String(value);
  }

  // payloadRows turns the payload into what the table shows. Driven by the
  // payload's keys, so nothing the box sends can be missing from this screen.
  // `schema` is dropped: it names the message format, not the household.
  function payloadRows(payload) {
    if (!payload || typeof payload !== "object") return [];
    return Object.keys(payload)
      .filter(function (key) { return key !== "schema"; })
      .map(function (key) {
        return { key: key, label: LABELS[key] || key, value: describe(key, payload[key]) };
      });
  }

  // whereLine says what happens to the message underneath it. Branched on the
  // switch, because "Sent to https://… once a day" printed under a ping that
  // is switched off is a claim this screen cannot keep — and not making claims
  // it cannot keep is the only reason this tab exists.
  //
  // Which is why the send time is described and not sold. It is a random walk
  // in six-hour steps, so it takes about four days to work its way round the
  // clock, and until then one box's arrival times are still loosely related.
  function whereLine(body) {
    var endpoint = (body && body.endpoint) || "nowhere";
    if (body && body.enabled) {
      return "Sent to " + endpoint +
        ", once a day, at a time that moves from day to day — it takes a few days to work its way round the clock.";
    }
    return "Nothing is sent. This is the message that would go to " + endpoint +
      ", once a day, if you switched it on.";
  }

  function row(label, value) {
    var el = document.createElement("div");
    el.className = "fleet-ping-row";

    var name = document.createElement("span");
    name.className = "fleet-ping-label";
    name.textContent = label;
    el.appendChild(name);

    var val = document.createElement("span");
    val.className = "fleet-ping-value";
    val.textContent = value;
    el.appendChild(val);

    return el;
  }

  function paint(body) {
    var slot = document.getElementById("fleet-ping-payload");
    if (!slot) return;
    slot.textContent = "";

    var rows = payloadRows(body && body.payload);
    if (rows.length === 0) {
      var none = document.createElement("p");
      none.className = "hint";
      none.textContent = "This box has nothing to send yet.";
      slot.appendChild(none);
      return;
    }
    rows.forEach(function (r) { slot.appendChild(row(r.label, r.value)); });

    var where = document.createElement("p");
    where.className = "hint";
    where.textContent = whereLine(body);
    slot.appendChild(where);

    var exact = document.createElement("pre");
    exact.className = "fleet-ping-json";
    exact.textContent = JSON.stringify(body.payload, null, 2);
    slot.appendChild(exact);
  }

  function refresh() {
    var slot = document.getElementById("fleet-ping-payload");
    if (!slot) return;
    fetch("/api/fleet-ping")
      .then(function (r) { return r.ok ? r.json() : null; })
      .then(function (body) {
        if (!body) {
          slot.textContent = "";
          var err = document.createElement("p");
          err.className = "hint";
          err.textContent = "The fleet ping is not running on this box, so there is nothing to show.";
          slot.appendChild(err);
          return;
        }
        paint(body);
      })
      .catch(function () {
        slot.textContent = "";
        var err = document.createElement("p");
        err.className = "hint";
        err.textContent = "Could not reach the box.";
        slot.appendChild(err);
      });
  }

  S.tabs.fleet = {
    _pure: { payloadRows: payloadRows, describe: describe, whereLine: whereLine },

    // The line under the payload is the box's answer, not the checkbox's, so
    // it can only be right again once the box has the new config. Saving is
    // the moment that happens.
    afterSave: refresh,

    render: function (ctx) {
      // Mirrors the box's own default (see applyDefaults in
      // go/internal/config): a box that has never had the section is sending.
      // Rendering it unchecked here would say the opposite of what is true.
      if (!ctx.config.fleet_ping) ctx.config.fleet_ping = { enabled: true };
      var enabled = !!ctx.config.fleet_ping.enabled;

      setTimeout(refresh, 0);

      return (
        "<fieldset><legend>Fleet ping</legend>" +
        '<p class="hint">Once a day this box tells Sourceful six things: its ' +
        "FTW version, its release channel, the kinds of device it talks to, " +
        "roughly how much battery, its price zone, and roughly how old the " +
        "install is. Nothing else — no id, no key, no serial, no site name, no " +
        "counter, no timestamp — so nothing in the message says which box sent " +
        "it.</p>" +
        '<p class="hint fleet-ping-limits">Two limits, plainly. Those six things still describe ' +
        "you: a beta box in a small price zone with a big battery may be the " +
        "only one of its kind. And Sourceful sees the address you send from, " +
        "the way any website does. Below is the whole message, as this box " +
        "would send it right now.</p>" +
        '<label><input type="checkbox" id="fleet-ping-enabled" ' +
        'data-checkbox-path="fleet_ping.enabled"' + (enabled ? " checked" : "") +
        "> Send the daily fleet ping</label>" +
        '<p class="hint">Takes effect as soon as you save. No restart.</p>' +
        '<div id="fleet-ping-payload"></div>' +
        "</fieldset>"
      );
    },
  };
})();
