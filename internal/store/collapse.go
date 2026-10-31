// SPDX-License-Identifier: AGPL-3.0-only

package store

import (
	"context"
	"errors"
	"fmt"
	"strings"

	"github.com/jackc/pgx/v5"
	"github.com/oklog/ulid/v2"
)

// COLLAPSING MAIL THAT WAS ALREADY DUPLICATED BEFORE THE FIX (#206 part 3).
//
// Migration 00023 + ingest stop NEW re-deliveries becoming a second message. This is the
// repair for rows that were already there: a few dozen of them on a real mailbox, listed
// three and four times in the inbox.
//
// IT DELETES MESSAGE ROWS FROM THE FK HUB, which ADR-001 calls the most expensive thing in
// this schema to get wrong, so the reasoning is written out rather than left to be
// reconstructed from the SQL.
//
// TWELVE FOREIGN KEYS reference app.messages, ten of them ON DELETE CASCADE. A plain
// DELETE would therefore destroy the losers' provenance and their labels along with the
// rows. What each one needs was decided by MEASURING the real data first, not by
// reasoning about it:
//
//	message_provenance   28 rows   MOVED to the keeper — this is the whole point
//	message_labels       25 rows   UNIONED onto the keeper (0 were unique to a loser,
//	                               but the code unions anyway; that number is data, not
//	                               a guarantee, and the next mailbox will differ) —
//	                               WITH their provenance (#859), see step 2
//	feedback_events        1 row   RE-POINTED — a correction the user made by hand, and
//	                               training data; cascading it away would silently
//	                               un-teach the model
//	learned_placements    re-pointed to the keeper (#882), one row per message: if
//	                      several copies have one, a CORRECTED row wins, then the
//	                      keeper's, then the earliest - see carryLearnedPlacements
//	setup_journal         re-pointed to the keeper (#882), merged per setup run:
//	                      day-one undo needs it to find the label it wrote - see
//	                      carryDayOneJournal. Two copies journaled with DIFFERENT
//	                      labels in one run cannot share a row, and BLOCK the group.
//	writeback_intents     31 rows  all `applied`, i.e. history. Cascade is correct for
//	                               those — but a PENDING one is queued IMAP work, so a
//	                               loser holding one BLOCKS the collapse (below)
//	triage_scores         28 rows  cascade; a model output, recomputable by `rescore`
//	submissions            0 rows  a loser referenced by something SENT blocks the
//	                               collapse: that link is the record of what we mailed
//	attachments/dsn/…      0 rows  nothing to move
//
// AND THE THING THE FIRST DESIGN MISSED: each loser has its OWN content_hash, because
// differing bytes is precisely why it became a separate row. Deleting the message leaves
// its app.blobs row and its file behind — orphaned ciphertext on a real mailbox.
// So the blob is reclaimed too, on exactly the terms HardDeleteMessage uses.
//
// FAIL CLOSED. Any loser this cannot fully account for blocks its whole group, and the
// group is reported rather than partially collapsed. A half-merged group is worse than an
// un-merged one: the duplicate is still visible AND its provenance has moved.

// CollapseLoser is one redundant copy, with everything that references it.
type CollapseLoser struct {
	ID          ulid.ULID
	ContentHash []byte
	// ThreadID is the thread this copy sits in. Today each duplicate owns its own
	// thread (#210), so collapsing usually empties one.
	ThreadID ulid.ULID

	ProvenanceRows int
	// LabelsToUnion is how many of this loser's labels the KEEPER does not already
	// have — the labelling that would actually be lost by a naive delete.
	LabelsToUnion int
	// LabelsToUpgrade is how many labels BOTH copies carry where this loser's
	// attribution is the stronger one (collapseProvenanceOrder, #859) — most
	// importantly a 'user' filing on the copy while the keeper's row says some
	// automation applied it. The keeper's row takes the loser's attribution.
	LabelsToUpgrade int
	FeedbackEvents  int

	// LearnedPlacements is how many learned-placement log rows this copy holds
	// (0 or 1: the key is (user, message)). They used to cascade away with the copy
	// (#882), so a correction on the keeper could not find its placement.
	// LearnedPlacementConflicts counts those where the KEEPER also has a row, and
	// LearnedCorrectionsWin those where this copy's row is a correction that outranks
	// the keeper's (so the keeper's row is replaced rather than kept).
	LearnedPlacements         int
	LearnedPlacementConflicts int
	LearnedCorrectionsWin     int
	// SetupJournalRows is how many day-one undo journal rows this copy holds (one per
	// setup run it was touched by). SetupJournalConflicts counts those where another
	// copy in the group has a row for the SAME run, to be merged into one;
	// SetupJournalLabelClashes counts those where the two rows name DIFFERENT labels -
	// they cannot share the one row the unique index allows, and BLOCK the group.
	SetupJournalRows         int
	SetupJournalConflicts    int
	SetupJournalLabelClashes int

	// PendingIntents and Submissions are BLOCKERS, not payload. Non-zero means this
	// copy is still referenced by work in flight or by a record of something sent.
	PendingIntents int
	Submissions    int
}

