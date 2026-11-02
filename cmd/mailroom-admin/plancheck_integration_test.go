// SPDX-License-Identifier: AGPL-3.0-only

//go:build integration

package main

import (
	"bytes"
	"context"
	"os"
	"strings"
	"testing"

	"github.com/jackc/pgx/v5/pgxpool"

	"mailroom/internal/store"
)

// oneConnStore is a Store over a pool of exactly ONE connection, so the
// transaction after a plan-check is guaranteed to run on the very connection
// plan-check used. With a larger pool a leaked session setting could sit on an
// idle connection the next transaction never picks, and the test would pass
// against the bug.
func oneConnStore(t *testing.T) *store.Store {
	t.Helper()
	cfg, err := pgxpool.ParseConfig(os.Getenv("MAILROOM_TEST_APP_URL"))
	if err != nil {
		t.Fatal(err)
	}
	cfg.MaxConns = 1
	pool, err := pgxpool.NewWithConfig(context.Background(), cfg)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(pool.Close)
	return store.NewFromPool(pool)
}

// plan-check runs the real store calls under both forced plan modes and writes
// nothing. Proved against a real database: every path reports both modes, the
// transaction left the data and the change stream untouched, and the forced
// mode did not leak onto the pooled connection.
func TestPlanCheckRunsEveryPathAndWritesNothing(t *testing.T) {
	h := newUntangleHarness(t)
	ctx := context.Background()
	userID := seedUntangleUser(t, h.super, "plancheck-"+store.NewULID().String())
	// plan-check cannot materialise the default areas and desk preferences (it
	// is read-only), so open the desk once, as a client would.
	if err := h.store.UserTx(ctx, userID, func(tx *store.UserTx) error {
		if _, err := tx.ListAreas(ctx); err != nil {
			return err
		}
		_, err := tx.GetDeskPreferences(ctx)
		return err
	}); err != nil {
		t.Fatalf("open desk: %v", err)
	}
	for _, subj := range []string{"a", "b", "c"} {
		seedUntangleMessage(t, h.super, userID, subj)
	}

	snapshot := func() (changes, messages int) {
		t.Helper()
		if err := h.super.QueryRow(ctx,
			`SELECT (SELECT count(*) FROM app.change_log WHERE user_id = $1),
			        (SELECT count(*) FROM app.messages WHERE user_id = $1)`,
			userID.String()).Scan(&changes, &messages); err != nil {
			t.Fatalf("snapshot: %v", err)
		}
		return
	}
	changesBefore, messagesBefore := snapshot()

	var out bytes.Buffer
	one := oneConnStore(t)
	if err := planCheck(ctx, &out, one, userID, 2); err != nil {
		t.Fatalf("planCheck: %v", err)
	}
	report := out.String()
	for _, path := range []string{
		"Area/getSummary: AreaSummary",
		"Area/getSummary: DrawerArchiveCounts",
		"Area/getDesk: DeskSnapshot",
		"Mailbox/get: all mailboxes",
	} {
		if strings.Count(report, path) < 3 { // table, cold line, verdict
			t.Errorf("path %q is not reported in the table, the cold lines and the verdicts:\n%s", path, report)
		}
	}
	// The seeded messages sit in no mailbox, so no area holds desk mail: the
	// Email/query path must say it was not exercised rather than vanish.
	if !strings.Contains(report, "Email/query: largest area, first page") {
		t.Errorf("Email/query path missing from the report:\n%s", report)
	}
	if !strings.HasSuffix(report, "READ ONLY: nothing was written (rolled back).\n") {
		t.Errorf("report does not end with the read-only line:\n%s", report)
	}

	if c, m := snapshot(); c != changesBefore || m != messagesBefore {
		t.Errorf("plan-check wrote: change_log %d -> %d, messages %d -> %d", changesBefore, c, messagesBefore, m)
	}

	// THE MODE DOES NOT LEAK. The forced mode is SET LOCAL, so a later
	// transaction on the pooled connection sees the default. The pool has one
	// connection, so that IS the connection plan-check ran on.
	for i := 0; i < 2; i++ {
		var mode string
		if err := one.UserTx(ctx, userID, func(tx *store.UserTx) error {
			var e error
			mode, e = tx.PlanCacheModeSetting(ctx)
			return e
		}); err != nil {
			t.Fatal(err)
		}
		if mode != "auto" {
			t.Fatalf("plan_cache_mode leaked past plan-check: %q, want auto", mode)
		}
	}
}

// A user who never opened the desk makes plan-check fail loudly (the read-only
// transaction refuses the materialising write), not report an empty success.
func TestPlanCheckRefusesAnUnopenedDesk(t *testing.T) {
	h := newUntangleHarness(t)
	userID := seedUntangleUser(t, h.super, "plancheckfresh-"+store.NewULID().String())
	var out bytes.Buffer
	if err := planCheck(context.Background(), &out, h.store, userID, 1); err == nil {
		t.Fatalf("expected an error for a user with no areas yet; output:\n%s", out.String())
	}
}

