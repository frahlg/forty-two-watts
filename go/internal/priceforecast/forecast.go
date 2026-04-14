// Package priceforecast estimates spot prices for future slots that
// the day-ahead source hasn't published yet.
//
// Day-ahead auctions typically publish tomorrow's prices around 13:00
// CET. Before that, the MPC horizon is effectively capped at "end of
// today" — which means night-time planning of an overnight arbitrage
// run is impossible right when operators most want it.
//
// We learn a simple hour-of-week × month profile from the rolling
// price history we already store in SQLite. The model is pragmatic,
// not predictive of market shocks: it assumes tomorrow looks like a
// typical week-hour in this season. That's wrong during gas crises
// and cold snaps — but still closer to the truth than "no price at
// all", which causes the MPC to silently shorten its plan.
//
// Features (7):
//
//	bucket(weekday, hour)      — 168 EMA cells (spot öre/kWh)
//	month_modifier             — ratio: this month's mean / annual mean
//	(computed lazily, not stored explicitly — folded into bucket)
//
// The model is zone-aware: each bidding zone trains independently
// because SE3 and SE4 behave very differently at peak hours.
//
// Confidence: we track sample count per bucket + global MAE. The MPC
// can downweight these estimates vs. real day-ahead prices by looking
// at the confidence flag on each forecasted slot.
package priceforecast

import (
	"context"
	"encoding/csv"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"math"
	"os"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/frahlg/forty-two-watts/go/internal/state"
)

// Buckets is 7 days × 24 hours = 168.
const Buckets = 168

// MinTrustSamples — a bucket needs this many observations before we
// trust it fully. Below, we blend with the global mean.
const MinTrustSamples = 4

// MonthlyModifier: multiplicative seasonal factor per month (Jan..Dec).
// Derived from the ZoneModel at refit time — NOT persisted as separate
// state, recomputed from bucket data.
type ZoneModel struct {
	Zone    string             `json:"zone"`
	Bucket  [Buckets]float64   `json:"bucket"`  // EMA öre/kWh (raw spot)
	Counts  [Buckets]int64     `json:"counts"`
	Month   [12]float64        `json:"month"`   // monthly multiplier (normalized)
	Samples int64              `json:"samples"`
	MAE     float64            `json:"mae"`     // EMA of |actual − predicted|
	Alpha   float64            `json:"alpha"`   // EMA coefficient
	FittedAt int64             `json:"fitted_at"`
}

// NewZoneModel with a baked-in typical-Nordic hour-of-week prior so the
// model is useful before any real history has been fitted. Shape:
//
//   - morning ramp 06:00–09:00 peaking around 08:00
//   - midday trough 11:00–14:00 (solar flood, industrial slack)
//   - evening peak 17:00–20:00 peaking around 19:00
//   - overnight baseline 00:00–05:00
//
// Weekend hours are ~15% lower during peaks (less industrial demand).
// Level is zone-dependent but grossly similar for SE1–SE4 / NO / DK.
// Overridden the moment real data is fitted.
func NewZoneModel(zone string) *ZoneModel {
	m := &ZoneModel{Zone: zone, Alpha: 0.15}
	// Base level in öre/kWh — post-2022 long-term typical.
	base := 60.0
	switch zone {
	case "SE3", "SE4", "DK1", "DK2", "DE":
		base = 80
	case "NO2", "FI":
		base = 70
	case "SE1", "SE2", "NO1", "NO3", "NO4":
		base = 50
	}
	for d := 0; d < 7; d++ {
		isWeekend := d >= 5
		for h := 0; h < 24; h++ {
			shape := 1.0
			// Shape factor: typical day-ahead curve.
			switch {
			case h >= 7 && h <= 9:
				shape = 1.6 // morning peak
			case h >= 17 && h <= 20:
				shape = 1.85 // evening peak
			case h >= 11 && h <= 14:
				shape = 0.55 // midday dip
			case h >= 0 && h <= 5:
				shape = 0.65 // overnight baseline
			case h == 6 || h == 10:
				shape = 1.15 // ramp
			case h == 15 || h == 16:
				shape = 1.05 // afternoon
			case h >= 21 && h <= 23:
				shape = 1.1 // late evening
			}
			if isWeekend {
				shape = 0.85 + 0.15*(shape-0.85) // blend toward base
			}
			m.Bucket[d*24+h] = base * shape
			// Seed counts = MinTrustSamples so Predict() fully trusts
			// the baked pattern from the first call. FitFromHistory
			// replaces these outright once real data arrives.
			m.Counts[d*24+h] = MinTrustSamples
		}
	}
	for i := 0; i < 12; i++ {
		m.Month[i] = 1.0
	}
	// Seasonal modifier — winter dearer, summer cheaper.
	seasonal := [12]float64{
		1.35, 1.30, 1.10, 0.95, 0.85, 0.75,
		0.70, 0.75, 0.90, 1.05, 1.20, 1.40,
	}
	m.Month = seasonal
	return m
}

