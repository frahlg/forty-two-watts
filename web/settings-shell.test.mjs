import assert from "node:assert/strict";
import { readFileSync } from "node:fs";
import { describe, it } from "node:test";
import vm from "node:vm";

// The settings shell owns save, and render() runs only when a tab is opened or
// switched to. So a tab that shows what the BOX says about itself — rather
// than what the form holds — keeps showing the pre-save answer unless the
// shell calls it back. The fleet ping tab is the one that does, and the line
// it prints is a claim about whether anything is being sent.
//
// settings.js is a plain IIFE over a handful of elements, so it loads under a
// small DOM shim and this test drives the real save handler.

const source = readFileSync(new URL("./settings.js", import.meta.url), "utf8");

const ELEMENT_IDS = [
  "settings-modal", "settings-btn", "settings-close",
  "settings-save", "settings-status", "settings-tabs", "settings-body",
];

function stubElement() {
  return {
    textContent: "",
    className: "",
    innerHTML: "",
    dataset: {},
    handlers: {},
    classList: { add() {}, remove() {}, toggle() {} },
    addEventListener(type, fn) { this.handlers[type] = fn; },
    querySelectorAll: () => [],
    appendChild() {},
  };
}

function loadShell(saveResponse, ok = true) {
  const elements = {};
  for (const id of ELEMENT_IDS) elements[id] = stubElement();

  const requests = [];
  const sandbox = {
    window: { FTWSettings: { tabs: {} } },
    document: {
      getElementById: (id) => elements[id] || null,
      createElement: () => stubElement(),
    },
    fetch(path, opts) {
      requests.push({ path, opts });
      return Promise.resolve({ ok, status: ok ? 200 : 400, json: () => Promise.resolve(saveResponse) });
    },
    // The shell only uses timers to clear the "Saved" status and to poll after
    // a restart; neither is what these tests are about.
    setTimeout: () => 0,
    Date, JSON, Array, Object, String, console,
  };
  sandbox.globalThis = sandbox;
  vm.createContext(sandbox);
  vm.runInContext(source, sandbox);
  return { elements, requests, tabs: sandbox.window.FTWSettings.tabs };
}

// One turn of the event loop, which is all the save chain needs to settle.
const settled = () => new Promise((resolve) => setImmediate(resolve));

describe("the settings shell after a save", () => {
  it("calls the tab back so it can ask the box again", async () => {
    const { elements, tabs } = loadShell({ restart_required: false, restart_reasons: [] });
    let called = 0;
    // "control" is the tab the shell opens on.
    tabs.control = { render: () => "", afterSave: () => { called += 1; } };

    elements["settings-save"].handlers.click();
    await settled();

    assert.equal(called, 1, "the tab was never told the save landed");
  });

  it("survives a tab that has no post-save hook", async () => {
    const { elements, tabs, requests } = loadShell({ restart_required: false, restart_reasons: [] });
    tabs.control = { render: () => "" };

    elements["settings-save"].handlers.click();
    await settled();

    assert.equal(requests.length, 1);
    assert.equal(requests[0].path, "/api/config");
  });

  it("does not call the hook when the save was rejected", async () => {
    // Refreshing after a rejected save would put the box's unchanged answer
    // under a "Save failed" banner, which reads as though it went through.
    const { elements, tabs } = loadShell({ error: "validation: nope" }, false);
    let called = 0;
    tabs.control = { render: () => "", afterSave: () => { called += 1; } };

    elements["settings-save"].handlers.click();
    await settled();

    assert.equal(called, 0, "a rejected save told the tab it landed");
  });
});
