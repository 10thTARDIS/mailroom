// SPDX-License-Identifier: AGPL-3.0-only

//go:build integration

package store

// Signed-in devices, the stream re-check, push session binding and the sweep
// (#937, ADR-048 sections 6 and 7, migration 00083), through the store methods as
// mailroom_app. What is proven:
//
//  1. The devices list shows only the caller's LIVE sessions, newest use first.
//  2. A device sign-out revokes exactly the given sessions of THIS user, returns
//     their sealed refresh tokens, and deletes exactly those sessions' push
//     subscriptions in the same transaction: a NULL-session subscription and
//     another session's survive, and another user's id matches nothing.
//  3. The stream re-check is false for a revoked session, a disabled or deleted
//     user and a session past the allowance, and true otherwise.
//  4. Re-registering a push endpoint MOVES it to the new session.
//  5. The sweep deletes what ADR-048 section 7 says, reports every reason (zero
//     included), and keeps the rest. It needs its owner SELECT policy: without it
//     the DELETE is a silent zero-row success.

import (
	"bytes"
	"context"
	"testing"
	"time"

	"github.com/oklog/ulid/v2"
)

func pushSub(t *testing.T, s *Store, user ulid.ULID, endpoint string, session *ulid.ULID) PushSubscription {
	t.Helper()
	var ps PushSubscription
	if err := s.UserTx(context.Background(), user, func(tx *UserTx) error {
		var err error
		ps, err = tx.CreatePushSubscription(context.Background(), PushSubscriptionInput{
			Endpoint: endpoint, KeysP256dh: "p", KeysAuth: "a", DeviceName: "dev", SessionID: session,
		})
		return err
	}); err != nil {
		t.Fatalf("create push subscription %s: %v", endpoint, err)
	}
	return ps
}

func pushSessionOf(t *testing.T, h *harness, id ulid.ULID) (exists bool, session *string) {
	t.Helper()
	err := h.super.QueryRow(context.Background(),
		`SELECT session_id FROM app.push_subscriptions WHERE id = $1`, id.String()).Scan(&session)
	if err != nil {
		if isNoRows(err) {
			return false, nil
		}
		t.Fatal(err)
	}
	return true, session
}

func TestListLiveSessions(t *testing.T) {
	h := newHarness(t)
	ctx := context.Background()
	a, b := seedTwoUsers(t, h.super)
	s := h.appStore()
	aID, bID := mustULID(t, a.ID), mustULID(t, b.ID)

	// seedTwoUsers already gave each user a seeded live session; look at the ones
	// this test adds by id, and count the rest as part of the list.
	older := createSession(t, s, aID, "list-a-older", func(n *NewSession) { n.UserAgent = "Older Browser"; n.Addr = "192.0.2.10" })
	newer := createSession(t, s, aID, "list-a-newer", func(n *NewSession) { n.UserAgent = "Newer Browser"; n.Addr = "192.0.2.11" })
	gone := createSession(t, s, aID, "list-a-revoked", nil)
	bs := createSession(t, s, bID, "list-b", nil)
	if _, err := h.super.Exec(ctx, `UPDATE app.sessions SET last_seen_at = now() - interval '2 hours' WHERE id = $1`, older.String()); err != nil {
		t.Fatal(err)
	}
	if _, err := h.super.Exec(ctx, `UPDATE app.sessions SET last_seen_at = now() + interval '1 hour' WHERE id = $1`, newer.String()); err != nil {
		t.Fatal(err)
	}
	if err := s.UserTx(ctx, aID, func(tx *UserTx) error {
		_, err := tx.RevokeSession(ctx, gone, SessionRevokedSignedOut)
		return err
	}); err != nil {
		t.Fatal(err)
	}

	var rows []SessionRow
	if err := s.UserTx(ctx, aID, func(tx *UserTx) error {
		var err error
		rows, err = tx.ListLiveSessions(ctx)
		return err
	}); err != nil {
		t.Fatal(err)
	}

	idx := map[ulid.ULID]int{}
	for i, r := range rows {
		idx[r.ID] = i
	}
	// Positive controls: the two live sessions of A are there, with their fields.
	for _, want := range []struct {
		id       ulid.ULID
		ua, addr string
	}{{newer, "Newer Browser", "192.0.2.11"}, {older, "Older Browser", "192.0.2.10"}} {
		i, ok := idx[want.id]
		if !ok {
			t.Fatalf("A's live session %s is not listed", want.id)
		}
		r := rows[i]
		if r.UserAgent != want.ua || r.LastAddr != want.addr || r.CreatedAt.IsZero() || r.LastSeenAt.IsZero() || r.LastCheckedAt.IsZero() {
			t.Errorf("session %s row = %+v, want ua %q addr %q with every time set", want.id, r, want.ua, want.addr)
		}
	}
	if _, ok := idx[gone]; ok {
		t.Error("a revoked session is listed")
	}
	if _, ok := idx[bs]; ok {
		t.Error("B's session is listed for A")
	}
	if idx[newer] > idx[older] {
		t.Errorf("order: the newer session is at %d after the older at %d, want newest use first", idx[newer], idx[older])
	}
	for i := 1; i < len(rows); i++ {
		if rows[i].LastSeenAt.After(rows[i-1].LastSeenAt) {
			t.Errorf("rows are not ordered by last_seen_at descending at %d", i)
		}
	}
}

