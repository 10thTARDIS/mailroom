// SPDX-License-Identifier: AGPL-3.0-only

//go:build integration

package store

// Integration suite for the auto-derived contacts layer (#37, spec §10a): the
// derivation rules (From is a correspondent that carries reached_me_at; To/Cc
// are correspondents that do not; the user's own addresses never become
// contacts), the change_log discipline (a row on create / name-fill /
// reached_me_at change, NEVER on a pure re-sighting), the seen_count bookkeeping,
// autocomplete ordering, and RLS isolation. Runs against the throwaway Postgres
// from scripts/integration-test.sh; skips when the env is absent (harness_test.go).

import (
	"context"
	"testing"

	"github.com/oklog/ulid/v2"
)

// contactChangeCount counts the Contact change_log rows for one object id, read
// as the superuser (an independent check of what the store actually wrote).
func contactChangeCount(t *testing.T, h *harness, userID, objectID string) int {
	t.Helper()
	var n int
	if err := h.super.QueryRow(context.Background(), `
		SELECT count(*) FROM app.change_log
		 WHERE user_id = $1 AND object_type = 'Contact' AND object_id = $2`,
		userID, objectID).Scan(&n); err != nil {
		t.Fatalf("count contact change_log: %v", err)
	}
	return n
}

// contactAddrRow reads one contact_addresses row directly (bypassing the Go
// projection) to prove real DB state.
func contactAddrRow(t *testing.T, h *harness, userID, address string) (contactID string, reachedMeAt string, seenCount int, found bool) {
	t.Helper()
	err := h.super.QueryRow(context.Background(), `
		SELECT contact_id, COALESCE(reached_me_at, ''), seen_count
		  FROM app.contact_addresses
		 WHERE user_id = $1 AND address = $2`,
		userID, address).Scan(&contactID, &reachedMeAt, &seenCount)
	if err != nil {
		if isNoRows(err) {
			return "", "", 0, false
		}
		t.Fatalf("read contact_addresses: %v", err)
	}
	return contactID, reachedMeAt, seenCount, true
}

// A received message from an external sender addressed to one of the user's
// aliases creates a contact for the sender with reached_me_at set, seen_count 1,
// and exactly ONE Contact change_log row. The user's own address (the To) never
// becomes a contact. A second identical message re-sights (seen_count 2) but
// appends NO new change row; a later message that changes reached_me_at DOES.
func TestUpsertContactsFromMessage(t *testing.T) {
	h := newHarness(t)
	a, _ := seedTwoUsers(t, h.super)
	s := h.appStore()
	aID := mustULID(t, a.ID)

	// Addresses are stored normalized (lowercased, matching how ingest stores
	// addressed_address), so build the expectations the same way — the tags carry
	// upper-case ULIDs.
	myAddr := NormalizeAddress(a.Tag + "@example.com") // the user's verified fixed identity (seedUser)
	run := NewULID().String()
	alice := NormalizeAddress("alice-" + run + "@ext.com")
	otherAlias := NormalizeAddress("other-" + run + "@example.com")

	// #1 — received from alice, delivered to myAddr (To = me).
	mustUpsert(t, s, aID, ContactSourceMessage{
		FromName: "Alice Example", FromAddress: alice,
		Recipients:       []ContactParticipant{{Address: myAddr}},
		AddressedAddress: myAddr,
	})

	aliceContact, reached, seen, found := contactAddrRow(t, h, a.ID, alice)
	if !found {
		t.Fatal("no contact_addresses row created for alice")
	}
	if reached != myAddr {
		t.Errorf("reached_me_at = %q, want %q", reached, myAddr)
	}
	if seen != 1 {
		t.Errorf("seen_count = %d, want 1", seen)
	}
	if n := contactChangeCount(t, h, a.ID, aliceContact); n != 1 {
		t.Errorf("Contact change rows after create = %d, want 1", n)
	}
	// The user's own address (To = me) must NOT have become a contact.
	if _, _, _, meFound := contactAddrRow(t, h, a.ID, myAddr); meFound {
		t.Error("the user's own address was turned into a contact")
	}

	// #2 — identical message: re-sighting only. seen_count moves, no new row.
	mustUpsert(t, s, aID, ContactSourceMessage{
		FromName: "Alice Example", FromAddress: alice,
		Recipients:       []ContactParticipant{{Address: myAddr}},
		AddressedAddress: myAddr,
	})
	_, _, seen, _ = contactAddrRow(t, h, a.ID, alice)
	if seen != 2 {
		t.Errorf("seen_count after re-sighting = %d, want 2", seen)
	}
	if n := contactChangeCount(t, h, a.ID, aliceContact); n != 1 {
		t.Errorf("Contact change rows after pure re-sighting = %d, want still 1", n)
	}

	// #3 — same sender, a DIFFERENT reached-me alias: synced state moved, so a
	// change row IS appended (and the alias is refreshed).
	mustUpsert(t, s, aID, ContactSourceMessage{
		FromName: "Alice Example", FromAddress: alice,
		AddressedAddress: otherAlias,
	})
	_, reached, seen, _ = contactAddrRow(t, h, a.ID, alice)
	if reached != otherAlias {
		t.Errorf("reached_me_at after alias change = %q, want %q", reached, otherAlias)
	}
	if seen != 3 {
		t.Errorf("seen_count = %d, want 3", seen)
	}
	if n := contactChangeCount(t, h, a.ID, aliceContact); n != 2 {
		t.Errorf("Contact change rows after reached_me_at change = %d, want 2", n)
	}
}

