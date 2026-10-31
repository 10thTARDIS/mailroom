// SPDX-License-Identifier: AGPL-3.0-only

package pgpkeyd

// Tests for the encrypt verb (#47, ADR-038). The cryptography is go-crypto's; what
// these prove is the policy around it: who is a recipient, what counts as a usable
// key, what is refused and how, and that the output opens for exactly the people
// it should — including the sender, through the real decrypt verb.

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"strings"
	"testing"
	"time"

	"github.com/ProtonMail/go-crypto/openpgp"
	"github.com/ProtonMail/go-crypto/openpgp/armor"
	"github.com/ProtonMail/go-crypto/openpgp/packet"

	"mailroom/internal/pgpmime"
	"mailroom/internal/pgpverify"
	"mailroom/internal/pgpwire"
)

// publicBinary serialises an entity's PUBLIC packets.
func publicBinary(t *testing.T, e *openpgp.Entity) []byte {
	t.Helper()
	var buf bytes.Buffer
	if err := e.Serialize(&buf); err != nil {
		t.Fatalf("serialize public key: %v", err)
	}
	return buf.Bytes()
}

// publicArmored is publicBinary, armored.
func publicArmored(t *testing.T, e *openpgp.Entity) []byte {
	t.Helper()
	var buf bytes.Buffer
	aw, err := armor.Encode(&buf, openpgp.PublicKeyType, nil)
	if err != nil {
		t.Fatal(err)
	}
	if err := e.Serialize(aw); err != nil {
		t.Fatal(err)
	}
	if err := aw.Close(); err != nil {
		t.Fatal(err)
	}
	return buf.Bytes()
}

func encryptAs(t *testing.T, h *harness, user, keyID string, entity string, to ...pgpwire.EncryptRecipient) (*pgpwire.EncryptResponse, error) {
	t.Helper()
	return h.client.Encrypt(context.Background(), pgpwire.EncryptRequest{
		UserID: user, KeyID: keyID, Recipients: to, Entity: []byte(entity),
	})
}

func rcpt(address string, key []byte) pgpwire.EncryptRecipient {
	return pgpwire.EncryptRecipient{Address: address, PublicKey: key}
}

// openWith decrypts an armored message with a key the TEST holds, through
// go-crypto directly (not the daemon): this is "the recipient's own client".
func openWith(t *testing.T, ciphertext string, key *openpgp.Entity) (string, error) {
	t.Helper()
	blk, err := armor.Decode(strings.NewReader(ciphertext))
	if err != nil {
		t.Fatalf("armor: %v", err)
	}
	md, err := openpgp.ReadMessage(blk.Body, openpgp.EntityList{key}, nil, nil)
	if err != nil {
		return "", err
	}
	b, err := io.ReadAll(md.UnverifiedBody)
	return string(b), err
}

// packetShape counts the session-key packets and reports whether the data packet
// is integrity protected.
func packetShape(t *testing.T, ciphertext string) (keys int, seipd bool) {
	t.Helper()
	blk, err := armor.Decode(strings.NewReader(ciphertext))
	if err != nil {
		t.Fatalf("armor: %v", err)
	}
	pkts := packet.NewReader(blk.Body)
	for {
		p, err := pkts.Next()
		if err != nil {
			return keys, false
		}
		switch v := p.(type) {
		case *packet.EncryptedKey:
			keys++
		case *packet.SymmetricallyEncrypted:
			return keys, v.IntegrityProtected
		}
	}
}

// ── The round trip: the recipient opens it, and so does the sender. ──

