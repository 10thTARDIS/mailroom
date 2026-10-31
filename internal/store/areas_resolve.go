// SPDX-License-Identifier: AGPL-3.0-only

package store

import (
	"context"
	"fmt"
	"strconv"

	"github.com/oklog/ulid/v2"

	"mailroom/internal/areas"
	"mailroom/internal/tabs"
)

// RESOLVING A MESSAGE'S AREA, in the two places that have to agree (#442,
// mirroring tab_resolve.go's own reasoning for ADR-022): the Go resolver
// (internal/areas.Resolve) answers what a report or a future API needs in
// application code, and the SQL joins below (areaJoinSQL) answer it
// inside a query — Email/query's area filter and AreaSummary's counts, both of
// which have to see the SAME area for the SAME message or a client filtering
// to an area could show a count that disagrees with the list it renders
// (#155's lesson, restated for a third resolver).
//
// THE PRECEDENCE, MATCHING internal/areas.Resolve EXACTLY:
//
//  1. LABELS FIRST. Every area fed by one of the message's labels is a
//     candidate; the one earliest in position order wins (area_feeds joined
//     through message_labels, ORDER BY a.position, a.id).
//  2. ELSE THE RESOLVED TAB — mapped label tab if any, else the stored
//     classifier verdict, EXACTLY tab_resolve.go's resolveOne precedence
//     (tabs.PrecedenceNames() bound the same way). An area whose feed names
//     that tab wins.
//  3. ELSE NEEDS SORTING — NULL. Nothing here folds a null verdict to Primary;
//     an area plan is stated against tabs.All as named, not against which
//     tabs today's strip happens to show (see area_report.go's own doc
//     comment on the identical choice for AreaReportRow).
//
// THIS FILE HOLDS BOTH IMPLEMENTATIONS ON PURPOSE, exactly as tab_resolve.go
// does: they cannot share code across the Go/SQL boundary, so a reader
// changing one is looking at the other, and
// TestAreaResolverAgreesWithTheQueryExpression makes disagreement a red build
// instead of a silent drift. ONE SQL implementation serves every caller —
// Email/query (AreaPredicate), AreaSummary, ResolveAreas, the drawer sweep and
// day-one setup — so the SQL side cannot drift from itself either.

// ⚠ SET-BASED, NEVER PER MESSAGE (#660). This resolver used to be two
// correlated scalar subqueries (`(SELECT f.area_id … WHERE … ml.message_id =
// m.id … LIMIT 1)`), which Postgres runs ONCE PER MESSAGE — and each run also
// re-evaluates the RLS policy's `app.current_user_id()` (a current_setting
// call plus the app.ulid domain's regex CHECK) because inside a correlated
// subquery the user is a column, not a constant the planner can fold into a
// one-time filter. On a desk of tens of thousands of messages that was most
// of Area/getSummary's 11 seconds.
//
// So the resolver is now three JOINS computed once for the whole set:
//
//	area_la  per message, the label-fed area: DISTINCT ON the message, first by
//	         (a.position, a.id) — exactly the old ORDER BY … LIMIT 1
//	area_lt  per message, the MAPPED tab: DISTINCT ON the message, first by the
//	         tabs.PrecedenceNames() order — exactly resolveOne's precedence
//	area_tf  the area fed by COALESCE(mapped tab, classified_tab); at most one,
//	         because area_feeds_tab_once is UNIQUE (user_id, tab)
//
// and the area is COALESCE(area_la.area_id, area_tf.area_id) — label first,
// then tab, else NULL (Needs sorting), as before.
//
// Every join is keyed on (user_id, message_id) — never on message_id alone —
// so a message can only ever be resolved through its OWN user's labels, feeds
// and areas, whatever the caller can see: the scoping is structural, not
// borrowed from RLS. The caller's `m.user_id = $1` then reaches every one of
// these relations as a constant through the join equalities (Postgres derives
// it across the LEFT JOINs and pushes it into the DISTINCT ON subqueries,
// because user_id is a DISTINCT ON key), which is what lets each scan use its
// (user_id, …) index and fold the RLS policy into a one-time filter.
// TestDeskQueriesStaySetBasedAtScale holds the plans to that.

// areaResolvedSQL is the resolved area — an app.ulid, or NULL for Needs
// sorting — for a message `m` whose FROM clause carries areaJoinSQL's joins.
const areaResolvedSQL = `COALESCE(area_la.area_id, area_tf.area_id)`

