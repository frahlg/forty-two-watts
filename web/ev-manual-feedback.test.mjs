import assert from 'node:assert/strict';
import { readFileSync } from 'node:fs';
import test from 'node:test';

const source = readFileSync(new URL('./app.js', import.meta.url), 'utf8');
const manual = source.slice(
  source.indexOf('function buildManualChargeSection'),
  source.indexOf('function sliderHeader'),
);
const statusText = source.slice(
  source.indexOf('function manualStatusText'),
  source.indexOf('function renderEvPlanStatus'),
);

// After Charge now the operator must always know what is happening (#1002).
// Field report 2026-09-05: the tab said "Charging at 16 A" a tenth of a
// second after the click, the Easee cloud takes 5–15 s to act, nothing on
// screen moved, and the operator removed the charger to charge by hand.

test('the status line follows the charger through every state', () => {
  for (const state of ['sent', 'accepted', 'charging', 'not_drawing', 'stalled', 'limited']) {
    assert.match(statusText, new RegExp(`case "${state}":`));
  }
  assert.match(statusText, /Waiting for it to confirm/);
  assert.match(statusText, /Waiting for the car to start drawing/);
  assert.match(statusText, /but the car is not drawing/);
  assert.match(statusText, /the charger has not acted on/);
  assert.match(statusText, /Main fuse limits this charge/);
  // The charger's own words are part of the sentence.
  assert.match(statusText, /Charger reports: " \+ m\.charger_reason/);
  // Elapsed time comes from the box's since_ms, not from click time.
  assert.match(statusText, /m\.since_ms/);
});

test('the manual tab is redrawn on every poll', () => {
  assert.match(manual, /return \{ el: box, update: update \};/);
  assert.match(source, /container\.update = function \(nextLp, d\) \{ manual\.update\(nextLp, d\); \};/);
  assert.match(source, /evTabsEl\.update\(matched, d\);/);
});

test("a refused Start reads as a failure with the server's reason", () => {
  assert.match(manual, /if \(r\.ok\) return r;/);
  assert.match(manual, /\.then\(failOn\)/);
  assert.match(manual, /"Start failed: " \+ \(\(e && e\.message\) \|\| "try again"\)/);
  assert.doesNotMatch(manual, /"Charging at " \+ a \+ " A until the car is full/);
});

test('the plan strip uses the same sentence while a manual charge runs', () => {
  const strip = source.slice(
    source.indexOf('function renderEvPlanStatus'),
    source.indexOf('// EV modal sub-elements held across refreshes'),
  );
  assert.match(strip, /manualStatusText\(lp, d\)/);
});