// A sent copy (From is the user) contributes its To/Cc as correspondents with no
// reached_me_at, and never a contact for the user themselves.
func TestUpsertContactsSentCopy(t *testing.T) {
	h := newHarness(t)
	ctx := context.Background()
	a, _ := seedTwoUsers(t, h.super)
	s := h.appStore()
	aID := mustULID(t, a.ID)

	myAddr := a.Tag + "@example.com"
	run := NewULID().String()
	bob := "bob-" + run + "@ext.com"

	mustUpsert(t, s, aID, ContactSourceMessage{
		FromName: "", FromAddress: myAddr, // From = me (sent copy)
		Recipients:       []ContactParticipant{{Address: bob, Name: "Bob"}},
		AddressedAddress: bob, // derived alias points at the recipient; irrelevant here
	})

	_, reached, seen, found := contactAddrRow(t, h, a.ID, bob)
	if !found {
		t.Fatal("no contact created for the To recipient of a sent message")
	}
	if reached != "" {
		t.Errorf("reached_me_at for a To recipient = %q, want empty", reached)
	}
	if seen != 1 {
		t.Errorf("seen_count = %d, want 1", seen)
	}
	if _, _, _, meFound := contactAddrRow(t, h, a.ID, myAddr); meFound {
		t.Error("the user's own From address was turned into a contact on a sent copy")
	}
	_ = ctx
}

