// SPDX-License-Identifier: AGPL-3.0-only

package api

import (
	"context"
	"time"

	"github.com/oklog/ulid/v2"

	"mailroom/internal/store"
)

// THE DRAWER ARCHIVE MARK ON Email (#443) — the opt-in property, mirroring
// area_email.go's resolveAreas/emailAreaProperty exactly: whether (and when)
// a drawer sweep moved this message to Archive is a fact recorded in
// app.drawer_archives, not a column on EmailView, so it is resolved here in
// one batch and costs nothing until a client actually asks for it.

// resolveDrawerArchivedAt answers the `drawerArchivedAt` property for a batch
// of messages, or nil when the caller did not ask for it — the same opt-in
// gate resolveAreas uses.
func (a *API) resolveDrawerArchivedAt(ctx context.Context, userID ulid.ULID, ids []ulid.ULID, props map[string]bool) (map[ulid.ULID]time.Time, *MethodError) {
	if !props["drawerArchivedAt"] || len(ids) == 0 {
		return nil, nil
	}
	var out map[ulid.ULID]time.Time
	if mErr := a.withUserTx(ctx, userID, func(tx *store.UserTx) *MethodError {
		var err error
		out, err = tx.DrawerArchivedAt(ctx, ids)
		if err != nil {
			return &MethodError{Type: errServerFail, Description: err.Error()}
		}
		return nil
	}); mErr != nil {
		return nil, mErr
	}
	return out, nil
}

// emailDrawerArchivedAtProperty renders the Email.drawerArchivedAt property:
// the UTCDate a drawer sweep archived this message, or null when it was
// never archived by one (or for a message resolveDrawerArchivedAt was not
// asked about at all).
func emailDrawerArchivedAtProperty(at time.Time, ok bool) any {
	if !ok {
		return nil
	}
	return jmapDate(at)
}
