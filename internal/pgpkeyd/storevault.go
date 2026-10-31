// SPDX-License-Identifier: AGPL-3.0-only

package pgpkeyd

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/oklog/ulid/v2"

	"mailroom/internal/store"
)

// StoreVault is the real vault: app.user_keys, under RLS (Forgejo #47
// increment 3). It replaces MemoryVault, which now exists only for tests.
//
// # Why this needs a user id to read a key by id
//
// Every database access in Mailroom goes through Store.UserTx, which sets
// mailroom.user_id transaction-locally so RLS can filter (CLAUDE.md: fail
// closed — no identity, no rows). That is a better default than the Vault
// interface's Get(keyID) shape, which was written for a map.
//
// So this type reads under the CALLER's identity and returns ErrNoSuchKey for
// anything RLS hides — and the service's explicit ownership comparison still
// runs on top. Two controls covering one property, deliberately:
//
//   - RLS is enforced by PostgreSQL and holds even if the Go code is wrong.
//     It is also the one a future refactor could route around without noticing
//     (a SECURITY DEFINER helper, a different role, a query someone moved).
//   - the explicit comparison is enforced by three lines a test can delete, and
//     therefore by the only one of the two whose removal a probe can demonstrate.
//
// Neither makes the other redundant. Dropping the comparison because "RLS covers
// it" would leave the property true but unprovable, which is how it comes to be
// removed for real later.
type StoreVault struct {
	store *store.Store
}

// NewStoreVault wraps a Store.
func NewStoreVault(s *store.Store) (*StoreVault, error) {
	if s == nil {
		return nil, errors.New("pgpkeyd: nil store")
	}
	return &StoreVault{store: s}, nil
}

// Insert implements Vault.
func (v *StoreVault) Insert(ctx context.Context, rec Record) error {
	userID, err := ulid.Parse(rec.UserID)
	if err != nil {
		return fmt.Errorf("pgpkeyd: user id %q: %w", rec.UserID, err)
	}
	keyID, err := ulid.Parse(rec.KeyID)
	if err != nil {
		return fmt.Errorf("pgpkeyd: key id %q: %w", rec.KeyID, err)
	}

	return v.store.UserTx(ctx, userID, func(tx *store.UserTx) error {
		_, err := tx.InsertUserKey(ctx, store.UserKey{
			ID:           keyID,
			UserID:       userID,
			Kind:         store.KeyPGP,
			Fingerprint:  rec.Fingerprint,
			PublicKey:    rec.PublicKey,
			PrivateKeyCT: rec.Sealed,
			EnvelopeVer:  1,
			Origin:       rec.Origin,
		})
		if errors.Is(err, store.ErrFingerprintTaken) {
			return ErrFingerprintTaken
		}
		return err
	})
}

// Get implements Vault.
//
// The Vault interface takes a key id alone, so the caller's identity arrives
// out of band via WithCaller. A Get with no caller set is a programming error
// and fails closed rather than reading as the wrong user — see callerFrom.
func (v *StoreVault) Get(ctx context.Context, keyID string) (Record, error) {
	caller, ok := callerFrom(ctx)
	if !ok {
		return Record{}, errors.New("pgpkeyd: vault read with no caller identity")
	}
	userID, err := ulid.Parse(caller)
	if err != nil {
		return Record{}, fmt.Errorf("pgpkeyd: caller id %q: %w", caller, err)
	}
	id, err := ulid.Parse(keyID)
	if err != nil {
		// A key id that is not a ULID cannot name a row. Report it as absent
		// rather than as a database error: it is the same answer, and it keeps
		// a malformed id from reaching the query at all.
		return Record{}, ErrNoSuchKey
	}

	var rec Record
	err = v.store.UserTx(ctx, userID, func(tx *store.UserTx) error {
		k, found, err := tx.GetUserKey(ctx, id)
		if err != nil {
			return err
		}
		if !found {
			return ErrNoSuchKey
		}
		rec = Record{
			KeyID:       k.ID.String(),
			UserID:      k.UserID.String(),
			Fingerprint: k.Fingerprint,
			PublicKey:   k.PublicKey,
			Sealed:      k.PrivateKeyCT,
			CreatedAt:   k.CreatedAt,
			DecryptOnly: k.DecryptOnly,
			RevokedAt:   k.RevokedAt,
		}
		return nil
	})
	if err != nil {
		return Record{}, err
	}
	return rec, nil
}

// List implements KeyLister: every key the CALLER holds, retired ones included,
// read under RLS. The service still filters by owner on top.
func (v *StoreVault) List(ctx context.Context) ([]Record, error) {
	caller, ok := callerFrom(ctx)
	if !ok {
		return nil, errors.New("pgpkeyd: vault list with no caller identity")
	}
	userID, err := ulid.Parse(caller)
	if err != nil {
		return nil, fmt.Errorf("pgpkeyd: caller id %q: %w", caller, err)
	}
	var out []Record
	err = v.store.UserTx(ctx, userID, func(tx *store.UserTx) error {
		keys, err := tx.ListUserKeys(ctx)
		if err != nil {
			return err
		}
		for _, k := range keys {
			// A row with no sealed private half is a public-only entry and has
			// nothing to decrypt with.
			if len(k.PrivateKeyCT) == 0 || k.Kind != store.KeyPGP {
				continue
			}
			out = append(out, Record{
				KeyID:       k.ID.String(),
				UserID:      k.UserID.String(),
				Fingerprint: k.Fingerprint,
				PublicKey:   k.PublicKey,
				Sealed:      k.PrivateKeyCT,
				CreatedAt:   k.CreatedAt,
				DecryptOnly: k.DecryptOnly,
				RevokedAt:   k.RevokedAt,
				Origin:      k.Origin,
			})
		}
		return nil
	})
	if err != nil {
		return nil, err
	}
	return out, nil
}

