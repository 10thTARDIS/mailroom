// SPDX-License-Identifier: AGPL-3.0-only

//go:build integration

package api

// A signature embedded IN the encryption (RFC 3156 section 6.2, "combined", what
// Thunderbird and GnuPG send; the #47 rc.2 finding), through Email/get, the REAL
// crypto service (pgpkeyd.Service, in process) and a real database: the keys are
// selected here, checked there, and judged by the same classifySignature as a
// multipart/signed message (ADR-039 section 6).
//
// Every message is made by go-crypto in the test: openpgp.Encrypt with a signer
// (the library's own combined writer), or the library's packet serializers for
// what that writer refuses to make (a signature over other bytes, a SHA-1 one).

import (
	"bytes"
	"context"
	stdcrypto "crypto"
	"crypto/rand"
	"io"
	"strings"
	"testing"
	"time"

	"github.com/ProtonMail/go-crypto/openpgp"
	"github.com/ProtonMail/go-crypto/openpgp/armor"
	"github.com/ProtonMail/go-crypto/openpgp/packet"
	"github.com/oklog/ulid/v2"

	"mailroom/internal/pgpkeyd"
	"mailroom/internal/pgpwire"
	"mailroom/internal/store"
)

// svcDecrypter is the production adapter's shape (cmd/mailroom/decrypter.go) over
// the in-process crypto service.
type svcDecrypter struct {
	svc   *pgpkeyd.Service
	calls *[][]byte // the signer lists sent, newest last
}

func (d svcDecrypter) Decrypt(ctx context.Context, userID, messageID ulid.ULID, purpose string, ct []byte, signers [][]byte) (DecryptResult, error) {
	if d.calls != nil {
		*d.calls = append(*d.calls, bytes.Join(signers, nil))
	}
	req := pgpwire.DecryptRequest{UserID: userID.String(), MessageID: messageID.String(),
		Purpose: pgpwire.DecryptPurpose(purpose), Ciphertext: ct}
	for _, s := range signers {
		req.Signers = append(req.Signers, pgpwire.DecryptSigner{PublicKey: s})
	}
	res, err := d.svc.Decrypt(ctx, req)
	if err != nil {
		return DecryptResult{}, err
	}
	issuer := res.Signature.IssuerFingerprint
	if issuer == "" {
		issuer = res.Signature.IssuerKeyID
	}
	return DecryptResult{Entity: res.Entity, Signature: EmbeddedSignature{
		Signed: res.Signature.Signed, Status: string(res.Signature.Status),
		SignerFingerprint: res.Signature.SignerFingerprint, Issuer: issuer,
	}}, nil
}

// withCryptoDecrypter wires the real crypto service as the decrypter and returns
// the user's own public key (the one me@example.com mail is encrypted to).
func (h *encHarness) withCryptoDecrypter(t *testing.T) openpgp.EntityList {
	t.Helper()
	h.api.decrypter = svcDecrypter{svc: h.svc}
	ring, err := openpgp.ReadKeyRing(bytes.NewReader(h.senderPub))
	if err != nil || len(ring) != 1 {
		t.Fatalf("user key: %v", err)
	}
	return ring
}

// combined encrypts entity to `to`, signed by signer inside the encryption
// (section 6.2), armored.
func combined(t *testing.T, to openpgp.EntityList, signer *openpgp.Entity, entity string) []byte {
	t.Helper()
	var ct bytes.Buffer
	aw, err := armor.Encode(&ct, "PGP MESSAGE", nil)
	if err != nil {
		t.Fatal(err)
	}
	w, err := openpgp.Encrypt(aw, to, signer, &openpgp.FileHints{IsBinary: true}, nil)
	if err != nil {
		t.Fatal(err)
	}
	_, _ = w.Write([]byte(entity))
	_ = w.Close()
	_ = aw.Close()
	return ct.Bytes()
}

