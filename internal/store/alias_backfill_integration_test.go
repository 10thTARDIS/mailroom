// SPDX-License-Identifier: AGPL-3.0-only

//go:build integration

package store

// Integration suite for the alias/contacts repair surface (mailroom#177): the
// guarded addressed-alias update, finding and removing contacts that are really
// the user's OWN addresses, and the outbound_count recompute.
//
// The load-bearing invariant (CLAUDE.md) is the focus. deliveredTo/identityId
// and contacts are JMAP-visible, so a real change appends exactly ONE change_log
// row in the same UserTx and a no-op appends NONE — which is also what makes
// these commands safe to re-run, the property an operator is being asked to
// trust. outbound_count is NOT JMAP-visible and must append nothing at all.
//
// Runs against the throwaway Postgres from scripts/integration-test.sh; skips
// when the env is absent (harness_test.go).

import (
	"context"
	"testing"

	"github.com/oklog/ulid/v2"
)

// contactChangeRows counts every Contact change_log row for one object id,
// regardless of op, read inside the caller's transaction.
func contactChangeRows(t *testing.T, tx *UserTx, objectID string) int {
	t.Helper()
	var n int
	if err := tx.tx.QueryRow(context.Background(), `
		SELECT count(*) FROM app.change_log
		 WHERE user_id = $1 AND object_type = 'Contact' AND object_id = $2`,
		tx.userID.String(), objectID).Scan(&n); err != nil {
		t.Fatalf("count Contact change_log: %v", err)
	}
	return n
}

func TestUpdateAddressedAlias(t *testing.T) {
	h := newHarness(t)
	ctx := context.Background()
	a, _ := seedTwoUsers(t, h.super)
	s := h.appStore()
	aID := mustULID(t, a.ID)
	msgID := mustULID(t, a.MessageID)

	// The identity seedUser created for this user; a real backfill attaches the
	// resolved identity alongside the address, so the test does too.
	var identStr string
	if err := h.super.QueryRow(ctx,
		`SELECT id FROM app.identities WHERE user_id = $1 LIMIT 1`, a.ID).Scan(&identStr); err != nil {
		t.Fatalf("read seeded identity: %v", err)
	}
	identID := mustULID(t, identStr)
	alias := NormalizeAddress("sub-" + a.Tag + "@example.com")

	if err := s.UserTx(ctx, aID, func(tx *UserTx) error {
		before := emailUpdatedCount(t, tx, a.MessageID)

		// 1. Real change → both columns written, exactly one new Email/updated.
		changed, err := tx.UpdateAddressedAlias(ctx, msgID, alias, identID)
		if err != nil {
			return err
		}
		if !changed {
			t.Error("first UpdateAddressedAlias: changed = false, want true")
		}
		if got := emailUpdatedCount(t, tx, a.MessageID); got != before+1 {
			t.Errorf("change_log Email/updated after change = %d, want %d", got, before+1)
		}
		var gotAddr, gotIdent string
		if err := tx.tx.QueryRow(ctx, `
			SELECT COALESCE(addressed_address::text, ''), COALESCE(addressed_identity_id::text, '')
			  FROM app.messages WHERE user_id = $1 AND id = $2`,
			tx.userID.String(), msgID.String()).Scan(&gotAddr, &gotIdent); err != nil {
			return err
		}
		if gotAddr != alias {
			t.Errorf("addressed_address = %q, want %q", gotAddr, alias)
		}
		if gotIdent != identID.String() {
			t.Errorf("addressed_identity_id = %q, want %q", gotIdent, identID)
		}

		// 2. RE-RUN THE BACKFILL: identical values → no write, no change_log row.
		// This is the idempotence the operator is promised.
		changed, err = tx.UpdateAddressedAlias(ctx, msgID, alias, identID)
		if err != nil {
			return err
		}
		if changed {
			t.Error("re-running with identical values: changed = true, want false")
		}
		if got := emailUpdatedCount(t, tx, a.MessageID); got != before+1 {
			t.Errorf("change_log after no-op = %d, want %d (no new row)", got, before+1)
		}

		// 3. SAME ADDRESS, IDENTITY CLEARED. The address alone is not the state:
		// an alias that resolved to an identity and one that resolved to nothing
		// are different answers, and a guard comparing only the address would
		// silently keep a stale identity.
		changed, err = tx.UpdateAddressedAlias(ctx, msgID, alias, ulid.ULID{})
		if err != nil {
			return err
		}
		if !changed {
			t.Error("clearing the identity: changed = false, want true")
		}
		if err := tx.tx.QueryRow(ctx, `
			SELECT COALESCE(addressed_identity_id::text, '')
			  FROM app.messages WHERE user_id = $1 AND id = $2`,
			tx.userID.String(), msgID.String()).Scan(&gotIdent); err != nil {
			return err
		}
		if gotIdent != "" {
			t.Errorf("addressed_identity_id after clear = %q, want empty", gotIdent)
		}

		// 4. Unknown id → no error, no change, no change_log row.
		other := NewULID()
		changed, err = tx.UpdateAddressedAlias(ctx, other, alias, identID)
		if err != nil {
			return err
		}
		if changed {
			t.Error("unknown message id: changed = true, want false")
		}
		if got := emailUpdatedCount(t, tx, other.String()); got != 0 {
			t.Errorf("change_log rows for an unknown id = %d, want 0", got)
		}
		return nil
	}); err != nil {
		t.Fatalf("UserTx: %v", err)
	}
}