func TestEncryptRoundTripRecipientAndSender(t *testing.T) {
	h := startDaemon(t)
	user := newUserID()
	sender := generate(t, h, user, "me@example.com")
	bob := newLocalEntity(t, "bob@example.com")

	// Both key encodings a caller may pass: binary (what contact_keys stores) and
	// armored.
	for name, key := range map[string][]byte{"binary": publicBinary(t, bob), "armored": publicArmored(t, bob)} {
		resp, err := encryptAs(t, h, user, sender.Key.KeyID, secretEntity, rcpt("bob@example.com", key))
		if err != nil {
			t.Fatalf("%s: Encrypt: %v", name, err)
		}

		// THE RECIPIENT'S OWN CLIENT (go-crypto in the test, holding bob's key).
		got, err := openWith(t, resp.Ciphertext, bob)
		if err != nil || got != secretEntity {
			t.Fatalf("%s: the recipient could not open it: %q %v", name, got, err)
		}

		// THE SENT COPY: the sender reads the SAME ciphertext through the real
		// decrypt verb. This is the self-recipient doing its job.
		dec, err := decryptAs(t, h, user, []byte(resp.Ciphertext))
		if err != nil || string(dec.Entity) != secretEntity {
			t.Fatalf("%s: the SENDER could not read their own sent copy: %v", name, err)
		}

		// Shape: two session-key packets (bob, self), integrity protected.
		if keys, seipd := packetShape(t, resp.Ciphertext); keys != 2 || !seipd {
			t.Errorf("%s: %d session-key packets (want 2), SEIPD=%v", name, keys, seipd)
		}
		if resp.SelfFingerprint != sender.Key.Fingerprint {
			t.Errorf("%s: self fingerprint %s, want %s", name, resp.SelfFingerprint, sender.Key.Fingerprint)
		}
		if len(resp.To) != 1 || resp.To[0].Address != "bob@example.com" ||
			resp.To[0].Fingerprint != strings.ToUpper(hexFpr(bob)) {
			t.Errorf("%s: To = %+v", name, resp.To)
		}
		if strings.Contains(resp.Ciphertext, "quarterly numbers") {
			t.Errorf("%s: plaintext is visible in the ciphertext", name)
		}
	}
}

func hexFpr(e *openpgp.Entity) string {
	const digits = "0123456789abcdef"
	var b strings.Builder
	for _, c := range e.PrimaryKey.Fingerprint {
		b.WriteByte(digits[c>>4])
		b.WriteByte(digits[c&0xf])
	}
	return b.String()
}

// ── Self-recipient: the property the round trip depends on, pinned alone. ──

func TestEncryptWithoutTheSenderIsNotWhatWeProduce(t *testing.T) {
	h := startDaemon(t)
	user := newUserID()
	sender := generate(t, h, user, "me@example.com")
	bob := newLocalEntity(t, "bob@example.com")
	resp, err := encryptAs(t, h, user, sender.Key.KeyID, secretEntity, rcpt("bob@example.com", publicBinary(t, bob)))
	if err != nil {
		t.Fatal(err)
	}
	// CONTROL: the message really is bob's.
	if _, err := openWith(t, resp.Ciphertext, bob); err != nil {
		t.Fatalf("CONTROL FAILED: bob cannot open it: %v", err)
	}
	// THE ASSERTION: the sender can open it too. Dropping the self-recipient makes
	// this fail (the probe in the PR description).
	if _, err := decryptAs(t, h, user, []byte(resp.Ciphertext)); err != nil {
		t.Fatalf("the sender cannot read their own message: the self-recipient is missing: %v", err)
	}
}

// Writing to yourself: the recipient key IS the sender's key. One session-key
// packet, not two.
func TestEncryptToSelfIsDeduplicated(t *testing.T) {
	h := startDaemon(t)
	user := newUserID()
	sender := generate(t, h, user, "me@example.com")
	resp, err := encryptAs(t, h, user, sender.Key.KeyID, secretEntity,
		rcpt("me@example.com", []byte(sender.Key.PublicKey)))
	if err != nil {
		t.Fatal(err)
	}
	if keys, _ := packetShape(t, resp.Ciphertext); keys != 1 {
		t.Errorf("%d session-key packets for a message to oneself, want 1", keys)
	}
	if dec, err := decryptAs(t, h, user, []byte(resp.Ciphertext)); err != nil || string(dec.Entity) != secretEntity {
		t.Errorf("sender cannot open a message to themselves: %v", err)
	}
}

// ── Sign, then encrypt: the signature survives and verifies with pgpverify. ──

