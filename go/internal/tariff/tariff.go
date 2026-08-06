// Package tariff models scheduled (non-spot) retail tariffs: time-of-use
// energy rates by season/day-class/band plus a demand charge on the
// billing-cycle peak. It is pure — no persistence, no clock reads; every
// query takes an explicit time.Time so tests and the MPC request builder
// stay deterministic.
//
// The first target is South African C&I supply (Eskom Megaflex-style and
// municipal variants), but nothing here is Eskom-specific: schedules are
// entirely data-driven from config.
//
// Money follows the repo convention: minor currency units per kWh
// (historically named "öre" in the MPC contract; ZAR cents behave
// identically). Power is W, energy Wh, per docs/site-convention.md.
package tariff

import (
	"fmt"
	"sort"
	"time"
)

// Band is a time-of-use price band.
type Band string

const (
	BandPeak     Band = "peak"
	BandStandard Band = "standard"
	BandOffPeak  Band = "offpeak"
)

// DayClass partitions days within a season. Public holidays resolve to
// the Sunday class by convention (matching Eskom's treatment of most
// holidays); the schedule's Holidays list drives that mapping.
type DayClass string

const (
	DayWeekday  DayClass = "weekday"
	DaySaturday DayClass = "saturday"
	DaySunday   DayClass = "sunday"
)

// Window is one contiguous [Start, End) span of minutes-since-midnight.
// End may be 24*60. Windows never wrap midnight: a config range like
// "22:00-06:00" is split into two Windows at parse time.
type Window struct {
	StartMin int
	EndMin   int
}

func (w Window) contains(minOfDay int) bool {
	return minOfDay >= w.StartMin && minOfDay < w.EndMin
}

// BandRate is a band's windows and energy rate for one day class.
type BandRate struct {
	Band      Band
	Windows   []Window
	RateCtKWh float64 // energy rate, minor currency units per kWh
}

// Season is a set of months with per-day-class band tables.
type Season struct {
	Name   string
	Months map[time.Month]bool
	// Bands per day class. Every minute of the day must be covered by
	// exactly one band's window set (validated by Schedule.Validate).
	Bands map[DayClass][]BandRate
}

// Schedule is a complete scheduled tariff.
type Schedule struct {
	// Timezone the windows/seasons are defined in (e.g. Africa/Johannesburg).
	Location *time.Location
	Seasons  []Season
	// Holidays are dates (in Location) treated as the Sunday day class.
	Holidays map[civilDate]bool

	// DemandChargeCtKVA is the monthly demand charge in minor currency
	// units per kVA of billing peak. 0 = no demand charge.
	DemandChargeCtKVA float64
	// DemandWindowBands lists the bands whose intervals count toward the
	// billing peak (typically peak+standard for Megaflex-style tariffs).
	// Empty with a non-zero demand charge = all bands count.
	DemandWindowBands map[Band]bool
	// DemandIntegrationMinutes is the utility's demand-integration
	// window (30 for SA utilities).
	DemandIntegrationMinutes int
	// BillingAnchorDay is the day-of-month the billing cycle starts (1-28).
	BillingAnchorDay int
}

type civilDate struct {
	Y int
	M time.Month
	D int
}

func dateOf(t time.Time) civilDate {
	y, m, d := t.Date()
	return civilDate{y, m, d}
}

// Resolved is the tariff state at one instant.
type Resolved struct {
	Season       string
	DayClass     DayClass
	Band         Band
	RateCtKWh    float64
	DemandActive bool // true when this instant's band counts toward the billing peak
}

// Resolve returns the band, energy rate and demand-window membership at t.
func (s *Schedule) Resolve(t time.Time) (Resolved, error) {
	lt := t.In(s.Location)
	season := s.seasonFor(lt.Month())
	if season == nil {
		return Resolved{}, fmt.Errorf("no season covers month %s", lt.Month())
	}
	dc := s.dayClassFor(lt)
	rates, ok := season.Bands[dc]
	if !ok {
		return Resolved{}, fmt.Errorf("season %q has no band table for day class %q", season.Name, dc)
	}
	minOfDay := lt.Hour()*60 + lt.Minute()
	for _, br := range rates {
		for _, w := range br.Windows {
			if w.contains(minOfDay) {
				return Resolved{
					Season:       season.Name,
					DayClass:     dc,
					Band:         br.Band,
					RateCtKWh:    br.RateCtKWh,
					DemandActive: s.demandActive(br.Band),
				}, nil
			}
		}
	}
	return Resolved{}, fmt.Errorf("season %q day class %q: no band covers minute %d", season.Name, dc, minOfDay)
}

func (s *Schedule) demandActive(b Band) bool {
	if s.DemandChargeCtKVA <= 0 {
		return false
	}
	if len(s.DemandWindowBands) == 0 {
		return true
	}
	return s.DemandWindowBands[b]
}

func (s *Schedule) seasonFor(m time.Month) *Season {
	for i := range s.Seasons {
		if s.Seasons[i].Months[m] {
			return &s.Seasons[i]
		}
	}
	return nil
}

