// SPDX-License-Identifier: AGPL-3.0-only

//go:build integration

package store

// ADR-049 increment 2: readers use the stored area (messages.area_id) once a
// user is ready, and the resolver otherwise.
//
// The claim worth testing is that the two modes give the SAME answer, so the
// tests here run one battery of every reader against one fixture in both modes
// and compare the results, row for row:
//
//   - TestStoredAreaReadersAgreeWithTheResolver: resolver mode first, then the
//     same user after the backfill marks it ready, then compared. Neither mode
//     is the oracle for the other; both are also held to their existing oracle
//     suites (eachAreaMode runs those in both modes).
//   - TestAreaReadersAreGatedOnReadiness: a user who is not ready sends no
//     statement that mentions the stored column, a ready one sends it (the
//     positive control that the first check can fail).
//   - TestStoredAreaReadersDetectACorruptedColumn: the negative control for the
//     agreement. If the stored column is wrong for some messages, the same
//     battery must differ from the resolver's.
//   - TestBoundedPageIsOnlyTheStoredPath: the bounded keyset page serves a ready
//     user's area list, never a user who is not ready, and its statement can be
//     served by messages_user_area_arrival in the list's own order.

import (
	"context"
	"fmt"
	"sort"
	"strings"
	"testing"
	"time"

	"github.com/oklog/ulid/v2"
)

// eachAreaMode runs body for a user whose area index is not ready (the
// resolver) and for one that is (the stored column). The body must call
// readyAreaIndex itself when ready is true, AFTER it has built its fixture.
func eachAreaMode(t *testing.T, body func(t *testing.T, ready bool)) {
	t.Helper()
	t.Run("resolver", func(t *testing.T) { body(t, false) })
	t.Run("stored", func(t *testing.T) { body(t, true) })
}

// dropJobsAtCleanup deletes the writeback jobs the test's store calls enqueue
// for userID when the test ends: the dispatcher tests claim at most 50 due jobs
// and would not find their own behind a pile left by every two-mode run.
func dropJobsAtCleanup(t *testing.T, h *harness, userID ulid.ULID) {
	t.Helper()
	t.Cleanup(func() {
		_, _ = h.super.Exec(context.Background(), `DELETE FROM app.jobs WHERE user_id = $1`, userID.String())
	})
}

// readyAreaIndex backfills the user's column and marks it ready, the way
// `mailroom-admin area-index --apply` does, refusing to continue unless the
// report then shows zero disagreements.
func readyAreaIndex(t *testing.T, st *Store, userID ulid.ULID) {
	t.Helper()
	ctx := context.Background()
	var after ulid.ULID
	for {
		var (
			last ulid.ULID
			n    int
		)
		if err := st.UserTx(ctx, userID, func(tx *UserTx) error {
			var e error
			last, n, _, e = tx.AreaIndexBackfillBatch(ctx, after, 400)
			return e
		}); err != nil {
			t.Fatalf("area index backfill: %v", err)
		}
		if n == 0 {
			break
		}
		after = last
	}
	if err := st.UserTx(ctx, userID, func(tx *UserTx) error {
		rep, err := tx.AreaIndexReport(ctx)
		if err != nil {
			return err
		}
		if n := rep.Disagreeing(); n != 0 {
			return fmt.Errorf("%d messages still disagree after the backfill: %+v", n, rep.Disagreements)
		}
		return tx.MarkAreaIndexReady(ctx)
	}); err != nil {
		t.Fatalf("mark area index ready: %v", err)
	}
	var src string
	if err := st.UserTx(ctx, userID, func(tx *UserTx) error {
		var e error
		src, e = tx.AreaSource(ctx)
		return e
	}); err != nil || src != AreaSourceStored {
		t.Fatalf("after marking ready the user's area source is %q (err %v), want stored", src, err)
	}
}

// areaBattery runs every area reader the desk and the area views use and
// returns one line per question asked, in a fixed order. Two batteries over the
// same data in different modes must be equal; each line is also counted so a
// caller can prove the battery asked real questions.
type areaBatteryResult struct {
	lines    []string
	nonEmpty int // lines whose answer held at least one id/row
}