// RecordDecrypt implements DecryptAuditor: one app.pgp_decrypt_audit row, in the
// caller's own transaction context, carrying no content (migration 00067).
func (v *StoreVault) RecordDecrypt(ctx context.Context, a DecryptAudit) error {
	caller, ok := callerFrom(ctx)
	if !ok {
		return errors.New("pgpkeyd: decrypt audit with no caller identity")
	}
	if caller != a.UserID {
		// The row would be refused by RLS's WITH CHECK anyway; say why here.
		return errors.New("pgpkeyd: decrypt audit for a user other than the caller")
	}
	userID, err := ulid.Parse(caller)
	if err != nil {
		return fmt.Errorf("pgpkeyd: caller id %q: %w", caller, err)
	}
	msgID, err := ulid.Parse(a.MessageID)
	if err != nil {
		return fmt.Errorf("pgpkeyd: message id %q: %w", a.MessageID, err)
	}
	return v.store.UserTx(ctx, userID, func(tx *store.UserTx) error {
		return tx.RecordPGPDecrypt(ctx, store.PGPDecryptAudit{
			MessageID:   msgID,
			Purpose:     a.Purpose,
			Decrypted:   a.Decrypted,
			Fingerprint: a.Fingerprint,
		})
	})
}

// BindIdentity implements IdentityBinder.
//
// Both mutations and both change_log rows happen in ONE transaction, which is
// the load-bearing invariant (CLAUDE.md): app.user_keys.identity_id and
// app.identities.default_key_id move together, or neither moves. A binding that
// half-applied would leave an identity advertising a key the key does not agree
// it signs for.
func (v *StoreVault) BindIdentity(ctx context.Context, keyID, identityID string) (bool, error) {
	caller, ok := callerFrom(ctx)
	if !ok {
		return false, errors.New("pgpkeyd: identity binding with no caller identity")
	}
	userID, err := ulid.Parse(caller)
	if err != nil {
		return false, fmt.Errorf("pgpkeyd: caller id %q: %w", caller, err)
	}
	kid, err := ulid.Parse(keyID)
	if err != nil {
		return false, nil
	}
	iid, err := ulid.Parse(identityID)
	if err != nil {
		return false, nil
	}

	var bound bool
	err = v.store.UserTx(ctx, userID, func(tx *store.UserTx) error {
		bound, err = tx.BindKeyToIdentity(ctx, kid, iid)
		return err
	})
	if err != nil {
		return false, err
	}
	return bound, nil
}

// UpdateKey implements KeyUpdater: the re-signed or revoked key, its re-sealed
// private half and the change_log rows in one transaction under the caller's
// identity (store.ReplaceUserKeyMaterial / store.RevokeUserKey).
func (v *StoreVault) UpdateKey(ctx context.Context, rec Record, revokedAt *time.Time) (bool, error) {
	caller, ok := callerFrom(ctx)
	if !ok {
		return false, errors.New("pgpkeyd: key update with no caller identity")
	}
	if caller != rec.UserID {
		return false, errors.New("pgpkeyd: key update for a user other than the caller")
	}
	userID, err := ulid.Parse(caller)
	if err != nil {
		return false, fmt.Errorf("pgpkeyd: caller id %q: %w", caller, err)
	}
	keyID, err := ulid.Parse(rec.KeyID)
	if err != nil {
		return false, nil
	}
	m := store.KeyMaterial{Fingerprint: rec.Fingerprint, PublicKey: rec.PublicKey, PrivateKeyCT: rec.Sealed}
	var found bool
	err = v.store.UserTx(ctx, userID, func(tx *store.UserTx) error {
		var e error
		if revokedAt != nil {
			found, e = tx.RevokeUserKey(ctx, keyID, m, *revokedAt)
		} else {
			found, e = tx.ReplaceUserKeyMaterial(ctx, keyID, m)
		}
		return e
	})
	if err != nil {
		return false, err
	}
	return found, nil
}

// callerKey is the context key carrying the authenticated caller's user id.
type callerKey struct{}

// WithCaller returns a context carrying the calling user's id, so a Vault that
// enforces per-user isolation in the database can open its transaction as the
// right user.
//
// This is threaded through context rather than added to the Vault interface
// because it must be IMPOSSIBLE TO FORGET rather than merely easy to pass: a
// Get without it fails closed, whereas an extra parameter can be filled with
// whatever the caller happens to have in scope — which, for a confused deputy,
// is precisely the wrong user.
func WithCaller(ctx context.Context, userID string) context.Context {
	return context.WithValue(ctx, callerKey{}, userID)
}

func callerFrom(ctx context.Context) (string, bool) {
	v, ok := ctx.Value(callerKey{}).(string)
	if !ok || v == "" {
		return "", false
	}
	return v, true
}

// ErrFingerprintTaken means the user already holds this key. Importing a key
// twice is a no-op the caller should be able to report as such, not a fault.
var ErrFingerprintTaken = errors.New("pgpkeyd: that key is already stored")