func (s *Schedule) dayClassFor(lt time.Time) DayClass {
	if s.Holidays[dateOf(lt)] {
		return DaySunday
	}
	switch lt.Weekday() {
	case time.Saturday:
		return DaySaturday
	case time.Sunday:
		return DaySunday
	default:
		return DayWeekday
	}
}

// SlotPrice is one planner slot's tariff view.
type SlotPrice struct {
	Start        time.Time
	Len          time.Duration
	RateCtKWh    float64
	Band         Band
	DemandActive bool
}

// SlotPrices renders the schedule as planner slots covering [from, until).
// Slots are aligned to slotLen boundaries of `from` (the caller aligns
// `from` itself). A slot that straddles a band boundary takes the
// time-weighted average rate and is demand-active if ANY covered minute
// is — conservative for peak avoidance.
func (s *Schedule) SlotPrices(from, until time.Time, slotLen time.Duration) ([]SlotPrice, error) {
	if slotLen < time.Minute {
		return nil, fmt.Errorf("slot length %v is below one minute", slotLen)
	}
	var out []SlotPrice
	for start := from; start.Before(until); start = start.Add(slotLen) {
		end := start.Add(slotLen)
		if end.After(until) {
			end = until
		}
		var weighted float64
		var demand bool
		var band Band
		var bandMinutes int
		total := 0
		for cur := start; cur.Before(end); cur = cur.Add(time.Minute) {
			r, err := s.Resolve(cur)
			if err != nil {
				return nil, err
			}
			weighted += r.RateCtKWh
			total++
			if r.DemandActive {
				demand = true
			}
			// Report the band that covers the majority of the slot.
			if band == "" || r.Band == band {
				bandMinutes++
			} else if bandMinutes < total-bandMinutes {
				band = r.Band
				bandMinutes = total - bandMinutes
			}
			if band == "" {
				band = r.Band
			}
		}
		if total == 0 {
			continue
		}
		out = append(out, SlotPrice{
			Start:        start,
			Len:          end.Sub(start),
			RateCtKWh:    weighted / float64(total),
			Band:         band,
			DemandActive: demand,
		})
	}
	return out, nil
}

// Validate checks structural completeness: every month maps to exactly
// one season, and within each season every day class covers all 1440
// minutes with no gaps or overlaps.
func (s *Schedule) Validate() error {
	if s.Location == nil {
		return fmt.Errorf("timezone is required")
	}
	if s.BillingAnchorDay < 1 || s.BillingAnchorDay > 28 {
		return fmt.Errorf("billing anchor day must be 1..28, got %d", s.BillingAnchorDay)
	}
	if s.DemandChargeCtKVA > 0 && s.DemandIntegrationMinutes <= 0 {
		return fmt.Errorf("demand charge requires a demand integration window")
	}
	for m := time.January; m <= time.December; m++ {
		n := 0
		for i := range s.Seasons {
			if s.Seasons[i].Months[m] {
				n++
			}
		}
		if n != 1 {
			return fmt.Errorf("month %s is covered by %d seasons, want exactly 1", m, n)
		}
	}
	for _, season := range s.Seasons {
		for _, dc := range []DayClass{DayWeekday, DaySaturday, DaySunday} {
			rates, ok := season.Bands[dc]
			if !ok {
				return fmt.Errorf("season %q: missing band table for %q", season.Name, dc)
			}
			if err := validateFullCoverage(season.Name, dc, rates); err != nil {
				return err
			}
		}
	}
	return nil
}

func validateFullCoverage(season string, dc DayClass, rates []BandRate) error {
	type span struct{ start, end int }
	var spans []span
	for _, br := range rates {
		if br.RateCtKWh < 0 {
			return fmt.Errorf("season %q %s band %q: negative rate", season, dc, br.Band)
		}
		for _, w := range br.Windows {
			if w.StartMin < 0 || w.EndMin > 24*60 || w.StartMin >= w.EndMin {
				return fmt.Errorf("season %q %s band %q: invalid window %d-%d", season, dc, br.Band, w.StartMin, w.EndMin)
			}
			spans = append(spans, span{w.StartMin, w.EndMin})
		}
	}
	sort.Slice(spans, func(i, j int) bool { return spans[i].start < spans[j].start })
	cursor := 0
	for _, sp := range spans {
		if sp.start > cursor {
			return fmt.Errorf("season %q %s: minutes %d-%d uncovered", season, dc, cursor, sp.start)
		}
		if sp.start < cursor {
			return fmt.Errorf("season %q %s: windows overlap at minute %d", season, dc, sp.start)
		}
		cursor = sp.end
	}
	if cursor != 24*60 {
		return fmt.Errorf("season %q %s: minutes %d-1440 uncovered", season, dc, cursor)
	}
	return nil
}

// BillingCycleStart returns the start (midnight, schedule timezone) of
// the billing cycle containing t.
func (s *Schedule) BillingCycleStart(t time.Time) time.Time {
	lt := t.In(s.Location)
	y, m, _ := lt.Date()
	anchor := time.Date(y, m, s.BillingAnchorDay, 0, 0, 0, 0, s.Location)
	if lt.Before(anchor) {
		anchor = anchor.AddDate(0, -1, 0)
	}
	return anchor
}