// Blocked reports whether this copy may not be collapsed, and why.
func (l CollapseLoser) Blocked() string {
	switch {
	case l.SetupJournalLabelClashes > 0:
		return fmt.Sprintf("%d day-one journal row(s) name a different label than another copy's row for the same run - one row per message per run cannot hold both, and merging would make undo skip a label", l.SetupJournalLabelClashes)
	case l.PendingIntents > 0:
		return fmt.Sprintf("%d writeback intent(s) not yet applied — collapsing would cancel queued IMAP work", l.PendingIntents)
	case l.Submissions > 0:
		return fmt.Sprintf("%d submission(s) reference it — that is the record of something we sent", l.Submissions)
	default:
		return ""
	}
}

// CollapseCandidate is one group of copies of the same message: the copy to keep and the
// ones to fold into it.
type CollapseCandidate struct {
	DedupHash []byte
	// KeeperID is the OLDEST copy. ULIDs sort by creation time, so it is the one that
	// arrived first — the copy whose received_at is the true arrival and whose id other
	// rows are likeliest to already reference.
	KeeperID ulid.ULID
	Losers   []CollapseLoser
}

// Blockers returns one line per copy that cannot be collapsed. A non-empty result means
// the whole group is skipped.
func (c CollapseCandidate) Blockers() []string {
	var out []string
	for _, l := range c.Losers {
		if why := l.Blocked(); why != "" {
			out = append(out, fmt.Sprintf("%s: %s", l.ID, why))
		}
	}
	return out
}

