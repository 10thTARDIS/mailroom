// SPDX-License-Identifier: AGPL-3.0-only

//go:build integration

package store

// Calendar invitations (#725, migration 00063): the index, the answers, and
// the change rows each write owes the Email objects whose card it changes.

import (
	"context"
	"errors"
	"testing"

	"github.com/oklog/ulid/v2"
)

// emailChangeRows counts Email change_log rows for one message, as superuser.
func emailChangeRows(t *testing.T, h *harness, msg ulid.ULID) int {
	t.Helper()
	var n int
	if err := h.super.QueryRow(context.Background(),
		`SELECT count(*) FROM app.change_log WHERE object_type = 'Email' AND object_id = $1`,
		msg.String()).Scan(&n); err != nil {
		t.Fatalf("count change rows: %v", err)
	}
	return n
}

func TestCalendarInviteIndexAppendsOneChangeRowPerSibling(t *testing.T) {
	h := newHarness(t)
	ctx := context.Background()
	a, _ := seedTwoUsers(t, h.super)
	s := h.appStore()
	uid := mustULID(t, a.ID)
	key := CalendarKey{UID: "evt-1@example.com", Organizer: "Org@Example.com"}

	v0, v1, foreign := seedDSNMessage(t, ctx, h, a), seedDSNMessage(t, ctx, h, a), seedDSNMessage(t, ctx, h, a)
	record := func(row CalendarInviteRow) error {
		return s.UserTx(ctx, uid, func(tx *UserTx) error { return tx.RecordCalendarInvite(ctx, row) })
	}

	if err := record(CalendarInviteRow{MessageID: v0, Key: key, Sequence: 0, Method: CalendarRequest}); err != nil {
		t.Fatal(err)
	}
	if n := emailChangeRows(t, h, v0); n != 0 {
		t.Fatalf("the first version got %d change rows from indexing (ingest owns its created row)", n)
	}

	// An UNVERIFIED update changes no other card, so it appends nothing for
	// the earlier version (#725 review: a UID is not a secret).
	before := emailChangeRows(t, h, v0)
	spoof := seedDSNMessage(t, ctx, h, a)
	if err := record(CalendarInviteRow{MessageID: spoof, Key: key, Sequence: 7, Method: CalendarCancel}); err != nil {
		t.Fatal(err)
	}
	if got := emailChangeRows(t, h, v0) - before; got != 0 {
		t.Fatalf("an unverified CANCEL appended %d change rows to the genuine invitation, want 0", got)
	}

	before = emailChangeRows(t, h, v0)
	if err := record(CalendarInviteRow{MessageID: v1, Key: key, Sequence: 1, Method: CalendarRequest, Verified: true}); err != nil {
		t.Fatal(err)
	}
	if got := emailChangeRows(t, h, v0) - before; got != 1 {
		t.Fatalf("a verified update appended %d change rows to the earlier version, want exactly 1", got)
	}
	if n := emailChangeRows(t, h, v1); n != 0 {
		t.Fatalf("the new version got %d change rows from its own indexing", n)
	}

	// Re-recording is a no-op: nothing changed, nothing appended.
	before = emailChangeRows(t, h, v0)
	if err := record(CalendarInviteRow{MessageID: v1, Key: key, Sequence: 1, Method: CalendarRequest, Verified: true}); err != nil {
		t.Fatal(err)
	}
	if got := emailChangeRows(t, h, v0) - before; got != 0 {
		t.Fatalf("a no-op re-record appended %d rows", got)
	}

	// A CANCEL for the same UID from a DIFFERENT organiser is a different
	// event: it touches neither card.
	before0, before1 := emailChangeRows(t, h, v0), emailChangeRows(t, h, v1)
	if err := record(CalendarInviteRow{MessageID: foreign,
		Key: CalendarKey{UID: key.UID, Organizer: "stranger@example.net"}, Sequence: 9, Method: CalendarCancel, Verified: true}); err != nil {
		t.Fatal(err)
	}
	if emailChangeRows(t, h, v0) != before0 || emailChangeRows(t, h, v1) != before1 {
		t.Fatal("a stranger's CANCEL appended change rows to the real event's messages")
	}

	var versions map[CalendarKey][]CalendarVersion
	if err := s.UserTx(ctx, uid, func(tx *UserTx) (err error) {
		versions, err = tx.CalendarVersions(ctx, []CalendarKey{key})
		return err
	}); err != nil {
		t.Fatal(err)
	}
	// The organiser is matched case-insensitively (the column is citext; the
	// key normalises), and the stranger's row is not among them.
	got := versions[key.norm()]
	if len(got) != 3 {
		t.Fatalf("versions = %+v, want v0, v1 and the unverified spoof", got)
	}
	for _, v := range got {
		if (v.MessageID == spoof && v.Verified) || (v.MessageID == v1 && !v.Verified) {
			t.Fatalf("verified flag wrong on %+v", v)
		}
	}

	// A key the table would refuse is refused before it reaches SQL, and
	// leaves the transaction usable.
	if err := s.UserTx(ctx, uid, func(tx *UserTx) error {
		if err := tx.RecordCalendarInvite(ctx, CalendarInviteRow{MessageID: v0,
			Key: CalendarKey{UID: "", Organizer: "o@example.com"}, Method: CalendarRequest}); !errors.Is(err, ErrCalendarKeyInvalid) {
			t.Errorf("empty UID: err = %v", err)
		}
		if err := tx.RecordCalendarInvite(ctx, CalendarInviteRow{MessageID: v0,
			Key: CalendarKey{UID: "x\x00y", Organizer: "o@example.com"}, Method: CalendarRequest}); !errors.Is(err, ErrCalendarKeyInvalid) {
			t.Errorf("NUL in UID: err = %v", err)
		}
		_, err := tx.ChangeState(ctx, ObjectEmail)
		return err
	}); err != nil {
		t.Fatalf("transaction unusable after a refused key: %v", err)
	}
}

