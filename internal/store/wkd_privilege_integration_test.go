// SPDX-License-Identifier: AGPL-3.0-only

//go:build integration

package store

// The privilege pattern behind the anonymous WKD surface, tested under the
// ownership model that SHIPS rather than the one CI happens to build
// (Forgejo #257, refs #47/#48, ADR-024).
//
// # Why this file exists at all
//
// TestWKDDefinerCannotReadPrivateKeyMaterial asserts on app.user_keys, and it
// cannot see the property it is named for. scripts/integration-test.sh applies
// db/schema.sql as the postgres superuser, so in CI mailroom_owner does not own
// app.user_keys and never holds the table-level privilege that is the whole
// problem. Every arrangement of GRANT and REVOKE looks identical from there.
//
// That blind spot has now produced the same defect twice:
//
//   - 00031's column-level GRANT was decoration, because a table owner holds
//     column privileges implicitly.
//   - 00031's column-level REVOKE, added to fix exactly that, was ALSO
//     decoration, because a column-level revoke cannot remove the table-level
//     privilege an owner holds. Measured TRUE on the deployed database.
//
// So this test stops asserting about app.user_keys and instead BUILDS the
// production ownership model — a table genuinely owned by mailroom_owner — and
// tests the pattern itself. That is falsifiable in CI today, without waiting on
// the harness change that would let the real table be tested (applying the
// schema as mailroom_owner, a separate open follow-up).

import (
	"context"
	"strings"
	"testing"

	"github.com/jackc/pgx/v5/pgxpool"
)

