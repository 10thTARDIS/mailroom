// SPDX-License-Identifier: AGPL-3.0-only

package store

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/oklog/ulid/v2"

	"mailroom/internal/pgpkeys"
)

// ContactKeySource records how a correspondent's public key reached us. The
// values match the `source` column's documented set in the baseline schema.
//
// ⚠ THE SOURCE IS NOT DECORATION. The routes differ in what they prove:
// `wkd` means the key was served by the address's own domain over HTTPS, while
// `autocrypt` means a message claiming that address carried it — anyone who can
// send mail can do the latter. A future "which key do we encrypt with" decision
// has to be able to tell them apart, so it is recorded now rather than
// reconstructed later.
const (
	ContactKeyAutocrypt = "autocrypt"
	ContactKeyWKD       = "wkd"
	ContactKeyManual    = "manual"
	ContactKeyKeyserver = "keyserver"
)

// ContactKeyRow is one correspondent's public key, as stored.
type ContactKeyRow struct {
	ContactID   ulid.ULID
	Kind        KeyKind
	Fingerprint string
	PublicKey   []byte
	Source      string
	// Authenticated: the message that carried this key was authenticated for its
	// From domain (DMARC pass under the topmost trusted Authentication-Results,
	// or an aligned local DKIM pass). Meaningful for Source autocrypt only;
	// encryption ignores autocrypt keys where it is false (#47, ADR-038).
	Authenticated bool
}

// ContactIDForAddress resolves an email address to the contact that owns it.
// ok is false when no contact has that address, which is ordinary: contacts are
// created by ingest, so a key can arrive for an address in the same message
// that creates the contact — callers upsert contacts FIRST, then look up.
//
// The address column is `citext`, so the comparison is case-insensitive in the
// database rather than by lowercasing here, which would disagree with it for
// non-ASCII.
func (tx *UserTx) ContactIDForAddress(ctx context.Context, addr string) (ulid.ULID, bool, error) {
	var id string
	err := tx.tx.QueryRow(ctx, `
		SELECT contact_id FROM app.contact_addresses
		 WHERE user_id = $1 AND address = $2
		 ORDER BY last_seen_at DESC
		 LIMIT 1`,
		tx.userID.String(), addr).Scan(&id)
	if errors.Is(err, pgx.ErrNoRows) {
		return ulid.ULID{}, false, nil
	}
	if err != nil {
		return ulid.ULID{}, false, fmt.Errorf("store: contact for address: %w", err)
	}
	u, err := ulid.Parse(id)
	if err != nil {
		return ulid.ULID{}, false, fmt.Errorf("store: parse contact id %q: %w", id, err)
	}
	return u, true, nil
}

// UpsertContactKey stores a discovered public key for a contact. created
// reports whether this was a key we did not already hold (a new row, or a key
// the user had removed and that is now current again: UpsertContactKeyDetail).
//
// ⚠ A NEW KEY APPENDS NO change_log ROW; A MERGE THAT CHANGES THE STORED KEY
// DOES. A brand-new key from ingest or discovery has been recorded this way
// since #48 and is left as it was. A RENEWAL is different: the stored bytes
// change under a key the Contact already lists, so it writes the Contact
// `updated` row that ImportContactKey and TrustContactKey write for a key change
// (Contact.keys is part of the synced Contact object; CLAUDE.md invariant). A
// merge that adds nothing writes no row.
//
// ⚠ RE-DISCOVERY IS NOT A NEW KEY. The unique key is (user, contact,
// fingerprint), so seeing the same key again advances fetched_at, and created
// is false. This is what makes running discovery repeatedly cheap and makes
// "how many NEW keys did that find" a real number rather than a count of rows
// touched.
//
// ⚠ BUT A RE-SIGHTING IS MERGED, NOT IGNORED (#795, ADR-040). Until v1.93 the
// conflict branch never touched public_key, so a correspondent who extended
// their key's expiry (same fingerprint, new self-signature) stayed expired here
// forever, by every route. Now the incoming copy is MERGED into the stored one
// (pgpkeys.Merge: union of user IDs, subkeys and verified self-signatures), and
// written only when the merge adds something. Merging rather than replacing is
// what stops a replayed old copy stripping a revocation. A copy that cannot be
// merged (unreadable, a different primary key, a result that would not parse)
// leaves the stored key exactly as it was — the pre-v1.93 behaviour — rather
// than failing the caller's transaction.
//
// Same fingerprint means same pin: nothing here touches app.contact_key_pins,
// so a renewal can never read as a changed key (ADR-039).
//
// ⚠ A CONTACT MAY HOLD SEVERAL KEYS. Nothing here replaces or supersedes an
// existing key: people rotate keys and publish more than one, and silently
// dropping the old one would break verification of everything already received.
// Choosing which key to ENCRYPT with is a separate decision that belongs with
// the encrypt work (#47), and it will need this history.
func (tx *UserTx) UpsertContactKey(ctx context.Context, k ContactKeyRow) (bool, error) {
	r, err := tx.UpsertContactKeyDetail(ctx, k)
	return r.Created || r.Revived, err
}