// THE #177 SCENARIO END TO END: contacts are derived BEFORE the catchall
// identity exists (so the user's own aliases are filed as correspondents), the
// wildcard is then provisioned, and the repair finds and removes exactly those
// rows — leaving genuine correspondents alone.
func TestOwnAddressContactsRepair(t *testing.T) {
	h := newHarness(t)
	ctx := context.Background()
	a, _ := seedTwoUsers(t, h.super)
	s := h.appStore()
	aID := mustULID(t, a.ID)

	// A domain of this user's own, distinct from the fixture's example.com so the
	// wildcard cannot accidentally cover the other fixture user.
	run := NewULID().String()
	catchall := "catchall-" + NormalizeAddress(run) + ".example"
	mine := NormalizeAddress("sub@" + catchall)
	alsoMine := NormalizeAddress("bank@" + catchall)
	stranger := NormalizeAddress("alice-" + run + "@ext.example")

	// 1. Ingest-time derivation with NO wildcard: the user's own aliases become
	// contacts, because nothing yet says they are the user's.
	if err := s.UserTx(ctx, aID, func(tx *UserTx) error {
		for _, addr := range []string{mine, alsoMine, stranger} {
			if err := tx.UpsertContactsFromMessage(ctx, ContactSourceMessage{
				FromAddress: addr, AddressedAddress: mine,
			}); err != nil {
				return err
			}
		}
		own, err := tx.OwnAddressContacts(ctx)
		if err != nil {
			return err
		}
		if len(own) != 0 {
			t.Fatalf("before the wildcard exists, own-address contacts = %d, want 0 "+
				"(nothing yet says these addresses are the user's)", len(own))
		}
		return nil
	}); err != nil {
		t.Fatalf("seed contacts: %v", err)
	}

	// 2. The operator provisions the catchall.
	if err := s.UserTx(ctx, aID, func(tx *UserTx) error {
		_, err := tx.CreateIdentity(ctx, IdentityRow{
			AccountID: mustULID(t, a.AccountID),
			Kind:      IdentityWildcard,
			Domain:    catchall,
			Enabled:   true,
		})
		return err
	}); err != nil {
		t.Fatalf("provision wildcard: %v", err)
	}

	// 3. The same rows are now recognisable as the user's own — and only those.
	var mineID ulid.ULID
	if err := s.UserTx(ctx, aID, func(tx *UserTx) error {
		own, err := tx.OwnAddressContacts(ctx)
		if err != nil {
			return err
		}
		if len(own) != 2 {
			t.Fatalf("own-address contacts = %d, want 2 (%s, %s)", len(own), mine, alsoMine)
		}
		found := map[string]OwnAddressContact{}
		for _, c := range own {
			found[c.Address] = c
			if c.Address == stranger {
				t.Error("a genuine correspondent was reported as one of the user's own addresses")
			}
			if !c.SoleAddress {
				t.Errorf("%s: SoleAddress = false, want true (auto-derived contacts are 1:1)", c.Address)
			}
		}
		if _, ok := found[mine]; !ok {
			t.Errorf("%s was not reported as an own address", mine)
		}
		mineID = found[mine].AddressID

		// 4. Remove one: the address goes, its now-empty contact goes, and exactly
		// one Contact `destroyed` row is appended.
		contactID := found[mine].ContactID
		before := contactChangeRows(t, tx, contactID.String())
		removed, err := tx.DeleteOwnAddressContact(ctx, mineID)
		if err != nil {
			return err
		}
		if !removed {
			t.Error("DeleteOwnAddressContact: removedContact = false, want true (sole address)")
		}
		if got := contactChangeRows(t, tx, contactID.String()); got != before+1 {
			t.Errorf("Contact change_log rows after removal = %d, want %d", got, before+1)
		}
		var stillThere int
		if err := tx.tx.QueryRow(ctx,
			`SELECT count(*) FROM app.contacts WHERE user_id = $1 AND id = $2`,
			tx.userID.String(), contactID.String()).Scan(&stillThere); err != nil {
			return err
		}
		if stillThere != 0 {
			t.Error("the emptied contact was left behind")
		}

		// 5. RE-RUN: already gone → no-op, and no second change_log row.
		removed, err = tx.DeleteOwnAddressContact(ctx, mineID)
		if err != nil {
			return err
		}
		if removed {
			t.Error("re-running the repair reported a second removal")
		}
		if got := contactChangeRows(t, tx, contactID.String()); got != before+1 {
			t.Errorf("Contact change_log rows after re-run = %d, want %d (no new row)", got, before+1)
		}

		// The genuine correspondent is untouched throughout.
		var strangerRows int
		if err := tx.tx.QueryRow(ctx,
			`SELECT count(*) FROM app.contact_addresses WHERE user_id = $1 AND address = $2`,
			tx.userID.String(), stranger).Scan(&strangerRows); err != nil {
			return err
		}
		if strangerRows != 1 {
			t.Errorf("genuine correspondent rows = %d, want 1", strangerRows)
		}
		return nil
	}); err != nil {
		t.Fatalf("repair: %v", err)
	}

	// 6. And ingest now excludes them by itself — the repair is for history only.
	if err := s.UserTx(ctx, aID, func(tx *UserTx) error {
		if err := tx.UpsertContactsFromMessage(ctx, ContactSourceMessage{
			FromAddress: mine, AddressedAddress: mine,
		}); err != nil {
			return err
		}
		var back int
		if err := tx.tx.QueryRow(ctx,
			`SELECT count(*) FROM app.contact_addresses WHERE user_id = $1 AND address = $2`,
			tx.userID.String(), mine).Scan(&back); err != nil {
			return err
		}
		if back != 0 {
			t.Error("an own address was filed as a contact again after the wildcard existed")
		}
		return nil
	}); err != nil {
		t.Fatalf("post-repair ingest: %v", err)
	}
}

