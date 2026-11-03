// SPDX-License-Identifier: AGPL-3.0-only

package store

import (
	"context"
	"fmt"
	"strconv"
	"strings"

	"github.com/oklog/ulid/v2"
)

// THE DESK IN ONE PASS (#665).
//
// A desk load used to be Area/getSummary plus one thread-collapsed Email/query
// per Review-first section (every area, then Needs sorting). Each of those
// statements resolved EVERY message's area on its own — AreaSummary through its
// desk CTE, each section through AreaPredicate's `IN (SELECT … areaJoinSQL …)`
// — and on a large mailbox that resolver costs most of a second, so a
// seven-section desk paid it eight times: many seconds per load, with JIT, work_mem,
// collation and generic plans ruled out by EXPLAIN.
//
// DeskSnapshot answers the same questions from ONE resolution:
//
//	WITH desk AS MATERIALIZED (deskResolvedSQL)   -- the resolver, once
//	     by_area, unsorted                         -- AreaSummary's own CTEs
//	     desk_cand                                 -- the sections' shared
//	                                                  Email/query predicates,
//	                                                  once, over `desk`
//	     s<i>_ranked                               -- collapsedRankSQL over
//	                                                  desk_cand, filtered on
//	                                                  the section's area and side
//	     s<i>_total, s<i>_page                     -- count and window
//	SELECT … one result set, tagged by kind and section
//
// WHY THE SECTIONS CANNOT DRIFT FROM Email/query. A section's predicates are not
// re-expressed here: DeskSectionQuery builds the EmailQuery the web client's
// sectionFilter produces (the api package pins that equivalence against its own
// parser), and emailQueryWhere — the ONE compiler QueryEmails uses — turns it
// into SQL. The substitution is that three of a section's filters are READ
// from `desk` instead of compiled again: its area (the same resolver —
// deskResolvedSQL is built from areaJoinSQL/areaResolvedSQL, or reads the
// stored area exactly as areaPredicateStored does), its side of the line (the
// same lineSideSQL against the same threshold) and onDesk itself (desk's rows
// ARE deskMessageSQL's, with the same account). Every other predicate a
// section has is compiled by emailQueryWhere, once for all sections, since
// they share all of them (#27). The agreement test
// (desk_snapshot_integration_test.go) holds every section to a per-section
// QueryEmails and the counts to AreaSummary, in both area modes, on a fixture
// with every shape the predicates distinguish.
//
// WHY A MATERIALIZED CTE AND NOT A TEMP TABLE. Both run the resolver once. A
// temp table needs the TEMP privilege on the database (a hardened deployment may
// revoke it from PUBLIC), writes catalog rows on every desk load (pg_class and
// pg_attribute churn, the classic autovacuum complaint at this frequency), has
// no statistics unless it is ANALYZEd — another statement — and turns one round
// trip into three or more. The CTE needs nothing, is one statement and one
// snapshot (so the counts and the lists are from the same instant by
// construction), and its lack of statistics does not matter here: the
// sections read it through one hash join (desk_cand, below).
// MATERIALIZED is stated rather than left to the planner's "referenced more
// than once" rule, so a future edit that leaves one reference cannot quietly
// inline the resolver back into every section.

// DeskSectionPage asks for one desk section: an area's surface (AreaID set) or
// Needs sorting (AreaID nil), windowed by Position and Limit like Email/query.
type DeskSectionPage struct {
	AreaID   *ulid.ULID
	Position int
	Limit    int
}

// DeskSnapshotQuery is one desk load: the scope and line every count and
// section is taken at, and the sections wanted, in the order they are returned.
// An empty Sections asks for the summary alone.
type DeskSnapshotQuery struct {
	Scope    DeskScope
	Sections []DeskSectionPage
}

// DeskSectionResult is one section's window and its thread-collapsed total —
// what that section's Email/query would have returned as ids and total.
type DeskSectionResult struct {
	AreaID *ulid.ULID
	IDs    []ulid.ULID
	Total  int
}

// DeskSnapshotResult is a whole desk, taken in one statement: AreaSummary's
// rows (same order, same meaning) and each requested section in request order.
type DeskSnapshotResult struct {
	Summary  []AreaCounts
	Sections []DeskSectionResult
}

// DeskSectionQuery is the Email/query a Review-first desk section is: on the
// desk, not snoozed, thread-collapsed and newest-returned first, scoped to the
// account when there is one; an area's section also keeps only the surface
// side of the line (Needs sorting takes both sides — web/src/core/desk.ts
// sectionFilter explains why). It is exactly what the api package parses the
// web client's section filter into, and TestDeskSectionQueryIsTheClientsFilter
// holds the two together.
func DeskSectionQuery(sec DeskSectionPage, scope DeskScope) EmailQuery {
	yes, no := true, false
	q := EmailQuery{
		OnDesk:                 &yes,
		Snoozed:                &no,
		InAccount:              scope.AccountID,
		SortByEffectiveArrival: true,
		CollapseThreads:        true,
		Position:               sec.Position,
		Limit:                  sec.Limit,
		LimitSet:               true,
	}
	if sec.AreaID == nil {
		q.Unsorted = true
	} else {
		id := *sec.AreaID
		q.InArea = &id
		q.OnSurface = &yes
		// Read only with OnSurface — exactly when emailQueryWith supplies it.
		q.LineThreshold = scope.Threshold
	}
	return q
}

