// SPDX-License-Identifier: AGPL-3.0-only

// Package pgpkeyd is the crypto process: the only place in Mailroom that holds
// PGP private key material (ADR-024).
//
// It is a separate binary (cmd/mailroom-crypto) with its OWN root secret, not
// the application's master key. That distinction is the whole point of the
// package. If PGP keys were sealed under HKDF(master, …) the application —
// which already holds the master key — could re-derive them at will, and the
// "boundary" would be process isolation alone: an attacker with application
// RCE would still walk away with the identity keys. A PGP private key is an
// identity, and its exposure is permanent: you cannot rotate away a signature a
// correspondent already trusted, and mail encrypted to that key stays readable
// by whoever took it, forever.
//
// # What this boundary does and does not buy
//
// It protects KEY MATERIAL. It does not make the application untrusted: an
// application that is fully compromised can still ask this process to sign and
// decrypt for the user it is currently serving. What it cannot do is take the
// private keys with it, or reach a different user's.
//
// # Scope
//
// internal/crypto's at-rest paths (blob DEKs, credential keys, model keys, blob
// path HMACs) are untouched and stay in the main application. ADR-013's
// deferral of the wrapped master → KEK → DEK hierarchy remains open on its own
// terms and is not a prerequisite for any of this.
//
// ⚠ This package's existence means the deployment has TWO root secrets, and
// their losses are not symmetric. See docs/key-escrow.md.
package pgpkeyd

import (
	"bytes"
	"context"
	stdcrypto "crypto"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"strings"
	"time"

	"github.com/ProtonMail/go-crypto/openpgp"
	"github.com/ProtonMail/go-crypto/openpgp/armor"
	"github.com/ProtonMail/go-crypto/openpgp/packet"
	"github.com/oklog/ulid/v2"

	"mailroom/internal/crypto"
	"mailroom/internal/pgpkeys"
	"mailroom/internal/pgpwire"
	"mailroom/internal/store"
)

const (
	// rootInfo is the HKDF domain separator for the per-user PGP key-wrapping
	// key. Versioned: a future scheme change gets a new info string rather
	// than silently reinterpreting existing sealed keys.
	rootInfo = "mailroom-pgp-v1"
	// sealAADPrefix begins the associated data bound into every sealed private
	// key. The AAD carries BOTH the user ID and the key ID, so a sealed key
	// cannot be relocated to another user or another key row even by something
	// with write access to the store — the tag will not verify.
	sealAADPrefix = "mailroom-pgp-key-v1:"
)

// Service implements the crypto process's operations. It holds the root secret
// for the process's lifetime and never exposes it.
type Service struct {
	root  []byte
	vault Vault
	log   *slog.Logger
	now   func() time.Time
	// detachSign is openpgp.DetachSign. It is a field for exactly one reason,
	// and the reason is a measurement (#301 C2).
	//
	// Sign refuses to release a signature made with MD5/SHA-1/RIPEMD160. The
	// vector the audit described — an imported key whose declared hash
	// preferences exclude SHA-2 — DOES NOT EXIST in go-crypto v1.4.1: the
	// substitution is governed by acceptableHashesToWrite, a hard-coded table
	// of strong hashes keyed on algorithm and curve, and the key's declared
	// preferences are never consulted on the detached-signature path (see the
	// note on cfg in Sign; the behaviour is asserted by
	// TestSignIgnoresLegacyHashPreferences, which is the canary for it
	// changing).
	//
	// That leaves the refusal unreachable through the real library, and an
	// unreachable check is one nobody can show is load-bearing — the failure
	// mode this project has shipped three times. So the test substitutes a
	// detach-signer that emits a GENUINE armored SHA-1 signature and asserts
	// the daemon refuses it, with the code and the empty body the retirement
	// refusal produces. Unexported, package-internal, and set once in
	// NewService: production always holds openpgp.DetachSign.
	detachSign func(w io.Writer, signer *openpgp.Entity, message io.Reader, cfg *packet.Config) error
	// decryptLimit is the per-user decrypt budget (#47 checklist item 4).
	// Replaced only by tests, in-package.
	decryptLimit *decryptLimiter
	// maxPlaintext is the decrypt output cap, pgpwire.MaxDecryptedBytes in
	// production. A field only so the cap tests need not allocate 64 MiB;
	// TestNewServiceUsesTheWireDecryptCap pins the production value.
	maxPlaintext int
	// decryptSlots bounds decrypts in flight (DefaultDecryptConcurrency).
	decryptSlots chan struct{}
}

// NewService builds the service around a 32-byte root secret. The caller's
// slice is copied; zeroize the original.
func NewService(root []byte, vault Vault, log *slog.Logger) (*Service, error) {
	if len(root) != crypto.KeySize {
		return nil, fmt.Errorf("pgpkeyd: root secret must be %d bytes, got %d", crypto.KeySize, len(root))
	}
	if vault == nil {
		return nil, errors.New("pgpkeyd: nil vault")
	}
	if log == nil {
		return nil, errors.New("pgpkeyd: nil logger")
	}
	cp := make([]byte, len(root))
	copy(cp, root)
	return &Service{
		root:         cp,
		vault:        vault,
		log:          log,
		now:          time.Now,
		detachSign:   openpgp.DetachSign,
		decryptLimit: newDecryptLimiter(DefaultDecryptBurst, DefaultDecryptPerSecond),
		maxPlaintext: pgpwire.MaxDecryptedBytes,
		decryptSlots: make(chan struct{}, DefaultDecryptConcurrency),
	}, nil
}

// wrappingKey derives this user's PGP key-wrapping key from the process root.
//
//	pgp root ──HKDF(salt=userID, info="mailroom-pgp-v1")──▶ wrapping key
//
// The master key appears nowhere in that chain, which is the property ADR-024
// exists to establish.
func (s *Service) wrappingKey(userID string) ([]byte, error) {
	return WrappingKey(s.root, userID)
}

// WrappingKey is the exported form of the same derivation, for the ONE other
// caller allowed to run it: the restore drill (cmd/mailroom-drill), which is
// handed the escrowed root directly and has no Service to ask. Two
// implementations of a key derivation drift silently; this keeps there being
// one.
func WrappingKey(root []byte, userID string) ([]byte, error) {
	return crypto.DeriveKey(root, []byte(userID), rootInfo)
}

func sealAAD(userID, keyID string) []byte {
	return SealAAD(userID, keyID)
}

