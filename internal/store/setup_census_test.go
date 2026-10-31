// SPDX-License-Identifier: AGPL-3.0-only

package store

import "testing"

// The census's two PURE pieces, tested where they can be falsified — no
// database, just the arithmetic and the ordering.
//
// classifyShape's ORDER is the whole definition, so the cases below are chosen
// to make the order observable rather than merely to cover each branch: a folder
// that satisfies TWO tests at once proves which one wins.

func TestClassifyShape(t *testing.T) {
	cases := []struct {
		name string
		in   FolderCensus
		want string
	}{
		{"an empty folder reports empty rather than a shape",
			FolderCensus{Live: 0, DistinctDomains: 0}, ShapeEmpty},
		{"one domain, whatever the sender count",
			FolderCensus{Live: 20, DistinctDomains: 1, DistinctSenders: 9, SendersFor90: 9}, ShapeOneDomain},
		{"a handful of senders across several domains",
			FolderCensus{Live: 20, DistinctDomains: 5, DistinctSenders: 5, SendersFor90: 5}, ShapeFewSenders},
		{"six senders for 90% is not a handful",
			FolderCensus{Live: 20, DistinctDomains: 6, DistinctSenders: 6, SendersFor90: 6}, ShapeMixed},
		{"overwhelmingly list mail",
			FolderCensus{Live: 10, DistinctDomains: 8, DistinctSenders: 8, SendersFor90: 8, BulkShare: 0.9}, ShapeBulk},
		{"exactly at the bulk threshold counts as bulk",
			FolderCensus{Live: 10, DistinctDomains: 8, SendersFor90: 8, BulkShare: 0.8}, ShapeBulk},
		{"just under it does not",
			FolderCensus{Live: 10, DistinctDomains: 8, SendersFor90: 8, BulkShare: 0.79}, ShapeMixed},
		{"mixed is the honest default",
			FolderCensus{Live: 12, DistinctDomains: 12, DistinctSenders: 12, SendersFor90: 11}, ShapeMixed},

		// ── THE ORDER, made observable ────────────────────────────────────────
		// Each of these satisfies more than one test; the expected answer names
		// which test wins, which is the part a reordering would break silently.
		{"one domain BEATS bulk: a newsletter from a single domain is described by the domain",
			FolderCensus{Live: 10, DistinctDomains: 1, SendersFor90: 1, BulkShare: 1.0}, ShapeOneDomain},
		{"few senders BEATS bulk: five newsletters is still five senders",
			FolderCensus{Live: 10, DistinctDomains: 4, SendersFor90: 3, BulkShare: 1.0}, ShapeFewSenders},
		{"empty BEATS everything: a zero-message folder has no shape to report",
			FolderCensus{Live: 0, DistinctDomains: 1, SendersFor90: 1, BulkShare: 1.0}, ShapeEmpty},

		// SendersFor90 == 0 means "no sender concentration is knowable" (every
		// message lacks a From address), NOT "one sender covers everything". A
		// folder like that must not read as concentrated.
		{"an entirely unclassifiable folder is mixed, not fewSenders",
			FolderCensus{Live: 6, Unclassified: 6, DistinctDomains: 0, SendersFor90: 0}, ShapeMixed},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := classifyShape(tc.in); got != tc.want {
				t.Errorf("classifyShape(%+v) = %q, want %q", tc.in, got, tc.want)
			}
		})
	}
}

func TestSendersForCoverage(t *testing.T) {
	cases := []struct {
		name   string
		counts []int
		total  int
		want   int
	}{
		{"one sender covers everything", []int{6}, 6, 1},
		{"the biggest two of four reach 90%", []int{10, 8, 1, 1}, 20, 2},
		{"an even spread needs almost all of them", []int{1, 1, 1, 1, 1, 1, 1, 1, 1, 1}, 10, 9},
		{"input order does not matter", []int{1, 1, 8, 10}, 20, 2},
		// THE DENOMINATOR IS THE WHOLE FOLDER, including mail with no sender: six
		// messages from one sender out of twelve is NOT one sender covering 90%.
		{"unclassified mail is in the denominator", []int{6}, 12, 1},
		{"nothing to cover reports 0, not 1", nil, 6, 0},
		{"an empty folder reports 0", []int{}, 0, 0},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := sendersForCoverage(tc.counts, tc.total, shapeCoverage); got != tc.want {
				t.Errorf("sendersForCoverage(%v, %d) = %d, want %d", tc.counts, tc.total, got, tc.want)
			}
		})
	}
}

// setupCensusRoleIncluded is the "mapped without asking" rule, and it is one
// line, which is exactly why it is worth pinning: adding a role to
// roleForSpecialUse without deciding whether day-one should offer it is how a
// wizard would start proposing to label everything in Sent.
func TestSetupCensusRoleIncluded(t *testing.T) {
	for _, role := range []string{"", "inbox"} {
		if !setupCensusRoleIncluded(role) {
			t.Errorf("role %q must get a census row", role)
		}
	}
	for _, role := range []string{"drafts", "sent", "trash", "archive", "junk", "all", "flagged"} {
		if setupCensusRoleIncluded(role) {
			t.Errorf("role %q must NOT get a census row — special-use folders are mapped without asking", role)
		}
	}
}
