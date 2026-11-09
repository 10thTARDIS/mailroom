// SPDX-License-Identifier: AGPL-3.0-only

package main

import (
	"strings"
	"testing"
	"time"
)

func TestParseActivityOptions(t *testing.T) {
	now := time.Date(2026, 11, 9, 12, 0, 0, 0, time.UTC)
	o, err := parseActivityOptions(now, "7d", "purge, session", "", 50, 100)
	if err != nil || !o.Since.Equal(now.Add(-7*24*time.Hour)) || len(o.Sources) != 2 || o.Sources[1] != "session" {
		t.Fatalf("7d: %+v, %v", o, err)
	}
	if o, err = parseActivityOptions(now, "36h", "", "", 50, 100); err != nil || !o.Since.Equal(now.Add(-36*time.Hour)) {
		t.Errorf("36h: %+v, %v", o, err)
	}
	if o, err = parseActivityOptions(now, "2026-11-01", "", "", 50, 100); err != nil || !o.Since.Equal(time.Date(2026, 11, 1, 0, 0, 0, 0, time.UTC)) {
		t.Errorf("date: %+v, %v", o, err)
	}
	for name, c := range map[string]struct {
		since, source, user string
		limit, cap          int
		want                string
	}{
		"bad since":    {"soon", "", "", 50, 1, "--since"},
		"bad source":   {"7d", "nope", "", 50, 1, "unknown source"},
		"bad user":     {"7d", "", "not-an-id", 50, 1, "--user"},
		"limit high":   {"7d", "", "", 101, 1, "--limit"},
		"limit zero":   {"7d", "", "", 0, 1, "--limit"},
		"cap zero":     {"7d", "", "", 50, 0, "--cap"},
		"negative day": {"-1d", "", "", 50, 1, "--since"},
	} {
		if _, err := parseActivityOptions(now, c.since, c.source, c.user, c.limit, c.cap); err == nil || !strings.Contains(err.Error(), c.want) {
			t.Errorf("%s: err = %v, want it to mention %q", name, err, c.want)
		}
	}
}

// Every kind the store can return has a row in the report, or it would be counted
// and never printed.
func TestActivityReportListsEveryKind(t *testing.T) {
	if len(activityKindsInOrder) != 8 {
		t.Errorf("the report lists %d kinds; update it with the store", len(activityKindsInOrder))
	}
}
