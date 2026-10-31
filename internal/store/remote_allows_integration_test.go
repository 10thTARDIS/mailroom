// SPDX-License-Identifier: AGPL-3.0-only

//go:build integration

package store

// Remembering "show remote content", per sender (#202).
//
// Two properties are worth pinning, and neither is about loading images:
//
//  1. THE SCHEMA CANNOT EXPRESS A BLANKET ALLOW. The whole justification for
//     persisting this decision at all is that it stays narrow — the original
//     non-persistent design was guarding against "this sender may track me
//     forever", and a wildcard row would be exactly that. If a value with a `*`, or
//     a bare domain suffix, can be stored, the guarantee is gone.
//  2. EVERY GRANT IS LISTABLE AND REVOCABLE, and both write paths append a
//     change_log row so other devices hear about it. A grant that could not be
//     listed would be invisible consent; one that could not be revoked would be
//     permanent consent.

import (
	"context"
	"errors"
	"testing"
)

func TestRemoteAllowRoundTripAndRevoke(t *testing.T) {
	h := newHarness(t)
	s := h.appStore()
	ctx := context.Background()
	u := seedUser(t, h.super, "ra-"+NewULID().String())
	userID := mustULID(t, u.ID)

	var domID ulid0
	if err := s.UserTx(ctx, userID, func(tx *UserTx) error {
		a, e := tx.AddRemoteAllow(ctx, RemoteAllowAddress, "News@Example.com")
		if e != nil {
			return e
		}
		// Stored lowercased, so the settings list shows what will actually match and
		// the user is not asked again for a different casing of one decision.
		if a.Value != "news@example.com" {
			t.Errorf("address value = %q, want it normalised to lowercase", a.Value)
		}
		d, e := tx.AddRemoteAllow(ctx, RemoteAllowDomain, "Example.COM")
		if e != nil {
			return e
		}
		domID = ulid0(d.ID.String())
		if d.Value != "example.com" {
			t.Errorf("domain value = %q, want it normalised", d.Value)
		}
		return nil
	}); err != nil {
		t.Fatalf("add: %v", err)
	}

	// Both come back, newest first — the order the settings list wants, because the
	// grant most likely to be reconsidered is the one just made.
	//
	// Asserted on the grants THIS test made, not on the table's total: the shared
	// fixture seeds one grant per user (so the RLS inventory has a row to probe), and
	// an absolute count here would be a test coupled to an unrelated fixture.
	if err := s.UserTx(ctx, userID, func(tx *UserTx) error {
		list, e := tx.ListRemoteAllows(ctx)
		if e != nil {
			return e
		}
		if !hasAllow(list, RemoteAllowAddress, "news@example.com") {
			t.Errorf("the address grant is missing: %+v", list)
		}
		if !hasAllow(list, RemoteAllowDomain, "example.com") {
			t.Errorf("the domain grant is missing: %+v", list)
		}
		// The domain grant was added last, so newest-first puts it at the head.
		if list[0].Kind != RemoteAllowDomain || list[0].Value != "example.com" {
			t.Errorf("newest-first broken: first is %+v", list[0])
		}
		return nil
	}); err != nil {
		t.Fatalf("list: %v", err)
	}

	// REVOCATION IS THE OTHER HALF OF THE FEATURE. Without it this table is a record
	// of consent nobody can withdraw.
	if err := s.UserTx(ctx, userID, func(tx *UserTx) error {
		found, e := tx.RemoveRemoteAllow(ctx, mustULID(t, string(domID)))
		if e != nil {
			return e
		}
		if !found {
			t.Error("revoking an existing grant reported not-found")
		}
		// Revoking twice is not an error: the caller wanted it gone, and it is.
		found, e = tx.RemoveRemoteAllow(ctx, mustULID(t, string(domID)))
		if e != nil {
			return e
		}
		if found {
			t.Error("revoking an already-revoked grant reported found")
		}
		list, e := tx.ListRemoteAllows(ctx)
		if e != nil {
			return e
		}
		if hasAllow(list, RemoteAllowDomain, "example.com") {
			t.Errorf("the revoked domain grant is still listed: %+v", list)
		}
		if !hasAllow(list, RemoteAllowAddress, "news@example.com") {
			t.Errorf("revoking the domain grant also removed the address one: %+v", list)
		}
		return nil
	}); err != nil {
		t.Fatalf("revoke: %v", err)
	}
}

// hasAllow reports whether a grant of exactly this kind and value is in the list.
func hasAllow(list []RemoteAllow, kind RemoteAllowKind, value string) bool {
	for _, a := range list {
		if a.Kind == kind && a.Value == value {
			return true
		}
	}
	return false
}

