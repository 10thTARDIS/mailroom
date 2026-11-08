// SPDX-License-Identifier: AGPL-3.0-only

//go:build integration

package store

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/oklog/ulid/v2"
)

// queryErr runs q as user A and returns the error.
func (f *filterFixture) queryErr(t *testing.T, q EmailQuery) error {
	t.Helper()
	return f.h.appStore().UserTx(context.Background(), mustULID(t, f.a.ID), func(tx *UserTx) error {
		_, _, err := tx.QueryEmails(context.Background(), q)
		return err
	})
}

// addTrash makes a trash-role mailbox in the given account.
func (f *filterFixture) addTrash(t *testing.T, account string) string {
	t.Helper()
	id := NewULID().String()
	mustExec(t, f.h, `INSERT INTO app.mailboxes (id, user_id, account_id, imap_name, display_name, special_use)
	      VALUES ($1, $2, $3, 'Trash', 'Trash', '\Trash')`, id, f.a.ID, account)
	return id
}

// moveByOtherClient moves a message's live provenance into a mailbox, as another
// IMAP client would: moved_mailbox_id and trashed_at stay NULL.
func (f *filterFixture) moveByOtherClient(t *testing.T, name, mailbox string) {
	t.Helper()
	mustExec(t, f.h, `UPDATE app.message_provenance SET mailbox_id = $1 WHERE message_id = $2`,
		mailbox, f.byName[name].id.String())
}

// A leaf costs one node per condition, not one per object (M2).
func TestFilterLeafConditionsCountTowardTheNodeBound(t *testing.T) {
	f := newFilterFixture(t)
	yes := true
	three := EmailQuery{HasAttachment: &yes, MinSize: ptr(int64(0)), MaxSize: ptr(int64(1 << 40))}
	if three.ConditionCount() != 3 {
		t.Fatalf("ConditionCount = %d, want 3", three.ConditionCount())
	}
	wide := func(n int) EmailQuery {
		root := EmailFilterNode{Operator: FilterOR}
		for i := 0; i < n; i++ {
			root.Conditions = append(root.Conditions, leaf(three))
		}
		return EmailQuery{Tree: &root}
	}
	// 21 leaves x 3 conditions + the OR = 64: exactly at the bound.
	if err := f.queryErr(t, wide(21)); err != nil {
		t.Errorf("a tree at the bound: %v", err)
	}
	// 22 leaves is 22 objects (well under 64) but 67 conditions.
	if err := f.queryErr(t, wide(22)); !errors.Is(err, ErrFilterTooComplex) {
		t.Errorf("a tree of 22 three-condition leaves: %v, want ErrFilterTooComplex", err)
	}
}

func TestFilterThreadKeywordConditionsAreCapped(t *testing.T) {
	f := newFilterFixture(t)
	all3 := leaf(EmailQuery{SomeInThreadHaveKeyword: "kwx", NoneInThreadHaveKeyword: "kwy", AllInThreadHaveKeyword: "kwz"})
	some := leaf(EmailQuery{SomeInThreadHaveKeyword: "kwx"})
	if err := f.queryErr(t, EmailQuery{Tree: ptr(op(FilterOR, all3, some))}); err != nil {
		t.Errorf("%d thread-keyword conditions: %v", MaxThreadKeywordConds, err)
	}
	if err := f.queryErr(t, EmailQuery{Tree: ptr(op(FilterOR, all3, some, some))}); !errors.Is(err, ErrFilterTooComplex) {
		t.Errorf("%d thread-keyword conditions: %v, want ErrFilterTooComplex", MaxThreadKeywordConds+1, err)
	}
}

func TestFilterMailboxListsAreCapped(t *testing.T) {
	f := newFilterFixture(t)
	list := func(n int) EmailQuery {
		l := make([]ulid.ULID, n)
		for i := range l {
			l[i] = NewULID()
		}
		return EmailQuery{InMailboxOtherThan: l}
	}
	if err := f.queryErr(t, list(MaxMailboxIDs)); err != nil {
		t.Errorf("a list at the cap: %v", err)
	}
	if err := f.queryErr(t, list(MaxMailboxIDs+1)); !errors.Is(err, ErrFilterTooComplex) {
		t.Errorf("a list past the cap: %v, want ErrFilterTooComplex", err)
	}
	// The same cap inside a tree leaf.
	if err := f.queryErr(t, EmailQuery{Tree: ptr(leaf(list(MaxMailboxIDs + 1)))}); !errors.Is(err, ErrFilterTooComplex) {
		t.Errorf("a leaf list past the cap: %v, want ErrFilterTooComplex", err)
	}
}