// UpsertResult reports what UpsertContactKeyDetail did with one sighting.
type UpsertResult struct {
	// ID is the held row's id (new, revived or existing). Zero only when the
	// insert lost a race to a concurrent insert of the same key.
	ID ulid.ULID
	// Created: a key this user did not hold before (a new row).
	Created bool
	// Revived: a key the user had REMOVED (#815) was seen again, and the row is
	// current again (ADR-040 §11).
	Revived bool
	// Merged: the stored copy of a held key changed (a renewal, a new user ID,
	// a revocation), and EventKind says which (contactKeyEventKind).
	Merged    bool
	EventKind string
}

// UpsertContactKeyDetail is UpsertContactKey with the outcome spelled out.
//
// ⚠ A REMOVED KEY SEEN AGAIN IS REVIVED, NOT DUPLICATED (#815, ADR-040 §11).
// The row the user removed becomes current again (removed_at cleared) with the
// sighting's source and authentication, refused cleared, and the incoming copy
// merged in: what a fresh row would have said, on the same id, so its history
// stays attached. A revival writes a first_seen event and the Contact `updated`
// row (the contact's key list grew back).
//
// Events (app.contact_key_events, same savepoint): first_seen for a new or a
// revived key; renewed, updated or revoked_seen for a merge that changed the
// stored copy; nothing for a re-sighting that adds nothing.
func (tx *UserTx) UpsertContactKeyDetail(ctx context.Context, k ContactKeyRow) (UpsertResult, error) {
	var res UpsertResult
	if k.Fingerprint == "" {
		return res, errors.New("store: contact key: fingerprint is required")
	}
	if len(k.PublicKey) == 0 {
		return res, errors.New("store: contact key: public key is required")
	}
	if len(k.PublicKey) > maxPublicKey {
		return res, fmt.Errorf("store: contact key: public key is longer than %d bytes", maxPublicKey)
	}
	if k.Kind == "" {
		k.Kind = KeyPGP
	}
	switch k.Source {
	case ContactKeyAutocrypt, ContactKeyWKD, ContactKeyManual, ContactKeyKeyserver:
	default:
		return res, fmt.Errorf("store: contact key: unknown source %q", k.Source)
	}

	// ONE savepoint around read, merge, write, event and change row: a caller
	// that recovers from an error here (ingest does) must not commit half of it.
	err := tx.inSavepoint(ctx, func() error {
		h, err := tx.lockHeldContactKey(ctx, k.ContactID, k.Fingerprint)
		if err != nil {
			return err
		}
		switch {
		case h.held && !h.removed:
			res.ID = h.id
			merged, changed, kind, err := tx.mergeIntoHeld(ctx, h, k.PublicKey)
			if err != nil {
				return err
			}
			if changed {
				res.Merged, res.EventKind = true, kind
				if err := tx.appendContactKeyEvent(ctx, h.id, kind, k.Source, merged); err != nil {
					return err
				}
				if _, err := tx.AppendChange(ctx, ObjectContact, k.ContactID, OpUpdated); err != nil {
					return err
				}
			}
			if _, err := tx.tx.Exec(ctx, `
				UPDATE app.contact_keys
				   SET fetched_at = now(), authenticated = authenticated OR $3
				 WHERE user_id = $1 AND id = $2`,
				tx.userID.String(), h.id.String(), k.Authenticated); err != nil {
				return fmt.Errorf("re-sighting: %w", err)
			}
			return nil

		case h.held:
			// Removed by the user, and seen again: revive (see above).
			res.ID, res.Revived = h.id, true
			merged, _, _, err := tx.mergeIntoHeld(ctx, h, k.PublicKey)
			if err != nil {
				return err
			}
			if _, err := tx.tx.Exec(ctx, `
				UPDATE app.contact_keys
				   SET removed_at = NULL, source = $3, authenticated = $4, refused = false, fetched_at = now()
				 WHERE user_id = $1 AND id = $2`,
				tx.userID.String(), h.id.String(), k.Source, k.Authenticated); err != nil {
				return fmt.Errorf("revive: %w", err)
			}
			if err := tx.appendContactKeyEvent(ctx, h.id, KeyEventFirstSeen, k.Source, merged); err != nil {
				return err
			}
			_, err = tx.AppendChange(ctx, ObjectContact, k.ContactID, OpUpdated)
			return err
		}

		// Not held when we looked. ON CONFLICT still guards the race with a
		// concurrent insert of the same key, exactly as before.
		id := NewULID()
		var inserted bool
		if err := tx.tx.QueryRow(ctx, `
			INSERT INTO app.contact_keys
			       (id, user_id, contact_id, kind, fingerprint, public_key, source, authenticated)
			VALUES ($1, $2, $3, $4, $5, $6, $7, $8)
			ON CONFLICT (user_id, contact_id, fingerprint)
			DO UPDATE SET fetched_at = now(),
			              authenticated = app.contact_keys.authenticated OR EXCLUDED.authenticated
			RETURNING (xmax = 0) AS inserted`,
			id.String(), tx.userID.String(), k.ContactID.String(),
			string(k.Kind), k.Fingerprint, k.PublicKey, k.Source, k.Authenticated).Scan(&inserted); err != nil {
			return err
		}
		if !inserted {
			return nil
		}
		res.ID, res.Created = id, true
		return tx.appendContactKeyEvent(ctx, id, KeyEventFirstSeen, k.Source, k.PublicKey)
	})
	if err != nil {
		return UpsertResult{}, fmt.Errorf("store: upsert contact key: %w", err)
	}
	return res, nil
}