func (b *areaBatteryResult) add(name string, nonEmpty bool, format string, args ...any) {
	b.lines = append(b.lines, name+" => "+fmt.Sprintf(format, args...))
	if nonEmpty {
		b.nonEmpty++
	}
}

func ulidPtrStr(p *ulid.ULID) string {
	if p == nil {
		return "-"
	}
	return p.String()
}

func idStrings(ids []ulid.ULID) string {
	ss := make([]string, len(ids))
	for i, id := range ids {
		ss[i] = id.String()
	}
	return strings.Join(ss, ",")
}

func runAreaBattery(t *testing.T, f *prefilterFixture) *areaBatteryResult {
	t.Helper()
	d := f.d
	ctx := context.Background()
	out := &areaBatteryResult{}
	yes, no := true, false

	var allIDs []ulid.ULID
	rows, err := d.h.super.Query(ctx, `SELECT id FROM app.messages WHERE user_id = $1 ORDER BY id`, d.userID.String())
	if err != nil {
		t.Fatal(err)
	}
	for rows.Next() {
		var s string
		if err := rows.Scan(&s); err != nil {
			t.Fatal(err)
		}
		allIDs = append(allIDs, mustULID(t, s))
	}
	rows.Close()

	names := make([]string, 0, len(f.areas))
	for n := range f.areas {
		names = append(names, n)
	}
	sort.Strings(names)
	type target struct {
		name string
		area *ulid.ULID
	}
	targets := []target{{"unsorted", nil}}
	for _, n := range names {
		id := f.areas[n]
		targets = append(targets, target{n, &id})
	}

	if err := d.st.UserTx(ctx, d.userID, func(tx *UserTx) error {
		// 1. Email/query: every shape the area views and the desk send.
		for _, tg := range targets {
			shapes := []struct {
				name string
				q    EmailQuery
			}{
				{"plain", EmailQuery{}},
				{"collapsed", EmailQuery{CollapseThreads: true, SortByEffectiveArrival: true}},
				{"collapsed page 0+50", EmailQuery{CollapseThreads: true, SortByEffectiveArrival: true, Limit: 50, LimitSet: true}},
				{"collapsed page 3+7", EmailQuery{CollapseThreads: true, SortByEffectiveArrival: true, Position: 3, Limit: 7, LimitSet: true}},
				{"collapsed asc", EmailQuery{CollapseThreads: true, SortByEffectiveArrival: true, SortAscending: true, Limit: 20, LimitSet: true}},
				{"collapsed by received_at", EmailQuery{CollapseThreads: true, Limit: 20, LimitSet: true}},
				{"collapsed total only", EmailQuery{CollapseThreads: true, SortByEffectiveArrival: true, LimitSet: true}},
				{"on desk", EmailQuery{OnDesk: &yes, CollapseThreads: true, SortByEffectiveArrival: true, Limit: 50, LimitSet: true}},
				{"on desk, account", EmailQuery{OnDesk: &yes, InAccount: &d.accountA, CollapseThreads: true, SortByEffectiveArrival: true, Limit: 50, LimitSet: true}},
				{"off desk", EmailQuery{OnDesk: &no, CollapseThreads: true, SortByEffectiveArrival: true, Limit: 50, LimitSet: true}},
				{"desk surface", EmailQuery{OnDesk: &yes, OnSurface: &yes, LineThreshold: 0.5, Snoozed: &no, CollapseThreads: true, SortByEffectiveArrival: true, Limit: 50, LimitSet: true}},
				{"desk drawer", EmailQuery{OnDesk: &yes, OnSurface: &no, LineThreshold: 0.5, Snoozed: &no, CollapseThreads: true, SortByEffectiveArrival: true, Limit: 50, LimitSet: true}},
			}
			for _, sh := range shapes {
				q := sh.q
				if tg.area == nil {
					q.Unsorted = true
				} else {
					a := *tg.area
					q.InArea = &a
				}
				ids, total, err := tx.QueryEmails(ctx, q)
				if err != nil {
					return fmt.Errorf("%s/%s: %w", tg.name, sh.name, err)
				}
				out.add("Email/query "+tg.name+" "+sh.name, len(ids) > 0, "total=%d ids=%s", total, idStrings(ids))
			}
			// 2. Search: the area atom, and its negation (two-valued).
			if tg.area != nil {
				for _, p := range []struct {
					name string
					p    SearchPredicate
				}{
					{"in", SearchPredicate{Op: SearchArea, AreaID: *tg.area}},
					{"not in", SearchPredicate{Op: SearchNot, Kids: []SearchPredicate{{Op: SearchArea, AreaID: *tg.area}}}},
				} {
					sp := p.p
					ids, total, err := tx.QueryEmails(ctx, EmailQuery{Search: &sp, Limit: 2000, LimitSet: true})
					if err != nil {
						return fmt.Errorf("search %s %s: %w", tg.name, p.name, err)
					}
					out.add("search "+tg.name+" "+p.name, len(ids) > 0, "total=%d ids=%s", total, idStrings(ids))
				}
			}
		}

		// 3. The counts, and the desk in one statement.
		var sections []DeskSectionPage
		for _, tg := range targets[1:] {
			sections = append(sections, DeskSectionPage{AreaID: tg.area, Limit: 50})
		}
		sections = append(sections, DeskSectionPage{Limit: 50})
		for _, acct := range []*ulid.ULID{nil, &d.accountA} {
			for _, th := range []float32{0.5, 0.25} {
				scope := DeskScope{AccountID: acct, Threshold: th}
				sum, err := tx.AreaSummary(ctx, scope)
				if err != nil {
					return err
				}
				out.add(fmt.Sprintf("AreaSummary account=%v th=%v", acct != nil, th), len(sum) > 0, "%s", fmtCounts(sum))
				snap, err := tx.DeskSnapshot(ctx, DeskSnapshotQuery{Scope: scope, Sections: sections})
				if err != nil {
					return err
				}
				var sb strings.Builder
				anyIDs := false
				for _, s := range snap.Sections {
					fmt.Fprintf(&sb, "[%s total=%d ids=%s]", ulidPtrStr(s.AreaID), s.Total, idStrings(s.IDs))
					anyIDs = anyIDs || len(s.IDs) > 0
				}
				out.add(fmt.Sprintf("DeskSnapshot account=%v th=%v", acct != nil, th), anyIDs, "summary=%s sections=%s", fmtCounts(snap.Summary), sb.String())
			}
		}

		// 4. Email/get's area, with which arm decided it.
		res, err := tx.ResolveAreas(ctx, allIDs)
		if err != nil {
			return err
		}
		var lines []string
		byLabel := 0
		for _, id := range allIDs {
			r, ok := res[id]
			if !ok {
				return fmt.Errorf("ResolveAreas dropped %s", id)
			}
			if r.ByLabel {
				byLabel++
			}
			lines = append(lines, fmt.Sprintf("%s:%s:%v", id, ulidPtrStr(r.AreaID), r.ByLabel))
		}
		out.add("ResolveAreas all", byLabel > 0, "%s", strings.Join(lines, ";"))
		return nil
	}); err != nil {
		t.Fatalf("battery: %v", err)
	}

	// 5. The drawer sweep and its report. Every area auto-archives, the clock is
	// far enough ahead that every desk message is old enough.
	if _, err := d.h.super.Exec(ctx, `UPDATE app.areas SET auto_archive = true, auto_archive_days = 1 WHERE user_id = $1`, d.userID.String()); err != nil {
		t.Fatal(err)
	}
	now := time.Now().UTC().Add(400 * 24 * time.Hour)
	if err := d.st.UserTx(ctx, d.userID, func(tx *UserTx) error {
		cands, err := tx.DrawerCandidates(ctx, now, 100000)
		if err != nil {
			return err
		}
		var parts []string
		for _, c := range cands {
			parts = append(parts, fmt.Sprintf("%s>%s", c.ID, c.AreaID))
		}
		sort.Strings(parts)
		out.add("DrawerCandidates", len(cands) > 0, "%s", strings.Join(parts, ","))
		for _, c := range cands {
			ok, err := tx.drawerCandidateStillQualifies(ctx, c.ID, c.AreaID, 0.5)
			if err != nil {
				return err
			}
			if !ok {
				return fmt.Errorf("a candidate the sweep just listed no longer qualifies on recheck: %s", c.ID)
			}
		}
		// ...and a message in another area does not.
		if len(cands) > 0 {
			var other ulid.ULID
			for _, tg := range targets[1:] {
				if *tg.area != cands[0].AreaID {
					other = *tg.area
					break
				}
			}
			ok, err := tx.drawerCandidateStillQualifies(ctx, cands[0].ID, other, 0.5)
			if err != nil {
				return err
			}
			out.add("drawerCandidateStillQualifies wrong area", ok, "%v", ok)
		}
		rep, err := tx.DrawerReport(ctx, now)
		if err != nil {
			return err
		}
		out.add("DrawerReport", rep.NeedsSorting.Candidates > 0, "%s", drawerReportString(rep))
		return nil
	}); err != nil {
		t.Fatalf("battery (drawer): %v", err)
	}

	// 6. The placement backfill's read-only plan resolves every message too.
	if err := d.st.UserReadTx(ctx, d.userID, func(tx *UserTx) error {
		plan, err := tx.PlanPlacementBackfill(ctx, PlacementBackfillOptions{})
		if err != nil {
			return err
		}
		out.add("PlanPlacementBackfill", plan.Held+plan.NoOp+plan.OffDesk > 0, "held=%d noop=%d offdesk=%d moves=%d",
			plan.Held, plan.NoOp, plan.OffDesk, len(plan.Moves))
		return nil
	}); err != nil {
		t.Fatalf("battery (placement): %v", err)
	}
	return out
}

