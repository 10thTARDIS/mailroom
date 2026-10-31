// SPDX-License-Identifier: AGPL-3.0-only

//go:build integration

package store

// Signatures and templates (#56, ADR-017).
//
// The interesting behaviour is not storage, it is the DEFAULT SLOT. "At most one
// default" is really "at most one per (user, kind, identity)", and the identity part
// includes the NULL binding as a slot of its own — which PostgreSQL's UNIQUE does not
// give you, because it treats NULLs as distinct. That is why there are two partial
// unique indexes and why the clearing statement compares with IS NOT DISTINCT FROM.
// Get it wrong in either direction and the user sees one of two bugs: a constraint
// error for the reasonable act of marking a second signature default, or several
// "defaults" at once, of which the one actually used is whichever sorted first.
//
// The other property worth pinning is that a TEMPLATE CANNOT BE BOUND to an address.
// It is checked twice — in Go for a clean message, and by the schema so no other
// write path can get round it.

import (
	"context"
	"errors"
	"testing"

	"github.com/oklog/ulid/v2"
)

// snippetByName finds one in a list, or fails: assertions here are about a named
// snippet's state, and an index would silently follow a reordering.
func snippetByName(t *testing.T, list []ComposeSnippet, name string) ComposeSnippet {
	t.Helper()
	for _, s := range list {
		if s.Name == name {
			return s
		}
	}
	t.Fatalf("no snippet named %q in %+v", name, list)
	return ComposeSnippet{}
}

func TestComposeSnippetDefaultIsPerKindAndIdentity(t *testing.T) {
	h := newHarness(t)
	s := h.appStore()
	ctx := context.Background()
	u := seedUser(t, h.super, "cs-"+NewULID().String())
	userID := mustULID(t, u.ID)
	bound := mustULID(t, u.IdentityID)

	// Four defaults that must coexist: a signature bound to the identity, an UNBOUND
	// signature, and a template (which has no identity at all). If the NULL slot were
	// not its own, the second and third would fight.
	if err := s.UserTx(ctx, userID, func(tx *UserTx) error {
		for _, want := range []ComposeSnippet{
			{Kind: SnippetSignature, IdentityID: &bound, Name: "bound-sig", BodyText: "b", IsDefault: true},
			{Kind: SnippetSignature, Name: "unbound-sig", BodyText: "u", IsDefault: true},
			{Kind: SnippetTemplate, Name: "tpl", BodyText: "t", IsDefault: true},
		} {
			if _, e := tx.CreateComposeSnippet(ctx, want); e != nil {
				return e
			}
		}
		return nil
	}); err != nil {
		t.Fatalf("create: %v", err)
	}

	if err := s.UserTx(ctx, userID, func(tx *UserTx) error {
		list, e := tx.ListComposeSnippets(ctx)
		if e != nil {
			return e
		}
		for _, name := range []string{"bound-sig", "unbound-sig", "tpl"} {
			if got := snippetByName(t, list, name); !got.IsDefault {
				t.Errorf("%s lost its default: the three slots are not independent", name)
			}
		}
		return nil
	}); err != nil {
		t.Fatalf("list: %v", err)
	}

	// A SECOND default in the SAME slot is a move, not a clash: the new one wins and
	// the old one is demoted, in the same transaction.
	if err := s.UserTx(ctx, userID, func(tx *UserTx) error {
		_, e := tx.CreateComposeSnippet(ctx, ComposeSnippet{
			Kind: SnippetSignature, IdentityID: &bound,
			Name: "bound-sig-2", BodyText: "b2", IsDefault: true,
		})
		return e
	}); err != nil {
		t.Fatalf("create second bound default: %v", err)
	}

	if err := s.UserTx(ctx, userID, func(tx *UserTx) error {
		list, e := tx.ListComposeSnippets(ctx)
		if e != nil {
			return e
		}
		if snippetByName(t, list, "bound-sig").IsDefault {
			t.Error("the old bound default was not demoted — two defaults in one slot")
		}
		if !snippetByName(t, list, "bound-sig-2").IsDefault {
			t.Error("the new bound default did not take")
		}
		// The other slots are untouched: demoting must be scoped, not global.
		if !snippetByName(t, list, "unbound-sig").IsDefault {
			t.Error("the unbound signature was demoted by a change to the bound slot")
		}
		if !snippetByName(t, list, "tpl").IsDefault {
			t.Error("the template was demoted by a change to a signature slot")
		}
		return nil
	}); err != nil {
		t.Fatalf("list after second: %v", err)
	}
}

