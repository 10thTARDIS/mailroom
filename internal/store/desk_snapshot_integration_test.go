// SPDX-License-Identifier: AGPL-3.0-only

//go:build integration

package store

// THE DESK IN ONE PASS (#665) — agreement and scale.
//
// DeskSnapshot replaces Area/getSummary plus one Email/query per desk section
// with one statement that resolves every message's area once. Faster is only
// worth having if it is the SAME answer, so:
//
//  1. AGREEMENT (TestDeskSnapshotAgreesWithQueryEmailsAndAreaSummary): for every
//     section, at several windows, two thresholds and both scopes, the ids and
//     total equal what QueryEmails returns for the EmailQuery the web client's
//     sectionFilter means — written out BY HAND here, not taken from
//     DeskSectionQuery, so a defect there cannot hide by being on both sides —
//     and the summary equals AreaSummary. The fixture is seedDeskScale's (every
//     shape the desk predicate and resolver distinguish) plus the shapes only
//     the per-section predicates distinguish: junk, Mailroom-trashed, drafts,
//     vanished, a copy in Trash beside a live INBOX copy, snoozes that have
//     already woken, and wake times that reorder the returnedAt sort.
//  2. SCALE (TestDeskSnapshotResolvesOnceAtScale): on the 20k fixture, the old
//     desk (summary + one query per section) against DeskSnapshot, both timed
//     and reported; the snapshot's plan resolves areas exactly once and does no
//     per-message work.

import (
	"context"
	"encoding/json"
	"fmt"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/oklog/ulid/v2"
)

// deskAgreementExtras adds the shapes seedDeskScale does not have, each to a
// deterministic slice of the user's messages (ordered by received_at, so the
// slices land across areas, sides and threads). Returns how many messages each
// shape touched, so the test can refuse a fixture in which one is empty.
func deskAgreementExtras(t *testing.T, d *deskScale) map[string]int64 {
	t.Helper()
	ctx := context.Background()
	u := d.userID.String()
	shapes := []struct{ name, sql string }{
		{"junk", `UPDATE app.messages m SET is_junk = true FROM n WHERE m.id = n.id AND n.k % 31 = 3`},
		{"trashed in Mailroom", `UPDATE app.messages m SET trashed_at = now(), purge_after = now() + interval '30 days'
		   FROM n WHERE m.id = n.id AND n.k % 37 = 4`},
		{"draft", `UPDATE app.messages m SET is_draft = true FROM n WHERE m.id = n.id AND n.k % 41 = 5`},
		{"vanished", `UPDATE app.messages m SET vanished_at = now() FROM n WHERE m.id = n.id AND n.k % 43 = 6`},
		{"woken snooze", `UPDATE app.messages m SET snooze_woke_at = now() - (n.k % 7) * interval '1 minute'
		   FROM n WHERE m.id = n.id AND n.k % 13 = 7`},
		{"snooze already due", `UPDATE app.messages m SET snoozed_until = now() - interval '1 hour'
		   FROM n WHERE m.id = n.id AND n.k % 47 = 8`},
		{"snoozed", `UPDATE app.messages m SET snoozed_until = now() + interval '2 days'
		   FROM n WHERE m.id = n.id AND n.k % 29 = 9`},
	}
	out := map[string]int64{}
	for _, s := range shapes {
		tag, err := d.h.super.Exec(ctx, `WITH n AS (
			SELECT id, (row_number() OVER (ORDER BY received_at))::int AS k FROM app.messages WHERE user_id = $1)
			`+s.sql, u)
		if err != nil {
			t.Fatalf("extra shape %q: %v", s.name, err)
		}
		out[s.name] = tag.RowsAffected()
	}
	// A second, live copy in Trash beside a live INBOX copy: on the desk by
	// provenance, deleted by the trash-folder arm (#631). Only the per-section
	// Email/query sees the difference, never AreaSummary. Ids are made here
	// because app.ulid is CHECKed and SQL has no ULID generator.
	rows, err := d.h.super.Query(ctx, `
		WITH n AS (SELECT id, (row_number() OVER (ORDER BY received_at))::int AS k
		             FROM app.messages WHERE user_id = $1)
		SELECT p.message_id, p.account_id, tr.id, n.k
		  FROM n JOIN app.message_provenance p ON p.message_id = n.id AND p.state = 'live'
		  JOIN app.mailboxes mb ON mb.id = p.mailbox_id AND upper(mb.imap_name) = 'INBOX'
		  JOIN app.mailboxes tr ON tr.user_id = p.user_id AND tr.account_id = p.account_id
		                       AND tr.special_use = '\Trash'
		 WHERE n.k % 53 = 10`, u)
	if err != nil {
		t.Fatal(err)
	}
	var pid, pmsg, pacct, pmb []string
	var puid []int64
	for rows.Next() {
		var msg, acct, mb string
		var k int64
		if err := rows.Scan(&msg, &acct, &mb, &k); err != nil {
			t.Fatal(err)
		}
		pid, pmsg, pacct, pmb, puid = append(pid, NewULID().String()), append(pmsg, msg),
			append(pacct, acct), append(pmb, mb), append(puid, 900000+k)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		t.Fatal(err)
	}
	if _, err := d.h.super.Exec(ctx, `
		INSERT INTO app.message_provenance (id, user_id, message_id, account_id, mailbox_id, uid, uidvalidity, state)
		SELECT id, $1, m, a, mb, uid, 1, 'live'
		  FROM unnest($2::text[], $3::text[], $4::text[], $5::text[], $6::bigint[]) AS x(id, m, a, mb, uid)`,
		u, pid, pmsg, pacct, pmb, puid); err != nil {
		t.Fatal(err)
	}
	out["copy in Trash"] = int64(len(pid))
	for _, tbl := range []string{"messages", "message_provenance"} {
		if _, err := d.h.super.Exec(ctx, `ANALYZE app.`+tbl); err != nil {
			t.Fatal(err)
		}
	}
	return out
}

