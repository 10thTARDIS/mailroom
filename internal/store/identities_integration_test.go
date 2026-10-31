// SPDX-License-Identifier: AGPL-3.0-only

//go:build integration

package store

// Integration suite for the Identity layer (mailroom#22, spec §5) — and above
// all for THE SEND-AS SECURITY BOUNDARY: ResolveSendIdentity is what stands
// between a client and sending mail as an address the user never verified, so
// its negative cases are tested at least as hard as its positive ones, against
// the real RLS-enforcing database.
//
// Runs against the throwaway Postgres provisioned by scripts/integration-test.sh;
// skips when the env is absent (see harness_test.go).

import (
	"context"
	"errors"
	"testing"

	"github.com/oklog/ulid/v2"
)

// seedAccount returns the id of the account seedUser created for a fixture user.
func seedAccount(t *testing.T, u fixtureUser) ulid.ULID {
	t.Helper()
	return mustULID(t, u.AccountID)
}

// TestResolveSendIdentityFixed: an exact verified address resolves; anything
// else at all does not.
func TestResolveSendIdentityFixed(t *testing.T) {
	h := newHarness(t)
	ctx := context.Background()
	a, _ := seedTwoUsers(t, h.super)
	s := h.appStore()
	aID := mustULID(t, a.ID)
	acct := seedAccount(t, a)

	err := s.UserTx(ctx, aID, func(tx *UserTx) error {
		id, err := tx.CreateIdentity(ctx, IdentityRow{
			AccountID: acct, Kind: IdentityFixed,
			Address: "me@fixed.example.com", Enabled: true,
		})
		if err != nil {
			return err
		}

		// Exact match, case-insensitively (addresses are citext / normalized).
		for _, probe := range []string{"me@fixed.example.com", "ME@Fixed.Example.COM", " me@fixed.example.com "} {
			si, err := tx.ResolveSendIdentity(ctx, probe)
			if err != nil {
				t.Fatalf("ResolveSendIdentity(%q): %v", probe, err)
			}
			if si.IdentityID != id {
				t.Errorf("ResolveSendIdentity(%q) resolved to the wrong identity", probe)
			}
			if si.AccountID != acct {
				t.Errorf("ResolveSendIdentity(%q) resolved to the wrong account", probe)
			}
			if si.FromAddress != "me@fixed.example.com" {
				t.Errorf("FromAddress = %q, want the normalized address", si.FromAddress)
			}
		}

		// A DIFFERENT local part at the same domain is NOT authorized by a fixed
		// identity: a fixed identity verifies one address, not a domain.
		if _, err := tx.ResolveSendIdentity(ctx, "someone-else@fixed.example.com"); !errors.Is(err, ErrFromNotVerified) {
			t.Fatalf("a fixed identity must not authorize another local part at its domain, got err=%v", err)
		}
		return nil
	})
	if err != nil {
		t.Fatalf("tx: %v", err)
	}
}