// DeskSnapshot computes a desk — AreaSummary's counts and every requested
// section's first page and total — in ONE statement that resolves areas ONCE.
// See this file's header for the shape and why it agrees with AreaSummary and
// per-section QueryEmails by construction.
func (tx *UserTx) DeskSnapshot(ctx context.Context, q DeskSnapshotQuery) (DeskSnapshotResult, error) {
	stored, err := tx.areaIndexReady(ctx)
	if err != nil {
		return DeskSnapshotResult{}, err
	}
	args := []any{tx.userID.String(), q.Scope.Threshold}
	bind := func(v any) string {
		args = append(args, v)
		return "$" + strconv.Itoa(len(args))
	}

	var b strings.Builder
	b.WriteString(`
		WITH desk AS MATERIALIZED (` + deskResolvedSQL(bind, tx.userID, q.Scope.AccountID, "$2", stored) + `),
		` + areaSummaryCTEs("desk") + `,
		summary AS (SELECT * FROM by_area UNION ALL SELECT * FROM unsorted)`)

	// THE SECTIONS SHARE ONE PASS OVER THE MESSAGES (#27). A section is its
	// Email/query (DeskSectionQuery), and every section's query is the same
	// apart from two things: WHICH AREA, and (for an area) THE SURFACE SIDE
	// of the line. `desk` already holds both for every desk message — its
	// area from the one resolution, its side from lineSideSQL against the
	// same threshold the section's OnSurface uses — and its rows are exactly
	// the messages the section's OnDesk filter admits (the same
	// deskMessageSQL with the same account, in the same statement). So the
	// rest of the section's predicates (snoozed, account, deleted, junk, …)
	// are compiled ONCE by emailQueryWhere, from the section query with the
	// area, side and on-desk filters taken out, and evaluated once over the
	// desk in `desk_cand`; each section is then a filter on its area and side.
	//
	// It used to be one emailQueryWhere per section with the area read from
	// `desk` through a hashed SubPlan: each section scanned the user's whole
	// mailbox and triage scores again to keep the messages `desk` had already
	// found, so a desk cost one full scan per section on top of the
	// resolution. Driving each section from `desk` by primary key instead was
	// tried and measured slower on a large desk (thousands of key lookups and
	// score probes per section); one hash join for all sections is cheaper
	// at every desk size.
	//
	// desk_cand carries the columns collapsedRankSQL and the desk sections'
	// order (effective arrival, then id: DeskSectionQuery's sort) read, under
	// the names app.messages gives them, so it stands in for `m` there. desk's
	// area and side are renamed so that nothing reads like the stored column
	// (messages.area_id), which only a ready user's statements may name.
	final := []string{`SELECT 0 AS kind, -1 AS sec,
		       row_number() OVER (ORDER BY position NULLS LAST, area_id NULLS LAST) AS ord,
		       area_id::text AS area_id, surface, drawer, unscored, NULL::text AS id
		  FROM summary`}
	if len(q.Sections) > 0 {
		common := DeskSectionQuery(q.Sections[0], q.Scope)
		common.InArea, common.Unsorted, common.OnSurface, common.OnDesk = nil, false, nil, nil
		from, where, err := tx.emailQueryWhere(ctx, common, bind, nil)
		if err != nil {
			return DeskSnapshotResult{}, err
		}
		b.WriteString(`,
		desk_cand AS MATERIALIZED (
			SELECT m.id, m.user_id, m.thread_id, m.snooze_woke_at, m.received_at, dk.area_id AS desk_area, dk.side AS desk_side
			  FROM ` + from + `
			  JOIN desk dk ON dk.id = m.id
			 WHERE ` + where + `)`)
	}
	for i, sec := range q.Sections {
		eq := DeskSectionQuery(sec, q.Scope)
		// The section's own area and side, read from desk_cand (above).
		where := `m.desk_area IS NULL`
		if eq.InArea != nil {
			// Unjudged mail counts as surface, exactly as emailQueryWhere's
			// OnSurface and AreaSummary's unscored bucket do.
			where = `m.desk_area = ` + bind(eq.InArea.String()) + ` AND (m.desk_side IS NULL OR m.desk_side = 'surface')`
		}
		from := "desk_cand m"
		body, outerOrder := collapsedRankSQL(from, where, emailOrderTerms(eq, bind))
		n := "s" + strconv.Itoa(i)
		page := `SELECT id, row_number() OVER (ORDER BY ` + outerOrder + `) AS ord
			  FROM ` + n + `_ranked WHERE rn = 1 ORDER BY ` + outerOrder + ` LIMIT ` + bind(eq.Limit)
		if eq.Position > 0 {
			page += ` OFFSET ` + bind(eq.Position)
		}
		b.WriteString(`,
		` + n + `_ranked AS (` + body + `),
		` + n + `_total AS (SELECT count(*) AS n FROM ` + n + `_ranked WHERE rn = 1),
		` + n + `_page AS (` + page + `)`)
		final = append(final,
			`SELECT 1, `+strconv.Itoa(i)+`, 0::bigint, NULL::text, n, 0::bigint, 0::bigint, NULL::text FROM `+n+`_total`,
			`SELECT 2, `+strconv.Itoa(i)+`, ord, NULL::text, 0::bigint, 0::bigint, 0::bigint, id::text FROM `+n+`_page`)
	}
	b.WriteString(`
		` + strings.Join(final, `
		UNION ALL `) + `
		ORDER BY kind, sec, ord`)

	out := DeskSnapshotResult{Sections: make([]DeskSectionResult, len(q.Sections))}
	for i, sec := range q.Sections {
		out.Sections[i].AreaID = sec.AreaID
	}
	// ⚠ A CUSTOM PLAN, EVERY TIME. The statement's text is the same on every
	// load of a desk with the same number of sections, so pgx's statement cache
	// re-executes one prepared statement, and after five executions Postgres
	// may switch it to a GENERIC plan — planned without knowing whose desk it
	// is, so every `user_id = $1` estimate is a per-user average. MEASURED on
	// the 20k-message scale fixture with 300 small users beside it, seven
	// loads per mode, two runs on a shared host: generic 2.1–4.5 s and
	// 0.8–1.9 s, forced custom 0.65–1.9 s and 0.7–1.1 s — never faster
	// generic, up to ~3x slower; and once the statement was cached, `auto`
	// timed like generic. Planning this statement costs ~16 ms. Scoped to this one statement and restored after
	// it, so the rest of the caller's transaction plans exactly as it would
	// have; TestDeskSnapshotResolvesOnceAtScale reads pg_prepared_statements
	// to hold it to zero generic plans.
	var planMode string
	if err := tx.tx.QueryRow(ctx,
		`SELECT current_setting('plan_cache_mode'), set_config('plan_cache_mode', 'force_custom_plan', true)`,
	).Scan(&planMode, new(string)); err != nil {
		return DeskSnapshotResult{}, fmt.Errorf("store: desk snapshot (plan mode): %w", err)
	}
	rows, err := tx.tx.Query(ctx, b.String(), args...)
	if err != nil {
		return DeskSnapshotResult{}, fmt.Errorf("store: desk snapshot: %w", err)
	}
	defer rows.Close()
	totals := 0
	for rows.Next() {
		var (
			kind, sec, ord        int64
			areaID, id            *string
			surface, drawer, unsc int64
		)
		if err := rows.Scan(&kind, &sec, &ord, &areaID, &surface, &drawer, &unsc, &id); err != nil {
			return DeskSnapshotResult{}, fmt.Errorf("store: scan desk snapshot row: %w", err)
		}
		switch kind {
		case 0:
			c := AreaCounts{Surface: int(surface), Drawer: int(drawer), Unscored: int(unsc)}
			if areaID != nil {
				aid, perr := ulid.Parse(*areaID)
				if perr != nil {
					return DeskSnapshotResult{}, fmt.Errorf("store: parse area id %q: %w", *areaID, perr)
				}
				c.AreaID = &aid
			}
			out.Summary = append(out.Summary, c)
		case 1:
			out.Sections[sec].Total = int(surface)
			totals++
		case 2:
			if id == nil {
				return DeskSnapshotResult{}, fmt.Errorf("store: desk snapshot: section %d row without an id", sec)
			}
			mid, perr := ulid.Parse(*id)
			if perr != nil {
				return DeskSnapshotResult{}, fmt.Errorf("store: parse message id %q: %w", *id, perr)
			}
			out.Sections[sec].IDs = append(out.Sections[sec].IDs, mid)
		}
	}
	if err := rows.Err(); err != nil {
		return DeskSnapshotResult{}, fmt.Errorf("store: desk snapshot: %w", err)
	}
	rows.Close()
	if _, err := tx.tx.Exec(ctx, `SELECT set_config('plan_cache_mode', $1, true)`, planMode); err != nil {
		return DeskSnapshotResult{}, fmt.Errorf("store: desk snapshot (plan mode): %w", err)
	}
	// Every section has exactly one total row (count(*) of a CTE always yields
	// one). Fewer would mean a section silently read as zero.
	if totals != len(q.Sections) {
		return DeskSnapshotResult{}, fmt.Errorf("store: desk snapshot: %d section totals for %d sections", totals, len(q.Sections))
	}
	return out, nil
}
