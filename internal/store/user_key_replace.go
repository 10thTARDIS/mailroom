// SPDX-License-Identifier: AGPL-3.0-only

package store

// Replacing an own key in one step (#817, ADR-040 §14): the new key (already
// generated, unbound, by the crypto process) takes the old key's address, its
// WKD publication if it had one, and the old key retires, recording when and by
// what. ONE savepoint, so a failure at any point leaves the old key exactly as
// it was: still bound, still published, not retired.

import (
	"context"
	"errors"
	"fmt"

	"github.com/oklog/ulid/v2"

	"mailroom/internal/wkd"
)

// Replace refusals. Each is a state in which replacing would be wrong, not a
// fault: the API maps them to distinct method errors.
var (
	// ErrKeyRevoked: revoking is its own deliberate act; a revoked key is not
	// replaced (generate a new one, which is what replacing would do anyway).
	ErrKeyRevoked = errors.New("store: that key is revoked")
	// ErrKeyRetired: the key is already retired, so there is nothing to put a
	// new key in the place of.
	ErrKeyRetired = errors.New("store: that key is already retired")
	// ErrKeyNotBound: a replacement takes the old key's address; a key that is
	// not its address's signing key (unbound, or superseded by another bound
	// key) has none to give.
	ErrKeyNotBound = errors.New("store: that key is not bound to an address")
	// ErrReplacementUnusable: the new key is not a fresh, unbound, live key with
	// a private half (the caller's generate went wrong, or raced).
	ErrReplacementUnusable = errors.New("store: the replacement key cannot take the old key's place")
)

// replaceFault is a TEST SEAM: when set, ReplaceUserKey calls it at each named
// stage inside the savepoint and fails with its error, so a test can prove that
// a failure after the bind (or after the retire) leaves nothing behind. Nil in
// production; only this package's tests set it.
var replaceFault func(stage string) error

// ReplaceResult reports what ReplaceUserKey did.
type ReplaceResult struct {
	// Found: the old key exists for this user.
	Found bool
	// IdentityID is the address the new key now signs for.
	IdentityID ulid.ULID
	// Published: the new key is published in WKD (the old one was, and the
	// caller said this instance serves the address).
	Published bool
	// Unbound lists OTHER identities that still named the old key as their
	// default; they are cleared (not moved to the new key, which names only
	// the one address it was generated for).
	Unbound []ulid.ULID
}