// sectionByHand is the Email/query the web client sends for a Review-first
// section (web/src/core/desk.ts sectionFilter, DEFAULT_SORT returnedAt desc,
// collapseThreads), written out independently of DeskSectionQuery.
func sectionByHand(areaID *ulid.ULID, account *ulid.ULID, threshold float32, position, limit int) EmailQuery {
	yes, no := true, false
	q := EmailQuery{
		OnDesk: &yes, Snoozed: &no, InAccount: account,
		CollapseThreads: true, SortByEffectiveArrival: true,
		Position: position, Limit: limit, LimitSet: true,
	}
	if areaID == nil {
		q.Unsorted = true
	} else {
		q.InArea, q.OnSurface, q.LineThreshold = areaID, &yes, threshold
	}
	return q
}

func TestDeskSnapshotAgreesWithQueryEmailsAndAreaSummary(t *testing.T) {
	d := seedDeskScale(t, 1500)
	shapes := deskAgreementExtras(t, d)
	for name, n := range shapes {
		if n == 0 {
			t.Fatalf("fixture shape %q touched no message — an agreement over an absent shape proves nothing", name)
		}
	}
	t.Logf("extra shapes: %v", shapes)
	ctx := context.Background()

	var areaIDs []*ulid.ULID
	if err := d.st.UserTx(ctx, d.userID, func(tx *UserTx) error {
		views, err := tx.ListAreas(ctx)
		for _, v := range views {
			areaIDs = append(areaIDs, &v.ID)
		}
		return err
	}); err != nil {
		t.Fatal(err)
	}
	areaIDs = append(areaIDs, nil) // Needs sorting, last — as the client asks

	// A first page, a small offset window (mixed with 3 below, so each
	// section's LIMIT is its own), an empty window, and a position past the
	// end — the four shapes whose totals and ids are computed differently.
	windows := []struct{ position, limit int }{{0, 50}, {5, 7}, {0, 0}, {10000, 5}}
	var (
		compared, nonEmpty, windowed, collapsed int
	)
	for _, account := range []*ulid.ULID{nil, &d.accountA} {
		for _, threshold := range []float32{0.5, 0.25} {
			scope := DeskScope{AccountID: account, Threshold: threshold}
			for _, w := range windows {
				name := fmt.Sprintf("account=%v threshold=%v window=%d+%d", account != nil, threshold, w.position, w.limit)
				// Each section gets a different window in some runs, so a
				// mix-up of which LIMIT belongs to which section cannot pass.
				var sections []DeskSectionPage
				for i, a := range areaIDs {
					lim := w.limit
					if w.limit == 7 && i%2 == 1 {
						lim = 3
					}
					sections = append(sections, DeskSectionPage{AreaID: a, Position: w.position, Limit: lim})
				}
				var (
					snap    DeskSnapshotResult
					summary []AreaCounts
				)
				if err := d.st.UserTx(ctx, d.userID, func(tx *UserTx) error {
					var err error
					if snap, err = tx.DeskSnapshot(ctx, DeskSnapshotQuery{Scope: scope, Sections: sections}); err != nil {
						return err
					}
					summary, err = tx.AreaSummary(ctx, scope)
					return err
				}); err != nil {
					t.Fatalf("%s: %v", name, err)
				}
				if !slices.EqualFunc(snap.Summary, summary, sameCounts) {
					t.Errorf("%s: DeskSnapshot summary %s, AreaSummary %s", name, fmtCounts(snap.Summary), fmtCounts(summary))
				}
				if len(snap.Sections) != len(sections) {
					t.Fatalf("%s: %d sections for %d asked", name, len(snap.Sections), len(sections))
				}
				for i, sec := range sections {
					want := sectionByHand(sec.AreaID, account, threshold, sec.Position, sec.Limit)
					var (
						ids   []ulid.ULID
						total int
						flat  int
					)
					if err := d.st.UserTx(ctx, d.userID, func(tx *UserTx) error {
						var err error
						if ids, total, err = tx.QueryEmails(ctx, want); err != nil {
							return err
						}
						if w.limit != 50 {
							return nil // the uncollapsed count only feeds the non-vacuity check
						}
						flatQ := want
						flatQ.CollapseThreads, flatQ.Limit, flatQ.Position = false, 0, 0
						_, flat, err = tx.QueryEmails(ctx, flatQ)
						return err
					}); err != nil {
						t.Fatalf("%s: query: %v", name, err)
					}
					got := snap.Sections[i]
					if !ulidEq(got.AreaID, sec.AreaID) {
						t.Errorf("%s section %d: answered for area %v, asked %v", name, i, got.AreaID, sec.AreaID)
					}
					if got.Total != total || !slices.Equal(got.IDs, ids) {
						t.Errorf("%s section %d (area %v): DeskSnapshot total %d ids %v; QueryEmails total %d ids %v",
							name, i, sec.AreaID, got.Total, got.IDs, total, ids)
					}
					compared++
					if total > 0 {
						nonEmpty++
					}
					if total > sec.Position+sec.Limit && sec.Limit > 0 {
						windowed++
					}
					if flat > total {
						collapsed++
					}
				}
			}
		}
	}
	t.Logf("compared %d sections: %d non-empty, %d cut by their window, %d where collapsing threads changed the total",
		compared, nonEmpty, windowed, collapsed)
	// Non-vacuity: an agreement between two empty answers, or between two
	// answers no window and no collapse ever touched, would pass whatever the
	// code did.
	if nonEmpty < compared/2 || windowed == 0 || collapsed == 0 {
		t.Fatalf("the fixture does not exercise the sections: %d/%d non-empty, %d windowed, %d collapsed",
			nonEmpty, compared, windowed, collapsed)
	}
}

