package mpc

import (
	"context"
	"log/slog"
	"math"
	"sync"
	"time"

	"github.com/frahlg/forty-two-watts/go/internal/state"
	"github.com/frahlg/forty-two-watts/go/internal/telemetry"
)

// PVPredictor lets the MPC plug in a learned PV predictor (the digital
// twin) without importing its package. Implemented by
// *pvmodel.Service.Predict. Leave nil to use the naive forecast stored
// in the DB.
type PVPredictor func(t time.Time, cloudPct float64) float64

// Service wires the optimizer to the rest of the stack: pulls prices +
// forecast from the SQLite store, reads current SoC from the telemetry
// store, and re-plans on a ticker. The latest plan is cached.
type Service struct {
	Store    *state.Store
	Tele     *telemetry.Store
	Zone     string
	BaseLoad float64 // baseline household load (W). 0 disables load assumption.
	Horizon  time.Duration
	Interval time.Duration
	PV       PVPredictor // optional — overrides stored pv_w_estimated

	// ExportBonusOreKwh and ExportFeeOreKwh flow in from config.Price.
	// Used to compute default ExportOrePerKWh when Params doesn't set it.
	ExportBonusOreKwh float64
	ExportFeeOreKwh   float64

	Defaults Params

	mu   sync.RWMutex
	last *Plan

	stop chan struct{}
	done chan struct{}
}

// New constructs a service. Caller wires it in main.go after store + telemetry.
func New(st *state.Store, tl *telemetry.Store, zone string, p Params) *Service {
	return &Service{
		Store:    st,
		Tele:     tl,
		Zone:     zone,
		Defaults: p,
		Horizon:  24 * time.Hour,
		Interval: 15 * time.Minute,
		stop:     make(chan struct{}),
		done:     make(chan struct{}),
	}
}

// Latest returns the most recently computed plan (nil before first run).
func (s *Service) Latest() *Plan {
	if s == nil {
		return nil
	}
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.last
}

// MaxPlanAge is the staleness cutoff. Once a plan's `generated_at_ms`
// is older than this, we consider it stale and the control loop falls
// back to self_consumption. Picked to be ~2× the replan interval so a
// single missed replan doesn't flip us into fallback.
const MaxPlanAge = 30 * time.Minute

// GridTargetAt returns the plan's grid-power target for the slot
// containing `now`. Returns (0, false) if the plan is missing, stale,
// or `now` is outside the plan's horizon.
//
// The result is already in site sign convention (+ import, − export).
func (s *Service) GridTargetAt(now time.Time) (float64, bool) {
	if s == nil {
		return 0, false
	}
	s.mu.RLock()
	p := s.last
	s.mu.RUnlock()
	if p == nil {
		return 0, false
	}
	if time.Since(time.UnixMilli(p.GeneratedAtMs)) > MaxPlanAge {
		return 0, false
	}
	nowMs := now.UnixMilli()
	for _, a := range p.Actions {
		end := a.SlotStartMs + int64(a.SlotLenMin)*60*1000
		if nowMs >= a.SlotStartMs && nowMs < end {
			return a.GridW, true
		}
	}
	return 0, false
}

// SetMode changes the planner's operating mode and forces an immediate
// replan so the new mode takes effect within one control cycle.
func (s *Service) SetMode(ctx context.Context, mode Mode) {
	if s == nil {
		return
	}
	s.mu.Lock()
	s.Defaults.Mode = mode
	s.mu.Unlock()
	s.replan(ctx)
}

// Start runs the planner in a goroutine. Does an initial plan immediately.
func (s *Service) Start(ctx context.Context) {
	if s == nil {
		return
	}
	go s.loop(ctx)
}

// Stop terminates the planner.
func (s *Service) Stop() {
	if s == nil {
		return
	}
	close(s.stop)
	<-s.done
}

func (s *Service) loop(ctx context.Context) {
	defer close(s.done)
	s.replan(ctx)
	t := time.NewTicker(s.Interval)
	defer t.Stop()
	for {
		select {
		case <-s.stop:
			return
		case <-ctx.Done():
			return
		case <-t.C:
			s.replan(ctx)
		}
	}
}

// Replan recomputes the plan once using current prices + forecast + SoC.
// Exposed for tests and API triggers.
func (s *Service) Replan(ctx context.Context) *Plan { return s.replan(ctx) }

func (s *Service) replan(_ context.Context) *Plan {
	now := time.Now()
	untilMs := now.Add(s.Horizon).UnixMilli()
	sinceMs := now.UnixMilli() - 15*60*1000 // small margin — slot starting ≤15min ago still in-flight

	prices, err := s.Store.LoadPrices(s.Zone, sinceMs, untilMs)
	if err != nil {
		slog.Warn("mpc: load prices", "err", err)
		return nil
	}
	if len(prices) == 0 {
		slog.Info("mpc: no prices available yet")
		return nil
	}

	forecasts, err := s.Store.LoadForecasts(sinceMs, untilMs)
	if err != nil {
		slog.Warn("mpc: load forecasts", "err", err)
		// continue without PV forecast
	}

	slots := buildSlots(prices, forecasts, s.BaseLoad, now.UnixMilli(), s.PV)
	if len(slots) == 0 {
		return nil
	}

	// Current SoC: average of battery readings (weighted by capacity is
	// ideal, but for v1 we aggregate into one "mega-battery" so a mean
	// across whatever batteries are reporting is fine).
	p := s.Defaults
	p.InitialSoCPct = currentSoCPct(s.Tele, p.InitialSoCPct)

	// Default terminal valuation: mean import price over horizon (so the
	// planner is SoC-neutral rather than always ending empty).
	if p.TerminalSoCPrice <= 0 {
		var sum float64
		for _, pr := range prices {
			sum += pr.TotalOreKwh
		}
		p.TerminalSoCPrice = sum / float64(len(prices))
	}
	// Default export revenue: mean spot (no VAT, no grid tariff) plus
	// optional ExportBonusOreKwh (e.g. retailer premium / tax reduction)
	// minus ExportFeeOreKwh (DSO feed-in fee, if any). ExportOrePerKWh
	// in Params overrides all of this if set explicitly.
	if p.ExportOrePerKWh <= 0 {
		var sum float64
		for _, pr := range prices {
			sum += pr.SpotOreKwh
		}
		meanSpot := sum / float64(len(prices))
		p.ExportOrePerKWh = meanSpot + s.ExportBonusOreKwh - s.ExportFeeOreKwh
		if p.ExportOrePerKWh < 0 {
			p.ExportOrePerKWh = 0
		}
	}

	plan := Optimize(slots, p)

	s.mu.Lock()
	s.last = &plan
	s.mu.Unlock()
	slog.Info("mpc: replanned",
		"slots", len(slots),
		"soc_start", p.InitialSoCPct,
		"cost_ore", plan.TotalCostOre)
	return &plan
}

