// SPDX-License-Identifier: AGPL-3.0-only

//go:build integration

package store

import (
	"context"
	"crypto/sha256"
	"errors"
	"fmt"
	"sort"
	"strings"
	"testing"
	"time"

	"github.com/oklog/ulid/v2"
)

type fuSeed struct {
	t      *testing.T
	h      *harness
	u      fixtureUser
	me     string
	now    time.Time
	counts int
}

func (s *fuSeed) exec(sql string, args ...any) {
	s.t.Helper()
	if _, err := s.h.super.Exec(context.Background(), sql, args...); err != nil {
		s.t.Fatalf("seed: %s: %v", strings.SplitN(strings.TrimSpace(sql), "\n", 2)[0], err)
	}
}

func (s *fuSeed) thread() string {
	id := NewULID().String()
	s.exec(`INSERT INTO app.threads (id, user_id) VALUES ($1,$2)`, id, s.u.ID)
	return id
}

// msg seeds one message; extra is "col = value" assignments applied after.
func (s *fuSeed) msg(thread, from string, daysAgo float64, extra string, args ...any) string {
	s.t.Helper()
	s.counts++
	id := NewULID().String()
	hash := sha256.Sum256([]byte(id))
	s.exec(`INSERT INTO app.blobs (user_id, content_hash, size_plain, size_stored, dek_id) VALUES ($1,$2,1,1,$3)`,
		s.u.ID, hash[:], NewULID().String())
	var thr any
	if thread != "" {
		thr = thread
	}
	s.exec(`INSERT INTO app.messages (id, user_id, content_hash, thread_id, from_address, received_at, size_bytes, message_id_hint)
	        VALUES ($1,$2,$3,$4,$5,$6,1,$7)`,
		id, s.u.ID, hash[:], thr, from, s.now.Add(-time.Duration(daysAgo*float64(24*time.Hour))), "<"+id+"@x>")
	if extra != "" {
		s.exec(`UPDATE app.messages SET `+extra+` WHERE id = $1`, append([]any{id}, args...)...)
	}
	return id
}

func fuIDs(ms []NudgeMessage) string {
	out := make([]string, 0, len(ms))
	for _, m := range ms {
		out = append(out, m.ID.String())
	}
	sort.Strings(out)
	return fmt.Sprint(out)
}

func fuWant(xs ...string) string {
	sort.Strings(xs)
	return fmt.Sprint(xs)
}

// FollowUpMessages (#58): exactly the candidates, their conversations and —
// only for the inbound rule — the sent-mail history; never another user's.
func TestFollowUpMessagesExact(t *testing.T) {
	h := newHarness(t)
	ctx := context.Background()
	a, b := seedTwoUsers(t, h.super)
	s := &fuSeed{t: t, h: h, u: a, me: strings.ToLower(a.Tag) + "@example.com", now: time.Now().UTC()}

	t1, t2 := s.thread(), s.thread()
	o1 := s.msg(t1, s.me, 10, "")                // outbound candidate
	r1 := s.msg(t1, "friend@example.org", 9, "") // its answer (related)
	d1 := s.msg(t1, s.me, 8, "")                 // dismissed: related, never a candidate
	s.exec(`INSERT INTO app.nudge_dismissals (user_id, message_id, kind) VALUES ($1,$2,'outbound')`, a.ID, d1)
	c1 := s.msg(t2, "other@example.org", 9, "in_reply_to = $2", "<"+o1+"@x>") // In-Reply-To child, other thread
	i1 := s.msg("", "friend@example.org", 5, "")                              // inbound candidate
	_ = s.msg("", "news@example.org", 5, "list_unsubscribe = '<mailto:u@example.org>'")
	_ = s.msg("", "friend@example.org", 5, "is_answered = true")
	snoozed := s.msg("", s.me, 10, "snoozed_until = $2", s.now.Add(time.Hour))
	woke := s.msg("", s.me, 10, "snoozed_until = $2", s.now.Add(-time.Hour))
	trashed := s.msg("", s.me, 10, "trashed_at = now()")
	old := s.msg("", s.me, 40, "")
	_ = s.msg("", s.me, 10, "is_draft = true")
	hist := s.msg("", s.me, 200, "")

	read := func(q FollowUpQuery) FollowUpSet {
		t.Helper()
		var set FollowUpSet
		if err := h.appStore().UserTx(ctx, mustULID(t, a.ID), func(tx *UserTx) error {
			var e error
			set, e = tx.FollowUpMessages(ctx, q)
			return e
		}); err != nil {
			t.Fatalf("FollowUpMessages: %v", err)
		}
		return set
	}

	out := read(FollowUpQuery{Now: s.now, MinAge: 7 * 24 * time.Hour})
	if got, want := fuIDs(out.Messages), fuWant(o1, r1, d1, c1, woke); got != want {
		t.Fatalf("outbound only: %s, want %s", got, want)
	}
	if len(out.Dismissed) != 1 || !out.Dismissed[mustULID(t, d1)] || len(out.Truncated) != 0 {
		t.Fatalf("dismissed %v truncated %v", out.Dismissed, out.Truncated)
	}
	var cands []string
	for id := range out.Candidates {
		cands = append(cands, id.String())
	}
	if got, want := fuWant(cands...), fuWant(o1, woke); got != want {
		t.Fatalf("candidates %s, want %s (the rest is context)", got, want)
	}

	both := read(FollowUpQuery{Now: s.now, MinAge: 3 * 24 * time.Hour, Inbound: true})
	// The sent-mail history is context: it includes the snoozed, trashed
	// and month-old sent mail, which are still mail the user wrote — and
	// which are NOT candidates.
	if got, want := fuIDs(both.Messages), fuWant(o1, r1, d1, c1, woke, i1, hist, snoozed, trashed, old); got != want {
		t.Fatalf("with inbound: %s, want %s", got, want)
	}
	cands = cands[:0]
	for id := range both.Candidates {
		cands = append(cands, id.String())
	}
	if got, want := fuWant(cands...), fuWant(o1, woke, i1, r1, c1); got != want { // r1, c1: inbound from people, in the window
		t.Fatalf("candidates with inbound %s, want %s", got, want)
	}

	// RLS: B reads none of A's mail (B's own seed message is not a candidate:
	// it is from a stranger and received now).
	var bset FollowUpSet
	if err := h.appStore().UserTx(ctx, mustULID(t, b.ID), func(tx *UserTx) error {
		var e error
		bset, e = tx.FollowUpMessages(ctx, FollowUpQuery{Now: s.now, MinAge: 0, Inbound: true})
		return e
	}); err != nil {
		t.Fatalf("B: %v", err)
	}
	for _, m := range bset.Messages {
		if m.ID.String() != b.MessageID {
			t.Fatalf("B read a message that is not its own: %s", m.ID)
		}
	}
}