// DuplicateCollapseCandidates reports the groups that could be collapsed, worst first.
// READ-ONLY: it is what the dry run prints, and what an operator reads before deciding.
func (tx *UserTx) DuplicateCollapseCandidates(ctx context.Context, limit int) ([]CollapseCandidate, error) {
	if limit <= 0 {
		limit = 200
	}
	rows, err := tx.tx.Query(ctx, `
		WITH dup AS (
		  SELECT dedup_hash, count(*) AS copies
		    FROM app.messages
		   WHERE user_id = $1 AND dedup_hash IS NOT NULL
		   GROUP BY dedup_hash
		  HAVING count(*) > 1
		   ORDER BY count(*) DESC, dedup_hash
		   LIMIT $2
		),
		ranked AS (
		  SELECT m.id, m.dedup_hash, m.content_hash, COALESCE(m.thread_id, '') AS thread_id,
		         row_number() OVER (PARTITION BY m.dedup_hash ORDER BY m.id) AS rn
		    FROM app.messages m
		    JOIN dup ON dup.dedup_hash = m.dedup_hash
		   WHERE m.user_id = $1
		),
		keeper AS (SELECT dedup_hash, id FROM ranked WHERE rn = 1)
		SELECT r.dedup_hash, k.id AS keeper_id, r.id, r.content_hash, r.thread_id,
		       (SELECT count(*) FROM app.message_provenance p
		         WHERE p.user_id = $1 AND p.message_id = r.id),
		       (SELECT count(*) FROM app.message_labels ml
		         WHERE ml.user_id = $1 AND ml.message_id = r.id
		           AND NOT EXISTS (SELECT 1 FROM app.message_labels km
		                            WHERE km.user_id = $1 AND km.message_id = k.id
		                              AND km.label_id = ml.label_id)),
		       (SELECT count(*) FROM app.message_labels ml
		          JOIN app.message_labels km
		            ON km.user_id = $1 AND km.message_id = k.id AND km.label_id = ml.label_id
		         WHERE ml.user_id = $1 AND ml.message_id = r.id
		           AND `+collapseProvenanceRankSQL("ml.applied_by")+` < `+collapseProvenanceRankSQL("km.applied_by")+`),
		       (SELECT count(*) FROM app.feedback_events fe
		         WHERE fe.user_id = $1 AND fe.message_id = r.id),
		       (SELECT count(*) FROM app.writeback_intents wi
		         WHERE wi.user_id = $1 AND wi.message_id = r.id AND wi.state <> 'applied'),
		       (SELECT count(*) FROM app.submissions s
		         WHERE s.user_id = $1 AND s.message_id = r.id),
		       (SELECT count(*) FROM app.learned_placements lp
		         WHERE lp.user_id = $1 AND lp.message_id = r.id),
		       (SELECT count(*) FROM app.learned_placements lp
		          JOIN app.learned_placements kp
		            ON kp.user_id = $1 AND kp.message_id = k.id
		         WHERE lp.user_id = $1 AND lp.message_id = r.id),
		       (SELECT count(*) FROM app.learned_placements lp
		          JOIN app.learned_placements kp
		            ON kp.user_id = $1 AND kp.message_id = k.id
		         WHERE lp.user_id = $1 AND lp.message_id = r.id
		           AND lp.outcome = 'corrected' AND kp.outcome <> 'corrected'),
		       (SELECT count(*) FROM app.setup_journal j
		         WHERE j.user_id = $1 AND j.message_id = r.id),
		       (SELECT count(*) FROM app.setup_journal j
		         WHERE j.user_id = $1 AND j.message_id = r.id
		           AND EXISTS (SELECT 1 FROM ranked r2
		                         JOIN app.setup_journal j2
		                           ON j2.user_id = $1 AND j2.message_id = r2.id
		                        WHERE r2.dedup_hash = r.dedup_hash AND r2.id <> r.id
		                          AND j2.run_id = j.run_id)),
		       (SELECT count(*) FROM app.setup_journal j
		         WHERE j.user_id = $1 AND j.message_id = r.id AND j.label_id IS NOT NULL
		           AND EXISTS (SELECT 1 FROM ranked r2
		                         JOIN app.setup_journal j2
		                           ON j2.user_id = $1 AND j2.message_id = r2.id
		                        WHERE r2.dedup_hash = r.dedup_hash AND r2.id <> r.id
		                          AND j2.run_id = j.run_id
		                          AND j2.label_id IS NOT NULL AND j2.label_id <> j.label_id))
		  FROM ranked r
		  JOIN keeper k ON k.dedup_hash = r.dedup_hash
		 WHERE r.rn > 1
		 ORDER BY r.dedup_hash, r.id`,
		tx.userID.String(), limit)
	if err != nil {
		return nil, fmt.Errorf("store: duplicate collapse candidates: %w", err)
	}
	defer rows.Close()

	var out []CollapseCandidate
	for rows.Next() {
		var (
			dedup            []byte
			keeperID, loseID string
			threadID         string
			l                CollapseLoser
		)
		if err := rows.Scan(&dedup, &keeperID, &loseID, &l.ContentHash, &threadID,
			&l.ProvenanceRows, &l.LabelsToUnion, &l.LabelsToUpgrade, &l.FeedbackEvents,
			&l.PendingIntents, &l.Submissions,
			&l.LearnedPlacements, &l.LearnedPlacementConflicts, &l.LearnedCorrectionsWin,
			&l.SetupJournalRows, &l.SetupJournalConflicts, &l.SetupJournalLabelClashes); err != nil {
			return nil, fmt.Errorf("store: scan collapse candidate: %w", err)
		}
		if l.ID, err = ulid.Parse(loseID); err != nil {
			return nil, fmt.Errorf("store: collapse loser id %q: %w", loseID, err)
		}
		if threadID != "" {
			if l.ThreadID, err = ulid.Parse(threadID); err != nil {
				return nil, fmt.Errorf("store: collapse thread id %q: %w", threadID, err)
			}
		}
		keeper, err := ulid.Parse(keeperID)
		if err != nil {
			return nil, fmt.Errorf("store: collapse keeper id %q: %w", keeperID, err)
		}
		// One row per LOSER, already grouped and ordered by the query, so groups are
		// built by watching the hash change rather than with a query per group.
		if n := len(out); n > 0 && string(out[n-1].DedupHash) == string(dedup) {
			out[n-1].Losers = append(out[n-1].Losers, l)
			continue
		}
		out = append(out, CollapseCandidate{DedupHash: dedup, KeeperID: keeper, Losers: []CollapseLoser{l}})
	}
	return out, rows.Err()
}

// CollapseResult reports what one group's collapse did.
type CollapseResult struct {
	ProvenanceMoved int
	LabelsUnioned   int
	// LabelsUpgraded counts keeper label rows whose attribution was replaced by a
	// loser's stronger one (#859) — e.g. the keeper said 'learned' and the copy
	// carried the user's own filing.
	LabelsUpgraded int
	FeedbackMoved  int
	// PlacementsMoved is 1 when a copy's learned-placement row became the keeper's;
	// PlacementsReplaced when that displaced a row the keeper already had (a
	// correction outranking it); PlacementsDropped counts rows discarded because
	// another copy's row won. JournalMoved counts day-one journal rows re-pointed,
	// JournalMerged the copies' rows folded into a row that already existed for the run.
	PlacementsMoved    int
	PlacementsReplaced int
	PlacementsDropped  int
	JournalMoved       int
	JournalMerged      int
	MessagesDeleted    int
	ThreadsDeleted     int
	// ReclaimBlobs are the content hashes whose app.blobs row was removed because
	// nothing references those bytes any more. THE CALLER MUST DELETE EACH FILE INSIDE
	// THIS TRANSACTION, before it commits — see HardDeleteMessage and
	// purge.Worker.hardDelete for why that ordering is the race proof.
	ReclaimBlobs [][]byte
}

