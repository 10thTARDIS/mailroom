// SPDX-License-Identifier: AGPL-3.0-only

//go:build integration

package ml

// priority-rescore (#842): the dry run writes nothing, --apply writes exactly
// what the dry run counted, and a second pass finds nothing to do.

import (
	"context"
	"fmt"
	"testing"
	"time"
)

func TestRescoreDryRunWritesNothingAndApplyMatchesIt(t *testing.T) {
	f := newTrainFixture(t)
	ctx := context.Background()

	// Ten direct strangers (cold start 0.67: important) and five newsletters
	// (cold start 0.14), all stored at 0.2 by a collapsed trained model.
	const direct, bulk = 10, 5
	base := f.now.Add(-20 * time.Hour)
	for i := 0; i < direct+bulk; i++ {
		id := f.seedLabelled(t, fmt.Sprintf("s%d@d%d.test", i, i), i >= direct, "archive_unread", base.Add(time.Duration(i)*time.Second))
		if _, err := f.super.Exec(ctx,
			`INSERT INTO app.triage_scores (user_id, message_id, score, model_version) VALUES ($1, $2, 0.2, 'priority:3')`,
			f.userID.String(), id.String()); err != nil {
			t.Fatalf("seed score: %v", err)
		}
	}
	snapshot := func() (stale, changes int) {
		if err := f.super.QueryRow(ctx,
			`SELECT count(*) FROM app.triage_scores WHERE user_id = $1 AND model_version = 'priority:3'`,
			f.userID.String()).Scan(&stale); err != nil {
			t.Fatal(err)
		}
		if err := f.super.QueryRow(ctx,
			`SELECT count(*) FROM app.change_log WHERE user_id = $1 AND object_type = 'Email'`,
			f.userID.String()).Scan(&changes); err != nil {
			t.Fatal(err)
		}
		return stale, changes
	}
	since := f.now.Add(-30 * 24 * time.Hour)

	staleBefore, changesBefore := snapshot()
	dry, err := RescorePriority(ctx, f.st, f.reg, f.userID, since, false)
	if err != nil {
		t.Fatalf("dry run: %v", err)
	}
	if dry.Scanned != direct+bulk || dry.Changed != direct+bulk || dry.BecomeImportant != direct || dry.StayNot != bulk {
		t.Fatalf("dry run = %+v, want scanned/changed %d, become important %d, stay not %d", dry, direct+bulk, direct, bulk)
	}
	if dry.Written != 0 {
		t.Errorf("dry run reports %d writes", dry.Written)
	}
	if s, c := snapshot(); s != staleBefore || c != changesBefore {
		t.Fatalf("the dry run wrote: stale %d->%d, change rows %d->%d", staleBefore, s, changesBefore, c)
	}

	applied, err := RescorePriority(ctx, f.st, f.reg, f.userID, since, true)
	if err != nil {
		t.Fatalf("apply: %v", err)
	}
	if applied.Written != dry.Changed || applied.BecomeImportant != dry.BecomeImportant || applied.Changed != dry.Changed {
		t.Fatalf("apply %+v does not match the dry run %+v", applied, dry)
	}
	stale, changes := snapshot()
	if stale != 0 {
		t.Errorf("%d rows still carry the old model's score", stale)
	}
	if changes-changesBefore != applied.Written {
		t.Errorf("apply appended %d Email change rows for %d written scores", changes-changesBefore, applied.Written)
	}

	// Idempotent: nothing left to change, nothing written, no change rows.
	again, err := RescorePriority(ctx, f.st, f.reg, f.userID, since, true)
	if err != nil {
		t.Fatalf("second apply: %v", err)
	}
	if again.Changed != 0 || again.Written != 0 {
		t.Errorf("second apply changed %d / wrote %d, want 0/0", again.Changed, again.Written)
	}
	if _, c := snapshot(); c != changes {
		t.Errorf("second apply appended %d change rows", c-changes)
	}
}