// A JMAP-visible object owes a change_log row per mutation (the load-bearing
// invariant), or a grant made on one device never reaches another — which for this
// object means a setting the user has to make again in every browser.
func TestRemoteAllowAppendsChangeRows(t *testing.T) {
	h := newHarness(t)
	s := h.appStore()
	ctx := context.Background()
	u := seedUser(t, h.super, "ra-chg-"+NewULID().String())
	userID := mustULID(t, u.ID)

	if err := s.UserTx(ctx, userID, func(tx *UserTx) error {
		before, e := tx.ChangeState(ctx, ObjectRemoteAllow)
		if e != nil {
			return e
		}
		a, e := tx.AddRemoteAllow(ctx, RemoteAllowAddress, "news@example.com")
		if e != nil {
			return e
		}
		afterAdd, e := tx.ChangeState(ctx, ObjectRemoteAllow)
		if e != nil {
			return e
		}
		if afterAdd <= before {
			t.Errorf("adding a grant did not advance the change state (%d -> %d)", before, afterAdd)
		}

		// RE-GRANTING THE SAME THING IS NOT A CHANGE. It is the same decision, and a
		// /changes stream that reports a change nobody made costs every client a
		// needless refetch.
		again, e := tx.AddRemoteAllow(ctx, RemoteAllowAddress, "NEWS@example.com")
		if e != nil {
			return e
		}
		if again.ID != a.ID {
			t.Errorf("re-granting produced a second row: %s then %s", a.ID, again.ID)
		}
		afterRepeat, e := tx.ChangeState(ctx, ObjectRemoteAllow)
		if e != nil {
			return e
		}
		if afterRepeat != afterAdd {
			t.Errorf("re-granting advanced the change state (%d -> %d)", afterAdd, afterRepeat)
		}

		if _, e = tx.RemoveRemoteAllow(ctx, a.ID); e != nil {
			return e
		}
		afterDel, e := tx.ChangeState(ctx, ObjectRemoteAllow)
		if e != nil {
			return e
		}
		if afterDel <= afterAdd {
			t.Errorf("revoking did not advance the change state (%d -> %d)", afterAdd, afterDel)
		}
		return nil
	}); err != nil {
		t.Fatalf("change rows: %v", err)
	}
}

// THE REFUSALS. Everything here would be a wider grant than the UI offers, or a
// value that silently never matches — and a row that cannot match is worse than an
// error, because the user believes they made a decision that does nothing.
func TestRemoteAllowRefusesAnythingWiderThanOneSenderOrDomain(t *testing.T) {
	h := newHarness(t)
	s := h.appStore()
	ctx := context.Background()
	u := seedUser(t, h.super, "ra-bad-"+NewULID().String())
	userID := mustULID(t, u.ID)

	bad := []struct {
		kind  RemoteAllowKind
		value string
		why   string
	}{
		{RemoteAllowDomain, "*", "a bare wildcard is the blanket allow this table exists to refuse"},
		{RemoteAllowDomain, "*.example.com", "a wildcard subdomain grant is wider than the UI offers"},
		{RemoteAllowDomain, ".example.com", "a leading dot reads as a suffix match"},
		{RemoteAllowDomain, "example.com.", "a trailing dot is not the domain the projection produces"},
		{RemoteAllowDomain, "localhost", "no dot: cannot be a sender domain"},
		{RemoteAllowDomain, "news@example.com", "an address is not a domain"},
		{RemoteAllowAddress, "example.com", "a domain is not an address"},
		{RemoteAllowAddress, "two@at@signs.test", "not a single address"},
		{RemoteAllowAddress, "@example.com", "no local part"},
		{RemoteAllowAddress, "news@", "no domain"},
		{RemoteAllowAddress, "news @example.com", "whitespace inside an address"},
		{RemoteAllowAddress, "", "empty"},
		{RemoteAllowKind("all"), "everything", "an unknown kind must not be storable at all"},
	}

	if err := s.UserTx(ctx, userID, func(tx *UserTx) error {
		// The baseline: the shared fixture seeds one grant per user, so "nothing was
		// stored" is a comparison against what was already there, not against zero.
		before, e := tx.ListRemoteAllows(ctx)
		if e != nil {
			return e
		}

		for _, tc := range bad {
			if _, e := tx.AddRemoteAllow(ctx, tc.kind, tc.value); e == nil {
				t.Errorf("AddRemoteAllow(%q, %q) was ACCEPTED — %s", tc.kind, tc.value, tc.why)
			} else if !errors.Is(e, ErrRemoteAllowInvalid) {
				t.Errorf("AddRemoteAllow(%q, %q) failed with %v, want ErrRemoteAllowInvalid",
					tc.kind, tc.value, e)
			}
		}

		after, e := tx.ListRemoteAllows(ctx)
		if e != nil {
			return e
		}
		if len(after) != len(before) {
			t.Errorf("a refused grant was stored anyway: %d grants before, %d after: %+v",
				len(before), len(after), after)
		}
		return nil
	}); err != nil {
		t.Fatalf("refusals: %v", err)
	}
}

// ulid0 is a tiny alias so the test can carry an id string between closures without
// importing ulid for one conversion.
type ulid0 string
