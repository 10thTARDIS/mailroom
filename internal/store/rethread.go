// SPDX-License-Identifier: AGPL-3.0-only

package store

import (
	"context"
	"fmt"

	"github.com/oklog/ulid/v2"
)

// SplitThreadGroup is one set of messages that share an RFC 5322 Message-ID
// but were assigned to more than one thread — the population #210's ingest fix
// prevents from forming, and which this type reports for mail that was already
// ingested before that fix existed.
//
// ⚠ THE INGEST FIX IS FORWARD-ONLY, which is why this exists. A message keeps
// the thread_id it was given at ingest, so correcting FindThreadByHints
// changes nothing about mail already in the mailbox. On a real
// corpus that was a set of groups, all split — a fix that shipped and changed
// nothing observable would be this project's most-repeated failure.
type SplitThreadGroup struct {
	// MessageIDHint is the shared RFC 5322 Message-ID.
	MessageIDHint string

	// Keeper is the thread the group collapses into: the one holding the
	// EARLIEST-received message.
	//
	// ⚠ That tie-break is not arbitrary and must not be changed casually — it
	// is the same choice FindThreadByHints makes (`ORDER BY received_at LIMIT
	// 1`). Picking differently here would mean a repaired mailbox and a
	// freshly-ingested one disagree about which thread a duplicate lands in.
	Keeper ulid.ULID

	// Losers are the other threads holding messages of this group. Their
	// messages move to Keeper and the threads are then deleted if empty.
	Losers []ulid.ULID

	// Messages is every message carrying MessageIDHint, across all threads.
	Messages []ulid.ULID
}

// MergeResult reports what MergeSplitThreadGroups actually changed, per
// category rather than as one total — a single number cannot distinguish
// "nothing to do" from "the thing I measured is always empty".
type MergeResult struct {
	Groups          int
	MessagesMoved   int
	ThreadsDeleted  int
	ThreadsAffected int
}

// SplitMessageIDGroups reports every group of messages that share a Message-ID
// while sitting in different threads. READ-ONLY: it is the report half of
// report-then-apply, and MergeSplitThreadGroups is the opt-in mutation.
//
// Messages with no Message-ID are excluded — there is no identity to merge on,
// and grouping them by their empty hint would merge unrelated mail.
func (tx *UserTx) SplitMessageIDGroups(ctx context.Context) ([]SplitThreadGroup, error) {
	rows, err := tx.tx.Query(ctx, `
		WITH dup AS (
		  SELECT message_id_hint
		    FROM app.messages
		   WHERE user_id = $1
		     AND message_id_hint IS NOT NULL AND message_id_hint <> ''
		     AND thread_id IS NOT NULL
		   GROUP BY message_id_hint
		  HAVING count(DISTINCT thread_id) > 1
		)
		SELECT m.message_id_hint,
		       (array_agg(m.thread_id ORDER BY m.received_at, m.id))[1] AS keeper,
		       array_agg(DISTINCT m.thread_id) AS threads,
		       array_agg(m.id ORDER BY m.received_at, m.id) AS messages
		  FROM app.messages m
		  JOIN dup ON dup.message_id_hint = m.message_id_hint
		 WHERE m.user_id = $1 AND m.thread_id IS NOT NULL
		 GROUP BY m.message_id_hint
		 ORDER BY m.message_id_hint`,
		tx.userID.String())
	if err != nil {
		return nil, fmt.Errorf("store: split message-id groups: %w", err)
	}
	defer rows.Close()

	var out []SplitThreadGroup
	for rows.Next() {
		var (
			hint     string
			keeper   string
			threads  []string
			messages []string
		)
		if err := rows.Scan(&hint, &keeper, &threads, &messages); err != nil {
			return nil, fmt.Errorf("store: scan split group: %w", err)
		}
		g := SplitThreadGroup{MessageIDHint: hint}
		if g.Keeper, err = ulid.Parse(keeper); err != nil {
			return nil, fmt.Errorf("store: parse keeper thread %q: %w", keeper, err)
		}
		for _, t := range threads {
			u, err := ulid.Parse(t)
			if err != nil {
				return nil, fmt.Errorf("store: parse thread %q: %w", t, err)
			}
			if u != g.Keeper {
				g.Losers = append(g.Losers, u)
			}
		}
		for _, m := range messages {
			u, err := ulid.Parse(m)
			if err != nil {
				return nil, fmt.Errorf("store: parse message %q: %w", m, err)
			}
			g.Messages = append(g.Messages, u)
		}
		out = append(out, g)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("store: split message-id groups: %w", err)
	}
	return out, nil
}

