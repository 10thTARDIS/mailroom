// SPDX-License-Identifier: AGPL-3.0-only

package store

import (
	"context"
	"fmt"

	"github.com/oklog/ulid/v2"
)

// WHERE A READER GETS A MESSAGE'S AREA (ADR-049 §5, step 3).
//
// A message's area is either WORKED OUT by the resolver (areaJoinSQL, the
// oracle) or READ from the stored column app.messages.area_id, which triggers
// keep equal to the resolver's answer. A reader may use the column only for a
// user whose column is complete: app.area_index_state.ready_at, set by
// `mailroom-admin area-index --apply` once it saw zero disagreements. Every
// reader asks areaIndexReady once and builds its SQL for that answer; a user
// who is not ready gets exactly the resolver SQL it always had, so nothing
// changes for them until the operator runs the backfill.

// AreaSourceStored and AreaSourceResolver name the two ways a reader gets the
// area; plan-check prints them.
const (
	AreaSourceStored   = "stored"
	AreaSourceResolver = "resolver"
)

// areaIndexReady reports whether the caller's stored area column is complete.
// One indexed row read, cached for the rest of the transaction (the only
// writer of the flag, MarkAreaIndexReady, drops the cache). It fails closed:
// an error is returned, never "ready".
func (tx *UserTx) areaIndexReady(ctx context.Context) (bool, error) {
	if tx.areaReady != nil {
		return *tx.areaReady, nil
	}
	var ready bool
	if err := tx.tx.QueryRow(ctx,
		`SELECT EXISTS (SELECT 1 FROM app.area_index_state WHERE user_id = $1 AND ready_at IS NOT NULL)`,
		tx.userID.String()).Scan(&ready); err != nil {
		return false, fmt.Errorf("store: area index readiness: %w", err)
	}
	tx.areaReady = &ready
	return ready, nil
}

// AreaSource says which of the two the caller's readers use right now.
func (tx *UserTx) AreaSource(ctx context.Context) (string, error) {
	ready, err := tx.areaIndexReady(ctx)
	if err != nil {
		return "", err
	}
	if ready {
		return AreaSourceStored, nil
	}
	return AreaSourceResolver, nil
}

// areaSourceSQL returns the joins a FROM clause over `app.messages m` needs
// and the expression that is m's resolved area (NULL = Needs sorting). Stored:
// no joins, the column. Not stored: areaJoinSQL's joins and areaResolvedSQL,
// byte for byte what the callers wrote before.
func areaSourceSQL(stored bool, bind func(any) string, restrict func(col string) string) (joins, resolved string) {
	if stored {
		return "", "m.area_id"
	}
	joins, _, _ = areaJoinSQL(bind, restrict)
	return joins, areaResolvedSQL
}

// areaPredicateStored is AreaPredicate over the stored column. TWO-VALUED, as
// AreaPredicate is: a search can wrap it in NOT, and `m.area_id = x` is NULL
// (not false) for a message with no area. The first form keeps the plain
// equality a top-level qual the planner can match to
// messages_user_area_arrival.
func areaPredicateStored(areaID *ulid.ULID, bind func(any) string) string {
	if areaID == nil {
		return `m.area_id IS NULL`
	}
	return `(m.area_id IS NOT NULL AND m.area_id = ` + bind(areaID.String()) + `)`
}
