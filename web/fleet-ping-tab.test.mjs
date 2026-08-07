import assert from "node:assert/strict";
import { readFileSync } from "node:fs";
import { describe, it } from "node:test";
import vm from "node:vm";

// The fleet ping tab makes one claim: what you see is what the box sends.
// These tests hold it to that. The rows must come from the payload rather
// than from a list in the tab, and the checkbox must reflect the box's own
// default rather than a friendlier one.
//
// fleet.js is a plain IIFE, so it loads under a small shim and these tests
// drive the real render() and the real row builder.

const source = readFileSync(new URL("./settings/tabs/fleet.js", import.meta.url), "utf8");

function loadTab() {
  const win = { FTWSettings: { tabs: {} } };
  const sandbox = {
    window: win,
    // render() defers its fetch; the tests below read the returned markup and
    // call the pure helpers, so the callback never needs to run.
    setTimeout: () => 0,
    document: { getElementById: () => null },
    fetch: () => new Promise(() => {}),
    JSON,
    Array,
    Object,
    String,
    console,
  };
  sandbox.globalThis = sandbox;
  vm.createContext(sandbox);
  vm.runInContext(source, sandbox);
  const tab = win.FTWSettings.tabs.fleet;
  assert.ok(tab && typeof tab.render === "function", "fleet.js registered no tab");
  return tab;
}

const tab = loadTab();
const { payloadRows, whereLine } = tab._pure;

function render(config) {
  return tab.render({ config });
}

const payload = {
  schema: "ftw.fleet/1",
  ftw_version: "v1.4.0-beta.2",
  channel: "beta",
  drivers: ["easee_cloud", "sungrow"],
  battery_kwh: "5-15",
  price_zone: "SE3",
  install_age: "6-12m",
};

