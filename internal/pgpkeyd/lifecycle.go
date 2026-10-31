// SPDX-License-Identifier: AGPL-3.0-only

package pgpkeyd

// Extend and revoke: the two key-lifecycle operations that need the private half
// (#795, ADR-040). Retire needs none and lives in the store alone.

import (
	"bytes"
	"context"
	stdcrypto "crypto"
	"errors"
	"fmt"
	"math"
	"strings"
	"time"

	"github.com/ProtonMail/go-crypto/openpgp"
	"github.com/ProtonMail/go-crypto/openpgp/packet"

	"mailroom/internal/crypto"
	"mailroom/internal/pgpkeys"
	"mailroom/internal/pgpwire"
)

// KeyUpdater is implemented by vaults that can store a re-signed or revoked
// copy of an existing key. Like IdentityBinder it is separate from Vault: it is
// a write to an existing row, with its own change_log rows, and only the two
// lifecycle verbs need it.
type KeyUpdater interface {
	// UpdateKey stores rec.PublicKey and rec.Sealed for rec.KeyID, whose
	// fingerprint must equal rec.Fingerprint. With revokedAt set it also marks
	// the key revoked and decrypt-only. found is false when no such unrevoked
	// key exists.
	UpdateKey(ctx context.Context, rec Record, revokedAt *time.Time) (found bool, err error)
}

var errKeyRevoked = errors.New("pgpkeyd: the key is revoked")

// lifecycleTarget resolves and ownership-checks a key for extend or revoke, in
// the same order and with the same three-line check as Sign: the key is found,
// then it must be the caller's, before anything else is asked of it.
func (s *Service) lifecycleTarget(ctx context.Context, op, rawUserID, keyID string) (string, Record, KeyUpdater, error) {
	userID, err := validUserID(rawUserID)
	if err != nil {
		return "", Record{}, nil, err
	}
	if strings.TrimSpace(keyID) == "" {
		return "", Record{}, nil, pgpwire.Errorf(pgpwire.CodeInvalidRequest, "key_id is required")
	}
	rec, err := s.vault.Get(WithCaller(ctx, userID), keyID)
	if errors.Is(err, ErrNoSuchKey) {
		return "", Record{}, nil, pgpwire.Errorf(pgpwire.CodeNotFound, "no such key")
	}
	if err != nil {
		s.log.Error("pgp key lookup failed", "op", op, "key_id", keyID, "err", err)
		return "", Record{}, nil, pgpwire.Errorf(pgpwire.CodeInternal, "key lookup failed")
	}
	// ─── THE OWNERSHIP CHECK ──── same three lines as ExportPublic and Sign.
	if rec.UserID != userID {
		s.log.Warn("pgp "+op+" refused: wrong owner",
			"caller_user_id", userID, "key_id", keyID, "owner_user_id", rec.UserID)
		return "", Record{}, nil, pgpwire.Errorf(pgpwire.CodeDenied, "key belongs to another user")
	}
	updater, ok := s.vault.(KeyUpdater)
	if !ok {
		return "", Record{}, nil, pgpwire.Errorf(pgpwire.CodeInternal, "this vault cannot update keys")
	}
	return userID, rec, updater, nil
}

