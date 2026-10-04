package main

import (
	"regexp"
	"strconv"
	"time"
)

var (
	localRe = regexp.MustCompile(`^\d{4}-\d{2}-\d{2}T\d{2}:\d{2}$`)
	dateRe  = regexp.MustCompile(`^\d{4}-\d{2}-\d{2}$`)
	hhmmRe  = regexp.MustCompile(`^\d{2}:\d{2}$`)
)

var weekdayNames = []string{"sun", "mon", "tue", "wed", "thu", "fri", "sat"}

const (
	localLayout = "2006-01-02T15:04"
	rfcLayout   = "2006-01-02T15:04:05-07:00"
)

// parseWall parses "YYYY-MM-DDTHH:MM" into its wall-clock components,
// encoded as a UTC time (no zone semantics yet).
func parseWall(s string) (time.Time, bool) {
	if !localRe.MatchString(s) {
		return time.Time{}, false
	}
	t, err := time.Parse(localLayout, s)
	if err != nil {
		return time.Time{}, false
	}
	return t, true
}

// resolveWall converts a wall-clock time (held in a UTC-encoded time) to an
// instant in loc. ok is false when the wall time does not exist (DST gap).
// For ambiguous wall times (DST overlap) the first occurrence is returned.
func resolveWall(loc *time.Location, wall time.Time) (time.Time, bool) {
	var best time.Time
	found := false
	seen := map[int]bool{}
	for _, d := range []time.Duration{-24 * time.Hour, 0, 24 * time.Hour} {
		_, off := wall.Add(d).In(loc).Zone()
		if seen[off] {
			continue
		}
		seen[off] = true
		cand := wall.Add(-time.Duration(off) * time.Second)
		l := cand.In(loc)
		if l.Year() == wall.Year() && l.Month() == wall.Month() && l.Day() == wall.Day() &&
			l.Hour() == wall.Hour() && l.Minute() == wall.Minute() {
			if !found || cand.Before(best) {
				best = cand
				found = true
			}
		}
	}
	return best, found
}

// parseHHMM returns minutes after midnight.
func parseHHMM(s string) (int, bool) {
	if !hhmmRe.MatchString(s) {
		return 0, false
	}
	h, _ := strconv.Atoi(s[:2])
	m, _ := strconv.Atoi(s[3:])
	if h > 23 || m > 59 {
		return 0, false
	}
	return h*60 + m, true
}

func fmtRFC(t time.Time, loc *time.Location) string {
	return t.In(loc).Format(rfcLayout)
}

func fmtLocal(t time.Time, loc *time.Location) string {
	return t.In(loc).Format(localLayout)
}