// TestResolveSendIdentityWildcard: the catchall model (spec §5) — any address at
// a verified domain is sendable, which is exactly what a wildcard identity means
// and what makes derived per-site aliases work without per-alias setup.
func TestResolveSendIdentityWildcard(t *testing.T) {
	h := newHarness(t)
	ctx := context.Background()
	a, _ := seedTwoUsers(t, h.super)
	s := h.appStore()
	aID := mustULID(t, a.ID)
	acct := seedAccount(t, a)

	err := s.UserTx(ctx, aID, func(tx *UserTx) error {
		wildID, err := tx.CreateIdentity(ctx, IdentityRow{
			AccountID: acct, Kind: IdentityWildcard,
			Domain: "catchall.example.com", Enabled: true,
		})
		if err != nil {
			return err
		}

		// Any local part at the domain — this is the "arbitrary per-site
		// addresses all land in one mailbox" case.
		for _, probe := range []string{"store@catchall.example.com", "some-random-site@catchall.example.com"} {
			si, err := tx.ResolveSendIdentity(ctx, probe)
			if err != nil {
				t.Fatalf("ResolveSendIdentity(%q): %v", probe, err)
			}
			if si.IdentityID != wildID {
				t.Errorf("%q should resolve to the wildcard identity", probe)
			}
			if si.Kind != IdentityWildcard {
				t.Errorf("Kind = %q, want the wildcard kind", si.Kind)
			}
			// The From we send with is the DERIVED alias, not the identity's own.
			if si.FromAddress != probe {
				t.Errorf("FromAddress = %q, want the derived alias %q", si.FromAddress, probe)
			}
		}

		// A different domain is not covered.
		if _, err := tx.ResolveSendIdentity(ctx, "me@other.example.net"); !errors.Is(err, ErrFromNotVerified) {
			t.Fatalf("a wildcard must not authorize another domain, got err=%v", err)
		}

		// A fixed identity outranks the wildcard for its exact address (so its
		// display name / signature win), while the wildcard still covers the rest.
		fixedID, err := tx.CreateIdentity(ctx, IdentityRow{
			AccountID: acct, Kind: IdentityFixed,
			Address: "me@catchall.example.com", DisplayName: "Me", Enabled: true,
		})
		if err != nil {
			return err
		}
		si, err := tx.ResolveSendIdentity(ctx, "me@catchall.example.com")
		if err != nil {
			return err
		}
		if si.IdentityID != fixedID {
			t.Error("an exact fixed identity must outrank the wildcard for its own address")
		}
		return nil
	})
	if err != nil {
		t.Fatalf("tx: %v", err)
	}
}

// THE NEGATIVE CASE THAT MATTERS (spec §5): an unverified From is refused, and a
// DISABLED identity authorizes nothing. This is the check a client cannot get
// around — every send path goes through it.
func TestResolveSendIdentityRejectsUnverified(t *testing.T) {
	h := newHarness(t)
	ctx := context.Background()
	a, _ := seedTwoUsers(t, h.super)
	s := h.appStore()
	aID := mustULID(t, a.ID)
	acct := seedAccount(t, a)

	err := s.UserTx(ctx, aID, func(tx *UserTx) error {
		disabledID, err := tx.CreateIdentity(ctx, IdentityRow{
			AccountID: acct, Kind: IdentityFixed,
			Address: "retired@example.com", Enabled: false,
		})
		if err != nil {
			return err
		}
		_ = disabledID

		unverified := []string{
			"ceo@some-bank.example",       // never verified at all
			"retired@example.com",         // verified once, now disabled
			"me@sub.fixed.example.com",    // a subdomain is a different domain
			"not-an-address",              // no domain
			"two@at@example.com",          // malformed
			"",                            // empty
			"victim@other-domain.example", // what a hostile Delivered-To proposes
		}
		for _, from := range unverified {
			if _, err := tx.ResolveSendIdentity(ctx, from); !errors.Is(err, ErrFromNotVerified) {
				t.Errorf("ResolveSendIdentity(%q) must be refused, got err=%v", from, err)
			}
		}
		return nil
	})
	if err != nil {
		t.Fatalf("tx: %v", err)
	}
}

