// SPDX-License-Identifier: AGPL-3.0-only

package store

import (
	"context"
	"fmt"

	"github.com/jackc/pgx/v5"
)

// The per-table census a hard delete has to be judged against (#54).
//
// # Why this reads the catalog instead of a list
//
// A hardcoded table list is a promise that someone will remember to update it.
// This project has already paid for the version of that promise where one
// producer forgot one INSERT for the whole life of the project (#316), and the
// consequence there was a sweep that walked past every model a user owned while
// reporting success. A delete is the worst possible place for that: "delete
// everything about me" is exactly the operation where doing nothing must not
// look like doing something.
//
// So the table set is derived from pg_catalog at run time. The 43rd table
// appears in the census the moment it is created, without anyone editing Go.
//
// # And the tables it CANNOT cover are reported, not hidden
//
// A user_id-driven census can only see tables that have a user_id. Any other
// table in `app` is outside the sweep by construction, and a reader has to be
// told which ones those are — otherwise "every table is accounted for" is a
// claim about the tables we happened to look at.

// TableCount is one table's row count for the calling user.
type TableCount struct {
	Table string
	Rows  int64
	// SkipReason is non-empty when the count could NOT be taken — typically
	// because the app role has no SELECT on that table.
	//
	// ⚠ A skipped table is reported, never silently omitted or counted as zero.
	// A table this report cannot read is a table a delete cannot verify, and
	// zero is the answer that would make it disappear.
	SkipReason string
}

// UserCensus is the whole picture for one user.
type UserCensus struct {
	// Tables carrying a user_id, with the caller's row count in each.
	Tables []TableCount
	// Root is the table every `user_id` column references — the row a hard
	// delete actually targets, and the origin of the ON DELETE CASCADE that
	// reaches everything in Tables.
	//
	// ⚠ IT IS DISCOVERED FROM THE FOREIGN KEYS, NOT NAMED HERE, for the same
	// reason the table set is: a constant would be a second claim about the
	// schema, free to drift from it.
	//
	// ⚠ AND IT IS SEPARATED FROM Uncovered BECAUSE THE FIRST VERSION OF THIS
	// REPORT PRINTED IT THERE — app.users has no `user_id` column, so a
	// user_id-driven census classified the delete's own target under "outside a
	// user-scoped delete ... a new table here would hold data no delete could
	// find". That is the precise opposite of true for the one table it named,
	// and it was visible on the first live run.
	Root     string
	RootRows int64
	// RootCandidates is every table nominated as the root. Root is set only when
	// there is exactly one; this field is what lets an ambiguous or empty answer
	// report its bound instead of presenting a guess as a fact.
	RootCandidates []string
	// RootSkipReason is non-empty when RootRows could NOT be counted.
	//
	// ⚠ WITHOUT THIS, A FAILED COUNT LEAVES RootRows AT ZERO — and zero here does
	// not read as "not measured", it reads as "this user's own row is already
	// gone", which is the single most misleading thing this report could say. The
	// per-table counts already distinguish the two; the root must as well.
	RootSkipReason string
	// Uncovered are tables in `app` with NO user_id column and which are not the
	// root. They are not part of a user-scoped delete and are listed so that
	// fact is visible.
	Uncovered []string
	// TotalRows across Tables (skipped tables contribute nothing and are
	// counted separately).
	TotalRows int64
	// Skipped is how many tables could not be read.
	Skipped int
}

