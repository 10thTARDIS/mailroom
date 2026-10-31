// SPDX-License-Identifier: AGPL-3.0-only

//go:build integration

package store

// THE inArea PRE-FILTER (#706) — agreement and plan shape.
//
// An Email/query with `inArea` used to resolve the area of EVERY message the
// user has and only then keep the requested area's. It now resolves only the
// area's CANDIDATES (areaCandidatesSQL, areas_resolve.go): a superset of the
// area, narrowing which messages are resolved, never how. Two tests hold that:
//
//  1. AGREEMENT (TestAreaPrefilterAgreesWithFullResolution): for every area —
//     the default plan's, user-created ones with label feeds, a tab feed,
//     several feeds and a position tie, and an empty one — the restricted
//     predicate selects exactly what the UNRESTRICTED resolver (areaMatchSQL)
//     and ResolveAreas place there, the Go resolver (internal/areas) agrees with
//     ResolveAreas, and QueryEmails with `inArea` returns the same ids and total
//     as the same query restricted to ResolveAreas' answer by id, across
//     collapsed and flat queries, windows, and the desk/side/account/tab/trash
//     filters. Named edge cases are pinned by outcome as well.
//  2. PLAN SHAPE (TestAreaQueryResolvesOnlyCandidatesAtScale): on the 20k
//     fixture, the resolver's DISTINCT ON arms and its message scans see at most
//     the candidate count, never every message — read off EXPLAIN ANALYZE, with
//     the timing reported beside it.

import (
	"context"
	"crypto/sha256"
	"encoding/json"
	"fmt"
	"slices"
	"sort"
	"strings"
	"testing"
	"time"

	"github.com/oklog/ulid/v2"

	"mailroom/internal/areas"
	"mailroom/internal/tabs"
)

// areaPlanRows is what the area resolver saw in one statement, read off
// EXPLAIN (ANALYZE, FORMAT JSON).
type areaPlanRows struct {
	execMS float64
	// arms: rows OUT of each resolver DISTINCT ON (a Unique node with a scan of
	// message_labels beneath it — resolverPasses' definition), rows x loops.
	arms []float64
	// armInputs: rows INTO each of those Unique nodes (what was sorted).
	armInputs []float64
	// messageScans: rows out of every scan of app.messages inside the area
	// SubPlan — the resolver's own scan of the user's messages, and (with the
	// pre-filter) the candidate set's classified-tab arm.
	messageScans []float64
	// areaSubPlans: SubPlans containing a resolver pass. Zero means the walker
	// is not looking at the area predicate at all.
	areaSubPlans int
}

