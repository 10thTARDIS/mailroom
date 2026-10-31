// SPDX-License-Identifier: AGPL-3.0-only

//go:build integration

package store

// Separating gives back what the merge borrowed (#834, ADR-041 §4): a merge
// records into's pre-merge state and the combined state it wrote; a separate
// that takes back every address the merge moved restores each of into's
// fields that still equals what the merge wrote. Against a real Postgres as
// mailroom_app; change rows and raw columns are read as the superuser.

import (
	"context"
	"errors"
	"reflect"
	"testing"
	"time"

	"github.com/oklog/ulid/v2"
)

// addrID is the address row id of addr (user A).
func (f *savedFixture) addrID(addr string) ulid.ULID {
	f.t.Helper()
	var id string
	if err := f.h.super.QueryRow(context.Background(),
		`SELECT id FROM app.contact_addresses WHERE user_id = $1 AND address = $2`, f.a.ID, addr).Scan(&id); err != nil {
		f.t.Fatalf("address id of %s: %v", addr, err)
	}
	return mustULID(f.t, id)
}

func (f *savedFixture) separate(id ulid.ULID, addrs ...string) SeparateResult {
	f.t.Helper()
	ids := make([]ulid.ULID, 0, len(addrs))
	for _, a := range addrs {
		ids = append(ids, f.addrID(a))
	}
	var res SeparateResult
	if err := f.s.UserTx(context.Background(), f.aID, func(tx *UserTx) error {
		var e error
		res, e = tx.SeparateContact(context.Background(), id, ids)
		return e
	}); err != nil {
		f.t.Fatalf("separate %v from %s: %v", addrs, id, err)
	}
	return res
}

func (f *savedFixture) mustMerge(into, from ulid.ULID) {
	f.t.Helper()
	if _, err := f.merge(into, from); err != nil {
		f.t.Fatalf("merge %s into %s: %v", from, into, err)
	}
}

func (f *savedFixture) mustSet(id ulid.ULID, p ContactMarkersPatch) {
	f.t.Helper()
	if _, err := f.set(id, p); err != nil {
		f.t.Fatalf("set %s: %v", id, err)
	}
}

// mergeRecords counts A's merge records with the given into_contact.
func (f *savedFixture) mergeRecords(into ulid.ULID) int {
	f.t.Helper()
	var n int
	if err := f.h.super.QueryRow(context.Background(),
		`SELECT count(*) FROM app.contact_merges WHERE user_id = $1 AND into_contact = $2`, f.a.ID, into.String()).Scan(&n); err != nil {
		f.t.Fatal(err)
	}
	return n
}

// changeRows counts every Contact change row for id (any op).
func (f *savedFixture) changeRows(id ulid.ULID) int {
	return f.rows(id, "created") + f.rows(id, "updated") + f.rows(id, "destroyed")
}

// pair seeds two contacts for A from received mail: into's mail carries
// intoName, from's fromName ("" = an address with no display name).
func (f *savedFixture) pair(intoName, fromName string) (into, from ulid.ULID, intoAddr, fromAddr string) {
	f.t.Helper()
	intoAddr, fromAddr = f.addr("into"), f.addr("from")
	f.received(intoAddr, intoName)
	f.received(fromAddr, fromName)
	return f.idOf(intoAddr), f.idOf(fromAddr), intoAddr, fromAddr
}

// sameRow compares two contact rows on every column read by row().
func sameRow(a, b contactRowState) bool {
	return reflect.DeepEqual(a.userName, b.userName) && a.displayName == b.displayName && a.notes == b.notes &&
		a.vip == b.vip && a.blocked == b.blocked && a.dest == b.dest &&
		eqTimePtr(a.blockedAt, b.blockedAt) && eqTimePtr(a.savedAt, b.savedAt) &&
		reflect.DeepEqual(a.mergedInto, b.mergedInto)
}

