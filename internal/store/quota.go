// SPDX-License-Identifier: AGPL-3.0-only

package store

import (
	"context"
	"fmt"
)

// Per-user quota reads (spec §10, matrix §14).
//
// This file exposes the raw limit + current usage; the ALLOW/DENY decision and
// the rejection shape live one layer up in internal/api (Quota.Admits), because
// the API layer owns the decision and only internal/store may touch the database
// (CLAUDE.md). Returning ints rather than an api.Quota keeps the dependency
// pointing the right way — store never imports api.

// QuotaAdmits is the ONE definition of the quota allow/deny rule, kept in this
// leaf package so every enforcement point makes the identical decision: the API
// layer (api.Quota.Admits delegates here — the blob upload endpoint, Email/set
// create) and the ingest path (internal/ingest, which may not import internal/api
// but must decide the same way mid-sync). Duplicating the rule is exactly the
// drift that would let one path admit what another refuses.
//
// The semantics are copied verbatim from api.Quota (quota.go):
//   - a non-positive limit is UNLIMITED (the schema's 0 = "no cap"), so it admits
//     anything — getting this wrong in either direction would either block ALL
//     ingest or enforce nothing;
//   - a non-positive delta (a delete, a dedup that adds no bytes, a no-op) always
//     admits;
//   - otherwise the addition must stay within the limit: used + delta <= limit.
func QuotaAdmits(limit, used, delta int64) bool {
	if limit <= 0 || delta <= 0 {
		return true
	}
	return used+delta <= limit
}

// StorageQuota returns the caller's storage limit (app.users.quota_storage_bytes)
// and their current usage (the sum of size_stored across their blobs), read in
// this one RLS-scoped transaction. RLS auto-scopes both statements to the
// transaction identity, so neither can read another user's figures.
//
// A limit of 0 is the schema's "unlimited" (see api.Quota.Unlimited); this method
// reports it verbatim and lets the decision layer interpret it. Usage is summed
// over app.blobs (stored ciphertext bytes), which is what a storage cap actually
// bounds — the on-disk footprint, dedup included, since a shared blob is one row.
func (tx *UserTx) StorageQuota(ctx context.Context) (limit, used int64, err error) {
	if err = tx.tx.QueryRow(ctx,
		`SELECT quota_storage_bytes FROM app.users WHERE id = $1`,
		tx.userID.String()).Scan(&limit); err != nil {
		return 0, 0, fmt.Errorf("store: storage quota limit: %w", err)
	}
	// ⚠ THE EXCLUSION IS LOAD-BEARING (migration 00038, ADR-026 §3e). Without
	// it, third-party images the image proxy cached would consume the quota that
	// bounds the user's OWN mail — and a user pushed over it stops being able to
	// receive or send. A cached image is derived, re-fetchable, and not theirs.
	if err = tx.tx.QueryRow(ctx,
		`SELECT COALESCE(sum(size_stored), 0) FROM app.blobs WHERE kind <> 'proxy_cache'`).Scan(&used); err != nil {
		return 0, 0, fmt.Errorf("store: storage usage: %w", err)
	}
	return limit, used, nil
}