func TestRevokeSessionsRevokesOnlyTheGivenIDsAndTheirPush(t *testing.T) {
	h := newHarness(t)
	ctx := context.Background()
	a, b := seedTwoUsers(t, h.super)
	s := h.appStore()
	aID, bID := mustULID(t, a.ID), mustULID(t, b.ID)

	s1 := createSession(t, s, aID, "rv-a1", func(n *NewSession) { n.RefreshCT = []byte("sealed-1") })
	s2 := createSession(t, s, aID, "rv-a2", nil)
	s3 := createSession(t, s, aID, "rv-a3", func(n *NewSession) { n.RefreshCT = nil })
	sb := createSession(t, s, bID, "rv-b1", nil)

	sub1 := pushSub(t, s, aID, "https://push.example.com/a1", &s1)
	sub2 := pushSub(t, s, aID, "https://push.example.com/a2", &s2)
	sub3 := pushSub(t, s, aID, "https://push.example.com/a-none", nil)
	subB := pushSub(t, s, bID, "https://push.example.com/b1", &sb)

	// Positive control: the bindings are really recorded, else "deleted exactly
	// those" below proves nothing.
	if ok, sess := pushSessionOf(t, h, sub1.ID); !ok || sess == nil || *sess != s1.String() {
		t.Fatalf("subscription 1 is bound to %v (exists %v), want %s", sess, ok, s1)
	}
	if ok, sess := pushSessionOf(t, h, sub3.ID); !ok || sess != nil {
		t.Fatalf("subscription 3 should have no session, got %v", sess)
	}

	var logBefore int
	if err := h.super.QueryRow(ctx, `SELECT count(*) FROM app.change_log WHERE user_id = $1`, a.ID).Scan(&logBefore); err != nil {
		t.Fatal(err)
	}

	// A names its own s1 and B's session. B's id matches nothing, by RLS.
	var got RevokedSessions
	if err := s.UserTx(ctx, aID, func(tx *UserTx) error {
		var err error
		got, err = tx.RevokeSessions(ctx, []ulid.ULID{s1, sb}, SessionRevokedSignedOutElsewhere)
		return err
	}); err != nil {
		t.Fatal(err)
	}
	if len(got.IDs) != 1 || got.IDs[0] != s1 {
		t.Fatalf("revoked %v, want only %s", got.IDs, s1)
	}
	if !bytes.Equal(got.RefreshCT[s1], []byte("sealed-1")) {
		t.Errorf("returned refresh ciphertext %q, want the session's sealed token", got.RefreshCT[s1])
	}
	if got.PushDeleted != 1 {
		t.Errorf("PushDeleted = %d, want 1", got.PushDeleted)
	}

	live := func(id ulid.ULID) bool {
		var revoked *string
		if err := h.super.QueryRow(ctx, `SELECT revoked_reason FROM app.sessions WHERE id = $1`, id.String()).Scan(&revoked); err != nil {
			t.Fatal(err)
		}
		return revoked == nil
	}
	var reason string
	if err := h.super.QueryRow(ctx, `SELECT revoked_reason FROM app.sessions WHERE id = $1`, s1.String()).Scan(&reason); err != nil || reason != SessionRevokedSignedOutElsewhere {
		t.Fatalf("s1 revoked_reason = %q, %v", reason, err)
	}
	if live(s1) || !live(s2) || !live(s3) || !live(sb) {
		t.Errorf("liveness s1=%v s2=%v s3=%v sb=%v, want only s1 revoked", live(s1), live(s2), live(s3), live(sb))
	}
	// Push: exactly s1's subscription is gone.
	if ok, _ := pushSessionOf(t, h, sub1.ID); ok {
		t.Error("the revoked session's push subscription survived")
	}
	for name, id := range map[string]ulid.ULID{"another session's": sub2.ID, "the NULL-session": sub3.ID, "B's": subB.ID} {
		if ok, _ := pushSessionOf(t, h, id); !ok {
			t.Errorf("%s push subscription was deleted", name)
		}
	}

	// A second revoke of the same id finds nothing live.
	if err := s.UserTx(ctx, aID, func(tx *UserTx) error {
		r, err := tx.RevokeSessions(ctx, []ulid.ULID{s1}, SessionRevokedSignedOutElsewhere)
		if err == nil && (len(r.IDs) != 0 || r.PushDeleted != 0) {
			t.Errorf("revoking an already-revoked session reported %+v", r)
		}
		return err
	}); err != nil {
		t.Fatal(err)
	}
	// No ids is a no-op, not "every session".
	if err := s.UserTx(ctx, aID, func(tx *UserTx) error {
		r, err := tx.RevokeSessions(ctx, nil, SessionRevokedSignedOutElsewhere)
		if err == nil && len(r.IDs) != 0 {
			t.Errorf("an empty id list revoked %v", r.IDs)
		}
		return err
	}); err != nil {
		t.Fatal(err)
	}
	if !live(s2) || !live(s3) {
		t.Error("an empty or repeated revoke damaged other sessions")
	}
	// Appends no change_log row: sessions are operational data.
	var logAfter int
	if err := h.super.QueryRow(ctx, `SELECT count(*) FROM app.change_log WHERE user_id = $1`, a.ID).Scan(&logAfter); err != nil {
		t.Fatal(err)
	}
	if logAfter != logBefore {
		t.Errorf("revoking sessions and deleting push subscriptions moved change_log from %d to %d rows", logBefore, logAfter)
	}
}

