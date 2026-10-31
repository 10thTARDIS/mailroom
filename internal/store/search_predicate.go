// SPDX-License-Identifier: AGPL-3.0-only

package store

import (
	"context"
	"fmt"
	"strings"

	"github.com/oklog/ulid/v2"
)

// THE STORE HALF OF A SEARCH (#722 area:, #723 is:snoozed / is:muted /
// is:pinned / is:important).
//
// Some search operators ask about state the index cannot hold (an area is
// derived from labels and feeds; a mute lives on the thread; a snooze ends with
// the clock; the line moves with DeskPreferences — none of which appends an
// Email change_log row the indexer would see). internal/search splits a query
// into the part the index answers and a boolean tree of these predicates
// (search.SplitStore); this file compiles that tree into one more condition of
// Email/query's WHERE, so Postgres — which already owns the total, the sort and
// the window — evaluates it over the same rows as every other filter.
//
// EVERY LEAF IS TWO-VALUED. The tree may negate anything (`-area:Work`,
// `-(is:pinned OR from:x)`), and NOT over a NULL is NULL, which WHERE treats as
// false: a three-valued leaf would silently drop rows from BOTH a predicate and
// its negation. So each leaf is a plain boolean over NOT NULL columns or wrapped
// in `IS TRUE` — see each one.
//
// RLS AND SCOPING: the predicate reads only through `m` (the caller's own
// messages) and subqueries bound to $1, the caller's own user id — the
// emailQueryWhere convention AreaPredicate already relies on. Another user's
// area id therefore matches nothing, under RLS and without it.

// SearchOp is one node kind of a SearchPredicate.
type SearchOp int

// The zero value is SearchNone: a predicate nobody filled in matches NOTHING
// (fail closed), never everything.
const (
	SearchNone SearchOp = iota
	SearchAll
	SearchAnd
	SearchOr
	SearchNot
	SearchArea      // resolves to AreaID (store.AreaPredicate)
	SearchIDs       // m.id is one of IDs — a subtree the index answered
	SearchSnoozed   // snoozed right now
	SearchMuted     // its thread is muted
	SearchPinned    // pinned
	SearchAboveLine // above the desk's line (EmailQuery.LineThreshold)
	SearchVIP       // from a contact marked VIP (#59)
)

// SearchPredicate is a boolean tree of store-side search predicates.
type SearchPredicate struct {
	Op     SearchOp
	Kids   []SearchPredicate // SearchAnd / SearchOr; SearchNot has exactly one
	AreaID ulid.ULID         // SearchArea
	IDs    []ulid.ULID       // SearchIDs
}

// Uses reports whether op occurs anywhere in the tree (the caller reads the
// desk's line only when SearchAboveLine does).
func (p SearchPredicate) Uses(op SearchOp) bool {
	if p.Op == op {
		return true
	}
	for _, k := range p.Kids {
		if k.Uses(op) {
			return true
		}
	}
	return false
}

// snoozedSQL is "snoozed right now", shared by Email/query's isSnoozed filter
// and the is:snoozed search operator. now() is the transaction timestamp, so
// every predicate in a single Email/query agrees on what "still snoozed"
// means. Two-valued: a NULL snoozed_until makes the AND false, not NULL.
const snoozedSQL = "(m.snoozed_until IS NOT NULL AND m.snoozed_until > now())"

// searchScoreJoin is the LEFT JOIN SearchAboveLine reads the score through —
// the same primary-key join onSurface uses (#660: one probe per message, not
// one correlated subquery per use of the score), under its own alias so the two
// can coexist in one statement.
const searchScoreJoin = " LEFT JOIN app.triage_scores srch_ts ON srch_ts.user_id = m.user_id AND srch_ts.message_id = m.id"