// heldContactKey is one (contact, fingerprint) row as locked for a write.
type heldContactKey struct {
	held, removed bool
	id            ulid.ULID
	stored        []byte
}

// lockHeldContactKey reads the row for (contact, fingerprint) FOR UPDATE, so
// two concurrent renewals cannot each merge into the same old copy and lose
// one another. A removed row is returned too (removed set): the unique key
// covers it, and each caller decides what a removed row means for it.
func (tx *UserTx) lockHeldContactKey(ctx context.Context, contactID ulid.ULID, fingerprint string) (heldContactKey, error) {
	var (
		h       heldContactKey
		id      string
		removed *time.Time
	)
	err := tx.tx.QueryRow(ctx, `
		SELECT id, public_key, removed_at FROM app.contact_keys
		 WHERE user_id = $1 AND contact_id = $2 AND fingerprint = $3
		 FOR UPDATE`,
		tx.userID.String(), contactID.String(), fingerprint).Scan(&id, &h.stored, &removed)
	if errors.Is(err, pgx.ErrNoRows) {
		return heldContactKey{}, nil
	}
	if err != nil {
		return heldContactKey{}, fmt.Errorf("store: read held contact key: %w", err)
	}
	parsed, err := ulid.Parse(id)
	if err != nil {
		return heldContactKey{}, fmt.Errorf("store: contact key id %q: %w", id, err)
	}
	h.held, h.removed, h.id = true, removed != nil, parsed
	return h, nil
}

// mergeIntoHeld merges incoming into the locked row's stored copy and writes
// the result when, and only when, the merge changed the stored bytes. It
// returns the bytes now stored (merged, or the old copy), whether they changed,
// and which kind of event the change is (contactKeyEventKind). Callers run it
// inside their own savepoint and write their own event and change row.
//
// An unmergeable incoming copy is not an error: the stored key stays as it was
// (see UpsertContactKey).
func (tx *UserTx) mergeIntoHeld(ctx context.Context, h heldContactKey, incoming []byte) (stored []byte, changed bool, kind string, err error) {
	merged, differs, merr := pgpkeys.Merge(h.stored, incoming)
	if merr != nil || !differs || len(merged) > maxPublicKey {
		return h.stored, false, "", nil
	}
	if _, err := tx.tx.Exec(ctx, `
		UPDATE app.contact_keys SET public_key = $3
		 WHERE user_id = $1 AND id = $2`,
		tx.userID.String(), h.id.String(), merged); err != nil {
		return h.stored, false, "", fmt.Errorf("store: write merged contact key: %w", err)
	}
	return merged, true, contactKeyEventKind(h.stored, merged, time.Now().UTC()), nil
}

// contactKeySelect is the projection scanContactKeys reads.
const contactKeySelect = `
		SELECT contact_id, kind, fingerprint, public_key, source, fetched_at, authenticated, refused, id, removed_at
		  FROM app.contact_keys`

// notRemoved is THE predicate for "a key the user holds now" (#815). Every
// reader that picks a key for encryption, verification, de-duplication or the
// current display uses it; only the history reads (includeRemoved) leave it out.
const notRemoved = ` AND removed_at IS NULL`