// areaJoinSQL returns the LEFT JOINs that resolve message `m`'s area, to be
// appended to a FROM clause that already has `app.messages m`, plus the
// label-fed and tab-fed arms as expressions over them (ResolveAreas reads both
// so it can report ByLabel; everything else reads areaResolvedSQL). Each join
// yields at most one row per message, so appending them never changes the
// caller's cardinality.
//
// restrict, when not nil, maps a message id column to a condition on it, and
// that condition is added inside the two DISTINCT ON subqueries (on
// ml.message_id), so a caller that resolves a known set of messages
// (ResolveAreas: one Email/get page; areaInSQL: one area's candidates) does not
// sort every labelled message the user has to find them. It narrows WHICH
// messages are resolved, never HOW: a message that passes keeps every one of
// its label rows, so its DISTINCT ON winner is the one an unrestricted pass
// would pick. The caller restricts its own scan of `m` the same way.
//
// bind registers a parameter and returns its placeholder (the QueryEmails/
// tabFilterSQL convention). The aliases area_la, area_lt and area_tf are
// reserved in any query that uses this.
func areaJoinSQL(bind func(any) string, restrict func(col string) string) (joins, labelArea, tabArea string) {
	labelRestrict, tabRestrict := "", ""
	if restrict != nil {
		labelRestrict = " WHERE " + restrict("ml.message_id")
		tabRestrict = " AND " + restrict("ml.message_id")
	}
	// The mapped-tab order is resolveOne's (tab_resolve.go), bound as a
	// parameter rather than restated — see tabFilterSQL. f.tab is the
	// app.category_tab ENUM and the mapped/classified half is TEXT, so the
	// comparison casts the column to text: the one direction that can never
	// itself need a NULL guard.
	joins = `
	  LEFT JOIN (SELECT DISTINCT ON (ml.user_id, ml.message_id) ml.user_id, ml.message_id, f.area_id
	               FROM app.message_labels ml
	               JOIN app.area_feeds f ON f.user_id = ml.user_id AND f.label_id = ml.label_id
	               JOIN app.areas a ON a.user_id = f.user_id AND a.id = f.area_id` + labelRestrict + `
	              ORDER BY ml.user_id, ml.message_id, a.position, a.id) area_la
	         ON area_la.user_id = m.user_id AND area_la.message_id = m.id
	  LEFT JOIN (SELECT DISTINCT ON (ml.user_id, ml.message_id) ml.user_id, ml.message_id, l.tab::text AS tab
	               FROM app.message_labels ml
	               JOIN app.labels l ON l.user_id = ml.user_id AND l.id = ml.label_id
	              WHERE l.tab IS NOT NULL` + tabRestrict + `
	              ORDER BY ml.user_id, ml.message_id,
	                       array_position(` + bind(tabs.PrecedenceNames()) + `::text[], l.tab::text)) area_lt
	         ON area_lt.user_id = m.user_id AND area_lt.message_id = m.id
	  LEFT JOIN app.area_feeds area_tf
	         ON area_tf.user_id = m.user_id
	        AND area_tf.tab::text = COALESCE(area_lt.tab, m.classified_tab::text)`
	return joins, "area_la.area_id", "area_tf.area_id"
}

// areaMatchSQL is "message m's resolved area satisfies cond" as a predicate,
// for a caller that can only add a WHERE condition (Email/query) or a FILTER
// (day-one setup's preview): a NON-CORRELATED `IN` over the same joins,
// `(m.user_id, m.id) IN (SELECT m.user_id, m.id FROM app.messages m …
// WHERE m.user_id = <userPH> AND cond)`. cond reads area_la / area_tf /
// areaResolvedSQL. userPH is the caller's placeholder for its own user id.
//
// SCOPED TWICE, NEITHER BY RLS: the subquery resolves only userPH's messages,
// and the outer message must match on the PAIR (user_id, id), so it can only
// ever match itself. A caller whose userPH were not its user would match
// nothing — fail closed, never another user's mail.
//
// ⚠ `IS TRUE` IS LOAD-BEARING, NOT DECORATION. It is a no-op on the value (the
// IN is never NULL, see below) and exists to stop Postgres converting a bare
// top-level IN into a SEMI-JOIN. As a semi-join the planner is free to put
// this whole resolution on the inner side of a NESTED LOOP and re-run it for
// every candidate message whenever it underestimates either side — MEASURED
// on the 20k-message scale fixture: a plan flip caused by an unrelated change
// to the desk predicate turned the desk Email/query from ~0.3 s into one that
// was still running after ten minutes. Wrapped, it is always a hashed SubPlan:
// the subquery runs ONCE, its ids go into a hash table, and each message costs
// one probe — whatever the estimates say.
//
// NEVER NULL: every column compared is NOT NULL, so IN is strictly true or
// false and `NOT (…)` of it is exact.
//
// It resolves EVERY message the user has. areaInSQL is the narrower form for
// "resolves to this one area", which is what `inArea` asks.
func areaMatchSQL(bind func(any) string, userPH, cond string) string {
	joins, _, _ := areaJoinSQL(bind, nil)
	return `((m.user_id, m.id) IN (SELECT m.user_id, m.id FROM app.messages m` + joins + `
	         WHERE m.user_id = ` + userPH + ` AND (` + cond + `))) IS TRUE`
}

