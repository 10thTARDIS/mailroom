// SPDX-License-Identifier: AGPL-3.0-only

//go:build integration

package store

// The per-user census behind `mailroom-admin delete-user` (#54).
//
// The claim these tests exist to defend is narrow and load-bearing: the census
// is derived from pg_catalog, so a table added tomorrow is covered without
// anyone editing Go. A test that merely counted today's tables would pass
// equally well against a hardcoded list, which is the thing being avoided.

import (
	"context"
	"fmt"
	"testing"

	"github.com/oklog/ulid/v2"
)

// TestUserCensusIsDerivedFromTheCatalogNotAList is the point of the whole
// design, and it is asserted by CREATING A TABLE and requiring it to appear.
//
// ⚠ Counting the existing tables would not distinguish a catalog-driven census
// from a hardcoded list that happens to be up to date today. #316 is what an
// out-of-date list costs: one producer forgot one INSERT for the life of the
// project, and a row-driven sweep walked past every model a user owned while
// reporting success. For a delete, that is "deleted everything about me" quietly
// leaving data behind.
func TestUserCensusIsDerivedFromTheCatalogNotAList(t *testing.T) {
	h := newHarness(t)
	ctx := context.Background()
	a, _ := seedTwoUsers(t, h.super)
	s := h.appStore()
	aID := mustULID(t, a.ID)

	before, err := censusFor(ctx, s, aID)
	if err != nil {
		t.Fatalf("census: %v", err)
	}
	if len(before.Tables) < 20 {
		t.Fatalf("census found only %d tables with a user_id — that is a broken query, "+
			"not a small schema", len(before.Tables))
	}
	if hasTable(before, "the_43rd_table") {
		t.Fatal("the test table already exists")
	}

	// A new user-scoped table, exactly as a future migration would add one.
	// Statement by statement: pgx uses the extended protocol, which refuses
	// multiple commands in one Exec when a parameter is present.
	for _, stmt := range []string{
		`CREATE TABLE app.the_43rd_table (
		   user_id app.ulid NOT NULL REFERENCES app.users(id) ON DELETE CASCADE,
		   note    text NOT NULL,
		   PRIMARY KEY (user_id, note))`,
		`ALTER TABLE app.the_43rd_table ENABLE ROW LEVEL SECURITY`,
		`ALTER TABLE app.the_43rd_table FORCE ROW LEVEL SECURITY`,
		`CREATE POLICY t43 ON app.the_43rd_table FOR ALL TO mailroom_app
		   USING (user_id = app.current_user_id())
		   WITH CHECK (user_id = app.current_user_id())`,
		`GRANT SELECT, INSERT, UPDATE, DELETE ON app.the_43rd_table TO mailroom_app`,
	} {
		if _, err := h.super.Exec(ctx, stmt); err != nil {
			t.Fatalf("create the 43rd table: %v", err)
		}
	}
	if _, err := h.super.Exec(ctx,
		`INSERT INTO app.the_43rd_table (user_id, note) VALUES ($1, 'x'), ($1, 'y')`,
		a.ID); err != nil {
		t.Fatalf("seed the 43rd table: %v", err)
	}
	t.Cleanup(func() {
		_, _ = h.super.Exec(context.Background(), `DROP TABLE IF EXISTS app.the_43rd_table`)
	})

	after, err := censusFor(ctx, s, aID)
	if err != nil {
		t.Fatalf("census after: %v", err)
	}
	if !hasTable(after, "the_43rd_table") {
		t.Fatal("a newly created user-scoped table did NOT appear in the census — the " +
			"table set is not coming from the catalog, and a delete would walk past it")
	}
	if got := rowsFor(after, "the_43rd_table"); got != 2 {
		t.Errorf("the_43rd_table rows = %d, want 2", got)
	}
	if after.TotalRows != before.TotalRows+2 {
		t.Errorf("total rows went %d -> %d, want +2", before.TotalRows, after.TotalRows)
	}
}

