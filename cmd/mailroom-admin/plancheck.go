// SPDX-License-Identifier: AGPL-3.0-only

package main

import (
	"context"
	"flag"
	"fmt"
	"io"
	"os"
	"sort"
	"strings"
	"time"

	"github.com/oklog/ulid/v2"

	"mailroom/internal/config"
	"mailroom/internal/store"
)

// `mailroom-admin plan-check` — do the hot read paths suffer from generic
// plans? READ-ONLY: one read-only transaction, always rolled back.
//
// WHY IT EXISTS. PostgreSQL caches a parameter-blind "generic" plan for a
// prepared statement, and pgx prepares statements by default. Every hot table
// leads its indexes with user_id, and on a single-user instance user_id has
// one distinct value, so a generic plan estimates "this user's rows" as nearly
// the whole table and can choose a sequential scan where a plan made with the
// actual values would walk an index. Statements found to suffer from it opt out
// with store.planForValues (plans.go). This command answers the same question
// for the next candidates BEFORE anything is changed: for each hot read path it
// runs the real store function the API runs, with the planner forced to the
// generic plan and then forced to a custom (value-aware) plan, and compares
// wall time. A path whose generic plan is clearly slower is a candidate for
// planForValues; a path where the modes agree is left alone.
//
// WHAT IT RUNS. The store functions behind Area/getSummary, Area/getDesk,
// Email/query (the first page of the user's largest area, in the default sort,
// thread-collapsed as the web client asks) and Mailbox/get (all mailboxes),
// with arguments built the way the API handlers build them. No SQL lives here.
//
// READING THE RESULT. Medians over --runs, plus the first run of each mode
// reported separately: the first generic run also pays for pgx preparing the
// statement, and the first custom run may be warmer or colder for the data
// pages, so the cold figures are context and the medians carry the verdict.
// Run it a couple of times; a small or cold database can show noise. A path
// that plans its own statements is not affected by either mode: the desk
// snapshot and an area's Email/query set plan_cache_mode=force_custom_plan for
// themselves (#665, #697), so their two columns differ only by noise and warm
// pages, and their verdict says so instead of naming a candidate
// (planSelfPlanned). The
// diagnostic needs a user whose desk has been opened at least once (areas and
// desk preferences exist): materialising them is a write, which this command
// refuses.
func runPlanCheck(ctx context.Context, args []string) error {
	fs := flag.NewFlagSet("plan-check", flag.ContinueOnError)
	var (
		userIDStr = fs.String("user-id", "", "user id (ULID)")
		runs      = fs.Int("runs", 3, "timed runs per path per plan mode")
		explain   = fs.Bool("explain", false, "print the real EXPLAIN (ANALYZE, BUFFERS) plan of every statement each path sends")
		pathSel   = fs.String("path", "", "with --explain: only the path whose name starts with this (e.g. Area/getDesk)")
	)
	if err := fs.Parse(args); err != nil {
		return err
	}
	userID, err := parseUserID(*userIDStr)
	if err != nil {
		return err
	}
	if *runs < 1 || *runs > 50 {
		return fmt.Errorf("--runs %d: must be between 1 and 50", *runs)
	}
	if *pathSel != "" && !*explain {
		return fmt.Errorf("--path only applies with --explain")
	}
	if *explain {
		cfg, err := config.Load()
		if err != nil {
			return err
		}
		st, rec, err := store.OpenRecording(ctx, cfg.DatabaseURL)
		if err != nil {
			return fmt.Errorf("open store: %w", err)
		}
		defer st.Close()
		return planExplain(ctx, os.Stdout, st, rec, userID, *pathSel)
	}
	d, err := openDeps(ctx)
	if err != nil {
		return err
	}
	defer d.close()
	return planCheck(ctx, os.Stdout, d.store, userID, *runs)
}

// planSourceNone is the Source of a path that reads no area.
const planSourceNone = "none"

