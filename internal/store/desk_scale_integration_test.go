// SPDX-License-Identifier: AGPL-3.0-only

//go:build integration

package store

// THE DESK AT SCALE (#660).
//
// After a folder consolidation the production Inbox held tens of thousands of
// messages, and the desk took tens of seconds to load on a phone: each
// thread-collapsed desk Email/query took 3-10 s and Area/getSummary up to 11 s.
// Those statements were built from per-message CORRELATED subqueries —
// deskLiveSQL's CASE/EXISTS pair, the area resolver's two LIMIT-1 lookups, a
// scalar triage-score lookup per side test, the trash exclusion's EXISTS — and
// in every one of them the user was the outer message's COLUMN, so the RLS
// policy (a current_setting call cast through the app.ulid domain's regex
// CHECK) could not fold into a one-time filter and ran per row. Measured on
// a large mailbox against the same rows: the #652 desk predicate took seconds to
// count what a set-based semi-join counted in a fraction of that.
//
// This test seeds one user with a realistically shaped mailbox of deskScaleN
// messages and holds the desk statements to three things:
//
//  1. STRUCTURE (the assertion that does not depend on the runner's speed):
//     EXPLAIN ANALYZE of each statement shows no per-message work — no SubPlan
//     that ran more than once, and no scan that evaluates the RLS policy per
//     row (perRowWork). Timing alone would be a weak gate: at this size the
//     #652 predicate is still well inside any sane bound on a fast machine.
//  2. TIME, with a generous bound (deskScaleBound), as a backstop that the
//     structure is also what makes it fast.
//  3. AGREEMENT: the summary's per-area counts equal what Email/query lists
//     for each area, they add up to the desk count, and onDesk:true plus
//     onDesk:false is the whole mailbox — so "faster" cannot have been bought
//     by answering a different question.
//
// Every shape the desk predicate and the area resolver distinguish is present
// in bulk, so a plan that is only fast because a branch is empty cannot pass:
// live INBOX provenance, moved to Archive with INBOX provenance still live,
// moved to INBOX from Archive, desk-archived, a feeds_desk folder, a plain
// folder, Trash, a second account; area labels, a second label feeding an
// area, a label mapped to a tab, classifier verdicts and none; scores and none;
// user placements; snoozes; flags; threads of three.

import (
	"context"
	"crypto/sha256"
	"encoding/json"
	"fmt"
	"os"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/oklog/ulid/v2"
)

const (
	deskScaleN     = 20000
	deskScaleBound = 1500 * time.Millisecond
)

// sqlRecorder is a pgx QueryTracer that keeps every statement the store sends,
// with its arguments, so the test can EXPLAIN exactly what QueryEmails and
// AreaSummary ran rather than a hand-copy of it that could drift.
type sqlRecorder struct {
	stmts []recordedSQL
}

type recordedSQL struct {
	sql  string
	args []any
}

func (r *sqlRecorder) TraceQueryStart(ctx context.Context, _ *pgx.Conn, d pgx.TraceQueryStartData) context.Context {
	r.stmts = append(r.stmts, recordedSQL{sql: d.SQL, args: d.Args})
	return ctx
}

func (r *sqlRecorder) TraceQueryEnd(context.Context, *pgx.Conn, pgx.TraceQueryEndData) {}

func (r *sqlRecorder) find(t *testing.T, marker string) recordedSQL {
	t.Helper()
	for i := len(r.stmts) - 1; i >= 0; i-- {
		if strings.Contains(r.stmts[i].sql, marker) {
			return r.stmts[i]
		}
	}
	t.Fatalf("no recorded statement contains %q", marker)
	return recordedSQL{}
}

type deskScale struct {
	h        *harness
	st       *Store
	rec      *sqlRecorder
	userID   ulid.ULID
	accountA ulid.ULID
	areaIDs  map[string]ulid.ULID
}