// AllContactKeys returns every correspondent key this user holds (not removed)
// — the population `pgp-report`'s expiry section counts (#795). Read-only.
func (tx *UserTx) AllContactKeys(ctx context.Context) ([]ContactKey, error) {
	return tx.allContactKeys(ctx, false)
}

// AllContactKeysIncludingRemoved is AllContactKeys plus the keys the user
// removed (#815, ContactKey/get includeRemoved): history, never a key source.
func (tx *UserTx) AllContactKeysIncludingRemoved(ctx context.Context) ([]ContactKey, error) {
	return tx.allContactKeys(ctx, true)
}

func (tx *UserTx) allContactKeys(ctx context.Context, includeRemoved bool) ([]ContactKey, error) {
	q := contactKeySelect + `
		 WHERE user_id = $1`
	if !includeRemoved {
		q += notRemoved
	}
	rows, err := tx.tx.Query(ctx, q+`
		 ORDER BY fetched_at DESC, contact_id, fingerprint`,
		tx.userID.String())
	if err != nil {
		return nil, fmt.Errorf("store: all contact keys: %w", err)
	}
	return scanContactKeys(rows)
}

// ContactKey is a stored correspondent key, as read back.
//
// PublicKey stays on this struct for the callers that already read it (the
// exporter, key discovery's own de-dup checks) — but Contact/get (#448/#374)
// MUST NOT serialise it: the client needs the FACT that a key is held, not the
// key material, and `internal/api/contact.go`'s renderer builds its JSON object
// field-by-field rather than round-tripping this struct, precisely so a future
// `json` tag added here cannot leak it by accident.
type ContactKey struct {
	// ID is the row's own id: what ContactKey/get publishes and ContactKey/set
	// destroys by (#796). An identifier, not key material.
	ID          ulid.ULID
	ContactID   ulid.ULID
	Kind        KeyKind
	Fingerprint string
	PublicKey   []byte
	Source      string
	// FetchedAt is when this key was last (re)discovered — UpsertContactKey
	// advances it on every re-sighting, so it also answers "how fresh is this".
	FetchedAt time.Time
	// Authenticated: see ContactKeyRow.Authenticated.
	Authenticated bool
	// Refused: the user chose to keep the old key rather than this one (#47,
	// migration 00069). A refused key is never encrypted to.
	Refused bool
	// RemovedAt: the user removed it (#815, soft delete). Only the
	// IncludingRemoved readers return such a row.
	RemovedAt *time.Time
}

// ContactKeysFor returns every key held for one contact (not removed), newest
// fetch first. It is the reader encryption (compose.ResolveRecipientKey),
// signature attribution (pgp_signature.go), the key-changed decision and key
// discovery's de-duplication all use, so a removed key is none of theirs.
func (tx *UserTx) ContactKeysFor(ctx context.Context, contactID ulid.ULID) ([]ContactKey, error) {
	rows, err := tx.tx.Query(ctx, contactKeySelect+`
		 WHERE user_id = $1 AND contact_id = $2`+notRemoved+`
		 ORDER BY fetched_at DESC, fingerprint`,
		tx.userID.String(), contactID.String())
	if err != nil {
		return nil, fmt.Errorf("store: contact keys: %w", err)
	}
	return scanContactKeys(rows)
}

// ContactKeysForMany is ContactKeysFor for a whole Contact/get id list, in ONE
// query rather than one per contact (#448/#374) — a Contact/get batch that
// fanned out per-id here would be the N+1 query Contact/get otherwise avoids
// (see GetContacts). A nil/empty ids list is a no-op, not a query naming an
// empty ANY() (which Postgres would happily answer with zero rows anyway, but
// there is no reason to round-trip for it).
func (tx *UserTx) ContactKeysForMany(ctx context.Context, ids []ulid.ULID) (map[ulid.ULID][]ContactKey, error) {
	return tx.contactKeysForMany(ctx, ids, false)
}

// ContactKeysForManyIncludingRemoved is ContactKeysForMany plus removed keys
// (ContactKey/get includeRemoved).
func (tx *UserTx) ContactKeysForManyIncludingRemoved(ctx context.Context, ids []ulid.ULID) (map[ulid.ULID][]ContactKey, error) {
	return tx.contactKeysForMany(ctx, ids, true)
}

func (tx *UserTx) contactKeysForMany(ctx context.Context, ids []ulid.ULID, includeRemoved bool) (map[ulid.ULID][]ContactKey, error) {
	out := make(map[ulid.ULID][]ContactKey, len(ids))
	if len(ids) == 0 {
		return out, nil
	}
	q := contactKeySelect + `
		 WHERE user_id = $1 AND contact_id = ANY($2)`
	if !includeRemoved {
		q += notRemoved
	}
	rows, err := tx.tx.Query(ctx, q+`
		 ORDER BY contact_id, fetched_at DESC, fingerprint`,
		tx.userID.String(), ulidStrings(ids))
	if err != nil {
		return nil, fmt.Errorf("store: contact keys for many: %w", err)
	}
	keys, err := scanContactKeys(rows)
	if err != nil {
		return nil, err
	}
	for _, k := range keys {
		out[k.ContactID] = append(out[k.ContactID], k)
	}
	return out, nil
}

