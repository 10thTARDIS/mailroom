// SPDX-License-Identifier: AGPL-3.0-only

package main

// The adapter that lets Email/get decrypt without knowing the crypto process
// exists (#47 increment 6b, ADR-037), mirroring signer.go: internal/api declares
// a one-method Decrypter interface, and the pgpwire client lives here.

import (
	"context"
	"log/slog"

	"github.com/oklog/ulid/v2"

	"mailroom/internal/api"
	"mailroom/internal/pgpwire"
)

// cryptoDecrypter adapts *pgpwire.Client to api.Decrypter.
type cryptoDecrypter struct {
	client *pgpwire.Client
}

// newCryptoDecrypter returns a decrypter for the crypto process at socketPath,
// or nil when no socket is configured — the same "nil is an answer" shape as
// newCryptoSigner. With none, encrypted mail is shown as stored, marked
// `encrypted: true, decrypted: false`.
func newCryptoDecrypter(socketPath string, log *slog.Logger) *cryptoDecrypter {
	if socketPath == "" {
		log.Info("encrypted mail will NOT be decrypted on open: no crypto socket configured")
		return nil
	}
	log.Info("encrypted mail will be decrypted on open, in memory only (ADR-037)",
		"crypto_socket", socketPath)
	return &cryptoDecrypter{client: pgpwire.NewClient(socketPath)}
}

// Decrypt asks the crypto process to open one PGP/MIME encrypted part.
//
// ⚠ The returned entity is PLAINTEXT and must never be persisted by any caller
// (ADR-037). The error is passed through unclassified: every cryptographic
// failure is already one opaque code, and the API layer shows "could not be
// decrypted" for all of them.
func (d *cryptoDecrypter) Decrypt(ctx context.Context, userID, messageID ulid.ULID, purpose string, ciphertext []byte, signers [][]byte) (api.DecryptResult, error) {
	callCtx, cancel := context.WithTimeout(ctx, pgpwire.DefaultTimeout)
	defer cancel()
	req := pgpwire.DecryptRequest{
		UserID:     userID.String(),
		MessageID:  messageID.String(),
		Purpose:    pgpwire.DecryptPurpose(purpose),
		Ciphertext: ciphertext,
	}
	for _, k := range signers {
		req.Signers = append(req.Signers, pgpwire.DecryptSigner{PublicKey: k})
	}
	res, err := d.client.Decrypt(callCtx, req)
	if err != nil {
		return api.DecryptResult{}, err
	}
	return api.DecryptResult{Entity: res.Entity, Signature: embeddedFromWire(res.Signature)}, nil
}

// embeddedFromWire translates the crypto process's section 6.2 result into the
// API's terms. Issuer prefers the fingerprint and falls back to the key id.
func embeddedFromWire(s pgpwire.EmbeddedSignature) api.EmbeddedSignature {
	issuer := s.IssuerFingerprint
	if issuer == "" {
		issuer = s.IssuerKeyID
	}
	return api.EmbeddedSignature{
		Signed:                   s.Signed,
		Status:                   string(s.Status),
		SignerFingerprint:        s.SignerFingerprint,
		Issuer:                   issuer,
		KeyExpired:               s.KeyExpired,
		ExpiredSignerFingerprint: s.ExpiredSignerFingerprint,
		SignedAt:                 s.SignedAt,
		KeyExpiredAt:             s.KeyExpiredAt,
	}
}