func TestRevokeSessionsExceptKeepsTheKeptOne(t *testing.T) {
	h := newHarness(t)
	ctx := context.Background()
	a, b := seedTwoUsers(t, h.super)
	s := h.appStore()
	aID, bID := mustULID(t, a.ID), mustULID(t, b.ID)

	keep := createSession(t, s, aID, "ex-keep", nil)
	other1 := createSession(t, s, aID, "ex-o1", func(n *NewSession) { n.RefreshCT = []byte("sealed-o1") })
	other2 := createSession(t, s, aID, "ex-o2", nil)
	sb := createSession(t, s, bID, "ex-b", nil)
	pKeep := pushSub(t, s, aID, "https://push.example.com/ex-keep", &keep)
	pOther := pushSub(t, s, aID, "https://push.example.com/ex-o1", &other1)
	pNone := pushSub(t, s, aID, "https://push.example.com/ex-none", nil)
	pB := pushSub(t, s, bID, "https://push.example.com/ex-b", &sb)

	var got RevokedSessions
	if err := s.UserTx(ctx, aID, func(tx *UserTx) error {
		var err error
		got, err = tx.RevokeSessionsExcept(ctx, keep, SessionRevokedSignedOutElsewhere)
		return err
	}); err != nil {
		t.Fatal(err)
	}
	revoked := map[ulid.ULID]bool{}
	for _, id := range got.IDs {
		revoked[id] = true
	}
	if revoked[keep] {
		t.Fatal("the kept session was revoked")
	}
	if !revoked[other1] || !revoked[other2] {
		t.Fatalf("revoked %v, want both of A's other sessions (%s, %s)", got.IDs, other1, other2)
	}
	if revoked[sb] {
		t.Fatal("B's session was revoked")
	}
	// The seeded session of A is also "other", so it is revoked too: count it.
	if !bytes.Equal(got.RefreshCT[other1], []byte("sealed-o1")) {
		t.Errorf("refresh ciphertext of other1 = %q", got.RefreshCT[other1])
	}
	if got.PushDeleted != 1 {
		t.Errorf("PushDeleted = %d, want 1 (only the revoked session's subscription)", got.PushDeleted)
	}
	if ok, _ := pushSessionOf(t, h, pOther.ID); ok {
		t.Error("the revoked session's subscription survived")
	}
	for name, id := range map[string]ulid.ULID{"the kept session's": pKeep.ID, "the NULL-session": pNone.ID, "B's": pB.ID} {
		if ok, _ := pushSessionOf(t, h, id); !ok {
			t.Errorf("%s push subscription was deleted", name)
		}
	}
	if _, ok, err := s.LookupSession(ctx, hashOf("ex-keep")); err != nil || !ok {
		t.Fatalf("the kept session no longer resolves: ok=%v err=%v", ok, err)
	}
	if _, ok, err := s.LookupSession(ctx, hashOf("ex-o1")); err != nil || ok {
		t.Fatalf("a revoked session still resolves: ok=%v err=%v", ok, err)
	}
	if _, ok, err := s.LookupSession(ctx, hashOf("ex-b")); err != nil || !ok {
		t.Fatalf("B's session was damaged: ok=%v err=%v", ok, err)
	}
}