// DismissNudge: one Email change row for a real change, none for a no-op,
// and another user's message is not found and untouched.
func TestDismissNudgeChangeRowsAndIsolation(t *testing.T) {
	h := newHarness(t)
	ctx := context.Background()
	a, b := seedTwoUsers(t, h.super)
	s := &fuSeed{t: t, h: h, u: a, me: strings.ToLower(a.Tag) + "@example.com", now: time.Now().UTC()}
	m := s.msg("", s.me, 10, "")
	st := h.appStore()

	dismiss := func(as string, id string, kind string) (bool, bool, error) {
		var found, changed bool
		err := st.UserTx(ctx, mustULID(t, as), func(tx *UserTx) error {
			var e error
			found, changed, e = tx.DismissNudge(ctx, mustULID(t, id), kind)
			return e
		})
		return found, changed, err
	}
	before := countChanges(t, h, a.ID, "Email")
	if f, c, err := dismiss(a.ID, m, "outbound"); err != nil || !f || !c {
		t.Fatalf("first dismiss: found %v changed %v err %v", f, c, err)
	}
	if n := countChanges(t, h, a.ID, "Email") - before; n != 1 {
		t.Fatalf("a dismissal appended %d Email change rows, want exactly 1", n)
	}
	if f, c, err := dismiss(a.ID, m, "outbound"); err != nil || !f || c {
		t.Fatalf("repeat dismiss: found %v changed %v err %v (want a no-op)", f, c, err)
	}
	if n := countChanges(t, h, a.ID, "Email") - before; n != 1 {
		t.Fatalf("a no-op dismissal appended a change row (%d total)", n)
	}
	if _, _, err := dismiss(a.ID, m, "sideways"); !errors.Is(err, ErrNudgeKind) {
		t.Fatalf("bad kind: %v", err)
	}

	// B dismissing A's message: not found, nothing written for anyone.
	bBefore := countChanges(t, h, b.ID, "Email")
	if f, c, err := dismiss(b.ID, m, "outbound"); err != nil || f || c {
		t.Fatalf("cross-user dismiss: found %v changed %v err %v", f, c, err)
	}
	var rows int
	if err := h.super.QueryRow(ctx, `SELECT count(*) FROM app.nudge_dismissals WHERE user_id = $1`, b.ID).Scan(&rows); err != nil {
		t.Fatal(err)
	}
	if rows != 1 || countChanges(t, h, b.ID, "Email") != bBefore {
		t.Fatalf("cross-user dismiss wrote something: B has %d dismissals (seed has 1)", rows)
	}
}

// The follow-up settings on desk preferences: defaults, one change row per
// real change, none for a no-op, and the CHECK on the delay.
func TestDeskPreferencesFollowUp(t *testing.T) {
	h := newHarness(t)
	ctx := context.Background()
	a, _ := seedTwoUsers(t, h.super)
	st := h.appStore()
	uid := mustULID(t, a.ID)

	var p DeskPreferences
	if err := st.UserTx(ctx, uid, func(tx *UserTx) error {
		var e error
		p, e = tx.GetDeskPreferences(ctx)
		return e
	}); err != nil {
		t.Fatal(err)
	}
	if p.FollowUp != (FollowUpSettings{Outbound: true, Inbound: false, InboundDays: 3}) {
		t.Fatalf("defaults = %+v, want outbound on, inbound off, 3 days", p.FollowUp)
	}
	update := func(f FollowUpSettings) error {
		return st.UserTx(ctx, uid, func(tx *UserTx) error { return tx.UpdateDeskPreferences(ctx, DeskPreferencesPatch{FollowUp: &f}) })
	}
	before := countChanges(t, h, a.ID, "DeskPreferences")
	want := FollowUpSettings{Outbound: false, Inbound: true, InboundDays: 7}
	if err := update(want); err != nil {
		t.Fatal(err)
	}
	if err := update(want); err != nil {
		t.Fatal(err)
	}
	if n := countChanges(t, h, a.ID, "DeskPreferences") - before; n != 1 {
		t.Fatalf("one change and one no-op appended %d change rows, want 1", n)
	}
	if err := update(FollowUpSettings{Outbound: true, InboundDays: 0}); !errors.Is(err, ErrDeskPreferencesInvalid) {
		t.Fatalf("inboundDays 0: %v, want ErrDeskPreferencesInvalid", err)
	}
	if err := st.UserTx(ctx, uid, func(tx *UserTx) error {
		var e error
		p, e = tx.GetDeskPreferences(ctx)
		return e
	}); err != nil {
		t.Fatal(err)
	}
	if p.FollowUp != want {
		t.Fatalf("read back %+v, want %+v", p.FollowUp, want)
	}
	_ = ulid.ULID{}
}
