// SPDX-License-Identifier: AGPL-3.0-only

//go:build integration

package api

// The `pgpSignature` property and ContactKey/decide (#47, #626 section E,
// ADR-039), through Email/get and the real method against a real database.
//
// Every key is a real go-crypto entity made in the test and every signature is a
// real detached signature wrapped by the real pgpmime.Sign; nothing is a
// hand-written stand-in for the format (the #48 lesson).

import (
	"bytes"
	"context"
	"strings"
	"testing"
	"time"

	"github.com/ProtonMail/go-crypto/openpgp"
	"github.com/ProtonMail/go-crypto/openpgp/packet"

	"mailroom/internal/pgpmime"
	"mailroom/internal/store"
)

const sigBodyText = "the figures are final\r\n"

// signWith wraps msg (content headers + body, optionally with message headers)
// in a multipart/signed made by signer.
func signWith(t *testing.T, signer *openpgp.Entity, msg string) []byte {
	t.Helper()
	out, err := pgpmime.Sign([]byte(msg), func(canonical []byte) (string, string, error) {
		var sig bytes.Buffer
		if err := openpgp.ArmoredDetachSign(&sig, signer, bytes.NewReader(canonical), nil); err != nil {
			return "", "", err
		}
		return sig.String(), "pgp-sha256", nil
	})
	if err != nil {
		t.Fatalf("pgpmime.Sign: %v", err)
	}
	return out
}

// signedFromAlice is a plaintext PGP/MIME message claiming to be from alice.
func signedFromAlice(t *testing.T, signer *openpgp.Entity) []byte {
	return signWith(t, signer, "From: Alice <alice@example.com>\r\nTo: me@example.com\r\nSubject: figures\r\n"+
		"Content-Type: text/plain; charset=utf-8\r\n\r\n"+sigBodyText)
}

// sigOf reads the pgpSignature property of a seeded message.
func (h *encHarness) sigOf(t *testing.T, raw []byte) map[string]any {
	t.Helper()
	id := seedRawMessage(t, h.composeHarness, h.a, raw)
	res := call(t, h.api.emailGet, h.a.UserID, map[string]any{
		"ids": []string{id.String()}, "properties": []string{"id", "pgpSignature", "textBody", "bodyValues"},
		"fetchTextBodyValues": true,
	})
	obj := res["list"].([]any)[0].(map[string]any)
	v, present := obj["pgpSignature"]
	if !present {
		t.Fatalf("pgpSignature is not in the response at all: %v", obj)
	}
	if v == nil {
		return nil
	}
	return v.(map[string]any)
}

func wantState(t *testing.T, got map[string]any, state string) {
	t.Helper()
	if got == nil || got["state"] != state {
		t.Fatalf("pgpSignature = %v, want state %q", got, state)
	}
}

func TestPgpSignatureUnsignedIsNull(t *testing.T) {
	h := newEncHarness(t)
	got := h.sigOf(t, []byte("From: Alice <alice@example.com>\r\nTo: me@example.com\r\nSubject: s\r\n"+
		"Content-Type: text/plain\r\n\r\nplain\r\n"))
	if got != nil {
		t.Errorf("an unsigned message has pgpSignature %v, want null", got)
	}
}

func TestPgpSignatureVerifiedByAHeldKeyThatNamesTheSender(t *testing.T) {
	h := newEncHarness(t)
	alice := newPeerKey(t, "alice@example.com", nil)
	fpr := h.seedKey(t, "alice@example.com", pubOf(t, alice), store.ContactKeyManual, false, time.Now())

	got := h.sigOf(t, signedFromAlice(t, alice))
	wantState(t, got, "verified")
	if got["fingerprint"] != fpr || got["address"] != "alice@example.com" {
		t.Errorf("verified = %v, want fingerprint %s", got, fpr)
	}
	// Trust on first verification: the key is now the pin, so a LATER different
	// key shows as a change even though the user never encrypted to alice.
	if pin, ok := h.pin(t, "alice@example.com"); !ok || pin != fpr {
		t.Errorf("pin after a first verification = %q, %v; want %s", pin, ok, fpr)
	}
}