// SealAAD is the exported form of the AAD construction, for the same reason
// WrappingKey is exported: the drill must bind the same user id and key id
// the sealer did, or a genuine restore would look indistinguishable from a
// tampered one.
func SealAAD(userID, keyID string) []byte {
	return []byte(sealAADPrefix + userID + ":" + keyID)
}

// Generate creates a fresh PGP key pair for a user, seals the private half
// under the process root, stores it, and returns the PUBLIC half only.
func (s *Service) Generate(ctx context.Context, req pgpwire.GenerateRequest) (*pgpwire.GenerateResponse, error) {
	// ⚠ userID, not req.UserID, from here down — see validUserID (#301 C1).
	userID, err := validUserID(req.UserID)
	if err != nil {
		return nil, err
	}
	email := strings.TrimSpace(req.Email)
	if email == "" {
		return nil, pgpwire.Errorf(pgpwire.CodeInvalidRequest, "email is required")
	}
	if !strings.Contains(email, "@") {
		return nil, pgpwire.Errorf(pgpwire.CodeInvalidRequest, "email must be an address")
	}

	// Keys EXPIRE, two years out by default (#795; ADR-040):
	// the primary key and the encryption subkey both, so neither outlives the
	// other. pgp-extend renews without changing the fingerprint.
	created := s.now().UTC().Truncate(time.Second)
	lifetime, err := generateLifetime(req.ExpiresDays, req.ExpiresAt, created)
	if err != nil {
		return nil, err
	}
	cfg := &packet.Config{
		// Ed25519 signing with a Curve25519 encryption subkey, v4 packets.
		// Chosen for interoperability rather than novelty: this is what the
		// clients that will consume these keys via WKD and Autocrypt (#48)
		// actually accept today. V6 keys are deliberately off — they are not
		// yet widely understood by the receiving side, and a key nobody can
		// use is not a published key.
		Algorithm:       packet.PubKeyAlgoEdDSA,
		Curve:           packet.Curve25519,
		V6Keys:          false,
		Time:            func() time.Time { return created },
		KeyLifetimeSecs: lifetime,
	}
	entity, err := openpgp.NewEntity(strings.TrimSpace(req.Name), "", email, cfg)
	if err != nil {
		s.log.Error("pgp key generation failed", "user_id", userID, "err", err)
		return nil, pgpwire.Errorf(pgpwire.CodeInternal, "key generation failed")
	}
	// ⚠ go-crypto's NewEntity puts KeyLifetimeSecs on the PRIMARY only: its
	// encryption subkey is bound with no expiry whatever the config says
	// (key_generation.go, "No key expiry here"; measured on v1.5.2 by
	// TestGenerateDefaultsToTwoYears). The subkey's binding is re-made with the
	// same lifetime, so the key correspondents encrypt to expires with it.
	if lifetime != 0 {
		if err := bindSubkeyLifetimes(entity, lifetime, cfg); err != nil {
			s.log.Error("pgp key generation: subkey lifetime failed", "user_id", userID, "err", err)
			return nil, pgpwire.Errorf(pgpwire.CodeInternal, "key generation failed")
		}
	}

	// Serialize WITHOUT re-signing: this is a storage round-trip, not a
	// publication, and re-signing here would give the stored bytes a different
	// signature on every save for no benefit.
	var priv bytes.Buffer
	if err := entity.SerializePrivateWithoutSigning(&priv, nil); err != nil {
		s.log.Error("pgp private serialization failed", "user_id", userID, "err", err)
		return nil, pgpwire.Errorf(pgpwire.CodeInternal, "key serialization failed")
	}
	defer crypto.Zero(priv.Bytes())

	pub, err := SerializePublic(entity)
	if err != nil {
		s.log.Error("pgp public serialization failed", "user_id", userID, "err", err)
		return nil, pgpwire.Errorf(pgpwire.CodeInternal, "key serialization failed")
	}
	// ⚠ THE ARTEFACT IS CHECKED, NOT ASSUMED (#813): the primary key AND the
	// encryption subkey must expire at exactly created + lifetime, to the
	// second, or never when no lifetime was asked for. An exact date the user
	// chose is a promise; a subkey left on another date would break it quietly.
	if err := checkGenerated(pub, created, lifetime); err != nil {
		s.log.Error("pgp key generation: refusing an artefact that does not say what was asked",
			"user_id", userID, "err", err)
		return nil, pgpwire.Errorf(pgpwire.CodeInternal, "key generation failed")
	}

	keyID := ulid.Make().String()

	wrap, err := s.wrappingKey(userID)
	if err != nil {
		s.log.Error("pgp wrapping key derivation failed", "user_id", userID, "err", err)
		return nil, pgpwire.Errorf(pgpwire.CodeInternal, "key sealing failed")
	}
	defer crypto.Zero(wrap)

	sealed, err := crypto.Seal(wrap, nil, priv.Bytes(), sealAAD(userID, keyID))
	if err != nil {
		s.log.Error("pgp private sealing failed", "user_id", userID, "err", err)
		return nil, pgpwire.Errorf(pgpwire.CodeInternal, "key sealing failed")
	}

	rec := Record{
		KeyID:       keyID,
		UserID:      userID,
		Fingerprint: fingerprint(entity),
		PublicKey:   pub,
		Sealed:      sealed,
		CreatedAt:   created,
		// ⚠ Minted here, so THIS IS THE ONLY COPY of the private half. A hard
		// delete of this user destroys it permanently and retroactively makes
		// any PGP-encrypted mail they exported unreadable (#54).
		Origin: store.KeyGenerated,
	}
	if err := s.vault.Insert(WithCaller(ctx, userID), rec); err != nil {
		s.log.Error("pgp key store failed", "user_id", userID, "key_id", keyID, "err", err)
		return nil, pgpwire.Errorf(pgpwire.CodeInternal, "key store failed")
	}

	s.log.Info("pgp key generated",
		"user_id", rec.UserID, "key_id", rec.KeyID, "fingerprint", rec.Fingerprint)

	info, err := s.publicInfo(rec)
	if err != nil {
		return nil, err
	}
	return &pgpwire.GenerateResponse{Key: info}, nil
}