func TestSessionLive(t *testing.T) {
	h := newHarness(t)
	ctx := context.Background()
	a, b := seedTwoUsers(t, h.super)
	s := h.appStore()
	aID, bID := mustULID(t, a.ID), mustULID(t, b.ID)
	const allowance = 12 * time.Hour

	live := func(user, id ulid.ULID, allow time.Duration) bool {
		t.Helper()
		var ok bool
		if err := s.UserTx(ctx, user, func(tx *UserTx) error {
			var err error
			ok, err = tx.SessionLive(ctx, id, allow, time.Now())
			return err
		}); err != nil {
			t.Fatalf("SessionLive: %v", err)
		}
		return ok
	}

	sess := createSession(t, s, aID, "live-a", nil)
	if !live(aID, sess, allowance) {
		t.Fatal("a fresh live session is not live (positive control)")
	}
	// Nothing is stamped by the check.
	var before, after time.Time
	_ = h.super.QueryRow(ctx, `SELECT last_seen_at FROM app.sessions WHERE id = $1`, sess.String()).Scan(&before)
	_ = live(aID, sess, allowance)
	_ = h.super.QueryRow(ctx, `SELECT last_seen_at FROM app.sessions WHERE id = $1`, sess.String()).Scan(&after)
	if !before.Equal(after) {
		t.Error("the re-check stamped last_seen_at")
	}

	// Another user's session id, and an unknown one.
	if live(bID, sess, allowance) {
		t.Error("B's check reports A's session live")
	}
	if live(aID, NewULID(), allowance) {
		t.Error("an unknown session is live")
	}

	// Past the allowance, and back inside a wider one.
	if _, err := h.super.Exec(ctx, `UPDATE app.sessions SET last_checked_at = now() - interval '13 hours' WHERE id = $1`, sess.String()); err != nil {
		t.Fatal(err)
	}
	if live(aID, sess, allowance) {
		t.Error("a session unconfirmed for 13 hours is live under a 12 hour allowance")
	}
	if !live(aID, sess, 14*time.Hour) {
		t.Error("the same session is not live under a 14 hour allowance")
	}
	if _, err := h.super.Exec(ctx, `UPDATE app.sessions SET last_checked_at = now() WHERE id = $1`, sess.String()); err != nil {
		t.Fatal(err)
	}

	// Disabled, then deleted user.
	for _, c := range []struct{ name, set, unset string }{
		{"disabled", "disabled_at = now()", "disabled_at = NULL"},
		{"deleted", "deleted_at = now()", "deleted_at = NULL"},
	} {
		if _, err := h.super.Exec(ctx, `UPDATE app.users SET `+c.set+` WHERE id = $1`, a.ID); err != nil {
			t.Fatal(err)
		}
		if live(aID, sess, allowance) {
			t.Errorf("a session of a %s user is live", c.name)
		}
		if _, err := h.super.Exec(ctx, `UPDATE app.users SET `+c.unset+` WHERE id = $1`, a.ID); err != nil {
			t.Fatal(err)
		}
		if !live(aID, sess, allowance) {
			t.Errorf("the session is not live again after the %s flag is cleared (the check did not isolate the flag)", c.name)
		}
	}

	// Revoked.
	if err := s.UserTx(ctx, aID, func(tx *UserTx) error {
		_, err := tx.RevokeSession(ctx, sess, SessionRevokedSignedOut)
		return err
	}); err != nil {
		t.Fatal(err)
	}
	if live(aID, sess, allowance) {
		t.Error("a revoked session is live")
	}
}