func TestPgpSignatureBadWhenTheContentWasAltered(t *testing.T) {
	h := newEncHarness(t)
	alice := newPeerKey(t, "alice@example.com", nil)
	h.seedKey(t, "alice@example.com", pubOf(t, alice), store.ContactKeyManual, false, time.Now())
	raw := signedFromAlice(t, alice)
	tampered := bytes.Replace(raw, []byte("final"), []byte("FINAL"), 1)
	if bytes.Equal(raw, tampered) {
		t.Fatal("fixture: nothing was tampered")
	}
	wantState(t, h.sigOf(t, tampered), "bad")
	// CONTROL: the untouched message verifies, so "bad" is the tampering's doing.
	wantState(t, h.sigOf(t, raw), "verified")
}

func TestPgpSignatureUnknownKeyWhenNoKeyIsHeld(t *testing.T) {
	h := newEncHarness(t)
	stranger := newPeerKey(t, "alice@example.com", nil)
	got := h.sigOf(t, signedFromAlice(t, stranger))
	wantState(t, got, "unknownKey")
	fp, _ := got["fingerprint"].(string)
	want := hexOf(stranger)
	if fp == "" || !strings.HasSuffix(want, fp[len(fp)-16:]) {
		t.Errorf("unknownKey names %q; the signing key is %s", fp, want)
	}
}

// The address-binding rule: a VALID signature by a key that is held for the
// sender but names another address is not "verified".
func TestPgpSignatureByAKeyForAnotherAddressIsUnknownKey(t *testing.T) {
	h := newEncHarness(t)
	other := newPeerKey(t, "someone-else@example.com", nil)
	// Held, imported, even pinned: none of it may turn a wrong-address key into
	// the sender's.
	otherFpr := h.seedKey(t, "alice@example.com", pubOf(t, other), store.ContactKeyManual, false, time.Now())
	if err := h.setPin("alice@example.com", otherFpr); err != nil {
		t.Fatal(err)
	}
	got := h.sigOf(t, signedFromAlice(t, other))
	wantState(t, got, "unknownKey")
	if got["fingerprint"] != otherFpr {
		t.Errorf("fingerprint = %v, want %s", got["fingerprint"], otherFpr)
	}
}

func (h *encHarness) setPin(address, fpr string) error {
	_, err := h.super.Exec(context.Background(), `INSERT INTO app.contact_key_pins (user_id, address, fingerprint, source)
	    VALUES ($1,$2,$3,'manual') ON CONFLICT (user_id, address) DO UPDATE SET fingerprint = EXCLUDED.fingerprint`,
		h.a.UserID.String(), address, fpr)
	return err
}

func TestPgpSignatureKeyChangedAgainstThePin(t *testing.T) {
	h := newEncHarness(t)
	old := newPeerKey(t, "alice@example.com", nil)
	oldFpr := h.seedKey(t, "alice@example.com", pubOf(t, old), store.ContactKeyWKD, false, time.Now().Add(-48*time.Hour))
	if err := h.setPin("alice@example.com", oldFpr); err != nil {
		t.Fatal(err)
	}
	// A new key that names alice, arrived by Autocrypt on this very message.
	fresh := newPeerKey(t, "alice@example.com", nil)
	freshFpr := h.seedKey(t, "alice@example.com", pubOf(t, fresh), store.ContactKeyAutocrypt, false, time.Now())

	got := h.sigOf(t, signedFromAlice(t, fresh))
	wantState(t, got, "keyChanged")
	if got["fingerprint"] != freshFpr || got["previousFingerprint"] != oldFpr {
		t.Errorf("keyChanged = %v; want fingerprint %s, previous %s", got, freshFpr, oldFpr)
	}
	// CONTROL: the pinned key itself still verifies.
	wantState(t, h.sigOf(t, signedFromAlice(t, old)), "verified")
}