const (
	// planCheckStatementTimeout is the safety net on every statement: the point
	// of the command is to force the worse plan, which can be a slow one.
	planCheckStatementTimeout = 30 * time.Second
	// planCheckPageSize is the web client's list page (web/src/core/sync.ts
	// PAGE_SIZE), which is also Area/getDesk's default section window.
	planCheckPageSize = 50
	// A path is a candidate when the generic median is at least this many times
	// the custom median AND at least planCheckMinGap slower.
	planCheckMinRatio = 2.0
	planCheckMinGap   = 5 * time.Millisecond
)

// planPathResult is one hot path's timings in both plan modes.
type planPathResult struct {
	Name    string
	Skipped string // non-empty: why the path could not be exercised
	Generic []time.Duration
	Custom  []time.Duration
	Rows    int
	// Source is where the path reads a message's area: stored, resolver, or
	// none for a path that does not use areas (ADR-049).
	Source string
}

// planPath is one hot path: run calls the real store function(s) and returns
// how many rows came back.
// planSelfPlanned names the paths whose store code forces a custom plan for
// its own statements, overriding the mode this command sets. A gap between
// their columns is not a plan-mode effect, and reading it as one sent a fix
// after a cost that was really a sort (#27).
var planSelfPlanned = map[string]bool{
	planDeskPath:  true,
	planEmailPath: true,
}

const (
	planDeskPath  = "Area/getDesk: DeskSnapshot"
	planEmailPath = "Email/query: largest area, first page"
)

type planPath struct {
	name string
	// source is store.AreaSourceStored / AreaSourceResolver, or "none".
	source string
	run    func(ctx context.Context, tx *store.UserTx) (int, error)
}

// planCheck does the work of the command against st and writes the report to
// w. Everything happens in one read-only transaction that is rolled back.
func planCheck(ctx context.Context, w io.Writer, st *store.Store, userID ulid.ULID, runs int) error {
	var results []planPathResult
	if err := st.UserReadTx(ctx, userID, func(tx *store.UserTx) error {
		var e error
		results, e = planCheckRun(ctx, tx, runs)
		return e
	}); err != nil {
		return fmt.Errorf("plan-check: %w (a user whose desk was never opened has no areas or desk preferences yet, and this command cannot create them)", err)
	}
	printPlanCheck(w, results, runs)
	return nil
}

// planCheckRun builds the paths from the user's own state (read in the same
// transaction) and times them. The modes are transaction-local; the rollback restores the default.
func planCheckRun(ctx context.Context, tx *store.UserTx, runs int) ([]planPathResult, error) {
	if err := tx.SetStatementTimeout(ctx, planCheckStatementTimeout); err != nil {
		return nil, err
	}
	paths, emailPath, err := planCheckPaths(ctx, tx)
	if err != nil {
		return nil, err
	}
	return planCheckTimeAll(ctx, tx, paths, emailPath, runs)
}