// ExportPublic returns the armored PUBLIC key of a stored key the calling user
// owns.
func (s *Service) ExportPublic(ctx context.Context, req pgpwire.ExportPublicRequest) (*pgpwire.ExportPublicResponse, error) {
	// ⚠ userID, not req.UserID, from here down — see validUserID (#301 C1).
	userID, err := validUserID(req.UserID)
	if err != nil {
		return nil, err
	}
	if strings.TrimSpace(req.KeyID) == "" {
		return nil, pgpwire.Errorf(pgpwire.CodeInvalidRequest, "key_id is required")
	}

	rec, err := s.vault.Get(WithCaller(ctx, userID), req.KeyID)
	if errors.Is(err, ErrNoSuchKey) {
		return nil, pgpwire.Errorf(pgpwire.CodeNotFound, "no such key")
	}
	if err != nil {
		s.log.Error("pgp key lookup failed", "key_id", req.KeyID, "err", err)
		return nil, pgpwire.Errorf(pgpwire.CodeInternal, "key lookup failed")
	}

	// ─── THE OWNERSHIP CHECK ────────────────────────────────────────────────
	// Spec §9's confused-deputy requirement, and the line ADR-024 says is most
	// likely to be quietly removed by a later refactor. The caller chose
	// req.KeyID; it may be wrong, stale, or derived from user input. Deleting
	// these three lines must turn TestConfusedDeputyFailsClosed red — that
	// probe is part of the test's contract, not a nicety.
	//
	// ⚠ WHAT THIS DOES NOT DO: req.UserID is ASSERTED by the caller, not
	// authenticated. There is no token on this socket and no SO_PEERCRED check,
	// so this refuses a key that belongs to someone other than the user the
	// caller NAMED — it cannot tell whether the caller was entitled to name
	// them. What constrains who may call at all is the socket: 0600, same uid
	// (see Serve, and deploy/compose.yml).
	//
	// That is deliberate and accepted — see ADR-024 §4, which said the opposite
	// until #275 corrected it. Anything added here must be safe under "the
	// caller names the user".
	//
	// ⚠ BOTH SIDES ARE CANONICAL ULID TEXT, and that is a precondition of the
	// comparison meaning anything (#301 C1). rec.UserID comes back from the
	// store as ulid.ULID.String(); userID is validUserID's parsed form. Compare
	// against req.UserID instead and a caller who merely lower-cased its own id
	// is refused as a confused deputy — a security alarm for a spelling.
	if rec.UserID != userID {
		s.log.Warn("pgp key request refused: wrong owner",
			"caller_user_id", userID, "key_id", req.KeyID, "owner_user_id", rec.UserID)
		return nil, pgpwire.Errorf(pgpwire.CodeDenied, "key belongs to another user")
	}
	// ────────────────────────────────────────────────────────────────────────

	info, err := s.publicInfo(rec)
	if err != nil {
		return nil, err
	}
	return &pgpwire.ExportPublicResponse{Key: info}, nil
}

// Import takes an existing armored PRIVATE key into custody: it is parsed,
// re-sealed under this process's root, and stored. The response carries the
// public half only.
//
// ⚠ req.PrivateKey is the one piece of private key material that arrives from
// outside. Nothing in this function logs req, and the error paths below name
// what went wrong without quoting the input — an error message containing the
// key would be a leak through the most boring possible channel.
func (s *Service) Import(ctx context.Context, req pgpwire.ImportRequest) (*pgpwire.ImportResponse, error) {
	// ⚠ userID, not req.UserID, from here down — see validUserID (#301 C1).
	userID, err := validUserID(req.UserID)
	if err != nil {
		return nil, err
	}
	armored := strings.TrimSpace(req.PrivateKey)
	if armored == "" {
		return nil, pgpwire.Errorf(pgpwire.CodeInvalidRequest, "private_key is required")
	}

	entities, err := openpgp.ReadArmoredKeyRing(strings.NewReader(armored))
	if err != nil {
		// Deliberately not %w and deliberately not echoing the input: the
		// library's error can quote the bytes it choked on.
		return nil, pgpwire.Errorf(pgpwire.CodeInvalidRequest, "not a readable OpenPGP key")
	}
	if len(entities) != 1 {
		return nil, pgpwire.Errorf(pgpwire.CodeInvalidRequest,
			"expected exactly one key, got %d", len(entities))
	}
	entity := entities[0]
	if entity.PrivateKey == nil {
		return nil, pgpwire.Errorf(pgpwire.CodeInvalidRequest,
			"that is a public key; import needs the private key")
	}
	// An encrypted key would store fine and then fail at every signature, at
	// send time, far from here. Refuse it while the caller can still act.
	//
	// With a passphrase (the restore half of pgp-export-wrapped, #626) the
	// secret packets are opened by go-crypto here and the key is then stored
	// exactly like an unprotected import, re-sealed under this process's root.
	// ⚠ A wrong passphrase is ONE opaque error: it must not echo the passphrase
	// or the library's text, and the passphrase is never logged.
	if entity.PrivateKey.Encrypted || anySubkeyEncrypted(entity) {
		if req.Passphrase == "" {
			return nil, pgpwire.Errorf(pgpwire.CodeInvalidRequest,
				"the key is passphrase-protected; supply --passphrase-file or decrypt it before importing")
		}
		if err := entity.DecryptPrivateKeys([]byte(req.Passphrase)); err != nil {
			return nil, pgpwire.Errorf(pgpwire.CodeInvalidRequest, "could not unlock the key")
		}
	}

	var priv bytes.Buffer
	if err := entity.SerializePrivateWithoutSigning(&priv, nil); err != nil {
		s.log.Error("pgp import serialization failed", "user_id", userID)
		return nil, pgpwire.Errorf(pgpwire.CodeInternal, "key serialization failed")
	}
	defer crypto.Zero(priv.Bytes())

	pub, err := SerializePublic(entity)
	if err != nil {
		s.log.Error("pgp import public serialization failed", "user_id", userID)
		return nil, pgpwire.Errorf(pgpwire.CodeInternal, "key serialization failed")
	}

	keyID := ulid.Make().String()
	wrap, err := s.wrappingKey(userID)
	if err != nil {
		s.log.Error("pgp wrapping key derivation failed", "user_id", userID)
		return nil, pgpwire.Errorf(pgpwire.CodeInternal, "key sealing failed")
	}
	defer crypto.Zero(wrap)

	sealed, err := crypto.Seal(wrap, nil, priv.Bytes(), sealAAD(userID, keyID))
	if err != nil {
		s.log.Error("pgp import sealing failed", "user_id", userID)
		return nil, pgpwire.Errorf(pgpwire.CodeInternal, "key sealing failed")
	}

	rec := Record{
		KeyID:       keyID,
		UserID:      userID,
		Fingerprint: fingerprint(entity),
		PublicKey:   pub,
		Sealed:      sealed,
		CreatedAt:   s.now().UTC().Truncate(time.Second),
		// ⚠ The user supplied these bytes, so they still hold them. Losing this
		// copy costs them nothing, which is what makes deleting it safe in a
		// way that deleting a generated key is not (#54).
		Origin: store.KeyImported,
	}
	err = s.vault.Insert(WithCaller(ctx, userID), rec)
	if errors.Is(err, ErrFingerprintTaken) {
		// Not a failure. The user asked for this key to be held and it is.
		s.log.Info("pgp key import: already stored",
			"user_id", userID, "fingerprint", rec.Fingerprint)
		return &pgpwire.ImportResponse{
			Key:           pgpwire.KeyInfo{UserID: rec.UserID, Fingerprint: rec.Fingerprint},
			AlreadyStored: true,
		}, nil
	}
	if err != nil {
		s.log.Error("pgp key store failed", "user_id", userID, "key_id", keyID, "err", err)
		return nil, pgpwire.Errorf(pgpwire.CodeInternal, "key store failed")
	}

	s.log.Info("pgp key imported",
		"user_id", rec.UserID, "key_id", rec.KeyID, "fingerprint", rec.Fingerprint)
	info, err := s.publicInfo(rec)
	if err != nil {
		return nil, err
	}
	return &pgpwire.ImportResponse{Key: info}, nil
}

