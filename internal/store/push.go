// SPDX-License-Identifier: AGPL-3.0-only

package store

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/oklog/ulid/v2"
)

// Web Push device registrations (spec §12, mailroom#42). A push_subscriptions row
// is a browser's push endpoint plus the keys needed to encrypt to it. Like
// account_sync_status, it is OPERATIONAL data read on demand (WebPushSubscription/
// get and the sync-loop notifier), NOT a JMAP-synced object: a write here appends
// NO change_log row and it is not a store ObjectType (CLAUDE.md, ADR-016).
//
// Every method runs inside a UserTx, so RLS (app.push_subscriptions FORCE ROW
// LEVEL SECURITY on app.current_user_id()) scopes every read and write to the
// caller's own rows — no identity, no rows (fail closed). The sync-loop notifier
// reaches these through a per-user UserTx opened from the sync fan-out's userID,
// never a bypass (ADR-016).

// PushSubscription is one stored device registration. The encryption keys
// (KeysP256dh, KeysAuth) are WRITE-ONLY at the JMAP surface — they are read only
// by the server-side sender and never returned to a client.
type PushSubscription struct {
	ID         ulid.ULID
	Endpoint   string
	KeysP256dh string
	KeysAuth   string
	DeviceName string
	// NotifyMode is this device's push mode (#59): one of the PushMode*
	// constants. It replaces important_only, which nothing ever read (#626
	// §E) and which this package no longer reads or writes either.
	NotifyMode string
	CreatedAt  time.Time
	ExpiredAt  *time.Time
}

// The per-device push modes (app.push_subscriptions.notify_mode, #59). What
// each one lets through is internal/notify's decision; the store only stores
// the word and counts the mail (PushCounts).
const (
	PushModeAll            = "all"
	PushModeAboveLine      = "above_line"
	PushModeVIP            = "vip"
	PushModeAboveLineOrVIP = "above_line_or_vip"
)

// ValidPushMode reports whether m is one of the four modes the schema accepts.
func ValidPushMode(m string) bool {
	switch m {
	case PushModeAll, PushModeAboveLine, PushModeVIP, PushModeAboveLineOrVIP:
		return true
	}
	return false
}

// ErrPushModeInvalid is returned for a mode outside the closed set, before any
// statement runs.
var ErrPushModeInvalid = errors.New("store: unknown push mode")

// PushSubscriptionInput is the create/upsert payload.
type PushSubscriptionInput struct {
	Endpoint   string
	KeysP256dh string
	KeysAuth   string
	DeviceName string
	// NotifyMode, when non-nil, sets the device's mode. Nil leaves the mode of
	// a re-subscribed endpoint exactly as the user last chose it, and gives a
	// new one the column default ('all') — the browser re-subscribes the same
	// endpoint on every "turn on", and that must not silently reset a choice.
	NotifyMode *string
	// SessionID is the browser session registering the subscription (ADR-048
	// section 6), so signing that device out can delete it. Nil for dev-auth and
	// for anything with no session. A re-registration OVERWRITES it, including
	// with nil: the subscription belongs to whoever registered it last.
	SessionID *ulid.ULID
}

// CreatePushSubscription upserts a device registration keyed on (user_id,
// endpoint): re-subscribing the same endpoint UPDATES the stored keys and name
// (and clears any prior expired_at) rather than creating a duplicate row, and
// returns the SAME id. This matches the browser's behavior of re-subscribing
// the same endpoint after a key rotation. The push mode is updated only when
// the input names one (PushSubscriptionInput.NotifyMode).
func (tx *UserTx) CreatePushSubscription(ctx context.Context, in PushSubscriptionInput) (PushSubscription, error) {
	if in.NotifyMode != nil && !ValidPushMode(*in.NotifyMode) {
		return PushSubscription{}, ErrPushModeInvalid
	}
	id := NewULID()
	var (
		returnedID string
		createdAt  time.Time
		mode       string
		sessionID  *string
	)
	if in.SessionID != nil {
		sid := in.SessionID.String()
		sessionID = &sid
	}
	err := tx.tx.QueryRow(ctx, `
		INSERT INTO app.push_subscriptions
			(id, user_id, endpoint, keys_p256dh, keys_auth, device_name, notify_mode, session_id)
		VALUES ($1, $2, $3, $4, $5, $6, COALESCE($7, 'all'), $8)
		ON CONFLICT (user_id, endpoint) DO UPDATE SET
			keys_p256dh = EXCLUDED.keys_p256dh,
			keys_auth   = EXCLUDED.keys_auth,
			device_name = EXCLUDED.device_name,
			notify_mode = COALESCE($7, app.push_subscriptions.notify_mode),
			session_id  = EXCLUDED.session_id,
			expired_at  = NULL
		RETURNING id, created_at, notify_mode`,
		id.String(), tx.userID.String(), in.Endpoint, in.KeysP256dh, in.KeysAuth,
		in.DeviceName, in.NotifyMode, sessionID).Scan(&returnedID, &createdAt, &mode)
	if err != nil {
		return PushSubscription{}, fmt.Errorf("store: create push subscription: %w", err)
	}
	rid, err := ulid.Parse(returnedID)
	if err != nil {
		return PushSubscription{}, fmt.Errorf("store: create push subscription id %q: %w", returnedID, err)
	}
	return PushSubscription{
		ID:         rid,
		Endpoint:   in.Endpoint,
		KeysP256dh: in.KeysP256dh,
		KeysAuth:   in.KeysAuth,
		DeviceName: in.DeviceName,
		NotifyMode: mode,
		CreatedAt:  createdAt,
	}, nil
}