// A forced mode is visible inside the transaction, gone after it COMMITS (the
// case where only SET LOCAL semantics protect the pool: a plain SET in a
// committed transaction would stay on the connection), and anything outside
// the closed set is refused. One-connection pool, so "the next transaction" is
// on the same connection.
func TestSetPlanCacheModeIsTransactionLocalAndClosed(t *testing.T) {
	h := newUntangleHarness(t)
	ctx := context.Background()
	userID := seedUntangleUser(t, h.super, "plancheckmode-"+store.NewULID().String())
	one := oneConnStore(t)
	if err := one.UserTx(ctx, userID, func(tx *store.UserTx) error {
		if err := tx.SetPlanCacheMode(ctx, store.PlanCacheForceGeneric); err != nil {
			return err
		}
		if got, _ := tx.PlanCacheModeSetting(ctx); got != "force_generic_plan" {
			t.Errorf("inside the tx plan_cache_mode = %q, want force_generic_plan", got)
		}
		if err := tx.SetPlanCacheMode(ctx, store.PlanCacheMode("force_generic_plan; DROP TABLE x")); err == nil {
			t.Error("a mode outside the closed set was accepted")
		}
		return nil // commit
	}); err != nil {
		t.Fatal(err)
	}
	if err := one.UserTx(ctx, userID, func(tx *store.UserTx) error {
		if got, _ := tx.PlanCacheModeSetting(ctx); got != "auto" {
			t.Errorf("plan_cache_mode leaked past a committed transaction: %q, want auto", got)
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
}

// --explain captures the statements each path really sends and prints a real
// plan for them, and writes nothing.
func TestPlanCheckExplainPrintsPlansAndWritesNothing(t *testing.T) {
	h := newUntangleHarness(t)
	ctx := context.Background()
	userID := seedUntangleUser(t, h.super, "planexplain-"+store.NewULID().String())
	if err := h.store.UserTx(ctx, userID, func(tx *store.UserTx) error {
		if _, err := tx.ListAreas(ctx); err != nil {
			return err
		}
		_, err := tx.GetDeskPreferences(ctx)
		return err
	}); err != nil {
		t.Fatalf("open desk: %v", err)
	}
	for _, subj := range []string{"a", "b", "c"} {
		seedUntangleMessage(t, h.super, userID, subj)
	}
	snapshot := func() (changes, messages int) {
		t.Helper()
		if err := h.super.QueryRow(ctx,
			`SELECT (SELECT count(*) FROM app.change_log WHERE user_id = $1),
			        (SELECT count(*) FROM app.messages WHERE user_id = $1)`,
			userID.String()).Scan(&changes, &messages); err != nil {
			t.Fatalf("snapshot: %v", err)
		}
		return
	}
	changesBefore, messagesBefore := snapshot()

	st, rec, err := store.OpenRecording(ctx, os.Getenv("MAILROOM_TEST_APP_URL"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(st.Close)
	var out bytes.Buffer
	if err := planExplain(ctx, &out, st, rec, userID, ""); err != nil {
		t.Fatalf("planExplain: %v", err)
	}
	report := out.String()
	if !strings.HasPrefix(report, "Plans may include values from your own mail") {
		t.Errorf("privacy notice missing at the top:\n%s", report)
	}
	for _, path := range []string{"Area/getSummary: AreaSummary", "Area/getDesk: DeskSnapshot", "Mailbox/get: all mailboxes"} {
		i := strings.Index(report, "##### "+path)
		if i < 0 {
			t.Fatalf("path %q missing:\n%s", path, report)
		}
		rest := report[i+1:]
		if j := strings.Index(rest, "#####"); j >= 0 {
			rest = rest[:j]
		}
		if !strings.Contains(rest, "actual time=") {
			t.Errorf("path %q printed no plan with actual time=:\n%s", path, rest)
		}
	}
	if !strings.HasSuffix(report, "READ ONLY: nothing was written (rolled back).\n") {
		t.Errorf("report does not end with the read-only line:\n%s", report)
	}
	if c, m := snapshot(); c != changesBefore || m != messagesBefore {
		t.Errorf("--explain wrote: change_log %d -> %d, messages %d -> %d", changesBefore, c, messagesBefore, m)
	}

	// --path limits to one path by prefix.
	out.Reset()
	if err := planExplain(ctx, &out, st, rec, userID, "Mailbox/get"); err != nil {
		t.Fatal(err)
	}
	if strings.Contains(out.String(), "##### Area/") || !strings.Contains(out.String(), "##### Mailbox/get") {
		t.Errorf("--path Mailbox/get did not limit the output:\n%s", out.String())
	}
}