// BindIdentity makes a stored key the default signing key for one of the
// user's addresses.
//
// The ownership check runs first and on the KEY, because that is the object a
// caller could name wrongly. The identity is protected by the store: the update
// is scoped to the calling user, so a foreign identity id matches no row and
// the binding reports not-found rather than reaching across users.
func (s *Service) BindIdentity(ctx context.Context, req pgpwire.BindIdentityRequest) (*pgpwire.BindIdentityResponse, error) {
	// ⚠ userID/identityID, not the req fields, from here down — see validUserID
	// and canonicalULID (#301 C1).
	userID, err := validUserID(req.UserID)
	if err != nil {
		return nil, err
	}
	if strings.TrimSpace(req.KeyID) == "" {
		return nil, pgpwire.Errorf(pgpwire.CodeInvalidRequest, "key_id is required")
	}
	identityID, ok := canonicalULID(strings.TrimSpace(req.IdentityID))
	if !ok {
		return nil, pgpwire.Errorf(pgpwire.CodeInvalidRequest, "identity_id is not a ULID")
	}

	// THE OWNERSHIP CHECK RUNS BEFORE ANY CAPABILITY CHECK. Ordering matters
	// here for a reason a test caught: asking "can this vault bind?" first made
	// a confused-deputy request fail with "internal" instead of "denied" — the
	// right outcome (nothing was bound) reported as the wrong thing, which is
	// how a boundary comes to look like a bug and gets "fixed".
	rec, err := s.vault.Get(WithCaller(ctx, userID), req.KeyID)
	if errors.Is(err, ErrNoSuchKey) {
		return nil, pgpwire.Errorf(pgpwire.CodeNotFound, "no such key")
	}
	if err != nil {
		s.log.Error("pgp key lookup failed", "key_id", req.KeyID, "err", err)
		return nil, pgpwire.Errorf(pgpwire.CodeInternal, "key lookup failed")
	}
	if rec.UserID != userID {
		s.log.Warn("pgp identity binding refused: wrong owner",
			"caller_user_id", userID, "key_id", req.KeyID, "owner_user_id", rec.UserID)
		return nil, pgpwire.Errorf(pgpwire.CodeDenied, "key belongs to another user")
	}

	binder, canBind := s.vault.(IdentityBinder)
	if !canBind {
		return nil, pgpwire.Errorf(pgpwire.CodeInternal, "this vault cannot bind identities")
	}

	// ⚠ identityID, not req.IdentityID. The old code validated the TRIMMED
	// identity id and then passed the UNTRIMMED one on, so a padded id passed
	// validation and then matched no row — the same "validated one string, used
	// another" shape as C1, one field over.
	bound, err := binder.BindIdentity(WithCaller(ctx, userID), req.KeyID, identityID)
	if err != nil {
		s.log.Error("pgp identity binding failed",
			"user_id", userID, "key_id", req.KeyID, "err", err)
		return nil, pgpwire.Errorf(pgpwire.CodeInternal, "identity binding failed")
	}
	if !bound {
		return nil, pgpwire.Errorf(pgpwire.CodeNotFound, "no such key or identity")
	}

	s.log.Info("pgp key bound to identity",
		"user_id", userID, "key_id", req.KeyID, "identity_id", identityID)
	info, err := s.publicInfo(rec)
	if err != nil {
		return nil, err
	}
	return &pgpwire.BindIdentityResponse{Key: info}, nil
}

