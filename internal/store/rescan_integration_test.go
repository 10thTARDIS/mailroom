// SPDX-License-Identifier: AGPL-3.0-only

//go:build integration

package store

// The re-scan selection predicate (#395, #396, ADR-027 §5c/§13).
//
// ⚠ THIS PREDICATE HAS BEEN WRONG TWICE, IN THE SAME WAY BOTH TIMES: a row that
// needed re-scanning did not match, the job ran, logged a successful pass, and
// did nothing. Neither defect could produce an error, and neither had a test
// until now.
//
//  1. ADR-027 §13 and #395 both specified `scanned_db_version < current`, which
//     is NULL — not true — for a never-scanned row. It would have skipped the
//     entire 255-attachment backlog the increment existed to clear. Caught by
//     measuring against the live database rather than by a test.
//  2. #396 added `part_hash`, written by the same pass that writes a verdict. So
//     every row from before that migration has a verdict and NO DIGEST — and a
//     release is keyed on the digest. Those rows can be quarantined and cannot
//     be released: a gate with no key. They are stamped at the current version,
//     so the version predicate does not reach them either; the backfill would
//     have arrived whenever freshclam next moved the number, which is luck about
//     an external timer rather than a design.
//
// Both arms are asserted below, each with the control that says the fixture
// could have failed.

import (
	"context"
	"crypto/sha256"
	"testing"
)

// TestRescanPredicateFindsRowsMissingAVerdictOrADigest pins both defects.
func TestRescanPredicateFindsRowsMissingAVerdictOrADigest(t *testing.T) {
	h := newHarness(t)
	ctx := context.Background()
	a, _ := seedTwoUsers(t, h.super)
	s := h.appStore()
	aID := mustULID(t, a.ID)
	msgID := mustULID(t, a.MessageID)

	const currentVersion = 28110

	if err := s.UserTx(ctx, aID, func(tx *UserTx) error {
		return tx.ReplaceAttachments(ctx, msgID, []AttachmentRow{
			{PartPath: "2", ContentType: "application/pdf", SizeBytes: 11},
		})
	}); err != nil {
		t.Fatalf("project: %v", err)
	}

	due := func() int {
		t.Helper()
		var n int
		if err := s.UserTx(ctx, aID, func(tx *UserTx) error {
			var e error
			n, e = tx.CountMessagesDueForRescan(ctx, currentVersion)
			return e
		}); err != nil {
			t.Fatalf("count due: %v", err)
		}
		return n
	}

	// ── 1. A never-scanned row is due. `NULL < 28110` is NULL, not true. ────
	if got := due(); got != 1 {
		t.Fatalf("a never-scanned attachment is not due for re-scan (%d) — this is the defect that "+
			"would have skipped the whole backlog while the job logged success", got)
	}

	// ── 2. Stamp it at the CURRENT version, with a digest: not due. ─────────
	// This is the control for both arms below. If it stayed due, everything
	// after it would pass for the wrong reason.
	hash := sha256.Sum256([]byte("part-2-bytes"))
	stamp := func(partHash []byte) {
		t.Helper()
		if _, err := h.super.Exec(ctx, `
			UPDATE app.attachments
			   SET scan_status='clean', scanned_at=now(),
			       scanned_db_version=$3, scanned_engine_version='1.5.4',
			       part_hash=$4
			 WHERE user_id=$1 AND message_id=$2 AND part_path='2'`,
			a.ID, msgID.String(), currentVersion, partHash); err != nil {
			t.Fatalf("stamp: %v", err)
		}
	}
	stamp(hash[:])
	if got := due(); got != 0 {
		t.Fatalf("CONTROL FAILED: a row stamped at the current version WITH a digest is still due "+
			"(%d), so the assertions below cannot distinguish anything", got)
	}

	// ── 3. Same row, digest removed: due again. ─────────────────────────────
	// The #396 arm. Nothing about the version changed, so only the part_hash
	// clause can produce this.
	stamp(nil)
	if got := due(); got != 1 {
		t.Errorf("a row with a verdict but NO CONTENT DIGEST is not due (%d) — it can be "+
			"quarantined and cannot be released, and nothing would ever fill the digest in "+
			"until freshclam happened to move the version", got)
	}

	// ── 4. And it is not merely counted — it is SELECTED. ───────────────────
	// The count and the select are separate queries over the same predicate,
	// and the job uses one to act and the other to report what is left. A
	// disagreement would make "remaining" a number about a different
	// population, which is worse than no number.
	var targets []RescanTarget
	if err := s.UserTx(ctx, aID, func(tx *UserTx) error {
		var e error
		targets, e = tx.MessagesDueForRescan(ctx, currentVersion, 10)
		return e
	}); err != nil {
		t.Fatalf("select due: %v", err)
	}
	if len(targets) != 1 || targets[0].MessageID.Compare(msgID) != 0 {
		t.Errorf("MessagesDueForRescan returned %d target(s), want the one message the count "+
			"agrees is due — the two predicates have drifted", len(targets))
	}

	// ── 5. A version BEHIND current is due, digest or no digest. ────────────
	// The ordinary case, asserted so a future edit cannot satisfy 3 and 4 by
	// deleting the version arm.
	stamp(hash[:])
	if _, err := h.super.Exec(ctx,
		`UPDATE app.attachments SET scanned_db_version=$3 WHERE user_id=$1 AND message_id=$2 AND part_path='2'`,
		a.ID, msgID.String(), currentVersion-1); err != nil {
		t.Fatalf("age the version: %v", err)
	}
	if got := due(); got != 1 {
		t.Errorf("a row stamped at an OLDER signature version is not due (%d)", got)
	}

	// ── 6. ⚠ A MISSING DIGEST IGNORES THE TIER GATE. ────────────────────────
	// The case the first version of this predicate got wrong, and the one the
	// test above could not see because its message is minutes old and therefore
	// in the strictest tier, which is due whenever the version moves.
	//
	// Aged to four months, the message sits in the weekly tier. Stamped at the
	// CURRENT version and scanned a minute ago, so the tier is NOT due and the
	// version is not stale — the only thing that can select it is the absent
	// digest. Measured on the deployed instance rather than imagined: after
	// migration 00044 one pass filled 41 of 268 digests and stopped, leaving 227
	// rows on older mail blockable and unreleasable for up to a week.
	if _, err := h.super.Exec(ctx,
		`UPDATE app.messages SET received_at = now() - interval '120 days' WHERE id = $1`,
		msgID.String()); err != nil {
		t.Fatalf("age the message: %v", err)
	}
	stamp(hash[:])
	if got := due(); got != 0 {
		t.Fatalf("CONTROL FAILED: a four-month-old message stamped at the current version is "+
			"already due (%d), so the assertion below cannot show the tier being bypassed", got)
	}
	stamp(nil)
	if got := due(); got != 1 {
		t.Errorf("a four-month-old row with NO DIGEST is not due (%d) — the digest backfill is "+
			"rationed by the re-scan cadence, so a quarantined attachment on older mail cannot "+
			"be released for up to a week. A missing digest is not a freshness question: it is a "+
			"fact about bytes that is simply absent", got)
	}
}
