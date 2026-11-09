// SPDX-License-Identifier: AGPL-3.0-only

//go:build integration

package store

import (
	"context"
	"strings"
	"testing"

	"github.com/oklog/ulid/v2"
)

// The recipient and List-Id conditions (ADR-055), against the filter fixture:
//
//	x1  from Bob          to "Dana Lee <dana@..>"          list plans
//	x2  (account 2)       to dana, cc ed, bcc secret
//	x3  from Alice        to erin, cc dana                 list plans
//	x4  from Alice A      to "Bob Builder <bob@example.net>"
//	y1  y2                to zed (case differs in the sort key)  y1 list tie
//	seed                  columns NULL (not backfilled)
func TestRecipientConditions(t *testing.T) {
	f := newFilterFixture(t)
	cases := []struct {
		name string
		q    EmailQuery
		want string
	}{
		{"to address", EmailQuery{ToContains: "dana@example.org"}, "x1,x2"},
		{"to name, any case", EmailQuery{ToContains: "DANA LEE"}, "x1"},
		{"to domain substring", EmailQuery{ToContains: "@example.org"}, "x1,x2,x3,y1,y2"},
		{"to does not see cc or bcc", EmailQuery{ToContains: "secret@"}, ""},
		{"to-only address does not match from", EmailQuery{FromContains: "bob@example.net"}, "x1"},
		{"to is the recipient, not the sender", EmailQuery{ToContains: "bob@example.net"}, "x4"},
		{"a sender is not a recipient", EmailQuery{ToContains: "alice"}, ""},
		{"from still finds the sender", EmailQuery{FromContains: "alice"}, "x3,x4"},
		{"cc", EmailQuery{CcContains: "dana"}, "x3"},
		{"cc does not see to", EmailQuery{CcContains: "erin"}, ""},
		{"bcc", EmailQuery{BccContains: "SECRET"}, "x2"},
		{"bcc on received-style mail finds nothing", EmailQuery{BccContains: "dana"}, ""},
		{"a query cannot span two stored lines", EmailQuery{ToContains: "lee\ndana"}, ""},
		{"% is literal", EmailQuery{ToContains: "%"}, ""},
		{"_ is literal", EmailQuery{ToContains: "dana_lee"}, ""},
		{"and together", EmailQuery{ToContains: "dana", CcContains: "dana"}, ""},
		{"List-Id present", EmailQuery{ListIDHeader: &ListIDCondition{}}, "x1,x3,y1"},
		{"List-Id value", EmailQuery{ListIDHeader: &ListIDCondition{Contains: "plans"}}, "x1,x3"},
		{"List-Id value with brackets and case", EmailQuery{ListIDHeader: &ListIDCondition{Contains: "<PLANS.Example.net>"}}, "x1,x3"},
		{"List-Id no match", EmailQuery{ListIDHeader: &ListIDCondition{Contains: "nope"}}, ""},
	}
	for _, c := range cases {
		ids, total := f.query(t, c.q)
		if got := sortedNames(f, ids); got != c.want || total != len(ids) {
			t.Errorf("%s: %q (total %d), want %q", c.name, got, total, c.want)
		}
	}
	// A NULL column (the seed message, not backfilled) matches nothing, and is in
	// the complement: NOT is a true complement, not three-valued.
	notDana, _ := f.query(t, EmailQuery{Tree: ptr(op(FilterNOT, leaf(EmailQuery{ToContains: "dana"})))})
	if g := sortedNames(f, notDana); g != "seed,x3,x4,y1,y2" {
		t.Errorf("NOT to dana: %q", g)
	}
	notList, _ := f.query(t, EmailQuery{Tree: ptr(op(FilterNOT, leaf(EmailQuery{ListIDHeader: &ListIDCondition{}})))})
	if g := sortedNames(f, notList); g != "seed,x2,x4,y2" {
		t.Errorf("NOT header List-Id: %q", g)
	}
	orIDs, _ := f.query(t, EmailQuery{Tree: ptr(op(FilterOR, leaf(EmailQuery{CcContains: "dana"}), leaf(EmailQuery{BccContains: "secret"}),
		leaf(EmailQuery{ListIDHeader: &ListIDCondition{Contains: "tie"}})))})
	if g := sortedNames(f, orIDs); g != "x2,x3,y1" {
		t.Errorf("OR of three new conditions: %q", g)
	}
}