// Sign produces a detached signature over req.Data with a stored key the
// calling user owns.
//
// This is the first method that puts a private key to work, so it is also the
// first place where "the application asked for the wrong key" would have a
// consequence that leaves the building: a signature is published to a
// correspondent and cannot be recalled. The ownership check below is therefore
// the SAME three lines as ExportPublic's, deliberately identical so that a
// reader comparing them can see it is one rule and not two similar ones.
func (s *Service) Sign(ctx context.Context, req pgpwire.SignRequest) (*pgpwire.SignResponse, error) {
	// ⚠ userID, not req.UserID, from here down — see validUserID (#301 C1).
	userID, err := validUserID(req.UserID)
	if err != nil {
		return nil, err
	}
	if strings.TrimSpace(req.KeyID) == "" {
		return nil, pgpwire.Errorf(pgpwire.CodeInvalidRequest, "key_id is required")
	}
	// An empty body is almost certainly a caller that lost its message, and a
	// signature over nothing verifies against nothing anyone will ever send.
	if len(req.Data) == 0 {
		return nil, pgpwire.Errorf(pgpwire.CodeInvalidRequest, "data is required")
	}

	rec, err := s.vault.Get(WithCaller(ctx, userID), req.KeyID)
	if errors.Is(err, ErrNoSuchKey) {
		return nil, pgpwire.Errorf(pgpwire.CodeNotFound, "no such key")
	}
	if err != nil {
		s.log.Error("pgp key lookup failed", "key_id", req.KeyID, "err", err)
		return nil, pgpwire.Errorf(pgpwire.CodeInternal, "key lookup failed")
	}

	// ─── THE OWNERSHIP CHECK ────────────────────────────────────────────────
	// Identical to ExportPublic's, and FIRST — before the retirement check and
	// before anything is unsealed. Ordering matters for the reason BindIdentity
	// records: a foreign key refused for some other reason reports the wrong
	// thing, and a boundary that looks like a bug gets "fixed".
	//
	// Both sides are canonical ULID text — see ExportPublic's copy of this
	// check for why that is a precondition rather than a detail (#301 C1).
	if rec.UserID != userID {
		s.log.Warn("pgp signature request refused: wrong owner",
			"caller_user_id", userID, "key_id", req.KeyID, "owner_user_id", rec.UserID)
		return nil, pgpwire.Errorf(pgpwire.CodeDenied, "key belongs to another user")
	}
	// ────────────────────────────────────────────────────────────────────────

	// A retired key keeps working for decryption forever and must never sign
	// again. Invalid-request rather than denied: the key IS the caller's, the
	// operation is not one it may still perform, and the caller can fix that by
	// choosing a current key.
	if rec.Retired() {
		s.log.Warn("pgp signature request refused: retired key",
			"user_id", userID, "key_id", req.KeyID,
			"decrypt_only", rec.DecryptOnly, "revoked", rec.RevokedAt != nil)
		return nil, pgpwire.Errorf(pgpwire.CodeInvalidRequest,
			"that key is retired and may only decrypt")
	}

	entity, err := s.unseal(rec)
	if err != nil {
		return nil, err
	}

	cfg := &packet.Config{
		// SHA-256 is the floor every verifier in use understands, and it is a
		// REQUEST, not a guarantee: go-crypto's selectHashForSigningKey may
		// substitute. Which is exactly why micalg is read back off the signature
		// below instead of assumed from this line.
		//
		// What it substitutes ACCORDING TO was measured rather than assumed
		// (#301 C2). In go-crypto v1.4.1's openpgp package the substitution is
		// driven by acceptableHashesToWrite, a hard-coded table keyed on the
		// public-key algorithm and curve — {SHA256, SHA384, SHA512, SHA3-256,
		// SHA3-512} for everything except Ed448/X448/P-521/BP-512 (SHA-512) and
		// P-384/BP-384 (SHA-384). The key's OWN declared hash preferences are
		// not consulted on this path at all. So today no key, generated or
		// imported, can steer this to a weak hash — the earlier comment here
		// said "the key's own preferences", which was never true of this library
		// version.
		//
		// The refusal after micAlgOf stands anyway, because that is a property
		// of a dependency's internals, checked once, on a signing path whose
		// output is published and cannot be recalled.
		DefaultHash: stdcrypto.SHA256,
		Time:        func() time.Time { return s.now().UTC() },
	}

	var out bytes.Buffer
	aw, err := armor.Encode(&out, openpgp.SignatureType, nil)
	if err != nil {
		s.log.Error("pgp signature armor failed", "user_id", userID, "key_id", req.KeyID, "err", err)
		return nil, pgpwire.Errorf(pgpwire.CodeInternal, "signing failed")
	}
	if err := s.detachSign(aw, entity, bytes.NewReader(req.Data), cfg); err != nil {
		_ = aw.Close()
		// Not %w and not relayed: the library's error can quote key internals.
		s.log.Error("pgp detach sign failed", "user_id", userID, "key_id", req.KeyID, "err", err)
		return nil, pgpwire.Errorf(pgpwire.CodeInternal, "signing failed")
	}
	if err := aw.Close(); err != nil {
		s.log.Error("pgp signature armor close failed", "user_id", userID, "key_id", req.KeyID, "err", err)
		return nil, pgpwire.Errorf(pgpwire.CodeInternal, "signing failed")
	}

	micalg, err := micAlgOf(out.Bytes())
	if err != nil {
		// Refusing beats guessing: a signature shipped with the wrong micalg
		// fails at the RECIPIENT, where nobody here will ever hear about it.
		s.log.Error("pgp signature hash unreadable", "user_id", userID, "key_id", req.KeyID, "err", err)
		return nil, pgpwire.Errorf(pgpwire.CodeInternal, "signing failed")
	}

	// ─── THE WEAK-HASH REFUSAL ──────────────────────────────────────────────
	// #301 C2. The signature exists in this process's memory at this point and
	// is thrown away unreturned, which is the only ordering available: the hash
	// is read off the ARTEFACT, and there is no honest way to know it before
	// there is an artefact. Predicting go-crypto's choice from the key's
	// declared preferences is precisely the "derived from intent instead of
	// from the artefact" mistake micAlgOf exists to avoid — and, per the note
	// on cfg above, that prediction would also have been wrong.
	//
	// Invalid-request, not internal, and deliberately the same code and shape
	// as the retired-key refusal above: nothing malfunctioned, the daemon holds
	// a key it will not sign with, and the caller fixes it by presenting a
	// different key. Internal would read as "retry later", and this never
	// succeeds on retry.
	//
	// Why refuse rather than downgrade-and-warn: a detached signature leaves
	// the building. RFC 3156 requires micalg to name the hash truly, so the
	// daemon's only alternatives are to publish a weak-hash signature under the
	// user's name and label it honestly, or to lie in the micalg and have it
	// fail at a recipient nobody here will hear from. Neither is a send worth
	// completing. ADR-024's no-fallback rule already accepts that a user with a
	// key cannot send while the boundary cannot serve them.
	if weakMicAlgs[micalg] {
		s.log.Warn("pgp signature request refused: the key selected a weak hash",
			"user_id", userID, "key_id", req.KeyID, "fingerprint", rec.Fingerprint,
			"micalg", micalg)
		return nil, pgpwire.Errorf(pgpwire.CodeInvalidRequest,
			"that key signs with %s, which is too weak to publish under your name", micalg)
	}
	// ────────────────────────────────────────────────────────────────────────

	s.log.Info("pgp signature made",
		"user_id", userID, "key_id", req.KeyID, "fingerprint", rec.Fingerprint,
		"micalg", micalg, "data_bytes", len(req.Data))

	return &pgpwire.SignResponse{
		Signature:   out.String(),
		Fingerprint: rec.Fingerprint,
		MicAlg:      micalg,
	}, nil
}

