// SPDX-License-Identifier: AGPL-3.0-only

// Package pgpverify answers one question about a received message: does its
// OpenPGP signature check out (#47 increment 6a, RFC 3156)?
//
// # It never touches private key material, and that is why it is here and not
// # behind the crypto process
//
// Verification needs only a PUBLIC key. So unlike signing and decryption, it
// does not cross ADR-024's boundary at all: no socket call, no root secret, no
// confused-deputy surface, and nothing here can leak a key because it is never
// given one. That asymmetry is worth stating, because "crypto" is otherwise a
// word that implies the crypto process is involved.
//
// # What a verdict does and does not mean
//
// A VERIFIED result says the bytes that arrived match the signature, and names
// the key that made it. It says NOTHING about whether that key belongs to who
// the From header claims — that is key discovery and trust, which is a separate
// problem and deliberately not solved here. A report that conflated the two
// would tell a user a forgery was "verified".
package pgpverify

import (
	"bytes"
	"crypto"
	"errors"
	"fmt"
	"io"
	"strings"
	"time"

	"github.com/ProtonMail/go-crypto/openpgp"
	"github.com/ProtonMail/go-crypto/openpgp/armor"
	pgperrors "github.com/ProtonMail/go-crypto/openpgp/errors"
	"github.com/ProtonMail/go-crypto/openpgp/packet"

	"mailroom/internal/pgpmime"
)

// Verdict is the outcome of examining one message.
type Verdict string

const (
	// VerdictNotSigned: no OpenPGP signature to check. The ordinary case.
	VerdictNotSigned Verdict = "not_signed"
	// VerdictVerified: the signature checks out over the bytes that arrived.
	VerdictVerified Verdict = "verified"
	// VerdictBadSignature: there IS a signature and it does not match the
	// content. ⚠ This is the finding worth acting on — it means either the mail
	// was altered in transit or it was never signed by the key it names.
	VerdictBadSignature Verdict = "bad_signature"
	// VerdictNoKey: a signature we hold no public key for. Expected for every
	// correspondent until key discovery exists; NOT a failure, and reporting it
	// as one would drown the real failures.
	VerdictNoKey Verdict = "no_key"
	// VerdictMalformed: it claims to be signed and cannot be taken apart —
	// a structural problem, not a cryptographic one. Kept separate so a broken
	// producer is not reported as a bad signature.
	VerdictMalformed Verdict = "malformed"
)

// PublicKey is one key to check against: binary OpenPGP packets, as
// app.user_keys.public_key stores them.
type PublicKey struct {
	Fingerprint string
	Binary      []byte
}

// Result is what Verify found.
type Result struct {
	Verdict Verdict
	// SignerFingerprint is the key that verified, uppercase hex. Set only on
	// VerdictVerified — naming a key we did not actually verify against would
	// be the same conflation the package doc warns about.
	SignerFingerprint string
	// IssuerFingerprint is the key the SIGNATURE says made it, uppercase hex: the
	// full fingerprint when the signature carries one (it may be a signing
	// subkey's), otherwise the 16-digit key id. Read from the signature packet,
	// so it is available when we hold no key (VerdictNoKey) and is what the
	// "signed with a key you have not seen" display names. Empty if the packet
	// cannot be read.
	IssuerFingerprint string
	// MicAlg is the container's declared hash. A hint from the message, not a
	// fact about the signature.
	MicAlg string
	// Err carries the underlying reason for a non-verified verdict, for the
	// operator's eyes. Never nil for malformed.
	Err error

	// KeyExpired (#795/#796): the verdict is VerdictBadSignature ONLY because the
	// key that made the signature has expired at verification time. The
	// cryptographic check itself PASSED: go-crypto reports ErrKeyExpired only
	// after the signature verified over the bytes (read.go,
	// checkMessageSignatureDetails runs after VerifySignature succeeds), and a
	// refused weak hash never sets this. So it is not a sign of forgery or
	// alteration, and a client can say "signed with a key that has expired"
	// rather than "bad signature".
	//
	// ⚠ THE STATE STAYS bad (ADR-039 §6: "an expired or revoked key" is `bad`).
	// This is a REASON on that state, not a new state, and it is not
	// attribution: it says a held key made the signature, not that the key
	// belongs to the sender.
	KeyExpired bool
	// ExpiredSignerFingerprint is the primary fingerprint of the expired key
	// that made the signature (KeyExpired only).
	ExpiredSignerFingerprint string
	// SignedAt is the signature's creation time (KeyExpired only).
	SignedAt *time.Time
	// KeyExpiredAt is when the signing key expired: the earlier of the primary
	// key's expiry and, for a signing subkey, the subkey's, under the
	// self-signatures in force now (KeyExpired only; nil if it cannot be read).
	KeyExpiredAt *time.Time
}