func seedDeskScale(t *testing.T, n int) *deskScale {
	t.Helper()
	h := newHarness(t)
	ctx := context.Background()

	cfg, err := pgxpool.ParseConfig(os.Getenv("MAILROOM_TEST_APP_URL"))
	if err != nil {
		t.Fatalf("parse app url: %v", err)
	}
	rec := &sqlRecorder{}
	cfg.ConnConfig.Tracer = rec
	cfg.MaxConns = 1 // the recorder is not concurrency-safe; one conn keeps it honest
	pool, err := pgxpool.NewWithConfig(ctx, cfg)
	if err != nil {
		t.Fatalf("open traced pool: %v", err)
	}
	t.Cleanup(pool.Close)

	d := &deskScale{h: h, st: NewFromPool(pool), rec: rec, userID: NewULID(), accountA: NewULID()}
	u := d.userID.String()
	exec := func(sql string, args ...any) {
		t.Helper()
		if _, err := h.super.Exec(ctx, sql, args...); err != nil {
			line := strings.SplitN(strings.TrimSpace(sql), "\n", 2)[0]
			t.Fatalf("seed: %s: %v", line, err)
		}
	}

	exec(`INSERT INTO app.users (id, oidc_subject, email) VALUES ($1, $2, $3)`,
		u, "sub-"+u, strings.ToLower(u)+"@example.com")
	accountB := NewULID()
	for _, a := range []ulid.ULID{d.accountA, accountB} {
		exec(`INSERT INTO app.accounts (id, user_id, name, imap_host, smtp_host, credentials_ct, credentials_dek)
		      VALUES ($1, $2, $3, 'mail.example.com', 'smtp.example.com', '\x00', $4)`,
			a.String(), u, "acct-"+a.String(), NewULID().String())
	}
	inbox, archive, deskFolder, plain, trash, inboxB := NewULID(), NewULID(), NewULID(), NewULID(), NewULID(), NewULID()
	for _, m := range []struct {
		id, acct         ulid.ULID
		name, specialUse string
		feedsDesk        bool
	}{
		{inbox, d.accountA, "INBOX", "", false},
		{archive, d.accountA, "Archive", `\Archive`, false},
		{deskFolder, d.accountA, "Follow-up", "", true},
		{plain, d.accountA, "Receipts", "", false},
		{trash, d.accountA, "Trash", `\Trash`, false},
		{inboxB, accountB, "INBOX", "", false},
	} {
		exec(`INSERT INTO app.mailboxes (id, user_id, account_id, imap_name, display_name, special_use, feeds_desk)
		      VALUES ($1, $2, $3, $4, $4, NULLIF($5, ''), $6)`,
			m.id.String(), u, m.acct.String(), m.name, m.specialUse, m.feedsDesk)
	}

	// The default area plan: one area per tab, each fed by its tab and its own
	// label — exactly what a new login gets.
	var views []AreaView
	if err := d.st.UserTx(ctx, d.userID, func(tx *UserTx) error {
		var e error
		views, e = tx.ListAreas(ctx)
		return e
	}); err != nil {
		t.Fatalf("materialise areas: %v", err)
	}
	if len(views) < 4 {
		t.Fatalf("default plan has %d areas, want at least 4", len(views))
	}
	d.areaIDs = map[string]ulid.ULID{}
	var areaLabels []string
	for _, v := range views {
		d.areaIDs[v.Name] = v.ID
		areaLabels = append(areaLabels, v.LabelID.String())
	}
	// A second label feeding the LAST area (so it contests with the areas'
	// own labels on position), and a label mapped to a tab that feeds no area
	// directly — the mapped-tab arm of the resolver.
	deals, mapped := NewULID(), NewULID()
	exec(`INSERT INTO app.labels (id, user_id, name) VALUES ($1, $2, 'Deals')`, deals.String(), u)
	exec(`INSERT INTO app.labels (id, user_id, name, tab) VALUES ($1, $2, 'Newsletters', 'updates')`, mapped.String(), u)
	exec(`INSERT INTO app.area_feeds (id, user_id, area_id, label_id) VALUES ($1, $2, $3, $4)`,
		NewULID().String(), u, views[len(views)-1].ID.String(), deals.String())

	tabNames := []string{"primary", "social", "promotions", "updates"}
	now := time.Now().UTC()
	var (
		ids, threads, blobHashes            []string
		hashes                              [][]byte
		received                            []time.Time
		classified, lineSide, moved         []*string
		deskArchived, snoozed               []*time.Time
		flagged                             []bool
		provID, provMsg, provAcct, provMbox []string
		provUID                             []int64
		lblMsg, lblLabel                    []string
		scoreMsg                            []string
		scores                              []float32
	)
	str := func(s string) *string { return &s }
	uid := int64(0)
	var thread string
	for i := 0; i < n; i++ {
		id := NewULID().String()
		if i%3 == 0 {
			thread = NewULID().String()
		}
		h := sha256.Sum256([]byte(u + strconv.Itoa(i)))
		ids = append(ids, id)
		threads = append(threads, thread)
		hashes = append(hashes, h[:])
		blobHashes = append(blobHashes, NewULID().String())
		received = append(received, now.Add(-time.Duration(i)*3*time.Minute))

		var ct *string
		if i%5 != 4 {
			ct = str(tabNames[i%4])
		}
		classified = append(classified, ct)
		var ls *string
		switch i % 50 {
		case 1:
			ls = str("surface")
		case 2:
			ls = str("drawer")
		}
		lineSide = append(lineSide, ls)
		var snz *time.Time
		if i%200 == 0 {
			at := now.Add(24 * time.Hour)
			snz = &at
		}
		snoozed = append(snoozed, snz)
		flagged = append(flagged, i%40 == 0)

		bucket := i % 100
		mbox, acct := inbox, d.accountA
		var mv *string
		var da *time.Time
		switch {
		case bucket < 70: // live INBOX
		case bucket < 75: // archived in Mailroom, INBOX provenance still live
			mv = str(archive.String())
		case bucket < 78: // restored to INBOX, bytes still in Archive
			mbox, mv = archive, str(inbox.String())
		case bucket < 83: // day-one archived
			at := now.Add(-time.Hour)
			da = &at
		case bucket < 88: // a folder that feeds the desk
			mbox = deskFolder
		case bucket < 94: // a plain folder
			mbox = plain
		case bucket < 96: // Trash
			mbox = trash
		default: // the second account's INBOX
			mbox, acct = inboxB, accountB
		}
		moved = append(moved, mv)
		deskArchived = append(deskArchived, da)
		uid++
		provID = append(provID, NewULID().String())
		provMsg = append(provMsg, id)
		provAcct = append(provAcct, acct.String())
		provMbox = append(provMbox, mbox.String())
		provUID = append(provUID, uid)

		if i%3 == 0 {
			lblMsg, lblLabel = append(lblMsg, id), append(lblLabel, areaLabels[(i/3)%len(areaLabels)])
		}
		if i%11 == 0 {
			lblMsg, lblLabel = append(lblMsg, id), append(lblLabel, deals.String())
		}
		if i%7 == 0 {
			lblMsg, lblLabel = append(lblMsg, id), append(lblLabel, mapped.String())
		}
		if i%5 != 0 {
			scoreMsg = append(scoreMsg, id)
			scores = append(scores, float32(i%10)/10)
		}
	}

	exec(`INSERT INTO app.blobs (user_id, content_hash, size_plain, size_stored, dek_id)
	      SELECT $1, h, 10, 20, k FROM unnest($2::bytea[], $3::text[]) AS x(h, k)`, u, hashes, blobHashes)
	exec(`INSERT INTO app.threads (id, user_id, subject)
	      SELECT DISTINCT t, $1, 'thread' FROM unnest($2::text[]) AS x(t)`, u, threads)
	exec(`INSERT INTO app.messages (id, user_id, content_hash, thread_id, subject, received_at, size_bytes,
	                                classified_tab, line_side, moved_mailbox_id, desk_archived_at,
	                                snoozed_until, is_flagged)
	      SELECT id, $1, h, th, 'subject', r, 10, ct::app.category_tab, ls, mv, da, sz, fl
	        FROM unnest($2::text[], $3::bytea[], $4::text[], $5::timestamptz[], $6::text[], $7::text[],
	                    $8::text[], $9::timestamptz[], $10::timestamptz[], $11::bool[])
	             AS x(id, h, th, r, ct, ls, mv, da, sz, fl)`,
		u, ids, hashes, threads, received, classified, lineSide, moved, deskArchived, snoozed, flagged)
	exec(`INSERT INTO app.message_provenance (id, user_id, message_id, account_id, mailbox_id, uid, uidvalidity, state)
	      SELECT id, $1, m, a, mb, uid, 1, 'live'
	        FROM unnest($2::text[], $3::text[], $4::text[], $5::text[], $6::bigint[]) AS x(id, m, a, mb, uid)`,
		u, provID, provMsg, provAcct, provMbox, provUID)
	exec(`INSERT INTO app.message_labels (user_id, message_id, label_id, applied_by)
	      SELECT $1, m, l, 'user' FROM unnest($2::text[], $3::text[]) AS x(m, l)`, u, lblMsg, lblLabel)
	exec(`INSERT INTO app.triage_scores (user_id, message_id, score, model_version)
	      SELECT $1, m, s, 'scale-test' FROM unnest($2::text[], $3::real[]) AS x(m, s)`, u, scoreMsg, scores)
	// Production has statistics (autovacuum); a freshly seeded test table does
	// not, and the planner would be judged on guesses nobody runs with.
	for _, tbl := range []string{"messages", "message_provenance", "message_labels", "labels",
		"mailboxes", "areas", "area_feeds", "triage_scores", "threads"} {
		exec(`ANALYZE app.` + tbl)
	}
	return d
}

