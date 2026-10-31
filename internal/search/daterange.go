// SPDX-License-Identifier: AGPL-3.0-only

package search

import (
	"fmt"
	"time"
)

// Date filtering without numeric fields.
//
// The tantivy-go binding's schema builder exposes TEXT fields only — there is no
// i64/date field and therefore no range query (verified against v1.0.6; see
// docs/search-spike.md). Rather than fake ranges by over-fetching and filtering
// after top-K (which silently loses recall: a date-filtered query would return
// fewer hits than asked for, and the "missing" ones would be the ones that
// ranked just outside the window), Mailroom's AST expands a date range into an
// EXACT set of calendar-bucket tokens.
//
// Index time: every document carries three token families derived from its
// received_at in UTC —
//
//	d2026-07-12   the day
//	m2026-07      the month
//	y2026         the year
//
// Query time: [from, to) is decomposed into the MINIMAL cover of whole years,
// whole months, and leftover days, and matched as an OR over those tokens. The
// result is exact — no over-fetch, no post-filter, no lost recall — and the
// clause count is bounded: at most ~30 days + ~11 months at each end, plus one
// token per whole year in between (a decade-wide range is ~70 tokens, which any
// engine shrugs at).
//
// The cost of the scheme is DAY granularity: a range boundary is rounded to the
// UTC day. That is the right granularity for mail search (`before:2026-07-01`),
// it is what the operator syntax exposes, and JMAP's `before`/`after` — which do
// carry a timestamp — are additionally enforced exactly by Postgres in the
// Email/query path, so the API's contract is not weakened by it.

// dateBuckets returns the bucket tokens a document with this received_at carries.
func dateBuckets(t time.Time) []string {
	if t.IsZero() {
		return nil
	}
	u := t.UTC()
	return []string{
		fmt.Sprintf("d%04d-%02d-%02d", u.Year(), int(u.Month()), u.Day()),
		fmt.Sprintf("m%04d-%02d", u.Year(), int(u.Month())),
		fmt.Sprintf("y%04d", u.Year()),
	}
}

func dayToken(t time.Time) string {
	u := t.UTC()
	return fmt.Sprintf("d%04d-%02d-%02d", u.Year(), int(u.Month()), u.Day())
}

func monthToken(t time.Time) string {
	u := t.UTC()
	return fmt.Sprintf("m%04d-%02d", u.Year(), int(u.Month()))
}

func yearToken(t time.Time) string {
	return fmt.Sprintf("y%04d", t.UTC().Year())
}

// minDateYear/maxDateYear bound an open-ended range so the decomposition always
// terminates. Mail predates neither.
const (
	minDateYear = 1970
	maxDateYear = 2100
)

// dateCover returns the minimal set of bucket tokens covering [from, to) at day
// granularity, or nil when the range is empty. from is inclusive, to exclusive;
// a zero bound is open-ended (clamped to minDateYear/maxDateYear).
//
// The algorithm is the classic greedy calendar cover: from the low end, take the
// largest whole unit (year, then month, then day) that still fits entirely below
// `to`, and advance.
func dateCover(from, to time.Time) []string {
	lo := truncDay(from)
	hi := truncDay(to)
	if from.IsZero() {
		lo = time.Date(minDateYear, 1, 1, 0, 0, 0, 0, time.UTC)
	}
	if to.IsZero() {
		hi = time.Date(maxDateYear, 1, 1, 0, 0, 0, 0, time.UTC)
	}
	// Explicit bounds outside [minDateYear, maxDateYear) are clamped too: no mail
	// is dated there, and an unclamped `after:0001-01-01` (or a relative date far
	// in the past) would otherwise emit one token per year back to year 1.
	if floor := time.Date(minDateYear, 1, 1, 0, 0, 0, 0, time.UTC); lo.Before(floor) {
		lo = floor
	}
	if ceil := time.Date(maxDateYear, 1, 1, 0, 0, 0, 0, time.UTC); hi.After(ceil) {
		hi = ceil
	}
	// `to` is exclusive at day granularity: a document received at any time on
	// the boundary day is outside the range only if hi is that day's midnight.
	if !hi.After(lo) {
		return nil
	}

	var out []string
	cur := lo
	for cur.Before(hi) {
		switch {
		case isYearStart(cur) && !addYear(cur).After(hi):
			out = append(out, yearToken(cur))
			cur = addYear(cur)
		case isMonthStart(cur) && !addMonth(cur).After(hi):
			out = append(out, monthToken(cur))
			cur = addMonth(cur)
		default:
			out = append(out, dayToken(cur))
			cur = cur.AddDate(0, 0, 1)
		}
	}
	return out
}

func truncDay(t time.Time) time.Time {
	u := t.UTC()
	return time.Date(u.Year(), u.Month(), u.Day(), 0, 0, 0, 0, time.UTC)
}

func isYearStart(t time.Time) bool  { return t.Month() == time.January && t.Day() == 1 }
func isMonthStart(t time.Time) bool { return t.Day() == 1 }

func addYear(t time.Time) time.Time  { return t.AddDate(1, 0, 0) }
func addMonth(t time.Time) time.Time { return t.AddDate(0, 1, 0) }

// dateRangeNode compiles a DateRange into a bucket-token Or. An empty range
// compiles to None (matches nothing) rather than being dropped.
func dateRangeNode(r DateRange) Node {
	tokens := dateCover(r.From, r.To)
	if len(tokens) == 0 {
		return None{}
	}
	branches := make([]Node, 0, len(tokens))
	for _, tok := range tokens {
		branches = append(branches, Term{Field: FieldDate, Value: tok, Match: MatchExact, Boost: 1})
	}
	if len(branches) == 1 {
		return branches[0]
	}
	return Or{Nodes: branches}
}
