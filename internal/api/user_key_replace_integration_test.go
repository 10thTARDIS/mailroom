// SPDX-License-Identifier: AGPL-3.0-only

//go:build integration

package api

// UserKey/replace (#817) against the real crypto service and store: the new key
// takes the address, the WKD entry and the signing role; the old one retires
// into it; WKD serves only the new key afterwards; the refusals reach neither
// the crypto process nor the store.

import (
	"bytes"
	"context"
	"testing"
	"time"

	"github.com/ProtonMail/go-crypto/openpgp"

	"mailroom/internal/pgpkeys"
	"mailroom/internal/wkd"
)

// servedFingerprints lists the fingerprints WKD serves for me@example.com, in
// order. The lookup is instance-wide (WKD has no caller), and every test's user
// A has that address, so assertions check membership of THIS test's keys.
func (h *keyHarness) servedFingerprints(t *testing.T) []string {
	t.Helper()
	pk, found, err := h.store.LookupWKDKey(context.Background(), "example.com", wkd.HashLocalPart("me"))
	if err != nil {
		t.Fatal(err)
	}
	if !found {
		return nil
	}
	ring, err := openpgp.ReadKeyRing(bytes.NewReader(pk.PublicKey))
	if err != nil {
		t.Fatal(err)
	}
	var out []string
	for _, e := range ring {
		var b bytes.Buffer
		if err := e.Serialize(&b); err != nil {
			t.Fatal(err)
		}
		k, err := pgpkeys.Parse(b.Bytes())
		if err != nil {
			t.Fatal(err)
		}
		out = append(out, k.Fingerprint)
	}
	return out
}

func TestUserKeyReplaceBindsPublishesAndRetires(t *testing.T) {
	h := newKeyHarness(t)
	a := h.a.UserID
	oldID, oldKey := h.generate(t, a, h.identA, map[string]any{"bind": true, "expiresDays": 3})
	if ok, e := h.update(t, a, oldID, map[string]any{"wkdPublished": true}); !ok {
		t.Fatalf("publish: %v", e)
	}
	if got := h.servedFingerprints(t); !strIn(got, oldKey["fingerprint"].(string)) {
		t.Fatalf("control: WKD serves %v before the replace, without the old key", got)
	}

	day := time.Now().UTC().AddDate(1, 0, 0).Format("2006-01-02")
	var out map[string]any
	k, i := h.delta(t, a, func() {
		out = h.must(t, "UserKey/replace", a, map[string]any{"id": oldID, "expiresAt": day})
	})
	// Key: created (custody) + updated (new) + updated (old); Identity: one.
	if k != 3 || i != 1 {
		t.Errorf("replace wrote Key %d / Identity %d rows, want 3/1", k, i)
	}
	newID := out["id"].(string)
	nk, ok := out["key"].(map[string]any)
	if !ok || out["replacedId"] != oldID || out["published"] != true || out["boundTo"] != h.identA.String() {
		t.Fatalf("replace = %v", out)
	}
	if nk["identityId"] != h.identA.String() || nk["wkdPublished"] != true || nk["canSign"] != true || nk["boundAddressListed"] != true {
		t.Errorf("new key = %v", nk)
	}
	want, _ := time.Parse("2006-01-02", day)
	if got := parseDate(t, nk["expiresAt"]); !got.Equal(want.Add(24*time.Hour - time.Second)) {
		t.Errorf("new key expiresAt = %v, want the end of %s", got, day)
	}
	ok2 := out["oldKey"].(map[string]any)
	if ok2["decryptOnly"] != true || ok2["wkdPublished"] != false || ok2["replacedBy"] != newID || ok2["retiredAt"] == nil {
		t.Errorf("old key = %v", ok2)
	}
	ids := h.must(t, "Identity/get", a, map[string]any{"ids": []string{h.identA.String()}})["list"].([]any)
	if got := ids[0].(map[string]any)["signingKeyId"]; got != newID {
		t.Errorf("Identity.signingKeyId = %v, want the new key %s", got, newID)
	}
	// WKD: the new key only. Revoking the retired old key does not bring it back
	// (it is unpublished; ADR-040 §4).
	if got := h.servedFingerprints(t); !strIn(got, nk["fingerprint"].(string)) || strIn(got, fprOld(oldKey)) {
		t.Errorf("WKD serves %v after the replace, want the new key %v and not the old", got, nk["fingerprint"])
	}
	fpr := oldKey["fingerprint"].(string)
	h.must(t, "UserKey/revoke", a, map[string]any{"id": oldID, "reason": "superseded", "confirm": fpr[len(fpr)-8:]})
	if got := h.servedFingerprints(t); !strIn(got, nk["fingerprint"].(string)) || strIn(got, fpr) {
		t.Errorf("WKD serves %v after revoking the old key, want the new key and not the old", got)
	}
	// The old key still decrypts: it is retired, not gone.
	var n int
	if err := h.super.QueryRow(context.Background(), `SELECT count(*) FROM app.user_keys WHERE id = $1 AND private_key_ct IS NOT NULL`, oldID).Scan(&n); err != nil || n != 1 {
		t.Errorf("the old key's private half is gone (n=%d err=%v)", n, err)
	}
}