// unseal opens a stored key and returns the usable entity.
//
// ⚠ The zeroization here covers the SERIALIZED private key only — the buffer
// this function decrypted and parsed. It does not, and cannot, cover the entity:
// go-crypto keeps the scalar in its own structures with no way to wipe it, and
// the Go heap may have copied either. What zeroizing buys is that the long,
// recognisable, greppable form of the key does not sit in this process's memory
// for the rest of its life. That is a smaller claim than "the key is gone", and
// it is the true one.
func (s *Service) unseal(rec Record) (*openpgp.Entity, error) {
	wrap, err := s.wrappingKey(rec.UserID)
	if err != nil {
		s.log.Error("pgp wrapping key derivation failed", "user_id", rec.UserID)
		return nil, pgpwire.Errorf(pgpwire.CodeInternal, "key unsealing failed")
	}
	defer crypto.Zero(wrap)

	priv, err := crypto.Open(wrap, rec.Sealed, sealAAD(rec.UserID, rec.KeyID))
	if err != nil {
		// The AAD binds owner and key id, so this also fires if the ciphertext
		// was relocated between rows — a store-tampering signal, not a bad key.
		s.log.Error("pgp key unseal failed", "user_id", rec.UserID, "key_id", rec.KeyID)
		return nil, pgpwire.Errorf(pgpwire.CodeInternal, "key unsealing failed")
	}
	defer crypto.Zero(priv)

	entities, err := openpgp.ReadKeyRing(bytes.NewReader(priv))
	if err != nil || len(entities) != 1 || entities[0].PrivateKey == nil {
		s.log.Error("pgp stored key unreadable", "user_id", rec.UserID, "key_id", rec.KeyID)
		return nil, pgpwire.Errorf(pgpwire.CodeInternal, "stored key is unreadable")
	}
	if entities[0].PrivateKey.Encrypted {
		// Import refuses these and Generate never makes them, so this means the
		// stored bytes are not what this process wrote.
		s.log.Error("pgp stored key is passphrase-protected", "user_id", rec.UserID, "key_id", rec.KeyID)
		return nil, pgpwire.Errorf(pgpwire.CodeInternal, "stored key is unusable")
	}
	return entities[0], nil
}

// micAlgOf reads the hash algorithm out of an armored detached signature and
// renders it as RFC 3156's micalg parameter value.
//
// It parses the signature rather than reporting what was requested, because
// go-crypto's selectHashForSigningKey may substitute a different hash when the
// key's declared preferences do not accept the configured one. A micalg derived
// from intent instead of from the artefact is right until the day it silently
// is not.
func micAlgOf(armored []byte) (string, error) {
	blk, err := armor.Decode(bytes.NewReader(armored))
	if err != nil {
		return "", fmt.Errorf("pgpkeyd: dearmor signature: %w", err)
	}
	pkts := packet.NewReader(blk.Body)
	for {
		p, err := pkts.Next()
		if errors.Is(err, io.EOF) {
			return "", errors.New("pgpkeyd: no signature packet")
		}
		if err != nil {
			return "", fmt.Errorf("pgpkeyd: read signature packet: %w", err)
		}
		sig, ok := p.(*packet.Signature)
		if !ok {
			continue
		}
		name, ok := micAlgNames[sig.Hash]
		if !ok {
			return "", fmt.Errorf("pgpkeyd: no micalg name for hash %v", sig.Hash)
		}
		return name, nil
	}
}

// micAlgNames maps a hash to RFC 3156 §5's micalg value.
//
// RFC 3156 predates SHA-2 and only enumerates values through ripemd160; the
// "pgp-" + lowercased RFC 9580 hash name convention below is what GnuPG and
// every mail client that reads it actually use. SHA-3 is listed for the same
// reason but has no deployed convention at all — if a key ever selects it, the
// entry here is the guess to revisit.
//
// ⚠ THE WEAK HASHES ARE STILL NAMED HERE ON PURPOSE, even though Sign refuses
// them (#301 C2). SHA-1 is the case that matters: go-crypto parses hash id 2
// on v4 signatures, so a SHA-1 signature reaches micAlgOf intact, and deleting
// the entry would turn a named refusal into "no micalg name for hash 3" — which
// Sign classifies as internal, so a weak-hash key would read as a broken daemon
// rather than as the specific thing it is. Name it, then refuse it by name.
//
// MD5 and RIPEMD160 are named for completeness and are unreachable in this
// library version: algorithm.HashById omits them, so Signature.parse rejects
// hash ids 1 and 3 before micAlgOf ever sees them (measured, see
// weakhash_test.go's header). That door is also closed — it just closes as an
// internal error rather than as a refusal.
var micAlgNames = map[stdcrypto.Hash]string{
	stdcrypto.MD5:       "pgp-md5",
	stdcrypto.SHA1:      "pgp-sha1",
	stdcrypto.RIPEMD160: "pgp-ripemd160",
	stdcrypto.SHA224:    "pgp-sha224",
	stdcrypto.SHA256:    "pgp-sha256",
	stdcrypto.SHA384:    "pgp-sha384",
	stdcrypto.SHA512:    "pgp-sha512",
	stdcrypto.SHA3_256:  "pgp-sha3-256",
	stdcrypto.SHA3_512:  "pgp-sha3-512",
}

// weakMicAlgs are the hashes this daemon will not publish a signature under
// (#301 C2). Keyed by micalg name rather than by stdcrypto.Hash so that the set
// is read off the SAME value the response would have carried — one string
// compared against one table, with nothing in between to disagree.
//
// MD5 and SHA-1 have practical collisions; RIPEMD160 is a 160-bit digest with
// no meaningful review and no deployed reason to prefer it. SHA-224 is NOT here
// deliberately: 112-bit collision resistance is above the line, it is a member
// of the SHA-2 family, and refusing it would be a judgement about strength
// rather than about brokenness. This table is for the latter.
var weakMicAlgs = map[string]bool{
	"pgp-md5":       true,
	"pgp-sha1":      true,
	"pgp-ripemd160": true,
}

// publicInfo projects a Record onto the wire. It exists so that the mapping
// from "what is stored" to "what may leave" is written once, in one place, and
// visibly omits Sealed.
//
// ⚠ THE COLUMN IS BINARY AND THE WIRE IS ARMORED, and this function is the
// only place the two representations meet.
//
// The column is binary because app.user_keys.public_key is served VERBATIM to
// anonymous WKD callers (store.LookupWKDKey → internal/keypub), and
// draft-koch-openpgp-webkey-service requires raw OpenPGP packets. Storing armor
// makes every published key unservable — and, because unpublished addresses 404
// while published ones then fail, turns the endpoint into an address oracle.
//
// The wire is armored because pgpwire.KeyInfo.PublicKey is a JSON string a
// person reads: `mailroom-admin pgp-generate --print-public` prints it and the
// operator pastes it into mail, a keyserver, or a colleague's chat. Binary in a
// JSON string would be neither.
func (s *Service) publicInfo(rec Record) (pgpwire.KeyInfo, error) {
	armored, err := ArmorPublicKey(rec.PublicKey)
	if err != nil {
		s.log.Error("pgp public armor failed",
			"user_id", rec.UserID, "key_id", rec.KeyID, "err", err)
		return pgpwire.KeyInfo{}, pgpwire.Errorf(pgpwire.CodeInternal, "key serialization failed")
	}
	info := pgpwire.KeyInfo{
		KeyID:       rec.KeyID,
		UserID:      rec.UserID,
		Fingerprint: rec.Fingerprint,
		PublicKey:   armored,
		CreatedAt:   rec.CreatedAt,
	}
	// Read off the stored public key, never computed from a request: what the
	// operator is told is what correspondents will see.
	if v, err := pgpkeys.Assess(rec.PublicKey, s.now()); err == nil {
		if until, never := v.UsableUntil(); !never {
			u := until.UTC()
			info.UsableUntil = &u
		}
	}
	return info, nil
}

