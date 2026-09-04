import assert from 'node:assert/strict';
import { readFileSync } from 'node:fs';
import test from 'node:test';

const source = readFileSync(new URL('./app.js', import.meta.url), 'utf8');

// The EV modal's plan-status strip (#1002): one sentence on why the
// charger is (not) charging and when it will. These assertions pin the
// user-facing decision branches so a refactor can't silently drop one.

test('plan-status strip renders every visibility state', () => {
  // Planned window with a start/end clock and the planned energy.
  assert.match(source, /Charging planned " \+ evFmtClock\(lp\.plan_next_start_ms\)/);
  assert.match(source, /plan_total_wh \/ 1000/);
  // Charger offering power the car does not take, with the charger's
  // own reason when the driver reports one.
  assert.match(source, /but the car isn't drawing/);
  assert.match(source, /reason_no_current_label/);
  // The silent grid-plan deferral is named instead of looking like a
  // PV-only mode nobody chose.
  assert.match(source, /Waiting for tomorrow's electricity prices/);
  assert.match(source, /grid_deferred/);
  // Manual hold names its cost: the plan is off until the car is full,
  // Stop or unplug.
  assert.match(source, /plan and PV logic are off until the car is full, Stop or unplug/);
  // The do-nothing default is called out with the three ways out.
  assert.match(source, /set a schedule, turn on PV only, or press Start/);
});

test('plan-status strip is the headline of the plan view', () => {
  // The sentence is rendered inside buildEvPlanView's update(), which
  // runs on every poll like the status table.
  assert.match(source, /var fresh = renderEvPlanStatus\(lpNow, dNow\)/);
  assert.match(source, /evPlanEl\.update\(matched, d\)/);
  // And reset when the modal short-circuits to "no charger".
  assert.match(source, /statusTableEl = null;\s*\n\s*evPlanEl = null;/);
});