// Extend moves a stored key's expiry to now + req.Days, by re-signing its
// self-signatures (#795). The fingerprint does not change.
func (s *Service) Extend(ctx context.Context, req pgpwire.ExtendRequest) (*pgpwire.ExtendResponse, error) {
	switch {
	case (req.Days != 0) == (req.ExpiresAt != nil):
		return nil, pgpwire.Errorf(pgpwire.CodeInvalidRequest, "give exactly one of days and expires_at")
	case req.ExpiresAt == nil && (req.Days < 0 || req.Days > pgpwire.MaxKeyLifetimeDays):
		return nil, pgpwire.Errorf(pgpwire.CodeInvalidRequest,
			"days must be between 1 and %d", pgpwire.MaxKeyLifetimeDays)
	}
	userID, rec, updater, err := s.lifecycleTarget(ctx, "extend", req.UserID, req.KeyID)
	if err != nil {
		return nil, err
	}
	if rec.RevokedAt != nil {
		return nil, pgpwire.Errorf(pgpwire.CodeInvalidRequest,
			"that key is revoked; a revoked key cannot be extended — generate a new one")
	}

	entity, err := s.unseal(rec)
	if err != nil {
		return nil, err
	}
	now := s.now().UTC().Truncate(time.Second)
	until := now.Add(time.Duration(req.Days) * 24 * time.Hour)
	if req.ExpiresAt != nil {
		if until, err = exactExpiry(*req.ExpiresAt, now); err != nil {
			return nil, err
		}
	}
	if err := extendEntity(entity, until, now); err != nil {
		if errors.Is(err, errKeyRevoked) {
			return nil, pgpwire.Errorf(pgpwire.CodeInvalidRequest,
				"that key carries a revocation; a revoked key cannot be extended")
		}
		// Not relayed: go-crypto's text can name key internals.
		s.log.Error("pgp extend: re-signing failed", "user_id", userID, "key_id", rec.KeyID, "err", err)
		return nil, pgpwire.Errorf(pgpwire.CodeInternal, "re-signing failed")
	}

	updated, err := s.reseal(rec, entity)
	if err != nil {
		return nil, err
	}
	// ⚠ THE ARTEFACT IS CHECKED, NOT ASSUMED: same fingerprint, and usable until
	// the moment asked for. A re-sign that silently left the encryption subkey on
	// its old expiry would report success and still stop working on the old date.
	if err := checkExtended(updated, rec.Fingerprint, until, now); err != nil {
		s.log.Error("pgp extend: refusing an artefact that does not say what was asked",
			"user_id", userID, "key_id", rec.KeyID, "err", err)
		return nil, pgpwire.Errorf(pgpwire.CodeInternal, "re-signing failed")
	}

	found, err := updater.UpdateKey(WithCaller(ctx, userID), updated, nil)
	if err != nil {
		s.log.Error("pgp extend: store failed", "user_id", userID, "key_id", rec.KeyID, "err", err)
		return nil, pgpwire.Errorf(pgpwire.CodeInternal, "key store failed")
	}
	if !found {
		// Revoked (or removed) between the read and the write.
		return nil, pgpwire.Errorf(pgpwire.CodeInvalidRequest, "that key is no longer extendable")
	}
	s.log.Info("pgp key extended", "user_id", userID, "key_id", rec.KeyID,
		"fingerprint", rec.Fingerprint, "until", until)
	info, err := s.publicInfo(updated)
	if err != nil {
		return nil, err
	}
	return &pgpwire.ExtendResponse{Key: info}, nil
}

// revocationReasons maps the wire's reason names to RFC 9580 reason codes.
var revocationReasons = map[string]packet.ReasonForRevocation{
	pgpwire.RevokeCompromised: packet.KeyCompromised,
	pgpwire.RevokeSuperseded:  packet.KeySuperseded,
	pgpwire.RevokeRetired:     packet.KeyRetired,
}

// Revoke adds a key revocation signature to a stored key and marks it revoked
// and decrypt-only (#795). It still decrypts afterwards, forever (ADR-037): a
// revocation stops NEW mail being encrypted to the key, not old mail being read.
func (s *Service) Revoke(ctx context.Context, req pgpwire.RevokeRequest) (*pgpwire.RevokeResponse, error) {
	reason, ok := revocationReasons[req.Reason]
	if !ok {
		return nil, pgpwire.Errorf(pgpwire.CodeInvalidRequest,
			"reason must be one of %s, %s, %s",
			pgpwire.RevokeCompromised, pgpwire.RevokeSuperseded, pgpwire.RevokeRetired)
	}
	userID, rec, updater, err := s.lifecycleTarget(ctx, "revoke", req.UserID, req.KeyID)
	if err != nil {
		return nil, err
	}
	// The confirmation is checked against the key THIS PROCESS resolved, after
	// the ownership check: the caller names a key id and proves it means the key
	// that id resolves to. Naming the expected suffix in the refusal is safe —
	// the fingerprint is public — and is what lets the operator see the mix-up.
	if !confirmMatches(rec.Fingerprint, req.Confirm) {
		return nil, pgpwire.Errorf(pgpwire.CodeInvalidRequest,
			"confirmation does not match: key %s has fingerprint ending %s",
			rec.KeyID, fingerprintSuffix(rec.Fingerprint))
	}
	if rec.RevokedAt != nil {
		return nil, pgpwire.Errorf(pgpwire.CodeInvalidRequest, "that key is already revoked")
	}

	entity, err := s.unseal(rec)
	if err != nil {
		return nil, err
	}
	now := s.now().UTC().Truncate(time.Second)
	cfg := &packet.Config{DefaultHash: stdcrypto.SHA256, Time: func() time.Time { return now }}
	if err := entity.RevokeKey(reason, "", cfg); err != nil {
		s.log.Error("pgp revoke: signing the revocation failed", "user_id", userID, "key_id", rec.KeyID, "err", err)
		return nil, pgpwire.Errorf(pgpwire.CodeInternal, "revocation failed")
	}
	updated, err := s.reseal(rec, entity)
	if err != nil {
		return nil, err
	}
	// Artefact check: the stored public key must READ as revoked, by the same
	// code every consumer in this codebase uses.
	v, err := pgpkeys.Assess(updated.PublicKey, now)
	if err != nil || !v.Revoked {
		s.log.Error("pgp revoke: refusing an artefact that does not read as revoked",
			"user_id", userID, "key_id", rec.KeyID, "err", err)
		return nil, pgpwire.Errorf(pgpwire.CodeInternal, "revocation failed")
	}

	found, err := updater.UpdateKey(WithCaller(ctx, userID), updated, &now)
	if err != nil {
		s.log.Error("pgp revoke: store failed", "user_id", userID, "key_id", rec.KeyID, "err", err)
		return nil, pgpwire.Errorf(pgpwire.CodeInternal, "key store failed")
	}
	if !found {
		return nil, pgpwire.Errorf(pgpwire.CodeInvalidRequest, "that key is already revoked")
	}
	s.log.Info("pgp key revoked", "user_id", userID, "key_id", rec.KeyID,
		"fingerprint", rec.Fingerprint, "reason", req.Reason)
	updated.RevokedAt = &now
	updated.DecryptOnly = true
	info, err := s.publicInfo(updated)
	if err != nil {
		return nil, err
	}
	return &pgpwire.RevokeResponse{Key: info, RevokedAt: now}, nil
}

