// SPDX-License-Identifier: AGPL-3.0-only

package pgpkeyd

// Key rotation, outgoing half (ADR-054 section 1): Generate with a certifier key
// makes the new key's user ID carry a certification by the old key, in the bytes
// that are stored. Driven through the real daemon on a real socket.
//
// PROBES (run on the way in, each red then reverted): deleting the ownership
// check in ownedKey turns TestGenerateCertifierConfusedDeputy red; deleting the
// certifierRefusal call turns the revoked/retired/expired cases red (the
// generation then fails instead of proceeding uncertified).

import (
	"bytes"
	"context"
	stdcrypto "crypto"
	"testing"
	"time"

	"github.com/ProtonMail/go-crypto/openpgp"
	"github.com/ProtonMail/go-crypto/openpgp/packet"

	"mailroom/internal/pgpkeys"
	"mailroom/internal/pgpwire"
)

func isCode(err error, want pgpwire.ErrorCode) bool {
	code, ok := pgpwire.CodeOf(err)
	return ok && code == want
}

func certifyGen(t *testing.T, h *harness, user, email string, certifier string) *pgpwire.GenerateResponse {
	t.Helper()
	res, err := h.client.Generate(context.Background(), pgpwire.GenerateRequest{
		UserID: user, Name: "Rot", Email: email, CertifierKeyID: certifier,
	})
	if err != nil {
		t.Fatalf("Generate: %v", err)
	}
	return res
}

func storedPublic(t *testing.T, h *harness, keyID string) []byte {
	t.Helper()
	rec, err := h.vault.Get(context.Background(), keyID)
	if err != nil {
		t.Fatal(err)
	}
	return rec.PublicKey
}

// thirdPartySigs returns the certification signatures in binary not made by its
// own primary key, read from the raw packets.
func thirdPartySigs(t *testing.T, binary []byte) []*packet.Signature {
	t.Helper()
	ring, err := openpgp.ReadKeyRing(bytes.NewReader(binary))
	if err != nil || len(ring) == 0 {
		t.Fatalf("ReadKeyRing: %v", err)
	}
	var out []*packet.Signature
	r := packet.NewReader(bytes.NewReader(binary))
	for {
		p, err := r.Next()
		if err != nil {
			return out
		}
		if s, ok := p.(*packet.Signature); ok && s.SigType >= packet.SigTypeGenericCert &&
			s.SigType <= packet.SigTypePositiveCert && !s.CheckKeyIdOrFingerprint(ring[0].PrimaryKey) {
			out = append(out, s)
		}
	}
}

func TestGenerateWithACertifierStoresACertifiedKey(t *testing.T) {
	h := startDaemon(t)
	user := newUserID()
	old := certifyGen(t, h, user, "rot@example.com", "")
	if old.Certified || old.CertifyReason != "" {
		t.Errorf("no certifier named: certified=%v reason=%q", old.Certified, old.CertifyReason)
	}
	if n := len(thirdPartySigs(t, storedPublic(t, h, old.Key.KeyID))); n != 0 {
		t.Fatalf("control: an uncertified key carries %d certifications", n)
	}

	res := certifyGen(t, h, user, "rot@example.com", old.Key.KeyID)
	if !res.Certified || res.CertifyReason != "" {
		t.Fatalf("certified=%v reason=%q, want certified", res.Certified, res.CertifyReason)
	}
	pub := storedPublic(t, h, res.Key.KeyID)
	oldPub := storedPublic(t, h, old.Key.KeyID)
	if !pgpkeys.CertifiedBy(pub, oldPub, "rot@example.com") {
		t.Fatal("the STORED bytes carry no certification that verifies under the old public key")
	}
	sigs := thirdPartySigs(t, pub)
	if len(sigs) != 1 {
		t.Fatalf("%d certifications, want 1", len(sigs))
	}
	s := sigs[0]
	if s.SigType != packet.SigTypeGenericCert {
		t.Errorf("type %#x, want 0x10", s.SigType)
	}
	if s.Hash != stdcrypto.SHA256 {
		t.Errorf("hash %d, want SHA-256", s.Hash)
	}
	if s.SigLifetimeSecs != nil && *s.SigLifetimeSecs != 0 {
		t.Error("the certification expires; it must not")
	}
	if len(s.IssuerFingerprint) != 20 {
		t.Error("no issuer fingerprint subpacket")
	}
	if !s.CreationTime.Equal(res.Key.CreatedAt.Truncate(time.Second)) && s.CreationTime.Sub(res.Key.CreatedAt).Abs() > 2*time.Second {
		t.Errorf("certification dated %v, key created %v", s.CreationTime, res.Key.CreatedAt)
	}
	// The key is otherwise the ordinary new key: still a valid, usable key.
	if _, err := pgpkeys.Assess(pub, time.Now()); err != nil {
		t.Errorf("certified key unreadable: %v", err)
	}
	// And the certification is NOT verifiable under an unrelated key.
	other := certifyGen(t, h, newUserID(), "other@example.com", "")
	if pgpkeys.CertifiedBy(pub, storedPublic(t, h, other.Key.KeyID), "rot@example.com") {
		t.Error("the certification verified under an unrelated key")
	}
}

