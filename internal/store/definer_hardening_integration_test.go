// SPDX-License-Identifier: AGPL-3.0-only

//go:build integration

package store

// The SECURITY DEFINER hardening of #309, asserted against the live catalog.
//
// Nothing here was broken when it was written — the definer layer audited clean.
// These tests exist so the two properties migration 00037 made STRUCTURAL cannot
// quietly revert to being true only by review.
//
// ⚠ scripts/integration-test.sh applies db/schema.sql AS mailroom_owner, which
// is what makes the default-privilege assertion meaningful here at all: a
// superuser-applied schema creates functions under the SUPERUSER's default ACL,
// where mailroom_owner's default privileges do not apply and everything looks
// PUBLIC-executable regardless. That is the same CI blind spot that let three
// attempts at the WKD narrowing ship inert (#266), and it is worth naming every
// time a privilege is asserted in this package.

import (
	"context"
	"strings"
	"testing"
	"time"
)

// TestEveryDefinerPinsPgTempLast is #309 item 2.
//
// PostgreSQL searches pg_temp FIRST for any unqualified RELATION, whatever
// search_path says, unless pg_temp appears in the path explicitly — in which
// case it is searched exactly where it is written. mailroom_app can create
// temporary objects (PUBLIC holds TEMP on the database), so a definer body with
// one unqualified `FROM users` would read a table the caller controls, with the
// definer's privileges.
//
// The assertion is on proconfig rather than on the bodies because that is the
// property: with the terminator the planner enforces it, and a future
// unqualified reference stops being a vulnerability.
func TestEveryDefinerPinsPgTempLast(t *testing.T) {
	h := newHarness(t)
	ctx := context.Background()

	rows, err := h.super.Query(ctx, `
		SELECT p.proname, p.pronargs, COALESCE(array_to_string(p.proconfig, '|'), '')
		  FROM pg_proc p
		  JOIN pg_namespace n ON n.oid = p.pronamespace
		 WHERE n.nspname = 'app' AND p.prosecdef
		 ORDER BY p.proname, p.pronargs`)
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	seen := 0
	for rows.Next() {
		var (
			name   string
			nargs  int
			config string
		)
		if err := rows.Scan(&name, &nargs, &config); err != nil {
			t.Fatal(err)
		}
		seen++
		if !strings.Contains(config, "search_path=app, pg_temp") {
			t.Errorf("app.%s/%d has proconfig %q, want search_path pinned to `app, pg_temp`. "+
				"Without the terminator, one unqualified relation reference in this body "+
				"resolves to the CALLER's pg_temp — the classic definer hijack (#309)",
				name, nargs, config)
		}
	}
	if err := rows.Err(); err != nil {
		t.Fatal(err)
	}
	// ⚠ THE COUNT IS AN ASSERTION, NOT A LOG LINE. A new definer must fail this
	// test, because a new cross-user surface is exactly the thing that should not
	// slip in unreviewed — and a count of ZERO would make every assertion above
	// vacuous.
	//
	// NINE as of migration 00040 (#54), and here is the argument the count
	// demands. The ninth is app.hard_delete_user, and it needs the definer for
	// the same reason app.provision_user does: the operation must run as
	// mailroom_owner. mailroom_app has no DELETE grant on app.users and no policy
	// there — deliberately, per ADR-009 — so without a definer the delete is
	// either impossible or would require widening the application role's rights
	// over the user table permanently. A narrow, EXECUTE-granted function is the
	// smaller surface: it takes a user id and an actor, deletes exactly that row,
	// and writes the tombstone in the same transaction.
	//
	// It is also the only definer that DESTROYS anything, which is why its own
	// tests live in hard_delete_user_integration_test.go and why the caller is
	// forbidden from treating "0 rows" as success.
	//
	// TEN as of migration 00054 (#524), and the argument the count demands. The
	// tenth is app.count_due_jobs, which exists so mailroom_job_queue_depth can
	// report real backlog. It needs the definer for the same reason
	// app.dequeue_due_jobs does: the dispatcher runs in a SystemTx where
	// app.current_user_id() is NULL, so the app.jobs policy matches SYSTEM rows
	// only and a plain count silently UNDERCOUNTS by every per-user job rather
	// than failing — a plausible wrong number, which is worse than an error.
	//
	// It is the SMALLEST of the ten. dequeue_due_jobs hands the same caller whole
	// job rows across every user; this one returns a single bigint and cannot
	// name a user, a job or a payload, so everything it could disclose is already
	// implied by the function beside it. It is also the only definer declared
	// STABLE — it reads and never writes.
	//
	// The eleventh and twelfth are agent access (#49, migration 00074, ADR-042).
	// app.api_key_for_token is bearer authentication: before any identity exists
	// it maps the SHA-256 of a presented token to (user_id, key_id) for a key that
	// is neither revoked nor suspended — the user_id_for_subject shape, two ids
	// and nothing else. app.api_key_pending_due_users names the users with an
	// expired approval request so the sweep can expire them in each user's own
	// UserTx — the count_due_jobs shape, ids only. Both are STABLE.
	//
	// The thirteenth is app.session_for_token (#937, migration 00082, ADR-048):
	// the per-request browser-session lookup, which has to run before any
	// identity exists. It is the api_key_for_token shape, an exact hash match
	// returning the session id, user id and last confirmation time, and nothing
	// else, refusing a revoked row and a disabled or deleted user. STABLE.
	//
	// The fourteenth is app.sweep_sessions (#937, migration 00083, ADR-048
	// section 7): the daily deletion of old sessions. It needs the definer because
	// it runs with no user identity and deletes ACROSS users, which mailroom_app
	// has no grant or policy for (it holds only SELECT, INSERT and UPDATE on
	// app.sessions, deliberately). It is the second definer that destroys rows,
	// so the caution applies: it takes only a clock, deletes only rows past the
	// documented ages (revoked more than 7 days ago, live and unseen for 45), is
	// bounded again by owner policies at the wall clock, and returns a count per
	// reason and nothing else, so it discloses no row, id or user. Its tests
	// (TestSweepSessions, TestSweepSessionsOwnerPolicies) fail if the owner SELECT
	// policy is removed, because a DELETE without it succeeds on zero rows.
	//
	// The fifteenth and sixteenth are the job sweep (migration 00084).
	// app.sweep_terminal_jobs deletes finished job rows across users, the third
	// definer that destroys rows. It takes a clock, two retention windows and a
	// batch size, deletes only 'done' and 'dead' rows older than those windows
	// (never 'pending' or 'running', so no live idempotency key is freed), and
	// returns a count per state and nothing else. Job rows are already the
	// deliberate cross-user surface dequeue_due_jobs hands to the same caller,
	// and mailroom_owner's existing ALL policy on app.jobs is what it runs under;
	// no new policy was added. app.job_census is its read-only report, STABLE,
	// returning counts per (kind, state) and the oldest finish time per group:
	// less than dequeue_due_jobs already discloses, the count_due_jobs shape.
	if seen != 16 {
		t.Errorf("schema app has %d SECURITY DEFINER functions, want 16. If a definer was "+
			"added deliberately, bump this number in the same commit that argues for the new "+
			"cross-user surface", seen)
	}
}

