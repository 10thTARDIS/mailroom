// SPDX-License-Identifier: AGPL-3.0-only

package pgpkeyd

import (
	"bytes"
	"context"
	stdcrypto "crypto"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"strings"
	"sync"
	"time"

	"github.com/ProtonMail/go-crypto/openpgp"
	"github.com/ProtonMail/go-crypto/openpgp/armor"
	pgperrors "github.com/ProtonMail/go-crypto/openpgp/errors"
	"github.com/ProtonMail/go-crypto/openpgp/packet"

	"mailroom/internal/pgpverify"
	"mailroom/internal/pgpwire"
)

// Decrypt (#47 increment 6b, ADR-037). Every line of this file answers one item
// of the 2026-08-21 checklist on #47, and the comments name which.
//
// ⚠ NO CRYPTOGRAPHY IS WRITTEN HERE. ProtonMail go-crypto parses the packets,
// unwraps the session key, decrypts and checks integrity. This file chooses
// which keys to offer it, which of its options to pin, how much output to
// accept, and what to tell the caller — which is the part that decides whether
// the method is an oracle.

// errDecrypt is the ONE wire error every cryptographic failure becomes
// (checklist item 1). It is a package variable rather than built per call so
// that "the same code and the same message" is true by construction: there is
// exactly one value, and the failure paths below all return it.
var errDecrypt = &pgpwire.Error{Code: pgpwire.CodeDecryptFailed, Message: pgpwire.DecryptFailedMessage}

// decryptConfig pins every go-crypto option the decrypt path depends on,
// EXPLICITLY, rather than inheriting defaults that a library upgrade could move.
//
// Defaults as measured in go-crypto v1.5.2 (openpgp/packet/config.go), and why
// each is restated:
//
//   - InsecureAllowUnauthenticatedMessages defaults to false. A false here is
//     what makes ReadMessage refuse a Symmetrically Encrypted Data packet
//     (tag 9, no MDC) with "message is not integrity protected" — EFAIL's
//     precondition (checklist item 2). Restated so the refusal is a line in this
//     file that a probe can flip, and a test (TestDecryptRefusesSED) proves the
//     flip is observable.
//   - CacheSessionKey defaults to false. True would put the session key into
//     the MessageDetails; nothing here reads it, and false keeps it from existing
//     outside the library's own stack (checklist item 6).
//   - CheckPacketSequence defaults to strict. Restated: a lenient grammar is
//     more inputs reaching the decryptor for no benefit.
//   - InsecureAllowDecryptionWithSigningKeys defaults to false. Restated: a key
//     flagged sign-only must not become a decryption key because a sender's
//     client ignored the flags.
//   - MaxDecompressedMessageSize defaults to UNLIMITED. Set to the plaintext
//     cap (pgpwire.MaxDecryptedBytes), which go-crypto applies per compressed layer
//     (checklist item 3). The read below is capped again independently, because
//     a per-layer limit is a property of a dependency's internals, and the
//     output cap is the property this method promises.
//
// Compression NESTING is bounded by go-crypto's packet reader: it refuses a
// 33rd pushed layer ("too many layers of packets", maxReaders = 32 in
// openpgp/packet/reader.go), and every layer is size-limited by the line above,
// so the total decompression work is bounded by 32 × MaxDecryptedBytes and the
// returned plaintext by MaxDecryptedBytes. TestDecryptRefusesDeepCompressionNesting
// pins the refusal.
func decryptConfig(maxPlaintext int) *packet.Config {
	// The per-layer limit counts DECOMPRESSED PACKET BYTES, which include the
	// literal packet's own header and partial-length framing around the
	// plaintext — so a limit equal to the plaintext cap would refuse a message
	// whose plaintext is exactly at it. The slack is framing-sized; the read cap
	// in decryptWithUserKeys is the exact one.
	limit := int64(maxPlaintext) + decompressionFramingSlack
	return &packet.Config{
		InsecureAllowUnauthenticatedMessages:   false,
		InsecureAllowDecryptionWithSigningKeys: false,
		CacheSessionKey:                        false,
		CheckPacketSequence:                    packet.BoolPointer(true),
		MaxDecompressedMessageSize:             &limit,
	}
}