func TestCalendarResponseRecordAndUndo(t *testing.T) {
	h := newHarness(t)
	ctx := context.Background()
	a, _ := seedTwoUsers(t, h.super)
	s := h.appStore()
	uid := mustULID(t, a.ID)
	key := CalendarKey{UID: "evt-2@example.com", Organizer: "org@example.com"}
	inv, upd := seedDSNMessage(t, ctx, h, a), seedDSNMessage(t, ctx, h, a)

	// The update is indexed; the original is NOT (it predates the index, as
	// all mail before migration 00063 does). Answering it must index it.
	if err := s.UserTx(ctx, uid, func(tx *UserTx) error {
		return tx.RecordCalendarInvite(ctx, CalendarInviteRow{MessageID: upd, Key: key, Sequence: 1, Method: CalendarRequest})
	}); err != nil {
		t.Fatal(err)
	}

	// A scheduled submission to carry the reply.
	subID := NewULID()
	if _, err := h.super.Exec(ctx, `
		INSERT INTO app.submissions (id, user_id, account_id, identity_id, message_id,
		    from_address, envelope_to, send_at)
		VALUES ($1, $2, $3, $4, $5, $6, ARRAY['org@example.com']::citext[], now() + interval '1 hour')`,
		subID.String(), a.ID, a.AccountID, a.IdentityID, a.MessageID, a.Tag+"@example.com"); err != nil {
		t.Fatal(err)
	}

	b0, b1 := emailChangeRows(t, h, inv), emailChangeRows(t, h, upd)
	if err := s.UserTx(ctx, uid, func(tx *UserTx) error {
		return tx.RecordCalendarResponse(ctx, CalendarResponseRow{MessageID: inv, Key: key,
			Sequence: 0, PartStat: "ACCEPTED", SubmissionID: subID})
	}); err != nil {
		t.Fatal(err)
	}
	if d0, d1 := emailChangeRows(t, h, inv)-b0, emailChangeRows(t, h, upd)-b1; d0 != 1 || d1 != 1 {
		t.Fatalf("an answer appended %d/%d change rows to the two messages of the event, want 1/1", d0, d1)
	}

	read := func() (CalendarResponse, bool) {
		var out map[CalendarKey]CalendarResponse
		if err := s.UserTx(ctx, uid, func(tx *UserTx) (err error) {
			out, err = tx.CalendarResponses(ctx, []CalendarKey{key})
			return err
		}); err != nil {
			t.Fatal(err)
		}
		r, ok := out[key]
		return r, ok
	}
	r, ok := read()
	if !ok || r.PartStat != "ACCEPTED" || r.SubmissionID == nil || *r.SubmissionID != subID {
		t.Fatalf("response = %+v, %v", r, ok)
	}

	// UNDO SEND takes the answer back, and both cards hear about it.
	b0, b1 = emailChangeRows(t, h, inv), emailChangeRows(t, h, upd)
	if err := s.UserTx(ctx, uid, func(tx *UserTx) error { return tx.CancelSubmission(ctx, subID) }); err != nil {
		t.Fatal(err)
	}
	if _, ok := read(); ok {
		t.Fatal("a cancelled reply still reads as the user's answer")
	}
	if d0, d1 := emailChangeRows(t, h, inv)-b0, emailChangeRows(t, h, upd)-b1; d0 != 1 || d1 != 1 {
		t.Fatalf("undo appended %d/%d change rows, want 1/1", d0, d1)
	}

	// An ordinary send's cancel appends no Email rows at all.
	plain := NewULID()
	if _, err := h.super.Exec(ctx, `
		INSERT INTO app.submissions (id, user_id, account_id, identity_id, message_id,
		    from_address, envelope_to, send_at)
		VALUES ($1, $2, $3, $4, $5, $6, ARRAY['x@example.com']::citext[], now() + interval '1 hour')`,
		plain.String(), a.ID, a.AccountID, a.IdentityID, a.MessageID, a.Tag+"@example.com"); err != nil {
		t.Fatal(err)
	}
	b0 = emailChangeRows(t, h, inv)
	if err := s.UserTx(ctx, uid, func(tx *UserTx) error { return tx.CancelSubmission(ctx, plain) }); err != nil {
		t.Fatal(err)
	}
	if d := emailChangeRows(t, h, inv) - b0; d != 0 {
		t.Fatalf("cancelling an unrelated send appended %d rows to the invitation", d)
	}
}