// reseal serializes a modified entity back into a Record for the same key id:
// the public half in binary, and the private half re-sealed under the SAME AAD
// (owner, key id). The self-signatures live in the private serialization too;
// leaving the old ones there would let the signer's copy expire on the old date
// while the published copy said otherwise.
func (s *Service) reseal(rec Record, entity *openpgp.Entity) (Record, error) {
	var priv bytes.Buffer
	if err := entity.SerializePrivateWithoutSigning(&priv, nil); err != nil {
		s.log.Error("pgp reseal: private serialization failed", "user_id", rec.UserID, "key_id", rec.KeyID)
		return Record{}, pgpwire.Errorf(pgpwire.CodeInternal, "key serialization failed")
	}
	defer crypto.Zero(priv.Bytes())
	pub, err := SerializePublic(entity)
	if err != nil {
		s.log.Error("pgp reseal: public serialization failed", "user_id", rec.UserID, "key_id", rec.KeyID)
		return Record{}, pgpwire.Errorf(pgpwire.CodeInternal, "key serialization failed")
	}
	if fingerprint(entity) != rec.Fingerprint {
		s.log.Error("pgp reseal: fingerprint changed", "user_id", rec.UserID, "key_id", rec.KeyID)
		return Record{}, pgpwire.Errorf(pgpwire.CodeInternal, "key serialization failed")
	}
	wrap, err := s.wrappingKey(rec.UserID)
	if err != nil {
		s.log.Error("pgp wrapping key derivation failed", "user_id", rec.UserID)
		return Record{}, pgpwire.Errorf(pgpwire.CodeInternal, "key sealing failed")
	}
	defer crypto.Zero(wrap)
	sealed, err := crypto.Seal(wrap, nil, priv.Bytes(), sealAAD(rec.UserID, rec.KeyID))
	if err != nil {
		s.log.Error("pgp reseal: sealing failed", "user_id", rec.UserID, "key_id", rec.KeyID)
		return Record{}, pgpwire.Errorf(pgpwire.CodeInternal, "key sealing failed")
	}
	out := rec
	out.PublicKey = pub
	out.Sealed = sealed
	return out, nil
}

