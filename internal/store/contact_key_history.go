// SPDX-License-Identifier: AGPL-3.0-only

package store

// The history of a correspondent's key (#815, ADR-040 §11): app.contact_key_events,
// one row per real change, appended in the same savepoint as the change.
//
// change_log cannot be this history: it is compacted (compact-changelog), and it
// records THAT a Contact changed, not what happened to which key. The events do
// not replace the change rows either: every mutation that appends an event also
// appends the Contact `updated` row it always owed (or, for a brand-new key from
// ingest, the documented absence of one).

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/oklog/ulid/v2"

	"mailroom/internal/pgpkeys"
)

// Event kinds (app.contact_key_events.kind; the migration's CHECK is the set).
const (
	// KeyEventFirstSeen: the key was stored for the first time, or a key the
	// user had removed was seen again (revived).
	KeyEventFirstSeen = "first_seen"
	// KeyEventRenewed: a merge moved the key's end of usability later (or to
	// never).
	KeyEventRenewed = "renewed"
	// KeyEventUpdated: a merge changed the stored copy without extending it or
	// revoking it (a new user ID or subkey, typically).
	KeyEventUpdated = "updated"
	// KeyEventRevokedSeen: a merge brought a revocation the stored copy lacked.
	KeyEventRevokedSeen = "revoked_seen"
	// KeyEventPinned: the user trusted or imported it, making it the pin.
	KeyEventPinned = "pinned"
	// KeyEventRefused: the user chose to keep the old key instead.
	KeyEventRefused = "refused"
	// KeyEventRemoved: the user removed it (ContactKey/set destroy).
	KeyEventRemoved = "removed"
)

// KeyEventSourceUser is an event's source when the user, not a key route, made
// the change. The other sources are the ContactKey* route names.
const KeyEventSourceUser = "user"

// ContactKeyEvent is one history row.
type ContactKeyEvent struct {
	ID           ulid.ULID
	ContactKeyID ulid.ULID
	Kind         string
	Source       string
	At           time.Time
	// ExpiresAt is the key's end of usability AFTER the event, read from the
	// packets as stored then. nil: never expires, or unreadable.
	ExpiresAt *time.Time
}

// appendContactKeyEvent writes one event for keyID. pub is the key as stored
// after the change; its usable-until goes in expires_at.
func (tx *UserTx) appendContactKeyEvent(ctx context.Context, keyID ulid.ULID, kind, source string, pub []byte) error {
	var expires *time.Time
	if v, err := pgpkeys.Assess(pub, time.Now().UTC()); err == nil {
		if until, never := v.UsableUntil(); !never {
			u := until.UTC()
			expires = &u
		}
	}
	if _, err := tx.tx.Exec(ctx, `
		INSERT INTO app.contact_key_events (id, user_id, contact_key_id, kind, source, expires_at)
		VALUES ($1, $2, $3, $4, $5, $6)`,
		NewULID().String(), tx.userID.String(), keyID.String(), kind, source, expires); err != nil {
		return fmt.Errorf("store: contact key event: %w", err)
	}
	return nil
}

// contactKeyEventKind names a merge that changed the stored copy: revoked_seen
// when it brought a revocation, renewed when it moved the end of usability
// later, updated otherwise.
func contactKeyEventKind(before, after []byte, now time.Time) string {
	vb, errB := pgpkeys.Assess(before, now)
	va, errA := pgpkeys.Assess(after, now)
	if errA != nil || errB != nil {
		return KeyEventUpdated
	}
	if va.Revoked && !vb.Revoked {
		return KeyEventRevokedSeen
	}
	ub, neverB := vb.UsableUntil()
	ua, neverA := va.UsableUntil()
	switch {
	case neverB:
		return KeyEventUpdated
	case neverA, ua.After(ub):
		return KeyEventRenewed
	default:
		return KeyEventUpdated
	}
}

