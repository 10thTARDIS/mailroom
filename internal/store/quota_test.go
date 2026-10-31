// SPDX-License-Identifier: AGPL-3.0-only

package store

import "testing"

// QuotaAdmits is the single definition of the quota allow/deny rule (api.Quota
// and internal/ingest both route through it). Its edges are load-bearing: the
// unlimited (limit <= 0) branch and the delta <= 0 (dedup / delete / no-op)
// branch each admit unconditionally, and getting either wrong would block all
// ingest or enforce nothing.
func TestQuotaAdmits(t *testing.T) {
	cases := []struct {
		name               string
		limit, used, delta int64
		want               bool
	}{
		// Unlimited: a non-positive limit is the schema's "no cap" — admits any
		// delta, including one that would be enormous under a real limit.
		{"unlimited zero admits anything", 0, 1 << 40, 1 << 40, true},
		{"unlimited negative admits anything", -1, 999, 1000, true},
		{"unlimited admits a zero delta", 0, 0, 0, true},

		// Dedup / delete / no-op: a non-positive delta always admits, even at or
		// past a finite limit — a message we already store adds no bytes.
		{"zero delta at the limit admits", 100, 100, 0, true},
		{"zero delta over the limit admits", 100, 200, 0, true},
		{"negative delta admits (a delete)", 100, 100, -50, true},

		// Finite limit: the addition must stay within it.
		{"well under the limit admits", 100, 10, 20, true},
		{"exactly at the limit admits", 100, 60, 40, true},
		{"one byte over the limit refuses", 100, 60, 41, false},
		{"already at the limit, any positive delta refuses", 100, 100, 1, false},
		{"already over the limit refuses", 100, 150, 1, false},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := QuotaAdmits(c.limit, c.used, c.delta); got != c.want {
				t.Errorf("QuotaAdmits(limit=%d, used=%d, delta=%d) = %v, want %v",
					c.limit, c.used, c.delta, got, c.want)
			}
		})
	}
}