// RLS: one user's identity must be invisible to another AND must not authorize
// their sends. This is the multi-tenant property spec §5 calls out by name —
// "one user's misconfiguration must never burn another domain's reputation from
// a shared instance".
func TestIdentityIsolation(t *testing.T) {
	h := newHarness(t)
	ctx := context.Background()
	a, b := seedTwoUsers(t, h.super)
	s := h.appStore()
	aID, bID := mustULID(t, a.ID), mustULID(t, b.ID)

	var aIdentity ulid.ULID
	if err := s.UserTx(ctx, aID, func(tx *UserTx) error {
		var err error
		aIdentity, err = tx.CreateIdentity(ctx, IdentityRow{
			AccountID: seedAccount(t, a), Kind: IdentityWildcard,
			Domain: "a-private.example.com", Enabled: true,
		})
		return err
	}); err != nil {
		t.Fatalf("create A's identity: %v", err)
	}

	if err := s.UserTx(ctx, bID, func(tx *UserTx) error {
		// B cannot read it...
		views, err := tx.GetIdentities(ctx, []ulid.ULID{aIdentity})
		if err != nil {
			return err
		}
		if len(views) != 0 {
			t.Fatalf("user B can see user A's identity: %+v", views)
		}
		// ...and, crucially, cannot SEND as it. A's verified domain grants B
		// nothing.
		if _, err := tx.ResolveSendIdentity(ctx, "anything@a-private.example.com"); !errors.Is(err, ErrFromNotVerified) {
			t.Fatalf("user B was authorized by user A's wildcard domain! err=%v", err)
		}
		// ...and cannot delete it (RLS hides the row, so it is simply not found).
		found, err := tx.DeleteIdentity(ctx, aIdentity)
		if err != nil {
			return err
		}
		if found {
			t.Fatal("user B deleted user A's identity")
		}
		return nil
	}); err != nil {
		t.Fatalf("tx as B: %v", err)
	}

	// A's identity is still there.
	if err := s.UserTx(ctx, aID, func(tx *UserTx) error {
		views, err := tx.GetIdentities(ctx, []ulid.ULID{aIdentity})
		if err != nil {
			return err
		}
		if len(views) != 1 {
			t.Fatal("user A's identity disappeared")
		}
		return nil
	}); err != nil {
		t.Fatalf("tx as A: %v", err)
	}
}

// The change_log invariant (CLAUDE.md): every Identity mutation appends exactly
// one row, in the same transaction — and a no-op patch appends none.
func TestIdentityChangeLogInvariant(t *testing.T) {
	h := newHarness(t)
	ctx := context.Background()
	a, _ := seedTwoUsers(t, h.super)
	s := h.appStore()
	aID := mustULID(t, a.ID)
	acct := seedAccount(t, a)

	var before, afterCreate, afterUpdate, afterNoop, afterDelete int64
	err := s.UserTx(ctx, aID, func(tx *UserTx) error {
		var err error
		if before, err = tx.ChangeState(ctx, ObjectIdentity); err != nil {
			return err
		}

		id, err := tx.CreateIdentity(ctx, IdentityRow{
			AccountID: acct, Kind: IdentityFixed,
			Address: "log@example.com", DisplayName: "Log", Enabled: true,
		})
		if err != nil {
			return err
		}
		if afterCreate, err = tx.ChangeState(ctx, ObjectIdentity); err != nil {
			return err
		}
		if afterCreate != before+1 {
			t.Errorf("create appended %d change rows, want exactly 1", afterCreate-before)
		}

		name := "Renamed"
		if _, err := tx.UpdateIdentity(ctx, id, IdentityPatch{DisplayName: &name}); err != nil {
			return err
		}
		if afterUpdate, err = tx.ChangeState(ctx, ObjectIdentity); err != nil {
			return err
		}
		if afterUpdate != afterCreate+1 {
			t.Errorf("update appended %d change rows, want exactly 1", afterUpdate-afterCreate)
		}

		// A patch that changes nothing is a TRUE no-op: no change_log row, so a
		// client polling /changes is not woken for nothing.
		if _, err := tx.UpdateIdentity(ctx, id, IdentityPatch{DisplayName: &name}); err != nil {
			return err
		}
		if afterNoop, err = tx.ChangeState(ctx, ObjectIdentity); err != nil {
			return err
		}
		if afterNoop != afterUpdate {
			t.Errorf("a no-op patch appended %d change rows, want 0", afterNoop-afterUpdate)
		}

		found, err := tx.DeleteIdentity(ctx, id)
		if err != nil {
			return err
		}
		if !found {
			t.Fatal("delete did not find the identity it just created")
		}
		if afterDelete, err = tx.ChangeState(ctx, ObjectIdentity); err != nil {
			return err
		}
		if afterDelete != afterNoop+1 {
			t.Errorf("delete appended %d change rows, want exactly 1", afterDelete-afterNoop)
		}
		return nil
	})
	if err != nil {
		t.Fatalf("tx: %v", err)
	}
}

