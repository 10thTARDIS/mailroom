//go:build integration

package store

import (
	"context"
	"testing"
)

// ⚠ WHAT THIS PINS, AND WHY IT COULD NOT HAVE EXISTED BEFORE (#351 B3).
//
// app.jobs.locked_at was written on every dequeue and cleared on every
// completion — and read by NOTHING. It appeared in no WHERE clause anywhere in
// db/, internal/ or cmd/. Meanwhile internal/jobs/dispatcher.go said, of the
// clean-shutdown path:
//
//	Shutting down: leave the job 'running' with its lock; the next
//	dispatcher (or this one on restart) reclaims it after the lock ages out.
//
// Nothing aged out. dequeue_due_jobs matched `state = 'pending'` only, and
// complete_job/fail_job are guarded on `locked_by = p_worker` with a worker id
// that is a fresh ULID per process — so after a restart no live worker could
// ever match, and the row stayed 'running' forever.
//
// The cost was not one lost job. EnsureRecurringUserJob treats
// `state IN ('pending','running')` as "already scheduled", so every boot found
// the zombie and returned scheduled=false WITHOUT LOGGING — which silently
// killed the purge_trash and ml_train chains for that user until somebody
// noticed Trash never emptying.
//
// The pair below is deliberate. Reclaiming proves the mechanism exists;
// NOT reclaiming a fresh lock proves it is a lease and not "dequeue ignores
// state", which is what a single positive test would also have accepted.
func TestAJobStrandedByADeadWorkerIsReclaimed(t *testing.T) {
	h := newHarness(t)
	ctx := context.Background()
	a, _ := seedTwoUsers(t, h.super)
	s := h.appStore()
	aID := mustULID(t, a.ID)

	jobID := NewULID()
	if err := s.UserTx(ctx, aID, func(tx *UserTx) error {
		_, err := tx.tx.Exec(ctx, `
			INSERT INTO app.jobs (id, user_id, kind, fire_at, payload, idempotency_key)
			VALUES ($1, $2, 'purge_trash', now() - interval '1 minute', '{}'::jsonb, $3)`,
			jobID.String(), a.ID, "reclaim-"+jobID.String())
		return err
	}); err != nil {
		t.Fatal(err)
	}

	// A worker claims it and then dies: the row keeps 'running' and a locked_by
	// naming a process that no longer exists. Backdating locked_at past the
	// lease is how a restart 16 minutes ago looks to the next dispatcher.
	if err := s.SystemTx(ctx, func(tx *SystemTx) error {
		jobs, err := tx.DequeueDueJobs(ctx, "worker-that-died", 50)
		if err != nil {
			return err
		}
		found := false
		for _, j := range jobs {
			if j.ID == jobID {
				found = true
			}
		}
		if !found {
			t.Fatal("the due job was not claimed on the first pass; the fixture is wrong")
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}

	// CONTROL: still inside the lease, the job must NOT be handed out again.
	// Without this, a dequeue that simply ignored `state` would pass the
	// reclaim assertion below and double-run every job in flight.
	if err := s.SystemTx(ctx, func(tx *SystemTx) error {
		jobs, err := tx.DequeueDueJobs(ctx, "another-live-worker", 50)
		if err != nil {
			return err
		}
		for _, j := range jobs {
			if j.ID == jobID {
				t.Error("a job still within its lease was handed to a second worker")
			}
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}

	if _, err := h.super.Exec(ctx,
		`UPDATE app.jobs SET locked_at = now() - interval '16 minutes' WHERE id = $1`,
		jobID.String()); err != nil {
		t.Fatalf("age the lock: %v", err)
	}

	// THE ASSERTION. Before this change the job was unreachable forever.
	var reclaimed *Job
	if err := s.SystemTx(ctx, func(tx *SystemTx) error {
		jobs, err := tx.DequeueDueJobs(ctx, "worker-after-restart", 50)
		if err != nil {
			return err
		}
		for i := range jobs {
			if jobs[i].ID == jobID {
				reclaimed = &jobs[i]
			}
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	if reclaimed == nil {
		t.Fatal("a job stranded in 'running' past its lease was NOT reclaimed: " +
			"every restart mid-job would permanently kill that user's recurring chain")
	}

	// The reclaim burns a retry, exactly as a fresh claim does. A job whose
	// handler reliably kills the process must exhaust max_attempts and
	// dead-letter, not be retried forever.
	if reclaimed.Attempts != 2 {
		t.Errorf("attempts after reclaim = %d, want 2 (the first claim plus the reclaim)", reclaimed.Attempts)
	}

	// And the lock now names the live worker, so complete_job/fail_job — both
	// guarded on locked_by — can actually finish it. This is the half that makes
	// the reclaim useful rather than merely repeated.
	var lockedBy string
	if err := h.super.QueryRow(ctx,
		`SELECT locked_by FROM app.jobs WHERE id = $1`, jobID.String()).Scan(&lockedBy); err != nil {
		t.Fatalf("read locked_by: %v", err)
	}
	if lockedBy != "worker-after-restart" {
		t.Errorf("locked_by = %q, want the reclaiming worker; a stale owner cannot complete the job", lockedBy)
	}
}
