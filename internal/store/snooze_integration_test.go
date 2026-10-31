// SPDX-License-Identifier: AGPL-3.0-only

//go:build integration

package store

// Integration coverage for snooze (spec §6, mailroom#51) against a real
// PostgreSQL with RLS enforced, connecting as mailroom_app.
//
// Snooze is mail-timing-adjacent: get it wrong and a message the user asked to
// see at 9am is either never seen again or comes back at the wrong time. So the
// tests here are written around the three claims the implementation actually
// makes, rather than around its functions:
//
//  1. SETTING a snooze commits the state and its wake TIMER together, and
//     appends exactly one Email change_log row — not two, not none.
//  2. WAKING is idempotent, because a durable job may fire more than once.
//  3. "Snoozed" is a question about the CLOCK, not about a column being
//     non-null — a message whose wake time has passed is already back, whether
//     or not the timer has run yet.

import (
	"context"
	"crypto/sha256"
	"testing"
	"time"

	"github.com/oklog/ulid/v2"
)

type snoozeFixture struct {
	h      *harness
	st     *Store
	userID ulid.ULID
}

func newSnoozeFixture(t *testing.T) *snoozeFixture {
	t.Helper()
	h := newHarness(t)
	f := &snoozeFixture{h: h, st: h.appStore(), userID: NewULID()}
	if _, err := h.super.Exec(context.Background(),
		`INSERT INTO app.users (id, oidc_subject, email) VALUES ($1, $2, $3)`,
		f.userID.String(), "sub-"+f.userID.String(), f.userID.String()+"@example.com"); err != nil {
		t.Fatalf("seed user: %v", err)
	}
	return f
}

// seedMessage inserts a canonical message (plus the blob and thread its foreign
// keys require) and returns its id.
func (f *snoozeFixture) seedMessage(t *testing.T, subject string) ulid.ULID {
	t.Helper()
	ctx := context.Background()
	id := NewULID()
	hash := sha256.Sum256([]byte(subject + id.String()))
	threadID := NewULID()
	exec := func(sql string, args ...any) {
		t.Helper()
		if _, err := f.h.super.Exec(ctx, sql, args...); err != nil {
			t.Fatalf("seed message: %v", err)
		}
	}
	exec(`INSERT INTO app.blobs (user_id, content_hash, size_plain, size_stored, dek_id)
	      VALUES ($1, $2, 10, 20, $3)`, f.userID.String(), hash[:], NewULID().String())
	exec(`INSERT INTO app.threads (id, user_id, subject) VALUES ($1, $2, $3)`,
		threadID.String(), f.userID.String(), subject)
	exec(`INSERT INTO app.messages (id, user_id, content_hash, thread_id, subject, received_at, size_bytes)
	      VALUES ($1, $2, $3, $4, $5, now(), 10)`,
		id.String(), f.userID.String(), hash[:], threadID.String(), subject)
	return id
}

// apply runs one Email/set-shaped update through the audited path.
func (f *snoozeFixture) apply(t *testing.T, id ulid.ULID, up EmailUpdate) {
	t.Helper()
	ctx := context.Background()
	if err := f.st.UserTx(ctx, f.userID, func(tx *UserTx) error {
		found, _, err := tx.ApplyEmailUpdate(ctx, id, up)
		if err != nil {
			return err
		}
		if !found {
			t.Fatalf("message %s not found by ApplyEmailUpdate", id)
		}
		return nil
	}); err != nil {
		t.Fatalf("apply email update: %v", err)
	}
}

// snoozedUntil reads the raw column, as the superuser: the assertions are about
// what is actually stored, not about what a projection reports.
func (f *snoozeFixture) snoozedUntil(t *testing.T, id ulid.ULID) *time.Time {
	t.Helper()
	var at *time.Time
	if err := f.h.super.QueryRow(context.Background(),
		`SELECT snoozed_until FROM app.messages WHERE user_id = $1 AND id = $2`,
		f.userID.String(), id.String()).Scan(&at); err != nil {
		t.Fatalf("read snoozed_until: %v", err)
	}
	return at
}

func (f *snoozeFixture) emailChanges(t *testing.T, id ulid.ULID) int {
	t.Helper()
	var n int
	if err := f.h.super.QueryRow(context.Background(),
		`SELECT count(*) FROM app.change_log
		  WHERE user_id = $1 AND object_type = 'Email' AND object_id = $2`,
		f.userID.String(), id.String()).Scan(&n); err != nil {
		t.Fatalf("count Email changes: %v", err)
	}
	return n
}

