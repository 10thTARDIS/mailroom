// SPDX-License-Identifier: AGPL-3.0-only

package store

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/oklog/ulid/v2"

	"mailroom/internal/pgpkeys"
	"mailroom/internal/wkd"
)

// Web Key Directory publication (Forgejo #48, spec §9, migration 00031).
//
// Two operations that look similar and are not:
//
//   - PublishKey runs as the OWNING USER, under RLS, like everything else. It
//     is a deliberate act by someone who is signed in.
//   - LookupWKDKey runs for NOBODY. A correspondent's mail client has no
//     session, so it goes through the narrow SECURITY DEFINER of migration
//     00031 — the only sanctioned way to cross users in this schema.
//
// The definer is itself subject to RLS (FORCE ROW LEVEL SECURITY applies to the
// owner too), so it works only because of two narrow policies, and its GRANT is
// column-level and excludes private_key_ct. That last part is the load-bearing
// one: the anonymous path cannot read sealed key material even if this Go code
// asked it to.

// ErrNotPublishable is returned when a key cannot be published, with a reason
// the operator can act on.
var ErrNotPublishable = errors.New("store: key cannot be published")

// PublishKey marks a key as published in WKD and records the identity's WKD
// hash, which is what makes the anonymous lookup able to see it at all.
//
// hash comes from internal/wkd, which is tested against the published draft
// test vector, and is PASSED IN rather than computed here so the caller
// publishes the same string it shows the operator — a CLI that printed one path
// and stored another would be unverifiable by the person running it.
//
// ⚠ IT IS THEN CHECKED AGAINST THE ADDRESS. An unchecked hash makes the wrong
// failure silent: the row commits, `wkd_published` reads true, and the key sits
// at a directory name no correspondent's client will ever request. There is no
// error anywhere and nothing to notice — the only symptom is at the far end,
// months later, in someone else's mail client. The check costs one SHA-1 over a
// local part we have already read for the fixed-vs-wildcard test.
//
// One savepoint. Each row is written, with its change_log row, only when it
// moves: publishing an already-published key is a no-op (#796).
//
// Refuses a key that is retired or revoked: those are kept forever so old mail
// stays readable (spec §9), and publishing one would tell the world to encrypt
// new mail to a key the user has stopped using.
func (tx *UserTx) PublishKey(ctx context.Context, keyID ulid.ULID, hash string) (bool, error) {
	if hash == "" {
		return false, fmt.Errorf("%w: no WKD hash supplied", ErrNotPublishable)
	}
	var found bool
	// ONE savepoint per logical write (#796): a caller that recovers from an
	// error here must not commit the identity hash without the key flag.
	err := tx.inSavepoint(ctx, func() error {
		var err error
		found, err = tx.publishKey(ctx, keyID, hash)
		return err
	})
	return found, err
}

