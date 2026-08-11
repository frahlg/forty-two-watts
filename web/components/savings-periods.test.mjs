import assert from "node:assert/strict";
import { describe, it } from "node:test";

import {
  SAVINGS_LOOKBACK_DAYS,
  buildSavingsPeriods,
  formatCompactMinor,
} from "./savings-periods.js";

function priced(day, savedMinor) {
  return {
    day,
    saved_ore: savedMinor,
    resolution: "slot",
  };
}

describe("compact monetary savings periods", () => {
  it("uses the box's latest day for today, the last 7 days, and month to date", () => {
    const periods = buildSavingsPeriods([
      priced("2026-01-29", 100),
      priced("2026-01-30", 200),
      priced("2026-01-31", 300),
      priced("2026-02-01", 400),
      priced("2026-02-02", 500),
      priced("2026-02-03", 600),
      priced("2026-02-04", 700),
    ]);

    assert.equal(SAVINGS_LOOKBACK_DAYS, 31);
    assert.deepEqual(
      {
        today: periods.today.savedMinor,
        week: periods.week.savedMinor,
        month: periods.month.savedMinor,
      },
      { today: 700, week: 2800, month: 2200 },
    );
    assert.deepEqual(
      {
        today: periods.today.totalDays,
        week: periods.week.totalDays,
        month: periods.month.totalDays,
      },
      { today: 1, week: 7, month: 4 },
    );
  });

  it("marks a period partial and excludes days with no price data", () => {
    const rows = [
      priced("2026-08-05", 100),
      priced("2026-08-06", 100),
      { ...priced("2026-08-07", 999), resolution: "no_prices" },
      priced("2026-08-08", 100),
      priced("2026-08-09", 100),
      priced("2026-08-10", 100),
      priced("2026-08-11", 100),
    ];

    const week = buildSavingsPeriods(rows).week;
    assert.equal(week.savedMinor, 600);
    assert.equal(week.pricedDays, 6);
    assert.equal(week.totalDays, 7);
    assert.equal(week.available, true);
    assert.equal(week.complete, false);
  });

  it("keeps an unavailable period distinct from a real zero", () => {
    const unavailable = buildSavingsPeriods([
      { ...priced("2026-08-11", 0), resolution: "no_prices" },
    ]).today;
    const zero = buildSavingsPeriods([
      priced("2026-08-11", 0),
    ]).today;

    assert.equal(unavailable.available, false);
    assert.equal(zero.available, true);
    assert.equal(formatCompactMinor(zero.savedMinor), "+0.00");
  });

  it("formats signed major-unit values for the small Overview tile", () => {
    assert.equal(formatCompactMinor(123), "+1.23");
    assert.equal(formatCompactMinor(-1250), "−12.5");
    assert.equal(formatCompactMinor(12500), "+125");
  });
});
