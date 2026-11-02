// SPDX-License-Identifier: AGPL-3.0-only

package store

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/oklog/ulid/v2"
)

// This file implements the JMAP `Foo/changes` collapse algorithm exactly as
// specified in mailroom-data-model.md §3.2 — the load-bearing sync primitive
// the whole protocol layer depends on. It is a store method (not API logic) so
// that the scan runs inside a single RLS-scoped transaction over committed,
// sequenced history (data-model §3.2: "deterministic under concurrent writes").

// ErrCannotCalculateChanges signals that the requested sinceState is older than
// the compaction floor (or is an impossible future state) and the client must
// refetch via `/query` + `/get` — RFC 8620's designed fallback. The API layer
// maps this to the JMAP `cannotCalculateChanges` method-level error.
var ErrCannotCalculateChanges = errors.New("store: cannot calculate changes")

// ChangesPage is the collapsed result of a `Foo/changes` scan: the three JMAP
// buckets plus the state bookkeeping. Ids within each bucket are in ascending
// change-sequence order (first-touch order), which is stable and deterministic.
type ChangesPage struct {
	OldState  int64
	NewState  int64
	HasMore   bool
	Created   []ulid.ULID
	Updated   []ulid.ULID
	Destroyed []ulid.ULID
}

// Changes computes the JMAP `Foo/changes` delta for object type t between
// sinceState (a decimal change sequence; 0 for an initial sync) and the current
// state, implementing mailroom-data-model.md §3.2:
//
//  1. If since < change_floor(user, t) → ErrCannotCalculateChanges. (An initial
//     sync with since == 0 succeeds while the floor is 0, the default.) A since
//     strictly greater than the current state is an impossible/forged state and
//     is likewise rejected.
//  2. Otherwise scan change_log (user, t, seq > since) in seq order, collapse
//     per object_id, and bucket by (firstOp, lastOp):
//     - created … not-destroyed  → created
//     - created … destroyed      → omitted (the client never knew it)
//     - not-created … destroyed  → destroyed
//     - otherwise                → updated
//  3. Honor maxChanges (> 0) by truncating at an object boundary — a change
//     sequence boundary, since each row belongs to exactly one object — and
//     setting HasMore. NewState is the max seq fully scanned, or the current
//     state when not truncated.
//
// ⚠ THIS MUST STAY ONE STATEMENT (mailroom#526). It used to be three: read the
// floor, read the current state (ChangeState), then scan change_log — three
// round trips inside the same UserTx. UserTx runs at the default READ
// COMMITTED (Store.UserTx / pgx.BeginFunc, no TxOptions), so each STATEMENT
// gets its own snapshot, not each transaction. A CompactChangeLog commit
// landing between statement 1 and statement 3 would raise the floor and
// delete exactly the rows the (now stale) floor read in statement 1 still
// promised were available — the scan in statement 3 would then return an
// INCOMPLETE delta with NO ERROR, silently, to every consumer of this method:
// JMAP clients and the search indexer (internal/search.StoreSource) alike.
// A single SQL statement sees one consistent snapshot even under READ
// COMMITTED, which is the only reason the query below is safe. Do not split
// it back into separate floor/state/scan queries.
func (tx *UserTx) Changes(ctx context.Context, t ObjectType, since int64, maxChanges int) (ChangesPage, error) {
	// f and c are each single-row CTEs (COALESCE over a scalar subquery), so
	// `f CROSS JOIN c` always yields exactly one row; the LEFT JOIN against
	// change_log can only ADD rows to that one, never remove it. The extra
	// join predicates ($3 >= f.floor_seq AND $3 <= c.cur) mean the join
	// matches nothing at all — leaving just the single floor/cur row, with
	// every change_log column NULL — whenever since is out of range, so a
	// since below the floor or above the current state never pays for a scan
	// whose answer is going to be discarded as ErrCannotCalculateChanges.
	//
	// planForValues: the generic plan for this statement scans the whole log
	// on a single-user instance (see plans.go).
	rows, err := tx.tx.Query(ctx, `
		WITH f AS (
			SELECT COALESCE(
				(SELECT floor_seq FROM app.change_floor
				  WHERE user_id = $1 AND object_type = $2), 0) AS floor_seq
		), c AS (
			SELECT COALESCE(
				(SELECT last_seq FROM app.change_seq
				  WHERE user_id = $1 AND object_type = $2), 0) AS cur
		)
		SELECT f.floor_seq, c.cur, l.seq, l.object_id, l.op
		  FROM f CROSS JOIN c
		  LEFT JOIN app.change_log l
		    ON l.user_id = $1 AND l.object_type = $2 AND l.seq > $3
		   AND $3 >= f.floor_seq AND $3 <= c.cur
		 ORDER BY l.seq`,
		planForValues, tx.userID.String(), t, since)
	if err != nil {
		return ChangesPage{}, fmt.Errorf("store: scan change log (%s): %w", t, err)
	}
	defer rows.Close()

	var (
		floor, current int64
		seenRow        bool
		entries        []changeEntry
	)
	for rows.Next() {
		var (
			f, c  int64
			seq   *int64
			objID *string
			op    *string
		)
		if err := rows.Scan(&f, &c, &seq, &objID, &op); err != nil {
			return ChangesPage{}, fmt.Errorf("store: scan change row (%s): %w", t, err)
		}
		floor, current, seenRow = f, c, true
		if seq == nil {
			// The floor/cur placeholder row (no matching change_log row): either
			// there is nothing to scan, or since was rejected by the join
			// predicates above and the floor/state check below will report it.
			continue
		}
		var e changeEntry
		e.seq = *seq
		if e.objectID, err = ulid.Parse(*objID); err != nil {
			return ChangesPage{}, fmt.Errorf("store: parse change object id %q: %w", *objID, err)
		}
		e.op = ChangeOp(*op)
		entries = append(entries, e)
	}
	if err := rows.Err(); err != nil {
		return ChangesPage{}, fmt.Errorf("store: scan change log (%s): %w", t, err)
	}
	if !seenRow {
		// Unreachable given the CTE shape above (see the comment on the query) —
		// guarded rather than assumed, so a future rewrite that breaks the
		// single-row guarantee fails loudly instead of silently reporting state 0.
		return ChangesPage{}, fmt.Errorf("store: change floor/state query (%s) returned no rows", t)
	}

	// §3.2 step 1: below the floor (compacted away) or an impossible future
	// state → the client must refetch.
	if since < floor || since > current {
		return ChangesPage{}, ErrCannotCalculateChanges
	}

	return collapseChanges(entries, since, current, maxChanges), nil
}