func TestUserKeyReplaceUnpublishedStaysUnpublished(t *testing.T) {
	h := newKeyHarness(t)
	a := h.a.UserID
	oldID, _ := h.generate(t, a, h.identA, map[string]any{"bind": true})
	out := h.must(t, "UserKey/replace", a, map[string]any{"id": oldID})
	nk := out["key"].(map[string]any)
	if out["published"] != false || nk["wkdPublished"] != false {
		t.Errorf("replacing an unpublished key published the new one: %v", out)
	}
	if got := h.servedFingerprints(t); strIn(got, nk["fingerprint"].(string)) {
		t.Errorf("WKD serves the new key %v", nk["fingerprint"])
	}
}

func strIn(list []string, s string) bool {
	for _, x := range list {
		if x == s {
			return true
		}
	}
	return false
}

func fprOld(k map[string]any) string { return k["fingerprint"].(string) }

func TestUserKeyReplaceRefusals(t *testing.T) {
	h := newKeyHarness(t)
	a := h.a.UserID
	unbound, _ := h.generate(t, a, h.identA, nil)
	revoked, rk := h.generate(t, a, h.identA, map[string]any{"bind": true})
	fpr := rk["fingerprint"].(string)
	h.must(t, "UserKey/revoke", a, map[string]any{"id": revoked, "reason": "compromised", "confirm": fpr[len(fpr)-8:]})
	retired, _ := h.generate(t, a, h.identA, map[string]any{"bind": true})
	if ok, e := h.update(t, a, retired, map[string]any{"decryptOnly": true, "identityId": nil}); !ok {
		t.Fatalf("retire: %v", e)
	}
	// superseded: bound once, then another key was bound to the same address.
	superseded, _ := h.generate(t, a, h.identA, map[string]any{"bind": true})
	h.generate(t, a, h.identA, map[string]any{"bind": true})

	for name, c := range map[string]struct {
		args map[string]any
		want string
	}{
		"unbound":    {map[string]any{"id": unbound}, errKeyNotBound},
		"revoked":    {map[string]any{"id": revoked}, errKeyRevoked},
		"retired":    {map[string]any{"id": retired}, errKeyRetired},
		"superseded": {map[string]any{"id": superseded}, errKeyNotBound},
		"not mine":   {map[string]any{"id": "01J00000000000000000000000"}, errNotFound},
		"both":       {map[string]any{"id": superseded, "expiresDays": 10, "expiresAt": "2030-01-01"}, errInvalidArguments},
	} {
		calls := h.custody.count()
		var k, i int
		k, i = h.delta(t, a, func() {
			if _, mErr := h.invoke(t, "UserKey/replace", a, c.args); mErr == nil || mErr.Type != c.want {
				t.Errorf("%s: %+v, want %s", name, mErr, c.want)
			}
		})
		if h.custody.count() != calls || k != 0 || i != 0 {
			t.Errorf("%s: reached the crypto process (%d calls) or wrote rows (%d/%d)", name, h.custody.count()-calls, k, i)
		}
	}
}
