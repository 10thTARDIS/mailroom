// SPDX-License-Identifier: AGPL-3.0-only

package store

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/oklog/ulid/v2"

	"mailroom/internal/pgpkeys"
)

// The user's own keys after creation: retire, extend, revoke (#795, ADR-040).
//
// Until v1.93 the decrypt_only and revoked_at columns had every READER they
// needed (signing refuses them, WKD and Autocrypt skip them, decrypt keeps
// using them) and no WRITER at all. These are the writers.
//
//   - RetireUserKey is store-only: it flips flags and touches no key material,
//     so it does not need the crypto process.
//   - ReplaceUserKeyMaterial and RevokeUserKey store NEW key material (a
//     re-signed or revoked key) that only the crypto process can produce,
//     because it needs the private half. Like InsertUserKey, their only
//     legitimate caller is that process (pgpkeyd.StoreVault).

// ErrKeyIsDefault is returned by RetireUserKey when the key is still an
// identity's bound default and the caller did not ask to unbind it. Retiring it
// silently would leave the identity pointing at a key that no longer signs, and
// its mail going out unsigned with nothing said.
var ErrKeyIsDefault = errors.New("store: that key is an identity's bound default key")

// RetireResult reports what RetireUserKey did, so the CLI can say it.
type RetireResult struct {
	// Found: the key exists for this user.
	Found bool
	// Changed: anything was written. A key already retired, unpublished and
	// unbound is a no-op, and writes no change_log row.
	Changed bool
	// Unbound lists the identities whose default key this was, cleared.
	Unbound []ulid.ULID
	// Unpublished: the key was in WKD and is not any more.
	Unpublished bool
}

// RetireUserKey makes a key decrypt-only (spec §9): it never signs or is
// published again, and it keeps decrypting forever (ADR-037). It records
// retired_at (#815) when the key goes from live to retired, and never moves it;
// a key that was already retired keeps what it had (NULL, "not recorded", for
// one retired before migration 00071), so no date is ever invented. One
// transaction,
// with change rows for every JMAP-visible object that moved:
//
//   - the Key row (decryptOnly, wkdPublished);
//   - each Identity whose default key this was, when unbind clears it (its
//     signing key and "Will sign" change);
//   - the Identity whose WKD hash is cleared, when this was the last published
//     key at that address.
//
// ⚠ THE IDENTITY'S WKD HASH IS CLEARED ONLY IF NO OTHER KEY IS STILL PUBLISHED
// THERE. The ordinary rotation is generate → bind → publish the NEW key, then
// retire the OLD one; clearing the hash unconditionally (as UnpublishKey used to)
// would take the new key out of WKD with it.
//
// A key that is an identity's bound default is refused with ErrKeyIsDefault
// unless unbind is set, in which case the binding is cleared in the same
// transaction.
func (tx *UserTx) RetireUserKey(ctx context.Context, keyID ulid.ULID, unbind bool) (RetireResult, error) {
	var res RetireResult
	err := tx.inSavepoint(ctx, func() error {
		var (
			identityID  *string
			decryptOnly bool
			published   bool
		)
		err := tx.tx.QueryRow(ctx, `
			SELECT identity_id, decrypt_only, wkd_published
			  FROM app.user_keys
			 WHERE user_id = $1 AND id = $2
			 FOR UPDATE`,
			tx.userID.String(), keyID.String()).Scan(&identityID, &decryptOnly, &published)
		if isNoRows(err) {
			return nil
		}
		if err != nil {
			return fmt.Errorf("store: retire key (read): %w", err)
		}
		res.Found = true

		defaults, err := tx.identitiesDefaultingTo(ctx, keyID)
		if err != nil {
			return err
		}
		if len(defaults) > 0 && !unbind {
			return fmt.Errorf("%w (identity %s); pass --unbind to clear the binding in the same step, "+
				"or bind a different key first", ErrKeyIsDefault, defaults[0])
		}

		changedIdentities := map[ulid.ULID]bool{}
		if len(defaults) > 0 {
			if _, err := tx.tx.Exec(ctx, `
				UPDATE app.identities SET default_key_id = NULL
				 WHERE user_id = $1 AND default_key_id = $2`,
				tx.userID.String(), keyID.String()); err != nil {
				return fmt.Errorf("store: retire key (unbind): %w", err)
			}
			for _, id := range defaults {
				changedIdentities[id] = true
			}
			res.Unbound = defaults
		}

		if !decryptOnly || published {
			if _, err := tx.tx.Exec(ctx, `
				UPDATE app.user_keys
				   SET decrypt_only = true, wkd_published = false,
				       retired_at = CASE WHEN decrypt_only THEN retired_at ELSE COALESCE(retired_at, now()) END
				 WHERE user_id = $1 AND id = $2`,
				tx.userID.String(), keyID.String()); err != nil {
				return fmt.Errorf("store: retire key: %w", err)
			}
			res.Unpublished = published
		}
		if published && identityID != nil {
			cleared, err := tx.clearWKDHashIfLastPublished(ctx, *identityID, keyID)
			if err != nil {
				return err
			}
			if cleared {
				iid, err := ulid.Parse(*identityID)
				if err != nil {
					return fmt.Errorf("store: retire key: identity id %q: %w", *identityID, err)
				}
				changedIdentities[iid] = true
			}
		}

		res.Changed = !decryptOnly || published || len(defaults) > 0
		if !res.Changed {
			return nil
		}
		if _, err := tx.AppendChange(ctx, ObjectKey, keyID, OpUpdated); err != nil {
			return err
		}
		for id := range changedIdentities {
			if _, err := tx.AppendChange(ctx, ObjectIdentity, id, OpUpdated); err != nil {
				return err
			}
		}
		return nil
	})
	if err != nil {
		return RetireResult{}, err
	}
	return res, nil
}

