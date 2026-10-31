// SPDX-License-Identifier: AGPL-3.0-only

package api

// Unit tests (no database) for the Contact JMAP extension rendering: the object
// shape, the nullable reachedMeAt alias, and the requested-property subset.

import (
	"testing"
	"time"

	"github.com/oklog/ulid/v2"

	"mailroom/internal/store"
)

func TestContactObjectFull(t *testing.T) {
	id := ulid.MustParse("00000000000000000000000001")
	reached := "me@example.com"
	c := store.Contact{
		ID:          id,
		DisplayName: "Alice Example",
		Addresses: []store.ContactAddress{
			{Address: "alice@ext.com", ReachedMeAt: &reached},
		},
	}
	obj := contactObject(c, nil, nil)

	if obj["id"] != id.String() {
		t.Errorf("id = %v", obj["id"])
	}
	if obj["displayName"] != "Alice Example" {
		t.Errorf("displayName = %v", obj["displayName"])
	}
	addrs, ok := obj["addresses"].([]map[string]any)
	if !ok || len(addrs) != 1 {
		t.Fatalf("addresses = %v", obj["addresses"])
	}
	if addrs[0]["address"] != "alice@ext.com" {
		t.Errorf("address = %v", addrs[0]["address"])
	}
	if addrs[0]["reachedMeAt"] != "me@example.com" {
		t.Errorf("reachedMeAt = %v", addrs[0]["reachedMeAt"])
	}
	// keys defaults to an empty array when the contact holds none — never
	// omitted (it is served by default alongside displayName/addresses).
	if keys, ok := obj["keys"].([]map[string]any); !ok || len(keys) != 0 {
		t.Errorf("keys = %v, want []", obj["keys"])
	}
	// The properties are isVip/isBlocked (#59), never the bare spec names
	// vip/blocked, which this server does not serve.
	for _, k := range []string{"vip", "blocked"} {
		if _, present := obj[k]; present {
			t.Errorf("unexpected property %q on Contact object", k)
		}
	}
}

// A contact holding a key renders its facts and never the key material.
func TestContactObjectWithKeys(t *testing.T) {
	id := ulid.MustParse("00000000000000000000000004")
	c := store.Contact{ID: id, DisplayName: "Dave", Addresses: []store.ContactAddress{{Address: "dave@ext.com"}}}
	fetched := time.Date(2026, 1, 2, 3, 4, 5, 0, time.UTC)
	keys := []store.ContactKey{{
		ContactID:   id,
		Kind:        store.KeyPGP,
		Fingerprint: "ABCD1234",
		PublicKey:   []byte("should never appear"),
		Source:      store.ContactKeyWKD,
		FetchedAt:   fetched,
	}}
	obj := contactObject(c, keys, nil)

	got, ok := obj["keys"].([]map[string]any)
	if !ok || len(got) != 1 {
		t.Fatalf("keys = %v, want 1 entry", obj["keys"])
	}
	k := got[0]
	if k["fingerprint"] != "ABCD1234" {
		t.Errorf("fingerprint = %v", k["fingerprint"])
	}
	if k["kind"] != "pgp" {
		t.Errorf("kind = %v", k["kind"])
	}
	if k["source"] != "wkd" {
		t.Errorf("source = %v", k["source"])
	}
	if k["fetchedAt"] != "2026-01-02T03:04:05Z" {
		t.Errorf("fetchedAt = %v", k["fetchedAt"])
	}
	if _, present := k["publicKey"]; present {
		t.Error("publicKey must never be rendered on a Contact key object")
	}
}

// An address with no known reply-from alias renders reachedMeAt as JSON null.
func TestContactObjectNoReachedMeAt(t *testing.T) {
	id := ulid.MustParse("00000000000000000000000002")
	c := store.Contact{
		ID:          id,
		DisplayName: "",
		Addresses:   []store.ContactAddress{{Address: "bob@ext.com"}},
	}
	obj := contactObject(c, nil, nil)
	addrs := obj["addresses"].([]map[string]any)
	if v, ok := addrs[0]["reachedMeAt"]; !ok || v != nil {
		t.Errorf("reachedMeAt = %v (ok=%v), want nil", v, ok)
	}
}

func TestContactObjectPropertySubset(t *testing.T) {
	id := ulid.MustParse("00000000000000000000000003")
	c := store.Contact{ID: id, DisplayName: "Carol", Addresses: []store.ContactAddress{{Address: "carol@ext.com"}}}
	obj := contactObject(c, nil, map[string]bool{"displayName": true})

	if _, ok := obj["addresses"]; ok {
		t.Error("addresses should be excluded when not requested")
	}
	if obj["id"] != id.String() {
		t.Error("id must always be present")
	}
	if obj["displayName"] != "Carol" {
		t.Errorf("displayName = %v", obj["displayName"])
	}
}
