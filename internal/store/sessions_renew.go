// SPDX-License-Identifier: AGPL-3.0-only

package store

import (
	"context"
	"fmt"
	"time"

	"github.com/oklog/ulid/v2"
)

// Silent session renewal's cross-process lock (#937, ADR-048 section 4).
//
// Authentik treats a refresh token as single-use: presenting a used one again
// is invalid_grant, which revokes the session. Two processes renewing the same
// session at once would therefore sign the user out. RenewSessionLocked
// serialises them with a per-session transaction-scoped advisory lock, held
// across the provider call (which the caller bounds to a few seconds), and
// re-reads the row after taking it so that the loser of the race uses the
// winner's result instead of calling the provider.

// SessionRenewState is what the locked re-read found, and what the renewal left
// behind. RefreshCT is the sealed refresh token as stored (nil when none).
type SessionRenewState struct {
	Revoked       bool
	LastCheckedAt time.Time
	Generation    int64
	RefreshCT     []byte
	// Subject is the user's oidc_subject (visible under users_self).
	Subject string
}

// SessionRenewAction is what the renewal callback wants written.
type SessionRenewAction int

const (
	// SessionRenewLeave writes nothing (a transient failure, or no refresh token).
	SessionRenewLeave SessionRenewAction = iota
	// SessionRenewRotate stores the new tokens and bumps generation. It does
	// NOT stamp last_checked_at: a refresh grant consumes the old token
	// (single-use at Authentik), so the rotation must be durable whatever
	// happens next, but the session is only confirmed once identity has been
	// re-checked (ConfirmSession).
	SessionRenewRotate
	// SessionRenewRefuse revokes the session as refused.
	SessionRenewRefuse
)

// SessionRenewWrite is the callback's answer. RefreshCT and IDTokenCT are sealed
// by the caller; a nil value keeps the stored one (a provider that did not
// rotate the refresh token, or sent no ID token).
type SessionRenewWrite struct {
	Action    SessionRenewAction
	RefreshCT []byte
	IDTokenCT []byte
}

// renewLockNamespace keeps this advisory-lock key space apart from the other
// advisory locks in the package, which hash their own prefixed strings the same
// way.
const renewLockNamespace = "session-renew:"

// RenewSessionLocked runs one renewal of the caller's session under a
// per-session lock. It:
//
//  1. takes pg_advisory_xact_lock(hashtextextended('session-renew:' || id, 0)):
//     the 64-bit key is derived from the session id text by Postgres's own
//     hash, namespaced so it cannot collide with another feature's lock on a
//     bare id;
//  2. re-reads the row FOR UPDATE (so a concurrent revoke or touch waits for us
//     rather than racing the write);
//  3. calls fn, unless the row is revoked or its last_checked_at is no longer
//     older than staleBefore (another process renewed while we waited), in
//     which case fn is NOT called and the current state is returned;
//  4. applies fn's write in the same transaction.
//
// ran reports whether fn was called. The returned state is the state after the
// write. An error from fn rolls the transaction back and is returned; the
// caller must treat it as a transient failure, never as a refusal. fn must not
// start work that outlives the transaction's context.
func (tx *UserTx) RenewSessionLocked(ctx context.Context, id ulid.ULID, staleBefore time.Time,
	fn func(cur SessionRenewState) (SessionRenewWrite, error)) (state SessionRenewState, ran bool, err error) {
	if _, err := tx.tx.Exec(ctx, `SELECT pg_advisory_xact_lock(hashtextextended($1, 0))`,
		renewLockNamespace+id.String()); err != nil {
		return SessionRenewState{}, false, fmt.Errorf("store: lock session renewal: %w", err)
	}
	var revokedAt *time.Time
	if err := tx.tx.QueryRow(ctx, `
		SELECT s.revoked_at, s.last_checked_at, s.generation, s.refresh_ct, u.oidc_subject
		  FROM app.sessions s
		  JOIN app.users u ON u.id = s.user_id
		 WHERE s.id = $1
		   FOR UPDATE OF s`, id.String(),
	).Scan(&revokedAt, &state.LastCheckedAt, &state.Generation, &state.RefreshCT, &state.Subject); err != nil {
		if isNoRows(err) {
			// Not the caller's session (RLS) or gone: nothing to renew, and
			// nothing live to serve.
			return SessionRenewState{Revoked: true}, false, nil
		}
		return SessionRenewState{}, false, fmt.Errorf("store: read session for renewal: %w", err)
	}
	state.Revoked = revokedAt != nil
	if state.Revoked || !state.LastCheckedAt.Before(staleBefore) {
		return state, false, nil
	}

	w, err := fn(state)
	if err != nil {
		return SessionRenewState{}, true, err
	}
	switch w.Action {
	case SessionRenewRotate:
		var refresh, idt []byte
		if len(w.RefreshCT) > 0 {
			refresh = w.RefreshCT
		}
		if len(w.IDTokenCT) > 0 {
			idt = w.IDTokenCT
		}
		// last_checked_at is deliberately not touched here (see SessionRenewRotate).
		if err := tx.tx.QueryRow(ctx, `
			UPDATE app.sessions
			   SET refresh_ct = COALESCE($2, refresh_ct),
			       id_token_ct = COALESCE($3, id_token_ct),
			       generation = generation + 1
			 WHERE id = $1
			RETURNING generation, refresh_ct`, id.String(), refresh, idt,
		).Scan(&state.Generation, &state.RefreshCT); err != nil {
			return SessionRenewState{}, true, fmt.Errorf("store: store renewed session: %w", err)
		}
	case SessionRenewRefuse:
		// Through the one revocation helper, so the session's push subscriptions
		// go in this same transaction.
		if _, err := tx.revokeSessionRow(ctx, id, SessionRevokedRefused); err != nil {
			return SessionRenewState{}, true, fmt.Errorf("store: revoke refused session: %w", err)
		}
		state.Revoked = true
	}
	return state, true, nil
}

// ConfirmSession stamps the session as confirmed with the provider, the last step
// of a renewal once identity has been re-checked. It matches on generation: if
// another process renewed since (generation moved on), nothing is written and
// ok is false, which the caller still counts as confirmed for this request,
// because that other renewal did the confirming. A revoked session is left
// alone and reported as revoked (as is a row that no longer exists), so the
// caller refuses rather than confirming a renewal that a sign-out raced. It
// runs in its own short transaction, outside the renewal lock.
func (tx *UserTx) ConfirmSession(ctx context.Context, id ulid.ULID, generation int64) (at time.Time, ok, revoked bool, err error) {
	// clock_timestamp(), not now(): the provider and identity work have run
	// since this transaction began. The second sub-select reads the row as of
	// the statement's snapshot; the UPDATE never changes revoked_at, so it is
	// the same answer either way.
	var stamped *time.Time
	var gone *bool
	err = tx.tx.QueryRow(ctx, `
		WITH upd AS (
			UPDATE app.sessions SET last_checked_at = clock_timestamp()
			 WHERE id = $1 AND generation = $2 AND revoked_at IS NULL
			RETURNING last_checked_at)
		SELECT (SELECT last_checked_at FROM upd),
		       (SELECT revoked_at IS NOT NULL FROM app.sessions WHERE id = $1)`,
		id.String(), generation).Scan(&stamped, &gone)
	if err != nil {
		return time.Time{}, false, false, fmt.Errorf("store: confirm session: %w", err)
	}
	if gone == nil || *gone {
		return time.Time{}, false, true, nil
	}
	if stamped == nil {
		return time.Time{}, false, false, nil
	}
	return *stamped, true, false, nil
}