// craftedCombined is a section 6.2 message from go-crypto's packet serializers:
// the signature (hash h) is over signedOver while the literal carries literal.
func craftedCombined(t *testing.T, to *openpgp.Entity, signer *openpgp.Entity, h stdcrypto.Hash, signedOver, literal string) []byte {
	t.Helper()
	encKey, ok := to.EncryptionKey(time.Now())
	if !ok {
		t.Fatal("recipient has no encryption key")
	}
	priv := signer.PrivateKey
	off := false
	cfg := &packet.Config{NonDeterministicSignaturesViaNotation: &off} // no salt length for SHA-1
	sig := &packet.Signature{Version: priv.Version, SigType: packet.SigTypeBinary, PubKeyAlgo: priv.PubKeyAlgo,
		Hash: h, CreationTime: time.Now(), IssuerKeyId: &priv.KeyId, IssuerFingerprint: priv.Fingerprint}
	hasher, err := sig.PrepareSign(cfg)
	if err != nil {
		t.Fatal(err)
	}
	hasher.Write([]byte(signedOver))
	if err := sig.Sign(hasher, priv, cfg); err != nil {
		t.Fatal(err)
	}
	sk := make([]byte, packet.CipherAES128.KeySize())
	if _, err := rand.Read(sk); err != nil {
		t.Fatal(err)
	}
	var bin bytes.Buffer
	if err := packet.SerializeEncryptedKey(&bin, encKey.PublicKey, packet.CipherAES128, sk, nil); err != nil {
		t.Fatal(err)
	}
	seipd, err := packet.SerializeSymmetricallyEncrypted(&bin, packet.CipherAES128, false, packet.CipherSuite{}, sk, nil)
	if err != nil {
		t.Fatal(err)
	}
	ops := &packet.OnePassSignature{Version: 3, SigType: packet.SigTypeBinary, Hash: h,
		PubKeyAlgo: priv.PubKeyAlgo, KeyId: priv.KeyId, IsLast: true}
	if err := ops.Serialize(seipd); err != nil {
		t.Fatal(err)
	}
	lit, err := packet.SerializeLiteral(keepOpen{seipd}, true, "", 0) // keep seipd open for the signature
	if err != nil {
		t.Fatal(err)
	}
	_, _ = lit.Write([]byte(literal))
	_ = lit.Close()
	if err := sig.Serialize(seipd); err != nil {
		t.Fatal(err)
	}
	if err := seipd.Close(); err != nil {
		t.Fatal(err)
	}
	var ct bytes.Buffer
	aw, _ := armor.Encode(&ct, "PGP MESSAGE", nil)
	_, _ = aw.Write(bin.Bytes())
	_ = aw.Close()
	return ct.Bytes()
}

type keepOpen struct{ io.Writer }

func (keepOpen) Close() error { return nil }

// fromAliceEncrypted wraps an armored OpenPGP message as RFC 3156 §4 mail from alice.
func fromAliceEncrypted(ct []byte) []byte {
	return []byte("From: Alice <alice@example.com>\r\nTo: Me <me@example.com>\r\nSubject: figures\r\n" +
		"MIME-Version: 1.0\r\n" +
		"Content-Type: multipart/encrypted; boundary=\"enc\"; protocol=\"application/pgp-encrypted\"\r\n\r\n" +
		"--enc\r\nContent-Type: application/pgp-encrypted\r\n\r\nVersion: 1\r\n" +
		"--enc\r\nContent-Type: application/octet-stream; name=\"encrypted.asc\"\r\n\r\n" +
		string(ct) + "\r\n--enc--\r\n")
}

const embeddedEntity = "Content-Type: text/plain; charset=utf-8\r\n\r\n" + sigBodyText