// MergeSplitThreadGroups moves every message out of each group's losing threads
// into its keeper, refreshes the keeper's rollups, deletes the threads left
// empty, and appends the change_log rows for all of it — in the caller's
// transaction, per the load-bearing invariant.
//
// ⚠ IT MOVES EVERY MESSAGE OF A LOSING THREAD, not only the duplicates. That
// is the point: the threads are being MERGED. A reply that threaded onto a
// duplicate belongs with the conversation the duplicate is part of, and leaving
// it behind would strand it in a thread whose only other member just left.
//
// ⚠ LOSING THREADS' OWN STATE IS DISCARDED — muted_at in particular. A merge
// cannot keep two values of a per-thread flag, and the keeper's is the one a
// freshly-ingested mailbox would have had. Worth knowing before running it on
// a mailbox where threads have been muted.
func (tx *UserTx) MergeSplitThreadGroups(ctx context.Context, groups []SplitThreadGroup) (MergeResult, error) {
	var res MergeResult
	for _, g := range groups {
		if len(g.Losers) == 0 {
			continue
		}
		loserIDs := make([]string, len(g.Losers))
		for i, l := range g.Losers {
			loserIDs[i] = l.String()
		}

		// 1. Move the messages, returning exactly which ones moved so the
		//    change rows describe reality rather than intent.
		rows, err := tx.tx.Query(ctx, `
			UPDATE app.messages
			   SET thread_id = $3
			 WHERE user_id = $1 AND thread_id = ANY($2)
			RETURNING id`,
			tx.userID.String(), loserIDs, g.Keeper.String())
		if err != nil {
			return res, fmt.Errorf("store: rethread (move messages): %w", err)
		}
		var moved []ulid.ULID
		for rows.Next() {
			var id string
			if err := rows.Scan(&id); err != nil {
				rows.Close()
				return res, fmt.Errorf("store: rethread (scan moved): %w", err)
			}
			u, err := ulid.Parse(id)
			if err != nil {
				rows.Close()
				return res, fmt.Errorf("store: rethread parse moved id %q: %w", id, err)
			}
			moved = append(moved, u)
		}
		rows.Close()
		if err := rows.Err(); err != nil {
			return res, fmt.Errorf("store: rethread (move messages): %w", err)
		}
		if len(moved) == 0 {
			continue
		}

		// 2. Recompute the keeper's rollups from the messages themselves
		//    rather than incrementing. An increment would be wrong the moment
		//    this is re-run, and recomputation is self-correcting.
		if _, err := tx.tx.Exec(ctx, `
			UPDATE app.threads t
			   SET message_count = c.n, last_received_at = c.last
			  FROM (SELECT count(*) AS n, max(received_at) AS last
			          FROM app.messages
			         WHERE user_id = $1 AND thread_id = $2) c
			 WHERE t.user_id = $1 AND t.id = $2`,
			tx.userID.String(), g.Keeper.String()); err != nil {
			return res, fmt.Errorf("store: rethread (keeper rollups): %w", err)
		}

		// 3. Delete the threads now left empty. Guarded on emptiness rather
		//    than assumed, exactly as collapse.go does: a loser that somehow
		//    still holds a message must not be deleted out from under it.
		thRows, err := tx.tx.Query(ctx, `
			DELETE FROM app.threads t
			 WHERE t.user_id = $1 AND t.id = ANY($2)
			   AND NOT EXISTS (SELECT 1 FROM app.messages m
			                    WHERE m.user_id = $1 AND m.thread_id = t.id)
			RETURNING t.id`,
			tx.userID.String(), loserIDs)
		if err != nil {
			return res, fmt.Errorf("store: rethread (delete empty threads): %w", err)
		}
		var gone []ulid.ULID
		for thRows.Next() {
			var id string
			if err := thRows.Scan(&id); err != nil {
				thRows.Close()
				return res, fmt.Errorf("store: rethread (scan deleted thread): %w", err)
			}
			u, err := ulid.Parse(id)
			if err != nil {
				thRows.Close()
				return res, fmt.Errorf("store: rethread parse thread id %q: %w", id, err)
			}
			gone = append(gone, u)
		}
		thRows.Close()
		if err := thRows.Err(); err != nil {
			return res, fmt.Errorf("store: rethread (delete empty threads): %w", err)
		}

		// 4. The invariant: every mutation of a JMAP-visible object appends a
		//    change_log row in the same transaction. Three kinds here — the
		//    moved Emails, the keeper Thread, and each Thread that ceased to
		//    exist. AppendChanges (#417) does the Email batch in two
		//    statements rather than taking the change_seq row lock per row.
		if err := tx.AppendChanges(ctx, ObjectEmail, moved, OpUpdated); err != nil {
			return res, err
		}
		if _, err := tx.AppendChange(ctx, ObjectThread, g.Keeper, OpUpdated); err != nil {
			return res, err
		}
		for _, t := range gone {
			if _, err := tx.AppendChange(ctx, ObjectThread, t, OpDestroyed); err != nil {
				return res, err
			}
		}

		res.Groups++
		res.MessagesMoved += len(moved)
		res.ThreadsDeleted += len(gone)
		res.ThreadsAffected++
	}
	return res, nil
}