// L1: only a Trash inMailbox reached through AND alone makes a Trash view.
func TestFilterTrashMailboxOnlyCountsUnderAnd(t *testing.T) {
	f := newFilterFixture(t)
	trash := mustULID(t, f.addTrash(t, f.acct1.String()))
	f.moveByOtherClient(t, "x3", trash.String())
	inTrash := leaf(EmailQuery{InMailbox: &trash})
	inInbox := leaf(EmailQuery{InMailbox: &f.mbox1})

	for name, tree := range map[string]EmailFilterNode{
		"OR":      op(FilterOR, inTrash, inInbox),
		"NOT":     op(FilterNOT, inTrash),
		"AND/OR":  op(FilterAND, inInbox, op(FilterOR, inTrash, inInbox)),
		"OR/AND":  op(FilterOR, inInbox, op(FilterAND, inTrash)),
		"NOT/NOT": op(FilterNOT, op(FilterNOT, inTrash)),
	} {
		tree := tree
		if err := f.queryErr(t, EmailQuery{Tree: &tree}); !errors.Is(err, ErrUnsupportedTrashFilter) {
			t.Errorf("%s with a Trash leaf: %v, want ErrUnsupportedTrashFilter", name, err)
		}
	}
	// AND with a Trash leaf is a Trash view: the other-client-trashed message shows.
	ids, total := f.query(t, EmailQuery{Tree: ptr(op(FilterAND, inTrash))})
	if g := sortedNames(f, ids); g != "x3" || total != 1 {
		t.Errorf("AND Trash view: %q (total %d), want x3", g, total)
	}
	// And a tree that never names Trash is not a Trash view: x3 stays hidden.
	ids, _ = f.query(t, EmailQuery{Tree: ptr(op(FilterOR, inInbox, leaf(EmailQuery{InMailbox: &f.mbox2})))})
	if slicesContainsName(f.names(ids), "x3") {
		t.Errorf("a non-Trash tree listed a trashed message: %v", f.names(ids))
	}
}

func slicesContainsName(names []string, n string) bool {
	for _, x := range names {
		if x == n {
			return true
		}
	}
	return false
}

// L3: a thread's other messages count only if not effectively in Trash.
func TestFilterThreadMembershipExcludesFolderTrashed(t *testing.T) {
	f := newFilterFixture(t)
	some := EmailQuery{SomeInThreadHaveKeyword: "kwx"}
	none := EmailQuery{NoneInThreadHaveKeyword: "kwx"}
	all := EmailQuery{AllInThreadHaveKeyword: "kwx"}
	has := func(q EmailQuery, name string) bool {
		ids, _ := f.query(t, q)
		return slicesContainsName(f.names(ids), name)
	}
	// Before: x2 (kwx) is in x1's thread; y2 (no kwx) is in y1's.
	if !has(some, "x1") || has(none, "x1") || has(all, "y1") {
		t.Fatal("fixture: thread T has kwx via x2, and thread V is not all-kwx")
	}
	trash := f.addTrash(t, f.acct2.String())
	f.moveByOtherClient(t, "x2", trash)
	f.moveByOtherClient(t, "y2", trash)
	if has(some, "x1") {
		t.Error("some: a thread message moved to Trash by another client still counts")
	}
	if !has(none, "x1") {
		t.Error("none: a thread message moved to Trash by another client still counts")
	}
	if !has(all, "y1") {
		t.Error("all: a thread message moved to Trash by another client still counts")
	}
}

// M1: a subject of 100,000 characters sorts, and its base subject is computed
// from the first 1000 characters.
func TestSortKeysLongSubjectIsBounded(t *testing.T) {
	f := newFilterFixture(t)
	long := &fxMsg{name: "long", id: NewULID(), subject: "Re: " + strings.Repeat("B", 100000) + " tail", fromAddr: "long@example.com", size: 1, age: 0}
	f.byName["long"] = long
	f.insert(t, f.a.ID, long, f.mbox1, f.acct1)
	var base, baseRaw, want string
	row := f.h.super.QueryRow(context.Background(),
		`SELECT app.base_subject(subject), app.base_subject_raw(subject),
		        app.base_subject(left(subject, 1000)) FROM app.messages WHERE id = $1`, long.id.String())
	if err := row.Scan(&base, &baseRaw, &want); err != nil {
		t.Fatal(err)
	}
	// "Re: " is stripped, then 996 of the first 1000 characters remain.
	if base != strings.Repeat("b", 996) || baseRaw != strings.Repeat("B", 996) || base != want {
		t.Errorf("base subject: len %d / raw len %d, want 996 characters of the first 1000", len(base), len(baseRaw))
	}
	for _, k := range []EmailSortKey{{Kind: SortKeySubject, Ascending: true}, {Kind: SortKeySubject, Octet: true}} {
		ids, total := f.query(t, EmailQuery{SortKeys: []EmailSortKey{k}})
		if total != 8 || !slicesContainsName(f.names(ids), "long") {
			t.Errorf("sort %+v: total %d, ids %v", k, total, f.names(ids))
		}
	}
}