// wakeJobs returns this user's pending snooze_wake jobs, earliest first.
func (f *snoozeFixture) wakeJobs(t *testing.T) []time.Time {
	t.Helper()
	rows, err := f.h.super.Query(context.Background(),
		`SELECT fire_at FROM app.jobs
		  WHERE user_id = $1 AND kind = $2 ORDER BY fire_at`,
		f.userID.String(), SnoozeJobKind)
	if err != nil {
		t.Fatalf("read jobs: %v", err)
	}
	defer rows.Close()
	var out []time.Time
	for rows.Next() {
		var at time.Time
		if err := rows.Scan(&at); err != nil {
			t.Fatalf("scan job: %v", err)
		}
		out = append(out, at)
	}
	return out
}

func (f *snoozeFixture) wake(t *testing.T, now time.Time) []ulid.ULID {
	t.Helper()
	ctx := context.Background()
	var woke []ulid.ULID
	if err := f.st.UserTx(ctx, f.userID, func(tx *UserTx) error {
		var err error
		woke, err = tx.WakeSnoozed(ctx, now)
		return err
	}); err != nil {
		t.Fatalf("wake snoozed: %v", err)
	}
	return woke
}

func (f *snoozeFixture) query(t *testing.T, q EmailQuery) []ulid.ULID {
	t.Helper()
	ctx := context.Background()
	var ids []ulid.ULID
	if err := f.st.UserTx(ctx, f.userID, func(tx *UserTx) error {
		var err error
		ids, _, err = tx.QueryEmails(ctx, q)
		return err
	}); err != nil {
		t.Fatalf("query emails: %v", err)
	}
	return ids
}

// THE HEADLINE. A snooze is state AND a timer, and they must commit together: a
// message snoozed with no pending wake is a message that never comes back.
func TestSnoozeCommitsStateAndTimerTogether(t *testing.T) {
	f := newSnoozeFixture(t)
	msg := f.seedMessage(t, "read me tomorrow")
	wake := time.Now().Add(3 * time.Hour).UTC().Truncate(time.Second)

	f.apply(t, msg, EmailUpdate{SnoozeUntil: &wake})

	got := f.snoozedUntil(t, msg)
	if got == nil || !got.Equal(wake) {
		t.Fatalf("snoozed_until = %v, want %v", got, wake)
	}
	if n := f.emailChanges(t, msg); n != 1 {
		t.Errorf("Email change_log rows = %d, want exactly 1 — snooze is JMAP-visible "+
			"state, and the invariant is one row per mutation", n)
	}
	jobs := f.wakeJobs(t)
	if len(jobs) != 1 {
		t.Fatalf("pending wake jobs = %d, want 1 — a snooze with no timer never comes back", len(jobs))
	}
	// The job fires at the wake time, to the minute the idempotency key is
	// truncated to (a wake is not a deadline; a minute of slack is by design).
	if diff := jobs[0].Sub(wake); diff < -time.Minute || diff > time.Minute {
		t.Errorf("wake job fires at %v, want ~%v", jobs[0], wake)
	}
}

// Re-snoozing to the same instant must not churn the change log: every row
// wakes the indexer and every client's sync cursor. The TIMER, by contrast, is
// re-asserted deliberately — it is idempotent, and re-asserting it is how a lost
// one repairs itself.
func TestReSnoozeToTheSameTimeIsANoOp(t *testing.T) {
	f := newSnoozeFixture(t)
	msg := f.seedMessage(t, "same time twice")
	wake := time.Now().Add(2 * time.Hour).UTC().Truncate(time.Second)

	f.apply(t, msg, EmailUpdate{SnoozeUntil: &wake})
	after := f.emailChanges(t, msg)
	f.apply(t, msg, EmailUpdate{SnoozeUntil: &wake})

	if n := f.emailChanges(t, msg); n != after {
		t.Errorf("Email change_log rows went %d → %d on a no-op re-snooze", after, n)
	}
	if jobs := f.wakeJobs(t); len(jobs) != 1 {
		t.Errorf("pending wake jobs = %d, want 1 — the idempotency key should have collapsed the second", len(jobs))
	}
}