// areaInSQL is "message m resolves to area areaPH" — areaMatchSQL's predicate
// with cond `areaResolvedSQL = areaPH`, the same `IN (…) IS TRUE` shape and the
// same scoping — except that it resolves only the area's CANDIDATES
// (areaCandidatesSQL) instead of every message the user has (#706).
//
// WHY: measured on a real instance (one user, tens of thousands of messages), most of an
// `inArea` Email/query was the resolver — both DISTINCT ON arms over every
// labelled message and hash joins over every message — to keep the small share in
// the requested area. A drawer or an area view opens with one of these.
//
// THE SUPERSET ARGUMENT, against areaJoinSQL as written. Write T(m) for the
// resolved tab COALESCE(mapped tab, m.classified_tab), and C(X) for the union
// of areaCandidatesSQL's three arms:
//
//	(a)  m carries a label that feeds X       (area_feeds.label_id, area X)
//	(b1) m.classified_tab is a tab feeding X  (area_feeds.tab, area X)
//	(b2) m carries a TAB-MAPPED label whose tab feeds X  (labels.tab)
//
// If m resolves to X then m is in C(X). Either the label arm decided it
// (area_la = X): the winning row is a feed of X on one of m's labels, so (a).
// Or no label of m feeds any area and area_tf = X, so T(m) is a tab feeding X:
// if a mapped label decided T(m), that label's tab feeds X, so (b2); if no
// label is mapped, T(m) = m.classified_tab, so (b1). A NULL verdict with no
// mapped label has no T(m) and resolves to no area — it is in no C(X), and
// rightly so.
//
// The converse is FALSE, which is why the full resolution stays: a candidate
// may resolve elsewhere. A label of X loses to a label of an area earlier in
// (a.position, a.id); any feeding label beats every tab; a classified tab of
// X loses to a mapped label of another tab; a mapped tab of X loses to a
// mapped tab earlier in tabs.Precedence(). C(X) only narrows WHICH messages
// are resolved, never HOW.
//
// RESTRICTING IS THEREFORE EXACT. A candidate keeps every one of its label
// rows (the restriction tests the message id, never the label), so area_la
// and area_lt pick for it exactly what an unrestricted pass picks, and
// area_tf reads only its own row; a non-candidate cannot resolve to X, so
// leaving it out of the IN set changes no answer. TestAreaPrefilterAgrees
// WithFullResolution holds this against areaMatchSQL, ResolveAreas and the Go
// resolver.
//
// ⚠ THERE IS NO TAB FOLD HERE, AND THE CANDIDATES MUST NOT ADD ONE. The
// enabled-tab fold (tabs.Fold: a disabled tab shows in Primary's strip) is
// the TAB layer's; the area resolver matches area_feeds.tab against the RAW
// T(m), and so does internal/areas.Resolve, which AreaReportRow.ResolvedTab
// feeds without the fold on purpose: an area plan is stated against tabs.All
// as named. So a message classified into a disabled tab resolves to the area
// fed by THAT tab, never to Primary's area, and (b1)/(b2) compare raw tabs
// too. Should the resolver ever gain the fold, Primary's candidates would have
// to gain every disabled tab in (b1) and (b2) in the same change; the
// agreement test pins a disabled-tab message to its own tab's area so that
// change cannot be made on one side alone.
//
// Needs sorting (areaID nil) is NOT narrowed and cannot be this way: it is
// the absence of every feed, so its "candidates" are everything outside every
// area's C(X) — no cheaper to find than resolving. Neither is Area/getDesk
// (DeskSnapshot), which wants every area at once and resolves once already.
//
// ⚠ EVERY MEMBERSHIP TEST IS `(… IN (SELECT … FROM area_cand)) IS TRUE` for
// areaMatchSQL's reason: a hashed SubPlan built once and probed per row, never
// a semi-join the planner could turn into a nested loop over the candidates.
// area_cand is MATERIALIZED and read three times (the two DISTINCT ON arms and
// the resolver's own scan of m), so the candidates are computed once.
//
// USER SCOPING: every relation in area_cand is bound to userPH explicitly and
// joined on user_id, exactly as the resolver's own joins are; the resolver's
// scan keeps `m.user_id = userPH`. Nothing is borrowed from RLS.
func areaInSQL(bind func(any) string, userPH, areaPH string) string {
	return areaInScopedSQL(bind, userPH, areaPH, areaCandidatesSQL(userPH, areaPH), "area_cand")
}

// areaInDeskSQL is areaInSQL with the resolver scoped to the DESK instead of
// the area's candidate set C(X): deskPred is deskMessageSQL's predicate over
// alias m (its placeholders already bound by the caller, who reuses the very
// same string as the outer onDesk condition).
//
// WHY (#706): for a tab-fed area C(X) is most of the mailbox (production:
// 15.4k of 25.8k messages) while about a hundred are on the desk, and the
// resolver's SubPlan was ~80% of a 1.1 s drawer query. Scoped to the desk the
// same query returned identical rows (EXCEPT ALL both ways, all eight areas)
// and ran 2.5-4x faster on the big areas, neutral on the small ones.
//
// EXACT, for the same reason C(X) is: the scope only narrows WHICH messages
// are resolved, never HOW. A message outside the scope is off the desk and so
// fails the outer onDesk condition this is always ANDed with; a message inside
// it keeps every one of its label rows (the restriction tests the message id),
// so it resolves exactly as an unrestricted pass resolves it. Only use this
// where the caller also applies deskPred as a top-level condition.
func areaInDeskSQL(bind func(any) string, userPH, areaPH, deskPred string) string {
	scope := `SELECT m.id FROM app.messages m WHERE m.user_id = ` + userPH + ` AND ` + deskPred
	return areaInScopedSQL(bind, userPH, areaPH, scope, "area_scope")
}

