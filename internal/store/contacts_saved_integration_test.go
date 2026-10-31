// SPDX-License-Identifier: AGPL-3.0-only

//go:build integration

package store

// Saved contacts, the user's name and notes, merge and separate (#816,
// ADR-041), against a real Postgres as mailroom_app. Every assertion that a
// change row was or was not written reads app.change_log as the superuser.

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/oklog/ulid/v2"
)

type savedFixture struct {
	t    *testing.T
	h    *harness
	s    *Store
	a, b fixtureUser
	aID  ulid.ULID
	bID  ulid.ULID
	me   string
	run  string
}

func newSavedFixture(t *testing.T) *savedFixture {
	t.Helper()
	h := newHarness(t)
	a, b := seedTwoUsers(t, h.super)
	return &savedFixture{t: t, h: h, s: h.appStore(), a: a, b: b,
		aID: mustULID(t, a.ID), bID: mustULID(t, b.ID),
		me:  NormalizeAddress(a.Tag + "@example.com"),
		run: strings.ToLower(NewULID().String())}
}

func (f *savedFixture) addr(name string) string {
	return NormalizeAddress(name + "-" + f.run + "@corp.test")
}

// sent records a message the user (A) sent to rcpts — the ingest hook.
func (f *savedFixture) sent(rcpts ...string) {
	f.t.Helper()
	var ps []ContactParticipant
	for _, r := range rcpts {
		ps = append(ps, ContactParticipant{Address: r})
	}
	mustUpsert(f.t, f.s, f.aID, ContactSourceMessage{FromAddress: f.me, Recipients: ps})
}

// received records a message From addr (with a display name) to the user.
func (f *savedFixture) received(addr, name string) {
	f.t.Helper()
	mustUpsert(f.t, f.s, f.aID, ContactSourceMessage{FromName: name, FromAddress: addr,
		Recipients: []ContactParticipant{{Address: f.me}}, AddressedAddress: f.me})
}

type contactRowState struct {
	id          ulid.ULID
	savedAt     *time.Time
	userName    *string
	displayName string
	notes       string
	vip         bool
	blocked     bool
	dest        string
	blockedAt   *time.Time
	mergedInto  *string
}

// row reads a contact's columns as the superuser, by id.
func (f *savedFixture) row(id ulid.ULID) contactRowState {
	f.t.Helper()
	var r contactRowState
	r.id = id
	if err := f.h.super.QueryRow(context.Background(), `
		SELECT saved_at, user_display_name, display_name, notes, vip, blocked, block_destination, blocked_at, merged_into
		  FROM app.contacts WHERE id = $1`, id.String()).Scan(
		&r.savedAt, &r.userName, &r.displayName, &r.notes, &r.vip, &r.blocked, &r.dest, &r.blockedAt, &r.mergedInto); err != nil {
		f.t.Fatalf("read contact %s: %v", id, err)
	}
	return r
}

// idOf is the contact owning addr for user A, as the superuser.
func (f *savedFixture) idOf(addr string) ulid.ULID {
	f.t.Helper()
	cid, _, _, found := contactAddrRow(f.t, f.h, f.a.ID, addr)
	if !found {
		f.t.Fatalf("no contact for %s", addr)
	}
	return mustULID(f.t, cid)
}

func (f *savedFixture) rows(id ulid.ULID, op string) int {
	f.t.Helper()
	var n int
	if err := f.h.super.QueryRow(context.Background(), `
		SELECT count(*) FROM app.change_log
		 WHERE user_id = $1 AND object_type = 'Contact' AND object_id = $2 AND op = $3`,
		f.a.ID, id.String(), op).Scan(&n); err != nil {
		f.t.Fatal(err)
	}
	return n
}

func (f *savedFixture) set(id ulid.ULID, p ContactMarkersPatch) (bool, error) {
	var changed bool
	err := f.s.UserTx(context.Background(), f.aID, func(tx *UserTx) error {
		var e error
		changed, e = tx.SetContactMarkers(context.Background(), id, p)
		return e
	})
	return changed, err
}

func (f *savedFixture) get(id ulid.ULID) (Contact, bool) {
	f.t.Helper()
	var cs []Contact
	if err := f.s.UserTx(context.Background(), f.aID, func(tx *UserTx) error {
		var e error
		cs, e = tx.GetContacts(context.Background(), []ulid.ULID{id})
		return e
	}); err != nil {
		f.t.Fatal(err)
	}
	if len(cs) != 1 {
		return Contact{}, false
	}
	return cs[0], true
}

func strp(s string) *string { return &s }