// CollapseDuplicateGroup folds every loser into the keeper, in one transaction.
//
// Order matters and is not arbitrary: everything worth keeping is MOVED first, and only
// then are the rows deleted. Done the other way round, a cascade would have already taken
// what the move was for.
//
// Returns ok=false without changing anything when the group is not, right now, safe to
// collapse — the same fail-closed discipline as HardDeleteMessage. The check is repeated
// here under the row lock rather than trusted from the report, because a report is a
// snapshot and an intent can be enqueued between reading it and acting on it.
func (tx *UserTx) CollapseDuplicateGroup(
	ctx context.Context, keeperID ulid.ULID, losers []ulid.ULID,
) (res CollapseResult, ok bool, err error) {
	if len(losers) == 0 {
		return res, false, nil
	}
	loserStrs := make([]string, len(losers))
	for i, l := range losers {
		if l == keeperID {
			// Refusing rather than silently skipping: being asked to fold a message
			// into itself means the caller computed the group wrongly, and the next
			// step would delete the keeper.
			return res, false, fmt.Errorf("store: collapse: keeper %s listed as its own loser", keeperID)
		}
		loserStrs[i] = l.String()
	}

	// LOCK THE ROWS FIRST, keeper included, and read what will be needed AFTER they are
	// gone. Without the keeper locked, a concurrent hard delete of it would leave the
	// moved provenance pointing at nothing; without reading the content hashes and
	// thread ids now, step 5 and step 6 could not find them — the rows they live on are
	// about to be deleted.
	rows, err := tx.tx.Query(ctx, `
		SELECT id, content_hash, COALESCE(thread_id, '')
		  FROM app.messages
		 WHERE user_id = $1 AND (id = $2 OR id = ANY($3))
		 ORDER BY id
		 FOR UPDATE`,
		tx.userID.String(), keeperID.String(), loserStrs)
	if err != nil {
		return res, false, fmt.Errorf("store: collapse (lock): %w", err)
	}
	var (
		locked       int
		loserHashes  [][]byte
		loserThreads []string
	)
	for rows.Next() {
		var id, threadID string
		var hash []byte
		if err := rows.Scan(&id, &hash, &threadID); err != nil {
			rows.Close()
			return res, false, fmt.Errorf("store: collapse (scan locked): %w", err)
		}
		locked++
		if id == keeperID.String() {
			continue
		}
		loserHashes = append(loserHashes, hash)
		if threadID != "" {
			loserThreads = append(loserThreads, threadID)
		}
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return res, false, fmt.Errorf("store: collapse (lock): %w", err)
	}
	if locked != len(losers)+1 {
		// Something vanished between the report and now. Do nothing; a later run
		// recomputes the group from what is actually there.
		return res, false, nil
	}

	// RE-CHECK THE BLOCKERS under the lock.
	var pending, submissions int
	if err := tx.tx.QueryRow(ctx, `
		SELECT (SELECT count(*) FROM app.writeback_intents
		         WHERE user_id = $1 AND message_id = ANY($2) AND state <> 'applied'),
		       (SELECT count(*) FROM app.submissions
		         WHERE user_id = $1 AND message_id = ANY($2))`,
		tx.userID.String(), loserStrs).Scan(&pending, &submissions); err != nil {
		return res, false, fmt.Errorf("store: collapse (blockers): %w", err)
	}
	if pending > 0 || submissions > 0 {
		return res, false, nil
	}
	// The same fail-closed rule for the day-one journal (#882): two copies of the
	// group (keeper included) journaled by one run with DIFFERENT labels.
	var clashes int
	if err := tx.tx.QueryRow(ctx, `
		SELECT count(*) FROM (
		  SELECT run_id FROM app.setup_journal
		   WHERE user_id = $1 AND label_id IS NOT NULL
		     AND (message_id = $2 OR message_id = ANY($3))
		   GROUP BY run_id HAVING count(DISTINCT label_id) > 1) c`,
		tx.userID.String(), keeperID.String(), loserStrs).Scan(&clashes); err != nil {
		return res, false, fmt.Errorf("store: collapse (journal clashes): %w", err)
	}
	if clashes > 0 {
		return res, false, nil
	}

	// 1. PROVENANCE MOVES. This is the point of the whole operation: the keeper must
	//    end up knowing about every place the message physically is, or writeback and
	//    the expunge reaper would stop seeing the copies that are still on the server.
	//    No conflict is possible — provenance is unique per (mailbox, uidvalidity, uid)
	//    and each loser holds different coordinates.
	moved, err := tx.tx.Exec(ctx, `
		UPDATE app.message_provenance SET message_id = $2
		 WHERE user_id = $1 AND message_id = ANY($3)`,
		tx.userID.String(), keeperID.String(), loserStrs)
	if err != nil {
		return res, false, fmt.Errorf("store: collapse (move provenance): %w", err)
	}
	res.ProvenanceMoved = int(moved.RowsAffected())

	// 2. LABELS UNION, WITH PROVENANCE (#859). A label on any copy is a label this
	//    message carries, so it survives on the keeper — and so does WHO applied it.
	//
	//    Until #859 this inserted only (label_id, applied_by_rule_id), so applied_by
	//    fell to its column default, 'user': a learned, desk, rule, agent or setup row
	//    on a collapsed copy arrived on the keeper as the person's own filing. Learned
	//    placement (placementFilings), the label model and rule induction all read
	//    applied_by = 'user' as "the user decided this", so every collapse could
	//    manufacture placement evidence. All three provenance columns travel now:
	//    applied_by, applied_by_rule_id (which LabelSource.columns only ever writes
	//    beside 'rule'), and created_at — the time the label was really applied, which
	//    placementFilings uses as its as-of cut, rather than the moment of the collapse.
	//
	//    TWO ROWS FOR ONE LABEL. Several copies can carry the same label, and the
	//    keeper may already have it. DISTINCT ON picks one candidate per label (the
	//    strongest attribution, then the earliest), because ON CONFLICT DO UPDATE may
	//    not touch the same target row twice in one statement. On conflict with the
	//    keeper's own row, the STRONGER attribution wins (collapseProvenanceOrder);
	//    a tie keeps the keeper's row untouched. This deliberately differs from the
	//    DO NOTHING every other writer uses: there, a later automation must not
	//    re-attribute a filing that already exists; here, both rows describe the SAME
	//    message, and dropping the copy's 'user' row in favour of the keeper's
	//    'learned' one would delete the person's filing by subtraction.
	//
	// The casts are NOT decoration. $1 is both an inserted column value and a WHERE
	// comparand, and user_id is the app.ulid DOMAIN, so without them Postgres deduces
	// two different types for one parameter and fails with 42P08 ("inconsistent types
	// deduced for parameter $1"). Same trap as store.InsertModelVersion hit.
	labelRows, err := tx.tx.Query(ctx, `
		INSERT INTO app.message_labels AS cur
		       (user_id, message_id, label_id, applied_by, applied_by_rule_id, created_at)
		SELECT DISTINCT ON (ml.label_id)
		       $1::app.ulid, $2::app.ulid, ml.label_id, ml.applied_by, ml.applied_by_rule_id, ml.created_at
		  FROM app.message_labels ml
		 WHERE ml.user_id = $1::app.ulid AND ml.message_id = ANY($3)
		 ORDER BY ml.label_id, `+collapseProvenanceRankSQL("ml.applied_by")+`, ml.created_at, ml.message_id
		ON CONFLICT (user_id, message_id, label_id) DO UPDATE
		   SET applied_by = EXCLUDED.applied_by,
		       applied_by_rule_id = EXCLUDED.applied_by_rule_id,
		       created_at = EXCLUDED.created_at
		 WHERE `+collapseProvenanceRankSQL("EXCLUDED.applied_by")+` < `+collapseProvenanceRankSQL("cur.applied_by")+`
		RETURNING (xmax = 0)`,
		tx.userID.String(), keeperID.String(), loserStrs)
	if err != nil {
		return res, false, fmt.Errorf("store: collapse (union labels): %w", err)
	}
	for labelRows.Next() {
		var inserted bool
		if err := labelRows.Scan(&inserted); err != nil {
			labelRows.Close()
			return res, false, fmt.Errorf("store: collapse (scan unioned label): %w", err)
		}
		if inserted {
			res.LabelsUnioned++
		} else {
			res.LabelsUpgraded++
		}
	}
	labelRows.Close()
	if err := labelRows.Err(); err != nil {
		return res, false, fmt.Errorf("store: collapse (union labels): %w", err)
	}

	// 3. FEEDBACK MOVES. These are corrections a person made by hand and the model's
	//    training data. Cascading them away would silently un-teach the model, and the
	//    user would have no way to know their correction had been dropped.
	fb, err := tx.tx.Exec(ctx, `
		UPDATE app.feedback_events SET message_id = $2
		 WHERE user_id = $1 AND message_id = ANY($3)`,
		tx.userID.String(), keeperID.String(), loserStrs)
	if err != nil {
		return res, false, fmt.Errorf("store: collapse (move feedback): %w", err)
	}
	res.FeedbackMoved = int(fb.RowsAffected())

	// 3b. THE PLACEMENT LOG AND THE DAY-ONE JOURNAL (#882). Both reference the copy ON
	//     DELETE CASCADE, so they were deleted with it: a correction on the keeper
	//     found no placement to correct, and day-one undo found no journal row, so the
	//     label it wrote was never taken back. Neither is JMAP-visible, so neither owns
	//     change_log rows; the Email `updated` row appended below covers the keeper.
	if err := tx.carryLearnedPlacements(ctx, keeperID, loserStrs, &res); err != nil {
		return res, false, err
	}
	if err := tx.carryDayOneJournal(ctx, keeperID, loserStrs, &res); err != nil {
		return res, false, err
	}
	// 3c. PREFETCHED IMAGES (#922). A copy's image references cascade away with it,
	//     which would hand the keeper's images back to the unread TTL though the
	//     keeper still shows them. Operational rows, so no change_log.
	if err := tx.carryProxyImageRefs(ctx, keeperID, loserStrs); err != nil {
		return res, false, err
	}

	// 4. THE ROWS GO. Everything still referencing them is now genuinely redundant:
	//    triage scores (recomputable model output), applied writeback intents
	//    (history), and label rows duplicating what step 2 put on the keeper.
	del, err := tx.tx.Exec(ctx,
		`DELETE FROM app.messages WHERE user_id = $1 AND id = ANY($2)`,
		tx.userID.String(), loserStrs)
	if err != nil {
		return res, false, fmt.Errorf("store: collapse (delete losers): %w", err)
	}
	res.MessagesDeleted = int(del.RowsAffected())

	// 5. RECLAIM THE BLOBS. Each loser had its OWN content_hash — differing bytes is
	//    exactly why it became a separate row — so its ciphertext is now orphaned.
	//    Through the ONE shared guard (blobreclaim.go): no message, no attachment,
	//    and — added by #300, absent from both earlier copies of this SQL — no
	//    ml_models.artifact_hash.
	reclaimed, err := tx.reclaimUnreferencedBlobs(ctx, loserHashes)
	if err != nil {
		return res, false, err
	}
	res.ReclaimBlobs = append(res.ReclaimBlobs, reclaimed...)

	// 6. THREADS LEFT EMPTY GO TOO. Each duplicate owns its own thread today (#210), so
	//    deleting the message leaves a thread with nothing in it, which would show up as
	//    an empty conversation. Guarded on the thread being genuinely empty, because a
	//    loser sharing a thread with a message that survives must NOT take it down.
	//
	//    app.messages.thread_id is ON DELETE SET NULL, so this cannot orphan a surviving
	//    message even if the guard were wrong — it would merely unthread it.
	var deletedThreads []string
	if len(loserThreads) > 0 {
		thRows, err := tx.tx.Query(ctx, `
			DELETE FROM app.threads t
			 WHERE t.user_id = $1 AND t.id = ANY($2)
			   AND NOT EXISTS (SELECT 1 FROM app.messages m
			                    WHERE m.user_id = $1 AND m.thread_id = t.id)
			RETURNING t.id`,
			tx.userID.String(), loserThreads)
		if err != nil {
			return res, false, fmt.Errorf("store: collapse (delete empty threads): %w", err)
		}
		for thRows.Next() {
			var id string
			if err := thRows.Scan(&id); err != nil {
				thRows.Close()
				return res, false, fmt.Errorf("store: scan deleted thread: %w", err)
			}
			deletedThreads = append(deletedThreads, id)
		}
		thRows.Close()
		if err := thRows.Err(); err != nil {
			return res, false, fmt.Errorf("store: collapse (delete empty threads): %w", err)
		}
		res.ThreadsDeleted = len(deletedThreads)
	}

	// 7. THE CHANGE LOG, in the same transaction — the load-bearing invariant
	//    (CLAUDE.md, ADR-003). Without these, a client that has already synced the
	//    duplicates keeps showing them forever and no /changes call ever says otherwise,
	//    which is the same "silently ceased to exist" failure the reaper's history
	//    records. Three kinds, and all three are needed:
	//
	//      Email destroyed  — per loser, so clients drop the rows they hold
	//      Email updated    — the keeper, whose membership changed when provenance moved
	//      Thread destroyed — per emptied thread
	for _, l := range losers {
		if _, err := tx.AppendChange(ctx, ObjectEmail, l, OpDestroyed); err != nil {
			return res, false, err
		}
	}
	if _, err := tx.AppendChange(ctx, ObjectEmail, keeperID, OpUpdated); err != nil {
		return res, false, err
	}
	for _, t := range deletedThreads {
		id, err := ulid.Parse(t)
		if err != nil {
			return res, false, fmt.Errorf("store: deleted thread id %q: %w", t, err)
		}
		if _, err := tx.AppendChange(ctx, ObjectThread, id, OpDestroyed); err != nil {
			return res, false, err
		}
	}

	return res, true, nil
}

