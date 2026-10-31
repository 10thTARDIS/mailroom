// SPDX-License-Identifier: AGPL-3.0-only

package store

import (
	"strings"
	"testing"
)

// #859: the collapse's provenance order. The user's own filing must rank first,
// and every attribution the store writes must be ranked — one missing would fall
// to "unknown" and lose to everything, including desk and setup.
func TestCollapseProvenanceOrder(t *testing.T) {
	if collapseProvenanceOrder[0] != LabelByUser {
		t.Fatalf("strongest attribution = %q, want %q", collapseProvenanceOrder[0], LabelByUser)
	}
	all := []string{LabelByUser, LabelByRule, LabelByML, LabelByAgent, LabelByDesk, LabelBySetup, LabelByLearned}
	seen := map[string]bool{}
	for _, by := range collapseProvenanceOrder {
		if seen[by] {
			t.Errorf("%q ranked twice", by)
		}
		seen[by] = true
	}
	for _, by := range all {
		if !seen[by] {
			t.Errorf("%q is not ranked", by)
		}
	}
	sql := collapseProvenanceRankSQL("x.applied_by")
	if !strings.HasPrefix(sql, "(CASE x.applied_by WHEN 'user' THEN 0 ") ||
		!strings.HasSuffix(sql, " ELSE 7 END)") {
		t.Errorf("rank SQL = %s", sql)
	}
}
