// SPDX-License-Identifier: AGPL-3.0-only

//go:build integration

package api

// ContactKey/import (#47, ADR-038), against a real database and the real crypto
// process, reusing the encrypted-submit harness: the question is not only "was a
// row written" but "does the next encrypted send actually use it".

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"testing"
	"time"

	"github.com/ProtonMail/go-crypto/openpgp"
	"github.com/ProtonMail/go-crypto/openpgp/armor"
	"github.com/ProtonMail/go-crypto/openpgp/packet"

	"mailroom/internal/store"
)

func armoredPub(t *testing.T, e *openpgp.Entity) string {
	t.Helper()
	var buf bytes.Buffer
	w, err := armor.Encode(&buf, openpgp.PublicKeyType, nil)
	if err != nil {
		t.Fatal(err)
	}
	if err := e.Serialize(w); err != nil {
		t.Fatal(err)
	}
	if err := w.Close(); err != nil {
		t.Fatal(err)
	}
	return buf.String()
}

func (h *encHarness) importKey(t *testing.T, address, key string) (map[string]any, *MethodError) {
	t.Helper()
	raw, mErr := h.api.contactKeyImport(context.Background(), h.a.UserID,
		mustJSON(t, map[string]any{"address": address, "publicKey": key}))
	if mErr != nil {
		return nil, mErr
	}
	return mustUnmarshalMap(t, raw), nil
}

func mustUnmarshalMap(t *testing.T, raw json.RawMessage) map[string]any {
	t.Helper()
	var out map[string]any
	if err := json.Unmarshal(raw, &out); err != nil {
		t.Fatal(err)
	}
	return out
}

func (h *encHarness) keyRows(t *testing.T) int {
	t.Helper()
	var n int
	if err := h.super.QueryRow(context.Background(),
		`SELECT count(*) FROM app.contact_keys WHERE user_id = $1`, h.a.UserID.String()).Scan(&n); err != nil {
		t.Fatal(err)
	}
	return n
}

func TestImportReplacesThePinAndEncryptionUsesTheImportedKey(t *testing.T) {
	h := newEncHarness(t)
	first := newPeerKey(t, "bob@example.com", nil)
	oldFpr := h.seedKey(t, "bob@example.com", pubOf(t, first), store.ContactKeyWKD, false, time.Now().Add(-48*time.Hour))
	if e := h.submit(t, h.draft(t, []string{"bob@example.com"}, nil), true); e != nil {
		t.Fatalf("first send refused: %v", e)
	}
	if got, _ := h.pin(t, "bob@example.com"); got != oldFpr {
		t.Fatalf("setup: pin = %q", got)
	}

	// A rotated key: the lookup now says keyChanged, and a send is refused.
	second := newPeerKey(t, "bob@example.com", nil)
	h.seedKey(t, "bob@example.com", pubOf(t, second), store.ContactKeyWKD, false, time.Now())
	imported := newPeerKey(t, "bob@example.com", nil)
	before := h.contactChanges(t, "bob@example.com")

	res, mErr := h.importKey(t, "Bob@Example.com", armoredPub(t, imported))
	if mErr != nil {
		t.Fatalf("import: %+v", mErr)
	}
	impFpr := hexOf(imported)
	if res["fingerprint"] != impFpr || res["pinnedFingerprint"] != impFpr || res["address"] != "bob@example.com" {
		t.Errorf("import result = %v, want fingerprint and pin %s", res, impFpr)
	}
	if got, _ := h.pin(t, "bob@example.com"); got != impFpr {
		t.Errorf("pin = %q, want the imported key %s (REPLACED the old pin %s)", got, impFpr, oldFpr)
	}
	var src string
	var authn, refused bool
	if err := h.super.QueryRow(context.Background(),
		`SELECT source, authenticated, refused FROM app.contact_keys WHERE user_id=$1 AND fingerprint=$2`,
		h.a.UserID.String(), impFpr).Scan(&src, &authn, &refused); err != nil {
		t.Fatal(err)
	}
	if src != store.ContactKeyManual || authn || refused {
		t.Errorf("stored key: source=%q authenticated=%v refused=%v", src, authn, refused)
	}
	if after := h.contactChanges(t, "bob@example.com"); after != before+1 {
		t.Errorf("Contact change_log rows %d -> %d; the import must append exactly one", before, after)
	}

	lk := call(t, h.api.recipientKeyLookup, h.a.UserID, map[string]any{"addresses": []string{"bob@example.com"}, "wkd": false})
	row := lk["list"].([]any)[0].(map[string]any)
	if row["usable"] != true || row["keyChanged"] != false || row["fingerprint"] != impFpr || row["source"] != "manual" {
		t.Errorf("lookup after import = %v", row)
	}
	if e := h.submit(t, h.draft(t, []string{"bob@example.com"}, nil), true); e != nil {
		t.Fatalf("a send after importing the key was refused: %v", e)
	}
	if got, _ := h.pin(t, "bob@example.com"); got != impFpr {
		t.Errorf("pin after the send = %q, want %s", got, impFpr)
	}
}

