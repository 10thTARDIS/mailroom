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
)

// The user's own PGP keys (app.user_keys, spec §9, ADR-024, Forgejo #47).
//
// ⚠ THE ONLY LEGITIMATE CALLER OF THE WRITE PATHS HERE IS THE CRYPTO PROCESS
// (cmd/mailroom-crypto). PrivateKeyCT holds a private key sealed under that
// process's OWN root secret, and nothing else in the system can produce those
// bytes — the server does not have the root and cannot seal, unseal, or
// meaningfully forge them.
//
// The server may READ these rows, and that is deliberate rather than an
// oversight: what it gets is ciphertext it cannot open, and key publication
// (#48) needs the PUBLIC half and the fingerprint from ordinary request paths.
// The boundary this design rests on is the root secret, not the database role —
// both processes connect as mailroom_app and both are subject to the same RLS.
//
// The AEAD associated data on PrivateKeyCT binds the owning user AND the key's
// own id, so relocating the ciphertext to another row is not an attack that
// yields anything: the tag stops verifying. RLS is the first line; the AAD is
// the line that holds even against something with write access to the table.

// KeyKind mirrors app.key_kind.
type KeyKind string

const (
	// KeyPGP is an OpenPGP key. The only kind that exists: S/MIME is deferred to
	// 2.x (#69) on measured evidence — zero S/MIME messages and zero S/MIME
	// correspondents across 940 examined (#47 step 0).
	KeyPGP KeyKind = "pgp"
	// KeySMIME is reserved by the schema enum and is not produced by any code
	// path today.
	KeySMIME KeyKind = "smime"
)

// UserKey is one of the user's own keys.
type UserKey struct {
	ID     ulid.ULID
	UserID ulid.ULID
	// IdentityID is the address this key signs for, or nil if unbound.
	IdentityID  *ulid.ULID
	Kind        KeyKind
	Fingerprint string
	// PublicKey is the public key as BINARY OpenPGP packets — NOT ASCII armor.
	// Safe to hand to anyone; that is the point of it (#48).
	//
	// ⚠ THE REPRESENTATION IS LOAD-BEARING. These bytes are what the anonymous
	// WKD endpoint serves verbatim (LookupWKDKey → internal/keypub), and
	// draft-koch-openpgp-webkey-service requires raw packets with
	// Content-Type application/octet-stream. Armor here would make every
	// published key unservable, and — since unpublished addresses 404 while
	// published ones would then fail — turn that endpoint into an address
	// oracle. InsertUserKey refuses armor for exactly that reason; armoring is
	// done at the boundaries that want text (pgpkeyd.ArmorPublicKey).
	PublicKey []byte
	// PrivateKeyCT is the SEALED private key. Callers other than the crypto
	// process have no way to open it and no reason to read it.
	PrivateKeyCT []byte
	EnvelopeVer  int16
	// DecryptOnly marks a retired key kept forever so old mail stays readable
	// (spec §9). Retired keys are never chosen for signing.
	DecryptOnly  bool
	WKDPublished bool
	// Origin is where the private half came from: KeyGenerated, KeyImported, or
	// "" for NOT RECORDED (rows predating migration 00039).
	//
	// ⚠ "" IS NOT KeyImported, AND CONFLATING THEM IS THE WHOLE HAZARD. A
	// generated key exists nowhere else, so destroying it retroactively makes
	// every PGP-encrypted message the user already exported unreadable — an
	// export taken months earlier turns to garbage. An imported key costs the
	// user nothing to lose here. "Unknown" is the CAUTIOUS case, not the safe
	// one; a reader that defaults it to imported has silently decided a delete
	// is harmless on no evidence (#54, ADR-024).
	Origin    KeyOrigin
	CreatedAt time.Time
	RevokedAt *time.Time
	// RetiredAt is when the key stopped being used (#815): set by retire and by
	// revoke, whichever came first. nil is NOT RECORDED for a key retired or
	// revoked before migration 00071 — never read it as "never retired";
	// DecryptOnly and RevokedAt say whether.
	RetiredAt *time.Time
	// ReplacedBy is the key UserKey/replace put in this one's place (#817).
	ReplacedBy *ulid.ULID
}