// explainNode is the subset of EXPLAIN (FORMAT JSON) this test reads.
type explainNode struct {
	NodeType     string        `json:"Node Type"`
	Relationship string        `json:"Parent Relationship"`
	SubplanName  string        `json:"Subplan Name"`
	Relation     string        `json:"Relation Name"`
	Filter       string        `json:"Filter"`
	JoinFilter   string        `json:"Join Filter"`
	Loops        float64       `json:"Actual Loops"`
	Rows         float64       `json:"Actual Rows"`
	Plans        []explainNode `json:"Plans"`
}

// perRowWork returns every place in the plan that does per-message work of
// the two kinds #660 was made of, each read off EXPLAIN ANALYZE rather than
// inferred from a timing:
//
//  1. A SubPlan that EXECUTED MORE THAN ONCE — a correlated subquery,
//     evaluated per outer row. An InitPlan (runs once) and a hashed SubPlan
//     (built once, probed by hash) do not count.
//  2. THE RLS POLICY EVALUATED PER ROW: `app.current_user_id()` (inlined as
//     current_setting('mailroom.user_id') cast through the app.ulid domain,
//     whose CHECK is a regex) in a node's per-row Filter or Join Filter. That
//     happens when a subquery's user is a COLUMN rather than a constant, so
//     the planner cannot fold the policy into a one-time filter; it is what
//     made the hashed form of the #652 predicate slow even though it ran once
//     (measured here: it was an order of magnitude slower to scan 20k provenance rows that way
//     than with the user bound). With the user constant, the policy appears only as a
//     Result node's One-Time Filter, which is not a per-row field.
func perRowWork(t *testing.T, ctx context.Context, tx *UserTx, sql string, args []any) (bad []string, text string) {
	t.Helper()
	var raw []byte
	if err := tx.tx.QueryRow(ctx, "EXPLAIN (ANALYZE, FORMAT JSON) "+sql, args...).Scan(&raw); err != nil {
		t.Fatalf("explain: %v\n%s", err, sql)
	}
	var doc []struct {
		Plan explainNode `json:"Plan"`
	}
	if err := json.Unmarshal(raw, &doc); err != nil || len(doc) != 1 {
		t.Fatalf("parse explain: %v", err)
	}
	var walk func(n explainNode)
	walk = func(n explainNode) {
		if n.Relationship == "SubPlan" && n.Loops > 1 {
			bad = append(bad, fmt.Sprintf("%s (%s) ran %.0f times", n.SubplanName, n.NodeType, n.Loops))
		}
		for _, f := range []string{n.Filter, n.JoinFilter} {
			if strings.Contains(f, "current_setting") {
				bad = append(bad, fmt.Sprintf("%s on %q evaluates the RLS policy per row (%.0f rows x %.0f loops)",
					n.NodeType, n.Relation, n.Rows, n.Loops))
			}
		}
		for _, c := range n.Plans {
			walk(c)
		}
	}
	walk(doc[0].Plan)

	rows, err := tx.tx.Query(ctx, "EXPLAIN (ANALYZE, COSTS OFF, TIMING OFF, SUMMARY OFF) "+sql, args...)
	if err != nil {
		t.Fatalf("explain text: %v", err)
	}
	defer rows.Close()
	var b strings.Builder
	for rows.Next() {
		var line string
		if err := rows.Scan(&line); err != nil {
			t.Fatalf("explain text scan: %v", err)
		}
		b.WriteString(line + "\n")
	}
	return bad, b.String()
}

