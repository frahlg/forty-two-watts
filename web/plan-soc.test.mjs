import assert from "node:assert/strict";
import { readFileSync } from "node:fs";
import { describe, it } from "node:test";

import { derivePlanBrief } from "./plan-brief.js";
import { actionSoCPct, fillPlanSoC, finiteNumber } from "./plan-soc.js";

const planSource = readFileSync(new URL("./plan.js", import.meta.url), "utf8");

// Björn / MilliWatt help dump 2026-08-27 20:33 UTC: 20 kWh Pixii, live
// 53.5%, first slot battery -577 W for 15 min, Charge end 52.7%.
const BJORN = {
  capacityWh: 20000,
  initialSoC: 53.5,
  batteryW: -577,
  slotLenMin: 15,
  chargeEff: 0.95,
  dischargeEff: 0.95,
};

describe("plan SoC reconstruction", () => {
  it("prefers a finite soc_pct over stored energy or integration", () => {
    const pct = actionSoCPct(
      { soc_pct: 52.7, battery_w: 7300, storage_energy_wh: { pixii: 0 } },
      { capacityWh: 20000, prevSoC: 40 },
    );
    assert.equal(pct, 52.7);
  });

  it("uses summed storage_energy_wh when soc_pct is missing", () => {
    const pct = actionSoCPct(
      { battery_w: 7300, storage_energy_wh: { pixii: 10540 } },
      { capacityWh: 20000, prevSoC: 40 },
    );
    assert.equal(pct, 52.7);
  });

  it("replays the Björn first slot from battery_w and efficiencies", () => {
    const pct = actionSoCPct(
      { battery_w: BJORN.batteryW, slot_len_min: BJORN.slotLenMin },
      {
        capacityWh: BJORN.capacityWh,
        prevSoC: BJORN.initialSoC,
        chargeEff: BJORN.chargeEff,
        dischargeEff: BJORN.dischargeEff,
      },
    );
    assert.ok(finiteNumber(pct));
    assert.ok(Math.abs(pct - 52.7) < 0.05, `got ${pct}, want ~52.7`);
  });

  it("does not treat a missing soc_pct as 0%", () => {
    assert.equal(actionSoCPct({ battery_w: 7300 }, {}), null);
    assert.equal(finiteNumber(null), false);
    assert.equal(finiteNumber(undefined), false);
    assert.equal(finiteNumber(NaN), false);
    assert.equal(finiteNumber(0), true);
  });

  it("fills a whole plan so later slots follow the first reconstructed SoC", () => {
    const plan = {
      capacity_wh: 20000,
      initial_soc_pct: 53.5,
      actions: [
        { slot_len_min: 15, battery_w: -577 },
        { slot_len_min: 15, battery_w: -577 },
      ],
    };
    fillPlanSoC(plan, { chargeEff: 0.95, dischargeEff: 0.95 });
    assert.ok(Math.abs(plan.actions[0].soc_pct - 52.7) < 0.05);
    assert.ok(plan.actions[1].soc_pct < plan.actions[0].soc_pct);
    assert.ok(plan.actions[1].soc_pct > 51);
  });

  it("leaves an already-finite soc_pct untouched", () => {
    const plan = {
      capacity_wh: 20000,
      initial_soc_pct: 53.5,
      actions: [{ slot_len_min: 15, battery_w: -10000, soc_pct: 52.7 }],
    };
    fillPlanSoC(plan, { chargeEff: 0.95, dischargeEff: 0.95 });
    assert.equal(plan.actions[0].soc_pct, 52.7);
  });
});

describe("plan UI uses reconstructed SoC", () => {
  it("loads fillPlanSoC before drawing or describing the plan", () => {
    assert.match(planSource, /import \{ fillPlanSoC \} from "\.\/plan-soc\.js"/);
    assert.match(planSource, /fillPlanSoC\(/);
    assert.match(planSource, /Number\.isFinite\(a\.soc_pct\)/);
    assert.match(planSource, /Number\.isFinite\(plan\.initial_soc_pct\)/);
  });

  it("surfaces Expected charge from a Björn-like plan that omitted soc_pct", () => {
    const now = Date.now();
    const plan = {
      capacity_wh: 20000,
      initial_soc_pct: 53.5,
      actions: [{
        slot_start_ms: now - 7 * 60_000,
        slot_len_min: 15,
        battery_w: -577,
        loadpoint_w: 0,
        pv_limit_w: 0,
        reason: "discharge — price above horizon mean",
        confidence: 1,
      }],
      solver: { engine: "cvxpy", backend: "highs", status: "optimal" },
    };
    const brief = derivePlanBrief({
      enabled: true,
      plan,
      status: { mode: "planner_passive_arbitrage", bat_soc: 0.535 },
      now,
      socOpts: { chargeEff: 0.95, dischargeEff: 0.95 },
    });
    assert.match(brief.soc.label, /53% after next step/);
    assert.match(brief.soc.detail, /53% at the end of the plan/);
  });
});