// KeyOrigin records how a private key entered Mailroom's custody.
type KeyOrigin string

const (
	// KeyOriginUnrecorded is the zero value: the row predates migration 00039
	// and nothing anywhere can now say which of the two it was.
	KeyOriginUnrecorded KeyOrigin = ""
	// KeyGenerated means the crypto process minted it and Mailroom holds the
	// ONLY copy of the private half.
	KeyGenerated KeyOrigin = "generated"
	// KeyImported means the user supplied it and therefore still holds it.
	KeyImported KeyOrigin = "imported"
)

// ErrKeyInvalid is returned for a key row this table will not hold.
var ErrKeyInvalid = errors.New("store: not a valid user key")

// ErrFingerprintTaken is returned when the user already has a key with this
// fingerprint. Its own error because it is the one failure an importing user can
// act on — the key is already here, so importing it again is a no-op rather than
// a fault, and the caller needs to be able to say so.
var ErrFingerprintTaken = errors.New("store: that key is already stored")

// maxPublicKey bounds one serialized public key. Generous for any real key
// (an Ed25519 public key is well under 1 KiB; a long-lived RSA key with many
// signatures and user IDs can reach tens of KiB), and small enough that this
// column cannot become somewhere to park a file.
const maxPublicKey = 256 * 1024

// armorPrefix is what an ASCII-armored OpenPGP block starts with. It is
// REJECTED on write rather than tolerated: this column is served verbatim to
// anonymous WKD callers, so armor here is a bug whose only symptom is at the
// far end — every real client silently declines a key that curl fetches
// happily. Refusing at the write is the only place the mistake is cheap.
const armorPrefix = "-----BEGIN PGP"

// maxPrivateCT bounds the sealed private key for the same reason.
const maxPrivateCT = 256 * 1024

// InsertUserKey stores a new key and appends its Key change_log row.
//
// It does NOT bind the key to an identity even when IdentityID is set on the
// row — binding sets app.identities.default_key_id, which is a mutation of a
// JMAP-visible object with its own change_log row, so it is BindKeyToIdentity's
// job and is visible as such in the log.
func (tx *UserTx) InsertUserKey(ctx context.Context, k UserKey) (ulid.ULID, error) {
	if k.Kind != KeyPGP && k.Kind != KeySMIME {
		return ulid.ULID{}, fmt.Errorf("%w: kind %q", ErrKeyInvalid, k.Kind)
	}
	fpr := strings.ToUpper(strings.TrimSpace(k.Fingerprint))
	if fpr == "" {
		return ulid.ULID{}, fmt.Errorf("%w: a fingerprint is required", ErrKeyInvalid)
	}
	if len(k.PublicKey) == 0 {
		return ulid.ULID{}, fmt.Errorf("%w: a public key is required", ErrKeyInvalid)
	}
	if len(k.PublicKey) > maxPublicKey {
		return ulid.ULID{}, fmt.Errorf("%w: public key is longer than %d bytes", ErrKeyInvalid, maxPublicKey)
	}
	if bytes.HasPrefix(bytes.TrimLeft(k.PublicKey, " \t\r\n"), []byte(armorPrefix)) {
		return ulid.ULID{}, fmt.Errorf(
			"%w: the public key is ASCII-armored; this column is served verbatim over WKD, "+
				"which requires binary OpenPGP packets", ErrKeyInvalid)
	}
	if len(k.PrivateKeyCT) > maxPrivateCT {
		return ulid.ULID{}, fmt.Errorf("%w: sealed private key is longer than %d bytes", ErrKeyInvalid, maxPrivateCT)
	}
	// A row whose id the caller did not choose cannot be sealed against, because
	// the AAD binds that id. The crypto process mints it before sealing; an
	// unset id here means the caller sealed against something else.
	if k.ID.Compare(ulid.ULID{}) == 0 {
		return ulid.ULID{}, fmt.Errorf("%w: the key id must be chosen before sealing", ErrKeyInvalid)
	}
	if k.EnvelopeVer == 0 {
		k.EnvelopeVer = 1
	}

	var identity *string
	if k.IdentityID != nil {
		s := k.IdentityID.String()
		identity = &s
	}

	// dek_id is deliberately not written: it is NULL for pgp rows (ADR-024,
	// migration 00030). The wrapping key is derived from the crypto process's
	// own root and never stored, so there is no DEK to reference.
	sp, err := tx.tx.Begin(ctx) // SAVEPOINT: a unique violation must not poison the tx
	if err != nil {
		return ulid.ULID{}, fmt.Errorf("store: insert user key (savepoint): %w", err)
	}
	// A caller that does not say where the key came from writes NULL, not a
	// guess. The column's CHECK permits NULL and rejects anything that is
	// neither 'generated' nor 'imported', so a typo fails loudly here rather
	// than becoming a third silent category.
	var origin *string
	if k.Origin != KeyOriginUnrecorded {
		s := string(k.Origin)
		origin = &s
	}

	_, err = sp.Exec(ctx, `
		INSERT INTO app.user_keys
		  (id, user_id, identity_id, kind, fingerprint, public_key,
		   private_key_ct, envelope_ver, decrypt_only, wkd_published, origin)
		VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10, $11)`,
		k.ID.String(), tx.userID.String(), identity, string(k.Kind), fpr,
		k.PublicKey, k.PrivateKeyCT, k.EnvelopeVer,
		k.DecryptOnly, k.WKDPublished, origin)
	if err != nil {
		_ = sp.Rollback(ctx)
		if isUniqueViolation(err) {
			return ulid.ULID{}, ErrFingerprintTaken
		}
		return ulid.ULID{}, fmt.Errorf("store: insert user key: %w", err)
	}
	if err := sp.Commit(ctx); err != nil {
		return ulid.ULID{}, fmt.Errorf("store: insert user key (commit savepoint): %w", err)
	}

	if _, err := tx.AppendChange(ctx, ObjectKey, k.ID, OpCreated); err != nil {
		return ulid.ULID{}, err
	}
	return k.ID, nil
}