// areaInScopedSQL is the shared body: scopeSQL (one column, id) names the
// messages to resolve, bound under the CTE name cte. Keeps the hashed
// `IN (…) IS TRUE` SubPlan shape described on areaInSQL.
func areaInScopedSQL(bind func(any) string, userPH, areaPH, scopeSQL, cte string) string {
	inScope := func(col string) string {
		return `(` + col + ` IN (SELECT c.id FROM ` + cte + ` c)) IS TRUE`
	}
	joins, _, _ := areaJoinSQL(bind, inScope)
	return `((m.user_id, m.id) IN (WITH ` + cte + ` AS MATERIALIZED (` + scopeSQL + `)
	         SELECT m.user_id, m.id FROM app.messages m` + joins + `
	         WHERE m.user_id = ` + userPH + ` AND ` + inScope("m.id") + `
	           AND ` + areaResolvedSQL + ` = ` + areaPH + `)) IS TRUE`
}

// areaCandidatesSQL is the candidate set C(X) for area areaPH — arms (a), (b1)
// and (b2) of areaInSQL's argument — as one column, id, with duplicates (UNION
// ALL: the consumers are hashed IN tests, which do not care, and a dedupe
// would be a sort for nothing). userPH is the caller's own user id.
//
// Plain joins on (user_id, …) keys, each starting from area X's own feeds; the
// classified-tab arm tests the column with an `IN … IS TRUE` over X's tab
// feeds (at most five rows) so the scan of the user's messages filters in
// place rather than joining.
func areaCandidatesSQL(userPH, areaPH string) string {
	return `
	           SELECT cml.message_id AS id
	             FROM app.message_labels cml
	             JOIN app.area_feeds cf ON cf.user_id = cml.user_id AND cf.label_id = cml.label_id
	            WHERE cml.user_id = ` + userPH + ` AND cf.area_id = ` + areaPH + `
	           UNION ALL
	           SELECT cml.message_id
	             FROM app.message_labels cml
	             JOIN app.labels cl ON cl.user_id = cml.user_id AND cl.id = cml.label_id
	             JOIN app.area_feeds cf ON cf.user_id = cl.user_id AND cf.tab = cl.tab
	            WHERE cml.user_id = ` + userPH + ` AND cf.area_id = ` + areaPH + `
	           UNION ALL
	           SELECT cm.id
	             FROM app.messages cm
	            WHERE cm.user_id = ` + userPH + `
	              AND (cm.classified_tab IN (SELECT cft.tab FROM app.area_feeds cft
	                                          WHERE cft.user_id = ` + userPH + ` AND cft.area_id = ` + areaPH + `
	                                            AND cft.tab IS NOT NULL)) IS TRUE`
}

// AreaPredicateOnDesk is AreaPredicate for a real area when the caller also
// applies deskPred (deskMessageSQL over alias m) as a top-level condition:
// the resolver runs over the desk only (areaInDeskSQL, #706). Same answer on
// every message that passes deskPred, which is all the caller keeps.
func AreaPredicateOnDesk(areaID ulid.ULID, bind func(any) string, deskPred string) string {
	return areaInDeskSQL(bind, "$1", bind(areaID.String()), deskPred)
}

// AreaPredicate is the Email/query predicate for `inArea`: the message's
// resolved area is this one, or (areaID == nil) it has none — Needs sorting.
// Exported so internal/api could build the filter without a second copy of
// the resolver's reasoning; QueryEmails is its caller.
//
// An area resolves only that area's candidates (areaInSQL, #706); Needs
// sorting resolves every message (areaMatchSQL), because it cannot be narrowed
// the same way. Both answer exactly what the unrestricted resolver answers.
//
// ⚠ THE CALLER'S $1 MUST BE ITS OWN USER ID — the convention every UserTx
// query builder here follows (QueryEmails, AreaSummary, ResolveAreas) — because
// areaMatchSQL scopes its subquery to a constant user and this signature has
// no room for a placeholder. Getting it wrong fails closed (see areaMatchSQL).
func AreaPredicate(areaID *ulid.ULID, bind func(any) string) string {
	if areaID == nil {
		return areaMatchSQL(bind, "$1", areaResolvedSQL+" IS NULL")
	}
	return areaInSQL(bind, "$1", bind(areaID.String()))
}

// AreaResolution is one message's resolved area. AreaID is nil for Needs
// sorting. ByLabel is true when a label decided it (as opposed to the
// resolved tab) — the SQL twin of areas.Resolution.Reason == areas.ByLabel.
type AreaResolution struct {
	AreaID  *ulid.ULID
	ByLabel bool
}

