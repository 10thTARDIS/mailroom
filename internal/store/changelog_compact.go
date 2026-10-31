// SPDX-License-Identifier: AGPL-3.0-only

package store

import (
	"context"
	"fmt"
	"time"
)

// This file implements change_log compaction (mailroom#526, ADR-003
// Consequences, mailroom-data-model.md §3.3): the weekly job that keeps the
// change log from growing forever by deleting history older than a retention
// window and raising app.change_floor to match. It never touches change_seq
// (JMAP state strings are unaffected) and it is the ONLY code in this package
// that deletes change_log rows — everything else only ever appends
// (AppendChange/AppendChanges), which is what makes the "rows currently in
// change_log for a type all have seq > that type's floor" invariant this file
// leans on actually hold.

// DefaultChangeLogCompactBatch bounds how many rows ONE CompactChangeLog call
// deletes per object type, independent of how large the eligible prefix
// actually is. A backlog spanning a long retention history — the first run
// after this feature ships, on an old account — drains across several short
// calls (the job's own loop, or `compact-changelog --apply`'s) rather than one
// DELETE holding a lock over an unbounded number of rows. Callers that want no
// cap at all may pass a non-positive maxDeletePerType instead.
const DefaultChangeLogCompactBatch int64 = 20000

// CompactionResult reports one object type's compaction outcome from a single
// CompactChangeLog call — either what WOULD happen (apply=false) or what DID
// happen (apply=true). The numbers are computed identically either way; only
// the write at the end is conditional on apply, so a report and the run it
// describes can never disagree about what the arithmetic says.
type CompactionResult struct {
	ObjectType ObjectType
	// RowsBefore is how many change_log rows this type had when this call
	// started (before any deletion this call might perform).
	RowsBefore int64
	// Deleted is how many of those rows this call removed (or would remove,
	// when apply is false) — always a prefix, seq in (OldFloor, NewFloor].
	Deleted  int64
	OldFloor int64
	NewFloor int64
	// OldestKept is the created_at of the earliest change_log row that
	// survives this call (seq > NewFloor), or nil when none does — this
	// call's boundary reaches every row that currently exists for the type.
	OldestKept *time.Time
}