// identitiesDefaultingTo lists the identities whose default_key_id is keyID.
func (tx *UserTx) identitiesDefaultingTo(ctx context.Context, keyID ulid.ULID) ([]ulid.ULID, error) {
	rows, err := tx.tx.Query(ctx, `
		SELECT id FROM app.identities
		 WHERE user_id = $1 AND default_key_id = $2
		 ORDER BY id`,
		tx.userID.String(), keyID.String())
	if err != nil {
		return nil, fmt.Errorf("store: identities defaulting to key: %w", err)
	}
	defer rows.Close()
	var out []ulid.ULID
	for rows.Next() {
		var s string
		if err := rows.Scan(&s); err != nil {
			return nil, fmt.Errorf("store: scan identity id: %w", err)
		}
		id, err := ulid.Parse(s)
		if err != nil {
			return nil, fmt.Errorf("store: identity id %q: %w", s, err)
		}
		out = append(out, id)
	}
	return out, rows.Err()
}

// clearWKDHashIfLastPublished clears the identity's WKD hash when no key other
// than except is still published at it, and reports whether it did. A revoked
// key that is still marked published counts as published: its revocation is
// being served there (ADR-040), and clearing the hash would stop that.
func (tx *UserTx) clearWKDHashIfLastPublished(ctx context.Context, identityID string, except ulid.ULID) (bool, error) {
	var others bool
	if err := tx.tx.QueryRow(ctx, `
		SELECT EXISTS (
		  SELECT 1 FROM app.user_keys
		   WHERE user_id = $1 AND identity_id = $2 AND id <> $3 AND wkd_published)`,
		tx.userID.String(), identityID, except.String()).Scan(&others); err != nil {
		return false, fmt.Errorf("store: other published keys: %w", err)
	}
	if others {
		return false, nil
	}
	tag, err := tx.tx.Exec(ctx, `
		UPDATE app.identities SET wkd_hash = NULL
		 WHERE user_id = $1 AND id = $2 AND wkd_hash IS NOT NULL`,
		tx.userID.String(), identityID)
	if err != nil {
		return false, fmt.Errorf("store: clear identity wkd hash: %w", err)
	}
	return tag.RowsAffected() > 0, nil
}

// KeyMaterial is a re-signed or revoked key, as the crypto process produced it.
type KeyMaterial struct {
	// Fingerprint must equal the stored one. Re-signing never changes it, and a
	// write that would is refused: a different fingerprint is a different key,
	// and storing it under this row's id would make every correspondent see
	// "key changed" for what was meant to be a renewal.
	Fingerprint string
	// PublicKey is binary OpenPGP packets (never armor; see UserKey.PublicKey).
	PublicKey []byte
	// PrivateKeyCT is the private half re-sealed under the same AAD (user, key
	// id). The self-signatures live in the private serialization too, and a
	// sealed copy left with the OLD ones would expire under the signer even
	// after the public key was renewed.
	PrivateKeyCT []byte
}

