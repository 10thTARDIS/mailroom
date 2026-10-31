// SPDX-License-Identifier: AGPL-3.0-only

//go:build integration

package store

// THE SUBADDRESS FALLBACK (#62): "me+shop@d" is sendable when "me@d" is a
// verified fixed identity.
//
// This widens the send-as boundary, so it is tested the way identities_integration_test.go
// tests that boundary — the negatives harder than the positives. Three things
// must hold, and each has a test whose failure would be a real defect rather
// than a cosmetic one:
//
//  1. the base must be VERIFIED and ENABLED — a subaddress of an address the
//     user does not own resolves to nothing;
//  2. NO MESSAGE DATA participates. The rejected design authorized a subaddress
//     once mail had "arrived" at it, which a sender can forge (Delivered-To is
//     attacker-supplied — internal/alias/derive.go). The test for this asserts
//     that a forged header changes nothing, because the mechanism it would have
//     exploited must not exist;
//  3. a catchall user's behaviour is UNCHANGED. The fallback is last, after the
//     exact and wildcard matches, so introducing it must not move which identity
//     an existing deployment resolves.

import (
	"context"
	"errors"
	"testing"

	"github.com/oklog/ulid/v2"
)

// TestSubaddressResolvesFromItsVerifiedBase: the feature itself.
func TestSubaddressResolvesFromItsVerifiedBase(t *testing.T) {
	h := newHarness(t)
	ctx := context.Background()
	a, _ := seedTwoUsers(t, h.super)
	s := h.appStore()
	aID := mustULID(t, a.ID)
	acct := seedAccount(t, a)

	if err := s.UserTx(ctx, aID, func(tx *UserTx) error {
		base, err := tx.CreateIdentity(ctx, IdentityRow{
			AccountID: acct, Kind: IdentityFixed,
			Address: "me@plain.example.com", Enabled: true, DisplayName: "Me",
		})
		if err != nil {
			return err
		}

		for _, probe := range []string{
			"me+shop@plain.example.com",
			"me+a+b@plain.example.com",         // split at the FIRST '+'
			"ME+Shop@Plain.Example.COM",        // normalized before matching
			"me+" + "x" + "@plain.example.com", // a one-character detail
		} {
			si, err := tx.ResolveSendIdentity(ctx, probe)
			if err != nil {
				t.Errorf("ResolveSendIdentity(%q): %v — a subaddress of a verified address must be sendable", probe, err)
				continue
			}
			if si.IdentityID != base {
				t.Errorf("%q resolved to %s, want the base identity %s", probe, si.IdentityID, base)
			}
			if !si.ViaSubaddress {
				t.Errorf("%q resolved without ViaSubaddress set — Identity/set would then "+
					"refuse to name it, reporting a duplicate that does not exist", probe)
			}
			// THE FROM ADDRESS IS THE SUBADDRESS, not the base. It is what goes
			// on the wire, and what ingest records as the message's
			// addressed_address — so `deliveredTo` and the `alias` rule field
			// keep naming the exact address the sender used. Returning the base
			// here would silently collapse every subaddress into one.
			if si.FromAddress != NormalizeAddress(probe) {
				t.Errorf("%q resolved with FromAddress %q, want the subaddress itself", probe, si.FromAddress)
			}
			// The base's display name carries over: it is the same person.
			if si.DisplayName != "Me" {
				t.Errorf("%q lost the base identity's display name (%q)", probe, si.DisplayName)
			}
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
}

// TestSubaddressOfAnUnverifiedBaseIsRefused: the boundary. Owning "me@d" must
// not imply anything about "someone-else@d".
func TestSubaddressOfAnUnverifiedBaseIsRefused(t *testing.T) {
	h := newHarness(t)
	ctx := context.Background()
	a, _ := seedTwoUsers(t, h.super)
	s := h.appStore()
	aID := mustULID(t, a.ID)
	acct := seedAccount(t, a)

	if err := s.UserTx(ctx, aID, func(tx *UserTx) error {
		if _, err := tx.CreateIdentity(ctx, IdentityRow{
			AccountID: acct, Kind: IdentityFixed,
			Address: "me@plain.example.com", Enabled: true,
		}); err != nil {
			return err
		}
		for _, probe := range []string{
			"boss+tag@plain.example.com", // a DIFFERENT local part at the same domain
			"me+shop@other.example.com",  // the right local part at another domain
			"me@other.example.com",       // the base itself, elsewhere
			"+shop@plain.example.com",    // an empty base local-part
			"me+@plain.example.com",      // an empty detail
			"mex+shop@plain.example.com", // a base that merely starts the same
		} {
			if _, err := tx.ResolveSendIdentity(ctx, probe); !errors.Is(err, ErrFromNotVerified) {
				t.Errorf("ResolveSendIdentity(%q) = %v, want ErrFromNotVerified — "+
					"this would let the user send as an address they do not own", probe, err)
			}
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
}

// A DISABLED identity authorizes nothing, including its subaddresses. Disabling
// is how an address is taken away; if the subaddresses survived it, the
// revocation would be partial and silent.
func TestSubaddressOfADisabledBaseIsRefused(t *testing.T) {
	h := newHarness(t)
	ctx := context.Background()
	a, _ := seedTwoUsers(t, h.super)
	s := h.appStore()
	aID := mustULID(t, a.ID)
	acct := seedAccount(t, a)

	if err := s.UserTx(ctx, aID, func(tx *UserTx) error {
		if _, err := tx.CreateIdentity(ctx, IdentityRow{
			AccountID: acct, Kind: IdentityFixed,
			Address: "off@plain.example.com", Enabled: false,
		}); err != nil {
			return err
		}
		// The control: the base is refused too, so the test below is about
		// `enabled` and not about a typo in the address.
		if _, err := tx.ResolveSendIdentity(ctx, "off@plain.example.com"); !errors.Is(err, ErrFromNotVerified) {
			t.Fatalf("a disabled base resolved: %v", err)
		}
		if _, err := tx.ResolveSendIdentity(ctx, "off+shop@plain.example.com"); !errors.Is(err, ErrFromNotVerified) {
			t.Error("a disabled identity still authorized its subaddress — revoking an " +
				"address must revoke everything it covered")
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
}

// TestSubaddressDoesNotChangeCatchallResolution — THE NON-REGRESSION.
//
// The fallback runs only when the main query matched nothing, so a deployment
// with a domain_wildcard must resolve exactly as it did before this existed.
// This is the test that would catch the fallback being folded into the main
// query, or ordered ahead of the wildcard: both look harmless and would silently
// change which identity (and so which signature and display name) a catchall
// user sends every subaddressed message from.
func TestSubaddressDoesNotChangeCatchallResolution(t *testing.T) {
	h := newHarness(t)
	ctx := context.Background()
	a, _ := seedTwoUsers(t, h.super)
	s := h.appStore()
	aID := mustULID(t, a.ID)
	acct := seedAccount(t, a)

	if err := s.UserTx(ctx, aID, func(tx *UserTx) error {
		wildcard, err := tx.CreateIdentity(ctx, IdentityRow{
			AccountID: acct, Kind: IdentityWildcard,
			Domain: "cat.example.com", Enabled: true, DisplayName: "Catchall",
		})
		if err != nil {
			return err
		}
		fixed, err := tx.CreateIdentity(ctx, IdentityRow{
			AccountID: acct, Kind: IdentityFixed,
			Address: "me@cat.example.com", Enabled: true, DisplayName: "Me",
		})
		if err != nil {
			return err
		}

		// A subaddress of the fixed identity, at a domain the wildcard covers.
		// The WILDCARD must win, because it matched the main query — the same
		// answer this returned before the fallback existed.
		si, err := tx.ResolveSendIdentity(ctx, "me+shop@cat.example.com")
		if err != nil {
			return err
		}
		if si.IdentityID != wildcard {
			t.Errorf("a catchall user's subaddress resolved to %s, want the wildcard %s — "+
				"the fallback must not outrank a match the main query already made", si.IdentityID, wildcard)
		}
		if si.ViaSubaddress {
			t.Error("a wildcard match was reported as ViaSubaddress")
		}
		if si.Kind != IdentityWildcard {
			t.Errorf("Kind = %q, want domain_wildcard", si.Kind)
		}

		// And an exact fixed identity still outranks the wildcard, unchanged.
		si, err = tx.ResolveSendIdentity(ctx, "me@cat.example.com")
		if err != nil {
			return err
		}
		if si.IdentityID != fixed {
			t.Errorf("the exact fixed identity no longer wins: got %s, want %s", si.IdentityID, fixed)
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
}

// TestSubaddressAuthorizationIgnoresMessageHeaders — THE SECURITY PROPERTY.
//
// The design this replaced would have authorized a subaddress once a message
// "arrived" at it. `Delivered-To` is attacker-supplied (internal/alias/derive.go
// is built around that fact), so that would let a sender mint entries in the
// user's own From picker. MatchVerifiedAddress is the seam where header-derived
// candidates meet authorization; feeding it forged candidates must move nothing.
func TestSubaddressAuthorizationIgnoresMessageHeaders(t *testing.T) {
	h := newHarness(t)
	ctx := context.Background()
	a, _ := seedTwoUsers(t, h.super)
	s := h.appStore()
	aID := mustULID(t, a.ID)
	acct := seedAccount(t, a)

	if err := s.UserTx(ctx, aID, func(tx *UserTx) error {
		base, err := tx.CreateIdentity(ctx, IdentityRow{
			AccountID: acct, Kind: IdentityFixed,
			Address: "me@plain.example.com", Enabled: true,
		})
		if err != nil {
			return err
		}

		// Exactly what a hostile sender would write into Delivered-To: plausible
		// subaddresses of somebody else, and of another domain entirely.
		forged := []string{
			"boss+urgent@plain.example.com",
			"security+alert@plain.example.com",
			"me+shop@attacker.example.com",
			"me@attacker.example.com",
		}
		if _, ok, err := tx.MatchVerifiedAddress(ctx, forged); err != nil {
			return err
		} else if ok {
			t.Fatal("forged Delivered-To candidates produced a verified identity — " +
				"message content must never widen what the user may send as")
		}

		// THE CONTROL, so the assertion above is not passing because
		// MatchVerifiedAddress simply never matches anything: put the user's own
		// subaddress last, behind the same forged candidates, and it is found.
		si, ok, err := tx.MatchVerifiedAddress(ctx, append(append([]string{}, forged...), "me+shop@plain.example.com"))
		if err != nil {
			return err
		}
		if !ok {
			t.Fatal("the user's own subaddress was not matched — the negative above proves nothing")
		}
		if si.IdentityID != base {
			t.Errorf("matched %s, want the base identity %s", si.IdentityID, base)
		}
		if si.FromAddress != "me+shop@plain.example.com" {
			t.Errorf("FromAddress = %q, want the subaddress", si.FromAddress)
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
}

// A named identity for a subaddress is a CREATE, not a duplicate: the row that
// authorized it belongs to the base address. Without ViaSubaddress the API
// reports "an identity for this address already exists", which is both wrong and
// unactionable — there is no such row to edit.
func TestNamingASubaddressIsNotADuplicate(t *testing.T) {
	h := newHarness(t)
	ctx := context.Background()
	a, _ := seedTwoUsers(t, h.super)
	s := h.appStore()
	aID := mustULID(t, a.ID)
	acct := seedAccount(t, a)

	if err := s.UserTx(ctx, aID, func(tx *UserTx) error {
		if _, err := tx.CreateIdentity(ctx, IdentityRow{
			AccountID: acct, Kind: IdentityFixed,
			Address: "me@plain.example.com", Enabled: true,
		}); err != nil {
			return err
		}
		si, err := tx.ResolveSendIdentity(ctx, "me+shop@plain.example.com")
		if err != nil {
			return err
		}
		if !si.ViaSubaddress {
			t.Fatal("ViaSubaddress not set")
		}

		// Naming it grants nothing new — the base already allowed it — and after
		// it exists the exact match takes over.
		named, err := tx.CreateIdentity(ctx, IdentityRow{
			AccountID: si.AccountID, Kind: IdentityFixed,
			Address: "me+shop@plain.example.com", Enabled: true, DisplayName: "Shopping",
		})
		if err != nil {
			return err
		}
		after, err := tx.ResolveSendIdentity(ctx, "me+shop@plain.example.com")
		if err != nil {
			return err
		}
		if after.IdentityID != named {
			t.Errorf("after naming it, resolved to %s, want the new identity %s", after.IdentityID, named)
		}
		if after.ViaSubaddress {
			t.Error("an exact identity was still reported as ViaSubaddress")
		}
		if after.DisplayName != "Shopping" {
			t.Errorf("display name = %q, want the subaddress identity's own", after.DisplayName)
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
}

// Two users, the same local part, different domains: one user's verified base
// must never authorize the other's subaddress. RLS makes this near-impossible to
// get wrong, which is exactly why it is worth pinning — the fallback runs a
// SECOND query, and a query that forgot its user_id predicate would pass every
// other test in this file.
func TestSubaddressIsScopedToItsOwner(t *testing.T) {
	h := newHarness(t)
	ctx := context.Background()
	a, b := seedTwoUsers(t, h.super)
	s := h.appStore()
	acct := seedAccount(t, a)

	if err := s.UserTx(ctx, mustULID(t, a.ID), func(tx *UserTx) error {
		_, err := tx.CreateIdentity(ctx, IdentityRow{
			AccountID: acct, Kind: IdentityFixed,
			Address: "shared@plain.example.com", Enabled: true,
		})
		return err
	}); err != nil {
		t.Fatal(err)
	}
	if err := s.UserTx(ctx, mustULID(t, b.ID), func(tx *UserTx) error {
		if _, err := tx.ResolveSendIdentity(ctx, "shared+tag@plain.example.com"); !errors.Is(err, ErrFromNotVerified) {
			t.Error("user B sent as a subaddress of user A's verified address")
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	_ = ulid.ULID{}
}