// TestUserCensusIsScopedByRLSNotByAHandWrittenFilter: the counts must be the
// numbers RLS admits, so B's rows never appear in A's census. The census writes
// no WHERE clause of its own — this is what proves that is sufficient.
//
// ⚠ THE SEEDS ARE SYMMETRIC, WHICH IS EXACTLY WHY COMPARING THE TWO TOTALS
// CANNOT TEST THIS. Two equal numbers are equal whether or not each is scoped,
// and an earlier version of this test compared `ca.TotalRows` against
// `ca.TotalRows + cb.TotalRows` — an inequality that is arithmetically
// impossible once cb is known non-zero, so it could not have failed for any
// implementation. The measurement has to be made ASYMMETRIC on purpose: give B
// rows that A does not have, and require A's number NOT to move. An unscoped
// census reports the sum and goes red here; a scoped one is unchanged.
func TestUserCensusIsScopedByRLSNotByAHandWrittenFilter(t *testing.T) {
	h := newHarness(t)
	ctx := context.Background()
	a, b := seedTwoUsers(t, h.super)
	s := h.appStore()

	aID, bID := mustULID(t, a.ID), mustULID(t, b.ID)
	ca, err := censusFor(ctx, s, aID)
	if err != nil {
		t.Fatalf("census A: %v", err)
	}
	cb, err := censusFor(ctx, s, bID)
	if err != nil {
		t.Fatalf("census B: %v", err)
	}
	if ca.TotalRows == 0 || cb.TotalRows == 0 {
		t.Fatalf("a seeded user owns no rows (A=%d B=%d) — this test cannot observe "+
			"isolation against an empty database", ca.TotalRows, cb.TotalRows)
	}

	// Rows for B and B alone, in a table both users already appear in.
	const extra = 3
	for i := 0; i < extra; i++ {
		if _, err := h.super.Exec(ctx,
			`INSERT INTO app.compose_snippets (id, user_id, kind, name, body_text)
			 VALUES ($1, $2, 'template', $3, 'scope probe')`,
			NewULID().String(), b.ID, fmt.Sprintf("scope-probe-%d", i)); err != nil {
			t.Fatalf("seed B-only rows: %v", err)
		}
	}

	ca2, err := censusFor(ctx, s, aID)
	if err != nil {
		t.Fatalf("census A after: %v", err)
	}
	cb2, err := censusFor(ctx, s, bID)
	if err != nil {
		t.Fatalf("census B after: %v", err)
	}
	if cb2.TotalRows != cb.TotalRows+extra {
		t.Fatalf("B's census went %d -> %d after %d B-owned inserts, want +%d — the "+
			"probe rows are not being counted at all, so A's total below proves nothing",
			cb.TotalRows, cb2.TotalRows, extra, extra)
	}
	if ca2.TotalRows != ca.TotalRows {
		t.Errorf("A's census moved %d -> %d when only B gained rows — the counts are "+
			"not scoped to the calling user", ca.TotalRows, ca2.TotalRows)
	}
	if got, want := rowsFor(ca2, "compose_snippets"), rowsFor(ca, "compose_snippets"); got != want {
		t.Errorf("A's compose_snippets count moved %d -> %d on B's inserts", want, got)
	}

	var messages int64
	for _, tc := range ca.Tables {
		if tc.Table == "messages" {
			messages = tc.Rows
		}
	}
	if messages == 0 {
		t.Error("A's census reports no messages; the seed creates one")
	}
}