// GetUserKey returns one key by id.
//
// RLS confines this to the calling user, so a key belonging to someone else is
// simply not found. The crypto process ALSO compares the owner explicitly
// (pgpkeyd), and that redundancy is deliberate: RLS is a database-side control
// that a future refactor could route around (a SECURITY DEFINER helper, a
// different role), while the explicit comparison is the one a test can delete
// to prove it was load-bearing.
func (tx *UserTx) GetUserKey(ctx context.Context, id ulid.ULID) (UserKey, bool, error) {
	rows, err := tx.tx.Query(ctx, `
		SELECT id, user_id, identity_id, kind, fingerprint, public_key,
		       private_key_ct, envelope_ver, decrypt_only, wkd_published, origin,
		       created_at, revoked_at, retired_at, replaced_by
		  FROM app.user_keys
		 WHERE user_id = $1 AND id = $2`,
		tx.userID.String(), id.String())
	if err != nil {
		return UserKey{}, false, fmt.Errorf("store: get user key: %w", err)
	}
	keys, err := scanUserKeys(rows)
	if err != nil {
		return UserKey{}, false, err
	}
	if len(keys) == 0 {
		return UserKey{}, false, nil
	}
	return keys[0], true, nil
}

// ListUserKeys returns the user's keys, newest first.
func (tx *UserTx) ListUserKeys(ctx context.Context) ([]UserKey, error) {
	rows, err := tx.tx.Query(ctx, `
		SELECT id, user_id, identity_id, kind, fingerprint, public_key,
		       private_key_ct, envelope_ver, decrypt_only, wkd_published, origin,
		       created_at, revoked_at, retired_at, replaced_by
		  FROM app.user_keys
		 WHERE user_id = $1
		 ORDER BY created_at DESC, id DESC`,
		tx.userID.String())
	if err != nil {
		return nil, fmt.Errorf("store: list user keys: %w", err)
	}
	return scanUserKeys(rows)
}