func TestEncryptedMessageIsSignedAndTheSignatureVerifies(t *testing.T) {
	h := startDaemon(t)
	user := newUserID()
	sender := generate(t, h, user, "me@example.com")
	bob := newLocalEntity(t, "bob@example.com")
	ctx := context.Background()

	raw := "From: me@example.com\r\nTo: bob@example.com\r\nSubject: hi\r\nMIME-Version: 1.0\r\n" +
		"Content-Type: text/plain; charset=utf-8\r\n\r\nthe quarterly numbers\r\n"

	// The application's two steps, exactly as compose.prepareOutbound does them.
	signed, err := pgpmime.Sign([]byte(raw), func(canon []byte) (string, string, error) {
		r, err := h.client.Sign(ctx, pgpwire.SignRequest{UserID: user, KeyID: sender.Key.KeyID, Data: canon})
		if err != nil {
			return "", "", err
		}
		return r.Signature, r.MicAlg, nil
	})
	if err != nil {
		t.Fatalf("Sign: %v", err)
	}
	enc, err := pgpmime.Encrypt(signed, func(entity []byte) (string, error) {
		r, err := h.client.Encrypt(ctx, pgpwire.EncryptRequest{
			UserID: user, KeyID: sender.Key.KeyID, Entity: entity,
			Recipients: []pgpwire.EncryptRecipient{rcpt("bob@example.com", publicBinary(t, bob))},
		})
		if err != nil {
			return "", err
		}
		return r.Ciphertext, nil
	})
	if err != nil {
		t.Fatalf("Encrypt: %v", err)
	}
	if !pgpmime.IsEncrypted(enc) {
		t.Fatal("the result is not an RFC 3156 multipart/encrypted")
	}
	// The outer headers stay readable; the body does not.
	if !bytes.Contains(enc, []byte("Subject: hi")) || bytes.Contains(enc, []byte("quarterly numbers")) {
		t.Fatal("outer headers must stay, and the body must not appear in the clear")
	}

	// RECIPIENT'S SIDE: split the framing, open it with bob's key, verify.
	ct, err := pgpmime.SplitEncrypted(enc)
	if err != nil {
		t.Fatalf("SplitEncrypted: %v", err)
	}
	entity, err := openWith(t, string(ct), bob)
	if err != nil {
		t.Fatalf("bob cannot open it: %v", err)
	}
	msg, err := pgpmime.Rewrap(enc, []byte(entity))
	if err != nil {
		t.Fatalf("Rewrap: %v", err)
	}
	senderPub, _ := armor.Decode(strings.NewReader(sender.Key.PublicKey))
	senderBin, _ := io.ReadAll(senderPub.Body)
	res := pgpverify.Verify(msg, []pgpverify.PublicKey{{Fingerprint: sender.Key.Fingerprint, Binary: senderBin}})
	if res.Verdict != pgpverify.VerdictVerified {
		t.Fatalf("the signature inside the encrypted message does not verify: %s (%v)", res.Verdict, res.Err)
	}

	// SENDER'S SIDE: the real decrypt verb, then the same verification.
	dec, err := decryptAs(t, h, user, ct)
	if err != nil {
		t.Fatalf("the sender cannot open their sent copy: %v", err)
	}
	msg2, _ := pgpmime.Rewrap(enc, dec.Entity)
	if res := pgpverify.Verify(msg2, []pgpverify.PublicKey{{Fingerprint: sender.Key.Fingerprint, Binary: senderBin}}); res.Verdict != pgpverify.VerdictVerified {
		t.Errorf("sent copy: %s (%v)", res.Verdict, res.Err)
	}

	// NEGATIVE CONTROL: the verdict can fail. Alter the decrypted body.
	tampered := bytes.Replace(msg, []byte("quarterly numbers"), []byte("quarterly NUMBERS"), 1)
	if bytes.Equal(tampered, msg) {
		t.Fatal("fixture: nothing to tamper with")
	}
	if res := pgpverify.Verify(tampered, []pgpverify.PublicKey{{Fingerprint: sender.Key.Fingerprint, Binary: senderBin}}); res.Verdict != pgpverify.VerdictBadSignature {
		t.Errorf("a tampered body verified as %s — the verdict cannot fail", res.Verdict)
	}
}

// ── Recipient keys that must be refused, naming the ADDRESS. ──