// ResolveAreas answers the area for a set of messages in one round trip, the
// same shape ResolveTabs uses for the tab layer.
func (tx *UserTx) ResolveAreas(ctx context.Context, ids []ulid.ULID) (map[ulid.ULID]AreaResolution, error) {
	out := make(map[ulid.ULID]AreaResolution, len(ids))
	if len(ids) == 0 {
		return out, nil
	}

	args := []any{tx.userID.String(), ulidStrings(ids)}
	bind := func(v any) string {
		args = append(args, v)
		return "$" + strconv.Itoa(len(args))
	}
	joins, labelArea, tabArea := areaJoinSQL(bind, func(col string) string { return col + " = ANY($2)" })

	rows, err := tx.tx.Query(ctx, `
		SELECT m.id, `+labelArea+`, `+tabArea+`
		  FROM app.messages m`+joins+`
		 WHERE m.user_id = $1 AND m.id = ANY($2)`, args...)
	if err != nil {
		return nil, fmt.Errorf("store: resolve areas: %w", err)
	}
	defer rows.Close()
	for rows.Next() {
		var (
			id             string
			byLabel, byTab *string
		)
		if err := rows.Scan(&id, &byLabel, &byTab); err != nil {
			return nil, fmt.Errorf("store: scan resolved area: %w", err)
		}
		mid, err := ulid.Parse(id)
		if err != nil {
			return nil, fmt.Errorf("store: parse message id %q: %w", id, err)
		}
		switch {
		case byLabel != nil:
			aid, perr := ulid.Parse(*byLabel)
			if perr != nil {
				return nil, fmt.Errorf("store: parse area id %q: %w", *byLabel, perr)
			}
			out[mid] = AreaResolution{AreaID: &aid, ByLabel: true}
		case byTab != nil:
			aid, perr := ulid.Parse(*byTab)
			if perr != nil {
				return nil, fmt.Errorf("store: parse area id %q: %w", *byTab, perr)
			}
			out[mid] = AreaResolution{AreaID: &aid}
		default:
			out[mid] = AreaResolution{} // nil AreaID = Needs sorting
		}
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("store: resolve areas: %w", err)
	}
	return out, nil
}

// AreaCounts is one area's Surface/Drawer/Unscored split. AreaID nil is the
// Needs-sorting row — always present, even at zero, so a caller can render
// "0 unsorted" rather than infer it from absence (CLAUDE.md: "a bounded or
// empty result must report its bound").
type AreaCounts struct {
	AreaID   *ulid.ULID
	Surface  int
	Drawer   int
	Unscored int
}

// DeskScope narrows AreaSummary to one account, or (AccountID nil) every
// account the caller has. Threshold is the desk preferences line_threshold —
// passed in rather than read here so the five-row lookup stays out of a
// counts query that may run on every render.
type DeskScope struct {
	AccountID *ulid.ULID
	Threshold float32
}

