// SPDX-License-Identifier: AGPL-3.0-only

//go:build integration

package store

// Store-layer integration tests: the UserTx/SystemTx contract and the
// change-seq / change_log helpers (mailroom-data-model.md §3/§5).

import (
	"context"
	"sync"
	"testing"
)

func TestChangeSeqAllocation(t *testing.T) {
	h := newHarness(t)
	ctx := context.Background()
	a, _ := seedTwoUsers(t, h.super)
	s := h.appStore()
	aID := mustULID(t, a.ID)

	// Fixtures leave (A, Email) at 2; allocation continues from there and is
	// independent per type (Mailbox starts fresh via lazy upsert).
	err := s.UserTx(ctx, aID, func(tx *UserTx) error {
		for want := int64(3); want <= 5; want++ {
			seq, err := tx.NextChangeSeq(ctx, ObjectEmail)
			if err != nil {
				return err
			}
			if seq != want {
				t.Errorf("Email seq = %d, want %d", seq, want)
			}
		}
		seq, err := tx.NextChangeSeq(ctx, ObjectMailbox)
		if err != nil {
			return err
		}
		if seq != 1 {
			t.Errorf("first Mailbox seq = %d, want 1 (independent per type)", seq)
		}
		state, err := tx.ChangeState(ctx, ObjectEmail)
		if err != nil {
			return err
		}
		if state != 5 {
			t.Errorf("ChangeState(Email) = %d, want 5", state)
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
}

// Concurrent writers must receive strictly distinct sequences — the row lock
// on change_seq is the serialization point (data-model §3).
func TestChangeSeqConcurrentAllocationsDistinct(t *testing.T) {
	h := newHarness(t)
	ctx := context.Background()
	a, _ := seedTwoUsers(t, h.super)
	s := h.appStore()
	aID := mustULID(t, a.ID)

	const workers, perWorker = 8, 5
	var (
		mu   sync.Mutex
		seen = make(map[int64]bool)
		wg   sync.WaitGroup
	)
	errs := make(chan error, workers)
	for w := 0; w < workers; w++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for i := 0; i < perWorker; i++ {
				err := s.UserTx(ctx, aID, func(tx *UserTx) error {
					seq, err := tx.NextChangeSeq(ctx, ObjectThread)
					if err != nil {
						return err
					}
					mu.Lock()
					defer mu.Unlock()
					if seen[seq] {
						t.Errorf("sequence %d allocated twice", seq)
					}
					seen[seq] = true
					return nil
				})
				if err != nil {
					errs <- err
					return
				}
			}
		}()
	}
	wg.Wait()
	close(errs)
	for err := range errs {
		t.Fatal(err)
	}
	if len(seen) != workers*perWorker {
		t.Fatalf("allocated %d distinct sequences, want %d", len(seen), workers*perWorker)
	}
}

// AppendChange writes the log row in the same transaction (the invariant) —
// visible inside the transaction, consistent with ChangeState, and scoped to
// the writing user.
func TestAppendChangeSameTransaction(t *testing.T) {
	h := newHarness(t)
	ctx := context.Background()
	a, b := seedTwoUsers(t, h.super)
	s := h.appStore()
	aID, bID := mustULID(t, a.ID), mustULID(t, b.ID)
	objectID := NewULID()

	var seq int64
	err := s.UserTx(ctx, aID, func(tx *UserTx) error {
		var err error
		if seq, err = tx.AppendChange(ctx, ObjectLabel, objectID, OpCreated); err != nil {
			return err
		}
		// Same-transaction visibility.
		var n int
		if err := tx.tx.QueryRow(ctx, `
			SELECT count(*) FROM app.change_log
			 WHERE user_id = $1 AND object_type = 'Label' AND seq = $2 AND object_id = $3`,
			a.ID, seq, objectID.String()).Scan(&n); err != nil {
			return err
		}
		if n != 1 {
			t.Errorf("change_log row not visible in the writing transaction")
		}
		state, err := tx.ChangeState(ctx, ObjectLabel)
		if err != nil {
			return err
		}
		if state != seq {
			t.Errorf("ChangeState = %d, want %d (state/data consistency)", state, seq)
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}

	// B never sees A's change stream.
	err = s.UserTx(ctx, bID, func(tx *UserTx) error {
		var n int
		if err := tx.tx.QueryRow(ctx, `
			SELECT count(*) FROM app.change_log WHERE object_id = $1`,
			objectID.String()).Scan(&n); err != nil {
			return err
		}
		if n != 0 {
			t.Errorf("user B sees %d change_log rows of user A, want 0", n)
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
}

// SystemTx: identity unset means user rows are invisible; the ONE narrow
// cross-user surface is app.dequeue_due_jobs, which claims due jobs across
// users without exposing mail content.
func TestSystemTxJobsDispatch(t *testing.T) {
	h := newHarness(t)
	ctx := context.Background()
	a, _ := seedTwoUsers(t, h.super)
	s := h.appStore()
	aID := mustULID(t, a.ID)

	// User rows are invisible in a SystemTx.
	err := s.SystemTx(ctx, func(tx *SystemTx) error {
		for _, table := range []string{"users", "messages", "change_log", "jobs"} {
			var n int
			if err := tx.tx.QueryRow(ctx, `SELECT count(*) FROM app.`+table).Scan(&n); err != nil {
				return err
			}
			if n != 0 {
				t.Errorf("SystemTx sees %d rows of app.%s directly, want 0", n, table)
			}
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}

	// A user schedules a due job through the normal door...
	jobID := NewULID()
	idem := "due-job-" + jobID.String()
	err = s.UserTx(ctx, aID, func(tx *UserTx) error {
		_, err := tx.tx.Exec(ctx, `
			INSERT INTO app.jobs (id, user_id, kind, fire_at, payload, idempotency_key)
			VALUES ($1, $2, 'submit', now() - interval '1 minute', '{}'::jsonb, $3)`,
			jobID.String(), a.ID, idem)
		return err
	})
	if err != nil {
		t.Fatal(err)
	}

	// ...and the dispatcher claims it via the SECURITY DEFINER function.
	var claimed *Job
	err = s.SystemTx(ctx, func(tx *SystemTx) error {
		jobs, err := tx.DequeueDueJobs(ctx, "test-worker", 50)
		if err != nil {
			return err
		}
		for i := range jobs {
			if jobs[i].ID == jobID {
				claimed = &jobs[i]
			}
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	if claimed == nil {
		t.Fatal("dispatcher did not claim the due job")
	}
	if claimed.UserID == nil || *claimed.UserID != aID {
		t.Errorf("claimed job user = %v, want %s", claimed.UserID, aID)
	}
	if claimed.Kind != "submit" || claimed.Attempts != 1 {
		t.Errorf("claimed job kind=%q attempts=%d, want submit/1", claimed.Kind, claimed.Attempts)
	}

	// The claim is stateful: a second dequeue must not return it.
	err = s.SystemTx(ctx, func(tx *SystemTx) error {
		jobs, err := tx.DequeueDueJobs(ctx, "test-worker-2", 50)
		if err != nil {
			return err
		}
		for _, j := range jobs {
			if j.ID == jobID {
				t.Error("job claimed twice")
			}
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}

	// Owner still sees their job (state now 'running') through UserTx.
	err = s.UserTx(ctx, aID, func(tx *UserTx) error {
		var state string
		if err := tx.tx.QueryRow(ctx,
			`SELECT state FROM app.jobs WHERE id = $1`, jobID.String()).Scan(&state); err != nil {
			return err
		}
		if state != "running" {
			t.Errorf("job state = %q, want running", state)
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
}

// Regression for mailroom#16: the dispatcher's cross-user reach must come from
// the deliberate jobs_dispatcher_owner RLS policy on a non-superuser definer —
// NOT from the definer happening to be a superuser (as it would be if the
// schema were applied by postgres without pinning app.dequeue_due_jobs's
// owner). If the owner regresses to a superuser, TestSystemTxJobsDispatch still
// passes (bypass hides the bug), but dequeue would silently return zero rows
// once production migrations run as the non-superuser mailroom_owner. This test
// pins the definer's identity so that regression is caught directly.
func TestDequeueDefinerIsNonSuperuser(t *testing.T) {
	h := newHarness(t)
	ctx := context.Background()

	var (
		owner           string
		isSuper, bypass bool
	)
	err := h.super.QueryRow(ctx, `
		SELECT r.rolname, r.rolsuper, r.rolbypassrls
		  FROM pg_proc p
		  JOIN pg_roles r ON r.oid = p.proowner
		 WHERE p.oid = 'app.dequeue_due_jobs(text, int)'::regprocedure`).
		Scan(&owner, &isSuper, &bypass)
	if err != nil {
		t.Fatal(err)
	}
	if owner != "mailroom_owner" {
		t.Errorf("dequeue_due_jobs owner = %q, want mailroom_owner (SECURITY DEFINER must not run as a superuser)", owner)
	}
	if isSuper || bypass {
		t.Errorf("definer role %q has rolsuper=%v rolbypassrls=%v; it must be subject to RLS", owner, isSuper, bypass)
	}
}