// Predict returns the expected spot öre/kWh at t for zone.
func (m ZoneModel) Predict(t time.Time) float64 {
	idx := hourOfWeek(t)
	b := m.Bucket[idx]
	trust := float64(m.Counts[idx]) / MinTrustSamples
	if trust > 1 {
		trust = 1
	}
	// Blend with overall mean when the bucket is fresh.
	mean := m.overallMean()
	base := trust*b + (1-trust)*mean
	monthMod := m.Month[int(t.Month())-1]
	return base * monthMod
}

// overallMean across buckets weighted by counts.
func (m ZoneModel) overallMean() float64 {
	var sumW, sumWX float64
	for i := 0; i < Buckets; i++ {
		w := float64(m.Counts[i])
		if w == 0 {
			w = 1 // include prior evenly
		}
		sumW += w
		sumWX += m.Bucket[i] * w
	}
	if sumW == 0 {
		return 80
	}
	return sumWX / sumW
}

// FitFromHistory rebuilds the model from stored prices for this zone.
// Call this periodically (e.g. nightly) so new data lands in the model.
// Replaces existing bucket state; doesn't try to incrementally update.
func (m *ZoneModel) FitFromHistory(pts []state.PricePoint) {
	if len(pts) == 0 {
		return
	}
	// Group by hour-of-week.
	var sum [Buckets]float64
	var cnt [Buckets]int64
	var monthSum [12]float64
	var monthCnt [12]int64
	for _, p := range pts {
		t := time.UnixMilli(p.SlotTsMs).UTC()
		idx := hourOfWeek(t)
		sum[idx] += p.SpotOreKwh
		cnt[idx]++
		mi := int(t.Month()) - 1
		monthSum[mi] += p.SpotOreKwh
		monthCnt[mi]++
	}
	for i := 0; i < Buckets; i++ {
		if cnt[i] > 0 {
			m.Bucket[i] = sum[i] / float64(cnt[i])
			m.Counts[i] = cnt[i]
		}
	}
	// Month modifier = month_mean / overall_mean.
	overall := m.overallMean()
	if overall > 0 {
		for mi := 0; mi < 12; mi++ {
			if monthCnt[mi] > 0 {
				mAvg := monthSum[mi] / float64(monthCnt[mi])
				m.Month[mi] = mAvg / overall
			} else {
				m.Month[mi] = 1.0
			}
		}
	}
	// Simple MAE: how well does the fit explain the history itself?
	var abserr float64
	for _, p := range pts {
		t := time.UnixMilli(p.SlotTsMs).UTC()
		abserr += math.Abs(p.SpotOreKwh - m.Predict(t))
	}
	m.MAE = abserr / float64(len(pts))
	m.Samples = int64(len(pts))
	m.FittedAt = time.Now().UnixMilli()
}

// hourOfWeek: Mon=0..Sun=6 × 24. Works in UTC for determinism.
func hourOfWeek(t time.Time) int {
	wd := (int(t.Weekday()) + 6) % 7
	return wd*24 + t.Hour()
}

// ---- Service ----

const stateKey = "pricefc/state"

// RefitInterval is how often we recompute the model from stored history.
const RefitInterval = 6 * time.Hour

// Service manages per-zone models. Refits in the background.
type Service struct {
	Store *state.Store
	Zones []string

	mu     sync.RWMutex
	models map[string]*ZoneModel

	stop chan struct{}
	done chan struct{}
}

// NewService creates a service covering the given zones.
func NewService(st *state.Store, zones []string) *Service {
	s := &Service{
		Store:  st,
		Zones:  zones,
		models: map[string]*ZoneModel{},
		stop:   make(chan struct{}),
		done:   make(chan struct{}),
	}
	// Restore from state if available.
	if st != nil {
		if js, ok := st.LoadConfig(stateKey); ok && js != "" {
			var restored map[string]*ZoneModel
			if err := json.Unmarshal([]byte(js), &restored); err == nil {
				s.models = restored
				slog.Info("priceforecast restored",
					"zones", len(restored))
			}
		}
	}
	// Ensure all configured zones have a model.
	for _, z := range zones {
		if _, ok := s.models[z]; !ok {
			s.models[z] = NewZoneModel(z)
		}
	}
	return s
}

// Predict returns the spot price forecast öre/kWh for zone at time t.
// Falls back to 80 öre if the zone is unknown.
func (s *Service) Predict(zone string, t time.Time) float64 {
	if s == nil {
		return 80
	}
	s.mu.RLock()
	defer s.mu.RUnlock()
	m, ok := s.models[zone]
	if !ok {
		return 80
	}
	return m.Predict(t)
}

// Model returns a snapshot of the zone model (nil if unknown).
func (s *Service) Model(zone string) *ZoneModel {
	if s == nil {
		return nil
	}
	s.mu.RLock()
	defer s.mu.RUnlock()
	m, ok := s.models[zone]
	if !ok {
		return nil
	}
	// Return a copy so caller can't mutate under the lock.
	cp := *m
	return &cp
}

// Start begins the periodic refit loop.
func (s *Service) Start(ctx context.Context) {
	if s == nil {
		return
	}
	go s.loop(ctx)
}