// Un-snoozing clears the column and IS a change: the message rejoined the inbox
// view, and every other device has to learn that.
func TestUnsnoozeClearsTheStateAndIsLogged(t *testing.T) {
	f := newSnoozeFixture(t)
	msg := f.seedMessage(t, "actually, now")
	wake := time.Now().Add(time.Hour).UTC()

	f.apply(t, msg, EmailUpdate{SnoozeUntil: &wake})
	before := f.emailChanges(t, msg)
	f.apply(t, msg, EmailUpdate{Unsnooze: true})

	if got := f.snoozedUntil(t, msg); got != nil {
		t.Errorf("snoozed_until = %v after un-snooze, want NULL", got)
	}
	if n := f.emailChanges(t, msg); n != before+1 {
		t.Errorf("Email change_log rows = %d, want %d (exactly one more)", n, before+1)
	}
	// And un-snoozing again changes nothing at all.
	f.apply(t, msg, EmailUpdate{Unsnooze: true})
	if n := f.emailChanges(t, msg); n != before+1 {
		t.Errorf("a second un-snooze appended a row (%d → %d)", before+1, n)
	}
}

// A snooze rides in the SAME patch as a flag, and the whole update still appends
// exactly one Email change_log row. This is the case that would regress if the
// snooze were ever routed through a second write path.
func TestSnoozeInTheSamePatchAsAFlagAppendsOneRow(t *testing.T) {
	f := newSnoozeFixture(t)
	msg := f.seedMessage(t, "read and snoozed at once")
	wake := time.Now().Add(time.Hour).UTC()
	seen := true

	f.apply(t, msg, EmailUpdate{Seen: &seen, SnoozeUntil: &wake})

	if n := f.emailChanges(t, msg); n != 1 {
		t.Errorf("Email change_log rows = %d, want exactly 1 for one Email/set update", n)
	}
}

// A durable job can fire twice — a crash between the work and the completion
// write, or two dispatchers racing a batch. The second run must be a no-op
// rather than a second round of change_log rows.
func TestWakeSnoozedIsIdempotentAndWakesOnlyWhatIsDue(t *testing.T) {
	f := newSnoozeFixture(t)
	due := f.seedMessage(t, "due now")
	later := f.seedMessage(t, "due next week")

	past := time.Now().Add(-time.Minute).UTC()
	future := time.Now().Add(7 * 24 * time.Hour).UTC()
	// Set the past wake directly: the API refuses a wake time in the past, so
	// this is the state a message reaches by WAITING, not by being asked for.
	f.apply(t, due, EmailUpdate{SnoozeUntil: &future})
	if _, err := f.h.super.Exec(context.Background(),
		`UPDATE app.messages SET snoozed_until = $3 WHERE user_id = $1 AND id = $2`,
		f.userID.String(), due.String(), past); err != nil {
		t.Fatalf("backdate wake: %v", err)
	}
	f.apply(t, later, EmailUpdate{SnoozeUntil: &future})

	now := time.Now()
	woke := f.wake(t, now)
	if len(woke) != 1 || woke[0] != due {
		t.Fatalf("woke = %v, want exactly the due message %v", woke, due)
	}
	if got := f.snoozedUntil(t, due); got != nil {
		t.Errorf("due message still snoozed until %v", got)
	}
	if got := f.snoozedUntil(t, later); got == nil {
		t.Error("a message due next week was woken now")
	}
	changesAfter := f.emailChanges(t, due)

	// Fire again — the job's retry, or its double.
	if woke := f.wake(t, now); len(woke) != 0 {
		t.Errorf("second wake woke %v, want nothing", woke)
	}
	if n := f.emailChanges(t, due); n != changesAfter {
		t.Errorf("a repeat wake appended change_log rows (%d → %d): the job is not idempotent", changesAfter, n)
	}
}

