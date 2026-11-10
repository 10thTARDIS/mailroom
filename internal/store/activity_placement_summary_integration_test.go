// SPDX-License-Identifier: AGPL-3.0-only

//go:build integration

package store

// The Activity placement branch reads one SUMMARY row per batch and direction
// (ADR-053 amendment) instead of grouping every journal row. Proven here:
//
//  1. The entries are the ones the old per-message grouping produced: same
//     time, id, direction and count (the oracle is that grouping, run in SQL).
//  2. Summarising history written before the summaries is report-first, writes
//     nothing without apply, fills exactly the gap, and is idempotent.
//  3. The read does not look at the per-message rows: delete the journal rows
//     after summarising and the entries do not change (the probe that a summary
//     read cannot pass by accident of still grouping the journal).
//  4. MEASURE: the first page's cost before (the grouping) and after, on a
//     synthetic history. MAILROOM_BACKFILL_ROWS sets the size (default 3000;
//     the recorded measurement used 50000).

import (
	"context"
	"os"
	"reflect"
	"sort"
	"strconv"
	"testing"
	"time"
)

type placementEntry struct {
	At      time.Time
	ID      string
	Outcome string
	Count   int
}

func placementEntries(t *testing.T, st *Store, uid string) []placementEntry {
	t.Helper()
	var out []placementEntry
	for _, e := range activityPage(t, st, uid, ActivityFilter{Sources: []string{ActivitySourceBulk}, Limit: 100}).Entries {
		if e.Kind != ActivityKindPlacement {
			continue
		}
		out = append(out, placementEntry{At: e.At.UTC().Truncate(time.Microsecond), ID: e.ID, Outcome: e.Outcome, Count: e.Count})
	}
	return out
}

// legacyGrouping is the placement branch as it was: one row per message grouped
// on read. It is the oracle for "entries unchanged" and the "before" timing.
const legacyGroupingSQL = `
	SELECT max(a.created_at), a.object_id::text, a.action::text, count(*)::int
	  FROM app.audit_log a
	 WHERE a.user_id = $1 AND a.object_type = '` + backfillObjectType + `'
	   AND a.action IN ('` + backfillActionMove + `', '` + backfillActionUndo + `')
	 GROUP BY a.object_id, a.action
	 ORDER BY 1 DESC, 2 DESC LIMIT 101`

func legacyEntries(t *testing.T, h *harness, uid string) []placementEntry {
	t.Helper()
	rows, err := h.super.Query(context.Background(), legacyGroupingSQL, uid)
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	var out []placementEntry
	for rows.Next() {
		var e placementEntry
		if err := rows.Scan(&e.At, &e.ID, &e.Outcome, &e.Count); err != nil {
			t.Fatal(err)
		}
		e.At = e.At.UTC().Truncate(time.Microsecond)
		out = append(out, e)
	}
	return out
}

func seedJournalRows(t *testing.T, h *harness, uid, batch, action string, n int, base time.Time) {
	t.Helper()
	if _, err := h.super.Exec(context.Background(), `
		INSERT INTO app.audit_log (user_id, actor, action, object_type, object_id, detail, created_at)
		SELECT $1, 'admin:placement-backfill', $2, $3, $4, '{}'::jsonb, $5::timestamptz + (g || ' seconds')::interval
		  FROM generate_series(1, $6) g`, uid, action, backfillObjectType, batch, base, n); err != nil {
		t.Fatal(err)
	}
}