func TestImportCreatesTheContactAndAcceptsBinaryBase64(t *testing.T) {
	h := newEncHarness(t)
	k := newPeerKey(t, "carol@example.com", nil)
	res, mErr := h.importKey(t, "carol@example.com", base64.StdEncoding.EncodeToString(pubOf(t, k)))
	if mErr != nil {
		t.Fatalf("import: %+v", mErr)
	}
	if res["fingerprint"] != hexOf(k) {
		t.Errorf("result = %v", res)
	}
	var n int
	if err := h.super.QueryRow(context.Background(),
		`SELECT count(*) FROM app.contact_addresses WHERE user_id=$1 AND address='carol@example.com'`, h.a.UserID.String()).Scan(&n); err != nil || n != 1 {
		t.Errorf("contact for the imported address: n=%d err=%v", n, err)
	}
	if e := h.submit(t, h.draft(t, []string{"carol@example.com"}, nil), true); e != nil {
		t.Fatalf("encrypted send to an imported-only recipient refused: %v", e)
	}
}

func TestImportDryRunStoresNothing(t *testing.T) {
	h := newEncHarness(t)
	k := newPeerKey(t, "dave@example.com", nil)
	raw, mErr := h.api.contactKeyImport(context.Background(), h.a.UserID,
		mustJSON(t, map[string]any{"address": "dave@example.com", "publicKey": armoredPub(t, k), "dryRun": true}))
	if mErr != nil {
		t.Fatalf("dry run: %+v", mErr)
	}
	res := mustUnmarshalMap(t, raw)
	if res["fingerprint"] != hexOf(k) || res["dryRun"] != true || res["userId"] == "" {
		t.Errorf("dry run result = %v", res)
	}
	if n := h.keyRows(t); n != 0 {
		t.Errorf("a dry run stored %d key rows", n)
	}
	if _, ok := h.pin(t, "dave@example.com"); ok {
		t.Error("a dry run wrote a pin")
	}
}

func TestImportRefusals(t *testing.T) {
	h := newEncHarness(t)
	const addr = "erin@example.com"
	old := time.Now().Add(-72 * time.Hour)

	secret := newPeerKey(t, addr, nil)
	var sec bytes.Buffer
	w, _ := armor.Encode(&sec, openpgp.PrivateKeyType, nil)
	if err := secret.SerializePrivate(w, nil); err != nil {
		t.Fatal(err)
	}
	_ = w.Close()
	var secBin bytes.Buffer
	if err := secret.SerializePrivate(&secBin, nil); err != nil {
		t.Fatal(err)
	}

	signOnly := newPeerKey(t, addr, nil)
	signOnly.Subkeys = nil
	expired := newPeerKey(t, addr, &packet.Config{KeyLifetimeSecs: 3600, Time: func() time.Time { return old }})
	revoked := newPeerKey(t, addr, nil)
	if err := revoked.RevokeKey(packet.KeyCompromised, "x", nil); err != nil {
		t.Fatal(err)
	}

	cases := []struct {
		name, key, want string
	}{
		{"armored secret key", sec.String(), errSecretKeyRefused},
		{"binary secret key", base64.StdEncoding.EncodeToString(secBin.Bytes()), errSecretKeyRefused},
		{"another address", armoredPub(t, newPeerKey(t, "mallory@example.com", nil)), errKeyDoesNotNameAddress},
		{"no encryption subkey", armoredPub(t, signOnly), errNoEncryptionSubkey},
		{"expired", armoredPub(t, expired), errKeyExpiredOrRevoked},
		{"revoked", armoredPub(t, revoked), errKeyExpiredOrRevoked},
		{"oversized", string(bytes.Repeat([]byte("A"), 2*256*1024+1)), errTooLarge},
		{"garbage", "-----BEGIN PGP PUBLIC KEY BLOCK-----\n\nnope\n-----END PGP PUBLIC KEY BLOCK-----", errInvalidKey},
		{"not base64", "this is not a key!", errInvalidKey},
	}
	for _, c := range cases {
		_, mErr := h.importKey(t, addr, c.key)
		if mErr == nil || mErr.Type != c.want {
			t.Errorf("%s: got %+v, want type %s", c.name, mErr, c.want)
		}
	}
	// NOTHING WAS STORED by any refusal: no key row (the secret one above least of
	// all), no pin, and no contact was made for the address.
	if n := h.keyRows(t); n != 0 {
		t.Errorf("refusals left %d contact_keys rows", n)
	}
	if got, ok := h.pin(t, addr); ok {
		t.Errorf("a refused import left a pin %q", got)
	}
	var contacts int
	if err := h.super.QueryRow(context.Background(),
		`SELECT count(*) FROM app.contact_addresses WHERE user_id=$1 AND address=$2`, h.a.UserID.String(), addr).Scan(&contacts); err != nil || contacts != 0 {
		t.Errorf("a refused import created a contact: n=%d err=%v", contacts, err)
	}
	// CONTROL: a good key is accepted, so the refusals came from the rules.
	if _, mErr := h.importKey(t, addr, armoredPub(t, newPeerKey(t, addr, nil))); mErr != nil {
		t.Errorf("CONTROL FAILED: a good key was refused: %+v", mErr)
	}
}

