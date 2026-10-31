// SPDX-License-Identifier: AGPL-3.0-only

//go:build integration

package store

// The five CI negative tests of mailroom-data-model.md §5 — red build on any
// failure (ADR-010). Fixtures are seeded as superuser; every assertion runs
// as the roles the application actually uses (mailroom_app / mailroom_admin).

import (
	"context"
	"errors"
	"fmt"
	"testing"

	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/oklog/ulid/v2"
)

const sqlstateInsufficientPrivilege = "42501" // covers both "permission denied" and RLS WITH CHECK violations

func TestRLSIsolation(t *testing.T) {
	h := newHarness(t)
	ctx := context.Background()
	a, b := seedTwoUsers(t, h.super)
	s := h.appStore()

	aID := mustULID(t, a.ID)

	// Guard: the table list the suites iterate must exactly match the live
	// schema, so a future table cannot dodge isolation coverage.
	t.Run("TableInventoryComplete", func(t *testing.T) {
		rows, err := h.super.Query(ctx,
			`SELECT tablename FROM pg_tables WHERE schemaname = 'app'`)
		if err != nil {
			t.Fatal(err)
		}
		defer rows.Close()
		live := map[string]bool{}
		for rows.Next() {
			var name string
			if err := rows.Scan(&name); err != nil {
				t.Fatal(err)
			}
			live[name] = true
		}
		known := map[string]bool{}
		for _, tbl := range append(append([]string{}, appTables...), systemTables...) {
			if known[tbl] {
				t.Errorf("app.%s is listed twice across appTables and systemTables", tbl)
			}
			known[tbl] = true
			if !live[tbl] {
				t.Errorf("app.%s is listed but does not exist in the schema", tbl)
			}
		}
		for tbl := range live {
			if !known[tbl] {
				t.Errorf("schema table app.%s missing from appTables — add fixtures + isolation coverage "+
					"(an instance-level table with no user_id belongs in systemTables instead, with its "+
					"own isolation test)", tbl)
			}
		}
	})

	// §5 test 1: as mailroom_app with NO identity set, every table is empty.
	// Fail closed: no identity, no rows.
	//
	// systemTables are exempt by construction, not by oversight: their rows
	// exist ONLY in the identity-less context, so this is precisely the context
	// in which they are not empty. Test 6 and each table's own test cover them.
	t.Run("1_NoIdentityNoRows", func(t *testing.T) {
		for _, table := range appTables {
			var n int
			if err := h.app.QueryRow(ctx,
				`SELECT count(*) FROM app.`+table).Scan(&n); err != nil {
				t.Errorf("app.%s: count without identity errored: %v", table, err)
				continue
			}
			if n != 0 {
				t.Errorf("app.%s: %d rows visible with no identity, want 0", table, n)
			}
		}
	})

	// §5 test 2: with identity A, B's rows are invisible to SELECT and
	// unreachable by UPDATE/DELETE, and writes carrying user B's id are
	// rejected by WITH CHECK. Positive control first: A sees its own rows.
	t.Run("2_CrossUserInvisibleUnreachable", func(t *testing.T) {
		for _, table := range appTables {
			col := userScopeColumn(table)

			// Positive control: A's fixture rows are visible to A.
			err := s.UserTx(ctx, aID, func(tx *UserTx) error {
				var n int
				if err := tx.tx.QueryRow(ctx,
					`SELECT count(*) FROM app.`+table+` WHERE `+col+` = $1`, a.ID).Scan(&n); err != nil {
					return err
				}
				if n == 0 {
					return fmt.Errorf("A cannot see its own rows")
				}
				return nil
			})
			if err != nil {
				t.Errorf("app.%s positive control: %v", table, err)
			}

			// B's rows invisible to SELECT.
			err = s.UserTx(ctx, aID, func(tx *UserTx) error {
				var n int
				if err := tx.tx.QueryRow(ctx,
					`SELECT count(*) FROM app.`+table+` WHERE `+col+` = $1`, b.ID).Scan(&n); err != nil {
					return err
				}
				if n != 0 {
					return fmt.Errorf("%d rows of B visible", n)
				}
				return nil
			})
			if err != nil {
				t.Errorf("app.%s SELECT: %v", table, err)
			}

			// B's rows unreachable by UPDATE and DELETE: either zero rows
			// affected (RLS filtered) or 42501 (no grant at all).
			assertUnreachable(t, s, aID, table,
				`UPDATE app.`+table+` SET `+col+` = `+col+` WHERE `+col+` = $1`, b.ID)
			assertUnreachable(t, s, aID, table,
				`DELETE FROM app.`+table+` WHERE `+col+` = $1`, b.ID)

			// Reassigning A's own rows to B must be rejected (WITH CHECK, or
			// missing grant on the admin-plane/audit tables) — never succeed.
			assertSQLState(t, s, aID, table+" reassign",
				`UPDATE app.`+table+` SET `+col+` = $1 WHERE `+col+` = $2`, b.ID, a.ID)
		}

		// Literal INSERTs carrying user_id = B, on representative tables.
		inserts := []struct {
			name string
			sql  string
			args []any
		}{
			{"labels", `INSERT INTO app.labels (id, user_id, name) VALUES ($1, $2, 'smuggled')`,
				[]any{NewULID().String(), b.ID}},
			{"contacts", `INSERT INTO app.contacts (id, user_id, display_name) VALUES ($1, $2, 'smuggled')`,
				[]any{NewULID().String(), b.ID}},
			{"feedback_events", `INSERT INTO app.feedback_events (id, user_id, action) VALUES ($1, $2, 'open')`,
				[]any{NewULID().String(), b.ID}},
		}
		for _, ins := range inserts {
			assertSQLState(t, s, aID, ins.name+" insert-as-B", ins.sql, ins.args...)
		}
	})

	// §5 test 3: mailroom_admin has NO grants on mail-content tables —
	// "admin can't read mail" is absent privilege, not a filtered view.
	//
	// ⚠ THE HARDCODED FOUR-TABLE LIST THIS USED TO BE COULD NOT SEE ITS OWN
	// FAILURE (#299). It probed messages/blobs/user_keys/contacts, so
	// `GRANT SELECT ON app.attachments TO mailroom_admin` — or ai_providers
	// (wrapped API keys), message_provenance, dsn_events, submissions,
	// contact_addresses, contact_keys, triage_scores, feedback_events — would let
	// the admin plane read mail with all five isolation tests green. This file is
	// the ONLY place in the repo asserting anything about mailroom_admin
	// privileges, so that gap was CI-wide.
	//
	// The set is now derived from the live catalog at runtime and asserted to
	// equal exactly {users, audit_log}, so a 43rd table cannot be silently missed.
	t.Run("3_AdminCannotReadMailContent", func(t *testing.T) {
		// ADR-009 / spec §10: the admin plane manages USERS and reads the AUDIT
		// trail. Nothing else. Adding to this set is a deliberate act that has to
		// be argued for here, which is the point of naming it.
		adminMayTouch := map[string]bool{"users": true, "audit_log": true}

		// has_any_column_privilege, not has_table_privilege: a COLUMN-level grant
		// is the exact shape a well-meaning "admin just needs the subject line"
		// change would take, and a table-level check cannot see one.
		rows, err := h.super.Query(ctx, `
			SELECT c.relname,
			       has_any_column_privilege('mailroom_admin', c.oid,
			                                'SELECT, INSERT, UPDATE, REFERENCES')
			    OR has_table_privilege('mailroom_admin', c.oid,
			                           'DELETE, TRUNCATE, TRIGGER')
			  FROM pg_class c
			  JOIN pg_namespace n ON n.oid = c.relnamespace
			 WHERE n.nspname = 'app' AND c.relkind = 'r'
			 ORDER BY c.relname`)
		if err != nil {
			t.Fatal(err)
		}
		defer rows.Close()
		granted := map[string]bool{}
		seen := 0
		for rows.Next() {
			var (
				name string
				any  bool
			)
			if err := rows.Scan(&name, &any); err != nil {
				t.Fatal(err)
			}
			seen++
			if any {
				granted[name] = true
			}
		}
		if err := rows.Err(); err != nil {
			t.Fatal(err)
		}
		// CONTROL ON THE QUERY ITSELF. If this returned nothing — a typo in the
		// schema name, a relkind that stopped matching — every assertion below
		// would pass vacuously, which is the failure mode this whole issue is
		// about.
		if seen < len(appTables) {
			t.Fatalf("the catalog query saw %d tables in schema app, fewer than the %d "+
				"appTables knows about — it is not looking where it thinks it is",
				seen, len(appTables))
		}
		for name := range granted {
			if !adminMayTouch[name] {
				t.Errorf("mailroom_admin holds a privilege on app.%s. "+
					"\"Admin cannot read mail\" (spec §10) is implemented as the ABSENCE of "+
					"grants; if this table genuinely belongs to the admin plane, add it to "+
					"adminMayTouch and say why", name)
			}
		}
		for name := range adminMayTouch {
			if !granted[name] {
				t.Errorf("mailroom_admin holds NO privilege on app.%s — the admin plane is "+
					"broken, and the negative assertions above would pass against a role "+
					"that can do nothing at all", name)
			}
		}

		// THE EXECUTOR IS NOT THE CATALOG. Ask it directly on the tables whose
		// exposure would be worst, both ways.
		for _, table := range []string{"messages", "blobs", "user_keys", "contacts", "attachments"} {
			_, err := h.admin.Exec(ctx, `SELECT 1 FROM app.`+table+` LIMIT 1`)
			var pgErr *pgconn.PgError
			if !errors.As(err, &pgErr) || pgErr.Code != sqlstateInsufficientPrivilege {
				t.Errorf("app.%s: admin SELECT: got %v, want SQLSTATE %s", table, err, sqlstateInsufficientPrivilege)
			}
		}
		// Sanity: the admin plane still works where it is supposed to.
		var n int
		if err := h.admin.QueryRow(ctx, `SELECT count(*) FROM app.users`).Scan(&n); err != nil {
			t.Errorf("admin SELECT on app.users should be granted: %v", err)
		} else if n < 2 {
			t.Errorf("admin sees %d users, want >= 2", n)
		}
	})

	// §5 test 4: cross-user reads through every API endpoint. Realized one layer
	// up, next to the handlers, in internal/api's RLS negative-test suite
	// (rls_integration_test.go: crossUserProbes drives every JMAP method AND the
	// download/upload/eventsource endpoints as user A naming user B's ids, under
	// this same mailroom_app pool). A forcing function (rls_forcing_test.go) fails
	// the build if a method or route is added without its cross-user probe, so the
	// suite stays exhaustive as the API grows. See #38.
	t.Run("4_APIEndpointsCrossUser", func(t *testing.T) {
		t.Skip("realized in internal/api (rls_integration_test.go), enforced under mailroom_app — see #38")
	})

	// §5 test 5: audit_log is append-only for every role.
	t.Run("5_AuditLogAppendOnly", func(t *testing.T) {
		for name, pool := range map[string]*pgxpool.Pool{
			"mailroom_app":   h.app,
			"mailroom_admin": h.admin,
		} {
			for _, stmt := range []string{
				`UPDATE app.audit_log SET action = 'tampered'`,
				`DELETE FROM app.audit_log`,
			} {
				_, err := pool.Exec(ctx, stmt)
				var pgErr *pgconn.PgError
				if !errors.As(err, &pgErr) || pgErr.Code != sqlstateInsufficientPrivilege {
					t.Errorf("%s: %q: got %v, want SQLSTATE %s", name, stmt, err, sqlstateInsufficientPrivilege)
				}
			}
		}
	})

	// §5 test 6 (#525): the tables exempt from tests 1 and 2 must have the
	// shape that makes the exemption safe. No user_id column — a table with one
	// holds per-user rows, belongs in appTables, and would be dodging isolation
	// coverage by sitting here — and row security forced, like every other
	// table. What each one admits, and to whom, is its own test.
	t.Run("6_SystemTablesAreUnscopedAndForced", func(t *testing.T) {
		for _, table := range systemTables {
			var hasUserID, enabled, forced bool
			err := h.super.QueryRow(ctx, `
				SELECT EXISTS (SELECT 1 FROM pg_attribute a
				                WHERE a.attrelid = c.oid AND a.attname = 'user_id'
				                  AND a.attnum > 0 AND NOT a.attisdropped),
				       c.relrowsecurity, c.relforcerowsecurity
				  FROM pg_class c
				  JOIN pg_namespace n ON n.oid = c.relnamespace
				 WHERE n.nspname = 'app' AND c.relname = $1 AND c.relkind = 'r'`,
				table).Scan(&hasUserID, &enabled, &forced)
			if err != nil {
				t.Errorf("app.%s: %v", table, err)
				continue
			}
			if hasUserID {
				t.Errorf("app.%s has a user_id column: it holds per-user rows, so it belongs in "+
					"appTables where tests 1 and 2 cover it, not in systemTables", table)
			}
			if !enabled || !forced {
				t.Errorf("app.%s: row security enabled=%v forced=%v, want both", table, enabled, forced)
			}
		}
	})

	// §5 test 7 (#776): every per-user table has row security ENABLED and FORCED.
	// The app role is not the owner, so the checks above bind it either way;
	// FORCE is what binds the OWNER (mailroom_owner runs migrations and holds
	// every privilege implicitly), and nothing above can observe it going
	// missing. Found by probing message_local_auth: removing its FORCE left the
	// whole suite green.
	t.Run("7_EveryAppTableIsForced", func(t *testing.T) {
		for _, table := range appTables {
			var enabled, forced bool
			if err := h.super.QueryRow(ctx, `
				SELECT c.relrowsecurity, c.relforcerowsecurity
				  FROM pg_class c JOIN pg_namespace n ON n.oid = c.relnamespace
				 WHERE n.nspname = 'app' AND c.relname = $1 AND c.relkind = 'r'`,
				table).Scan(&enabled, &forced); err != nil {
				t.Errorf("app.%s: %v", table, err)
				continue
			}
			if !enabled || !forced {
				t.Errorf("app.%s: row security enabled=%v forced=%v, want both", table, enabled, forced)
			}
		}
	})
}