// carryLearnedPlacements gives the keeper the group's learned-placement log row
// (app.learned_placements, PK (user_id, message_id): at most one per message).
//
// WHICH ROW WINS when more than one copy has one: a CORRECTED row first, then the
// keeper's own, then the earliest decided. A correction is the user's explicit act
// (they took the learned label off) and the evidence that blocks the key and feeds
// the area's correction gate; an open or kept row is only the ingest's own decision,
// so letting a copy's open row displace the keeper's correction - or dropping a
// copy's correction because the keeper's row was open - would un-teach the user's
// judgement by subtraction, the same reasoning as the label attribution rank
// (collapseProvenanceOrder). Past that, the keeper is the message that survives, so
// its row is the default. The winner moves WHOLE (outcome and its stamp together),
// which keeps the stamped CHECK constraints satisfied; no row is ever mixed.
func (tx *UserTx) carryLearnedPlacements(
	ctx context.Context, keeperID ulid.ULID, loserStrs []string, res *CollapseResult,
) error {
	group := append([]string{keeperID.String()}, loserStrs...)
	var (
		winner string
		total  int
	)
	err := tx.tx.QueryRow(ctx, `
		SELECT message_id, count(*) OVER ()
		  FROM app.learned_placements
		 WHERE user_id = $1 AND message_id = ANY($2)
		 ORDER BY (outcome = 'corrected') DESC, (message_id = $3) DESC, decided_at, message_id
		 LIMIT 1`,
		tx.userID.String(), group, keeperID.String()).Scan(&winner, &total)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil
	}
	if err != nil {
		return fmt.Errorf("store: collapse (pick placement): %w", err)
	}
	if winner == keeperID.String() {
		res.PlacementsDropped += total - 1
		return nil
	}
	tag, err := tx.tx.Exec(ctx, `
		DELETE FROM app.learned_placements WHERE user_id = $1 AND message_id = $2`,
		tx.userID.String(), keeperID.String())
	if err != nil {
		return fmt.Errorf("store: collapse (replace keeper placement): %w", err)
	}
	replaced := int(tag.RowsAffected())
	if _, err := tx.tx.Exec(ctx, `
		UPDATE app.learned_placements SET message_id = $2
		 WHERE user_id = $1 AND message_id = $3`,
		tx.userID.String(), keeperID.String(), winner); err != nil {
		return fmt.Errorf("store: collapse (move placement): %w", err)
	}
	res.PlacementsMoved++
	res.PlacementsReplaced += replaced
	res.PlacementsDropped += total - 1 - replaced
	return nil
}

