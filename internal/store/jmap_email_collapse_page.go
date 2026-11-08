// SPDX-License-Identifier: AGPL-3.0-only

package store

import (
	"context"
	"fmt"
	"time"

	"github.com/oklog/ulid/v2"
)

// A thread-collapsed area list in arrival order, for a user whose area index is
// ready (ADR-049), is served here by a BOUNDED scan instead of the full ranking
// in QueryEmails. (A user who is not ready keeps the full ranking: the scan is
// only worth having when each batch is an index range and not a fresh
// resolution of the whole area.)
//
// The full ranking numbers every matching message inside its thread, sorts the
// lot and counts it before LIMIT is applied, so the first page of a large area
// costs the whole area. The first message of a thread in sort order is simply
// the first time the scan, walking the area in sort order, meets that thread.
// So the scan reads the area newest-first in batches (a keyset cursor on the
// sort key and id, not OFFSET), keeps each thread's first message, and stops
// once position+limit threads are in hand. Cost follows the page.
//
// The answer is the same as the full ranking's, id for id: the scan uses the
// same predicate and the same total order (sort key, then id, same direction).
// TestCollapsedPageAgreesWithFullRanking holds that, with the old statement as
// the oracle.
//
// `total` (distinct threads in the filter) is free when the scan reached the
// end of the area; otherwise one separate count of distinct threads is run,
// which needs no ranking or sorting, and reads the same index range.

// collapseBatchBase is the first batch's size; it doubles per batch up to
// collapseBatchMax. A variable so a test can make a small fixture loop.
var (
	collapseBatchBase = 256
	collapseBatchMax  = 4096
)

// collapseThreadKey is the grouping the full ranking partitions by: a message
// with no thread is a thread of one.
//
// COLLATE "C" because it is only ever grouped or counted, never shown in
// order: equality is the same bytewise under any deterministic collation, so
// no thread is split or merged, but under the database's default (a
// language-aware locale) every comparison in the sort behind a
// count(DISTINCT) or a PARTITION BY goes through the locale's rules. On an
// area of thousands of messages that sort was most of the exact total's cost.
const collapseThreadKey = `(COALESCE(m.thread_id::text, 'msg:' || m.id::text) COLLATE "C")`

// collapseBoundedEligible reports whether q is the shape the bounded scan
// serves: an area list (inArea or unsorted) on the STORED path, collapsed,
// ordered by effective arrival then id and nothing else. The caller has
// already established that the user's area index is ready. The
// search-relevance, priority and suggestion-confidence orders put extra
// leading keys in front and stay on the full ranking, as do negative
// position/limit values (the full ranking leaves those to Postgres).
func collapseBoundedEligible(q EmailQuery) bool {
	if !q.CollapseThreads || q.Position < 0 || q.Limit < 0 {
		return false
	}
	if q.SortByPriority || q.SortBySuggestionConfidence {
		return false
	}
	if q.SortByRank || len(q.SortKeys) > 0 {
		return false
	}
	// The order the area index holds, and so the one a range scan can walk.
	return q.SortByEffectiveArrival && (q.InArea != nil || q.Unsorted)
}

// queryCollapsedBounded runs the scan. base holds the arguments already bound
// by emailQueryWhere ($1 is the user id).
func (tx *UserTx) queryCollapsedBounded(ctx context.Context, q EmailQuery, from, where string, base []any) ([]ulid.ULID, int, error) {
	dir, cmp := "DESC", "<"
	if q.SortAscending {
		dir, cmp = "ASC", ">"
	}
	key := "m.received_at"
	if q.SortByEffectiveArrival {
		key = "COALESCE(m.snooze_woke_at, m.received_at)"
	}
	limited := q.LimitSet || q.Limit > 0
	want := q.Position + q.Limit // threads to look at, when limited

	var (
		picked    []ulid.ULID
		threads   int
		seen      = map[string]struct{}{}
		exhausted bool
	)
	// A zero-length page (limit 0) asks only for the total: no scan.
	if !limited || q.Limit > 0 {
		var (
			curKey time.Time
			curID  string
			have   bool
		)
		batch := collapseBatchBase
		for {
			args := base[:len(base):len(base)]
			sql := `SELECT /* collapsed page */ m.id, ` + key + `, ` + collapseThreadKey + ` FROM ` + from + ` WHERE (` + where + `)`
			if have {
				args = append(args, curKey, curID)
				n := len(args)
				sql += fmt.Sprintf(` AND (%s, m.id) %s ($%d, $%d)`, key, cmp, n-1, n)
			}
			args = append(args, batch)
			sql += fmt.Sprintf(` ORDER BY %s %s, m.id %s LIMIT $%d`, key, dir, dir, len(args))

			rows, err := tx.tx.Query(ctx, sql, args...)
			if err != nil {
				return nil, 0, fmt.Errorf("store: query emails (bounded): %w", err)
			}
			n := 0
			for rows.Next() {
				var id, tk string
				var k time.Time
				if err := rows.Scan(&id, &k, &tk); err != nil {
					rows.Close()
					return nil, 0, fmt.Errorf("store: scan query id: %w", err)
				}
				n++
				curKey, curID, have = k, id, true
				if _, dup := seen[tk]; dup {
					continue
				}
				seen[tk] = struct{}{}
				threads++
				if threads > q.Position && (!limited || len(picked) < q.Limit) {
					u, err := ulid.Parse(id)
					if err != nil {
						rows.Close()
						return nil, 0, fmt.Errorf("store: parse query id %q: %w", id, err)
					}
					picked = append(picked, u)
				}
			}
			rows.Close()
			if err := rows.Err(); err != nil {
				return nil, 0, fmt.Errorf("store: query emails (bounded): %w", err)
			}
			if n < batch {
				exhausted = true
				break
			}
			if limited && threads >= want {
				break
			}
			if batch < collapseBatchMax {
				batch = min(batch*2, collapseBatchMax)
			}
		}
	}
	if exhausted {
		return picked, threads, nil
	}
	var total int
	if err := tx.tx.QueryRow(ctx,
		`SELECT count(DISTINCT `+collapseThreadKey+`) FROM `+from+` WHERE `+where, base...).Scan(&total); err != nil {
		return nil, 0, fmt.Errorf("store: query emails (count): %w", err)
	}
	return picked, total, nil
}