describe("the fleet ping tab", () => {
  it("offers a checkbox bound to fleet_ping.enabled", () => {
    const html = render({});
    assert.match(html, /data-checkbox-path="fleet_ping\.enabled"/);
    assert.match(html, /type="checkbox"/);
  });

  it("reflects the saved setting", () => {
    assert.doesNotMatch(render({ fleet_ping: { enabled: false } }), /checkbox"[^>]*checked/);
    assert.match(render({ fleet_ping: { enabled: true } }), /checked/);
  });

  it("mirrors the box's default when the section is missing", () => {
    // A box that has never had fleet_ping in its YAML is sending, because
    // applyDefaults turns it on. An unchecked box here would say the opposite
    // of what is true, which is the one thing this screen must never do.
    const config = {};
    const html = render(config);
    // Compared field by field: the object is built inside the vm sandbox, so
    // its prototype comes from another realm and deepStrictEqual refuses it.
    assert.ok(config.fleet_ping, "fleet_ping was not created");
    assert.equal(config.fleet_ping.enabled, true);
    assert.match(html, /checked/);
  });

  it("says the switch needs no restart", () => {
    // The one thing about this setting that differs from the FTW app tab
    // next to it, where saving is not enough.
    assert.match(render({}), /[Nn]o restart/);
  });

  it("names every field the message carries", () => {
    // This screen is the whole argument for a feature that is on by default,
    // and the sentence people actually read is the paragraph, not the table
    // under it. A paragraph that names four of six fields is the screen
    // making its case with the awkward ones left out — the price zone above
    // all, since it is the only field that says roughly where the house is.
    const html = render({});
    for (const field of [/version/i, /channel/i, /device|driver/i, /battery/i, /price zone/i, /how old/i]) {
      assert.match(html, field, `the paragraph does not name ${field}`);
    }
  });

  it("counts the fields it names, and the count is the fixture's", () => {
    // "six things" is a claim like any other on this screen, and the only one
    // that can go stale in silence.
    //
    // What this holds is the sentence against the fixture above, which is a
    // copy of the payload written here by hand. It is not a guard on the box:
    // a seventh field would leave this passing. That guard is
    // TestPayloadCarriesOnlyTheAgreedFields in go/internal/fleetping, which
    // counts the marshalled fields and fails on the seventh — and whoever
    // answers it has to come back through here.
    const words = ["no", "one", "two", "three", "four", "five", "six", "seven", "eight"];
    const fields = Object.keys(payload).length - 1; // schema names the format, not the box
    assert.ok(words[fields], `no number word for ${fields} fields`);
    assert.match(render({}), new RegExp(words[fields] + " things"));
  });

  it("names what the message leaves out, counter and timestamp included", () => {
    const html = render({});
    for (const absent of [/no id/i, /no key/i, /no serial/i, /no counter/i, /no timestamp/i]) {
      assert.match(html, absent, `the paragraph does not say ${absent}`);
    }
  });

  it("concedes the limits instead of claiming the box cannot be recognised", () => {
    // go/internal/fleetping says it plainly: the payload is still a
    // quasi-identifier, and the source IP is the honest limit. A screen that
    // promises more than the package behind it claims is the one thing this
    // tab must never do.
    const html = render({});
    assert.match(html, /the only one of its kind/i);
    assert.match(html, /address you send from/i);
    assert.doesNotMatch(html, /nothing that lets two days be tied to the same box/i);
    // And the first paragraph must not settle the question the second one
    // exists to concede. go/internal/fleetping says two pings from an unusual
    // box could be guessed to be the same box; a reader who stops after the
    // first paragraph would already have been told they could not.
    assert.doesNotMatch(html, /came from the same box/i);
    // And set apart, because .hint carries no margin: two of them in a row
    // render as one slab, which is how a paragraph nobody reads happens.
    assert.match(html, /<p class="hint fleet-ping-limits">Two limits/);
  });

  it("leaves a slot for the payload rather than writing values into the markup", () => {
    // If the tab printed the fields itself, the screen would be a second
    // rendering that could disagree with what is sent.
    const html = render({});
    assert.match(html, /id="fleet-ping-payload"/);
    assert.doesNotMatch(html, /sungrow|SE3|v1\.\d/);
  });
});

describe("the line under the payload", () => {
  const endpoint = "https://telemetry.sourceful.energy/v1/fleet";

  it("says where the message goes while the ping is on", () => {
    const line = whereLine({ enabled: true, endpoint, payload });
    assert.match(line, /Sent to https:\/\/telemetry\.sourceful\.energy/);
    assert.match(line, /once a day/);
  });

  it("does not promise the box cannot be known by when it calls", () => {
    // The send time is a random walk in six-hour steps, so it takes about four
    // days to work its way round the clock. Until then one box's arrival times
    // are still loosely related, and a line saying otherwise is a promise the
    // schedule does not keep.
    const line = whereLine({ enabled: true, endpoint, payload });
    assert.doesNotMatch(line, /cannot be recognised/i);
    assert.match(line, /a few days/i);
  });

  it("does not claim anything is sent while the ping is off", () => {
    // This is the one screen whose entire justification is that it makes no
    // claim it cannot keep. "Sent to https://… once a day" under a switched
    // off ping is a false one.
    const line = whereLine({ enabled: false, endpoint, payload });
    assert.doesNotMatch(line, /^Sent to/);
    assert.match(line, /Nothing is sent/);
    // The address still shows: somebody deciding whether to switch it on
    // needs to see where it would go.
    assert.match(line, /telemetry\.sourceful\.energy/);
  });

  it("reads a missing body as off rather than as sending", () => {
    assert.match(whereLine(undefined), /Nothing is sent/);
    assert.match(whereLine({}), /Nothing is sent/);
  });
});

describe("keeping the line true after a save", () => {
  it("registers a post-save hook, because render() runs only on a tab switch", () => {
    // Without this the line keeps whatever the box said when the tab opened:
    // switch the ping off, save, and the screen still reads "Sent to …".
    assert.equal(typeof tab.afterSave, "function");
  });

  it("asks the box again when the shell calls it", () => {
    // The hook has to re-read /api/fleet-ping — the new answer is the box's,
    // and the checkbox in the form is not it.
    const asked = [];
    const sandbox = {
      window: { FTWSettings: { tabs: {} } },
      setTimeout: () => 0,
      document: { getElementById: () => ({ textContent: "", appendChild() {} }), createElement: () => ({ classList: {}, appendChild() {} }) },
      fetch: (path) => { asked.push(path); return new Promise(() => {}); },
      JSON, Array, Object, String, console,
    };
    sandbox.globalThis = sandbox;
    vm.createContext(sandbox);
    vm.runInContext(source, sandbox);
    sandbox.window.FTWSettings.tabs.fleet.afterSave();
    assert.deepEqual(asked, ["/api/fleet-ping"]);
  });
});

describe("payloadRows", () => {
  it("shows every field the box sends, in plain words", () => {
    const rows = payloadRows(payload);
    const byKey = Object.fromEntries(rows.map((r) => [r.key, r]));

    assert.equal(byKey.ftw_version.value, "v1.4.0-beta.2");
    assert.equal(byKey.channel.value, "beta");
    assert.equal(byKey.drivers.value, "easee_cloud, sungrow");
    assert.equal(byKey.battery_kwh.value, "5 to 15 kWh");
    assert.equal(byKey.price_zone.value, "SE3");
    assert.equal(byKey.install_age.value, "six to twelve months");

    assert.equal(byKey.battery_kwh.label, "Battery size");
  });

  it("drops only the schema name", () => {
    const keys = payloadRows(payload).map((r) => r.key);
    assert.ok(!keys.includes("schema"), "schema is the message format, not the household");
    assert.equal(keys.length, Object.keys(payload).length - 1);
  });

  it("cannot hide a field the box started sending", () => {
    // The whole point of this screen. If a future version of the box added
    // something it should not, a tab driven by a fixed list would look
    // unchanged; this one shows the field under its own name.
    const rows = payloadRows({ ...payload, gateway_id: "8f2a1c" });
    const row = rows.find((r) => r.key === "gateway_id");
    assert.ok(row, "an unexpected field was silently dropped");
    assert.equal(row.label, "gateway_id");
    assert.equal(row.value, "8f2a1c");
  });

  it("reads an empty driver list as none rather than blank", () => {
    const rows = payloadRows({ ...payload, drivers: [] });
    assert.equal(rows.find((r) => r.key === "drivers").value, "none");
  });

  it("survives having nothing to show", () => {
    // Lengths, not deepEqual: the arrays are built inside the vm sandbox, so
    // their prototype comes from another realm.
    assert.equal(payloadRows(null).length, 0);
    assert.equal(payloadRows(undefined).length, 0);
    assert.equal(payloadRows("not an object").length, 0);
  });
});