// The pin is what makes a changed key visible even for an AUTHENTICATED newer
// key, which on its own is eligible and would be "verified".
func TestPgpSignatureAnEligibleNewerKeyStillShowsAsChangedWhenPinned(t *testing.T) {
	h := newEncHarness(t)
	old := newPeerKey(t, "alice@example.com", nil)
	oldFpr := h.seedKey(t, "alice@example.com", pubOf(t, old), store.ContactKeyWKD, false, time.Now().Add(-48*time.Hour))
	if err := h.setPin("alice@example.com", oldFpr); err != nil {
		t.Fatal(err)
	}
	fresh := newPeerKey(t, "alice@example.com", nil)
	h.seedKey(t, "alice@example.com", pubOf(t, fresh), store.ContactKeyAutocrypt, true, time.Now())
	wantState(t, h.sigOf(t, signedFromAlice(t, fresh)), "keyChanged")
}

// An Autocrypt key nobody vouched for, planted by the message it signs, is never
// "verified": with nothing to compare it to it is unknown.
func TestPgpSignatureUnauthenticatedAutocryptKeyIsNotVerified(t *testing.T) {
	h := newEncHarness(t)
	planted := newPeerKey(t, "alice@example.com", nil)
	h.seedKey(t, "alice@example.com", pubOf(t, planted), store.ContactKeyAutocrypt, false, time.Now())
	wantState(t, h.sigOf(t, signedFromAlice(t, planted)), "unknownKey")

	// CONTROL: the same key from an AUTHENTICATED message is eligible.
	h2 := newEncHarness(t)
	h2.seedKey(t, "alice@example.com", pubOf(t, planted), store.ContactKeyAutocrypt, true, time.Now())
	wantState(t, h2.sigOf(t, signedFromAlice(t, planted)), "verified")
}

// ... and with an eligible key already held, the planted one is a CHANGE.
func TestPgpSignatureUnauthenticatedKeyAgainstAnEligibleOneIsAChange(t *testing.T) {
	h := newEncHarness(t)
	real := newPeerKey(t, "alice@example.com", nil)
	realFpr := h.seedKey(t, "alice@example.com", pubOf(t, real), store.ContactKeyWKD, false, time.Now().Add(-time.Hour))
	planted := newPeerKey(t, "alice@example.com", nil)
	h.seedKey(t, "alice@example.com", pubOf(t, planted), store.ContactKeyAutocrypt, false, time.Now())
	got := h.sigOf(t, signedFromAlice(t, planted))
	wantState(t, got, "keyChanged")
	if got["previousFingerprint"] != realFpr {
		t.Errorf("previousFingerprint = %v, want %s", got["previousFingerprint"], realFpr)
	}
}

// ---- signed AND encrypted ----------------------------------------------------

// signedThenEncrypted builds an RFC 3156 message from alice whose decrypted
// entity is a multipart/signed made by signer, encrypted to rcpt.
func signedThenEncrypted(t *testing.T, signer, rcpt *openpgp.Entity) []byte {
	t.Helper()
	entity := signWith(t, signer, "Content-Type: text/plain; charset=utf-8\r\n\r\n"+sigBodyText)
	return encryptedRFC3156(t, rcpt, string(entity))
}

func (h *encHarness) withDecrypter(t *testing.T) *openpgp.Entity {
	t.Helper()
	rcpt, err := openpgp.NewEntity("Reader", "", "reader@example.com", &packet.Config{
		Algorithm: packet.PubKeyAlgoEdDSA, Curve: packet.Curve25519,
	})
	if err != nil {
		t.Fatal(err)
	}
	h.api.decrypter = &localDecrypter{key: rcpt}
	return rcpt
}