// buildSlots joins price rows with forecast rows by start time. Prices drive
// slot count + duration; forecast PV is interpolated forward (last valid
// value carries) because forecast is usually hourly while prices are 15-min.
//
// If `pv` is non-nil, the planner uses the learned twin's prediction
// (fed with the forecast's cloud cover) instead of the naive pv_w_estimated
// that the forecast service stored at fetch time. This lets the model
// learn system-specific orientation/shading/soiling and drive planning
// off the better signal without re-fetching weather.
func buildSlots(prices []state.PricePoint, forecasts []state.ForecastPoint, baseLoad float64, nowMs int64, pv PVPredictor) []Slot {
	out := make([]Slot, 0, len(prices))
	for _, pr := range prices {
		slotLen := pr.SlotLenMin
		if slotLen <= 0 {
			slotLen = 60
		}
		slotEnd := pr.SlotTsMs + int64(slotLen)*60*1000
		if slotEnd <= nowMs {
			continue // past slot
		}
		var pvW float64
		if pv != nil {
			cloud := lookupCloud(forecasts, pr.SlotTsMs)
			pvW = pv(time.UnixMilli(pr.SlotTsMs), cloud)
		} else {
			pvW = lookupPV(forecasts, pr.SlotTsMs)
		}
		out = append(out, Slot{
			StartMs:  pr.SlotTsMs,
			LenMin:   slotLen,
			PriceOre: pr.TotalOreKwh,
			PVW:      -math.Abs(pvW), // PV pushes in → negative (site sign)
			LoadW:    baseLoad,
		})
	}
	return out
}

// lookupCloud returns the cloud cover (%) for the forecast row covering
// `ts`, falling back to the nearest neighbour. 50% is the neutral
// prior if no forecast is available at all.
func lookupCloud(forecasts []state.ForecastPoint, ts int64) float64 {
	if len(forecasts) == 0 {
		return 50
	}
	for i, f := range forecasts {
		slotLen := f.SlotLenMin
		if slotLen <= 0 {
			slotLen = 60
		}
		end := f.SlotTsMs + int64(slotLen)*60*1000
		if ts >= f.SlotTsMs && ts < end {
			if f.CloudCoverPct != nil {
				return *f.CloudCoverPct
			}
			return 50
		}
		if ts < f.SlotTsMs && i > 0 {
			if prev := forecasts[i-1]; prev.CloudCoverPct != nil {
				return *prev.CloudCoverPct
			}
		}
	}
	if last := forecasts[len(forecasts)-1]; last.CloudCoverPct != nil {
		return *last.CloudCoverPct
	}
	return 50
}

// lookupPV finds the forecast row whose slot covers ts and returns its PV
// estimate (W, non-negative). Returns 0 if no forecast or no estimate.
func lookupPV(forecasts []state.ForecastPoint, ts int64) float64 {
	if len(forecasts) == 0 {
		return 0
	}
	// Binary-search would be faster, but len is typically ≤ 49 (met.no).
	for i, f := range forecasts {
		slotLen := f.SlotLenMin
		if slotLen <= 0 {
			slotLen = 60
		}
		end := f.SlotTsMs + int64(slotLen)*60*1000
		if ts >= f.SlotTsMs && ts < end {
			if f.PVWEstimated != nil {
				return *f.PVWEstimated
			}
			return 0
		}
		// Fall back: if before first, or between rows, use the preceding row.
		if ts < f.SlotTsMs && i > 0 {
			if prev := forecasts[i-1]; prev.PVWEstimated != nil {
				return *prev.PVWEstimated
			}
		}
	}
	// After last row — use last known
	if last := forecasts[len(forecasts)-1]; last.PVWEstimated != nil {
		return *last.PVWEstimated
	}
	return 0
}

// currentSoCPct averages SoC across battery readings in the telemetry store.
// Telemetry stores SoC as a fraction in [0, 1]; the MPC expects [0, 100].
// Falls back to `fallback` (already in percent) if no readings are present.
func currentSoCPct(t *telemetry.Store, fallback float64) float64 {
	if t == nil {
		return fallback
	}
	bats := t.ReadingsByType(telemetry.DerBattery)
	if len(bats) == 0 {
		return fallback
	}
	var sum float64
	var n int
	for _, b := range bats {
		if b.SoC != nil {
			sum += *b.SoC
			n++
		}
	}
	if n == 0 {
		return fallback
	}
	return sum / float64(n) * 100.0
}
