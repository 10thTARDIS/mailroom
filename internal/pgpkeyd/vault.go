// SPDX-License-Identifier: AGPL-3.0-only

package pgpkeyd

import (
	"context"
	"errors"
	"sync"
	"time"

	"mailroom/internal/store"
)

// ErrNoSuchKey is returned by a Vault when no key has the requested ID.
var ErrNoSuchKey = errors.New("pgpkeyd: no such key")

// Record is one stored PGP key pair.
//
// Sealed holds the private half, encrypted under a key derived from the crypto
// process's own root secret (ADR-024). It is the only field that must never
// leave this process.
type Record struct {
	// KeyID is Mailroom's identifier for this key (a ULID).
	KeyID string
	// UserID is the owner. It is a FIELD, not part of the lookup key — see
	// Vault below for why that is deliberate.
	UserID string
	// Fingerprint is the OpenPGP primary key fingerprint, uppercase hex.
	Fingerprint string
	// PublicKey is the public key as BINARY OpenPGP packets — not armor. This
	// is the representation the store column holds and the WKD endpoint serves
	// verbatim; armor is applied only at the wire boundary (see publicInfo).
	PublicKey []byte
	// Sealed is the private key, sealed under HKDF(pgpRoot, salt=userID).
	Sealed    []byte
	CreatedAt time.Time
	// DecryptOnly and RevokedAt are the key's RETIREMENT state, and they are on
	// the record rather than left to the store because the service has to refuse
	// signing with a retired key (spec §9). A retired key is kept forever so old
	// mail stays readable; what it must never do is produce a NEW signature,
	// because a correspondent cannot tell "signed before retirement" from
	// "signed by whoever took the key afterwards".
	DecryptOnly bool
	RevokedAt   *time.Time
	// Origin says whether this key was minted here or handed to us, and it is
	// set by the two handlers that create records — Generate and Import — which
	// are the only places in the system that know.
	//
	// ⚠ IT IS RECORDED BECAUSE A HARD DELETE CANNOT BE JUDGED WITHOUT IT (#54).
	// Destroying a GENERATED key destroys the only copy of the private half, and
	// retroactively makes every PGP-encrypted message the user already exported
	// unreadable. Destroying an IMPORTED key costs the user nothing, because
	// they still hold it. The rows are otherwise identical, and nothing else in
	// the system — not app.audit_log, which is empty — can tell them apart
	// after the fact.
	Origin store.KeyOrigin
}

// Retired reports that this key may only decrypt. Signing paths ask this
// question; the two fields answer it for different reasons (an explicit
// downgrade, and an actual revocation) and neither alone is the whole answer.
func (r Record) Retired() bool { return r.DecryptOnly || r.RevokedAt != nil }

// Vault stores key records. Increment 3 (Forgejo #47) replaces the in-memory
// implementation with a Postgres-backed one under RLS; the interface is what
// keeps increment 2 free of migrations.
//
// # Why Get takes a key ID alone
//
// Looking up by (userID, keyID) would make cross-user isolation structural:
// another user's key simply would not be found, with no check to write and
// none to get wrong. That sounds strictly better and is in fact worse HERE,
// because it is unprobeable. ADR-024's confused-deputy test is required to
// demonstrate that it can fail, and a test whose subject cannot be deleted
// cannot be shown capable of failing — this project has shipped three such
// tests already, each of which passed for a reason unrelated to the mechanism
// it named.
//
// So the ownership comparison lives in the service, in the open, one line, and
// the test proves it is load-bearing by removing it. The database-backed vault
// of increment 3 adds RLS underneath as defence in depth; it does not replace
// this check.
type Vault interface {
	// Insert stores a new record. It must reject a duplicate KeyID, and must
	// return ErrFingerprintTaken if the user already holds that fingerprint.
	Insert(ctx context.Context, rec Record) error
	// Get returns the record with this key ID, or ErrNoSuchKey. An
	// implementation may or may not filter by owner — StoreVault does, via RLS;
	// MemoryVault deliberately does not — so THE CALLER PERFORMS THE OWNERSHIP
	// CHECK REGARDLESS. See MemoryVault for why that redundancy is the point
	// rather than an oversight.
	//
	// The calling user's id travels on the context (WithCaller) rather than as
	// a parameter, so that an implementation which needs it cannot silently be
	// handed the wrong one — it either has the caller or fails closed.
	Get(ctx context.Context, keyID string) (Record, error)
}