// TestCalendarResponseOutlivesItsSentCopy: deleting the reply's message
// cascades its submission, which SETs NULL on the answer — the user still
// answered. Proves the FK action works under the app role's grants (no UPDATE
// on the table), which is not something to assume.
func TestCalendarResponseOutlivesItsSentCopy(t *testing.T) {
	h := newHarness(t)
	ctx := context.Background()
	a, _ := seedTwoUsers(t, h.super)
	s := h.appStore()
	uid := mustULID(t, a.ID)
	key := CalendarKey{UID: "evt-3@example.com", Organizer: "org@example.com"}
	inv, replyMsg := seedDSNMessage(t, ctx, h, a), seedDSNMessage(t, ctx, h, a)
	subID := NewULID()
	if _, err := h.super.Exec(ctx, `
		INSERT INTO app.submissions (id, user_id, account_id, identity_id, message_id,
		    from_address, envelope_to, state)
		VALUES ($1, $2, $3, $4, $5, $6, ARRAY['org@example.com']::citext[], 'sent')`,
		subID.String(), a.ID, a.AccountID, a.IdentityID, replyMsg.String(), a.Tag+"@example.com"); err != nil {
		t.Fatal(err)
	}
	if err := s.UserTx(ctx, uid, func(tx *UserTx) error {
		return tx.RecordCalendarResponse(ctx, CalendarResponseRow{MessageID: inv, Key: key,
			PartStat: "DECLINED", SubmissionID: subID})
	}); err != nil {
		t.Fatal(err)
	}
	if err := s.UserTx(ctx, uid, func(tx *UserTx) error {
		_, err := tx.tx.Exec(ctx, `DELETE FROM app.messages WHERE id = $1`, replyMsg.String())
		return err
	}); err != nil {
		t.Fatalf("delete the sent reply as the app role: %v", err)
	}
	var out map[CalendarKey]CalendarResponse
	if err := s.UserTx(ctx, uid, func(tx *UserTx) (err error) {
		out, err = tx.CalendarResponses(ctx, []CalendarKey{key})
		return err
	}); err != nil {
		t.Fatal(err)
	}
	r, ok := out[key]
	if !ok || r.PartStat != "DECLINED" || r.SubmissionID != nil {
		t.Fatalf("after deleting the sent copy: %+v, %v (want DECLINED with no submission)", r, ok)
	}
}

// TestCalendarRowsAreUserIsolated is the behavioural RLS negative test for the
// two queries (the table-level inventory in rls_test.go covers the tables).
// B's fixture carries an indexed invitation and an answer; A asks for B's
// exact key and must see neither — and the control proves B can.
func TestCalendarRowsAreUserIsolated(t *testing.T) {
	h := newHarness(t)
	ctx := context.Background()
	a, b := seedTwoUsers(t, h.super)
	s := h.appStore()
	bKey := CalendarKey{UID: "event-" + b.Tag + "@example.com", Organizer: "organizer-" + b.Tag + "@example.net"}

	look := func(user string) (int, int) {
		var (
			vs map[CalendarKey][]CalendarVersion
			rs map[CalendarKey]CalendarResponse
		)
		if err := s.UserTx(ctx, mustULID(t, user), func(tx *UserTx) (err error) {
			if vs, err = tx.CalendarVersions(ctx, []CalendarKey{bKey}); err != nil {
				return err
			}
			rs, err = tx.CalendarResponses(ctx, []CalendarKey{bKey})
			return err
		}); err != nil {
			t.Fatal(err)
		}
		return len(vs[bKey.norm()]), len(rs)
	}
	if v, r := look(b.ID); v != 1 || r != 1 {
		t.Fatalf("CONTROL: B sees %d versions and %d answers of B's own event, want 1 and 1", v, r)
	}
	if v, r := look(a.ID); v != 0 || r != 0 {
		t.Fatalf("A sees %d versions and %d answers of B's event", v, r)
	}
	// (That A cannot ANSWER B's invitation is the API's claim, not the
	// store's — Email/replyToInvite resolves the message under A's identity
	// first — and is probed in internal/api's rls_integration_test.go.)
}