// carryDayOneJournal re-points the copies' day-one undo journal rows
// (app.setup_journal) at the keeper, one row per (run, message) as the unique index
// requires.
//
// Day-one undo reads this table, per message, to know what the run did: which label
// it inserted, whether it archived, which folder it moved the message out of. The
// copies' labels are carried onto the keeper with their attribution (step 2), so a
// label the run wrote as 'setup' on a copy is a 'setup' row on the keeper, and undo
// can only take it back if a journal row on the keeper names it.
//
// WHEN SEVERAL COPIES WERE JOURNALED BY THE SAME RUN the rows are MERGED into one,
// keeping everything undo needs and nothing it would misapply:
//   - label_id: the one label any of them names. Two DIFFERENT labels cannot share a
//     row, and picking one would leave the other a 'setup' label nothing undoes, so
//     that group is BLOCKED before this runs (CollapseLoser.Blocked, re-checked under
//     the lock) rather than resolved here.
//   - desk_archived: true if any copy's was - undo clears the keeper's
//     desk_archived_at only if told to, and un-archiving is the safe direction.
//   - moved_from_mailbox_id: the keeper's own, else the earliest copy's that has one.
//     Undo moves the one surviving message back to a single folder.
//   - the keeper's row id and created_at are kept when it has one (the undo cursor
//     orders by id); otherwise the earliest copy's row is the one that moves.
func (tx *UserTx) carryDayOneJournal(
	ctx context.Context, keeperID ulid.ULID, loserStrs []string, res *CollapseResult,
) error {
	// Per run, over the LOSERS only: the aggregate to fold in.
	const losersAgg = `
		SELECT run_id,
		       (array_agg(id ORDER BY id))[1] AS first_id,
		       (array_agg(label_id ORDER BY id) FILTER (WHERE label_id IS NOT NULL))[1] AS label_id,
		       bool_or(desk_archived) AS desk_archived,
		       (array_agg(moved_from_mailbox_id ORDER BY id) FILTER (WHERE moved_from_mailbox_id IS NOT NULL))[1] AS moved_from
		  FROM app.setup_journal
		 WHERE user_id = $1 AND message_id = ANY($3)
		 GROUP BY run_id`
	// 1. Runs where the keeper already has a row: fold the copies into it.
	if _, err := tx.tx.Exec(ctx, `
		UPDATE app.setup_journal k
		   SET label_id = COALESCE(k.label_id, m.label_id),
		       desk_archived = k.desk_archived OR m.desk_archived,
		       moved_from_mailbox_id = COALESCE(k.moved_from_mailbox_id, m.moved_from)
		  FROM (`+losersAgg+`) m
		 WHERE k.user_id = $1 AND k.message_id = $2 AND k.run_id = m.run_id`,
		tx.userID.String(), keeperID.String(), loserStrs); err != nil {
		return fmt.Errorf("store: collapse (merge journal): %w", err)
	}
	// 2. Runs where it does not: the earliest copy's row moves, carrying the merge.
	moved, err := tx.tx.Exec(ctx, `
		UPDATE app.setup_journal j
		   SET message_id = $2, label_id = m.label_id,
		       desk_archived = m.desk_archived, moved_from_mailbox_id = m.moved_from
		  FROM (`+losersAgg+`) m
		 WHERE j.user_id = $1 AND j.id = m.first_id
		   AND NOT EXISTS (SELECT 1 FROM app.setup_journal k
		                    WHERE k.user_id = $1 AND k.message_id = $2 AND k.run_id = m.run_id)`,
		tx.userID.String(), keeperID.String(), loserStrs)
	if err != nil {
		return fmt.Errorf("store: collapse (move journal): %w", err)
	}
	res.JournalMoved += int(moved.RowsAffected())
	// The copies' remaining rows were folded into one of the above; they go with
	// the copies.
	left, err := tx.tx.Exec(ctx, `
		DELETE FROM app.setup_journal WHERE user_id = $1 AND message_id = ANY($2)`,
		tx.userID.String(), loserStrs)
	if err != nil {
		return fmt.Errorf("store: collapse (drop folded journal): %w", err)
	}
	res.JournalMerged += int(left.RowsAffected())
	return nil
}