// planCheckPaths builds the hot paths from the user's own state (read in tx),
// exactly as the API handlers do. emailPath is nil when no area holds mail.
func planCheckPaths(ctx context.Context, tx *store.UserTx) (paths []planPath, emailPath *planPath, err error) {

	// Setup, exactly as the API handlers do it, untimed.
	views, err := tx.ListAreas(ctx)
	if err != nil {
		return nil, nil, err
	}
	prefs, err := tx.GetDeskPreferences(ctx)
	if err != nil {
		return nil, nil, err
	}
	scope := store.DeskScope{Threshold: prefs.LineThreshold}
	src, err := tx.AreaSource(ctx)
	if err != nil {
		return nil, nil, err
	}
	counts, err := tx.AreaSummary(ctx, scope)
	if err != nil {
		return nil, nil, err
	}
	var largest *ulid.ULID
	best := 0
	for _, c := range counts {
		if n := c.Surface + c.Drawer + c.Unscored; c.AreaID != nil && n > best {
			largest, best = c.AreaID, n
		}
	}
	// Area/getDesk as the Review-first desk asks: every area, then Needs sorting.
	sections := make([]store.DeskSectionPage, 0, len(views)+1)
	for _, v := range views {
		id := v.ID
		sections = append(sections, store.DeskSectionPage{AreaID: &id, Limit: planCheckPageSize})
	}
	sections = append(sections, store.DeskSectionPage{Limit: planCheckPageSize})

	paths = []planPath{
		{"Area/getSummary: AreaSummary", src, func(ctx context.Context, tx *store.UserTx) (int, error) {
			c, err := tx.AreaSummary(ctx, scope)
			return len(c), err
		}},
		{"Area/getSummary: DrawerArchiveCounts", planSourceNone, func(ctx context.Context, tx *store.UserTx) (int, error) {
			m, err := tx.DrawerArchiveCounts(ctx)
			return len(m), err
		}},
		{planDeskPath, src, func(ctx context.Context, tx *store.UserTx) (int, error) {
			s, err := tx.DeskSnapshot(ctx, store.DeskSnapshotQuery{Scope: scope, Sections: sections})
			n := 0
			for _, sec := range s.Sections {
				n += len(sec.IDs)
			}
			return n, err
		}},
		{"Mailbox/get: all mailboxes", planSourceNone, func(ctx context.Context, tx *store.UserTx) (int, error) {
			ids, err := tx.ListMailboxIDs(ctx)
			if err != nil {
				return 0, err
			}
			mv, err := tx.GetMailboxes(ctx, ids)
			return len(mv), err
		}},
	}
	if largest != nil && best > 0 {
		area := *largest
		emailPath = &planPath{planEmailPath, src, func(ctx context.Context, tx *store.UserTx) (int, error) {
			// What the web client sends for an area view: inArea, the default
			// returnedAt-descending sort, thread-collapsed, one page.
			ids, _, err := tx.QueryEmails(ctx, store.EmailQuery{
				InArea:                 &area,
				SortByEffectiveArrival: true,
				CollapseThreads:        true,
				Limit:                  planCheckPageSize,
				LimitSet:               true,
			})
			return len(ids), err
		}}
	}
	return paths, emailPath, nil
}

// planCheckTimeAll times every path (the email path right after the desk).
func planCheckTimeAll(ctx context.Context, tx *store.UserTx, paths []planPath, emailPath *planPath, runs int) ([]planPathResult, error) {
	var out []planPathResult
	for _, p := range paths {
		r, err := planCheckPath(ctx, tx, p, runs)
		if err != nil {
			return nil, err
		}
		out = append(out, r)
		if p.name == planDeskPath && emailPath != nil {
			er, err := planCheckPath(ctx, tx, *emailPath, runs)
			if err != nil {
				return nil, err
			}
			out = append(out, er)
		}
	}
	if emailPath == nil {
		out = append(out, planPathResult{Name: planEmailPath,
			Skipped: "no area holds any mail on the desk, so there is no area view to page"})
	}
	// No restore to auto here, on purpose: the modes are transaction-local, so the
	// rollback is what puts them back, and an explicit reset would hide a
	// session-level leak from the integration test that guards exactly that.
	return out, nil
}

// planCheckPath times one path under each forced mode.
func planCheckPath(ctx context.Context, tx *store.UserTx, p planPath, runs int) (planPathResult, error) {
	r := planPathResult{Name: p.name, Source: p.source}
	for _, m := range []struct {
		mode store.PlanCacheMode
		into *[]time.Duration
	}{
		{store.PlanCacheForceGeneric, &r.Generic},
		{store.PlanCacheForceCustom, &r.Custom},
	} {
		if err := tx.SetPlanCacheMode(ctx, m.mode); err != nil {
			return r, err
		}
		for i := 0; i < runs; i++ {
			start := time.Now()
			n, err := p.run(ctx, tx)
			el := time.Since(start)
			if err != nil {
				return r, fmt.Errorf("%s (%s): %w", p.name, m.mode, err)
			}
			*m.into = append(*m.into, el)
			r.Rows = n
		}
	}
	return r, nil
}

// medianDuration returns the median of ds (the mean of the middle two for an
// even count), or 0 for none. It does not modify ds.
func medianDuration(ds []time.Duration) time.Duration {
	if len(ds) == 0 {
		return 0
	}
	s := append([]time.Duration(nil), ds...)
	sort.Slice(s, func(i, j int) bool { return s[i] < s[j] })
	if len(s)%2 == 1 {
		return s[len(s)/2]
	}
	return (s[len(s)/2-1] + s[len(s)/2]) / 2
}