// "Snoozed" is a question about the clock. A message whose wake time has passed
// but whose job has not run is ALREADY BACK in the inbox view — that is what
// makes the view correct even when the dispatcher is late.
func TestIsSnoozedFilterFollowsTheClockNotTheColumn(t *testing.T) {
	f := newSnoozeFixture(t)
	awake := f.seedMessage(t, "never snoozed")
	sleeping := f.seedMessage(t, "still asleep")
	overdue := f.seedMessage(t, "wake time passed, job has not run")

	future := time.Now().Add(6 * time.Hour).UTC()
	f.apply(t, sleeping, EmailUpdate{SnoozeUntil: &future})
	f.apply(t, overdue, EmailUpdate{SnoozeUntil: &future})
	if _, err := f.h.super.Exec(context.Background(),
		`UPDATE app.messages SET snoozed_until = now() - interval '1 minute'
		  WHERE user_id = $1 AND id = $2`,
		f.userID.String(), overdue.String()); err != nil {
		t.Fatalf("backdate wake: %v", err)
	}

	yes, no := true, false
	snoozed := f.query(t, EmailQuery{Snoozed: &yes})
	if len(snoozed) != 1 || snoozed[0] != sleeping {
		t.Errorf("isSnoozed:true = %v, want exactly the still-sleeping message %v "+
			"(an overdue message must not be listed as snoozed)", snoozed, sleeping)
	}

	visible := map[ulid.ULID]bool{}
	for _, id := range f.query(t, EmailQuery{Snoozed: &no}) {
		visible[id] = true
	}
	if !visible[awake] || !visible[overdue] {
		t.Errorf("isSnoozed:false = %v, want both the never-snoozed message and the overdue one", visible)
	}
	if visible[sleeping] {
		t.Error("a snoozed message is still showing in the inbox view")
	}
}

// THE POINT OF SNOOZING (#152). A message snoozed on Monday and woken on Friday
// has a Monday received_at, so a plain receivedAt sort returns it to the middle
// of the week's mail — where the user never sees it, which is the outcome
// snoozing exists to prevent. Ordering by effective arrival brings it back to
// the TOP, and does so WITHOUT rewriting received_at.
func TestWokenMailReturnsToTheTopWithoutRewritingReceivedAt(t *testing.T) {
	f := newSnoozeFixture(t)
	ctx := context.Background()

	// Three messages, oldest first. The oldest is the one that gets snoozed.
	old := f.seedMessage(t, "from last week")
	mid := f.seedMessage(t, "from yesterday")
	recent := f.seedMessage(t, "this morning")
	for id, age := range map[ulid.ULID]string{
		old: "7 days", mid: "1 day", recent: "1 hour",
	} {
		if _, err := f.h.super.Exec(ctx,
			`UPDATE app.messages SET received_at = now() - $3::interval
			  WHERE user_id = $1 AND id = $2`,
			f.userID.String(), id.String(), age); err != nil {
			t.Fatalf("age message: %v", err)
		}
	}

	// Snooze the oldest, then wake it: it came back just now.
	wake := time.Now().Add(time.Hour).UTC()
	f.apply(t, old, EmailUpdate{SnoozeUntil: &wake})
	if _, err := f.h.super.Exec(ctx,
		`UPDATE app.messages SET snoozed_until = now() - interval '1 minute'
		  WHERE user_id = $1 AND id = $2`,
		f.userID.String(), old.String()); err != nil {
		t.Fatalf("backdate wake: %v", err)
	}
	if woke := f.wake(t, time.Now()); len(woke) != 1 {
		t.Fatalf("woke %d messages, want 1", len(woke))
	}

	// Effective arrival: the returning message leads.
	got := f.query(t, EmailQuery{SortByEffectiveArrival: true})
	if len(got) != 3 || got[0] != old {
		t.Fatalf("effective-arrival order = %v, want the returned message %v first "+
			"(a snooze that returns mail to where its age puts it is no snooze at all)", got, old)
	}

	// received_at is UNTOUCHED, so the RFC sort still answers the question it was
	// asked — the returning message is back where its age puts it.
	byReceived := f.query(t, EmailQuery{})
	if len(byReceived) != 3 || byReceived[0] != recent || byReceived[2] != old {
		t.Fatalf("receivedAt order = %v, want [%v %v %v] — received_at must not be rewritten",
			byReceived, recent, mid, old)
	}
}

// Un-snoozing by hand is a return too: "bring it back" has to actually bring it
// back into view, not to a position three days down.
func TestManualUnsnoozeAlsoReturnsToTheTop(t *testing.T) {
	f := newSnoozeFixture(t)
	ctx := context.Background()

	old := f.seedMessage(t, "old but wanted now")
	recent := f.seedMessage(t, "arrived this morning")
	if _, err := f.h.super.Exec(ctx,
		`UPDATE app.messages SET received_at = now() - interval '9 days'
		  WHERE user_id = $1 AND id = $2`,
		f.userID.String(), old.String()); err != nil {
		t.Fatalf("age message: %v", err)
	}

	wake := time.Now().Add(48 * time.Hour).UTC()
	f.apply(t, old, EmailUpdate{SnoozeUntil: &wake})
	// Snoozing clears any previous return: it is away, not back.
	f.apply(t, old, EmailUpdate{Unsnooze: true})

	got := f.query(t, EmailQuery{SortByEffectiveArrival: true})
	if len(got) != 2 || got[0] != old {
		t.Errorf("order after bringing it back = %v, want %v first (then %v)", got, old, recent)
	}
}