// ContactKeyByID returns one held (not removed) correspondent key by its row id.
func (tx *UserTx) ContactKeyByID(ctx context.Context, id ulid.ULID) (ContactKey, bool, error) {
	rows, err := tx.tx.Query(ctx, contactKeySelect+`
		 WHERE user_id = $1 AND id = $2`+notRemoved,
		tx.userID.String(), id.String())
	if err != nil {
		return ContactKey{}, false, fmt.Errorf("store: contact key by id: %w", err)
	}
	keys, err := scanContactKeys(rows)
	if err != nil || len(keys) == 0 {
		return ContactKey{}, false, err
	}
	return keys[0], true, nil
}

// scanContactKeys reads the contactKeySelect projection.
func scanContactKeys(rows interface {
	Next() bool
	Scan(...any) error
	Err() error
	Close()
},
) ([]ContactKey, error) {
	defer rows.Close()

	var out []ContactKey
	for rows.Next() {
		var (
			k    ContactKey
			cid  string
			kind string
			kid  string
		)
		if err := rows.Scan(&cid, &kind, &k.Fingerprint, &k.PublicKey, &k.Source, &k.FetchedAt,
			&k.Authenticated, &k.Refused, &kid, &k.RemovedAt); err != nil {
			return nil, fmt.Errorf("store: scan contact key: %w", err)
		}
		keyID, err := ulid.Parse(kid)
		if err != nil {
			return nil, fmt.Errorf("store: parse contact key id %q: %w", kid, err)
		}
		k.ID = keyID
		id, err := ulid.Parse(cid)
		if err != nil {
			return nil, fmt.Errorf("store: parse contact id %q: %w", cid, err)
		}
		k.ContactID = id
		k.Kind = KeyKind(kind)
		out = append(out, k)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("store: contact keys: %w", err)
	}
	return out, nil
}

// CountContactKeys reports how many correspondent keys this user holds (not
// removed), split by source — the number that says whether key discovery is
// working at all.
//
// Per source rather than as one total, deliberately: "12 keys" cannot
// distinguish a working WKD fetcher from a working Autocrypt reader, and those
// two failing are different problems with different fixes.
func (tx *UserTx) CountContactKeys(ctx context.Context) (map[string]int, error) {
	rows, err := tx.tx.Query(ctx, `
		SELECT source, count(*) FROM app.contact_keys
		 WHERE user_id = $1`+notRemoved+` GROUP BY source ORDER BY source`,
		tx.userID.String())
	if err != nil {
		return nil, fmt.Errorf("store: count contact keys: %w", err)
	}
	defer rows.Close()

	out := map[string]int{}
	for rows.Next() {
		var (
			src string
			n   int
		)
		if err := rows.Scan(&src, &n); err != nil {
			return nil, fmt.Errorf("store: scan contact key count: %w", err)
		}
		out[src] = n
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("store: count contact keys: %w", err)
	}
	return out, nil
}

// ContactKeyPin returns the fingerprint last encrypted to for address, if any
// (trust on first use, migration 00068).
func (tx *UserTx) ContactKeyPin(ctx context.Context, address string) (fingerprint string, found bool, err error) {
	err = tx.tx.QueryRow(ctx, `
		SELECT fingerprint FROM app.contact_key_pins
		 WHERE user_id = $1 AND address = $2`,
		tx.userID.String(), address).Scan(&fingerprint)
	if errors.Is(err, pgx.ErrNoRows) {
		return "", false, nil
	}
	if err != nil {
		return "", false, fmt.Errorf("store: contact key pin: %w", err)
	}
	return fingerprint, true, nil
}

// SetContactKeyPin records the fingerprint encrypted to for address, replacing
// any earlier pin. Callers decide WHEN replacing is legitimate (compose: the
// first send, an unchanged key, or an explicitly imported key); this only writes.
func (tx *UserTx) SetContactKeyPin(ctx context.Context, address, fingerprint, source string) error {
	if address == "" || fingerprint == "" {
		return errors.New("store: contact key pin: address and fingerprint are required")
	}
	if _, err := tx.tx.Exec(ctx, `
		INSERT INTO app.contact_key_pins (user_id, address, fingerprint, source)
		VALUES ($1, $2, $3, $4)
		ON CONFLICT (user_id, address)
		DO UPDATE SET fingerprint = EXCLUDED.fingerprint, source = EXCLUDED.source, pinned_at = now()`,
		tx.userID.String(), address, fingerprint, source); err != nil {
		return fmt.Errorf("store: set contact key pin: %w", err)
	}
	return nil
}

