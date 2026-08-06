package main

import (
	"log/slog"
	"time"

	"github.com/srcfl/ftw/go/internal/config"
	"github.com/srcfl/ftw/go/internal/demand"
	"github.com/srcfl/ftw/go/internal/mpc"
	"github.com/srcfl/ftw/go/internal/state"
	"github.com/srcfl/ftw/go/internal/tariff"
)

// demandPersister adapts state.Store to the demand package's narrow
// Persister interface without coupling the two packages.
type demandPersister struct{ st *state.Store }

func (p demandPersister) SaveDemandInterval(iv demand.Interval) error {
	return p.st.SaveDemandInterval(state.DemandInterval{
		CycleStartMs:    iv.CycleStartMs,
		IntervalStartMs: iv.IntervalStartMs,
		AvgKVA:          iv.AvgKVA,
		AvgKW:           iv.AvgKW,
		Band:            iv.Band,
		Counted:         iv.Counted,
	})
}

// newDemandTracker compiles the tariff schedule and builds the billing-
// demand tracker, seeded with the persisted peak for the current cycle.
// Returns nil (no tracking) when no tariff is configured; a broken tariff
// config is a startup error — the operator asked for demand-charge
// management, silently skipping it would be a billing surprise.
func newDemandTracker(cfg *config.Config, st *state.Store) (*demand.Tracker, *tariff.Schedule, error) {
	if cfg.Tariff == nil {
		return nil, nil, nil
	}
	sched, err := tariff.Compile(cfg.Tariff)
	if err != nil {
		return nil, nil, err
	}
	classify := func(ts time.Time) demand.Classification {
		r, err := sched.Resolve(ts)
		if err != nil {
			// Validated schedules cover every minute; treat a resolve
			// failure as an uncounted gap rather than guessing a band.
			return demand.Classification{}
		}
		return demand.Classification{Band: string(r.Band), Counted: r.DemandActive}
	}
	windowLen := time.Duration(cfg.Tariff.DemandIntegrationMin) * time.Minute
	tr := demand.New(windowLen, classify, sched.BillingCycleStart, demandPersister{st})

	cycle := sched.BillingCycleStart(time.Now())
	if peak, ivMs, ok, err := st.BillingPeak(cycle.UnixMilli()); err != nil {
		slog.Warn("billing peak restore failed", "err", err)
	} else if ok {
		tr.SeedPeak(cycle, peak, ivMs)
		slog.Info("billing peak restored", "cycle_start", cycle.Format(time.DateOnly), "peak_kva", peak)
	}
	return tr, sched, nil
}

// tariffPriceSource renders the compiled schedule as planner price rows.
// SpotOreKwh stays 0: a scheduled-tariff C&I site without an export
// agreement earns nothing at the meter, so the planner must never value
// battery-to-grid export. Source "tariff" gets full confidence in
// buildSlots — the rate table is deterministic.
func tariffPriceSource(sched *tariff.Schedule, zone string) func(fromMs, untilMs int64) []state.PricePoint {
	return func(fromMs, untilMs int64) []state.PricePoint {
		from := time.UnixMilli(fromMs).In(sched.Location).Truncate(time.Hour)
		until := time.UnixMilli(untilMs).In(sched.Location)
		if !until.Truncate(time.Hour).Equal(until) {
			// Whole hours only — a partial tail slot would carry a
			// misleading length into the planner.
			until = until.Truncate(time.Hour).Add(time.Hour)
		}
		slots, err := sched.SlotPrices(from, until, time.Hour)
		if err != nil {
			slog.Warn("tariff price source", "err", err)
			return nil
		}
		out := make([]state.PricePoint, 0, len(slots))
		for _, sp := range slots {
			out = append(out, state.PricePoint{
				Zone:        zone,
				SlotTsMs:    sp.Start.UnixMilli(),
				SlotLenMin:  int(sp.Len / time.Minute),
				SpotOreKwh:  0,
				TotalOreKwh: sp.RateCtKWh,
				Source:      "tariff",
			})
		}
		return out
	}
}

// wireCommercialSpec connects the tariff schedule + demand tracker to the
// planner: per-slot demand-window flags, the live billing peak (kVA →
// real-power W via the assumed power factor, since the planner optimizes
// W), the kVA demand rate converted the same way, and the backup reserve.
func wireCommercialSpec(svc *mpc.Service, cfg *config.Config, sched *tariff.Schedule, tr *demand.Tracker) {
	if svc == nil || sched == nil || tr == nil {
		return
	}
	pf := cfg.Site.EffectivePowerFactor()
	rateOrePerKW := 0.0
	if cfg.Tariff != nil && cfg.Tariff.DemandChargeCtKVA > 0 {
		// cost = rate_kva × kVA = (rate_kva / pf) × kW.
		rateOrePerKW = cfg.Tariff.DemandChargeCtKVA / pf
	}
	backupWh := 0.0
	if cfg.Site.BackupReserve != nil {
		backupWh = cfg.Site.BackupReserve.MinUsableEnergyWh
	}
	if rateOrePerKW == 0 && backupWh == 0 {
		return
	}
	svc.Commercial = func(slots []mpc.Slot) *mpc.CommercialSpec {
		spec := &mpc.CommercialSpec{
			DemandRateOrePerKW:      rateOrePerKW,
			BillingPeakSoFarW:       tr.PeakSoFarKVA(time.Now()) * 1000 * pf,
			BackupMinUsableEnergyWh: backupWh,
		}
		if rateOrePerKW > 0 {
			active := make([]bool, len(slots))
			for i, sl := range slots {
				mid := time.UnixMilli(sl.StartMs).Add(time.Duration(sl.LenMin) * time.Minute / 2)
				if r, err := sched.Resolve(mid); err == nil {
					active[i] = r.DemandActive
				}
			}
			spec.DemandActive = active
		}
		return spec
	}
}