// ReplaceUserKey puts newID in oldID's place. publish says whether the new key
// goes into WKD; the caller decides it (the old key was published, and this
// instance serves the address's domain), and it is ignored for an old key that
// was not published.
//
// In ONE savepoint:
//  1. the new key is bound to the old key's identity (the key's identity_id
//     and the identity's default_key_id);
//  2. with publish, it is published there (the identity's WKD hash is the same
//     address's, so it is set if absent);
//  3. the old key retires: decrypt_only, unpublished, retired_at (once),
//     replaced_by = newID. If the new key is not published and the old one was,
//     the identity's WKD hash is cleared when no other key is published there;
//  4. change rows, ONE per object that moved: Key(new), Key(old), the
//     Identity, and any other Identity that defaulted to the old key.
//
// Refusals (no write at all): ErrKeyRevoked, ErrKeyRetired, ErrKeyNotBound,
// ErrReplacementUnusable. found is false when oldID is not the caller's.
func (tx *UserTx) ReplaceUserKey(ctx context.Context, oldID, newID ulid.ULID, publish bool) (ReplaceResult, error) {
	var res ReplaceResult
	fault := func(stage string) error {
		if replaceFault != nil {
			return replaceFault(stage)
		}
		return nil
	}
	err := tx.inSavepoint(ctx, func() error {
		var (
			oldIdentity        *string
			oldRetired         bool
			oldRevoked         *string
			oldPublished       bool
			newIdentity        *string
			newRetired, hasCT  bool
			newRevoked         *string
			identityKind       string
			address, wkdHashed *string
			defaultKey         *string
		)
		err := tx.tx.QueryRow(ctx, `
			SELECT identity_id, decrypt_only, revoked_at::text, wkd_published
			  FROM app.user_keys WHERE user_id = $1 AND id = $2 FOR UPDATE`,
			tx.userID.String(), oldID.String()).Scan(&oldIdentity, &oldRetired, &oldRevoked, &oldPublished)
		if isNoRows(err) {
			return nil
		}
		if err != nil {
			return fmt.Errorf("store: replace key (old): %w", err)
		}
		res.Found = true
		switch {
		case oldRevoked != nil:
			return ErrKeyRevoked
		case oldRetired:
			return ErrKeyRetired
		case oldIdentity == nil:
			return ErrKeyNotBound
		}
		err = tx.tx.QueryRow(ctx, `
			SELECT identity_id, decrypt_only, revoked_at::text, private_key_ct IS NOT NULL
			  FROM app.user_keys WHERE user_id = $1 AND id = $2 FOR UPDATE`,
			tx.userID.String(), newID.String()).Scan(&newIdentity, &newRetired, &newRevoked, &hasCT)
		if isNoRows(err) {
			return fmt.Errorf("%w: it does not exist", ErrReplacementUnusable)
		}
		if err != nil {
			return fmt.Errorf("store: replace key (new): %w", err)
		}
		if newIdentity != nil || newRetired || newRevoked != nil || !hasCT || newID == oldID {
			return ErrReplacementUnusable
		}
		identityID, err := ulid.Parse(*oldIdentity)
		if err != nil {
			return fmt.Errorf("store: replace key: identity id %q: %w", *oldIdentity, err)
		}
		res.IdentityID = identityID
		if err := tx.tx.QueryRow(ctx, `
			SELECT kind, address::text, wkd_hash, default_key_id FROM app.identities
			 WHERE user_id = $1 AND id = $2 FOR UPDATE`,
			tx.userID.String(), identityID.String()).Scan(&identityKind, &address, &wkdHashed, &defaultKey); err != nil {
			if isNoRows(err) {
				return ErrKeyNotBound
			}
			return fmt.Errorf("store: replace key (identity): %w", err)
		}
		// The old key must be the address's signing key, not merely a key that
		// once named it: replacing a key another key already superseded would
		// silently displace that other key.
		if defaultKey == nil || *defaultKey != oldID.String() {
			return ErrKeyNotBound
		}

		publish = publish && oldPublished
		var hash *string
		if publish {
			if identityKind != "fixed" || address == nil {
				return fmt.Errorf("%w: only a key at a fixed address is published", ErrNotPublishable)
			}
			local, _, err := wkd.SplitAddress(*address)
			if err != nil {
				return fmt.Errorf("%w: the address does not parse", ErrNotPublishable)
			}
			h := wkd.HashLocalPart(local)
			hash = &h
		}

		// 1 and 2: the new key takes the address (and the directory entry).
		if _, err := tx.tx.Exec(ctx, `
			UPDATE app.user_keys SET identity_id = $3, wkd_published = $4
			 WHERE user_id = $1 AND id = $2`,
			tx.userID.String(), newID.String(), identityID.String(), publish); err != nil {
			return fmt.Errorf("store: replace key (bind new): %w", err)
		}
		if _, err := tx.tx.Exec(ctx, `
			UPDATE app.identities
			   SET default_key_id = $3, wkd_hash = COALESCE($4, wkd_hash)
			 WHERE user_id = $1 AND id = $2`,
			tx.userID.String(), identityID.String(), newID.String(), hash); err != nil {
			return fmt.Errorf("store: replace key (identity): %w", err)
		}
		if err := fault("bound"); err != nil {
			return err
		}

		// 3: the old key retires, pointing at its replacement.
		if _, err := tx.tx.Exec(ctx, `
			UPDATE app.user_keys
			   SET decrypt_only = true, wkd_published = false,
			       retired_at = COALESCE(retired_at, now()), replaced_by = $3
			 WHERE user_id = $1 AND id = $2`,
			tx.userID.String(), oldID.String(), newID.String()); err != nil {
			return fmt.Errorf("store: replace key (retire old): %w", err)
		}
		if oldPublished && !publish {
			if _, err := tx.clearWKDHashIfLastPublished(ctx, identityID.String(), oldID); err != nil {
				return err
			}
		}
		others, err := tx.identitiesDefaultingTo(ctx, oldID)
		if err != nil {
			return err
		}
		if len(others) > 0 {
			if _, err := tx.tx.Exec(ctx, `
				UPDATE app.identities SET default_key_id = NULL
				 WHERE user_id = $1 AND default_key_id = $2`,
				tx.userID.String(), oldID.String()); err != nil {
				return fmt.Errorf("store: replace key (other identities): %w", err)
			}
			res.Unbound = others
		}
		if err := fault("retired"); err != nil {
			return err
		}

		// 4: one change row per object that moved.
		for _, id := range []ulid.ULID{newID, oldID} {
			if _, err := tx.AppendChange(ctx, ObjectKey, id, OpUpdated); err != nil {
				return err
			}
		}
		for _, id := range append([]ulid.ULID{identityID}, others...) {
			if _, err := tx.AppendChange(ctx, ObjectIdentity, id, OpUpdated); err != nil {
				return err
			}
		}
		res.Published = publish
		return fault("logged")
	})
	if err != nil {
		return ReplaceResult{Found: res.Found}, err
	}
	return res, nil
}

// KeyIsIdentityDefault reports whether keyID is identityID's bound signing key
// (app.identities.default_key_id), whatever its packets say. UserKey/replace
// asks it before generating anything.
func (tx *UserTx) KeyIsIdentityDefault(ctx context.Context, keyID, identityID ulid.ULID) (bool, error) {
	var yes bool
	if err := tx.tx.QueryRow(ctx, `
		SELECT EXISTS (SELECT 1 FROM app.identities WHERE user_id = $1 AND id = $2 AND default_key_id = $3)`,
		tx.userID.String(), identityID.String(), keyID.String()).Scan(&yes); err != nil {
		return false, fmt.Errorf("store: key is identity default: %w", err)
	}
	return yes, nil
}