func TestPlacementActivityReadsSummariesAndEntriesAreUnchanged(t *testing.T) {
	h := newHarness(t)
	ctx := context.Background()
	a, b := seedTwoUsers(t, h.super)
	s := h.appStore()
	aID := mustULID(t, a.ID)
	base := time.Now().UTC().Add(-48 * time.Hour).Truncate(time.Second)

	// History written BEFORE summaries: three batches, two directions, plus a
	// partly summarised one (2 of 5 rows already covered) and another user's.
	b1, b2, b3, b4 := NewULID().String(), NewULID().String(), NewULID().String(), NewULID().String()
	seedJournalRows(t, h, a.ID, b1, backfillActionMove, 40, base)
	seedJournalRows(t, h, a.ID, b1, backfillActionUndo, 7, base.Add(time.Hour))
	seedJournalRows(t, h, a.ID, b2, backfillActionMove, 3, base.Add(2*time.Hour))
	seedJournalRows(t, h, a.ID, b3, backfillActionMove, 1, base.Add(3*time.Hour))
	seedJournalRows(t, h, a.ID, b4, backfillActionMove, 5, base.Add(4*time.Hour))
	seedJournalRows(t, h, b.ID, NewULID().String(), backfillActionMove, 9, base)
	if _, err := h.super.Exec(ctx, `
		INSERT INTO app.audit_log (user_id, actor, action, object_type, object_id, detail, created_at)
		VALUES ($1, 'admin:placement-backfill', $2, $3, $4, '{"v":1,"count":2}', $5)`,
		a.ID, backfillActionMove, backfillSummaryObjectType, b4, base.Add(4*time.Hour+2*time.Second)); err != nil {
		t.Fatal(err)
	}

	want := legacyEntries(t, h, a.ID)
	if len(want) != 5 {
		t.Fatalf("oracle: %d entries, want 5 (b1 move, b1 undo, b2, b3, b4): %+v", len(want), want)
	}
	// What the page shows before any summary exists for b1-b3: b4's two
	// covered rows only. This is the regression window the backfill closes.
	if got := placementEntries(t, s, a.ID); len(got) != 1 || got[0].ID != b4 || got[0].Count != 2 {
		t.Fatalf("before summarising, the page shows %+v; want only the partly summarised batch", got)
	}

	// REPORT ONLY: counts, and nothing written.
	var before int
	if err := h.super.QueryRow(ctx, `SELECT count(*) FROM app.audit_log WHERE object_type = $1`, backfillSummaryObjectType).Scan(&before); err != nil {
		t.Fatal(err)
	}
	rep, err := s.SummarisePlacementBackfills(ctx, aID, false)
	if err != nil {
		t.Fatal(err)
	}
	if rep.Move.Batches != 4 || rep.Move.Rows != 40+3+1+3 || rep.Move.Summarised != 2 || rep.Undo.Batches != 1 || rep.Undo.Rows != 7 {
		t.Fatalf("report = %+v", rep)
	}
	var after int
	_ = h.super.QueryRow(ctx, `SELECT count(*) FROM app.audit_log WHERE object_type = $1`, backfillSummaryObjectType).Scan(&after)
	if after != before {
		t.Fatalf("a report-only run wrote %d summary rows", after-before)
	}

	// APPLY: the entries are the oracle's, to the microsecond.
	if _, err := s.SummarisePlacementBackfills(ctx, aID, true); err != nil {
		t.Fatal(err)
	}
	got := placementEntries(t, s, a.ID)
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("entries changed by the summary read.\n got  %+v\n want %+v", got, want)
	}
	// Idempotent: nothing left, nothing more written.
	rep, err = s.SummarisePlacementBackfills(ctx, aID, true)
	if err != nil || rep.Move.Batches != 0 || rep.Undo.Batches != 0 {
		t.Fatalf("second run: %+v %v", rep, err)
	}
	// Another user's history is untouched, and invisible to this user's page.
	if other, _ := s.SummarisePlacementBackfills(ctx, mustULID(t, b.ID), false); other.Move.Batches != 1 {
		t.Errorf("the other user's batch should still be unsummarised: %+v", other)
	}
	for _, e := range got {
		if e.Count == 9 {
			t.Error("another user's batch leaked into the page")
		}
	}

	// PROBE: the read does not group the journal. Delete every per-message row;
	// a branch that still grouped them would now show nothing.
	if _, err := h.super.Exec(ctx, `DELETE FROM app.audit_log WHERE user_id = $1 AND object_type = $2`, a.ID, backfillObjectType); err != nil {
		t.Fatal(err)
	}
	if again := placementEntries(t, s, a.ID); !reflect.DeepEqual(again, want) {
		t.Fatalf("the entries depend on the per-message rows:\n got  %+v\n want %+v", again, want)
	}
}