func TestEmbeddedSignatureVerifiedByAnEligibleKeyAndPinned(t *testing.T) {
	h := newEncHarness(t)
	me := h.withCryptoDecrypter(t)
	alice := newPeerKey(t, "alice@example.com", nil)
	fpr := h.seedKey(t, "alice@example.com", pubOf(t, alice), store.ContactKeyWKD, false, time.Now())

	got := h.sigOf(t, fromAliceEncrypted(combined(t, me, alice, embeddedEntity)))
	wantState(t, got, "verified")
	if got["fingerprint"] != fpr || got["address"] != "alice@example.com" {
		t.Errorf("verified = %v, want fingerprint %s", got, fpr)
	}
	if pin, ok := h.pin(t, "alice@example.com"); !ok || pin != fpr {
		t.Errorf("pin after a first embedded verification = %q, %v; want %s", pin, ok, fpr)
	}
}

func TestEmbeddedSignatureByAKeyNotHeldIsUnknownKey(t *testing.T) {
	h := newEncHarness(t)
	me := h.withCryptoDecrypter(t)
	stranger := newPeerKey(t, "alice@example.com", nil)
	got := h.sigOf(t, fromAliceEncrypted(combined(t, me, stranger, embeddedEntity)))
	wantState(t, got, "unknownKey")
	if got["fingerprint"] != hexOf(stranger) {
		t.Errorf("unknownKey names %v; the signing key is %s", got["fingerprint"], hexOf(stranger))
	}
	if _, ok := h.pin(t, "alice@example.com"); ok {
		t.Error("an unknown key was pinned")
	}
}

func TestEmbeddedSignatureTamperedIsBadAndTheTextStillShows(t *testing.T) {
	h := newEncHarness(t)
	me := h.withCryptoDecrypter(t)
	alice := newPeerKey(t, "alice@example.com", nil)
	h.seedKey(t, "alice@example.com", pubOf(t, alice), store.ContactKeyManual, false, time.Now())
	swapped := "Content-Type: text/plain; charset=utf-8\r\n\r\npay mallory instead\r\n"

	// CONTROL: the same construction untampered verifies.
	wantState(t, h.sigOf(t, fromAliceEncrypted(craftedCombined(t, me[0], alice, stdcrypto.SHA256, embeddedEntity, embeddedEntity))), "verified")

	raw := fromAliceEncrypted(craftedCombined(t, me[0], alice, stdcrypto.SHA256, embeddedEntity, swapped))
	id := seedRawMessage(t, h.composeHarness, h.a, raw)
	res := call(t, h.api.emailGet, h.a.UserID, map[string]any{
		"ids": []string{id.String()}, "properties": []string{"id", "pgpSignature", "decrypted", "textBody", "bodyValues"},
		"fetchTextBodyValues": true,
	})
	obj := res["list"].([]any)[0].(map[string]any)
	sig, _ := obj["pgpSignature"].(map[string]any)
	wantState(t, sig, "bad")
	if obj["decrypted"] != true {
		t.Errorf("decrypted = %v; a bad signature must not undo the decryption", obj["decrypted"])
	}
	bv, _ := obj["bodyValues"].(map[string]any)
	var text string
	for _, v := range bv {
		text += v.(map[string]any)["value"].(string)
	}
	if !strings.Contains(text, "pay mallory instead") {
		t.Errorf("the text of a badly signed message is not shown: %v", bv)
	}
}

func TestEmbeddedSignatureByAHeldKeyThatDiffersFromThePinIsKeyChanged(t *testing.T) {
	h := newEncHarness(t)
	me := h.withCryptoDecrypter(t)
	old := newPeerKey(t, "alice@example.com", nil)
	oldFpr := h.seedKey(t, "alice@example.com", pubOf(t, old), store.ContactKeyWKD, false, time.Now().Add(-48*time.Hour))
	if err := h.setPin("alice@example.com", oldFpr); err != nil {
		t.Fatal(err)
	}
	fresh := newPeerKey(t, "alice@example.com", nil)
	freshFpr := h.seedKey(t, "alice@example.com", pubOf(t, fresh), store.ContactKeyWKD, false, time.Now())

	got := h.sigOf(t, fromAliceEncrypted(combined(t, me, fresh, embeddedEntity)))
	wantState(t, got, "keyChanged")
	if got["fingerprint"] != freshFpr || got["previousFingerprint"] != oldFpr {
		t.Errorf("keyChanged = %v; want fingerprint %s, previous %s", got, freshFpr, oldFpr)
	}
	// CONTROL: the pinned key verifies.
	wantState(t, h.sigOf(t, fromAliceEncrypted(combined(t, me, old, embeddedEntity))), "verified")
	if pin, _ := h.pin(t, "alice@example.com"); pin != oldFpr {
		t.Errorf("the pin moved to %s", pin)
	}
}