// IdentityBinder is implemented by vaults that can bind a key to one of the
// user's addresses. It is separate from Vault because binding touches a second
// table (app.identities) and its own change_log stream, which the in-memory
// test double has no business simulating.
type IdentityBinder interface {
	// BindIdentity reports whether the binding happened. False means the key or
	// the identity was not the caller's to bind.
	BindIdentity(ctx context.Context, keyID, identityID string) (bool, error)
}

// KeyLister is implemented by vaults that can return every key a user holds.
// Decrypt needs it: an encrypted message names its recipient by OpenPGP key id,
// not by Mailroom's key id, so the process has to offer the library the user's
// whole keyring rather than one record.
//
// ⚠ LIKE Get, AN IMPLEMENTATION MAY OR MAY NOT FILTER BY OWNER, AND THE SERVICE
// FILTERS REGARDLESS. StoreVault reads under RLS; MemoryVault deliberately
// returns every user's records, so that deleting the service's ownership filter
// in Decrypt turns TestDecryptConfusedDeputyFailsClosed red.
//
// ⚠ IT RETURNS RETIRED KEYS. A decrypt_only or revoked key still decrypts the
// mail encrypted to it, forever (spec §9); filtering here "for safety" would be
// copying Sign's shape into the one method where it is exactly wrong.
type KeyLister interface {
	List(ctx context.Context) ([]Record, error)
}

// DecryptAudit is one decrypt call, as the persistent trail records it. It
// carries NO content: no plaintext, no ciphertext, no subject, no session key.
type DecryptAudit struct {
	UserID    string
	MessageID string
	Purpose   string
	// Decrypted is the outcome. Failures are recorded too: a run of failures
	// for one user is what an oracle attempt through a compromised application
	// would look like from here.
	Decrypted bool
	// Fingerprint is the PRIMARY key fingerprint of the key that opened the
	// message, set only when Decrypted.
	Fingerprint string
}

// DecryptAuditor is implemented by vaults that persist the decrypt trail
// (#47 checklist item 4).
//
// ⚠ Decrypt FAILS CLOSED WITHOUT ONE. A vault that cannot record the call does
// not get plaintext back: "decrypted N messages for user U" must be
// reconstructable, and a trail with gaps is not.
type DecryptAuditor interface {
	RecordDecrypt(ctx context.Context, a DecryptAudit) error
}

// MemoryVault is the UNFILTERED vault, and since increment 3 that is its
// entire purpose: it is a test double, not a storage option.
//
// StoreVault (the real one) reads under RLS, so a request for another user's
// key never returns a row at all — isolation becomes structural, exactly the
// property this package deliberately avoided relying on alone. That is good for
// production and bad for testing, because a service check that can never fire
// cannot be shown to be load-bearing.
//
// So MemoryVault hands back whatever key id it is asked for, regardless of
// owner. It is what lets TestConfusedDeputyFailsClosed prove that the SERVICE
// refuses a foreign key on its own account rather than merely inheriting a
// refusal from PostgreSQL — and it is what makes deleting the ownership
// comparison turn that test red. The database layer's refusal is proved
// separately, against the real store, in the integration suite.
//
// Two independent controls over one property, each with its own test:
//
//	RLS               enforced by PostgreSQL, holds if the Go code is wrong
//	ownership compare enforced by three lines, holds if RLS is routed around
//
// Neither is redundant. Deleting the comparison because "RLS covers it" leaves
// the property true but unprovable, which is how it comes to be removed for real.
type MemoryVault struct {
	mu      sync.RWMutex
	records map[string]Record
	audits  []DecryptAudit
	// auditErr, when set, makes RecordDecrypt fail — the test hook for "the
	// trail could not be written, so no plaintext leaves".
	auditErr error
}