func TestEncryptRefusalNamesTheAddressNotTheKey(t *testing.T) {
	h := startDaemon(t)
	user := newUserID()
	sender := generate(t, h, user, "me@example.com")
	good := newLocalEntity(t, "good@example.com")
	goodKey := publicBinary(t, good)

	// CONTROL: the good key alone works, so the refusals below are about the bad
	// recipients and nothing else about the request.
	if _, err := encryptAs(t, h, user, sender.Key.KeyID, secretEntity, rcpt("good@example.com", goodKey)); err != nil {
		t.Fatalf("CONTROL FAILED: %v", err)
	}

	junk := []byte{0x99, 0x01, 0x02, 0x03}
	_, err := encryptAs(t, h, user, sender.Key.KeyID, secretEntity,
		rcpt("good@example.com", goodKey),
		rcpt("nokey@example.com", junk),
		rcpt("alsonone@example.com", []byte("not a key at all")))
	code, ok := pgpwire.CodeOf(err)
	if !ok || code != pgpwire.CodeNoRecipientKey {
		t.Fatalf("err = %v, want code %s", err, pgpwire.CodeNoRecipientKey)
	}
	msg := err.Error()
	for _, want := range []string{"nokey@example.com", "alsonone@example.com"} {
		if !strings.Contains(msg, want) {
			t.Errorf("the refusal does not name %s: %s", want, msg)
		}
	}
	if strings.Contains(msg, "good@example.com") {
		t.Errorf("the refusal names a recipient that was fine: %s", msg)
	}
	// No key material: neither the junk nor the good key (hex or text).
	if strings.Contains(msg, "99010203") || strings.Contains(msg, string(goodKey)) || strings.Contains(msg, "BEGIN PGP") {
		t.Errorf("the refusal carries key material: %s", msg)
	}
}

func TestEncryptSkipsRevokedExpiredAndEncryptionlessKeys(t *testing.T) {
	h := startDaemon(t)
	user := newUserID()
	sender := generate(t, h, user, "me@example.com")

	// Revoked primary key.
	revoked := newLocalEntity(t, "revoked@example.com")
	revokedCtl := publicBinary(t, revoked) // before revocation: usable
	if _, err := encryptAs(t, h, user, sender.Key.KeyID, secretEntity, rcpt("revoked@example.com", revokedCtl)); err != nil {
		t.Fatalf("CONTROL FAILED: the key is refused before it is revoked: %v", err)
	}
	if err := revoked.RevokeKey(packet.KeyCompromised, "compromised", nil); err != nil {
		t.Fatal(err)
	}

	// Revoked ENCRYPTION SUBKEY only (the primary is fine).
	subRevoked := newLocalEntity(t, "subrevoked@example.com")
	if err := subRevoked.RevokeSubkey(&subRevoked.Subkeys[0], packet.KeyRetired, "retired", nil); err != nil {
		t.Fatal(err)
	}

	// A key with no encryption subkey (sign-only).
	signOnly := newLocalEntity(t, "signonly@example.com")
	signOnly.Subkeys = nil

	for name, e := range map[string]*openpgp.Entity{
		"revoked@example.com":    revoked,
		"subrevoked@example.com": subRevoked,
		"signonly@example.com":   signOnly,
	} {
		_, err := encryptAs(t, h, user, sender.Key.KeyID, secretEntity, rcpt(name, publicBinary(t, e)))
		if code, _ := pgpwire.CodeOf(err); code != pgpwire.CodeNoRecipientKey || !strings.Contains(err.Error(), name) {
			t.Errorf("%s: err = %v, want %s naming the address", name, err, pgpwire.CodeNoRecipientKey)
		}
	}

	// Expired: usable now, refused once the daemon's clock passes the lifetime.
	exp, err := openpgp.NewEntity("Exp", "", "expired@example.com", &packet.Config{
		Algorithm: packet.PubKeyAlgoEdDSA, Curve: packet.Curve25519, V6Keys: false,
		KeyLifetimeSecs: 3600,
	})
	if err != nil {
		t.Fatal(err)
	}
	expKey := publicBinary(t, exp)
	if _, err := encryptAs(t, h, user, sender.Key.KeyID, secretEntity, rcpt("expired@example.com", expKey)); err != nil {
		t.Fatalf("CONTROL FAILED: the key is refused before it expires: %v", err)
	}
	future := time.Now().Add(48 * time.Hour)
	h.svc.now = func() time.Time { return future }
	if _, ok := exp.EncryptionKey(future); ok {
		t.Fatal("fixture: the entity does not report itself expired at the daemon's clock")
	}
	_, err = encryptAs(t, h, user, sender.Key.KeyID, secretEntity, rcpt("expired@example.com", expKey))
	if code, _ := pgpwire.CodeOf(err); code != pgpwire.CodeNoRecipientKey || !strings.Contains(err.Error(), "expired@example.com") {
		t.Errorf("expired key: err = %v, want %s", err, pgpwire.CodeNoRecipientKey)
	}
}