// Ingest SAVES a contact when the user sends to it (a new contact, in its
// `created` row; an existing one, with one `updated` row), and only once.
// A received message does not save. Ingest NEVER writes user_display_name:
// a later message with another name leaves the user's name alone.
func TestIngestSavesOnSendAndNeverWritesUserName(t *testing.T) {
	f := newSavedFixture(t)
	bob, carol := f.addr("bob"), f.addr("carol")

	// New contact as a recipient of the user's mail: saved from the start.
	f.sent(bob)
	bobID := f.idOf(bob)
	if f.row(bobID).savedAt == nil {
		t.Fatal("a contact created by the user's own sent mail is not saved")
	}
	if c, u := f.rows(bobID, "created"), f.rows(bobID, "updated"); c != 1 || u != 0 {
		t.Errorf("bob change rows created/updated = %d/%d, want 1/0 (saved rides the created row)", c, u)
	}

	// Received mail does not save.
	f.received(carol, "Carol Example")
	carolID := f.idOf(carol)
	if f.row(carolID).savedAt != nil {
		t.Fatal("a received message saved its sender")
	}
	// Writing to an existing contact saves it: one updated row.
	f.sent(carol)
	if f.row(carolID).savedAt == nil {
		t.Fatal("writing to an existing contact did not save it")
	}
	if n := f.rows(carolID, "updated"); n != 1 {
		t.Errorf("carol updated rows after the save = %d, want 1", n)
	}
	firstSaved := *f.row(carolID).savedAt
	// Writing again is a re-sighting: no row, the saved time kept.
	f.sent(carol)
	if n := f.rows(carolID, "updated"); n != 1 {
		t.Errorf("a second send appended a change row: %d", n)
	}
	if got := f.row(carolID).savedAt; got == nil || !got.Equal(firstSaved) {
		t.Errorf("saved_at moved on a re-send: %v -> %v", firstSaved, got)
	}

	// The user's own name survives the next message's name.
	if _, err := f.set(carolID, ContactMarkersPatch{UserNameSet: true, UserName: strp("Carol (work)")}); err != nil {
		t.Fatal(err)
	}
	f.received(carol, "Carol Someone Else")
	r := f.row(carolID)
	if r.userName == nil || *r.userName != "Carol (work)" {
		t.Errorf("ingest touched user_display_name: %v", r.userName)
	}
	if r.displayName != "Carol Example" {
		t.Errorf("display_name = %q, want the first learned name kept", r.displayName)
	}
	// A contact with no learned name gets one filled by ingest, still into
	// display_name and never into the user's column.
	dan := f.addr("dan")
	f.sent(dan) // no name
	danID := f.idOf(dan)
	f.received(dan, "Dan Learned")
	if r := f.row(danID); r.userName != nil || r.displayName != "Dan Learned" {
		t.Errorf("name fill wrote user_display_name=%v display_name=%q", r.userName, r.displayName)
	}
	c, _ := f.get(carolID)
	if c.DisplayedName() != "Carol (work)" {
		t.Errorf("displayed name = %q, want the user's", c.DisplayedName())
	}
}

// VIP saves (one row); block does not save.
func TestContactVIPSavesBlockDoesNot(t *testing.T) {
	f := newSavedFixture(t)
	v, bl := f.addr("vip"), f.addr("blocked")
	f.received(v, "")
	f.received(bl, "")
	vID, bID := f.idOf(v), f.idOf(bl)

	if _, err := f.set(vID, ContactMarkersPatch{VIP: boolp(true)}); err != nil {
		t.Fatal(err)
	}
	if r := f.row(vID); !r.vip || r.savedAt == nil {
		t.Errorf("VIP: vip=%v saved=%v, want both", r.vip, r.savedAt)
	}
	if n := f.rows(vID, "updated"); n != 1 {
		t.Errorf("VIP+save wrote %d rows, want 1", n)
	}
	if _, err := f.set(bID, ContactMarkersPatch{Blocked: boolp(true)}); err != nil {
		t.Fatal(err)
	}
	if r := f.row(bID); !r.blocked || r.savedAt != nil {
		t.Errorf("block: blocked=%v saved=%v, want blocked and NOT saved", r.blocked, r.savedAt)
	}
	// isVip:true with isSaved:false in one patch is refused.
	if _, err := f.set(bID, ContactMarkersPatch{VIP: boolp(true), Saved: boolp(false)}); !errors.Is(err, ErrContactSaveConflict) {
		t.Errorf("VIP + unsave: err=%v, want ErrContactSaveConflict", err)
	}
}