// Two summaries for one batch and direction (an undo run twice) are ONE entry
// whose count is the sum, as the journal grouping gave.
func TestPlacementActivityFoldsRepeatedSummaries(t *testing.T) {
	h := newHarness(t)
	ctx := context.Background()
	a, _ := seedTwoUsers(t, h.super)
	s := h.appStore()
	aID := mustULID(t, a.ID)
	batch := NewULID()
	base := time.Now().UTC().Add(-time.Hour).Truncate(time.Second)
	if err := s.UserTx(ctx, aID, func(tx *UserTx) error {
		first := base
		second := base.Add(time.Minute)
		if err := tx.writeBackfillSummary(ctx, batch, backfillActionUndo, 4, &first); err != nil {
			return err
		}
		return tx.writeBackfillSummary(ctx, batch, backfillActionUndo, 6, &second)
	}); err != nil {
		t.Fatal(err)
	}
	got := placementEntries(t, s, a.ID)
	if len(got) != 1 || got[0].Count != 10 || got[0].Outcome != backfillActionUndo || got[0].ID != batch.String() ||
		!got[0].At.Equal(base.Add(time.Minute).Truncate(time.Microsecond)) {
		t.Fatalf("entries = %+v, want one undo entry of 10 at the later time", got)
	}
}

// MEASUREMENT. The first page of Activity before (the grouping of every
// per-message row) and after (the summaries) on a synthetic history of
// MAILROOM_BACKFILL_ROWS journal rows spread over batches of 500. The "before"
// is the legacy grouping SQL run on its own, the same statement the branch used.
// The guard is deliberately generous: it fails only if the summary read stops
// being much cheaper than the grouping or becomes slow in absolute terms.
func TestPlacementActivityFirstPageCost(t *testing.T) {
	rows := 3000
	if v, err := strconv.Atoi(os.Getenv("MAILROOM_BACKFILL_ROWS")); err == nil && v > 0 {
		rows = v
	}
	h := newHarness(t)
	ctx := context.Background()
	a, _ := seedTwoUsers(t, h.super)
	s := h.appStore()
	aID := mustULID(t, a.ID)
	base := time.Now().UTC().Add(-30 * 24 * time.Hour).Truncate(time.Second)
	batches := 0
	for left, i := rows, 0; left > 0; i++ {
		n := min(500, left)
		seedJournalRows(t, h, a.ID, NewULID().String(), backfillActionMove, n, base.Add(time.Duration(i)*time.Hour))
		left -= n
		batches++
	}
	if _, err := h.super.Exec(ctx, `ANALYZE app.audit_log`); err != nil {
		t.Fatal(err)
	}
	median := func(f func()) time.Duration {
		var ds []time.Duration
		for i := 0; i < 7; i++ {
			st := time.Now()
			f()
			ds = append(ds, time.Since(st))
		}
		sort.Slice(ds, func(i, j int) bool { return ds[i] < ds[j] })
		return ds[len(ds)/2]
	}
	// BEFORE: the old branch's statement, as the app role.
	var legacyN int
	before := median(func() {
		_ = s.UserTx(ctx, aID, func(tx *UserTx) error {
			r, err := tx.tx.Query(ctx, legacyGroupingSQL, a.ID)
			if err != nil {
				return err
			}
			defer r.Close()
			legacyN = 0
			for r.Next() {
				legacyN++
			}
			return r.Err()
		})
	})
	if _, err := s.SummarisePlacementBackfills(ctx, aID, true); err != nil {
		t.Fatal(err)
	}
	// AFTER: the real first page of the placement source.
	var entries int
	after := median(func() {
		entries = len(placementEntries(t, s, a.ID))
	})
	t.Logf("MEASURED first page, %d journal rows in %d batches: before (grouping every row) %v, after (summaries) %v; entries %d vs %d",
		rows, batches, before, after, legacyN, entries)
	if entries != min(batches, 100) || legacyN != entries {
		t.Fatalf("entries differ: %d before, %d after, %d batches", legacyN, entries, batches)
	}
	if batches <= 100 {
		total := 0
		for _, e := range placementEntries(t, s, a.ID) {
			total += e.Count
		}
		if total != rows {
			t.Fatalf("the entries' counts sum to %d, want %d journal rows", total, rows)
		}
	}
	if after > 250*time.Millisecond || (rows >= 20000 && after*5 > before) {
		t.Errorf("summary read %v is not much cheaper than the grouping %v (rows %d)", after, before, rows)
	}
}
