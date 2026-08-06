package main

import (
	"log/slog"
	"time"

	"github.com/srcfl/ftw/go/internal/config"
	"github.com/srcfl/ftw/go/internal/demand"
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
func newDemandTracker(cfg *config.Config, st *state.Store) (*demand.Tracker, error) {
	if cfg.Tariff == nil {
		return nil, nil
	}
	sched, err := tariff.Compile(cfg.Tariff)
	if err != nil {
		return nil, err
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
	return tr, nil
}
