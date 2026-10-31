// SPDX-License-Identifier: AGPL-3.0-only

package pgpkeyd

// A signature inside the encryption (RFC 3156 section 6.2) by a key that has
// since EXPIRED (#795/#796): still `bad` (ADR-039 §6), with key_expired and the
// dates, so the application can say "signed with a key that has expired". Real
// go-crypto keys with real lifetimes; the message is written by go-crypto's own
// combined writer at a time the signer was valid.

import (
	"bytes"
	"context"
	"testing"
	"time"

	"github.com/ProtonMail/go-crypto/openpgp"
	"github.com/ProtonMail/go-crypto/openpgp/armor"
	"github.com/ProtonMail/go-crypto/openpgp/packet"

	"mailroom/internal/pgpwire"
)

func entityAt(t *testing.T, email string, created time.Time, lifetime time.Duration) *openpgp.Entity {
	t.Helper()
	e, err := openpgp.NewEntity("Dated", "", email, &packet.Config{
		Algorithm: packet.PubKeyAlgoEdDSA, Curve: packet.Curve25519,
		Time:            func() time.Time { return created },
		KeyLifetimeSecs: uint32(lifetime / time.Second),
	})
	if err != nil {
		t.Fatal(err)
	}
	return e
}

// signEncryptAt is signEncryptTo with the writer's clock set to at.
func signEncryptAt(t *testing.T, to *openpgp.Entity, signer *openpgp.Entity, plain []byte, at time.Time) []byte {
	t.Helper()
	var buf bytes.Buffer
	aw, err := armor.Encode(&buf, "PGP MESSAGE", nil)
	if err != nil {
		t.Fatal(err)
	}
	cfg := &packet.Config{Time: func() time.Time { return at }}
	w, err := openpgp.Encrypt(aw, openpgp.EntityList{to}, signer, &openpgp.FileHints{IsBinary: true}, cfg)
	if err != nil {
		t.Fatalf("encrypt+sign: %v", err)
	}
	if _, err := w.Write(plain); err != nil {
		t.Fatal(err)
	}
	if err := w.Close(); err != nil {
		t.Fatal(err)
	}
	if err := aw.Close(); err != nil {
		t.Fatal(err)
	}
	return buf.Bytes()
}

func TestDecryptEmbeddedSignatureByAnExpiredKeySaysSo(t *testing.T) {
	h := startDaemon(t)
	now := time.Now().UTC().Truncate(time.Second)
	// The reader's key predates everything and never expires; the signer's key
	// lived 30 days and died 10 days ago; the mail was written 35 days ago.
	reader := entityAt(t, "reader@example.com", now.Add(-50*24*time.Hour), 0)
	user := newUserID()
	if _, err := h.client.Import(context.Background(), pgpwire.ImportRequest{
		UserID: user, PrivateKey: armorPrivateEntity(t, reader),
	}); err != nil {
		t.Fatal(err)
	}
	created := now.Add(-40 * 24 * time.Hour)
	alice := entityAt(t, "alice@example.com", created, 30*24*time.Hour)
	signedAt := now.Add(-35 * 24 * time.Hour)
	ct := signEncryptAt(t, reader, alice, []byte(signedEntity), signedAt)

	resp := mustDecryptSigned(t, h, user, ct, publicOf(t, alice))
	if string(resp.Entity) != signedEntity {
		t.Fatalf("an expired signer withheld the plaintext: %q", resp.Entity)
	}
	s := resp.Signature
	if !s.Signed || s.Status != pgpwire.EmbeddedBad {
		t.Fatalf("signature = %+v, want bad (ADR-039 §6)", s)
	}
	if !s.KeyExpired || s.ExpiredSignerFingerprint != fingerprint(alice) {
		t.Fatalf("signature = %+v, want key_expired by %s", s, fingerprint(alice))
	}
	if s.SignedAt == nil || !s.SignedAt.Equal(signedAt) {
		t.Errorf("signed_at = %v, want %v", s.SignedAt, signedAt)
	}
	if want := created.Add(30 * 24 * time.Hour); s.KeyExpiredAt == nil || !s.KeyExpiredAt.Equal(want) {
		t.Errorf("key_expired_at = %v, want %v", s.KeyExpiredAt, want)
	}
	if s.SignerFingerprint != "" {
		t.Errorf("signer_fingerprint = %q; it is reserved for a verified signature", s.SignerFingerprint)
	}

	// Control: the same signer, live (a key that does not expire), is verified
	// and carries no expiry reason, so the flag above is about the date.
	live := entityAt(t, "alice@example.com", created, 0)
	ct = signEncryptAt(t, reader, live, []byte(signedEntity), signedAt)
	if s := mustDecryptSigned(t, h, user, ct, publicOf(t, live)).Signature; s.Status != pgpwire.EmbeddedVerified || s.KeyExpired {
		t.Fatalf("live signer: %+v, want verified without key_expired", s)
	}
}
