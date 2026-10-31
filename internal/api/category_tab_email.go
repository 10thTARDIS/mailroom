// SPDX-License-Identifier: AGPL-3.0-only

package api

import (
	"context"

	"github.com/oklog/ulid/v2"

	"mailroom/internal/store"
	"mailroom/internal/tabs"
)

// THE CATEGORY TAB ON Email — the property and the query filter (#228, ADR-022).
//
// A message's tab is DERIVED: COALESCE(the tabs its mapped labels point at, the
// stored classifier verdict), then folded against the tabs this user has
// switched on. Nothing about it is stored on the Email, which is why it is
// computed here rather than read off store.EmailView.
//
// BOTH HALVES DO THE FOLD IN GO, deliberately. `Email/query`'s inTab expands the
// requested tab into the set that lands there, and `Email.categoryTab` folds the
// resolved answer. Neither expresses tabs.Fold in SQL, so there is exactly one
// implementation of "a disabled tab's mail appears in Primary".

// expandTabFilter turns the tab a client asked for into the set of resolved tabs
// that actually land there.
//
// Asking for Primary means primary PLUS every disabled tab, because that is
// where their mail folds. Asking for an enabled tab means that tab alone.
//
// A DISABLED TAB IS REFUSED rather than answered with an empty list. A client
// should not be offering a tab the user switched off, and an empty result would
// read as "this tab has no mail" — a false statement about the mailbox, and the
// kind that is hard to notice. `invalidArguments` says what actually happened.
func (a *API) expandTabFilter(ctx context.Context, userID ulid.ULID, name string) ([]store.Tab, *MethodError) {
	want, ok := tabs.Parse(name)
	if !ok {
		return nil, &MethodError{Type: errInvalidArguments,
			Description: "inTab is not a category tab"}
	}
	var enabled map[tabs.Tab]bool
	if mErr := a.withUserTx(ctx, userID, func(tx *store.UserTx) *MethodError {
		e, err := tx.EnabledTabs(ctx)
		if err != nil {
			return &MethodError{Type: errServerFail, Description: err.Error()}
		}
		enabled = e
		return nil
	}); mErr != nil {
		return nil, mErr
	}
	if !enabled[want] {
		return nil, &MethodError{Type: errInvalidArguments,
			Description: "inTab names a tab this account has switched off; its mail is in primary"}
	}
	out := []store.Tab{want}
	if want == tabs.Primary {
		for _, t := range tabs.All {
			if !enabled[t] {
				out = append(out, t)
			}
		}
	}
	return out, nil
}

// resolveCategoryTabs answers the `categoryTab` property for a batch of
// messages, or nil when the caller did not ask for it.
//
// OPT-IN, like every other Mailroom Email property: `categoryTab` is not in
// defaultEmailProperties, so this join and the five-row enabled-set read cost
// nothing until a client names it. That is the same property-level gating the
// triage and organize extensions use.
func (a *API) resolveCategoryTabs(ctx context.Context, userID ulid.ULID, ids []ulid.ULID, props map[string]bool) (map[ulid.ULID]store.ResolvedTab, *MethodError) {
	if !props["categoryTab"] || len(ids) == 0 {
		return nil, nil
	}
	var out map[ulid.ULID]store.ResolvedTab
	if mErr := a.withUserTx(ctx, userID, func(tx *store.UserTx) *MethodError {
		enabled, err := tx.EnabledTabs(ctx)
		if err != nil {
			return &MethodError{Type: errServerFail, Description: err.Error()}
		}
		out, err = tx.ResolveTabs(ctx, ids, enabled)
		if err != nil {
			return &MethodError{Type: errServerFail, Description: err.Error()}
		}
		return nil
	}); mErr != nil {
		return nil, mErr
	}
	return out, nil
}

// emailCategoryTab renders the Email.categoryTab property.
//
// NULL WHEN NOTHING HAS DECIDED — no mapped label and no stored classifier
// verdict — rather than "primary". That distinction is the whole reason this is
// an object rather than a bare string: such a message is also INVISIBLE to an
// inTab query (ADR-022), so a client that rendered it as Primary would put it in
// a tab whose own list does not contain it. Null says "not yet classified", and
// `mailroom-admin classify-tabs` is what fixes it.
//
// `fromMapping` is exposed because the two sources are not equally trustworthy:
// a tab from the label mapping is the user's own decision, while a classified
// one is a guess whose measured precision on the hard boundary is 0.418 (#219).
// A client that wants to present the second more tentatively needs to be able to
// tell them apart.
func emailCategoryTab(r store.ResolvedTab, ok bool) any {
	if !ok || !r.Classified {
		return nil
	}
	return map[string]any{
		"tab":         string(r.Tab),
		"fromMapping": r.FromMapping,
	}
}
