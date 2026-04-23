package loadpoint

import "time"

// auto_charge.go owns the plug-in auto-schedule path: when the
// charger transitions to connected and no operator-set target
// exists, the manager posts a default schedule (configured per
// loadpoint, optionally clamped to the bound vehicle's
// charge_limit_soc). Helpers here also translate operator-friendly
// "HH:MM in local time" strings into the next concrete deadline.

// applyAutoSchedule posts a target on the loadpoint matching the
// configured plug-in defaults. When a vehicle driver is bound and
// reports a charge_limit_soc, the SoC target clamps to min(config,
// limit) so the plan never aims past what the car will accept.
// Timezone is the process's local time (docker container's TZ
// env, typically the operator's site locale).
func (m *Manager) applyAutoSchedule(lpID string, defaultSoc float64, timeLocal string, vehicleLimit float64) {
	// Target SoC: fallback to 80 when unconfigured or zero.
	soc := defaultSoc
	if soc <= 0 {
		soc = 80
	}
	// If the vehicle advertises a tighter limit, respect it.
	if vehicleLimit > 0 && vehicleLimit < soc {
		soc = vehicleLimit
	}
	// Deadline: next occurrence of HH:MM (default 06:00) in the host
	// timezone. A morning-commute default matches the typical "plug
	// in overnight, leave at 7" use case.
	deadline := nextLocalTimeOfDay(timeLocal, "06:00", time.Now())

	// SetTarget uses the currently-persisted policy by passing nil
	// (preserves operator's allow_grid / only_surplus / etc. choice).
	// Auto-schedule doesn't assume a specific energy-source posture
	// — if the operator previously POSTed only_surplus:true, the
	// auto plan respects it and may simply sit on 0 W all night.
	m.SetTarget(lpID, soc, deadline, nil)
}

// nextLocalTimeOfDay parses "HH:MM" and returns the next time that
// hour+minute occurs in the process's local timezone relative to
// `now`. When the parse fails, the fallback string is used; if that
// ALSO fails, returns now+8h as a last resort.
func nextLocalTimeOfDay(hhmm, fallback string, now time.Time) time.Time {
	parse := func(s string) (h, mi int, ok bool) {
		if len(s) < 4 || len(s) > 5 {
			return 0, 0, false
		}
		// Accept "HH:MM" or "H:MM".
		parts := splitHHMM(s)
		if parts == nil {
			return 0, 0, false
		}
		h = parts[0]
		mi = parts[1]
		if h < 0 || h > 23 || mi < 0 || mi > 59 {
			return 0, 0, false
		}
		return h, mi, true
	}
	h, mi, ok := parse(hhmm)
	if !ok {
		h, mi, ok = parse(fallback)
	}
	if !ok {
		return now.Add(8 * time.Hour)
	}
	loc := now.Location()
	candidate := time.Date(now.Year(), now.Month(), now.Day(), h, mi, 0, 0, loc)
	if !candidate.After(now) {
		candidate = candidate.Add(24 * time.Hour)
	}
	return candidate
}

// splitHHMM is a manual "HH:MM" → [h, mi] split that avoids pulling
// fmt.Sscanf into the auto-charge hot path. Accepts "HH:MM" and
// "H:MM" — any single colon separating two non-negative ints. nil
// on parse failure.
func splitHHMM(s string) []int {
	for i, r := range s {
		if r == ':' {
			h := atoiPositive(s[:i])
			mi := atoiPositive(s[i+1:])
			if h < 0 || mi < 0 {
				return nil
			}
			return []int{h, mi}
		}
	}
	return nil
}

// atoiPositive parses a non-negative integer string. -1 on failure.
// Saves a strconv.Atoi import for the one place we need it.
func atoiPositive(s string) int {
	if s == "" {
		return -1
	}
	n := 0
	for _, r := range s {
		if r < '0' || r > '9' {
			return -1
		}
		n = n*10 + int(r-'0')
	}
	return n
}
