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

	slots := buildSlots(prices, forecasts, s.BaseLoad, now.UnixMilli())
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
	// Default export revenue: mean spot (no VAT, no grid tariff) — what
	// most Swedish retailers credit for surplus PV/battery export.
	if p.ExportOrePerKWh <= 0 {
		var sum float64
		for _, pr := range prices {
			sum += pr.SpotOreKwh
		}
		p.ExportOrePerKWh = sum / float64(len(prices))
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
func buildSlots(prices []state.PricePoint, forecasts []state.ForecastPoint, baseLoad float64, nowMs int64) []Slot {
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
		pvW := lookupPV(forecasts, pr.SlotTsMs)
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
// Falls back to `fallback` if no readings are present.
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
	return sum / float64(n)
}
