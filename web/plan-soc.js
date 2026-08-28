// Reconstruct planned battery SoC when a slot omits a finite soc_pct.
// The on-box Plan chart used to treat a missing value as 0% (canvas NaN
// becomes the 0% axis) and skip the tooltip row, while the help report
// still printed Charge end from the in-memory Go plan.

export function finiteNumber(value) {
  return typeof value === "number" && Number.isFinite(value);
}

function storageEnergyWh(action) {
  const stored = action && action.storage_energy_wh;
  if (!stored || typeof stored !== "object" || Array.isArray(stored)) return null;
  let sum = 0;
  let any = false;
  for (const value of Object.values(stored)) {
    if (!finiteNumber(value)) continue;
    sum += value;
    any = true;
  }
  return any ? sum : null;
}

function clampPct(pct) {
  if (!finiteNumber(pct)) return null;
  return Math.min(100, Math.max(0, pct));
}

function integrateBatteryWh(batteryW, slotLenMin, chargeEff, dischargeEff) {
  if (!finiteNumber(batteryW) || !finiteNumber(slotLenMin) || slotLenMin <= 0) return 0;
  const hours = slotLenMin / 60;
  const charge = finiteNumber(chargeEff) && chargeEff > 0 ? chargeEff : 1;
  const discharge = finiteNumber(dischargeEff) && dischargeEff > 0 ? dischargeEff : 1;
  // Site sign: +W charges the battery. Efficiency matches mpc.Optimize.
  if (batteryW >= 0) return batteryW * hours * charge;
  return batteryW * hours / discharge;
}

export function actionSoCPct(action, {
  capacityWh = null,
  prevSoC = null,
  chargeEff = 1,
  dischargeEff = 1,
} = {}) {
  if (finiteNumber(action && action.soc_pct)) return action.soc_pct;
  const storedWh = storageEnergyWh(action);
  if (finiteNumber(storedWh) && finiteNumber(capacityWh) && capacityWh > 0) {
    return clampPct(storedWh / capacityWh * 100);
  }
  if (!finiteNumber(prevSoC) || !finiteNumber(capacityWh) || capacityWh <= 0) return null;
  const deltaWh = integrateBatteryWh(
    action && action.battery_w,
    action && action.slot_len_min,
    chargeEff,
    dischargeEff,
  );
  return clampPct(prevSoC + deltaWh / capacityWh * 100);
}

export function fillPlanSoC(plan, opts = {}) {
  if (!plan || !Array.isArray(plan.actions)) return plan;
  const capacityWh = finiteNumber(plan.capacity_wh) ? plan.capacity_wh : null;
  let prevSoC = finiteNumber(plan.initial_soc_pct) ? plan.initial_soc_pct : null;
  for (const action of plan.actions) {
    const pct = actionSoCPct(action, { capacityWh, prevSoC, ...opts });
    if (!finiteNumber(pct)) continue;
    action.soc_pct = pct;
    prevSoC = pct;
  }
  return plan;
}
