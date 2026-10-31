// SPDX-License-Identifier: AGPL-3.0-only

package api

import (
	"testing"
	"time"
)

// TestParseExpiresAt pins the exact-expiry rules (#813, ADR-040 §12): a date is
// the end of that day in UTC, a date-time is its instant in UTC to the second,
// and the window is one day to ten years from now.
func TestParseExpiresAt(t *testing.T) {
	now := time.Date(2026, 10, 21, 22, 30, 0, 0, time.UTC)
	ok := map[string]time.Time{
		// Date only: 23:59:59Z that day.
		"2026-11-04": time.Date(2026, 11, 4, 23, 59, 59, 0, time.UTC),
		// UTCDate.
		"2026-11-04T12:00:00Z": time.Date(2026, 11, 4, 12, 0, 0, 0, time.UTC),
		// A late evening in a UTC- zone is the NEXT day in UTC: the server keeps
		// the instant and never re-reads it as a date.
		"2026-11-04T23:30:00-08:00": time.Date(2026, 11, 5, 7, 30, 0, 0, time.UTC),
		// Fractions are dropped (packets have second resolution).
		"2026-11-04T12:00:00.987Z": time.Date(2026, 11, 4, 12, 0, 0, 0, time.UTC),
		// Tomorrow as a date is always far enough: at least 24h01m from 22:30.
		"2026-10-22": time.Date(2026, 10, 22, 23, 59, 59, 0, time.UTC),
	}
	for in, want := range ok {
		got, mErr := parseExpiresAt(in, now)
		if mErr != nil || !got.Equal(want) {
			t.Errorf("parseExpiresAt(%q) = %v, %+v; want %v", in, got, mErr, want)
		}
	}
	bad := []string{
		"2026-10-21",           // today: under a day left
		"2026-10-20",           // the past
		"2026-10-22T22:00:00Z", // 23h30m out
		"2036-11-01",           // past ten years (3650 days)
		"04/11/2026",           // not a form we read
		"",
	}
	for _, in := range bad {
		if got, mErr := parseExpiresAt(in, now); mErr == nil || mErr.Type != errInvalidArguments {
			t.Errorf("parseExpiresAt(%q) = %v, %+v; want invalidArguments", in, got, mErr)
		}
	}
	days, at := 30, "2026-12-01"
	if _, mErr := generateExpiry(&days, &at, now); mErr == nil {
		t.Error("expiresDays with expiresAt was accepted")
	}
	if e, mErr := generateExpiry(nil, &at, now); mErr != nil || e.At == nil || e.Days != nil {
		t.Errorf("expiresAt alone = %+v, %+v", e, mErr)
	}
	if e, mErr := generateExpiry(nil, nil, now); mErr != nil || e.At != nil || e.Days != nil {
		t.Errorf("neither = %+v, %+v; want the default", e, mErr)
	}
}