// validUserID checks the caller's user id and returns it in CANONICAL form.
//
// ⚠ RETURNING THE PARSED FORM IS THE WHOLE POINT, not a convenience (#301 C1).
// Every caller must use the returned string and never req.UserID again.
//
// A ULID's text encoding is Crockford base32, which FOLDS: it is case
// insensitive and reads `i`/`l` as `1` and `o` as `0`. So ulid.Parse accepts
// several distinct strings for one id, and this function used to hand the
// verbatim one back to the handlers. That string then fed two things that must
// agree with what the STORE will later report:
//
//	WrappingKey(root, userID)  — the HKDF salt
//	SealAAD(userID, keyID)     — the sealed key's associated data
//
// while StoreVault reads a record back as k.UserID.String(), which is always
// canonical. Generate with a lowercase id and the key was sealed under one
// string and stored under another, so NO LATER REQUEST COULD EVER USE IT:
//
//   - sign with the same lowercase string → rec.UserID != req.UserID → denied,
//     which cmd/mailroom/signer.go logs as "CONFUSED DEPUTY … a bug in Mailroom"
//   - sign with the canonical string → ownership passes, unseal derives from
//     rec.UserID, crypto.Open fails → internal, logged as store tampering
//
// Both alarms fire for something that is not a security event, and the key is
// bricked either way. Canonicalising here is what makes "the id the daemon
// sealed under" and "the id the database will report" the same string by
// construction rather than by every caller remembering.
//
// This is fail-closed either way — a case variant derives a DIFFERENT wrapping
// key, never another user's — so the bug was availability, not isolation. That
// is exactly why it is worth fixing loudly: an unreachable key that reports
// itself as a confused deputy is how a real confused deputy comes to be ignored.
//
// ⚠ The input is deliberately NOT trimmed before parsing. Whitespace around an
// id was rejected before this change and stays rejected; widening what the
// daemon accepts is not what canonicalisation means.
func validUserID(id string) (string, error) {
	if strings.TrimSpace(id) == "" {
		return "", pgpwire.Errorf(pgpwire.CodeInvalidRequest, "user_id is required")
	}
	canon, ok := canonicalULID(id)
	if !ok {
		return "", pgpwire.Errorf(pgpwire.CodeInvalidRequest, "user_id is not a ULID")
	}
	return canon, nil
}

// canonicalULID parses a ULID a request supplied and returns its canonical
// text — the same form ulid.ULID.String() produces everywhere else in Mailroom,
// and therefore the same form the store will report a row back as.
//
// It exists so that "validated" and "used" are the same string for every id
// that crosses this boundary. Validating one representation and then using
// another is the shape of #301 C1, and it is the kind of bug that reappears
// each time a new request field carries an id.
//
// ⚠ NOT applied to key ids, and that omission is deliberate. A key id is never
// used to derive or bind anything: the AAD's key-id half always comes from a
// canonical source (Generate mints it with ulid.Make; unseal reads it back from
// the record). It is only ever a lookup argument, and StoreVault documents why
// a key id it cannot parse must read as ABSENT rather than as a rejected
// request — routing key ids through here would turn that documented not-found
// into an invalid-request and change the wire contract for no gain.
func canonicalULID(raw string) (string, bool) {
	id, err := ulid.Parse(raw)
	if err != nil {
		return "", false
	}
	return id.String(), true
}

func fingerprint(e *openpgp.Entity) string {
	return strings.ToUpper(hex.EncodeToString(e.PrimaryKey.Fingerprint))
}

// SerializePublic writes the entity's PUBLIC packets, in BINARY. This is what
// goes in app.user_keys.public_key, because that column is what the anonymous
// WKD endpoint serves byte for byte (see publicInfo).
//
// Entity.Serialize is the public-only serializer — SerializePrivate is its
// sibling, and is called nowhere outside Generate's and Import's sealing steps.
//
// Exported for the ONE other caller allowed to write a key row: the restore
// drill (cmd/mailroom-drill), which mints a real fixture key for `seed` and must
// produce the same bytes the crypto process would have — not a second,
// driftable copy of the encoding. A drill fixture stored in the other
// representation would seed a row that WKD cannot serve, which is precisely the
// failure the drill exists to catch rather than commit.
func SerializePublic(e *openpgp.Entity) ([]byte, error) {
	var buf bytes.Buffer
	if err := e.Serialize(&buf); err != nil {
		return nil, err
	}
	return buf.Bytes(), nil
}

// ArmorPublicKey wraps already-serialized public packets in ASCII armor, for
// the boundaries that want text: pgpwire.KeyInfo.PublicKey, and therefore the
// CLI output a human copies and pastes.
//
// It takes BYTES rather than an entity so that what is armored is exactly what
// was stored. Armoring a re-derived entity would let the two drift — the stored
// bytes and the published text could describe different key material with
// nothing to notice it.
func ArmorPublicKey(pkts []byte) (string, error) {
	var buf bytes.Buffer
	w, err := armor.Encode(&buf, openpgp.PublicKeyType, nil)
	if err != nil {
		return "", err
	}
	if _, err := w.Write(pkts); err != nil {
		_ = w.Close()
		return "", err
	}
	if err := w.Close(); err != nil {
		return "", err
	}
	return buf.String(), nil
}

