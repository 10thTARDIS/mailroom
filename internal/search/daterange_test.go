// SPDX-License-Identifier: AGPL-3.0-only

package search

import (
	"reflect"
	"testing"
	"time"
)

func day(y int, m time.Month, d int) time.Time {
	return time.Date(y, m, d, 0, 0, 0, 0, time.UTC)
}

// The bucket cover is the whole date-filter mechanism (the engine binding has no
// numeric fields), so it is tested exhaustively and engine-free.
func TestDateCover(t *testing.T) {
	tests := []struct {
		name     string
		from, to time.Time
		want     []string
	}{
		{"one day", day(2026, 7, 12), day(2026, 7, 13), []string{"d2026-07-12"}},
		{"three days", day(2026, 7, 12), day(2026, 7, 15),
			[]string{"d2026-07-12", "d2026-07-13", "d2026-07-14"}},
		{"a whole month collapses to one token", day(2026, 7, 1), day(2026, 8, 1),
			[]string{"m2026-07"}},
		{"a whole year collapses to one token", day(2026, 1, 1), day(2027, 1, 1),
			[]string{"y2026"}},
		{"two whole years", day(2025, 1, 1), day(2027, 1, 1),
			[]string{"y2025", "y2026"}},
		{"month + partial", day(2026, 7, 1), day(2026, 8, 3),
			[]string{"m2026-07", "d2026-08-01", "d2026-08-02"}},
		{"greedy: days up to a month boundary, then months", day(2026, 6, 29), day(2026, 9, 1),
			[]string{"d2026-06-29", "d2026-06-30", "m2026-07", "m2026-08"}},
		{"empty range", day(2026, 7, 12), day(2026, 7, 12), nil},
		{"inverted range", day(2026, 7, 12), day(2026, 7, 1), nil},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got := dateCover(tc.from, tc.to)
			if !reflect.DeepEqual(got, tc.want) {
				t.Fatalf("dateCover:\n got  %v\n want %v", got, tc.want)
			}
		})
	}
}

// The cover must be EXACT: every day in the range must be matched by some token
// the cover produced, and no day outside it may be.
func TestDateCoverIsExact(t *testing.T) {
	from, to := day(2024, 3, 15), day(2026, 7, 12)
	cover := map[string]bool{}
	for _, tok := range dateCover(from, to) {
		cover[tok] = true
	}

	matched := func(d time.Time) bool {
		for _, tok := range dateBuckets(d) {
			if cover[tok] {
				return true
			}
		}
		return false
	}

	// Every day inside [from, to) is covered.
	for d := from; d.Before(to); d = d.AddDate(0, 0, 1) {
		if !matched(d) {
			t.Fatalf("%s is inside the range but not covered", d.Format("2006-01-02"))
		}
	}
	// The boundaries just outside are not.
	for _, d := range []time.Time{from.AddDate(0, 0, -1), to, to.AddDate(0, 0, 1)} {
		if matched(d) {
			t.Fatalf("%s is outside the range but matched", d.Format("2006-01-02"))
		}
	}
	// And the cover is small: a two-and-a-half year range must not degenerate to
	// ~850 day tokens.
	if n := len(dateCover(from, to)); n > 60 {
		t.Fatalf("cover is too large: %d tokens", n)
	}
}

// An open-ended range still terminates and still covers the interesting days.
func TestDateCoverOpenEnded(t *testing.T) {
	after := dateCover(day(2026, 7, 1), time.Time{})
	if len(after) == 0 {
		t.Fatal("after: empty cover")
	}
	before := dateCover(time.Time{}, day(1971, 1, 1))
	if !reflect.DeepEqual(before, []string{"y1970"}) {
		t.Fatalf("before: got %v", before)
	}
}

func TestDateBuckets(t *testing.T) {
	got := dateBuckets(time.Date(2026, 7, 12, 15, 4, 5, 0, time.UTC))
	want := []string{"d2026-07-12", "m2026-07", "y2026"}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("got %v, want %v", got, want)
	}
	if dateBuckets(time.Time{}) != nil {
		t.Error("a zero time has no buckets")
	}
}

func TestDateRangeNodeEmptyIsNone(t *testing.T) {
	if _, ok := dateRangeNode(DateRange{From: day(2026, 7, 12), To: day(2026, 7, 12)}).(None); !ok {
		t.Fatal("an empty range must compile to None (match nothing), not be dropped")
	}
}
