// SPDX-License-Identifier: AGPL-3.0-only

// Package notify decides WHICH new mail a device is told about (#59): the
// per-device push mode, quiet hours, and the VIP exception. It is pure — no
// database, no clock of its own, no network — so every rule here is tested
// with a fake clock and literal counts, and the store (PushCounts) and the
// sender (internal/syncrunner) stay mechanisms.
//
// THE RULES, in the order they apply to one device and one batch:
//
//  1. The ACCOUNT may be switched off (Account.notifyPush): nothing at all.
//     That is the caller's check, before any of this runs.
//  2. QUIET HOURS, when on and active now: only VIP mail goes through, and only
//     while "Allow VIPs during quiet hours" is on (the default). A device in
//     every-message mode is silenced too — quiet hours are per USER, a
//     statement about the person's night, not about one phone.
//  3. Otherwise the device's MODE picks the count:
//     all                every new message (today's behaviour and the default)
//     above_line         above the desk's line, or marked important by a rule
//     vip                from a VIP
//     above_line_or_vip  either
//
// A device whose count comes out zero is sent nothing: no "0 new messages".
package notify

import (
	"fmt"
	"time"
	// The runtime image is Alpine without the tzdata package, and a quiet-hours
	// zone that failed to load would silently fall back to "not quiet" (see
	// QuietHours.Active). Embedding the database (~450 KB) makes every IANA
	// name the settings page offers loadable everywhere this binary runs.
	_ "time/tzdata"
)

// Mode is a device's push mode. The values are the store's
// (app.push_subscriptions.notify_mode) — the same words, one vocabulary.
type Mode string

// The four modes.
const (
	ModeAll            Mode = "all"
	ModeAboveLine      Mode = "above_line"
	ModeVIP            Mode = "vip"
	ModeAboveLineOrVIP Mode = "above_line_or_vip"
)

// Counts is one batch of new mail, broken down the way the modes ask
// (store.PushCounts measures it).
type Counts struct {
	All            int
	AboveLine      int
	VIP            int
	AboveLineOrVIP int
}

// NeedsBreakdown reports whether deciding for these modes, with quiet hours
// in force or not right now, needs anything beyond the plain count — so a
// user with every device in every-message mode outside quiet hours costs no
// extra query at all (today's behaviour, at today's cost).
func NeedsBreakdown(modes []Mode, quietNow bool) bool {
	if quietNow {
		return true
	}
	for _, m := range modes {
		if m != ModeAll {
			return true
		}
	}
	return false
}

// Decide returns how many new messages a device in mode m is told about, given
// the batch's counts and whether quiet hours are active right now. Zero means
// send nothing. An unknown mode is treated as every message: the column's
// CHECK makes it unreachable, and failing open to today's behaviour is the
// one choice that cannot lose a notification someone relied on.
func Decide(m Mode, c Counts, quietNow, allowVIPWhenQuiet bool) int {
	if quietNow {
		if allowVIPWhenQuiet {
			return c.VIP
		}
		return 0
	}
	switch m {
	case ModeAboveLine:
		return c.AboveLine
	case ModeVIP:
		return c.VIP
	case ModeAboveLineOrVIP:
		return c.AboveLineOrVIP
	default:
		return c.All
	}
}

// QuietHours is the user's quiet-hours setting (DeskPreferences, #59).
type QuietHours struct {
	Enabled bool
	// Start and End are minutes after LOCAL midnight, 0..1439. Start > End is
	// an overnight period (22:00–07:00); Start == End is refused by the API,
	// so it never reaches here, and is treated as never quiet if it does.
	Start, End int
	// Days is a bitmask of the weekday a quiet period STARTS on, bit 0 =
	// Sunday (time.Weekday). An overnight period that starts on Friday runs
	// into Saturday morning whether or not Saturday is set.
	Days uint8
	// Zone is an IANA time zone name ("Europe/London"). The period follows
	// the WALL CLOCK there, across daylight-saving changes.
	Zone string
	// AllowVIP lets VIP mail through during quiet hours (default on).
	AllowVIP bool
}

// AllDays is every day of the week.
const AllDays uint8 = 0x7f

// ValidateZone reports whether name is an IANA zone this binary can load.
func ValidateZone(name string) error {
	if name == "" {
		return fmt.Errorf("notify: empty time zone")
	}
	if _, err := time.LoadLocation(name); err != nil {
		return fmt.Errorf("notify: unknown time zone %q", name)
	}
	return nil
}

// Active reports whether quiet hours are in force at now.
//
// THE ARITHMETIC IS IN WALL-CLOCK MINUTES, not in durations, and that is the
// whole of the daylight-saving story: the question "is it between 22:00 and
// 07:00 where I live" is about the clock on the wall, which is what
// hour*60+minute of now.In(zone) is. On a spring-forward night a period that
// starts at 02:30 (a time that does not exist) begins at the first minute
// that does, 03:00; on a fall-back night 01:30 happens twice and is quiet both
// times if the period covers it. A duration-based window (start + 9h) would
// end an hour early or late on those nights.
//
// A zone that cannot be loaded makes this report false — not quiet. The API
// refuses an unloadable zone at write time, so this is only reachable if the
// zone database changes under a stored name; failing towards "notify" there
// keeps a VIP from being silenced by a typo nobody can see.
func (q QuietHours) Active(now time.Time) bool {
	if !q.Enabled || q.Start == q.End || q.Days&AllDays == 0 {
		return false
	}
	loc, err := time.LoadLocation(q.Zone)
	if err != nil {
		return false
	}
	local := now.In(loc)
	minute := local.Hour()*60 + local.Minute()
	today := local.Weekday()
	yesterday := (today + 6) % 7
	starts := func(d time.Weekday) bool { return q.Days&(1<<uint(d)) != 0 }

	if q.Start < q.End {
		// Same-day period, 13:00–14:00.
		return starts(today) && minute >= q.Start && minute < q.End
	}
	// Overnight period, 22:00–07:00: the evening half belongs to a period that
	// starts today, the morning half to one that started yesterday.
	if minute >= q.Start {
		return starts(today)
	}
	if minute < q.End {
		return starts(yesterday)
	}
	return false
}