// TestWKDRoleSeesOnlyPublishedRows asserts the ROW filter on the anonymous
// cross-user surface, as mailroom_wkd, with a bare SELECT.
//
// ⚠ THIS EXISTS BECAUSE NOTHING COULD SEE THAT FILTER FAIL (#299). Setting
// either policy to `USING (true)` left the ENTIRE suite green while
// mailroom_wkd could read the granted columns of every user's keys and
// identities — who holds a key, their fingerprints, their public keys, their
// identity bindings — published or not. Three separate things hid it:
//
//   - rls_test.go never assumes mailroom_wkd at all.
//   - TestWKDDefinerCannotReadPrivateKeyMaterial does `SET ROLE mailroom_wkd`
//     but asserts only that a SELECT SUCCEEDS: no row count, no "the
//     unpublished row is invisible" claim.
//   - every functional "unpublished ⇒ not found" test goes through
//     app.wkd_public_key, WHOSE OWN WHERE CLAUSE RE-FILTERS on wkd_published
//     and wkd_hash. Those tests pass identically with the policy wide open,
//     because the function's predicate is doing the work the policy claims to.
//
// So the assertion has to be made directly, on the table, as the role — never
// through the definer. Both conjuncts of each policy are exercised, so
// weakening either half is red:
//
//	user_keys_wkd_lookup   USING (wkd_published)
//	identities_wkd_lookup  USING (wkd_hash IS NOT NULL AND enabled)
func TestWKDRoleSeesOnlyPublishedRows(t *testing.T) {
	h := newHarness(t)
	ctx := context.Background()
	s := h.appStore()

	a, b := seedTwoUsers(t, h.super)
	aID := mustULID(t, a.ID)

	// A PUBLISHES. This is the row that must remain visible.
	publishedKey := seedPublishableKey(t, s, aID, mustULID(t, a.IdentityID))
	_, hash := publishedAs(t, h, a.IdentityID)
	if err := s.UserTx(ctx, aID, func(tx *UserTx) error {
		ok, e := tx.PublishKey(ctx, publishedKey, hash)
		if e != nil {
			return e
		}
		if !ok {
			t.Fatal("PublishKey refused the seeded key; the positive control cannot be set up")
		}
		return nil
	}); err != nil {
		t.Fatalf("publish: %v", err)
	}

	// B DOES NOT. Its fixture key is unpublished and its fixture identity has no
	// wkd_hash — the two states the policies exist to hide.
	var unpublishedKey string
	if err := h.super.QueryRow(ctx,
		`SELECT id FROM app.user_keys WHERE user_id = $1 LIMIT 1`, b.ID).Scan(&unpublishedKey); err != nil {
		t.Fatalf("read B's unpublished key: %v", err)
	}

	// A THIRD STATE: an identity that IS published but has been DISABLED. Without
	// it, dropping `AND enabled` from identities_wkd_lookup would go unnoticed.
	disabledIdentity := NewULID().String()
	if _, err := h.super.Exec(ctx, `
		INSERT INTO app.identities (id, user_id, account_id, kind, address, enabled, wkd_hash)
		VALUES ($1, $2, $3, 'fixed', $4, false, $5)`,
		disabledIdentity, b.ID, b.AccountID, "disabled-"+b.Tag+"@example.com", "zzzzzzzzzzzzzzzzzzzzzzzzzzzzzzzz",
	); err != nil {
		t.Fatalf("seed disabled published identity: %v", err)
	}

	// Everything below runs AS mailroom_wkd. SET ROLE drops the superuser's
	// BYPASSRLS for the session, so these policies actually apply — which is the
	// entire point (issued as the superuser, every count would be the total).
	conn, err := h.super.Acquire(ctx)
	if err != nil {
		t.Fatalf("acquire: %v", err)
	}
	defer conn.Release()
	if _, err := conn.Exec(ctx, `SET ROLE mailroom_wkd`); err != nil {
		t.Fatalf("SET ROLE mailroom_wkd: %v", err)
	}
	defer func() { _, _ = conn.Exec(context.Background(), `RESET ROLE`) }()

	countAs := func(sql string, args ...any) int {
		t.Helper()
		var n int
		if err := conn.QueryRow(ctx, sql, args...).Scan(&n); err != nil {
			t.Fatalf("as mailroom_wkd: %s: %v", sql, err)
		}
		return n
	}
	countSuper := func(sql string, args ...any) int {
		t.Helper()
		var n int
		if err := h.super.QueryRow(ctx, sql, args...).Scan(&n); err != nil {
			t.Fatalf("as superuser: %s: %v", sql, err)
		}
		return n
	}

	// ---- 1. THE POSITIVE CONTROL, FIRST -------------------------------------
	// Without it, a policy denying EVERYTHING satisfies every assertion below —
	// and that policy breaks WKD outright rather than securing it.
	if n := countAs(`SELECT count(*) FROM app.user_keys WHERE id = $1`, publishedKey.String()); n != 1 {
		t.Fatalf("mailroom_wkd sees %d rows for a PUBLISHED key, want 1 — the lookup cannot "+
			"work and every negative assertion below proves nothing", n)
	}
	if n := countAs(`SELECT count(*) FROM app.identities WHERE id = $1`, a.IdentityID); n != 1 {
		t.Fatalf("mailroom_wkd sees %d rows for a PUBLISHED identity, want 1", n)
	}

	// ---- 2. THE ROW THE POLICY EXISTS TO HIDE -------------------------------
	if n := countAs(`SELECT count(*) FROM app.user_keys WHERE id = $1`, unpublishedKey); n != 0 {
		t.Errorf("mailroom_wkd can see an UNPUBLISHED key (%d rows). user_keys_wkd_lookup is "+
			"not filtering: the anonymous, cross-user surface can enumerate which users hold "+
			"keys, and read their fingerprints and public key material, whether or not they "+
			"chose to publish", n)
	}
	if n := countAs(`SELECT count(*) FROM app.identities WHERE id = $1`, b.IdentityID); n != 0 {
		t.Errorf("mailroom_wkd can see an identity with NO wkd_hash (%d rows) — "+
			"identities_wkd_lookup is not filtering, so the anonymous surface can enumerate "+
			"every address every user holds", n)
	}
	if n := countAs(`SELECT count(*) FROM app.identities WHERE id = $1`, disabledIdentity); n != 0 {
		t.Errorf("mailroom_wkd can see a DISABLED identity that carries a wkd_hash (%d rows) — "+
			"the `AND enabled` half of identities_wkd_lookup is not doing anything, and a "+
			"user who turned an address off is still in the public directory", n)
	}

	// ---- 3. SET EQUALITY, WHICH IS WHAT CATCHES `USING (true)` --------------
	// A per-row check can be defeated by a policy that happens to filter that one
	// row; this compares the WHOLE visible set against the whole set the policy
	// claims to admit. Each equality is paired with a control proving there was
	// something to exclude, so it cannot pass by there being nothing to hide.
	keysTotal := countSuper(`SELECT count(*) FROM app.user_keys`)
	keysPublished := countSuper(`SELECT count(*) FROM app.user_keys WHERE wkd_published`)
	if keysTotal <= keysPublished {
		t.Fatalf("every user_keys row is published (%d of %d) — the equality below would hold "+
			"with the policy wide open", keysPublished, keysTotal)
	}
	if n := countAs(`SELECT count(*) FROM app.user_keys`); n != keysPublished {
		t.Errorf("mailroom_wkd sees %d of %d user_keys rows; exactly %d are published. "+
			"The row filter is wrong in the direction that matters most", n, keysTotal, keysPublished)
	}

	identTotal := countSuper(`SELECT count(*) FROM app.identities`)
	identVisible := countSuper(
		`SELECT count(*) FROM app.identities WHERE wkd_hash IS NOT NULL AND enabled`)
	if identTotal <= identVisible {
		t.Fatalf("every identity is published and enabled (%d of %d) — the equality below "+
			"would hold with the policy wide open", identVisible, identTotal)
	}
	if n := countAs(`SELECT count(*) FROM app.identities`); n != identVisible {
		t.Errorf("mailroom_wkd sees %d of %d identities; exactly %d are published and enabled",
			n, identTotal, identVisible)
	}
}

