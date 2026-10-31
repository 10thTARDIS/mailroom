// SPDX-License-Identifier: AGPL-3.0-only

//go:build integration

package store

// SENDERS AND NOTIFICATIONS (#59), the store half: contact markers and their
// change rows, the blocked-sender lookup, the push counter, `is:vip`, quiet
// hours on DeskPreferences and per-account push — each with its RLS negative.

import (
	"context"
	"errors"
	"testing"

	"github.com/oklog/ulid/v2"

	"mailroom/internal/rules"
)

// seedContact inserts a contact owning one address for user, with the given
// markers, straight through the superuser (the fixture, not the subject).
func seedContact(t *testing.T, h *harness, user ulid.ULID, addr string, vip, blocked bool) ulid.ULID {
	t.Helper()
	ctx := context.Background()
	id := NewULID()
	if _, err := h.super.Exec(ctx, `
		INSERT INTO app.contacts (id, user_id, display_name, vip, blocked, blocked_at)
		VALUES ($1, $2, '', $3, $4, CASE WHEN $4 THEN now() END)`,
		id.String(), user.String(), vip, blocked); err != nil {
		t.Fatalf("seed contact: %v", err)
	}
	if _, err := h.super.Exec(ctx, `
		INSERT INTO app.contact_addresses (id, user_id, contact_id, address) VALUES ($1, $2, $3, $4)`,
		NewULID().String(), user.String(), id.String(), addr); err != nil {
		t.Fatalf("seed contact address: %v", err)
	}
	return id
}

func contactUpdatedRows(t *testing.T, h *harness, user, contact ulid.ULID) int {
	t.Helper()
	var n int
	if err := h.super.QueryRow(context.Background(),
		`SELECT count(*) FROM app.change_log WHERE user_id=$1 AND object_type='Contact' AND object_id=$2 AND op='updated'`,
		user.String(), contact.String()).Scan(&n); err != nil {
		t.Fatal(err)
	}
	return n
}

func TestContactMarkersAppendOneChangeRowPerChange(t *testing.T) {
	f := newDrawerFixture(t)
	ctx := context.Background()
	cid := seedContact(t, f.h, f.userID, "someone@example.com", false, false)
	yes, no := true, false
	trash := BlockToTrash

	set := func(p ContactMarkersPatch) (bool, error) {
		var changed bool
		err := f.st.UserTx(ctx, f.userID, func(tx *UserTx) error {
			var e error
			changed, e = tx.SetContactMarkers(ctx, cid, p)
			return e
		})
		return changed, err
	}
	read := func() Contact {
		var c []Contact
		if err := f.st.UserTx(ctx, f.userID, func(tx *UserTx) error {
			var e error
			c, e = tx.GetContacts(ctx, []ulid.ULID{cid})
			return e
		}); err != nil || len(c) != 1 {
			t.Fatalf("get contact: %v %d", err, len(c))
		}
		return c[0]
	}

	if changed, err := set(ContactMarkersPatch{VIP: &yes}); err != nil || !changed {
		t.Fatalf("mark VIP: changed=%v err=%v", changed, err)
	}
	if got := contactUpdatedRows(t, f.h, f.userID, cid); got != 1 {
		t.Fatalf("change rows after marking VIP = %d, want 1", got)
	}
	// The same value again is a no-op: no change row.
	if changed, err := set(ContactMarkersPatch{VIP: &yes}); err != nil || changed {
		t.Fatalf("re-mark VIP: changed=%v err=%v, want a no-op", changed, err)
	}
	if got := contactUpdatedRows(t, f.h, f.userID, cid); got != 1 {
		t.Fatalf("a no-op appended a change row: %d", got)
	}

	// Block (default destination junk): blocked_at stamped.
	if _, err := set(ContactMarkersPatch{Blocked: &yes}); err != nil {
		t.Fatal(err)
	}
	c := read()
	if !c.Blocked || c.BlockDestination != BlockToJunk || c.BlockedAt == nil || !c.VIP {
		t.Fatalf("after block: %+v", c)
	}
	firstStamp := *c.BlockedAt
	// Changing only the destination keeps the original block time.
	if _, err := set(ContactMarkersPatch{BlockDestination: &trash}); err != nil {
		t.Fatal(err)
	}
	c = read()
	if c.BlockDestination != BlockToTrash || c.BlockedAt == nil || !c.BlockedAt.Equal(firstStamp) {
		t.Fatalf("after re-destination: %+v (want trash, stamp kept %v)", c, firstStamp)
	}
	// Unblock clears the stamp.
	if _, err := set(ContactMarkersPatch{Blocked: &no}); err != nil {
		t.Fatal(err)
	}
	if c = read(); c.Blocked || c.BlockedAt != nil {
		t.Fatalf("after unblock: %+v", c)
	}
	if got := contactUpdatedRows(t, f.h, f.userID, cid); got != 4 {
		t.Errorf("change rows = %d, want 4 (VIP, block, destination, unblock)", got)
	}

	bad := "spam-folder"
	if _, err := set(ContactMarkersPatch{BlockDestination: &bad}); !errors.Is(err, ErrBlockDestinationInvalid) {
		t.Errorf("invalid destination: err=%v, want ErrBlockDestinationInvalid", err)
	}
}