// changeEntry is one scanned change_log row, the input to the pure collapse.
type changeEntry struct {
	seq      int64
	objectID ulid.ULID
	op       ChangeOp
}

// collapseChanges is the pure, database-free core of the §3.2 algorithm: given
// the change_log rows for (user, t) with seq > since already ordered by seq, it
// collapses per object_id and buckets, honoring maxChanges at an object (== seq)
// boundary. It is separated from the SQL scan so the bucketing can be
// exhaustively table-tested. Callers must pass entries in ascending seq order.
func collapseChanges(entries []changeEntry, since, current int64, maxChanges int) ChangesPage {
	type collapsed struct {
		first, last ChangeOp
	}
	order := make([]ulid.ULID, 0, len(entries))
	seen := make(map[ulid.ULID]*collapsed, len(entries))
	newState := since
	capped := false

	for _, e := range entries {
		c, ok := seen[e.objectID]
		if !ok {
			// A new distinct object. Truncate BEFORE including it if that would
			// exceed maxChanges — newState stays at the previous row's seq, an
			// object boundary, and the excluded rows resurface next round.
			if maxChanges > 0 && len(order) >= maxChanges {
				capped = true
				break
			}
			c = &collapsed{first: e.op, last: e.op}
			seen[e.objectID] = c
			order = append(order, e.objectID)
		} else {
			c.last = e.op
		}
		newState = e.seq
	}
	if !capped {
		// The scan covered everything up to the current state; the last log row
		// for the type carries seq == current, but pin it explicitly so an empty
		// range still reports the true current state.
		newState = current
	}

	page := ChangesPage{OldState: since, NewState: newState, HasMore: capped}
	for _, oid := range order {
		c := seen[oid]
		switch {
		case c.first == OpCreated && c.last == OpDestroyed:
			// Created and destroyed within the window — omit entirely.
		case c.first == OpCreated:
			page.Created = append(page.Created, oid)
		case c.last == OpDestroyed:
			page.Destroyed = append(page.Destroyed, oid)
		default:
			page.Updated = append(page.Updated, oid)
		}
	}
	return page
}

// OldestUnconsumedEmailChangeAt returns when the oldest Email change AFTER
// `since` was recorded, and whether there is one at all. It backs
// mailroom_index_cursor_lag_seconds (#524): the caller subtracts it from now to
// get how far behind the search index consumer has fallen, in seconds.
//
// ⚠ SECONDS, NOT SEQUENCE DISTANCE, and the difference is the whole point. The
// indexer already knows its cursor and the head of the stream, so "how many
// changes behind" is free — but a gauge named _seconds fed a row count would be
// a metric measuring the wrong thing while looking authoritative. Ten changes
// behind means nothing; ten MINUTES behind is an alert. Only a timestamp
// distinguishes a consumer that is briefly behind a burst from one that stopped.
//
// Returns ok=false when the consumer is caught up, which the caller must report
// as zero lag rather than skipping — a gauge left unset keeps its last value, so
// a queue that drains would otherwise stay stuck at its high-water mark.
//
// The (user_id, object_type, seq) primary key covers this scan.
func (tx *UserTx) OldestUnconsumedEmailChangeAt(ctx context.Context, since int64) (at time.Time, ok bool, err error) {
	var t *time.Time
	if err := tx.tx.QueryRow(ctx, `
		SELECT min(created_at) FROM app.change_log
		 WHERE user_id = $1 AND object_type = 'Email' AND seq > $2`,
		tx.userID.String(), since).Scan(&t); err != nil {
		return time.Time{}, false, fmt.Errorf("store: oldest unconsumed email change: %w", err)
	}
	if t == nil {
		return time.Time{}, false, nil
	}
	return *t, true, nil
}