// deskLiveSQL returns the predicate for "message m is on the desk": it has NOT
// been desk-archived by day-one setup, AND it is effectively in a mailbox that
// is INBOX or has feeds_desk set — its canonical placement if it has one, else
// a live provenance row. THE ONE DEFINITION OF "ON THE DESK" (#446),
// factored out so AreaSummary's counts and the drawer sweep's candidates
// (internal/store/drawer_sweep.go, #443) cannot disagree about what counts as
// unactioned. The same one-implementation reasoning areaJoinSQL already gives
// for the area resolver, restated for a second predicate several consumers must
// agree on.
//
// ⚠ `desk_archived_at IS NULL` IS THE FIRST CLAUSE AND IT IS LOAD-BEARING
// (#446, migration 00050). Day-one setup takes mail older than the window OFF
// THE DESK by setting that column and NOTHING ELSE — the bytes stay exactly
// where the provider has them, no writeback intent is enqueued, every other
// client sees no change. So "off the desk" exists ONLY as this column, and
// every consumer of this predicate has to honour it or the desk would keep
// counting, listing and (worst of all) SWEEPING mail day-one had already
// filed away. The drawer sweep is the one that matters: its candidate scan
// goes through this predicate, so a day-one archived message can never be
// server-moved to Archive — which is what makes "nothing moves on the server"
// still true a month after the wizard ran, rather than only on the day.
//
// ⚠ THE CANONICAL PLACEMENT DECIDES FIRST, exactly as it does for every other
// view (mailboxMembershipSQL, jmap_email.go). When the user moves a message in
// Mailroom (`e`, Send & archive, a drag, the drawer sweep), ApplyEmailUpdate
// writes moved_mailbox_id NOW and the INBOX provenance row stays 'live' until
// that folder's next live-UID observation (hourly plus jitter; reaper.go's
// RetireCanonicalPlacements is the bridge). Until #648's follow-up this
// predicate read provenance alone, so an archived message stayed on the desk —
// in Email/query `onDesk`, in Area/getSummary's counts — for up to an hour
// after every other view had moved it. So:
//
//	moved_mailbox_id set  -> on the desk iff THAT mailbox is INBOX or feeds_desk
//	                         (a restore to INBOX whose move has not landed IS on
//	                         the desk; an archive whose move has not landed is not)
//	moved_mailbox_id NULL -> live provenance decides, as before
//
// ⚠ SET-BASED, NEVER PER MESSAGE (#660). Until v1.80.2 each arm was an EXISTS
// correlated on the message (`CASE WHEN … THEN EXISTS (…) ELSE EXISTS (…)
// END`), which Postgres evaluates per message — and the subquery's RLS policy
// with it, a current_setting call plus the app.ulid domain's regex CHECK per
// provenance row, because the user was a column rather than a constant.
// Measured on a real instance, on an Inbox of tens of thousands of messages:
// several seconds to COUNT the desk. Each arm is now a NON-CORRELATED `IN (SELECT …)`
// over the user's own rows, bound to the caller's user id as a constant, which
// Postgres builds ONCE as a hash and probes per message; the provenance side
// scans only that user's rows, and the policy folds into a one-time filter.
// The meaning is unchanged: the same two arms, chosen by the same
// moved_mailbox_id test. The arms sit inside an OR, so Postgres never turns
// them into a join it could mis-order (see areaMatchSQL for what that costs).
//
// THREE-VALUED-LOGIC-FREE, like inMailboxSQL, because deskMessageSQL is wrapped
// in NOT by the `onDesk:false` filter: every arm is an IS [NOT] NULL test, or
// an IN whose left side is guarded NOT NULL (m.id is a primary key; the moved
// arm tests moved_mailbox_id IS NOT NULL first) and whose subquery selects a
// NOT NULL column (mailboxes.id, message_provenance.message_id) — and `x IN
// (…)` is NULL only when x is NULL or the list holds a NULL. So the whole
// expression is strictly true or false for every row.
//
// USER SCOPING: the subqueries are scoped to userID explicitly — bound here,
// never borrowed from RLS or from the caller's alias — so they only ever
// contain that user's mailboxes and provenance rows. Every caller is a UserTx
// method whose `m` is already restricted to the same user (`m.user_id = $1`,
// $1 = tx.userID), so a message and the rows that place it are always one
// user's.
//
// Its consumers, all of which therefore agree by construction: deskMessageSQL
// (and through it AreaSummary's counts and Email/query's `onDesk`),
// DrawerCandidates, drawerCandidateStillQualifies and DrawerReport. (The drawer
// predicates additionally require moved_mailbox_id IS NULL — a message with a
// pending move is never swept — so for them this change is a no-op, kept in
// step by sharing the definition rather than by coincidence.)
//
// bind registers a parameter and returns its placeholder; `m` is the caller's
// message alias and userID the caller's own user (tx.userID). accountID
// narrows to one account when set (nil = every account the caller has): the
// live provenance row must be that account's, and so must the mailbox a
// canonical placement names.
func deskLiveSQL(bind func(any) string, userID ulid.ULID, accountID *ulid.ULID) string {
	user := bind(userID.String())
	movedAcct, provAcct := "", ""
	if accountID != nil {
		ph := bind(accountID.String())
		movedAcct = ` AND dmb.account_id = ` + ph
		provAcct = ` AND mp.account_id = ` + ph
	}
	return `m.desk_archived_at IS NULL AND (
	          (m.moved_mailbox_id IS NOT NULL AND m.moved_mailbox_id IN (
	            SELECT dmb.id FROM app.mailboxes dmb
	             WHERE dmb.user_id = ` + user + `
	               AND (upper(dmb.imap_name) = 'INBOX' OR dmb.feeds_desk)` + movedAcct + `))
	          OR (m.moved_mailbox_id IS NULL AND m.id IN (
	            SELECT mp.message_id
	              FROM app.message_provenance mp
	              JOIN app.mailboxes mb ON mb.user_id = mp.user_id AND mb.id = mp.mailbox_id
	             WHERE mp.user_id = ` + user + ` AND mp.state = 'live'
	               AND (upper(mb.imap_name) = 'INBOX' OR mb.feeds_desk)` + provAcct + `)))`
}

// deskMessageSQL returns the FULL "on the desk" predicate for message alias
// `m` (#375): NOT junk, NOT trashed, NOT vanished, NOT a draft, AND
// deskLiveSQL — the EXACT conjunction AreaSummary's CTE filters its counts by
// and QueryEmails' OnDesk filter now shares, so a desk tab's Email/query and
// its Area/getSummary count can never disagree about which messages are "on
// the desk". Parenthesised as one expression so a caller can safely wrap it
// in `NOT (...)` for the negative case.
//
// bind/accountID are deskLiveSQL's own parameters, passed straight through —
// see its doc comment.
func deskMessageSQL(bind func(any) string, userID ulid.ULID, accountID *ulid.ULID) string {
	return `(NOT m.is_junk AND m.trashed_at IS NULL AND m.vanished_at IS NULL AND NOT m.is_draft AND ` +
		deskLiveSQL(bind, userID, accountID) + `)`
}

