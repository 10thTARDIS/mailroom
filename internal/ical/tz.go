// SPDX-License-Identifier: AGPL-3.0-only

package ical

import (
	"strconv"
	"strings"
	"time"

	// The zone database is compiled in so TZID resolution does not depend on
	// the container shipping /usr/share/zoneinfo (internal/notify does the same).
	_ "time/tzdata"
)

// maxTZIDLen bounds a TZID this package will look at. Real ones are short
// ("America/New_York", "W. Europe Standard Time", the Mozilla-prefixed
// "/mozilla.org/20050126_1/Europe/London"); a longer one is not a zone name.
const maxTZIDLen = 128

// resolveTime turns a DTSTART/DTEND-shaped property into a Time.
//
// The zone is resolved in this order, and NOTHING is ever fetched:
//
//  1. a Z suffix: UTC;
//  2. a TZID that names an IANA zone — directly, or as the tail of a
//     Mozilla/Citadel-style prefixed path;
//  3. the calendar's own VTIMEZONE with that TZID, evaluated from its
//     STANDARD/DAYLIGHT observances (this is what makes Outlook's Windows zone
//     names — "Pacific Standard Time" — work without a mapping table);
//  4. otherwise the time is FLOATING, and the TZID is kept (cleaned) so a client
//     can say which zone the time was written in rather than guess.
//
// A malformed or hostile TZID — too long, control characters, path traversal —
// falls through to 4 with no name, which is the honest answer: the time as
// written, zone unknown.
func resolveTime(p *Property, cal *Component) (Time, bool) {
	if p == nil {
		return Time{}, false
	}
	wall, isDate, isUTC, err := parseDateOrDateTime(p.Value, p.Param("VALUE"))
	if err != nil {
		return Time{}, false
	}
	if isDate {
		return Time{Kind: TimeDate, Date: wall.Format("2006-01-02")}, true
	}
	if isUTC {
		return Time{Kind: TimeUTC, UTC: wall.UTC()}, true
	}
	tzid := strings.TrimSpace(p.Param("TZID"))
	if tzid == "" {
		return Time{Kind: TimeFloating, Local: wall.Format("2006-01-02T15:04:05")}, true
	}
	if len(tzid) > maxTZIDLen || hasControl(tzid) {
		return Time{Kind: TimeFloating, Local: wall.Format("2006-01-02T15:04:05")}, true
	}
	if loc, name := ianaZone(tzid); loc != nil {
		t := time.Date(wall.Year(), wall.Month(), wall.Day(), wall.Hour(), wall.Minute(), wall.Second(), 0, loc)
		return Time{Kind: TimeUTC, UTC: t.UTC(), TZID: name}, true
	}
	if cal != nil {
		for _, vtz := range cal.ChildrenNamed("VTIMEZONE") {
			id := vtz.Prop("TZID")
			if id == nil || strings.TrimSpace(id.Value) != tzid {
				continue
			}
			if off, ok := vtimezoneOffset(vtz, wall); ok {
				return Time{Kind: TimeUTC, UTC: wall.Add(-off).UTC(), TZID: cleanText(tzid, maxTZIDLen)}, true
			}
		}
	}
	return Time{Kind: TimeFloating, Local: wall.Format("2006-01-02T15:04:05"), TZID: cleanText(tzid, maxTZIDLen)}, true
}

// ianaZone tries tzid, then its last two and three path segments, against the
// compiled-in zone database. "Local" and "UTC"-alikes other than UTC itself are
// refused: "Local" would mean the SERVER's zone, which is nobody's meeting time.
func ianaZone(tzid string) (*time.Location, string) {
	cands := []string{tzid}
	if strings.Contains(tzid, "/") {
		segs := strings.Split(strings.Trim(tzid, "/"), "/")
		for _, n := range []int{3, 2} {
			if len(segs) > n {
				cands = append(cands, strings.Join(segs[len(segs)-n:], "/"))
			}
		}
	}
	for _, c := range cands {
		if c == "" || c == "Local" || strings.Contains(c, "..") || strings.HasPrefix(c, "/") || strings.ContainsAny(c, `\ `) {
			continue
		}
		loc, err := time.LoadLocation(c)
		if err == nil && loc != nil {
			return loc, c
		}
	}
	return nil, ""
}

