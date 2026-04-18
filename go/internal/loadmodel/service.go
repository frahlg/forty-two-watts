package loadmodel

import (
	"context"
	"encoding/json"
	"log/slog"
	"sync"
	"time"

	"github.com/frahlg/forty-two-watts/go/internal/state"
	"github.com/frahlg/forty-two-watts/go/internal/telemetry"
)

// TempFunc returns outdoor temperature (°C) for a given time, (value, ok).
// Same shape as pvmodel.CloudFunc — injected by main.go to decouple this
// package from the forecast module.
type TempFunc func(t time.Time) (float64, bool)

// Bumped from "loadmodel/state" after HourOfWeek switched to UTC
// coercion: pre-switch buckets were indexed in local zone and would
// silently misalign if restored. Fresh init retrains from telemetry.
const stateKey = "loadmodel/state_utc"

// Service trains the load model online from telemetry. Mirrors
// pvmodel.Service so operators + future code have one pattern.
type Service struct {
	Store          *state.Store
	Tele           *telemetry.Store
	SiteMeter      string   // driver name that carries the site's grid meter
	Temp           TempFunc // optional outdoor-temp source (forecast)
	SampleInterval time.Duration
	PersistEvery   int64

	mu    sync.RWMutex
	model *Model

	stop chan struct{}
	done chan struct{}
}

// NewService constructs + restores from state if present.
func NewService(st *state.Store, tel *telemetry.Store, siteMeter string, peakW float64) *Service {
	s := &Service{
		Store:          st,
		Tele:           tel,
		SiteMeter:      siteMeter,
		SampleInterval: 60 * time.Second,
		PersistEvery:   10,
		stop:           make(chan struct{}),
		done:           make(chan struct{}),
	}
	if st != nil {
		if js, ok := st.LoadConfig(stateKey); ok && js != "" {
			var m Model
			if err := json.Unmarshal([]byte(js), &m); err == nil && m.Alpha > 0 {
				m.PeakW = peakW // config may have changed
				s.model = &m
				slog.Info("loadmodel restored", "samples", m.Samples, "mae_w", m.MAE, "quality", m.Quality())
			}
		}
	}
	if s.model == nil {
		s.model = NewModel(peakW)
	}
	return s
}

// Model returns a snapshot.
func (s *Service) Model() Model {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return *s.model
}

// Predict is the MPC's integration point — expected load at time t.
// If a temperature source is wired, the heating-gain correction is
// included; otherwise we predict assuming indoor setpoint (no heating).
func (s *Service) Predict(t time.Time) float64 {
	if s == nil {
		return 0
	}
	temp := HeatingReferenceC
	if s.Temp != nil {
		if v, ok := s.Temp(t); ok {
			temp = v
		}
	}
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.model.Predict(t, temp)
}

// Start kicks off the online-learning goroutine.
func (s *Service) Start(ctx context.Context) {
	if s == nil {
		return
	}
	go s.loop(ctx)
}

// Stop terminates the learner + persists once.
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

// sample computes measured house load = grid_w − pv_w − bat_w − ev_w
// and feeds it to the model. Skips when drivers haven't settled yet
// (no site meter reading). EV is subtracted so the weekly-pattern
// learner tracks house consumption, not "house + occasional 10 kWh
// car session" — otherwise every Monday-evening bucket inflates when
// the driver happens to plug in after work.
func (s *Service) sample() {
	now := time.Now()
	meter := s.Tele.Get(s.SiteMeter, telemetry.DerMeter)
	if meter == nil {
		slog.Debug("loadmodel: skip (no site meter yet)")
		return
	}
	gridW := meter.SmoothedW
	var pvW, batW float64
	for _, r := range s.Tele.ReadingsByType(telemetry.DerPV) {
		pvW += r.SmoothedW // site-sign: negative = generating
	}
	for _, r := range s.Tele.ReadingsByType(telemetry.DerBattery) {
		batW += r.SmoothedW // site-sign: positive = charging
	}
	evW := s.Tele.SumOnlineEVW() // online-only so stale readings don't poison load
	loadW := gridW - pvW - batW - evW
	if loadW < 0 {
		// Almost always a transient — during a PI step the measured
		// flow can briefly appear negative. Skip rather than train
		// on a physically impossible value.
		slog.Debug("loadmodel: skip (neg load)", "grid_w", gridW, "pv_w", pvW, "bat_w", batW, "ev_w", evW)
		return
	}

	// Outdoor temp for heating-fit. HeatingReferenceC = "no contribution".
	temp := HeatingReferenceC
	if s.Temp != nil {
		if v, ok := s.Temp(now); ok {
			temp = v
		}
	}

	s.mu.Lock()
	updated := s.model.Update(now, loadW, temp)
	samples := s.model.Samples
	mae := s.model.MAE
	heating := s.model.HeatingW_per_degC
	s.mu.Unlock()

	slog.Info("loadmodel: sample", "load_w", loadW, "temp_c", temp, "samples", samples, "mae_w", mae, "heat_w_per_c", heating, "updated", updated)

	if updated && samples%s.PersistEvery == 0 {
		s.persist()
	}
}

func (s *Service) persist() {
	if s.Store == nil {
		return
	}
	s.mu.RLock()
	js, err := json.Marshal(s.model)
	s.mu.RUnlock()
	if err != nil {
		return
	}
	if err := s.Store.SaveConfig(stateKey, string(js)); err != nil {
		slog.Warn("loadmodel persist", "err", err)
	}
}

// SetHeatingCoef lets the operator declare heating-load sensitivity
// from config. Units: W per °C below 18°C. 0 disables.
func (s *Service) SetHeatingCoef(w float64) {
	if s == nil {
		return
	}
	s.mu.Lock()
	s.model.HeatingW_per_degC = w
	s.mu.Unlock()
}

// Reset clears the model (e.g. after a big appliance change).
func (s *Service) Reset() {
	if s == nil {
		return
	}
	s.mu.Lock()
	peak := s.model.PeakW
	s.model = NewModel(peak)
	s.mu.Unlock()
	s.persist()
}
