// Choosing which driver version runs, and being able to change your mind.
//
// Every driver runs locally; they differ only in where the file came from. So
// what an operator needs is narrow: see what is running, see what else they
// could run, switch, and switch back when the new one misbehaves.
//
// These tests drive the real code with the payload the API really returns.
// The previous suite matched the source with regexes, which is how a picker
// that read the wrong field and rendered zero rows shipped green.

import assert from "node:assert/strict";
import { readFileSync } from "node:fs";
import test from "node:test";
import vm from "node:vm";

// The click handler returns nothing, so awaiting click() does not await the
// fetch chain behind it. Let the microtasks drain.
const settle = () => new Promise((resolve) => setImmediate(resolve));

const source = readFileSync(new URL("./settings/tabs/devices.js", import.meta.url), "utf8");

function element(tag) {
  const listeners = new Map();
  const el = {
    tag,
    children: [],
    className: "",
    disabled: false,
    style: {},
    textContent: "",
    dataset: {},
    type: "",
    addEventListener(name, handler) { listeners.set(name, handler); },
    click() { return listeners.get("click")?.({ target: el }); },
    appendChild(child) { el.children.push(child); return child; },
    remove() {},
    querySelector() { return null; },
    querySelectorAll() { return []; },
  };
  return el;
}

// The whole subtree as one string, the way it reads on screen.
function textOf(el) {
  return [el.textContent, ...el.children.map(textOf)].filter(Boolean).join(" ");
}

function buttonsOf(el) {
  return el.children.flatMap((c) => (c.tag === "button" ? [c] : buttonsOf(c)));
}

function load() {
  const calls = [];
  const window = { FTWSettings: { tabs: {} } };
  vm.runInNewContext(source, {
    document: { createElement: element, getElementById: () => null },
    fetch: async (path, options = {}) => {
      calls.push({ path, body: options.body ? JSON.parse(options.body) : null });
      return { ok: true, json: async () => ({ status: "ok" }) };
    },
    window,
  });
  return { api: window.FTWSettings.driverVersions, calls };
}

// Exactly what GET /api/device_repository/drivers/{id}/versions answers with:
// a VersionCandidate carries the version on .driver, and .installed only when
// that version is already on disk.
const PAYLOAD = {
  driver_id: "ferroamp",
  installed: null,
  available: [
    {
      repository_id: "ftw-official",
      driver: {
        version: "1.1.1",
        sha256: "f825…",
        metadata: { verification_status: "experimental" },
      },
    },
    {
      repository_id: "ftw-official",
      driver: {
        version: "1.0.0",
        sha256: "aa11…",
        metadata: { verification_status: "production" },
      },
      installed: { version: "1.0.0", sha256: "aa11…", active: true },
    },
  ],
};

test("the channel payload produces one row per version", () => {
  const { api } = load();
  const rows = api.versionRows(PAYLOAD);

  assert.equal(rows.length, 2,
    "the version lives on candidate.driver, not on the candidate itself; " +
    "reading the outer object filters every row away and the panel renders empty");
  assert.equal(rows.map((r) => r.version).join(" "), "1.1.1 1.0.0");
});

test("a candidate already on disk is marked from .installed, not by matching strings", () => {
  const { api } = load();
  const [newer, running] = api.versionRows(PAYLOAD);

  assert.equal(newer.downloaded, false, "1.1.1 has no install record");
  assert.equal(running.downloaded, true);
  assert.equal(running.active, true, "and it is the one running");
});

test("each row carries how well tested that version is", () => {
  const { api } = load();
  const [newer, running] = api.versionRows(PAYLOAD);

  // Upgrading onto an untested driver is a decision, so it has to be visible
  // at the moment of choosing rather than only in the setup wizard.
  assert.equal(newer.verification, "untested");
  assert.equal(running.verification, "verified on hardware");
});

test("a version on disk the channel no longer lists is still offered", () => {
  const { api } = load();
  const rows = api.versionRows({
    installed: [{ version: "0.9.2", sha256: "bb22…", active: false }],
    available: [],
  });

  assert.equal(rows.map((r) => r.version).join(" "), "0.9.2",
    "an older version whose manifest entry was dropped is exactly what " +
    "someone reaches for when a new driver misbehaves");
  assert.equal(rows[0].downloaded, true);
});

test("the same version is not listed twice when it is both installed and offered", () => {
  const { api } = load();
  const rows = api.versionRows({
    installed: [{ version: "1.0.0", sha256: "aa11…", active: true }],
    available: PAYLOAD.available,
  });

  assert.equal(rows.map((r) => r.version).join(" "), "1.1.1 1.0.0");
});

test("the panel says what is running and what can be switched to", () => {
  const { api } = load();
  const panel = element("div");
  api.render(panel, "ferroamp", PAYLOAD, { runningVersion: "1.0.0" });

  const text = textOf(panel);
  assert.match(text, /v1\.1\.1/);
  assert.match(text, /untested/);
  assert.match(text, /running now/);
  assert.match(text, /verified on hardware/);
});

test("switching to a version that is not downloaded fetches it first", async () => {
  const { api, calls } = load();
  const panel = element("div");
  api.render(panel, "ferroamp", PAYLOAD, { runningVersion: "1.0.0" });

  const [useNewer] = buttonsOf(panel);
  assert.equal(useNewer.textContent, "Use this");
  useNewer.click();
  await settle();

  assert.equal(calls.length, 1);
  assert.equal(calls[0].path, "/api/device_repository/drivers/ferroamp/install",
    "a version that is not on disk has to come down from the channel");
  assert.equal(calls[0].body.version, "1.1.1");

  // POST /install answers 400 "repository_id or channel is required" without
  // it. A version on its own does not say who signed it.
  assert.equal(calls[0].body.repository_id, "ftw-official");
});