// RLS NEGATIVE: user A can neither mark B's contact nor see B's block.
func TestContactMarkersAndBlockAreUserScoped(t *testing.T) {
	a := newDrawerFixture(t)
	b := newDrawerFixture(t)
	ctx := context.Background()
	const addr = "shared@example.com"
	bContact := seedContact(t, b.h, b.userID, addr, false, true)
	seedContact(t, a.h, a.userID, addr, false, false)

	yes := true
	err := a.st.UserTx(ctx, a.userID, func(tx *UserTx) error {
		_, e := tx.SetContactMarkers(ctx, bContact, ContactMarkersPatch{VIP: &yes})
		return e
	})
	if !errors.Is(err, ErrContactNotFound) {
		t.Errorf("A marking B's contact: err=%v, want ErrContactNotFound", err)
	}
	var vip bool
	if err := a.h.super.QueryRow(ctx, `SELECT vip FROM app.contacts WHERE id=$1`, bContact.String()).Scan(&vip); err != nil || vip {
		t.Errorf("B's contact vip=%v err=%v after A's attempt", vip, err)
	}

	// B blocked the address; A did not. A's lookup must not see B's block.
	if err := a.st.UserTx(ctx, a.userID, func(tx *UserTx) error {
		_, blocked, e := tx.BlockedDestination(ctx, addr)
		if blocked {
			t.Error("A's BlockedDestination saw B's block")
		}
		return e
	}); err != nil {
		t.Fatal(err)
	}
	// ...and B's does (the positive control that makes the negative mean something).
	if err := b.st.UserTx(ctx, b.userID, func(tx *UserTx) error {
		dest, blocked, e := tx.BlockedDestination(ctx, "SHARED@example.com")
		if !blocked || dest != BlockToJunk {
			t.Errorf("B's own block: blocked=%v dest=%q, want junk (case-insensitively)", blocked, dest)
		}
		return e
	}); err != nil {
		t.Fatal(err)
	}
}

func (f *drawerFixture) setFrom(t *testing.T, id ulid.ULID, addr string) {
	t.Helper()
	if _, err := f.h.super.Exec(context.Background(),
		`UPDATE app.messages SET from_address = NULLIF($2, '') WHERE id = $1`, id.String(), addr); err != nil {
		t.Fatal(err)
	}
}