// THE OPERATOR'S CASE. A contact with the user's own name and no learned name
// (its mail carries none) borrows the learned name of the contact merged into
// it; separating gives it back: the learned name is "" again and the user's
// name is untouched. One Contact row each for the original and the recreated
// contact; the merge record is consumed.
func TestSeparateGivesBackTheBorrowedLearnedName(t *testing.T) {
	f := newSavedFixture(t)
	into, from, _, fromAddr := f.pair("", "service@payments.example")
	f.mustSet(into, ContactMarkersPatch{UserNameSet: true, UserName: strp("Provider Support")})
	if r := f.row(into); r.displayName != "" || r.userName == nil || *r.userName != "Provider Support" {
		t.Fatalf("setup: into = %+v", r)
	}
	f.mustMerge(into, from)
	if r := f.row(into); r.displayName != "service@payments.example" {
		t.Fatalf("setup: the merge did not borrow the learned name (%q), so this test proves nothing", r.displayName)
	}
	if f.mergeRecords(into) != 1 {
		t.Fatalf("the merge wrote %d records, want 1", f.mergeRecords(into))
	}
	intoRows := f.changeRows(into)

	res := f.separate(into, fromAddr)

	r := f.row(into)
	if r.displayName != "" {
		t.Errorf("into's learned name = %q after separating, want \"\" (what it was before the merge)", r.displayName)
	}
	if r.userName == nil || *r.userName != "Provider Support" {
		t.Errorf("into's userName = %v, want Provider Support (untouched)", r.userName)
	}
	if !res.Restored || len(res.KeptFields) != 0 {
		t.Errorf("result restored=%v kept=%v, want true and none", res.Restored, res.KeptFields)
	}
	n := res.Created[0]
	if nr := f.row(n); nr.displayName != "service@payments.example" {
		t.Errorf("the separated contact's learned name = %q", nr.displayName)
	}
	if got := f.changeRows(into) - intoRows; got != 1 || f.rows(into, "updated") < 1 {
		t.Errorf("into got %d new change rows, want exactly 1 (updated)", got)
	}
	if f.changeRows(n) != 1 || f.rows(n, "created") != 1 {
		t.Errorf("the separated contact has %d change rows, want exactly 1 (created)", f.changeRows(n))
	}
	if f.mergeRecords(into) != 0 {
		t.Errorf("the undone merge's record is still there")
	}
}

// Notes are un-joined: into keeps only its own; the separated contact has
// only its own (no duplicate on both).
func TestSeparateUnjoinsNotes(t *testing.T) {
	f := newSavedFixture(t)
	into, from, _, fromAddr := f.pair("Provider", "Payments")
	f.mustSet(into, ContactMarkersPatch{Notes: strp("provider notes")})
	f.mustSet(from, ContactMarkersPatch{Notes: strp("payments notes")})
	f.mustMerge(into, from)
	if r := f.row(into); r.notes != "provider notes"+mergeNotesSeparator+"payments notes" {
		t.Fatalf("setup: merged notes = %q", r.notes)
	}
	res := f.separate(into, fromAddr)
	if r := f.row(into); r.notes != "provider notes" {
		t.Errorf("into's notes = %q, want only its own", r.notes)
	}
	if r := f.row(res.Created[0]); r.notes != "payments notes" {
		t.Errorf("separated notes = %q, want only its own", r.notes)
	}
}