// planVerdict applies the rule: the generic plan is a candidate for
// planForValues when its median is at least planCheckMinRatio times the custom
// median and at least planCheckMinGap slower.
func planVerdict(generic, custom time.Duration) (candidate bool, ratio float64) {
	if custom > 0 {
		ratio = float64(generic) / float64(custom)
	}
	candidate = generic-custom >= planCheckMinGap && float64(generic) >= planCheckMinRatio*float64(custom)
	return candidate, ratio
}

func planMS(d time.Duration) string { return fmt.Sprintf("%.1f", float64(d)/float64(time.Millisecond)) }

func printPlanCheck(w io.Writer, results []planPathResult, runs int) {
	p := func(f string, a ...any) { _, _ = fmt.Fprintf(w, f, a...) }
	p("=== plan-check: generic vs custom plans on the hot read paths (%d runs per mode) ===\n\n", runs)
	p("%-42s %12s %12s %7s %6s\n", "PATH", "GENERIC MED", "CUSTOM MED", "RATIO", "ROWS")
	for _, r := range results {
		if r.Skipped != "" {
			p("%-42s %s\n", r.Name, "not exercised: "+r.Skipped)
			continue
		}
		g, c := medianDuration(r.Generic), medianDuration(r.Custom)
		_, ratio := planVerdict(g, c)
		p("%-42s %9s ms %9s ms %6.1fx %6d\n", r.Name, planMS(g), planMS(c), ratio, r.Rows)
	}
	p("\nFirst run of each mode (cold; the first generic run also pays for pgx preparing the statement):\n")
	for _, r := range results {
		if r.Skipped != "" {
			continue
		}
		p("  %-42s generic %s ms   custom %s ms\n", r.Name, planMS(r.Generic[0]), planMS(r.Custom[0]))
	}
	p("\nVerdicts (candidate = generic median >= %.0fx custom and >= %s slower):\n", planCheckMinRatio, planCheckMinGap)
	for _, r := range results {
		if r.Skipped != "" {
			p("  %-42s not exercised\n", r.Name)
			continue
		}
		g, c := medianDuration(r.Generic), medianDuration(r.Custom)
		if planSelfPlanned[r.Name] {
			p("  %-42s plans its own statements (custom), so the modes do not apply\n", r.Name)
		} else if cand, ratio := planVerdict(g, c); cand {
			p("  %-42s generic plan is slower by %.1fx - candidate for planForValues\n", r.Name, ratio)
		} else {
			p("  %-42s no plan-mode effect\n", r.Name)
		}
	}
	p("\nWhere each path reads a message's area (stored = the per-message stored area, resolver = worked out in the query, none = no area involved):\n")
	for _, r := range results {
		if r.Skipped != "" {
			p("  %-42s not exercised\n", r.Name)
			continue
		}
		p("  %-42s %s\n", r.Name, r.Source)
	}
	p("\nREAD ONLY: nothing was written (rolled back).\n")
}

const (
	// planExplainStatementTimeout bounds each EXPLAIN ANALYZE, which executes
	// the statement under the custom plan.
	planExplainStatementTimeout = 60 * time.Second
	planExplainSQLHeader        = 160
)

// planExplainStatement is one captured statement with its plan.
type planExplainStatement struct {
	Pos  int // 1-based position among the path's captured statements
	SQL  string
	Plan string
	Exec time.Duration // "Execution Time" from the plan; 0 when absent
}

// planExplainPath is one path's captured statements with plans.
type planExplainPath struct {
	Name       string
	Source     string
	Skipped    string
	Statements []planExplainStatement
}

// planExecTime parses the "Execution Time: 12.345 ms" line of a plan.
func planExecTime(plan string) time.Duration {
	for _, line := range strings.Split(plan, "\n") {
		line = strings.TrimSpace(line)
		if v, ok := strings.CutPrefix(line, "Execution Time:"); ok {
			var ms float64
			if _, err := fmt.Sscanf(strings.TrimSpace(v), "%f", &ms); err == nil {
				return time.Duration(ms * float64(time.Millisecond))
			}
		}
	}
	return 0
}