// A "public key" that carries a private one is not a recipient key.
func TestEncryptRefusesAKeyCarryingPrivateMaterial(t *testing.T) {
	h := startDaemon(t)
	user := newUserID()
	sender := generate(t, h, user, "me@example.com")
	withPriv := newLocalEntity(t, "priv@example.com")
	_, err := encryptAs(t, h, user, sender.Key.KeyID, secretEntity,
		rcpt("priv@example.com", []byte(armorPrivateEntity(t, withPriv))))
	if code, _ := pgpwire.CodeOf(err); code != pgpwire.CodeNoRecipientKey {
		t.Fatalf("err = %v, want %s", err, pgpwire.CodeNoRecipientKey)
	}
}

// ── SEIPD only. ──

func TestRequireIntegrityProtected(t *testing.T) {
	e := newLocalEntity(t, "sed@example.com")
	sk := sessionKey(t)

	good := craftedMessage(t, e, sk, []byte("x"), false, 0)
	if err := requireIntegrityProtected(armorMessage(t, good), 1); err != nil {
		t.Fatalf("a genuine SEIPD message was refused: %v", err)
	}
	sed := craftedMessage(t, e, sk, []byte("x"), true, 0)
	if err := requireIntegrityProtected(armorMessage(t, sed), 1); err == nil {
		t.Error("a Symmetrically Encrypted Data (tag 9, no MDC) message was ACCEPTED")
	}
	if err := requireIntegrityProtected(armorMessage(t, good), 2); err == nil {
		t.Error("a message with the wrong number of session-key packets was accepted")
	}
	if err := requireIntegrityProtected([]byte("not armor"), 1); err == nil {
		t.Error("garbage was accepted")
	}
}

func TestEncryptWritesSEIPDAndNeverSED(t *testing.T) {
	h := startDaemon(t)
	user := newUserID()
	sender := generate(t, h, user, "me@example.com")
	bob := newLocalEntity(t, "bob@example.com")
	resp, err := encryptAs(t, h, user, sender.Key.KeyID, secretEntity, rcpt("bob@example.com", publicBinary(t, bob)))
	if err != nil {
		t.Fatal(err)
	}
	if _, seipd := packetShape(t, resp.Ciphertext); !seipd {
		t.Fatal("the data packet is not integrity protected")
	}
	// The same independent check the daemon runs on its own output, run here on
	// the real artefact.
	if err := requireIntegrityProtected([]byte(resp.Ciphertext), 2); err != nil {
		t.Fatal(err)
	}
	// And a flipped bit in the MDC makes go-crypto refuse it, so the integrity
	// protection is real rather than a flag.
	bad := tamperTail(t, []byte(resp.Ciphertext), 3)
	if _, err := openWith(t, string(bad), bob); err == nil {
		t.Error("a tampered ciphertext opened")
	}
}

// ── Ownership, retirement, input bounds. ──