// A duplicate identity is a RECOVERABLE error, not a poisoned transaction
// (SQLSTATE 25P02 — the class of bug the label layer already had). An
// Identity/set batch must be able to report one duplicate in notCreated and
// still commit the rest.
func TestIdentityDuplicateDoesNotPoisonTx(t *testing.T) {
	h := newHarness(t)
	ctx := context.Background()
	a, _ := seedTwoUsers(t, h.super)
	s := h.appStore()
	aID := mustULID(t, a.ID)
	acct := seedAccount(t, a)

	var survivor ulid.ULID
	err := s.UserTx(ctx, aID, func(tx *UserTx) error {
		if _, err := tx.CreateIdentity(ctx, IdentityRow{
			AccountID: acct, Kind: IdentityFixed, Address: "dup@example.com", Enabled: true,
		}); err != nil {
			return err
		}
		// The duplicate fails...
		if _, err := tx.CreateIdentity(ctx, IdentityRow{
			AccountID: acct, Kind: IdentityFixed, Address: "dup@example.com", Enabled: true,
		}); !errors.Is(err, ErrIdentityExists) {
			t.Fatalf("duplicate create: err = %v, want ErrIdentityExists", err)
		}
		// ...and the transaction is still usable, so this one commits.
		var err error
		survivor, err = tx.CreateIdentity(ctx, IdentityRow{
			AccountID: acct, Kind: IdentityFixed, Address: "survivor@example.com", Enabled: true,
		})
		return err
	})
	if err != nil {
		t.Fatalf("the unique violation poisoned the transaction: %v", err)
	}

	if err := s.UserTx(ctx, aID, func(tx *UserTx) error {
		views, err := tx.GetIdentities(ctx, []ulid.ULID{survivor})
		if err != nil {
			return err
		}
		if len(views) != 1 {
			t.Fatal("the create that followed a duplicate was lost — the tx was poisoned")
		}
		return nil
	}); err != nil {
		t.Fatalf("verify: %v", err)
	}
}

// MatchVerifiedAddress is the store half of the reply-From derivation: candidates
// in the caller's precedence order, first VERIFIED one wins. An attacker-supplied
// header that ranks first but is not verified is skipped, not honored.
func TestMatchVerifiedAddressSkipsUnverified(t *testing.T) {
	h := newHarness(t)
	ctx := context.Background()
	a, _ := seedTwoUsers(t, h.super)
	s := h.appStore()
	aID := mustULID(t, a.ID)
	acct := seedAccount(t, a)

	err := s.UserTx(ctx, aID, func(tx *UserTx) error {
		if _, err := tx.CreateIdentity(ctx, IdentityRow{
			AccountID: acct, Kind: IdentityWildcard,
			Domain: "mine.example.com", Enabled: true,
		}); err != nil {
			return err
		}

		si, ok, err := tx.MatchVerifiedAddress(ctx, []string{
			"attacker@evil.example",  // a hostile Delivered-To: ranked first...
			"store@mine.example.com", // ...but THIS is the one we may send as
		})
		if err != nil {
			return err
		}
		if !ok {
			t.Fatal("no candidate matched, but the second one is verified")
		}
		if si.FromAddress != "store@mine.example.com" {
			t.Fatalf("From = %q, want the first VERIFIED candidate, not the first candidate", si.FromAddress)
		}

		_, ok, err = tx.MatchVerifiedAddress(ctx, []string{"a@nope.example", "b@nope.example"})
		if err != nil {
			return err
		}
		if ok {
			t.Fatal("no candidate is verified: the match must fail closed")
		}
		return nil
	})
	if err != nil {
		t.Fatalf("tx: %v", err)
	}
}