// Re-saving the CURRENT default must leave it default. The clearing statement excludes
// the row being written for exactly this reason; without the exclusion, an edit that
// changed only the body would quietly turn the default off.
func TestComposeSnippetUpdateKeepsItsOwnDefault(t *testing.T) {
	h := newHarness(t)
	s := h.appStore()
	ctx := context.Background()
	u := seedUser(t, h.super, "cs-"+NewULID().String())
	userID := mustULID(t, u.ID)

	var id ulid.ULID
	if err := s.UserTx(ctx, userID, func(tx *UserTx) error {
		created, e := tx.CreateComposeSnippet(ctx, ComposeSnippet{
			Kind: SnippetSignature, Name: "only", BodyText: "first", IsDefault: true,
		})
		id = created
		return e
	}); err != nil {
		t.Fatalf("create: %v", err)
	}

	if err := s.UserTx(ctx, userID, func(tx *UserTx) error {
		found, e := tx.UpdateComposeSnippet(ctx, id, ComposeSnippet{
			Kind: SnippetSignature, Name: "only", BodyText: "second", IsDefault: true,
		})
		if e != nil {
			return e
		}
		if !found {
			t.Fatal("update reported the snippet missing")
		}
		list, e := tx.ListComposeSnippets(ctx)
		if e != nil {
			return e
		}
		got := snippetByName(t, list, "only")
		if !got.IsDefault {
			t.Error("editing the default cleared it")
		}
		if got.BodyText != "second" {
			t.Errorf("body = %q, want the update to have landed", got.BodyText)
		}
		return nil
	}); err != nil {
		t.Fatalf("update: %v", err)
	}
}

func TestComposeSnippetRefusesWhatItCannotMean(t *testing.T) {
	h := newHarness(t)
	s := h.appStore()
	ctx := context.Background()
	u := seedUser(t, h.super, "cs-"+NewULID().String())
	userID := mustULID(t, u.ID)
	bound := mustULID(t, u.IdentityID)

	cases := []struct {
		name string
		in   ComposeSnippet
	}{
		// A template bound to an address is a category error: a template is a message
		// you start from, not a way you sign, so the binding would have no meaning.
		{"template bound to an identity", ComposeSnippet{
			Kind: SnippetTemplate, IdentityID: &bound, Name: "t", BodyText: "x"}},
		{"no name", ComposeSnippet{Kind: SnippetSignature, Name: "  ", BodyText: "x"}},
		// A menu entry that does nothing when picked.
		{"no body at all", ComposeSnippet{Kind: SnippetSignature, Name: "empty"}},
		{"whitespace-only body", ComposeSnippet{
			Kind: SnippetSignature, Name: "blank", BodyText: "  ", BodyHTML: "\n"}},
		{"unknown kind", ComposeSnippet{Kind: "footer", Name: "f", BodyText: "x"}},
	}

	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			err := s.UserTx(ctx, userID, func(tx *UserTx) error {
				_, e := tx.CreateComposeSnippet(ctx, c.in)
				return e
			})
			if !errors.Is(err, ErrSnippetInvalid) {
				t.Fatalf("err = %v, want ErrSnippetInvalid", err)
			}
		})
	}

	// NEGATIVE CONTROL: the same shapes minus the one bad field are accepted, so the
	// refusals above are about the field and not about the harness.
	if err := s.UserTx(ctx, userID, func(tx *UserTx) error {
		if _, e := tx.CreateComposeSnippet(ctx, ComposeSnippet{
			Kind: SnippetTemplate, Name: "unbound template", BodyText: "x"}); e != nil {
			return e
		}
		_, e := tx.CreateComposeSnippet(ctx, ComposeSnippet{
			Kind: SnippetSignature, IdentityID: &bound, Name: "bound signature", BodyText: "x"})
		return e
	}); err != nil {
		t.Fatalf("the valid shapes were refused too: %v", err)
	}
}