func (tx *UserTx) publishKey(ctx context.Context, keyID ulid.ULID, hash string) (bool, error) {
	var (
		identityID  *string
		decryptOnly bool
		revoked     *string
		kind        string
		published   bool
	)
	err := tx.tx.QueryRow(ctx, `
		SELECT identity_id, decrypt_only, revoked_at::text, kind, wkd_published
		  FROM app.user_keys
		 WHERE user_id = $1 AND id = $2
		 FOR UPDATE`,
		tx.userID.String(), keyID.String()).Scan(&identityID, &decryptOnly, &revoked, &kind, &published)
	if err != nil {
		if isNoRows(err) {
			return false, nil
		}
		return false, fmt.Errorf("store: publish key (read): %w", err)
	}

	// Each of these is a state in which publishing would produce a directory
	// entry that actively misleads a correspondent, so each is refused with the
	// reason rather than silently doing nothing.
	switch {
	case kind != string(KeyPGP):
		return false, fmt.Errorf("%w: only PGP keys are published over WKD", ErrNotPublishable)
	case identityID == nil:
		return false, fmt.Errorf("%w: bind it to an identity first — WKD publishes a key AT an address", ErrNotPublishable)
	case decryptOnly:
		return false, fmt.Errorf("%w: the key is retired (decrypt-only); publishing it would invite mail it should not receive", ErrNotPublishable)
	case revoked != nil:
		return false, fmt.Errorf("%w: the key is revoked", ErrNotPublishable)
	}

	// The identity must be a real address. A domain wildcard covers infinitely
	// many derived aliases, and WKD names keys per hashed local part — so a
	// wildcard is not publishable in bulk and never will be (spec §9's catchall
	// corollary). Refusing it here is the whole enforcement of that rule.
	var identityKind string
	var address, currentHash *string
	if err := tx.tx.QueryRow(ctx, `
		SELECT kind, address::text, wkd_hash FROM app.identities
		 WHERE user_id = $1 AND id = $2
		 FOR UPDATE`,
		tx.userID.String(), *identityID).Scan(&identityKind, &address, &currentHash); err != nil {
		if isNoRows(err) {
			return false, nil
		}
		return false, fmt.Errorf("store: publish key (identity): %w", err)
	}
	if identityKind != "fixed" || address == nil {
		return false, fmt.Errorf(
			"%w: only a real designated address can be published — a domain wildcard stands for "+
				"unboundedly many derived aliases, and WKD names a key per address", ErrNotPublishable)
	}

	localPart, _, err := wkd.SplitAddress(*address)
	if err != nil {
		return false, fmt.Errorf("%w: the bound identity's address %q does not parse", ErrNotPublishable, *address)
	}
	if want := wkd.HashLocalPart(localPart); hash != want {
		// Naming both is safe — the address is the thing being published, and
		// the hash is a public function of it — and it is the only way the
		// operator can see WHICH of the two is wrong.
		return false, fmt.Errorf(
			"%w: the supplied WKD hash %q is not the hash of %s (that is %q); publishing it would "+
				"put the key at a path no client will request", ErrNotPublishable, hash, *address, want)
	}

	// Each row is written, and its change_log row appended, only when it
	// actually moves (#796): publishing a key that is already published at this
	// hash is a no-op with no rows, and publishing a second key at an address
	// whose hash is already set moves only the Key.
	hashMoves := currentHash == nil || *currentHash != hash
	if hashMoves {
		if _, err := tx.tx.Exec(ctx, `
			UPDATE app.identities SET wkd_hash = $3
			 WHERE user_id = $1 AND id = $2`,
			tx.userID.String(), *identityID, hash); err != nil {
			return false, fmt.Errorf("store: publish key (identity hash): %w", err)
		}
	}
	if !published {
		if _, err := tx.tx.Exec(ctx, `
			UPDATE app.user_keys SET wkd_published = true
			 WHERE user_id = $1 AND id = $2`,
			tx.userID.String(), keyID.String()); err != nil {
			return false, fmt.Errorf("store: publish key: %w", err)
		}
		if _, err := tx.AppendChange(ctx, ObjectKey, keyID, OpUpdated); err != nil {
			return false, err
		}
	}
	if hashMoves {
		identityULID, err := ulid.Parse(*identityID)
		if err != nil {
			return false, fmt.Errorf("store: publish key: identity id %q: %w", *identityID, err)
		}
		if _, err := tx.AppendChange(ctx, ObjectIdentity, identityULID, OpUpdated); err != nil {
			return false, err
		}
	}
	return true, nil
}

// UnpublishKey withdraws a key from WKD. One savepoint; a key that is not
// published is a no-op that writes no change_log row (#796).
//
// It clears the identity's hash as well as the key's flag when this was the
// LAST key published at that address, so the address stops being visible to the
// anonymous policy entirely rather than merely having no key behind it.
// Withdrawal should remove the entry, not leave a hole that confirms the address
// exists.
//
// ⚠ ONLY THE LAST ONE (#795). Until v1.93 the hash was cleared unconditionally,
// so withdrawing an OLD key after publishing its replacement silently took the
// replacement out of WKD as well — the address had no hash, so nothing at it
// was found. Rotation (publish new, then retire or unpublish old) is the
// ordinary case, so the hash now goes only with the last published key.
//
// ⚠ This does not un-publish anything a correspondent already fetched and
// cached, and it cannot. Withdrawal stops new lookups; it does not undo old
// ones. Revocation is the mechanism for "stop trusting this key", and it is a
// different act.
func (tx *UserTx) UnpublishKey(ctx context.Context, keyID ulid.ULID) (bool, error) {
	var found bool
	err := tx.inSavepoint(ctx, func() error {
		var err error
		found, err = tx.unpublishKey(ctx, keyID)
		return err
	})
	return found, err
}

func (tx *UserTx) unpublishKey(ctx context.Context, keyID ulid.ULID) (bool, error) {
	var (
		identityID *string
		published  bool
	)
	err := tx.tx.QueryRow(ctx, `
		SELECT identity_id, wkd_published FROM app.user_keys
		 WHERE user_id = $1 AND id = $2
		 FOR UPDATE`,
		tx.userID.String(), keyID.String()).Scan(&identityID, &published)
	if err != nil {
		if isNoRows(err) {
			return false, nil
		}
		return false, fmt.Errorf("store: unpublish key (read): %w", err)
	}
	// A key that is not published is a no-op (#796): no write, no change_log
	// row. Found is still true — the key exists, there was nothing to withdraw.
	if !published {
		return true, nil
	}

	if _, err := tx.tx.Exec(ctx, `
		UPDATE app.user_keys SET wkd_published = false
		 WHERE user_id = $1 AND id = $2`,
		tx.userID.String(), keyID.String()); err != nil {
		return false, fmt.Errorf("store: unpublish key: %w", err)
	}
	hashCleared := false
	if identityID != nil {
		hashCleared, err = tx.clearWKDHashIfLastPublished(ctx, *identityID, keyID)
		if err != nil {
			return false, fmt.Errorf("store: unpublish key (identity hash): %w", err)
		}
	}

	if _, err := tx.AppendChange(ctx, ObjectKey, keyID, OpUpdated); err != nil {
		return false, err
	}
	if identityID != nil && hashCleared {
		identityULID, err := ulid.Parse(*identityID)
		if err != nil {
			return false, fmt.Errorf("store: unpublish key: identity id %q: %w", *identityID, err)
		}
		if _, err := tx.AppendChange(ctx, ObjectIdentity, identityULID, OpUpdated); err != nil {
			return false, err
		}
	}
	return true, nil
}