func diffBatteries(a, b *areaBatteryResult) []string {
	var diffs []string
	if len(a.lines) != len(b.lines) {
		diffs = append(diffs, fmt.Sprintf("%d lines vs %d lines", len(a.lines), len(b.lines)))
	}
	for i := 0; i < len(a.lines) && i < len(b.lines); i++ {
		if a.lines[i] != b.lines[i] {
			diffs = append(diffs, fmt.Sprintf("%s\n    resolver: %s\n    stored:   %s", strings.SplitN(a.lines[i], " => ", 2)[0],
				trunc(strings.SplitN(a.lines[i], " => ", 2)[1], 400), trunc(strings.SplitN(b.lines[i], " => ", 2)[1], 400)))
		}
	}
	return diffs
}

func TestStoredAreaReadersAgreeWithTheResolver(t *testing.T) {
	oldBase := collapseBatchBase
	collapseBatchBase = 2 // the bounded page loops many times per query
	t.Cleanup(func() { collapseBatchBase = oldBase })

	f := seedPrefilterFixture(t)
	d := f.d
	ctx := context.Background()

	resolver := runAreaBattery(t, f)
	if err := d.st.UserTx(ctx, d.userID, func(tx *UserTx) error {
		src, err := tx.AreaSource(ctx)
		if src != AreaSourceResolver {
			return fmt.Errorf("fixture user's source is %q before the backfill (err %v)", src, err)
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	readyAreaIndex(t, d.st, d.userID)
	stored := runAreaBattery(t, f)

	if diffs := diffBatteries(resolver, stored); len(diffs) > 0 {
		t.Errorf("%d of %d answers differ between the resolver and the stored column:\n%s", len(diffs), len(resolver.lines), strings.Join(diffs, "\n"))
	}
	// Agreement between empty answers proves nothing: most of the battery must
	// have returned something, and the arms that matter must be present.
	if resolver.nonEmpty < len(resolver.lines)/2 {
		t.Fatalf("only %d of %d battery answers were non-empty: the agreement is mostly between empty results", resolver.nonEmpty, len(resolver.lines))
	}
	for _, name := range []string{"DrawerCandidates", "ResolveAreas all", "PlanPlacementBackfill"} {
		found := false
		for _, l := range resolver.lines {
			if strings.HasPrefix(l, name+" =>") && !strings.HasSuffix(l, "=> ") {
				found = true
			}
		}
		if !found {
			t.Errorf("battery line %q missing", name)
		}
	}
	t.Logf("compared %d answers (%d non-empty) between the two modes", len(resolver.lines), resolver.nonEmpty)
}

// TestAreaReadersAreGatedOnReadiness: until a user is ready, no reader sends a
// statement that reads the stored column; once ready, they do. The second half
// is the positive control: without it the first half would pass for a reader
// that never learned to use the column at all.
func TestAreaReadersAreGatedOnReadiness(t *testing.T) {
	f := seedPrefilterFixture(t)
	d := f.d
	d.rec.stmts = nil
	runAreaBattery(t, f)
	for _, s := range d.rec.stmts {
		if strings.Contains(s.sql, "m.area_id") && !strings.Contains(s.sql, "area_index_state") {
			t.Fatalf("a user who is not ready sent a statement that reads the stored column:\n%s", s.sql)
		}
	}
	resolverStmts := 0
	for _, s := range d.rec.stmts {
		if strings.Contains(s.sql, "area_la.area_id") {
			resolverStmts++
		}
	}
	if resolverStmts == 0 {
		t.Fatal("no resolver statement was recorded for a user who is not ready")
	}

	readyAreaIndex(t, d.st, d.userID)
	d.rec.stmts = nil
	runAreaBattery(t, f)
	storedStmts := 0
	for _, s := range d.rec.stmts {
		if strings.Contains(s.sql, "m.area_id") {
			storedStmts++
		}
	}
	if storedStmts == 0 {
		t.Fatal("a ready user sent no statement that reads the stored column: the gate test above proves nothing")
	}
	// The readers that scope the resolver to the desk must not run it at all.
	for _, marker := range []string{"/* collapsed page */", "WITH desk AS"} {
		found := false
		for _, s := range d.rec.stmts {
			if strings.Contains(s.sql, marker) {
				found = true
				if strings.Contains(s.sql, "area_la") {
					t.Errorf("a ready user's statement with %q still runs the resolver:\n%s", marker, s.sql)
				}
			}
		}
		if !found {
			t.Errorf("no statement with %q recorded for a ready user", marker)
		}
	}
}

// TestStoredAreaReadersDetectACorruptedColumn is the negative control for the
// agreement test: wrong stored values must change the battery. Only the
// column is touched (the update trigger watches classified_tab, not area_id).
func TestStoredAreaReadersDetectACorruptedColumn(t *testing.T) {
	f := seedPrefilterFixture(t)
	d := f.d
	ctx := context.Background()
	resolver := runAreaBattery(t, f)
	readyAreaIndex(t, d.st, d.userID)
	if diffs := diffBatteries(resolver, runAreaBattery(t, f)); len(diffs) > 0 {
		t.Fatalf("control cannot start: the modes already differ:\n%s", strings.Join(diffs, "\n"))
	}
	tag, err := d.h.super.Exec(ctx, `
		UPDATE app.messages m SET area_id = CASE WHEN m.area_id IS NULL THEN $2::app.ulid ELSE NULL END
		 WHERE m.user_id = $1 AND m.id IN (SELECT id FROM app.messages WHERE user_id = $1 ORDER BY id LIMIT 40)`,
		d.userID.String(), f.areas["Empty"].String())
	if err != nil || tag.RowsAffected() == 0 {
		t.Fatalf("corrupt the column: %v (%d rows)", err, tag.RowsAffected())
	}
	if diffs := diffBatteries(resolver, runAreaBattery(t, f)); len(diffs) == 0 {
		t.Fatal("the battery stayed identical with the stored column corrupted: it cannot see a stale column")
	}
}

// TestBoundedPageIsOnlyTheStoredPath: the bounded keyset page serves a ready
// user's thread-collapsed arrival-ordered area list and nobody else's, and the
// statement is one messages_user_area_arrival can serve in the list's own order.
func TestBoundedPageIsOnlyTheStoredPath(t *testing.T) {
	oldBase := collapseBatchBase
	collapseBatchBase = 2
	t.Cleanup(func() { collapseBatchBase = oldBase })

	f := seedPrefilterFixture(t)
	d := f.d
	ctx := context.Background()
	area := f.areas["Promotions"]
	q := EmailQuery{InArea: &area, CollapseThreads: true, SortByEffectiveArrival: true, Limit: 20, LimitSet: true}
	run := func() {
		t.Helper()
		d.rec.stmts = nil
		if err := d.st.UserTx(ctx, d.userID, func(tx *UserTx) error {
			_, _, err := tx.QueryEmails(ctx, q)
			return err
		}); err != nil {
			t.Fatal(err)
		}
	}
	has := func(marker string) bool {
		for _, s := range d.rec.stmts {
			if strings.Contains(s.sql, marker) {
				return true
			}
		}
		return false
	}

	run()
	if has("/* collapsed page */") {
		t.Fatal("a user who is not ready was served by the bounded page: every batch would re-run the resolver")
	}
	if !has("count(*) OVER () AS total FROM ranked") {
		t.Fatal("a user who is not ready did not get the full-ranking statement")
	}

	readyAreaIndex(t, d.st, d.userID)
	run()
	if !has("/* collapsed page */") {
		t.Fatal("a ready user was not served by the bounded page")
	}
	for _, s := range d.rec.stmts {
		if strings.Contains(s.sql, "/* collapsed page */") && !strings.Contains(s.sql, "m.area_id") {
			t.Fatalf("a bounded batch does not filter on the stored area:\n%s", s.sql)
		}
	}
	// Other sorts keep the full ranking, whatever the readiness.
	q2 := q
	q2.SortByEffectiveArrival = false
	d.rec.stmts = nil
	if err := d.st.UserTx(ctx, d.userID, func(tx *UserTx) error {
		_, _, err := tx.QueryEmails(ctx, q2)
		return err
	}); err != nil {
		t.Fatal(err)
	}
	if has("/* collapsed page */") || !has("count(*) OVER () AS total FROM ranked") {
		t.Fatal("a received_at-ordered list left the full ranking, which the arrival index does not serve")
	}

	// The batch statement, planned with sequential scans off, walks the area
	// index and needs no Sort: it is a range scan in the list's own order.
	run()
	stmt := d.rec.find(t, "/* collapsed page */")
	if err := d.st.UserTx(ctx, d.userID, func(tx *UserTx) error {
		if _, err := tx.tx.Exec(ctx, `SELECT set_config('enable_seqscan', 'off', true), set_config('enable_bitmapscan', 'off', true), set_config('enable_sort', 'off', true)`); err != nil {
			return err
		}
		plan, err := tx.ExplainAnalyze(ctx, CapturedStatement{SQL: stmt.sql, Args: stmt.args})
		if err != nil {
			return err
		}
		if !strings.Contains(plan, "messages_user_area_arrival") {
			t.Errorf("the bounded batch does not use messages_user_area_arrival:\n%s", plan)
		}
		if strings.Contains(plan, "Sort") {
			t.Errorf("the bounded batch sorts; the index should give the list's order:\n%s", plan)
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
}

// drawerReportString prints a report without its area-id pointers.
func drawerReportString(r DrawerReportRows) string {
	var sb strings.Builder
	for _, a := range append(append([]DrawerAreaReport{}, r.Areas...), r.NeedsSorting) {
		fmt.Fprintf(&sb, "[%s %s period=%d/%v cand=%d oldest=%v newest=%v excl=%+v]", ulidPtrStr(a.AreaID), a.AreaName,
			a.PeriodDays, a.PeriodSource, a.Candidates, timePtrStr(a.Oldest), timePtrStr(a.Newest), a.Excluded)
	}
	return sb.String()
}

func timePtrStr(p *time.Time) string {
	if p == nil {
		return "-"
	}
	return p.UTC().Format(time.RFC3339Nano)
}

// TestAnUnbackfilledColumnIsNeverRead is the gate's own negative control: the
// column is wiped (the state of every user the moment the migration lands) and
// the user is not ready, so every reader must still answer from the resolver.
// With the readiness check skipped (always stored) this goes red.
func TestAnUnbackfilledColumnIsNeverRead(t *testing.T) {
	f := seedPrefilterFixture(t)
	d := f.d
	ctx := context.Background()
	baseline := runAreaBattery(t, f)
	tag, err := d.h.super.Exec(ctx, `UPDATE app.messages SET area_id = NULL WHERE user_id = $1 AND area_id IS NOT NULL`, d.userID.String())
	if err != nil || tag.RowsAffected() == 0 {
		t.Fatalf("wipe the column: %v (%d rows)", err, tag.RowsAffected())
	}
	if diffs := diffBatteries(baseline, runAreaBattery(t, f)); len(diffs) > 0 {
		t.Fatalf("a user who is not ready reads the stored column (%d answers changed when it was wiped):\n%s", len(diffs), strings.Join(diffs, "\n"))
	}
}

// TestStoredAreaReadersDetectAMissingTrigger: with the label trigger dropped,
// labels added afterwards leave the column stale. A user marked ready WITHOUT
// the backfill (the operator skipping it) reads the stale values, and the
// two-mode battery must say so. This is the control that the agreement tests
// above would go red if the triggers stopped keeping the column right.
func TestStoredAreaReadersDetectAMissingTrigger(t *testing.T) {
	f := seedPrefilterFixture(t)
	d := f.d
	ctx := context.Background()
	var def string
	if err := d.h.super.QueryRow(ctx, `SELECT pg_get_triggerdef(oid) FROM pg_trigger
		WHERE tgname='message_labels_area_ins' AND tgrelid='app.message_labels'::regclass`).Scan(&def); err != nil {
		t.Fatal(err)
	}
	if _, err := d.h.super.Exec(ctx, `DROP TRIGGER message_labels_area_ins ON app.message_labels`); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if _, err := d.h.super.Exec(ctx, def); err != nil {
			t.Errorf("restore trigger: %v", err)
		}
	})
	// Give 60 messages the label of the Empty area: they now resolve there, and
	// the column (not updated) still says what it said before.
	var emptyLabel string
	if err := d.h.super.QueryRow(ctx, `SELECT label_id FROM app.areas WHERE user_id = $1 AND id = $2`,
		d.userID.String(), f.areas["Empty"].String()).Scan(&emptyLabel); err != nil {
		t.Fatal(err)
	}
	tag, err := d.h.super.Exec(ctx, `
		INSERT INTO app.message_labels (user_id, message_id, label_id)
		SELECT user_id, id, $2 FROM app.messages WHERE user_id = $1 AND trashed_at IS NULL ORDER BY id LIMIT 60
		ON CONFLICT DO NOTHING`, d.userID.String(), emptyLabel)
	if err != nil || tag.RowsAffected() == 0 {
		t.Fatalf("add labels: %v (%d rows)", err, tag.RowsAffected())
	}
	resolver := runAreaBattery(t, f)
	if err := d.st.UserTx(ctx, d.userID, func(tx *UserTx) error { return tx.MarkAreaIndexReady(ctx) }); err != nil {
		t.Fatal(err)
	}
	if diffs := diffBatteries(resolver, runAreaBattery(t, f)); len(diffs) == 0 {
		t.Fatal("the battery stayed identical with stale stored areas: the two-mode comparison cannot see a missing trigger")
	}
}