func sameCounts(a, b AreaCounts) bool {
	return ulidEq(a.AreaID, b.AreaID) && a.Surface == b.Surface && a.Drawer == b.Drawer && a.Unscored == b.Unscored
}

func ulidEq(a, b *ulid.ULID) bool {
	if a == nil || b == nil {
		return a == nil && b == nil
	}
	return *a == *b
}

func fmtCounts(cs []AreaCounts) string {
	var b strings.Builder
	for _, c := range cs {
		fmt.Fprintf(&b, "[%v %d/%d/%d]", c.AreaID, c.Surface, c.Drawer, c.Unscored)
	}
	return b.String()
}

// resolverPasses counts, in an EXPLAIN (ANALYZE, FORMAT JSON) plan, the area
// resolver's DISTINCT ON derivations: Unique nodes with a scan of
// app.message_labels beneath them. One resolver pass is exactly two (area_la,
// area_lt). Nothing else in a desk statement reads message_labels. It also
// returns the most times any of them executed, which must be 1.
func resolverPasses(t *testing.T, ctx context.Context, tx *UserTx, sql string, args []any) (passes int, maxLoops float64, labelScans int) {
	t.Helper()
	var raw []byte
	if err := tx.tx.QueryRow(ctx, "EXPLAIN (ANALYZE, FORMAT JSON) "+sql, args...).Scan(&raw); err != nil {
		t.Fatalf("explain: %v", err)
	}
	var doc []struct {
		Plan explainNode `json:"Plan"`
	}
	if err := json.Unmarshal(raw, &doc); err != nil || len(doc) != 1 {
		t.Fatalf("parse explain: %v", err)
	}
	var readsLabels func(n explainNode) bool
	readsLabels = func(n explainNode) bool {
		if n.Relation == "message_labels" {
			return true
		}
		for _, c := range n.Plans {
			if readsLabels(c) {
				return true
			}
		}
		return false
	}
	var walk func(n explainNode)
	walk = func(n explainNode) {
		if n.Relation == "message_labels" {
			labelScans++
		}
		if n.NodeType == "Unique" && readsLabels(n) {
			passes++
			if n.Loops > maxLoops {
				maxLoops = n.Loops
			}
		}
		for _, c := range n.Plans {
			walk(c)
		}
	}
	walk(doc[0].Plan)
	return passes, maxLoops, labelScans
}