// lineSideSQL returns the ONE expression that answers WHICH SIDE OF THE
// DESK'S LINE a message is on (#464) — `'surface'`, `'drawer'`, or NULL for
// "nothing has judged it and nobody has placed it":
//
//	COALESCE(m.line_side, CASE WHEN <score> IS NULL THEN NULL
//	                          WHEN <score> >= <threshold> THEN 'surface'
//	                          ELSE 'drawer' END)
//
// THE USER'S OWN PLACEMENT COMES FIRST, and that ordering is the feature.
// #375 §5 makes Shift+↑/↓ a correction: the crossing is what the model learns
// from, so it has to MOVE the row now, not at the next retrain. Until #464
// every consumer split on the raw score alone, so the row stayed exactly
// where it was — and the drawer sweep was free to archive a message the user
// had just pulled above the line.
//
// ⚠ IT IS THE ONLY DEFINITION OF THE SPLIT, for the same reason areaJoinSQL
// and deskLiveSQL are single implementations of theirs: AreaSummary's counts,
// Email/query's `onSurface` and the drawer sweep's candidate predicate must
// see the SAME side for the SAME message, or a client's count disagrees with
// the list it renders (#155's lesson) — or, worse, the sweep archives what
// the surface is showing. Nothing else may compare a score to the threshold
// to decide a side.
//
// scoreExpr is whatever the caller already has for the score — `t.score`
// where app.triage_scores is LEFT JOINed as `t`, or QueryEmails' scalar
// subquery — and thresholdBind is the caller's own `$n` placeholder for the
// line. `m` is the caller's app.messages alias, as everywhere else here.
func lineSideSQL(scoreExpr string, thresholdBind string) string {
	return `COALESCE(m.line_side, CASE WHEN ` + scoreExpr + ` IS NULL THEN NULL` +
		` WHEN ` + scoreExpr + ` >= ` + thresholdBind + ` THEN 'surface'` +
		` ELSE 'drawer' END)`
}

// deskResolvedSQL is the body of AreaSummary's `desk` CTE: every message ON
// THE DESK (deskMessageSQL, scoped to accountID when set) with its resolved
// area (areaJoinSQL — NULL for Needs sorting) and its side of the line
// (lineSideSQL against thresholdPH). Columns: id, area_id, side.
//
// It is the area resolver's ONE pass over the desk. AreaSummary counts from it;
// DeskSnapshot (desk_snapshot.go, #665) materialises it once and serves both its
// counts AND every section's area filter from it, which is what takes a desk
// load from one resolver pass per section to one in total. A single function,
// so the two cannot resolve a message differently.
//
// The caller's $1 must be its own user id; bind registers the remaining
// parameters (the resolver's tab precedence, deskLiveSQL's user/account).
func deskResolvedSQL(bind func(any) string, userID ulid.ULID, accountID *ulid.ULID, thresholdPH string) string {
	areaJoins, _, _ := areaJoinSQL(bind, nil)
	return `
			SELECT m.id, ` + areaResolvedSQL + ` AS area_id, ` + lineSideSQL("t.score", thresholdPH) + ` AS side
			  FROM app.messages m
			  LEFT JOIN app.triage_scores t ON t.user_id = m.user_id AND t.message_id = m.id` + areaJoins + `
			 WHERE m.user_id = $1
			   AND ` + deskMessageSQL(bind, userID, accountID) + `
		`
}

// areaSummaryCTEs returns AreaSummary's two counting CTEs, `by_area` (one row
// per area of the caller's, zero counts included) and `unsorted` (the
// Needs-sorting row, present at zero), over the relation named desk — which
// must have deskResolvedSQL's columns. Both carry (area_id, position, surface,
// drawer, unscored). The caller's $1 must be its own user id. Shared with
// DeskSnapshot so its summary rows are AreaSummary's, computed the same way
// from the same resolved set.
func areaSummaryCTEs(desk string) string {
	return `by_area AS (
			SELECT a.id AS area_id, a.position,
			       count(d.id) FILTER (WHERE d.side = 'surface') AS surface,
			       count(d.id) FILTER (WHERE d.side = 'drawer')  AS drawer,
			       count(d.id) FILTER (WHERE d.side IS NULL)     AS unscored
			  FROM app.areas a
			  LEFT JOIN ` + desk + ` d ON d.area_id = a.id
			 WHERE a.user_id = $1
			 GROUP BY a.id, a.position
		),
		unsorted AS (
			SELECT NULL::app.ulid AS area_id, NULL::int AS position,
			       count(d.id) FILTER (WHERE d.side = 'surface') AS surface,
			       count(d.id) FILTER (WHERE d.side = 'drawer')  AS drawer,
			       count(d.id) FILTER (WHERE d.side IS NULL)     AS unscored
			  FROM ` + desk + ` d WHERE d.area_id IS NULL
		)`
}

