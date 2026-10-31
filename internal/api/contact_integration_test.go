// SPDX-License-Identifier: AGPL-3.0-only

//go:build integration

package api

// Integration tests for the Contact JMAP extension (#37) against a real
// Postgres: Contact/query autocomplete returns the right ids, Contact/get
// renders the object with its address + reached_me_at alias, and the REQUIRED
// cross-user RLS probe proves A can neither read nor affect B's contacts.

import (
	"context"
	"testing"

	"github.com/oklog/ulid/v2"

	"mailroom/internal/store"
)

// seedContact records a correspondent for userID through the store's ingest
// hook — the same path production ingest uses.
func seedContact(t *testing.T, h *apiHarness, userID ulid.ULID, name, addr, reachedMeAt string) {
	t.Helper()
	ctx := context.Background()
	if err := h.store.UserTx(ctx, userID, func(tx *store.UserTx) error {
		return tx.UpsertContactsFromMessage(ctx, store.ContactSourceMessage{
			FromName:         name,
			FromAddress:      addr,
			AddressedAddress: reachedMeAt,
		})
	}); err != nil {
		t.Fatalf("seed contact %q: %v", addr, err)
	}
}

// contactIDFor reads the contact id owning an address, as the superuser.
func contactIDFor(t *testing.T, h *apiHarness, userID ulid.ULID, addr string) ulid.ULID {
	t.Helper()
	var idStr string
	if err := h.super.QueryRow(context.Background(),
		`SELECT contact_id FROM app.contact_addresses WHERE user_id = $1 AND address = $2`,
		userID.String(), addr).Scan(&idStr); err != nil {
		t.Fatalf("contactIDFor %q: %v", addr, err)
	}
	id, err := ulid.Parse(idStr)
	if err != nil {
		t.Fatalf("parse contact id %q: %v", idStr, err)
	}
	return id
}

func TestContactGetQueryAndCrossUser(t *testing.T) {
	h := newAPIHarness(t)
	a, b := seedTwo(t, h.super)

	// Stored addresses are normalized (lowercased) by the ingest hook, so build
	// the expectations the same way — run carries an upper-case ULID.
	run := store.NewULID().String()
	alice := store.NormalizeAddress("alice-" + run + "@corp.test")
	aliceAlias := store.NormalizeAddress("me-" + run + "@example.com")
	bob := store.NormalizeAddress("bob-" + run + "@corp.test")

	seedContact(t, h, a.UserID, "Alice Example", alice, aliceAlias)
	seedContact(t, h, b.UserID, "Bob Roberts", bob, "")

	aliceID := contactIDFor(t, h, a.UserID, alice)
	bobID := contactIDFor(t, h, b.UserID, bob)

	// Contact/query by name substring returns alice's id.
	res := call(t, h.api.contactQuery, a.UserID, map[string]any{
		"filter": map[string]any{"text": "alice ex"},
	})
	ids := res["ids"].([]any)
	if len(ids) != 1 || ids[0] != aliceID.String() {
		t.Fatalf("Contact/query by name = %v, want [%s]", ids, aliceID)
	}

	// Contact/query by address prefix returns alice's id.
	res = call(t, h.api.contactQuery, a.UserID, map[string]any{
		"filter": map[string]any{"text": "alice-" + run},
	})
	if ids = res["ids"].([]any); len(ids) != 1 || ids[0] != aliceID.String() {
		t.Fatalf("Contact/query by address = %v, want [%s]", ids, aliceID)
	}

	// Contact/get renders the object with its address + reached_me_at alias.
	res = call(t, h.api.contactGet, a.UserID, map[string]any{"ids": []string{aliceID.String()}})
	list := res["list"].([]any)
	if len(list) != 1 {
		t.Fatalf("Contact/get own: list len %d, want 1", len(list))
	}
	obj := list[0].(map[string]any)
	if obj["displayName"] != "Alice Example" {
		t.Errorf("displayName = %v", obj["displayName"])
	}
	addrs := obj["addresses"].([]any)
	if len(addrs) != 1 {
		t.Fatalf("addresses = %v", obj["addresses"])
	}
	a0 := addrs[0].(map[string]any)
	if a0["address"] != alice {
		t.Errorf("address = %v, want %s", a0["address"], alice)
	}
	if a0["reachedMeAt"] != aliceAlias {
		t.Errorf("reachedMeAt = %v, want %s", a0["reachedMeAt"], aliceAlias)
	}

	// Cross-user Contact/get: A requesting B's contact id → notFound, no leak.
	res = call(t, h.api.contactGet, a.UserID, map[string]any{"ids": []string{bobID.String()}})
	if n := len(res["list"].([]any)); n != 0 {
		t.Errorf("A/Contact/get B leaked %d objects", n)
	}
	nf := res["notFound"].([]any)
	if len(nf) != 1 || nf[0] != bobID.String() {
		t.Errorf("A/Contact/get B: notFound = %v, want [%s]", nf, bobID)
	}

	// Cross-user Contact/query: A searching for B's contact returns nothing.
	res = call(t, h.api.contactQuery, a.UserID, map[string]any{
		"filter": map[string]any{"text": "bob-" + run},
	})
	if n := len(res["ids"].([]any)); n != 0 {
		t.Errorf("A/Contact/query for B's contact returned %d ids, want 0", n)
	}

	// Mutation-isolation probe: A recording the SAME address as B gets its OWN
	// separate contact; B's contact id and data are untouched.
	seedContact(t, h, a.UserID, "A's own Bob", bob, "")
	aBobID := contactIDFor(t, h, a.UserID, bob)
	if aBobID == bobID {
		t.Error("A and B share a contact row for the same address — isolation broken")
	}
	res = call(t, h.api.contactGet, b.UserID, map[string]any{"ids": []string{bobID.String()}})
	list = res["list"].([]any)
	if len(list) != 1 || list[0].(map[string]any)["displayName"] != "Bob Roberts" {
		t.Errorf("B's own contact was disturbed by A's write: %v", res["list"])
	}
}