func TestPgpSignatureInsideAnEncryptedMessage(t *testing.T) {
	h := newEncHarness(t)
	rcpt := h.withDecrypter(t)
	alice := newPeerKey(t, "alice@example.com", nil)
	aliceFpr := h.seedKey(t, "alice@example.com", pubOf(t, alice), store.ContactKeyManual, false, time.Now().Add(-time.Hour))

	got := h.sigOf(t, signedThenEncrypted(t, alice, rcpt))
	wantState(t, got, "verified")
	if got["fingerprint"] != aliceFpr {
		t.Errorf("fingerprint = %v, want %s", got["fingerprint"], aliceFpr)
	}

	// A new key signs inside the encryption: changed, against the pin.
	if err := h.setPin("alice@example.com", aliceFpr); err != nil {
		t.Fatal(err)
	}
	fresh := newPeerKey(t, "alice@example.com", nil)
	h.seedKey(t, "alice@example.com", pubOf(t, fresh), store.ContactKeyAutocrypt, false, time.Now())
	got = h.sigOf(t, signedThenEncrypted(t, fresh, rcpt))
	wantState(t, got, "keyChanged")
	if got["previousFingerprint"] != aliceFpr {
		t.Errorf("previousFingerprint = %v", got["previousFingerprint"])
	}

	// A key held for another address: unknownKey, inside encryption too.
	other := newPeerKey(t, "someone-else@example.com", nil)
	h.seedKey(t, "alice@example.com", pubOf(t, other), store.ContactKeyManual, false, time.Now())
	wantState(t, h.sigOf(t, signedThenEncrypted(t, other, rcpt)), "unknownKey")

	// No key at all: unknownKey. Tampered: bad.
	stranger := newPeerKey(t, "alice@example.com", nil)
	wantState(t, h.sigOf(t, signedThenEncrypted(t, stranger, rcpt)), "unknownKey")
	entity := bytes.Replace(signWith(t, alice, "Content-Type: text/plain; charset=utf-8\r\n\r\n"+sigBodyText),
		[]byte("final"), []byte("FINAL"), 1)
	wantState(t, h.sigOf(t, encryptedRFC3156(t, rcpt, string(entity))), "bad")

	// An encrypted but unsigned message has no verdict, not a false one.
	if got := h.sigOf(t, encryptedRFC3156(t, rcpt, "Content-Type: text/plain\r\n\r\nunsigned\r\n")); got != nil {
		t.Errorf("unsigned encrypted message has pgpSignature %v", got)
	}
}

// ---- the decision ------------------------------------------------------------

func (h *encHarness) contactChanges(t *testing.T, address string) int {
	t.Helper()
	var n int
	err := h.super.QueryRow(context.Background(), `
		SELECT count(*) FROM app.change_log cl
		  JOIN app.contact_addresses ca ON ca.contact_id = cl.object_id AND ca.user_id = cl.user_id
		 WHERE cl.user_id = $1 AND cl.object_type = 'Contact' AND ca.address = $2`,
		h.a.UserID.String(), address).Scan(&n)
	if err != nil {
		t.Fatal(err)
	}
	return n
}

func (h *encHarness) decide(t *testing.T, address, fpr, decision string) map[string]any {
	t.Helper()
	return call(t, h.api.contactKeyDecide, h.a.UserID, map[string]any{
		"address": address, "fingerprint": fpr, "decision": decision,
	})
}

// changedKeyFixture: bob has an old key that was encrypted to (pinned) and a newer
// WKD key that is therefore refused as a change.
func changedKeyFixture(t *testing.T) (h *encHarness, oldFpr, newFpr string) {
	h = newEncHarness(t)
	first := newPeerKey(t, "bob@example.com", nil)
	oldFpr = h.seedKey(t, "bob@example.com", pubOf(t, first), store.ContactKeyWKD, false, time.Now().Add(-48*time.Hour))
	if e := h.submit(t, h.draft(t, []string{"bob@example.com"}, nil), true); e != nil {
		t.Fatalf("first send refused: %v", e)
	}
	second := newPeerKey(t, "bob@example.com", nil)
	newFpr = h.seedKey(t, "bob@example.com", pubOf(t, second), store.ContactKeyWKD, false, time.Now())
	id := h.draft(t, []string{"bob@example.com"}, nil)
	h.refused(t, id, h.submit(t, id, true), h.hash(t, id))
	return h, oldFpr, newFpr
}

