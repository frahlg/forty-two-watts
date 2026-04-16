package pvmodel

import (
	"context"
	"encoding/json"
	"log/slog"
	"sync"
	"time"

	"github.com/frahlg/forty-two-watts/go/internal/state"
	"github.com/frahlg/forty-two-watts/go/internal/telemetry"
)

// stateKey is the config k/v key where we persist the model JSON.
const stateKey = "pvmodel/state"

// ClearSkyFunc is injected by main.go to decouple pvmodel from the
// forecast package. Returns clear-sky GHI (W/m²) for the site's lat/lon
// baked into the closure.
type ClearSkyFunc func(t time.Time) float64

// CloudFunc returns the cloud-cover percentage (0..100) for a given
// time, based on the latest forecast. Returns (value, ok) where ok is
// false if no forecast covers `t`.
type CloudFunc func(t time.Time) (float64, bool)

// Service owns the online-learning loop for the PV twin. It samples
// measured PV telemetry once per SampleInterval, pulls the matching
// clear-sky + cloud values for that instant, and runs one RLS update.
// The model is persisted to state every PersistEvery samples.
type Service struct {
	Store          *state.Store
	Tele           *telemetry.Store
	ClearSky       ClearSkyFunc
	Cloud          CloudFunc
	SampleInterval time.Duration
	PersistEvery   int64 // samples between SQLite writes

	mu        sync.RWMutex
	model     *Model
	persistMu sync.Mutex // serialises SQLite writes so a stale persist can't clobber a Reset

	stop chan struct{}
	done chan struct{}
}

// NewService constructs the service. If model state exists in the DB,
// it's restored; otherwise a fresh prior is initialized using ratedW.
func NewService(st *state.Store, tel *telemetry.Store, cs ClearSkyFunc, cf CloudFunc, ratedW float64) *Service {
	s := &Service{
		Store:          st,
		Tele:           tel,
		ClearSky:       cs,
		Cloud:          cf,
		SampleInterval: 60 * time.Second,
		PersistEvery:   10,
		stop:           make(chan struct{}),
		done:           make(chan struct{}),
	}
	if st != nil {
		if js, ok := st.LoadConfig(stateKey); ok && js != "" {
			var m Model
			if err := json.Unmarshal([]byte(js), &m); err == nil && m.Forgetting > 0 {
				m.RatedW = ratedW // config may have changed rated value
				s.model = &m
				slog.Info("pvmodel restored", "samples", m.Samples, "mae_w", m.MAE, "quality", m.Quality())
			}
		}
	}
	if s.model == nil {
		s.model = NewModel(ratedW)
	}
	return s
}

// Model returns the current model (safe for reads; copies are cheap).
func (s *Service) Model() Model {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return *s.model
}

// Predict is the main integration point: MPC + UI call this to get the
// twin's prediction for any future instant.
func (s *Service) Predict(t time.Time, cloudPct float64) float64 {
	if s == nil {
		return 0
	}
	cs := s.ClearSky(t)
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.model.Predict(cs, cloudPct, t)
}

// PredictNow returns the twin's prediction for right now using the
// latest cloud cover from the forecast cache. Used by the UI to
// overlay predicted vs actual PV on the live chart.
func (s *Service) PredictNow() float64 {
	if s == nil {
		return 0
	}
	now := time.Now()
	cloud := 50.0
	if s.Cloud != nil {
		if v, ok := s.Cloud(now); ok {
			cloud = v
		}
	}
	return s.Predict(now, cloud)
}

// Start begins the online-learning loop. Safe to call multiple times.
func (s *Service) Start(ctx context.Context) {
	if s == nil {
		return
	}
	go s.loop(ctx)
}

// Stop terminates the learner + flushes a final persist.
func (s *Service) Stop() {
	if s == nil {
		return
	}
	close(s.stop)
	<-s.done
}

func (s *Service) loop(ctx context.Context) {
	defer close(s.done)
	t := time.NewTicker(s.SampleInterval)
	defer t.Stop()
	// Initial sample so we don't wait a full interval.
	s.sample()
	for {
		select {
		case <-s.stop:
			s.persist()
			return
		case <-ctx.Done():
			s.persist()
			return
		case <-t.C:
			s.sample()
		}
	}
}

// sample reads current PV telemetry, pulls current clear-sky + cloud,
// and runs one RLS update.
func (s *Service) sample() {
	now := time.Now()
	cs := s.ClearSky(now)
	if cs < 50 {
		slog.Debug("pvmodel: skip (night)", "cs", cs)
		return // night / near-night — no signal
	}
	cloud := 50.0 // neutral fallback if no forecast row
	if s.Cloud != nil {
		if v, ok := s.Cloud(now); ok {
			cloud = v
		}
	}
	// Aggregate PV across all drivers. PV telemetry is stored as
	// site-sign (negative = generating), so flip to positive.
	var pvW float64
	readings := s.Tele.ReadingsByType(telemetry.DerPV)
	for _, r := range readings {
		if r.SmoothedW < 0 {
			pvW += -r.SmoothedW
		}
	}
	// Guard: if all drivers report 0 when there's meaningful clear-sky,
	// that's likely a driver outage — skip so we don't learn "0 output".
	if pvW < 1 {
		slog.Debug("pvmodel: skip (no PV reading)", "readings", len(readings), "cs", cs)
		return
	}

	s.mu.Lock()
	updated := s.model.Update(cs, cloud, now, pvW)
	samples := s.model.Samples
	mae := s.model.MAE
	s.mu.Unlock()

	slog.Info("pvmodel: sample", "cs_wm2", cs, "cloud_pct", cloud, "pv_w", pvW, "samples", samples, "mae_w", mae, "updated", updated)

	if updated && samples%s.PersistEvery == 0 {
		s.persist()
	}
}

func (s *Service) persist() {
	if s.Store == nil {
		return
	}
	// Serialise the entire marshal+save so a sample-loop persist that
	// started before a Reset cannot finish after Reset's persist and
	// clobber the clean state with stale coefficients.
	s.persistMu.Lock()
	defer s.persistMu.Unlock()
	s.mu.RLock()
	js, err := json.Marshal(s.model)
	s.mu.RUnlock()
	if err != nil {
		return
	}
	if err := s.Store.SaveConfig(stateKey, string(js)); err != nil {
		slog.Warn("pvmodel persist", "err", err)
	}
}

// Reset clears the model to a fresh prior (useful after a system change
// — new panels, cleaning, etc.).
func (s *Service) Reset() {
	if s == nil {
		return
	}
	s.mu.Lock()
	rated := s.model.RatedW
	s.model = NewModel(rated)
	s.mu.Unlock()
	s.persist()
}