func TestPushUpsertMovesTheSession(t *testing.T) {
	h := newHarness(t)
	a, b := seedTwoUsers(t, h.super)
	s := h.appStore()
	aID, bID := mustULID(t, a.ID), mustULID(t, b.ID)
	s1 := createSession(t, s, aID, "mv-1", nil)
	s2 := createSession(t, s, aID, "mv-2", nil)
	const endpoint = "https://push.example.com/moving"

	sessionOf := func(id ulid.ULID) string {
		t.Helper()
		ok, sess := pushSessionOf(t, h, id)
		if !ok {
			t.Fatal("subscription vanished")
		}
		if sess == nil {
			return "<null>"
		}
		return *sess
	}

	first := pushSub(t, s, aID, endpoint, &s1)
	if got := sessionOf(first.ID); got != s1.String() {
		t.Fatalf("session after the first registration = %s, want %s", got, s1)
	}
	again := pushSub(t, s, aID, endpoint, &s2)
	if again.ID != first.ID {
		t.Fatalf("re-registering the endpoint made a second row (%s vs %s)", again.ID, first.ID)
	}
	if got := sessionOf(first.ID); got != s2.String() {
		t.Fatalf("session after re-registration = %s, want it moved to %s", got, s2)
	}
	// Signing out the OLD session no longer takes the subscription with it.
	if err := s.UserTx(context.Background(), aID, func(tx *UserTx) error {
		r, err := tx.RevokeSessions(context.Background(), []ulid.ULID{s1}, SessionRevokedSignedOutElsewhere)
		if err == nil && r.PushDeleted != 0 {
			t.Errorf("revoking the old session deleted %d subscriptions", r.PushDeleted)
		}
		return err
	}); err != nil {
		t.Fatal(err)
	}
	if got := sessionOf(first.ID); got != s2.String() {
		t.Fatalf("session = %s after revoking the old one, want %s", got, s2)
	}
	// A registration with no session overwrites to NULL (dev-auth).
	pushSub(t, s, aID, endpoint, nil)
	if got := sessionOf(first.ID); got != "<null>" {
		t.Fatalf("session after a session-less registration = %s, want NULL", got)
	}
	// B registering the same endpoint is B's own row: A's binding is untouched.
	pushSub(t, s, aID, endpoint, &s2)
	bsess := createSession(t, s, bID, "mv-b", nil)
	bSub := pushSub(t, s, bID, endpoint, &bsess)
	if bSub.ID == first.ID {
		t.Fatal("B's registration reused A's row")
	}
	if got := sessionOf(first.ID); got != s2.String() {
		t.Fatalf("B's registration changed A's binding to %s", got)
	}
}