func TestDecideTrustMakesTheNewKeyThePinAndUsesItToEncrypt(t *testing.T) {
	h, oldFpr, newFpr := changedKeyFixture(t)
	before := h.contactChanges(t, "bob@example.com")

	res := h.decide(t, "bob@example.com", newFpr, "trust")
	if res["pinnedFingerprint"] != newFpr || res["decision"] != "trust" {
		t.Errorf("decide = %v", res)
	}
	if got, _ := h.pin(t, "bob@example.com"); got != newFpr {
		t.Errorf("pin = %q, want the trusted key %s (was %s)", got, newFpr, oldFpr)
	}
	var src string
	if err := h.super.QueryRow(context.Background(),
		`SELECT source FROM app.contact_keys WHERE user_id=$1 AND fingerprint=$2`, h.a.UserID.String(), newFpr).Scan(&src); err != nil {
		t.Fatal(err)
	}
	if src != store.ContactKeyManual {
		t.Errorf("trusted key source = %q, want manual", src)
	}
	if after := h.contactChanges(t, "bob@example.com"); after != before+1 {
		t.Errorf("Contact change_log rows %d -> %d; the decision must append exactly one", before, after)
	}

	// The lookup now offers the new key, and a send encrypts to it.
	lk := call(t, h.api.recipientKeyLookup, h.a.UserID, map[string]any{"addresses": []string{"bob@example.com"}, "wkd": false})
	row := lk["list"].([]any)[0].(map[string]any)
	if row["usable"] != true || row["keyChanged"] != false || row["fingerprint"] != newFpr {
		t.Errorf("lookup after trust = %v", row)
	}
	if e := h.submit(t, h.draft(t, []string{"bob@example.com"}, nil), true); e != nil {
		t.Fatalf("a send after trusting the new key was refused: %v", e)
	}
	if got, _ := h.pin(t, "bob@example.com"); got != newFpr {
		t.Errorf("pin after the send = %q", got)
	}
}

func TestDecideKeepRefusesTheNewKeyAndKeepsUsingTheOldOne(t *testing.T) {
	h, oldFpr, newFpr := changedKeyFixture(t)
	before := h.contactChanges(t, "bob@example.com")

	res := h.decide(t, "bob@example.com", newFpr, "keep")
	if res["pinnedFingerprint"] != oldFpr {
		t.Errorf("decide = %v; the pin must stay %s", res, oldFpr)
	}
	if got, _ := h.pin(t, "bob@example.com"); got != oldFpr {
		t.Errorf("pin = %q after keep, want the old key %s", got, oldFpr)
	}
	if after := h.contactChanges(t, "bob@example.com"); after != before+1 {
		t.Errorf("Contact change_log rows %d -> %d; the decision must append exactly one", before, after)
	}

	// The refused key is never the one encrypted to: the lookup names the old one.
	lk := call(t, h.api.recipientKeyLookup, h.a.UserID, map[string]any{"addresses": []string{"bob@example.com"}, "wkd": false})
	row := lk["list"].([]any)[0].(map[string]any)
	if row["fingerprint"] != oldFpr || row["keyChanged"] != false {
		t.Errorf("lookup after keep = %v; want the old key %s and no change", row, oldFpr)
	}
	if e := h.submit(t, h.draft(t, []string{"bob@example.com"}, nil), true); e != nil {
		t.Fatalf("a send after keeping the old key was refused: %v", e)
	}
	if got, _ := h.pin(t, "bob@example.com"); got != oldFpr {
		t.Errorf("a send moved the pin to %q", got)
	}
}

