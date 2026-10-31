// SPDX-License-Identifier: AGPL-3.0-only

package notify

import (
	"testing"
	"time"
)

func utc(s string) time.Time {
	t, err := time.Parse(time.RFC3339, s)
	if err != nil {
		panic(err)
	}
	return t
}

// bit returns the Days mask for the given weekdays.
func bit(days ...time.Weekday) uint8 {
	var m uint8
	for _, d := range days {
		m |= 1 << uint(d)
	}
	return m
}

func TestDecideModes(t *testing.T) {
	c := Counts{All: 10, AboveLine: 3, VIP: 2, AboveLineOrVIP: 4}
	cases := []struct {
		mode        Mode
		quiet, vipQ bool
		want        int
	}{
		{ModeAll, false, true, 10},
		{ModeAboveLine, false, true, 3},
		{ModeVIP, false, true, 2},
		{ModeAboveLineOrVIP, false, true, 4},
		{Mode("nonsense"), false, true, 10}, // fails open to today's behaviour
		// Quiet hours: VIPs only, whatever the device's mode...
		{ModeAll, true, true, 2},
		{ModeAboveLine, true, true, 2},
		{ModeVIP, true, true, 2},
		{ModeAboveLineOrVIP, true, true, 2},
		// ...and nothing at all once the VIP exception is switched off.
		{ModeAll, true, false, 0},
		{ModeVIP, true, false, 0},
	}
	for _, tc := range cases {
		if got := Decide(tc.mode, c, tc.quiet, tc.vipQ); got != tc.want {
			t.Errorf("Decide(%s, quiet=%v, allowVIP=%v) = %d, want %d", tc.mode, tc.quiet, tc.vipQ, got, tc.want)
		}
	}
}

func TestNeedsBreakdown(t *testing.T) {
	if NeedsBreakdown([]Mode{ModeAll, ModeAll}, false) {
		t.Error("every device in every-message mode, not quiet: no breakdown is needed")
	}
	if !NeedsBreakdown([]Mode{ModeAll, ModeVIP}, false) {
		t.Error("a VIP-mode device needs the breakdown")
	}
	if !NeedsBreakdown([]Mode{ModeAll}, true) {
		t.Error("quiet hours in force need the VIP count")
	}
}