// TestNoDefinerIsPublicExecutable is #309 item 4's observable consequence: the
// per-function REVOKE boilerplate is still there, and now so is the default that
// makes forgetting it harmless.
func TestNoDefinerIsPublicExecutable(t *testing.T) {
	h := newHarness(t)
	ctx := context.Background()

	rows, err := h.super.Query(ctx, `
		SELECT p.proname, p.oid::regprocedure::text,
		       has_function_privilege('public', p.oid, 'EXECUTE'),
		       has_function_privilege('mailroom_app', p.oid, 'EXECUTE')
		  FROM pg_proc p
		  JOIN pg_namespace n ON n.oid = p.pronamespace
		 WHERE n.nspname = 'app' AND p.prosecdef
		 ORDER BY p.proname`)
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	seen, callable := 0, 0
	for rows.Next() {
		var (
			name, sig    string
			pub, appRole bool
		)
		if err := rows.Scan(&name, &sig, &pub, &appRole); err != nil {
			t.Fatal(err)
		}
		seen++
		if pub {
			t.Errorf("%s is EXECUTE-able by PUBLIC. It is a SECURITY DEFINER on a cross-user "+
				"surface; anyone who can connect can call it (#309)", sig)
		}
		if appRole {
			callable++
		}
	}
	if err := rows.Err(); err != nil {
		t.Fatal(err)
	}
	if seen == 0 {
		t.Fatal("no definer functions found — the query is not looking where it thinks it is")
	}
	// POSITIVE CONTROL. Without it, a schema where NOTHING could execute these
	// would satisfy the assertion above, and that schema cannot log a user in,
	// fire a timer, or answer a WKD lookup.
	if callable == 0 {
		t.Error("mailroom_app cannot execute ANY definer — login, the job dispatcher and the " +
			"WKD lookup are all broken, and the PUBLIC assertions above prove nothing")
	}
}