// TestCalendarVersionsCapIsPerKeyAndKeepsTheDecisiveRows: the cap is applied
// PER EVENT KEY in SQL, and the key's newest verified CANCEL (and highest
// verified REQUEST) is returned whatever the cap (#725 review). The old code
// took one LIMIT over every row sharing the UID and filtered by the full key
// afterwards, so a sibling instance's, or a stranger's, rows could push the
// real CANCEL out of the window.
func TestCalendarVersionsCapIsPerKeyAndKeepsTheDecisiveRows(t *testing.T) {
	h := newHarness(t)
	ctx := context.Background()
	a, _ := seedTwoUsers(t, h.super)
	s := h.appStore()
	uid := mustULID(t, a.ID)
	saved := maxCalendarVersions
	maxCalendarVersions = 3
	t.Cleanup(func() { maxCalendarVersions = saved })

	key := CalendarKey{UID: "evt-cap@example.com", Organizer: "org@example.com"}
	sibling := CalendarKey{UID: key.UID, RecurrenceID: "U20261110T090000Z", Organizer: "org@example.com"}
	stranger := CalendarKey{UID: key.UID, Organizer: "mallory@example.org"}
	record := func(id ulid.ULID, k CalendarKey, seq int, method string, verified bool) {
		t.Helper()
		if err := s.UserTx(ctx, uid, func(tx *UserTx) error {
			return tx.RecordCalendarInvite(ctx, CalendarInviteRow{MessageID: id, Key: k, Sequence: seq, Method: method, Verified: verified})
		}); err != nil {
			t.Fatal(err)
		}
	}
	read := func(k CalendarKey) []CalendarVersion {
		t.Helper()
		var out map[CalendarKey][]CalendarVersion
		if err := s.UserTx(ctx, uid, func(tx *UserTx) (err error) {
			out, err = tx.CalendarVersions(ctx, []CalendarKey{k})
			return err
		}); err != nil {
			t.Fatal(err)
		}
		return out[k.norm()]
	}

	// The real event: an original (unverified, so only the per-key cap can
	// keep it) and the organiser's verified CANCEL, OLDER than everything
	// seeded after (ULIDs order the seeding; received_at ties).
	original := seedDSNMessage(t, ctx, h, a)
	record(original, key, 0, CalendarRequest, false)
	cancel := seedDSNMessage(t, ctx, h, a)
	record(cancel, key, 1, CalendarCancel, true)
	// Then twice the cap of NEWER rows on the sibling instance and from a
	// stranger reusing the UID.
	for i := 0; i < 2*maxCalendarVersions; i++ {
		record(seedDSNMessage(t, ctx, h, a), sibling, i, CalendarRequest, false)
		record(seedDSNMessage(t, ctx, h, a), stranger, i, CalendarRequest, false)
	}
	got := read(key)
	if len(got) != 2 {
		t.Fatalf("real key: %d versions, want its 2 (other keys' rows must not crowd it out): %+v", len(got), got)
	}

	// Now flood the REAL key with newer unverified rows: the cap trims them,
	// and the verified CANCEL is still returned.
	for i := 0; i < 2*maxCalendarVersions; i++ {
		record(seedDSNMessage(t, ctx, h, a), key, 9, CalendarRequest, false)
	}
	got = read(key)
	sawCancel := false
	for _, v := range got {
		if v.MessageID == cancel {
			sawCancel = true
		}
	}
	if !sawCancel {
		t.Fatalf("the verified CANCEL was pushed out of the window by newer rows: %+v", got)
	}
	if len(got) > maxCalendarVersions+2 {
		t.Fatalf("%d versions returned, want at most the cap (%d) plus the decisive rows", len(got), maxCalendarVersions)
	}
}
