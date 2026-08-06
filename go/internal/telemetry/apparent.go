package telemetry

import (
	"fmt"
	"math"
	"time"
)

// Apparent-power (kVA) estimation for demand-charge tracking. South
// African C&I demand charges bill on kVA, so the demand tracker needs S,
// not P. Site meters vary in what they emit; three methods, best first:
//
//  1. "phase_va": per-phase current × voltage. S = Σ V_n·I_n by
//     definition of apparent power. Currents (meter_l{n}_a) are already
//     emitted by every site-meter driver the fuse guard supports;
//     measured phase voltage (meter_l{n}_v) is used when fresh, else the
//     configured phase-neutral voltage.
//  2. "reactive": S = √(P² + Q²) from reactive telemetry. Q per phase is
//     read from meter_q_l{n}_var (signed net) or the DSMR-style split
//     meter_q_imp_l{n}_var − meter_q_exp_l{n}_var, plus the site-total
//     spellings meter_q_var / meter_q_imp_var − meter_q_exp_var.
//  3. "power_factor": S = |P| / pf, the configured assumption of last
//     resort (site.assumed_power_factor).
//
// Metric-name convention documented in docs/site-convention.md.

// ApparentPowerEstimate is one estimation result.
type ApparentPowerEstimate struct {
	VA     float64
	Method string // "phase_va" | "reactive" | "power_factor"
}

// SiteApparentPowerVA estimates the site's apparent power from the site
// meter's live metrics. realPowerW is the signed site real power (import
// positive); phases and phaseVoltage come from the fuse config; pf is the
// resolved assumed power factor. Metrics older than maxAge are ignored so
// a stale phase reading can't fabricate demand.
func SiteApparentPowerVA(store *Store, meterDriver string, realPowerW float64, phases int, phaseVoltage, pf float64, now time.Time, maxAge time.Duration) ApparentPowerEstimate {
	if phases < 1 {
		phases = 1
	} else if phases > 3 {
		phases = 3
	}
	if phaseVoltage <= 0 {
		phaseVoltage = 230
	}

	fresh := func(name string) (float64, bool) {
		v, at, ok := store.LatestMetric(meterDriver, name)
		if !ok || now.Sub(at) > maxAge {
			return 0, false
		}
		return v, true
	}

	// Method 1: Σ V·I over configured phases. All phases must have a
	// fresh current — a partial sum would understate demand.
	var totalVA float64
	haveAll := true
	for n := 1; n <= phases; n++ {
		i, ok := fresh(fmt.Sprintf("meter_l%d_a", n))
		if !ok {
			haveAll = false
			break
		}
		v := phaseVoltage
		if mv, ok := fresh(fmt.Sprintf("meter_l%d_v", n)); ok && mv > 0 {
			v = mv
		}
		totalVA += v * math.Abs(i)
	}
	if haveAll && totalVA > 0 {
		return ApparentPowerEstimate{VA: totalVA, Method: "phase_va"}
	}

	// Method 2: √(P²+Q²) from reactive telemetry.
	if q, ok := siteReactiveVAR(fresh, phases); ok {
		return ApparentPowerEstimate{
			VA:     math.Hypot(realPowerW, q),
			Method: "reactive",
		}
	}

	// Method 3: configured power-factor assumption.
	if pf <= 0 || pf > 1 {
		pf = 0.95
	}
	return ApparentPowerEstimate{VA: math.Abs(realPowerW) / pf, Method: "power_factor"}
}

// siteReactiveVAR assembles total reactive power from whichever spelling
// the driver emits. Per-phase beats site-total; signed net beats the
// import/export split.
func siteReactiveVAR(fresh func(string) (float64, bool), phases int) (float64, bool) {
	// Per-phase signed net.
	var total float64
	haveAll := true
	for n := 1; n <= phases; n++ {
		q, ok := fresh(fmt.Sprintf("meter_q_l%d_var", n))
		if !ok {
			haveAll = false
			break
		}
		total += q
	}
	if haveAll {
		return total, true
	}
	// Per-phase import/export split (DSMR).
	total = 0
	haveAll = true
	for n := 1; n <= phases; n++ {
		imp, okImp := fresh(fmt.Sprintf("meter_q_imp_l%d_var", n))
		exp, okExp := fresh(fmt.Sprintf("meter_q_exp_l%d_var", n))
		if !okImp && !okExp {
			haveAll = false
			break
		}
		total += imp - exp
	}
	if haveAll {
		return total, true
	}
	// Site totals.
	if q, ok := fresh("meter_q_var"); ok {
		return q, true
	}
	imp, okImp := fresh("meter_q_imp_var")
	exp, okExp := fresh("meter_q_exp_var")
	if okImp || okExp {
		return imp - exp, true
	}
	return 0, false
}
