import assert from "node:assert/strict";
import { readFileSync } from "node:fs";
import { dirname, join } from "node:path";
import { describe, it } from "node:test";
import { fileURLToPath } from "node:url";

const webRoot = dirname(fileURLToPath(import.meta.url));
const html = readFileSync(join(webRoot, "index.html"), "utf8");
const app = readFileSync(join(webRoot, "app.js"), "utf8");

describe("strategy mode picker", () => {
  it("keeps Manual… on the Plan card in simple view", () => {
    // The simple/advanced UI toggle hides diagnostics. Manual fallbacks
    // used to ride that same class, so a house already on Self (manual)
    // had no selected button and no labelled way back to a planner
    // strategy until someone found ★ Advanced.
    const strategy = html.match(/class="plan-strategy"[\s\S]*?class="plan-help"/)?.[0] || "";
    assert.match(strategy, /id="mode-advanced-btn"/);
    assert.match(strategy, /id="mode-buttons"/);
    assert.doesNotMatch(strategy, /class="[^"]*advanced-only/);
  });

  it("opens the manual drawer when the live mode lives there", () => {
    assert.match(app, /function revealManualModes\(mode\)/);
    assert.match(app, /revealManualModes\(activeMode\)/);
    assert.match(app, /revealManualModes\(currentMode\)/);
  });

  it("auto-opens only when the mode changes", () => {
    // The status poll repeats the same mode every couple of seconds. Without
    // the early return, each one would force the drawer back open and undo
    // "Hide manual" a second after the user pressed it.
    assert.match(app, /lastRevealedMode = null/);
    assert.match(app, /if \(!mode \|\| mode === lastRevealedMode\) return;/);
  });

  it("marks the tapped mode before the POST returns", () => {
    assert.match(app, /function markModeActive\(mode\)/);
    assert.match(
      app,
      /function setMode\(mode\) \{\s*markModeActive\(mode\);\s*revealManualModes\(mode\);/s,
    );
    // …and holds it, so a status read already in flight with the old mode
    // can't flash the previous button back.
    assert.match(app, /pendingMode = mode;\s*pendingModeUntil = Date\.now\(\)/);
    assert.match(app, /var activeMode = pendingMode \|\| data\.mode;/);
  });
});