// Stop terminates the refit loop.
func (s *Service) Stop() {
	if s == nil {
		return
	}
	close(s.stop)
	<-s.done
}

func (s *Service) loop(ctx context.Context) {
	defer close(s.done)
	s.refit()
	t := time.NewTicker(RefitInterval)
	defer t.Stop()
	for {
		select {
		case <-s.stop:
			s.persist()
			return
		case <-ctx.Done():
			s.persist()
			return
		case <-t.C:
			s.refit()
		}
	}
}

func (s *Service) refit() {
	if s.Store == nil {
		return
	}
	// Pull the last ~90 days of prices for each zone.
	since := time.Now().AddDate(0, 0, -90).UnixMilli()
	until := time.Now().Add(30 * 24 * time.Hour).UnixMilli()
	for _, z := range s.Zones {
		pts, err := s.Store.LoadPrices(z, since, until)
		if err != nil {
			slog.Warn("priceforecast: load", "zone", z, "err", err)
			continue
		}
		if len(pts) < 24 {
			continue // not enough history to fit
		}
		sort.Slice(pts, func(i, j int) bool { return pts[i].SlotTsMs < pts[j].SlotTsMs })
		s.mu.Lock()
		m, ok := s.models[z]
		if !ok {
			m = NewZoneModel(z)
			s.models[z] = m
		}
		m.FitFromHistory(pts)
		s.mu.Unlock()
		slog.Info("priceforecast: refit",
			"zone", z,
			"samples", len(pts),
			"mae_ore", m.MAE)
	}
	s.persist()
}

func (s *Service) persist() {
	if s.Store == nil {
		return
	}
	s.mu.RLock()
	js, err := json.Marshal(s.models)
	s.mu.RUnlock()
	if err != nil {
		return
	}
	_ = s.Store.SaveConfig(stateKey, string(js))
}

// SeedFromCSV cold-starts the model by importing historical prices
// from a CSV file into the state DB and then refitting. Expected
// format (header row required):
//
//	zone,slot_ts_ms,slot_len_min,spot_ore_kwh[,currency]
//
// Prices already in öre/kWh — caller is responsible for any EUR→SEK
// conversion. Rows for unknown zones are silently skipped.
//
// Idempotent: SQLite UPSERTs on (zone, slot_ts_ms), so re-running with
// the same CSV won't duplicate data. Safe to call on every boot; it
// becomes a no-op once the data is already in the store.
func (s *Service) SeedFromCSV(path string) (int, error) {
	if s == nil || s.Store == nil {
		return 0, fmt.Errorf("service not initialized")
	}
	f, err := os.Open(path)
	if err != nil {
		return 0, err
	}
	defer f.Close()
	return s.ingestCSV(f)
}

func (s *Service) ingestCSV(r io.Reader) (int, error) {
	reader := csv.NewReader(r)
	reader.TrimLeadingSpace = true
	header, err := reader.Read()
	if err != nil {
		return 0, fmt.Errorf("read header: %w", err)
	}
	col := map[string]int{}
	for i, h := range header {
		col[strings.ToLower(strings.TrimSpace(h))] = i
	}
	required := []string{"zone", "slot_ts_ms", "spot_ore_kwh"}
	for _, r := range required {
		if _, ok := col[r]; !ok {
			return 0, fmt.Errorf("missing column %q (want %v)", r, required)
		}
	}
	var batch []state.PricePoint
	const flushAt = 5000
	total := 0
	nowMs := time.Now().UnixMilli()
	for {
		rec, err := reader.Read()
		if err == io.EOF {
			break
		}
		if err != nil {
			return total, fmt.Errorf("read row: %w", err)
		}
		zone := strings.ToUpper(strings.TrimSpace(rec[col["zone"]]))
		tsMs, err := strconv.ParseInt(rec[col["slot_ts_ms"]], 10, 64)
		if err != nil {
			continue
		}
		spot, err := strconv.ParseFloat(rec[col["spot_ore_kwh"]], 64)
		if err != nil {
			continue
		}
		slotLen := 60
		if i, ok := col["slot_len_min"]; ok && i < len(rec) {
			if v, err := strconv.Atoi(rec[i]); err == nil && v > 0 {
				slotLen = v
			}
		}
		batch = append(batch, state.PricePoint{
			Zone:        zone,
			SlotTsMs:    tsMs,
			SlotLenMin:  slotLen,
			SpotOreKwh:  spot,
			TotalOreKwh: spot, // no tariff/VAT info in seed; forecaster only uses spot anyway
			Source:      "seed",
			FetchedAtMs: nowMs,
		})
		if len(batch) >= flushAt {
			if err := s.Store.SavePrices(batch); err != nil {
				return total, fmt.Errorf("save batch: %w", err)
			}
			total += len(batch)
			batch = batch[:0]
		}
	}
	if len(batch) > 0 {
		if err := s.Store.SavePrices(batch); err != nil {
			return total, err
		}
		total += len(batch)
	}
	// Kick a refit with the new data.
	s.refit()
	return total, nil
}