// TestFutureFunctionsAreNotBornPublicExecutable is the #309 item 4 property
// itself, exercised the only way that is not a tautology: by creating a ninth
// function as mailroom_owner and looking at what it was born with.
//
// ⚠ THIS IS THE TEST THAT WOULD HAVE CAUGHT THE GAP. Asserting that the existing
// eight are not PUBLIC-executable says nothing about the next one — each of them
// carries its own explicit REVOKE, so they would pass with pg_default_acl empty,
// which is exactly the state the audit found.
func TestFutureFunctionsAreNotBornPublicExecutable(t *testing.T) {
	h := newHarness(t)
	ctx := context.Background()

	conn, err := h.super.Acquire(ctx)
	if err != nil {
		t.Fatalf("acquire: %v", err)
	}
	defer conn.Release()
	// AS mailroom_owner: default privileges are keyed on the CREATING role, and
	// mailroom_owner is the role migrations run as.
	if _, err := conn.Exec(ctx, `SET ROLE mailroom_owner`); err != nil {
		t.Fatalf("SET ROLE mailroom_owner: %v", err)
	}
	defer func() { _, _ = conn.Exec(context.Background(), `RESET ROLE`) }()

	name := "app.ninth_definer_probe_" + strings.ToLower(NewULID().String())
	// Cleanup goes through the POOL, not through conn: t.Cleanup runs after the
	// deferred conn.Release() above, and a released *pgxpool.Conn panics on use.
	t.Cleanup(func() {
		_, _ = h.super.Exec(context.Background(), `DROP FUNCTION IF EXISTS `+name+`()`)
	})
	if _, err := conn.Exec(ctx,
		`CREATE FUNCTION `+name+`() RETURNS int LANGUAGE sql AS 'SELECT 1'`); err != nil {
		t.Fatalf("create probe function as mailroom_owner: %v", err)
	}

	var pub bool
	if err := conn.QueryRow(ctx,
		`SELECT has_function_privilege('public', $1::regprocedure, 'EXECUTE')`,
		name+"()").Scan(&pub); err != nil {
		t.Fatalf("read probe privilege: %v", err)
	}
	if pub {
		t.Error("a function created in schema app by mailroom_owner is EXECUTE-able by " +
			"PUBLIC. ALTER DEFAULT PRIVILEGES … REVOKE EXECUTE ON FUNCTIONS FROM PUBLIC is " +
			"not in effect, so a ninth SECURITY DEFINER written without its own REVOKE " +
			"boilerplate would be callable by anyone who can connect (#309)")
	}
}

// TestRLSAnchorStaysCallable is the negative-control side of the same migration.
//
// The default-privilege revoke sits BEFORE app.current_user_id() is created in
// db/schema.sql, so on a fresh owner-applied build PUBLIC never gets EXECUTE on
// it — and almost every RLS policy calls it, evaluated with the privileges of
// the role running the query. Miss a grant and the application reads zero rows
// everywhere. That failure is total rather than subtle, which is the right
// shape; this asserts it has not happened.
func TestRLSAnchorStaysCallable(t *testing.T) {
	h := newHarness(t)
	ctx := context.Background()

	for _, role := range []string{"mailroom_app", "mailroom_admin", "mailroom_owner", "mailroom_wkd"} {
		var ok bool
		if err := h.super.QueryRow(ctx,
			`SELECT has_function_privilege($1, 'app.current_user_id()', 'EXECUTE')`, role).Scan(&ok); err != nil {
			t.Fatalf("has_function_privilege(%s): %v", role, err)
		}
		if !ok {
			t.Errorf("%s cannot execute app.current_user_id(); every RLS policy naming it "+
				"fails and that role reads nothing at all (#309)", role)
		}
	}
	// AND THE EXECUTOR, not just the catalog: a real query through a real policy.
	a, _ := seedTwoUsers(t, h.super)
	s := h.appStore()
	if err := s.UserTx(ctx, mustULID(t, a.ID), func(tx *UserTx) error {
		var n int
		if e := tx.tx.QueryRow(ctx,
			`SELECT count(*) FROM app.messages WHERE user_id = $1`, a.ID).Scan(&n); e != nil {
			return e
		}
		if n == 0 {
			t.Error("a policy-filtered read returned nothing for a user with seeded mail — " +
				"the RLS anchor is not resolving")
		}
		return nil
	}); err != nil {
		t.Fatalf("policy-filtered read: %v", err)
	}
}