// The load-bearing invariant: every mutation of a JMAP-visible object appends a
// change_log row in the same transaction, or other devices never learn about it.
func TestComposeSnippetAppendsChangeRows(t *testing.T) {
	h := newHarness(t)
	s := h.appStore()
	ctx := context.Background()
	u := seedUser(t, h.super, "cs-"+NewULID().String())
	userID := mustULID(t, u.ID)

	count := func(t *testing.T) int64 {
		t.Helper()
		var n int64
		if err := s.UserTx(ctx, userID, func(tx *UserTx) error {
			return tx.tx.QueryRow(ctx, `
				SELECT count(*) FROM app.change_log
				 WHERE user_id = $1 AND object_type = 'ComposeSnippet'`,
				userID.String()).Scan(&n)
		}); err != nil {
			t.Fatalf("count change rows: %v", err)
		}
		return n
	}

	// Baseline-relative, not absolute: the shared fixture seeds a snippet, and an
	// absolute count would couple this test to unrelated fixture changes.
	before := count(t)

	var id ulid.ULID
	if err := s.UserTx(ctx, userID, func(tx *UserTx) error {
		created, e := tx.CreateComposeSnippet(ctx, ComposeSnippet{
			Kind: SnippetSignature, Name: "sig", BodyText: "x"})
		id = created
		return e
	}); err != nil {
		t.Fatalf("create: %v", err)
	}
	if got := count(t) - before; got != 1 {
		t.Errorf("after create: %d new change rows, want 1", got)
	}

	if err := s.UserTx(ctx, userID, func(tx *UserTx) error {
		_, e := tx.UpdateComposeSnippet(ctx, id, ComposeSnippet{
			Kind: SnippetSignature, Name: "sig", BodyText: "y"})
		return e
	}); err != nil {
		t.Fatalf("update: %v", err)
	}
	if got := count(t) - before; got != 2 {
		t.Errorf("after update: %d new change rows, want 2", got)
	}

	if err := s.UserTx(ctx, userID, func(tx *UserTx) error {
		found, e := tx.DeleteComposeSnippet(ctx, id)
		if e == nil && !found {
			t.Error("delete reported the snippet missing")
		}
		return e
	}); err != nil {
		t.Fatalf("delete: %v", err)
	}
	if got := count(t) - before; got != 3 {
		t.Errorf("after delete: %d new change rows, want 3", got)
	}

	// A write that FAILS VALIDATION must append nothing — a change row for a mutation
	// that did not happen would make other devices refetch and find no difference.
	_ = s.UserTx(ctx, userID, func(tx *UserTx) error {
		_, e := tx.CreateComposeSnippet(ctx, ComposeSnippet{Kind: SnippetSignature, Name: "no body"})
		return e
	})
	if got := count(t) - before; got != 3 {
		t.Errorf("a refused create appended a change row: %d, want 3", got)
	}

	// Naming a snippet that does not exist is "not found", not an error, and appends
	// nothing either.
	if err := s.UserTx(ctx, userID, func(tx *UserTx) error {
		found, e := tx.DeleteComposeSnippet(ctx, NewULID())
		if e == nil && found {
			t.Error("deleting an unknown id reported success")
		}
		return e
	}); err != nil {
		t.Fatalf("delete unknown: %v", err)
	}
	if got := count(t) - before; got != 3 {
		t.Errorf("deleting an unknown id appended a change row: %d, want 3", got)
	}
}
