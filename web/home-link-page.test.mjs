import test from "node:test";
import assert from "node:assert/strict";
import { readFileSync } from "node:fs";

const page = readFileSync(new URL("./home-link.html", import.meta.url), "utf8");

test("Home Link loads its design tokens and renders a dashboard", () => {
  assert.match(page, /href="\/components\/theme\.css"/);
  assert.match(page, /background:[\s\S]*var\(--ink\)/);
  assert.match(page, /color:\s*var\(--fg\)/);
  assert.match(page, /data-scope="ftw\.overview\.read"/);
  assert.match(page, /id="remote-grid"/);
  assert.match(page, /id="remote-pv"/);
  assert.match(page, /id="remote-battery"/);
  assert.match(page, /id="remote-load"/);
  assert.match(page, /id="remote-soc"/);
});

test("Home Link renders typed values instead of raw JSON", () => {
  assert.doesNotMatch(page, /JSON\.stringify\(response/);
  assert.doesNotMatch(page, /<pre\b/);
  assert.match(page, /function renderOverview/);
  assert.match(page, /function renderPlan/);
  assert.match(page, /function renderAssets/);
  assert.match(page, /function renderHistory/);
});

test("Home Link puts remote values into text nodes", () => {
  assert.doesNotMatch(page, /\.innerHTML\s*=/);
  assert.match(page, /data\.textContent = value/);
  assert.match(page, /label\.textContent = asset\.label/);
});

test("Home Link opens a new session instead of dying with the old one", () => {
  // The relay ends a browser stream on idle and caps its lifetime, so every
  // read must be able to reconnect rather than fail until a manual reload.
  assert.match(page, /function ensureSession/);
  assert.match(page, /session\.isFailed\(\)/);
  assert.match(page, /ensureSession\(\)\.then\(function \(\) \{\s*return session\.read/);
  assert.doesNotMatch(page, /^\s*session\.read\(scope, history\)/m);
});

test("Home Link shows why a session or read failed", () => {
  // A swallowed reason left a relay-side rejection indistinguishable from an
  // offline gateway, so failures must carry their cause into the UI.
  assert.match(page, /function reason\(error\)/);
  assert.doesNotMatch(page, /\.catch\(function \(\) \{/);
  assert.match(page, /"Could not reach this home\." \+ reason\(error\)/);
});