// signingKeyUsable is THE predicate for "this key may still produce a
// signature", as SQL over an app.user_keys row aliased `k`.
//
// ⚠ ONE CONST, TWO READS, AND THAT IS THE POINT (#492). It is used by
// SigningKeyForIdentity — the send path's choice of key — and by the identity
// projection's LEFT JOIN (identities.go's identityFrom), which is what tells a
// client whether to print "Will sign". Two hand-written copies would be free to
// drift, and the symptom of drift is the worst kind: a client promising a
// signature on a revoked key, or withholding the badge on a key that does sign.
//
// Kind is deliberately NOT part of it, for the reason SigningKeyForIdentity
// gives below: only 'pgp' rows exist today, and filtering on kind here would be
// a no-op that quietly became a bug the day the second kind arrived.
const signingKeyUsable = `NOT k.decrypt_only
	   AND k.revoked_at IS NULL
	   AND k.private_key_ct IS NOT NULL`

// SigningKeyForIdentity returns the key this identity signs outbound mail with,
// if it has one that may still produce signatures.
//
// NOT FOUND IS NOT AN ERROR. An identity with no key is the ordinary case —
// most users will never have a PGP key at all — and it means "send this
// unsigned", not "something went wrong". Reporting it as an error would put the
// common path down the failure branch of every caller.
//
// The join runs through app.identities.default_key_id rather than
// app.user_keys.identity_id because the identity is the object that decides:
// BindKeyToIdentity writes both, and the identity's column is the one JMAP
// clients see. Retired keys are excluded here rather than by the caller so that
// "which key signs for this address" has ONE answer, and the answer is never a
// key the crypto process would then refuse (pgpkeyd.Service.Sign).
//
// A row with no private_key_ct is excluded for the same reason: those are
// public-only entries, and one bound as a default key would otherwise be
// selected here and fail at the far side of the crypto boundary, at send time.
//
// The caller must still switch on Kind. Only 'pgp' rows exist today (S/MIME is
// deferred to 2.x, #69), so filtering on kind here would be a no-op that quietly
// became a bug the day the second kind arrived.
func (tx *UserTx) SigningKeyForIdentity(ctx context.Context, identityID ulid.ULID) (UserKey, bool, error) {
	rows, err := tx.tx.Query(ctx, `
		SELECT k.id, k.user_id, k.identity_id, k.kind, k.fingerprint, k.public_key,
		       k.private_key_ct, k.envelope_ver, k.decrypt_only, k.wkd_published,
		       k.origin, k.created_at, k.revoked_at, k.retired_at, k.replaced_by
		  FROM app.identities i
		  JOIN app.user_keys k ON k.id = i.default_key_id
		 WHERE i.user_id = $1
		   AND i.id = $2
		   AND k.user_id = $1
		   AND `+signingKeyUsable,
		tx.userID.String(), identityID.String())
	if err != nil {
		return UserKey{}, false, fmt.Errorf("store: signing key for identity: %w", err)
	}
	keys, err := scanUserKeys(rows)
	if err != nil {
		return UserKey{}, false, err
	}
	if len(keys) == 0 {
		return UserKey{}, false, nil
	}
	return keys[0], true, nil
}