// decompressionFramingSlack is the room left in go-crypto's per-layer
// decompression limit for packet framing (see decryptConfig).
const decompressionFramingSlack = 64 << 10

// Decrypt opens one PGP/MIME encrypted part with the caller's keys and returns
// the decrypted MIME entity.
func (s *Service) Decrypt(ctx context.Context, req pgpwire.DecryptRequest) (*pgpwire.DecryptResponse, error) {
	// ⚠ userID, not req.UserID, from here down — see validUserID (#301 C1).
	userID, err := validUserID(req.UserID)
	if err != nil {
		return nil, err
	}
	msgID, ok := canonicalULID(req.MessageID)
	if !ok {
		return nil, pgpwire.Errorf(pgpwire.CodeInvalidRequest, "message_id is not a ULID")
	}
	switch req.Purpose {
	case pgpwire.DecryptForBody, pgpwire.DecryptForAttachment:
	default:
		return nil, pgpwire.Errorf(pgpwire.CodeInvalidRequest, "purpose must be body or attachment")
	}
	if len(req.Ciphertext) == 0 {
		return nil, pgpwire.Errorf(pgpwire.CodeInvalidRequest, "ciphertext is required")
	}
	// The candidate signer keys (RFC 3156 section 6.2) are arguments, and are
	// validated as arguments: before the budget, before any key of the user's is
	// touched, and with their own (non-cryptographic) code.
	signers, err := s.readSigners(req.Signers)
	if err != nil {
		return nil, err
	}

	// ── CHECKLIST ITEM 4: the per-user budget, BEFORE any key is touched. ──
	// A refusal here unseals nothing and decrypts nothing, so it is not a
	// cryptographic outcome and may have its own code.
	if !s.decryptLimit.allow(userID, s.now()) {
		s.log.Warn("pgp decrypt refused: rate limit", "user_id", userID, "message_id", msgID)
		return nil, pgpwire.Errorf(pgpwire.CodeRateLimited, "too many decryptions; try again shortly")
	}

	// ── CHECKLIST ITEM 3, memory: at most DefaultDecryptConcurrency in flight.
	select {
	case s.decryptSlots <- struct{}{}:
		defer func() { <-s.decryptSlots }()
	case <-ctx.Done():
		return nil, pgpwire.Errorf(pgpwire.CodeInternal, "decrypt cancelled while waiting")
	}

	// The capabilities this method cannot work without. Checked before the keys
	// are read, and reported as internal: a deployment fault, not a refusal.
	lister, canList := s.vault.(KeyLister)
	auditor, canAudit := s.vault.(DecryptAuditor)
	if !canList || !canAudit {
		return nil, pgpwire.Errorf(pgpwire.CodeInternal, "this vault cannot decrypt")
	}

	audit := DecryptAudit{UserID: userID, MessageID: msgID, Purpose: string(req.Purpose)}

	entity, fingerprint, sig, reason := s.decryptWithUserKeys(ctx, lister, userID, req.Ciphertext, signers)

	// ── CHECKLIST ITEM 4: the trail is written BEFORE anything is returned. ──
	// Success and failure both. A trail that only recorded successes would hide
	// the shape an oracle attempt has: many failures for one user.
	audit.Decrypted = reason == ""
	audit.Fingerprint = fingerprint
	if err := auditor.RecordDecrypt(WithCaller(ctx, userID), audit); err != nil {
		// ⚠ FAIL CLOSED. Plaintext that left without its audit row is a
		// decryption nobody can reconstruct afterwards.
		s.log.Error("pgp decrypt: audit row not written; refusing to return plaintext",
			"user_id", userID, "message_id", msgID, "err", err)
		return nil, pgpwire.Errorf(pgpwire.CodeInternal, "decrypt audit failed")
	}

	if reason != "" {
		// The REASON goes to the operator's log, in our own words, never the
		// library's error text; the caller gets errDecrypt and nothing else.
		s.log.Warn("pgp decrypt failed",
			"user_id", userID, "message_id", msgID, "purpose", string(req.Purpose), "reason", reason)
		return nil, errDecrypt
	}

	s.log.Info("pgp message decrypted",
		"user_id", userID, "message_id", msgID, "purpose", string(req.Purpose),
		"fingerprint", fingerprint, "plaintext_bytes", len(entity),
		"signed", sig.Signed, "signature", string(sig.Status), "candidate_signers", len(signers))
	return &pgpwire.DecryptResponse{Entity: entity, Signature: sig}, nil
}

