// SPDX-License-Identifier: AGPL-3.0-only

//go:build integration

package store

// Integration coverage for CompactChangeLog (mailroom#526 Part 2,
// changelog_compact.go). These prove the strict-prefix property against a
// real Postgres: the boundary computed from created_at, the batch cap, the
// report-mode no-op, and RLS isolation. jobs/CLI-level wiring is covered
// elsewhere (internal/changelog, cmd/mailroom-admin).

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/oklog/ulid/v2"
)

// snapshotChanges captures tx.Changes(t, since, 0) across a whole range of
// since values, so a before/after compaction comparison can assert byte-for-
// byte equality rather than trusting a summary count.
func snapshotChanges(t *testing.T, ctx context.Context, tx *UserTx, typ ObjectType, from, to int64) map[int64]ChangesPage {
	t.Helper()
	out := make(map[int64]ChangesPage, to-from+1)
	for since := from; since <= to; since++ {
		page, err := tx.Changes(ctx, typ, since, 0)
		if err != nil {
			t.Fatalf("snapshot Changes(%d): %v", since, err)
		}
		out[since] = page
	}
	return out
}

func assertPagesEqual(t *testing.T, since int64, got, want ChangesPage) {
	t.Helper()
	if got.OldState != want.OldState || got.NewState != want.NewState || got.HasMore != want.HasMore {
		t.Errorf("Changes(%d) state/hasMore = %+v, want %+v", since, got, want)
	}
	eq := func(name string, g, w []ulid.ULID) {
		if len(g) != len(w) {
			t.Errorf("Changes(%d).%s = %v, want %v", since, name, g, w)
			return
		}
		for i := range g {
			if g[i] != w[i] {
				t.Errorf("Changes(%d).%s = %v, want %v", since, name, g, w)
				return
			}
		}
	}
	eq("Created", got.Created, want.Created)
	eq("Updated", got.Updated, want.Updated)
	eq("Destroyed", got.Destroyed, want.Destroyed)
}

// resultFor picks one type's CompactionResult out of a CompactChangeLog call
// that (by design) reports on every type in app.change_seq for the user.
func resultFor(t *testing.T, results []CompactionResult, typ ObjectType) CompactionResult {
	t.Helper()
	for _, r := range results {
		if r.ObjectType == typ {
			return r
		}
	}
	t.Fatalf("no CompactionResult for %s among %d results", typ, len(results))
	return CompactionResult{}
}