// observance is one STANDARD or DAYLIGHT block of a VTIMEZONE.
type observance struct {
	start      time.Time // wall-clock onset, as written
	offsetFrom time.Duration
	offsetTo   time.Duration
	rule       *yearlyRule
}

// yearlyRule is the only RRULE shape a VTIMEZONE observance uses in practice:
// FREQ=YEARLY with BYMONTH and either BYDAY (nth weekday, -1 = last) or
// BYMONTHDAY, optionally ending at UNTIL.
type yearlyRule struct {
	month    time.Month
	nth      int // 0 when BYMONTHDAY is used
	weekday  time.Weekday
	monthDay int
	until    time.Time
}

// vtimezoneOffset is the UTC offset in force at wall-clock time t under this
// VTIMEZONE: the observance whose most recent onset is at or before t.
func vtimezoneOffset(vtz *Component, t time.Time) (time.Duration, bool) {
	var obs []observance
	for _, ch := range vtz.Children {
		if ch.Name != "STANDARD" && ch.Name != "DAYLIGHT" {
			continue
		}
		o, ok := readObservance(ch)
		if ok {
			obs = append(obs, o)
		}
	}
	if len(obs) == 0 {
		return 0, false
	}
	var (
		best      time.Time
		bestOff   time.Duration
		found     bool
		earliest  time.Time
		earlyFrom time.Duration
	)
	for _, o := range obs {
		for _, y := range []int{t.Year(), t.Year() - 1} {
			on, ok := o.onsetIn(y)
			if !ok {
				continue
			}
			if earliest.IsZero() || on.Before(earliest) {
				earliest, earlyFrom = on, o.offsetFrom
			}
			if on.After(t) {
				continue
			}
			if !found || on.After(best) {
				best, bestOff, found = on, o.offsetTo, true
			}
		}
	}
	if found {
		return bestOff, true
	}
	if !earliest.IsZero() {
		// Before every onset this zone describes: the offset in force then is the
		// one the first transition moved AWAY from.
		return earlyFrom, true
	}
	return 0, false
}

func readObservance(c *Component) (observance, bool) {
	var o observance
	ds := c.Prop("DTSTART")
	from, to := c.Prop("TZOFFSETFROM"), c.Prop("TZOFFSETTO")
	if ds == nil || to == nil {
		return o, false
	}
	wall, isDate, _, err := parseDateOrDateTime(ds.Value, "")
	if err != nil || isDate {
		return o, false
	}
	o.start = wall
	if o.offsetTo, err = parseUTCOffset(to.Value); err != nil {
		return o, false
	}
	o.offsetFrom = o.offsetTo
	if from != nil {
		if f, err := parseUTCOffset(from.Value); err == nil {
			o.offsetFrom = f
		}
	}
	if rr := c.Prop("RRULE"); rr != nil {
		r, ok := parseYearlyRule(rr.Value, wall)
		if !ok {
			// A rule we cannot read is not evidence the observance happens only
			// once; it is not usable at all.
			return o, false
		}
		o.rule = r
	}
	return o, true
}

// onsetIn is the observance's onset in year y, if it has one.
func (o observance) onsetIn(y int) (time.Time, bool) {
	if o.rule == nil {
		if o.start.Year() == y {
			return o.start, true
		}
		return time.Time{}, false
	}
	if y < o.start.Year() {
		return time.Time{}, false
	}
	r := o.rule
	var day int
	if r.nth == 0 {
		day = r.monthDay
	} else {
		day = nthWeekday(y, r.month, r.weekday, r.nth)
	}
	if day < 1 {
		return time.Time{}, false
	}
	on := time.Date(y, r.month, day, o.start.Hour(), o.start.Minute(), o.start.Second(), 0, time.UTC)
	if on.Month() != r.month {
		return time.Time{}, false
	}
	if !r.until.IsZero() && on.After(r.until) {
		return time.Time{}, false
	}
	return on, true
}