// SetPushSubscriptionMode changes one device's push mode (#59). found is false
// when the id does not exist for the caller (another user's id included, by
// RLS). Not a change_log object (see the file header), so no change row.
func (tx *UserTx) SetPushSubscriptionMode(ctx context.Context, id ulid.ULID, mode string) (found bool, err error) {
	if !ValidPushMode(mode) {
		return false, ErrPushModeInvalid
	}
	tag, err := tx.tx.Exec(ctx, `
		UPDATE app.push_subscriptions SET notify_mode = $3
		 WHERE user_id = $1 AND id = $2`,
		tx.userID.String(), id.String(), mode)
	if err != nil {
		return false, fmt.Errorf("store: set push subscription mode: %w", err)
	}
	return tag.RowsAffected() > 0, nil
}

// ListActivePushSubscriptions returns the caller's subscriptions that have not
// been marked expired (expired_at IS NULL) — the set the sync-loop notifier
// actually sends to. Keys are included: the sender needs them.
func (tx *UserTx) ListActivePushSubscriptions(ctx context.Context) ([]PushSubscription, error) {
	rows, err := tx.tx.Query(ctx, `
		SELECT id, endpoint, keys_p256dh, keys_auth, device_name, notify_mode, created_at, expired_at
		  FROM app.push_subscriptions
		 WHERE user_id = $1 AND expired_at IS NULL
		 ORDER BY created_at, id`,
		tx.userID.String())
	if err != nil {
		return nil, fmt.Errorf("store: list active push subscriptions: %w", err)
	}
	defer rows.Close()
	return scanPushSubscriptions(rows)
}

// ListPushSubscriptions returns ALL of the caller's subscriptions (active and
// expired), ordered stably — the WebPushSubscription/get "ids:null" surface.
func (tx *UserTx) ListPushSubscriptions(ctx context.Context) ([]PushSubscription, error) {
	rows, err := tx.tx.Query(ctx, `
		SELECT id, endpoint, keys_p256dh, keys_auth, device_name, notify_mode, created_at, expired_at
		  FROM app.push_subscriptions
		 WHERE user_id = $1
		 ORDER BY created_at, id`,
		tx.userID.String())
	if err != nil {
		return nil, fmt.Errorf("store: list push subscriptions: %w", err)
	}
	defer rows.Close()
	return scanPushSubscriptions(rows)
}

// GetPushSubscriptions returns the caller's subscriptions for the requested ids
// (RLS-scoped: another user's id is simply absent). Order is unspecified; the
// caller re-associates by id.
func (tx *UserTx) GetPushSubscriptions(ctx context.Context, ids []ulid.ULID) ([]PushSubscription, error) {
	if len(ids) == 0 {
		return nil, nil
	}
	rows, err := tx.tx.Query(ctx, `
		SELECT id, endpoint, keys_p256dh, keys_auth, device_name, notify_mode, created_at, expired_at
		  FROM app.push_subscriptions
		 WHERE user_id = $1 AND id = ANY($2)`,
		tx.userID.String(), ulidStrings(ids))
	if err != nil {
		return nil, fmt.Errorf("store: get push subscriptions: %w", err)
	}
	defer rows.Close()
	return scanPushSubscriptions(rows)
}

// DeletePushSubscription removes a subscription. found is false when the id does
// not exist for the caller (indistinguishable from another user's, by RLS).
func (tx *UserTx) DeletePushSubscription(ctx context.Context, id ulid.ULID) (found bool, err error) {
	var deleted string
	err = tx.tx.QueryRow(ctx,
		`DELETE FROM app.push_subscriptions WHERE user_id = $1 AND id = $2 RETURNING id`,
		tx.userID.String(), id.String()).Scan(&deleted)
	if err != nil {
		if isNoRows(err) {
			return false, nil
		}
		return false, fmt.Errorf("store: delete push subscription: %w", err)
	}
	return true, nil
}

// MarkPushSubscriptionExpired stamps expired_at on a subscription the push
// service reported as gone (HTTP 404/410). It is idempotent (a no-op if already
// expired) and RLS-scoped, so the sync-loop notifier can retire a dead endpoint
// in the same per-user transaction it discovered the death in (ADR-016).
func (tx *UserTx) MarkPushSubscriptionExpired(ctx context.Context, id ulid.ULID) error {
	_, err := tx.tx.Exec(ctx, `
		UPDATE app.push_subscriptions
		   SET expired_at = now()
		 WHERE user_id = $1 AND id = $2 AND expired_at IS NULL`,
		tx.userID.String(), id.String())
	if err != nil {
		return fmt.Errorf("store: mark push subscription expired: %w", err)
	}
	return nil
}

func scanPushSubscriptions(rows interface {
	Next() bool
	Scan(...any) error
	Err() error
}) ([]PushSubscription, error) {
	var out []PushSubscription
	for rows.Next() {
		var (
			ps PushSubscription
			id string
		)
		if err := rows.Scan(&id, &ps.Endpoint, &ps.KeysP256dh, &ps.KeysAuth,
			&ps.DeviceName, &ps.NotifyMode, &ps.CreatedAt, &ps.ExpiredAt); err != nil {
			return nil, fmt.Errorf("store: scan push subscription: %w", err)
		}
		var err error
		if ps.ID, err = ulid.Parse(id); err != nil {
			return nil, fmt.Errorf("store: parse push subscription id %q: %w", id, err)
		}
		out = append(out, ps)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("store: scan push subscriptions: %w", err)
	}
	return out, nil
}
