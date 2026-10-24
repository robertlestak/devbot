package agent

import (
	"regexp"
	"strconv"
	"strings"
	"time"

	// A reset names its zone ("resets 6pm (Europe/Berlin)"); the agent image
	// mustn't need tzdata installed to read it.
	_ "time/tzdata"
)

// clockPattern is a reset time as the CLIs print it, in local time or the
// named zone: "6pm (UTC)", "6:30pm (America/Los_Angeles)", "Oct 20, 9am",
// "Mon 9am", "Monday at 9am", "Oct 18th, 2026 3:45 PM", "18:00".
const clockPattern = `(?:(mon|tue|wed|thu|fri|sat|sun)[a-z]*,? (?:at )?)?` +
	`(?:([a-z]{3})[a-z]*\.? (\d{1,2})(?:st|nd|rd|th)?,?(?: (\d{4}),?)? (?:at )?)?` +
	`(\d{1,2})(?::(\d{2}))? ?([ap]\.?m\.?)?(?: ?\(([^)]+)\))?`

// ClockRE matches prefix (a regexp, "resets?" say) followed by a reset
// time, for ClockTime.
func ClockRE(prefix string) *regexp.Regexp {
	return regexp.MustCompile(`(?i)` + prefix + ` ` + clockPattern)
}

// ClockTime is the next moment after now that the reset time a ClockRE
// matched names, or zero if it names none: a time without a date already
// past today is tomorrow's, a date without a year already past is next
// year's.
func ClockTime(m []string, now time.Time) time.Time {
	weekday, month, day, year, hour, minute, ampm, zone := m[1], m[2], m[3], m[4], m[5], m[6], m[7], m[8]
	if minute == "" && ampm == "" {
		return time.Time{} // a bare number isn't a time
	}
	h, _ := strconv.Atoi(hour)
	mi, _ := strconv.Atoi(minute)
	if ampm != "" {
		if h < 1 || h > 12 {
			return time.Time{}
		}
		h %= 12
		if strings.HasPrefix(strings.ToLower(ampm), "p") {
			h += 12
		}
	}
	if h > 23 || mi > 59 {
		return time.Time{}
	}
	loc := time.Local
	if zone != "" {
		var err error
		if loc, err = time.LoadLocation(strings.TrimSpace(zone)); err != nil {
			return time.Time{}
		}
	}
	now = now.In(loc)
	if month != "" {
		mo := strings.Index("janfebmaraprmayjunjulaugsepoctnovdec", strings.ToLower(month))
		d, _ := strconv.Atoi(day)
		if mo < 0 || mo%3 != 0 || d < 1 || d > 31 {
			return time.Time{}
		}
		y := now.Year()
		if year != "" {
			y, _ = strconv.Atoi(year)
		}
		t := time.Date(y, time.Month(mo/3+1), d, h, mi, 0, 0, loc)
		if year == "" && t.Before(now) {
			t = t.AddDate(1, 0, 0)
		}
		return t
	}
	t := time.Date(now.Year(), now.Month(), now.Day(), h, mi, 0, 0, loc)
	if !t.After(now) {
		t = t.AddDate(0, 0, 1)
	}
	if weekday != "" {
		for !strings.EqualFold(t.Weekday().String()[:3], weekday) {
			t = t.AddDate(0, 0, 1)
		}
	}
	return t
}