// VIP and saved go back: into was neither; the merged-in contact was both.
func TestSeparateRestoresVIPAndSaved(t *testing.T) {
	f := newSavedFixture(t)
	into, from, _, fromAddr := f.pair("Provider", "Payments")
	f.mustSet(from, ContactMarkersPatch{VIP: boolp(true)}) // VIP saves
	fromSaved := f.row(from).savedAt
	if r := f.row(into); r.vip || r.savedAt != nil {
		t.Fatalf("setup: into vip=%v saved=%v", r.vip, r.savedAt)
	}
	f.mustMerge(into, from)
	if r := f.row(into); !r.vip || r.savedAt == nil {
		t.Fatalf("setup: the merge did not OR VIP/saved in: %+v", r)
	}
	res := f.separate(into, fromAddr)
	if r := f.row(into); r.vip || r.savedAt != nil {
		t.Errorf("into after separate: vip=%v saved=%v, want neither", r.vip, r.savedAt)
	}
	if r := f.row(res.Created[0]); !r.vip || !eqTimePtr(r.savedAt, fromSaved) {
		t.Errorf("separated: vip=%v saved=%v, want VIP and saved at %v", r.vip, r.savedAt, fromSaved)
	}
}

// The block goes back with the address that had it: into returns to
// unblocked (its pre-merge state, destination preference included), and the
// separated contact keeps its own block, destination and date.
func TestSeparateBlockGoesBackWithItsAddress(t *testing.T) {
	f := newSavedFixture(t)
	ctx := context.Background()
	into, from, _, fromAddr := f.pair("Provider", "Payments")
	early := time.Now().UTC().Add(-48 * time.Hour).Truncate(time.Second)
	if _, err := f.h.super.Exec(ctx, `UPDATE app.contacts SET blocked = true, blocked_at = $2, block_destination = 'trash' WHERE id = $1`,
		from.String(), early); err != nil {
		t.Fatal(err)
	}
	f.mustMerge(into, from)
	if r := f.row(into); !r.blocked || r.dest != BlockToTrash {
		t.Fatalf("setup: the merge did not carry the block: %+v", r)
	}
	res := f.separate(into, fromAddr)
	if r := f.row(into); r.blocked || r.blockedAt != nil || r.dest != BlockToJunk {
		t.Errorf("into after separate: blocked=%v at=%v dest=%s, want unblocked, junk (its own)", r.blocked, r.blockedAt, r.dest)
	}
	if r := f.row(res.Created[0]); !r.blocked || r.dest != BlockToTrash || r.blockedAt == nil || !r.blockedAt.Equal(early) {
		t.Errorf("separated: blocked=%v dest=%s at=%v, want its own block (trash, %v)", r.blocked, r.dest, r.blockedAt, early)
	}
}

// A field edited after the merge is kept and reported; the rest are restored.
// A block the user set after the merge is kept on into AND goes with the
// separated address (separating never unblocks what the user blocked).
func TestSeparateKeepsAndReportsFieldsEditedAfterTheMerge(t *testing.T) {
	f := newSavedFixture(t)
	into, from, _, fromAddr := f.pair("", "Payments")
	f.mustSet(from, ContactMarkersPatch{Notes: strp("payments notes"), VIP: boolp(true)})
	f.mustMerge(into, from)
	f.mustSet(into, ContactMarkersPatch{Notes: strp("my own words"), Blocked: boolp(true)})

	res := f.separate(into, fromAddr)
	if !res.Restored || !reflect.DeepEqual(res.KeptFields, []string{"notes"}) {
		t.Errorf("result restored=%v kept=%v, want true and [notes]", res.Restored, res.KeptFields)
	}
	r := f.row(into)
	if r.notes != "my own words" {
		t.Errorf("the user's edited notes were not kept: %q", r.notes)
	}
	if r.vip || r.displayName != "" || r.savedAt != nil {
		t.Errorf("unedited borrowed fields were not restored: vip=%v name=%q saved=%v", r.vip, r.displayName, r.savedAt)
	}
	if !r.blocked {
		t.Error("the block the user set after the merge was dropped from into")
	}
	if nr := f.row(res.Created[0]); !nr.blocked || nr.notes != "payments notes" {
		t.Errorf("separated: blocked=%v notes=%q, want blocked (the user's block carries) and its own notes", nr.blocked, nr.notes)
	}
}