// Snoozing again clears the return stamp — otherwise a message that is away
// would still carry a "came back" cue, and would sort by a return that has been
// superseded.
func TestReSnoozingClearsTheReturnStamp(t *testing.T) {
	f := newSnoozeFixture(t)
	ctx := context.Background()
	msg := f.seedMessage(t, "back and away again")

	wake := time.Now().Add(time.Hour).UTC()
	f.apply(t, msg, EmailUpdate{SnoozeUntil: &wake})
	f.apply(t, msg, EmailUpdate{Unsnooze: true})

	var wokeAt *time.Time
	read := func() {
		t.Helper()
		if err := f.h.super.QueryRow(ctx,
			`SELECT snooze_woke_at FROM app.messages WHERE user_id = $1 AND id = $2`,
			f.userID.String(), msg.String()).Scan(&wokeAt); err != nil {
			t.Fatalf("read snooze_woke_at: %v", err)
		}
	}
	read()
	if wokeAt == nil {
		t.Fatal("bringing a message back did not stamp when it came back")
	}

	later := time.Now().Add(6 * time.Hour).UTC()
	f.apply(t, msg, EmailUpdate{SnoozeUntil: &later})
	read()
	if wokeAt != nil {
		t.Errorf("snooze_woke_at = %v after re-snoozing, want NULL: the message is away again", wokeAt)
	}
}

// Every distinct wake time needs its own timer, but "tomorrow morning" applied
// to fifty messages must not produce fifty jobs. The minute-truncated
// idempotency key is what draws that line.
func TestWakeJobsCollapseByMinuteButNotAcrossTimes(t *testing.T) {
	f := newSnoozeFixture(t)
	base := time.Now().Add(time.Hour).UTC().Truncate(time.Minute)

	sameMinute := base.Add(10 * time.Second)
	otherMinute := base.Add(5 * time.Minute)
	for _, wake := range []time.Time{base, sameMinute, otherMinute} {
		w := wake
		f.apply(t, f.seedMessage(t, "batch member"), EmailUpdate{SnoozeUntil: &w})
	}

	jobs := f.wakeJobs(t)
	if len(jobs) != 2 {
		t.Fatalf("pending wake jobs = %d, want 2 — two messages in the same minute share a timer, "+
			"a third five minutes later needs its own", len(jobs))
	}
}

// The worker reschedules itself from the DATA, so it needs to know what is due
// next. Nothing pending must be reported as nothing pending, not as the zero
// time — otherwise a wake would be scheduled for the year 1.
func TestNextSnoozeWakeReportsTheEarliestPending(t *testing.T) {
	f := newSnoozeFixture(t)
	ctx := context.Background()

	next := func() (time.Time, bool) {
		t.Helper()
		var (
			at   time.Time
			more bool
		)
		if err := f.st.UserTx(ctx, f.userID, func(tx *UserTx) error {
			var err error
			at, more, err = tx.NextSnoozeWake(ctx)
			return err
		}); err != nil {
			t.Fatalf("next snooze wake: %v", err)
		}
		return at, more
	}

	if _, more := next(); more {
		t.Fatal("NextSnoozeWake reported a pending wake with nothing snoozed")
	}

	soon := time.Now().Add(time.Hour).UTC().Truncate(time.Second)
	late := time.Now().Add(48 * time.Hour).UTC().Truncate(time.Second)
	early := f.seedMessage(t, "back in an hour")
	f.apply(t, early, EmailUpdate{SnoozeUntil: &soon})
	f.apply(t, f.seedMessage(t, "back in two days"), EmailUpdate{SnoozeUntil: &late})

	at, more := next()
	if !more || !at.Equal(soon) {
		t.Fatalf("NextSnoozeWake = %v,%v want %v,true", at, more, soon)
	}

	// Once the earliest is woken, the next one takes its place — the property the
	// worker relies on to chain a long queue of snoozes off one timer at a time.
	f.apply(t, early, EmailUpdate{Unsnooze: true})
	at, more = next()
	if !more || !at.Equal(late) {
		t.Fatalf("after un-snoozing the earliest: NextSnoozeWake = %v,%v want %v,true", at, more, late)
	}
}
