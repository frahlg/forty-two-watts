// Choosing which driver version runs, and never being surprised by it.
//
// Two properties matter here and they pull in opposite directions:
//
//   1. An operator running their own copy must still be told when the channel
//      has something newer. An override shadows the channel silently, so
//      without this they never find out.
//
//   2. Being told must not mean being changed. Their copy keeps running until
//      they act. The channel keeps every version it has ever signed, and
//      reaching a specific one is their decision.

import { test } from "node:test";
import assert from "node:assert/strict";
import { readFileSync } from "node:fs";
import { fileURLToPath } from "node:url";
import { dirname, join } from "node:path";

const here = dirname(fileURLToPath(import.meta.url));
const source = readFileSync(join(here, "settings/tabs/devices.js"), "utf8");

// Just renderVersionPicker, so assertions about it cannot be satisfied or
// broken by unrelated code further down the file.
function renderVersionPickerBody() {
  const start = source.indexOf("function renderVersionPicker");
  assert.notEqual(start, -1, "renderVersionPicker must exist");
  const next = source.indexOf("\n  function ", start + 1);
  return source.slice(start, next === -1 ? undefined : next);
}

test("a local override is told what the channel has", () => {
  assert.match(source, /source === "local" && entry\.upstream_version/,
    "an override shadows the channel, so it must surface the newer version " +
    "rather than leaving the operator to find out some other way");
  assert.match(source, /channel has v/,
    "the version itself has to be on screen, not just the fact of an update");
});

test("the override is not offered a button that would do nothing", () => {
  // Installing a channel version while a local file is present changes
  // nothing: the local file still wins. A button implying otherwise would be
  // a lie the operator only discovers by debugging.
  const localBranch = source.slice(
    source.indexOf('source === "local" && entry.upstream_version'),
    source.indexOf('if (source === "managed")'));
  assert.ok(!/drv-module-update/.test(localBranch),
    "the update button belongs to managed drivers, not overrides");
  assert.match(localBranch, /your own copy is used while it is present/,
    "say why nothing is being offered");
});

test("the version picker is offered for managed drivers only", () => {
  assert.match(source, /source !== "local"[\s\S]{0,200}drv-module-versions/,
    "an override has no channel versions to switch between");
});

test("the picker distinguishes downloaded versions from ones still in the channel", () => {
  const render = renderVersionPickerBody();
  assert.match(render, /body && body\.installed/);
  assert.match(render, /body && body\.available/);

  // Activating something already on disk needs no network; fetching does.
  // The labels have to tell those apart or the operator cannot tell why one
  // is instant and the other is not.
  assert.match(render, /row\.local \? "Activate" : "Fetch and activate"/);
  assert.match(render, /row\.local[\s\S]{0,120}"\/activate"[\s\S]{0,40}"\/install"/,
    "an installed version activates; one that is not installed installs");
});

test("the running version is marked and cannot be re-activated", () => {
  const render = renderVersionPickerBody();
  assert.match(render, /row\.active \? " · running" : ""/,
    "an operator must be able to see which one is live");
  assert.match(render, /if \(!row\.active\) \{/,
    "no button on the version that is already running");
});

test("the picker builds DOM instead of assembling HTML", () => {
  const render = renderVersionPickerBody();
  assert.ok(!/innerHTML/.test(render),
    "version strings come from a signed manifest, but the manifest is still " +
    "remote input and must not be able to inject markup");
  assert.match(render, /createElement\("button"\)/);
  assert.match(render, /textContent = "v" \+ row\.version/);
});

test("an empty history says so rather than rendering nothing", () => {
  const render = renderVersionPickerBody();
  assert.match(render, /No versions found for this driver/,
    "a silently empty panel looks like a broken request");
});
