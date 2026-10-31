// SPDX-License-Identifier: AGPL-3.0-only

package main

// drawer-report's formatting logic, tested the same way area-report's is
// (areareport_test.go): hand-built values, no database, because a report
// whose labels lie about what the data says is worse than one that never ran.

import (
	"strings"
	"testing"
	"time"

	"mailroom/internal/store"
)

// TestPrintDrawerReportHeaderNamesTheSwitchState pins the master switch's
// header line for both states (migration 00048, #443): the report's whole
// point is to be read BEFORE the hourly job can move anything, so an
// operator must see plainly, without scrolling, whether it currently can.
func TestPrintDrawerReportHeaderNamesTheSwitchState(t *testing.T) {
	for _, tc := range []struct {
		name    string
		enabled bool
		want    string
	}{
		{name: "off is the default", enabled: false,
			want: "drawer sweep: OFF for this login — the hourly job archives nothing until it is switched on"},
		{name: "on", enabled: true, want: "drawer sweep: ON"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			out := captureStdout(t, func() {
				printDrawerReport(store.DeskPreferences{DrawerSweepEnabled: tc.enabled}, store.DrawerReportRows{})
			})
			if !strings.Contains(out, tc.want) {
				t.Errorf("printDrawerReport output did not contain %q:\n%s", tc.want, out)
			}
		})
	}
}

func TestDrawerPeriodLabel(t *testing.T) {
	for _, tc := range []struct {
		name string
		r    store.DrawerAreaReport
		want string
	}{
		{
			name: "off names no number the way it would sweep, so the WOULD-BE days still print",
			r:    store.DrawerAreaReport{PeriodSource: store.DrawerPeriodOff, PeriodDays: 30},
			want: "off",
		},
		{
			name: "the login default is labelled as such, not left indistinguishable from an override",
			r:    store.DrawerAreaReport{PeriodSource: store.DrawerPeriodDefault, PeriodDays: 30},
			want: "30 d (default)",
		},
		{
			name: "an area's own period prints bare",
			r:    store.DrawerAreaReport{PeriodSource: store.DrawerPeriodOwn, PeriodDays: 45},
			want: "45 d",
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := drawerPeriodLabel(tc.r); got != tc.want {
				t.Errorf("drawerPeriodLabel(%+v) = %q, want %q", tc.r, got, tc.want)
			}
		})
	}
}

func TestDrawerDateOrDash(t *testing.T) {
	if got := drawerDateOrDash(nil); got != "-" {
		t.Errorf("drawerDateOrDash(nil) = %q, want %q — a report with no candidates must not print a zero date", got, "-")
	}
	at := time.Date(2026, 8, 1, 12, 0, 0, 0, time.UTC)
	if got := drawerDateOrDash(&at); got != "2026-08-01" {
		t.Errorf("drawerDateOrDash(2026-08-01T12:00:00Z) = %q, want %q", got, "2026-08-01")
	}
}

// TestHasSeenKeyword pins the unread-count checked fact's own building
// block: it must find $seen among other keywords, and must not confuse a
// LOOKALIKE keyword ("seen", no dollar sign) with the real one.
func TestHasSeenKeyword(t *testing.T) {
	for _, tc := range []struct {
		name string
		kw   []string
		want bool
	}{
		{name: "no keywords at all", kw: nil, want: false},
		{name: "seen among others", kw: []string{store.KeywordFlagged, store.KeywordSeen}, want: true},
		{name: "flagged but not seen", kw: []string{store.KeywordFlagged}, want: false},
		{name: "a lookalike keyword is not the real one", kw: []string{"seen"}, want: false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := hasSeenKeyword(tc.kw); got != tc.want {
				t.Errorf("hasSeenKeyword(%v) = %v, want %v", tc.kw, got, tc.want)
			}
		})
	}
}