func TestEmbeddedSignatureUnauthenticatedAutocryptKeyIsNotVerified(t *testing.T) {
	h := newEncHarness(t)
	me := h.withCryptoDecrypter(t)
	planted := newPeerKey(t, "alice@example.com", nil)
	h.seedKey(t, "alice@example.com", pubOf(t, planted), store.ContactKeyAutocrypt, false, time.Now())
	// Sent as a candidate (so it is not "no key"), never verified.
	wantState(t, h.sigOf(t, fromAliceEncrypted(combined(t, me, planted, embeddedEntity))), "unknownKey")

	// With an eligible key held, the planted one is a change.
	real := newPeerKey(t, "alice@example.com", nil)
	realFpr := h.seedKey(t, "alice@example.com", pubOf(t, real), store.ContactKeyWKD, false, time.Now().Add(-time.Hour))
	got := h.sigOf(t, fromAliceEncrypted(combined(t, me, planted, embeddedEntity)))
	wantState(t, got, "keyChanged")
	if got["previousFingerprint"] != realFpr {
		t.Errorf("previousFingerprint = %v, want %s", got["previousFingerprint"], realFpr)
	}

	// CONTROL: the same kind of key from an AUTHENTICATED message is eligible.
	h2 := newEncHarness(t)
	me2 := h2.withCryptoDecrypter(t)
	vouched := newPeerKey(t, "alice@example.com", nil)
	h2.seedKey(t, "alice@example.com", pubOf(t, vouched), store.ContactKeyAutocrypt, true, time.Now())
	wantState(t, h2.sigOf(t, fromAliceEncrypted(combined(t, me2, vouched, embeddedEntity))), "verified")
}

func TestEmbeddedSignatureByAKeyThatDoesNotNameTheSenderIsUnknownKey(t *testing.T) {
	h := newEncHarness(t)
	me := h.withCryptoDecrypter(t)
	other := newPeerKey(t, "someone-else@example.com", nil)
	otherFpr := h.seedKey(t, "alice@example.com", pubOf(t, other), store.ContactKeyManual, false, time.Now())
	if err := h.setPin("alice@example.com", otherFpr); err != nil {
		t.Fatal(err)
	}
	got := h.sigOf(t, fromAliceEncrypted(combined(t, me, other, embeddedEntity)))
	wantState(t, got, "unknownKey")
	if got["fingerprint"] != otherFpr {
		t.Errorf("fingerprint = %v, want %s", got["fingerprint"], otherFpr)
	}
}

func TestEmbeddedSignatureSHA1IsBad(t *testing.T) {
	h := newEncHarness(t)
	me := h.withCryptoDecrypter(t)
	alice := newPeerKey(t, "alice@example.com", nil)
	h.seedKey(t, "alice@example.com", pubOf(t, alice), store.ContactKeyManual, false, time.Now())
	wantState(t, h.sigOf(t, fromAliceEncrypted(craftedCombined(t, me[0], alice, stdcrypto.SHA1, embeddedEntity, embeddedEntity))), "bad")
	if _, ok := h.pin(t, "alice@example.com"); ok {
		t.Error("a SHA-1 signature pinned a key")
	}
}

func TestEmbeddedSignatureUnsignedEncryptedIsNull(t *testing.T) {
	h := newEncHarness(t)
	me := h.withCryptoDecrypter(t)
	alice := newPeerKey(t, "alice@example.com", nil)
	h.seedKey(t, "alice@example.com", pubOf(t, alice), store.ContactKeyManual, false, time.Now())
	if got := h.sigOf(t, fromAliceEncrypted(combined(t, me, nil, embeddedEntity))); got != nil {
		t.Errorf("an unsigned encrypted message has pgpSignature %v", got)
	}
}