// ContactKeyHistory returns a key's events, oldest first. found is false when no
// such key (removed or not) is the caller's.
func (tx *UserTx) ContactKeyHistory(ctx context.Context, keyID ulid.ULID) ([]ContactKeyEvent, bool, error) {
	var exists bool
	if err := tx.tx.QueryRow(ctx, `
		SELECT EXISTS (SELECT 1 FROM app.contact_keys WHERE user_id = $1 AND id = $2)`,
		tx.userID.String(), keyID.String()).Scan(&exists); err != nil {
		return nil, false, fmt.Errorf("store: contact key history (key): %w", err)
	}
	if !exists {
		return nil, false, nil
	}
	rows, err := tx.tx.Query(ctx, `
		SELECT id, kind, source, at, expires_at FROM app.contact_key_events
		 WHERE user_id = $1 AND contact_key_id = $2
		 ORDER BY at, id`,
		tx.userID.String(), keyID.String())
	if err != nil {
		return nil, false, fmt.Errorf("store: contact key history: %w", err)
	}
	defer rows.Close()
	out := []ContactKeyEvent{}
	for rows.Next() {
		e := ContactKeyEvent{ContactKeyID: keyID}
		var id string
		if err := rows.Scan(&id, &e.Kind, &e.Source, &e.At, &e.ExpiresAt); err != nil {
			return nil, false, fmt.Errorf("store: scan contact key event: %w", err)
		}
		if e.ID, err = ulid.Parse(id); err != nil {
			return nil, false, fmt.Errorf("store: contact key event id %q: %w", id, err)
		}
		out = append(out, e)
	}
	if err := rows.Err(); err != nil {
		return nil, false, fmt.Errorf("store: contact key history: %w", err)
	}
	return out, true, nil
}

// ErrCheckedTooRecently is returned by ClaimContactKeyCheck when the key was
// refreshed less than the minimum interval ago (#818).
var ErrCheckedTooRecently = errors.New("store: that key was checked too recently")

// ContactKeyLastChecked returns when ContactKey/refresh last asked about the key
// (nil: never). found is false for a key that is not held (or removed).
func (tx *UserTx) ContactKeyLastChecked(ctx context.Context, keyID ulid.ULID) (*time.Time, bool, error) {
	var at *time.Time
	err := tx.tx.QueryRow(ctx, `
		SELECT checked_at FROM app.contact_keys
		 WHERE user_id = $1 AND id = $2`+notRemoved,
		tx.userID.String(), keyID.String()).Scan(&at)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, false, nil
	}
	if err != nil {
		return nil, false, fmt.Errorf("store: contact key checked_at: %w", err)
	}
	return at, true, nil
}

// ClaimContactKeyCheck records that a refresh of keyID starts at now, unless the
// last one was less than minInterval ago (ErrCheckedTooRecently, with the time
// it was). One conditional UPDATE, so two concurrent refreshes cannot both
// claim the same slot.
//
// NO change_log ROW AND NO EVENT, deliberately: checked_at is bookkeeping for
// the interval and is not on the wire (Contact.keys and ContactKey/get do not
// carry it). Only what a refresh FINDS is a change, and the store writes that
// (UpsertContactKeyDetail) with its own event and row.
func (tx *UserTx) ClaimContactKeyCheck(ctx context.Context, keyID ulid.ULID, now time.Time, minInterval time.Duration) (last *time.Time, err error) {
	tag, err := tx.tx.Exec(ctx, `
		UPDATE app.contact_keys SET checked_at = $3
		 WHERE user_id = $1 AND id = $2 AND removed_at IS NULL
		   AND (checked_at IS NULL OR checked_at <= $4)`,
		tx.userID.String(), keyID.String(), now.UTC(), now.UTC().Add(-minInterval))
	if err != nil {
		return nil, fmt.Errorf("store: claim contact key check: %w", err)
	}
	if tag.RowsAffected() == 1 {
		return nil, nil
	}
	at, found, err := tx.ContactKeyLastChecked(ctx, keyID)
	if err != nil {
		return nil, err
	}
	if !found {
		return nil, ErrContactKeyNotFound
	}
	return at, ErrCheckedTooRecently
}