// CompactChangeLog removes a strict seq PREFIX of the user's change_log rows,
// per object type, for every type app.change_seq knows about (every type ever
// mutated for this user) — raising app.change_floor to the last deleted seq
// for each type it actually advances. apply=false computes exactly the same
// numbers and writes nothing (CLAUDE.md: "build the report before the fix");
// apply=true performs it, every type handled inside the ONE transaction this
// method runs in (the caller's UserTx). maxDeletePerType caps how many rows
// one type may lose in this call; non-positive means no cap.
//
// ⚠ COMPACTION APPENDS NO change_log ROW, AND THAT IS CORRECT, NOT A GAP IN
// THE LOAD-BEARING INVARIANT. CLAUDE.md's invariant is "every mutation of a
// JMAP-visible OBJECT appends a change_log row in the same transaction as the
// mutation" — it binds mutations of messages, mailboxes, labels, threads,
// identities, contacts, submissions and the rest, because those are the
// things a JMAP client can observe through Foo/get and Foo/changes. Deleting
// old HISTORY ROWS mutates no such object: no Email, Mailbox, or any other
// JMAP-visible thing changes state, gains a property, or stops existing —
// compaction removes entries from the LOG ITSELF, exactly the kind of change
// data-model §3.4 already places outside what the log records about itself. A
// client whose cursor falls below the new floor gets cannotCalculateChanges
// and refetches: the designed reaction to compaction, not a missed update to
// something it can see.
//
// ⚠ THE STRICT-PREFIX RULE, AND WHY IT IS NOT "delete rows older than
// cutoff". change_log.created_at is DEFAULT now() — the enclosing
// TRANSACTION'S START time — not necessarily monotonic with seq: two
// concurrent writers can commit out of created_at order relative to when each
// took its change_seq row-lock turn, so a row can be OLDER BY SEQ (allocated
// earlier) but YOUNGER BY CREATED_AT (committed later), or the reverse — an
// inversion. Deleting straight off "created_at < cutoff" could then delete a
// row above the chosen boundary while leaving one below it: a HOLE in the
// middle of what Changes() assumes is contiguous history above the floor (its
// scan is `seq > since ORDER BY seq`, which silently skips a hole rather than
// erroring — a client would just never learn about the object the hole ate).
// So the boundary D is chosen ONCE per type, from created_at, and then EVERY
// row with seq <= D is deleted regardless of that individual row's own
// created_at — never a row above D, even one that is individually older than
// cutoff (see the inversion case in the integration test: row 6 is
// old-dated but sits above D and is never touched).
//
// ⚠ FLOOR SEMANTICS: floor_seq = D, THE HIGHEST DELETED SEQ, NOT D+1.
// Changes() rejects `since < floor` and accepts `since == floor` — a client
// sitting exactly at the boundary already has every row past it, by
// construction, because D is by definition the last seq this call deleted.
// Setting the floor to D+1 would reject that client's otherwise-satisfiable
// request for nothing (TestCompactChangeLogPrefixProperty pins the exact
// boundary). mailroom-data-model.md §3.3 said "+1 the highest deleted seq";
// that line is corrected alongside this change.
func (tx *UserTx) CompactChangeLog(ctx context.Context, cutoff time.Time, maxDeletePerType int64, apply bool) ([]CompactionResult, error) {
	typeRows, err := tx.tx.Query(ctx,
		`SELECT object_type FROM app.change_seq WHERE user_id = $1 ORDER BY object_type`,
		tx.userID.String())
	if err != nil {
		return nil, fmt.Errorf("store: list change-log object types: %w", err)
	}
	var types []ObjectType
	for typeRows.Next() {
		var raw string
		if err := typeRows.Scan(&raw); err != nil {
			typeRows.Close()
			return nil, fmt.Errorf("store: scan change-log object type: %w", err)
		}
		types = append(types, ObjectType(raw))
	}
	if err := typeRows.Err(); err != nil {
		typeRows.Close()
		return nil, fmt.Errorf("store: list change-log object types: %w", err)
	}
	typeRows.Close()

	results := make([]CompactionResult, 0, len(types))
	for _, t := range types {
		res, err := tx.compactOneChangeLogType(ctx, t, cutoff, maxDeletePerType, apply)
		if err != nil {
			return nil, err
		}
		results = append(results, res)
	}
	return results, nil
}