// UserRowCensus counts the caller's rows in every `app` table that has a
// user_id, discovering the table set from pg_catalog.
//
// ⚠ IT RUNS INSIDE THE CALLER'S UserTx, so RLS scopes every count to that user
// without this code writing a single WHERE clause. That is deliberate: a census
// that filtered by hand would be a SECOND implementation of the isolation rule,
// free to disagree with the policies that actually govern the delete. Here the
// number reported IS the number RLS admits.
func (tx *UserTx) UserRowCensus(ctx context.Context) (UserCensus, error) {
	var out UserCensus

	rows, err := tx.tx.Query(ctx, `
		SELECT c.relname,
		       bool_or(a.attname = 'user_id') AS has_user_id
		  FROM pg_class c
		  JOIN pg_namespace n ON n.oid = c.relnamespace
		  LEFT JOIN pg_attribute a
		         ON a.attrelid = c.oid AND NOT a.attisdropped AND a.attnum > 0
		 WHERE n.nspname = 'app'
		   AND c.relkind = 'r'
		 GROUP BY c.relname
		 ORDER BY c.relname`)
	if err != nil {
		return out, fmt.Errorf("store: list app tables: %w", err)
	}
	type tbl struct {
		name      string
		hasUserID bool
	}
	var tables []tbl
	for rows.Next() {
		var t tbl
		if err := rows.Scan(&t.name, &t.hasUserID); err != nil {
			rows.Close()
			return out, fmt.Errorf("store: scan app table: %w", err)
		}
		tables = append(tables, t)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return out, fmt.Errorf("store: list app tables: %w", err)
	}
	if len(tables) == 0 {
		// Not an empty result — a census that found no tables at all did not
		// measure an empty database, it failed to look.
		return out, fmt.Errorf("store: user census found no tables in schema app; " +
			"that is a broken query, not an empty schema")
	}

	// Which table do the user_id columns point at? That is the delete's root,
	// derived rather than assumed.
	//
	// ⚠ THE FOREIGN KEY MUST BE THE user_id COLUMN ALONE — `array_length(conkey,1)
	// = 1`. Without that clause this query has TWO answers, and a probe caught it
	// picking the wrong one: app.message_provenance (and friends) carry a
	// COMPOSITE key `(user_id, content_hash) REFERENCES app.blobs`, so a match on
	// "some column of this key is named user_id" also nominates app.blobs. On the
	// deployed schema the candidates were `users` (42 constraints) and `blobs`
	// (1), and a bare `DISTINCT ... LIMIT 1` with no ORDER BY returned whichever
	// the planner reached first. It compiled, it ran, and it named the wrong root.
	//
	// ⚠ AND AN AMBIGUOUS ANSWER IS REPORTED, NOT RESOLVED. Root is set only when
	// exactly one candidate survives; otherwise it stays empty and the candidates
	// are handed back so the report can say what it actually found. Picking one of
	// several would be the same silent guess in a new place.
	rootRows, err := tx.tx.Query(ctx, `
		SELECT DISTINCT target.relname
		  FROM pg_constraint co
		  JOIN pg_class child   ON child.oid = co.conrelid
		  JOIN pg_class target  ON target.oid = co.confrelid
		  JOIN pg_namespace n   ON n.oid = child.relnamespace
		  JOIN pg_attribute a   ON a.attrelid = co.conrelid
		                       AND a.attnum = ANY(co.conkey)
		 WHERE co.contype = 'f'
		   AND n.nspname = 'app'
		   AND a.attname = 'user_id'
		   AND array_length(co.conkey, 1) = 1
		 ORDER BY target.relname`)
	if err != nil {
		return out, fmt.Errorf("store: find census root: %w", err)
	}
	for rootRows.Next() {
		var name string
		if err := rootRows.Scan(&name); err != nil {
			rootRows.Close()
			return out, fmt.Errorf("store: scan census root: %w", err)
		}
		out.RootCandidates = append(out.RootCandidates, name)
	}
	rootRows.Close()
	if err := rootRows.Err(); err != nil {
		return out, fmt.Errorf("store: find census root: %w", err)
	}
	if len(out.RootCandidates) == 1 {
		out.Root = out.RootCandidates[0]
	}

	for _, t := range tables {
		if !t.hasUserID {
			if t.name == out.Root {
				// The delete's target, not an unreachable table.
				var n int64
				if err := tx.tx.QueryRow(ctx,
					"SELECT count(*) FROM "+pgx.Identifier{"app", t.name}.Sanitize()).Scan(&n); err != nil {
					out.RootSkipReason = err.Error()
				} else {
					out.RootRows = n
				}
				continue
			}
			out.Uncovered = append(out.Uncovered, t.name)
			continue
		}
		// The identifier comes from pg_catalog, but it is quoted anyway: a
		// value's provenance is not a substitute for quoting it.
		q := "SELECT count(*) FROM " + pgx.Identifier{"app", t.name}.Sanitize()
		var n int64
		if err := tx.tx.QueryRow(ctx, q).Scan(&n); err != nil {
			out.Tables = append(out.Tables, TableCount{Table: t.name, SkipReason: err.Error()})
			out.Skipped++
			continue
		}
		out.Tables = append(out.Tables, TableCount{Table: t.name, Rows: n})
		out.TotalRows += n
	}
	return out, nil
}