// planExplain runs each hot path once under a custom plan, capturing the
// statements the store sends, then EXPLAINs each captured SELECT/WITH with its
// captured arguments in the same read-only transaction (rolled back).
func planExplain(ctx context.Context, w io.Writer, st *store.Store, rec *store.StatementRecorder, userID ulid.ULID, pathSel string) error {
	var results []planExplainPath
	if err := st.UserReadTx(ctx, userID, func(tx *store.UserTx) error {
		if err := tx.SetStatementTimeout(ctx, planExplainStatementTimeout); err != nil {
			return err
		}
		paths, emailPath, err := planCheckPaths(ctx, tx)
		if err != nil {
			return err
		}
		if emailPath != nil {
			paths = append(paths, *emailPath)
		} else if pathSel == "" || strings.HasPrefix(planEmailPath, pathSel) {
			results = append(results, planExplainPath{Name: planEmailPath,
				Skipped: "no area holds any mail on the desk, so there is no area view to page"})
		}
		if err := tx.SetPlanCacheMode(ctx, store.PlanCacheForceCustom); err != nil {
			return err
		}
		for _, p := range paths {
			if pathSel != "" && !strings.HasPrefix(p.name, pathSel) {
				continue
			}
			rec.Start()
			_, runErr := p.run(ctx, tx)
			captured := rec.Stop()
			if runErr != nil {
				return fmt.Errorf("%s: %w", p.name, runErr)
			}
			r := planExplainPath{Name: p.name, Source: p.source}
			pos := 0
			for _, c := range captured {
				if !strings.HasPrefix(strings.ToUpper(strings.TrimSpace(c.SQL)), "SELECT") &&
					!strings.HasPrefix(strings.ToUpper(strings.TrimSpace(c.SQL)), "WITH") {
					continue
				}
				if strings.Contains(c.SQL, "set_config(") || strings.Contains(c.SQL, "current_setting(") {
					continue
				}
				pos++
				plan, err := tx.ExplainAnalyze(ctx, c)
				if err != nil {
					return fmt.Errorf("%s statement %d: %w", p.name, pos, err)
				}
				r.Statements = append(r.Statements, planExplainStatement{Pos: pos, SQL: c.SQL, Plan: plan, Exec: planExecTime(plan)})
			}
			sort.SliceStable(r.Statements, func(i, j int) bool { return r.Statements[i].Exec > r.Statements[j].Exec })
			results = append(results, r)
		}
		return nil
	}); err != nil {
		return fmt.Errorf("plan-check --explain: %w (a user whose desk was never opened has no areas or desk preferences yet, and this command cannot create them)", err)
	}
	if pathSel != "" && len(results) == 0 {
		return fmt.Errorf("plan-check --explain: no path name starts with %q", pathSel)
	}
	printPlanExplain(w, results)
	return nil
}

func printPlanExplain(w io.Writer, results []planExplainPath) {
	p := func(f string, a ...any) { _, _ = fmt.Fprintf(w, f, a...) }
	p("Plans may include values from your own mail (addresses, label ids). Do not paste them into public places.\n\n")
	p("=== plan-check --explain: real plans of the statements each hot path sends (custom plan mode, one run) ===\n")
	for _, r := range results {
		p("\n##### %s\n", r.Name)
		if r.Source != "" {
			p("area source: %s\n", r.Source)
		}
		if r.Skipped != "" {
			p("not exercised: %s\n", r.Skipped)
			continue
		}
		if len(r.Statements) == 0 {
			p("no SELECT statements were sent\n")
			continue
		}
		for _, s := range r.Statements {
			sql := strings.Join(strings.Fields(s.SQL), " ")
			if len(sql) > planExplainSQLHeader {
				sql = sql[:planExplainSQLHeader]
			}
			p("\n--- statement %d of the path, execution %s ms: %s\n", s.Pos, planMS(s.Exec), sql)
			p("%s", s.Plan)
		}
	}
	p("\nREAD ONLY: nothing was written (rolled back).\n")
}
