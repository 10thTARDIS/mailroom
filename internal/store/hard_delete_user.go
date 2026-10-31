// SPDX-License-Identifier: AGPL-3.0-only

package store

import (
	"context"
	"fmt"

	"github.com/oklog/ulid/v2"
)

// The destructive half of #54.
//
// ⚠ THIS IS THE ONE CALL IN THE STORE THAT CANNOT BE UNDONE, and everything
// about its shape is chosen so that a mistake is loud rather than quiet.

// ErrHardDeleteNoRows is returned when the delete removed nothing.
//
// ⚠ IT IS AN ERROR, NOT A ZERO RETURNED QUIETLY. Zero rows is exactly what the
// missing `users_owner_delete` policy produced for the whole life of this
// project — a DELETE that reported success and did nothing. A caller that
// treated "0" as an ordinary outcome would reproduce that failure with the fix
// in place, so the store refuses to let it be ignored.
var ErrHardDeleteNoRows = fmt.Errorf("store: hard delete removed no rows")

// HardDeleteUser destroys a user and everything that cascades from them.
//
// It runs through app.hard_delete_user, a SECURITY DEFINER owned by
// mailroom_owner: mailroom_app has no DELETE grant on app.users and no policy
// there, so this is the single narrow way in — the same shape provisioning has.
// The definer writes an audit tombstone BEFORE the delete, recording the user id
// in a column the cascade cannot blank.
//
// ⚠ IT TAKES A SystemTx, NOT A UserTx, AND THAT IS FORCED. A UserTx sets
// mailroom.user_id and every policy filters to that user — but the user is being
// removed, and the definer must be able to see and delete the row regardless.
// This is cross-user work through a narrow definer, which is precisely what
// SystemTx exists for (CLAUDE.md, data-model §5).
func (tx *SystemTx) HardDeleteUser(ctx context.Context, userID ulid.ULID, actor string) error {
	if actor == "" {
		// The tombstone's whole purpose is saying WHO did this. An empty actor
		// would leave a record that answers every question except that one.
		return fmt.Errorf("store: hard delete requires an actor")
	}
	var deleted int
	if err := tx.tx.QueryRow(ctx,
		`SELECT app.hard_delete_user($1, $2)`, userID.String(), actor).Scan(&deleted); err != nil {
		return fmt.Errorf("store: hard delete user: %w", err)
	}
	if deleted == 0 {
		return ErrHardDeleteNoRows
	}
	if deleted != 1 {
		// A primary key cannot match twice. If it did, something is wrong that
		// no amount of proceeding will make better.
		return fmt.Errorf("store: hard delete removed %d rows for one id", deleted)
	}
	return nil
}
