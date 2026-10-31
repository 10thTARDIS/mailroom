// SPDX-License-Identifier: AGPL-3.0-only

//go:build integration

package snooze

// Integration coverage for the snooze wake worker against a real PostgreSQL
// (connecting as mailroom_app, RLS enforced).
//
// The store tests prove the QUERY is idempotent and clock-driven. What is left
// to prove here is the part the worker itself owns, and it is the part that
// makes a long snooze work at all: EACH FIRE SCHEDULES THE NEXT ONE FROM THE
// DATA. Without that, mail snoozed past the first wake in the queue sleeps
// forever; with it, one timer at a time walks the whole queue and a lost timer
// repairs itself.

import (
	"context"
	"crypto/sha256"
	"io"
	"log/slog"
	"os"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/oklog/ulid/v2"

	"mailroom/internal/store"
)

type wakeHarness struct {
	super  *pgxpool.Pool
	st     *store.Store
	userID ulid.ULID
}

func newWakeHarness(t *testing.T) *wakeHarness {
	t.Helper()
	superURL := os.Getenv("MAILROOM_TEST_SUPER_URL")
	appURL := os.Getenv("MAILROOM_TEST_APP_URL")
	if superURL == "" || appURL == "" {
		t.Skip("integration database not configured — run via scripts/integration-test.sh")
	}
	ctx := context.Background()
	super, err := pgxpool.New(ctx, superURL)
	if err != nil {
		t.Fatalf("open super pool: %v", err)
	}
	t.Cleanup(super.Close)
	app, err := pgxpool.New(ctx, appURL)
	if err != nil {
		t.Fatalf("open app pool: %v", err)
	}
	t.Cleanup(app.Close)

	h := &wakeHarness{super: super, st: store.NewFromPool(app), userID: store.NewULID()}
	if _, err := super.Exec(ctx,
		`INSERT INTO app.users (id, oidc_subject, email) VALUES ($1, $2, $3)`,
		h.userID.String(), "sub-"+h.userID.String(), h.userID.String()+"@example.com"); err != nil {
		t.Fatalf("seed user: %v", err)
	}
	return h
}

// seedSnoozed inserts a message already snoozed until wake. It writes the column
// directly rather than going through Email/set, because these tests are about
// what the worker does with mail that IS snoozed — including mail snoozed to a
// time the API would refuse to set now.
func (h *wakeHarness) seedSnoozed(t *testing.T, subject string, wake time.Time) ulid.ULID {
	t.Helper()
	ctx := context.Background()
	id := store.NewULID()
	sum := sha256.Sum256([]byte(subject + id.String()))
	threadID := store.NewULID()
	exec := func(sql string, args ...any) {
		t.Helper()
		if _, err := h.super.Exec(ctx, sql, args...); err != nil {
			t.Fatalf("seed message: %v", err)
		}
	}
	exec(`INSERT INTO app.blobs (user_id, content_hash, size_plain, size_stored, dek_id)
	      VALUES ($1, $2, 10, 20, $3)`, h.userID.String(), sum[:], store.NewULID().String())
	exec(`INSERT INTO app.threads (id, user_id, subject) VALUES ($1, $2, $3)`,
		threadID.String(), h.userID.String(), subject)
	exec(`INSERT INTO app.messages (id, user_id, content_hash, thread_id, subject, received_at,
	                                size_bytes, snoozed_until)
	      VALUES ($1, $2, $3, $4, $5, now(), 10, $6)`,
		id.String(), h.userID.String(), sum[:], threadID.String(), subject, wake.UTC())
	return id
}

func (h *wakeHarness) snoozedUntil(t *testing.T, id ulid.ULID) *time.Time {
	t.Helper()
	var at *time.Time
	if err := h.super.QueryRow(context.Background(),
		`SELECT snoozed_until FROM app.messages WHERE user_id = $1 AND id = $2`,
		h.userID.String(), id.String()).Scan(&at); err != nil {
		t.Fatalf("read snoozed_until: %v", err)
	}
	return at
}