// TestCompactChangeLogPrefixProperty is the prefix-rule test the spec calls
// for: seq 1-4 OLD, 5 NEW, 6 OLD (a created_at INVERSION — older wall-clock
// time than its neighbours despite a higher seq), 7-10 NEW. It uses
// ObjectLabel, a type the fixture user never touches, so the 10 rows here are
// the type's entire history and nothing from seedUser's own Email rows
// contaminates the assertions.
func TestCompactChangeLogPrefixProperty(t *testing.T) {
	h := newHarness(t)
	ctx := context.Background()
	a, _ := seedTwoUsers(t, h.super)
	s := h.appStore()
	aID := mustULID(t, a.ID)

	// Ten real AppendChange calls — the actual mutation path, not a hand-
	// crafted INSERT — so seq allocation is exactly what production does.
	if err := s.UserTx(ctx, aID, func(tx *UserTx) error {
		for i := 0; i < 10; i++ {
			if _, err := tx.AppendChange(ctx, ObjectLabel, NewULID(), OpCreated); err != nil {
				return err
			}
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}

	// Backdate 1-4 and 6 (the inversion) well past the retention window;
	// leave 5 and 7-10 at their natural "now" timestamp, which the cutoff
	// below sits in front of.
	if _, err := h.super.Exec(ctx, `
		UPDATE app.change_log SET created_at = now() - interval '100 days'
		 WHERE user_id = $1 AND object_type = 'Label' AND seq = ANY($2)`,
		a.ID, []int64{1, 2, 3, 4, 6}); err != nil {
		t.Fatalf("backdate rows: %v", err)
	}
	cutoff := time.Now().Add(-50 * 24 * time.Hour)

	var before map[int64]ChangesPage
	if err := s.UserTx(ctx, aID, func(tx *UserTx) error {
		before = snapshotChanges(t, ctx, tx, ObjectLabel, 0, 10)
		return nil
	}); err != nil {
		t.Fatal(err)
	}

	// Compact: D = min(seq where created_at>=cutoff) - 1 = 5 - 1 = 4.
	var results []CompactionResult
	if err := s.UserTx(ctx, aID, func(tx *UserTx) error {
		var err error
		results, err = tx.CompactChangeLog(ctx, cutoff, 0, true)
		return err
	}); err != nil {
		t.Fatal(err)
	}
	res := resultFor(t, results, ObjectLabel)
	if res.OldFloor != 0 || res.NewFloor != 4 || res.Deleted != 4 {
		t.Fatalf("compaction result = %+v, want OldFloor=0 NewFloor=4 Deleted=4", res)
	}
	if res.RowsBefore != 10 {
		t.Errorf("RowsBefore = %d, want 10", res.RowsBefore)
	}

	// Rows 5..10 all still exist, INCLUDING the old-dated inversion row 6.
	var remaining int
	if err := h.super.QueryRow(ctx,
		`SELECT count(*) FROM app.change_log WHERE user_id = $1 AND object_type = 'Label'`,
		a.ID).Scan(&remaining); err != nil {
		t.Fatal(err)
	}
	if remaining != 6 {
		t.Errorf("rows remaining = %d, want 6 (seq 5..10)", remaining)
	}
	var row6Exists bool
	if err := h.super.QueryRow(ctx,
		`SELECT EXISTS(SELECT 1 FROM app.change_log WHERE user_id = $1 AND object_type = 'Label' AND seq = 6)`,
		a.ID).Scan(&row6Exists); err != nil {
		t.Fatal(err)
	}
	if !row6Exists {
		t.Error("row 6 (the old-dated inversion, seq above the boundary) was deleted; the prefix rule was violated")
	}

	if err := s.UserTx(ctx, aID, func(tx *UserTx) error {
		// Below the new floor: rejected.
		if _, err := tx.Changes(ctx, ObjectLabel, 3, 0); !errors.Is(err, ErrCannotCalculateChanges) {
			t.Errorf("Changes(3) after compaction = %v, want ErrCannotCalculateChanges", err)
		}
		// 4..10: identical to the pre-compaction snapshot.
		after := snapshotChanges(t, ctx, tx, ObjectLabel, 4, 10)
		for since := int64(4); since <= 10; since++ {
			assertPagesEqual(t, since, after[since], before[since])
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}

	// change_seq is untouched by compaction.
	var lastSeq int64
	if err := h.super.QueryRow(ctx,
		`SELECT last_seq FROM app.change_seq WHERE user_id = $1 AND object_type = 'Label'`,
		a.ID).Scan(&lastSeq); err != nil {
		t.Fatal(err)
	}
	if lastSeq != 10 {
		t.Errorf("change_seq.last_seq = %d, want 10 (compaction must not touch it)", lastSeq)
	}

	// Second compaction: a no-op. 0 deleted, floor unchanged.
	if err := s.UserTx(ctx, aID, func(tx *UserTx) error {
		results2, err := tx.CompactChangeLog(ctx, cutoff, 0, true)
		if err != nil {
			return err
		}
		res2 := resultFor(t, results2, ObjectLabel)
		if res2.Deleted != 0 || res2.NewFloor != 4 || res2.OldFloor != 4 {
			t.Errorf("second compaction = %+v, want a no-op at floor 4", res2)
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
}

// TestCompactChangeLogBatchCap proves maxDeletePerType bounds a single call
// (25 old rows, cap 10) and that repeated calls converge while the prefix
// property holds at every step.
func TestCompactChangeLogBatchCap(t *testing.T) {
	h := newHarness(t)
	ctx := context.Background()
	a, _ := seedTwoUsers(t, h.super)
	s := h.appStore()
	aID := mustULID(t, a.ID)

	const n = 25
	if err := s.UserTx(ctx, aID, func(tx *UserTx) error {
		for i := 0; i < n; i++ {
			if _, err := tx.AppendChange(ctx, ObjectRule, NewULID(), OpCreated); err != nil {
				return err
			}
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	// All 25 rows are old; cutoff is in the future relative to their natural
	// "now" created_at, so every row is eligible.
	cutoff := time.Now().Add(time.Hour)

	wantFloor := int64(0)
	for _, wantDeleted := range []int64{10, 10, 5, 0} {
		var res CompactionResult
		if err := s.UserTx(ctx, aID, func(tx *UserTx) error {
			results, err := tx.CompactChangeLog(ctx, cutoff, 10, true)
			if err != nil {
				return err
			}
			res = resultFor(t, results, ObjectRule)
			return nil
		}); err != nil {
			t.Fatal(err)
		}
		if res.Deleted != wantDeleted {
			t.Fatalf("Deleted = %d, want %d (floor was %d)", res.Deleted, wantDeleted, wantFloor)
		}
		wantFloor += wantDeleted
		if res.NewFloor != wantFloor {
			t.Fatalf("NewFloor = %d, want %d", res.NewFloor, wantFloor)
		}
		// Prefix property at each step: exactly the rows above the floor exist.
		var remaining int
		if err := h.super.QueryRow(ctx,
			`SELECT count(*) FROM app.change_log WHERE user_id = $1 AND object_type = 'Rule'`,
			a.ID).Scan(&remaining); err != nil {
			t.Fatal(err)
		}
		if want := n - int(wantFloor); remaining != want {
			t.Fatalf("rows remaining = %d, want %d", remaining, want)
		}
		var belowFloor int
		if err := h.super.QueryRow(ctx,
			`SELECT count(*) FROM app.change_log
			  WHERE user_id = $1 AND object_type = 'Rule' AND seq <= $2`,
			a.ID, wantFloor).Scan(&belowFloor); err != nil {
			t.Fatal(err)
		}
		if belowFloor != 0 {
			t.Fatalf("%d row(s) at or below the floor still exist", belowFloor)
		}
	}
}

// TestCompactChangeLogDeletedIsMeasuredNotDerived pins that Deleted is a COUNT
// of rows, not NewFloor - OldFloor. The two agree only while seq is gap-free
// above the floor, which holds today (compaction is the only DELETE on
// change_log) but is an assumption — and compact-changelog prints Deleted as the
// evidence in its `before − deleted = after` line. A hole is manufactured here
// directly so the difference is observable.
func TestCompactChangeLogDeletedIsMeasuredNotDerived(t *testing.T) {
	h := newHarness(t)
	ctx := context.Background()
	a, _ := seedTwoUsers(t, h.super)
	s := h.appStore()
	aID := mustULID(t, a.ID)

	if err := s.UserTx(ctx, aID, func(tx *UserTx) error {
		for i := 0; i < 10; i++ {
			if _, err := tx.AppendChange(ctx, ObjectRule, NewULID(), OpCreated); err != nil {
				return err
			}
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	if _, err := h.super.Exec(ctx,
		`DELETE FROM app.change_log WHERE user_id = $1 AND object_type = 'Rule' AND seq IN (3, 4)`,
		a.ID); err != nil {
		t.Fatal(err)
	}
	cutoff := time.Now().Add(time.Hour) // every row is older than this

	for _, apply := range []bool{false, true} {
		var res CompactionResult
		if err := s.UserTx(ctx, aID, func(tx *UserTx) error {
			results, err := tx.CompactChangeLog(ctx, cutoff, 0, apply)
			if err != nil {
				return err
			}
			res = resultFor(t, results, ObjectRule)
			return nil
		}); err != nil {
			t.Fatal(err)
		}
		if res.NewFloor != 10 || res.RowsBefore != 8 || res.Deleted != 8 {
			t.Errorf("apply=%t: result = %+v; want NewFloor=10 RowsBefore=8 Deleted=8 "+
				"(8 rows exist; NewFloor-OldFloor would say 10)", apply, res)
		}
	}
}

// TestCompactChangeLogReportModeWritesNothing proves apply=false changes
// nothing — the row count and the floor are identical before and after a
// report call — and that its numbers equal what apply=true then produces.
func TestCompactChangeLogReportModeWritesNothing(t *testing.T) {
	h := newHarness(t)
	ctx := context.Background()
	a, _ := seedTwoUsers(t, h.super)
	s := h.appStore()
	aID := mustULID(t, a.ID)

	const n = 8
	if err := s.UserTx(ctx, aID, func(tx *UserTx) error {
		for i := 0; i < n; i++ {
			if _, err := tx.AppendChange(ctx, ObjectSavedSearch, NewULID(), OpCreated); err != nil {
				return err
			}
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	cutoff := time.Now().Add(time.Hour) // every row eligible

	rowCount := func() int {
		t.Helper()
		var c int
		if err := h.super.QueryRow(ctx,
			`SELECT count(*) FROM app.change_log WHERE user_id = $1 AND object_type = 'SavedSearch'`,
			a.ID).Scan(&c); err != nil {
			t.Fatal(err)
		}
		return c
	}
	floorVal := func() int64 {
		t.Helper()
		var f int64
		if err := h.super.QueryRow(ctx, `
			SELECT COALESCE((SELECT floor_seq FROM app.change_floor
			                   WHERE user_id = $1 AND object_type = 'SavedSearch'), 0)`,
			a.ID).Scan(&f); err != nil {
			t.Fatal(err)
		}
		return f
	}

	rowsBeforeReport, floorBeforeReport := rowCount(), floorVal()

	var reportRes CompactionResult
	if err := s.UserTx(ctx, aID, func(tx *UserTx) error {
		results, err := tx.CompactChangeLog(ctx, cutoff, 0, false)
		if err != nil {
			return err
		}
		reportRes = resultFor(t, results, ObjectSavedSearch)
		return nil
	}); err != nil {
		t.Fatal(err)
	}

	if got := rowCount(); got != rowsBeforeReport {
		t.Errorf("row count after report = %d, want unchanged %d", got, rowsBeforeReport)
	}
	if got := floorVal(); got != floorBeforeReport {
		t.Errorf("floor after report = %d, want unchanged %d", got, floorBeforeReport)
	}
	if reportRes.Deleted != int64(n) || reportRes.NewFloor != int64(n) {
		t.Fatalf("report result = %+v, want Deleted=%d NewFloor=%d", reportRes, n, n)
	}

	// Now apply, and confirm it produced exactly what the report promised.
	var applyRes CompactionResult
	if err := s.UserTx(ctx, aID, func(tx *UserTx) error {
		results, err := tx.CompactChangeLog(ctx, cutoff, 0, true)
		if err != nil {
			return err
		}
		applyRes = resultFor(t, results, ObjectSavedSearch)
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	// Field-by-field: OldestKept is a *time.Time, so a bare struct != would
	// compare pointer identity (always unequal across two separate scans)
	// rather than the value it points to.
	if applyRes.ObjectType != reportRes.ObjectType ||
		applyRes.RowsBefore != reportRes.RowsBefore ||
		applyRes.Deleted != reportRes.Deleted ||
		applyRes.OldFloor != reportRes.OldFloor ||
		applyRes.NewFloor != reportRes.NewFloor {
		t.Errorf("apply result %+v != report result %+v", applyRes, reportRes)
	}
	switch {
	case (applyRes.OldestKept == nil) != (reportRes.OldestKept == nil):
		t.Errorf("apply OldestKept=%v, report OldestKept=%v", applyRes.OldestKept, reportRes.OldestKept)
	case applyRes.OldestKept != nil && !applyRes.OldestKept.Equal(*reportRes.OldestKept):
		t.Errorf("apply OldestKept=%v, report OldestKept=%v", *applyRes.OldestKept, *reportRes.OldestKept)
	}
}

// TestCompactChangeLogRLSIsolation proves compacting user A leaves user B's
// change_log rows and floor untouched, even though both users hold Email
// history old enough to be eligible.
func TestCompactChangeLogRLSIsolation(t *testing.T) {
	h := newHarness(t)
	ctx := context.Background()
	a, b := seedTwoUsers(t, h.super)
	s := h.appStore()
	aID := mustULID(t, a.ID)

	// Both fixture users' seeded Email rows (seq 1,2) are backdated so both
	// are equally eligible — isolation must come from user scoping, not from
	// B simply not qualifying.
	for _, u := range []string{a.ID, b.ID} {
		if _, err := h.super.Exec(ctx, `
			UPDATE app.change_log SET created_at = now() - interval '100 days'
			 WHERE user_id = $1 AND object_type = 'Email'`, u); err != nil {
			t.Fatalf("backdate %s: %v", u, err)
		}
	}
	cutoff := time.Now().Add(-50 * 24 * time.Hour)

	bRowsBefore, bFloorBefore := 0, int64(0)
	if err := h.super.QueryRow(ctx,
		`SELECT count(*) FROM app.change_log WHERE user_id = $1 AND object_type = 'Email'`,
		b.ID).Scan(&bRowsBefore); err != nil {
		t.Fatal(err)
	}
	if err := h.super.QueryRow(ctx, `
		SELECT COALESCE((SELECT floor_seq FROM app.change_floor
		                   WHERE user_id = $1 AND object_type = 'Email'), 0)`,
		b.ID).Scan(&bFloorBefore); err != nil {
		t.Fatal(err)
	}

	if err := s.UserTx(ctx, aID, func(tx *UserTx) error {
		results, err := tx.CompactChangeLog(ctx, cutoff, 0, true)
		if err != nil {
			return err
		}
		res := resultFor(t, results, ObjectEmail)
		if res.Deleted == 0 {
			t.Error("A's Email compaction deleted nothing; the fixture is wrong")
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}

	var bRowsAfter int
	if err := h.super.QueryRow(ctx,
		`SELECT count(*) FROM app.change_log WHERE user_id = $1 AND object_type = 'Email'`,
		b.ID).Scan(&bRowsAfter); err != nil {
		t.Fatal(err)
	}
	if bRowsAfter != bRowsBefore {
		t.Errorf("B's Email row count changed: %d -> %d after A's compaction", bRowsBefore, bRowsAfter)
	}
	var bFloorAfter int64
	if err := h.super.QueryRow(ctx, `
		SELECT COALESCE((SELECT floor_seq FROM app.change_floor
		                   WHERE user_id = $1 AND object_type = 'Email'), 0)`,
		b.ID).Scan(&bFloorAfter); err != nil {
		t.Fatal(err)
	}
	if bFloorAfter != bFloorBefore {
		t.Errorf("B's Email floor changed: %d -> %d after A's compaction", bFloorBefore, bFloorAfter)
	}
}
