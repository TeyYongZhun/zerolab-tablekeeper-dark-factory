package main

import (
	"encoding/json"
	"time"
)

// terms is the set of booking rules in force for a date: policy 0 is the
// restaurant's base configuration, later versions come from published policies.
type terms struct {
	Version    int            `json:"policy_version"`
	Slot       int            `json:"slot_minutes"`
	Duration   int            `json:"reservation_duration_minutes"`
	Cutoff     int            `json:"cancellation_cutoff_minutes"`
	Hours      []openHours    `json:"opening_hours"`
	Capacities map[string]int `json:"capacities"`
}

// policy is a published, versioned set of terms with an effective date.
type policy struct {
	terms
	EffectiveFrom string `json:"effective_from"`
}

func (r *restaurant) baseTerms() *terms {
	caps := map[string]int{}
	for _, t := range r.Tables {
		caps[t.ID] = t.Capacity
	}
	hours := append([]openHours{}, r.Hours...)
	return &terms{Version: 0, Slot: r.Slot, Duration: r.Duration, Cutoff: r.Cutoff, Hours: hours, Capacities: caps}
}

// termsFor selects the policy for a local date (YYYY-MM-DD): the greatest
// effective_from not after the date, ties going to the greatest version.
func (r *restaurant) termsFor(date string) *terms {
	var best *policy
	for i := range r.Policies {
		p := &r.Policies[i]
		if p.EffectiveFrom > date {
			continue
		}
		if best == nil || p.EffectiveFrom > best.EffectiveFrom ||
			(p.EffectiveFrom == best.EffectiveFrom && p.Version > best.Version) {
			best = p
		}
	}
	if best == nil {
		return r.baseTerms()
	}
	t := best.terms
	return &t
}

func (t *terms) capacity(id string) int { return t.Capacities[id] }

func (t *terms) duration() time.Duration { return time.Duration(t.Duration) * time.Minute }

// window returns the opening window for a local date in minutes after midnight.
func (t *terms) window(wall time.Time) (int, int, bool) {
	wd := weekdayNames[wall.Weekday()]
	for _, h := range t.Hours {
		if h.Weekday == wd {
			o, _ := parseHHMM(h.Opens)
			c, _ := parseHHMM(h.Closes)
			return o, c, true
		}
	}
	return 0, 0, false
}

// closesAt returns the instant of the closing time on the date of wall.
func (t *terms) closesAt(loc *time.Location, wall time.Time, closeMin int) time.Time {
	cw := time.Date(wall.Year(), wall.Month(), wall.Day(), closeMin/60, closeMin%60, 0, 0, time.UTC)
	if x, ok := resolveWall(loc, cw); ok {
		return x
	}
	return time.Date(wall.Year(), wall.Month(), wall.Day(), closeMin/60, closeMin%60, 0, 0, loc)
}

func (t *terms) json() string {
	b, _ := json.Marshal(t)
	return string(b)
}

func parseTerms(s string) *terms {
	t := &terms{}
	json.Unmarshal([]byte(s), t)
	if t.Hours == nil {
		t.Hours = []openHours{}
	}
	if t.Capacities == nil {
		t.Capacities = map[string]int{}
	}
	return t
}
