// Pure view-model builder for <ftw-demand-card>. Separated from the
// element so node --test can exercise the derivations without a DOM.
//
// Input is the /api/demand payload:
//   { demand: {peak_kva, window_avg_kva, window_avg_kw, window_start_ms,
//              window_len_ms, window_covered_ms, window_band,
//              window_counted, projected_peak_kva, cycle_start_ms},
//     nmd_kva, demand_charge_ct_kva, currency, intervals: [...] }

const BAND_LABELS = { peak: "Peak", standard: "Standard", offpeak: "Off-peak" };

export function buildDemandView(payload) {
  const d = (payload && payload.demand) || {};
  const nmd = Number(payload && payload.nmd_kva) || 0;
  const peak = Number(d.peak_kva) || 0;
  const windowAvg = Number(d.window_avg_kva) || 0;
  const projected = Math.max(Number(d.projected_peak_kva) || 0, peak);

  const lenMs = Number(d.window_len_ms) || 0;
  const coveredMs = Number(d.window_covered_ms) || 0;
  const windowProgress = lenMs > 0 ? Math.min(1, coveredMs / lenMs) : 0;

  // Utilization of the notified maximum demand. >1 means the site is
  // over its contracted NMD — penalty territory.
  const peakUtil = nmd > 0 ? peak / nmd : 0;
  const windowUtil = nmd > 0 ? windowAvg / nmd : 0;

  // Severity drives the highlight color: the running window matters
  // only while it counts toward billing.
  let severity = "ok";
  const counted = Boolean(d.window_counted);
  const worst = Math.max(peakUtil, counted ? windowUtil : 0);
  if (nmd > 0 && worst >= 1) severity = "over";
  else if (nmd > 0 && worst >= 0.85) severity = "near";

  // The running window threatens a new billing peak when it is counted
  // and its average already exceeds the recorded peak.
  const threatens = counted && windowAvg > peak && peak >= 0;

  return {
    hasNMD: nmd > 0,
    nmdKVA: nmd,
    peakKVA: peak,
    projectedKVA: projected,
    windowKVA: windowAvg,
    windowKW: Number(d.window_avg_kw) || 0,
    windowProgress,
    windowBand: BAND_LABELS[d.window_band] || d.window_band || "",
    windowCounted: counted,
    peakUtil,
    windowUtil,
    severity,
    threatens,
    demandChargeCt: Number(payload && payload.demand_charge_ct_kva) || 0,
    currency: (payload && payload.currency) || "",
  };
}

// formatKVA renders a kVA figure for the card: whole numbers above 100,
// one decimal below, and "—" when nothing has been measured yet.
export function formatKVA(v) {
  if (!Number.isFinite(v) || v <= 0) return "—";
  return v >= 100 ? String(Math.round(v)) : v.toFixed(1);
}

// estimateDemandCost renders the billing-cycle demand cost so far in
// major currency units (peak kVA × rate in minor units), or null when
// either side is unknown.
export function estimateDemandCost(peakKVA, rateCt) {
  if (!(peakKVA > 0) || !(rateCt > 0)) return null;
  return (peakKVA * rateCt) / 100;
}