// readSigners parses the request's candidate signer keys (RFC 3156 section 6.2).
// Over the count or size bounds, or any key carrying private material, refuses
// the request: the list is the application's choice and a secret in it means
// something upstream is wrong. An unreadable key is skipped, so that one bad
// stored key cannot hide the others (pgpverify's rule).
func (s *Service) readSigners(in []pgpwire.DecryptSigner) (openpgp.EntityList, error) {
	if len(in) > pgpwire.MaxDecryptSigners {
		return nil, pgpwire.Errorf(pgpwire.CodeInvalidRequest, "too many signer keys (over %d)", pgpwire.MaxDecryptSigners)
	}
	total := 0
	for _, k := range in {
		if len(k.PublicKey) > 2*pgpwire.MaxRecipientKeyBytes {
			return nil, pgpwire.Errorf(pgpwire.CodeInvalidRequest, "a signer key is too large")
		}
		total += len(k.PublicKey)
	}
	if total > pgpwire.MaxDecryptSignerBytes {
		return nil, pgpwire.Errorf(pgpwire.CodeInvalidRequest, "signer keys are too large in total")
	}
	ring := make(openpgp.EntityList, 0, len(in))
	for i, k := range in {
		e, err := readPublicEntity(k.PublicKey)
		if errors.Is(err, errKeyHasPrivateMaterial) {
			s.log.Warn("pgp decrypt refused: a candidate signer key carries private material", "index", i)
			return nil, pgpwire.Errorf(pgpwire.CodeInvalidRequest, "signer keys must be public keys")
		}
		if err != nil {
			s.log.Warn("pgp decrypt: skipping an unreadable candidate signer key", "index", i)
			continue
		}
		ring = append(ring, e)
	}
	return ring, nil
}

// decryptKeyRing keeps the two jobs of go-crypto's KeyRing apart (RFC 3156
// section 6.2): the USER'S keys decrypt, the CANDIDATE keys verify.
// openpgp.ReadMessage asks a KeyRing for decryption keys (DecryptionKeys,
// KeysById on a session-key packet) and for the signer of a one-pass signature
// (KeysByIdUsage with the sign flag) — three methods, two of each kind, measured
// in go-crypto v1.5.2 openpgp/read.go.
//
// ⚠ WHY SPLIT. One merged ring would let the user's own PRIVATE key answer a
// signature lookup, so a message signed by the user's key would come back
// "verified" whether or not the application offered that key for this From
// address — the decision of which keys may vouch for a sender would move from
// the application (ADR-039) into whatever the crypto process happens to hold.
// And no candidate public key can ever be asked to decrypt.
type decryptKeyRing struct {
	own     openpgp.EntityList
	signers openpgp.EntityList
}

func (r decryptKeyRing) KeysById(id uint64) []openpgp.Key { return r.own.KeysById(id) }
func (r decryptKeyRing) DecryptionKeys() []openpgp.Key    { return r.own.DecryptionKeys() }
func (r decryptKeyRing) KeysByIdUsage(id uint64, usage byte) []openpgp.Key {
	return r.signers.KeysByIdUsage(id, usage)
}

