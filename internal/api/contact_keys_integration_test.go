// SPDX-License-Identifier: AGPL-3.0-only

//go:build integration

package api

// Integration tests for Contact.keys and Contact/query's hasKey filter
// (#448/#374 increment 1): Contact/get exposes a keyed contact's public key
// FACTS (fingerprint/kind/source/fetchedAt) and never the key material, even
// base64-encoded; Contact/query{hasKey} partitions correctly and reports a
// real `total`, including the limit:0 case (#469's lesson, applied here — see
// store.QueryContacts's own comment).

import (
	"context"
	"encoding/base64"
	"strings"
	"testing"

	"mailroom/internal/store"
)

func TestContactKeysGetAndQuery(t *testing.T) {
	h := newAPIHarness(t)
	a, _ := seedTwo(t, h.super)
	ctx := context.Background()

	run := store.NewULID().String()
	keyed := store.NormalizeAddress("keyed-" + run + "@corp.test")
	bare := store.NormalizeAddress("bare-" + run + "@corp.test")

	seedContact(t, h, a.UserID, "Keyed Correspondent", keyed, "")
	seedContact(t, h, a.UserID, "Bare Correspondent", bare, "")

	keyedID := contactIDFor(t, h, a.UserID, keyed)
	bareID := contactIDFor(t, h, a.UserID, bare)

	fingerprint := "FPR" + run
	publicKeyBytes := []byte("fake-public-key-bytes-" + run)
	if err := h.store.UserTx(ctx, a.UserID, func(tx *store.UserTx) error {
		_, err := tx.UpsertContactKey(ctx, store.ContactKeyRow{
			ContactID:   keyedID,
			Fingerprint: fingerprint,
			PublicKey:   publicKeyBytes,
			Source:      store.ContactKeyWKD,
		})
		return err
	}); err != nil {
		t.Fatalf("seed contact key: %v", err)
	}

	// Contact/get: the keyed contact carries its key facts; the bare one [].
	res := call(t, h.api.contactGet, a.UserID, map[string]any{
		"ids": []string{keyedID.String(), bareID.String()},
	})
	list := res["list"].([]any)
	if len(list) != 2 {
		t.Fatalf("Contact/get list len = %d, want 2: %v", len(list), list)
	}
	var keyedObj, bareObj map[string]any
	for _, item := range list {
		obj := item.(map[string]any)
		switch obj["id"] {
		case keyedID.String():
			keyedObj = obj
		case bareID.String():
			bareObj = obj
		}
	}
	if keyedObj == nil || bareObj == nil {
		t.Fatalf("Contact/get did not return both contacts: %v", list)
	}
	keys, _ := keyedObj["keys"].([]any)
	if len(keys) != 1 {
		t.Fatalf("keyed contact's keys = %v, want 1 entry", keyedObj["keys"])
	}
	keyObj := keys[0].(map[string]any)
	if keyObj["fingerprint"] != fingerprint {
		t.Errorf("fingerprint = %v, want %q", keyObj["fingerprint"], fingerprint)
	}
	if keyObj["kind"] != "pgp" {
		t.Errorf("kind = %v, want pgp", keyObj["kind"])
	}
	if keyObj["source"] != "wkd" {
		t.Errorf("source = %v, want wkd", keyObj["source"])
	}
	if _, ok := keyObj["fetchedAt"].(string); !ok {
		t.Errorf("fetchedAt = %v, want a UTCDate string", keyObj["fetchedAt"])
	}
	if bareKeys, ok := bareObj["keys"].([]any); !ok || len(bareKeys) != 0 {
		t.Errorf("bare contact's keys = %v, want []", bareObj["keys"])
	}

	// Negative control: the raw JSON never carries the key material, however
	// it might be spelled, nor its base64 form (decode-to-string, not a
	// hand-rolled substring guess).
	rawStr := string(mustJSON(t, res))
	forbidden := []string{"publicKey", "public_key", base64.StdEncoding.EncodeToString(publicKeyBytes)}
	for _, f := range forbidden {
		if strings.Contains(rawStr, f) {
			t.Errorf("Contact/get response leaked key material: contains %q\n%s", f, rawStr)
		}
	}

	// Contact/query{hasKey:true} returns exactly the keyed contact, total 1.
	// (a is a fresh user with exactly these two contacts, so no extra scoping
	// filter is needed to make the count exact.)
	res = call(t, h.api.contactQuery, a.UserID, map[string]any{
		"filter": map[string]any{"hasKey": true},
	})
	ids, _ := res["ids"].([]any)
	if len(ids) != 1 || ids[0] != keyedID.String() {
		t.Errorf("Contact/query{hasKey:true} ids = %v, want [%s]", ids, keyedID)
	}
	if total, _ := res["total"].(float64); total != 1 {
		t.Errorf("Contact/query{hasKey:true} total = %v, want 1", res["total"])
	}

	// {hasKey:false} returns the bare contact.
	res = call(t, h.api.contactQuery, a.UserID, map[string]any{
		"filter": map[string]any{"hasKey": false},
	})
	ids, _ = res["ids"].([]any)
	if len(ids) != 1 || ids[0] != bareID.String() {
		t.Errorf("Contact/query{hasKey:false} ids = %v, want [%s]", ids, bareID)
	}
	if total, _ := res["total"].(float64); total != 1 {
		t.Errorf("Contact/query{hasKey:false} total = %v, want 1", res["total"])
	}

	// {hasKey:true, limit:0, calculateTotal:true}: no ids, but the real total —
	// the #469 lesson (an explicit limit:0 must not silently apply the default
	// page instead).
	res = call(t, h.api.contactQuery, a.UserID, map[string]any{
		"filter":         map[string]any{"hasKey": true},
		"limit":          0,
		"calculateTotal": true,
	})
	if ids, ok := res["ids"].([]any); ok && len(ids) != 0 {
		t.Errorf("Contact/query{hasKey:true,limit:0} ids = %v, want []", ids)
	}
	if total, _ := res["total"].(float64); total != 1 {
		t.Errorf("Contact/query{hasKey:true,limit:0} total = %v, want 1", res["total"])
	}
}
