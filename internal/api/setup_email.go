// SPDX-License-Identifier: AGPL-3.0-only

package api

import (
	"context"
	"time"

	"github.com/oklog/ulid/v2"

	"mailroom/internal/store"
)

// THE DAY-ONE ARCHIVE MARK ON Email (#446) — the opt-in property, mirroring
// drawer_email.go's resolveDrawerArchivedAt exactly, because it answers the
// neighbouring question and a client renders the two side by side.
//
// ⚠ THE TWO MARKS MEAN DIFFERENT THINGS AND A CLIENT MUST NOT CONFLATE THEM:
//
//	drawerArchivedAt  the drawer sweep MOVED this message to the Archive folder.
//	                  A real server-side move; every other client sees it.
//	deskArchivedAt    day-one setup took this message off the desk and NOTHING
//	                  MOVED. The bytes are exactly where the provider has them,
//	                  the message is still in its folder, and no other client
//	                  saw anything happen at all.
//
// That is why this is a second property and not a widening of the first: "your
// mail was moved" and "your mail was filed away without being moved" are
// different sentences to show a person, and the second one is the promise #446
// makes.

// resolveDeskArchivedAt answers the `deskArchivedAt` property for a batch of
// messages, or nil when the caller did not ask for it — the same opt-in gate
// resolveAreas and resolveDrawerArchivedAt use, so it costs nothing until a
// client names the property.
func (a *API) resolveDeskArchivedAt(ctx context.Context, userID ulid.ULID, ids []ulid.ULID, props map[string]bool) (map[ulid.ULID]time.Time, *MethodError) {
	if !props["deskArchivedAt"] || len(ids) == 0 {
		return nil, nil
	}
	var out map[ulid.ULID]time.Time
	if mErr := a.withUserTx(ctx, userID, func(tx *store.UserTx) *MethodError {
		var err error
		out, err = tx.DeskArchivedAt(ctx, ids)
		if err != nil {
			return &MethodError{Type: errServerFail, Description: err.Error()}
		}
		return nil
	}); mErr != nil {
		return nil, mErr
	}
	return out, nil
}

// emailDeskArchivedAtProperty renders Email.deskArchivedAt: the UTCDate day-one
// setup archived this message in Mailroom, or null when it never did.
func emailDeskArchivedAtProperty(at time.Time, ok bool) any {
	if !ok {
		return nil
	}
	return jmapDate(at)
}