// A partial separate (some of the merged-in contact's addresses) restores
// nothing on into, and says so; the record stays. Separating the rest later
// completes the merge's undo.
func TestSeparatePartialRestoresNothing(t *testing.T) {
	f := newSavedFixture(t)
	ctx := context.Background()
	into, from, _, fromAddr := f.pair("", "Payments")
	// A second address on from (in production only a merge makes one; a
	// direct row keeps this test about the partial rule alone).
	fromAddr2 := f.addr("from2")
	if _, err := f.h.super.Exec(ctx, `INSERT INTO app.contact_addresses (id, user_id, contact_id, address) VALUES ($1, $2, $3, $4)`,
		NewULID().String(), f.a.ID, from.String(), fromAddr2); err != nil {
		t.Fatal(err)
	}
	f.mustSet(from, ContactMarkersPatch{Notes: strp("payments notes"), VIP: boolp(true)})
	f.mustMerge(into, from)
	merged := f.row(into)
	intoRows := f.changeRows(into)

	res := f.separate(into, fromAddr)
	if res.Restored || len(res.KeptFields) != 0 {
		t.Errorf("partial: restored=%v kept=%v, want false and none", res.Restored, res.KeptFields)
	}
	if after := f.row(into); !sameRow(after, merged) {
		t.Errorf("a partial separate changed into:\n before %+v\n after  %+v", merged, after)
	}
	if f.mergeRecords(into) != 1 {
		t.Error("a partial separate consumed the merge record")
	}
	if got := f.changeRows(into) - intoRows; got != 1 {
		t.Errorf("into got %d change rows, want 1 (its addresses changed)", got)
	}

	// The rest of from's addresses: now nothing of from is left, so the
	// merge is undone.
	res = f.separate(into, fromAddr2)
	if r := f.row(into); !res.Restored || r.displayName != "" || r.notes != "" || r.vip {
		t.Errorf("the completing separate: restored=%v into=%+v, want restored to the pre-merge state", res.Restored, r)
	}
}

// Stacked merges undone out of order: A and B both merged into C; separating
// A first rebuilds C from C's own state and B's (B's VIP and notes stay),
// then separating B returns C to exactly its own state.
func TestSeparateStackedMergesOutOfOrder(t *testing.T) {
	f := newSavedFixture(t)
	cAddr, aAddr, bAddr := f.addr("c"), f.addr("a"), f.addr("b")
	f.received(cAddr, "C")
	f.received(aAddr, "A")
	f.received(bAddr, "B")
	c, a, b := f.idOf(cAddr), f.idOf(aAddr), f.idOf(bAddr)
	f.mustSet(c, ContactMarkersPatch{Notes: strp("c")})
	f.mustSet(a, ContactMarkersPatch{Notes: strp("a"), VIP: boolp(true)})
	f.mustSet(b, ContactMarkersPatch{Notes: strp("b"), VIP: boolp(true)})
	own := f.row(c)
	f.mustMerge(c, a)
	f.mustMerge(c, b)
	if r := f.row(c); r.notes != "c"+mergeNotesSeparator+"a"+mergeNotesSeparator+"b" || !r.vip {
		t.Fatalf("setup: %+v", r)
	}

	res := f.separate(c, aAddr)
	r := f.row(c)
	if !res.Restored || len(res.KeptFields) != 0 {
		t.Errorf("first separate: restored=%v kept=%v", res.Restored, res.KeptFields)
	}
	if r.notes != "c"+mergeNotesSeparator+"b" || !r.vip || r.savedAt == nil {
		t.Errorf("after separating A: notes=%q vip=%v saved=%v, want c+b, VIP and saved (B's)", r.notes, r.vip, r.savedAt)
	}
	res = f.separate(c, bAddr)
	if r := f.row(c); !res.Restored || !sameRow(r, own) {
		t.Errorf("after separating B too: restored=%v\n got  %+v\n want %+v", res.Restored, r, own)
	}
}