// Each new condition, flat and under NOT and OR, is asked as a key restricted to
// account 1 and unrestricted. The unrestricted answer must contain account 2's
// mail (so the test can see a leak), and the restricted one must never.
func TestRecipientConditionsKeyScopeAndRLS(t *testing.T) {
	f := newFilterFixture(t)
	restrict := []ulid.ULID{f.acct1}
	conds := map[string]EmailQuery{
		"to":     {ToContains: "dana"},
		"cc":     {CcContains: "ed@"},
		"bcc":    {BccContains: "secret"},
		"header": {ListIDHeader: &ListIDCondition{Contains: "plans"}},
	}
	// x2 is in account 2 and matches to, cc and bcc; y2 (account 2) matches none of
	// them, so it is the one a NOT must not hand to the key.
	forbidden := []string{"x2", "y2"}
	for name, c := range conds {
		c := c
		shapes := map[string]EmailQuery{
			"flat":        c,
			"NOT":         {Tree: ptr(op(FilterNOT, leaf(c)))},
			"NOT NOT":     {Tree: ptr(op(FilterNOT, op(FilterNOT, leaf(c))))},
			"OR inAcct2":  {Tree: ptr(op(FilterOR, leaf(c), leaf(EmailQuery{InAccount: &f.acct2})))},
			"OR mailbox2": {Tree: ptr(op(FilterOR, leaf(c), leaf(EmailQuery{InMailbox: &f.mbox2})))},
		}
		sawForbiddenUnrestricted := false
		for shape, q := range shapes {
			unr, _ := f.query(t, q)
			q.RestrictAccounts = restrict
			ids, total := f.query(t, q)
			for _, n := range f.names(ids) {
				if n == "x2" || n == "y2" || strings.HasPrefix(n, "bx") || strings.HasPrefix(n, "?") {
					t.Errorf("%s/%s: the restricted key was shown %s", name, shape, n)
				}
			}
			if total != len(ids) {
				t.Errorf("%s/%s: total %d differs from the ids counted (%d)", name, shape, total, len(ids))
			}
			for _, n := range f.names(unr) {
				if n == forbidden[0] || n == forbidden[1] {
					sawForbiddenUnrestricted = true
				}
				if strings.HasPrefix(n, "bx") {
					t.Errorf("%s/%s: user A's query returned user B's %s (RLS)", name, shape, n)
				}
			}
		}
		if !sawForbiddenUnrestricted {
			t.Errorf("%s: no shape returned account 2's mail unrestricted, so the test cannot show a leak", name)
		}
	}
	// RLS: user B holds mail whose recipient columns match A's queries; B sees only
	// its own, and A never sees B's (checked above through the bx prefix).
	var bIDs []ulid.ULID
	if err := f.h.appStore().UserTx(context.Background(), mustULID(t, f.b.ID), func(tx *UserTx) error {
		var err error
		bIDs, _, err = tx.QueryEmails(context.Background(), EmailQuery{ToContains: "dana"})
		return err
	}); err != nil {
		t.Fatal(err)
	}
	if len(bIDs) != 2 {
		t.Fatalf("user B's own to-filter returned %d messages, want its 2", len(bIDs))
	}
	for _, id := range bIDs {
		if !slicesContainsULID(f.bMsgs, id) {
			t.Errorf("user B's query returned a message that is not B's: %s", id)
		}
	}
}

func slicesContainsULID(s []ulid.ULID, id ulid.ULID) bool {
	for _, x := range s {
		if x == id {
			return true
		}
	}
	return false
}

// The octet collation of the `to` sort keeps case (the stored key is unfolded):
// "Zed" sorts before "zed" under i;octet and ties with it under the default.
func TestToSortOctetKeepsCase(t *testing.T) {
	f := newFilterFixture(t)
	y1, y2 := f.byName["y1"].id, f.byName["y2"].id
	pos := func(octet bool) (int, int) {
		ids, _ := f.query(t, EmailQuery{SortKeys: []EmailSortKey{{Kind: SortKeyTo, Ascending: true, Octet: octet}}})
		p1, p2 := -1, -1
		for i, id := range ids {
			switch id {
			case y1:
				p1 = i
			case y2:
				p2 = i
			}
		}
		return p1, p2
	}
	if p1, p2 := pos(true); p2 > p1 {
		t.Errorf("octet: y2 (Zed) at %d must precede y1 (zed) at %d", p2, p1)
	}
	// Folded, they tie on the key, so the id (descending by default) orders them:
	// the same relative order as with the date tie y1/y2 share.
	p1, p2 := pos(false)
	if (y1.String() > y2.String()) != (p1 < p2) {
		t.Errorf("folded tie not broken by id: y1 at %d, y2 at %d", p1, p2)
	}
}