// Un-saving deletes nothing: keys (and their history), VIP and block stay.
func TestContactUnsaveKeepsKeysVIPAndBlock(t *testing.T) {
	f := newSavedFixture(t)
	ctx := context.Background()
	x := f.addr("x")
	f.sent(x)
	id := f.idOf(x)
	if err := f.s.UserTx(ctx, f.aID, func(tx *UserTx) error {
		_, e := tx.UpsertContactKey(ctx, ContactKeyRow{ContactID: id, Fingerprint: "FPR-UNSAVE-" + f.run,
			PublicKey: []byte{0x01, 0x02}, Source: ContactKeyAutocrypt})
		return e
	}); err != nil {
		t.Fatal(err)
	}
	if _, err := f.set(id, ContactMarkersPatch{VIP: boolp(true), Blocked: boolp(true), Notes: strp("keep me")}); err != nil {
		t.Fatal(err)
	}
	before := f.rows(id, "updated")
	if changed, err := f.set(id, ContactMarkersPatch{Saved: boolp(false)}); err != nil || !changed {
		t.Fatalf("unsave: changed=%v err=%v", changed, err)
	}
	if n := f.rows(id, "updated"); n != before+1 {
		t.Errorf("unsave wrote %d rows, want 1", n-before)
	}
	r := f.row(id)
	if r.savedAt != nil || !r.vip || !r.blocked || r.blockedAt == nil || r.notes != "keep me" {
		t.Errorf("after unsave: %+v", r)
	}
	var keys, events int
	if err := f.h.super.QueryRow(ctx, `SELECT count(*) FROM app.contact_keys WHERE contact_id = $1 AND removed_at IS NULL`, id.String()).Scan(&keys); err != nil {
		t.Fatal(err)
	}
	if err := f.h.super.QueryRow(ctx, `SELECT count(*) FROM app.contact_key_events e JOIN app.contact_keys k ON k.id = e.contact_key_id WHERE k.contact_id = $1`, id.String()).Scan(&events); err != nil {
		t.Fatal(err)
	}
	if keys != 1 || events != 1 {
		t.Errorf("after unsave: %d keys, %d key events, want 1 and 1", keys, events)
	}
	// Unsave again: a no-op, no row.
	if changed, err := f.set(id, ContactMarkersPatch{Saved: boolp(false)}); err != nil || changed {
		t.Errorf("second unsave: changed=%v err=%v, want a no-op", changed, err)
	}
	if n := f.rows(id, "updated"); n != before+1 {
		t.Errorf("a no-op unsave wrote a row")
	}
}

// One change row per real change; none for a no-op, field by field.
func TestContactEditsOneRowPerChangeNoneForNoop(t *testing.T) {
	f := newSavedFixture(t)
	x := f.addr("edits")
	f.received(x, "")
	id := f.idOf(x)
	steps := []struct {
		name    string
		p       ContactMarkersPatch
		changed bool
	}{
		{"save", ContactMarkersPatch{Saved: boolp(true)}, true},
		{"save again", ContactMarkersPatch{Saved: boolp(true)}, false},
		{"name", ContactMarkersPatch{UserNameSet: true, UserName: strp("Ex")}, true},
		{"same name", ContactMarkersPatch{UserNameSet: true, UserName: strp("  Ex ")}, false},
		{"notes", ContactMarkersPatch{Notes: strp("hello")}, true},
		{"same notes", ContactMarkersPatch{Notes: strp("hello")}, false},
		{"clear name", ContactMarkersPatch{UserNameSet: true, UserName: nil}, true},
		{"clear name again", ContactMarkersPatch{UserNameSet: true, UserName: strp("   ")}, false},
		{"VIP on a saved contact", ContactMarkersPatch{VIP: boolp(true)}, true},
		{"empty patch", ContactMarkersPatch{}, false},
	}
	want := 0
	for _, s := range steps {
		changed, err := f.set(id, s.p)
		if err != nil {
			t.Fatalf("%s: %v", s.name, err)
		}
		if changed != s.changed {
			t.Errorf("%s: changed=%v, want %v", s.name, changed, s.changed)
		}
		if s.changed {
			want++
		}
		if n := f.rows(id, "updated"); n != want {
			t.Errorf("%s: %d updated rows, want %d", s.name, n, want)
		}
	}
	if r := f.row(id); r.userName != nil || r.notes != "hello" || r.savedAt == nil || !r.vip {
		t.Errorf("final state %+v", r)
	}
}

// userName and notes are cleaned and capped in the store, and the schema's
// CHECKs hold the same caps against a writer that skipped the store.
func TestContactUserNameAndNotesSanitised(t *testing.T) {
	f := newSavedFixture(t)
	x := f.addr("clean")
	f.received(x, "")
	id := f.idOf(x)
	if _, err := f.set(id, ContactMarkersPatch{UserNameSet: true, UserName: strp("  Bob\u202e\x07 Smith\n ")}); err != nil {
		t.Fatal(err)
	}
	if r := f.row(id); r.userName == nil || *r.userName != "Bob Smith" {
		t.Errorf("cleaned name = %v, want %q", r.userName, "Bob Smith")
	}
	if _, err := f.set(id, ContactMarkersPatch{Notes: strp("line one\r\nline\ttwo\x00\u2066 ")}); err != nil {
		t.Fatal(err)
	}
	if r := f.row(id); r.notes != "line one\nline\ttwo" {
		t.Errorf("cleaned notes = %q", r.notes)
	}
	if _, err := f.set(id, ContactMarkersPatch{UserNameSet: true, UserName: strp(strings.Repeat("é", 201))}); !errors.Is(err, ErrContactUserNameTooLong) {
		t.Errorf("201-character name: err=%v", err)
	}
	if _, err := f.set(id, ContactMarkersPatch{UserNameSet: true, UserName: strp(strings.Repeat("é", 200))}); err != nil {
		t.Errorf("200-character name refused: %v", err)
	}
	if _, err := f.set(id, ContactMarkersPatch{Notes: strp(strings.Repeat("n", 4001))}); !errors.Is(err, ErrContactNotesTooLong) {
		t.Errorf("4,001-character notes: err=%v", err)
	}
	// The CHECKs: a direct write past either cap fails.
	ctx := context.Background()
	if _, err := f.h.super.Exec(ctx, `UPDATE app.contacts SET notes = repeat('n', 4001) WHERE id = $1`, id.String()); err == nil {
		t.Error("the notes CHECK let 4,001 characters in")
	}
	if _, err := f.h.super.Exec(ctx, `UPDATE app.contacts SET user_display_name = repeat('n', 201) WHERE id = $1`, id.String()); err == nil {
		t.Error("the name CHECK let 201 characters in")
	}
	if _, err := f.h.super.Exec(ctx, `UPDATE app.contacts SET notes = repeat('n', 4000) WHERE id = $1`, id.String()); err != nil {
		t.Errorf("control: the notes CHECK refused 4,000 characters: %v", err)
	}
}