// TestOwnerRoleAttributesStayNarrow is #309 item 1's testable half.
//
// The comments now say LOGIN is expected and that the protection is elsewhere.
// This asserts where: mailroom_owner must stay non-superuser and non-BYPASSRLS,
// because those two are what keep FORCE ROW LEVEL SECURITY binding on the role
// migrations connect as. A future `ALTER ROLE … SUPERUSER` to fix some deploy
// problem would silently remove every guarantee in this package.
func TestOwnerRoleAttributesStayNarrow(t *testing.T) {
	h := newHarness(t)
	ctx := context.Background()

	for _, role := range []string{"mailroom_owner", "mailroom_app", "mailroom_admin", "mailroom_wkd"} {
		var super, bypass bool
		if err := h.super.QueryRow(ctx,
			`SELECT rolsuper, rolbypassrls FROM pg_roles WHERE rolname = $1`, role).Scan(&super, &bypass); err != nil {
			t.Fatalf("read %s attributes: %v", role, err)
		}
		if super {
			t.Errorf("%s is a SUPERUSER. RLS does not apply to it, so every isolation "+
				"guarantee in this package is void for anything running as that role", role)
		}
		if bypass {
			t.Errorf("%s holds BYPASSRLS. FORCE ROW LEVEL SECURITY no longer binds it and "+
				"cross-user isolation is gone", role)
		}
	}
}

// TestJobPayloadsCarryReferencesOnly is #309 item 3.
//
// app.dequeue_due_jobs is a SECURITY DEFINER declared `RETURNS SETOF app.jobs`,
// so the dispatcher receives WHOLE ROWS — payload included — for every user. The
// claim that it never sees another user's mail is therefore not enforced by the
// definer, the policies or the grants: it is enforced by what callers put in the
// payload, and until #309 that was convention with nothing checking it.
//
// This walks every job row present and holds its payload keys against
// store.jobPayloadKeys. A kind that starts embedding a subject line, a snippet
// or a body fails here rather than silently turning a definer into a cross-user
// mail conduit.
func TestJobPayloadsCarryReferencesOnly(t *testing.T) {
	h := newHarness(t)
	ctx := context.Background()

	// Seed one job of a known kind so the walk below has something to walk even
	// on a clean database. Without this the test would pass on zero rows, which
	// is the "empty result reads as success" failure this project keeps paying
	// for (#155, #176).
	a, _ := seedTwoUsers(t, h.super)
	s := h.appStore()
	if err := s.UserTx(ctx, mustULID(t, a.ID), func(tx *UserTx) error {
		_, err := tx.EnqueueJob(ctx, "submit", time.Now().Add(time.Hour),
			[]byte(`{"submission_id":"`+NewULID().String()+`"}`),
			"jobs-payload-probe-"+NewULID().String())
		return err
	}); err != nil {
		t.Fatal(err)
	}

	rows, err := h.super.Query(ctx, `
		SELECT j.kind,
		       COALESCE((SELECT string_agg(k, ',' ORDER BY k)
		                   FROM jsonb_object_keys(j.payload) k), '')
		  FROM app.jobs j
		 GROUP BY 1, 2`)
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()

	seen := 0
	for rows.Next() {
		var kind, keyList string
		if err := rows.Scan(&kind, &keyList); err != nil {
			t.Fatal(err)
		}
		seen++

		// Through the SAME decision procedure the unit test pins, never a second
		// copy of it: a walk that judged payloads its own way could disagree with
		// the rule, and then neither would mean anything (#299's duplicate-
		// allocator lesson, applied before it happens).
		var keys []string
		if keyList != "" {
			keys = strings.Split(keyList, ",")
		}
		bad, known := PayloadKeysOutsideAllowlist(kind, keys)
		if !known {
			t.Errorf("app.jobs holds kind %q, which store.jobPayloadKeys does not describe. "+
				"Every kind must declare what its payload may carry, because the dispatcher "+
				"receives that payload for EVERY user through a SECURITY DEFINER (#309 item 3).", kind)
			continue
		}
		for _, k := range bad {
			t.Errorf("job kind %q carries payload key %q, which is not on its allowlist. "+
				"Job payloads carry IDS ONLY: app.dequeue_due_jobs hands whole rows across "+
				"user boundaries, so anything here that is not a reference is a cross-user "+
				"disclosure. If a handler needs the message, it must re-read it under the "+
				"target user's identity (#309 item 3).", kind, k)
		}
	}
	if err := rows.Err(); err != nil {
		t.Fatal(err)
	}

	// CONTROL: the walk must have walked something. A query that matched nothing
	// — wrong schema, a renamed table — passes every assertion above in silence.
	if seen == 0 {
		t.Fatal("the payload walk saw zero job kinds, but this test seeded one. " +
			"It is not looking at app.jobs.")
	}
}
