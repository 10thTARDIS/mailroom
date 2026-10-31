// SPDX-License-Identifier: AGPL-3.0-only

package pgpkeyd

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"strings"
	"time"

	"github.com/ProtonMail/go-crypto/openpgp"
	"github.com/ProtonMail/go-crypto/openpgp/armor"
	"github.com/ProtonMail/go-crypto/openpgp/packet"

	"mailroom/internal/pgpwire"
)

// Encrypt (#47, ADR-038): encrypt one MIME entity to the recipients' public keys
// and to the sender's own.
//
// ⚠ NO CRYPTOGRAPHY IS WRITTEN HERE, and NO PRIVATE KEY IS USED. go-crypto's
// openpgp.Encrypt picks the session key, wraps it to each recipient's encryption
// subkey and writes the SEIPD packet. This file decides who is a recipient, what
// counts as a usable key, which options are pinned, and what is refused.
//
// The signature is made BEFORE this verb, by sign (the application wraps the
// message in an RFC 3156 multipart/signed and passes THAT as the entity), which
// is RFC 3156 §6.1's sign-then-encrypt. Keeping the two apart means the signed
// entity is exactly what decrypt-on-open hands back to pgpverify, with no second
// signing path in this process.

// Encrypt implements the encrypt verb.
func (s *Service) Encrypt(ctx context.Context, req pgpwire.EncryptRequest) (*pgpwire.EncryptResponse, error) {
	// ⚠ userID, not req.UserID, from here down — see validUserID (#301 C1).
	userID, err := validUserID(req.UserID)
	if err != nil {
		return nil, err
	}
	if strings.TrimSpace(req.KeyID) == "" {
		return nil, pgpwire.Errorf(pgpwire.CodeInvalidRequest, "key_id is required")
	}
	if len(req.Entity) == 0 {
		return nil, pgpwire.Errorf(pgpwire.CodeInvalidRequest, "entity is required")
	}
	if len(req.Entity) > pgpwire.MaxEncryptInputBytes {
		return nil, pgpwire.Errorf(pgpwire.CodeInvalidRequest,
			"the message is too large to encrypt (over %d MiB)", pgpwire.MaxEncryptInputBytes>>20)
	}
	if len(req.Recipients) == 0 {
		return nil, pgpwire.Errorf(pgpwire.CodeInvalidRequest, "at least one recipient is required")
	}
	if len(req.Recipients) > pgpwire.MaxEncryptRecipients {
		return nil, pgpwire.Errorf(pgpwire.CodeInvalidRequest,
			"too many recipients to encrypt to (over %d)", pgpwire.MaxEncryptRecipients)
	}
	for _, r := range req.Recipients {
		if !plausibleAddress(r.Address) {
			return nil, pgpwire.Errorf(pgpwire.CodeInvalidRequest, "a recipient address is missing or malformed")
		}
		if len(r.PublicKey) > 2*pgpwire.MaxRecipientKeyBytes {
			return nil, pgpwire.Errorf(pgpwire.CodeNoRecipientKey, "no usable encryption key for: %s", r.Address)
		}
	}

	// One heavy operation at a time per slot, shared with decrypt: a call holds
	// the entity, its base64 request, the ciphertext, its armor and the encoded
	// response at once, and the container's memory limit is small.
	select {
	case s.decryptSlots <- struct{}{}:
		defer func() { <-s.decryptSlots }()
	case <-ctx.Done():
		return nil, pgpwire.Errorf(pgpwire.CodeInternal, "encrypt cancelled while waiting")
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
	// The same three lines as Sign's and ExportPublic's, for the same reason: the
	// key id is the caller's choice. Here the damage of getting it wrong is that
	// ANOTHER USER'S public key is added as a recipient of this user's mail.
	if rec.UserID != userID {
		s.log.Warn("pgp encrypt request refused: wrong owner",
			"caller_user_id", userID, "key_id", req.KeyID, "owner_user_id", rec.UserID)
		return nil, pgpwire.Errorf(pgpwire.CodeDenied, "key belongs to another user")
	}
	// ────────────────────────────────────────────────────────────────────────

	if rec.Retired() {
		return nil, pgpwire.Errorf(pgpwire.CodeInvalidRequest, "that key is retired and may only decrypt")
	}
	now := s.now().UTC()

	self, err := readPublicEntity(rec.PublicKey)
	if err != nil {
		s.log.Error("pgp stored public key unreadable", "user_id", userID, "key_id", req.KeyID)
		return nil, pgpwire.Errorf(pgpwire.CodeInternal, "stored key is unreadable")
	}
	if _, ok := self.EncryptionKey(now); !ok {
		// An imported key can be sign-only. Encrypting without the sender in the
		// recipient list would make the Sent copy unreadable, so refuse instead.
		s.log.Warn("pgp encrypt refused: the sender's own key has no usable encryption subkey",
			"user_id", userID, "key_id", req.KeyID, "fingerprint", rec.Fingerprint)
		return nil, pgpwire.Errorf(pgpwire.CodeInvalidRequest,
			"your own key has no usable encryption subkey, so the sent copy could not be read back")
	}

	// ─── EVERY RECIPIENT, OR NONE ───────────────────────────────────────────
	// All failures are collected before one is reported, so the user fixes the
	// list once rather than discovering recipients one refusal at a time. The
	// message names ADDRESSES; nothing about the keys leaves this function.
	ring := openpgp.EntityList{}
	seen := map[string]bool{}
	var (
		bad []string
		to  = make([]pgpwire.EncryptedTo, 0, len(req.Recipients))
	)
	for _, r := range req.Recipients {
		e, rerr := readPublicEntity(r.PublicKey)
		if rerr != nil {
			bad = append(bad, r.Address)
			continue
		}
		// ─── THE USABILITY RULE ─────────────────────────────────────────────
		// EncryptionKey requires, at `now`: a primary self-signature, an
		// unexpired and unrevoked primary key, and an unexpired, unrevoked
		// subkey flagged for encryption. A revoked or expired recipient key is
		// refused here instead of being quietly encrypted to.
		if _, ok := e.EncryptionKey(now); !ok {
			bad = append(bad, r.Address)
			continue
		}
		fpr := fingerprint(e)
		to = append(to, pgpwire.EncryptedTo{Address: r.Address, Fingerprint: fpr})
		if !seen[fpr] {
			seen[fpr] = true
			ring = append(ring, e)
		}
	}
	if len(bad) > 0 {
		s.log.Warn("pgp encrypt refused: recipients without a usable encryption key",
			"user_id", userID, "key_id", req.KeyID, "addresses", bad)
		return nil, pgpwire.Errorf(pgpwire.CodeNoRecipientKey,
			"no usable encryption key for: %s", strings.Join(bad, ", "))
	}

	// ─── THE SELF-RECIPIENT ─────────────────────────────────────────────────
	// The sender's own key is ALWAYS a recipient: the Sent copy is this exact
	// ciphertext, and without it the sender could not read their own mail.
	selfFpr := fingerprint(self)
	if !seen[selfFpr] {
		ring = append(ring, self)
	}

	ciphertext, err := encryptEntity(ring, req.Entity, now)
	if err != nil {
		// Not %w and not relayed: the library's error text stays in our log.
		s.log.Error("pgp encrypt failed", "user_id", userID, "key_id", req.KeyID, "err", err)
		return nil, pgpwire.Errorf(pgpwire.CodeInternal, "encryption failed")
	}
	// ─── THE OUTPUT CHECK ───────────────────────────────────────────────────
	// Read the artefact back, as sign reads micalg off its signature: one key
	// packet per recipient and exactly one integrity-protected data packet. The
	// library never writes a SED packet, but "never" is a property of a
	// dependency, checked once on a path whose output cannot be recalled.
	if err := requireIntegrityProtected(ciphertext, len(ring)); err != nil {
		s.log.Error("pgp encrypt output refused", "user_id", userID, "key_id", req.KeyID, "reason", err.Error())
		return nil, pgpwire.Errorf(pgpwire.CodeInternal, "encryption failed")
	}

	s.log.Info("pgp message encrypted",
		"user_id", userID, "key_id", req.KeyID, "recipients", len(ring),
		"self_fingerprint", selfFpr, "entity_bytes", len(req.Entity))
	return &pgpwire.EncryptResponse{
		Ciphertext:      string(ciphertext),
		To:              to,
		SelfFingerprint: selfFpr,
	}, nil
}

// encryptConfig pins every go-crypto option the encrypt path depends on.
//
//   - AEADConfig stays nil, so go-crypto writes SEIPD version 1 (a verified MDC),
//     the form every OpenPGP implementation in use reads. A non-nil value would
//     opt into SEIPD v2 for keys that advertise it; that is a decision with an
//     interoperability cost, left for when there is a reason to take it.
//   - compression is left at the library default (none): compressing text that
//     a chosen-plaintext attacker can influence next to secrets is the CRIME
//     shape, and the saving on MIME that is already base64/QP is small.
//   - the clock is the service's, so key expiry is judged by the same `now` the
//     usability rule used.
func encryptConfig(now time.Time) *packet.Config {
	return &packet.Config{
		DefaultCipher: packet.CipherAES256,
		Time:          func() time.Time { return now },
	}
}

// encryptEntity writes entity as an armored OpenPGP message to every key in ring.
func encryptEntity(ring openpgp.EntityList, entity []byte, now time.Time) ([]byte, error) {
	var out bytes.Buffer
	aw, err := armor.Encode(&out, "PGP MESSAGE", nil)
	if err != nil {
		return nil, fmt.Errorf("armor: %w", err)
	}
	// Binary literal data: the entity is MIME, and a text-mode literal invites a
	// recipient's tooling to rewrite line endings inside it.
	w, err := openpgp.Encrypt(aw, ring, nil, &openpgp.FileHints{IsBinary: true}, encryptConfig(now))
	if err != nil {
		_ = aw.Close()
		return nil, err
	}
	if _, err := io.Copy(w, bytes.NewReader(entity)); err != nil {
		_ = w.Close()
		_ = aw.Close()
		return nil, err
	}
	if err := w.Close(); err != nil {
		_ = aw.Close()
		return nil, err
	}
	if err := aw.Close(); err != nil {
		return nil, err
	}
	return out.Bytes(), nil
}

// requireIntegrityProtected parses the packet sequence at the head of an armored
// message and refuses anything but: wantKeys public-key-encrypted session key
// packets followed by ONE integrity-protected data packet (SEIPD, tag 18).
// A Symmetrically Encrypted Data packet (tag 9, no MDC) is EFAIL's precondition
// and is refused outright.
func requireIntegrityProtected(armored []byte, wantKeys int) error {
	blk, err := armor.Decode(bytes.NewReader(armored))
	if err != nil || blk.Type != "PGP MESSAGE" {
		return errors.New("output is not an armored PGP MESSAGE")
	}
	pkts := packet.NewReader(blk.Body)
	keys := 0
	for {
		p, err := pkts.Next()
		if err != nil {
			return errors.New("no data packet after the session key packets")
		}
		switch v := p.(type) {
		case *packet.EncryptedKey:
			keys++
		case *packet.SymmetricallyEncrypted:
			if !v.IntegrityProtected {
				return errors.New("data packet is not integrity-protected (SED)")
			}
			if keys != wantKeys {
				return fmt.Errorf("%d session key packets, want %d", keys, wantKeys)
			}
			// Stop: the data packet's contents are ciphertext, not packets.
			return nil
		default:
			return fmt.Errorf("unexpected packet %T", p)
		}
	}
}

// readPublicEntity parses one OpenPGP public key, binary or armored, and returns
// its first entity (the same one pgpkeys.Parse describes). A key carrying private
// material is refused: a recipient key is somebody else's public key, and secret
// packets in it mean something is wrong with where it came from.
func readPublicEntity(b []byte) (*openpgp.Entity, error) {
	trimmed := bytes.TrimLeft(b, " \t\r\n")
	var (
		ring openpgp.EntityList
		err  error
	)
	if bytes.HasPrefix(trimmed, []byte("-----BEGIN PGP")) {
		ring, err = openpgp.ReadArmoredKeyRing(bytes.NewReader(trimmed))
	} else {
		ring, err = openpgp.ReadKeyRing(bytes.NewReader(b))
	}
	if err != nil || len(ring) == 0 || ring[0].PrimaryKey == nil {
		return nil, errors.New("unreadable public key")
	}
	for _, e := range ring {
		if e.PrivateKey != nil {
			return nil, errKeyHasPrivateMaterial
		}
		for _, sub := range e.Subkeys {
			if sub.PrivateKey != nil {
				return nil, errKeyHasPrivateMaterial
			}
		}
	}
	return ring[0], nil
}

// errKeyHasPrivateMaterial is readPublicEntity's refusal of a "public" key with
// secret packets in it. A sentinel so Decrypt's signer list can refuse the whole
// request on it (a caller bug or worse) while merely skipping an unreadable key.
var errKeyHasPrivateMaterial = errors.New("key carries private material")

// plausibleAddress reports whether a recipient address is safe to echo in an error
// message and a log line: non-empty, bounded, printable, no spaces or control
// characters (it names a recipient; it is never parsed).
func plausibleAddress(a string) bool {
	if a == "" || len(a) > 320 {
		return false
	}
	for _, r := range a {
		if r <= 0x20 || r == 0x7f || r == ',' {
			return false
		}
	}
	return true
}