func TestEncryptConfusedDeputyFailsClosed(t *testing.T) {
	h := startDaemon(t)
	alice, bob := newUserID(), newUserID()
	ga := generate(t, h, alice, "alice@example.com")
	gb := generate(t, h, bob, "bob@example.com")
	rc := newLocalEntity(t, "rc@example.com")

	// CONTROL: alice can encrypt with her own key.
	if _, err := encryptAs(t, h, alice, ga.Key.KeyID, secretEntity, rcpt("rc@example.com", publicBinary(t, rc))); err != nil {
		t.Fatalf("CONTROL FAILED: %v", err)
	}
	// MemoryVault returns bob's record to alice's request; only the service's
	// ownership comparison stands between alice and adding bob's key as the
	// "sender" recipient of her mail.
	resp, err := encryptAs(t, h, alice, gb.Key.KeyID, secretEntity, rcpt("rc@example.com", publicBinary(t, rc)))
	if code, _ := pgpwire.CodeOf(err); code != pgpwire.CodeDenied {
		t.Fatalf("alice used bob's key as the sender key: err=%v resp=%v", err, resp)
	}
}

func TestEncryptRetiredSenderKeyIsRefused(t *testing.T) {
	h := startDaemon(t)
	user := newUserID()
	sender := generate(t, h, user, "me@example.com")
	rc := newLocalEntity(t, "rc@example.com")
	now := time.Now()
	h.vault.retire(sender.Key.KeyID, true, &now)
	_, err := encryptAs(t, h, user, sender.Key.KeyID, secretEntity, rcpt("rc@example.com", publicBinary(t, rc)))
	if code, _ := pgpwire.CodeOf(err); code != pgpwire.CodeInvalidRequest {
		t.Fatalf("err = %v, want invalid_request", err)
	}
}

func TestEncryptRejectsBadInput(t *testing.T) {
	h := startDaemon(t)
	user := newUserID()
	sender := generate(t, h, user, "me@example.com")
	rc := publicBinary(t, newLocalEntity(t, "rc@example.com"))
	ok := rcpt("rc@example.com", rc)

	many := make([]pgpwire.EncryptRecipient, pgpwire.MaxEncryptRecipients+1)
	for i := range many {
		many[i] = ok
	}
	cases := map[string]pgpwire.EncryptRequest{
		"no entity":        {UserID: user, KeyID: sender.Key.KeyID, Recipients: []pgpwire.EncryptRecipient{ok}},
		"no recipients":    {UserID: user, KeyID: sender.Key.KeyID, Entity: []byte(secretEntity)},
		"no key id":        {UserID: user, Recipients: []pgpwire.EncryptRecipient{ok}, Entity: []byte(secretEntity)},
		"bad user":         {UserID: "nope", KeyID: sender.Key.KeyID, Recipients: []pgpwire.EncryptRecipient{ok}, Entity: []byte(secretEntity)},
		"too many":         {UserID: user, KeyID: sender.Key.KeyID, Recipients: many, Entity: []byte(secretEntity)},
		"address w/ space": {UserID: user, KeyID: sender.Key.KeyID, Recipients: []pgpwire.EncryptRecipient{rcpt("a b@example.com", rc)}, Entity: []byte(secretEntity)},
		"address w/ comma": {UserID: user, KeyID: sender.Key.KeyID, Recipients: []pgpwire.EncryptRecipient{rcpt("a,b@example.com", rc)}, Entity: []byte(secretEntity)},
		"empty address":    {UserID: user, KeyID: sender.Key.KeyID, Recipients: []pgpwire.EncryptRecipient{rcpt("", rc)}, Entity: []byte(secretEntity)},
		"unknown key":      {UserID: user, KeyID: newUserID(), Recipients: []pgpwire.EncryptRecipient{ok}, Entity: []byte(secretEntity)},
		"over the cap": {UserID: user, KeyID: sender.Key.KeyID, Recipients: []pgpwire.EncryptRecipient{ok},
			Entity: bytes.Repeat([]byte("a"), pgpwire.MaxEncryptInputBytes+1)},
	}
	for name, req := range cases {
		if _, err := h.client.Encrypt(context.Background(), req); err == nil {
			t.Errorf("%s: accepted", name)
		}
	}
	// CONTROL: an entity exactly at the cap is accepted, so "over the cap" is the
	// bound and not a broken request.
	if _, err := h.client.Encrypt(context.Background(), pgpwire.EncryptRequest{
		UserID: user, KeyID: sender.Key.KeyID, Recipients: []pgpwire.EncryptRecipient{ok},
		Entity: bytes.Repeat([]byte("a"), pgpwire.MaxEncryptInputBytes),
	}); err != nil {
		t.Errorf("an entity exactly at the cap was refused: %v", err)
	}
}