// TestTheDeleteRootIsFoundFromTheForeignKeysAndIsNotReportedAsUncovered.
//
// ⚠ THIS TEST EXISTS BECAUSE THE FIRST LIVE RUN PRINTED THE OPPOSITE OF THE
// TRUTH ABOUT THE MOST IMPORTANT TABLE. app.users has no `user_id` column, so a
// user_id-driven census put it under "tables with NO user_id: outside a
// user-scoped delete ... a new table here would hold data no delete could
// find" — naming the delete's own target as the one thing a delete cannot
// reach. Every count was correct; the sentence around them was not, which is
// the failure mode a report has instead of a crash.
//
// The root is DERIVED from the foreign keys, and the check is agreement with an
// independently written catalog query rather than with the constant "users" —
// a constant here would be the same unchecked promise the table list avoids.
func TestTheDeleteRootIsFoundFromTheForeignKeysAndIsNotReportedAsUncovered(t *testing.T) {
	h := newHarness(t)
	ctx := context.Background()
	a, _ := seedTwoUsers(t, h.super)

	c, err := censusFor(ctx, h.appStore(), mustULID(t, a.ID))
	if err != nil {
		t.Fatalf("census: %v", err)
	}

	// The independent read, written from the other direction: the table that the
	// most single-column user_id foreign keys point at.
	//
	// ⚠ `array_length(conkey,1) = 1` IS LOAD-BEARING AND WAS FOUND BY A PROBE.
	// Without it the composite key `(user_id, content_hash) REFERENCES app.blobs`
	// also qualifies, giving two answers — and the first version of both this
	// query and the production one then picked arbitrarily between `users` and
	// `blobs`. The probe that was supposed to test root detection reported
	// `the foreign keys say "blobs"`, which is how the defect surfaced.
	var want string
	var candidates int
	if err := h.super.QueryRow(ctx, `
		SELECT target.relname, count(*) OVER ()
		  FROM (SELECT DISTINCT target.relname
		          FROM pg_constraint co
		          JOIN pg_class child  ON child.oid = co.conrelid
		          JOIN pg_class target ON target.oid = co.confrelid
		          JOIN pg_namespace n  ON n.oid = child.relnamespace
		          JOIN pg_attribute at ON at.attrelid = co.conrelid
		                              AND at.attnum = ANY(co.conkey)
		         WHERE co.contype = 'f' AND n.nspname = 'app'
		           AND at.attname = 'user_id'
		           AND array_length(co.conkey, 1) = 1) AS target
		 LIMIT 1`).Scan(&want, &candidates); err != nil {
		t.Fatalf("independent root read: %v", err)
	}
	if candidates != 1 {
		t.Fatalf("the schema nominates %d root candidates, not 1 — the census cannot "+
			"be expected to pick one, and this test's premise is gone", candidates)
	}
	if c.Root != want {
		t.Errorf("census root = %q, the foreign keys say %q", c.Root, want)
	}
	if len(c.RootCandidates) != 1 {
		t.Errorf("census reported %d root candidates %v, want exactly 1",
			len(c.RootCandidates), c.RootCandidates)
	}
	if c.Root == "" {
		t.Fatal("no root found — every user_id is a foreign key to something, so an " +
			"empty answer means the query failed to look, not that the schema has no root")
	}
	if c.RootRows != 1 {
		t.Errorf("root rows visible to the user = %d, want 1 (their own row)", c.RootRows)
	}
	for _, u := range c.Uncovered {
		if u == c.Root {
			t.Errorf("the delete's root %q is listed as uncovered — the report would "+
				"describe its own target as data no delete could find", u)
		}
	}
	// The root must not be double-counted as a user_id table either.
	if hasTable(c, c.Root) {
		t.Errorf("root %q also appears among the user_id-scoped tables", c.Root)
	}
}

// TestSelfExistsSeparatesAMissingUserFromAnEmptyOne is the distinction a delete
// report must not blur: an RLS transaction for an id that does not exist reads
// zero from every table and looks exactly like a real user who owns nothing.
func TestSelfExistsSeparatesAMissingUserFromAnEmptyOne(t *testing.T) {
	h := newHarness(t)
	ctx := context.Background()
	a, _ := seedTwoUsers(t, h.super)
	s := h.appStore()

	var real, ghost bool
	if err := s.UserTx(ctx, mustULID(t, a.ID), func(tx *UserTx) error {
		var e error
		real, e = tx.SelfExists(ctx)
		return e
	}); err != nil {
		t.Fatalf("self exists (real): %v", err)
	}
	if !real {
		t.Error("a seeded user reports as not existing")
	}

	// An id that was never provisioned.
	if err := s.UserTx(ctx, NewULID(), func(tx *UserTx) error {
		var e error
		ghost, e = tx.SelfExists(ctx)
		return e
	}); err != nil {
		t.Fatalf("self exists (ghost): %v", err)
	}
	if ghost {
		t.Error("an id that was never provisioned reports as existing")
	}
}

