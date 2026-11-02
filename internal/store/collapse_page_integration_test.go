// SPDX-License-Identifier: AGPL-3.0-only

//go:build integration

package store

// THE BOUNDED FIRST PAGE OF A COLLAPSED LIST agrees with the full ranking.
//
// A thread-collapsed, date-sorted list (every area, drawer and desk list) is
// served by a bounded scan (jmap_email_collapse_page.go) instead of ranking
// the whole area. Faster is only worth having if it is the SAME answer, so
// this test runs both and compares row for row. The oracle (oldCollapsedQuery)
// is the statement QueryEmails used to send for this shape, rebuilt from the
// same helpers (emailQueryWhere, emailOrderTerms, collapsedRankSQL), which the
// full ranking still uses for every shape the bounded scan does not serve.
//
// The fixture is the desk scale fixture (areas, three-message threads, copies
// in several mailboxes and accounts, snoozes, woken snoozes) plus deliberate
// ties on both sort keys, and the batch size is set to 2 so the scan loops many
// times per page.

import (
	"context"
	"fmt"
	"strconv"
	"strings"
	"testing"

	"github.com/oklog/ulid/v2"
)

// oldCollapsedQuery is the full-ranking Email/query for a collapsed list: ids
// windowed by position/limit, and the thread count.
func oldCollapsedQuery(t *testing.T, tx *UserTx, q EmailQuery) ([]string, int) {
	t.Helper()
	ctx := context.Background()
	args := []any{tx.userID.String()}
	bind := func(v any) string {
		args = append(args, v)
		return "$" + strconv.Itoa(len(args))
	}
	from, where, err := tx.emailQueryWhere(ctx, q, bind, nil)
	if err != nil {
		t.Fatalf("oracle where: %v", err)
	}
	body, outerOrder := collapsedRankSQL(from, where, emailOrderTerms(q, bind))
	ranked := `WITH ranked AS (` + body + `) `
	countSQL := ranked + `SELECT count(*) FROM ranked WHERE rn = 1`
	pageSQL := ranked + `SELECT id, count(*) OVER () AS total FROM ranked WHERE rn = 1 ORDER BY ` + outerOrder
	countArgs := args[:len(args):len(args)]
	if q.LimitSet || q.Limit > 0 {
		pageSQL += " LIMIT " + bind(q.Limit)
	}
	if q.Position > 0 {
		pageSQL += " OFFSET " + bind(q.Position)
	}
	rows, err := tx.tx.Query(ctx, pageSQL, args...)
	if err != nil {
		t.Fatalf("oracle page: %v", err)
	}
	var ids []string
	total := 0
	for rows.Next() {
		var id string
		if err := rows.Scan(&id, &total); err != nil {
			t.Fatal(err)
		}
		ids = append(ids, id)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		t.Fatal(err)
	}
	if len(ids) == 0 {
		if err := tx.tx.QueryRow(ctx, countSQL, countArgs...).Scan(&total); err != nil {
			t.Fatal(err)
		}
	}
	return ids, total
}

func TestCollapsedPageAgreesWithFullRanking(t *testing.T) { eachAreaMode(t, testCollapsedPageAgreesWithFullRanking) }