// SelfExists reports whether the calling user's own app.users row is visible.
//
// ⚠ THE DISTINCTION IT DRAWS IS THE ONE A DELETE REPORT MUST NOT GET WRONG. A
// UserTx opened for an id that does not exist reads zero rows from every table
// and is indistinguishable from a real user who owns nothing — so a report
// without this check would answer "nothing to delete" for a typo'd id, in the
// most confident possible tone.
//
// It works from inside the user's own transaction and needs no admin plane and
// no new SECURITY DEFINER: app.users' app-role policy is
// `users_self USING (id = app.current_user_id())`, so this count is 1 for a
// user that exists and 0 for one that does not. It deliberately does NOT filter
// on deleted_at — a soft-deleted user is exactly who a hard delete is for, and
// reporting them as absent would refuse the primary case.
func (tx *UserTx) SelfExists(ctx context.Context) (bool, error) {
	var n int
	if err := tx.tx.QueryRow(ctx, `SELECT count(*) FROM app.users`).Scan(&n); err != nil {
		return false, fmt.Errorf("store: self exists: %w", err)
	}
	return n > 0, nil
}

// SelfIsAdmin reports whether the transaction's user holds the admin role
// (app.users.role, set from the identity provider's admin group at each login).
// Like SelfExists it needs no admin plane: users_self lets a user read exactly
// its own row. Account/getHealth uses it to show operator-only notices (#52).
func (tx *UserTx) SelfIsAdmin(ctx context.Context) (bool, error) {
	var n int
	if err := tx.tx.QueryRow(ctx,
		`SELECT count(*) FROM app.users WHERE id = $1 AND role = 'admin'`,
		tx.userID.String()).Scan(&n); err != nil {
		return false, fmt.Errorf("store: self is admin: %w", err)
	}
	return n > 0, nil
}

// DeletePolicyStatus reports whether a hard delete could actually remove the
// app.users row, read from pg_catalog at run time.
//
// ⚠ IT IS DERIVED, NEVER ASSERTED FROM schema.sql, because the dangerous state
// is invisible there. app.users is owned by mailroom_owner on a deployed
// instance, and a table owner holds DELETE implicitly — so information_schema
// reports a grant the schema file never writes, and nothing raises a permission
// error. With FORCE ROW LEVEL SECURITY on and no FOR DELETE policy for that
// owner, the statement is filtered to zero rows and DELETE reports success.
//
// ⚠ WHICH CI JOB YOU MEAN DECIDES THE ANSWER, and getting that backwards is
// what made an earlier version of this comment wrong. scripts/schema-apply.sh
// applies the schema as postgres, so that job is blind to every ownership-
// dependent property. scripts/integration-test.sh applies it as
// **mailroom_owner** on purpose — its header explains why, and names the two
// defects the superuser version shipped green (#48, #257). So the store suite
// runs against exactly the deployed arrangement, and the negative controls in
// delete_policy_integration_test.go measure it on app.users itself.
//
// Reading the live catalog is still the only way to get a true answer for a
// GIVEN database, which is why this exists at all: a deployment could be
// provisioned either way, and the report must not guess.
func (tx *UserTx) DeletePolicyStatus(ctx context.Context) (owner string, forced bool, policies int, err error) {
	err = tx.tx.QueryRow(ctx, `
		SELECT COALESCE((SELECT tableowner FROM pg_tables
		                  WHERE schemaname='app' AND tablename='users'), ''),
		       COALESCE((SELECT relforcerowsecurity FROM pg_class
		                  WHERE oid='app.users'::regclass), false),
		       (SELECT count(*) FROM pg_policies
		         WHERE schemaname='app' AND tablename='users'
		           AND cmd IN ('DELETE','ALL')
		           AND (SELECT tableowner FROM pg_tables
		                 WHERE schemaname='app' AND tablename='users') = ANY(roles))`,
	).Scan(&owner, &forced, &policies)
	if err != nil {
		return "", false, 0, fmt.Errorf("store: delete policy status: %w", err)
	}
	return owner, forced, policies, nil
}