func TestDeskSnapshotResolvesOnceAtScale(t *testing.T) {
	d := seedDeskScale(t, deskScaleN)
	ctx := context.Background()
	threshold := float32(0.5)

	var areaIDs []*ulid.ULID
	if err := d.st.UserTx(ctx, d.userID, func(tx *UserTx) error {
		views, err := tx.ListAreas(ctx)
		for _, v := range views {
			areaIDs = append(areaIDs, &v.ID)
		}
		return err
	}); err != nil {
		t.Fatal(err)
	}
	areaIDs = append(areaIDs, nil)
	scope := DeskScope{Threshold: threshold}
	var sections []DeskSectionPage
	for _, a := range areaIDs {
		sections = append(sections, DeskSectionPage{AreaID: a, Limit: 50})
	}

	// Every statement here runs under a statement_timeout, so a plan that
	// goes pathological is a reported finding with its plan, not a hung CI
	// job. DeskSnapshot is measured FIRST: it is the subject.
	const stmtTimeout = 20 * time.Second
	limited := func(fn func(tx *UserTx) error) error {
		return d.st.UserTx(ctx, d.userID, func(tx *UserTx) error {
			if _, err := tx.tx.Exec(ctx, fmt.Sprintf("SET LOCAL statement_timeout = %d", stmtTimeout.Milliseconds())); err != nil {
				return err
			}
			return fn(tx)
		})
	}
	explain := func(stmt recordedSQL) string {
		var b strings.Builder
		err := d.st.UserTx(ctx, d.userID, func(tx *UserTx) error {
			rows, err := tx.tx.Query(ctx, "EXPLAIN "+stmt.sql, stmt.args...)
			if err != nil {
				return err
			}
			defer rows.Close()
			for rows.Next() {
				var line string
				_ = rows.Scan(&line)
				b.WriteString(line + "\n")
			}
			return rows.Err()
		})
		if err != nil {
			return "EXPLAIN failed: " + err.Error()
		}
		return b.String()
	}

	var snap DeskSnapshotResult
	newTook := timed(t, "DeskSnapshot (one statement)", func() error {
		err := limited(func(tx *UserTx) error {
			var err error
			snap, err = tx.DeskSnapshot(ctx, DeskSnapshotQuery{Scope: scope, Sections: sections})
			return err
		})
		if err != nil {
			t.Fatalf("DeskSnapshot: %v\nplan:\n%s", err, explain(d.rec.find(t, "WITH desk AS MATERIALIZED")))
		}
		return nil
	})
	stmt := d.rec.find(t, "WITH desk AS MATERIALIZED")

	// THE OLD DESK: Area/getSummary, then one Email/query per section — each
	// its own method call, so its own transaction, as the API ran them. It is
	// the baseline, not the subject, so it is bounded: best of two passes, and
	// a section over the timeout is reported with its plan, counted AT the
	// timeout (the old number becomes a lower bound), and ends the timing.
	//
	// Not hypothetical. In the full store suite — many other users' rows in
	// the same tables — the per-section Email/query stalls past the timeout
	// from its SIXTH execution on a connection, which is when Postgres's plan
	// cache starts weighing a generic plan; forced generic, every section
	// stalls from the first. DeskSnapshot plans its statement custom every
	// time (asserted below) and took about half a second on the same data.
	// Since #697 the per-section Email/query is forced custom as well (also
	// asserted below), so this baseline no longer stalls; it still pays one
	// area resolution per section, which is what DeskSnapshot removes.
	var (
		oldTotals    []int
		oldTimedOut  []int
		bestTimedOut []int
		oldTook      = time.Duration(1<<63 - 1)
		// oldOne is the Needs-sorting statement the old path ran, kept for
		// the resolver-count control below.
		oldOne recordedSQL
	)
	for pass := 0; pass < 2 && len(oldTimedOut) == 0; pass++ {
		start := time.Now()
		var passTimedOut []int
		oldTotals = oldTotals[:0]
		if err := limited(func(tx *UserTx) error {
			_, err := tx.AreaSummary(ctx, scope)
			return err
		}); err != nil {
			t.Fatalf("old path, summary: %v", err)
		}
		for i, s := range sections {
			n := -1
			err := limited(func(tx *UserTx) error {
				var err error
				_, n, err = tx.QueryEmails(ctx, sectionByHand(s.AreaID, nil, threshold, 0, 50))
				return err
			})
			if oldOne.sql == "" && s.AreaID == nil {
				// Before anything below runs EXPLAIN, whose own text also
				// carries the marker. Needs sorting's, because it is the one
				// section that still resolves every message: an area's
				// section resolves only its candidates (#706), whose own
				// message_labels scans the control's count would include.
				oldOne = d.rec.find(t, "count(*) OVER () AS total FROM ranked")
			}
			if err != nil && strings.Contains(err.Error(), "57014") {
				passTimedOut = append(passTimedOut, i)
				t.Logf("old path, pass %d section %d: the per-section Email/query exceeded %v. "+
					"EXPLAIN of it now (a fresh custom plan, which need not be the plan that ran):\n%s",
					pass, i, stmtTimeout, explain(d.rec.find(t, "count(*) OVER () AS total FROM ranked")))
				n = -1
			} else if err != nil {
				t.Fatalf("old path, section %d: %v", i, err)
			}
			oldTotals = append(oldTotals, n)
		}
		oldTimedOut = append(oldTimedOut, passTimedOut...)
		if took := time.Since(start); took < oldTook {
			oldTook, bestTimedOut = took, passTimedOut
		}
	}
	t.Logf("%-40s %6d ms", fmt.Sprintf("old desk (summary + %d queries)", len(sections)), oldTook.Milliseconds())
	note := ""
	switch {
	case len(bestTimedOut) > 0:
		note = fmt.Sprintf(" — a LOWER BOUND: sections %v hit the %v timeout", bestTimedOut, stmtTimeout)
	case len(oldTimedOut) > 0:
		note = fmt.Sprintf(" (a complete pass; on another pass sections %v exceeded %v)", oldTimedOut, stmtTimeout)
	}
	t.Logf("desk load at %d messages, %d sections: old %d ms%s, DeskSnapshot %d ms (%.1fx)",
		deskScaleN, len(sections), oldTook.Milliseconds(), note, newTook.Milliseconds(),
		float64(oldTook)/float64(max(newTook, time.Millisecond)))

	for i, s := range snap.Sections {
		if oldTotals[i] >= 0 && s.Total != oldTotals[i] {
			t.Errorf("section %d: DeskSnapshot total %d, old path %d", i, s.Total, oldTotals[i])
		}
		if s.Total == 0 {
			t.Errorf("section %d is empty at scale — a timing over an empty section proves nothing", i)
		}
	}
	// TIME, as backstops (the structure below is the gate). A whole desk is
	// several of the statements deskScaleBound bounds one at a time, so it
	// gets twice that; and it must beat the path it replaces, measured in the
	// same run on the same host — the comparison that survives a busy runner.
	if newTook > 2*deskScaleBound {
		t.Errorf("DeskSnapshot took %v, bound %v", newTook, 2*deskScaleBound)
	}
	if newTook >= oldTook {
		t.Errorf("DeskSnapshot (%v) is not faster than the per-section desk it replaces (%v)", newTook, oldTook)
	}

	// NEVER A GENERIC PLAN. The snapshot's text repeats on every load, so the
	// statement cache would otherwise hand Postgres the same prepared
	// statement until it settled on a generic plan (DeskSnapshot's ⚠ comment
	// has the measurement). Run it past the five-execution threshold, then
	// ask the server how this connection's copy was planned: the traced pool
	// has one connection, so it is the statement DeskSnapshot used.
	// Half the runs come from a caller that has itself asked for generic plans
	// — the strongest way to see that DeskSnapshot plans its own statement,
	// and that it hands the caller's setting back afterwards.
	for i := 0; i < 6; i++ {
		if err := limited(func(tx *UserTx) error {
			want := "auto"
			if i%2 == 1 {
				want = "force_generic_plan"
				if _, err := tx.tx.Exec(ctx, "SET LOCAL plan_cache_mode = force_generic_plan"); err != nil {
					return err
				}
			}
			if _, err := tx.DeskSnapshot(ctx, DeskSnapshotQuery{Scope: scope, Sections: sections}); err != nil {
				return err
			}
			var mode string
			if err := tx.tx.QueryRow(ctx, `SELECT current_setting('plan_cache_mode')`).Scan(&mode); err != nil {
				return err
			}
			if mode != want {
				t.Errorf("plan_cache_mode after DeskSnapshot = %q, want the caller's %q restored", mode, want)
			}
			return nil
		}); err != nil {
			t.Fatalf("DeskSnapshot: %v", err)
		}
	}
	if err := d.st.UserTx(ctx, d.userID, func(tx *UserTx) error {
		var generic, custom int64
		if err := tx.tx.QueryRow(ctx, `SELECT coalesce(sum(generic_plans), 0), coalesce(sum(custom_plans), 0)
			  FROM pg_prepared_statements
			 WHERE statement LIKE '%WITH desk AS MATERIALIZED%'
			   AND statement NOT LIKE '%pg_prepared_statements%'`).Scan(&generic, &custom); err != nil {
			return err
		}
		t.Logf("DeskSnapshot's prepared statement: %d custom plans, %d generic", custom, generic)
		if custom < 6 {
			t.Errorf("only %d custom plans counted for DeskSnapshot — the check is not looking at its statement", custom)
		}
		if generic != 0 {
			t.Errorf("DeskSnapshot ran on a GENERIC plan %d times; it must be planned for the caller every time", generic)
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}

	// THE PER-SECTION Email/query NEVER RUNS GENERIC EITHER (#697). Drawers and
	// clients without Area/getDesk still send it, and it is the statement that
	// stalled from its sixth execution above. Same shape as the DeskSnapshot
	// check: past the threshold, half the runs from a caller that forced
	// generic plans (which is what makes a regression certain to show), then
	// the caller's setting must come back and the server must report zero
	// generic plans for the statement.
	before := map[string]int64{}
	planCounts := func(tx *UserTx) (generic, custom int64, err error) {
		err = tx.tx.QueryRow(ctx, `SELECT coalesce(sum(generic_plans), 0), coalesce(sum(custom_plans), 0)
			  FROM pg_prepared_statements
			 WHERE statement LIKE '%count(*) OVER () AS total FROM ranked%'
			   AND statement NOT LIKE '%pg_prepared_statements%'`).Scan(&generic, &custom)
		return generic, custom, err
	}
	if err := d.st.UserTx(ctx, d.userID, func(tx *UserTx) error {
		g, c, err := planCounts(tx)
		before["generic"], before["custom"] = g, c
		return err
	}); err != nil {
		t.Fatal(err)
	}
	for i := 0; i < 6; i++ {
		if err := limited(func(tx *UserTx) error {
			want := "auto"
			if i%2 == 1 {
				want = "force_generic_plan"
				if _, err := tx.tx.Exec(ctx, "SET LOCAL plan_cache_mode = force_generic_plan"); err != nil {
					return err
				}
			}
			if _, _, err := tx.QueryEmails(ctx, sectionByHand(sections[0].AreaID, nil, threshold, 0, 50)); err != nil {
				return err
			}
			var mode string
			if err := tx.tx.QueryRow(ctx, `SELECT current_setting('plan_cache_mode')`).Scan(&mode); err != nil {
				return err
			}
			if mode != want {
				t.Errorf("plan_cache_mode after QueryEmails = %q, want the caller's %q restored", mode, want)
			}
			return nil
		}); err != nil {
			t.Fatalf("QueryEmails: %v", err)
		}
	}
	if err := d.st.UserTx(ctx, d.userID, func(tx *UserTx) error {
		g, c, err := planCounts(tx)
		if err != nil {
			return err
		}
		generic, custom := g-before["generic"], c-before["custom"]
		t.Logf("per-section Email/query, 6 more runs: %d custom plans, %d generic", custom, generic)
		if custom+generic < 6 {
			t.Errorf("only %d plans counted for the per-section Email/query — the check is not looking at its statement", custom+generic)
		}
		if generic != 0 {
			t.Errorf("the per-section Email/query ran on a GENERIC plan %d times; a desk filter must be planned for the caller every time", generic)
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}

	if err := d.st.UserTx(ctx, d.userID, func(tx *UserTx) error {
		passes, loops, scans := resolverPasses(t, ctx, tx, stmt.sql, stmt.args)
		t.Logf("DeskSnapshot plan: %d resolver DISTINCT ON nodes (max loops %.0f), %d message_labels scans", passes, loops, scans)
		if passes != 2 || scans != 2 || loops != 1 {
			t.Errorf("DeskSnapshot resolves areas %d/2 times (%d message_labels scans, max loops %.0f), want exactly once",
				passes, scans, loops)
		}
		// The control: one old per-section query alone resolves once too, so
		// the counter can see a resolver pass. Seven of them were seven.
		passes, _, scans = resolverPasses(t, ctx, tx, oldOne.sql, oldOne.args)
		if passes != 2 || scans != 2 {
			t.Errorf("control: a per-section Email/query shows %d resolver nodes / %d label scans, want 2/2 — "+
				"the counter cannot see the resolver", passes, scans)
		}
		bad, text := perRowWork(t, ctx, tx, stmt.sql, stmt.args)
		if len(bad) > 0 {
			t.Errorf("DeskSnapshot does per-message work (#660): %s\n%s", strings.Join(bad, "; "), text)
		} else if testing.Verbose() {
			t.Logf("DeskSnapshot plan:\n%s", text)
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
}