// PinContactKeyIfAbsent records fingerprint as the pinned key for address only
// when none is pinned yet (trust on first use, by VERIFICATION rather than by
// sending: the first eligible key whose signature checked out is "the key we
// have seen"). It never replaces a pin, and it appends no change_log row for
// the same reason SetContactKeyPin does not (the pin is not a JMAP object).
func (tx *UserTx) PinContactKeyIfAbsent(ctx context.Context, address, fingerprint, source string) error {
	if address == "" || fingerprint == "" {
		return errors.New("store: contact key pin: address and fingerprint are required")
	}
	if _, err := tx.tx.Exec(ctx, `
		INSERT INTO app.contact_key_pins (user_id, address, fingerprint, source)
		VALUES ($1, $2, $3, $4)
		ON CONFLICT (user_id, address) DO NOTHING`,
		tx.userID.String(), address, fingerprint, source); err != nil {
		return fmt.Errorf("store: pin contact key if absent: %w", err)
	}
	return nil
}

// ErrContactKeyNotFound: the contact holds no key with that fingerprint.
var ErrContactKeyNotFound = errors.New("store: contact key not found")

// lockCurrentContactKey reads a held, NOT REMOVED key for a decision: a removed
// key cannot be trusted or refused (it is not one the user holds).
func (tx *UserTx) lockCurrentContactKey(ctx context.Context, contactID ulid.ULID, fingerprint string) (id ulid.ULID, pub []byte, source string, refused bool, err error) {
	var s string
	err = tx.tx.QueryRow(ctx, `
		SELECT id, public_key, source, refused FROM app.contact_keys
		 WHERE user_id = $1 AND contact_id = $2 AND fingerprint = $3 AND removed_at IS NULL
		 FOR UPDATE`,
		tx.userID.String(), contactID.String(), fingerprint).Scan(&s, &pub, &source, &refused)
	if errors.Is(err, pgx.ErrNoRows) {
		return ulid.ULID{}, nil, "", false, ErrContactKeyNotFound
	}
	if err != nil {
		return ulid.ULID{}, nil, "", false, fmt.Errorf("store: read contact key: %w", err)
	}
	id, err = ulid.Parse(s)
	if err != nil {
		return ulid.ULID{}, nil, "", false, fmt.Errorf("store: contact key id %q: %w", s, err)
	}
	return id, pub, source, refused, nil
}

// TrustContactKey records "trust this key from now on" for address: the key
// becomes source 'manual' (so ADR-038 ranks it above WKD and Autocrypt) and
// eligible (refused cleared, most recently fetched), and it becomes the pin.
// The pin write, the key write, the `pinned` event and the Contact change_log
// row are one savepoint: Contact.keys (source, refused) is part of the synced
// Contact object, so the change row is owed (CLAUDE.md: every mutation of a
// JMAP-visible object).
//
// A key that is already the trusted pin (manual, not refused, pinned for this
// address) is a no-op: no write, no event, no row (#815).
func (tx *UserTx) TrustContactKey(ctx context.Context, contactID ulid.ULID, address, fingerprint string) error {
	return tx.inSavepoint(ctx, func() error {
		id, pub, source, refused, err := tx.lockCurrentContactKey(ctx, contactID, fingerprint)
		if err != nil {
			return err
		}
		pinned, hasPin, err := tx.ContactKeyPin(ctx, address)
		if err != nil {
			return err
		}
		if source == ContactKeyManual && !refused && hasPin && pinned == fingerprint {
			return nil
		}
		if _, err := tx.tx.Exec(ctx, `
			UPDATE app.contact_keys
			   SET source = $3, refused = false, fetched_at = now()
			 WHERE user_id = $1 AND id = $2`,
			tx.userID.String(), id.String(), ContactKeyManual); err != nil {
			return fmt.Errorf("store: trust contact key: %w", err)
		}
		if err := tx.SetContactKeyPin(ctx, address, fingerprint, ContactKeyManual); err != nil {
			return err
		}
		if err := tx.appendContactKeyEvent(ctx, id, KeyEventPinned, KeyEventSourceUser, pub); err != nil {
			return err
		}
		_, err = tx.AppendChange(ctx, ObjectContact, contactID, OpUpdated)
		return err
	})
}