// TestOwnerColumnGrantNeedsTableLevelRevoke pins the ordering that makes the
// narrowing real, and demonstrates the trap it replaces.
//
// ⚠ PROBED: reverting step 3 to the column-only pattern (drop the table-level
// REVOKE, keep `REVOKE SELECT (private_key_ct)`) turns the sealed-column
// assertion red. Deleting the re-GRANT instead turns the POSITIVE control red,
// which is the other half of the trap — `REVOKE SELECT ON <table>` also drops
// every column grant, so revoke-after-grant denies the definer everything and
// breaks WKD outright.
func TestOwnerColumnGrantNeedsTableLevelRevoke(t *testing.T) {
	h := newHarness(t)
	ctx := context.Background()

	// A scratch schema owned by mailroom_owner, so the table below is owned by
	// mailroom_owner exactly as app.user_keys is in production.
	wkdPrivExecPool(ctx, t, h.super, `DROP SCHEMA IF EXISTS wkdpriv CASCADE`)
	wkdPrivExecPool(ctx, t, h.super, `CREATE SCHEMA wkdpriv AUTHORIZATION mailroom_owner`)
	t.Cleanup(func() {
		_, _ = h.super.Exec(context.Background(), `DROP SCHEMA IF EXISTS wkdpriv CASCADE`)
	})

	// Everything below runs as mailroom_owner. SET ROLE drops the superuser's
	// privileges for the session, so these are the owner's own rights — which
	// is the entire point.
	conn, err := h.super.Acquire(ctx)
	if err != nil {
		t.Fatalf("acquire: %v", err)
	}
	defer conn.Release()
	if _, err := conn.Exec(ctx, `SET ROLE mailroom_owner`); err != nil {
		t.Fatalf("SET ROLE mailroom_owner: %v", err)
	}
	defer func() { _, _ = conn.Exec(context.Background(), `RESET ROLE`) }()

	if _, err := conn.Exec(ctx, `CREATE TABLE wkdpriv.keys (
		id             text PRIMARY KEY,
		public_key     bytea,
		private_key_ct bytea
	)`); err != nil {
		t.Fatalf("create scratch table as mailroom_owner: %v", err)
	}

	// CONTROL ON THE SETUP: mailroom_owner must actually own it, or nothing
	// below is testing what it claims to.
	var owner string
	if err := conn.QueryRow(ctx,
		`SELECT tableowner FROM pg_tables WHERE schemaname='wkdpriv' AND tablename='keys'`,
	).Scan(&owner); err != nil {
		t.Fatalf("read table owner: %v", err)
	}
	if owner != "mailroom_owner" {
		t.Fatalf("scratch table is owned by %q, not mailroom_owner — this test would "+
			"prove nothing about the production model", owner)
	}

	canRead := func(col string) bool {
		var ok bool
		if err := conn.QueryRow(ctx,
			`SELECT has_column_privilege('mailroom_owner', 'wkdpriv.keys', $1, 'SELECT')`, col,
		).Scan(&ok); err != nil {
			t.Fatalf("has_column_privilege(%s): %v", col, err)
		}
		return ok
	}

	// ---- 1. THE TRAP, DEMONSTRATED -------------------------------------------
	// This is migration 00031's pattern verbatim. It reads as a narrow grant and
	// is not one.
	wkdPrivExec(ctx, t, conn,
		`GRANT SELECT (id, public_key) ON wkdpriv.keys TO mailroom_owner`)
	wkdPrivExec(ctx, t, conn,
		`REVOKE SELECT (private_key_ct) ON wkdpriv.keys FROM mailroom_owner`)

	if !canRead("private_key_ct") {
		t.Error("the column-only pattern denied the sealed column — if PostgreSQL has " +
			"changed this behaviour, #257's premise and migration 00032 need revisiting")
	}

	// ---- 2. AND THE ORDERING TRAP THAT REPLACES IT ---------------------------
	// REVOKE SELECT ON <table> also drops the column grants, so a naive
	// "just add the table-level revoke" leaves the definer able to read nothing.
	wkdPrivExec(ctx, t, conn, `REVOKE SELECT ON wkdpriv.keys FROM mailroom_owner`)
	if canRead("public_key") {
		t.Error("table-level REVOKE left a column grant standing — the ordering hazard " +
			"documented in 00032 would not be real, and its comment is wrong")
	}

	// ---- 3. THE SHIPPING PATTERN: revoke first, then re-grant ----------------
	wkdPrivExec(ctx, t, conn,
		`GRANT SELECT (id, public_key) ON wkdpriv.keys TO mailroom_owner`)

	if canRead("private_key_ct") {
		t.Error("mailroom_owner can still read the sealed column after the table-level " +
			"REVOKE and narrow re-GRANT — migration 00032 does not do what it says")
	}
	// POSITIVE CONTROL. Without this, a schema where the owner could read
	// nothing at all would satisfy the assertion above, and that schema breaks
	// app.wkd_public_key completely.
	if !canRead("public_key") {
		t.Error("mailroom_owner cannot read public_key either — the WKD definer cannot " +
			"work, and the negative assertion above proves nothing")
	}

	// ---- 4. THE CATALOG FUNCTION IS NOT THE ONLY WITNESS ---------------------
	// has_column_privilege could in principle disagree with the executor. Ask
	// the executor directly, both ways.
	if _, err := conn.Exec(ctx, `SELECT private_key_ct FROM wkdpriv.keys`); err == nil {
		t.Error("SELECT private_key_ct succeeded for the table owner — the privilege " +
			"catalog and the executor disagree, and the executor is what matters")
	} else if !strings.Contains(strings.ToLower(err.Error()), "permission denied") {
		t.Errorf("SELECT private_key_ct failed for the WRONG reason (%v) — the test would "+
			"pass even with the privilege in place", err)
	}
	if _, err := conn.Exec(ctx, `SELECT id, public_key FROM wkdpriv.keys`); err != nil {
		t.Errorf("SELECT of the granted columns was refused (%v) — the definer's own "+
			"query would fail and every WKD lookup with it", err)
	}
}

func wkdPrivExecPool(ctx context.Context, t *testing.T, pool *pgxpool.Pool, sql string) {
	t.Helper()
	if _, err := pool.Exec(ctx, sql); err != nil {
		t.Fatalf("exec %q: %v", sql, err)
	}
}

func wkdPrivExec(ctx context.Context, t *testing.T, conn *pgxpool.Conn, sql string) {
	t.Helper()
	if _, err := conn.Exec(ctx, sql); err != nil {
		t.Fatalf("exec %q: %v", sql, err)
	}
}