// NewMemoryVault returns an empty in-memory vault.
func NewMemoryVault() *MemoryVault {
	return &MemoryVault{records: make(map[string]Record)}
}

// Insert implements Vault.
func (v *MemoryVault) Insert(ctx context.Context, rec Record) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	v.mu.Lock()
	defer v.mu.Unlock()
	if _, exists := v.records[rec.KeyID]; exists {
		return errors.New("pgpkeyd: duplicate key id")
	}
	// Same contract as the real vault: a fingerprint the user already holds is
	// a no-op, not a fault. Without this the in-memory double would let an
	// import test pass that the store would reject.
	for _, existing := range v.records {
		if existing.UserID == rec.UserID && existing.Fingerprint == rec.Fingerprint {
			return ErrFingerprintTaken
		}
	}
	v.records[rec.KeyID] = rec
	return nil
}

// Get implements Vault.
func (v *MemoryVault) Get(ctx context.Context, keyID string) (Record, error) {
	if err := ctx.Err(); err != nil {
		return Record{}, err
	}
	v.mu.RLock()
	defer v.mu.RUnlock()
	rec, ok := v.records[keyID]
	if !ok {
		return Record{}, ErrNoSuchKey
	}
	return rec, nil
}

// List implements KeyLister. ⚠ UNFILTERED, on purpose: see KeyLister.
func (v *MemoryVault) List(ctx context.Context) ([]Record, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	v.mu.RLock()
	defer v.mu.RUnlock()
	out := make([]Record, 0, len(v.records))
	for _, r := range v.records {
		out = append(out, r)
	}
	return out, nil
}

// RecordDecrypt implements DecryptAuditor.
func (v *MemoryVault) RecordDecrypt(ctx context.Context, a DecryptAudit) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	v.mu.Lock()
	defer v.mu.Unlock()
	if v.auditErr != nil {
		return v.auditErr
	}
	v.audits = append(v.audits, a)
	return nil
}

// Audits returns a copy of the decrypt trail recorded so far (tests).
func (v *MemoryVault) Audits() []DecryptAudit {
	v.mu.RLock()
	defer v.mu.RUnlock()
	return append([]DecryptAudit(nil), v.audits...)
}

// UpdateKey implements KeyUpdater, with the store's contract: same fingerprint,
// not already revoked. Unfiltered by owner, like Get — the service checks.
func (v *MemoryVault) UpdateKey(ctx context.Context, rec Record, revokedAt *time.Time) (bool, error) {
	if err := ctx.Err(); err != nil {
		return false, err
	}
	v.mu.Lock()
	defer v.mu.Unlock()
	cur, ok := v.records[rec.KeyID]
	if !ok || cur.Fingerprint != rec.Fingerprint || cur.RevokedAt != nil {
		return false, nil
	}
	cur.PublicKey = rec.PublicKey
	cur.Sealed = rec.Sealed
	if revokedAt != nil {
		t := *revokedAt
		cur.RevokedAt = &t
		cur.DecryptOnly = true
	}
	v.records[rec.KeyID] = cur
	return true, nil
}

// retire marks a stored record retired, for tests that need a decrypt_only or
// revoked key. The vault has no write path for retirement because nothing in
// the process performs one; the store does (migration-era rows, the CLI).
func (v *MemoryVault) retire(keyID string, decryptOnly bool, revokedAt *time.Time) bool {
	v.mu.Lock()
	defer v.mu.Unlock()
	r, ok := v.records[keyID]
	if !ok {
		return false
	}
	r.DecryptOnly = decryptOnly
	r.RevokedAt = revokedAt
	v.records[keyID] = r
	return true
}