// Contact/query: the isSaved filter, the two sorts both ways, paging with a
// total, and the default (autocomplete) order and coverage unchanged.
func TestContactQuerySavedFilterSortsAndPaging(t *testing.T) {
	f := newSavedFixture(t)
	ctx := context.Background()
	// Three saved contacts with names in mixed case, one unsaved, seen counts
	// that order the default query differently from both sorts.
	al, bo, ca, un := f.addr("al"), f.addr("bo"), f.addr("ca"), f.addr("un")
	f.received(al, "alice")
	f.received(bo, "Bob")
	f.received(bo, "Bob")
	f.received(ca, "carol")
	f.received(ca, "carol")
	f.received(ca, "carol")
	f.received(un, "Unsaved")
	for _, a := range []string{al, bo, ca} {
		if _, err := f.set(f.idOf(a), ContactMarkersPatch{Saved: boolp(true)}); err != nil {
			t.Fatal(err)
		}
	}
	// Last mail: bob newest, then alice, then carol.
	setLast := func(a string, daysAgo int) {
		if _, err := f.h.super.Exec(ctx, `UPDATE app.contact_addresses SET last_seen_at = now() - make_interval(days => $2) WHERE user_id = $1 AND address = $3`,
			f.a.ID, daysAgo, a); err != nil {
			t.Fatal(err)
		}
	}
	setLast(bo, 1)
	setLast(al, 5)
	setLast(ca, 30)
	alID, boID, caID, unID := f.idOf(al), f.idOf(bo), f.idOf(ca), f.idOf(un)

	q := func(fl ContactQueryFilter, p ContactPage) ([]ulid.ULID, int, int) {
		t.Helper()
		var (
			ids        []ulid.ULID
			total, pos int
		)
		if err := f.s.UserTx(ctx, f.aID, func(tx *UserTx) error {
			var e error
			ids, total, pos, e = tx.QueryContactsPage(ctx, fl, p)
			return e
		}); err != nil {
			t.Fatal(err)
		}
		return ids, total, pos
	}
	eq := func(name string, got []ulid.ULID, want ...ulid.ULID) {
		t.Helper()
		if len(got) != len(want) {
			t.Errorf("%s: got %v, want %v", name, got, want)
			return
		}
		for i := range want {
			if got[i] != want[i] {
				t.Errorf("%s: got %v, want %v", name, got, want)
				return
			}
		}
	}
	saved := ContactQueryFilter{Text: f.run, IsSaved: boolp(true)}
	ids, total, _ := q(saved, ContactPage{Sort: ContactSortName, Ascending: true, Limit: 10, LimitSet: true})
	eq("saved by name asc", ids, alID, boID, caID)
	if total != 3 {
		t.Errorf("saved total = %d, want 3", total)
	}
	ids, _, _ = q(saved, ContactPage{Sort: ContactSortName, Ascending: false, Limit: 10, LimitSet: true})
	eq("saved by name desc", ids, caID, boID, alID)
	ids, _, _ = q(saved, ContactPage{Sort: ContactSortLastMailAt, Ascending: false, Limit: 10, LimitSet: true})
	eq("saved by lastMailAt desc", ids, boID, alID, caID)
	ids, _, _ = q(saved, ContactPage{Sort: ContactSortLastMailAt, Ascending: true, Limit: 10, LimitSet: true})
	eq("saved by lastMailAt asc", ids, caID, alID, boID)
	// Paging: position 1, limit 1; total still 3.
	ids, total, pos := q(saved, ContactPage{Sort: ContactSortName, Ascending: true, Position: 1, Limit: 1, LimitSet: true})
	eq("page 2 of 3", ids, boID)
	if total != 3 || pos != 1 {
		t.Errorf("page: total=%d pos=%d", total, pos)
	}
	ids, _, pos = q(saved, ContactPage{Sort: ContactSortName, Ascending: true, Position: -1, Limit: 5, LimitSet: true})
	eq("position -1", ids, caID)
	if pos != 2 {
		t.Errorf("position -1 resolved to %d, want 2", pos)
	}
	// isSaved:false finds only the unsaved one.
	ids, _, _ = q(ContactQueryFilter{Text: f.run, IsSaved: boolp(false)}, ContactPage{Limit: 10, LimitSet: true})
	eq("unsaved", ids, unID)
	// The default query (no sort, no isSaved): ALL four, most-sighted first,
	// exactly as QueryContacts always answered.
	ids, total, err := func() ([]ulid.ULID, int, error) {
		var ids []ulid.ULID
		var total int
		err := f.s.UserTx(ctx, f.aID, func(tx *UserTx) error {
			var e error
			ids, total, e = tx.QueryContacts(ctx, ContactQueryFilter{Text: f.run}, 0, false)
			return e
		})
		return ids, total, err
	}()
	if err != nil {
		t.Fatal(err)
	}
	if total != 4 || len(ids) != 4 || ids[0] != caID || ids[1] != boID {
		t.Errorf("default query = %v (total %d), want carol, bob first of all four", ids, total)
	}
	// text matches the user's own name too.
	if _, err := f.set(unID, ContactMarkersPatch{UserNameSet: true, UserName: strp("Zebra " + f.run)}); err != nil {
		t.Fatal(err)
	}
	ids, _, _ = q(ContactQueryFilter{Text: "zebra " + f.run}, ContactPage{Limit: 10, LimitSet: true})
	eq("text on userName", ids, unID)
	// Case-insensitivity that the collation cannot supply on its own: two
	// names equal but for case tie under lower() and fall back to the id, so
	// the LOWER id carries the upper-case name. Without lower() the database
	// collation (en_US in the postgres image) puts "zed" before "Zed" and the
	// order flips; under a C collation "Z" sorts before every lower-case name.
	z1, z2 := f.addr("case1"), f.addr("case2")
	f.received(z1, "")
	f.received(z2, "")
	lo, hi := f.idOf(z1), f.idOf(z2)
	if hi.Compare(lo) < 0 {
		lo, hi = hi, lo
	}
	for id, name := range map[ulid.ULID]string{lo: "Zed-" + f.run, hi: "zed-" + f.run} {
		if _, err := f.set(id, ContactMarkersPatch{Saved: boolp(true), UserNameSet: true, UserName: strp(name)}); err != nil {
			t.Fatal(err)
		}
	}
	ids, _, _ = q(ContactQueryFilter{Text: "zed-" + f.run, IsSaved: boolp(true)}, ContactPage{Sort: ContactSortName, Ascending: true, Limit: 10, LimitSet: true})
	eq("names equal but for case tie on the id", ids, lo, hi)
	// An unknown sort is refused.
	if err := f.s.UserTx(ctx, f.aID, func(tx *UserTx) error {
		_, _, _, e := tx.QueryContactsPage(ctx, ContactQueryFilter{}, ContactPage{Sort: "seenCount"})
		return e
	}); !errors.Is(err, ErrContactSortInvalid) {
		t.Errorf("unknown sort: err=%v", err)
	}
}