// ExportWrapped returns the caller's private key encrypted under a passphrase
// THEY chose (spec §9, ADR-024 §4 as amended, #54).
//
// ⚠ THIS IS THE ONLY METHOD WHOSE RESPONSE CARRIES PRIVATE KEY MATERIAL, and
// what makes that acceptable is precisely what it does NOT do: the key is
// re-encrypted under a caller-supplied passphrase before it leaves, and this
// process neither stores that passphrase nor can re-derive it. Mailroom's own
// root does not open the result. Nobody here can.
//
// The alternative — wrapping under the process's own secret — would satisfy spec
// §9's wording and deliver zero portability: an exported file that only this
// deployment could read, which is the outcome the whole operation exists to
// avoid.
//
// ⚠ THE KEY IS NOT RETIRED-CHECKED, unlike Sign. A retired key is exactly the
// one a user most needs out: it decrypts their old mail and can never sign
// again, so refusing to export it would strand the archive it exists to open.
func (s *Service) ExportWrapped(ctx context.Context, req pgpwire.ExportWrappedRequest) (*pgpwire.ExportWrappedResponse, error) {
	// ⚠ userID, not req.UserID, from here down — see validUserID (#301 C1).
	userID, err := validUserID(req.UserID)
	if err != nil {
		return nil, err
	}
	if strings.TrimSpace(req.KeyID) == "" {
		return nil, pgpwire.Errorf(pgpwire.CodeInvalidRequest, "key_id is required")
	}
	// ⚠ Length on the RAW passphrase, not a trimmed copy: leading and trailing
	// spaces are legitimate passphrase characters, and silently trimming them
	// would produce a file whose passphrase is not the one the user typed.
	if len(req.Passphrase) < pgpwire.MinExportPassphrase {
		return nil, pgpwire.Errorf(pgpwire.CodeInvalidRequest,
			"passphrase must be at least %d characters: it is the only thing protecting "+
				"the exported key, and this process cannot help you open it later",
			pgpwire.MinExportPassphrase)
	}

	rec, err := s.vault.Get(WithCaller(ctx, userID), req.KeyID)
	if errors.Is(err, ErrNoSuchKey) {
		return nil, pgpwire.Errorf(pgpwire.CodeNotFound, "no such key")
	}
	if err != nil {
		s.log.Error("pgp key lookup failed", "key_id", req.KeyID, "err", err)
		return nil, pgpwire.Errorf(pgpwire.CodeInternal, "key lookup failed")
	}

	// ─── THE OWNERSHIP CHECK ────────────────────────────────────────────────
	// Same shape and same position as Sign's and ExportPublic's: FIRST, before
	// anything is unsealed. It matters more here than anywhere else in this
	// file — every other method hands back a derived artefact, and this one
	// hands back the key.
	if rec.UserID != userID {
		s.log.Warn("pgp wrapped export refused: wrong owner",
			"caller_user_id", userID, "key_id", req.KeyID, "owner_user_id", rec.UserID)
		return nil, pgpwire.Errorf(pgpwire.CodeDenied, "key belongs to another user")
	}
	// ────────────────────────────────────────────────────────────────────────

	entity, err := s.unseal(rec)
	if err != nil {
		return nil, err
	}

	// Re-encrypt every secret packet under the user's passphrase. After this the
	// in-memory entity's private keys are encrypted too, which is why nothing
	// below may assume it can still sign.
	if err := entity.EncryptPrivateKeys([]byte(req.Passphrase), nil); err != nil {
		s.log.Error("pgp wrapped export: encrypt under passphrase failed",
			"user_id", userID, "key_id", req.KeyID)
		return nil, pgpwire.Errorf(pgpwire.CodeInternal, "key wrapping failed")
	}

	var buf bytes.Buffer
	aw, err := armor.Encode(&buf, openpgp.PrivateKeyType, nil)
	if err != nil {
		s.log.Error("pgp wrapped export: armor failed", "user_id", userID, "key_id", req.KeyID)
		return nil, pgpwire.Errorf(pgpwire.CodeInternal, "key wrapping failed")
	}
	// SerializePrivateWithoutSigning, not SerializePrivate: the latter re-signs
	// the identities, which needs the secret material it has just encrypted.
	if err := entity.SerializePrivateWithoutSigning(aw, nil); err != nil {
		s.log.Error("pgp wrapped export: serialize failed", "user_id", userID, "key_id", req.KeyID)
		return nil, pgpwire.Errorf(pgpwire.CodeInternal, "key wrapping failed")
	}
	if err := aw.Close(); err != nil {
		s.log.Error("pgp wrapped export: armor close failed", "user_id", userID, "key_id", req.KeyID)
		return nil, pgpwire.Errorf(pgpwire.CodeInternal, "key wrapping failed")
	}

	// ⚠ THE ARTEFACT IS CHECKED, NOT ASSUMED. A serialization that silently
	// emitted unencrypted secret packets would be the worst possible bug in this
	// file, and it would look exactly like success. Re-read what is about to be
	// returned and refuse it unless every private key in it is encrypted.
	if err := assertWrapped(buf.Bytes()); err != nil {
		s.log.Error("pgp wrapped export: refusing an unprotected artefact",
			"user_id", userID, "key_id", req.KeyID, "err", err)
		return nil, pgpwire.Errorf(pgpwire.CodeInternal, "key wrapping failed")
	}

	s.log.Info("pgp key exported wrapped",
		"user_id", userID, "key_id", req.KeyID, "fingerprint", rec.Fingerprint)

	return &pgpwire.ExportWrappedResponse{
		Key:        pgpwire.KeyInfo{UserID: rec.UserID, KeyID: rec.KeyID, Fingerprint: rec.Fingerprint},
		PrivateKey: buf.String(),
	}, nil
}

// assertWrapped re-reads an armored key and requires every secret packet in it
// to be passphrase-encrypted.
//
// It exists because "we called EncryptPrivateKeys" is a claim about a library,
// and the cost of that claim being wrong is the raw key on someone's disk. This
// checks the bytes that are actually leaving.
func assertWrapped(armored []byte) error {
	blk, err := armor.Decode(bytes.NewReader(armored))
	if err != nil {
		return fmt.Errorf("dearmor: %w", err)
	}
	entities, err := openpgp.ReadKeyRing(blk.Body)
	if err != nil {
		return fmt.Errorf("reread: %w", err)
	}
	if len(entities) != 1 {
		return fmt.Errorf("expected one entity, got %d", len(entities))
	}
	e := entities[0]
	if e.PrivateKey == nil {
		return errors.New("no private key in the serialized output")
	}
	if !e.PrivateKey.Encrypted {
		return errors.New("the primary private key is NOT encrypted")
	}
	for i, sub := range e.Subkeys {
		if sub.PrivateKey == nil {
			continue
		}
		if !sub.PrivateKey.Encrypted {
			return fmt.Errorf("subkey %d's private key is NOT encrypted", i)
		}
	}
	return nil
}

// anySubkeyEncrypted reports whether a subkey still has encrypted secret
// packets, so a key with an unprotected primary but protected subkey is not
// stored in a half-usable state.
func anySubkeyEncrypted(e *openpgp.Entity) bool {
	for _, sk := range e.Subkeys {
		if sk.PrivateKey != nil && sk.PrivateKey.Encrypted {
			return true
		}
	}
	return false
}