// weakSignatureHashes are the hashes an embedded signature is refused under
// (reported `bad`), the verify-side twin of Sign's weakMicAlgs (#301 C2).
//
// ⚠ go-crypto's v1 API DOES NOT REFUSE THEM ITSELF. Measured in v1.5.2:
// packet.Config.RejectMessageHashAlgorithms (SHA-1, MD5, RIPEMD-160 by default) is
// consulted only by the openpgp/v2 reader; openpgp.ReadMessage verifies a SHA-1
// one-pass signature and reports it good (TestDecryptEmbeddedSignatureRefusesSHA1
// proves both halves). MD5 and RIPEMD-160 cannot even be parsed by this library
// (their packets are skipped as unsupported, so such a message reads as
// unsigned, never as verified); they are listed as insurance against a version
// that re-adds them.
var weakSignatureHashes = map[stdcrypto.Hash]bool{
	stdcrypto.MD5:       true,
	stdcrypto.SHA1:      true,
	stdcrypto.RIPEMD160: true,
}

// embeddedResult reads the signature verdict off md.
//
// ⚠ ONLY AFTER md.UnverifiedBody WAS READ TO EOF. go-crypto checks the signature
// when the literal data ends; before that SignatureError is nil for every
// message, tampered or not. The caller reads the whole body first (and a body
// that could not be read to EOF never gets here).
//
// ⚠ AND A NIL SignatureError IS NOT A GOOD SIGNATURE. When no candidate key
// matches, go-crypto leaves SignedBy nil, hashes nothing, verifies nothing, and
// leaves SignatureError nil too. Verified therefore requires a candidate key
// (SignedBy), the checked signature packet (Signature) and no error — all three.
func embeddedResult(md *openpgp.MessageDetails) pgpwire.EmbeddedSignature {
	if !md.IsSigned {
		return pgpwire.EmbeddedSignature{Signed: false}
	}
	out := pgpwire.EmbeddedSignature{Signed: true}

	// Who the signature says made it: the checked packet, else any trailing
	// signature packet, else the one-pass header.
	sig := md.Signature
	if sig == nil && len(md.UnverifiedSignatures) > 0 {
		sig = md.UnverifiedSignatures[0]
	}
	switch {
	case sig != nil && len(sig.IssuerFingerprint) > 0:
		out.IssuerFingerprint = strings.ToUpper(hex.EncodeToString(sig.IssuerFingerprint))
	case sig != nil && sig.IssuerKeyId != nil:
		out.IssuerKeyID = fmt.Sprintf("%016X", *sig.IssuerKeyId)
	case len(md.SignedByFingerprint) > 0:
		out.IssuerFingerprint = strings.ToUpper(hex.EncodeToString(md.SignedByFingerprint))
	default:
		out.IssuerKeyID = fmt.Sprintf("%016X", md.SignedByKeyId)
	}

	switch {
	case md.SignedBy == nil || md.SignedBy.Entity == nil:
		out.Status = pgpwire.EmbeddedNoKey
	case md.Signature != nil && md.SignatureError != nil && !weakSignatureHashes[md.Signature.Hash] &&
		errors.Is(md.SignatureError, pgperrors.ErrKeyExpired):
		// Still bad (ADR-039 §6), with the reason: go-crypto sets ErrKeyExpired
		// only after the signature VERIFIED over the literal data (read.go runs
		// checkMessageSignatureDetails after VerifySignature succeeds), so this
		// is a key past its date, not altered content (#795/#796). A weak hash is
		// excluded above so it can never be softened into this reason.
		out.Status = pgpwire.EmbeddedBad
		out.KeyExpired = true
		signed := md.Signature.CreationTime.UTC()
		out.SignedAt = &signed
		out.KeyExpiredAt = pgpverify.SigningKeyExpiry(md.SignedBy.Entity, md.SignedByKeyId)
		out.ExpiredSignerFingerprint = fingerprint0(md.SignedBy.Entity)
	case md.Signature == nil || md.SignatureError != nil:
		out.Status = pgpwire.EmbeddedBad
	case weakSignatureHashes[md.Signature.Hash]:
		out.Status = pgpwire.EmbeddedBad
	default:
		out.Status = pgpwire.EmbeddedVerified
		out.SignerFingerprint = fingerprint0(md.SignedBy.Entity)
	}
	return out
}