// onlyCoRecipient: an address with no mail From it and never written to.
func TestContactQueryOnlyCoRecipient(t *testing.T) {
	f := newSavedFixture(t)
	ctx := context.Background()
	co, wrote, from := f.addr("co"), f.addr("wrote"), f.addr("from")
	// A co-recipient: seen on a received message's Cc only.
	mustUpsert(t, f.s, f.aID, ContactSourceMessage{FromAddress: f.addr("listsender"),
		Recipients: []ContactParticipant{{Address: f.me}, {Address: co}}, AddressedAddress: f.me})
	f.sent(wrote)
	f.received(from, "")
	// "from" needs a real message From it for the filter to see mail.
	if _, err := f.h.super.Exec(ctx, `UPDATE app.messages SET from_address = $2 WHERE id = $1`, f.a.MessageID, from); err != nil {
		t.Fatal(err)
	}
	var ids []ulid.ULID
	if err := f.s.UserTx(ctx, f.aID, func(tx *UserTx) error {
		var e error
		ids, _, _, e = tx.QueryContactsPage(ctx, ContactQueryFilter{Text: f.run, OnlyCoRecipient: boolp(true)}, ContactPage{Limit: 10, LimitSet: true})
		return e
	}); err != nil {
		t.Fatal(err)
	}
	// listsender has no message From it in the DB either (ingest hook only),
	// so it is a "no mail found" contact too.
	want := map[ulid.ULID]bool{f.idOf(co): true, f.idOf(f.addr("listsender")): true}
	if len(ids) != len(want) {
		t.Fatalf("onlyCoRecipient = %v, want co and listsender", ids)
	}
	for _, id := range ids {
		if !want[id] {
			t.Errorf("onlyCoRecipient returned %s (wrote or from)", id)
		}
	}
}