func readAreaPlanRows(t *testing.T, ctx context.Context, tx *UserTx, sql string, args []any) areaPlanRows {
	t.Helper()
	// The EXPLAIN text repeats across areas and shapes, so after five runs the
	// statement cache could hand Postgres a generic plan — not the plan
	// QueryEmails runs, which forces a custom one for an area filter (#697).
	// Measure the shipping configuration, for the rest of this transaction.
	if _, err := tx.tx.Exec(ctx, "SET LOCAL plan_cache_mode = force_custom_plan"); err != nil {
		t.Fatalf("plan mode: %v", err)
	}
	var raw []byte
	if err := tx.tx.QueryRow(ctx, "EXPLAIN (ANALYZE, FORMAT JSON) "+sql, args...).Scan(&raw); err != nil {
		t.Fatalf("explain: %v", err)
	}
	var doc []struct {
		Plan          explainNode `json:"Plan"`
		ExecutionTime float64     `json:"Execution Time"`
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
	var hasResolver func(n explainNode) bool
	hasResolver = func(n explainNode) bool {
		if n.NodeType == "Unique" && readsLabels(n) {
			return true
		}
		for _, c := range n.Plans {
			if hasResolver(c) {
				return true
			}
		}
		return false
	}
	out := areaPlanRows{execMS: doc[0].ExecutionTime}
	var walk func(n explainNode, inArea bool)
	walk = func(n explainNode, inArea bool) {
		if n.Relationship == "SubPlan" && !inArea && hasResolver(n) {
			inArea = true
			out.areaSubPlans++
		}
		if n.NodeType == "Unique" && readsLabels(n) {
			out.arms = append(out.arms, n.Rows*n.Loops)
			in := 0.0
			for _, c := range n.Plans {
				in += c.Rows * c.Loops
			}
			out.armInputs = append(out.armInputs, in)
		}
		if inArea && n.Relation == "messages" {
			out.messageScans = append(out.messageScans, n.Rows*n.Loops)
		}
		for _, c := range n.Plans {
			walk(c, inArea)
		}
	}
	walk(doc[0].Plan, false)
	return out
}

// areaCandidateCount is the candidate set's size for one area, computed HERE
// from the definition (#706) rather than from the store's SQL, so the bound the
// plan test asserts against is not the code under test grading itself: a
// message carrying a label that feeds the area, OR whose stored verdict is a tab
// the area is fed by, OR carrying a tab-mapped label whose tab the area is fed
// by. Also returns how many messages actually resolve there (ResolveAreas'
// SQL twin, unrestricted) and the user's total.
func areaCandidateCount(t *testing.T, d *deskScale, area ulid.ULID) (candidates, resolved, total int) {
	t.Helper()
	ctx := context.Background()
	if err := d.h.super.QueryRow(ctx, `
		SELECT count(*) FILTER (WHERE
		         EXISTS (SELECT 1 FROM app.message_labels ml
		                   JOIN app.area_feeds f ON f.user_id = ml.user_id AND f.label_id = ml.label_id
		                  WHERE ml.user_id = m.user_id AND ml.message_id = m.id AND f.area_id = $2)
		      OR EXISTS (SELECT 1 FROM app.area_feeds f
		                  WHERE f.user_id = m.user_id AND f.area_id = $2 AND f.tab = m.classified_tab)
		      OR EXISTS (SELECT 1 FROM app.message_labels ml
		                   JOIN app.labels l ON l.user_id = ml.user_id AND l.id = ml.label_id
		                   JOIN app.area_feeds f ON f.user_id = l.user_id AND f.tab = l.tab
		                  WHERE ml.user_id = m.user_id AND ml.message_id = m.id AND f.area_id = $2)),
		       count(*)
		  FROM app.messages m WHERE m.user_id = $1`,
		d.userID.String(), area.String()).Scan(&candidates, &total); err != nil {
		t.Fatalf("candidate count: %v", err)
	}
	if err := d.st.UserTx(ctx, d.userID, func(tx *UserTx) error {
		args := []any{d.userID.String()}
		bind := bindFunc(&args)
		return tx.tx.QueryRow(ctx, `SELECT count(*) FROM app.messages m WHERE m.user_id = $1 AND `+
			areaMatchSQL(bind, "$1", areaResolvedSQL+" = "+bind(area.String())), args...).Scan(&resolved)
	}); err != nil {
		t.Fatalf("resolved count: %v", err)
	}
	return candidates, resolved, total
}

func fmtRows(xs []float64) string {
	parts := make([]string, len(xs))
	for i, x := range xs {
		parts[i] = fmt.Sprintf("%.0f", x)
	}
	return "[" + strings.Join(parts, " ") + "]"
}

func TestAreaQueryResolvesOnlyCandidatesAtScale(t *testing.T) {
	d := seedDeskScale(t, deskScaleN)
	ctx := context.Background()
	yes, no := true, false

	var views []AreaView
	if err := d.st.UserTx(ctx, d.userID, func(tx *UserTx) error {
		var err error
		views, err = tx.ListAreas(ctx)
		return err
	}); err != nil {
		t.Fatal(err)
	}

	// The drawer's Email/query — what #706 measured on a real instance: one area,
	// on the desk, below the line, not snoozed, thread-collapsed, by effective
	// arrival, one page — and the area view without the desk.
	shapes := []struct {
		name string
		q    func(area ulid.ULID) EmailQuery
		desk bool // resolved over the desk (areaInDeskSQL), not C(X)
	}{
		{"drawer", func(a ulid.ULID) EmailQuery {
			return EmailQuery{InArea: &a, OnDesk: &yes, OnSurface: &no, LineThreshold: 0.5, Snoozed: &no,
				CollapseThreads: true, SortByEffectiveArrival: true, Limit: 50, LimitSet: true}
		}, true},
		{"area view", func(a ulid.ULID) EmailQuery {
			return EmailQuery{InArea: &a, CollapseThreads: true, Limit: 50, LimitSet: true}
		}, false},
	}

	// With onDesk the resolver is scoped to the desk (#706), so its bound is the
	// desk's size, not C(X)'s. Non-vacuity: the desk must be smaller than the
	// whole mailbox, or the control's full resolution would pass the bound too.
	var deskCount, mailboxCount int
	if err := d.st.UserTx(ctx, d.userID, func(tx *UserTx) error {
		args := []any{d.userID.String()}
		return tx.tx.QueryRow(ctx, `SELECT count(*) FILTER (WHERE `+deskMessageSQL(bindFunc(&args), d.userID, nil)+`), count(*)
		  FROM app.messages m WHERE m.user_id = $1`, args...).Scan(&deskCount, &mailboxCount)
	}); err != nil {
		t.Fatal(err)
	}
	if deskCount == 0 || deskCount >= mailboxCount {
		t.Fatalf("desk holds %d of %d messages — the fixture cannot separate a desk-scoped resolution from a full one", deskCount, mailboxCount)
	}

	// The CONTROL, measured in the same run: Needs sorting has no pre-filter
	// (it cannot — see AreaPredicate), so its statement shows what a full
	// resolution looks like on this fixture. If the walker could not see a
	// full resolution, the bounds below would pass for the wrong reason.
	var control areaPlanRows
	if err := d.st.UserTx(ctx, d.userID, func(tx *UserTx) error {
		if _, _, err := tx.QueryEmails(ctx, EmailQuery{Unsorted: true, CollapseThreads: true, Limit: 50, LimitSet: true}); err != nil {
			return err
		}
		stmt := d.rec.find(t, "count(*) OVER () AS total FROM ranked")
		control = readAreaPlanRows(t, ctx, tx, stmt.sql, stmt.args)
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	t.Logf("control (Needs sorting, full resolution): arms out %s in %s, message scans %s, %.1f ms",
		fmtRows(control.arms), fmtRows(control.armInputs), fmtRows(control.messageScans), control.execMS)
	if control.areaSubPlans != 1 || len(control.arms) != 2 || len(control.messageScans) == 0 ||
		slices.Max(control.messageScans) < float64(deskScaleN) {
		t.Fatalf("control: the walker does not see a full resolution (%d area SubPlans, arms %s, message scans %s)",
			control.areaSubPlans, fmtRows(control.arms), fmtRows(control.messageScans))
	}

	for _, v := range views {
		cands, resolved, total := areaCandidateCount(t, d, v.ID)
		// Non-vacuity: a bound at or above every message cannot tell a
		// restricted resolution from a full one.
		if cands == 0 || resolved == 0 || cands > total/2 {
			t.Fatalf("area %s: %d candidates, %d resolved, %d messages — the fixture cannot separate a "+
				"restricted resolution from a full one", v.Name, cands, resolved, total)
		}
		for _, sh := range shapes {
			q := sh.q(v.ID)
			var n int
			took := timed(t, fmt.Sprintf("%s %s Email/query", v.Name, sh.name), func() error {
				return d.st.UserTx(ctx, d.userID, func(tx *UserTx) error {
					var err error
					_, n, err = tx.QueryEmails(ctx, q)
					return err
				})
			})
			stmt := d.rec.find(t, "count(*) OVER () AS total FROM ranked")
			var rows areaPlanRows
			if err := d.st.UserTx(ctx, d.userID, func(tx *UserTx) error {
				rows = readAreaPlanRows(t, ctx, tx, stmt.sql, stmt.args)
				if bad, text := perRowWork(t, ctx, tx, stmt.sql, stmt.args); len(bad) > 0 {
					t.Errorf("%s %s does per-message work (#660): %s\n%s", v.Name, sh.name, strings.Join(bad, "; "), text)
				}
				return nil
			}); err != nil {
				t.Fatal(err)
			}
			t.Logf("%-10s %-9s total %5d | %5d candidates, %5d resolve here, %d messages | "+
				"arms out %s in %s, message scans %s | %4d ms wall, %.1f ms exec",
				v.Name, sh.name, n, cands, resolved, total,
				fmtRows(rows.arms), fmtRows(rows.armInputs), fmtRows(rows.messageScans), took.Milliseconds(), rows.execMS)

			if rows.areaSubPlans != 1 || len(rows.arms) != 2 || len(rows.messageScans) == 0 {
				t.Errorf("%s %s: %d area SubPlans, %d resolver arms, %d message scans in it — want one resolver pass "+
					"(two arms) the walker can read", v.Name, sh.name, rows.areaSubPlans, len(rows.arms), len(rows.messageScans))
				continue
			}
			// ROWS, NOT TIME. Each arm emits at most one row per message, and
			// every message the pre-filter lets through is a candidate, so
			// nothing in the resolver may see more messages than that. A full
			// resolution sees every message (the control above).
			bound := cands
			if sh.desk {
				bound = deskCount
			}
			for i, r := range rows.arms {
				if r > float64(bound) {
					t.Errorf("%s %s: resolver arm %d emitted %.0f rows, more than its bound of %d (candidates, or the desk for onDesk) — "+
						"it is resolving messages that cannot be in the area (#706)", v.Name, sh.name, i, r, bound)
				}
			}
			for i, r := range rows.messageScans {
				if r > float64(bound) {
					t.Errorf("%s %s: message scan %d in the area predicate returned %.0f rows, more than its bound "+
						"of %d (candidates, or the desk for onDesk; of %d messages) — the resolver is not restricted (#706)",
						v.Name, sh.name, i, r, bound, total)
				}
			}
			if took > deskScaleBound {
				t.Errorf("%s %s took %v, bound %v", v.Name, sh.name, took, deskScaleBound)
			}
		}
	}
}

// prefilterFixture is seedDeskScale's realistic bulk plus a hand-made set of
// user-created areas and edge-case messages, each named for what it tests.
type prefilterFixture struct {
	d     *deskScale
	areas map[string]ulid.ULID // by name
	msgs  map[string]ulid.ULID // edge cases, by name
}

func seedPrefilterFixture(t *testing.T) *prefilterFixture {
	t.Helper()
	d := seedDeskScale(t, 1200)
	shapes := deskAgreementExtras(t, d)
	for name, n := range shapes {
		if n == 0 {
			t.Fatalf("fixture shape %q touched no message", name)
		}
	}
	ctx := context.Background()
	f := &prefilterFixture{d: d, areas: map[string]ulid.ULID{}, msgs: map[string]ulid.ULID{}}
	u := d.userID.String()

	// THE PLAN. The default five (Primary 0 … Forums 4, each fed by its tab and
	// its own tab-mapped label; seedDeskScale adds Deals to the last) plus:
	//   Work     position 1 — TIES Social on position; fed by its own label and
	//            a second label, Clients. Created after Social, so its id is
	//            greater and Social wins the tie (ORDER BY a.position, a.id).
	//   Reading  position 6 — fed by the FORUMS TAB (taken from the Forums area,
	//            which keeps its own label and Deals) and a label, Books.
	//   Empty    position 7 — fed only by its own label, which nothing carries.
	// Plus labels that feed no area: Groups (mapped to forums), Friends (mapped
	// to social). And the Social TAB IS DISABLED — the strip folds it into
	// Primary, which the area resolver deliberately does not (see
	// areaCandidatesSQL).
	labels := map[string]ulid.ULID{}
	if err := d.st.UserTx(ctx, d.userID, func(tx *UserTx) error {
		views, err := tx.ListAreas(ctx)
		if err != nil {
			return err
		}
		for _, v := range views {
			f.areas[v.Name] = v.ID
			labels[v.Name] = v.LabelID
		}
		for _, l := range []struct {
			name string
			tab  tabs.Tab
		}{{"Clients", ""}, {"Books", ""}, {"Misc", ""}, {"Groups", tabs.Forums}, {"Friends", tabs.Social}} {
			id, err := tx.CreateLabel(ctx, LabelRow{Name: l.name, Tab: l.tab})
			if err != nil {
				return err
			}
			labels[l.name] = id
		}
		for _, name := range []string{"Deals", "Newsletters"} { // seedDeskScale's
			var id string
			if err := tx.tx.QueryRow(ctx, `SELECT id FROM app.labels WHERE user_id = $1 AND name = $2`, u, name).Scan(&id); err != nil {
				return fmt.Errorf("label %s: %w", name, err)
			}
			labels[name] = mustULID(t, id)
		}
		deals := labels["Deals"]
		if found, err := tx.UpdateArea(ctx, f.areas["Forums"], AreaPatch{SetFeeds: true, Feeds: []AreaFeed{{LabelID: &deals}}}); err != nil || !found {
			return fmt.Errorf("strip forums tab from Forums: found=%v err=%w", found, err)
		}
		clients, books := labels["Clients"], labels["Books"]
		for _, a := range []AreaRow{
			{Name: "Work", Position: 1, Feeds: []AreaFeed{{LabelID: &clients}}},
			{Name: "Reading", Position: 6, Feeds: []AreaFeed{{Tab: tabs.Forums}, {LabelID: &books}}},
			{Name: "Empty", Position: 7},
		} {
			id, err := tx.CreateArea(ctx, a)
			if err != nil {
				return fmt.Errorf("create %s: %w", a.Name, err)
			}
			f.areas[a.Name] = id
		}
		got, err := tx.GetAreas(ctx, []ulid.ULID{f.areas["Work"], f.areas["Reading"], f.areas["Empty"]})
		if err != nil {
			return err
		}
		for _, v := range got {
			labels[v.Name] = v.LabelID
		}
		cts, err := tx.ListCategoryTabs(ctx)
		if err != nil {
			return err
		}
		for _, c := range cts {
			if c.Tab == tabs.Social {
				off := false
				if _, err := tx.UpdateCategoryTab(ctx, c.ID, CategoryTabPatch{Enabled: &off}); err != nil {
					return err
				}
			}
		}
		enabled, err := tx.EnabledTabs(ctx)
		if err != nil {
			return err
		}
		if enabled[tabs.Social] {
			return fmt.Errorf("the Social tab is still enabled")
		}
		return nil
	}); err != nil {
		t.Fatalf("build plan: %v", err)
	}

	var inbox string
	if err := d.h.super.QueryRow(ctx, `SELECT id FROM app.mailboxes WHERE user_id = $1 AND account_id = $2 AND imap_name = 'INBOX'`,
		u, d.accountA.String()).Scan(&inbox); err != nil {
		t.Fatal(err)
	}
	exec := func(sql string, args ...any) {
		t.Helper()
		if _, err := d.h.super.Exec(ctx, sql, args...); err != nil {
			t.Fatalf("seed edge case: %v\n%s", err, sql)
		}
	}
	// Every edge message is live in INBOX, scored above the line and newest
	// first, so it is on the desk and on page one of its area's queries.
	seed := func(name string, classified tabs.Tab, trashed bool, withLabels ...string) {
		t.Helper()
		id, thread := NewULID(), NewULID()
		h := sha256.Sum256([]byte(u + "-edge-" + name))
		exec(`INSERT INTO app.blobs (user_id, content_hash, size_plain, size_stored, dek_id) VALUES ($1,$2,10,20,$3)`,
			u, h[:], NewULID().String())
		exec(`INSERT INTO app.threads (id, user_id, subject) VALUES ($1, $2, 'edge')`, thread.String(), u)
		var ct any
		if classified != "" {
			ct = string(classified)
		}
		var trashedAt any
		if trashed {
			trashedAt = time.Now().UTC()
		}
		exec(`INSERT INTO app.messages (id, user_id, content_hash, thread_id, subject, received_at, size_bytes,
		                                classified_tab, trashed_at, purge_after)
		      VALUES ($1,$2,$3,$4,$5, now() + interval '1 hour', 10, $6::app.category_tab, $7::timestamptz,
		              $7::timestamptz + interval '30 days')`,
			id.String(), u, h[:], thread.String(), name, ct, trashedAt)
		exec(`INSERT INTO app.message_provenance (id, user_id, message_id, account_id, mailbox_id, uid, uidvalidity, state)
		      VALUES ($1,$2,$3,$4,$5,$6,1,'live')`, NewULID().String(), u, id.String(), d.accountA.String(), inbox, 800000+testUID())
		exec(`INSERT INTO app.triage_scores (user_id, message_id, score, model_version) VALUES ($1,$2,0.9,'edge')`, u, id.String())
		for _, l := range withLabels {
			lid, ok := labels[l]
			if !ok {
				t.Fatalf("edge case %s: no label %q", name, l)
			}
			exec(`INSERT INTO app.message_labels (user_id, message_id, label_id) VALUES ($1,$2,$3)`, u, id.String(), lid.String())
		}
		f.msgs[name] = id
	}
	seed("two area labels", "", false, "Promotions", "Updates")
	seed("earlier position wins", "", false, "Updates", "Work")
	seed("position tie", "", false, "Work", "Social")
	seed("second label feed", "", false, "Clients")
	seed("label beats tab", tabs.Updates, false, "Work")
	seed("mapped tab overrides classified", tabs.Promotions, false, "Newsletters")
	seed("disabled tab, classified", tabs.Social, false)
	seed("disabled tab, mapped label", tabs.Primary, false, "Friends")
	seed("tab moved to a new area", "", false, "Groups")
	seed("classified tab follows it to a new area", tabs.Forums, false)
	seed("no classification", "", false)
	seed("label feeds nothing, no classification", "", false, "Misc")
	seed("trashed", "", true, "Promotions")
	for _, tbl := range []string{"messages", "message_labels", "labels", "area_feeds", "areas", "triage_scores", "message_provenance"} {
		exec(`ANALYZE app.` + tbl)
	}
	return f
}

func TestAreaPrefilterAgreesWithFullResolution(t *testing.T) {
	f := seedPrefilterFixture(t)
	d := f.d
	ctx := context.Background()

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

	// 1. ResolveAreas (SQL, unrestricted logic) for every message, and the Go
	// resolver for every live one (AreaReportRowsAfter walks live mail only).
	var (
		sqlRes map[ulid.ULID]AreaResolution
		cfg    areas.Config
		byName map[string]ulid.ULID
		report []AreaReportRow
	)
	if err := d.st.UserTx(ctx, d.userID, func(tx *UserTx) error {
		var e error
		if sqlRes, e = tx.ResolveAreas(ctx, allIDs); e != nil {
			return e
		}
		if cfg, byName, e = tx.AreaConfig(ctx); e != nil {
			return e
		}
		after := ulid.ULID{}
		for {
			page, e := tx.AreaReportRowsAfter(ctx, after, 500)
			if e != nil {
				return e
			}
			report = append(report, page...)
			if len(page) < 500 {
				return nil
			}
			after = page[len(page)-1].ID
		}
	}); err != nil {
		t.Fatal(err)
	}
	if len(sqlRes) != len(allIDs) {
		t.Fatalf("ResolveAreas answered %d of %d messages", len(sqlRes), len(allIDs))
	}
	goChecked := 0
	for _, row := range report {
		tab, known := row.ResolvedTab()
		res := cfg.Resolve(areas.Facts{Labels: row.Labels, Tab: tab, TabKnown: known})
		got := sqlRes[row.ID]
		var want *ulid.ULID
		if !res.Reason.Unsorted() {
			id, ok := byName[res.Area]
			if !ok {
				t.Fatalf("Go resolved %q, which AreaConfig does not know", res.Area)
			}
			want = &id
		}
		if !ulidEq(got.AreaID, want) {
			t.Errorf("message %s: Go resolver says %v (%s), ResolveAreas says %v", row.ID, want, res.Reason, got.AreaID)
		}
		goChecked++
	}
	if goChecked < len(allIDs)/2 {
		t.Fatalf("the Go resolver checked only %d of %d messages", goChecked, len(allIDs))
	}

	// 2. The named edge cases, by outcome, so agreement cannot hide both sides
	// being wrong the same way.
	pins := map[string]string{
		"two area labels":                         "Promotions", // position 2 beats Updates' 3
		"earlier position wins":                   "Work",       // position 1 beats Updates' 3
		"position tie":                            "Social",     // both position 1; Social's id is smaller
		"second label feed":                       "Work",
		"label beats tab":                         "Work",    // classified updates, but a label decides first
		"mapped tab overrides classified":         "Updates", // Newsletters maps to updates; verdict says promotions
		"disabled tab, classified":                "Social",  // NOT Primary: areas do not fold disabled tabs
		"disabled tab, mapped label":              "Social",  // Friends maps to (disabled) social; verdict primary
		"tab moved to a new area":                 "Reading", // Groups maps to forums, now Reading's tab
		"classified tab follows it to a new area": "Reading", // the forums verdict follows the tab to Reading
		"no classification":                       "",
		"label feeds nothing, no classification":  "",
		"trashed":                                 "Promotions",
	}
	for name, want := range pins {
		id, ok := f.msgs[name]
		if !ok {
			t.Fatalf("no edge message %q", name)
		}
		got := sqlRes[id].AreaID
		var wantID *ulid.ULID
		if want != "" {
			w := f.areas[want]
			wantID = &w
		}
		if !ulidEq(got, wantID) {
			t.Errorf("edge case %q: resolved to %v, want %s (%v)", name, got, want, wantID)
		}
	}

	// The area set ResolveAreas defines, per area. Every area must hold mail
	// except Empty, and Empty must hold none — both are comparisons the
	// pre-filter has to get right.
	areaIDs := make([]ulid.ULID, 0, len(f.areas))
	for _, id := range f.areas {
		areaIDs = append(areaIDs, id)
	}
	sort.Slice(areaIDs, func(i, j int) bool { return areaIDs[i].Compare(areaIDs[j]) < 0 })
	members := map[ulid.ULID][]ulid.ULID{}
	for _, id := range allIDs {
		if a := sqlRes[id].AreaID; a != nil {
			members[*a] = append(members[*a], id)
		}
	}
	nameOf := map[ulid.ULID]string{}
	for n, id := range f.areas {
		nameOf[id] = n
	}
	for _, a := range areaIDs {
		if n := len(members[a]); (n == 0) != (nameOf[a] == "Empty") {
			t.Fatalf("area %s holds %d messages — the fixture does not exercise it as intended", nameOf[a], n)
		}
	}

	// 3. THE PREDICATE: the restricted AreaPredicate against the UNRESTRICTED
	// areaMatchSQL and against ResolveAreas, over every message of the user.
	selectIDs := func(pred func(bind func(any) string) string) []ulid.ULID {
		t.Helper()
		var out []ulid.ULID
		if err := d.st.UserTx(ctx, d.userID, func(tx *UserTx) error {
			args := []any{d.userID.String()}
			rows, err := tx.tx.Query(ctx, `SELECT m.id FROM app.messages m WHERE m.user_id = $1 AND `+
				pred(bindFunc(&args))+` ORDER BY m.id`, args...)
			if err != nil {
				return err
			}
			defer rows.Close()
			for rows.Next() {
				var s string
				if err := rows.Scan(&s); err != nil {
					return err
				}
				out = append(out, mustULID(t, s))
			}
			return rows.Err()
		}); err != nil {
			t.Fatalf("predicate: %v", err)
		}
		return out
	}
	for _, a := range areaIDs {
		restricted := selectIDs(func(bind func(any) string) string { return AreaPredicate(&a, bind) })
		full := selectIDs(func(bind func(any) string) string {
			return areaMatchSQL(bind, "$1", areaResolvedSQL+" = "+bind(a.String()))
		})
		want := members[a] // allIDs is in id order, so this is too
		if !slices.Equal(restricted, full) || !slices.Equal(restricted, want) {
			t.Errorf("area %s: restricted predicate selects %d, unrestricted %d, ResolveAreas places %d\n"+
				"  restricted-only %v\n  missing %v", nameOf[a], len(restricted), len(full), len(want),
				setMinus(restricted, want), setMinus(want, restricted))
		}
	}

	// 4. Email/query: QueryEmails with inArea against the SAME query with the
	// area replaced by ResolveAreas' answer as an id restriction — one compiler
	// on both sides, so only the area condition differs.
	yes, no := true, false
	acct := d.accountA
	bases := []struct {
		name string
		q    EmailQuery
	}{
		{"plain", EmailQuery{}},
		{"onDesk", EmailQuery{OnDesk: &yes}},
		{"offDesk", EmailQuery{OnDesk: &no}},
		{"surface", EmailQuery{OnDesk: &yes, OnSurface: &yes, LineThreshold: 0.5, Snoozed: &no}},
		{"drawer", EmailQuery{OnDesk: &yes, OnSurface: &no, LineThreshold: 0.25, Snoozed: &no}},
		{"account", EmailQuery{OnDesk: &yes, InAccount: &acct}},
		{"inTab primary+social", EmailQuery{InTab: []Tab{tabs.Primary, tabs.Social}}},
		{"trashed", EmailQuery{OnlyTrashed: true}},
	}
	windows := []struct{ position, limit int }{{0, 50}, {5, 7}, {0, 0}, {10000, 5}}
	var compared, nonEmpty, windowed, collapsedDiffer int
	seen := map[ulid.ULID]bool{}
	for _, a := range areaIDs {
		for _, b := range bases {
			for _, collapse := range []bool{false, true} {
				for _, w := range windows {
					q := b.q
					q.CollapseThreads = collapse
					q.SortByEffectiveArrival = collapse // both orders, one per collapse mode
					q.Position, q.Limit, q.LimitSet = w.position, w.limit, true
					ref := q
					ref.RestrictIDs = append([]ulid.ULID{}, members[a]...) // non-nil even when empty
					q.InArea = &a
					var (
						gotIDs, refIDs     []ulid.ULID
						gotTotal, refTotal int
					)
					if err := d.st.UserTx(ctx, d.userID, func(tx *UserTx) error {
						var err error
						if gotIDs, gotTotal, err = tx.QueryEmails(ctx, q); err != nil {
							return err
						}
						refIDs, refTotal, err = tx.QueryEmails(ctx, ref)
						return err
					}); err != nil {
						t.Fatalf("%s/%s: %v", nameOf[a], b.name, err)
					}
					name := fmt.Sprintf("area %s, %s, collapse=%v, window %d+%d", nameOf[a], b.name, collapse, w.position, w.limit)
					if gotTotal != refTotal || !slices.Equal(gotIDs, refIDs) {
						t.Errorf("%s: inArea total %d ids %v; full resolution total %d ids %v",
							name, gotTotal, gotIDs, refTotal, refIDs)
					}
					compared++
					if refTotal > 0 {
						nonEmpty++
					}
					if refTotal > w.position+w.limit && w.limit > 0 {
						windowed++
					}
					for _, id := range gotIDs {
						seen[id] = true
					}
					if collapse && w.limit == 50 {
						flat := ref
						flat.CollapseThreads, flat.Limit, flat.Position = false, 0, 0
						var n int
						if err := d.st.UserTx(ctx, d.userID, func(tx *UserTx) error {
							var err error
							_, n, err = tx.QueryEmails(ctx, flat)
							return err
						}); err != nil {
							t.Fatal(err)
						}
						if n > refTotal {
							collapsedDiffer++
						}
					}
				}
			}
		}
	}
	t.Logf("compared %d inArea queries: %d non-empty, %d cut by their window, %d where collapsing changed the total",
		compared, nonEmpty, windowed, collapsedDiffer)
	if nonEmpty < compared/3 || windowed == 0 || collapsedDiffer == 0 {
		t.Fatalf("the comparison is vacuous: %d/%d non-empty, %d windowed, %d collapsed", nonEmpty, compared, windowed, collapsedDiffer)
	}
	// Every edge case that has an area must have come back from some inArea
	// query, so its resolution was compared through QueryEmails too.
	for name, want := range pins {
		if want != "" && !seen[f.msgs[name]] {
			t.Errorf("edge case %q never appeared in an inArea page — it was not compared through Email/query", name)
		}
	}
}

// setMinus is a minus b, for failure messages.
func setMinus(a, b []ulid.ULID) []ulid.ULID {
	in := map[ulid.ULID]bool{}
	for _, x := range b {
		in[x] = true
	}
	var out []ulid.ULID
	for _, x := range a {
		if !in[x] {
			out = append(out, x)
		}
	}
	return out
}

// TestAreaOnDeskScopeAgreesWithFullResolution pins #706: inArea + onDesk
// resolves only the desk's messages (areaInDeskSQL), and must select exactly
// the area's members (ResolveAreas, unrestricted) that are on the desk
// (the onDesk filter alone). The fixture has area members that are off the
// desk (the trashed one), desk mail in other areas, and mail fed by tabs and
// by labels; every area is checked, with and without an account.
func TestAreaOnDeskScopeAgreesWithFullResolution(t *testing.T) {
	f := seedPrefilterFixture(t)
	d := f.d
	ctx := context.Background()
	yes := true
	acct := d.accountA

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

	query := func(tx *UserTx, q EmailQuery) []ulid.ULID {
		t.Helper()
		q.Limit, q.LimitSet = 1000, true
		ids, _, err := tx.QueryEmails(ctx, q)
		if err != nil {
			t.Fatal(err)
		}
		return ids
	}
	var checked, nonEmpty, offDeskMembers, deskElsewhere int
	for _, withAccount := range []bool{false, true} {
		if err := d.st.UserTx(ctx, d.userID, func(tx *UserTx) error {
			res, err := tx.ResolveAreas(ctx, allIDs)
			if err != nil {
				return err
			}
			base := EmailQuery{OnDesk: &yes}
			if withAccount {
				base.InAccount = &acct
			}
			onDesk := query(tx, base)
			deskSet := map[ulid.ULID]bool{}
			for _, id := range onDesk {
				deskSet[id] = true
			}
			for name, a := range f.areas {
				a := a
				var want []ulid.ULID
				for _, id := range allIDs {
					if r := res[id].AreaID; r != nil && *r == a {
						if deskSet[id] {
							want = append(want, id)
						} else {
							offDeskMembers++
						}
					}
				}
				q := base
				q.InArea = &a
				got := query(tx, q)
				sort.Slice(got, func(i, j int) bool { return got[i].Compare(got[j]) < 0 })
				if !slices.Equal(got, want) {
					t.Errorf("area %s (account=%v): inArea+onDesk selects %v, unrestricted resolution ∩ desk is %v",
						name, withAccount, got, want)
				}
				checked++
				if len(want) > 0 {
					nonEmpty++
				}
			}
			for _, id := range onDesk {
				if r := res[id].AreaID; r == nil {
					deskElsewhere++
				}
			}
			return nil
		}); err != nil {
			t.Fatal(err)
		}
	}
	if nonEmpty < 4 || offDeskMembers == 0 {
		t.Fatalf("vacuous: %d/%d areas non-empty, %d off-desk members", nonEmpty, checked, offDeskMembers)
	}
	n := 0
	t.Log("generated:", AreaPredicateOnDesk(f.areas["Promotions"], func(any) string { n++; return fmt.Sprintf("$%d", n+1) }, "(<desk predicate>)"))
}