// Failure reasons, for the operator's log only. They never reach the wire.
const (
	reasonNoKeys       = "no_usable_key"
	reasonUnreadable   = "unreadable_ciphertext"
	reasonNotEncrypted = "not_an_encrypted_message"
	reasonNoMatch      = "no_key_opens_it_or_not_integrity_protected"
	reasonIntegrity    = "integrity_or_parse_failure_while_reading"
	reasonTooLarge     = "plaintext_over_cap"
)

// decryptWithUserKeys does the work and returns the plaintext, the opening
// key's fingerprint and the embedded-signature result, or a non-empty reason. It
// returns NO error value at all: the only thing a caller can learn from a
// failure is that it failed.
func (s *Service) decryptWithUserKeys(ctx context.Context, lister KeyLister, userID string, ciphertext []byte,
	signers openpgp.EntityList) (plaintext []byte, fingerprint string, sig pgpwire.EmbeddedSignature, reason string) {
	plaintext, fingerprint, md, reason := s.readWithUserKeys(ctx, lister, userID, ciphertext, signers)
	if reason != "" {
		return nil, "", pgpwire.EmbeddedSignature{}, reason
	}
	// The body has been read to EOF (readWithUserKeys returns no md otherwise),
	// so the signature fields are now meaningful.
	return plaintext, fingerprint, embeddedResult(md), ""
}

func (s *Service) readWithUserKeys(ctx context.Context, lister KeyLister, userID string, ciphertext []byte,
	signers openpgp.EntityList) (plaintext []byte, fingerprint string, md *openpgp.MessageDetails, reason string) {
	records, err := lister.List(WithCaller(ctx, userID))
	if err != nil {
		s.log.Error("pgp decrypt: key list failed", "user_id", userID, "err", err)
		return nil, "", nil, reasonNoKeys
	}

	var ring openpgp.EntityList
	byPrimary := map[string]string{}
	for _, rec := range records {
		// ─── THE OWNERSHIP FILTER ───────────────────────────────────────────
		// The same rule as Sign's and ExportPublic's comparison, applied to a
		// list. StoreVault already reads under RLS; MemoryVault deliberately
		// does not, so deleting this line turns
		// TestDecryptConfusedDeputyFailsClosed red — that is what keeps it
		// demonstrably load-bearing rather than redundant-looking.
		if rec.UserID != userID {
			continue
		}
		// ─── CHECKLIST ITEM 5: NO RETIREMENT CHECK, AND THAT IS THE POINT. ──
		// Sign refuses rec.Retired(); Decrypt must not. A decrypt_only or
		// revoked key still opens the mail that was encrypted to it, forever —
		// copying Sign's refusal here would silently orphan the user's archive.
		// TestRevokedKeyStillDecrypts is the positive control.
		e, err := s.unseal(rec)
		if err != nil {
			// One unusable key must not hide the others: keep going. unseal has
			// already logged it (as a store-tampering signal).
			continue
		}
		ring = append(ring, e)
		byPrimary[fingerprint0(e)] = rec.Fingerprint
	}
	if len(ring) == 0 {
		return nil, "", nil, reasonNoKeys
	}

	body, ok := openArmorOrBinary(ciphertext)
	if !ok {
		return nil, "", nil, reasonUnreadable
	}

	md, err = openpgp.ReadMessage(body, decryptKeyRing{own: ring, signers: signers}, nil, decryptConfig(s.maxPlaintext))
	if err != nil {
		// Wrong key, SED refused, malformed packets, unsupported algorithms:
		// all one outcome. err is deliberately dropped unread.
		return nil, "", nil, reasonNoMatch
	}
	if !md.IsEncrypted || md.DecryptedWith.Entity == nil {
		// A bare literal or signed-only message inside a multipart/encrypted
		// container is not encrypted mail, and must not come back as though it
		// had been decrypted.
		return nil, "", nil, reasonNotEncrypted
	}

	// ── CHECKLIST ITEM 3: the hard cap on what comes OUT. ──
	// One byte past the cap is read so "exactly at the cap" and "over it" can be
	// told apart. The MDC/AEAD tag is verified only at EOF, so a read that
	// stops at the cap never verified integrity — and is refused, never
	// returned.
	out, err := io.ReadAll(io.LimitReader(md.UnverifiedBody, int64(s.maxPlaintext)+1))
	if err != nil {
		// Integrity failure (MDC mismatch, AEAD tag) or a parse failure inside
		// the decrypted stream. go-crypto reports both as errors at EOF; both
		// are the same opaque outcome here (checklist item 1).
		return nil, "", nil, reasonIntegrity
	}
	if len(out) > s.maxPlaintext {
		return nil, "", nil, reasonTooLarge
	}

	fpr := byPrimary[fingerprint0(md.DecryptedWith.Entity)]
	if fpr == "" {
		fpr = fingerprint0(md.DecryptedWith.Entity)
	}
	return out, fpr, md, ""
}