// outbound_count is a scoring input, not a Contact property. Recomputing it must
// converge on re-run and must NOT fabricate sync traffic for every client.
func TestSetOutboundCount(t *testing.T) {
	h := newHarness(t)
	ctx := context.Background()
	a, _ := seedTwoUsers(t, h.super)
	s := h.appStore()
	aID := mustULID(t, a.ID)
	peer := NormalizeAddress("bob-" + NewULID().String() + "@ext.example")

	if err := s.UserTx(ctx, aID, func(tx *UserTx) error {
		if err := tx.UpsertContactsFromMessage(ctx, ContactSourceMessage{
			FromAddress: peer, AddressedAddress: NormalizeAddress(a.Tag + "@example.com"),
		}); err != nil {
			return err
		}
		var contactID string
		if err := tx.tx.QueryRow(ctx,
			`SELECT contact_id FROM app.contact_addresses WHERE user_id = $1 AND address = $2`,
			tx.userID.String(), peer).Scan(&contactID); err != nil {
			return err
		}
		before := contactChangeRows(t, tx, contactID)

		changed, err := tx.SetOutboundCount(ctx, peer, 7)
		if err != nil {
			return err
		}
		if !changed {
			t.Error("SetOutboundCount: changed = false, want true")
		}
		var got int
		if err := tx.tx.QueryRow(ctx,
			`SELECT outbound_count FROM app.contact_addresses WHERE user_id = $1 AND address = $2`,
			tx.userID.String(), peer).Scan(&got); err != nil {
			return err
		}
		if got != 7 {
			t.Errorf("outbound_count = %d, want 7", got)
		}
		// SET, not increment: the same recompute twice lands on the same number.
		if changed, err = tx.SetOutboundCount(ctx, peer, 7); err != nil {
			return err
		} else if changed {
			t.Error("re-running the recompute: changed = true, want false")
		}
		if err := tx.tx.QueryRow(ctx,
			`SELECT outbound_count FROM app.contact_addresses WHERE user_id = $1 AND address = $2`,
			tx.userID.String(), peer).Scan(&got); err != nil {
			return err
		}
		if got != 7 {
			t.Errorf("outbound_count after re-run = %d, want still 7 (SET, not increment)", got)
		}
		// Not JMAP-visible: no Contact change_log row for any of it.
		if after := contactChangeRows(t, tx, contactID); after != before {
			t.Errorf("Contact change_log rows = %d, want %d — outbound_count is not a "+
				"JMAP-visible property and must not generate sync traffic", after, before)
		}

		// An address with no contact row is a no-op, not an error: the user can
		// have written to someone who never wrote back.
		if changed, err = tx.SetOutboundCount(ctx, "nobody@ext.example", 3); err != nil {
			return err
		} else if changed {
			t.Error("SetOutboundCount for an unknown address: changed = true, want false")
		}
		return nil
	}); err != nil {
		t.Fatalf("UserTx: %v", err)
	}
}