func TestSweepSessions(t *testing.T) {
	h := newHarness(t)
	ctx := context.Background()
	a, b := seedTwoUsers(t, h.super)
	s := h.appStore()
	aID, bID := mustULID(t, a.ID), mustULID(t, b.ID)

	// Start from a clean slate so the counts below are exactly this test's.
	if _, err := s.SweepSessions(ctx, time.Now()); err != nil {
		t.Fatalf("priming sweep: %v", err)
	}

	type row struct {
		name   string
		secret string
		user   ulid.ULID
		// revokedAgo: how long ago it was revoked ("" for live); reason with it.
		revokedAgo string
		reason     string
		seenAgo    string
		wantGone   bool
		wantReason string
	}
	rows := []row{
		{"revoked 8d signed_out", "sw-1", aID, "8 days", SessionRevokedSignedOut, "1 hour", true, "signed_out"},
		{"revoked 9d signed_out (second)", "sw-1b", bID, "9 days", SessionRevokedSignedOut, "1 hour", true, "signed_out"},
		{"revoked 8d refused", "sw-2", aID, "8 days", SessionRevokedRefused, "1 hour", true, "refused"},
		{"revoked 8d replaced", "sw-3", aID, "8 days", SessionRevokedReplaced, "1 hour", true, "replaced"},
		{"revoked 8d expired", "sw-4", aID, "8 days", SessionRevokedExpired, "1 hour", true, "expired"},
		{"revoked 8d elsewhere", "sw-5", bID, "8 days", SessionRevokedSignedOutElsewhere, "1 hour", true, "signed_out_elsewhere"},
		{"revoked 6d: kept", "sw-6", aID, "6 days", SessionRevokedSignedOut, "1 hour", false, ""},
		{"live unseen 46d", "sw-7", aID, "", "", "46 days", true, "unseen"},
		{"live unseen 44d: kept", "sw-8", aID, "", "", "44 days", false, ""},
		{"live recent: kept", "sw-9", bID, "", "", "1 minute", false, ""},
		// Revoked long ago but seen recently is still swept as its REVOKED reason,
		// and a revoked row last seen 46 days ago is NOT counted "unseen".
		{"revoked 8d, last seen 46d ago", "sw-10", aID, "8 days", SessionRevokedSignedOut, "46 days", true, "signed_out"},
	}
	ids := make([]ulid.ULID, len(rows))
	for i, r := range rows {
		ids[i] = createSession(t, s, r.user, r.secret, nil)
		if r.revokedAgo != "" {
			if _, err := h.super.Exec(ctx,
				`UPDATE app.sessions SET revoked_at = now() - $2::interval, revoked_reason = $3 WHERE id = $1`,
				ids[i].String(), r.revokedAgo, r.reason); err != nil {
				t.Fatal(err)
			}
		}
		if _, err := h.super.Exec(ctx, `UPDATE app.sessions SET last_seen_at = now() - $2::interval WHERE id = $1`,
			ids[i].String(), r.seenAgo); err != nil {
			t.Fatal(err)
		}
	}
	// Push subscriptions: those of swept sessions go WITH them (never unbound and
	// still receiving), counted apart; those of kept sessions and of no session stay.
	revokedOld, unseenOld := ids[0], ids[7] // "revoked 8d signed_out", "live unseen 46d"
	keptLive, keptRecent := ids[8], ids[9]  // "live unseen 44d", "live recent"
	psubRevoked := pushSub(t, s, aID, "https://push.example.com/swept-revoked", &revokedOld)
	psubUnseen := pushSub(t, s, aID, "https://push.example.com/swept-unseen", &unseenOld)
	psubKept := pushSub(t, s, aID, "https://push.example.com/kept-44d", &keptLive)
	psubRecent := pushSub(t, s, bID, "https://push.example.com/kept-recent", &keptRecent)
	psubNone := pushSub(t, s, aID, "https://push.example.com/no-session", nil)

	counts, err := s.SweepSessions(ctx, time.Now())
	if err != nil {
		t.Fatalf("SweepSessions: %v", err)
	}

	want := map[string]int64{"push_unseen": 1, "push_revoked": 1}
	for _, r := range rows {
		if r.wantGone {
			want[r.wantReason]++
		}
	}
	// EVERY reason is reported, zero included: a missing key must not read as
	// "nothing to do".
	if len(counts) != len(SweepReasons) {
		t.Errorf("counts has %d keys (%v), want all %d reasons", len(counts), counts, len(SweepReasons))
	}
	for _, reason := range SweepReasons {
		n, present := counts[reason]
		if !present {
			t.Errorf("reason %q is missing from the report", reason)
		}
		if n != want[reason] {
			t.Errorf("deleted %q = %d, want %d", reason, n, want[reason])
		}
	}
	for i, r := range rows {
		var exists bool
		if err := h.super.QueryRow(ctx, `SELECT EXISTS (SELECT 1 FROM app.sessions WHERE id = $1)`, ids[i].String()).Scan(&exists); err != nil {
			t.Fatal(err)
		}
		if exists == r.wantGone {
			t.Errorf("%s: row exists = %v, want gone = %v", r.name, exists, r.wantGone)
		}
	}
	for name, id := range map[string]ulid.ULID{"a swept unseen session's": psubUnseen.ID, "a swept revoked session's": psubRevoked.ID} {
		if ok, _ := pushSessionOf(t, h, id); ok {
			t.Errorf("%s push subscription survived the sweep (or was NULLed and kept receiving)", name)
		}
	}
	for name, id := range map[string]ulid.ULID{"a kept 44d session's": psubKept.ID, "a recent session's": psubRecent.ID, "the NULL-session": psubNone.ID} {
		if ok, _ := pushSessionOf(t, h, id); !ok {
			t.Errorf("%s push subscription was deleted by the sweep", name)
		}
	}

	// A second run finds nothing, and says so per reason.
	again, err := s.SweepSessions(ctx, time.Now())
	if err != nil {
		t.Fatal(err)
	}
	for reason, n := range again {
		if n != 0 {
			t.Errorf("a second sweep deleted %d %q", n, reason)
		}
	}
	if len(again) != len(SweepReasons) {
		t.Errorf("a quiet sweep reported %d reasons, want all %d", len(again), len(SweepReasons))
	}
}

