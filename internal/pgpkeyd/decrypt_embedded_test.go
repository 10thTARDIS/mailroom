// SPDX-License-Identifier: AGPL-3.0-only

package pgpkeyd

// Signatures embedded in the encryption (RFC 3156 section 6.2, "combined"), as
// Thunderbird and GnuPG send them (#47 rc.2 finding, ADR-039 section 6): one-pass
// signature, literal data and signature packets inside the SEIPD packet, checked
// by go-crypto in the same read as the decryption.
//
// Every message is built IN-TEST with go-crypto: openpgp.Encrypt with a signer
// (the library's own combined writer) where it can, and the library's packet
// serializers where it will not — a signature over different bytes than the
// literal (tampering that survives decryption) and a SHA-1 signature, which
// openpgp.Encrypt refuses to write.

import (
	"bytes"
	"context"
	stdcrypto "crypto"
	"crypto/rand"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"strings"
	"testing"
	"time"

	"github.com/ProtonMail/go-crypto/openpgp"
	"github.com/ProtonMail/go-crypto/openpgp/armor"
	"github.com/ProtonMail/go-crypto/openpgp/packet"

	"mailroom/internal/pgpverify"
	"mailroom/internal/pgpwire"
)

// The two weak-hash lists (multipart/signed in pgpverify, embedded here) are one
// policy and must not drift.
func TestWeakSignatureHashesMatchPgpverify(t *testing.T) {
	if len(weakSignatureHashes) != len(pgpverify.WeakHashes) {
		t.Fatalf("pgpkeyd refuses %d hashes, pgpverify %d", len(weakSignatureHashes), len(pgpverify.WeakHashes))
	}
	for h := range pgpverify.WeakHashes {
		if !weakSignatureHashes[h] {
			t.Errorf("pgpverify refuses %v, the crypto process does not", h)
		}
	}
}

const signedEntity = "Content-Type: text/plain; charset=utf-8\r\n\r\nsigned inside the encryption\r\n"

// publicOf serialises an entity's PUBLIC packets, binary, as contact_keys holds them.
func publicOf(t *testing.T, e *openpgp.Entity) []byte {
	t.Helper()
	var b bytes.Buffer
	if err := e.Serialize(&b); err != nil {
		t.Fatal(err)
	}
	return b.Bytes()
}

