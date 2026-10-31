// SPDX-License-Identifier: AGPL-3.0-only

package api

import (
	"context"
	"slices"

	"github.com/oklog/ulid/v2"

	"mailroom/internal/store"
)

// AN API KEY'S ACCOUNT RESTRICTION, inside the methods (#49, ADR-042 §5).
//
// A key restricted to some of the user's accounts reaches only the methods the
// scope table marks accountRestricted, and each of those keeps to the
// restriction in its OWN query/get/set path — not by checking the ids a client
// passed in, which would leave every query that returns ids unguarded:
//
//   - Email/query: store.EmailQuery.RestrictAccounts, in the one WHERE every
//     query path builds;
//   - Email/get: messages outside the accounts are notFound;
//   - Email/set: an update or destroy outside them is notFound, a move into a
//     mailbox outside them is forbidden, and a create must be From one of them
//     and attach nothing (an attachment is a blob id, and blob ids do not
//     carry an account);
//   - EmailSubmission/set: the account the verified From resolves to must be
//     one of them;
//   - Mailbox/get, Mailbox/query, Identity/get: only their accounts' objects.
//
// The membership predicate is store.keyAccountsSQL, shared by the query and the
// id checks, so what a key can find and what it can fetch cannot drift.
//
// LABEL restriction is not implemented in this release: creating a key with
// labelIds is refused (ApiKey/set), so no key ever holds one.

// keyAccounts is the restriction of the request's key, or nil (no key, or a
// key over every account).
func keyAccounts(ctx context.Context) []ulid.ULID {
	p := principalFrom(ctx)
	if !p.restricted() {
		return nil
	}
	return p.key.AccountIDs
}

// keyAllowsAccount reports whether the request may touch this account.
func keyAllowsAccount(ctx context.Context, account ulid.ULID) bool {
	accts := keyAccounts(ctx)
	return accts == nil || slices.Contains(accts, account)
}

// keyMaySendFrom refuses a restricted key's send from another account.
func (a *API) keyMaySendFrom(ctx context.Context, account ulid.ULID) *SetError {
	if keyAllowsAccount(ctx, account) {
		return nil
	}
	return &SetError{Type: setForbidden,
		Description: "this API key cannot send from that account"}
}

// keyVisibleMessages returns which of ids the request may see, or nil when no
// restriction applies (everything RLS lets through is visible).
func keyVisibleMessages(ctx context.Context, tx *store.UserTx, ids []ulid.ULID) (map[ulid.ULID]bool, error) {
	accts := keyAccounts(ctx)
	if accts == nil {
		return nil, nil
	}
	return tx.MessagesInAccounts(ctx, ids, accts)
}

// keyMayArchive refuses a restricted key's Send & archive list (#648) when it
// names any message outside the key's accounts (#49 review). The restriction
// is enforced here, in the application, not by RLS, so without this a
// restricted key could archive up to maxArchiveOnSuccess messages of other
// accounts as a side effect of a send. The whole create is refused; ids are
// never silently dropped. A lookup failure refuses too (fail closed). It runs
// when a send is created, when it is held for approval, and again when an
// approved send runs, since a message's account can change in between.
func keyMayArchive(ctx context.Context, tx *store.UserTx, ids []ulid.ULID) *SetError {
	if len(ids) == 0 || keyAccounts(ctx) == nil {
		return nil
	}
	refused := &SetError{Type: setForbidden, Properties: []string{"archiveOnSuccess"},
		Description: "this API key cannot archive mail outside the accounts it is restricted to"}
	visible, err := keyVisibleMessages(ctx, tx, ids)
	if err != nil {
		return refused
	}
	for _, id := range ids {
		if !visible[id] {
			return refused
		}
	}
	return nil
}

// keyMaySubmitDraft refuses a key's submission of a draft it did not write
// (#49 review, ADR-042 §3): a send key mails out only what it composed, never
// what the user, or another key, has been writing. A session passes.
//
// lock takes the key's draft lock first (#872), so a send cannot slip between
// a discard's or a replacement's check and its move to Trash. The SEND path
// locks. The HOLD path must not: it runs in the admission transaction, which
// already holds the key's api_keys row FOR UPDATE, while a draft create holds
// the draft lock and then needs that row (the FOREIGN KEY check of its
// app.api_key_drafts insert) — the two orders would deadlock. A hold sends
// nothing; the approved run comes back through the send path, which locks
// and checks again.
func keyMaySubmitDraft(ctx context.Context, tx *store.UserTx, emailID ulid.ULID, lock bool) *SetError {
	p := principalFrom(ctx)
	if p == nil {
		return nil
	}
	refused := &SetError{Type: setForbidden, Properties: []string{"emailId"},
		Description: "an API key can send only a draft it wrote itself"}
	if lock {
		if err := tx.LockAPIKeyDrafts(ctx, p.key.ID); err != nil {
			return refused
		}
	}
	st, err := tx.APIKeyDraftStatus(ctx, p.key.ID, emailID)
	if err != nil || !st.Own {
		return refused
	}
	// A version the key replaced or discarded is in Trash, still draft-
	// flagged and still the key's: sending it would send words the key took
	// back. Only a draft that is still in Drafts is sendable by a key (#872).
	if st.IsDraft && (st.Trashed || !st.InDrafts) {
		return &SetError{Type: setForbidden, Properties: []string{"emailId"},
			Description: "this draft was replaced or discarded and is no longer in Drafts; send the current version"}
	}
	return nil
}

// keyMayApplyEmailUpdate refuses an update that would move a restricted key's
// message into a mailbox of another account.
func keyMayApplyEmailUpdate(ctx context.Context, tx *store.UserTx, up store.EmailUpdate) (*SetError, error) {
	if keyAccounts(ctx) == nil || up.MoveToMailboxID == nil {
		return nil, nil
	}
	views, err := tx.GetMailboxes(ctx, []ulid.ULID{*up.MoveToMailboxID})
	if err != nil {
		return nil, err
	}
	if len(views) != 1 || !keyAllowsAccount(ctx, views[0].AccountID) {
		return &SetError{Type: setForbidden,
			Description: "this API key cannot move mail into that mailbox"}, nil
	}
	return nil, nil
}