// collapseProvenanceOrder ranks label attributions, STRONGEST FIRST, for the one
// place two rows describe the same label on the same message: a collapse (#859).
//
// The rank is "what would be lost if this attribution were the one dropped":
//
//	user     The person's own filing: the ONLY evidence learned placement, the label
//	         model and rule induction learn from, and the one row an API key may not
//	         take off (ErrUserFilingProtected). Always wins.
//	rule     Names a rule the user wrote; taking the label off records a
//	         rule_correction against that rule, the loop that narrows it.
//	learned  Taking it off records learned placement's correction, the loop that
//	         stops a sender being placed again. Below rule: inferred, not written
//	         by the user, and there is no rule to point at.
//	agent    An API key's deliberate act, but no feedback loop reads it.
//	desk     Re-derivable from the tab verdict, and re-filing removes it freely
//	         (area_filing.go).
//	setup    Lowest of the live sources: day-one undo deletes rows by THIS
//	         attribution, so when another source also applied the label, keeping
//	         that one means undo leaves the label alone — the conservative outcome.
//	ml       Never written today (#169); legacy.
//
// Anything not listed (the column has no CHECK) ranks below all of these.
var collapseProvenanceOrder = []string{
	LabelByUser, LabelByRule, LabelByLearned, LabelByAgent, LabelByDesk, LabelBySetup, LabelByML,
}

// collapseProvenanceRankSQL renders collapseProvenanceOrder as a CASE over col:
// 0 for the strongest, len(order) for anything unrecognised. The values are this
// package's own constants, never input, so inlining them as literals is safe.
func collapseProvenanceRankSQL(col string) string {
	var b strings.Builder
	b.WriteString("(CASE " + col)
	for i, by := range collapseProvenanceOrder {
		fmt.Fprintf(&b, " WHEN '%s' THEN %d", by, i)
	}
	fmt.Fprintf(&b, " ELSE %d END)", len(collapseProvenanceOrder))
	return b.String()
}