// A MERGE OF A MERGE, undone at once: A into B, then B into C. Separating A's
// and B's addresses from C restores C, and B's new contact is B as it was
// before A was merged into it (its own empty learned name and notes, not
// VIP), from the first merge's own record.
func TestSeparateMergeOfMergeAtOnce(t *testing.T) {
	f := newSavedFixture(t)
	cAddr, bAddr, aAddr := f.addr("c"), f.addr("b"), f.addr("a")
	f.received(cAddr, "C Learned")
	f.received(bAddr, "")
	f.received(aAddr, "A Learned")
	c, b, a := f.idOf(cAddr), f.idOf(bAddr), f.idOf(aAddr)
	f.mustSet(c, ContactMarkersPatch{UserNameSet: true, UserName: strp("C Mine")})
	f.mustSet(b, ContactMarkersPatch{Notes: strp("b notes")})
	f.mustSet(a, ContactMarkersPatch{Notes: strp("a notes"), VIP: boolp(true)})
	own := f.row(c)
	f.mustMerge(b, a)
	f.mustMerge(c, b)
	if r := f.row(c); r.notes != "b notes"+mergeNotesSeparator+"a notes" || !r.vip {
		t.Fatalf("setup: C = %+v", r)
	}

	tombRows := f.changeRows(b)

	res := f.separate(c, bAddr, aAddr)
	if !res.Restored {
		t.Error("restored = false")
	}
	if r := f.row(c); !sameRow(r, own) {
		t.Errorf("C not restored:\n got  %+v\n want %+v", r, own)
	}
	bNew, aNew := f.idOf(bAddr), f.idOf(aAddr)
	if bNew == c || aNew == c || bNew == aNew {
		t.Fatalf("addresses not separated into two new contacts: b=%s a=%s c=%s", bNew, aNew, c)
	}
	if r := f.row(bNew); r.displayName != "" || r.notes != "b notes" || r.vip || r.savedAt != nil {
		t.Errorf("B's new contact = %+v, want B before A was merged into it (no learned name, b notes, not VIP, not saved)", r)
	}
	if r := f.row(aNew); r.displayName != "A Learned" || r.notes != "a notes" || !r.vip {
		t.Errorf("A's new contact = %+v", r)
	}
	if f.changeRows(b) != tombRows {
		t.Errorf("the tombstone B got %d change rows from its restore, want 0", f.changeRows(b)-tombRows)
	}
	// (seedUser's own fixture record is on another contact.)
	if left := f.mergeRecords(c) + f.mergeRecords(b) + f.mergeRecords(bNew); left != 0 {
		t.Errorf("%d merge records left, want 0 (both undone)", left)
	}
}

