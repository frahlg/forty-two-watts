import assert from 'node:assert/strict';
import { readFileSync } from 'node:fs';
import test from 'node:test';

const source = readFileSync(new URL('./app.js', import.meta.url), 'utf8');

// "Charge now → target" (#1002): Start is a bounded boost that hands
// back to the plan at the target SoC, not a pin-forever.

test('Start posts a release target and says where it stops', () => {
  assert.match(source, /release_at_soc_pct: releasePct/);
  // Target defaults to the schedule SoC, 80 % otherwise.
  assert.match(source, /lp\.schedule && lp\.schedule\.soc > 0\)\s*\n?\s*\? Math\.round\(lp\.schedule\.soc \* 100\) : 80/);
  // The button names its contract.
  assert.match(source, /"Charge now → " \+ releasePct \+ " %"/);
  // Active state explains the release, both in the manual tab and the
  // plan strip.
  assert.match(source, /stops at " \+ Math\.round\(lp\.manual_release_soc \* 100\)/);
  assert.match(source, /returns to plan at/);
});