// AreaSummary counts, in ONE query, every LIVE message that is ALSO ON THE
// DESK — a live provenance row in a mailbox that is INBOX or has feeds_desk
// set ("folders feed the desk", #446) — grouped by resolved area. Per group:
// Surface = side 'surface', Drawer = side 'drawer', Unscored = side NULL,
// where the side is lineSideSQL's: the user's own placement if they made one,
// else the score against Threshold. Returns one row per existing area (zero
// counts included, in position order) plus the Needs-sorting row (also
// present at zero).
//
// ⚠ `Unscored` KEEPS ITS NAME AND NOW MEANS "NEITHER SCORED NOR PLACED"
// (#464). An unscored message the user has dropped in the drawer by hand
// counts in Drawer instead — which is what they asked for, and what
// Email/query's `onSurface:false` returns for it. The field is not renamed:
// it is the same bucket, and renaming it would break every client over a
// widened edge case.
func (tx *UserTx) AreaSummary(ctx context.Context, scope DeskScope) ([]AreaCounts, error) {
	args := []any{tx.userID.String(), scope.Threshold}
	bind := func(v any) string {
		args = append(args, v)
		return "$" + strconv.Itoa(len(args))
	}

	query := `
		WITH desk AS (` + deskResolvedSQL(bind, tx.userID, scope.AccountID, "$2") + `),
		` + areaSummaryCTEs("desk") + `
		SELECT area_id, surface, drawer, unscored
		  FROM (SELECT * FROM by_area UNION ALL SELECT * FROM unsorted) x
		 ORDER BY position NULLS LAST, area_id NULLS LAST`

	rows, err := tx.tx.Query(ctx, query, args...)
	if err != nil {
		return nil, fmt.Errorf("store: area summary: %w", err)
	}
	defer rows.Close()
	var out []AreaCounts
	for rows.Next() {
		var (
			areaID *string
			c      AreaCounts
		)
		if err := rows.Scan(&areaID, &c.Surface, &c.Drawer, &c.Unscored); err != nil {
			return nil, fmt.Errorf("store: scan area summary row: %w", err)
		}
		if areaID != nil {
			id, perr := ulid.Parse(*areaID)
			if perr != nil {
				return nil, fmt.Errorf("store: parse area id %q: %w", *areaID, perr)
			}
			c.AreaID = &id
		}
		out = append(out, c)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("store: area summary: %w", err)
	}
	return out, nil
}

// AreaConfig builds the Go resolver's areas.Config from the caller's areas
// (ListAreas, which materialises the default plan if needed) — area names in
// position order, feeds resolved to tabs.Tab / label NAMES — plus a
// name→id map, so the agreement test and a future report can resolve against
// the EXACT SAME configuration the SQL above uses, rather than a second
// hand-built one that could drift from what is actually stored.
func (tx *UserTx) AreaConfig(ctx context.Context) (areas.Config, map[string]ulid.ULID, error) {
	views, err := tx.ListAreas(ctx)
	if err != nil {
		return areas.Config{}, nil, err
	}

	var labelIDs []ulid.ULID
	for _, v := range views {
		for _, f := range v.Feeds {
			if f.LabelID != nil {
				labelIDs = append(labelIDs, *f.LabelID)
			}
		}
	}
	labels, err := tx.GetLabels(ctx, labelIDs)
	if err != nil {
		return areas.Config{}, nil, fmt.Errorf("store: area config (labels): %w", err)
	}
	nameByLabel := make(map[ulid.ULID]string, len(labels))
	for _, l := range labels {
		nameByLabel[l.ID] = l.Name
	}

	cfg := areas.Config{}
	byName := make(map[string]ulid.ULID, len(views))
	for _, v := range views {
		a := areasArea(v, nameByLabel)
		cfg.Areas = append(cfg.Areas, a)
		byName[v.Name] = v.ID
	}
	return cfg, byName, nil
}

// areasArea converts one AreaView into an areas.Area, resolving each label
// feed to the NAME internal/areas.Resolve compares against (it works on label
// names, not ids — see areas.Facts).
func areasArea(v AreaView, nameByLabel map[ulid.ULID]string) areas.Area {
	a := areas.Area{Name: v.Name}
	for _, f := range v.Feeds {
		switch {
		case f.Tab != "":
			a.Tabs = append(a.Tabs, f.Tab)
		case f.LabelID != nil:
			if name, ok := nameByLabel[*f.LabelID]; ok {
				a.Labels = append(a.Labels, name)
			}
		}
	}
	return a
}

// AreaLabelProvenance returns, per message, who applied each AREA'S OWN label
// it carries: message id -> area id -> app.message_labels.applied_by (#714).
// The API's Email.areaProvenance picks the resolved area out of it. A message
// carrying no area's own label has no entry.
func (tx *UserTx) AreaLabelProvenance(ctx context.Context, ids []ulid.ULID) (map[ulid.ULID]map[ulid.ULID]string, error) {
	out := map[ulid.ULID]map[ulid.ULID]string{}
	if len(ids) == 0 {
		return out, nil
	}
	rows, err := tx.tx.Query(ctx, `
		SELECT ml.message_id, a.id, ml.applied_by
		  FROM app.message_labels ml
		  JOIN app.areas a ON a.user_id = ml.user_id AND a.label_id = ml.label_id
		 WHERE ml.user_id = $1 AND ml.message_id = ANY($2)`,
		tx.userID.String(), ulidStrings(ids))
	if err != nil {
		return nil, fmt.Errorf("store: area label provenance: %w", err)
	}
	defer rows.Close()
	for rows.Next() {
		var msgStr, areaStr, by string
		if err := rows.Scan(&msgStr, &areaStr, &by); err != nil {
			return nil, fmt.Errorf("store: scan area label provenance: %w", err)
		}
		mid, err := ulid.Parse(msgStr)
		if err != nil {
			return nil, fmt.Errorf("store: parse message id %q: %w", msgStr, err)
		}
		aid, err := ulid.Parse(areaStr)
		if err != nil {
			return nil, fmt.Errorf("store: parse area id %q: %w", areaStr, err)
		}
		if out[mid] == nil {
			out[mid] = map[ulid.ULID]string{}
		}
		out[mid][aid] = by
	}
	return out, rows.Err()
}