// What can be encrypted can always be opened again by its sender: the encrypt cap
// is below the decrypt cap.
func TestEncryptCapIsBelowTheDecryptCap(t *testing.T) {
	if pgpwire.MaxEncryptInputBytes >= pgpwire.MaxDecryptedBytes {
		t.Fatalf("MaxEncryptInputBytes (%d) must stay below MaxDecryptedBytes (%d), or the Sent copy of a "+
			"maximum-size message could not be opened by its own sender",
			pgpwire.MaxEncryptInputBytes, pgpwire.MaxDecryptedBytes)
	}
}

// ── No private key, no session key, in any response or log. ──

func TestEncryptLeaksNeitherSessionKeyNorPrivateKey(t *testing.T) {
	var logBuf syncBuffer
	h := startDaemonWithLogger(t, slog.New(slog.NewTextHandler(&logBuf, &slog.HandlerOptions{Level: slog.LevelDebug})))
	user := newUserID()
	sender := generate(t, h, user, "me@example.com")
	bob := newLocalEntity(t, "bob@example.com")

	priv := h.unsealPrivate(t, sender.Key.KeyID)
	det := newLeakDetector(t, priv, sender.Key.PublicKey)

	// A SUCCEEDING encrypt: a refusal never touches the session key.
	var raw json.RawMessage
	if err := h.client.Call(context.Background(), pgpwire.MethodEncrypt, pgpwire.EncryptRequest{
		UserID: user, KeyID: sender.Key.KeyID, Entity: []byte(secretEntity),
		Recipients: []pgpwire.EncryptRecipient{rcpt("bob@example.com", publicBinary(t, bob))},
	}, &raw); err != nil {
		t.Fatalf("the probe encrypt failed, so nothing below examined a real encryption: %v", err)
	}
	var er pgpwire.EncryptResponse
	if err := json.Unmarshal(raw, &er); err != nil {
		t.Fatal(err)
	}

	// Recover the session key the library chose, the way a recipient could.
	// (go-crypto v1 does not hand it back from ReadMessage, so unwrap the
	// session-key packet addressed to bob's encryption subkey ourselves.)
	blk, _ := armor.Decode(strings.NewReader(er.Ciphertext))
	pkts := packet.NewReader(blk.Body)
	var sk []byte
	for {
		p, err := pkts.Next()
		if err != nil {
			break
		}
		if ek, ok := p.(*packet.EncryptedKey); ok && ek.KeyId == bob.Subkeys[0].PublicKey.KeyId {
			if err := ek.Decrypt(bob.Subkeys[0].PrivateKey, nil); err != nil {
				t.Fatal(err)
			}
			sk = ek.Key
			break
		}
	}
	if len(sk) < 16 {
		t.Fatalf("fixture: recovered session key is %d bytes", len(sk))
	}

	corpus := append([]byte(nil), raw...)
	// Failures are output too.
	_, ferr := encryptAs(t, h, user, sender.Key.KeyID, secretEntity, rcpt("x@example.com", []byte("junk")))
	if ferr != nil {
		corpus = append(corpus, ferr.Error()...)
	}

	logs := logBuf.Bytes()
	for name, body := range map[string][]byte{"responses": corpus, "daemon log": logs} {
		if leak := det.findLeak(body); leak != "" {
			t.Errorf("%s: private key material: %s", name, leak)
		}
		if sessionKeyIn(body, sk) {
			t.Errorf("%s: the SESSION KEY appears", name)
		}
		if strings.Contains(string(body), "quarterly numbers") {
			t.Errorf("%s: the plaintext appears", name)
		}
	}
	// CONTROLS on the instruments.
	if !sessionKeyIn(mustJSON(t, map[string][]byte{"k": sk}), sk) {
		t.Fatal("the session-key search cannot see a session key; every assertion above is vacuous")
	}
	if !strings.Contains(string(logs), "pgp message encrypted") {
		t.Fatalf("the daemon log never recorded the encryption; the log assertions are vacuous:\n%s", logs)
	}
}