// THE PUSH COUNTER against the desk's own line, the $important rule keyword
// and the VIP set — and nothing of another user's.
func TestPushCountsByCategory(t *testing.T) {
	f := newDrawerFixture(t)
	other := newDrawerFixture(t)
	ctx := context.Background()
	seedContact(t, f.h, f.userID, "boss@example.com", true, false)
	// The OTHER user marks a different address VIP; it must not count for f.
	seedContact(t, other.h, other.userID, "stranger@example.com", true, false)

	above := f.seedMessage(t, "above", msgOpt{score: score32(0.9)})
	below := f.seedMessage(t, "below", msgOpt{score: score32(0.1)})
	unscored := f.seedMessage(t, "unscored", msgOpt{})
	pulledUp := f.seedMessage(t, "pulled up", msgOpt{score: score32(0.1)})
	pushedDown := f.seedMessage(t, "pushed down", msgOpt{score: score32(0.9)})
	important := f.seedMessage(t, "rule said important", msgOpt{score: score32(0.1)})
	fromVIP := f.seedMessage(t, "from the boss", msgOpt{score: score32(0.1)})
	fromStranger := f.seedMessage(t, "from the other user's VIP", msgOpt{score: score32(0.1)})
	f.setFrom(t, fromVIP, "Boss@Example.com") // citext: case does not matter
	f.setFrom(t, fromStranger, "stranger@example.com")
	if _, err := f.h.super.Exec(ctx, `UPDATE app.messages SET keywords = ARRAY[$2::text] WHERE id = $1`,
		important.String(), rules.KeywordImportant); err != nil {
		t.Fatal(err)
	}
	for id, side := range map[ulid.ULID]LineSide{pulledUp: LineSideSurface, pushedDown: LineSideDrawer} {
		if err := f.st.UserTx(ctx, f.userID, func(tx *UserTx) error { return tx.SetLineSide(ctx, id, side) }); err != nil {
			t.Fatal(err)
		}
	}
	foreign := other.seedMessage(t, "another user's, above", msgOpt{score: score32(0.9)})

	ids := []ulid.ULID{above, below, unscored, pulledUp, pushedDown, important, fromVIP, fromStranger, foreign}
	var c PushCounts
	if err := f.st.UserTx(ctx, f.userID, func(tx *UserTx) error {
		var e error
		c, e = tx.PushCounts(ctx, ids, 0.5)
		return e
	}); err != nil {
		t.Fatal(err)
	}
	// above + pulledUp + important; NOT pushedDown (the user's placement wins),
	// NOT unscored (no side), NOT the foreign one (RLS).
	want := PushCounts{Found: 8, AboveLine: 3, VIP: 1, AboveLineOrVIP: 4}
	if c != want {
		t.Errorf("PushCounts = %+v, want %+v", c, want)
	}

	// The threshold is the caller's line: at 0.05 every scored message is
	// above it except the one the user pushed down.
	if err := f.st.UserTx(ctx, f.userID, func(tx *UserTx) error {
		var e error
		c, e = tx.PushCounts(ctx, ids, 0.05)
		return e
	}); err != nil {
		t.Fatal(err)
	}
	if c.AboveLine != 6 {
		t.Errorf("AboveLine at line 0.05 = %d, want 6 (all scored mail but the pushed-down one)", c.AboveLine)
	}
}

// is:vip is a two-valued store predicate: it and its negation partition the
// mailbox exactly, including mail with no From at all.
func TestSearchVIPPredicatePartitions(t *testing.T) {
	f := newDrawerFixture(t)
	ctx := context.Background()
	seedContact(t, f.h, f.userID, "vip@example.com", true, false)
	seedContact(t, f.h, f.userID, "plain@example.com", false, false)
	vipMsg := f.seedMessage(t, "vip", msgOpt{})
	plainMsg := f.seedMessage(t, "plain", msgOpt{})
	noFrom := f.seedMessage(t, "no from", msgOpt{})
	f.setFrom(t, vipMsg, "vip@example.com")
	f.setFrom(t, plainMsg, "plain@example.com")
	f.setFrom(t, noFrom, "")

	query := func(p SearchPredicate) map[ulid.ULID]bool {
		out := map[ulid.ULID]bool{}
		if err := f.st.UserTx(ctx, f.userID, func(tx *UserTx) error {
			ids, _, e := tx.QueryEmails(ctx, EmailQuery{Search: &p})
			for _, id := range ids {
				out[id] = true
			}
			return e
		}); err != nil {
			t.Fatal(err)
		}
		return out
	}
	vip := query(SearchPredicate{Op: SearchVIP})
	notVIP := query(SearchPredicate{Op: SearchNot, Kids: []SearchPredicate{{Op: SearchVIP}}})
	if !vip[vipMsg] || vip[plainMsg] || vip[noFrom] || len(vip) != 1 {
		t.Errorf("is:vip = %v, want exactly the VIP's message", vip)
	}
	if notVIP[vipMsg] || !notVIP[plainMsg] || !notVIP[noFrom] {
		t.Errorf("-is:vip = %v, want the plain and the From-less message", notVIP)
	}
}