// The structural half of the sweep probe: the owner policies exist, and the
// SELECT one is what lets the DELETE see anything. TestSweepSessions is the
// behavioural half (with the SELECT policy removed it goes red: every count is 0
// and every row survives, a zero-row "success").
func TestSweepSessionsOwnerPolicies(t *testing.T) {
	h := newHarness(t)
	ctx := context.Background()
	seen := map[string]string{}
	rows, err := h.super.Query(ctx, `
		SELECT policyname, cmd FROM pg_policies
		 WHERE schemaname = 'app' AND tablename = 'sessions' AND 'mailroom_owner' = ANY (roles)`)
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	for rows.Next() {
		var name, cmd string
		if err := rows.Scan(&name, &cmd); err != nil {
			t.Fatal(err)
		}
		seen[name] = cmd
	}
	if seen["sessions_sweep_select"] != "SELECT" {
		t.Errorf("sessions_sweep_select is %q, want a SELECT policy for mailroom_owner: without it the sweep's DELETE is a silent zero-row success", seen["sessions_sweep_select"])
	}
	if seen["sessions_sweep_delete"] != "DELETE" {
		t.Errorf("sessions_sweep_delete is %q, want a DELETE policy for mailroom_owner", seen["sessions_sweep_delete"])
	}
	rows2, err := h.super.Query(ctx, `
		SELECT policyname, cmd FROM pg_policies
		 WHERE schemaname = 'app' AND tablename = 'push_subscriptions' AND 'mailroom_owner' = ANY (roles)`)
	if err != nil {
		t.Fatal(err)
	}
	defer rows2.Close()
	pushSeen := map[string]string{}
	for rows2.Next() {
		var name, cmd string
		if err := rows2.Scan(&name, &cmd); err != nil {
			t.Fatal(err)
		}
		pushSeen[name] = cmd
	}
	if pushSeen["push_subscriptions_sweep_select"] != "SELECT" || pushSeen["push_subscriptions_sweep_delete"] != "DELETE" {
		t.Errorf("push_subscriptions owner policies = %v, want a sweep SELECT and a sweep DELETE", pushSeen)
	}
	// The app role cannot delete sessions directly, whatever the policies say.
	if _, err := h.app.Exec(ctx, `DELETE FROM app.sessions`); err == nil {
		t.Error("mailroom_app can DELETE from app.sessions directly")
	}
}