// ValidWhenSigned reports whether a KeyExpired signature was made BEFORE its
// key expired, by the signature's own creation time. It is a claim the signer's
// clock makes, so it is shown as context ("signed on 3 May, key expired on
// 1 June"), never used to upgrade the verdict.
func (r Result) ValidWhenSigned() bool {
	return r.KeyExpired && r.SignedAt != nil && r.KeyExpiredAt != nil && r.SignedAt.Before(*r.KeyExpiredAt)
}

// Verify checks a raw RFC 5322 message against the supplied public keys.
//
// ⚠ THE BYTES HASHED ARE THE CANONICAL FORM OF THE ENTITY AS IT ARRIVED
// (RFC 3156 §5), recovered by pgpmime.SplitSigned rather than by a MIME parser.
// A parser decodes transfer encodings, and hashing decoded bytes fails on mail
// that is perfectly valid — a failure that looks cryptographic and is not.
func Verify(raw []byte, keys []PublicKey) Result {
	parts, err := pgpmime.SplitSigned(raw)
	switch {
	case errors.Is(err, pgpmime.ErrNotSigned):
		return Result{Verdict: VerdictNotSigned}
	case err != nil:
		return Result{Verdict: VerdictMalformed, Err: err}
	}

	res := Result{MicAlg: parts.MicAlg, IssuerFingerprint: issuerOf(parts.Signature)}

	if len(keys) == 0 {
		res.Verdict = VerdictNoKey
		res.Err = errors.New("no public keys available to check against")
		return res
	}

	// Each key is tried on its own rather than as one keyring, so that a single
	// unparsable key cannot make every other key's verdict disappear — and so
	// the fingerprint reported is the one that actually verified, not whichever
	// entity the library happened to select.
	var sawUnknownIssuer, sawUsable bool
	var lastErr error
	for _, k := range keys {
		ring, err := openpgp.ReadKeyRing(bytes.NewReader(k.Binary))
		if err != nil || len(ring) == 0 {
			// A stored key we cannot parse is a real problem, but it is this
			// key's problem: keep going and let another key verify.
			lastErr = fmt.Errorf("unreadable stored key %s: %w", k.Fingerprint, err)
			continue
		}
		sawUsable = true
		signer, sig, err := checkArmored(ring, pgpmime.Canonicalize(parts.Entity), parts.Signature)
		if err == nil {
			res.Verdict = VerdictVerified
			res.SignerFingerprint = fingerprintOf(signer, k.Fingerprint)
			return res
		}
		if errors.Is(err, pgperrors.ErrKeyExpired) && sig != nil && signer != nil {
			// The signature checked out; only the key's date failed it (see
			// Result.KeyExpired). Still bad, with the reason attached.
			res.Verdict = VerdictBadSignature
			res.Err = err
			MarkKeyExpired(&res, signer, sig)
			res.ExpiredSignerFingerprint = fingerprintOf(signer, k.Fingerprint)
			return res
		}
		if errors.Is(err, pgperrors.ErrUnknownIssuer) {
			// This signature was not made by this key. Says nothing about the
			// signature's validity — only that we asked the wrong key.
			sawUnknownIssuer = true
			continue
		}
		lastErr = err
	}

	switch {
	case !sawUsable:
		res.Verdict = VerdictNoKey
		res.Err = lastErr
	case lastErr == nil && sawUnknownIssuer:
		// Every key we hold disclaimed the signature. We cannot say the
		// signature is bad — we have never seen the key that made it.
		res.Verdict = VerdictNoKey
		res.Err = errors.New("no held key matches this signature's issuer")
	default:
		// A key accepted the issuer and the check still failed: the content
		// does not match the signature.
		res.Verdict = VerdictBadSignature
		res.Err = lastErr
	}
	return res
}

// WeakHashes are the hashes a signature is refused under even when it verifies
// (#301 C2's refusal, on the verify side; the crypto process applies the same set
// to signatures inside encrypted mail, ADR-039 section 6).
//
// ⚠ go-crypto's v1 API DOES NOT REFUSE THEM ITSELF. Measured in v1.5.2: a SHA-1
// detached signature verifies as good (packet.Config.RejectMessageHashAlgorithms is
// consulted only by the openpgp/v2 API). MD5 and RIPEMD-160 cannot be parsed by
// this library at all; they are listed as insurance against a version that
// re-adds them.
var WeakHashes = map[crypto.Hash]bool{crypto.MD5: true, crypto.SHA1: true, crypto.RIPEMD160: true}

// errWeakHash is a signature that verified under a refused hash. It is reported as
// a bad signature: it is not evidence the content is what was signed.
var errWeakHash = errors.New("signature uses a refused hash algorithm")