func testCollapsedPageAgreesWithFullRanking(t *testing.T, ready bool) {
	old := collapseBatchBase
	collapseBatchBase = 2
	t.Cleanup(func() { collapseBatchBase = old })

	d := seedDeskScale(t, 900)
	deskAgreementExtras(t, d)
	ctx := context.Background()
	// Ties on both sort keys: groups of messages sharing one received_at, and
	// some woken snoozes landing exactly on another message's received_at.
	for _, sql := range []string{
		`UPDATE app.messages SET received_at = date_trunc('hour', received_at) WHERE user_id = $1 AND id::text > '0'
		   AND (extract(minute FROM received_at)::int % 4) = 0`,
		`UPDATE app.messages m SET snooze_woke_at = (SELECT o.received_at FROM app.messages o
		     WHERE o.user_id = m.user_id ORDER BY o.id LIMIT 1 OFFSET (extract(second FROM m.received_at)::int))
		   WHERE m.user_id = $1 AND m.snooze_woke_at IS NULL AND m.snoozed_until IS NOT NULL`,
	} {
		if _, err := d.h.super.Exec(ctx, sql, d.userID.String()); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := d.h.super.Exec(ctx, `ANALYZE app.messages`); err != nil {
		t.Fatal(err)
	}

	if ready {
		readyAreaIndex(t, d.st, d.userID)
	}
	yes := true
	var areas []*ulid.ULID
	if err := d.st.UserTx(ctx, d.userID, func(tx *UserTx) error {
		views, err := tx.ListAreas(ctx)
		for _, v := range views {
			areas = append(areas, &v.ID)
		}
		return err
	}); err != nil {
		t.Fatal(err)
	}
	type filter struct {
		name string
		set  func(*EmailQuery)
	}
	filters := []filter{{"none", func(*EmailQuery) {}}}
	for i := range areas {
		a := areas[i]
		filters = append(filters,
			filter{fmt.Sprintf("area%d", i), func(q *EmailQuery) { q.InArea = a }},
			filter{fmt.Sprintf("area%d+desk", i), func(q *EmailQuery) { q.InArea, q.OnDesk = a, &yes }})
	}
	filters = append(filters,
		filter{"unsorted", func(q *EmailQuery) { q.Unsorted = true }},
		filter{"account", func(q *EmailQuery) { q.InAccount = &d.accountA }})

	windows := []struct {
		pos, lim int
		set      bool
	}{{0, 0, false}, {0, 50, true}, {0, 1, true}, {3, 7, true}, {40, 13, true}, {0, 0, true}, {100000, 5, true}, {-1, 5, true}}

	compared, nonEmpty, multi := 0, 0, 0
	if err := d.st.UserTx(ctx, d.userID, func(tx *UserTx) error {
		for _, f := range filters {
			for _, eff := range []bool{true, false} {
				for _, asc := range []bool{false, true} {
					for _, w := range windows {
						q := EmailQuery{CollapseThreads: true, SortByEffectiveArrival: eff, SortAscending: asc,
							Position: w.pos, Limit: w.lim, LimitSet: w.set}
						f.set(&q)
						if w.pos < 0 { // exactly at the end
							unq := q
							unq.Position, unq.Limit, unq.LimitSet = 0, 0, false
							_, n := oldCollapsedQuery(t, tx, unq)
							q.Position = n
						}
						name := fmt.Sprintf("%s eff=%v asc=%v pos=%d lim=%d/%v", f.name, eff, asc, q.Position, q.Limit, q.LimitSet)
						wantIDs, wantTotal := oldCollapsedQuery(t, tx, q)
						gotIDs, gotTotal, err := tx.QueryEmails(ctx, q)
						if err != nil {
							t.Fatalf("%s: %v", name, err)
						}
						got := make([]string, len(gotIDs))
						for i, id := range gotIDs {
							got[i] = id.String()
						}
						compared++
						if len(got) > 0 {
							nonEmpty++
						}
						if len(got) > 3 {
							multi++
						}
						if gotTotal != wantTotal || strings.Join(got, ",") != strings.Join(wantIDs, ",") {
							t.Errorf("%s: bounded = %d ids / total %d, full ranking = %d ids / total %d\n got  %v\n want %v",
								name, len(got), gotTotal, len(wantIDs), wantTotal, got, wantIDs)
						}
					}
				}
			}
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	// The bounded page serves a ready user's area lists and nobody else's: the
	// comparison above is only a comparison of two different statements if it ran.
	bounded := 0
	for _, st := range d.rec.stmts {
		if strings.Contains(st.sql, "/* collapsed page */") {
			bounded++
		}
	}
	if ready && bounded == 0 {
		t.Fatal("a ready user's area lists never used the bounded page: this run compared the full ranking with itself")
	}
	if !ready && bounded != 0 {
		t.Fatalf("a user who is not ready sent %d bounded-page statements", bounded)
	}
	// Agreement over empty results proves nothing: the matrix must have
	// compared real pages.
	if nonEmpty < compared/3 || multi < compared/4 {
		t.Fatalf("matrix compared %d queries but only %d non-empty / %d with >3 rows", compared, nonEmpty, multi)
	}
	t.Logf("compared %d queries (%d non-empty, %d with more than 3 rows)", compared, nonEmpty, multi)
}