// RevokeSession, whatever the reason, deletes the push subscriptions registered
// from THAT session and nothing else: another session's and a NULL-session
// subscription survive. (Logout, a renewal refusal, an expiry and a replaced
// sign-in all end here.)
func TestRevokeSessionDeletesItsPushForEveryReason(t *testing.T) {
	h := newHarness(t)
	ctx := context.Background()
	a, _ := seedTwoUsers(t, h.super)
	s := h.appStore()
	aID := mustULID(t, a.ID)
	other := createSession(t, s, aID, "rs-other", nil)
	pOther := pushSub(t, s, aID, "https://push.example.com/rs-other", &other)
	pNone := pushSub(t, s, aID, "https://push.example.com/rs-none", nil)

	for _, reason := range []string{SessionRevokedSignedOut, SessionRevokedRefused, SessionRevokedExpired,
		SessionRevokedReplaced, SessionRevokedSignedOutElsewhere} {
		sess := createSession(t, s, aID, "rs-"+reason, nil)
		ps := pushSub(t, s, aID, "https://push.example.com/rs-"+reason, &sess)
		if ok, _ := pushSessionOf(t, h, ps.ID); !ok {
			t.Fatalf("%s: subscription not created", reason)
		}
		if err := s.UserTx(ctx, aID, func(tx *UserTx) error {
			ok, err := tx.RevokeSession(ctx, sess, reason)
			if err == nil && !ok {
				t.Errorf("%s: nothing revoked", reason)
			}
			return err
		}); err != nil {
			t.Fatal(err)
		}
		if ok, _ := pushSessionOf(t, h, ps.ID); ok {
			t.Errorf("%s: the revoked session's push subscription survived", reason)
		}
		for name, id := range map[string]ulid.ULID{"another session's": pOther.ID, "the NULL-session": pNone.ID} {
			if ok, _ := pushSessionOf(t, h, id); !ok {
				t.Errorf("%s: %s push subscription was deleted", reason, name)
			}
		}
	}

	// B cannot delete A's subscriptions by revoking A's session id.
	_, b := seedTwoUsers(t, h.super)
	bID := mustULID(t, b.ID)
	victim := createSession(t, s, aID, "rs-victim", nil)
	pVictim := pushSub(t, s, aID, "https://push.example.com/rs-victim", &victim)
	if err := s.UserTx(ctx, bID, func(tx *UserTx) error {
		_, err := tx.RevokeSession(ctx, victim, SessionRevokedSignedOut)
		return err
	}); err != nil {
		t.Fatal(err)
	}
	if ok, _ := pushSessionOf(t, h, pVictim.ID); !ok {
		t.Error("B's RevokeSession of A's session id deleted A's push subscription")
	}
}