// ImportContactKey stores a key the USER deliberately imported for address
// (#47, ADR-038): source 'manual', eligible (refused cleared; a key already held
// under the same fingerprint is promoted, not duplicated, and the imported copy
// is MERGED into the held one rather than replacing it — #795, ADR-040, so an
// old export the user happens to paste cannot strip a revocation or shorten a
// renewal we already hold), `authenticated` left
// false because it has no meaning for a manual key, and it REPLACES the pin for
// the address. Replacing is legitimate here and in TrustContactKey only: the user
// is naming the key explicitly. A key the user had removed is revived (#815).
//
// Key row, pin, ONE event (first_seen for a key not held, or held but removed;
// pinned for a held key) and the Contact `updated` change_log row are one
// savepoint (Contact.keys is part of the synced Contact; CLAUDE.md invariant).
func (tx *UserTx) ImportContactKey(ctx context.Context, contactID ulid.ULID, address string, k ContactKeyRow) error {
	if k.Fingerprint == "" || len(k.PublicKey) == 0 {
		return errors.New("store: import contact key: fingerprint and public key are required")
	}
	if len(k.PublicKey) > maxPublicKey {
		return fmt.Errorf("store: import contact key: public key is longer than %d bytes", maxPublicKey)
	}
	return tx.inSavepoint(ctx, func() error {
		// Merge first; the INSERT below then only promotes the held row. No
		// change row or event from the merge: the import writes its own below,
		// and one logical change is one row and one event.
		h, err := tx.lockHeldContactKey(ctx, contactID, k.Fingerprint)
		if err != nil {
			return err
		}
		stored := k.PublicKey
		if h.held {
			if stored, _, _, err = tx.mergeIntoHeld(ctx, h, k.PublicKey); err != nil {
				return err
			}
		}
		var id string
		if err := tx.tx.QueryRow(ctx, `
			INSERT INTO app.contact_keys
			       (id, user_id, contact_id, kind, fingerprint, public_key, source, authenticated, refused)
			VALUES ($1, $2, $3, 'pgp', $4, $5, $6, false, false)
			ON CONFLICT (user_id, contact_id, fingerprint)
			DO UPDATE SET source = EXCLUDED.source, refused = false, fetched_at = now(), removed_at = NULL
			RETURNING id`,
			NewULID().String(), tx.userID.String(), contactID.String(),
			k.Fingerprint, k.PublicKey, ContactKeyManual).Scan(&id); err != nil {
			return fmt.Errorf("store: import contact key: %w", err)
		}
		keyID, err := ulid.Parse(id)
		if err != nil {
			return fmt.Errorf("store: contact key id %q: %w", id, err)
		}
		if err := tx.SetContactKeyPin(ctx, address, k.Fingerprint, ContactKeyManual); err != nil {
			return err
		}
		kind := KeyEventPinned
		if !h.held || h.removed {
			kind = KeyEventFirstSeen
		}
		if err := tx.appendContactKeyEvent(ctx, keyID, kind, ContactKeyManual, stored); err != nil {
			return err
		}
		_, err = tx.AppendChange(ctx, ObjectContact, contactID, OpUpdated)
		return err
	})
}

// RefuseContactKey records "keep the old key": the key is marked refused, which
// stops it being encrypted to and keeps a signature by it showing as a changed
// key. The pin is untouched. Written with a `refused` event and a Contact
// change_log row, as above; a key already refused is a no-op that writes
// nothing (#815).
func (tx *UserTx) RefuseContactKey(ctx context.Context, contactID ulid.ULID, fingerprint string) error {
	return tx.inSavepoint(ctx, func() error {
		id, pub, _, refused, err := tx.lockCurrentContactKey(ctx, contactID, fingerprint)
		if err != nil {
			return err
		}
		if refused {
			return nil
		}
		if _, err := tx.tx.Exec(ctx, `
			UPDATE app.contact_keys SET refused = true
			 WHERE user_id = $1 AND id = $2`,
			tx.userID.String(), id.String()); err != nil {
			return fmt.Errorf("store: refuse contact key: %w", err)
		}
		if err := tx.appendContactKeyEvent(ctx, id, KeyEventRefused, KeyEventSourceUser, pub); err != nil {
			return err
		}
		_, err = tx.AppendChange(ctx, ObjectContact, contactID, OpUpdated)
		return err
	})
}

