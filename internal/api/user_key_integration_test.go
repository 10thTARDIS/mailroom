// SPDX-License-Identifier: AGPL-3.0-only

//go:build integration

package api

// Integration test for UserKey/get (#448/#374 increment 1, spec §9): the
// login's own keys, public facts only, and fail-closed across users.

import (
	"context"
	"encoding/base64"
	"strings"
	"testing"

	"github.com/oklog/ulid/v2"

	"mailroom/internal/store"
)

func TestUserKeyGetAndCrossUser(t *testing.T) {
	h := newAPIHarness(t)
	a, b := seedTwo(t, h.super)
	ctx := context.Background()

	run := store.NewULID().String()
	fingerprint := "FPRUK" + run
	// A fake, non-empty ciphertext — real sealing is the crypto process's job
	// (internal/store/user_keys.go), but the negative control below only means
	// something if there is a distinctive value to look for.
	privateCT := []byte("fake-sealed-private-key-" + run)
	publicKeyBytes := []byte("fake-public-key-" + run)

	var keyID ulid.ULID
	if err := h.store.UserTx(ctx, a.UserID, func(tx *store.UserTx) error {
		id := store.NewULID()
		var err error
		keyID, err = tx.InsertUserKey(ctx, store.UserKey{
			ID:           id,
			Kind:         store.KeyPGP,
			Fingerprint:  fingerprint,
			PublicKey:    publicKeyBytes,
			PrivateKeyCT: privateCT,
			WKDPublished: true,
		})
		return err
	}); err != nil {
		t.Fatalf("seed user key: %v", err)
	}

	// UserKey/get ids:null lists it with the public facts.
	res := call(t, h.api.userKeyGet, a.UserID, map[string]any{})
	list := res["list"].([]any)
	if len(list) != 1 {
		t.Fatalf("UserKey/get list len = %d, want 1: %v", len(list), list)
	}
	obj := list[0].(map[string]any)
	if obj["id"] != keyID.String() {
		t.Errorf("id = %v, want %s", obj["id"], keyID)
	}
	// InsertUserKey upper-cases the fingerprint on write.
	if obj["fingerprint"] != strings.ToUpper(fingerprint) {
		t.Errorf("fingerprint = %v, want %q", obj["fingerprint"], strings.ToUpper(fingerprint))
	}
	if obj["kind"] != "pgp" {
		t.Errorf("kind = %v, want pgp", obj["kind"])
	}
	if obj["wkdPublished"] != true {
		t.Errorf("wkdPublished = %v, want true", obj["wkdPublished"])
	}
	if obj["decryptOnly"] != false {
		t.Errorf("decryptOnly = %v, want false", obj["decryptOnly"])
	}
	if obj["identityId"] != nil {
		t.Errorf("identityId = %v, want nil", obj["identityId"])
	}
	if _, ok := obj["createdAt"].(string); !ok {
		t.Errorf("createdAt = %v, want a UTCDate string", obj["createdAt"])
	}
	if obj["revokedAt"] != nil {
		t.Errorf("revokedAt = %v, want nil", obj["revokedAt"])
	}

	// Negative control: the raw JSON carries none of the private material,
	// however it might be spelled, nor its base64 form — nor the public key's,
	// which this method also never serialises.
	rawStr := string(mustJSON(t, res))
	forbidden := []string{
		"privateKey", "private_key_ct", "dekId", "dek_id", "publicKey", "public_key",
		base64.StdEncoding.EncodeToString(privateCT),
		base64.StdEncoding.EncodeToString(publicKeyBytes),
	}
	for _, f := range forbidden {
		if strings.Contains(rawStr, f) {
			t.Errorf("UserKey/get response leaked private material: contains %q\n%s", f, rawStr)
		}
	}

	// Fail closed: B's UserKey/get sees nothing of A's key, by any door.
	bAll := call(t, h.api.userKeyGet, b.UserID, map[string]any{})
	if bList, _ := bAll["list"].([]any); len(bList) != 0 {
		t.Errorf("B's UserKey/get (all) = %v, want []", bList)
	}
	bByID := call(t, h.api.userKeyGet, b.UserID, map[string]any{"ids": []string{keyID.String()}})
	if bList, _ := bByID["list"].([]any); len(bList) != 0 {
		t.Errorf("B's UserKey/get by A's id returned %d objects, want 0", len(bList))
	}
	nf, _ := bByID["notFound"].([]any)
	if len(nf) != 1 || nf[0] != keyID.String() {
		t.Errorf("B's UserKey/get by A's id: notFound = %v, want [%s]", nf, keyID)
	}
}