test("after switching, undo activates a previous version kept on disk", async () => {
  const { api, calls } = load();
  const panel = element("div");
  api.render(panel, "ferroamp", PAYLOAD, {
    runningVersion: "1.0.0", logicalPath: "drivers/ferroamp.lua",
  });

  buttonsOf(panel)[0].click();
  await settle();
  const undo = buttonsOf(panel).find((b) => b.textContent.startsWith("Undo"));

  // Trying a driver and putting the old one back is the loop that makes
  // testing safe. It must not need a second trip through the list.
  assert.ok(undo, "a switch has to be reversible from where it happened");
  assert.match(undo.textContent, /back to v1\.0\.0/);

  undo.click();
  await settle();
  assert.equal(calls[1].path, "/api/device_repository/drivers/ferroamp/activate",
    "1.0.0 is retained on disk, so switching back needs no network");
  assert.equal(calls[1].body.version, "1.0.0");

  // Labbing means going back and forth, so the switch has to be re-armed
  // rather than left greyed out until the panel is reopened.
  assert.equal(buttonsOf(panel)[0].disabled, false,
    "after undo, trying the other version again is one click");
});

test("undo rolls back instead when the previous version is the bundled driver", async () => {
  const { api, calls } = load();
  const panel = element("div");
  // Nothing retained: the running 1.0.0 is the copy shipped with the build,
  // which is not an install and cannot be activated by version.
  api.render(panel, "ferroamp", { installed: [], available: [PAYLOAD.available[0]] }, {
    runningVersion: "1.0.0", logicalPath: "drivers/ferroamp.lua",
  });

  buttonsOf(panel)[0].click();
  await settle();
  const undo = buttonsOf(panel).find((b) => b.textContent.startsWith("Undo"));
  assert.ok(undo, "installing over the bundled driver is the first thing anyone does");

  undo.click();
  await settle();
  assert.equal(calls[1].path, "/api/device_repository/drivers/ferroamp/rollback",
    "activate answers 'not retained locally' for a version that was never installed");
  assert.equal(calls[1].body.logical_path, "drivers/ferroamp.lua");
});

test("the version that is running is not offered as a switch target", () => {
  const { api } = load();
  const panel = element("div");
  api.render(panel, "ferroamp", PAYLOAD, { runningVersion: "1.0.0" });

  const labels = buttonsOf(panel).map((b) => b.textContent);
  assert.equal(labels.join(" "), "Use this", "only 1.1.1 is a switch; 1.0.0 already runs");
});

test("an override downloads without claiming it will take over", async () => {
  const { api, calls } = load();
  const panel = element("div");
  api.render(panel, "ferroamp", PAYLOAD, { overridden: true, runningVersion: "1.0.0" });

  assert.match(textOf(panel), /Your own file runs while it is there/,
    "say why nothing here changes what runs");

  const buttons = buttonsOf(panel);
  assert.equal(buttons.map((b) => b.textContent).join(" "), "Download Downloaded",
    "an override shadows the channel, so 'Use this' would be a lie");
  assert.equal(buttons[1].disabled, true, "already on disk, nothing to fetch");

  buttons[0].click();
  await settle();
  assert.equal(calls[0].path, "/api/device_repository/drivers/ferroamp/install");
});

test("an empty history says so rather than rendering nothing", () => {
  const { api } = load();
  const panel = element("div");
  api.render(panel, "ferroamp", { installed: [], available: [] });

  assert.match(panel.textContent, /No versions found for this driver/,
    "a silently empty panel looks like a broken request");
});

test("what is running reads as words, not as an enum", () => {
  const { api } = load();

  const managed = api.runningSummary({
    source: "managed", installed_version: "1.1.1", verification_status: "production",
  });
  assert.equal(managed.headline, "v1.1.1");
  assert.equal(managed.detail, "official · verified on hardware");

  const bundled = api.runningSummary({
    source: "bundled", version: "1.0.0", verification_status: "experimental",
  });
  assert.equal(bundled.headline, "v1.0.0");
  assert.equal(bundled.detail, "official, shipped with this build · untested");

  // An operator's own file has no version the channel would recognise, so
  // naming one would read as provenance it does not have. Point at the file
  // instead, which is what they need to edit or delete.
  const own = api.runningSummary({
    source: "local", version: "local", path: "drivers/ferroamp.lua",
  });
  assert.equal(own.headline, "your own file");
  assert.equal(own.detail, "drivers/ferroamp.lua");
});

test("an override is not offered an Update button", () => {
  const { api } = load();

  const managed = api.runningSummary({
    source: "managed", version: "1.0.0", update_available: true,
    repository_id: "ftw-official", upstream_version: "1.1.1",
  });
  assert.equal(managed.updatable, true);

  // Installing a channel version while a local file is present changes
  // nothing: the local file still wins.
  const overridden = api.runningSummary({
    source: "local", version: "local", update_available: true,
    repository_id: "ftw-official", upstream_version: "1.1.1",
  });
  assert.equal(overridden.updatable, false);
});

test("manifest data becomes text, never markup", () => {
  const { api } = load();
  const picker = source.slice(
    source.indexOf("function renderVersionPicker"),
    source.indexOf("function offerUndo"));

  assert.ok(!/innerHTML/.test(picker),
    "version strings come from a signed manifest, but a signed manifest is " +
    "still remote input and must not be able to inject markup");
  assert.match(picker, /createElement\("button"\)/);
});