// PRECEDENCE: a decrypted entity that is itself multipart/signed is judged by that
// inner signature; the embedded one is not reported. Both directions, so the rule
// is not "whichever is better".
func TestEmbeddedSignaturePrecedenceInnerMultipartSignedWins(t *testing.T) {
	h := newEncHarness(t)
	me := h.withCryptoDecrypter(t)
	alice := newPeerKey(t, "alice@example.com", nil)
	aliceFpr := h.seedKey(t, "alice@example.com", pubOf(t, alice), store.ContactKeyManual, false, time.Now())
	stranger := newPeerKey(t, "alice@example.com", nil)
	inner := func(signer *openpgp.Entity) string {
		return string(signWith(t, signer, embeddedEntity))
	}

	// Inner by alice (held), embedded by a stranger: verified, alice's key.
	got := h.sigOf(t, fromAliceEncrypted(combined(t, me, stranger, inner(alice))))
	wantState(t, got, "verified")
	if got["fingerprint"] != aliceFpr {
		t.Errorf("fingerprint = %v, want the inner signer %s", got["fingerprint"], aliceFpr)
	}
	// Inner by the stranger, embedded by alice: unknownKey, the stranger's.
	got = h.sigOf(t, fromAliceEncrypted(combined(t, me, alice, inner(stranger))))
	wantState(t, got, "unknownKey")
	// CONTROL: alice's embedded signature alone does verify, so the line above is
	// the precedence rule and not a failure to see the embedded one.
	wantState(t, h.sigOf(t, fromAliceEncrypted(combined(t, me, alice, embeddedEntity))), "verified")
}

// The keys sent: those for the From address (all of them, eligible or not) and the
// user's own; never another contact's.
func TestEmbeddedSignatureCandidatesAreTheSendersKeys(t *testing.T) {
	h := newEncHarness(t)
	me := h.withCryptoDecrypter(t)
	var sent [][]byte
	h.api.decrypter = svcDecrypter{svc: h.svc, calls: &sent}
	alice := newPeerKey(t, "alice@example.com", nil)
	planted := newPeerKey(t, "alice@example.com", nil)
	bob := newPeerKey(t, "bob@example.com", nil)
	h.seedKey(t, "alice@example.com", pubOf(t, alice), store.ContactKeyWKD, false, time.Now())
	h.seedKey(t, "alice@example.com", pubOf(t, planted), store.ContactKeyAutocrypt, false, time.Now())
	h.seedKey(t, "bob@example.com", pubOf(t, bob), store.ContactKeyManual, false, time.Now())

	h.sigOf(t, fromAliceEncrypted(combined(t, me, alice, embeddedEntity)))
	if len(sent) != 1 {
		t.Fatalf("decrypt calls = %d", len(sent))
	}
	all := sent[0]
	for name, want := range map[string][]byte{"alice's eligible key": pubOf(t, alice),
		"alice's unauthenticated key": pubOf(t, planted), "the user's own key": h.senderPub} {
		if !bytes.Contains(all, want) {
			t.Errorf("%s was not a candidate", name)
		}
	}
	if bytes.Contains(all, pubOf(t, bob)) {
		t.Error("another contact's key was sent as a candidate")
	}

	// And no candidates at all when the signature was not asked for.
	sent = nil
	id := seedRawMessage(t, h.composeHarness, h.a, fromAliceEncrypted(combined(t, me, alice, embeddedEntity)))
	call(t, h.api.emailGet, h.a.UserID, map[string]any{"ids": []string{id.String()}, "properties": []string{"id", "textBody"}})
	if len(sent) != 1 || len(sent[0]) != 0 {
		t.Errorf("a body-only Email/get sent signer keys: %d calls", len(sent))
	}
}