// Autocomplete matches on address OR display name (case-insensitive substring)
// and orders most-corresponded-with first (seen_count desc).
func TestQueryContactsForAutocomplete(t *testing.T) {
	h := newHarness(t)
	ctx := context.Background()
	a, _ := seedTwoUsers(t, h.super)
	s := h.appStore()
	aID := mustULID(t, a.ID)

	run := NewULID().String()
	alice := NormalizeAddress("alice-" + run + "@corp.test")
	bob := NormalizeAddress("bob-" + run + "@corp.test")

	// Alice seen 3×, Bob 1× — both under @corp.test so a domain query returns both.
	for i := 0; i < 3; i++ {
		mustUpsert(t, s, aID, ContactSourceMessage{FromName: "Alice Example", FromAddress: alice, AddressedAddress: a.Tag + "@example.com"})
	}
	mustUpsert(t, s, aID, ContactSourceMessage{FromName: "Bob Roberts", FromAddress: bob, AddressedAddress: a.Tag + "@example.com"})

	err := s.UserTx(ctx, aID, func(tx *UserTx) error {
		// By display-name substring.
		byName, err := tx.QueryContactsForAutocomplete(ctx, "alice ex", 10)
		if err != nil {
			return err
		}
		if !matchAddress(byName, alice) {
			t.Errorf("query by name did not return alice: %+v", byName)
		}
		// By address prefix.
		byAddr, err := tx.QueryContactsForAutocomplete(ctx, "alice-"+run, 10)
		if err != nil {
			return err
		}
		if !matchAddress(byAddr, alice) {
			t.Errorf("query by address did not return alice: %+v", byAddr)
		}
		// Ordering: a shared domain query returns alice (3) before bob (1).
		both, err := tx.QueryContactsForAutocomplete(ctx, run+"@corp.test", 10)
		if err != nil {
			return err
		}
		gotAlice, gotBob := indexOfAddress(both, alice), indexOfAddress(both, bob)
		if gotAlice < 0 || gotBob < 0 {
			t.Fatalf("shared-domain query missing a contact: alice=%d bob=%d (%+v)", gotAlice, gotBob, both)
		}
		if gotAlice > gotBob {
			t.Errorf("ordering: alice (seen 3) at %d must precede bob (seen 1) at %d", gotAlice, gotBob)
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
}

// RLS isolation at the store layer: user B's autocomplete and GetContacts never
// return user A's contacts, and B upserting the SAME address creates B's own
// separate contact without touching A's row.
func TestContactRLSIsolation(t *testing.T) {
	h := newHarness(t)
	ctx := context.Background()
	a, b := seedTwoUsers(t, h.super)
	s := h.appStore()
	aID, bID := mustULID(t, a.ID), mustULID(t, b.ID)

	run := NewULID().String()
	shared := NormalizeAddress("shared-" + run + "@corp.test")

	// A records the address with an alias.
	mustUpsert(t, s, aID, ContactSourceMessage{FromName: "A View", FromAddress: shared, AddressedAddress: a.Tag + "@example.com"})
	aContactIDStr, aReached, _, _ := contactAddrRow(t, h, a.ID, shared)
	aContactID := mustULID(t, aContactIDStr)

	// B's autocomplete for the same domain returns nothing (A's rows are
	// invisible), and B cannot fetch A's contact by id.
	err := s.UserTx(ctx, bID, func(tx *UserTx) error {
		matches, err := tx.QueryContactsForAutocomplete(ctx, run+"@corp.test", 10)
		if err != nil {
			return err
		}
		if len(matches) != 0 {
			t.Errorf("B's autocomplete leaked %d of A's contacts", len(matches))
		}
		got, err := tx.GetContacts(ctx, []ulid.ULID{aContactID})
		if err != nil {
			return err
		}
		if len(got) != 0 {
			t.Errorf("B's GetContacts(A's id) returned %d contacts, want 0", len(got))
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}

	// B records the SAME address: gets its own contact, A's is untouched.
	mustUpsert(t, s, bID, ContactSourceMessage{FromName: "B View", FromAddress: shared, AddressedAddress: b.Tag + "@example.com"})
	bContactID, bReached, _, _ := contactAddrRow(t, h, b.ID, shared)
	if bContactID == aContactIDStr {
		t.Error("A and B share a contact row for the same address — RLS/isolation broken")
	}
	if bReached != NormalizeAddress(b.Tag+"@example.com") {
		t.Errorf("B's reached_me_at = %q, want B's own alias", bReached)
	}
	// A's row is unchanged by B's write.
	_, aReachedAfter, _, _ := contactAddrRow(t, h, a.ID, shared)
	if aReachedAfter != aReached {
		t.Errorf("A's reached_me_at changed after B's write: %q -> %q", aReached, aReachedAfter)
	}
}

// mustUpsert runs UpsertContactsFromMessage in its own UserTx and fails on error.
func mustUpsert(t *testing.T, s *Store, userID ulid.ULID, msg ContactSourceMessage) {
	t.Helper()
	ctx := context.Background()
	if err := s.UserTx(ctx, userID, func(tx *UserTx) error {
		return tx.UpsertContactsFromMessage(ctx, msg)
	}); err != nil {
		t.Fatalf("UpsertContactsFromMessage: %v", err)
	}
}

func matchAddress(ms []ContactMatch, addr string) bool {
	return indexOfAddress(ms, addr) >= 0
}

func indexOfAddress(ms []ContactMatch, addr string) int {
	for i, m := range ms {
		if m.Address == addr {
			return i
		}
	}
	return -1
}