// PublishedKey is what an anonymous WKD lookup yields: public material only.
type PublishedKey struct {
	// Address is the published address this key belongs to. Public by
	// definition: publishing an address is what put it here.
	Address string
	// PublicKey is the OpenPGP public key, in BINARY, exactly as stored — the
	// representation the draft requires and the one UserKey.PublicKey holds
	// (InsertUserKey refuses armor so this cannot be anything else). It is
	// served byte for byte by internal/keypub; nothing between here and the
	// wire re-encodes it.
	PublicKey   []byte
	Fingerprint string
	// RevocationsOnly: no live key is served at this address, only revoked
	// ones (ADR-040). The WKD endpoint still serves them — that is how a
	// correspondent learns of the revocation — but the human-facing key page
	// has no key to offer.
	RevocationsOnly bool
}

// LookupWKDKey answers a correspondent's anonymous key lookup.
//
// It runs in a SystemTx — no user identity, because the caller has none — and
// reaches the rows through app.wkd_public_key, the narrow SECURITY DEFINER of
// migration 00031. Everything about the exposure is in that migration's
// policies and its column-level grant; this function cannot widen it.
//
// WHAT IS SERVED (#795, ADR-040): the newest LIVE published key that can still
// be encrypted to, followed by every REVOKED published key at the address, as
// concatenated binary key blocks — draft-koch-openpgp-webkey-service's "a
// server may return revoked keys in addition to a new key". An EXPIRED key is
// never served as the live key: nobody can encrypt to it, and advertising it
// would only say "here is a key" for an address that has none usable. When the
// only keys left are revoked ones they are still served (RevocationsOnly), so a
// correspondent who refreshes learns the key they cached is revoked.
//
// Expiry is decided HERE, from the key's packets, because it is not a column the
// definer could filter on. A key whose packets cannot be read is not served.
//
// Not found is (PublishedKey{}, false, nil), never an error: an address with no
// published key is the ordinary case, and a WKD server answers it with a plain
// 404.
func (s *Store) LookupWKDKey(ctx context.Context, domain, hash string) (PublishedKey, bool, error) {
	if domain == "" || hash == "" {
		return PublishedKey{}, false, nil
	}

	var rowsOut []wkdRow
	err := s.SystemTx(ctx, func(tx *SystemTx) error {
		rows, err := tx.tx.Query(ctx,
			`SELECT address, public_key, fingerprint FROM app.wkd_public_key($1, $2)`, domain, hash)
		if err != nil {
			return fmt.Errorf("store: wkd lookup: %w", err)
		}
		defer rows.Close()
		for rows.Next() {
			var r wkdRow
			if err := rows.Scan(&r.address, &r.pub, &r.fpr); err != nil {
				return fmt.Errorf("store: scan wkd key: %w", err)
			}
			rowsOut = append(rowsOut, r)
		}
		return rows.Err()
	})
	if err != nil {
		return PublishedKey{}, false, err
	}
	out, found := selectWKDKeys(rowsOut, time.Now())
	return out, found, nil
}

// wkdRow is one row of app.wkd_public_key.
type wkdRow struct {
	address string
	pub     []byte
	fpr     string
}

// selectWKDKeys applies LookupWKDKey's serving rule to the definer's rows (live
// keys first, newest first; then revoked ones). Pure, so the rule is tested
// without a database (wkd_select_test.go).
func selectWKDKeys(rows []wkdRow, now time.Time) (PublishedKey, bool) {
	var (
		live    *wkdRow
		revoked []wkdRow
	)
	for i := range rows {
		v, err := pgpkeys.Assess(rows[i].pub, now)
		if err != nil {
			continue
		}
		switch {
		case v.Revoked:
			revoked = append(revoked, rows[i])
		case live == nil && v.Usable(now):
			live = &rows[i]
		}
	}
	var out PublishedKey
	switch {
	case live != nil:
		out.Address, out.Fingerprint = live.address, live.fpr
		out.PublicKey = append(out.PublicKey, live.pub...)
	case len(revoked) > 0:
		out.Address, out.Fingerprint = revoked[0].address, revoked[0].fpr
		out.RevocationsOnly = true
	default:
		return PublishedKey{}, false
	}
	for _, r := range revoked {
		out.PublicKey = append(out.PublicKey, r.pub...)
	}
	return out, true
}
