// SPDX-License-Identifier: AGPL-3.0-only

package api

import (
	"context"
	"fmt"

	"github.com/oklog/ulid/v2"

	"mailroom/internal/auth"
	"mailroom/internal/store"
)

// NewResolveUser builds the auth.ResolveUser the orchestrator passes to
// auth.New: it maps a verified OIDC subject to a mailroom user id via the store
// (the SECURITY DEFINER app.user_id_for_subject, since login has no RLS identity
// yet). It is LOOKUP ONLY — operator/user provisioning is issue #24; an unknown
// subject aborts the login with an error rather than auto-creating a user.
func NewResolveUser(st *store.Store) auth.ResolveUser {
	return func(ctx context.Context, claims auth.Claims) (ulid.ULID, error) {
		if claims.Subject == "" {
			return ulid.ULID{}, fmt.Errorf("api: resolve user: empty OIDC subject")
		}
		id, found, err := st.UserIDForSubject(ctx, claims.Subject)
		if err != nil {
			return ulid.ULID{}, fmt.Errorf("api: resolve user: %w", err)
		}
		if !found {
			return ulid.ULID{}, fmt.Errorf("api: resolve user: no mailroom user for subject %q", claims.Subject)
		}
		return id, nil
	}
}