// searchPredicateSQL compiles the tree. lineThreshold is the desk's line, bound
// only if SearchAboveLine occurs; the caller must have added searchScoreJoin to
// the FROM clause in that case.
func searchPredicateSQL(p SearchPredicate, bind func(any) string, lineThreshold float32) (string, error) {
	var threshold string
	var compile func(p SearchPredicate) (string, error)
	compile = func(p SearchPredicate) (string, error) {
		switch p.Op {
		case SearchNone:
			return "FALSE", nil
		case SearchAll:
			return "TRUE", nil
		case SearchAnd, SearchOr:
			if len(p.Kids) == 0 {
				if p.Op == SearchAnd {
					return "TRUE", nil
				}
				return "FALSE", nil
			}
			parts := make([]string, 0, len(p.Kids))
			for _, k := range p.Kids {
				s, err := compile(k)
				if err != nil {
					return "", err
				}
				parts = append(parts, s)
			}
			sep := " AND "
			if p.Op == SearchOr {
				sep = " OR "
			}
			return "(" + strings.Join(parts, sep) + ")", nil
		case SearchNot:
			if len(p.Kids) != 1 {
				return "", fmt.Errorf("store: search predicate: NOT takes one operand, got %d", len(p.Kids))
			}
			s, err := compile(p.Kids[0])
			if err != nil {
				return "", err
			}
			return "(NOT " + s + ")", nil
		case SearchArea:
			// The resolver Email/query's inArea uses — `(… IN (…)) IS TRUE`,
			// two-valued, a hashed SubPlan built once (areas_resolve.go).
			id := p.AreaID
			return AreaPredicate(&id, bind), nil
		case SearchIDs:
			// An empty set is a real answer ("that subtree matched nothing"):
			// `= ANY('{}')` is false for every row. Two-valued: m.id is the
			// primary key and the array holds no NULL.
			return "(m.id = ANY(" + bind(ulidStrings(p.IDs)) + "))", nil
		case SearchSnoozed:
			return snoozedSQL, nil
		case SearchMuted:
			// Non-correlated, so it is one hashed SubPlan over the user's muted
			// threads (threads_user_muted), probed per message. IS TRUE makes
			// a message with no thread (NULL thread_id) false, not NULL.
			return "((m.thread_id IN (SELECT mt.id FROM app.threads mt" +
				" WHERE mt.user_id = $1 AND mt.muted_at IS NOT NULL)) IS TRUE)", nil
		case SearchPinned:
			return "(m.pinned_at IS NOT NULL)", nil
		case SearchAboveLine:
			// Above the line = the SURFACE side, by the desk's own rule
			// (lineSideSQL): the user's placement if they made one, else the
			// score against the line. Unjudged mail has no side and is not
			// above it; IS TRUE makes that false rather than NULL.
			if threshold == "" {
				threshold = bind(lineThreshold)
			}
			return "((" + lineSideSQL("srch_ts.score", threshold) + " = 'surface') IS TRUE)", nil
		case SearchVIP:
			// The push counter's own definition (contact_flags.go fromVIPSQL):
			// a hashed SubPlan over the caller's VIP addresses, two-valued.
			return fromVIPSQL, nil
		}
		return "", fmt.Errorf("store: search predicate: unknown op %d", p.Op)
	}
	return compile(p)
}

// junkSQL is search's definition of junk mail (#723): the provider's verdict
// (is_junk, mirrored from $junk) OR effectively in a junk-role folder — the
// Junk view's own membership rule — exactly as deleted mail is trashed_at OR
// in a Trash folder (#631). Two-valued (inAnyMailboxSQL is, and is_junk is NOT
// NULL), so its NOT is the exact complement.
func (tx *UserTx) junkSQL(ctx context.Context, bind func(any) string) (string, error) {
	junk, err := tx.junkRoleMailboxIDs(ctx)
	if err != nil {
		return "", err
	}
	if len(junk) == 0 {
		return "(m.is_junk)", nil
	}
	return "(m.is_junk OR " + inAnyMailboxSQL(bind, tx.userID, junk) + ")", nil
}

// AreaIDByName resolves an area name (case-insensitive; area names are unique
// per user in that sense — areas_user_lower_name) to its id: how `area:Work` in
// a search becomes the area the resolver tests for. Another user's area is
// invisible here (RLS plus the explicit user_id), so it resolves to nothing.
//
// It materialises the default areas first, as ListAreas and GetAreas do, so an
// account that has never listed its areas still finds the ones it would see.
func (tx *UserTx) AreaIDByName(ctx context.Context, name string) (ulid.ULID, bool, error) {
	if err := tx.materialiseDefaultAreas(ctx); err != nil {
		return ulid.ULID{}, false, err
	}
	var id string
	err := tx.tx.QueryRow(ctx, `
		SELECT id FROM app.areas
		 WHERE user_id = $1 AND lower(name) = lower($2)
		 ORDER BY id
		 LIMIT 1`,
		tx.userID.String(), strings.TrimSpace(name)).Scan(&id)
	if err != nil {
		if isNoRows(err) {
			return ulid.ULID{}, false, nil
		}
		return ulid.ULID{}, false, fmt.Errorf("store: area by name: %w", err)
	}
	u, err := ulid.Parse(id)
	if err != nil {
		return ulid.ULID{}, false, fmt.Errorf("store: parse area id %q: %w", id, err)
	}
	return u, true, nil
}