// extendEntity gives e new self-signatures, made at now, under which the
// primary key and every live subkey expire at until. Each new signature copies
// the one it supersedes (flags, preferences, features) and changes only its
// creation time and key lifetime, so the key keeps meaning what it meant.
//
// It works on a key that never expired (it then sets an expiry) and refuses one
// that carries a revocation. Revoked user IDs and revoked subkeys are left
// alone: re-signing them would not un-revoke them, and would be noise.
func extendEntity(e *openpgp.Entity, until, now time.Time) error {
	if e.Revoked(now) {
		return errKeyRevoked
	}
	cfg := &packet.Config{DefaultHash: stdcrypto.SHA256, Time: func() time.Time { return now }}

	lifetime := func(created time.Time) (uint32, error) {
		d := until.Sub(created)
		if d <= 0 || d/time.Second > math.MaxUint32 {
			return 0, errors.New("pgpkeyd: requested expiry is out of range for this key")
		}
		return uint32(d / time.Second), nil
	}
	renew := func(old *packet.Signature, created time.Time) (*packet.Signature, error) {
		l, err := lifetime(created)
		if err != nil {
			return nil, err
		}
		cp := *old
		cp.Notations = append([]*packet.Notation(nil), old.Notations...)
		// STRICTLY LATER than the signature it supersedes. Every reader picks
		// the newest self-signature by creation time, and a tie goes to
		// whichever copy it read first — so a renewal made in the same second
		// as the key (or as a previous renewal) would lose to the old
		// signature in a correspondent's merged copy.
		cp.CreationTime = now
		if !now.After(old.CreationTime) {
			cp.CreationTime = old.CreationTime.Add(time.Second)
		}
		cp.KeyLifetimeSecs = &l
		cp.SigLifetimeSecs = nil // a self-signature that itself expires would undo the extension
		if weakHashes[cp.Hash] {
			cp.Hash = stdcrypto.SHA256
		}
		return &cp, nil
	}

	renewed := 0
	for name, ident := range e.Identities {
		if ident.SelfSignature == nil || ident.Revoked(now) {
			continue
		}
		sig, err := renew(ident.SelfSignature, e.PrimaryKey.CreationTime)
		if err != nil {
			return err
		}
		if err := sig.SignUserId(name, e.PrimaryKey, e.PrivateKey, cfg); err != nil {
			return err
		}
		ident.Signatures = replaceSig(ident.Signatures, ident.SelfSignature, sig)
		ident.SelfSignature = sig
		renewed++
	}
	// v6 keys carry their expiry on the direct-key self-signature.
	if e.PrimaryKey.Version == 6 && e.SelfSignature != nil {
		sig, err := renew(e.SelfSignature, e.PrimaryKey.CreationTime)
		if err != nil {
			return err
		}
		if err := sig.SignDirectKeyBinding(e.PrimaryKey, e.PrivateKey, cfg); err != nil {
			return err
		}
		e.Signatures = replaceSig(e.Signatures, e.SelfSignature, sig)
		e.SelfSignature = sig
		renewed++
	}
	if renewed == 0 {
		return errors.New("pgpkeyd: no live self-signature to renew")
	}

	for i := range e.Subkeys {
		sub := &e.Subkeys[i]
		if sub.Sig == nil || sub.Revoked(now) {
			continue
		}
		sig, err := renew(sub.Sig, sub.PublicKey.CreationTime)
		if err != nil {
			return err
		}
		// A signing-capable subkey's binding carries an embedded back-signature
		// made BY THE SUBKEY; a new binding needs a new one, or the subkey stops
		// verifying. A subkey whose private half is absent cannot make one, so
		// it keeps its old binding rather than being given a broken new one.
		if sig.FlagSign {
			if sub.PrivateKey == nil || sub.PrivateKey.Dummy() || sub.Sig.EmbeddedSignature == nil {
				continue
			}
			emb := *sub.Sig.EmbeddedSignature
			emb.Notations = append([]*packet.Notation(nil), sub.Sig.EmbeddedSignature.Notations...)
			emb.CreationTime = sig.CreationTime
			if weakHashes[emb.Hash] {
				emb.Hash = stdcrypto.SHA256
			}
			if err := emb.CrossSignKey(sub.PublicKey, e.PrimaryKey, sub.PrivateKey, cfg); err != nil {
				return err
			}
			sig.EmbeddedSignature = &emb
		}
		if err := sig.SignKey(sub.PublicKey, e.PrivateKey, cfg); err != nil {
			return err
		}
		sub.Sig = sig
	}
	return nil
}

var weakHashes = map[stdcrypto.Hash]bool{
	stdcrypto.MD5: true, stdcrypto.SHA1: true, stdcrypto.RIPEMD160: true,
}

func replaceSig(list []*packet.Signature, old, repl *packet.Signature) []*packet.Signature {
	out := make([]*packet.Signature, 0, len(list)+1)
	for _, s := range list {
		if s != old {
			out = append(out, s)
		}
	}
	return append(out, repl)
}

// checkExtended re-reads an extended key's public half and requires it to be the
// same key, usable until the requested moment (to the second).
func checkExtended(rec Record, wantFpr string, until, now time.Time) error {
	k, err := pgpkeys.Parse(rec.PublicKey)
	if err != nil {
		return err
	}
	if k.Fingerprint != wantFpr {
		return errors.New("fingerprint changed")
	}
	v, err := pgpkeys.Assess(rec.PublicKey, now)
	if err != nil {
		return err
	}
	if !v.Usable(now) {
		return errors.New("the extended key is not usable")
	}
	got, never := v.UsableUntil()
	if never || !got.Truncate(time.Second).Equal(until.Truncate(time.Second)) {
		return errors.New("the extended key is not usable until the requested time")
	}
	return nil
}

