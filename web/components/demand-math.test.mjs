import assert from "node:assert/strict";
import { describe, it } from "node:test";

import { buildDemandView, formatKVA, estimateDemandCost } from "./demand-math.js";

function payload(overrides = {}, demandOverrides = {}) {
  return {
    demand: {
      peak_kva: 180,
      window_avg_kva: 120,
      window_avg_kw: 114,
      window_len_ms: 1_800_000,
      window_covered_ms: 900_000,
      window_band: "peak",
      window_counted: true,
      projected_peak_kva: 180,
      ...demandOverrides,
    },
    nmd_kva: 250,
    demand_charge_ct_kva: 35000,
    currency: "ZAR",
    ...overrides,
  };
}

describe("buildDemandView", () => {
  it("derives utilization and progress", () => {
    const v = buildDemandView(payload());
    assert.equal(v.hasNMD, true);
    assert.equal(v.peakKVA, 180);
    assert.equal(v.windowProgress, 0.5);
    assert.ok(Math.abs(v.peakUtil - 0.72) < 1e-9);
    assert.equal(v.severity, "ok");
    assert.equal(v.windowBand, "Peak");
  });

  it("flags near and over NMD", () => {
    assert.equal(buildDemandView(payload({}, { peak_kva: 220 })).severity, "near");
    assert.equal(buildDemandView(payload({}, { peak_kva: 260 })).severity, "over");
  });

  it("ignores an uncounted window for severity and threat", () => {
    const v = buildDemandView(
      payload({}, { window_counted: false, window_band: "offpeak", window_avg_kva: 400 }),
    );
    assert.equal(v.severity, "ok");
    assert.equal(v.threatens, false);
    assert.equal(v.windowBand, "Off-peak");
  });

  it("marks a counted window that beats the recorded peak", () => {
    const v = buildDemandView(payload({}, { window_avg_kva: 200 }));
    assert.equal(v.threatens, true);
  });

  it("copes with a site that has no NMD declared", () => {
    const v = buildDemandView(payload({ nmd_kva: 0 }));
    assert.equal(v.hasNMD, false);
    assert.equal(v.severity, "ok");
    assert.equal(v.peakUtil, 0);
  });

  it("survives an empty payload", () => {
    const v = buildDemandView({});
    assert.equal(v.peakKVA, 0);
    assert.equal(v.hasNMD, false);
    assert.equal(v.windowProgress, 0);
  });
});

describe("formatKVA", () => {
  it("renders dashes, decimals and whole numbers by magnitude", () => {
    assert.equal(formatKVA(0), "—");
    assert.equal(formatKVA(NaN), "—");
    assert.equal(formatKVA(42.35), "42.4");
    assert.equal(formatKVA(180.4), "180");
  });
});

describe("estimateDemandCost", () => {
  it("multiplies peak by the minor-unit rate into major units", () => {
    assert.equal(estimateDemandCost(180, 35000), 63000);
    assert.equal(estimateDemandCost(0, 35000), null);
    assert.equal(estimateDemandCost(180, 0), null);
  });
});