func timed(t *testing.T, name string, fn func() error) time.Duration {
	t.Helper()
	best := time.Duration(1<<63 - 1)
	for i := 0; i < 3; i++ { // best of three: the first run also warms the cache
		start := time.Now()
		if err := fn(); err != nil {
			t.Fatalf("%s: %v", name, err)
		}
		if d := time.Since(start); d < best {
			best = d
		}
	}
	t.Logf("%-40s %6d ms", name, best.Milliseconds())
	return best
}

func TestDeskQueriesStaySetBasedAtScale(t *testing.T) {
	d := seedDeskScale(t, deskScaleN)
	ctx := context.Background()
	threshold := float32(0.5)
	yes, no := true, false
	area := d.areaIDs["Promotions"]
	if area == (ulid.ULID{}) {
		t.Fatalf("no Promotions area in %v", d.areaIDs)
	}

	type measured struct {
		name string
		took time.Duration
		stmt recordedSQL
	}
	var checks []measured

	// 1. The bare desk predicate, counted — what every desk statement filters by.
	countArgs := []any{d.userID.String()}
	countSQL := `SELECT count(*) FROM app.messages m WHERE m.user_id = $1 AND ` +
		deskMessageSQL(bindFunc(&countArgs), d.userID, nil)
	var deskCount int
	took := timed(t, "desk predicate count", func() error {
		return d.st.UserTx(ctx, d.userID, func(tx *UserTx) error {
			return tx.tx.QueryRow(ctx, countSQL, countArgs...).Scan(&deskCount)
		})
	})
	checks = append(checks, measured{"desk predicate count", took, recordedSQL{countSQL, countArgs}})

	// 2. The desk's own Email/query, shaped as the client sends it: one area,
	// on the desk, above the line, not snoozed, thread-collapsed, newest first.
	// Its time is the whole call — one statement for the page and the total.
	var deskTotal int
	took = timed(t, "desk Email/query (one pass)", func() error {
		return d.st.UserTx(ctx, d.userID, func(tx *UserTx) error {
			var e error
			_, deskTotal, e = tx.QueryEmails(ctx, EmailQuery{
				InArea: &area, OnDesk: &yes, OnSurface: &yes, LineThreshold: threshold,
				Snoozed: &no, CollapseThreads: true, SortByEffectiveArrival: true,
				Limit: 50, LimitSet: true,
			})
			return e
		})
	})
	// One statement since #665: the total rides on the page as count(*) OVER ().
	checks = append(checks,
		measured{"desk Email/query (one pass)", took, d.rec.find(t, "count(*) OVER () AS total FROM ranked")})

	// 3. Area/getSummary.
	var summary []AreaCounts
	took = timed(t, "Area/getSummary", func() error {
		return d.st.UserTx(ctx, d.userID, func(tx *UserTx) error {
			var e error
			summary, e = tx.AreaSummary(ctx, DeskScope{Threshold: threshold})
			return e
		})
	})
	checks = append(checks, measured{"Area/getSummary", took, d.rec.find(t, "WITH desk AS")})

	// 4. The drawer sweep's candidate scan — the same desk predicate and area
	// resolver, run by a background job rather than a phone, but over the same
	// rows.
	var candidates []DrawerCandidate
	took = timed(t, "drawer sweep candidates", func() error {
		return d.st.UserTx(ctx, d.userID, func(tx *UserTx) error {
			var e error
			candidates, e = tx.DrawerCandidates(ctx, time.Now(), 200)
			return e
		})
	})
	checks = append(checks, measured{"drawer sweep candidates", took, d.rec.find(t, "AS period_days")})

	t.Logf("desk count %d, desk query total %d, summary rows %d, drawer candidates %d",
		deskCount, deskTotal, len(summary), len(candidates))
	if deskCount == 0 || deskTotal == 0 || len(summary) == 0 || len(candidates) == 0 {
		t.Fatalf("a scale test over an empty result proves nothing: count=%d total=%d summary=%d candidates=%d",
			deskCount, deskTotal, len(summary), len(candidates))
	}

	for _, c := range checks {
		if c.took > deskScaleBound {
			t.Errorf("%s took %v, bound %v (#660)", c.name, c.took, deskScaleBound)
		}
		if err := d.st.UserTx(ctx, d.userID, func(tx *UserTx) error {
			bad, text := perRowWork(t, ctx, tx, c.stmt.sql, c.stmt.args)
			if len(bad) > 0 {
				t.Errorf("%s does per-message work (#660): %s\n%s", c.name, strings.Join(bad, "; "), text)
			} else if testing.Verbose() {
				t.Logf("%s plan:\n%s", c.name, text)
			}
			return nil
		}); err != nil {
			t.Fatal(err)
		}
	}

	// THE COUNTS AND THE LISTS STILL AGREE AT SCALE — the invariant every one
	// of these statements exists to keep (#155's lesson). Each area's summary
	// total must be exactly what Email/query lists for that area on the desk,
	// the areas plus Needs sorting must partition the desk count, and
	// onDesk:true and onDesk:false must partition the mailbox (a NULL in the
	// desk predicate would drop a message from both).
	query := func(q EmailQuery) int {
		t.Helper()
		q.Limit, q.LimitSet = 0, true
		var n int
		if err := d.st.UserTx(ctx, d.userID, func(tx *UserTx) error {
			var e error
			_, n, e = tx.QueryEmails(ctx, q)
			return e
		}); err != nil {
			t.Fatalf("query %+v: %v", q, err)
		}
		return n
	}
	summed := 0
	for _, row := range summary {
		n := row.Surface + row.Drawer + row.Unscored
		summed += n
		q := EmailQuery{OnDesk: &yes, Unsorted: row.AreaID == nil, InArea: row.AreaID}
		if got := query(q); got != n {
			t.Errorf("area %v: Area/getSummary counts %d, Email/query onDesk lists %d", row.AreaID, n, got)
		}
	}
	if summed != deskCount {
		t.Errorf("Area/getSummary totals %d, the desk predicate counts %d", summed, deskCount)
	}
	all, on, off := query(EmailQuery{}), query(EmailQuery{OnDesk: &yes}), query(EmailQuery{OnDesk: &no})
	if on != deskCount || on+off != all {
		t.Errorf("onDesk:true %d + onDesk:false %d, want %d (the desk count) + the rest of %d", on, off, deskCount, all)
	}
}