// BindKeyToIdentity makes a key the identity's default signing key.
//
// TWO change_log rows, in one savepoint, and both are required: the key row
// moved (Key) and app.identities.default_key_id moved (Identity). Each is
// written only when its row actually moved (#796), so re-binding a key that is
// already bound writes nothing. The second is
// the one easily missed — it is natural to think of this as "a key change" and
// not notice that a JMAP-visible Identity object now reports something different
// to every client syncing through /changes.
func (tx *UserTx) BindKeyToIdentity(ctx context.Context, keyID, identityID ulid.ULID) (bool, error) {
	// ⚠ BOTH ROWS ARE CHECKED BEFORE EITHER IS TOUCHED, and that ordering is
	// load-bearing rather than tidy.
	//
	// The obvious shape — update the key, then update the identity, and bail
	// out if the second matches nothing — HALF-APPLIES. Returning early without
	// an error lets UserTx commit, so app.user_keys.identity_id keeps pointing
	// at an identity that never agreed to it. The FK does not save us either:
	// it only requires the identity row to exist, not to belong to this user,
	// and FK checks are not subject to RLS. So a foreign identity id would have
	// been written, referencing another user's address, and committed.
	//
	// Found by reading this function back rather than by a test — the test that
	// should have caught it checked only that the OTHER user's identity was
	// untouched, and never looked at what happened to the key row. It does now.
	//
	// ⚠ AND A BINDING THAT IS ALREADY IN PLACE IS A NO-OP (#796): no write and
	// no change_log row. Both rows are read FOR UPDATE, so the "already bound"
	// answer cannot be overtaken by a concurrent rebind before this commits.
	var found bool
	err := tx.inSavepoint(ctx, func() error {
		var keyIdentity *string
		err := tx.tx.QueryRow(ctx, `
			SELECT identity_id FROM app.user_keys WHERE user_id = $1 AND id = $2 FOR UPDATE`,
			tx.userID.String(), keyID.String()).Scan(&keyIdentity)
		if isNoRows(err) {
			return nil
		}
		if err != nil {
			return fmt.Errorf("store: bind key to identity (key lookup): %w", err)
		}

		var identityDefault *string
		err = tx.tx.QueryRow(ctx, `
			SELECT default_key_id FROM app.identities WHERE user_id = $1 AND id = $2 FOR UPDATE`,
			tx.userID.String(), identityID.String()).Scan(&identityDefault)
		if isNoRows(err) {
			return nil
		}
		if err != nil {
			return fmt.Errorf("store: bind key to identity (identity lookup): %w", err)
		}
		found = true

		keyMoves := keyIdentity == nil || *keyIdentity != identityID.String()
		identityMoves := identityDefault == nil || *identityDefault != keyID.String()
		if keyMoves {
			if _, err := tx.tx.Exec(ctx, `
				UPDATE app.user_keys
				   SET identity_id = $3
				 WHERE user_id = $1 AND id = $2`,
				tx.userID.String(), keyID.String(), identityID.String()); err != nil {
				return fmt.Errorf("store: bind key to identity: %w", err)
			}
			if _, err := tx.AppendChange(ctx, ObjectKey, keyID, OpUpdated); err != nil {
				return err
			}
		}
		if identityMoves {
			if _, err := tx.tx.Exec(ctx, `
				UPDATE app.identities
				   SET default_key_id = $3
				 WHERE user_id = $1 AND id = $2`,
				tx.userID.String(), identityID.String(), keyID.String()); err != nil {
				return fmt.Errorf("store: bind key to identity (identity): %w", err)
			}
			if _, err := tx.AppendChange(ctx, ObjectIdentity, identityID, OpUpdated); err != nil {
				return err
			}
		}
		return nil
	})
	if err != nil {
		return false, err
	}
	return found, nil
}

// ErrKeyPublished is returned by UnbindUserKey for a key that is still
// published in WKD. WKD publishes a key AT its bound address; unbinding it
// silently would leave a directory entry for an address the key no longer
// belongs to. Unpublish it first (or retire it, which does both).
var ErrKeyPublished = errors.New("store: that key is published in WKD")

// UnbindResult reports what UnbindUserKey did.
type UnbindResult struct {
	// Found: the key exists for this user.
	Found bool
	// Changed: anything was written. An unbound key is a no-op.
	Changed bool
	// Unbound lists the identities whose default key this was, cleared.
	Unbound []ulid.ULID
}