// checkArmored is openpgp.CheckArmoredDetachedSignature plus the weak-hash
// refusal, which needs the signature packet the Check variant does not return.
//
// It also returns the signature packet and signer when go-crypto rejected the
// signature for the key's date alone (ErrKeyExpired), which it reports only
// AFTER the signature verified; a weak hash is refused before that is looked at,
// so an expired key never launders a SHA-1 signature into a softer reason.
func checkArmored(ring openpgp.KeyRing, canonical, armored []byte) (*openpgp.Entity, *packet.Signature, error) {
	blk, err := armor.Decode(bytes.NewReader(armored))
	if err != nil {
		return nil, nil, err
	}
	if blk.Type != openpgp.SignatureType {
		return nil, nil, fmt.Errorf("armor type %q is not a signature", blk.Type)
	}
	sig, signer, err := openpgp.VerifyDetachedSignature(ring, bytes.NewReader(canonical), blk.Body, nil)
	if sig != nil && WeakHashes[sig.Hash] {
		return nil, nil, errWeakHash
	}
	if err != nil {
		if errors.Is(err, pgperrors.ErrKeyExpired) {
			return signer, sig, err
		}
		return nil, nil, err
	}
	if sig == nil {
		return nil, nil, errWeakHash
	}
	return signer, sig, nil
}

// MarkKeyExpired fills a KeyExpired result's dates from the signer and the
// signature packet: SignedAt is the signature's creation time, KeyExpiredAt the
// signing key's expiry (SigningKeyExpiry). Shared with the crypto process's
// reading of a signature inside an encrypted message (ADR-039 §6), so both RFC
// 3156 shapes report an expired key the same way.
func MarkKeyExpired(res *Result, signer *openpgp.Entity, sig *packet.Signature) {
	res.KeyExpired = true
	t := sig.CreationTime.UTC()
	res.SignedAt = &t
	var issuer uint64
	if sig.IssuerKeyId != nil {
		issuer = *sig.IssuerKeyId
	}
	res.KeyExpiredAt = SigningKeyExpiry(signer, issuer)
}

// SigningKeyExpiry is when the key with OpenPGP key id issuer in e stopped being
// valid for signing: the primary key's expiry (creation + key lifetime under its
// self-signature in force) and, when issuer is a subkey, the earlier of that and
// the subkey's own. nil when neither expires or the key cannot be found. It
// mirrors the two KeyExpired checks go-crypto makes when it returns
// ErrKeyExpired, so the date reported is the date that failed the check.
func SigningKeyExpiry(e *openpgp.Entity, issuer uint64) *time.Time {
	if e == nil || e.PrimaryKey == nil {
		return nil
	}
	self, _ := e.PrimarySelfSignature()
	out := lifetimeEnd(e.PrimaryKey, self)
	if issuer == 0 || issuer == e.PrimaryKey.KeyId {
		return out
	}
	for i := range e.Subkeys {
		sub := &e.Subkeys[i]
		if sub.PublicKey == nil || sub.PublicKey.KeyId != issuer {
			continue
		}
		if t := lifetimeEnd(sub.PublicKey, sub.Sig); t != nil && (out == nil || t.Before(*out)) {
			out = t
		}
		break
	}
	return out
}

func lifetimeEnd(pk *packet.PublicKey, sig *packet.Signature) *time.Time {
	if pk == nil || sig == nil || sig.KeyLifetimeSecs == nil || *sig.KeyLifetimeSecs == 0 {
		return nil
	}
	t := pk.CreationTime.Add(time.Duration(*sig.KeyLifetimeSecs) * time.Second).UTC()
	return &t
}

// fingerprintOf reads the verifying key's fingerprint back off the entity that
// verified, falling back to the stored value only if the library gives nothing.
// Reading it back is what makes the reported fingerprint a property of the
// signature rather than of our own database row.
func fingerprintOf(signer *openpgp.Entity, stored string) string {
	if signer != nil && signer.PrimaryKey != nil {
		return strings.ToUpper(fmt.Sprintf("%X", signer.PrimaryKey.Fingerprint))
	}
	return stored
}

// issuerOf reads who a detached signature claims to be from, without verifying
// anything. "" when the armor or packet cannot be read.
func issuerOf(armored []byte) string {
	blk, err := armor.Decode(bytes.NewReader(armored))
	if err != nil {
		return ""
	}
	p, err := packet.Read(io.LimitReader(blk.Body, 1<<16))
	if err != nil {
		return ""
	}
	sig, ok := p.(*packet.Signature)
	if !ok {
		return ""
	}
	if len(sig.IssuerFingerprint) > 0 {
		return strings.ToUpper(fmt.Sprintf("%X", sig.IssuerFingerprint))
	}
	if sig.IssuerKeyId != nil {
		return strings.ToUpper(fmt.Sprintf("%016X", *sig.IssuerKeyId))
	}
	return ""
}
