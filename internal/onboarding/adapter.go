// SPDX-License-Identifier: AGPL-3.0-only

package onboarding

import (
	"context"

	"github.com/oklog/ulid/v2"

	"mailroom/internal/store"
)

// StoreAdapter satisfies AccountStore over a *store.Store, running every call
// inside a Store.UserTx so RLS scopes each statement to the owning user
// (CLAUDE.md). It holds no per-request state; the user identity is passed to
// each method.
type StoreAdapter struct {
	store *store.Store
}

// NewStoreAdapter wraps a *store.Store as an AccountStore.
func NewStoreAdapter(s *store.Store) *StoreAdapter { return &StoreAdapter{store: s} }

var _ AccountStore = (*StoreAdapter)(nil)

// CreateAccount runs UserTx.CreateAccount.
func (a *StoreAdapter) CreateAccount(ctx context.Context, userID ulid.ULID, row store.AccountRow) (ulid.ULID, error) {
	var id ulid.ULID
	err := a.store.UserTx(ctx, userID, func(tx *store.UserTx) error {
		var e error
		id, e = tx.CreateAccount(ctx, row)
		return e
	})
	return id, err
}

// SetAccountSyncEnabled runs UserTx.SetAccountSyncEnabled.
func (a *StoreAdapter) SetAccountSyncEnabled(ctx context.Context, userID, accountID ulid.ULID, enabled bool) error {
	return a.store.UserTx(ctx, userID, func(tx *store.UserTx) error {
		return tx.SetAccountSyncEnabled(ctx, accountID, enabled)
	})
}