// UnbindUserKey removes a key's binding (#796, UserKey/set identityId: null):
// every identity whose default key it is stops signing with it, and the key no
// longer names an address. One savepoint; change rows for the Key (when its
// identityId moved) and for each Identity whose signing key moved; none for a
// key that was not bound. A key still published in WKD is refused with
// ErrKeyPublished.
func (tx *UserTx) UnbindUserKey(ctx context.Context, keyID ulid.ULID) (UnbindResult, error) {
	var res UnbindResult
	err := tx.inSavepoint(ctx, func() error {
		var (
			identity  *string
			published bool
		)
		err := tx.tx.QueryRow(ctx, `
			SELECT identity_id, wkd_published FROM app.user_keys
			 WHERE user_id = $1 AND id = $2
			 FOR UPDATE`,
			tx.userID.String(), keyID.String()).Scan(&identity, &published)
		if isNoRows(err) {
			return nil
		}
		if err != nil {
			return fmt.Errorf("store: unbind key (read): %w", err)
		}
		res.Found = true
		defaults, err := tx.identitiesDefaultingTo(ctx, keyID)
		if err != nil {
			return err
		}
		if identity == nil && len(defaults) == 0 {
			return nil
		}
		if published {
			return ErrKeyPublished
		}
		if len(defaults) > 0 {
			if _, err := tx.tx.Exec(ctx, `
				UPDATE app.identities SET default_key_id = NULL
				 WHERE user_id = $1 AND default_key_id = $2`,
				tx.userID.String(), keyID.String()); err != nil {
				return fmt.Errorf("store: unbind key (identities): %w", err)
			}
			for _, id := range defaults {
				if _, err := tx.AppendChange(ctx, ObjectIdentity, id, OpUpdated); err != nil {
					return err
				}
			}
			res.Unbound = defaults
		}
		if identity != nil {
			if _, err := tx.tx.Exec(ctx, `
				UPDATE app.user_keys SET identity_id = NULL
				 WHERE user_id = $1 AND id = $2`,
				tx.userID.String(), keyID.String()); err != nil {
				return fmt.Errorf("store: unbind key: %w", err)
			}
			if _, err := tx.AppendChange(ctx, ObjectKey, keyID, OpUpdated); err != nil {
				return err
			}
		}
		res.Changed = true
		return nil
	})
	if err != nil {
		return UnbindResult{}, err
	}
	return res, nil
}

func scanUserKeys(rows interface {
	Next() bool
	Scan(...any) error
	Err() error
	Close()
},
) ([]UserKey, error) {
	defer rows.Close()

	var out []UserKey
	for rows.Next() {
		var (
			id, userID, kind, fpr string
			identity              *string
			pub, priv             []byte
			envVer                int16
			// Not named `wkd`: internal/wkd is imported by this package
			// (wkd.go), and a local of that name shadows it silently.
			decryptOnly, published bool
			created                time.Time
			revoked, retired       *time.Time
			replacedBy             *string
			// A POINTER, so SQL NULL survives the scan as "not recorded"
			// instead of collapsing into one of the two real values.
			origin *string
		)
		if err := rows.Scan(&id, &userID, &identity, &kind, &fpr, &pub, &priv,
			&envVer, &decryptOnly, &published, &origin, &created, &revoked, &retired, &replacedBy); err != nil {
			return nil, fmt.Errorf("store: scan user key: %w", err)
		}
		k := UserKey{
			Kind: KeyKind(kind), Fingerprint: fpr, PublicKey: pub,
			PrivateKeyCT: priv, EnvelopeVer: envVer, DecryptOnly: decryptOnly,
			WKDPublished: published, CreatedAt: created, RevokedAt: revoked,
			RetiredAt: retired,
		}
		if replacedBy != nil {
			r, err := ulid.Parse(*replacedBy)
			if err != nil {
				return nil, fmt.Errorf("store: user key replaced_by %q: %w", *replacedBy, err)
			}
			k.ReplacedBy = &r
		}
		if origin != nil {
			k.Origin = KeyOrigin(*origin)
		}
		parsed, err := ulid.Parse(id)
		if err != nil {
			return nil, fmt.Errorf("store: user key id %q: %w", id, err)
		}
		k.ID = parsed
		owner, err := ulid.Parse(userID)
		if err != nil {
			return nil, fmt.Errorf("store: user key owner %q: %w", userID, err)
		}
		k.UserID = owner
		if identity != nil {
			iid, err := ulid.Parse(*identity)
			if err != nil {
				return nil, fmt.Errorf("store: user key identity %q: %w", *identity, err)
			}
			k.IdentityID = &iid
		}
		out = append(out, k)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("store: iterate user keys: %w", err)
	}
	return out, nil
}