// A MERGE OF A MERGE, undone in reverse order over two separates: A into B,
// B into C. Separating B first is partial for the second merge (A's address
// came with it and stays), so C keeps everything, and B's new contact still
// carries A's borrowings (A is still here). Separating A then completes both
// merges: C is restored from the second merge's record, and B's new contact
// from the first's (the record followed B to its new contact), with exactly
// one Contact `updated` row for it.
func TestSeparateMergeOfMergeInTwoSteps(t *testing.T) {
	f := newSavedFixture(t)
	cAddr, bAddr, aAddr := f.addr("c"), f.addr("b"), f.addr("a")
	f.received(cAddr, "C Learned")
	f.received(bAddr, "")
	f.received(aAddr, "A Learned")
	c, b, a := f.idOf(cAddr), f.idOf(bAddr), f.idOf(aAddr)
	f.mustSet(b, ContactMarkersPatch{Notes: strp("b notes")})
	f.mustSet(a, ContactMarkersPatch{Notes: strp("a notes"), VIP: boolp(true)})
	own := f.row(c)
	f.mustMerge(b, a)
	f.mustMerge(c, b)
	merged := f.row(c)

	res := f.separate(c, bAddr)
	if res.Restored {
		t.Error("separating B alone (A's address came with B's merge and stays) said restored")
	}
	if r := f.row(c); !sameRow(r, merged) {
		t.Errorf("a partial separate changed C:\n got  %+v\n want %+v", r, merged)
	}
	bNew := res.Created[0]
	if r := f.row(bNew); r.displayName != "A Learned" || r.notes != "b notes"+mergeNotesSeparator+"a notes" {
		t.Fatalf("setup: B's new contact = %+v, want B's state with A's borrowings", r)
	}
	bRows := f.changeRows(bNew)

	res = f.separate(c, aAddr)
	if !res.Restored {
		t.Error("separating A (the rest of the second merge) said not restored")
	}
	if r := f.row(c); !sameRow(r, own) {
		t.Errorf("C not restored:\n got  %+v\n want %+v", r, own)
	}
	if r := f.row(bNew); r.displayName != "" || r.notes != "b notes" || r.vip || r.savedAt != nil {
		t.Errorf("B's new contact = %+v, want B before A was merged into it", r)
	}
	if got := f.changeRows(bNew) - bRows; got != 1 || f.rows(bNew, "updated") != 1 {
		t.Errorf("B's new contact got %d change rows, want exactly 1 updated", got)
	}
}

// RLS on app.contact_merges, behaviourally (the generic suite covers it too:
// it is in appTables). Positive control first: A sees its own record.
func TestContactMergeRecordsAreUserIsolated(t *testing.T) {
	f := newSavedFixture(t)
	ctx := context.Background()
	into, from, _, _ := f.pair("Provider", "Payments")
	f.mustMerge(into, from)

	count := func(as ulid.ULID) int {
		var n int
		if err := f.s.UserTx(ctx, as, func(tx *UserTx) error {
			return tx.tx.QueryRow(ctx, `SELECT count(*) FROM app.contact_merges WHERE into_contact = $1`, into.String()).Scan(&n)
		}); err != nil {
			t.Fatal(err)
		}
		return n
	}
	if count(f.aID) != 1 {
		t.Fatalf("positive control: A sees %d of its own merge records, want 1", count(f.aID))
	}
	if n := count(f.bID); n != 0 {
		t.Errorf("B sees %d of A's merge records", n)
	}
	var none int
	if err := f.h.app.QueryRow(ctx, `SELECT count(*) FROM app.contact_merges`).Scan(&none); err != nil || none != 0 {
		t.Errorf("no identity: %d rows (err %v), want 0", none, err)
	}
	if err := f.s.UserTx(ctx, f.bID, func(tx *UserTx) error {
		tag, err := tx.tx.Exec(ctx, `UPDATE app.contact_merges SET before_state = '{}' WHERE into_contact = $1`, into.String())
		if err != nil {
			return err
		}
		if tag.RowsAffected() != 0 {
			return errors.New("B updated A's merge record")
		}
		tag, err = tx.tx.Exec(ctx, `DELETE FROM app.contact_merges WHERE into_contact = $1`, into.String())
		if err != nil {
			return err
		}
		if tag.RowsAffected() != 0 {
			return errors.New("B deleted A's merge record")
		}
		return nil
	}); err != nil {
		t.Error(err)
	}
	// B writing a record carrying A's user id is refused (WITH CHECK).
	err := f.s.UserTx(ctx, f.bID, func(tx *UserTx) error {
		_, err := tx.tx.Exec(ctx, `INSERT INTO app.contact_merges (id, user_id, into_contact, from_contact, addresses, before_state, after_state)
			VALUES ($1, $2, $3, $4, '{}', '{}', '{}')`, NewULID().String(), f.a.ID, into.String(), from.String())
		return err
	})
	if err == nil {
		t.Error("B inserted a merge record for A")
	}
	if count(f.aID) != 1 {
		t.Error("A's record changed under B's attempts")
	}
}
