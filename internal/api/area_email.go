// SPDX-License-Identifier: AGPL-3.0-only

package api

import (
	"context"

	"github.com/oklog/ulid/v2"

	"mailroom/internal/store"
)

// THE AREA ON Email — the opt-in property and the query-side existence check
// (#442), mirroring category_tab_email.go's resolveCategoryTabs/
// emailCategoryTab exactly: an area is DERIVED (internal/store's
// ResolveAreas/AreaPredicate, the SQL twin of internal/areas.Resolve), so it
// is computed here rather than read off store.EmailView, and it costs
// nothing until a client actually asks for it.

// resolveAreas answers the `area` property for a batch of messages, or nil
// when the caller did not ask for it — the same opt-in gate resolveCategoryTabs
// uses.
//
// It also answers `areaProvenance` (#714) when that is asked for, which needs
// the resolution too: who applied the RESOLVED area's own label. provs is nil
// unless areaProvenance was requested.
func (a *API) resolveAreas(ctx context.Context, userID ulid.ULID, ids []ulid.ULID, props map[string]bool) (out map[ulid.ULID]store.AreaResolution, provs map[ulid.ULID]map[ulid.ULID]string, mErr *MethodError) {
	if (!props["area"] && !props["areaProvenance"]) || len(ids) == 0 {
		return nil, nil, nil
	}
	if mErr := a.withUserTx(ctx, userID, func(tx *store.UserTx) *MethodError {
		var err error
		out, err = tx.ResolveAreas(ctx, ids)
		if err != nil {
			return &MethodError{Type: errServerFail, Description: err.Error()}
		}
		if props["areaProvenance"] {
			provs, err = tx.AreaLabelProvenance(ctx, ids)
			if err != nil {
				return &MethodError{Type: errServerFail, Description: err.Error()}
			}
		}
		return nil
	}); mErr != nil {
		return nil, nil, mErr
	}
	return out, provs, nil
}

// emailAreaProvenanceProperty renders Email.areaProvenance (#714): the
// `applied_by` of the resolved area's OWN label on this message ("user",
// "rule", "desk", "setup", "learned", ...), or null when the message is in
// no area, or is there without its own label (by its tab verdict alone, or
// through another label that feeds the area).
//
// WHY A PROPERTY OF ITS OWN. An area's own label is local-only (ADR-031), and
// `labelProvenance` is keyed by keyword atom, so it has never carried one:
// every area chip read "automatically", whoever filed the message. This is
// the same fact `labelProvenance` gives for mirrored labels, for the one
// label the area chip asks about.
func emailAreaProvenanceProperty(r store.AreaResolution, ok bool, provs map[ulid.ULID]string) any {
	if !ok || r.AreaID == nil {
		return nil
	}
	if by, has := provs[*r.AreaID]; has {
		return by
	}
	return nil
}

// emailAreaProperty renders the Email.area property: the resolved area's id,
// or null for Needs sorting (or for a message resolveAreas was not asked
// about at all).
func emailAreaProperty(r store.AreaResolution, ok bool) any {
	if !ok || r.AreaID == nil {
		return nil
	}
	return r.AreaID.String()
}

// checkAreaExists validates an Email/query `inArea` filter's area id BEFORE
// it reaches the store: an id naming no area of this account must be
// invalidArguments, not a silently empty result — the same reasoning
// expandTabFilter (category_tab_email.go) gives for refusing a disabled tab
// rather than answering with zero rows. GetAreas also materialises the
// default plan, so a first-contact query against a freshly provisioned
// account still resolves a real area rather than reporting a false negative.
func (a *API) checkAreaExists(ctx context.Context, userID ulid.ULID, id ulid.ULID) *MethodError {
	var found bool
	if mErr := a.withUserTx(ctx, userID, func(tx *store.UserTx) *MethodError {
		views, err := tx.GetAreas(ctx, []ulid.ULID{id})
		if err != nil {
			return &MethodError{Type: errServerFail, Description: err.Error()}
		}
		found = len(views) > 0
		return nil
	}); mErr != nil {
		return mErr
	}
	if !found {
		return &MethodError{Type: errInvalidArguments, Description: "inArea names no area of this account"}
	}
	return nil
}
