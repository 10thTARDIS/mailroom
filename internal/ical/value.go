// SPDX-License-Identifier: AGPL-3.0-only

package ical

import (
	"errors"
	"net/mail"
	"strconv"
	"strings"
	"time"
	"unicode"
	"unicode/utf8"
)

// unescapeText decodes an RFC 5545 §3.3.11 TEXT value: \n or \N is a newline,
// \, \; and \\ are themselves. An unknown escape keeps the character after the
// backslash, which is what every real parser does with Outlook's stray "\:".
func unescapeText(s string) string {
	if !strings.Contains(s, `\`) {
		return s
	}
	var b strings.Builder
	b.Grow(len(s))
	for i := 0; i < len(s); i++ {
		c := s[i]
		if c != '\\' || i+1 >= len(s) {
			b.WriteByte(c)
			continue
		}
		i++
		switch s[i] {
		case 'n', 'N':
			b.WriteByte('\n')
		default:
			b.WriteByte(s[i])
		}
	}
	return b.String()
}

// cleanText makes a sender-authored string safe to hand to a client: every
// control character (newlines included) becomes a space, runs of whitespace
// collapse, and the result is capped at max runes. The card shows one line per
// field, so a newline has no meaning there — and a CR or LF that survived to a
// caller is the raw material of an injection.
func cleanText(s string, max int) string {
	if !utf8.ValidString(s) {
		s = strings.ToValidUTF8(s, "�")
	}
	var b strings.Builder
	space := false
	n := 0
	for _, r := range s {
		if unicode.IsControl(r) || unicode.IsSpace(r) {
			space = true
			continue
		}
		if space && b.Len() > 0 {
			b.WriteByte(' ')
			n++
		}
		space = false
		if n >= max {
			break
		}
		b.WriteRune(r)
		n++
	}
	out := b.String()
	if utf8.RuneCountInString(out) > max {
		out = string([]rune(out)[:max])
	}
	return out
}

// hasControl reports whether s contains any control character.
func hasControl(s string) bool {
	for _, r := range s {
		if unicode.IsControl(r) {
			return true
		}
	}
	return false
}

// calAddress reads a CAL-ADDRESS (RFC 5545 §3.3.3) as an email address. Only a
// mailto: URI with a single, plain addr-spec is an address: anything else — a
// display name smuggled into the URI, a comment, a second address, an http: URI,
// a control character — is not one, and the result is "".
func calAddress(v string) string {
	v = strings.TrimSpace(v)
	if len(v) < 7 || !strings.EqualFold(v[:7], "mailto:") {
		return ""
	}
	addr := strings.TrimSpace(v[7:])
	return ValidAddress(addr)
}

// ValidAddress returns addr if it is one plain RFC 5322 addr-spec and nothing
// else, or "". It is exported because the reply path asks the same question of
// the organiser address before it sends anything to it.
func ValidAddress(addr string) string {
	if addr == "" || len(addr) > 320 || hasControl(addr) || strings.ContainsAny(addr, " <>\"(),;:") {
		return ""
	}
	parsed, err := mail.ParseAddress(addr)
	if err != nil || parsed.Address != addr || parsed.Name != "" {
		return ""
	}
	return addr
}

// TimeKind is how a calendar time is anchored.
type TimeKind string

const (
	// TimeUTC is an absolute instant: written in UTC, or in a zone this package
	// could resolve. Time.UTC holds it.
	TimeUTC TimeKind = "utc"
	// TimeDate is an all-day date (VALUE=DATE). Time.Date holds YYYY-MM-DD.
	TimeDate TimeKind = "date"
	// TimeFloating is a wall-clock time with no zone this package could resolve
	// — a true floating time, or a TZID nothing could interpret. Time.Local holds
	// YYYY-MM-DDTHH:MM:SS, and Time.TZID the zone's name when one was given, so a
	// client can show the time AS WRITTEN and say which zone it is in, rather than
	// silently pretending it is in the reader's.
	TimeFloating TimeKind = "floating"
)

// Time is one calendar time.
type Time struct {
	Kind  TimeKind
	UTC   time.Time
	Date  string
	Local string
	// TZID is the zone the time was written in, when it had one: the resolved
	// IANA name for a TimeUTC, the name as written (cleaned) for an unresolved
	// TimeFloating.
	TZID string
}

var errBadTime = errors.New("ical: malformed date or date-time")

// parseDateOrDateTime reads a DATE (YYYYMMDD) or DATE-TIME (YYYYMMDDTHHMMSS[Z])
// as a wall-clock time in a UTC-located time.Time, plus whether it was a date,
// and whether it carried the Z suffix. The ranges are checked by round-trip, so
// "20261340" is an error rather than a normalised February.
func parseDateOrDateTime(v string, valueParam string) (wall time.Time, isDate, isUTC bool, err error) {
	v = strings.TrimSpace(v)
	if strings.EqualFold(valueParam, "DATE") || len(v) == 8 {
		t, err := time.Parse("20060102", v)
		if err != nil {
			return time.Time{}, false, false, errBadTime
		}
		return t, true, false, nil
	}
	if strings.HasSuffix(v, "Z") || strings.HasSuffix(v, "z") {
		isUTC = true
		v = v[:len(v)-1]
	}
	t, err := time.Parse("20060102T150405", v)
	if err != nil {
		return time.Time{}, false, false, errBadTime
	}
	return t, false, isUTC, nil
}

// maxDuration bounds a DURATION value: anything longer than this is treated as
// malformed rather than producing an end time decades away.
const maxDuration = 10 * 366 * 24 * time.Hour

// parseDuration reads an RFC 5545 §3.3.6 DURATION ("P1D", "PT1H30M", "P2W",
// "-PT15M"). Bounded by maxDuration.
func parseDuration(v string) (time.Duration, error) {
	v = strings.TrimSpace(strings.ToUpper(v))
	neg := false
	switch {
	case strings.HasPrefix(v, "-"):
		neg = true
		v = v[1:]
	case strings.HasPrefix(v, "+"):
		v = v[1:]
	}
	if !strings.HasPrefix(v, "P") || len(v) < 3 {
		return 0, errBadTime
	}
	v = v[1:]
	var total time.Duration
	inTime := false
	num := ""
	seen := false
	for _, r := range v {
		switch {
		case r >= '0' && r <= '9':
			if len(num) >= 6 {
				return 0, errBadTime
			}
			num += string(r)
		case r == 'T':
			if inTime || num != "" {
				return 0, errBadTime
			}
			inTime = true
		default:
			if num == "" {
				return 0, errBadTime
			}
			n, _ := strconv.Atoi(num)
			num = ""
			unit := time.Duration(0)
			switch {
			case r == 'W' && !inTime:
				unit = 7 * 24 * time.Hour
			case r == 'D' && !inTime:
				unit = 24 * time.Hour
			case r == 'H' && inTime:
				unit = time.Hour
			case r == 'M' && inTime:
				unit = time.Minute
			case r == 'S' && inTime:
				unit = time.Second
			default:
				return 0, errBadTime
			}
			total += time.Duration(n) * unit
			if total > maxDuration {
				return 0, errBadTime
			}
			seen = true
		}
	}
	if num != "" || !seen {
		return 0, errBadTime
	}
	if neg {
		total = -total
	}
	return total, nil
}

// parseUTCOffset reads a UTC-OFFSET (RFC 5545 §3.3.14): "+0100", "-0530",
// "+013000". Bounded to ±24h.
func parseUTCOffset(v string) (time.Duration, error) {
	v = strings.TrimSpace(v)
	if len(v) != 5 && len(v) != 7 {
		return 0, errBadTime
	}
	sign := time.Duration(1)
	switch v[0] {
	case '+':
	case '-':
		sign = -1
	default:
		return 0, errBadTime
	}
	for _, c := range v[1:] {
		if c < '0' || c > '9' {
			return 0, errBadTime
		}
	}
	h, _ := strconv.Atoi(v[1:3])
	m, _ := strconv.Atoi(v[3:5])
	s := 0
	if len(v) == 7 {
		s, _ = strconv.Atoi(v[5:7])
	}
	if h > 23 || m > 59 || s > 59 {
		return 0, errBadTime
	}
	return sign * (time.Duration(h)*time.Hour + time.Duration(m)*time.Minute + time.Duration(s)*time.Second), nil
}