// mergePair seeds two contacts for A with distinct state, keys and pins.
func (f *savedFixture) mergePair() (into, from ulid.ULID, intoAddr, fromAddr string) {
	f.t.Helper()
	ctx := context.Background()
	intoAddr, fromAddr = f.addr("into"), f.addr("from")
	f.received(intoAddr, "Into Learned")
	f.received(fromAddr, "From Learned")
	into, from = f.idOf(intoAddr), f.idOf(fromAddr)
	key := func(cid ulid.ULID, fpr string) {
		if err := f.s.UserTx(ctx, f.aID, func(tx *UserTx) error {
			_, e := tx.UpsertContactKey(ctx, ContactKeyRow{ContactID: cid, Fingerprint: fpr,
				PublicKey: []byte{0x01}, Source: ContactKeyAutocrypt})
			return e
		}); err != nil {
			f.t.Fatal(err)
		}
	}
	key(into, "FPR-INTO-"+f.run)
	key(from, "FPR-FROM-"+f.run)
	key(from, "FPR-SHARED-"+f.run)
	key(into, "FPR-SHARED-"+f.run)
	if _, err := f.h.super.Exec(ctx, `INSERT INTO app.contact_key_pins (user_id, address, fingerprint, source) VALUES ($1,$2,$3,'autocrypt')`,
		f.a.ID, fromAddr, "FPR-FROM-"+f.run); err != nil {
		f.t.Fatal(err)
	}
	return into, from, intoAddr, fromAddr
}

func (f *savedFixture) merge(into, from ulid.ULID) (ContactMergeResult, error) {
	var res ContactMergeResult
	err := f.s.UserTx(context.Background(), f.aID, func(tx *UserTx) error {
		var e error
		res, e = tx.MergeContacts(context.Background(), into, from)
		return e
	})
	return res, err
}

// The merge combine rules: block wins with the EARLIER blocked_at and the
// stricter destination; saved/VIP either; notes joined; userName from into
// unless it has none. Every address and key moves; a shared key is combined
// with its history; pins are untouched. Change rows: into updated, from
// destroyed; from is gone from reads.
func TestContactMergeCombineRules(t *testing.T) {
	f := newSavedFixture(t)
	ctx := context.Background()
	into, from, intoAddr, fromAddr := f.mergePair()
	early := time.Now().UTC().Add(-48 * time.Hour).Truncate(time.Second)
	late := time.Now().UTC().Add(-1 * time.Hour).Truncate(time.Second)
	// into: blocked to junk LATE, notes, no user name, not saved, not VIP.
	// from: blocked to trash EARLY, notes, a user name, saved, VIP.
	if _, err := f.h.super.Exec(ctx, `UPDATE app.contacts SET blocked = true, blocked_at = $2, block_destination = 'junk', notes = 'into notes' WHERE id = $1`,
		into.String(), late); err != nil {
		t.Fatal(err)
	}
	if _, err := f.h.super.Exec(ctx, `UPDATE app.contacts SET blocked = true, blocked_at = $2, block_destination = 'trash', notes = 'from notes',
		user_display_name = 'From Mine', saved_at = now(), vip = true WHERE id = $1`, from.String(), early); err != nil {
		t.Fatal(err)
	}
	intoUpd, fromUpd := f.rows(into, "updated"), f.rows(from, "updated")

	res, err := f.merge(into, from)
	if err != nil {
		t.Fatal(err)
	}
	if res.Addresses != 1 || res.Keys != 1 || res.KeysCombined != 1 {
		t.Errorf("moved %+v, want 1 address, 1 key moved, 1 combined", res)
	}
	r := f.row(into)
	if !r.blocked || r.dest != BlockToTrash || r.blockedAt == nil || !r.blockedAt.Equal(early) {
		t.Errorf("block: blocked=%v dest=%s at=%v, want blocked, trash, the earlier %v", r.blocked, r.dest, r.blockedAt, early)
	}
	if r.savedAt == nil || !r.vip {
		t.Errorf("saved=%v vip=%v, want both from 'from'", r.savedAt, r.vip)
	}
	if r.notes != "into notes"+mergeNotesSeparator+"from notes" {
		t.Errorf("notes = %q", r.notes)
	}
	if r.userName == nil || *r.userName != "From Mine" || r.displayName != "Into Learned" {
		t.Errorf("names: user=%v learned=%q", r.userName, r.displayName)
	}
	// Addresses and keys all on into; from is a tombstone, hidden.
	if f.idOf(fromAddr) != into || f.idOf(intoAddr) != into {
		t.Error("an address did not move to into")
	}
	var keys, sharedEvents, pins int
	if err := f.h.super.QueryRow(ctx, `SELECT count(*) FROM app.contact_keys WHERE contact_id = $1`, into.String()).Scan(&keys); err != nil {
		t.Fatal(err)
	}
	if err := f.h.super.QueryRow(ctx, `SELECT count(*) FROM app.contact_key_events e JOIN app.contact_keys k ON k.id = e.contact_key_id
		WHERE k.contact_id = $1 AND k.fingerprint = $2`, into.String(), "FPR-SHARED-"+f.run).Scan(&sharedEvents); err != nil {
		t.Fatal(err)
	}
	if err := f.h.super.QueryRow(ctx, `SELECT count(*) FROM app.contact_key_pins WHERE user_id = $1 AND address = $2 AND fingerprint = $3`,
		f.a.ID, fromAddr, "FPR-FROM-"+f.run).Scan(&pins); err != nil {
		t.Fatal(err)
	}
	if keys != 3 || sharedEvents != 2 || pins != 1 {
		t.Errorf("keys on into=%d (want 3), shared key events=%d (want 2: both histories), pin kept=%d", keys, sharedEvents, pins)
	}
	if tomb := f.row(from); tomb.mergedInto == nil || *tomb.mergedInto != into.String() {
		t.Errorf("from is not a tombstone of into: %v", tomb.mergedInto)
	}
	if _, found := f.get(from); found {
		t.Error("the merged-away contact is still readable")
	}
	if f.rows(into, "updated") != intoUpd+1 || f.rows(from, "destroyed") != 1 || f.rows(from, "updated") != fromUpd {
		t.Errorf("change rows: into updated +%d, from destroyed %d, from updated +%d; want +1, 1, +0",
			f.rows(into, "updated")-intoUpd, f.rows(from, "destroyed"), f.rows(from, "updated")-fromUpd)
	}
	// A tombstone cannot be edited or merged again.
	if _, err := f.set(from, ContactMarkersPatch{VIP: boolp(true)}); !errors.Is(err, ErrContactNotFound) {
		t.Errorf("editing a tombstone: err=%v", err)
	}
	if _, err := f.merge(into, from); !errors.Is(err, ErrContactNotFound) {
		t.Errorf("merging a tombstone: err=%v", err)
	}
	if _, err := f.merge(into, into); !errors.Is(err, ErrMergeSelf) {
		t.Errorf("merging with itself: err=%v", err)
	}
}