func (m KeyMaterial) validate() (string, error) {
	fpr := strings.ToUpper(strings.TrimSpace(m.Fingerprint))
	switch {
	case fpr == "":
		return "", fmt.Errorf("%w: a fingerprint is required", ErrKeyInvalid)
	case len(m.PublicKey) == 0:
		return "", fmt.Errorf("%w: a public key is required", ErrKeyInvalid)
	case len(m.PublicKey) > maxPublicKey:
		return "", fmt.Errorf("%w: public key is longer than %d bytes", ErrKeyInvalid, maxPublicKey)
	case bytes.HasPrefix(bytes.TrimLeft(m.PublicKey, " \t\r\n"), []byte(armorPrefix)):
		return "", fmt.Errorf("%w: the public key is ASCII-armored; this column is served verbatim over WKD", ErrKeyInvalid)
	case len(m.PrivateKeyCT) == 0:
		return "", fmt.Errorf("%w: the re-sealed private key is required", ErrKeyInvalid)
	case len(m.PrivateKeyCT) > maxPrivateCT:
		return "", fmt.Errorf("%w: sealed private key is longer than %d bytes", ErrKeyInvalid, maxPrivateCT)
	}
	// The fingerprint is checked against the BYTES, not only against the row:
	// a caller that named the right fingerprint but handed over a different key
	// would otherwise store another key under this id, and WKD would serve it.
	k, err := pgpkeys.Parse(m.PublicKey)
	if err != nil {
		return "", fmt.Errorf("%w: the public key does not parse: %v", ErrKeyInvalid, err)
	}
	if k.Fingerprint != fpr {
		return "", fmt.Errorf("%w: the public key's fingerprint is %s, not %s", ErrKeyInvalid, k.Fingerprint, fpr)
	}
	return fpr, nil
}

// ReplaceUserKeyMaterial stores a re-signed copy of a key (pgp-extend): same
// fingerprint, new self-signatures. It refuses a revoked key — extending one is
// meaningless and the crypto process refuses it first — and a fingerprint that
// does not match the row. found is false when no such live key exists.
//
// One Key change_log row. No Identity row: nothing an Identity projects
// (address, signing key id, "Will sign") changes when a key's expiry moves.
func (tx *UserTx) ReplaceUserKeyMaterial(ctx context.Context, keyID ulid.ULID, m KeyMaterial) (bool, error) {
	fpr, err := m.validate()
	if err != nil {
		return false, err
	}
	var found bool
	err = tx.inSavepoint(ctx, func() error {
		tag, err := tx.tx.Exec(ctx, `
			UPDATE app.user_keys SET public_key = $4, private_key_ct = $5
			 WHERE user_id = $1 AND id = $2 AND fingerprint = $3 AND revoked_at IS NULL`,
			tx.userID.String(), keyID.String(), fpr, m.PublicKey, m.PrivateKeyCT)
		if err != nil {
			return fmt.Errorf("store: replace key material: %w", err)
		}
		if tag.RowsAffected() == 0 {
			return nil
		}
		found = true
		_, err = tx.AppendChange(ctx, ObjectKey, keyID, OpUpdated)
		return err
	})
	return found, err
}

// RevokeUserKey stores a revoked copy of a key (pgp-revoke) and marks it
// revoked and decrypt-only. found is false when no such unrevoked key exists.
//
// ⚠ wkd_published IS LEFT AS IT WAS (ADR-040). A key that was published stays
// published AS A REVOCATION: app.wkd_public_key serves revoked keys after the
// address's current one, so a correspondent's client that refreshes the address
// learns the key is revoked instead of silently keeping the copy it cached. A
// key that was never published is not newly exposed. `pgp-unpublish` withdraws
// the revocation too, if the operator wants that.
//
// Change rows: the Key, and every Identity whose bound default this was — the
// identity's signing key (and "Will sign") disappears, because signingKeyUsable
// excludes revoked keys. The binding itself is left in place: it is inert, and
// keeping it records which address the revoked key served.
func (tx *UserTx) RevokeUserKey(ctx context.Context, keyID ulid.ULID, m KeyMaterial, at time.Time) (bool, error) {
	fpr, err := m.validate()
	if err != nil {
		return false, err
	}
	var found bool
	err = tx.inSavepoint(ctx, func() error {
		tag, err := tx.tx.Exec(ctx, `
			UPDATE app.user_keys
			   SET public_key = $4, private_key_ct = $5, revoked_at = $6, decrypt_only = true,
			       retired_at = CASE WHEN decrypt_only THEN retired_at ELSE COALESCE(retired_at, $6) END
			 WHERE user_id = $1 AND id = $2 AND fingerprint = $3 AND revoked_at IS NULL`,
			tx.userID.String(), keyID.String(), fpr, m.PublicKey, m.PrivateKeyCT, at.UTC())
		if err != nil {
			return fmt.Errorf("store: revoke key: %w", err)
		}
		if tag.RowsAffected() == 0 {
			return nil
		}
		found = true
		if _, err := tx.AppendChange(ctx, ObjectKey, keyID, OpUpdated); err != nil {
			return err
		}
		defaults, err := tx.identitiesDefaultingTo(ctx, keyID)
		if err != nil {
			return err
		}
		for _, id := range defaults {
			if _, err := tx.AppendChange(ctx, ObjectIdentity, id, OpUpdated); err != nil {
				return err
			}
		}
		return nil
	})
	return found, err
}