func (h *wakeHarness) wakeJobs(t *testing.T) []time.Time {
	t.Helper()
	rows, err := h.super.Query(context.Background(),
		`SELECT fire_at FROM app.jobs WHERE user_id = $1 AND kind = $2 ORDER BY fire_at`,
		h.userID.String(), store.SnoozeJobKind)
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

// job builds the durable job the dispatcher would hand the worker.
func (h *wakeHarness) job() store.Job {
	uid := h.userID
	return store.Job{ID: store.NewULID(), UserID: &uid, Kind: store.SnoozeJobKind, FireAt: time.Now()}
}

func quietLogger() *slog.Logger {
	return slog.New(slog.NewTextHandler(io.Discard, nil))
}

// THE HEADLINE. One fire wakes what is due and leaves a timer for what is not —
// so a queue of snoozes at different times costs one pending timer at a time,
// and nothing further out is forgotten.
func TestHandleWakesDueMailAndSchedulesTheNext(t *testing.T) {
	h := newWakeHarness(t)
	due := h.seedSnoozed(t, "due", time.Now().Add(-time.Minute))
	next := time.Now().Add(4 * time.Hour).UTC().Truncate(time.Second)
	later := h.seedSnoozed(t, "later", next)

	w := NewWorker(h.st, quietLogger())
	if err := w.Handle(context.Background(), h.job()); err != nil {
		t.Fatalf("handle: %v", err)
	}

	if at := h.snoozedUntil(t, due); at != nil {
		t.Errorf("due message still snoozed until %v", at)
	}
	if at := h.snoozedUntil(t, later); at == nil {
		t.Error("a message due in four hours was woken now")
	}
	jobs := h.wakeJobs(t)
	if len(jobs) != 1 {
		t.Fatalf("pending wake jobs after the fire = %d, want 1 — the chain is what makes "+
			"a long snooze cost one timer instead of a poll, and losing it strands the mail", len(jobs))
	}
	if diff := jobs[0].Sub(next); diff < -time.Minute || diff > time.Minute {
		t.Errorf("next wake scheduled for %v, want ~%v (the earliest still-pending wake)", jobs[0], next)
	}
}

// With nothing left snoozed, the chain ENDS. A worker that always rescheduled
// would turn one snooze into a timer that fires forever.
func TestHandleStopsTheChainWhenNothingIsPending(t *testing.T) {
	h := newWakeHarness(t)
	h.seedSnoozed(t, "the last one", time.Now().Add(-time.Minute))

	w := NewWorker(h.st, quietLogger())
	if err := w.Handle(context.Background(), h.job()); err != nil {
		t.Fatalf("handle: %v", err)
	}
	if jobs := h.wakeJobs(t); len(jobs) != 0 {
		t.Errorf("pending wake jobs = %d, want 0 — nothing is snoozed, so nothing should be timed", len(jobs))
	}
}

// A wake is inherently per-user (it walks that user's RLS-scoped mail), so a
// system-scoped job is a wiring bug. It must fail loudly enough for the
// dispatcher to dead-letter it rather than being silently treated as "nothing
// to do".
func TestHandleRejectsASystemScopedJob(t *testing.T) {
	h := newWakeHarness(t)
	w := NewWorker(h.st, quietLogger())
	job := h.job()
	job.UserID = nil
	if err := w.Handle(context.Background(), job); err == nil {
		t.Error("a system-scoped snooze_wake job was accepted; it should fail so the dispatcher dead-letters it")
	}
}

// The boot seed is the repair path for a timer the queue lost outright (a
// dropped job, or a restore from a backup predating the job row). Without it,
// mail snoozed at that moment would sleep forever.
func TestEnsureScheduledSeedsAndIsIdempotent(t *testing.T) {
	h := newWakeHarness(t)
	ctx := context.Background()

	// Nothing snoozed: nothing to schedule, and no error.
	scheduled, err := EnsureScheduled(ctx, h.st, h.userID)
	if err != nil {
		t.Fatalf("ensure scheduled (empty): %v", err)
	}
	if scheduled {
		t.Error("EnsureScheduled created a timer with no snoozed mail")
	}

	wake := time.Now().Add(2 * time.Hour).UTC().Truncate(time.Second)
	h.seedSnoozed(t, "stranded by a lost timer", wake)

	if scheduled, err = EnsureScheduled(ctx, h.st, h.userID); err != nil || !scheduled {
		t.Fatalf("EnsureScheduled = %v,%v want true,nil — snoozed mail with no timer is exactly "+
			"the case this repairs", scheduled, err)
	}
	// Running it again (every boot does) must not pile up duplicates.
	if scheduled, err = EnsureScheduled(ctx, h.st, h.userID); err != nil {
		t.Fatalf("ensure scheduled (repeat): %v", err)
	} else if scheduled {
		t.Error("a second EnsureScheduled inserted another timer for the same wake")
	}
	if jobs := h.wakeJobs(t); len(jobs) != 1 {
		t.Errorf("pending wake jobs = %d, want 1", len(jobs))
	}
}

// The clock seam exists so a test can drive the boundary without sleeping. A
// message due one second from now is NOT woken by a pass that runs now.
func TestHandleHonorsTheInjectedClock(t *testing.T) {
	h := newWakeHarness(t)
	wake := time.Now().Add(time.Hour)
	msg := h.seedSnoozed(t, "an hour out", wake)

	w := NewWorker(h.st, quietLogger()).WithClock(func() time.Time { return wake.Add(-time.Second) })
	if err := w.Handle(context.Background(), h.job()); err != nil {
		t.Fatalf("handle: %v", err)
	}
	if at := h.snoozedUntil(t, msg); at == nil {
		t.Fatal("a message one second short of its wake time was woken")
	}

	w = NewWorker(h.st, quietLogger()).WithClock(func() time.Time { return wake.Add(time.Second) })
	if err := w.Handle(context.Background(), h.job()); err != nil {
		t.Fatalf("handle (after): %v", err)
	}
	if at := h.snoozedUntil(t, msg); at != nil {
		t.Errorf("a message one second past its wake time is still snoozed until %v", at)
	}
}

// wakeJobStates returns every wake job for this user as "<state> <key>", newest
// last, so a test can assert on the LIFECYCLE of the rows and not merely count
// them — the distinction #357 turned on.
func (h *wakeHarness) wakeJobStates(t *testing.T) []string {
	t.Helper()
	rows, err := h.super.Query(context.Background(),
		`SELECT state::text, idempotency_key FROM app.jobs
		  WHERE user_id = $1 AND kind = $2 ORDER BY id`,
		h.userID.String(), store.SnoozeJobKind)
	if err != nil {
		t.Fatalf("read job states: %v", err)
	}
	defer rows.Close()
	var out []string
	for rows.Next() {
		var state, key string
		if err := rows.Scan(&state, &key); err != nil {
			t.Fatalf("scan job state: %v", err)
		}
		out = append(out, state+" "+key)
	}
	return out
}

// deadLetterWakeJob drives the one outstanding wake job to 'dead' the way
// exhaustion actually does it — by claiming it and calling app.fail_job with
// attempts already at max_attempts — rather than by writing 'dead' into the
// row. A hand-written state would prove nothing about the transition that
// produces it in production.
func (h *wakeHarness) deadLetterWakeJob(t *testing.T) string {
	t.Helper()
	ctx := context.Background()
	var id, key string
	if err := h.super.QueryRow(ctx,
		`UPDATE app.jobs SET state = 'running', locked_by = 'test-worker',
		                     locked_at = now(), attempts = max_attempts
		  WHERE user_id = $1 AND kind = $2 AND state = 'pending'
		 RETURNING id, idempotency_key`,
		h.userID.String(), store.SnoozeJobKind).Scan(&id, &key); err != nil {
		t.Fatalf("claim the wake job: %v", err)
	}
	var state string
	if err := h.super.QueryRow(ctx,
		`SELECT app.fail_job($1, 'test-worker', 'handler kept failing', now())`,
		id).Scan(&state); err != nil {
		t.Fatalf("fail the wake job: %v", err)
	}
	if state != "dead" {
		t.Fatalf("fail_job left the job %q, want dead — the test never reached the case it "+
			"exists to cover", state)
	}
	return key
}

// THE #357 CASE. A wake job that DEAD-LETTERS used to hold its idempotency key
// for the lifetime of the database, and the key is derived from the data rather
// than chosen: `snooze_wake:<user>:<wake minute>`. So the boot repair path
// re-derived the same key from the same unwoken message, collided with the dead
// row, inserted nothing, and returned scheduled=false — forever. The deferred
// mail never came back, and nothing said so.
//
// This asserts the repair path repairs. Probed by reverting migration 00042 in
// db/schema.sql (and the matching ON CONFLICT predicate): EnsureScheduled then
// returns false here and the test fails on that line.
func TestEnsureScheduledRepairsAfterAWakeJobDeadLetters(t *testing.T) {
	h := newWakeHarness(t)
	ctx := context.Background()

	// Snoozed to the past: this is a message whose wake ALREADY MISSED, which is
	// the only state the repair path ever finds after a dead-letter — and the
	// reason the re-derived key matches the dead row's minute exactly.
	msg := h.seedSnoozed(t, "deferred and then abandoned", time.Now().Add(-2*time.Hour))

	scheduled, err := EnsureScheduled(ctx, h.st, h.userID)
	if err != nil || !scheduled {
		t.Fatalf("EnsureScheduled (seed) = %v,%v want true,nil", scheduled, err)
	}
	deadKey := h.deadLetterWakeJob(t)

	// The message is still snoozed — nothing woke it — so the queue now holds a
	// dead row for exactly the key the repair path is about to compute.
	if at := h.snoozedUntil(t, msg); at == nil {
		t.Fatal("the message woke without a job running; the test is not exercising a lost wake")
	}

	scheduled, err = EnsureScheduled(ctx, h.st, h.userID)
	if err != nil {
		t.Fatalf("EnsureScheduled (repair): %v", err)
	}
	if !scheduled {
		t.Fatalf("EnsureScheduled declined to repair after a dead-letter (#357): the key %q is "+
			"held by a job that will never run, so the snoozed mail never returns", deadKey)
	}

	states := h.wakeJobStates(t)
	if len(states) != 2 {
		t.Fatalf("wake jobs = %v, want the dead one plus its replacement", states)
	}
	if states[0] != "dead "+deadKey || states[1] != "pending "+deadKey {
		t.Errorf("wake jobs = %v; want the SAME key reused, dead then pending — a replacement "+
			"under a different key would mean the collapse was abandoned rather than scoped",
			states)
	}

	// ...and the collapse still holds on the live row. Widening uniqueness to
	// terminal jobs only is the whole change; a third call must still be a no-op,
	// or every boot would pile up duplicate timers.
	if scheduled, err = EnsureScheduled(ctx, h.st, h.userID); err != nil {
		t.Fatalf("EnsureScheduled (third): %v", err)
	} else if scheduled {
		t.Error("a repaired timer no longer collapses: EnsureScheduled inserted a second pending job")
	}
}