func mustULID(t *testing.T, s string) ulid.ULID {
	t.Helper()
	id, err := ulid.Parse(s)
	if err != nil {
		t.Fatalf("parse ulid %q: %v", s, err)
	}
	return id
}

// assertUnreachable runs a statement targeting user B's rows as user A and
// requires it to touch nothing: zero rows affected, or 42501 where the role
// has no grant at all (audit_log, users).
func assertUnreachable(t *testing.T, s *Store, as ulid.ULID, table, sql string, args ...any) {
	t.Helper()
	var affected int64
	err := s.UserTx(context.Background(), as, func(tx *UserTx) error {
		tag, err := tx.tx.Exec(context.Background(), sql, args...)
		affected = tag.RowsAffected()
		return err
	})
	if err != nil {
		var pgErr *pgconn.PgError
		if errors.As(err, &pgErr) && pgErr.Code == sqlstateInsufficientPrivilege {
			return // no grant at all — even stronger than RLS filtering
		}
		t.Errorf("app.%s: %q errored unexpectedly: %v", table, sql, err)
		return
	}
	if affected != 0 {
		t.Errorf("app.%s: %q affected %d rows of user B, want 0", table, sql, affected)
	}
}

// assertSQLState runs a statement as user A and requires SQLSTATE 42501.
func assertSQLState(t *testing.T, s *Store, as ulid.ULID, label, sql string, args ...any) {
	t.Helper()
	err := s.UserTx(context.Background(), as, func(tx *UserTx) error {
		_, err := tx.tx.Exec(context.Background(), sql, args...)
		return err
	})
	var pgErr *pgconn.PgError
	if !errors.As(err, &pgErr) || pgErr.Code != sqlstateInsufficientPrivilege {
		t.Errorf("%s: got %v, want SQLSTATE %s", label, err, sqlstateInsufficientPrivilege)
	}
}