// The block rule is symmetric: a block only on `from` still wins, and an
// unblocked pair keeps into's destination preference and stays unblocked.
func TestContactMergeBlockFromOnlyWins(t *testing.T) {
	f := newSavedFixture(t)
	ctx := context.Background()
	into, from, _, _ := f.mergePair()
	if _, err := f.h.super.Exec(ctx, `UPDATE app.contacts SET blocked = true, blocked_at = now(), block_destination = 'junk' WHERE id = $1`, from.String()); err != nil {
		t.Fatal(err)
	}
	if _, err := f.h.super.Exec(ctx, `UPDATE app.contacts SET block_destination = 'trash' WHERE id = $1`, into.String()); err != nil {
		t.Fatal(err)
	}
	if _, err := f.merge(into, from); err != nil {
		t.Fatal(err)
	}
	if r := f.row(into); !r.blocked || r.dest != BlockToJunk {
		t.Errorf("from-only block: blocked=%v dest=%s, want blocked to junk (the blocked side's)", r.blocked, r.dest)
	}
}

// Notes that would pass the cap together refuse the merge, writing nothing.
func TestContactMergeRefusesOverlongNotes(t *testing.T) {
	f := newSavedFixture(t)
	ctx := context.Background()
	into, from, _, fromAddr := f.mergePair()
	if _, err := f.h.super.Exec(ctx, `UPDATE app.contacts SET notes = repeat('a', 3000) WHERE id = ANY($1)`, []string{into.String(), from.String()}); err != nil {
		t.Fatal(err)
	}
	if _, err := f.merge(into, from); !errors.Is(err, ErrMergeNotesTooLong) {
		t.Fatalf("err=%v, want ErrMergeNotesTooLong", err)
	}
	if f.idOf(fromAddr) != from || f.rows(from, "destroyed") != 0 {
		t.Error("a refused merge wrote something")
	}
}