// openArmorOrBinary accepts the ciphertext as RFC 3156 transmits it (an armored
// PGP MESSAGE) or as raw packets.
func openArmorOrBinary(ct []byte) (io.Reader, bool) {
	trimmed := bytes.TrimLeft(ct, " \t\r\n")
	if bytes.HasPrefix(trimmed, []byte("-----BEGIN PGP")) {
		blk, err := armor.Decode(bytes.NewReader(trimmed))
		if err != nil || blk.Type != "PGP MESSAGE" {
			return nil, false
		}
		return blk.Body, true
	}
	// A binary OpenPGP packet always has the high bit of its first octet set
	// (RFC 9580 §4.2). Anything else is neither form.
	if len(ct) == 0 || ct[0]&0x80 == 0 {
		return nil, false
	}
	return bytes.NewReader(ct), true
}

// fingerprint0 is fingerprint() for an entity that may be partially populated.
func fingerprint0(e *openpgp.Entity) string {
	if e == nil || e.PrimaryKey == nil {
		return ""
	}
	return fingerprint(e)
}

// ---------------------------------------------------------------------------
// The per-user decrypt budget (checklist item 4).
// ---------------------------------------------------------------------------

// Decrypt budget defaults. Generous for a person reading mail and tight for a
// loop: opening a message costs one call, and each inline image or attachment
// download inside it costs one more (the part is re-derived by decrypting
// again, ADR-037), so a long encrypted thread with images can spend a few dozen
// in a burst. A sustained one per second is far above reading speed and far
// below what bulk exfiltration of a mailbox through a compromised application
// would want.
const (
	DefaultDecryptBurst     = 120
	DefaultDecryptPerSecond = 1.0
	// DefaultDecryptConcurrency bounds decrypts in flight in the process. One
	// call can hold several times pgpwire.MaxDecryptedBytes at peak, and the
	// crypto container's memory limit is small; serialising to two keeps the
	// peak bounded no matter how many requests the application sends.
	DefaultDecryptConcurrency = 2
)

// decryptLimiter is a per-user token bucket, in-process. The crypto process
// serves one deployment's handful of users, so the map needs no eviction; a
// restart refills every bucket, which is the honest cost of keeping it out of
// the database.
type decryptLimiter struct {
	mu        sync.Mutex
	burst     float64
	perSecond float64
	buckets   map[string]*decryptBucket
}

type decryptBucket struct {
	tokens float64
	last   time.Time
}

func newDecryptLimiter(burst int, perSecond float64) *decryptLimiter {
	return &decryptLimiter{
		burst:     float64(burst),
		perSecond: perSecond,
		buckets:   map[string]*decryptBucket{},
	}
}

func (l *decryptLimiter) allow(userID string, now time.Time) bool {
	l.mu.Lock()
	defer l.mu.Unlock()
	b, ok := l.buckets[userID]
	if !ok {
		b = &decryptBucket{tokens: l.burst, last: now}
		l.buckets[userID] = b
	}
	if elapsed := now.Sub(b.last).Seconds(); elapsed > 0 {
		b.tokens += elapsed * l.perSecond
		if b.tokens > l.burst {
			b.tokens = l.burst
		}
		b.last = now
	}
	if b.tokens < 1 {
		return false
	}
	b.tokens--
	return true
}