// TestQuietHoursWallClock runs every edge on a fake clock: the instant is
// fixed in UTC, and the question is whether the period is in force on the
// user's wall clock at that instant.
func TestQuietHoursWallClock(t *testing.T) {
	night := QuietHours{Enabled: true, Start: 22 * 60, End: 7 * 60, Days: AllDays, Zone: "America/New_York", AllowVIP: true}
	cases := []struct {
		name string
		q    QuietHours
		at   string
		want bool
	}{
		// --- an ordinary overnight period
		{"evening half", night, "2026-06-16T03:30:00Z", true}, // 23:30 EDT
		{"morning half", night, "2026-06-16T10:59:00Z", true}, // 06:59 EDT
		{"end is exclusive", night, "2026-06-16T11:00:00Z", false},
		{"start is inclusive", night, "2026-06-16T02:00:00Z", true}, // 22:00 EDT
		{"daytime", night, "2026-06-16T16:00:00Z", false},

		// --- SPRING FORWARD (New York, Sun 8 Mar 2026, 02:00 EST -> 03:00 EDT).
		// The night is an hour SHORTER: it still ends at 07:00 on the wall.
		{"spring: 23:30 EST the night before", night, "2026-03-08T04:30:00Z", true},
		{"spring: 06:59 EDT still quiet", night, "2026-03-08T10:59:00Z", true},
		// A duration-based window (22:00 EST + 9h = 08:00 EDT) would say quiet here.
		{"spring: 07:30 EDT is morning", night, "2026-03-08T11:30:00Z", false},

		// --- FALL BACK (New York, Sun 1 Nov 2026, 02:00 EDT -> 01:00 EST).
		// The night is an hour LONGER: it still ends at 07:00 on the wall.
		{"fall: 22:30 EDT the night before", night, "2026-11-01T02:30:00Z", true},
		{"fall: 06:30 EST still quiet", night, "2026-11-01T11:30:00Z", true},
		// A duration-based window (22:00 EDT + 9h = 06:00 EST) would end early.
		{"fall: 07:00 EST is morning", night, "2026-11-01T12:00:00Z", false},

		// --- a period STARTING in the spring-forward gap (02:30 does not exist):
		// it begins at the first minute that does, 03:00 EDT.
		{"gap: 01:59 EST before", QuietHours{Enabled: true, Start: 150, End: 240, Days: AllDays, Zone: "America/New_York"}, "2026-03-08T06:59:00Z", false},
		{"gap: 03:15 EDT inside", QuietHours{Enabled: true, Start: 150, End: 240, Days: AllDays, Zone: "America/New_York"}, "2026-03-08T07:15:00Z", true},

		// --- a period covering the REPEATED hour is quiet both times round.
		{"repeat: 01:30 EDT", QuietHours{Enabled: true, Start: 60, End: 120, Days: AllDays, Zone: "America/New_York"}, "2026-11-01T05:30:00Z", true},
		{"repeat: 01:30 EST", QuietHours{Enabled: true, Start: 60, End: 120, Days: AllDays, Zone: "America/New_York"}, "2026-11-01T06:30:00Z", true},

		// --- London's own change (Sun 29 Mar 2026, 01:00 GMT -> 02:00 BST).
		{"london: 06:59 BST quiet", QuietHours{Enabled: true, Start: 22 * 60, End: 7 * 60, Days: AllDays, Zone: "Europe/London"}, "2026-03-29T05:59:00Z", true},
		{"london: 07:00 BST morning", QuietHours{Enabled: true, Start: 22 * 60, End: 7 * 60, Days: AllDays, Zone: "Europe/London"}, "2026-03-29T06:00:00Z", false},

		// --- the SAME instant in three zones: it is the user's zone that decides.
		{"zone: 22:00 in Los Angeles", QuietHours{Enabled: true, Start: 22 * 60, End: 7 * 60, Days: AllDays, Zone: "America/Los_Angeles"}, "2026-06-15T05:00:00Z", true},
		{"zone: 06:00 in London", QuietHours{Enabled: true, Start: 22 * 60, End: 7 * 60, Days: AllDays, Zone: "Europe/London"}, "2026-06-15T05:00:00Z", true},
		{"zone: 14:00 in Tokyo", QuietHours{Enabled: true, Start: 22 * 60, End: 7 * 60, Days: AllDays, Zone: "Asia/Tokyo"}, "2026-06-15T05:00:00Z", false},

		// --- DAYS name the day a period STARTS on. Friday night only:
		{"days: Fri 23:00", QuietHours{Enabled: true, Start: 22 * 60, End: 7 * 60, Days: bit(time.Friday), Zone: "UTC"}, "2026-10-16T23:00:00Z", true},
		{"days: Sat 06:00 belongs to Friday's night", QuietHours{Enabled: true, Start: 22 * 60, End: 7 * 60, Days: bit(time.Friday), Zone: "UTC"}, "2026-10-17T06:00:00Z", true},
		{"days: Sat 23:00 is not Friday", QuietHours{Enabled: true, Start: 22 * 60, End: 7 * 60, Days: bit(time.Friday), Zone: "UTC"}, "2026-10-17T23:00:00Z", false},
		{"days: Fri 06:00 belongs to Thursday", QuietHours{Enabled: true, Start: 22 * 60, End: 7 * 60, Days: bit(time.Friday), Zone: "UTC"}, "2026-10-16T06:00:00Z", false},
		// A same-day period on Mondays only.
		{"days: Mon 13:30", QuietHours{Enabled: true, Start: 13 * 60, End: 14 * 60, Days: bit(time.Monday), Zone: "UTC"}, "2026-10-12T13:30:00Z", true},
		{"days: Tue 13:30", QuietHours{Enabled: true, Start: 13 * 60, End: 14 * 60, Days: bit(time.Monday), Zone: "UTC"}, "2026-10-13T13:30:00Z", false},
		// The local DAY decides, not the UTC one: Fri 22:30 in New York is Sat in UTC.
		{"days: local Friday", QuietHours{Enabled: true, Start: 22 * 60, End: 7 * 60, Days: bit(time.Friday), Zone: "America/New_York"}, "2026-10-17T02:30:00Z", true},

		// --- off states
		{"disabled", QuietHours{Enabled: false, Start: 0, End: 1439, Days: AllDays, Zone: "UTC"}, "2026-10-16T12:00:00Z", false},
		{"start == end", QuietHours{Enabled: true, Start: 600, End: 600, Days: AllDays, Zone: "UTC"}, "2026-10-16T10:00:00Z", false},
		{"no days", QuietHours{Enabled: true, Start: 0, End: 1439, Days: 0, Zone: "UTC"}, "2026-10-16T12:00:00Z", false},
		{"unknown zone fails open", QuietHours{Enabled: true, Start: 0, End: 1439, Days: AllDays, Zone: "Mars/Olympus"}, "2026-10-16T12:00:00Z", false},
	}
	for _, tc := range cases {
		if got := tc.q.Active(utc(tc.at)); got != tc.want {
			t.Errorf("%s: Active(%s) = %v, want %v", tc.name, tc.at, got, tc.want)
		}
	}
}

func TestValidateZone(t *testing.T) {
	for _, ok := range []string{"UTC", "Europe/London", "America/New_York", "Pacific/Chatham"} {
		if err := ValidateZone(ok); err != nil {
			t.Errorf("ValidateZone(%q) = %v, want nil", ok, err)
		}
	}
	for _, bad := range []string{"", "Mars/Olympus", "../etc/passwd"} {
		if err := ValidateZone(bad); err == nil {
			t.Errorf("ValidateZone(%q) = nil, want an error", bad)
		}
	}
}