func TestQuietHoursRoundTripOnDeskPreferences(t *testing.T) {
	f := newDrawerFixture(t)
	ctx := context.Background()
	var p DeskPreferences
	if err := f.st.UserTx(ctx, f.userID, func(tx *UserTx) error {
		var e error
		p, e = tx.GetDeskPreferences(ctx)
		return e
	}); err != nil {
		t.Fatal(err)
	}
	def := QuietHours{Enabled: false, Start: 1320, End: 420, Days: 127, Timezone: "UTC", AllowVIP: true}
	if p.QuietHours != def {
		t.Fatalf("default quiet hours = %+v, want %+v (off, VIPs allowed)", p.QuietHours, def)
	}
	updatedRows := func() int {
		var n int
		if err := f.h.super.QueryRow(ctx, `SELECT count(*) FROM app.change_log WHERE user_id=$1 AND object_type='DeskPreferences' AND op='updated'`,
			f.userID.String()).Scan(&n); err != nil {
			t.Fatal(err)
		}
		return n
	}
	q := QuietHours{Enabled: true, Start: 1380, End: 360, Days: 0b0111110, Timezone: "Europe/London", AllowVIP: false}
	for i := 0; i < 2; i++ { // the second write is a no-op
		if err := f.st.UserTx(ctx, f.userID, func(tx *UserTx) error {
			return tx.UpdateDeskPreferences(ctx, DeskPreferencesPatch{QuietHours: &q})
		}); err != nil {
			t.Fatal(err)
		}
	}
	if got := updatedRows(); got != 1 {
		t.Errorf("DeskPreferences updated rows = %d, want 1 (a repeat is a no-op)", got)
	}
	if err := f.st.UserTx(ctx, f.userID, func(tx *UserTx) error {
		var e error
		p, e = tx.GetDeskPreferences(ctx)
		return e
	}); err != nil {
		t.Fatal(err)
	}
	if p.QuietHours != q {
		t.Errorf("quiet hours read back %+v, want %+v", p.QuietHours, q)
	}
	// The CHECK holds a value Go forgot to validate.
	badQ := QuietHours{Enabled: true, Start: 1440, End: 0, Days: 1, Timezone: "UTC"}
	err := f.st.UserTx(ctx, f.userID, func(tx *UserTx) error {
		return tx.UpdateDeskPreferences(ctx, DeskPreferencesPatch{QuietHours: &badQ})
	})
	if !errors.Is(err, ErrDeskPreferencesInvalid) {
		t.Errorf("start=1440: err=%v, want ErrDeskPreferencesInvalid", err)
	}
}

func TestAccountNotifyPushToggle(t *testing.T) {
	f := newDrawerFixture(t)
	ctx := context.Background()
	off := false
	read := func() Account {
		var a Account
		if err := f.st.UserTx(ctx, f.userID, func(tx *UserTx) error {
			var e error
			a, e = tx.GetAccount(ctx, f.accountID)
			return e
		}); err != nil {
			t.Fatal(err)
		}
		return a
	}
	if !read().NotifyPush {
		t.Fatal("a new account must push by default")
	}
	rows := func() int {
		var n int
		if err := f.h.super.QueryRow(ctx, `SELECT count(*) FROM app.change_log WHERE user_id=$1 AND object_type='Account' AND object_id=$2`,
			f.userID.String(), f.accountID.String()).Scan(&n); err != nil {
			t.Fatal(err)
		}
		return n
	}
	before := rows()
	for i := 0; i < 2; i++ {
		if err := f.st.UserTx(ctx, f.userID, func(tx *UserTx) error {
			_, e := tx.UpdateAccount(ctx, f.accountID, AccountPatch{NotifyPush: &off})
			return e
		}); err != nil {
			t.Fatal(err)
		}
	}
	if read().NotifyPush {
		t.Error("notifyPush still true after switching it off")
	}
	if got := rows() - before; got != 1 {
		t.Errorf("Account change rows = %d, want 1 (the repeat is a no-op)", got)
	}
}