// signEncryptTo is openpgp.Encrypt with a signer: the library's own section 6.2
// writer (one-pass signature, literal, signature, inside SEIPD), armored.
func signEncryptTo(t *testing.T, to openpgp.EntityList, signer *openpgp.Entity, plain []byte) []byte {
	t.Helper()
	var buf bytes.Buffer
	aw, err := armor.Encode(&buf, "PGP MESSAGE", nil)
	if err != nil {
		t.Fatal(err)
	}
	w, err := openpgp.Encrypt(aw, to, signer, &openpgp.FileHints{IsBinary: true}, nil)
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

// craftedSignedEncrypted builds a section 6.2 message from go-crypto's packet
// serializers: PKESK to recipient, then SEIPD( one-pass signature, literal(literal),
// signature over signedOver with hash h ). signedOver != literal is the tamper
// case; h = SHA-1 the weak-hash case. sessionKey may be nil (random). hashIDPatch,
// if non-zero, overwrites the hash-algorithm octet of both the one-pass and the
// signature packet AFTER signing (for hashes the library cannot write).
// Returns binary packets.
func craftedSignedEncrypted(t *testing.T, recipient, signer *openpgp.Entity, h stdcrypto.Hash,
	signedOver, literal, sessionKey []byte, hashIDPatch byte) []byte {
	t.Helper()
	encKey, ok := recipient.EncryptionKey(time.Now())
	if !ok {
		t.Fatal("recipient has no usable encryption key")
	}
	cipherFunc := packet.CipherAES128
	if sessionKey == nil {
		sessionKey = make([]byte, cipherFunc.KeySize())
		if _, err := rand.Read(sessionKey); err != nil {
			t.Fatal(err)
		}
	}
	priv := signer.PrivateKey

	// The signature, made by the library exactly as weakhash_test.go's
	// detachSignWith does (salt notation off: it has no salt length for SHA-1).
	off := false
	cfg := &packet.Config{NonDeterministicSignaturesViaNotation: &off}
	sig := &packet.Signature{
		Version: priv.Version, SigType: packet.SigTypeBinary, PubKeyAlgo: priv.PubKeyAlgo, Hash: h,
		CreationTime: time.Now(), IssuerKeyId: &priv.KeyId, IssuerFingerprint: priv.Fingerprint,
	}
	hasher, err := sig.PrepareSign(cfg)
	if err != nil {
		t.Fatalf("PrepareSign: %v", err)
	}
	hasher.Write(signedOver)
	if err := sig.Sign(hasher, priv, cfg); err != nil {
		t.Fatalf("Sign: %v", err)
	}
	var opsBuf, sigBuf bytes.Buffer
	ops := &packet.OnePassSignature{Version: 3, SigType: packet.SigTypeBinary, Hash: h,
		PubKeyAlgo: priv.PubKeyAlgo, KeyId: priv.KeyId, IsLast: true}
	if err := ops.Serialize(&opsBuf); err != nil {
		t.Fatalf("one-pass signature: %v", err)
	}
	if err := sig.Serialize(&sigBuf); err != nil {
		t.Fatalf("signature: %v", err)
	}
	opsBytes, sigBytes := opsBuf.Bytes(), sigBuf.Bytes()
	if hashIDPatch != 0 {
		// New-format headers: the one-pass body is short (2-octet header); the
		// signature's header length depends on its body length.
		opsBytes[2+2] = hashIDPatch
		hl := 2
		switch {
		case sigBytes[1] >= 192 && sigBytes[1] < 224:
			hl = 3
		case sigBytes[1] == 255:
			hl = 6
		}
		sigBytes[hl+3] = hashIDPatch
	}

	var out bytes.Buffer
	if err := packet.SerializeEncryptedKey(&out, encKey.PublicKey, cipherFunc, sessionKey, nil); err != nil {
		t.Fatalf("PKESK: %v", err)
	}
	seipd, err := packet.SerializeSymmetricallyEncrypted(&out, cipherFunc, false, packet.CipherSuite{}, sessionKey, nil)
	if err != nil {
		t.Fatalf("SEIPD: %v", err)
	}
	if _, err := seipd.Write(opsBytes); err != nil {
		t.Fatal(err)
	}
	// The literal is closed through a no-op closer so that closing it does not
	// close the SEIPD writer before the signature packet is in (go-crypto's
	// packet writers close what they wrap).
	lit, err := packet.SerializeLiteral(nopWriteCloser{seipd}, true, "", 0)
	if err != nil {
		t.Fatalf("literal: %v", err)
	}
	if _, err := lit.Write(literal); err != nil {
		t.Fatal(err)
	}
	if err := lit.Close(); err != nil {
		t.Fatal(err)
	}
	if _, err := seipd.Write(sigBytes); err != nil {
		t.Fatal(err)
	}
	if err := seipd.Close(); err != nil { // writes the MDC
		t.Fatal(err)
	}
	return out.Bytes()
}

// decryptSigned calls Decrypt with candidate signer keys.
func decryptSigned(t *testing.T, h *harness, user string, ct []byte, signers ...[]byte) (*pgpwire.DecryptResponse, error) {
	t.Helper()
	req := pgpwire.DecryptRequest{UserID: user, MessageID: newMessageID(), Purpose: pgpwire.DecryptForBody, Ciphertext: ct}
	for _, s := range signers {
		req.Signers = append(req.Signers, pgpwire.DecryptSigner{PublicKey: s})
	}
	return h.client.Decrypt(context.Background(), req)
}

func mustDecryptSigned(t *testing.T, h *harness, user string, ct []byte, signers ...[]byte) *pgpwire.DecryptResponse {
	t.Helper()
	resp, err := decryptSigned(t, h, user, ct, signers...)
	if err != nil {
		t.Fatalf("Decrypt: %v", err)
	}
	return resp
}

// importedUser makes a user whose key the TEST holds, so it can also make
// messages encrypted to it with its own packet code.
func importedUser(t *testing.T, h *harness, email string) (string, *openpgp.Entity) {
	t.Helper()
	user := newUserID()
	local := newLocalEntity(t, email)
	if _, err := h.client.Import(context.Background(), pgpwire.ImportRequest{
		UserID: user, PrivateKey: armorPrivateEntity(t, local),
	}); err != nil {
		t.Fatal(err)
	}
	return user, local
}

func TestDecryptEmbeddedSignatureVerifiedByACandidate(t *testing.T) {
	h := startDaemon(t)
	user := newUserID()
	g := generate(t, h, user, "reader@example.com")
	alice := newLocalEntity(t, "alice@example.com")
	ct := signEncryptTo(t, pubRing(t, g.Key.PublicKey), alice, []byte(signedEntity))

	resp := mustDecryptSigned(t, h, user, ct, publicOf(t, alice))
	if string(resp.Entity) != signedEntity {
		t.Errorf("entity = %q", resp.Entity)
	}
	s := resp.Signature
	if !s.Signed || s.Status != pgpwire.EmbeddedVerified || s.SignerFingerprint != fingerprint(alice) {
		t.Fatalf("signature = %+v, want verified by %s", s, fingerprint(alice))
	}
	if s.IssuerFingerprint == "" && s.IssuerKeyID == "" {
		t.Errorf("a verified signature names no issuer: %+v", s)
	}

	// The same message with NO candidates: signed, by a key not offered. The
	// issuer is still named, so the display can show it.
	resp = mustDecryptSigned(t, h, user, ct)
	if s := resp.Signature; !s.Signed || s.Status != pgpwire.EmbeddedNoKey || s.SignerFingerprint != "" {
		t.Errorf("no candidates: signature = %+v, want noKey", s)
	} else if s.IssuerFingerprint != fingerprint(alice) {
		t.Errorf("noKey issuer = %q, want the signing key %s", s.IssuerFingerprint, fingerprint(alice))
	}
	if string(resp.Entity) != signedEntity {
		t.Error("a signature by an unknown key withheld the plaintext")
	}

	// A different candidate does not make it verified either.
	resp = mustDecryptSigned(t, h, user, ct, publicOf(t, newLocalEntity(t, "alice@example.com")))
	if resp.Signature.Status != pgpwire.EmbeddedNoKey {
		t.Errorf("another key as candidate: %+v, want noKey", resp.Signature)
	}
}

func TestDecryptEmbeddedSignatureUnsignedIsNotSigned(t *testing.T) {
	h := startDaemon(t)
	user := newUserID()
	g := generate(t, h, user, "reader@example.com")
	alice := newLocalEntity(t, "alice@example.com")
	ct := encryptTo(t, pubRing(t, g.Key.PublicKey), []byte(secretEntity), nil)
	resp := mustDecryptSigned(t, h, user, ct, publicOf(t, alice))
	if resp.Signature != (pgpwire.EmbeddedSignature{}) {
		t.Errorf("an unsigned message reports %+v, want {signed:false} and nothing else", resp.Signature)
	}
	// And on the wire it says signed:false explicitly.
	var raw json.RawMessage
	if err := h.client.Call(context.Background(), pgpwire.MethodDecrypt, pgpwire.DecryptRequest{
		UserID: user, MessageID: newMessageID(), Purpose: pgpwire.DecryptForBody, Ciphertext: ct,
	}, &raw); err != nil {
		t.Fatal(err)
	}
	if !bytes.Contains(raw, []byte(`"signature":{"signed":false}`)) {
		t.Errorf("wire shape for an unsigned message: %s", raw)
	}
}

// The tamper case: the signature is over A, the literal carries B, and the
// encryption is intact. Decryption succeeds and the plaintext comes back; the
// signature is bad. go-crypto only knows this at EOF.
func TestDecryptEmbeddedSignatureBadStillReturnsThePlaintext(t *testing.T) {
	h := startDaemon(t)
	user, rcpt := importedUser(t, h, "reader@example.com")
	alice := newLocalEntity(t, "alice@example.com")
	original := []byte("Content-Type: text/plain\r\n\r\npay 10 to alice\r\n")
	swapped := []byte("Content-Type: text/plain\r\n\r\npay 10000 to mallory\r\n")

	// CONTROL: the same construction, untampered, verifies — so the fixture is a
	// real signature and "bad" below is the swap's doing.
	good := craftedSignedEncrypted(t, rcpt, alice, stdcrypto.SHA256, original, original, nil, 0)
	if s := mustDecryptSigned(t, h, user, good, publicOf(t, alice)).Signature; s.Status != pgpwire.EmbeddedVerified {
		t.Fatalf("control: the untampered crafted message is %+v, want verified", s)
	}

	bad := craftedSignedEncrypted(t, rcpt, alice, stdcrypto.SHA256, original, swapped, nil, 0)
	resp, err := decryptSigned(t, h, user, bad, publicOf(t, alice))
	if err != nil {
		t.Fatalf("a bad signature made the DECRYPT fail (%v); the reader must still get the text", err)
	}
	if string(resp.Entity) != string(swapped) {
		t.Errorf("entity = %q, want the literal as sent", resp.Entity)
	}
	if s := resp.Signature; !s.Signed || s.Status != pgpwire.EmbeddedBad || s.SignerFingerprint != "" {
		t.Errorf("tampered: signature = %+v, want bad with no signer", s)
	}
	// Still audited as a decryption.
	audits := h.vault.Audits()
	if last := audits[len(audits)-1]; !last.Decrypted {
		t.Errorf("audit row for the bad-signature decrypt = %+v", last)
	}
}

// #301 C2, verify side. go-crypto's v1 reader accepts a SHA-1 signature; the
// daemon must not.
func TestDecryptEmbeddedSignatureRefusesSHA1(t *testing.T) {
	h := startDaemon(t)
	user, rcpt := importedUser(t, h, "reader@example.com")
	alice := newLocalEntity(t, "alice@example.com")
	ct := craftedSignedEncrypted(t, rcpt, alice, stdcrypto.SHA1, []byte(signedEntity), []byte(signedEntity), nil, 0)

	// THE LIBRARY HALF, measured rather than assumed: go-crypto's own reader
	// verifies this SHA-1 signature as good. If it ever starts refusing it, this
	// goes red and the comment on weakSignatureHashes should be revisited.
	md, err := openpgp.ReadMessage(bytes.NewReader(ct), openpgp.EntityList{rcpt, alice}, nil, nil)
	if err != nil {
		t.Fatalf("fixture: go-crypto cannot read the SHA-1 message: %v", err)
	}
	if _, err := io.ReadAll(md.UnverifiedBody); err != nil {
		t.Fatal(err)
	}
	if md.SignedBy == nil || md.Signature == nil || md.SignatureError != nil || md.Signature.Hash != stdcrypto.SHA1 {
		t.Fatalf("fixture: go-crypto did not verify a SHA-1 signature (signedBy=%v err=%v); "+
			"the refusal below would not be ours", md.SignedBy != nil, md.SignatureError)
	}

	resp := mustDecryptSigned(t, h, user, ct, publicOf(t, alice))
	if s := resp.Signature; s.Status != pgpwire.EmbeddedBad {
		t.Errorf("SHA-1 signature = %+v, want bad", s)
	}
	if string(resp.Entity) != signedEntity {
		t.Error("a refused hash withheld the plaintext")
	}
}

// MD5 and RIPEMD-160 cannot be written by go-crypto, so the hash octet is patched
// after signing. Whatever the library then does, the result must not be verified.
// Measured: it skips the packets as unsupported and the message reads as unsigned.
func TestDecryptEmbeddedSignatureMD5IsNeverVerified(t *testing.T) {
	h := startDaemon(t)
	user, rcpt := importedUser(t, h, "reader@example.com")
	alice := newLocalEntity(t, "alice@example.com")
	for _, id := range []byte{1 /* MD5 */, 3 /* RIPEMD-160 */} {
		ct := craftedSignedEncrypted(t, rcpt, alice, stdcrypto.SHA256, []byte(signedEntity), []byte(signedEntity), nil, id)
		resp, err := decryptSigned(t, h, user, ct, publicOf(t, alice))
		if err != nil {
			assertOpaque(t, "patched hash", resp, err)
			continue
		}
		if resp.Signature.Status == pgpwire.EmbeddedVerified {
			t.Errorf("hash id %d: reported verified", id)
		}
		t.Logf("hash id %d: signature = %+v", id, resp.Signature)
	}
}

// The split keyring: the user's own PRIVATE key decrypts but is not a signer
// candidate unless the application offers its public half.
func TestDecryptEmbeddedSignatureOwnKeyIsOnlyACandidateWhenOffered(t *testing.T) {
	h := startDaemon(t)
	user, me := importedUser(t, h, "me@example.com")
	ct := signEncryptTo(t, openpgp.EntityList{me}, me, []byte(signedEntity))

	if s := mustDecryptSigned(t, h, user, ct).Signature; s.Status != pgpwire.EmbeddedNoKey {
		t.Errorf("own key, not offered: %+v, want noKey (the decryption ring must not vouch)", s)
	}
	if s := mustDecryptSigned(t, h, user, ct, publicOf(t, me)).Signature; s.Status != pgpwire.EmbeddedVerified ||
		s.SignerFingerprint != fingerprint(me) {
		t.Errorf("own key, offered: %+v, want verified", s)
	}
}

func TestDecryptRefusesBadSignerLists(t *testing.T) {
	h := startDaemon(t)
	user, rcpt := importedUser(t, h, "reader@example.com")
	alice := newLocalEntity(t, "alice@example.com")
	ct := signEncryptTo(t, openpgp.EntityList{rcpt}, alice, []byte(signedEntity))
	before := len(h.vault.Audits())

	refuse := func(label string, signers ...[]byte) {
		t.Helper()
		resp, err := decryptSigned(t, h, user, ct, signers...)
		var werr *pgpwire.Error
		if err == nil || !errors.As(err, &werr) || werr.Code != pgpwire.CodeInvalidRequest {
			t.Errorf("%s: got %v (resp %v), want invalid_request", label, err, resp != nil)
		}
	}
	refuse("a private key as signer", []byte(armorPrivateEntity(t, alice)))
	refuse("private key among public ones", publicOf(t, alice), []byte(armorPrivateEntity(t, newLocalEntity(t, "x@example.com"))))
	many := make([][]byte, pgpwire.MaxDecryptSigners+1)
	for i := range many {
		many[i] = publicOf(t, alice)
	}
	refuse("over the count", many...)
	big := bytes.Repeat([]byte{0x99}, 2*pgpwire.MaxRecipientKeyBytes+1)
	refuse("one key over its bound", big)
	chunk := bytes.Repeat([]byte{0x99}, pgpwire.MaxRecipientKeyBytes)
	refuse("over the total", chunk, chunk, chunk, chunk, chunk)

	if after := len(h.vault.Audits()); after != before {
		t.Errorf("a refused signer list reached the keys: %d audit rows written", after-before)
	}

	// An UNREADABLE key is skipped, not fatal: the good one still verifies.
	if s := mustDecryptSigned(t, h, user, ct, []byte("junk"), publicOf(t, alice)).Signature; s.Status != pgpwire.EmbeddedVerified {
		t.Errorf("junk + good: %+v, want verified", s)
	}
}

// The request bound still holds the largest ciphertext once signers ride along.
func TestDecryptRequestBoundCoversSigners(t *testing.T) {
	b64 := func(n int) int { return (n + 2) / 3 * 4 }
	armored := pgpwire.MaxDecryptedBytes * 135 / 100
	need := b64(armored) + b64(pgpwire.MaxDecryptSignerBytes) + pgpwire.MaxDecryptSigners*64 + 4096
	if need > pgpwire.MaxDecryptRequestBytes {
		t.Errorf("a maximal decrypt request needs %d bytes; MaxDecryptRequestBytes is %d", need, pgpwire.MaxDecryptRequestBytes)
	}
}

// Checklist item 6 for the signed path: a SUCCEEDING signed decrypt (and a bad
// one) leak neither the session key nor the private key, nor library text.
func TestDecryptEmbeddedSignatureLeaksNothing(t *testing.T) {
	var logBuf syncBuffer
	h := startDaemonWithLogger(t, slog.New(slog.NewTextHandler(&logBuf, &slog.HandlerOptions{Level: slog.LevelDebug})))
	user := newUserID()
	local := newLocalEntity(t, "leak@example.com")
	imp, err := h.client.Import(context.Background(), pgpwire.ImportRequest{UserID: user, PrivateKey: armorPrivateEntity(t, local)})
	if err != nil {
		t.Fatal(err)
	}
	det := newLeakDetector(t, h.unsealPrivate(t, imp.Key.KeyID), mustExportPublic(t, h, user, imp.Key.KeyID))
	alice := newLocalEntity(t, "alice@example.com")
	sk := sessionKey(t)

	var corpus []byte
	for _, tc := range []struct {
		over []byte
		want pgpwire.EmbeddedSignatureStatus
	}{
		{[]byte(signedEntity), pgpwire.EmbeddedVerified},
		{[]byte("something else"), pgpwire.EmbeddedBad},
	} {
		ct := craftedSignedEncrypted(t, local, alice, stdcrypto.SHA256, tc.over, []byte(signedEntity), sk, 0)
		var raw json.RawMessage
		if err := h.client.Call(context.Background(), pgpwire.MethodDecrypt, pgpwire.DecryptRequest{
			UserID: user, MessageID: newMessageID(), Purpose: pgpwire.DecryptForBody, Ciphertext: ct,
			Signers: []pgpwire.DecryptSigner{{PublicKey: publicOf(t, alice)}},
		}, &raw); err != nil {
			t.Fatalf("the probe decrypt failed, so nothing below examined a real decryption: %v", err)
		}
		var dr pgpwire.DecryptResponse
		if err := json.Unmarshal(raw, &dr); err != nil || dr.Signature.Status != tc.want {
			t.Fatalf("probe decrypt: %+v (%v), want %s — the signed path did not run as intended", dr.Signature, err, tc.want)
		}
		corpus = append(corpus, raw...)
	}
	logs := logBuf.Bytes()
	for name, body := range map[string][]byte{"responses": corpus, "daemon log": logs} {
		if leak := det.findLeak(body); leak != "" {
			t.Errorf("%s: private key material: %s", name, leak)
		}
		if sessionKeyIn(body, sk) {
			t.Errorf("%s: the SESSION KEY appears", name)
		}
		for _, libText := range []string{"openpgp:", "signature error", "unsupported hash", "hash tag", "structural error", "invalid signature"} {
			if strings.Contains(strings.ToLower(string(body)), libText) {
				t.Errorf("%s: library text %q leaked", name, libText)
			}
		}
	}
	if !strings.Contains(string(logs), "signature=bad") || !strings.Contains(string(logs), "signature=verified") {
		t.Fatalf("the log never recorded the signature outcomes; the log assertions are vacuous:\n%s", logs)
	}
}