// nthWeekday is the day of month of the nth (1..5, or -1..-5 from the end)
// weekday wd in month m of year y; a 5th that does not exist is the last.
func nthWeekday(y int, m time.Month, wd time.Weekday, nth int) int {
	if nth > 0 {
		first := time.Date(y, m, 1, 0, 0, 0, 0, time.UTC)
		d := 1 + (int(wd)-int(first.Weekday())+7)%7 + (nth-1)*7
		last := time.Date(y, m+1, 0, 0, 0, 0, 0, time.UTC).Day()
		for d > last {
			d -= 7
		}
		return d
	}
	last := time.Date(y, m+1, 0, 0, 0, 0, 0, time.UTC)
	d := last.Day() - (int(last.Weekday())-int(wd)+7)%7 + (nth+1)*7
	for d < 1 {
		d += 7
	}
	return d
}

var weekdays = map[string]time.Weekday{
	"SU": time.Sunday, "MO": time.Monday, "TU": time.Tuesday, "WE": time.Wednesday,
	"TH": time.Thursday, "FR": time.Friday, "SA": time.Saturday,
}

func parseYearlyRule(v string, start time.Time) (*yearlyRule, bool) {
	parts := rruleParts(v)
	if parts == nil || parts["FREQ"] != "YEARLY" {
		return nil, false
	}
	r := &yearlyRule{month: start.Month(), monthDay: start.Day()}
	if m := parts["BYMONTH"]; m != "" {
		n, err := strconv.Atoi(m)
		if err != nil || n < 1 || n > 12 {
			return nil, false
		}
		r.month = time.Month(n)
	}
	if d := parts["BYDAY"]; d != "" {
		nth, wd, ok := parseByDay(d)
		if !ok || nth == 0 {
			return nil, false
		}
		r.nth, r.weekday = nth, wd
	} else if d := parts["BYMONTHDAY"]; d != "" {
		n, err := strconv.Atoi(d)
		if err != nil || n < 1 || n > 31 {
			return nil, false
		}
		r.monthDay = n
	}
	if u := parts["UNTIL"]; u != "" {
		wall, _, _, err := parseDateOrDateTime(u, "")
		if err != nil {
			return nil, false
		}
		r.until = wall
	}
	return r, true
}

// parseByDay reads one BYDAY entry: an optional signed ordinal and a weekday.
func parseByDay(s string) (int, time.Weekday, bool) {
	s = strings.TrimSpace(strings.ToUpper(s))
	if len(s) < 2 || len(s) > 5 {
		return 0, 0, false
	}
	wd, ok := weekdays[s[len(s)-2:]]
	if !ok {
		return 0, 0, false
	}
	num := s[:len(s)-2]
	if num == "" {
		return 0, wd, true
	}
	n, err := strconv.Atoi(num)
	if err != nil || n == 0 || n < -53 || n > 53 {
		return 0, 0, false
	}
	return n, wd, true
}

// maxRRuleParts bounds the name=value pairs read from one RRULE.
const maxRRuleParts = 16

// rruleParts splits an RRULE value into its parts, upper-cased. A repeated part
// or a part without '=' makes the whole rule unreadable (nil).
func rruleParts(v string) map[string]string {
	out := map[string]string{}
	for i, kv := range strings.Split(v, ";") {
		if i >= maxRRuleParts {
			return nil
		}
		k, val, ok := strings.Cut(kv, "=")
		if !ok {
			return nil
		}
		k = strings.ToUpper(strings.TrimSpace(k))
		if _, dup := out[k]; dup || k == "" {
			return nil
		}
		out[k] = strings.ToUpper(strings.TrimSpace(val))
	}
	return out
}
