// SPDX-License-Identifier: AGPL-3.0-only

package store

import (
	"context"
	"fmt"

	"github.com/oklog/ulid/v2"
)

// THE LINE OVERRIDE (#464, migration 00049) — the WRITE and READ sides of
// app.messages.line_side. The SPLIT itself lives in exactly one place,
// lineSideSQL (areas_resolve.go), which every consumer of "which side is this
// on" goes through; this file is only how a placement gets written and how a
// client reads back the one it made.
//
// #375 §5: Shift+↑/↓ moves a row across the desk's line, and the crossing IS
// the correction the priority model learns from. Recording it as a
// feedback_event alone left the row where it was until the next retrain — and
// left the drawer sweep free to archive a message the user had just pulled up.

// LineSide is which side of the desk's line a message sits on. The empty
// value is not one of them: "no override" is the ABSENCE of a row's value
// (SQL NULL, and an absent map key in LineSides), never LineSide("").
type LineSide string

const (
	// LineSideSurface is above the line — the user says this matters.
	LineSideSurface LineSide = "surface"
	// LineSideDrawer is below it — the user says it does not, and the drawer
	// sweep may archive it once it is old enough.
	LineSideDrawer LineSide = "drawer"
)

// Valid reports whether s is one of the two sides the database's CHECK
// constraint accepts. Validating in Go as well as in SQL is what keeps a
// typo'd side a clean error instead of a constraint violation that poisons
// the caller's transaction.
func (s LineSide) Valid() bool {
	return s == LineSideSurface || s == LineSideDrawer
}

// SetLineSide records the USER'S OWN placement of a message relative to the
// desk's line, and appends the Email `updated` change_log row in the SAME
// transaction (the load-bearing invariant, CLAUDE.md) — the message moves
// between the surface and the drawer for every reader, so every client has to
// hear about it through /changes exactly as it hears about a flag or a move.
//
// It sets line_side_at to now() on every call, including one that repeats the
// side already stored: the timestamp is "when the user last said so", which is
// the only reading that keeps a re-affirmation from looking older than it is.
// A re-affirmation still appends a change row, because a client's cursor
// cannot distinguish "unchanged" from "never told", and the cost is one row.
//
// ⚠ NOTHING ELSE WRITES THESE COLUMNS. Scoring never clears them (a user's
// placement outranks the model and survives a rescoring), and Triage/set's
// model reset/rollback deliberately leaves them alone — see triage.go.
//
// ⚠ SAVEPOINT-GUARDED, like EmitFeedbackEvent beside which it is called
// (api/triage.go corrects a BATCH of ids in one transaction): a failure here
// must cost that one id, not the whole batch, and without a savepoint the
// first failed statement would abort the outer transaction and take every
// other correction with it. The UPDATE and its change row are inside the SAME
// savepoint, so they roll back together — a placement can never land without
// its change_log row, nor a change row without its placement.
//
// A message that does not exist for this caller (including another user's id,
// which RLS makes indistinguishable) is ErrMessageNotFound, and NOTHING is
// written — no update, no change row.
func (tx *UserTx) SetLineSide(ctx context.Context, id ulid.ULID, side LineSide) error {
	if !side.Valid() {
		return fmt.Errorf("store: set line side: unknown side %q", string(side))
	}
	return tx.inSavepoint(ctx, func() error {
		tag, err := tx.tx.Exec(ctx, `
			UPDATE app.messages
			   SET line_side = $3, line_side_at = now()
			 WHERE user_id = $1 AND id = $2`,
			tx.userID.String(), id.String(), string(side))
		if err != nil {
			return fmt.Errorf("store: set line side: %w", err)
		}
		if tag.RowsAffected() == 0 {
			return ErrMessageNotFound
		}
		_, err = tx.AppendChange(ctx, ObjectEmail, id, OpUpdated)
		return err
	})
}

// LineSides returns the placements the user has made among the given message
// ids. A message with NO override is simply ABSENT from the map — the same
// shape DrawerArchivedAt uses, and how Email.lineSide renders null: an
// absence here means "the score decides", not "surface".
func (tx *UserTx) LineSides(ctx context.Context, ids []ulid.ULID) (map[ulid.ULID]LineSide, error) {
	out := make(map[ulid.ULID]LineSide, len(ids))
	if len(ids) == 0 {
		return out, nil
	}
	rows, err := tx.tx.Query(ctx, `
		SELECT id, line_side FROM app.messages
		 WHERE user_id = $1 AND id = ANY($2) AND line_side IS NOT NULL`,
		tx.userID.String(), ulidStrings(ids))
	if err != nil {
		return nil, fmt.Errorf("store: line sides: %w", err)
	}
	defer rows.Close()
	for rows.Next() {
		var idStr, side string
		if err := rows.Scan(&idStr, &side); err != nil {
			return nil, fmt.Errorf("store: scan line side: %w", err)
		}
		mid, err := ulid.Parse(idStr)
		if err != nil {
			return nil, fmt.Errorf("store: parse line side message id %q: %w", idStr, err)
		}
		out[mid] = LineSide(side)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("store: line sides: %w", err)
	}
	return out, nil
}