// The confused deputy: the caller names a certifier that is another user's key.
func TestGenerateCertifierConfusedDeputy(t *testing.T) {
	h := startDaemon(t)
	alice, mallory := newUserID(), newUserID()
	aliceKey := certifyGen(t, h, alice, "alice@example.com", "")

	_, err := h.client.Generate(context.Background(), pgpwire.GenerateRequest{
		UserID: mallory, Email: "mallory@example.com", CertifierKeyID: aliceKey.Key.KeyID,
	})
	if !isCode(err, pgpwire.CodeDenied) {
		t.Fatalf("another user's certifier: err = %v, want denied", err)
	}
	// Nothing was minted for mallory by the refused call.
	if got := len(h.vault.records); got != 1 {
		t.Errorf("vault holds %d keys after a denied generate, want 1", got)
	}
	// POSITIVE CONTROL: the owner naming the same key is certified.
	if res := certifyGen(t, h, alice, "alice@example.com", aliceKey.Key.KeyID); !res.Certified {
		t.Error("control: the owner's certification was refused")
	}
	// A key id that does not exist is not_found, not a silent uncertified key.
	if _, err := h.client.Generate(context.Background(), pgpwire.GenerateRequest{
		UserID: alice, Email: "alice@example.com", CertifierKeyID: newUserID(),
	}); !isCode(err, pgpwire.CodeNotFound) {
		t.Errorf("unknown certifier: err = %v, want not_found", err)
	}
}

// A certifier that can no longer sign proceeds UNCERTIFIED with a reason; the new
// key is still made and stored. Each has the certified control above.
func TestGenerateCertifierThatCannotSignProceedsUncertified(t *testing.T) {
	cases := []struct {
		name   string
		break_ func(t *testing.T, h *harness, user, keyID string)
		reason string
	}{
		{"revoked", func(t *testing.T, h *harness, user, keyID string) {
			rec, _ := h.vault.Get(context.Background(), keyID)
			if _, err := h.client.Revoke(context.Background(), pgpwire.RevokeRequest{
				UserID: user, KeyID: keyID, Reason: pgpwire.RevokeSuperseded, Confirm: fingerprintSuffix(rec.Fingerprint),
			}); err != nil {
				t.Fatal(err)
			}
		}, pgpwire.CertifyReasonOldKeyRevoked},
		{"retired", func(t *testing.T, h *harness, user, keyID string) {
			h.mutateRecord(t, keyID, func(r *Record) { r.DecryptOnly = true })
		}, pgpwire.CertifyReasonOldKeyRetired},
		{"expired", func(t *testing.T, h *harness, user, keyID string) {
			later := time.Now().Add(3 * 24 * time.Hour)
			h.svc.now = func() time.Time { return later }
			t.Cleanup(func() { h.svc.now = time.Now })
		}, pgpwire.CertifyReasonOldKeyExpired},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			h := startDaemon(t)
			user := newUserID()
			one := 1
			old, err := h.client.Generate(context.Background(), pgpwire.GenerateRequest{
				UserID: user, Email: "rot@example.com", ExpiresDays: &one,
			})
			if err != nil {
				t.Fatal(err)
			}
			// Control, before breaking it: this very key certifies.
			if ctl := certifyGen(t, h, user, "rot@example.com", old.Key.KeyID); !ctl.Certified {
				t.Fatalf("control: not certified (%q)", ctl.CertifyReason)
			}
			c.break_(t, h, user, old.Key.KeyID)
			res := certifyGen(t, h, user, "rot@example.com", old.Key.KeyID)
			if res.Certified || res.CertifyReason != c.reason {
				t.Errorf("certified=%v reason=%q, want uncertified with %q", res.Certified, res.CertifyReason, c.reason)
			}
			if n := len(thirdPartySigs(t, storedPublic(t, h, res.Key.KeyID))); n != 0 {
				t.Errorf("the key carries %d certifications although none was made", n)
			}
		})
	}
}