// Signatures by the refused key keep showing as changed, and by the trusted key
// stop doing so.
func TestDecideChangesWhatTheSignatureSays(t *testing.T) {
	h := newEncHarness(t)
	old := newPeerKey(t, "alice@example.com", nil)
	oldFpr := h.seedKey(t, "alice@example.com", pubOf(t, old), store.ContactKeyWKD, false, time.Now().Add(-48*time.Hour))
	if err := h.setPin("alice@example.com", oldFpr); err != nil {
		t.Fatal(err)
	}
	fresh := newPeerKey(t, "alice@example.com", nil)
	freshFpr := h.seedKey(t, "alice@example.com", pubOf(t, fresh), store.ContactKeyAutocrypt, true, time.Now())

	got := h.sigOf(t, signedFromAlice(t, fresh))
	wantState(t, got, "keyChanged")
	if got["refused"] == true {
		t.Errorf("refused before any decision: %v", got)
	}

	h.decide(t, "alice@example.com", freshFpr, "keep")
	got = h.sigOf(t, signedFromAlice(t, fresh))
	wantState(t, got, "keyChanged")
	if got["refused"] != true || got["previousFingerprint"] != oldFpr {
		t.Errorf("after keep: %v; want keyChanged, refused, previous %s", got, oldFpr)
	}
	// Contact.keys carries the flag, never the key.
	cg := call(t, h.api.contactGet, h.a.UserID, map[string]any{"properties": []string{"id", "addresses", "keys"}})
	if !strings.Contains(string(mustJSON(t, cg)), `"refused":true`) {
		t.Errorf("Contact/get does not report the refusal: %s", mustJSON(t, cg))
	}

	h.decide(t, "alice@example.com", freshFpr, "trust")
	wantState(t, h.sigOf(t, signedFromAlice(t, fresh)), "verified")
	// The OLD key, still held, is now the one that differs from the pin; it was
	// imported by nobody, so it shows as changed rather than verified.
	wantState(t, h.sigOf(t, signedFromAlice(t, old)), "keyChanged")
}

func TestDecideRefusals(t *testing.T) {
	h := newEncHarness(t)
	other := newPeerKey(t, "someone-else@example.com", nil)
	otherFpr := h.seedKey(t, "bob@example.com", pubOf(t, other), store.ContactKeyAutocrypt, true, time.Now())
	good := newPeerKey(t, "bob@example.com", nil)
	goodFpr := h.seedKey(t, "bob@example.com", pubOf(t, good), store.ContactKeyAutocrypt, true, time.Now())

	try := func(address, fpr, decision string) *MethodError {
		_, mErr := h.api.contactKeyDecide(context.Background(), h.a.UserID,
			mustJSON(t, map[string]any{"address": address, "fingerprint": fpr, "decision": decision}))
		return mErr
	}
	if e := try("bob@example.com", otherFpr, "trust"); e == nil || e.Type != errKeyDoesNotNameAddress {
		t.Errorf("trusting a key that names another address: %+v", e)
	}
	if got, ok := h.pin(t, "bob@example.com"); ok {
		t.Errorf("a refused trust left a pin %q", got)
	}
	if e := try("bob@example.com", "0123456789ABCDEF", "trust"); e == nil || e.Type != "notFound" {
		t.Errorf("a fingerprint nobody holds: %+v", e)
	}
	if e := try("nobody@example.com", goodFpr, "trust"); e == nil || e.Type != "notFound" {
		t.Errorf("an address with no contact: %+v", e)
	}
	if e := try("bob@example.com", goodFpr, "maybe"); e == nil || e.Type != errInvalidArguments {
		t.Errorf("an unknown decision: %+v", e)
	}
	// CONTROL: the good key is accepted, so the refusals above came from the rules.
	if e := try("bob@example.com", goodFpr, "trust"); e != nil {
		t.Errorf("CONTROL FAILED: trusting a key that names the address: %+v", e)
	}
}
