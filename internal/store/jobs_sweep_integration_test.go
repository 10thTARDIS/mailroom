//go:build integration

package store

import (
	"context"
	"testing"
	"time"
)

// The job sweep (migration 00084). What these pin:
//
//   - only 'done' and 'dead' rows past their windows go, for every user and for
//     system jobs, across more than one batch;
//   - 'pending' and 'running' rows never go, however old, so a live
//     idempotency key is never freed;
//   - a dead row from before 00084 (no completed_at) is aged by its other
//     timestamps instead of being kept forever;
//   - a finished submit job referenced by a submission is swept and the
//     submission keeps its row with job_id NULL, instead of the sweep failing on
//     the foreign key.
func TestJobSweepDeletesOnlyFinishedRowsPastTheirWindows(t *testing.T) {
	h := newHarness(t)
	ctx := context.Background()
	a, b := seedTwoUsers(t, h.super)
	s := h.appStore()

	type row struct {
		user  *string
		kind  string
		state string
		sql   string // the timestamps, as SET assignments
	}
	ids := map[string]string{}
	add := func(name string, r row) {
		t.Helper()
		id := NewULID().String()
		ids[name] = id
		if _, err := h.super.Exec(ctx, `
			INSERT INTO app.jobs (id, user_id, kind, fire_at, payload, idempotency_key, state)
			VALUES ($1, $2, $3, now(), '{}'::jsonb, $4, $5::app.job_state)`,
			id, r.user, r.kind, "sweep-"+name, r.state); err != nil {
			t.Fatalf("seed %s: %v", name, err)
		}
		if r.sql != "" {
			if _, err := h.super.Exec(ctx, `UPDATE app.jobs SET `+r.sql+` WHERE id = $1`, id); err != nil {
				t.Fatalf("backdate %s: %v", name, err)
			}
		}
	}
	add("a_done_old", row{&a.ID, "purge_trash", "done", "completed_at = now() - interval '40 days'"})
	add("a_done_young", row{&a.ID, "purge_trash", "done", "completed_at = now() - interval '5 days'"})
	add("a_submit_old", row{&a.ID, "submit", "done", "completed_at = now() - interval '45 days'"})
	add("b_done_old", row{&b.ID, "drawer_sweep", "done", "completed_at = now() - interval '31 days'"})
	add("sys_done_old", row{nil, "proxy_prefetch", "done", "completed_at = now() - interval '60 days'"})
	add("a_dead_legacy", row{&a.ID, "submit", "dead", "completed_at = NULL, locked_at = NULL, created_at = now() - interval '100 days'"})
	add("a_dead_young", row{&a.ID, "submit", "dead", "completed_at = now() - interval '10 days'"})
	add("a_pending_ancient", row{&a.ID, "snooze_wake", "pending", "created_at = now() - interval '400 days', fire_at = now() + interval '1 day'"})
	add("a_running_ancient", row{&a.ID, "ml_train", "running", "created_at = now() - interval '400 days', locked_at = now() - interval '400 days', locked_by = 'w'"})

	if _, err := h.super.Exec(ctx, `UPDATE app.submissions SET job_id = $1 WHERE user_id = $2`, ids["a_submit_old"], a.ID); err != nil {
		t.Fatal(err)
	}

	// Batch 2 with three old done rows forces a second call.
	counts, capped, err := s.SweepTerminalJobs(ctx, time.Now(), 30*24*time.Hour, 90*24*time.Hour, 2)
	if err != nil {
		t.Fatalf("sweep: %v", err)
	}
	if capped {
		t.Error("capped = true for a handful of rows")
	}
	// At least: the database is shared with other tests, whose old finished
	// rows this sweep may also take. Which rows went is pinned exactly below.
	if counts["done"] < 4 || counts["dead"] < 1 {
		t.Errorf("counts = %v, want done >= 4 (a_done_old, a_submit_old, b_done_old, sys_done_old) and dead >= 1 (a_dead_legacy)", counts)
	}

	gone := map[string]bool{"a_done_old": true, "a_submit_old": true, "b_done_old": true, "sys_done_old": true, "a_dead_legacy": true}
	for name, id := range ids {
		var n int
		if err := h.super.QueryRow(ctx, `SELECT count(*) FROM app.jobs WHERE id = $1`, id).Scan(&n); err != nil {
			t.Fatal(err)
		}
		if gone[name] && n != 0 {
			t.Errorf("%s survived the sweep", name)
		}
		if !gone[name] && n != 1 {
			t.Errorf("%s was swept; it must stay", name)
		}
	}

	// The fixture's own pending jobs (one per user) are untouched too.
	var live int
	if err := h.super.QueryRow(ctx, `SELECT count(*) FROM app.jobs WHERE idempotency_key IN ($1, $2)`,
		"job-"+a.Tag, "job-"+b.Tag).Scan(&live); err != nil {
		t.Fatal(err)
	}
	if live != 2 {
		t.Errorf("fixture pending jobs left = %d, want 2", live)
	}

	// The submission outlived its job, with the reference cleared.
	var jobID *string
	if err := h.super.QueryRow(ctx, `SELECT job_id FROM app.submissions WHERE user_id = $1`, a.ID).Scan(&jobID); err != nil {
		t.Fatalf("submission row after sweep: %v", err)
	}
	if jobID != nil {
		t.Errorf("submission job_id = %v after its job was swept, want NULL", *jobID)
	}

	// The live job's idempotency key is still held: a second outstanding job
	// with the same key is refused.
	if _, err := h.super.Exec(ctx, `
		INSERT INTO app.jobs (id, user_id, kind, fire_at, payload, idempotency_key)
		VALUES ($1, $2, 'snooze_wake', now(), '{}'::jsonb, 'sweep-a_pending_ancient')`,
		NewULID().String(), a.ID); err == nil {
		t.Error("a second pending job took the live job's idempotency key")
	}

	// A second run finds nothing, and still reports both states.
	counts, _, err = s.SweepTerminalJobs(ctx, time.Now(), 30*24*time.Hour, 90*24*time.Hour, 2)
	if err != nil {
		t.Fatal(err)
	}
	if len(counts) != 2 || counts["done"] != 0 || counts["dead"] != 0 {
		t.Errorf("second run counts = %v, want done 0 and dead 0, both present", counts)
	}
}