// TestDeletePolicyStatusReportsTheRealCatalog. The value of this call is that
// it reads the catalog rather than restating schema.sql — so the assertion is
// that what it reports MATCHES the catalog, checked here by a second, separately
// written query rather than against a constant.
//
// ⚠ AN EARLIER VERSION OF THIS TEST ASSERTED ONLY `owner != ""` AND `forced`,
// on the stated grounds that "CI owns the tables as postgres, so the dangerous
// arrangement does not occur here". That was false: scripts/integration-test.sh
// applies db/schema.sql as mailroom_owner, so this harness has the deployed
// arrangement exactly (owner = mailroom_owner, FORCE RLS on, zero FOR DELETE
// policies naming the owner). Believing otherwise cost a scratch-table model of
// a table that was testable directly all along — see
// delete_policy_integration_test.go.
//
// A constant would be the wrong assertion even now, because a deployment could
// legitimately be provisioned with a different owner; the property under test is
// AGREEMENT WITH THE CATALOG, so that is what is measured.
func TestDeletePolicyStatusReportsTheRealCatalog(t *testing.T) {
	h := newHarness(t)
	ctx := context.Background()
	a, _ := seedTwoUsers(t, h.super)

	var owner string
	var forced bool
	var policies int
	if err := h.appStore().UserTx(ctx, mustULID(t, a.ID), func(tx *UserTx) error {
		var e error
		owner, forced, policies, e = tx.DeletePolicyStatus(ctx)
		return e
	}); err != nil {
		t.Fatalf("delete policy status: %v", err)
	}

	// The independent read: different query shape, superuser connection, no RLS
	// in the way. If the two disagree, the report is describing a database that
	// is not this one.
	var wantOwner string
	var wantForced bool
	if err := h.super.QueryRow(ctx, `
		SELECT pg_get_userbyid(c.relowner), c.relforcerowsecurity
		  FROM pg_class c JOIN pg_namespace n ON n.oid = c.relnamespace
		 WHERE n.nspname = 'app' AND c.relname = 'users'`).Scan(&wantOwner, &wantForced); err != nil {
		t.Fatalf("independent catalog read: %v", err)
	}
	var wantPolicies int
	if err := h.super.QueryRow(ctx, `
		SELECT count(*) FROM pg_policies
		 WHERE schemaname = 'app' AND tablename = 'users'
		   AND cmd IN ('DELETE', 'ALL') AND $1 = ANY(roles)`, wantOwner).Scan(&wantPolicies); err != nil {
		t.Fatalf("independent policy read: %v", err)
	}

	if owner != wantOwner {
		t.Errorf("owner reported %q, catalog says %q", owner, wantOwner)
	}
	if forced != wantForced {
		t.Errorf("FORCE RLS reported %v, catalog says %v", forced, wantForced)
	}
	if policies != wantPolicies {
		t.Errorf("owner DELETE policies reported %d, catalog says %d", policies, wantPolicies)
	}
	if !forced {
		t.Error("FORCE ROW LEVEL SECURITY is not set on app.users, but schema.sql sets it")
	}

	// Not an assertion — a record of what this harness actually is, so a future
	// reader does not have to re-derive it from two shell scripts.
	t.Logf("app.users here: owner=%s force_rls=%v owner_delete_policies=%d",
		owner, forced, policies)
}

func censusFor(ctx context.Context, s *Store, id ulid.ULID) (UserCensus, error) {
	var c UserCensus
	err := s.UserTx(ctx, id, func(tx *UserTx) error {
		var e error
		c, e = tx.UserRowCensus(ctx)
		return e
	})
	return c, err
}

func hasTable(c UserCensus, name string) bool {
	for _, t := range c.Tables {
		if t.Table == name {
			return true
		}
	}
	return false
}

func rowsFor(c UserCensus, name string) int64 {
	for _, t := range c.Tables {
		if t.Table == name {
			return t.Rows
		}
	}
	return -1
}