// compactOneChangeLogType is CompactChangeLog's per-type body: compute the
// boundary D, cap it, and — only when apply is true and there is anything to
// do — raise the floor and delete the prefix, both in the caller's
// transaction.
func (tx *UserTx) compactOneChangeLogType(ctx context.Context, t ObjectType, cutoff time.Time, maxDeletePerType int64, apply bool) (CompactionResult, error) {
	res := CompactionResult{ObjectType: t}

	var floor int64
	if err := tx.tx.QueryRow(ctx, `
		SELECT COALESCE((SELECT floor_seq FROM app.change_floor
		                   WHERE user_id = $1 AND object_type = $2), 0)`,
		tx.userID.String(), t,
	).Scan(&floor); err != nil {
		return res, fmt.Errorf("store: read change floor (%s): %w", t, err)
	}
	res.OldFloor, res.NewFloor = floor, floor

	// One scan of the type's CURRENTLY EXISTING rows — every one of which
	// already has seq > floor, because CompactChangeLog is the only code that
	// ever deletes a change_log row, and it always deletes exactly the prefix
	// up through its own NewFloor. minNewSeq is the earliest row this
	// retention window says to KEEP; maxSeq is the latest row that exists at
	// all, the fallback boundary when every existing row is older than cutoff.
	var (
		rowsBefore int64
		minNewSeq  *int64
		maxSeq     *int64
	)
	if err := tx.tx.QueryRow(ctx, `
		SELECT count(*), min(seq) FILTER (WHERE created_at >= $3), max(seq)
		  FROM app.change_log
		 WHERE user_id = $1 AND object_type = $2`,
		tx.userID.String(), t, cutoff,
	).Scan(&rowsBefore, &minNewSeq, &maxSeq); err != nil {
		return res, fmt.Errorf("store: scan change log boundary (%s): %w", t, err)
	}
	res.RowsBefore = rowsBefore

	var (
		boundary     int64
		haveBoundary bool
	)
	switch {
	case minNewSeq != nil:
		// At least one row is inside the retention window: the prefix rule
		// stops one short of it, whatever that row's neighbours' own dates say.
		boundary, haveBoundary = *minNewSeq-1, true
	case maxSeq != nil:
		// No row is inside the window — every row that exists is eligible,
		// up to the cap below.
		boundary, haveBoundary = *maxSeq, true
	} // else: no rows at all for this type right now; nothing to do.

	if haveBoundary && boundary > floor {
		if maxDeletePerType > 0 && boundary-floor > maxDeletePerType {
			// Cap this call's deletion by lowering the boundary to the seq
			// that bounds exactly maxDeletePerType rows — a prefix of a
			// prefix is still a prefix, so this stays within the strict-
			// prefix rule; the remainder surfaces next call as a fresh,
			// still-eligible boundary above the new (capped) floor.
			boundary = floor + maxDeletePerType
		}
		res.NewFloor = boundary // boundary > floor was just established

		// Deleted is COUNTED, not derived as NewFloor - floor. The two agree only
		// while seq is gap-free above the floor — true today (this file holds the
		// only DELETE on change_log, and it removes a prefix) but an ASSUMPTION a
		// report must not print as a fact: compact-changelog's
		// `before − deleted = after` line is the evidence an operator reads.
		// apply below replaces it with the DELETE's own RowsAffected.
		if err := tx.tx.QueryRow(ctx, `
			SELECT count(*) FROM app.change_log
			 WHERE user_id = $1 AND object_type = $2 AND seq > $3 AND seq <= $4`,
			tx.userID.String(), t, floor, res.NewFloor,
		).Scan(&res.Deleted); err != nil {
			return res, fmt.Errorf("store: count compactable change log rows (%s): %w", t, err)
		}
	}

	// OldestKept is well-defined whether or not apply actually deletes
	// anything: it is simply the earliest surviving row under NewFloor,
	// computed against the rows that exist right now (report and apply agree
	// on this number by construction).
	var oldestKept *time.Time
	if err := tx.tx.QueryRow(ctx, `
		SELECT min(created_at) FROM app.change_log
		 WHERE user_id = $1 AND object_type = $2 AND seq > $3`,
		tx.userID.String(), t, res.NewFloor,
	).Scan(&oldestKept); err != nil {
		return res, fmt.Errorf("store: read oldest kept change (%s): %w", t, err)
	}
	res.OldestKept = oldestKept

	if !apply || res.NewFloor == floor {
		return res, nil
	}

	// ONE transaction (the caller's): the floor raise and the delete commit
	// together or not at all. Their ORDER inside the transaction is invisible to
	// every other session; what keeps a concurrent reader safe is that atomic
	// commit PLUS Changes() reading floor, state and rows in ONE statement
	// (jmap_changes.go), so a reader sees either the old floor with every row or
	// the new floor without the prefix — never the new-rows/old-floor mix.
	if _, err := tx.tx.Exec(ctx, `
		INSERT INTO app.change_floor (user_id, object_type, floor_seq)
		VALUES ($1, $2, $3)
		ON CONFLICT (user_id, object_type)
		DO UPDATE SET floor_seq = GREATEST(change_floor.floor_seq, EXCLUDED.floor_seq)`,
		tx.userID.String(), t, res.NewFloor,
	); err != nil {
		return res, fmt.Errorf("store: raise change floor (%s): %w", t, err)
	}
	tag, err := tx.tx.Exec(ctx, `
		DELETE FROM app.change_log
		 WHERE user_id = $1 AND object_type = $2 AND seq <= $3`,
		tx.userID.String(), t, res.NewFloor,
	)
	if err != nil {
		return res, fmt.Errorf("store: delete compacted change log rows (%s): %w", t, err)
	}
	// What the DELETE actually removed, not what the count above predicted.
	res.Deleted = tag.RowsAffected()

	return res, nil
}