// confirmMatches compares a revocation confirmation against the fingerprint's
// last 8 hex digits, case-insensitively.
func confirmMatches(fpr, confirm string) bool {
	c := strings.ToUpper(strings.TrimSpace(confirm))
	return len(c) == 8 && c == fingerprintSuffix(fpr)
}

func fingerprintSuffix(fpr string) string {
	f := strings.ToUpper(fpr)
	if len(f) < 8 {
		return f
	}
	return f[len(f)-8:]
}

// bindSubkeyLifetimes re-makes every subkey binding of a freshly generated
// entity with the given key lifetime (seconds from the subkey's creation), at
// the generation time cfg carries. Signing-capable subkeys are not touched:
// NewEntity makes none, and their bindings need a back-signature this does not
// re-make.
func bindSubkeyLifetimes(e *openpgp.Entity, lifetime uint32, cfg *packet.Config) error {
	for i := range e.Subkeys {
		sub := &e.Subkeys[i]
		if sub.Sig == nil || sub.Sig.FlagSign {
			continue
		}
		cp := *sub.Sig
		cp.Notations = append([]*packet.Notation(nil), sub.Sig.Notations...)
		l := lifetime
		cp.KeyLifetimeSecs = &l
		if err := cp.SignKey(sub.PublicKey, e.PrivateKey, cfg); err != nil {
			return err
		}
		sub.Sig = &cp
	}
	return nil
}

// generateLifetime turns GenerateRequest.ExpiresDays or .ExpiresAt into a key
// lifetime in seconds from created (0 = never). An exact expiry becomes
// at - created, so created + lifetime is that instant to the second (#813).
func generateLifetime(days *int, at *time.Time, created time.Time) (uint32, error) {
	if days != nil && at != nil {
		return 0, pgpwire.Errorf(pgpwire.CodeInvalidRequest, "give at most one of expires_days and expires_at")
	}
	if at != nil {
		until, err := exactExpiry(*at, created)
		if err != nil {
			return 0, err
		}
		return uint32(until.Sub(created) / time.Second), nil
	}
	d := pgpwire.DefaultKeyLifetimeDays
	if days != nil {
		d = *days
	}
	if d < 0 || d > pgpwire.MaxKeyLifetimeDays {
		return 0, pgpwire.Errorf(pgpwire.CodeInvalidRequest,
			"expires_days must be between 0 (never) and %d", pgpwire.MaxKeyLifetimeDays)
	}
	return uint32(time.Duration(d) * 24 * time.Hour / time.Second), nil
}

// exactExpiry validates an exact expiry against now and returns it in UTC,
// truncated to the second (OpenPGP's resolution): at least MinKeyLifetime and at
// most MaxKeyLifetimeDays out (#813).
func exactExpiry(at, now time.Time) (time.Time, error) {
	until := at.UTC().Truncate(time.Second)
	if d := until.Sub(now); d < pgpwire.MinKeyLifetime || d > time.Duration(pgpwire.MaxKeyLifetimeDays)*24*time.Hour {
		return time.Time{}, pgpwire.Errorf(pgpwire.CodeInvalidRequest,
			"expires_at must be between one day and %d days from now", pgpwire.MaxKeyLifetimeDays)
	}
	return until, nil
}

// checkGenerated re-reads a freshly generated public key and requires its
// primary key and its encryption subkey to expire at created + lifetime (to the
// second), or never when lifetime is 0.
func checkGenerated(pub []byte, created time.Time, lifetime uint32) error {
	v, err := pgpkeys.Assess(pub, created)
	if err != nil {
		return err
	}
	if !v.HasEncryptionKey {
		return errors.New("no encryption subkey")
	}
	if lifetime == 0 {
		if v.PrimaryExpires != nil || v.EncryptionExpires != nil {
			return errors.New("the key expires, but no lifetime was asked for")
		}
		return nil
	}
	want := created.Add(time.Duration(lifetime) * time.Second)
	for name, got := range map[string]*time.Time{"primary key": v.PrimaryExpires, "encryption subkey": v.EncryptionExpires} {
		if got == nil || !got.Equal(want) {
			return fmt.Errorf("the %s expires at %v, not %v", name, got, want)
		}
	}
	return nil
}
