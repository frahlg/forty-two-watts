package calendar

import (
	"regexp"
	"strconv"
	"strings"
	"time"
)

// Interval is a half-open [Start, End) "away"/vacation window derived from a
// calendar event.
type Interval struct {
	Start time.Time `json:"start"`
	End   time.Time `json:"end"`
	UID   string    `json:"uid,omitempty"`
	Title string    `json:"title,omitempty"`
}

// Contains reports whether t falls inside [Start, End).
func (iv Interval) Contains(t time.Time) bool {
	return !t.Before(iv.Start) && t.Before(iv.End)
}

// EVDeadline is "loadpoint LoadpointID must reach TargetSoC (0–1) by Departure",
// derived from a calendar event whose start time is the departure. Titles
// still write "80%"; that percent is converted here.
type EVDeadline struct {
	LoadpointID string    `json:"loadpoint_id,omitempty"`
	TargetSoC   float64   `json:"target_soc"`
	Departure   time.Time `json:"departure"`
	UID         string    `json:"uid,omitempty"`
	Title       string    `json:"title,omitempty"`
}

// Intents is the parsed result of one calendar fetch.
type Intents struct {
	Away []Interval   `json:"away"`
	EV   []EVDeadline `json:"ev"`
}

// pctRe extracts a target percentage like "80%" or "80 %" from an event title.
var pctRe = regexp.MustCompile(`(\d{1,3})\s*%`)

// lpRe extracts an explicit loadpoint selector like "lp:garage" or "lp=garage".
var lpRe = regexp.MustCompile(`(?i)\blp[:=]\s*([A-Za-z0-9_-]+)`)

// parser classifies event titles into intents. It is deliberately free of any
// config / network dependency so the title→intent rules are unit-testable in
// isolation. Keywords are stored already lower-cased.
type parser struct {
	awayKeywords       []string
	evKeywords         []string
	defaultLoadpointID string
	defaultTargetSoC   float64
}

func newParser(awayKeywords, evKeywords []string, defaultLoadpointID string, defaultTargetSoC float64) *parser {
	return &parser{
		awayKeywords:       lowerAll(awayKeywords),
		evKeywords:         lowerAll(evKeywords),
		defaultLoadpointID: defaultLoadpointID,
		defaultTargetSoC:   defaultTargetSoC,
	}
}

// classify maps a single event to at most one intent. EV is checked before
// away because EV titles are the more specific case (they carry a target %).
// A non-matching title yields (nil, nil) and is ignored.
// maxTitleLen bounds how much of an event title we inspect — a hostile server
// could otherwise ship a multi-megabyte SUMMARY to burn CPU on a Pi.
const maxTitleLen = 4096

func (p *parser) classify(title string, start, end time.Time, uid string) (*Interval, *EVDeadline) {
	if len(title) > maxTitleLen {
		title = title[:maxTitleLen]
	}
	lt := strings.ToLower(strings.TrimSpace(title))
	if lt == "" || start.IsZero() {
		return nil, nil
	}

	if matchesAny(lt, p.evKeywords) {
		soc := p.defaultTargetSoC
		if m := pctRe.FindStringSubmatch(title); m != nil {
			if v, err := strconv.ParseFloat(m[1], 64); err == nil {
				soc = titlePercentToFraction(v)
			}
		}
		lp := p.defaultLoadpointID
		if m := lpRe.FindStringSubmatch(title); m != nil {
			lp = m[1]
		}
		return nil, &EVDeadline{
			LoadpointID: lp,
			TargetSoC:   soc,
			Departure:   start,
			UID:         uid,
			Title:       title,
		}
	}

	if matchesAny(lt, p.awayKeywords) {
		e := end
		// All-day or DTEND-less events: assume a one-day window so a bare
		// "Away" still suppresses load for a sensible span rather than zero.
		if !e.After(start) {
			e = start.Add(24 * time.Hour)
		}
		return &Interval{Start: start, End: e, UID: uid, Title: title}, nil
	}

	return nil, nil
}

func matchesAny(lowerTitle string, keywords []string) bool {
	for _, k := range keywords {
		if k != "" && strings.Contains(lowerTitle, k) {
			return true
		}
	}
	return false
}

func lowerAll(in []string) []string {
	out := make([]string, 0, len(in))
	for _, s := range in {
		s = strings.ToLower(strings.TrimSpace(s))
		if s != "" {
			out = append(out, s)
		}
	}
	return out
}

func titlePercentToFraction(v float64) float64 {
	if v < 0 {
		return 0
	}
	if v > 100 {
		v = 100
	}
	return v / 100.0
}
