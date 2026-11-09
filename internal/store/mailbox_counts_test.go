// SPDX-License-Identifier: AGPL-3.0-only

package store

import (
	"strings"
	"testing"
)

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

// The id restriction must reach every branch of the membership derivation: one
// branch left unrestricted reads the user's whole mail per call (ADR-052).
func TestMembershipOfMessagesRestrictsEveryBranch(t *testing.T) {
	branches := strings.Count(effectiveMembershipSQL, "UNION ALL") + 1
	if got := strings.Count(membershipOfMessagesSQL, "m.id = ANY($2)"); got != branches {
		t.Fatalf("messages restricted %d times for %d branches:\n%s", got, branches, membershipOfMessagesSQL)
	}
	if got := strings.Count(membershipOfMessagesSQL, "p.message_id = ANY($2)"); got != 1 {
		t.Fatalf("provenance restricted %d times, want 1:\n%s", got, membershipOfMessagesSQL)
	}
}