// Separate restores: the merged-in addresses go to a NEW contact made from
// their home's own pre-merge state, with the home's keys; the contact they
// leave keeps its own address. A block on the merged contact survives.
func TestContactSeparateRestores(t *testing.T) {
	f := newSavedFixture(t)
	ctx := context.Background()
	into, from, intoAddr, fromAddr := f.mergePair()
	if _, err := f.h.super.Exec(ctx, `UPDATE app.contacts SET notes = 'from notes', user_display_name = 'From Mine', saved_at = now() WHERE id = $1`, from.String()); err != nil {
		t.Fatal(err)
	}
	if _, err := f.merge(into, from); err != nil {
		t.Fatal(err)
	}
	// After the merge the user blocks the merged contact.
	if _, err := f.set(into, ContactMarkersPatch{Blocked: boolp(true)}); err != nil {
		t.Fatal(err)
	}
	c, _ := f.get(into)
	var fromAddrID ulid.ULID
	for _, a := range c.Addresses {
		if a.Address == fromAddr {
			fromAddrID = a.ID
			if a.MergedFrom == nil || *a.MergedFrom != from {
				t.Errorf("address mergedFrom = %v, want %s", a.MergedFrom, from)
			}
		}
	}
	intoUpd := f.rows(into, "updated")

	var res SeparateResult
	if err := f.s.UserTx(ctx, f.aID, func(tx *UserTx) error {
		var e error
		res, e = tx.SeparateContact(ctx, into, []ulid.ULID{fromAddrID})
		return e
	}); err != nil {
		t.Fatal(err)
	}
	if len(res.Created) != 1 || res.AddressContact[fromAddrID] != res.Created[0] || res.Keys != 1 {
		t.Fatalf("separate result %+v", res)
	}
	n := res.Created[0]
	if n == from {
		t.Error("separate reused the destroyed id")
	}
	r := f.row(n)
	if r.displayName != "From Learned" || r.userName == nil || *r.userName != "From Mine" || r.notes != "from notes" || r.savedAt == nil {
		t.Errorf("separated contact did not restore from's state: %+v", r)
	}
	if !r.blocked {
		t.Error("separating unblocked an address the user had blocked")
	}
	if f.idOf(fromAddr) != n || f.idOf(intoAddr) != into {
		t.Error("addresses are not where separate should have put them")
	}
	// from's own key went back; into's own and the combined shared key stay.
	var nKeys, iKeys int
	if err := f.h.super.QueryRow(ctx, `SELECT count(*) FROM app.contact_keys WHERE contact_id = $1 AND fingerprint = $2 AND merged_from_contact IS NULL`,
		n.String(), "FPR-FROM-"+f.run).Scan(&nKeys); err != nil {
		t.Fatal(err)
	}
	if err := f.h.super.QueryRow(ctx, `SELECT count(*) FROM app.contact_keys WHERE contact_id = $1`, into.String()).Scan(&iKeys); err != nil {
		t.Fatal(err)
	}
	if nKeys != 1 || iKeys != 2 {
		t.Errorf("keys: separated has from's key=%d (want 1), into keeps %d (want 2)", nKeys, iKeys)
	}
	if f.rows(n, "created") != 1 || f.rows(into, "updated") != intoUpd+1 {
		t.Errorf("change rows: created %d, into updated +%d; want 1, +1", f.rows(n, "created"), f.rows(into, "updated")-intoUpd)
	}
	// Separating the last address, or one that is not the contact's, is refused.
	c, _ = f.get(into)
	if err := f.s.UserTx(ctx, f.aID, func(tx *UserTx) error {
		_, e := tx.SeparateContact(ctx, into, []ulid.ULID{c.Addresses[0].ID})
		return e
	}); !errors.Is(err, ErrSeparateAll) {
		t.Errorf("separating every address: err=%v", err)
	}
	if err := f.s.UserTx(ctx, f.aID, func(tx *UserTx) error {
		_, e := tx.SeparateContact(ctx, into, []ulid.ULID{fromAddrID})
		return e
	}); !errors.Is(err, ErrContactAddressNotFound) {
		t.Errorf("separating an address the contact no longer has: err=%v", err)
	}
}

// RLS: A cannot merge (either side) or separate B's contacts, nor pull B's
// address out through A's own contact. Positive control: A merging two of
// A's own contacts succeeds, so the refusals are about ownership.
func TestContactMergeAcrossUsersRefused(t *testing.T) {
	f := newSavedFixture(t)
	ctx := context.Background()
	mine := f.addr("mine")
	f.received(mine, "")
	mineID := f.idOf(mine)
	theirs := f.addr("theirs")
	mustUpsert(t, f.s, f.bID, ContactSourceMessage{FromAddress: theirs, AddressedAddress: f.b.Tag + "@example.com"})
	theirCID, _, _, _ := contactAddrRow(t, f.h, f.b.ID, theirs)
	theirID := mustULID(t, theirCID)
	var theirAddrID string
	if err := f.h.super.QueryRow(ctx, `SELECT id FROM app.contact_addresses WHERE user_id = $1 AND address = $2`, f.b.ID, theirs).Scan(&theirAddrID); err != nil {
		t.Fatal(err)
	}

	if _, err := f.merge(mineID, theirID); !errors.Is(err, ErrContactNotFound) {
		t.Errorf("A merging B's contact into A's: err=%v", err)
	}
	if _, err := f.merge(theirID, mineID); !errors.Is(err, ErrContactNotFound) {
		t.Errorf("A merging A's contact into B's: err=%v", err)
	}
	if err := f.s.UserTx(ctx, f.aID, func(tx *UserTx) error {
		_, e := tx.SeparateContact(ctx, theirID, []ulid.ULID{mustULID(t, theirAddrID)})
		return e
	}); !errors.Is(err, ErrContactNotFound) {
		t.Errorf("A separating B's contact: err=%v", err)
	}
	// B's rows untouched.
	if cid, _, _, _ := contactAddrRow(t, f.h, f.b.ID, theirs); cid != theirCID {
		t.Error("B's address moved")
	}
	var mi *string
	if err := f.h.super.QueryRow(ctx, `SELECT merged_into FROM app.contacts WHERE id = $1`, theirCID).Scan(&mi); err != nil || mi != nil {
		t.Errorf("B's contact merged_into = %v (err %v)", mi, err)
	}
	// Positive control.
	other := f.addr("other")
	f.received(other, "")
	if _, err := f.merge(mineID, f.idOf(other)); err != nil {
		t.Errorf("control: A merging A's own contacts failed: %v", err)
	}
	// A's contact now has two addresses; naming B's address id is not one of them.
	f.received(f.addr("third"), "")
	if err := f.s.UserTx(ctx, f.aID, func(tx *UserTx) error {
		_, e := tx.SeparateContact(ctx, mineID, []ulid.ULID{mustULID(t, theirAddrID)})
		return e
	}); !errors.Is(err, ErrContactAddressNotFound) {
		t.Errorf("A separating B's address out of A's contact: err=%v", err)
	}
}