// ContactKeyPinsFor returns the encryption pins (trust on first use, migration
// 00068) of every address the given contacts own, as contact id → address →
// pinned fingerprint. A nil ids slice means every contact of the user (#796:
// ContactKey/get with no ids). Pins are keyed by ADDRESS, not by contact, so the
// join through app.contact_addresses is what says whose pin it is.
func (tx *UserTx) ContactKeyPinsFor(ctx context.Context, ids []ulid.ULID) (map[ulid.ULID]map[string]string, error) {
	out := map[ulid.ULID]map[string]string{}
	q := `
		SELECT ca.contact_id, p.address::text, p.fingerprint
		  FROM app.contact_key_pins p
		  JOIN app.contact_addresses ca ON ca.user_id = p.user_id AND ca.address = p.address
		 WHERE p.user_id = $1`
	args := []any{tx.userID.String()}
	if ids != nil {
		if len(ids) == 0 {
			return out, nil
		}
		q += ` AND ca.contact_id = ANY($2)`
		args = append(args, ulidStrings(ids))
	}
	rows, err := tx.tx.Query(ctx, q, args...)
	if err != nil {
		return nil, fmt.Errorf("store: contact key pins: %w", err)
	}
	defer rows.Close()
	for rows.Next() {
		var cid, addr, fpr string
		if err := rows.Scan(&cid, &addr, &fpr); err != nil {
			return nil, fmt.Errorf("store: scan contact key pin: %w", err)
		}
		id, err := ulid.Parse(cid)
		if err != nil {
			return nil, fmt.Errorf("store: parse contact id %q: %w", cid, err)
		}
		if out[id] == nil {
			out[id] = map[string]string{}
		}
		out[id][addr] = fpr
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("store: contact key pins: %w", err)
	}
	return out, nil
}

// RemovedContactKey reports what RemoveContactKey did.
type RemovedContactKey struct {
	// Found: the key existed for this user (and was removed).
	Found     bool
	ContactID ulid.ULID
	// Unpinned lists the contact's addresses whose encryption pin was this key
	// and is now cleared.
	Unpinned []string
}

// RemoveContactKey removes one held correspondent key by its row id (#796,
// ContactKey/set destroy) and, if it was the pinned key for any of its
// contact's addresses, removes that pin too: a pin naming a key we no longer
// hold would turn the next key that arrives into a "key changed" the user can
// do nothing about.
//
// ⚠ A SOFT DELETE SINCE #815. The row stays with removed_at set, so its history
// survives; every reader that chooses keys excludes it (notRemoved). A
// re-sighting revives it (UpsertContactKeyDetail).
//
// ONE savepoint: the removal, the pin removal, ONE `removed` event and ONE
// Contact `updated` change_log row (Contact.keys is part of the synced
// Contact). A key that is not held, already removed, or another user's (RLS
// makes the cases the same) is a no-op that writes nothing.
func (tx *UserTx) RemoveContactKey(ctx context.Context, keyID ulid.ULID) (RemovedContactKey, error) {
	var res RemovedContactKey
	err := tx.inSavepoint(ctx, func() error {
		var cid, fpr string
		var pub []byte
		err := tx.tx.QueryRow(ctx, `
			UPDATE app.contact_keys SET removed_at = now()
			 WHERE user_id = $1 AND id = $2 AND removed_at IS NULL
			 RETURNING contact_id, fingerprint, public_key`,
			tx.userID.String(), keyID.String()).Scan(&cid, &fpr, &pub)
		if errors.Is(err, pgx.ErrNoRows) {
			return nil
		}
		if err != nil {
			return fmt.Errorf("store: remove contact key: %w", err)
		}
		contactID, err := ulid.Parse(cid)
		if err != nil {
			return fmt.Errorf("store: parse contact id %q: %w", cid, err)
		}
		res.Found, res.ContactID = true, contactID
		rows, err := tx.tx.Query(ctx, `
			DELETE FROM app.contact_key_pins p
			 USING app.contact_addresses ca
			 WHERE p.user_id = $1 AND ca.user_id = $1
			   AND ca.contact_id = $2 AND ca.address = p.address
			   AND p.fingerprint = $3
			 RETURNING p.address::text`,
			tx.userID.String(), cid, fpr)
		if err != nil {
			return fmt.Errorf("store: remove contact key pin: %w", err)
		}
		for rows.Next() {
			var a string
			if err := rows.Scan(&a); err != nil {
				rows.Close()
				return fmt.Errorf("store: scan removed pin: %w", err)
			}
			res.Unpinned = append(res.Unpinned, a)
		}
		rows.Close()
		if err := rows.Err(); err != nil {
			return fmt.Errorf("store: remove contact key pin: %w", err)
		}
		if err := tx.appendContactKeyEvent(ctx, keyID, KeyEventRemoved, KeyEventSourceUser, pub); err != nil {
			return err
		}
		_, err = tx.AppendChange(ctx, ObjectContact, contactID, OpUpdated)
		return err
	})
	if err != nil {
		return RemovedContactKey{}, err
	}
	return res, nil
}
