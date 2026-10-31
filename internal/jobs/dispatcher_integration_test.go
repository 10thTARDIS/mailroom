// SPDX-License-Identifier: AGPL-3.0-only

//go:build integration

package jobs

// Integration coverage for the durable job dispatcher against the real
// app.dequeue_due_jobs / app.complete_job / app.fail_job SECURITY DEFINER
// functions (mailroom#13, #16). Seeds system jobs (user_id NULL) via the
// superuser pool, then drives the transitions through StoreQueue as the
// mailroom_app role — the exact production path. Skips when the integration
// database is not configured (run via scripts/integration-test.sh).

import (
	"context"
	"os"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/oklog/ulid/v2"

	"mailroom/internal/store"
)

func newIntegrationQueue(t *testing.T) (StoreQueue, *pgxpool.Pool) {
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
	st, err := store.Open(ctx, appURL)
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	t.Cleanup(st.Close)
	return StoreQueue{Store: st}, super
}

// seedJob inserts a due system job (user_id NULL) via the superuser pool and
// returns its id. A far-past fire_at keeps it due; a unique idempotency key
// keeps runs independent.
func seedJob(t *testing.T, super *pgxpool.Pool, kind string, maxAttempts int) ulid.ULID {
	t.Helper()
	id := ulid.Make()
	_, err := super.Exec(context.Background(), `
		INSERT INTO app.jobs (id, user_id, kind, fire_at, payload, idempotency_key, max_attempts)
		VALUES ($1, NULL, $2, now() - interval '1 minute', '{}'::jsonb, $3, $4)`,
		id.String(), kind, "it-"+id.String(), maxAttempts)
	if err != nil {
		t.Fatalf("seed job: %v", err)
	}
	// Clean up so these system jobs (user_id NULL, deliberately visible in the
	// no-identity / SystemTx context) do not leak into other packages' tests
	// that assert app.jobs is empty. Tests run serially (-p 1), so cleanup here
	// completes before the store suite runs.
	t.Cleanup(func() {
		_, _ = super.Exec(context.Background(), `DELETE FROM app.jobs WHERE id = $1`, id.String())
	})
	return id
}

func jobState(t *testing.T, super *pgxpool.Pool, id ulid.ULID) string {
	t.Helper()
	var s string
	if err := super.QueryRow(context.Background(),
		`SELECT state FROM app.jobs WHERE id = $1`, id.String()).Scan(&s); err != nil {
		t.Fatalf("read job state: %v", err)
	}
	return s
}

// claim dequeues until the target id appears (tolerating unrelated due jobs
// that other serial tests may have left), returning the claimed Job.
func claim(t *testing.T, q StoreQueue, worker string, id ulid.ULID) store.Job {
	t.Helper()
	ctx := context.Background()
	for i := 0; i < 20; i++ {
		batch, err := q.DequeueDue(ctx, worker, 100)
		if err != nil {
			t.Fatalf("dequeue: %v", err)
		}
		for _, j := range batch {
			if j.ID == id {
				return j
			}
		}
		if len(batch) == 0 {
			break
		}
	}
	t.Fatalf("job %s was never claimed", id)
	return store.Job{}
}

func TestIntegrationCompleteJob(t *testing.T) {
	q, super := newIntegrationQueue(t)
	ctx := context.Background()
	const worker = "it-complete"

	id := seedJob(t, super, "it-noop", 5)
	job := claim(t, q, worker, id)
	if job.Attempts != 1 {
		t.Errorf("attempts after dequeue = %d, want 1", job.Attempts)
	}
	if jobState(t, super, id) != "running" {
		t.Errorf("state after dequeue = %q, want running", jobState(t, super, id))
	}

	done, err := q.Complete(ctx, id, worker)
	if err != nil || !done {
		t.Fatalf("complete: done=%v err=%v", done, err)
	}
	if got := jobState(t, super, id); got != "done" {
		t.Errorf("state after complete = %q, want done", got)
	}
}

func TestIntegrationFailRetriesThenDeadLetters(t *testing.T) {
	q, super := newIntegrationQueue(t)
	ctx := context.Background()
	const worker = "it-fail"

	// max_attempts = 5: first failure retries (back to pending, future fire_at).
	retryID := seedJob(t, super, "it-retry", 5)
	claim(t, q, worker, retryID)
	retryAt := time.Now().Add(30 * time.Minute)
	state, err := q.Fail(ctx, retryID, worker, "boom", retryAt)
	if err != nil {
		t.Fatalf("fail (retry): %v", err)
	}
	if state != store.JobPending {
		t.Errorf("state = %q, want pending (retry)", state)
	}
	if got := jobState(t, super, retryID); got != "pending" {
		t.Errorf("db state = %q, want pending", got)
	}

	// max_attempts = 1: the single attempt is used at dequeue, so failure
	// dead-letters immediately.
	deadID := seedJob(t, super, "it-dead", 1)
	claim(t, q, worker, deadID)
	state, err = q.Fail(ctx, deadID, worker, "fatal", time.Now().Add(time.Hour))
	if err != nil {
		t.Fatalf("fail (dead): %v", err)
	}
	if state != store.JobDead {
		t.Errorf("state = %q, want dead", state)
	}
	if got := jobState(t, super, deadID); got != "dead" {
		t.Errorf("db state = %q, want dead", got)
	}
}

func TestIntegrationTransitionGuardRejectsWrongWorker(t *testing.T) {
	q, super := newIntegrationQueue(t)
	ctx := context.Background()

	id := seedJob(t, super, "it-guard", 5)
	claim(t, q, "owner-worker", id)

	// A different worker must not be able to complete a job it does not hold.
	done, err := q.Complete(ctx, id, "impostor-worker")
	if err != nil {
		t.Fatalf("complete (impostor): %v", err)
	}
	if done {
		t.Error("impostor worker completed a job it did not claim")
	}
	if got := jobState(t, super, id); got != "running" {
		t.Errorf("state = %q, want still running", got)
	}
}