func TestImportOfAnOwnAddressIsRefusedAndStoresNothing(t *testing.T) {
	h := newEncHarness(t)
	k := newPeerKey(t, "me@example.com", nil)
	for _, dry := range []bool{false, true} {
		raw, mErr := h.api.contactKeyImport(context.Background(), h.a.UserID,
			mustJSON(t, map[string]any{"address": "Me@Example.com", "publicKey": armoredPub(t, k), "dryRun": dry}))
		if mErr == nil || mErr.Type != errOwnAddress {
			t.Fatalf("dryRun=%v: got %s %+v, want %s", dry, raw, mErr, errOwnAddress)
		}
	}
	if n := h.keyRows(t); n != 0 {
		t.Errorf("an own-address import stored %d key rows", n)
	}
	if _, ok := h.pin(t, "me@example.com"); ok {
		t.Error("an own-address import wrote a pin")
	}
	var contacts int
	if err := h.super.QueryRow(context.Background(),
		`SELECT count(*) FROM app.contact_addresses WHERE user_id=$1 AND address='me@example.com'`, h.a.UserID.String()).Scan(&contacts); err != nil || contacts != 0 {
		t.Errorf("an own-address import made a contact: n=%d err=%v", contacts, err)
	}
	// CONTROL: the same call for a correspondent works, so the refusal was the rule.
	if _, mErr := h.importKey(t, "zed@example.com", armoredPub(t, newPeerKey(t, "zed@example.com", nil))); mErr != nil {
		t.Errorf("CONTROL FAILED: %+v", mErr)
	}
}

// An imported key is a manual key, so it must flow into BOTH signature shapes:
// RFC 3156 section 6.1 (multipart/signed, pgpverify) and section 6.2 (inside the
// encryption, candidates sent to the crypto process). The imported key REPLACES
// the pin, so it is the first candidate, and a message it signed reads verified
// even when another (WKD) key was pinned before.
func TestAnImportedKeyVerifiesSignaturesInBothShapes(t *testing.T) {
	h := newEncHarness(t)
	me := h.withCryptoDecrypter(t)
	prior := newPeerKey(t, "alice@example.com", nil)
	priorFpr := h.seedKey(t, "alice@example.com", pubOf(t, prior), store.ContactKeyWKD, false, time.Now().Add(-time.Hour))
	if err := h.setPin("alice@example.com", priorFpr); err != nil {
		t.Fatal(err)
	}
	imported := newPeerKey(t, "alice@example.com", nil)

	// BEFORE the import the same key is not known: unknownKey in both shapes, or
	// the verified results below prove nothing about the import.
	wantState(t, h.sigOf(t, signedFromAlice(t, imported)), "unknownKey")
	wantState(t, h.sigOf(t, fromAliceEncrypted(combined(t, me, imported, embeddedEntity))), "unknownKey")

	if _, mErr := h.importKey(t, "alice@example.com", armoredPub(t, imported)); mErr != nil {
		t.Fatalf("import: %+v", mErr)
	}
	fpr := hexOf(imported)

	got := h.sigOf(t, signedFromAlice(t, imported)) // 6.1
	wantState(t, got, "verified")
	if got["fingerprint"] != fpr {
		t.Errorf("6.1 fingerprint = %v, want %s", got["fingerprint"], fpr)
	}
	got = h.sigOf(t, fromAliceEncrypted(combined(t, me, imported, embeddedEntity))) // 6.2
	wantState(t, got, "verified")
	if got["fingerprint"] != fpr {
		t.Errorf("6.2 fingerprint = %v, want %s", got["fingerprint"], fpr)
	}
}
