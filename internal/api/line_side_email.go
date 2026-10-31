// SPDX-License-Identifier: AGPL-3.0-only

package api

import (
	"context"

	"github.com/oklog/ulid/v2"

	"mailroom/internal/store"
)

// THE LINE OVERRIDE ON Email (#464) — the opt-in, read-only property,
// mirroring drawer_email.go's resolveDrawerArchivedAt/
// emailDrawerArchivedAtProperty exactly: whether the USER placed this message
// above or below the desk's line is a column nothing else on EmailView reads,
// so it is resolved here in one batch and costs a client that never asks
// exactly nothing.
//
// ⚠ IT IS THE OVERRIDE, NOT THE SIDE. `null` means "the user has not placed
// this one" — the score still decides which side it renders on — NOT
// "surface". A client wanting the effective side asks Email/query with
// `onSurface`, which is the ONE expression that answers that question
// (store.lineSideSQL). What this property is for is the sentence a client can
// only say with it: "you put this above the line."
//
// Read-only: Email/set has no `lineSide` case in its patch switch, so a patch
// naming it is refused as an unsupported property, exactly like `area` and
// `drawerArchivedAt`. Triage/correct is the only way to set one.

// resolveLineSides answers the `lineSide` property for a batch of messages, or
// nil when the caller did not ask for it — the same opt-in gate resolveAreas
// and resolveDrawerArchivedAt use.
func (a *API) resolveLineSides(ctx context.Context, userID ulid.ULID, ids []ulid.ULID, props map[string]bool) (map[ulid.ULID]store.LineSide, *MethodError) {
	if !props["lineSide"] || len(ids) == 0 {
		return nil, nil
	}
	var out map[ulid.ULID]store.LineSide
	if mErr := a.withUserTx(ctx, userID, func(tx *store.UserTx) *MethodError {
		var err error
		out, err = tx.LineSides(ctx, ids)
		if err != nil {
			return &MethodError{Type: errServerFail, Description: err.Error()}
		}
		return nil
	}); mErr != nil {
		return nil, mErr
	}
	return out, nil
}

// emailLineSideProperty renders the Email.lineSide property: "surface" or
// "drawer" when the user placed this message themselves, null when they did
// not (or for a message resolveLineSides was not asked about at all).
func emailLineSideProperty(side store.LineSide, ok bool) any {
	if !ok || !side.Valid() {
		return nil
	}
	return string(side)
}
