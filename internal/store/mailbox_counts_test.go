// SPDX-License-Identifier: AGPL-3.0-only

package store

import "testing"

func TestCollapseChangesMarksCountOnlyMailboxes(t *testing.T) {
	x, y := NewULID(), NewULID()
	page := collapseChanges([]changeEntry{
		{seq: 1, objectID: x, op: OpUpdated, countsOnly: true},
		{seq: 2, objectID: x, op: OpUpdated, countsOnly: true},
		{seq: 3, objectID: y, op: OpUpdated, countsOnly: true},
		{seq: 4, objectID: y, op: OpUpdated}, // a rename in the same window
	}, 0, 4, 0)
	if !page.UpdatedCountsOnly[x] || page.UpdatedCountsOnly[y] {
		t.Fatalf("count-only marks = %v, want x only", page.UpdatedCountsOnly)
	}
}
