package tariff

import (
	"fmt"
	"strconv"
	"strings"
	"time"

	"github.com/srcfl/ftw/go/internal/config"
)

// Compile turns the operator's tariff config into a validated Schedule.
// config.Validate() has already checked shapes and formats; Compile owns
// the structural semantics (full-day coverage, month partitioning) via
// Schedule.Validate.
func Compile(c *config.Tariff) (*Schedule, error) {
	if c == nil {
		return nil, nil
	}
	loc, err := time.LoadLocation(c.Timezone)
	if err != nil {
		return nil, fmt.Errorf("tariff timezone: %w", err)
	}
	s := &Schedule{
		Location:                 loc,
		Holidays:                 map[civilDate]bool{},
		DemandChargeCtKVA:        c.DemandChargeCtKVA,
		DemandWindowBands:        map[Band]bool{},
		DemandIntegrationMinutes: c.DemandIntegrationMin,
		BillingAnchorDay:         c.BillingCycleAnchorDay,
	}
	for _, b := range c.DemandWindowBands {
		s.DemandWindowBands[Band(b)] = true
	}
	for _, h := range c.Holidays {
		t, err := time.ParseInLocation("2006-01-02", h, loc)
		if err != nil {
			return nil, fmt.Errorf("holiday %q: %w", h, err)
		}
		s.Holidays[dateOf(t)] = true
	}
	for _, cs := range c.Seasons {
		season := Season{
			Name:   cs.Name,
			Months: map[time.Month]bool{},
			Bands:  map[DayClass][]BandRate{},
		}
		for _, m := range cs.Months {
			season.Months[time.Month(m)] = true
		}
		for dc, bands := range cs.Bands {
			var rates []BandRate
			for _, b := range bands {
				windows, err := parseHourRanges(b.Hours)
				if err != nil {
					return nil, fmt.Errorf("season %q %s band %q: %w", cs.Name, dc, b.Band, err)
				}
				rates = append(rates, BandRate{
					Band:      Band(b.Band),
					Windows:   windows,
					RateCtKWh: b.RateCtKWh,
				})
			}
			season.Bands[DayClass(dc)] = rates
		}
		s.Seasons = append(s.Seasons, season)
	}
	if err := s.Validate(); err != nil {
		return nil, err
	}
	return s, nil
}

// parseHourRanges converts "HH:MM-HH:MM" strings into windows, splitting
// ranges that cross midnight ("22:00-06:00" → 22:00-24:00 + 00:00-06:00).
// An end of "00:00" or "24:00" means end-of-day.
func parseHourRanges(hours []string) ([]Window, error) {
	var out []Window
	for _, h := range hours {
		parts := strings.SplitN(h, "-", 2)
		if len(parts) != 2 {
			return nil, fmt.Errorf("%q is not HH:MM-HH:MM", h)
		}
		start, err := parseMinOfDay(parts[0])
		if err != nil {
			return nil, fmt.Errorf("%q: %w", h, err)
		}
		end, err := parseMinOfDay(parts[1])
		if err != nil {
			return nil, fmt.Errorf("%q: %w", h, err)
		}
		if end == 0 {
			end = 24 * 60
		}
		if start == end {
			return nil, fmt.Errorf("%q is empty", h)
		}
		if start < end {
			out = append(out, Window{StartMin: start, EndMin: end})
		} else {
			out = append(out,
				Window{StartMin: start, EndMin: 24 * 60},
				Window{StartMin: 0, EndMin: end})
		}
	}
	return out, nil
}

func parseMinOfDay(s string) (int, error) {
	parts := strings.SplitN(strings.TrimSpace(s), ":", 2)
	if len(parts) != 2 {
		return 0, fmt.Errorf("%q is not HH:MM", s)
	}
	hh, err := strconv.Atoi(parts[0])
	if err != nil || hh < 0 || hh > 24 {
		return 0, fmt.Errorf("bad hour in %q", s)
	}
	mm, err := strconv.Atoi(parts[1])
	if err != nil || mm < 0 || mm > 59 {
		return 0, fmt.Errorf("bad minute in %q", s)
	}
	min := hh*60 + mm
	if min > 24*60 {
		return 0, fmt.Errorf("%q is past 24:00", s)
	}
	return min, nil
}