// A job that exhausts its attempts records when it died, so the sweep can age
// it (before 00084, fail_job left completed_at NULL).
func TestFailJobStampsCompletedAtOnDeadLetter(t *testing.T) {
	h := newHarness(t)
	ctx := context.Background()
	a, _ := seedTwoUsers(t, h.super)
	s := h.appStore()

	id := NewULID()
	if _, err := h.super.Exec(ctx, `
		INSERT INTO app.jobs (id, user_id, kind, fire_at, payload, idempotency_key, state, attempts, max_attempts, locked_by, locked_at)
		VALUES ($1, $2, 'submit', now(), '{}'::jsonb, 'dead-stamp', 'running', 5, 5, 'w', now())`,
		id.String(), a.ID); err != nil {
		t.Fatal(err)
	}
	if err := s.SystemTx(ctx, func(tx *SystemTx) error {
		st, err := tx.FailJob(ctx, id, "w", "boom", time.Now().Add(time.Minute))
		if err != nil {
			return err
		}
		if st != JobState("dead") {
			t.Fatalf("state = %q, want dead", st)
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	var stamped bool
	if err := h.super.QueryRow(ctx, `SELECT completed_at IS NOT NULL FROM app.jobs WHERE id = $1`, id.String()).Scan(&stamped); err != nil {
		t.Fatal(err)
	}
	if !stamped {
		t.Error("a dead-lettered job has no completed_at")
	}
}

// The census sees every user's rows (it is a definer for that reason) and
// counts what the sweep would take.
func TestJobCensusCountsSweepableRowsAcrossUsers(t *testing.T) {
	h := newHarness(t)
	ctx := context.Background()
	a, b := seedTwoUsers(t, h.super)
	s := h.appStore()

	// Kinds unique to this test: the database is shared, and other tests'
	// fixture jobs would otherwise land in the same groups.
	doneKind, pendingKind := "census_done_"+a.Tag, "census_pending_"+a.Tag
	for _, u := range []string{a.ID, b.ID} {
		if _, err := h.super.Exec(ctx, `
			INSERT INTO app.jobs (id, user_id, kind, fire_at, payload, idempotency_key, state, completed_at)
			VALUES ($1, $2, $3, now(), '{}'::jsonb, $4, 'done', now() - interval '40 days'),
			       ($5, $2, $6, now() + interval '1 day', '{}'::jsonb, $7, 'pending', NULL)`,
			NewULID().String(), u, doneKind, "census-done-"+u,
			NewULID().String(), pendingKind, "census-pending-"+u); err != nil {
			t.Fatal(err)
		}
	}
	rows, err := s.JobCensus(ctx, time.Now(), 30*24*time.Hour, 90*24*time.Hour)
	if err != nil {
		t.Fatal(err)
	}
	var drawerDone, pending JobCensusRow
	for _, r := range rows {
		if r.Kind == doneKind && r.State == "done" {
			drawerDone = r
		}
		if r.Kind == pendingKind && r.State == "pending" {
			pending = r
		}
	}
	if drawerDone.Total != 2 || drawerDone.Sweepable != 2 || drawerDone.OldestFinished == nil {
		t.Errorf("done group = %+v, want 2 rows, 2 sweepable, an oldest time", drawerDone)
	}
	// Both users' fixture jobs: proof the census is not limited to system rows.
	if pending.Total != 2 || pending.Sweepable != 0 {
		t.Errorf("pending group = %+v, want 2 rows across both users, none sweepable", pending)
	}
}
