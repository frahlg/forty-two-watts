package config

import (
	"fmt"
	"regexp"
	"time"
)

var hourRangeRe = regexp.MustCompile(`^([01]\d|2[0-3]):[0-5]\d-(([01]\d|2[0-3]):[0-5]\d|24:00)$`)

// validateTariff checks the tariff block's shape: names, ranges and
// formats. Full structural semantics (every minute of every day class
// covered exactly once, months partitioned across seasons) are enforced
// by the compiler in go/internal/tariff, which runs at startup — this
// keeps config free of an import cycle while still rejecting the
// obviously-broken shapes at save time.
func (c *Config) validateTariff() error {
	t := c.Tariff
	if t == nil {
		return nil
	}
	if t.Timezone == "" {
		return fmt.Errorf("tariff.timezone is required (e.g. Africa/Johannesburg)")
	}
	if _, err := time.LoadLocation(t.Timezone); err != nil {
		return fmt.Errorf("tariff.timezone: %w", err)
	}
	if t.BillingCycleAnchorDay < 1 || t.BillingCycleAnchorDay > 28 {
		return fmt.Errorf("tariff.billing_cycle_anchor_day must be 1..28")
	}
	if t.DemandChargeCtKVA < 0 {
		return fmt.Errorf("tariff.demand_charge_ct_kva must be >= 0")
	}
	if t.DemandChargeCtKVA > 0 && t.DemandIntegrationMin <= 0 {
		return fmt.Errorf("tariff.demand_integration_min must be > 0 with a demand charge")
	}
	for _, b := range t.DemandWindowBands {
		if !validBandName(b) {
			return fmt.Errorf("tariff.demand_window_bands: unknown band %q", b)
		}
	}
	for _, h := range t.Holidays {
		if _, err := time.Parse("2006-01-02", h); err != nil {
			return fmt.Errorf("tariff.holidays: %q is not YYYY-MM-DD", h)
		}
	}
	if len(t.Seasons) == 0 {
		return fmt.Errorf("tariff.seasons must not be empty")
	}
	for _, s := range t.Seasons {
		if s.Name == "" {
			return fmt.Errorf("tariff season: name is required")
		}
		if len(s.Months) == 0 {
			return fmt.Errorf("tariff season %q: months must not be empty", s.Name)
		}
		for _, m := range s.Months {
			if m < 1 || m > 12 {
				return fmt.Errorf("tariff season %q: month %d out of range", s.Name, m)
			}
		}
		if len(s.Bands) == 0 {
			return fmt.Errorf("tariff season %q: bands must not be empty", s.Name)
		}
		for dc, bands := range s.Bands {
			switch dc {
			case "weekday", "saturday", "sunday":
			default:
				return fmt.Errorf("tariff season %q: unknown day class %q (weekday|saturday|sunday)", s.Name, dc)
			}
			for _, b := range bands {
				if !validBandName(b.Band) {
					return fmt.Errorf("tariff season %q %s: unknown band %q (peak|standard|offpeak)", s.Name, dc, b.Band)
				}
				if b.RateCtKWh < 0 {
					return fmt.Errorf("tariff season %q %s band %q: rate_ct_kwh must be >= 0", s.Name, dc, b.Band)
				}
				if len(b.Hours) == 0 {
					return fmt.Errorf("tariff season %q %s band %q: hours must not be empty", s.Name, dc, b.Band)
				}
				for _, h := range b.Hours {
					if !hourRangeRe.MatchString(h) {
						return fmt.Errorf("tariff season %q %s band %q: %q is not HH:MM-HH:MM", s.Name, dc, b.Band, h)
					}
				}
			}
		}
	}
	return nil
}

func validBandName(b string) bool {
	switch b {
	case "peak", "standard", "offpeak":
		return true
	}
	return false
}
